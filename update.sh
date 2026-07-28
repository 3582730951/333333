#!/usr/bin/env bash
# update.sh — rebuild + redeploy the Codex Account Pool Server on this host,
# preserving every pooled account. Designed to be GENERIC and future-proof: it
# never hard-codes the feature set — it just rebuilds from whatever source is in
# this folder and delegates install/restart to scripts/install.sh. So shipping a
# NEW feature later = re-upload the folder + run this same script; update.sh
# itself does not need to change (see README "更新部署").
#
# Run it ON the VPS, from inside the uploaded project folder:
#     sudo ./update.sh                 # full update, bind 0.0.0.0 (external), restart
#     sudo ./update.sh --minimal       # gateway only
#     sudo ./update.sh --open-firewall # also open the listen port in ufw/firewalld
#     sudo RUN_TESTS=1 ./update.sh     # enable tests (skipped by default for speed)
#
# What it does:
#   1. Backs up the accounts SQLite DB (online, gzip-compressed snapshot) first,
#      with a disk-space preflight so a large pool/history never half-deploys.
#   2. Clears the Go build cache + .build so the binary is recompiled from scratch.
#   3. Runs scripts/install.sh (rebuilds the binary incl. the embedded UI, reinstalls
#      the sidecar + gopay, preserves the existing config/admin_token, restarts the
#      systemd units and atomically switches a ready worker behind the stable handoff).
#      install.sh NEVER writes the DB, so accounts survive.
#   4. Verifies the account count did not drop, prints the public access URL.
#
# Zero-config external HTTP: with no flags the service binds 0.0.0.0:<port> (keeps
# the current port on re-update; default 8787) so the panel is reachable at
# http://<public-ip>:<port>/ . Override with LISTEN_ADDR=127.0.0.1:8787 (e.g. behind
# a reverse proxy) or --listen-addr.
#
# Forward-compat: the new binary runs idempotent additive schema migrations on the
# preserved DB at startup, and unknown config keys get built-in defaults, so future
# code/schema changes apply in place without losing accounts. The pre-update backup
# is the rollback safety net.
#
# Env overrides (all optional; sensible defaults / auto-discovery otherwise):
#   DATA_DIR (default /var/lib/codex-pool), DATABASE_PATH, CONFIG_FILE,
#   SERVICE_NAME (default codex-pool), LISTEN_ADDR (default 0.0.0.0:<port>),
#   BACKUP_DIR (default <db-dir>/backups), BACKUP_KEEP (default 10),
#   SKIP_BACKUP=1 (skip the DB backup — only if you back up yourself),
#   CLEAN_MODCACHE=1 (also wipe the Go module cache),
#   CLEAN_NPM_CACHE=1 (also wipe the npm download cache for the node registrar).
set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$PROJECT_ROOT"

SERVICE_NAME="${SERVICE_NAME:-codex-pool}"
HANDOFF_SERVICE_NAME="${HANDOFF_SERVICE_NAME:-${SERVICE_NAME}-handoff}"
DATA_DIR="${DATA_DIR:-/var/lib/codex-pool}"
CONFIG_FILE="${CONFIG_FILE:-/etc/codex-pool/config.json}"
BACKUP=""
BEFORE=""
AFTER=""

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

bool_enabled() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

# human renders a byte count as B/KB/MB/GB/TB for friendly logs.
human() {
  awk -v b="${1:-0}" 'BEGIN{split("B KB MB GB TB",u," ");i=1;while(b>=1024&&i<5){b/=1024;i++};printf (i==1?"%d%s":"%.1f%s"),b,u[i]}'
}

# run_root runs a command as root when we are not already root (via sudo).
run_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    die "root privileges are required for: $*"
  fi
}

# discover_*_from_systemd read the effective env out of the installed unit so the
# current DB path / listen address are honored without passing any flags.
discover_env_from_systemd() {
  command -v systemctl >/dev/null 2>&1 || return 1
  local key="$1" env
  env="$(systemctl show -p Environment "${SERVICE_NAME}.service" 2>/dev/null)" || return 1
  case "$env" in
    *"${key}="*) env="${env#*${key}=}"; printf '%s\n' "${env%% *}" ;;
    *) return 1 ;;
  esac
}
discover_db_from_systemd()     { discover_env_from_systemd CODEX_POOL_DATABASE; }
discover_listen_from_systemd() { discover_env_from_systemd CODEX_POOL_LISTEN_ADDR; }

