#!/usr/bin/env bash
# install.sh — friendly entry point for first install AND in-place upgrades.
#
# The real, authoritative installer is scripts/install.sh. It:
#   - builds the Go gateway WITH the embedded admin UI (-> codex-pool-server),
#   - installs the curl_cffi sidecar and registration workers,
#   - writes the systemd units + activation socket and starts the service,
#   - preserves an existing config + the accounts SQLite DB.
#
# This wrapper just forwards to it so `./install.sh` and `./update.sh` always use the
# SAME, correct build/install path (the previous standalone install.sh built a different
# binary name and an incompatible config, which then failed to start — that is fixed by
# delegating here). It also runs the same stale-source self-heal that update.sh does, so a
# folder re-uploaded over an older release still compiles.
#
# First install (binds 0.0.0.0:8787, full feature set):
#     sudo ./install.sh
# Running the same command again auto-detects the managed config/database/release
# and takes the backup-protected update path. During its atomic worker handoff,
# established HTTP/SSE/WebSocket requests remain on the old worker and new requests
# wait without receiving a synthetic upstream/downstream error; admission resumes
# automatically on success or rollback. The installer returns once the ready new worker
# owns new traffic; a background reaper removes the old process and release storage as
# soon as its established requests finish. Shared auxiliary services are staged but not
# hot-restarted. Override only when provisioning an intentionally separate fresh tree:
#     sudo ./install.sh --fresh-install
#
# Any scripts/install.sh flag passes straight through, e.g.:
#     sudo ./install.sh --minimal
#     sudo ./install.sh --listen-addr 127.0.0.1:8787 --without-warp --no-tests
#     sudo ./install.sh --migrate-user-groups       # skip the interactive question
#     sudo ./install.sh --no-migrate-user-groups    # keep account-pool groups separate
#     ./install.sh --help            # full option list
#
# Just want a local dev binary, no systemd/root? Skip this script and run:
#     go build -o pool-server ./cmd/pool-server
set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$PROJECT_ROOT"

[[ -f scripts/install.sh ]] || {
  printf 'ERROR: scripts/install.sh not found next to install.sh — run this from inside the project folder\n' >&2
  exit 1
}

install_mode="${CODEX_POOL_INSTALL_MODE:-auto}"
admin_port_override=""
egress_tcp_port_override=""
egress_udp_port_override=""
admin_port_choice_set=0
egress_tcp_port_choice_set=0
egress_udp_port_choice_set=0
warp_exits_choice_set=0
[[ -v LISTEN_ADDR ]] && admin_port_choice_set=1
[[ -v SIDECAR_ADDR ]] && egress_tcp_port_choice_set=1
[[ -v WARP_BASE_PORT ]] && egress_udp_port_choice_set=1
[[ -v WARP_EXITS ]] && warp_exits_choice_set=1

forward_args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --fresh-install)
      install_mode="fresh"
      shift
      ;;
    --update)
      install_mode="update"
      shift
      ;;
    --admin-port)
      admin_port_override="${2:?missing value for --admin-port}"
      admin_port_choice_set=1
      shift 2
      ;;
    --admin-port=*)
      admin_port_override="${1#*=}"
      admin_port_choice_set=1
      shift
      ;;
    --egress-tcp-port)
      egress_tcp_port_override="${2:?missing value for --egress-tcp-port}"
      egress_tcp_port_choice_set=1
      shift 2
      ;;
    --egress-tcp-port=*)
      egress_tcp_port_override="${1#*=}"
      egress_tcp_port_choice_set=1
      shift
      ;;
    --egress-udp-port|--warp-base-port)
      egress_udp_port_override="${2:?missing value for $1}"
      egress_udp_port_choice_set=1
      shift 2
      ;;
    --egress-udp-port=*|--warp-base-port=*)
      egress_udp_port_override="${1#*=}"
      egress_udp_port_choice_set=1
      shift
      ;;
    *)
      forward_args+=("$1")
      shift
      ;;
  esac
