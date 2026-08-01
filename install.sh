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
# and takes the backup-protected update path. Override only when provisioning an
# intentionally separate fresh tree:
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
forward_args=()
for arg in "$@"; do
  case "$arg" in
    --fresh-install) install_mode="fresh" ;;
    --update) install_mode="update" ;;
    *) forward_args+=("$arg") ;;
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
for arg in "$@"; do
  case "$arg" in
    --migrate-user-groups|--no-migrate-user-groups) migration_choice_set=1 ;;
    -h|--help) help_requested=1 ;;
  esac
done

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