# db_from_config extracts "database_path" from the JSON config, if present.
db_from_config() {
  [[ -f "$1" ]] || return 1
  local v
  v="$(sed -n 's/.*"database_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" | head -1)"
  [[ -n "$v" ]] && printf '%s\n' "$v"
}

resolve_db_path() {
  if [[ -n "${DATABASE_PATH:-}" ]]; then
    DB="$DATABASE_PATH"
  elif DB="$(discover_db_from_systemd)" && [[ -n "$DB" ]]; then
    :
  elif DB="$(db_from_config "$CONFIG_FILE")" && [[ -n "$DB" ]]; then
    :
  else
    DB="${DATA_DIR%/}/pool.sqlite3"
  fi
}

# resolve_listen_addr implements the zero-config "bind 0.0.0.0" default. An explicit
# operator choice (LISTEN_ADDR env or --listen-addr flag) always wins; otherwise we
# bind all interfaces, keeping whatever port is already in use (default 8787).
resolve_listen_addr() {
  local a cur port
  for a in "$@"; do
    case "$a" in --listen-addr|--listen-addr=*) return 0 ;; esac
  done
  if [[ -n "${LISTEN_ADDR:-}" ]]; then export LISTEN_ADDR; return 0; fi
  cur="$(discover_listen_from_systemd || true)"
  port="${cur##*:}"
  [[ "$port" =~ ^[0-9]+$ ]] || port=8787
  LISTEN_ADDR="0.0.0.0:${port}"
  export LISTEN_ADDR
  case "$cur" in
    127.*|localhost:*|\[::1\]:*)
      warn "之前是仅本机监听(${cur})；按“对外可访问”默认改为 ${LISTEN_ADDR}。如需保持本机/反代：LISTEN_ADDR=${cur} sudo ./update.sh" ;;
  esac
}

# detect_public_ip best-effort resolves the public IPv4 for the access-URL hint.
# Falls back to the primary outbound local IP when there is no egress.
detect_public_ip() {
  local ip u
  for u in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com https://ip.sb; do
    ip="$(curl -fsS --max-time 5 "$u" 2>/dev/null | tr -d '[:space:]')" || ip=""
    if [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ || "$ip" =~ ^[0-9a-fA-F:]+$ ]]; then
      printf '%s\n' "$ip"; return 0
    fi
  done
  if command -v ip >/dev/null 2>&1; then
    ip="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -1)"
    [[ -n "$ip" ]] && { printf '%s\n' "$ip"; return 0; }
  fi
  hostname -I 2>/dev/null | awk '{print $1}'
}

# count_accounts prints the row count of the accounts table, or "" if it cannot be
# determined (no DB yet, or sqlite3 not installed) — never fatal. Instant even for
# hundreds of accounts (it is an indexed count, not a scan of usage history).
count_accounts() {
  local db="$1"
  [[ -f "$db" ]] || { printf ''; return 0; }
  command -v sqlite3 >/dev/null 2>&1 || { printf ''; return 0; }
  run_root sqlite3 "$db" "select count(*) from accounts;" 2>/dev/null || printf ''
}

# file_size prints a file's size in bytes (0 if absent).
file_size() { run_root stat -c %s "$1" 2>/dev/null || echo 0; }