done
set -- "${forward_args[@]}"

case "$install_mode" in
  auto|fresh|update) ;;
  *) printf 'ERROR: CODEX_POOL_INSTALL_MODE must be auto, fresh, or update\n' >&2; exit 2 ;;
esac

# Resolve enough installer paths to distinguish an existing managed deployment.
# This deliberately uses only durable markers, never the presence of a source tree.
detect_existing_install() {
  local prefix="${INSTALL_PREFIX:-/usr/local}"
  local app_dir="${APP_DIR:-}"
  local data_dir="${DATA_DIR:-/var/lib/codex-pool}"
  local config_dir="${CONFIG_DIR:-/etc/codex-pool}"
  local config_file="${CONFIG_FILE:-}"
  local database_path="${DATABASE_PATH:-}"
  local systemd_dir="${SYSTEMD_DIR:-/etc/systemd/system}"
  local service_name="${SERVICE_NAME:-codex-pool}"
  local app_explicit=0 config_explicit=0 database_explicit=0
  [[ -n "${APP_DIR:-}" ]] && app_explicit=1
  [[ -n "${CONFIG_FILE:-}" ]] && config_explicit=1
  [[ -n "${DATABASE_PATH:-}" ]] && database_explicit=1

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --prefix) prefix="${2:-$prefix}"; [[ "$app_explicit" == 0 ]] && app_dir="${prefix%/}/lib/codex-pool"; shift 2 ;;
      --prefix=*) prefix="${1#*=}"; [[ "$app_explicit" == 0 ]] && app_dir="${prefix%/}/lib/codex-pool"; shift ;;
      --app-dir) app_dir="${2:-}"; app_explicit=1; shift 2 ;;
      --app-dir=*) app_dir="${1#*=}"; app_explicit=1; shift ;;
      --data-dir) data_dir="${2:-$data_dir}"; [[ "$database_explicit" == 0 ]] && database_path="${data_dir%/}/pool.sqlite3"; shift 2 ;;
      --data-dir=*) data_dir="${1#*=}"; [[ "$database_explicit" == 0 ]] && database_path="${data_dir%/}/pool.sqlite3"; shift ;;
      --config-dir) config_dir="${2:-$config_dir}"; [[ "$config_explicit" == 0 ]] && config_file="${config_dir%/}/config.json"; shift 2 ;;
      --config-dir=*) config_dir="${1#*=}"; [[ "$config_explicit" == 0 ]] && config_file="${config_dir%/}/config.json"; shift ;;
      --database-path) database_path="${2:-}"; database_explicit=1; shift 2 ;;
      --database-path=*) database_path="${1#*=}"; database_explicit=1; shift ;;
      *) shift ;;
    esac
  done
  [[ -n "$app_dir" ]] || app_dir="${prefix%/}/lib/codex-pool"
  [[ -n "$config_file" ]] || config_file="${config_dir%/}/config.json"
  [[ -n "$database_path" ]] || database_path="${data_dir%/}/pool.sqlite3"
  [[ -f "$config_file" || -f "$database_path" || -L "${app_dir%/}/current" || \
     -f "${systemd_dir%/}/${service_name}.service" || -f "${systemd_dir%/}/${service_name}-handoff.service" ]]
}

valid_port() {
  [[ "${1:-}" =~ ^[0-9]+$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

listen_port() {
  local addr="${1:-}"
  case "$addr" in
    \[*\]:*|*:*) printf '%s\n' "${addr##*:}" ;;
    *) printf '%s\n' "$addr" ;;
  esac
}

address_with_port() {
  local addr="${1:-}" port="$2"
  case "$addr" in
    \[*\]:*) printf '%s:%s\n' "${addr%:*}" "$port" ;;
    :*) printf ':%s\n' "$port" ;;
    *:*) printf '%s:%s\n' "${addr%:*}" "$port" ;;
    "") printf '0.0.0.0:%s\n' "$port" ;;
    *) printf '%s:%s\n' "$addr" "$port" ;;
  esac
}

config_value() {
  local file="$1" key="$2"
  [[ -r "$file" ]] || return 1
  sed -n "s/.*\"${key}\"[[:space:]]*:[[:space:]]*\"\{0,1\}\([^\",}]*\)\"\{0,1\}[[:space:]]*,\{0,1\}.*/\1/p" "$file" | head -1
}

load_installed_port_defaults() {
  local config_file="${CONFIG_FILE:-/etc/codex-pool/config.json}" value
  [[ -r "$config_file" ]] || return 0
  if [[ "$admin_port_choice_set" == 0 ]]; then
    value="$(config_value "$config_file" listen_addr || true)"
    [[ -n "$value" ]] && LISTEN_ADDR="$value"
  fi
  if [[ "$egress_tcp_port_choice_set" == 0 ]]; then
    value="$(config_value "$config_file" default_sidecar_endpoint || true)"
    value="${value#http://}"
    value="${value#https://}"
    [[ -n "$value" ]] && SIDECAR_ADDR="$value"
  fi
  if [[ "$egress_udp_port_choice_set" == 0 ]]; then
    value="$(config_value "$config_file" warp_exit_base_port || true)"
    valid_port "$value" && WARP_BASE_PORT="$value"
  fi
}

prompt_port() {
  local label="$1" default_port="$2" answer
  while true; do
    printf '%s [%s]: ' "$label" "$default_port" >&2
    IFS= read -r answer || true
    answer="${answer:-$default_port}"
    if valid_port "$answer"; then
      printf '%s\n' "$answer"
      return 0
    fi
    printf '端口必须是 1-65535 的整数，请重新输入。\n' >&2
  done
}

echo "================================"
echo " Pool Server 安装 / 更新"
echo "================================"
echo "==> Codex skills 兼容提示：完整官方 skills/plugins/Browser Use 体验请使用官方 Codex 账号通道；第三方 API 通道为 best-effort。"
echo

# The legacy compatibility migration copies every account-pool group into a
# same-named user-facing routing group. That is useful for some upgrades, but
# recreates deliberately removed user groups on the next service start. Ask on
# the friendly entry point and default to keeping the two group layers separate.
# Automation can bypass the question with either documented flag or by setting
# MIGRATE_USER_GROUPS=0/1.
migration_choice_set=0
help_requested=0
sidecar_prompt_enabled=1
warp_prompt_enabled=1
for arg in "$@"; do
  case "$arg" in
    --migrate-user-groups|--no-migrate-user-groups) migration_choice_set=1 ;;
    --listen-addr|--listen-addr=*) admin_port_choice_set=1 ;;
    --sidecar-addr|--sidecar-addr=*) egress_tcp_port_choice_set=1 ;;
    --warp-exits|--warp-exits=*) warp_exits_choice_set=1 ;;
    --minimal) sidecar_prompt_enabled=0; warp_prompt_enabled=0 ;;
    --without-sidecar) sidecar_prompt_enabled=0 ;;
    --without-warp) warp_prompt_enabled=0 ;;
    -h|--help) help_requested=1 ;;
  esac
done

if [[ -v WITH_SIDECAR ]] && ! [[ "${WITH_SIDECAR}" =~ ^(1|true|TRUE|yes|YES|on|ON)$ ]]; then
  sidecar_prompt_enabled=0
fi
if [[ -v WITH_WARP ]] && ! [[ "${WITH_WARP}" =~ ^(1|true|TRUE|yes|YES|on|ON)$ ]]; then
  warp_prompt_enabled=0
fi