# backup_db takes a compressed, consistent snapshot of the accounts DB. It scales:
# the snapshot is gzip-compressed (SQLite compresses heavily, so a big usage/ledger
# history stays small) and a disk-space preflight aborts BEFORE rebuilding rather
# than risk filling the disk or deploying without a safety net. 200-300 accounts is
# tiny for SQLite; this guard matters when the history DB grows large.
backup_db() {
  local db="$1" dir ts tmp dest dbsize wal free need
  if [[ ! -f "$db" ]]; then
    warn "no existing database at ${db} — treating this as a fresh install (nothing to back up)"
    return 0
  fi
  if bool_enabled "${SKIP_BACKUP:-0}"; then
    warn "SKIP_BACKUP=1 — 跳过账号库备份（请确保你已自行备份 ${db}）"
    return 0
  fi
  dir="${BACKUP_DIR:-$(dirname "$db")/backups}"
  run_root install -d -m 0750 "$dir"

  dbsize=$(file_size "$db")
  wal=$(file_size "${db}-wal")
  dbsize=$(( dbsize + wal ))
  free=$(df -Pk "$dir" 2>/dev/null | awk 'NR==2{printf "%.0f", $4*1024}')
  need=$(( dbsize * 13 / 10 + 1048576 )) # temp .backup (~1x) + gzip output + 1MB headroom
  if [[ -n "$free" && "$free" -gt 0 && "$free" -lt "$need" ]]; then
    die "备份空间不足：账号库≈$(human "$dbsize")，需≈$(human "$need")，可用$(human "$free")。请清磁盘、或 BACKUP_DIR=<更大分区路径> sudo ./update.sh、或 SKIP_BACKUP=1（自担风险）后重试。"
  fi

  ts="$(date +%Y%m%d-%H%M%S)"
  tmp="${dir}/.pool-${ts}.tmp"
  dest="${dir}/pool-${ts}.sqlite3.gz"
  if command -v sqlite3 >/dev/null 2>&1; then
    # Online .backup is consistent even while the service is running (WAL-safe),
    # then we compress and drop the uncompressed temp.
    run_root sqlite3 "$db" ".backup '${tmp}'"
    run_root sh -c "gzip -c '${tmp}' > '${dest}'"
    run_root rm -f "${tmp}"
  else
    run_root sh -c "gzip -c '${db}' > '${dest}'"
    [[ -f "${db}-wal" ]] && run_root sh -c "gzip -c '${db}-wal' > '${dest%.gz}-wal.gz'" || true
  fi
  BACKUP="$dest"
  log "Backed up accounts DB -> ${dest} (压缩后 $(human "$(file_size "$dest")")，源库 $(human "$dbsize"))"

  # Retain the most recent BACKUP_KEEP (default 10). Sort by the timestamped
  # filename (deterministic) rather than mtime.
  local keep="${BACKUP_KEEP:-10}"
  [[ "$keep" =~ ^[0-9]+$ ]] || keep=10
  ls -1 "${dir}"/pool-*.sqlite3.gz 2>/dev/null | sort -r | tail -n +$(( keep + 1 )) | while read -r f; do
    run_root rm -f "$f" "${f%.gz}-wal.gz" 2>/dev/null || true
  done
}

clean_build() {
  rm -rf "${PROJECT_ROOT}/.build" 2>/dev/null || true
  if command -v go >/dev/null 2>&1; then
    log "Clearing Go build cache (go clean -cache) for a clean recompile"
    go clean -cache 2>/dev/null || warn "go clean -cache failed (continuing)"
    if bool_enabled "${CLEAN_MODCACHE:-0}"; then
      log "Clearing Go module cache (go clean -modcache)"
      go clean -modcache 2>/dev/null || warn "go clean -modcache failed (continuing)"
    fi
  else
    warn "go not on PATH yet; scripts/install.sh will install it. No prior build cache to clear."
  fi
  # Node registrar: scripts/install.sh reinstalls it with `npm ci` (which wipes and
  # rebuilds node_modules from the lockfile), so an update always gets clean registrar
  # deps. Also drop any stale local node_modules in the SOURCE tree so a re-uploaded
  # folder never ships a half-installed dependency into the build, and optionally clear
  # the npm download cache (CLEAN_NPM_CACHE=1) for a fully cold dependency fetch.
  if [[ -d "${PROJECT_ROOT}/other_new_gpt_register/node_modules" ]]; then
    log "Removing stale source node_modules (other_new_gpt_register) — install.sh will reinstall via npm ci"
    rm -rf "${PROJECT_ROOT}/other_new_gpt_register/node_modules" 2>/dev/null || true
  fi
  if bool_enabled "${CLEAN_NPM_CACHE:-0}" && command -v npm >/dev/null 2>&1; then
    log "Clearing npm cache (CLEAN_NPM_CACHE=1)"
    npm cache clean --force >/dev/null 2>&1 || warn "npm cache clean failed (continuing)"
  fi
}

# STALE_SOURCES lists project source files that were REMOVED (or renamed) in a newer
# version of the code. They are deleted here before building because a folder that is
# re-uploaded additively (scp/rsync without --delete, an overlay copy, or unzipping over
# an existing tree) keeps files the new release dropped. A lingering .go file that still
# declares a type/func/method now defined elsewhere makes `go build` fail with
#   "X redeclared in this block" / "method ... already declared"
# — an otherwise-correct update that cannot compile. Re-running ./update.sh after a
# re-upload then always converges to the current tree, no manual cleanup needed.
#
# WHEN YOU DELETE OR RENAME A COMPILED SOURCE FILE in this project, append its OLD path
# here (relative to the project root) so every existing deployment self-heals on update.
STALE_SOURCES=(
  "internal/api/lifecycle_routes.go"   # consolidated into internal/api/lifecycle.go (handlers were redeclared)
)

# remove_stale_sources deletes the audited STALE_SOURCES list. Safe by construction: it
# only ever removes those exact, known paths — never anything discovered at runtime.
remove_stale_sources() {
  local f removed=0
  for f in "${STALE_SOURCES[@]}"; do
    [[ -n "$f" ]] || continue
    if [[ -e "${PROJECT_ROOT}/${f}" ]]; then
      if rm -f "${PROJECT_ROOT}/${f}" 2>/dev/null; then
        log "清理历史残留源码（旧版本已删除、增量上传残留）：${f}"
        removed=$((removed + 1))
      else
        warn "无法删除残留文件 ${f}（权限？）——若编译报 redeclared，请手动删除它"
      fi
    fi
  done
  [[ "$removed" -gt 0 ]] && log "共清理 ${removed} 个历史残留源码文件，避免重复声明导致编译失败。"
  return 0
}