if [[ "$help_requested" == 0 ]]; then
  load_installed_port_defaults
  LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:8787}"
  SIDECAR_ADDR="${SIDECAR_ADDR:-127.0.0.1:8790}"
  WARP_BASE_PORT="${WARP_BASE_PORT:-40000}"

  if [[ -n "$admin_port_override" ]]; then
    valid_port "$admin_port_override" || { printf 'ERROR: --admin-port must be 1-65535\n' >&2; exit 2; }
    LISTEN_ADDR="$(address_with_port "$LISTEN_ADDR" "$admin_port_override")"
  elif [[ "$admin_port_choice_set" == 0 && -t 0 ]]; then
    selected_port="$(prompt_port '管理员前端绑定监听端口' "$(listen_port "$LISTEN_ADDR")")"
    LISTEN_ADDR="$(address_with_port "$LISTEN_ADDR" "$selected_port")"
  fi
  export LISTEN_ADDR

  if [[ "$sidecar_prompt_enabled" == 1 ]]; then
    if [[ -n "$egress_tcp_port_override" ]]; then
      valid_port "$egress_tcp_port_override" || { printf 'ERROR: --egress-tcp-port must be 1-65535\n' >&2; exit 2; }
      SIDECAR_ADDR="$(address_with_port "$SIDECAR_ADDR" "$egress_tcp_port_override")"
    elif [[ "$egress_tcp_port_choice_set" == 0 && -t 0 ]]; then
      selected_port="$(prompt_port '全部上游流量 TCP 出口监听端口' "$(listen_port "$SIDECAR_ADDR")")"
      SIDECAR_ADDR="$(address_with_port "$SIDECAR_ADDR" "$selected_port")"
    fi
    export SIDECAR_ADDR
  fi

  if [[ "$warp_prompt_enabled" == 1 ]]; then
    if [[ -n "$egress_udp_port_override" ]]; then
      valid_port "$egress_udp_port_override" || { printf 'ERROR: --egress-udp-port must be 1-65535\n' >&2; exit 2; }
      WARP_BASE_PORT="$egress_udp_port_override"
    elif [[ "$egress_udp_port_choice_set" == 0 && -t 0 ]]; then
      WARP_BASE_PORT="$(prompt_port '全部上游流量 UDP/WARP 出口监听端口' "$WARP_BASE_PORT")"
    fi
    if [[ -n "$egress_udp_port_override" || "$egress_udp_port_choice_set" == 1 || -t 0 ]]; then
      WITH_WARP=1
      if [[ "$warp_exits_choice_set" == 0 ]]; then
        WARP_EXITS=1
      fi
      export WITH_WARP WARP_EXITS WARP_BASE_PORT
    fi
  fi
fi

if [[ "$help_requested" == "0" && "$migration_choice_set" == "0" && ! -v MIGRATE_USER_GROUPS ]]; then
  migration_answer=""
  if [[ -t 0 ]]; then
    printf '是否将现有账号池分组迁移为同名用户分组？这可能会创建多个用户分组。 [y/N]: '
    IFS= read -r migration_answer || true
  else
    echo "==> 非交互执行：默认跳过账号池分组→用户分组迁移"
  fi
  case "$migration_answer" in
    y|Y|yes|YES|Yes|是) set -- "$@" --migrate-user-groups ;;
    *) set -- "$@" --no-migrate-user-groups ;;
  esac
fi

if [[ "$help_requested" == "0" ]] && { [[ "$install_mode" == "update" ]] || { [[ "$install_mode" == "auto" ]] && detect_existing_install "$@"; }; }; then
  [[ -f update.sh ]] || { printf 'ERROR: update.sh not found next to install.sh\n' >&2; exit 1; }
  echo "==> 检测到现有配置、数据库或已激活 release：自动进入备份保护的更新流程"
  echo "==> 如需刻意新建独立实例，请使用 --fresh-install 并传入独立的 CONFIG_FILE/DATA_DIR/APP_DIR。"
  exec bash update.sh "$@"
fi

if [[ "$help_requested" == "0" && -x scripts/prune-managed-source.sh ]]; then
  bash scripts/prune-managed-source.sh
fi
echo "==> 转交给规范安装器 scripts/install.sh（构建内嵌 UI 的二进制、安装 sidecar/注册 worker、写入 systemd）"
exec bash scripts/install.sh "$@"