# find_duplicate_go_decls scans every Go package directory for a top-level func or method
# declared in more than one file of the SAME package — the fingerprint of a stale leftover
# source file that STALE_SOURCES did not yet cover. It is package-aware (an in-package
# `package foo` and an external `package foo_test` in the same dir are NOT conflated) and
# prints one human-readable line per real conflict; prints nothing when the tree is clean.
find_duplicate_go_decls() {
  command -v awk >/dev/null 2>&1 || return 0
  find "${PROJECT_ROOT}" -type f -name '*.go' \
       -not -path '*/.build/*' -not -path '*/vendor/*' 2>/dev/null \
  | while IFS= read -r f; do
      awk -v dir="$(dirname "$f")" -v file="$f" '
        /^package[ \t]/ { pkg=$2; next }
        /^func[ \t]/ {
          s=$0; sub(/^func[ \t]+/,"",s)
          if (substr(s,1,1)=="(") {                       # method: func (r T) Name(
            recv=s; sub(/\).*/,"",recv); sub(/^\(/,"",recv)
            nf=split(recv,a," "); rtype=a[nf]; sub(/^\*/,"",rtype)
            rest=s; sub(/^\([^)]*\)[ \t]*/,"",rest)
            mname=rest; sub(/[ \t(].*/,"",mname)
            key="(" rtype ")." mname
          } else {                                         # plain func: func Name(
            mname=s; sub(/[ \t(].*/,"",mname)
            if (mname == "init" || mname == "_") next       # Go allows many init()/_() per package
            key=mname
          }
          if (key != "") print dir "\t" pkg "\t" key "\t" file
        }
      ' "$f"
    done \
  | sort -t"$(printf '\t')" -k1,1 -k2,2 -k3,3 -k4,4 \
  | awk -F"$(printf '\t')" '
      {
        grp=$1 SUBSEP $2 SUBSEP $3
        if (grp != cur) {
          if (cur != "" && nfiles > 1) print pd " (package " pp ") :: " pk " 重复声明于 -> " flist
          cur=grp; pd=$1; pp=$2; pk=$3; flist=$4; nfiles=1; seen=$4; next
        }
        if ($4 != seen) { nfiles++; flist=flist ", " $4; seen=$4 }
      }
      END { if (cur != "" && nfiles > 1) print pd " (package " pp ") :: " pk " 重复声明于 -> " flist }
    '
}

# diagnose_build_failure runs only after scripts/install.sh fails. It turns a cryptic Go
# "redeclared / already declared" error into an actionable hint that names the exact
# conflicting files, since that is by far the most common cause of an update that built
# fine on the dev machine but not on a host that was re-uploaded additively.
diagnose_build_failure() {
  local dups
  dups="$(find_duplicate_go_decls)"
  if [[ -n "$dups" ]]; then
    warn "检测到 Go 源码重复声明（极可能是旧版本残留文件未随更新删除）："
    printf '       %s\n' "$dups" >&2
    warn "处理：删除上面成对文件中“旧的那个”，或把它加入 update.sh 的 STALE_SOURCES 后重跑；"
    warn "      最稳妥是重新完整上传整个项目目录（覆盖式）再执行 ./update.sh。"
  fi
}

print_summary() {
  local listen ip port
  echo
  echo "更新完成 / Update complete."
  echo "  项目目录 Project:   ${PROJECT_ROOT}"
  echo "  账号数据库 DB:      ${DB}"
  [[ -n "$BACKUP" ]] && echo "  本次备份 Backup:    ${BACKUP}  (恢复: 停服后 gunzip -c <file> > ${DB})"
  if [[ -n "$AFTER" ]]; then
    echo "  池内账号 Accounts:  ${AFTER}（更新前 ${BEFORE:-?}）— 已保留 / preserved"
  else
    echo "  池内账号 Accounts:  数据库文件已原样保留（未安装 sqlite3，跳过计数校验）"
  fi
  if command -v systemctl >/dev/null 2>&1; then
    echo "  服务状态 Service:   $(systemctl is-active "${HANDOFF_SERVICE_NAME}.service" 2>/dev/null || echo unknown) (${HANDOFF_SERVICE_NAME}.service)"
  fi
  listen="$(discover_listen_from_systemd || true)"
  [[ -z "$listen" ]] && listen="${LISTEN_ADDR:-}"
  if [[ -n "$listen" ]]; then
    port="${listen##*:}"
    echo "  监听 Listen:        ${listen}"
    case "$listen" in
      127.*|localhost:*|\[::1\]:*)
        echo "                      ↳ 仅本机监听（如需外部访问：LISTEN_ADDR=0.0.0.0:${port} sudo ./update.sh）" ;;
      *)
        ip="$(detect_public_ip || true)"
        echo "  面板地址 Panel:     http://${ip:-<服务器IP>}:${port}/   （对外开放）"
        echo "                      若云厂商有安全组/防火墙，请放行 TCP ${port}（或重跑加 --open-firewall）" ;;
    esac
  fi
  echo
  echo "  日志 logs: journalctl -u ${HANDOFF_SERVICE_NAME}.service -f"
}

main() {
  [[ -f scripts/install.sh ]] || die "scripts/install.sh not found — run update.sh from inside the uploaded project folder"

  resolve_listen_addr "$@"
  resolve_db_path
  BEFORE="$(count_accounts "$DB")"
  [[ -n "$BEFORE" ]] && log "Accounts before update: ${BEFORE} (db: ${DB})"

  backup_db "$DB"
  clean_build
  remove_stale_sources

  log "Rebuilding + reinstalling via scripts/install.sh (LISTEN_ADDR=${LISTEN_ADDR:-<from args/config>}) $*"
  if ! bash scripts/install.sh "$@"; then
    diagnose_build_failure || true
    die "构建/安装失败（详见上方输出）。账号库已在更新前备份：${BACKUP:-<无备份>}"
  fi

  AFTER="$(count_accounts "$DB")"
  if [[ -n "$BEFORE" && -n "$AFTER" && "$AFTER" -lt "$BEFORE" ]]; then
    die "账号数从 ${BEFORE} 降到 ${AFTER}！更新已完成但请立即从备份恢复：${BACKUP:-<无备份>}"
  fi

  print_summary
}

main "$@"
