#!/usr/bin/env bash
set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$PROJECT_ROOT"

APP_NAME="codex-pool-server"
HANDOFF_NAME="codex-pool-handoff"
SERVICE_NAME="${SERVICE_NAME:-codex-pool}"
HANDOFF_SERVICE_NAME="${HANDOFF_SERVICE_NAME:-${SERVICE_NAME}-handoff}"
SERVICE_USER="${SERVICE_USER:-codex-pool}"
SERVICE_GROUP="${SERVICE_GROUP:-$SERVICE_USER}"
INSTALL_PREFIX="${INSTALL_PREFIX:-/usr/local}"
CONFIG_DIR="${CONFIG_DIR:-/etc/codex-pool}"
DATA_DIR="${DATA_DIR:-/var/lib/codex-pool}"
SYSTEMD_DIR="${SYSTEMD_DIR:-/etc/systemd/system}"
RUN_TESTS="${RUN_TESTS:-0}"
INSTALL_SYSTEMD="${INSTALL_SYSTEMD:-auto}"
START_SERVICE="${START_SERVICE:-1}"
WITH_SIDECAR="${WITH_SIDECAR:-1}"
WITH_WARP="${WITH_WARP:-0}"
MIGRATE_USER_GROUPS="${MIGRATE_USER_GROUPS:-0}"
# Node registration engine: installs Node.js + a headless
# Chrome + Xvfb and the puppeteer-real-browser registrar so the pool can auto-register
# accounts on a no-display cloud VPS. Default on; disable with --without-registration.
WITH_REGISTRATION="${WITH_REGISTRATION:-1}"
NODE_MIN_MAJOR="${NODE_MIN_MAJOR:-22}"
NODE_INSTALL_MAJOR="${NODE_INSTALL_MAJOR:-22}"
if [[ -n "${REGISTRAR_INSTALL:-}" ]]; then
  REGISTRAR_INSTALL_EXPLICIT=1
else
  REGISTRAR_INSTALL_EXPLICIT=0
fi
REGISTRAR_SOURCE="${REGISTRAR_SOURCE:-${PROJECT_ROOT}/workers/node-registrar}"
REGISTRAR_INSTALL="${REGISTRAR_INSTALL:-${DATA_DIR%/}/registrar}"
PY_REGISTRAR_SOURCE="${PY_REGISTRAR_SOURCE:-${PROJECT_ROOT}/services/codex_register}"
CODEX_REAUTH_WORKER_SOURCE="${CODEX_REAUTH_WORKER_SOURCE:-${PROJECT_ROOT}/services/codex_reauth_worker.py}"
CODEX_REAUTH_ADDR="${CODEX_REAUTH_ADDR:-127.0.0.1:8802}"
CODEX_REAUTH_CONCURRENCY="${CODEX_REAUTH_CONCURRENCY:-1}"
NODE_BIN="${NODE_BIN:-}"
CHROME_BIN="${CHROME_BIN:-}"
WARP_EXITS="${WARP_EXITS:-8}"
WARP_BASE_PORT="${WARP_BASE_PORT:-40000}"
WARP_ACCOUNTS_PER_EXIT="${WARP_ACCOUNTS_PER_EXIT:-3}"
CF_SOLVER_URL="${CF_SOLVER_URL:-}"
LISTEN_ADDR="${LISTEN_ADDR:-0.0.0.0:8787}"
ADMIN_TOKEN="${ADMIN_TOKEN:-}"
OPEN_FIREWALL="${OPEN_FIREWALL:-0}"
PUBLIC_URL="${PUBLIC_URL:-}"
INSTALL_GO="${INSTALL_GO:-auto}"
GO_INSTALL_VERSION="${GO_INSTALL_VERSION:-1.25.12}"
GO_INSTALL_ROOT="${GO_INSTALL_ROOT:-/usr/local}"
BUILD_DIR="${BUILD_DIR:-${PROJECT_ROOT}/.build}"
SKIP_OS_PACKAGES="${SKIP_OS_PACKAGES:-0}"
# Deployment storage is bounded before any candidate can take traffic. The
# defaults intentionally favor preserving live releases and a healthy filesystem
# reserve over forcing another update through a nearly full disk.
MAX_DRAINING_RELEASES="${MAX_DRAINING_RELEASES:-2}"
INSTALL_FREE_RESERVE_MIN_BYTES="${INSTALL_FREE_RESERVE_MIN_BYTES:-536870912}"
INSTALL_FREE_RESERVE_PERCENT="${INSTALL_FREE_RESERVE_PERCENT:-10}"
RELEASE_STORAGE_MAX_BYTES="${RELEASE_STORAGE_MAX_BYTES:-0}"
CONSOLE_GENERATION_MAX_BYTES="${CONSOLE_GENERATION_MAX_BYTES:-268435456}"
BUILD_CACHE_MAX_BYTES="${BUILD_CACHE_MAX_BYTES:-2147483648}"
CONSOLE_GENERATION_KEEP="${CONSOLE_GENERATION_KEEP:-2}"
CONSOLE_GENERATION_MAX_AGE_SECONDS="${CONSOLE_GENERATION_MAX_AGE_SECONDS:-86400}"
STATUS_ONLY=0
SIDECAR_ADDR="${SIDECAR_ADDR:-127.0.0.1:8790}"
ADMIN_PORT_OVERRIDE=""
EGRESS_TCP_PORT_OVERRIDE=""
EGRESS_UDP_PORT_OVERRIDE=""
# HEALTH_TIMEOUT bounds private-worker and post-switch /readyz gates.
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"
# Abort a candidate that is crashing in a loop instead of silently polling the
# Unix socket for the full health timeout. A transient first restart is allowed.
WORKER_START_RESTART_LIMIT="${WORKER_START_RESTART_LIMIT:-5}"
# DRAIN_TIMEOUT controls only the ordinary post-switch reaper's progress-log cadence.
# It is not an installation deadline: after an atomic traffic switch, the superseded
# worker drains in the background while the promoted candidate performs full startup.
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-30}"
WORKER_DESTROY_TIMEOUT="${WORKER_DESTROY_TIMEOUT:-30}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/var/lock/codex-pool-install.lock}"
HANDOFF_CONTROL_SOCKET="${HANDOFF_CONTROL_SOCKET:-${DATA_DIR%/}/run/handoff-control.sock}"
PROC_ROOT="${PROC_ROOT:-/proc}"
HANDOFF_PAUSE_STATE="${HANDOFF_PAUSE_STATE:-${DATA_DIR%/}/run/admission-paused.json}"
RELEASE_ID=""
RELEASE_DIR=""
PREVIOUS_RELEASE_ID=""
HANDOFF_PAUSED=0
HANDOFF_PAUSE_STARTED_MS=0
LEGACY_SERVICE_ACTIVE=0
ACTIVATION_PENDING=0
ACTIVATION_OLD_RELEASE=""
ACTIVATION_OLD_SOCKET=""
ACTIVATION_OLD_WORKER_RELEASE=""
ACTIVATION_NEW_RELEASE=""
DEPLOY_CURRENT_RELEASE=""
DEPLOY_TOTAL_RELEASE_BYTES=0
DEPLOY_RELEASE_BUDGET_BYTES=0
DEPLOY_FREE_BYTES=0
DEPLOY_RESERVE_BYTES=0
DEPLOY_PREDICTED_PEAK_BYTES=0
DEPLOY_BACKUP_BYTES=0
DEPLOY_CONSOLE_BYTES=0
DEPLOY_CANDIDATE_ESTIMATE_BYTES=0
DEPLOY_DRAINING_COUNT=0
DEPLOY_DRAINING_JSON=""
DEPLOY_LAST_RECLAIM_ERROR=""
DEPLOY_ADMISSION_PAUSE_DURATION_MS=0
# Set by install_sidecar: 1 when the sidecar source changed (or first install), 0 when
# unchanged — lets the restart block skip a needless sidecar restart that would sever
# in-flight upstream streams.
SIDECAR_CHANGED=1
SUPER_INSTRUCT_SOURCE=""
SUPER_INSTRUCT_SOURCE_KIND="none"

if [[ -n "${BIN_DIR:-}" ]]; then
  BIN_DIR_EXPLICIT=1
else
  BIN_DIR_EXPLICIT=0
  BIN_DIR="${INSTALL_PREFIX}/bin"
fi
if [[ -n "${APP_DIR:-}" ]]; then
  APP_DIR_EXPLICIT=1
else
  APP_DIR_EXPLICIT=0
  APP_DIR="${INSTALL_PREFIX}/lib/codex-pool"
fi
if [[ -n "${CONFIG_FILE:-}" ]]; then
  CONFIG_FILE_EXPLICIT=1
else
  CONFIG_FILE_EXPLICIT=0
  CONFIG_FILE="${CONFIG_DIR}/config.json"
fi
if [[ -n "${DATABASE_PATH:-}" ]]; then
  DATABASE_PATH_EXPLICIT=1
else
  DATABASE_PATH_EXPLICIT=0
  DATABASE_PATH="${DATA_DIR}/pool.sqlite3"
fi
if [[ -n "${SIDECAR_VENV:-}" ]]; then
  SIDECAR_VENV_EXPLICIT=1
else
  SIDECAR_VENV_EXPLICIT=0
  SIDECAR_VENV="${DATA_DIR%/}/sidecar-venv"
fi
if [[ -n "${SIDECAR_INSTALL_DIR:-}" ]]; then
  SIDECAR_INSTALL_DIR_EXPLICIT=1
else
  SIDECAR_INSTALL_DIR_EXPLICIT=0
  SIDECAR_INSTALL_DIR="${APP_DIR%/}/sidecar"
fi
if [[ -n "${SIDECAR_COOKIE_DIR:-}" ]]; then
  SIDECAR_COOKIE_DIR_EXPLICIT=1
else
  SIDECAR_COOKIE_DIR_EXPLICIT=0
  SIDECAR_COOKIE_DIR="${DATA_DIR%/}/sidecar-cookies"
fi
log() {
  printf '==> %s\n' "$*"
}

warn() {
  printf 'WARN: %s\n' "$*" >&2
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<EOF
Usage: scripts/install.sh [options]

Build and install ${APP_NAME}.

Options:
  --prefix PATH             Install prefix. Default: ${INSTALL_PREFIX}
  --bin-dir PATH            Binary directory. Default: <prefix>/bin
  --app-dir PATH            Read-only app assets directory. Default: <prefix>/lib/codex-pool
  --config-dir PATH         Config directory. Default: ${CONFIG_DIR}
  --data-dir PATH           Runtime data directory. Default: ${DATA_DIR}
  --database-path PATH      SQLite database path. Default: <data-dir>/pool.sqlite3
  --systemd                 Install a systemd unit even when auto-detection is unsure
  --no-systemd              Do not install a systemd unit
  --no-start                Do not restart/start the systemd service
  --no-tests                Skip go test ./... (default, use RUN_TESTS=1 to enable)
  --full                    Install all supported optional components (default)
  --minimal                 Install only the Go gateway
  --with-sidecar            Install and manage the curl_cffi sidecar (default)
  --without-sidecar         Do not install the curl_cffi sidecar
  --with-registration       Install all repository-owned registration workers: the locked
                            Python protocol/browser runtime plus Node.js, Chrome and Xvfb.
                            Default. Registration remains disabled until readiness and a
                            disposable per-method canary have succeeded.
  --without-registration    Do not install registration worker runtimes
  --migrate-user-groups     Copy missing account-pool groups to same-named user groups on service start
  --no-migrate-user-groups  Keep account-pool groups separate (default)
  --with-warp               Provision the multi-exit WARP CF-fallback pool (wgcf + wireproxy)
  --without-warp            Do not provision WARP (default)
  --warp-exits N            Independent WARP exits to provision (implies --with-warp; default ${WARP_EXITS})
  --warp-base-port PORT     First local WARP egress port. Default: ${WARP_BASE_PORT}
  --cf-solver-url URL       FlareSolverr-compatible cf_clearance solver base URL (enables the solver rung)
  --listen-addr ADDR        HTTP listen address for API + embedded frontend. Default: ${LISTEN_ADDR}
  --admin-port PORT         Change only the embedded admin/frontend port
  --egress-tcp-port PORT    TCP sidecar egress port; occupied foreign listener is terminated or PORT+1 is used
  --egress-udp-port PORT    UDP/WARP egress base port; occupied foreign listener is terminated or PORT+1 is used
  --admin-token TOKEN       Admin API token. Generated for new externally exposed configs when empty
  --public-url URL          Public frontend URL to print in the install summary
  --open-firewall           Try to open the listen TCP port in ufw/firewalld
  --no-open-firewall        Do not change firewall rules (default)
  --sidecar-addr ADDR       Sidecar listen address. Default: ${SIDECAR_ADDR}
  --without-go-install      Fail instead of installing Go when Go is missing/too old
  --go-version VERSION      Go version to install when needed. Default: ${GO_INSTALL_VERSION}
  --max-draining-releases N Refuse an update when N old releases are still draining. Default: ${MAX_DRAINING_RELEASES}
  --install-free-reserve-min-bytes N Minimum filesystem reserve. Default: ${INSTALL_FREE_RESERVE_MIN_BYTES}
  --install-free-reserve-percent N  Filesystem reserve percentage (0-90). Default: ${INSTALL_FREE_RESERVE_PERCENT}
  --release-storage-max-bytes N  Release byte ceiling; 0 selects the automatic budget
  --console-generation-max-bytes N  Old console asset ceiling. Default: ${CONSOLE_GENERATION_MAX_BYTES}
  --build-cache-max-bytes N Build/npm cache pressure ceiling. Default: ${BUILD_CACHE_MAX_BYTES}
  --status                  Print deployment/reaper/disk storage status and exit
  -h, --help                Show this help

Environment overrides:
  SERVICE_NAME, SERVICE_USER, SERVICE_GROUP, INSTALL_PREFIX, BIN_DIR, APP_DIR,
  CONFIG_DIR, CONFIG_FILE, DATA_DIR, DATABASE_PATH, SYSTEMD_DIR,
  RUN_TESTS, INSTALL_SYSTEMD, START_SERVICE, WITH_SIDECAR, MIGRATE_USER_GROUPS,
  WITH_WARP, WARP_EXITS, WARP_BASE_PORT, WARP_ACCOUNTS_PER_EXIT, WARP_DIR, CF_SOLVER_URL,
  LISTEN_ADDR, ADMIN_TOKEN, PUBLIC_URL, OPEN_FIREWALL,
  SIDECAR_ADDR, SIDECAR_VENV, SIDECAR_INSTALL_DIR, SIDECAR_COOKIE_DIR,
  INSTALL_GO, GO_INSTALL_VERSION, GO_INSTALL_ROOT, GO_BIN, HEALTH_TIMEOUT,
  WORKER_START_RESTART_LIMIT,
  DRAIN_TIMEOUT (ordinary post-switch background drain log interval),
  MAX_DRAINING_RELEASES, INSTALL_FREE_RESERVE_MIN_BYTES,
  INSTALL_FREE_RESERVE_PERCENT, RELEASE_STORAGE_MAX_BYTES,
  CONSOLE_GENERATION_MAX_BYTES, BUILD_CACHE_MAX_BYTES,
  SKIP_OS_PACKAGES, GO_TARBALL_SHA256,
  WITH_REGISTRATION, NODE_MIN_MAJOR, NODE_INSTALL_MAJOR, NODE_BIN, CHROME_BIN,
  REGISTRAR_SOURCE, REGISTRAR_INSTALL, PY_REGISTRAR_SOURCE,
  CODEX_REAUTH_WORKER_SOURCE, CODEX_REAUTH_ADDR, CODEX_REAUTH_CONCURRENCY
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --prefix)
      INSTALL_PREFIX="${2:?missing value for --prefix}"
      if [[ "$BIN_DIR_EXPLICIT" == "0" ]]; then
        BIN_DIR="${INSTALL_PREFIX}/bin"
      fi
      if [[ "$APP_DIR_EXPLICIT" == "0" ]]; then
        APP_DIR="${INSTALL_PREFIX}/lib/codex-pool"
      fi
      if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_INSTALL_DIR="${APP_DIR%/}/sidecar"
      fi
      shift 2
      ;;
    --prefix=*)
      INSTALL_PREFIX="${1#*=}"
      if [[ "$BIN_DIR_EXPLICIT" == "0" ]]; then
        BIN_DIR="${INSTALL_PREFIX}/bin"
      fi
      if [[ "$APP_DIR_EXPLICIT" == "0" ]]; then
        APP_DIR="${INSTALL_PREFIX}/lib/codex-pool"
      fi
      if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_INSTALL_DIR="${APP_DIR%/}/sidecar"
      fi
      shift
      ;;
    --bin-dir)
      BIN_DIR="${2:?missing value for --bin-dir}"
      BIN_DIR_EXPLICIT=1
      shift 2
      ;;
    --bin-dir=*)
      BIN_DIR="${1#*=}"
      BIN_DIR_EXPLICIT=1
      shift
      ;;
    --app-dir)
      APP_DIR="${2:?missing value for --app-dir}"
      APP_DIR_EXPLICIT=1
      if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_INSTALL_DIR="${APP_DIR%/}/sidecar"
      fi
      shift 2
      ;;
    --app-dir=*)
      APP_DIR="${1#*=}"
      APP_DIR_EXPLICIT=1
      if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_INSTALL_DIR="${APP_DIR%/}/sidecar"
      fi
      shift
      ;;
    --config-dir)
      CONFIG_DIR="${2:?missing value for --config-dir}"
      if [[ "$CONFIG_FILE_EXPLICIT" == "0" ]]; then
        CONFIG_FILE="${CONFIG_DIR}/config.json"
      fi
      shift 2
      ;;
    --config-dir=*)
      CONFIG_DIR="${1#*=}"
      if [[ "$CONFIG_FILE_EXPLICIT" == "0" ]]; then
        CONFIG_FILE="${CONFIG_DIR}/config.json"
      fi
      shift
      ;;
    --data-dir)
      DATA_DIR="${2:?missing value for --data-dir}"
      if [[ "$DATABASE_PATH_EXPLICIT" == "0" ]]; then
        DATABASE_PATH="${DATA_DIR}/pool.sqlite3"
      fi
      if [[ "$SIDECAR_VENV_EXPLICIT" == "0" ]]; then
        SIDECAR_VENV="${DATA_DIR%/}/sidecar-venv"
      fi
      if [[ "$SIDECAR_COOKIE_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_COOKIE_DIR="${DATA_DIR%/}/sidecar-cookies"
      fi
      shift 2
      ;;
    --data-dir=*)
      DATA_DIR="${1#*=}"
      if [[ "$DATABASE_PATH_EXPLICIT" == "0" ]]; then
        DATABASE_PATH="${DATA_DIR}/pool.sqlite3"
      fi
      if [[ "$SIDECAR_VENV_EXPLICIT" == "0" ]]; then
        SIDECAR_VENV="${DATA_DIR%/}/sidecar-venv"
      fi
      if [[ "$SIDECAR_COOKIE_DIR_EXPLICIT" == "0" ]]; then
        SIDECAR_COOKIE_DIR="${DATA_DIR%/}/sidecar-cookies"
      fi
      shift
      ;;
    --database-path)
      DATABASE_PATH="${2:?missing value for --database-path}"
      DATABASE_PATH_EXPLICIT=1
      shift 2
      ;;
    --database-path=*)
      DATABASE_PATH="${1#*=}"
      DATABASE_PATH_EXPLICIT=1
      shift
      ;;
    --systemd)
      INSTALL_SYSTEMD=1
      shift
      ;;
    --no-systemd)
      INSTALL_SYSTEMD=0
      shift
      ;;
    --no-start)
      START_SERVICE=0
      shift
      ;;
    --no-tests)
      RUN_TESTS=0
      shift
      ;;
    --full)
      WITH_SIDECAR=1
      WITH_REGISTRATION=1
      shift
      ;;
    --minimal)
      WITH_SIDECAR=0
      WITH_REGISTRATION=0
      shift
      ;;
    --with-sidecar)
      WITH_SIDECAR=1
      shift
      ;;
    --without-sidecar)
      WITH_SIDECAR=0
      shift
      ;;
    --with-registration)
      WITH_REGISTRATION=1
      shift
      ;;
    --without-registration)
      WITH_REGISTRATION=0
      shift
      ;;
    --migrate-user-groups)
      MIGRATE_USER_GROUPS=1
      shift
      ;;
    --no-migrate-user-groups)
      MIGRATE_USER_GROUPS=0
      shift
      ;;
    --with-warp)
      WITH_WARP=1
      shift
      ;;
    --without-warp)
      WITH_WARP=0
      shift
      ;;
    --warp-exits)
      WARP_EXITS="${2:?missing value for --warp-exits}"
      WITH_WARP=1
      shift 2
      ;;
    --warp-exits=*)
      WARP_EXITS="${1#*=}"
      WITH_WARP=1
      shift
      ;;
    --warp-base-port)
      EGRESS_UDP_PORT_OVERRIDE="${2:?missing value for --warp-base-port}"
      WITH_WARP=1
      shift 2
      ;;
    --warp-base-port=*)
      EGRESS_UDP_PORT_OVERRIDE="${1#*=}"
      WITH_WARP=1
      shift
      ;;
    --cf-solver-url)
      CF_SOLVER_URL="${2:?missing value for --cf-solver-url}"
      shift 2
      ;;
    --cf-solver-url=*)
      CF_SOLVER_URL="${1#*=}"
      shift
      ;;
    --listen-addr)
      LISTEN_ADDR="${2:?missing value for --listen-addr}"
      shift 2
      ;;
    --listen-addr=*)
      LISTEN_ADDR="${1#*=}"
      shift
      ;;
    --admin-port)
      ADMIN_PORT_OVERRIDE="${2:?missing value for --admin-port}"
      shift 2
      ;;
    --admin-port=*)
      ADMIN_PORT_OVERRIDE="${1#*=}"
      shift
      ;;
    --egress-tcp-port)
      EGRESS_TCP_PORT_OVERRIDE="${2:?missing value for --egress-tcp-port}"
      WITH_SIDECAR=1
      shift 2
      ;;
    --egress-tcp-port=*)
      EGRESS_TCP_PORT_OVERRIDE="${1#*=}"
      WITH_SIDECAR=1
      shift
      ;;
    --egress-udp-port)
      EGRESS_UDP_PORT_OVERRIDE="${2:?missing value for --egress-udp-port}"
      WITH_WARP=1
      shift 2
      ;;
    --egress-udp-port=*)
      EGRESS_UDP_PORT_OVERRIDE="${1#*=}"
      WITH_WARP=1
      shift
      ;;
    --admin-token)
      ADMIN_TOKEN="${2:?missing value for --admin-token}"
      shift 2
      ;;
    --admin-token=*)
      ADMIN_TOKEN="${1#*=}"
      shift
      ;;
    --public-url)
      PUBLIC_URL="${2:?missing value for --public-url}"
      shift 2
      ;;
    --public-url=*)
      PUBLIC_URL="${1#*=}"
      shift
      ;;
    --open-firewall)
      OPEN_FIREWALL=1
      shift
      ;;
    --no-open-firewall)
      OPEN_FIREWALL=0
      shift
      ;;
    --sidecar-addr)
      SIDECAR_ADDR="${2:?missing value for --sidecar-addr}"
      shift 2
      ;;
    --sidecar-addr=*)
      SIDECAR_ADDR="${1#*=}"
      shift
      ;;
    --without-go-install)
      INSTALL_GO=0
      shift
      ;;
    --go-version)
      GO_INSTALL_VERSION="${2:?missing value for --go-version}"
      shift 2
      ;;
    --go-version=*)
      GO_INSTALL_VERSION="${1#*=}"
      shift
      ;;
    --max-draining-releases)
      MAX_DRAINING_RELEASES="${2:?missing value for --max-draining-releases}"
      shift 2
      ;;
    --max-draining-releases=*)
      MAX_DRAINING_RELEASES="${1#*=}"
      shift
      ;;
    --free-reserve-min-bytes|--install-free-reserve-min-bytes)
      INSTALL_FREE_RESERVE_MIN_BYTES="${2:?missing value for $1}"
      shift 2
      ;;
    --free-reserve-min-bytes=*|--install-free-reserve-min-bytes=*)
      INSTALL_FREE_RESERVE_MIN_BYTES="${1#*=}"
      shift
      ;;
    --free-reserve-percent|--install-free-reserve-percent)
      INSTALL_FREE_RESERVE_PERCENT="${2:?missing value for $1}"
      shift 2
      ;;
    --free-reserve-percent=*|--install-free-reserve-percent=*)
      INSTALL_FREE_RESERVE_PERCENT="${1#*=}"
      shift
      ;;
    --release-storage-max-bytes)
      RELEASE_STORAGE_MAX_BYTES="${2:?missing value for --release-storage-max-bytes}"
      shift 2
      ;;
    --release-storage-max-bytes=*)
      RELEASE_STORAGE_MAX_BYTES="${1#*=}"
      shift
      ;;
    --console-generation-max-bytes)
      CONSOLE_GENERATION_MAX_BYTES="${2:?missing value for --console-generation-max-bytes}"
      shift 2
      ;;
    --console-generation-max-bytes=*)
      CONSOLE_GENERATION_MAX_BYTES="${1#*=}"
      shift
      ;;
    --build-cache-max-bytes)
      BUILD_CACHE_MAX_BYTES="${2:?missing value for --build-cache-max-bytes}"
      shift 2
      ;;
    --build-cache-max-bytes=*)
      BUILD_CACHE_MAX_BYTES="${1#*=}"
      shift
      ;;
    --status)
      STATUS_ONLY=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1"
      ;;
  esac
done

case "$(uname -s)" in
  Linux) ;;
  *) die "this installer targets Linux hosts; use go test/go build manually on this OS" ;;
esac

is_absolute_path() {
  case "$1" in
    /*) return 0 ;;
    *) return 1 ;;
  esac
}

require_absolute_path() {
  local name="$1" value="$2"
  is_absolute_path "$value" || die "${name} must be an absolute path: ${value}"
}

run_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo "$@"
  else
    die "root privileges are required for: $*"
  fi
}

run_service() {
  if [[ "$(id -un)" == "$SERVICE_USER" ]]; then
    "$@"
  elif [[ "$(id -u)" -eq 0 ]] && command -v runuser >/dev/null 2>&1; then
    runuser -u "$SERVICE_USER" -- "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo -u "$SERVICE_USER" -- "$@"
  else
    die "cannot execute as service user ${SERVICE_USER}: $*"
  fi
}

bool_enabled() {
  case "${1:-}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

normalize_migrate_user_groups() {
  case "${MIGRATE_USER_GROUPS:-}" in
    1|true|TRUE|yes|YES|on|ON) MIGRATE_USER_GROUPS=1 ;;
    ""|0|false|FALSE|no|NO|off|OFF) MIGRATE_USER_GROUPS=0 ;;
    *) die "MIGRATE_USER_GROUPS must be a boolean (0/1, false/true, no/yes, off/on)" ;;
  esac
}

json_escape() {
  local s="${1:-}"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

generate_admin_token() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 24
  else
    od -An -N24 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

listen_host() {
  local addr="${1:-}"
  case "$addr" in
    :*) printf '\n' ;;
    \[*\]:*) printf '%s\n' "${addr%%\]:*}]" | sed 's/^\[//;s/\]$//' ;;
    *:*) printf '%s\n' "${addr%:*}" ;;
    *) printf '%s\n' "$addr" ;;
  esac
}

listen_port() {
  local addr="${1:-}"
  case "$addr" in
    \[*\]:*) printf '%s\n' "${addr##*:}" ;;
    *:*) printf '%s\n' "${addr##*:}" ;;
    *) printf '%s\n' "$addr" ;;
  esac
}

valid_port() {
  [[ "${1:-}" =~ ^[0-9]+$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
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

apply_port_overrides() {
  if [[ -n "$ADMIN_PORT_OVERRIDE" ]]; then
    valid_port "$ADMIN_PORT_OVERRIDE" || die "--admin-port must be between 1 and 65535"
    LISTEN_ADDR="$(address_with_port "$LISTEN_ADDR" "$ADMIN_PORT_OVERRIDE")"
  fi
  if [[ -n "$EGRESS_TCP_PORT_OVERRIDE" ]]; then
    valid_port "$EGRESS_TCP_PORT_OVERRIDE" || die "--egress-tcp-port must be between 1 and 65535"
    SIDECAR_ADDR="$(address_with_port "$SIDECAR_ADDR" "$EGRESS_TCP_PORT_OVERRIDE")"
  fi
  if [[ -n "$EGRESS_UDP_PORT_OVERRIDE" ]]; then
    valid_port "$EGRESS_UDP_PORT_OVERRIDE" || die "--egress-udp-port/--warp-base-port must be between 1 and 65535"
    WARP_BASE_PORT="$EGRESS_UDP_PORT_OVERRIDE"
  fi
}

listener_pids() {
  local protocol="$1" port="$2"
  if command -v lsof >/dev/null 2>&1; then
    if [[ "$protocol" == tcp ]]; then
      run_root lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true
    else
      run_root lsof -nP -t -iUDP:"$port" 2>/dev/null || true
    fi
    return 0
  fi
  if command -v fuser >/dev/null 2>&1; then
    run_root fuser -n "$protocol" "$port" 2>&1 | tr ' ' '\n' | grep -E '^[0-9]+$' || true
    return 0
  fi
  if command -v ss >/dev/null 2>&1; then
    if [[ "$protocol" == tcp ]]; then
      run_root ss -H -ltnp "sport = :${port}" 2>/dev/null
    else
      run_root ss -H -lunp "sport = :${port}" 2>/dev/null
    fi | grep -oE 'pid=[0-9]+' | cut -d= -f2 || true
  fi
}

port_in_use() {
  local protocol="$1" port="$2"
  if [[ -n "$(listener_pids "$protocol" "$port")" ]]; then
    return 0
  fi
  if command -v ss >/dev/null 2>&1; then
    if [[ "$protocol" == tcp ]]; then
      run_root ss -H -ltn "sport = :${port}" 2>/dev/null | grep -q .
    else
      run_root ss -H -lun "sport = :${port}" 2>/dev/null | grep -q .
    fi
    return $?
  fi
  if command -v python3 >/dev/null 2>&1; then
    run_root python3 - "$protocol" "$port" <<'PY'
import errno
import socket
import sys

protocol, port = sys.argv[1], int(sys.argv[2])
kind = socket.SOCK_STREAM if protocol == "tcp" else socket.SOCK_DGRAM
occupied = False
for family, address in (
    (socket.AF_INET, ("0.0.0.0", port)),
    (socket.AF_INET6, ("::", port)),
):
    try:
        sock = socket.socket(family, kind)
    except OSError:
        continue
    try:
        if family == socket.AF_INET6:
            sock.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        sock.bind(address)
    except OSError as exc:
        if exc.errno == errno.EADDRINUSE:
            occupied = True
    finally:
        sock.close()
sys.exit(0 if occupied else 1)
PY
    return $?
  fi
  return 1
}

ports_in_use() {
  local protocols="$1" port="$2" protocol
  for protocol in $protocols; do
    port_in_use "$protocol" "$port" && return 0
  done
  return 1
}

managed_unit_pid() {
  local unit="$1"
  command -v systemctl >/dev/null 2>&1 || return 1
  run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null | grep -E '^[1-9][0-9]*$'
}

port_owned_by_managed_unit() {
  local protocols="$1" port="$2" unit="$3" managed pid protocol found=0
  [[ -n "$unit" ]] || return 1
  managed="$(managed_unit_pid "$unit" || true)"
  [[ -n "$managed" ]] || return 1
  for protocol in $protocols; do
    while IFS= read -r pid; do
      [[ -n "$pid" ]] || continue
      found=1
      [[ "$pid" == "$managed" ]] || return 1
    done < <(listener_pids "$protocol" "$port" | sort -u)
  done
  (( found == 1 ))
}

terminate_port_listeners() {
  local protocols="$1" port="$2" unit="$3" protocol pid managed comm
  local -a pids=()
  managed="$(managed_unit_pid "$unit" || true)"
  for protocol in $protocols; do
    while IFS= read -r pid; do
      [[ "$pid" =~ ^[1-9][0-9]*$ ]] || continue
      [[ "$pid" == "$managed" || "$pid" == "$$" || "$pid" == "1" ]] && continue
      comm="$(cat "/proc/${pid}/comm" 2>/dev/null || true)"
      case "$comm" in systemd|init|sshd) continue ;; esac
      pids+=("$pid")
    done < <(listener_pids "$protocol" "$port" | sort -u)
  done
  (( ${#pids[@]} > 0 )) || return 1
  log "Port ${port} is occupied; terminating listener pid(s): ${pids[*]}"
  run_root kill -TERM "${pids[@]}" >/dev/null 2>&1 || return 1
  for _ in {1..30}; do
    sleep 0.1
    ports_in_use "$protocols" "$port" || return 0
    port_owned_by_managed_unit "$protocols" "$port" "$unit" && return 0
  done
  return 1
}

port_conflicts_with_selected_listener() {
  local protocols="$1" port="$2" admin_port sidecar_port
  admin_port="$(listen_port "$LISTEN_ADDR")"
  sidecar_port="$(listen_port "$SIDECAR_ADDR")"
  if [[ " $protocols " == *" tcp "* && "$port" == "$admin_port" ]]; then
    return 0
  fi
  if [[ " $protocols " == *" tcp "* && -n "${RESOLVED_SIDECAR_PORT:-}" && "$port" == "$sidecar_port" ]]; then
    return 0
  fi
  return 1
}

resolve_egress_port() {
  local protocols="$1" requested="$2" label="$3" managed_unit="$4" candidate
  valid_port "$requested" || die "${label} port must be between 1 and 65535: ${requested}"
  candidate="$requested"
  if ! port_conflicts_with_selected_listener "$protocols" "$candidate" && ! ports_in_use "$protocols" "$candidate"; then
    RESOLVED_EGRESS_PORT="$candidate"
    return 0
  fi
  if ! port_conflicts_with_selected_listener "$protocols" "$candidate" &&
    port_owned_by_managed_unit "$protocols" "$candidate" "$managed_unit"; then
    RESOLVED_EGRESS_PORT="$candidate"
    return 0
  fi
  if ! port_conflicts_with_selected_listener "$protocols" "$candidate" && terminate_port_listeners "$protocols" "$candidate" "$managed_unit"; then
    RESOLVED_EGRESS_PORT="$candidate"
    return 0
  fi

  warn "${label} port ${requested} remains occupied; selecting the first free +1 port"
  for (( candidate=requested+1; candidate<=65535; candidate++ )); do
    port_conflicts_with_selected_listener "$protocols" "$candidate" && continue
    ports_in_use "$protocols" "$candidate" && continue
    RESOLVED_EGRESS_PORT="$candidate"
    log "${label} port changed ${requested} -> ${candidate}"
    return 0
  done
  die "no free ${label} port remains above ${requested}"
}

resolve_requested_egress_ports() {
  local port
  RESOLVED_SIDECAR_PORT=""
  if bool_enabled "$WITH_SIDECAR"; then
    port="$(listen_port "$SIDECAR_ADDR")"
    resolve_egress_port "tcp" "$port" "TCP egress" "${SERVICE_NAME}-sidecar.service"
    SIDECAR_ADDR="$(address_with_port "$SIDECAR_ADDR" "$RESOLVED_EGRESS_PORT")"
    RESOLVED_SIDECAR_PORT="$RESOLVED_EGRESS_PORT"
  fi
  if bool_enabled "$WITH_WARP"; then
    [[ "$WARP_EXITS" =~ ^[0-9]+$ && "$WARP_EXITS" -ge 1 ]] || die "--warp-exits must be a positive integer (got: ${WARP_EXITS})"
    resolve_egress_port "tcp udp" "$WARP_BASE_PORT" "UDP/WARP egress" "${SERVICE_NAME}-warp@1.service"
    WARP_BASE_PORT="$RESOLVED_EGRESS_PORT"
    (( WARP_BASE_PORT + WARP_EXITS - 1 <= 65535 )) || die "WARP exit port range exceeds 65535"
  fi
}

codex_reauth_base_url() {
  local host port
  host="$(listen_host "$CODEX_REAUTH_ADDR")"
  port="$(listen_port "$CODEX_REAUTH_ADDR")"
  if [[ "$host" == *:* && "$host" != \[*\] ]]; then
    host="[${host}]"
  fi
  printf 'http://%s:%s\n' "$host" "$port"
}

validate_codex_reauth_settings() {
  bool_enabled "$WITH_REGISTRATION" || return 0
  local host port
  host="$(listen_host "$CODEX_REAUTH_ADDR")"
  port="$(listen_port "$CODEX_REAUTH_ADDR")"
  case "$host" in
    127.*|localhost|::1) ;;
    *) die "CODEX_REAUTH_ADDR must bind to loopback, got: ${CODEX_REAUTH_ADDR}" ;;
  esac
  [[ "$port" =~ ^[0-9]+$ ]] && (( port >= 1 && port <= 65535 )) ||
    die "CODEX_REAUTH_ADDR has an invalid port: ${CODEX_REAUTH_ADDR}"
  [[ "$CODEX_REAUTH_CONCURRENCY" =~ ^[0-9]+$ ]] &&
    (( CODEX_REAUTH_CONCURRENCY >= 1 && CODEX_REAUTH_CONCURRENCY <= 64 )) ||
    die "CODEX_REAUTH_CONCURRENCY must be between 1 and 64"
}

external_listen_enabled() {
  local host
  host="$(listen_host "$LISTEN_ADDR")"
  case "$host" in
    ""|"0.0.0.0"|"::") return 0 ;;
    "127."*|"localhost"|"::1") return 1 ;;
    *) return 0 ;;
  esac
}

config_string_value() {
  local file="$1" key="$2"
  [[ -f "$file" ]] || return 1
  awk -v key="$key" '
    $0 ~ "\"" key "\"[[:space:]]*:" {
      line = $0
      sub("^[^\"]*\"" key "\"[[:space:]]*:[[:space:]]*\"", "", line)
      sub("\"[[:space:]]*,?[[:space:]]*$", "", line)
      print line
      found = 1
      exit
    }
    END { if (!found) exit 1 }
  ' "$file"
}

render_runtime_config() {
  local listen_json data_json database_json admin_json sidecar_json reauth_json
  listen_json="$(json_escape "$LISTEN_ADDR")"
  data_json="$(json_escape "${DATA_DIR%/}/data")"
  database_json="$(json_escape "$DATABASE_PATH")"
  admin_json="$(json_escape "$ADMIN_TOKEN")"
  if bool_enabled "$WITH_SIDECAR"; then
    sidecar_json="$(json_escape "http://${SIDECAR_ADDR}")"
  else
    sidecar_json=""
  fi
  if bool_enabled "$WITH_REGISTRATION"; then
    reauth_json="$(json_escape "$(codex_reauth_base_url)")"
  else
    reauth_json=""
  fi
  awk \
    -v listen="$listen_json" \
    -v data_dir="$data_json" \
    -v database="$database_json" \
    -v admin="$admin_json" \
    -v sidecar="$sidecar_json" \
    -v reauth="$reauth_json" '
      /^[[:space:]]*"listen_addr"[[:space:]]*:/ {
        print "  \"listen_addr\": \"" listen "\","
        next
      }
      /^[[:space:]]*"data_dir"[[:space:]]*:/ {
        print "  \"data_dir\": \"" data_dir "\","
        next
      }
      /^[[:space:]]*"database_path"[[:space:]]*:/ {
        print "  \"database_path\": \"" database "\","
        next
      }
      /^[[:space:]]*"admin_token"[[:space:]]*:/ {
        print "  \"admin_token\": \"" admin "\","
        next
      }
      /^[[:space:]]*"default_sidecar_endpoint"[[:space:]]*:/ {
        print "  \"default_sidecar_endpoint\": \"" sidecar "\","
        next
      }
      /^[[:space:]]*"codex_reauth_worker_url"[[:space:]]*:/ {
        print "  \"codex_reauth_worker_url\": \"" reauth "\","
        next
      }
      /^[[:space:]]*"super_instruct_local_enabled"[[:space:]]*:/ {
        # Compatibility-only field: deployment-wide enablement was retired.
        # Effective behavior is selected by API-key installation and bounded
        # by the API key user-group policy.
        print "  \"super_instruct_local_enabled\": false,"
        next
      }
      # A fresh server install must not turn optional Codex client policy into
      # an operational override.  Empty values mean that the generated Codex
      # installer preserves client-side approval, sandbox, and reasoning
      # settings.  Existing server configs are kept byte-for-byte elsewhere in
      # install_binary_and_config, so previously saved non-empty values remain
      # explicit administrator choices.
      /^[[:space:]]*"codex_install_effort"[[:space:]]*:/ {
        print "  \"codex_install_effort\": \"\","
        next
      }
      /^[[:space:]]*"codex_install_approval_policy"[[:space:]]*:/ {
        print "  \"codex_install_approval_policy\": \"\","
        next
      }
      /^[[:space:]]*"codex_install_sandbox_mode"[[:space:]]*:/ {
        print "  \"codex_install_sandbox_mode\": \"\","
        next
      }
      { print }
    ' config.example.json
}

python_runtime_enabled() {
  bool_enabled "$WITH_SIDECAR" || bool_enabled "$WITH_REGISTRATION"
}

group_exists() {
  if command -v getent >/dev/null 2>&1; then
    getent group "$1" >/dev/null 2>&1
  else
    awk -F: -v name="$1" '$1 == name { found = 1 } END { exit found ? 0 : 1 }' /etc/group
  fi
}

user_exists() {
  if command -v id >/dev/null 2>&1; then
    id -u "$1" >/dev/null 2>&1
  elif command -v getent >/dev/null 2>&1; then
    getent passwd "$1" >/dev/null 2>&1
  else
    awk -F: -v name="$1" '$1 == name { found = 1 } END { exit found ? 0 : 1 }' /etc/passwd
  fi
}

nologin_shell() {
  if [[ -x /usr/sbin/nologin ]]; then
    printf '/usr/sbin/nologin\n'
  elif [[ -x /sbin/nologin ]]; then
    printf '/sbin/nologin\n'
  else
    printf '/bin/false\n'
  fi
}

create_group() {
  if command -v groupadd >/dev/null 2>&1; then
    run_root groupadd --system "$SERVICE_GROUP"
  elif command -v addgroup >/dev/null 2>&1; then
    run_root addgroup -S "$SERVICE_GROUP"
  else
    die "cannot create group; groupadd/addgroup is missing"
  fi
}

create_user() {
  local shell
  shell="$(nologin_shell)"
  if command -v useradd >/dev/null 2>&1; then
    run_root useradd --system --gid "$SERVICE_GROUP" --home-dir "$DATA_DIR" --create-home --shell "$shell" "$SERVICE_USER"
  elif command -v adduser >/dev/null 2>&1; then
    run_root adduser -S -D -H -h "$DATA_DIR" -s "$shell" -G "$SERVICE_GROUP" "$SERVICE_USER"
    run_root mkdir -p "$DATA_DIR"
  else
    die "cannot create user; useradd/adduser is missing"
  fi
}

version_ge() {
  local left="${1#go}" right="${2#go}"
  local l_major l_minor l_patch r_major r_minor r_patch

  IFS=. read -r l_major l_minor l_patch <<<"$left"
  IFS=. read -r r_major r_minor r_patch <<<"$right"
  l_patch="${l_patch:-0}"
  r_patch="${r_patch:-0}"

  [[ "$l_major" =~ ^[0-9]+$ && "$l_minor" =~ ^[0-9]+$ && "$l_patch" =~ ^[0-9]+$ ]] || return 1
  [[ "$r_major" =~ ^[0-9]+$ && "$r_minor" =~ ^[0-9]+$ && "$r_patch" =~ ^[0-9]+$ ]] || return 1

  if (( l_major != r_major )); then
    (( l_major > r_major ))
    return
  fi
  if (( l_minor != r_minor )); then
    (( l_minor > r_minor ))
    return
  fi
  (( l_patch >= r_patch ))
}

go_min_version() {
  awk '$1 == "go" { print $2; exit }' go.mod
}

go_version() {
  "$1" version | awk '{ print $3 }' | sed 's/^go//'
}

find_go() {
  if [[ -n "${GO_BIN:-}" && -x "${GO_BIN:-}" ]]; then
    printf '%s\n' "$GO_BIN"
    return 0
  fi
  command -v go 2>/dev/null || return 1
}

go_platform() {
  local arch
  arch="$(uname -m)"
  case "$arch" in
    x86_64|amd64) printf 'linux-amd64\n' ;;
    aarch64|arm64) printf 'linux-arm64\n' ;;
    armv6l|armv7l|armv7) printf 'linux-armv6l\n' ;;
    i386|i686) printf 'linux-386\n' ;;
    *) die "unsupported CPU architecture for automatic Go install: $arch" ;;
  esac
}

install_os_packages() {
  [[ "$SKIP_OS_PACKAGES" == "1" ]] && return 0

  local base_pkgs sidecar_pkgs
  if command -v apt-get >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar build-essential sqlite3 lsof)
    sidecar_pkgs=(python3 python3-venv python3-pip)
    log "Installing OS packages with apt-get"
    run_root env DEBIAN_FRONTEND=noninteractive apt-get update
    if python_runtime_enabled; then
      run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${base_pkgs[@]}"
    fi
  elif command -v dnf >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar gcc gcc-c++ make sqlite lsof)
    sidecar_pkgs=(python3 python3-pip)
    log "Installing OS packages with dnf"
    if python_runtime_enabled; then
      run_root dnf install -y "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root dnf install -y "${base_pkgs[@]}"
    fi
  elif command -v yum >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar gcc gcc-c++ make sqlite lsof)
    sidecar_pkgs=(python3 python3-pip)
    log "Installing OS packages with yum"
    if python_runtime_enabled; then
      run_root yum install -y "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root yum install -y "${base_pkgs[@]}"
    fi
  elif command -v apk >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar build-base sqlite lsof)
    sidecar_pkgs=(python3 py3-pip py3-virtualenv)
    log "Installing OS packages with apk"
    if python_runtime_enabled; then
      run_root apk add --no-cache "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root apk add --no-cache "${base_pkgs[@]}"
    fi
  elif command -v pacman >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar base-devel sqlite lsof)
    sidecar_pkgs=(python python-pip python-virtualenv)
    log "Installing OS packages with pacman"
    if python_runtime_enabled; then
      run_root pacman -Sy --needed --noconfirm "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root pacman -Sy --needed --noconfirm "${base_pkgs[@]}"
    fi
  else
    warn "No supported package manager found. Install ca-certificates, curl, tar, gcc/make, and sqlite3 manually."
  fi
}

need_system_deps() {
  command -v curl >/dev/null 2>&1 || return 0
  command -v tar >/dev/null 2>&1 || return 0
  command -v sqlite3 >/dev/null 2>&1 || return 0
  { command -v lsof >/dev/null 2>&1 || command -v fuser >/dev/null 2>&1 || command -v ss >/dev/null 2>&1; } || return 0
  { command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1; } || return 0
  if python_runtime_enabled; then
    command -v python3 >/dev/null 2>&1 || return 0
    python3 -m venv --help >/dev/null 2>&1 || return 0
  fi
  return 1
}

install_go_toolchain() {
  command -v curl >/dev/null 2>&1 || die "curl is required to install Go"
  command -v tar >/dev/null 2>&1 || die "tar is required to install Go"

  local platform url tmp target
  platform="$(go_platform)"
  url="https://go.dev/dl/go${GO_INSTALL_VERSION}.${platform}.tar.gz"
  target="${GO_INSTALL_ROOT%/}/go${GO_INSTALL_VERSION}"
  tmp="$(mktemp -d)"

  log "Installing Go ${GO_INSTALL_VERSION} into ${target}"
  curl -fsSL --retry 3 -o "${tmp}/go.tgz" "$url"
  if [[ -n "${GO_TARBALL_SHA256:-}" ]]; then
    printf '%s  %s\n' "$GO_TARBALL_SHA256" "${tmp}/go.tgz" | sha256sum -c -
  else
    warn "GO_TARBALL_SHA256 is not set; downloaded Go archive will not be checksum-verified."
  fi

  run_root rm -rf "${target}.tmp"
  run_root mkdir -p "${target}.tmp"
  run_root tar -C "${target}.tmp" --strip-components=1 -xzf "${tmp}/go.tgz"
  run_root rm -rf "$target"
  run_root mv "${target}.tmp" "$target"
  rm -rf "$tmp"

  GO_BIN="${target}/bin/go"
}

ensure_go() {
  local min current go_bin
  min="$(go_min_version)"
  [[ -n "$min" ]] || die "could not read minimum Go version from go.mod"

  if go_bin="$(find_go)"; then
    current="$(go_version "$go_bin")"
    if version_ge "$current" "$min"; then
      GO_BIN="$go_bin"
      log "Using Go ${current}: ${GO_BIN}"
      return 0
    fi
    warn "Go ${current} is lower than required ${min}"
  else
    warn "Go is not installed"
  fi

  case "$INSTALL_GO" in
    1|true|TRUE|yes|YES|auto)
      install_go_toolchain
      ;;
    *)
      die "Go ${min}+ is required. Install Go or rerun without --without-go-install."
      ;;
  esac

  current="$(go_version "$GO_BIN")"
  version_ge "$current" "$min" || die "installed Go ${current} is lower than required ${min}"
  log "Using Go ${current}: ${GO_BIN}"
}

ensure_project_files() {
  [[ -f go.mod ]] || die "go.mod not found; run this script from the project checkout"
  [[ -f cmd/pool-server/main.go ]] || die "cmd/pool-server/main.go not found"
  [[ -f cmd/pool-handoff/main.go ]] || die "cmd/pool-handoff/main.go not found"
  [[ -f scripts/rollback-release.sh ]] || die "scripts/rollback-release.sh not found"
  [[ -f config.example.json ]] || die "config.example.json not found"
  [[ -f internal/console/dist/index.html ]] || die "embedded console index.html not found"
  [[ -f scripts/verify-console-release.sh ]] || die "embedded console release verifier not found"
  PROJECT_ROOT="$PROJECT_ROOT" \
    MANAGED_SOURCE_MANIFEST="${PROJECT_ROOT}/scripts/managed-source-manifest.txt" \
    CONSOLE_RELEASE_ALLOW_STALE=1 \
    bash "${PROJECT_ROOT}/scripts/verify-console-release.sh" >/dev/null ||
    die "embedded console release is incomplete; refusing to build or switch releases"
  # Internal Markdown resources are intentionally absent from the GitHub
  # publication. A local/internal source tree still stages the complete bundle;
  # an in-place public update preserves the bundle from the active release. A
  # fresh public install remains valid with the optional capability unavailable.
  if [[ -f "${PROJECT_ROOT}/super-instruct/bridge.md" && -d "${PROJECT_ROOT}/super-instruct/codex-skills" ]]; then
    SUPER_INSTRUCT_SOURCE="${PROJECT_ROOT}/super-instruct"
    SUPER_INSTRUCT_SOURCE_KIND="bundled"
  elif [[ -f "${APP_DIR%/}/current/super-instruct/bridge.md" && -d "${APP_DIR%/}/current/super-instruct/codex-skills" ]]; then
    SUPER_INSTRUCT_SOURCE="${APP_DIR%/}/current/super-instruct"
    SUPER_INSTRUCT_SOURCE_KIND="preserved"
    warn "Super-Instruct resources are absent from this source; preserving the active release bundle"
  else
    warn "Super-Instruct resources are not included in this source; installing the gateway with that optional capability unavailable"
  fi
  if bool_enabled "$WITH_SIDECAR"; then
    [[ -f sidecar/curl_cffi_sidecar.py ]] || die "sidecar/curl_cffi_sidecar.py not found"
    [[ -f sidecar/requirements.txt ]] || die "sidecar/requirements.txt not found"
  fi
  if bool_enabled "$WITH_REGISTRATION"; then
    [[ -f "${REGISTRAR_SOURCE%/}/package.json" ]] || die "repository-owned Node registrar not found: ${REGISTRAR_SOURCE}"
    [[ -f "${REGISTRAR_SOURCE%/}/package-lock.json" ]] || die "Node registrar lockfile not found: ${REGISTRAR_SOURCE}/package-lock.json"
    [[ -f "${REGISTRAR_SOURCE%/}/sbom.cdx.json" ]] || die "Node registrar SBOM not found: ${REGISTRAR_SOURCE}/sbom.cdx.json"
    local registrar_file
    for registrar_file in protocol_register.py browser_register.py reg_v3.py phone_verify.py requirements.txt; do
      [[ -f "${PY_REGISTRAR_SOURCE%/}/${registrar_file}" ]] ||
        die "repository-owned Python registrar artifact not found: ${PY_REGISTRAR_SOURCE}/${registrar_file}"
    done
    [[ -f "${PY_REGISTRAR_SOURCE%/}/login_oauth.py" ]] ||
      die "repository-owned Codex OAuth login artifact not found: ${PY_REGISTRAR_SOURCE}/login_oauth.py"
    [[ -f "$CODEX_REAUTH_WORKER_SOURCE" ]] ||
      die "repository-owned Codex reauth worker not found: ${CODEX_REAUTH_WORKER_SOURCE}"
  fi
  if bool_enabled "$WITH_WARP"; then
    [[ -f scripts/warp-exit.sh ]] || die "scripts/warp-exit.sh not found (required for --with-warp)"
  fi
}

ensure_absolute_paths() {
  require_absolute_path "INSTALL_PREFIX" "$INSTALL_PREFIX"
  require_absolute_path "BIN_DIR" "$BIN_DIR"
  require_absolute_path "APP_DIR" "$APP_DIR"
  require_absolute_path "CONFIG_DIR" "$CONFIG_DIR"
  require_absolute_path "CONFIG_FILE" "$CONFIG_FILE"
  require_absolute_path "DATA_DIR" "$DATA_DIR"
  require_absolute_path "HANDOFF_CONTROL_SOCKET" "$HANDOFF_CONTROL_SOCKET"
  require_absolute_path "HANDOFF_PAUSE_STATE" "$HANDOFF_PAUSE_STATE"
  (( ${#HANDOFF_CONTROL_SOCKET} < 104 )) || die "handoff control Unix socket path is too long: ${HANDOFF_CONTROL_SOCKET}"
  require_absolute_path "DATABASE_PATH" "$DATABASE_PATH"
  require_absolute_path "SYSTEMD_DIR" "$SYSTEMD_DIR"
  require_absolute_path "GO_INSTALL_ROOT" "$GO_INSTALL_ROOT"
  require_absolute_path "BUILD_DIR" "$BUILD_DIR"
  require_absolute_path "SIDECAR_INSTALL_DIR" "$SIDECAR_INSTALL_DIR"
  require_absolute_path "SIDECAR_VENV" "$SIDECAR_VENV"
  require_absolute_path "CODEX_REAUTH_WORKER_SOURCE" "$CODEX_REAUTH_WORKER_SOURCE"
  require_absolute_path "SIDECAR_COOKIE_DIR" "$SIDECAR_COOKIE_DIR"
  require_absolute_path "REGISTRAR_SOURCE" "$REGISTRAR_SOURCE"
  require_absolute_path "REGISTRAR_INSTALL" "$REGISTRAR_INSTALL"
  require_absolute_path "PY_REGISTRAR_SOURCE" "$PY_REGISTRAR_SOURCE"
  [[ "$BUILD_DIR" != "/" && "$APP_DIR" != "/" && "$DATA_DIR" != "/" ]] ||
    die "BUILD_DIR, APP_DIR, and DATA_DIR must not be the filesystem root"
}

ensure_system_deps() {
  if need_system_deps; then
    install_os_packages
  fi

  command -v curl >/dev/null 2>&1 || die "curl is required"
  command -v tar >/dev/null 2>&1 || die "tar is required"
  { command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1; } || die "a C compiler is required because github.com/mattn/go-sqlite3 uses CGO"
  if python_runtime_enabled; then
    command -v python3 >/dev/null 2>&1 || die "python3 is required for Python components"
    python3 -m venv --help >/dev/null 2>&1 || die "python3 venv support is required for Python components"
  fi
}

build_project() {
  export CGO_ENABLED=1
  mkdir -p "$BUILD_DIR"

  log "Downloading Go modules"
  "$GO_BIN" mod download

  if bool_enabled "$RUN_TESTS"; then
    log "Running go test ./..."
    "$GO_BIN" test ./...
  else
    warn "Skipping go test ./..."
  fi

  RELEASE_ID="${RELEASE_ID_OVERRIDE:-$(date -u +%Y%m%dT%H%M%SZ)-$(git rev-parse --short=12 HEAD 2>/dev/null || echo source)-$$}"
  RELEASE_ID="${RELEASE_ID//[^A-Za-z0-9_.-]/-}"
  (( ${#RELEASE_ID} <= 48 )) || die "release id is too long (48 byte maximum): ${RELEASE_ID}"
  log "Building ${APP_NAME} release ${RELEASE_ID}"
  "$GO_BIN" build -trimpath -ldflags="-s -w" -o "${BUILD_DIR}/${APP_NAME}" ./cmd/pool-server
  "$GO_BIN" build -trimpath -ldflags="-s -w" -o "${BUILD_DIR}/${HANDOFF_NAME}" ./cmd/pool-handoff

  "${BUILD_DIR}/${APP_NAME}" --self-test >/dev/null
  "${BUILD_DIR}/${HANDOFF_NAME}" --self-test >/dev/null

  log "Building downloadable gateway binaries"
  mkdir -p "${BUILD_DIR}/gateway-bin"
  local target goos goarch ext
  for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
    goos="${target%/*}"
    goarch="${target#*/}"
    ext=""
    if [[ "$goos" == "windows" ]]; then
      ext=".exe"
    fi
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      "$GO_BIN" build -trimpath -ldflags="-s -w" -o "${BUILD_DIR}/gateway-bin/gateway-${goos}-${goarch}${ext}" ./cmd/gateway
  done
}

ensure_persistent_key() {
  local key_path="$1" tmp size
  if run_root test -L "$key_path"; then
    die "persistent key must not be a symlink: ${key_path}"
  fi
  if run_root test -e "$key_path"; then
    size="$(run_root stat -c %s -- "$key_path" 2>/dev/null || true)"
    [[ "$size" == "32" ]] || die "persistent key must contain exactly 32 bytes: ${key_path}"
    run_root chown "$SERVICE_USER:$SERVICE_GROUP" "$key_path"
    run_root chmod 0600 "$key_path"
    return
  fi
  tmp="$(mktemp)"
  (umask 077; dd if=/dev/urandom of="$tmp" bs=32 count=1 status=none)
  run_root install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp" "$key_path"
  rm -f "$tmp"
}

prepare_runtime_layout() {
  local config_parent database_parent persistent_root
  config_parent="$(dirname "$CONFIG_FILE")"
  database_parent="$(dirname "$DATABASE_PATH")"
  persistent_root="${DATA_DIR%/}/data"

  if ! group_exists "$SERVICE_GROUP"; then
    log "Creating group ${SERVICE_GROUP}"
    create_group
  fi

  if ! user_exists "$SERVICE_USER"; then
    log "Creating user ${SERVICE_USER}"
    create_user
  fi

  run_root install -d -m 0755 "$BIN_DIR"
  run_root install -d -m 0755 "$APP_DIR"
  run_root install -d -m 0755 "${APP_DIR}/releases"
  run_root install -d -m 0755 "${APP_DIR}/bin"
  run_root install -d -m 0755 "$CONFIG_DIR" "$config_parent"
  run_root install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$DATA_DIR" "$persistent_root"
  run_root install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_GROUP" \
    "${persistent_root}/spool" "${persistent_root}/journal" "${persistent_root}/diagnostics" \
    "${persistent_root}/tmp" "${persistent_root}/tmp/browser" "${persistent_root}/run" \
    "${persistent_root}/keys" "${persistent_root}/core-state" "${DATA_DIR}/run"
  ensure_persistent_key "${persistent_root}/keys/master.key"
  ensure_persistent_key "${persistent_root}/keys/identity.key"
  ensure_persistent_key "${persistent_root}/keys/diagnostic-alias.key"
  ensure_persistent_key "${persistent_root}/keys/core-state.key"
  run_root install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$database_parent"
  if bool_enabled "$WITH_SIDECAR"; then
    run_root install -d -m 0755 "$SIDECAR_INSTALL_DIR"
    run_root install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$SIDECAR_COOKIE_DIR"
  fi
}

install_binary_and_config() {
  local tmp existing_admin staging current_target commit_id built_at
  RELEASE_DIR="${APP_DIR%/}/releases/${RELEASE_ID}"
  staging="${APP_DIR%/}/releases/.staging-${RELEASE_ID}"
  [[ ! -e "$RELEASE_DIR" ]] || die "release already exists and is immutable: ${RELEASE_DIR}"
  current_target="$(run_root readlink "${APP_DIR%/}/current" 2>/dev/null || true)"
  PREVIOUS_RELEASE_ID="${current_target##*/}"
  [[ "$PREVIOUS_RELEASE_ID" == "$current_target" && "$current_target" != */* ]] || true

  log "Staging immutable release ${RELEASE_ID}"
  run_root rm -rf "$staging"
  run_root install -d -m 0755 "$staging" "$staging/gateway-bin" "$staging/sidecar" "$staging/super-instruct"
  run_root install -m 0755 "${BUILD_DIR}/${APP_NAME}" "$staging/${APP_NAME}"
  run_root install -m 0755 "${BUILD_DIR}/${HANDOFF_NAME}" "$staging/${HANDOFF_NAME}"
  run_root install -m 0755 "${BUILD_DIR}/gateway-bin"/gateway-* "$staging/gateway-bin/"
  if [[ -f sidecar/curl_cffi_sidecar.py ]]; then
    run_root install -m 0755 sidecar/curl_cffi_sidecar.py "$staging/sidecar/"
    run_root install -m 0644 sidecar/requirements.txt "$staging/sidecar/"
  fi
  if [[ -n "$SUPER_INSTRUCT_SOURCE" ]]; then
    run_root cp -a "${SUPER_INSTRUCT_SOURCE%/}/." "$staging/super-instruct/"
    # cp -a preserves source ownership, which may be root:root or a build user.
    # The worker runs as ${SERVICE_USER}:${SERVICE_GROUP} and the tree is later
    # tightened to o=, so the service group must own read/execute access.
    run_root chown -R "root:${SERVICE_GROUP}" "$staging/super-instruct"
    run_root chmod -R u=rwX,g=rX,o= "$staging/super-instruct"
  fi
  commit_id="$(git rev-parse HEAD 2>/dev/null || echo source)"
  built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
{"release_id":"$(json_escape "$RELEASE_ID")","commit":"$(json_escape "$commit_id")","built_at":"${built_at}","layout":"release-scoped-worker"}
EOF
  run_root install -m 0644 "$tmp" "$staging/release.json"
  rm -f "$tmp"
  run_root "$staging/${APP_NAME}" --self-test
  run_root "$staging/${HANDOFF_NAME}" --self-test
  run_root mv "$staging" "$RELEASE_DIR"

  # Compatibility entry points follow current, whose symlink changes only after the
  # staged worker has passed /readyz. The handoff process itself is never restarted on
  # an ordinary update.
  run_root ln -sfn "${APP_DIR%/}/current/${APP_NAME}" "${BIN_DIR}/${APP_NAME}"
  run_root ln -sfn "${APP_DIR%/}/current/${HANDOFF_NAME}" "${BIN_DIR}/${HANDOFF_NAME}"
  run_root install -m 0755 "${PROJECT_ROOT}/scripts/clear-context-journal.sh" "${BIN_DIR}/codex-pool-clear-context"
  run_root install -m 0755 "${PROJECT_ROOT}/scripts/rollback-release.sh" "${BIN_DIR}/codex-pool-rollback"
  # Keep the drain/reclaim helper outside immutable releases. A reaper may outlive
  # the installer and the release it is deleting. Replace it by rename so a reaper
  # already reading the previous script keeps a stable inode through the next update.
  run_root install -m 0755 "${PROJECT_ROOT}/scripts/reap-old-release.sh" "${APP_DIR%/}/bin/.${APP_NAME}-reaper.next"
  run_root mv -f "${APP_DIR%/}/bin/.${APP_NAME}-reaper.next" "${APP_DIR%/}/bin/${APP_NAME}-reaper"
  run_root install -m 0755 "${BUILD_DIR}/gateway-bin"/gateway-* "${APP_DIR}/bin/"

  if [[ -f "$CONFIG_FILE" ]]; then
    warn "Keeping existing config: ${CONFIG_FILE}"
    if [[ -z "$ADMIN_TOKEN" ]]; then
      existing_admin="$(config_string_value "$CONFIG_FILE" "admin_token" || true)"
      if [[ -n "$existing_admin" ]]; then
        ADMIN_TOKEN="$existing_admin"
      elif external_listen_enabled; then
        ADMIN_TOKEN="$(generate_admin_token)"
        warn "Existing config has an empty admin_token while LISTEN_ADDR=${LISTEN_ADDR} is externally reachable."
        warn "Generated CODEX_POOL_ADMIN_TOKEN for the service; add it to ${CONFIG_FILE} too if you run manually."
      fi
    fi
  else
    if [[ -z "$ADMIN_TOKEN" ]] && external_listen_enabled; then
      ADMIN_TOKEN="$(generate_admin_token)"
      log "Generated admin token for externally reachable frontend"
    fi
    log "Installing default config to ${CONFIG_FILE}"
    tmp="$(mktemp)"
    render_runtime_config >"$tmp"
    run_root install -m 0640 -o root -g "$SERVICE_GROUP" "$tmp" "$CONFIG_FILE"
    rm -f "$tmp"
  fi
  if [[ -n "$ADMIN_TOKEN" ]]; then
    tmp="$(mktemp)"
    (umask 077; printf '%s\n' "$ADMIN_TOKEN" >"$tmp")
    run_root install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp" "${DATA_DIR%/}/data/keys/admin.token"
    rm -f "$tmp"
  else
    run_root rm -f -- "${DATA_DIR%/}/data/keys/admin.token"
  fi
}

run_expand_only_migration() {
  local binary="${RELEASE_DIR%/}/${APP_NAME}"
  [[ -x "$binary" ]] || die "expand-only migration binary is missing: ${binary}"
  log "Applying additive expand-only storage migrations before standby startup"
  run_service env \
    CODEX_POOL_DATABASE="$DATABASE_PATH" \
    CODEX_POOL_DATA_DIR="${DATA_DIR%/}/data" \
    CODEX_POOL_MASTER_KEY_FILE="${DATA_DIR%/}/data/keys/master.key" \
    CODEX_POOL_IDENTITY_KEY_FILE="${DATA_DIR%/}/data/keys/identity.key" \
    CODEX_POOL_DIAGNOSTIC_ALIAS_KEY_FILE="${DATA_DIR%/}/data/keys/diagnostic-alias.key" \
    "$binary" --config "$CONFIG_FILE" --migrate-only --expand-only ||
    die "expand-only migration failed; the active release and traffic links were not changed"
}

systemd_requested() {
  case "$INSTALL_SYSTEMD" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    0|false|FALSE|no|NO|off|OFF) return 1 ;;
    auto)
      command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]
      return
      ;;
    *) die "INSTALL_SYSTEMD must be auto, 1, or 0" ;;
  esac
}

systemd_running() {
  command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]
}

remove_legacy_auxiliary_units() {
  local unit unit_path removed=0
  for unit in "${SERVICE_NAME}-payment.service" "${SERVICE_NAME}-register.service"; do
    unit_path="${SYSTEMD_DIR%/}/${unit}"
    if command -v systemctl >/dev/null 2>&1; then
      run_root systemctl stop "$unit" >/dev/null 2>&1 || true
      run_root systemctl disable "$unit" >/dev/null 2>&1 || true
    fi
    if [[ -e "$unit_path" || -L "$unit_path" ]]; then
      log "Removing retired auxiliary unit ${unit}"
      run_root rm -f -- "$unit_path"
      removed=1
    fi
  done
  if (( removed == 1 )) && systemd_running; then
    run_root systemctl daemon-reload
    run_root systemctl reset-failed >/dev/null 2>&1 || true
  fi
}

acquire_deploy_lock() {
  command -v flock >/dev/null 2>&1 || die "flock is required for serialized deployments"
  if [[ "${CODEX_POOL_DEPLOY_LOCK_FD:-}" =~ ^[0-9]+$ ]] &&
      [[ -e "/proc/$$/fd/${CODEX_POOL_DEPLOY_LOCK_FD}" ]]; then
    local inherited_target
    inherited_target="$(readlink -f "/proc/$$/fd/${CODEX_POOL_DEPLOY_LOCK_FD}" 2>/dev/null || true)"
    if [[ "$inherited_target" == "$(readlink -f "$DEPLOY_LOCK_FILE" 2>/dev/null || true)" ]] &&
        flock -n "$CODEX_POOL_DEPLOY_LOCK_FD"; then
      exec 9>&"$CODEX_POOL_DEPLOY_LOCK_FD"
      log "Reusing the update wrapper's deployment lock"
      return 0
    fi
    die "inherited deployment lock descriptor is invalid"
  fi
  if [[ "$(id -u)" -eq 0 ]]; then
    install -d -m 0755 "$(dirname "$DEPLOY_LOCK_FILE")"
    exec 9>"$DEPLOY_LOCK_FILE"
  else
    # Keep one host-wide inode even when sudo is used for the install operations.
    run_root install -d -m 0755 "$(dirname "$DEPLOY_LOCK_FILE")"
    run_root touch "$DEPLOY_LOCK_FILE"
    run_root chown "$(id -u):$(id -g)" "$DEPLOY_LOCK_FILE"
    exec 9>"$DEPLOY_LOCK_FILE"
  fi
  flock -n 9 || die "another codex-pool deployment is already running"
}

atomic_symlink() {
  local target="$1" link="$2" tmp
  tmp="${link}.next.$$"
  run_root rm -f "$tmp"
  run_root ln -s "$target" "$tmp"
  run_root mv -Tf "$tmp" "$link"
}

wait_for_service_health() {
  local url="$1" max="$2" phase="$3" expected_release="${4:-}"
  local started="$SECONDS" deadline=$((SECONDS + max)) next_report=$((SECONDS + 10)) state curl_error payload

  while (( SECONDS < deadline )); do
    # A host-level HTTP(S)_PROXY must never intercept a loopback health probe.
    # Capture curl's reason so an active-but-pre-listener process is distinguishable
    # from an HTTP error instead of reporting only a generic timeout.
    if payload="$(curl --noproxy '*' -fsS --connect-timeout 1 --max-time 2 "$url" 2>&1)"; then
      if [[ -z "$expected_release" || "$payload" == *"\"release_id\":\"${expected_release}\""* ]]; then
        return 0
      fi
      curl_error="healthy endpoint reported a different release"
    else
      curl_error="$payload"
    fi
    if (( SECONDS >= next_report )); then
      state="$(run_root systemctl show "${HANDOFF_SERVICE_NAME}.service" \
        --property=ActiveState --property=SubState --property=Result --property=NRestarts \
        2>/dev/null | tr '\n' ' ' || true)"
      warn "Still waiting for ${phase} health ($((SECONDS - started))s/${max}s): ${state:-service state unavailable} curl=${curl_error:-unknown failure}"
      next_report=$((next_report + 10))
    fi
    sleep 1
  done
  return 1
}

activate_codex_reauth_worker() {
  bool_enabled "$WITH_REGISTRATION" || return 0
  if run_root systemctl is-active --quiet "${SERVICE_NAME}-reauth.service" 2>/dev/null; then
    log "Codex reauth worker is active; leaving its running sessions untouched (new release staged for its next cold start)"
    if wait_for_service_health "$(codex_reauth_base_url)/healthz" "$HEALTH_TIMEOUT" "existing Codex reauth worker"; then
      return 0
    fi
    run_root systemctl --no-pager --full status "${SERVICE_NAME}-reauth.service" >&2 || true
    return 1
  fi
  log "Starting ${SERVICE_NAME}-reauth.service for release ${RELEASE_ID}"
  if ! run_root systemctl start "${SERVICE_NAME}-reauth.service" ||
    ! wait_for_service_health "$(codex_reauth_base_url)/healthz" "$HEALTH_TIMEOUT" "Codex reauth worker"; then
    run_root systemctl --no-pager --full status "${SERVICE_NAME}-reauth.service" >&2 || true
    return 1
  fi
}

restore_codex_reauth_worker_after_rollback() {
  bool_enabled "$WITH_REGISTRATION" || return 0
  if run_root test -x "${APP_DIR%/}/current/codex-reauth/codex_reauth_worker.py" &&
    run_root test -x "${APP_DIR%/}/current/registrar-python-venv/bin/python"; then
    if ! run_root systemctl is-active --quiet "${SERVICE_NAME}-reauth.service" 2>/dev/null; then
      run_root systemctl start "${SERVICE_NAME}-reauth.service" || return 1
    fi
    wait_for_service_health "$(codex_reauth_base_url)/healthz" "$HEALTH_TIMEOUT" "Codex reauth rollback"
    return $?
  fi
  run_root systemctl stop "${SERVICE_NAME}-reauth.service" >/dev/null 2>&1 || true
}

wait_worker_ready() {
  local socket="$1" expected="$2" max="$3" require_warm="${4:-1}" started="$SECONDS" payload="" unit
  local elapsed=0 next_report=0 state="unknown" substate="unknown" pid="0" restarts="0" result=""
  unit="${SERVICE_NAME}-worker@${expected}.service"
  [[ "$WORKER_START_RESTART_LIMIT" =~ ^[1-9][0-9]*$ ]] || die "WORKER_START_RESTART_LIMIT must be a positive integer: ${WORKER_START_RESTART_LIMIT}"
  while (( SECONDS - started < max )); do
    payload="$(curl --noproxy '*' --silent --show-error --max-time 2 --unix-socket "$socket" http://localhost/standbyz 2>&1 || true)"
    if [[ "$payload" == *'"standby_ready":true'* && "$payload" == *"\"release_id\":\"${expected}\""* ]]; then
      if [[ "$require_warm" == "0" ]]; then
        [[ "$payload" == *'"schema_compatible":true'* ]] && return 0
      elif [[ "$payload" == *'"warm":true'* && "$payload" == *'"schema_compatible":true'* && "$payload" == *'"write_side_effects":false'* ]]; then
        return 0
      else
        payload="${payload} (candidate did not prove warm=true, schema_compatible=true, write_side_effects=false)"
      fi
    fi

    elapsed=$((SECONDS - started))
    state="$(run_root systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
    restarts="$(run_root systemctl show "$unit" --property=NRestarts --value 2>/dev/null || true)"
    [[ "$restarts" =~ ^[0-9]+$ ]] || restarts=0
    if [[ "$state" == "failed" || ( "$state" == "inactive" && "$elapsed" -ge 3 ) ]]; then
      warn "Staged worker stopped before /standbyz became ready: unit=${unit} state=${state} elapsed=${elapsed}s"
      return 1
    fi
    if (( restarts >= WORKER_START_RESTART_LIMIT )); then
      warn "Staged worker restarted ${restarts} times before /standbyz became ready; stopping the crash loop"
      return 1
    fi
    if (( elapsed >= next_report )); then
      substate="$(run_root systemctl show "$unit" --property=SubState --value 2>/dev/null || true)"
      pid="$(run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
      result="$(run_root systemctl show "$unit" --property=Result --value 2>/dev/null || true)"
      payload="${payload//$'\n'/ }"
      warn "Waiting for staged worker: elapsed=${elapsed}s state=${state:-unknown}/${substate:-unknown} pid=${pid:-0} restarts=${restarts} result=${result:-unknown} standby=${payload:0:180}"
      next_report=$((elapsed + 10))
    fi
    sleep 1
  done
  return 1
}

WORKER_FAILURE_REPORT=""

capture_worker_startup_failure() {
  local release="$1" started_epoch="$2" unit socket failure_dir failure_file tmp pid
  unit="${SERVICE_NAME}-worker@${release}.service"
  socket="${DATA_DIR%/}/run/worker-${release}.sock"
  failure_dir="${DATA_DIR%/}/deploy-failures"
  failure_file="${failure_dir%/}/worker-${release}.log"
  tmp="$(mktemp)"
  pid="$(run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  {
    printf 'captured_at=%s\nrelease=%s\nunit=%s\nsocket=%s\n' "$(date -u +%FT%TZ)" "$release" "$unit" "$socket"
    printf '\n[systemctl-show]\n'
    run_root systemctl show "$unit" \
      --property=ActiveState,SubState,Result,MainPID,ExecMainPID,ExecMainCode,ExecMainStatus,NRestarts,MemoryCurrent,MemoryPeak,CPUUsageNSec 2>&1 || true
    printf '\n[systemctl-status]\n'
    run_root systemctl --no-pager --full status "$unit" 2>&1 || true
    printf '\n[unit-journal]\n'
    run_root journalctl --no-pager --output=short-precise -u "$unit" --since "@${started_epoch}" -n 400 2>&1 || true
    printf '\n[kernel-oom]\n'
    run_root journalctl --no-pager --output=short-precise -k --since "@${started_epoch}" 2>&1 \
      | grep -Ei 'out of memory|oom-kill|killed process|memory cgroup out of memory' || true
    printf '\n[process]\n'
    if [[ "$pid" =~ ^[1-9][0-9]*$ && -d "${PROC_ROOT%/}/${pid}" ]]; then
      run_root cat "${PROC_ROOT%/}/${pid}/status" 2>&1 || true
      run_root cat "${PROC_ROOT%/}/${pid}/cgroup" 2>&1 || true
    else
      printf 'MainPID is not alive: %s\n' "${pid:-0}"
    fi
    printf '\n[resources]\n'
    run_root free -h 2>&1 || true
    run_root df -h "$DATA_DIR" "$APP_DIR" 2>&1 || true
    run_root ls -l "$socket" 2>&1 || true
    printf '\n[sqlite-locks]\n'
    emit_sqlite_lock_evidence 2>&1 || true
  } >"$tmp" 2>&1

  if run_root install -d -m 0750 "$failure_dir" && run_root install -m 0640 "$tmp" "$failure_file"; then
    run_root chown "root:${SERVICE_GROUP}" "$failure_file" 2>/dev/null || true
    WORKER_FAILURE_REPORT="$failure_file"
    warn "Staged worker failure report saved to ${failure_file}"
    run_root tail -n 220 "$failure_file" >&2 || true
  else
    warn "Could not persist staged worker failure report; printing the temporary capture"
    tail -n 220 "$tmp" >&2 || true
  fi
  rm -f "$tmp"
}

handoff_public_base_url() {
  local port host
  port="$(listen_port "$LISTEN_ADDR")"
  host="$(listen_host "$LISTEN_ADDR")"
  case "$host" in ""|"0.0.0.0"|"::") host="127.0.0.1" ;; esac
  case "$host" in *:*) host="[${host}]" ;; esac
  printf 'http://%s:%s' "$host" "$port"
}

handoff_control_get() {
  curl --noproxy '*' --silent --show-error --max-time 2 \
    --unix-socket "$HANDOFF_CONTROL_SOCKET" http://localhost/handoffz 2>/dev/null
}

handoff_control_post() {
  local action="$1" url suffix=""
  url="http://localhost/_codex_pool/handoff/${action}"
  if [[ "$action" == "pause" ]]; then
    suffix="?reason=install&release=${RELEASE_ID}"
    url="${url}${suffix}"
  fi
  if curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
      --unix-socket "$HANDOFF_CONTROL_SOCKET" "$url" 2>/dev/null; then
    return 0
  fi
  # Compatibility fallback for a handoff started without its dedicated Unix control
  # socket. The public endpoint itself accepts control only from a loopback peer.
  curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
    "$(handoff_public_base_url)/_codex_pool/handoff/${action}${suffix}" 2>/dev/null
}

handoff_control_is_available() {
  local payload
  payload="$(handoff_control_get 2>/dev/null || true)"
  [[ "$payload" == *'"deployment_state":"handoff"'* ]]
}

handoff_control_is_ready() {
  local payload
  payload="$(handoff_control_get 2>/dev/null || true)"
  [[ "$payload" == *'"deployment_state":"handoff"'* && "$payload" == *'"ready":true'* ]]
}

handoff_is_ready() {
  local expected_instance="${1:-}" payload
  payload="$(curl --noproxy '*' --silent --max-time 2 "$(handoff_public_base_url)/handoffz" 2>/dev/null || true)"
  [[ "$payload" == *'"deployment_state":"handoff"'* && "$payload" == *'"ready":true'* ]] || return 1
  [[ -z "$expected_instance" || "$payload" == *"\"instance_id\":\"${expected_instance}\""* ]]
}

wait_handoff_control_ready() {
  local started="$SECONDS"
  while (( SECONDS - started < HEALTH_TIMEOUT )); do
    handoff_control_is_ready && return 0
    sleep 1
  done
  return 1
}

wait_handoff_public_instance() {
  local expected="$1" started="$SECONDS"
  while (( SECONDS - started < HEALTH_TIMEOUT )); do
    handoff_is_ready "$expected" && return 0
    sleep 1
  done
  return 1
}

pause_handoff_admission() {
  local payload
  payload="$(handoff_control_post pause 2>/dev/null || true)"
  [[ "$payload" == *'"admission_paused":true'* ]] || return 1
  HANDOFF_PAUSED=1
  HANDOFF_PAUSE_STARTED_MS="$(date +%s%3N 2>/dev/null || printf '%s000' "$(date +%s)")"
  log "New request admission is paused; established HTTP/SSE/WebSocket connections continue"
}

adopt_interrupted_install_pause() {
  local payload
  command -v curl >/dev/null 2>&1 || return 0
  payload="$(handoff_control_get 2>/dev/null || true)"
  if [[ "$payload" == *'"admission_paused":true'* && "$payload" == *'"pause_reason":"install"'* ]]; then
    HANDOFF_PAUSED=1
    warn "Resuming an installer that previously stopped while new-request admission was paused"
  elif payload="$(run_root cat "$HANDOFF_PAUSE_STATE" 2>/dev/null || true)" && \
      [[ "$payload" == *'"reason":"install"'* ]]; then
    HANDOFF_PAUSED=1
    warn "Recovering an armed admission pause left by an interrupted installer"
  fi
}

arm_initial_handoff_pause() {
  local tmp
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
{"paused_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)","reason":"install","release":"${RELEASE_ID}"}
EOF
  run_root install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp" "$HANDOFF_PAUSE_STATE"
  rm -f "$tmp"
  HANDOFF_PAUSED=1
  HANDOFF_PAUSE_STARTED_MS="$(date +%s%3N 2>/dev/null || printf '%s000' "$(date +%s)")"
}

resume_handoff_admission() {
  local payload
  (( HANDOFF_PAUSED == 1 )) || return 0
  payload="$(handoff_control_post resume 2>/dev/null || true)"
  if [[ "$payload" == *'"admission_paused":false'* ]]; then
    local resumed_ms
    resumed_ms="$(date +%s%3N 2>/dev/null || printf '%s000' "$(date +%s)")"
    if [[ "$HANDOFF_PAUSE_STARTED_MS" =~ ^[0-9]+$ && "$resumed_ms" =~ ^[0-9]+$ && "$resumed_ms" -ge "$HANDOFF_PAUSE_STARTED_MS" ]]; then
      DEPLOY_ADMISSION_PAUSE_DURATION_MS=$((resumed_ms - HANDOFF_PAUSE_STARTED_MS))
    fi
    HANDOFF_PAUSED=0
    log "New request admission resumed on release ${RELEASE_ID}"
    return 0
  fi
  # If the handoff never started, remove the armed state so a later socket activation
  # does not inherit a stale deployment barrier.
  if ! run_root systemctl is-active --quiet "${HANDOFF_SERVICE_NAME}.service" 2>/dev/null; then
    run_root rm -f "$HANDOFF_PAUSE_STATE"
    HANDOFF_PAUSED=0
    return 0
  fi
  return 1
}

valid_worker_release_id() {
  [[ "$1" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]]
}

human_bytes() {
  awk -v bytes="${1:-0}" 'BEGIN {
    split("B KiB MiB GiB TiB", units, " "); unit = 1
    while (bytes >= 1024 && unit < 5) { bytes /= 1024; unit++ }
    if (unit == 1) printf "%d %s", bytes, units[unit]
    else printf "%.1f %s", bytes, units[unit]
  }'
}

validate_deployment_limits() {
  local name value
  for name in MAX_DRAINING_RELEASES INSTALL_FREE_RESERVE_MIN_BYTES \
    INSTALL_FREE_RESERVE_PERCENT RELEASE_STORAGE_MAX_BYTES \
    CONSOLE_GENERATION_MAX_BYTES BUILD_CACHE_MAX_BYTES \
    CONSOLE_GENERATION_KEEP CONSOLE_GENERATION_MAX_AGE_SECONDS; do
    value="${!name}"
    [[ "$value" =~ ^[0-9]+$ ]] || die "${name} must be a non-negative integer: ${value}"
  done
  (( MAX_DRAINING_RELEASES >= 1 )) || die "MAX_DRAINING_RELEASES must be at least 1"
  (( INSTALL_FREE_RESERVE_PERCENT <= 90 )) || die "INSTALL_FREE_RESERVE_PERCENT must be between 0 and 90"
  (( CONSOLE_GENERATION_KEEP >= 1 )) || die "CONSOLE_GENERATION_KEEP must be at least 1"
}

directory_bytes() {
  local path="$1" size
  [[ -e "$path" ]] || { printf '0\n'; return 0; }
  size="$(run_root du -sb -- "$path" 2>/dev/null | awk 'NR==1 {print $1}' || true)"
  [[ "$size" =~ ^[0-9]+$ ]] || size=0
  printf '%s\n' "$size"
}

file_bytes() {
  local size
  size="$(run_root stat -c %s -- "$1" 2>/dev/null || true)"
  [[ "$size" =~ ^[0-9]+$ ]] || size=0
  printf '%s\n' "$size"
}

filesystem_probe_path() {
  local path="$1"
  while [[ ! -e "$path" && "$path" != "/" ]]; do
    path="$(dirname "$path")"
  done
  [[ -e "$path" ]] || path="/"
  printf '%s\n' "$path"
}

filesystem_total_free() {
  local probe="$1"
  run_root df -Pk "$probe" 2>/dev/null | awk 'END {printf "%.0f %.0f\n", $2 * 1024, $4 * 1024}'
}

release_process_ids() {
  local release_dir="$1" exe target pid first=1
  for exe in "${PROC_ROOT%/}"/[0-9]*/exe; do
    [[ -L "$exe" ]] || continue
    target="$(readlink "$exe" 2>/dev/null || true)"
    target="${target% (deleted)}"
    [[ "$target" == "${release_dir%/}/"* ]] || continue
    pid="${exe%/exe}"
    pid="${pid##*/}"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || continue
    (( first == 1 )) || printf ','
    printf '%s' "$pid"
    first=0
  done
  printf '\n'
}

json_integer_field() {
  local payload="$1" field="$2" value
  value="$(printf '%s' "$payload" | sed -n "s/.*\"${field}\"[[:space:]]*:[[:space:]]*\(-\{0,1\}[0-9][0-9]*\).*/\1/p" | head -1)"
  [[ "$value" =~ ^-?[0-9]+$ ]] || return 1
  printf '%s\n' "$value"
}

json_string_field() {
  local payload="$1" field="$2"
  printf '%s' "$payload" | sed -n "s/.*\"${field}\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
}

collect_deployment_storage() {
  local phase="${1:-status}" current_target releases_root path release bytes age mtime now
  local pids first_pid unit_pid socket payload critical resumable state heartbeat error state_file
  local fs_probe fs_total fs_free reserve_percent automatic_budget hard_budget effective_budget
  local source_estimate candidate_base backup_peak database_live_bytes console_estimate predicted_extra
  local draining_json="" details="" is_draining=0

  releases_root="${APP_DIR%/}/releases"
  current_target=""
  if run_root test -L "${APP_DIR%/}/current"; then
    current_target="$(run_root readlink -f "${APP_DIR%/}/current" 2>/dev/null || true)"
  fi
  DEPLOY_CURRENT_RELEASE="${current_target##*/}"
  [[ -n "$current_target" ]] || DEPLOY_CURRENT_RELEASE=""
  DEPLOY_TOTAL_RELEASE_BYTES="$(directory_bytes "$releases_root")"
  DEPLOY_CONSOLE_BYTES="$(directory_bytes "${DATA_DIR%/}/console-generations")"
  DEPLOY_BACKUP_BYTES="$(directory_bytes "${BACKUP_DIR:-$(dirname "$DATABASE_PATH")/backups}")"
  DEPLOY_DRAINING_COUNT=0
  DEPLOY_DRAINING_JSON=""
  DEPLOY_DRAINING_DETAILS=""
  now="$(date +%s)"

  if [[ -d "$releases_root" ]]; then
    while IFS= read -r path; do
      [[ -n "$path" && "$path" != "$current_target" ]] || continue
      release="${path##*/}"
      [[ "$release" != .staging-* ]] || continue
      valid_worker_release_id "$release" || continue
      bytes="$(directory_bytes "$path")"
      mtime="$(run_root stat -c %Y -- "$path" 2>/dev/null || printf '%s' "$now")"
      [[ "$mtime" =~ ^[0-9]+$ ]] || mtime="$now"
      age=$(( now - mtime )); (( age >= 0 )) || age=0
      pids="$(release_process_ids "$path")"
      first_pid="${pids%%,*}"
      [[ "$first_pid" =~ ^[1-9][0-9]*$ ]] || first_pid=0
      unit_pid=0
      if command -v systemctl >/dev/null 2>&1; then
        unit_pid="$(run_root systemctl show "${SERVICE_NAME}-worker@${release}.service" --property=MainPID --value 2>/dev/null || true)"
        [[ "$unit_pid" =~ ^[1-9][0-9]*$ ]] || unit_pid=0
      fi
      if (( first_pid == 0 && unit_pid > 0 )); then first_pid="$unit_pid"; pids="$unit_pid"; fi
      socket="${DATA_DIR%/}/run/worker-${release}.sock"
      payload="$(curl --noproxy '*' --silent --max-time 1 --unix-socket "$socket" http://localhost/readyz 2>/dev/null || true)"
      critical="$(json_integer_field "$payload" critical_inflight 2>/dev/null || true)"
      resumable="$(json_integer_field "$payload" resumable_inflight 2>/dev/null || true)"
      [[ "$critical" =~ ^[0-9]+$ ]] || critical=-1
      [[ "$resumable" =~ ^[0-9]+$ ]] || resumable=-1
      state_file="${DATA_DIR%/}/run/reapers/${release}.json"
      state=""; heartbeat=0; error=""
      if [[ -r "$state_file" ]]; then
        payload="$(run_root cat "$state_file" 2>/dev/null || true)"
        state="$(json_string_field "$payload" state || true)"
        heartbeat="$(json_integer_field "$payload" heartbeat_at 2>/dev/null || true)"
        error="$(json_string_field "$payload" last_error || true)"
      fi
      [[ "$heartbeat" =~ ^[0-9]+$ ]] || heartbeat=0
      [[ -n "$state" ]] || { if (( first_pid > 0 )); then state="draining"; else state="orphaned"; fi; }
      is_draining=0
      if (( first_pid > 0 || unit_pid > 0 || critical > 0 || resumable > 0 )); then
        is_draining=1
      fi
      case "$state" in starting|waiting|draining|stopping) is_draining=1 ;; esac
      (( is_draining == 0 )) || DEPLOY_DRAINING_COUNT=$((DEPLOY_DRAINING_COUNT + 1))
      [[ -z "$draining_json" ]] || draining_json+=" ,"
      draining_json+="{\"release_id\":\"$(json_escape "$release")\",\"pid\":${first_pid},\"bytes\":${bytes},\"age_seconds\":${age},\"critical_inflight\":${critical},\"resumable_inflight\":${resumable},\"state\":\"$(json_escape "$state")\",\"heartbeat_at\":${heartbeat},\"last_error\":\"$(json_escape "$error")\"}"
      details+=$'\n'
      details+="  - ${release}: size=$(human_bytes "$bytes"), age=${age}s, pid=${pids:-none}, critical=${critical}, resumable=${resumable}, reaper=${state}, error=${error:-none}"
    done < <(run_root find "$releases_root" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort)
  fi
  DEPLOY_DRAINING_JSON="$draining_json"
  DEPLOY_DRAINING_DETAILS="$details"

  fs_probe="$(filesystem_probe_path "$APP_DIR")"
  read -r fs_total fs_free < <(filesystem_total_free "$fs_probe")
  [[ "$fs_total" =~ ^[0-9]+$ && "$fs_free" =~ ^[0-9]+$ ]] || die "could not calculate filesystem capacity for ${fs_probe}"
  DEPLOY_FREE_BYTES="$fs_free"
  reserve_percent=$(( fs_total * INSTALL_FREE_RESERVE_PERCENT / 100 ))
  DEPLOY_RESERVE_BYTES="$INSTALL_FREE_RESERVE_MIN_BYTES"
  (( reserve_percent <= DEPLOY_RESERVE_BYTES )) || DEPLOY_RESERVE_BYTES="$reserve_percent"

  source_estimate="$(directory_bytes "$BUILD_DIR")"
  if (( source_estimate == 0 )) && [[ -n "$current_target" ]]; then
    source_estimate="$(directory_bytes "$current_target")"
  fi
  # Fresh installs have no previous immutable release from which to estimate the
  # stripped binaries. Keep a conservative floor until the post-build check can use
  # the exact staged output size.
  (( source_estimate >= 134217728 )) || source_estimate=134217728
  candidate_base="$source_estimate"
  if [[ -n "$current_target" ]]; then
    bytes="$(directory_bytes "$current_target")"
    (( bytes <= candidate_base )) || candidate_base="$bytes"
  fi
  if [[ -n "$RELEASE_DIR" && -d "$RELEASE_DIR" ]]; then
    bytes="$(directory_bytes "$RELEASE_DIR")"
    (( bytes <= candidate_base )) || candidate_base="$bytes"
  fi
  DEPLOY_CANDIDATE_ESTIMATE_BYTES=$(( (candidate_base * 125 + 99) / 100 ))
  automatic_budget=$(( DEPLOY_CANDIDATE_ESTIMATE_BYTES * 3 ))
  (( automatic_budget >= 1073741824 )) || automatic_budget=1073741824
  database_live_bytes=$(( $(file_bytes "$DATABASE_PATH") + $(file_bytes "${DATABASE_PATH}-wal") ))
  backup_peak=$(( database_live_bytes * 13 / 10 + 1048576 ))
  hard_budget=$(( fs_total - DEPLOY_RESERVE_BYTES - backup_peak ))
  (( hard_budget > 0 )) || hard_budget=0
  if (( RELEASE_STORAGE_MAX_BYTES > 0 )); then
    effective_budget="$RELEASE_STORAGE_MAX_BYTES"
  else
    effective_budget="$automatic_budget"
  fi
  (( effective_budget <= hard_budget )) || effective_budget="$hard_budget"
  DEPLOY_RELEASE_BUDGET_BYTES="$effective_budget"

  console_estimate="$(directory_bytes "${PROJECT_ROOT}/internal/console/dist")"
  (( console_estimate >= 16777216 )) || console_estimate=16777216
  (( console_estimate <= CONSOLE_GENERATION_MAX_BYTES )) || console_estimate="$CONSOLE_GENERATION_MAX_BYTES"
  case "$phase" in
    prebuild)
      predicted_extra=$(( DEPLOY_CANDIDATE_ESTIMATE_BYTES * 2 + backup_peak + console_estimate ))
      ;;
    poststage)
      predicted_extra=$(( backup_peak + console_estimate ))
      ;;
    *) predicted_extra=0 ;;
  esac
  DEPLOY_PREDICTED_PEAK_BYTES=$(( DEPLOY_TOTAL_RELEASE_BYTES + predicted_extra ))
}

write_deployment_storage_state() {
  local state_dir state_file tmp now
  state_dir="${DATA_DIR%/}/run"
  run_root install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$state_dir"
  state_file="${state_dir}/deployment-storage.json"
  now="$(date +%s)"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
{"current_release":"$(json_escape "$DEPLOY_CURRENT_RELEASE")","total_release_bytes":${DEPLOY_TOTAL_RELEASE_BYTES},"release_budget_bytes":${DEPLOY_RELEASE_BUDGET_BYTES},"free_bytes":${DEPLOY_FREE_BYTES},"free_reserve_bytes":${DEPLOY_RESERVE_BYTES},"predicted_peak_bytes":${DEPLOY_PREDICTED_PEAK_BYTES},"backup_bytes":${DEPLOY_BACKUP_BYTES},"console_generation_bytes":${DEPLOY_CONSOLE_BYTES},"admission_pause_duration_ms":${DEPLOY_ADMISSION_PAUSE_DURATION_MS},"draining":[${DEPLOY_DRAINING_JSON}],"last_reclaim_error":"$(json_escape "$DEPLOY_LAST_RECLAIM_ERROR")","updated_at":${now}}
EOF
  run_root install -m 0640 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp" "${state_file}.next"
  run_root mv -f "${state_file}.next" "$state_file"
  rm -f "$tmp"
}

print_deployment_storage_status() {
  log "Deployment storage: current=${DEPLOY_CURRENT_RELEASE:-none}, releases=$(human_bytes "$DEPLOY_TOTAL_RELEASE_BYTES")/$(human_bytes "$DEPLOY_RELEASE_BUDGET_BYTES"), free=$(human_bytes "$DEPLOY_FREE_BYTES"), reserve=$(human_bytes "$DEPLOY_RESERVE_BYTES"), predicted-peak=$(human_bytes "$DEPLOY_PREDICTED_PEAK_BYTES")"
  log "Deployment storage: draining=${DEPLOY_DRAINING_COUNT}/${MAX_DRAINING_RELEASES}, backups=$(human_bytes "$DEPLOY_BACKUP_BYTES"), console-generations=$(human_bytes "$DEPLOY_CONSOLE_BYTES")/$(human_bytes "$CONSOLE_GENERATION_MAX_BYTES")"
  if [[ -n "$DEPLOY_DRAINING_DETAILS" ]]; then
    printf 'Draining/reclaimable releases:%s\n' "$DEPLOY_DRAINING_DETAILS"
  fi
}

deployment_budget_failure() {
  local reason="$1"
  print_deployment_storage_status >&2
  printf 'ERROR: deployment storage preflight failed: %s\n' "$reason" >&2
  if [[ -n "$DEPLOY_DRAINING_DETAILS" ]]; then
    printf 'Protected release diagnostics:%s\n' "$DEPLOY_DRAINING_DETAILS" >&2
  fi
  printf 'Suggested action: let critical streams finish, close stale consoles, inspect reaper state, or raise an explicit budget only after adding disk capacity. Live releases are never force-deleted.\n' >&2
  return 1
}

enforce_deployment_budget() {
  local phase="$1" projected_release_bytes free_after
  collect_deployment_storage "$phase"
  print_deployment_storage_status
  if [[ "$phase" == "prebuild" ]] && (( DEPLOY_DRAINING_COUNT >= MAX_DRAINING_RELEASES )); then
    deployment_budget_failure "${DEPLOY_DRAINING_COUNT} releases are still draining; limit is ${MAX_DRAINING_RELEASES}" || return 1
  fi
  projected_release_bytes="$DEPLOY_TOTAL_RELEASE_BYTES"
  if [[ "$phase" == "prebuild" ]]; then
    projected_release_bytes=$(( projected_release_bytes + DEPLOY_CANDIDATE_ESTIMATE_BYTES ))
  fi
  if (( DEPLOY_RELEASE_BUDGET_BYTES <= 0 || projected_release_bytes > DEPLOY_RELEASE_BUDGET_BYTES )); then
    deployment_budget_failure "projected release storage $(human_bytes "$projected_release_bytes") exceeds effective budget $(human_bytes "$DEPLOY_RELEASE_BUDGET_BYTES")" || return 1
  fi
  free_after="$DEPLOY_FREE_BYTES"
  if [[ "$phase" == "prebuild" ]]; then
    free_after=$(( DEPLOY_FREE_BYTES - (DEPLOY_PREDICTED_PEAK_BYTES - DEPLOY_TOTAL_RELEASE_BYTES) ))
  else
    # At the post-stage gate the candidate and build tree already occupy disk; only
    # the remaining backup/snapshot peak needs to fit above the reserve.
    free_after=$(( DEPLOY_FREE_BYTES - $(file_bytes "$DATABASE_PATH") - 16777216 ))
  fi
  if (( free_after < DEPLOY_RESERVE_BYTES )); then
    deployment_budget_failure "predicted free space $(human_bytes "$free_after") would fall below reserve $(human_bytes "$DEPLOY_RESERVE_BYTES")" || return 1
  fi
  if user_exists "$SERVICE_USER"; then
    write_deployment_storage_state
  fi
}

cleanup_abandoned_deployment_artifacts() {
  local path release active_socket socket payload unit_pid current_target previous_target
  if [[ -d "${APP_DIR%/}/releases" ]]; then
    while IFS= read -r path; do
      [[ -n "$path" ]] || continue
      release="${path##*/.staging-}"
      valid_worker_release_id "$release" || continue
      if [[ -n "$(release_process_ids "$path")" ]]; then
        warn "Preserving interrupted staging still used by a process: ${path}"
        continue
      fi
      log "Removing abandoned release staging ${path}"
      run_root rm -rf -- "$path"
    done < <(run_root find "${APP_DIR%/}/releases" -mindepth 1 -maxdepth 1 -type d -name '.staging-*' -print 2>/dev/null || true)

    current_target="$(run_root readlink -f "${APP_DIR%/}/current" 2>/dev/null || true)"
    previous_target="$(run_root readlink -f "${APP_DIR%/}/previous" 2>/dev/null || true)"
    while IFS= read -r path; do
      [[ -n "$path" && "$path" != "$current_target" ]] || continue
      release="${path##*/}"
      [[ "$release" != .staging-* ]] || continue
      valid_worker_release_id "$release" || continue
      [[ -z "$(release_process_ids "$path")" ]] || continue
      unit_pid="$(run_root systemctl show "${SERVICE_NAME}-worker@${release}.service" --property=MainPID --value 2>/dev/null || true)"
      [[ ! "$unit_pid" =~ ^[1-9][0-9]*$ ]] || continue
      socket="${DATA_DIR%/}/run/worker-${release}.sock"
      payload="$(curl --noproxy '*' --silent --max-time 1 --unix-socket "$socket" http://localhost/readyz 2>/dev/null || true)"
      [[ -z "$payload" ]] || continue
      log "Reclaiming inactive release with no process, worker, or request owner: ${release}"
      run_root systemctl disable "${SERVICE_NAME}-worker@${release}.service" >/dev/null 2>&1 || true
      run_root systemctl disable "${SERVICE_NAME}-reaper@${release}.service" >/dev/null 2>&1 || true
      run_root rm -f -- "$socket"
      if [[ "$previous_target" == "$path" ]]; then
        run_root rm -f -- "${APP_DIR%/}/previous"
      fi
      run_root rm -rf -- "$path"
      run_root rm -f -- "${DATA_DIR%/}/run/reapers/${release}.json"
    done < <(run_root find "${APP_DIR%/}/releases" -mindepth 1 -maxdepth 1 -type d ! -name '.staging-*' -print 2>/dev/null | sort)
  fi
  active_socket="$(run_root readlink "${DATA_DIR%/}/run/active-worker.sock" 2>/dev/null || true)"
  if [[ -d "${DATA_DIR%/}/run" ]]; then
    while IFS= read -r socket; do
      [[ -n "$socket" && "$socket" != "$active_socket" ]] || continue
      release="${socket##*/worker-}"; release="${release%.sock}"
      valid_worker_release_id "$release" || continue
      unit_pid="$(run_root systemctl show "${SERVICE_NAME}-worker@${release}.service" --property=MainPID --value 2>/dev/null || true)"
      [[ "$unit_pid" =~ ^[1-9][0-9]*$ ]] && continue
      payload="$(curl --noproxy '*' --silent --max-time 1 --unix-socket "$socket" http://localhost/livez 2>/dev/null || true)"
      [[ -z "$payload" ]] || continue
      log "Removing orphan worker socket ${socket}"
      run_root rm -f -- "$socket"
    done < <(run_root find "${DATA_DIR%/}/run" -maxdepth 1 \( -type s -o -type l \) -name 'worker-*.sock' -print 2>/dev/null || true)
  fi
  if [[ -d "${DATA_DIR%/}/run/reapers" ]]; then
    run_root find "${DATA_DIR%/}/run/reapers" -maxdepth 1 -type f \
      \( -name '*.json.next.*' -o -name '*.json' -mtime +1 \) -delete 2>/dev/null || true
  fi
}

prune_console_generations() {
  local protected="${1:-}" root="${DATA_DIR%/}/console-generations" now path name mtime age total
  local -a generations=()
  [[ -d "$root" ]] || return 0
  now="$(date +%s)"
  while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    name="${path##*/}"
    if [[ "$name" == .staging-* ]]; then
      log "Removing abandoned console generation staging ${path}"
      run_root rm -rf -- "$path"
      continue
    fi
    generations+=("$path")
  done < <(run_root find "$root" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort)

  for path in "${generations[@]}"; do
    name="${path##*/}"
    [[ "$name" != "$protected" ]] || continue
    mtime="$(run_root stat -c %Y -- "$path" 2>/dev/null || printf '%s' "$now")"
    [[ "$mtime" =~ ^[0-9]+$ ]] || mtime="$now"
    age=$(( now - mtime ))
    if (( age > CONSOLE_GENERATION_MAX_AGE_SECONDS )); then
      log "Pruning expired console generation ${name} (${age}s old)"
      run_root rm -rf -- "$path"
    fi
  done

  mapfile -t generations < <(
    run_root find "$root" -mindepth 1 -maxdepth 1 -type d ! -name '.staging-*' \
      -printf '%T@ %p\n' 2>/dev/null | sort -nr | cut -d' ' -f2-
  )
  while (( ${#generations[@]} > CONSOLE_GENERATION_KEEP )); do
    path="${generations[${#generations[@]}-1]}"
    name="${path##*/}"
    if [[ "$name" == "$protected" ]]; then
      # The protected snapshot is normally newest; if clock skew made it oldest,
      # discard the next-oldest generation instead.
      if (( ${#generations[@]} <= 1 )); then break; fi
      path="${generations[${#generations[@]}-2]}"
      unset "generations[$((${#generations[@]} - 2))]"
      generations=("${generations[@]}")
    else
      unset "generations[$((${#generations[@]} - 1))]"
    fi
    log "Pruning excess console generation ${path##*/}"
    run_root rm -rf -- "$path"
    generations=("${generations[@]}")
  done

  total="$(directory_bytes "$root")"
  while (( total > CONSOLE_GENERATION_MAX_BYTES && ${#generations[@]} > 0 )); do
    path="${generations[${#generations[@]}-1]}"
    name="${path##*/}"
    if [[ "$name" == "$protected" ]]; then
      if (( ${#generations[@]} == 1 )); then
        warn "Protected console generation ${protected} alone exceeds CONSOLE_GENERATION_MAX_BYTES"
        return 1
      fi
      path="${generations[${#generations[@]}-2]}"
      unset "generations[$((${#generations[@]} - 2))]"
    else
      unset "generations[$((${#generations[@]} - 1))]"
    fi
    log "Pruning console generation ${path##*/} to honor byte budget"
    run_root rm -rf -- "$path"
    generations=("${generations[@]}")
    total="$(directory_bytes "$root")"
  done
}

console_asset_path_valid() {
  local path="$1" rel
  [[ "$path" == /console/assets/* ]] || return 1
  rel="${path#/console/}"
  [[ "$rel" =~ ^assets/[A-Za-z0-9._/-]+$ ]] || return 1
  [[ "/$rel/" != *"/../"* && "/$rel/" != *"/./"* && "$rel" != */ ]] || return 1
}

console_asset_mime_valid() {
  local path="$1" content_type="${2%%;*}" extension
  content_type="${content_type,,}"
  extension="${path##*.}"; extension="${extension,,}"
  case "$extension" in
    js|mjs) [[ "$content_type" == "text/javascript" || "$content_type" == "application/javascript" || "$content_type" == "application/octet-stream" ]] ;;
    css) [[ "$content_type" == "text/css" ]] ;;
    json|map) [[ "$content_type" == "application/json" || "$content_type" == "application/octet-stream" ]] ;;
    wasm) [[ "$content_type" == "application/wasm" || "$content_type" == "application/octet-stream" ]] ;;
    svg) [[ "$content_type" == "image/svg+xml" ]] ;;
    png) [[ "$content_type" == "image/png" ]] ;;
    jpg|jpeg) [[ "$content_type" == "image/jpeg" ]] ;;
    webp) [[ "$content_type" == "image/webp" ]] ;;
    gif) [[ "$content_type" == "image/gif" ]] ;;
    ico) [[ "$content_type" == "image/x-icon" || "$content_type" == "image/vnd.microsoft.icon" ]] ;;
    woff) [[ "$content_type" == "font/woff" || "$content_type" == "application/font-woff" || "$content_type" == "application/octet-stream" ]] ;;
    woff2) [[ "$content_type" == "font/woff2" || "$content_type" == "application/octet-stream" ]] ;;
    ttf) [[ "$content_type" == "font/ttf" || "$content_type" == "application/x-font-ttf" || "$content_type" == "application/octet-stream" ]] ;;
    *) [[ -n "$content_type" && "$content_type" != "text/html" ]] ;;
  esac
}

fetch_old_console_path() {
  local socket="$1" path="$2" output="$3" headers="${4:-}"
  local -a args=(--noproxy '*' --fail --silent --show-error --max-time 20 --header 'Accept-Encoding: identity')
  [[ -z "$headers" ]] || args+=(--dump-header "$headers")
  if [[ -n "$socket" && ( -S "$socket" || -L "$socket" ) ]]; then
    if curl "${args[@]}" --unix-socket "$socket" "http://localhost${path}" --output "$output"; then
      return 0
    fi
  fi
  curl "${args[@]}" "$(handoff_public_base_url)${path}" --output "$output"
}

download_console_asset() {
  local socket="$1" path="$2" staging="$3" expected_size="${4:-}" expected_sha="${5:-}"
  local rel target headers actual_size actual_sha content_type
  console_asset_path_valid "$path" || die "old console manifest contains an unsafe path: ${path}"
  rel="${path#/console/}"
  target="${staging%/}/${rel}"
  headers="${target}.headers"
  run_root install -d -m 0755 "$(dirname "$target")"
  fetch_old_console_path "$socket" "$path" "$target" "$headers" || return 1
  actual_size="$(file_bytes "$target")"
  if [[ -n "$expected_size" && "$actual_size" != "$expected_size" ]]; then
    warn "old console asset size mismatch: ${path} expected=${expected_size} actual=${actual_size}"
    return 1
  fi
  actual_sha="$(sha256sum "$target" | awk '{print $1}')"
  if [[ -n "$expected_sha" && "$actual_sha" != "$expected_sha" ]]; then
    warn "old console asset digest mismatch: ${path}"
    return 1
  fi
  content_type="$(tr -d '\r' <"$headers" | awk 'BEGIN{IGNORECASE=1} /^Content-Type:/ {sub(/^[^:]*:[[:space:]]*/, ""); value=$0} END{print value}')"
  rm -f "$headers"
  case "$content_type" in
    text/html*) warn "old console asset unexpectedly returned HTML: ${path}"; return 1 ;;
  esac
  if ! console_asset_mime_valid "$path" "$content_type"; then
    warn "old console asset MIME mismatch: ${path} content-type=${content_type:-missing}"
    return 1
  fi
  [[ -s "$target" ]] || { warn "old console asset is empty: ${path}"; return 1; }
  return 0
}

snapshot_legacy_console_closure() {
  local socket="$1" staging="$2" index queue path rel target found=0
  local -A seen=()
  index="${staging%/}/.legacy-index.html"
  fetch_old_console_path "$socket" "/console/" "$index" || return 1
  queue="${staging%/}/.asset-queue"
  : >"$queue"
  grep -Eo '/console/assets/[A-Za-z0-9._/-]+' "$index" | sort -u >>"$queue" || true
  while IFS= read -r path; do
    [[ -n "$path" && -z "${seen[$path]:-}" ]] || continue
    seen["$path"]=1
    console_asset_path_valid "$path" || return 1
    download_console_asset "$socket" "$path" "$staging" || return 1
    found=$((found + 1))
    rel="${path#/console/}"
    target="${staging%/}/${rel}"
    grep -Eo '/console/assets/[A-Za-z0-9._/-]+' "$target" 2>/dev/null | sort -u >>"$queue" || true
    while IFS= read -r rel; do
      [[ -n "$rel" ]] || continue
      printf '/console/assets/%s\n' "${rel#./}" >>"$queue"
    done < <(grep -Eo '\./[A-Za-z0-9._-]+\.(js|css|json|wasm|svg|png|jpg|jpeg|webp|gif|woff|woff2|ttf|ico)' "$target" 2>/dev/null | sort -u || true)
  done <"$queue"
  rm -f "$index" "$queue"
  (( found > 0 ))
}

snapshot_old_console_generation() {
  local socket="$1" suggested_release="$2" root manifest staging generation release manifest_assets=0
  local record path size sha total meta
  [[ -n "$socket" || -n "$suggested_release" || LEGACY_SERVICE_ACTIVE == 1 ]] || return 0
  root="${DATA_DIR%/}/console-generations"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$root"
  prune_console_generations || die "could not prune old console generations before snapshot"
  manifest="$(mktemp)"
  release="$suggested_release"
  if fetch_old_console_path "$socket" "/console/.release-manifest.json" "$manifest" 2>/dev/null && \
      grep -q '"assets"[[:space:]]*:' "$manifest"; then
    generation="$(json_string_field "$(cat "$manifest")" release_id || true)"
    [[ -n "$generation" ]] || generation="$release"
    valid_worker_release_id "$generation" || generation="legacy-$(date -u +%Y%m%dT%H%M%SZ)"
    staging="${root}/.staging-${generation}-$$"
    run_root rm -rf -- "$staging"
    run_root install -d -m 0755 "$staging"
    while IFS= read -r record; do
      [[ "$record" == *'"path"'* ]] || continue
      path="$(json_string_field "$record" path || true)"
      size="$(json_integer_field "$record" size 2>/dev/null || true)"
      sha="$(json_string_field "$record" sha256 || true)"
      [[ "$size" =~ ^[0-9]+$ && "$sha" =~ ^[a-f0-9]{64}$ ]] || die "old console manifest contains an invalid asset record"
      download_console_asset "$socket" "$path" "$staging" "$size" "$sha" || die "could not validate old console asset ${path}"
      manifest_assets=$((manifest_assets + 1))
    done < <(sed 's/{"path"/\n{"path"/g' "$manifest")
    (( manifest_assets > 0 )) || die "old console manifest did not contain any assets"
  else
    generation="$release"
    valid_worker_release_id "$generation" || generation="legacy-$(date -u +%Y%m%dT%H%M%SZ)"
    staging="${root}/.staging-${generation}-$$"
    run_root rm -rf -- "$staging"
    run_root install -d -m 0755 "$staging"
    log "Old release has no console manifest; resolving its index/import closure"
    snapshot_legacy_console_closure "$socket" "$staging" || die "could not capture the legacy console asset closure"
  fi
  rm -f "$manifest"
  total="$(directory_bytes "$staging")"
  (( total <= CONSOLE_GENERATION_MAX_BYTES )) || die "old console generation $(human_bytes "$total") exceeds CONSOLE_GENERATION_MAX_BYTES"
  meta="$(mktemp)"
  cat >"$meta" <<EOF
{"release_id":"$(json_escape "$generation")","captured_at":$(date +%s),"bytes":${total},"manifest_assets":${manifest_assets}}
EOF
  run_root install -m 0644 "$meta" "${staging}/generation.json"
  rm -f "$meta"
  run_root chown -R "root:${SERVICE_GROUP}" "$staging"
  run_root chmod -R u=rwX,g=rX,o= "$staging"
  if [[ -d "${root}/${generation}" ]]; then
    run_root rm -rf -- "$staging"
    log "Console generation ${generation} is already retained"
  else
    run_root mv "$staging" "${root}/${generation}"
    log "Captured old console generation ${generation} ($(human_bytes "$total"))"
  fi
  prune_console_generations "$generation" || die "captured console generation cannot fit its configured byte budget"
}

reclaim_build_caches_under_pressure() {
  local phase="$1" cache path bytes total=0
  collect_deployment_storage "$phase"
  if (( DEPLOY_FREE_BYTES >= DEPLOY_RESERVE_BYTES + DEPLOY_CANDIDATE_ESTIMATE_BYTES )); then
    return 0
  fi
  warn "Filesystem is under deployment reserve pressure; reclaiming bounded build caches"
  if command -v go >/dev/null 2>&1; then
    cache="$(go env GOCACHE 2>/dev/null || true)"
    if [[ -n "$cache" && "$cache" == /* && "$cache" != "/" ]]; then
      bytes="$(directory_bytes "$cache")"; total=$((total + bytes))
      if (( bytes > BUILD_CACHE_MAX_BYTES || DEPLOY_FREE_BYTES < DEPLOY_RESERVE_BYTES )); then
        go clean -cache >/dev/null 2>&1 || warn "could not reclaim Go build cache"
      fi
    fi
  fi
  if command -v npm >/dev/null 2>&1; then
    path="$(npm config get cache 2>/dev/null || true)"
    if [[ -n "$path" && "$path" == /* && "$path" != "/" ]]; then
      bytes="$(directory_bytes "$path")"; total=$((total + bytes))
      if (( bytes > BUILD_CACHE_MAX_BYTES || total > BUILD_CACHE_MAX_BYTES )); then
        npm cache clean --force >/dev/null 2>&1 || warn "could not reclaim npm cache"
      fi
    fi
  fi
}

schedule_release_reaper() {
  local release="$1" unit
  if [[ "$release" != "_legacy" ]] && ! valid_worker_release_id "$release"; then
    warn "Refusing malformed release reaper id: ${release}"
    return 1
  fi
  unit="${SERVICE_NAME}-reaper@${release}.service"
  # A oneshot service normally makes `systemctl start` wait until ExecStart exits.
  # --no-block is essential here: long SSE/WebSocket requests remain owned by the
  # previous worker, while install.sh returns as soon as the new release is live.
  # Enable the instance until it succeeds so an unexpected reboot resumes cleanup.
  run_root systemctl enable "$unit" >/dev/null || return 1
  if ! run_root systemctl start --no-block "$unit" >/dev/null; then
    run_root systemctl disable "$unit" >/dev/null 2>&1 || true
    return 1
  fi
  log "Background drain/reclaim scheduled: ${unit}"
}

schedule_superseded_release_reapers() {
  local current_release="$1" protected_release="${2:-}" unit release socket path
  local -A scheduled=()

  while IFS= read -r unit; do
    [[ -n "$unit" ]] || continue
    release="${unit#${SERVICE_NAME}-worker@}"
    release="${release%.service}"
    valid_worker_release_id "$release" || die "malformed loaded worker unit: ${unit}"
    [[ "$release" != "$current_release" && "$release" != "$protected_release" ]] || continue
    scheduled["$release"]=1
  done < <(
    {
      run_root systemctl list-units --type=service --all --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
      run_root systemctl list-unit-files --type=service --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
    } | awk '{print $1}' | grep -F "${SERVICE_NAME}-worker@" | grep -E '@[^[:space:]]+\.service$' | sort -u || true
  )

  while IFS= read -r socket; do
    [[ -n "$socket" ]] || continue
    release="${socket##*/worker-}"
    release="${release%.sock}"
    valid_worker_release_id "$release" || die "malformed stale worker socket: ${socket}"
    [[ "$release" != "$current_release" && "$release" != "$protected_release" ]] || continue
    scheduled["$release"]=1
  done < <(run_root find "${DATA_DIR%/}/run" -maxdepth 1 \( -type s -o -type l \) -name 'worker-*.sock' -print 2>/dev/null || true)

  while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    release="${path##*/}"
    [[ "$release" != .staging-* ]] || continue
    valid_worker_release_id "$release" || die "malformed stale release directory: ${path}"
    [[ "$release" != "$current_release" && "$release" != "$protected_release" ]] || continue
    scheduled["$release"]=1
  done < <(run_root find "${APP_DIR%/}/releases" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort)

  for release in "${!scheduled[@]}"; do
    schedule_release_reaper "$release" || return 1
  done
}

worker_unit_destroyed() {
  local unit="$1" state pid
  state="$(run_root systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
  pid="$(run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  [[ -z "$pid" || "$pid" == "0" ]] || return 1
  case "$state" in
    ""|inactive|failed) return 0 ;;
    *) return 1 ;;
  esac
}

wait_worker_unit_destroyed() {
  local unit="$1" timeout="$2" started="$SECONDS"
  while (( SECONDS - started < timeout )); do
    worker_unit_destroyed "$unit" && return 0
    sleep 0.2
  done
  worker_unit_destroyed "$unit"
}

destroy_worker_instance() {
  local release="$1" socket="${2:-}" unit
  if ! valid_worker_release_id "$release"; then
    warn "Refusing malformed worker release id: ${release}"
    return 1
  fi
  if [[ ! "$WORKER_DESTROY_TIMEOUT" =~ ^[0-9]+$ ]]; then
    warn "WORKER_DESTROY_TIMEOUT must be a non-negative integer: ${WORKER_DESTROY_TIMEOUT}"
    return 1
  fi
  unit="${SERVICE_NAME}-worker@${release}.service"

  log "Destroying superseded worker instance ${unit}"
  run_root systemctl stop --no-block "$unit" >/dev/null 2>&1 || true
  if ! wait_worker_unit_destroyed "$unit" "$WORKER_DESTROY_TIMEOUT"; then
    warn "${unit} did not exit within ${WORKER_DESTROY_TIMEOUT}s; killing its complete cgroup"
    run_root systemctl kill --kill-who=all --signal=SIGKILL "$unit" >/dev/null 2>&1 || true
    run_root systemctl stop "$unit" >/dev/null 2>&1 || true
  fi
  run_root systemctl disable "$unit" >/dev/null 2>&1 || true
  run_root systemctl reset-failed "$unit" >/dev/null 2>&1 || true
  if [[ -n "$socket" ]]; then
    run_root rm -f -- "$socket"
  fi
  if ! worker_unit_destroyed "$unit"; then
    warn "Superseded worker ${unit} is still running after forced destruction"
    return 1
  fi
}

destroy_stale_worker_instances() {
  local current_release="$1" current_unit unit release socket
  current_unit="${SERVICE_NAME}-worker@${current_release}.service"
  while IFS= read -r unit; do
    [[ -n "$unit" && "$unit" != "$current_unit" ]] || continue
    release="${unit#${SERVICE_NAME}-worker@}"
    release="${release%.service}"
    valid_worker_release_id "$release" || die "malformed loaded worker unit: ${unit}"
    socket="${DATA_DIR%/}/run/worker-${release}.sock"
    destroy_worker_instance "$release" "$socket" || die "failed to destroy stale worker ${unit}"
  done < <(
    {
      run_root systemctl list-units --type=service --all --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
      run_root systemctl list-unit-files --type=service --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
    } | awk '{print $1}' | grep -F "${SERVICE_NAME}-worker@" | grep -E '@[^[:space:]]+\.service$' | sort -u || true
  )

  # A killed process can leave an unlinked or stale Unix socket without a loaded
  # systemd unit. Remove every non-current worker socket through the same validated
  # instance cleanup path so the next deployment cannot accidentally rediscover it.
  while IFS= read -r socket; do
    [[ -n "$socket" && "$socket" != "${DATA_DIR%/}/run/worker-${current_release}.sock" ]] || continue
    release="${socket##*/worker-}"
    release="${release%.sock}"
    valid_worker_release_id "$release" || die "malformed stale worker socket: ${socket}"
    destroy_worker_instance "$release" "$socket" || die "failed to destroy stale worker socket ${socket}"
  done < <(run_root find "${DATA_DIR%/}/run" -maxdepth 1 \( -type s -o -type l \) -name 'worker-*.sock' -print 2>/dev/null || true)
}

destroy_legacy_worker_process() {
  local unit="${SERVICE_NAME}.service" pid state next_report
  pid="$(run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  state="$(run_root systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
  if [[ "$state" == "active" || "$state" == "activating" || "$state" == "deactivating" || "$pid" =~ ^[1-9][0-9]*$ ]]; then
    log "Draining superseded legacy worker process in ${unit}"
    run_root systemctl stop --no-block "$unit" >/dev/null 2>&1 || true
    next_report=$((SECONDS + 30))
    while ! worker_unit_destroyed "$unit"; do
      if (( SECONDS >= next_report )); then
        warn "Legacy worker is still draining established connections; preserving it instead of forcing client-visible errors"
        next_report=$((next_report + 30))
      fi
      sleep 1
    done
  fi
  run_root systemctl disable "$unit" >/dev/null 2>&1 || true
  run_root systemctl reset-failed "$unit" >/dev/null 2>&1 || true
  worker_unit_destroyed "$unit" || die "legacy worker process did not finish its graceful drain"
}

superseded_release_process() {
  local pid="$1" current_release="$2" target release prefix
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  target="$(readlink "${PROC_ROOT%/}/${pid}/exe" 2>/dev/null || true)"
  target="${target% (deleted)}"
  prefix="${APP_DIR%/}/releases/"
  [[ "$target" == "${prefix}"*"${APP_NAME}" ]] || return 1
  release="${target#${prefix}}"
  release="${release%%/*}"
  valid_worker_release_id "$release" || return 1
  [[ "$target" == "${prefix}${release}/${APP_NAME}" && "$release" != "$current_release" ]]
}

destroy_superseded_release_processes() {
  local current_release="$1" exe pid started
  local -a stale_pids=()
  for exe in "${PROC_ROOT%/}"/[0-9]*/exe; do
    [[ -L "$exe" ]] || continue
    pid="${exe%/exe}"
    pid="${pid##*/}"
    superseded_release_process "$pid" "$current_release" || continue
    stale_pids+=("$pid")
    log "Terminating detached superseded release process pid=${pid}"
    run_root kill -TERM "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${stale_pids[@]}"; do
    started="$SECONDS"
    while superseded_release_process "$pid" "$current_release" &&
      (( SECONDS - started < WORKER_DESTROY_TIMEOUT )); do
      sleep 0.2
    done
    if superseded_release_process "$pid" "$current_release"; then
      warn "Detached superseded release process pid=${pid} ignored TERM; forcing exit"
      run_root kill -KILL "$pid" >/dev/null 2>&1 || true
    fi
    superseded_release_process "$pid" "$current_release" &&
      die "detached superseded release process pid=${pid} remains after forced destruction"
  done
  return 0
}

release_directory_has_blocking_process() {
  local release_dir="$1" exe target
  for exe in "${PROC_ROOT%/}"/[0-9]*/exe; do
    [[ -L "$exe" ]] || continue
    target="$(readlink "$exe" 2>/dev/null || true)"
    target="${target% (deleted)}"
    [[ "$target" == "${release_dir%/}/"* ]] || continue
    # The handoff binary is intentionally allowed to keep running across worker
    # cutovers so established streams are not severed. Removing its old immutable
    # release directory is safe on Unix: the live process keeps its executable
    # inode until its next normal restart, while the directory contents become
    # reclaimable and systemd's ExecStart points at the current compatibility
    # entrypoint.
    [[ "$target" == "${release_dir%/}/${HANDOFF_NAME}" ]] && continue
    return 0
  done
  return 1
}

prune_superseded_release_artifacts() {
  local current_release="$1" current_path previous_path path base staging_release
  current_path="${APP_DIR%/}/releases/${current_release}"
  previous_path="$(run_root readlink -f "${APP_DIR%/}/previous" 2>/dev/null || true)"
  while IFS= read -r path; do
    [[ -n "$path" && "$path" != "$current_path" && "$path" != "$previous_path" ]] || continue
    base="${path##*/}"
    if [[ "$base" == .staging-* ]]; then
      staging_release="${base#.staging-}"
      valid_worker_release_id "$staging_release" || die "malformed stale release staging directory: ${path}"
    else
      valid_worker_release_id "$base" || die "malformed stale release directory: ${path}"
    fi
    release_directory_has_blocking_process "$path" &&
      die "refusing to delete release artifact still used by a process: ${path}"
    log "Reclaiming superseded release artifact ${path}"
    run_root rm -rf -- "$path"
    [[ ! -e "$path" ]] || die "superseded release artifact was not removed: ${path}"
  done < <(run_root find "${APP_DIR%/}/releases" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | sort)
  return 0
}

reclaim_superseded_install_resources() {
  local current_release="$1"
  destroy_stale_worker_instances "$current_release"
  destroy_legacy_worker_process
  destroy_superseded_release_processes "$current_release"
  # A detached process may have held a stale socket after its systemd instance was
  # already forgotten. Sweep worker resources once more after process destruction.
  destroy_stale_worker_instances "$current_release"
  verify_single_active_worker "$current_release"
  prune_superseded_release_artifacts "$current_release"
  log "Superseded release processes and runtime resources were reclaimed"
}

verify_single_active_worker() {
  local current_release="$1" expected
  local unit pid count=0
  expected="${SERVICE_NAME}-worker@${current_release}.service"
  while IFS= read -r unit; do
    [[ -n "$unit" ]] || continue
    ((count += 1))
    [[ "$unit" == "$expected" ]] || die "unexpected active worker remains after cutover: ${unit}"
  done < <(run_root systemctl list-units --type=service --state=active --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null | awk '{print $1}')
  (( count == 1 )) || die "expected exactly one active worker (${expected}), found ${count}"
  pid="$(run_root systemctl show "$expected" --property=MainPID --value 2>/dev/null || true)"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || die "active worker ${expected} has no live MainPID"
  log "Single-worker invariant verified: ${expected} (MainPID=${pid})"
}

verify_current_worker_active() {
  local current_release="$1" unit state pid
  unit="${SERVICE_NAME}-worker@${current_release}.service"
  state="$(run_root systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
  pid="$(run_root systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  [[ "$state" == "active" && "$pid" =~ ^[1-9][0-9]*$ ]] ||
    die "new worker is not active after cutover: ${unit} state=${state:-unknown} pid=${pid:-0}"
  log "Current worker verified: ${unit} (MainPID=${pid}); older workers may still be draining"
}

rollback_pending_activation() {
  (( ACTIVATION_PENDING == 1 )) || return 0
  warn "Rolling back the incomplete traffic switch before reopening admission"
  if [[ -n "$ACTIVATION_OLD_RELEASE" && -n "$ACTIVATION_OLD_SOCKET" ]]; then
    atomic_symlink "$ACTIVATION_OLD_RELEASE" "${APP_DIR%/}/current" || return 1
    atomic_symlink "$ACTIVATION_OLD_SOCKET" "${DATA_DIR%/}/run/active-worker.sock" || return 1
    if [[ -n "$ACTIVATION_OLD_WORKER_RELEASE" ]]; then
      run_root systemctl enable "${SERVICE_NAME}-worker@${ACTIVATION_OLD_WORKER_RELEASE}.service" >/dev/null || return 1
      run_root systemctl start "${SERVICE_NAME}-worker@${ACTIVATION_OLD_WORKER_RELEASE}.service" >/dev/null || return 1
      wait_for_service_health "$(handoff_public_base_url)/readyz" "$HEALTH_TIMEOUT" "activation rollback" "$ACTIVATION_OLD_WORKER_RELEASE" || return 1
    fi
  fi
  if [[ -n "$ACTIVATION_NEW_RELEASE" ]]; then
    destroy_worker_instance "$ACTIVATION_NEW_RELEASE" "${DATA_DIR%/}/run/worker-${ACTIVATION_NEW_RELEASE}.sock" || return 1
  fi
  if [[ -n "$ACTIVATION_OLD_WORKER_RELEASE" ]]; then
    # Older generations may still own streams from an earlier successful update.
    # Rollback only destroys the failed candidate above; it must not collapse those
    # independently draining workers into a single-process invariant.
    verify_current_worker_active "$ACTIVATION_OLD_WORKER_RELEASE" || return 1
  fi
  restore_codex_reauth_worker_after_rollback ||
    warn "previous Codex reauth worker did not recover during activation rollback"
  ACTIVATION_PENDING=0
}

deployment_exit_cleanup() {
  local status="$?"
  if (( ACTIVATION_PENDING == 1 )); then
    set +e
    if (( HANDOFF_PAUSED == 0 )) && handoff_control_is_available; then
      pause_handoff_admission || warn "could not pause admission before incomplete activation rollback"
    fi
    rollback_pending_activation || warn "incomplete activation rollback needs operator inspection"
    set -e
  fi
  if (( HANDOFF_PAUSED == 1 )); then
    set +e
    resume_handoff_admission || warn "handoff admission resume failed during installer cleanup; inspect ${HANDOFF_CONTROL_SOCKET}"
    set -e
  fi
  return "$status"
}

start_handoff_for_legacy_migration() {
  local socket_started=0 started
  if run_root systemctl is-active --quiet "${SERVICE_NAME}.socket"; then
    socket_started=1
  elif (( LEGACY_SERVICE_ACTIVE == 1 )); then
    # Do not wait for the old service's long graceful drain. SIGTERM closes its copy
    # of the listener immediately; established streams remain in that old process.
    log "Handing new connections off from legacy ${SERVICE_NAME}.service without waiting for its streams"
    run_root systemctl set-property --runtime "${SERVICE_NAME}.service" TimeoutStopUSec=infinity >/dev/null 2>&1 || true
    run_root systemctl stop --no-block "${SERVICE_NAME}.service" || true
  fi

  if (( socket_started == 0 )); then
    started="$SECONDS"
    while (( SECONDS - started < HEALTH_TIMEOUT )); do
      if run_root systemctl start "${SERVICE_NAME}.socket" 2>/dev/null && \
          run_root systemctl is-active --quiet "${SERVICE_NAME}.socket"; then
        socket_started=1
        break
      fi
      sleep 0.1
    done
  fi
  (( socket_started == 1 )) || die "public activation socket did not take ownership of ${LISTEN_ADDR}"

  log "Starting independent ${HANDOFF_SERVICE_NAME}.service in paused-admission mode"
  run_root systemctl start "${HANDOFF_SERVICE_NAME}.service" || {
    run_root systemctl --no-pager --full status "${HANDOFF_SERVICE_NAME}.service" >&2 || true
    die "stable handoff service did not start"
  }
  wait_handoff_control_ready || {
    run_root systemctl --no-pager --full status "${HANDOFF_SERVICE_NAME}.service" >&2 || true
    die "stable handoff control socket did not become ready"
  }

  if (( LEGACY_SERVICE_ACTIVE == 1 )); then
    # With an already-active .socket both processes briefly share its listening file
    # description. The old server stops accepting, but keeps every established stream.
    run_root systemctl set-property --runtime "${SERVICE_NAME}.service" TimeoutStopUSec=infinity >/dev/null 2>&1 || true
    run_root systemctl stop --no-block "${SERVICE_NAME}.service" || true
  fi
  wait_handoff_public_instance "$RELEASE_ID" || die "new handoff did not become the public listener"
}

activate_staged_release() {
  [[ -f "${RELEASE_DIR%/}/.staged-ok" ]] || die "release staging is incomplete: ${RELEASE_DIR}"
  bool_enabled "$START_SERVICE" || {
    atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
    return 0
  }
  systemd_running || {
    atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
    warn "systemd is not running; release activated for manual use"
    return 0
  }

  local new_socket="${DATA_DIR%/}/run/worker-${RELEASE_ID}.sock"
  local new_unit="${SERVICE_NAME}-worker@${RELEASE_ID}.service" startup_started failure_hint=""
  local old_socket old_release old_release_id="" old_worker_release="" protected_release="" health_url had_handoff=0
  local warm_required=0 legacy_warm_marker="${DATA_DIR%/}/run/legacy-worker.sock"
  [[ "$DRAIN_TIMEOUT" =~ ^[0-9]+$ ]] || die "DRAIN_TIMEOUT must be a non-negative integer: ${DRAIN_TIMEOUT}"
  [[ "$WORKER_DESTROY_TIMEOUT" =~ ^[0-9]+$ ]] || die "WORKER_DESTROY_TIMEOUT must be a non-negative integer: ${WORKER_DESTROY_TIMEOUT}"
  (( ${#new_socket} < 104 )) || die "worker Unix socket path is too long: ${new_socket}"
  old_socket="$(run_root readlink "${DATA_DIR%/}/run/active-worker.sock" 2>/dev/null || true)"
  old_release="$(run_root readlink "${APP_DIR%/}/current" 2>/dev/null || true)"
  if [[ -n "$old_release" ]]; then
    old_release_id="${old_release##*/}"
    valid_worker_release_id "$old_release_id" || old_release_id=""
  fi
  if [[ -n "$old_socket" && "$old_socket" != "$new_socket" ]]; then
    old_worker_release="${old_socket##*/worker-}"
    old_worker_release="${old_worker_release%.sock}"
    if [[ "$old_socket" == "$legacy_warm_marker" ]]; then
      old_worker_release=""
    else
      valid_worker_release_id "$old_worker_release" || die "malformed active worker socket target: ${old_socket}"
    fi
    warm_required=1
  elif (( LEGACY_SERVICE_ACTIVE == 1 )); then
    # A first migration has no old private worker socket. A deliberately dangling
    # marker still lets the auto-role candidate prove full read-only warm standby;
    # the public legacy process remains untouched until the later socket handoff.
    atomic_symlink "$legacy_warm_marker" "${DATA_DIR%/}/run/active-worker.sock"
    warm_required=1
  fi

  # With an existing A/B worker, auto mode starts this generation as a tiny
  # pre-initialized standby. It binds only its private deployment socket and does
  # not open config, keys, SQLite, migrations, modules, or application listeners.
  # That makes the candidate observable without competing with the live writer.
  log "Starting pre-initialized staged worker ${RELEASE_ID} on ${new_socket}"
  startup_started="$(date +%s)"
  WORKER_FAILURE_REPORT=""
  if ! run_root systemctl start "$new_unit"; then
    capture_worker_startup_failure "$RELEASE_ID" "$startup_started"
    [[ -z "$WORKER_FAILURE_REPORT" ]] || failure_hint="; failure report: ${WORKER_FAILURE_REPORT}"
    die "could not start staged worker ${RELEASE_ID}; active release was not changed${failure_hint}"
  fi
  if ! wait_worker_ready "$new_socket" "$RELEASE_ID" "$HEALTH_TIMEOUT" "$warm_required"; then
    # Capture the whole unit (including every restarted PID and kernel OOM lines)
    # before cleanup. Querying only the first MainPID after this point cannot explain
    # the failure because the failed candidate is intentionally stopped below.
    capture_worker_startup_failure "$RELEASE_ID" "$startup_started"
    run_root systemctl stop "$new_unit" || true
    [[ -z "$WORKER_FAILURE_REPORT" ]] || failure_hint="; failure report: ${WORKER_FAILURE_REPORT}"
    die "staged worker ${RELEASE_ID} did not pass /standbyz; active release was not changed${failure_hint}"
  fi

  ACTIVATION_PENDING=1
  ACTIVATION_OLD_RELEASE="$old_release"
  ACTIVATION_OLD_SOCKET="$old_socket"
  ACTIVATION_OLD_WORKER_RELEASE="$old_worker_release"
  ACTIVATION_NEW_RELEASE="$RELEASE_ID"
  if [[ -n "$old_release" || -n "$old_socket" || LEGACY_SERVICE_ACTIVE == 1 ]]; then
    snapshot_old_console_generation "$old_socket" "$old_release_id"
  fi
  # Close only the new-request admission barrier, then promote by atomically moving
  # the worker link. Existing HTTP/SSE/WebSocket requests stay pinned to the old
  # process. Link observation demotes its background writers and execs the candidate
  # into full startup; no connection-count drain or old-worker stop is required.
  if handoff_control_is_available; then
    had_handoff=1
    pause_handoff_admission || die "stable handoff did not acknowledge the admission pause"
  else
    arm_initial_handoff_pause
  fi

  [[ -z "$old_release" ]] || atomic_symlink "$old_release" "${APP_DIR%/}/previous"
  atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
  atomic_symlink "$new_socket" "${DATA_DIR%/}/run/active-worker.sock"

  # On the first migration a separately named handoff begins accepting from the
  # systemd-owned socket while the legacy service drains in its original cgroup. This
  # avoids `systemctl restart` waiting on every long-lived stream before a new process
  # can start. Ordinary updates never restart the public listener at all.
  if (( had_handoff == 0 )); then
    start_handoff_for_legacy_migration
  fi
  health_url="$(handoff_public_base_url)/readyz"
  if ! wait_for_service_health "$health_url" "$HEALTH_TIMEOUT" "handoff switch" "$RELEASE_ID"; then
    warn "new release failed after switch; restoring the complete previous release"
    if [[ -n "$old_socket" && -n "$old_release" ]]; then
      die "release ${RELEASE_ID} failed the public readiness gate; installer cleanup is restoring the previous release"
    fi
    die "first handoff activation failed; staged release remains at ${RELEASE_DIR}"
  fi
  activate_codex_reauth_worker ||
    die "Codex OAuth reauth worker failed its health gate; installer cleanup is restoring the previous release"

  run_root systemctl enable "${SERVICE_NAME}-worker@${RELEASE_ID}.service" >/dev/null ||
    die "new worker passed readiness but could not be enabled for reboot"
  if [[ -n "$old_worker_release" ]]; then
    # Disable boot activation before reopening admission. Even if the host
    # reboots during the bounded drain, systemd starts only the new release.
    run_root systemctl disable "${SERVICE_NAME}-worker@${old_worker_release}.service" >/dev/null ||
      die "superseded worker could not be disabled before drain"
  fi
  resume_handoff_admission || die "release switched but queued request admission did not resume"
  log "Release ${RELEASE_ID} is active; stable public listener was not restarted"

  verify_current_worker_active "$RELEASE_ID"
  if [[ -n "$old_socket" && "$old_socket" != "$new_socket" ]]; then
    protected_release="$old_worker_release"
  elif (( LEGACY_SERVICE_ACTIVE == 1 )); then
    protected_release="$old_release_id"
  fi

  # Interrupted older deployments can leave disabled units, sockets, or immutable
  # release trees behind. Give each one an independent non-blocking reaper too. The
  # just-scheduled predecessor is protected from a duplicate legacy cleanup path.
  schedule_superseded_release_reapers "$RELEASE_ID" "$protected_release" ||
    die "new release is live, but stale release reapers could not all be started"
  if [[ -n "$old_socket" && "$old_socket" != "$new_socket" ]]; then
    schedule_release_reaper "$old_worker_release" ||
      die "new release is live, but the previous worker reaper could not be started"
  elif (( LEGACY_SERVICE_ACTIVE == 1 )); then
    # The legacy process has no private worker socket. Its dedicated reaper waits
    # for the non-blocking graceful stop started during listener migration.
    schedule_release_reaper _legacy ||
      die "new release is live, but the legacy worker reaper could not be started"
  fi
  ACTIVATION_PENDING=0
  log "Release ${RELEASE_ID} accepted the handoff; old releases will be reclaimed asynchronously"
}

install_systemd_unit() {
  if ! systemd_requested; then
    warn "Skipping systemd unit install"
    atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
    return 0
  fi

  local database_parent read_write_paths tmp unit handoff_unit worker_unit reaper_unit sidecar_unit reauth_unit extra_env admin_env no_new_priv warp_dir_env legacy_pid
  local node_registrar_unit_dir python_registrar_unit_dir python_registrar_unit_venv
  if systemd_running && ! run_root systemctl is-active --quiet "${HANDOFF_SERVICE_NAME}.service" 2>/dev/null; then
    legacy_pid="$(run_root systemctl show "${SERVICE_NAME}.service" --property=MainPID --value 2>/dev/null || true)"
    if [[ "$legacy_pid" =~ ^[1-9][0-9]*$ ]]; then
      LEGACY_SERVICE_ACTIVE=1
    fi
  fi
  database_parent="$(dirname "$DATABASE_PATH")"
  if [[ "$database_parent" == "$DATA_DIR" ]]; then
    read_write_paths="$DATA_DIR"
  else
    read_write_paths="$DATA_DIR $database_parent"
  fi
  extra_env=""
  if bool_enabled "$WITH_SIDECAR"; then
    extra_env="${extra_env}
Environment=\"CODEX_POOL_DEFAULT_SIDECAR_ENDPOINT=http://${SIDECAR_ADDR}\""
  fi
  if bool_enabled "$WITH_REGISTRATION"; then
    if (( REGISTRAR_INSTALL_EXPLICIT == 1 )); then
      node_registrar_unit_dir="$REGISTRAR_INSTALL"
    else
      node_registrar_unit_dir="${APP_DIR%/}/releases/%i/registrar-node"
    fi
    python_registrar_unit_dir="${APP_DIR%/}/releases/%i/registrar-python"
    python_registrar_unit_venv="${APP_DIR%/}/releases/%i/registrar-python-venv"
    # Every worker resolves the immutable registrar artifacts from its own release.
    # Chrome is an OS runtime dependency; credentials are injected only into the
    # short-lived child process and never into the systemd unit.
    extra_env="${extra_env}
Environment=\"CODEX_REG_NODE_DIR=${node_registrar_unit_dir}\"
Environment=\"CODEX_REG_PYTHON=${python_registrar_unit_venv}/bin/python\"
Environment=\"CODEX_REG_PROTOCOL_SCRIPT=${python_registrar_unit_dir}/protocol_register.py\"
Environment=\"CODEX_REG_SCRIPT=${python_registrar_unit_dir}/browser_register.py\"
Environment=\"CODEX_REG_V3_SCRIPT=${python_registrar_unit_dir}/reg_v3.py\""
    if [[ -n "${NODE_BIN:-}" ]]; then
      extra_env="${extra_env}
Environment=\"CODEX_REG_NODE=${NODE_BIN}\""
    fi
    if [[ -n "${CHROME_BIN:-}" ]]; then
      extra_env="${extra_env}
Environment=\"CODEX_REG_CHROME=${CHROME_BIN}\"
Environment=\"CHROME_PATH=${CHROME_BIN}\""
    fi
  fi
  no_new_priv="true"
  if bool_enabled "$WITH_WARP"; then
    warp_dir_env="${WARP_DIR:-${DATA_DIR%/}/warp}"
    read_write_paths="${read_write_paths} ${warp_dir_env}"
    # WARP re-registration restarts an exit's wireproxy unit via `sudo systemctl`,
    # which requires NoNewPrivileges to be OFF on this unit (the sudoers rule is scoped
    # to exactly the warp@ units). Only relaxed when WARP is enabled.
    no_new_priv="false"
    extra_env="${extra_env}
Environment=\"CODEX_POOL_WARP_ENABLED=1\"
Environment=\"CODEX_POOL_WARP_EXIT_COUNT=${WARP_EXITS}\"
Environment=\"CODEX_POOL_WARP_EXIT_BASE_PORT=${WARP_BASE_PORT}\"
Environment=\"CODEX_POOL_WARP_ACCOUNTS_PER_EXIT=${WARP_ACCOUNTS_PER_EXIT}\"
Environment=\"CODEX_POOL_WARP_EXIT_SCRIPT=${warp_dir_env}/warp-exit.sh\"
Environment=\"WARP_DIR=${warp_dir_env}\"
Environment=\"WARP_BASE_PORT=${WARP_BASE_PORT}\"
Environment=\"SERVICE_NAME=${SERVICE_NAME}\""
  fi
  if [[ -n "$CF_SOLVER_URL" ]]; then
    extra_env="${extra_env}
Environment=\"CODEX_POOL_CF_SOLVER_ENABLED=1\"
Environment=\"CODEX_POOL_CF_SOLVER_URL=${CF_SOLVER_URL}\""
  fi
  admin_env=""
  if [[ -n "$ADMIN_TOKEN" ]]; then
    admin_env="
LoadCredential=admin.token:${DATA_DIR%/}/data/keys/admin.token"
  fi

  run_root install -d -m 0755 "$SYSTEMD_DIR"

  handoff_unit="${SYSTEMD_DIR%/}/${HANDOFF_SERVICE_NAME}.service"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool stable public HTTP handoff
After=network-online.target ${SERVICE_NAME}.socket
Wants=network-online.target
Requires=${SERVICE_NAME}.socket

[Service]
Type=simple
Sockets=${SERVICE_NAME}.socket
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${DATA_DIR}
Environment="CODEX_POOL_DATA_DIR=${DATA_DIR%/}/data"
LoadCredential=core-state.key:${DATA_DIR%/}/data/keys/core-state.key
ExecStart=${BIN_DIR}/${HANDOFF_NAME} --listen ${LISTEN_ADDR} --backend-link ${DATA_DIR%/}/run/active-worker.sock --control-socket ${HANDOFF_CONTROL_SOCKET} --pause-state ${HANDOFF_PAUSE_STATE} --instance-id ${RELEASE_ID}
Restart=always
RestartSec=3
TimeoutStopSec=300
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${read_write_paths}

[Install]
WantedBy=multi-user.target
EOF
  log "Installing independent handoff unit to ${handoff_unit}"
  run_root install -m 0644 "$tmp" "$handoff_unit"
  rm -f "$tmp"

  # Keep the historical service name as a compatibility marker. During the one-time
  # migration its already-running MainPID remains attached here and can drain after a
  # non-blocking stop; on later boots/starts this unit only requires the true handoff.
  unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}.service"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool compatibility service (public listener is ${HANDOFF_SERVICE_NAME}.service)
Requires=${HANDOFF_SERVICE_NAME}.service
After=${HANDOFF_SERVICE_NAME}.service

[Service]
Type=oneshot
Environment="CODEX_POOL_DATABASE=${DATABASE_PATH}"
Environment="CODEX_POOL_LISTEN_ADDR=${LISTEN_ADDR}"
ExecStart=/bin/true
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF
  log "Installing compatibility systemd unit to ${unit}"
  run_root install -m 0644 "$tmp" "$unit"
  rm -f "$tmp"

  worker_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-worker@.service"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool worker release %i
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${DATA_DIR}
Environment="CODEX_POOL_DATABASE=${DATABASE_PATH}"
Environment="CODEX_POOL_MIGRATE_USER_GROUPS=${MIGRATE_USER_GROUPS}"
Environment="CODEX_POOL_DATA_DIR=${DATA_DIR%/}/data"
Environment="CODEX_POOL_INSTALL_DATA_DIR=${DATA_DIR}"
Environment="CODEX_POOL_SUPER_INSTRUCT_DIR=${APP_DIR%/}/releases/%i/super-instruct/codex-skills"
Environment="CODEX_POOL_SUPER_INSTRUCT_BRIDGE_FILE=${APP_DIR%/}/releases/%i/super-instruct/bridge.md"
LoadCredential=master.key:${DATA_DIR%/}/data/keys/master.key
LoadCredential=identity.key:${DATA_DIR%/}/data/keys/identity.key
LoadCredential=diagnostic-alias.key:${DATA_DIR%/}/data/keys/diagnostic-alias.key
LoadCredential=core-state.key:${DATA_DIR%/}/data/keys/core-state.key
Environment="CODEX_POOL_RELEASE_ID=%i"
Environment="CODEX_POOL_INSTANCE_ID=%i"${admin_env}${extra_env}
ExecStart=${APP_DIR%/}/releases/%i/${APP_NAME} --config ${CONFIG_FILE} --release-id %i --deployment-role auto --unix-socket ${DATA_DIR%/}/run/worker-%i.sock
Restart=on-failure
RestartSec=3
# The worker reaper sends SIGTERM only after worker-local inflight has remained at
# zero. Infinity is a final safety net for durable-write flushes during shutdown.
TimeoutStopSec=infinity
NoNewPrivileges=${no_new_priv}
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${read_write_paths}

[Install]
WantedBy=multi-user.target
EOF
  log "Installing A/B worker template to ${worker_unit}"
  run_root install -m 0644 "$tmp" "$worker_unit"
  rm -f "$tmp"

  reaper_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-reaper@.service"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool background drain and release reaper %i
After=network.target
StartLimitIntervalSec=0

[Service]
Type=oneshot
Environment="CODEX_POOL_REAPER_REPORT_SECONDS=${DRAIN_TIMEOUT}"
ExecStart=${APP_DIR%/}/bin/${APP_NAME}-reaper --service-name ${SERVICE_NAME} --app-dir ${APP_DIR} --data-dir ${DATA_DIR} --lock-file ${DEPLOY_LOCK_FILE} --app-name ${APP_NAME} --handoff-name ${HANDOFF_NAME} --release %i
Restart=on-failure
RestartSec=30
TimeoutStartSec=infinity
TimeoutStopSec=infinity

[Install]
WantedBy=multi-user.target
EOF
  log "Installing asynchronous release reaper template to ${reaper_unit}"
  run_root install -m 0644 "$tmp" "$reaper_unit"
  rm -f "$tmp"

  # The socket explicitly activates the independently named handoff, never the legacy
  # worker service. Its fd remains open while an old process drains established streams.
  local socket_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}.socket"
  tmp="$(mktemp)"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Account Pool Server activation socket

[Socket]
ListenStream=${LISTEN_ADDR}
Service=${HANDOFF_SERVICE_NAME}.service
Backlog=4096

[Install]
WantedBy=sockets.target
EOF
  log "Installing systemd socket unit to ${socket_unit}"
  run_root install -m 0644 "$tmp" "$socket_unit"
  rm -f "$tmp"

  if bool_enabled "$WITH_SIDECAR"; then
    tmp="$(mktemp)"
    sidecar_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-sidecar.service"
    cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool curl_cffi Sidecar
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${DATA_DIR}
Environment="CODEX_POOL_SIDECAR_ADDR=${SIDECAR_ADDR}"
Environment="CODEX_POOL_SIDECAR_COOKIE_DIR=${SIDECAR_COOKIE_DIR}"
Environment="CODEX_POOL_SIDECAR_TIMEOUT=120"
Environment="CODEX_POOL_SIDECAR_ACCEPT_ENCODING=gzip, deflate"
Environment="CODEX_POOL_SIDECAR_DRAIN_SECONDS=20"
Environment="PYTHONDONTWRITEBYTECODE=1"
ExecStart=${SIDECAR_VENV}/bin/python ${SIDECAR_INSTALL_DIR}/curl_cffi_sidecar.py
Restart=always
RestartSec=3
TimeoutStopSec=60
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
    log "Installing systemd unit to ${sidecar_unit}"
    run_root install -m 0644 "$tmp" "$sidecar_unit"
    rm -f "$tmp"
  fi

  if bool_enabled "$WITH_REGISTRATION"; then
    local reauth_host reauth_port reauth_chrome_env
    reauth_host="$(listen_host "$CODEX_REAUTH_ADDR")"
    reauth_port="$(listen_port "$CODEX_REAUTH_ADDR")"
    reauth_chrome_env=""
    if [[ -n "${CHROME_BIN:-}" ]]; then
      reauth_chrome_env="
Environment=\"CHROME_PATH=${CHROME_BIN}\"
Environment=\"REG_CHROME=${CHROME_BIN}\""
    fi
    tmp="$(mktemp)"
    reauth_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-reauth.service"
    cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool OAuth reauth worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${DATA_DIR}
Environment="HOME=${DATA_DIR}"
Environment="PYTHONDONTWRITEBYTECODE=1"
Environment="REG_HEADLESS=1"${reauth_chrome_env}
ExecStart=${APP_DIR%/}/current/registrar-python-venv/bin/python ${APP_DIR%/}/current/codex-reauth/codex_reauth_worker.py --host ${reauth_host} --port ${reauth_port} --concurrency ${CODEX_REAUTH_CONCURRENCY}
Restart=always
RestartSec=3
TimeoutStopSec=45
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${read_write_paths}
UMask=0077

[Install]
WantedBy=multi-user.target
EOF
    log "Installing Codex OAuth reauth worker unit to ${reauth_unit}"
    run_root install -m 0644 "$tmp" "$reauth_unit"
    rm -f "$tmp"
  else
    reauth_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-reauth.service"
    if systemd_running; then
      run_root systemctl disable --now "${SERVICE_NAME}-reauth.service" >/dev/null 2>&1 || true
    fi
    run_root rm -f "$reauth_unit"
  fi

  if systemd_running; then
    run_root systemctl daemon-reload
    run_root systemctl enable "${SERVICE_NAME}.socket" >/dev/null 2>&1 || true
    run_root systemctl enable "${HANDOFF_SERVICE_NAME}.service" >/dev/null 2>&1 || true
    run_root systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
    if bool_enabled "$WITH_SIDECAR"; then
      run_root systemctl enable "${SERVICE_NAME}-sidecar.service" >/dev/null 2>&1 || true
    fi
    if bool_enabled "$WITH_REGISTRATION"; then
      run_root systemctl enable "${SERVICE_NAME}-reauth.service" >/dev/null 2>&1 || true
    fi
    if bool_enabled "$START_SERVICE"; then
      # Start and self-test the private A/B worker first. activate_staged_release then
      # atomically moves new requests and delegates any long old streams to a
      # background reaper, so installation never waits for their completion.
      activate_staged_release
      run_root systemctl --no-pager --full status "${HANDOFF_SERVICE_NAME}.service" || true

      # Never hot-restart a live shared sidecar during an upgrade. Both old and new
      # workers may own streams through it; the staged release is picked up on the
      # sidecar's next normal cold start. A fresh/inactive install starts it now.
      if bool_enabled "$WITH_SIDECAR"; then
        if run_root systemctl is-active --quiet "${SERVICE_NAME}-sidecar.service" 2>/dev/null; then
          if [[ "${SIDECAR_CHANGED:-1}" == "1" ]]; then
            log "Sidecar update staged; leaving the active process and its streams untouched until the next cold start"
          else
            log "Sidecar unchanged; leaving ${SERVICE_NAME}-sidecar.service running (no stream cut)"
          fi
        else
          log "Starting ${SERVICE_NAME}-sidecar.service"
          run_root systemctl start "${SERVICE_NAME}-sidecar.service" || true
          run_root systemctl --no-pager --full status "${SERVICE_NAME}-sidecar.service" || true
        fi
      fi

      # Superseded worker processes, sockets, and immutable releases are owned by
      # ${SERVICE_NAME}-reaper@*.service. Do not synchronously reclaim them here:
      # an old worker may legitimately retain a long HTTP/SSE/WebSocket request.
    else
      atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
      warn "Service installed but not started"
    fi
  else
    atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
    warn "systemd is not running; unit file installed only"
  fi
}

install_sidecar() {
  bool_enabled "$WITH_SIDECAR" || return 0

  local new_hash old_hash release_source release_venv hash_file runtime_hash
  release_source="${RELEASE_DIR%/}/sidecar"
  release_venv="${RELEASE_DIR%/}/sidecar-venv"
  new_hash="$(cat sidecar/curl_cffi_sidecar.py sidecar/requirements.txt 2>/dev/null | sha256sum | awk '{print $1}')"
  old_hash="$(run_root cat "${APP_DIR%/}/current/sidecar/.source-hash" 2>/dev/null || true)"
  [[ -n "$old_hash" ]] || old_hash="$(run_root cat "${SIDECAR_INSTALL_DIR%/}/.source-hash" 2>/dev/null || true)"
  if [[ -n "$new_hash" && "$new_hash" == "$old_hash" ]]; then SIDECAR_CHANGED=0; else SIDECAR_CHANGED=1; fi

  # The complete Python runtime is built and tested inside the still-inactive release.
  # A failed pip install or self-test therefore leaves current and its running sidecar
  # untouched. ProtectSystem=strict later makes this tree read-only to the service.
  log "Staging sidecar source and virtualenv in release ${RELEASE_ID}"
  run_root install -d -m 0755 "$release_source"
  run_root install -m 0644 sidecar/requirements.txt "$release_source/requirements.txt"
  run_root install -m 0755 sidecar/curl_cffi_sidecar.py "$release_source/curl_cffi_sidecar.py"
  printf '%s\n' "$new_hash" | run_root tee "$release_source/.source-hash" >/dev/null
  if [[ "$SIDECAR_CHANGED" == 0 && -x "${APP_DIR%/}/current/sidecar-venv/bin/python" ]]; then
    log "Cloning unchanged sidecar venv into release staging"
    run_root install -d -m 0755 "$release_venv"
    run_root cp -a "${APP_DIR%/}/current/sidecar-venv/." "$release_venv/"
  else
    run_root python3 -m venv "$release_venv"
    run_root "$release_venv/bin/pip" install --upgrade pip
    run_root "$release_venv/bin/pip" install -r "$release_source/requirements.txt"
  fi
  run_root env PYTHONDONTWRITEBYTECODE=1 "$release_venv/bin/python" "$release_source/curl_cffi_sidecar.py" --selftest

  # Defaults become release-scoped. Explicit operator paths remain supported, but the
  # independently staged release copy above is always complete and self-tested.
  if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == 0 ]]; then SIDECAR_INSTALL_DIR="$release_source"; fi
  if [[ "$SIDECAR_VENV_EXPLICIT" == 0 ]]; then SIDECAR_VENV="$release_venv"; fi
  run_root chown -R "root:${SERVICE_GROUP}" "$release_source" "$release_venv"
  run_root chmod -R g+rX "$release_source" "$release_venv"
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$SIDECAR_COOKIE_DIR"

  # If either runtime location was explicitly overridden, keep that runtime populated
  # too while retaining the release-local rollback artifact.
  if [[ "$SIDECAR_INSTALL_DIR" != "$release_source" || "$SIDECAR_VENV" != "$release_venv" ]]; then
    hash_file="${SIDECAR_INSTALL_DIR%/}/.source-hash"
    runtime_hash="$(run_root cat "$hash_file" 2>/dev/null || true)"
    if [[ "$runtime_hash" != "$new_hash" || ! -x "${SIDECAR_VENV}/bin/python" ]]; then
      run_root install -d -m 0755 "$SIDECAR_INSTALL_DIR"
      run_root install -m 0644 sidecar/requirements.txt "${SIDECAR_INSTALL_DIR}/requirements.txt"
      run_root install -m 0755 sidecar/curl_cffi_sidecar.py "${SIDECAR_INSTALL_DIR}/curl_cffi_sidecar.py"
      run_root python3 -m venv "$SIDECAR_VENV"
      run_root "${SIDECAR_VENV}/bin/pip" install --upgrade pip
      run_root "${SIDECAR_VENV}/bin/pip" install -r "${SIDECAR_INSTALL_DIR}/requirements.txt"
      run_root env PYTHONDONTWRITEBYTECODE=1 "${SIDECAR_VENV}/bin/python" "${SIDECAR_INSTALL_DIR}/curl_cffi_sidecar.py" --selftest
      printf '%s\n' "$new_hash" | run_root tee "$hash_file" >/dev/null
    fi
  fi
  if [[ "$SIDECAR_INSTALL_DIR_EXPLICIT" == 0 ]]; then SIDECAR_INSTALL_DIR="${APP_DIR%/}/current/sidecar"; fi
  if [[ "$SIDECAR_VENV_EXPLICIT" == 0 ]]; then SIDECAR_VENV="${APP_DIR%/}/current/sidecar-venv"; fi
}

# ── Repository-owned Node auto-registration engine ──────────────────────────────────
# Installs Node.js + a Chrome/Chromium browser + Xvfb + the puppeteer-real-browser
# registrar so the pool can auto-register accounts on a NO-DISPLAY cloud VPS. The Go
# orchestrator (internal/registration/pipeline/registrar_node.go) launches the registrar
# HEADED inside a per-process Xvfb (it strips DISPLAY/REG_HEADLESS from the worker env) —
# OpenAI's signup page serves the form to a headed browser but withholds it from a pure
# --headless one, so headed-in-Xvfb is what actually works headless on a server.

node_version_major() { "$1" --version 2>/dev/null | sed 's/^v//; s/\..*//'; }

find_node() {
  if [[ -n "${NODE_BIN:-}" && -x "${NODE_BIN:-}" ]]; then printf '%s\n' "$NODE_BIN"; return 0; fi
  command -v node 2>/dev/null || return 1
}

find_npm() {
  if [[ -n "${NODE_BIN:-}" && -x "$(dirname "$NODE_BIN")/npm" ]]; then
    printf '%s\n' "$(dirname "$NODE_BIN")/npm"; return 0
  fi
  command -v npm 2>/dev/null || return 1
}

install_nodejs() {
  bool_enabled "$WITH_REGISTRATION" || return 0
  local nb maj
  if nb="$(find_node)"; then
    maj="$(node_version_major "$nb")"
    if [[ "$maj" =~ ^[0-9]+$ && "$maj" -ge "$NODE_MIN_MAJOR" ]]; then
      NODE_BIN="$nb"; log "Using Node $("$nb" --version 2>/dev/null): ${NODE_BIN}"; return 0
    fi
    warn "Node $("$nb" --version 2>/dev/null) is older than v${NODE_MIN_MAJOR}; installing Node ${NODE_INSTALL_MAJOR}.x"
  else
    warn "Node.js not found; installing Node ${NODE_INSTALL_MAJOR}.x"
  fi
  if command -v apt-get >/dev/null 2>&1; then
    run_root sh -c "curl -fsSL https://deb.nodesource.com/setup_${NODE_INSTALL_MAJOR}.x | bash -"
    run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y nodejs
  elif command -v dnf >/dev/null 2>&1; then
    run_root sh -c "curl -fsSL https://rpm.nodesource.com/setup_${NODE_INSTALL_MAJOR}.x | bash -"
    run_root dnf install -y nodejs
  elif command -v yum >/dev/null 2>&1; then
    run_root sh -c "curl -fsSL https://rpm.nodesource.com/setup_${NODE_INSTALL_MAJOR}.x | bash -"
    run_root yum install -y nodejs
  elif command -v apk >/dev/null 2>&1; then
    run_root apk add --no-cache nodejs npm
  elif command -v pacman >/dev/null 2>&1; then
    run_root pacman -Sy --needed --noconfirm nodejs npm
  else
    die "cannot auto-install Node.js; install Node >= v${NODE_MIN_MAJOR} manually or rerun with --without-registration"
  fi
  nb="$(find_node)" || die "Node.js installation failed"
  maj="$(node_version_major "$nb")"
  [[ "$maj" =~ ^[0-9]+$ && "$maj" -ge "$NODE_MIN_MAJOR" ]] || die "installed Node ($("$nb" --version 2>/dev/null)) is older than v${NODE_MIN_MAJOR}"
  NODE_BIN="$nb"
  log "Using Node $("$nb" --version 2>/dev/null): ${NODE_BIN}"
}

find_chrome() {
  local c
  if [[ -n "${CHROME_BIN:-}" && -x "${CHROME_BIN:-}" ]]; then
    printf '%s\n' "$CHROME_BIN"
    return 0
  fi
  for c in google-chrome-stable google-chrome chromium chromium-browser; do
    if command -v "$c" >/dev/null 2>&1; then command -v "$c"; return 0; fi
  done
  for c in /usr/bin/google-chrome-stable /usr/bin/google-chrome /usr/bin/chromium /usr/bin/chromium-browser /snap/bin/chromium; do
    [[ -x "$c" ]] && { printf '%s\n' "$c"; return 0; }
  done
  return 1
}

install_chrome() {
  bool_enabled "$WITH_REGISTRATION" || return 0

  # Xvfb + base fonts first: Chrome runs HEADED inside a per-process Xvfb (no monitor
  # required), so a virtual display + fonts must exist even on a headless VPS.
  if bool_enabled "$SKIP_OS_PACKAGES"; then
    log "Skipping Xvfb/font package changes (SKIP_OS_PACKAGES=1)"
  else
    log "Installing Xvfb + fonts (virtual display for the headed-in-Xvfb browser)"
  if command -v apt-get >/dev/null 2>&1; then
    run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends xvfb fonts-liberation || warn "could not install xvfb/fonts via apt"
  elif command -v dnf >/dev/null 2>&1; then
    run_root dnf install -y xorg-x11-server-Xvfb liberation-fonts || warn "could not install Xvfb/fonts via dnf"
  elif command -v yum >/dev/null 2>&1; then
    run_root yum install -y xorg-x11-server-Xvfb liberation-fonts || warn "could not install Xvfb/fonts via yum"
  elif command -v apk >/dev/null 2>&1; then
    run_root apk add --no-cache xvfb ttf-freefont || warn "could not install xvfb/fonts via apk"
  elif command -v pacman >/dev/null 2>&1; then
    run_root pacman -Sy --needed --noconfirm xorg-server-xvfb ttf-liberation || warn "could not install xvfb/fonts via pacman"
  fi

  fi

  local chrome arch
  if chrome="$(find_chrome)"; then CHROME_BIN="$chrome"; log "Using browser: ${CHROME_BIN}"; return 0; fi
  arch="$(uname -m)"
  log "Installing a Chrome/Chromium browser for the registrar (arch ${arch})"
  if command -v apt-get >/dev/null 2>&1; then
    if [[ "$arch" == "x86_64" || "$arch" == "amd64" ]]; then
      local tmp; tmp="$(mktemp -d)"
      if curl -fsSL --retry 3 -o "${tmp}/chrome.deb" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb; then
        run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y "${tmp}/chrome.deb" \
          || run_root sh -c "dpkg -i '${tmp}/chrome.deb' || DEBIAN_FRONTEND=noninteractive apt-get -f install -y"
      else
        warn "Google Chrome .deb download failed; falling back to Chromium"
        run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y chromium || run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y chromium-browser || true
      fi
      rm -rf "$tmp"
    else
      run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y chromium || run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y chromium-browser || true
    fi
  elif command -v dnf >/dev/null 2>&1; then
    run_root dnf install -y chromium || true
  elif command -v yum >/dev/null 2>&1; then
    run_root yum install -y chromium || true
  elif command -v apk >/dev/null 2>&1; then
    run_root apk add --no-cache chromium || true
  elif command -v pacman >/dev/null 2>&1; then
    run_root pacman -Sy --needed --noconfirm chromium || true
  fi
  chrome="$(find_chrome)" || die "could not install a Chrome/Chromium browser; install google-chrome-stable manually or rerun with --without-registration"
  CHROME_BIN="$chrome"
  log "Using browser: ${CHROME_BIN}"
}

install_node_registrar() {
  bool_enabled "$WITH_REGISTRATION" || return 0
  if [[ ! -d "$REGISTRAR_SOURCE" ]]; then
    warn "Node registrar source not found at ${REGISTRAR_SOURCE}; the node auto-registration engine will be unavailable (the relay still runs and serves accounts)."
    return 0
  fi
  local npm_bin release_install tmp
  npm_bin="$(find_npm)" || die "npm not found after Node install"

  release_install="${RELEASE_DIR%/}/registrar-node"
  tmp="${release_install}.tmp"
  log "Installing immutable Node registrar to ${release_install}"
  run_root rm -rf "$tmp"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp"
  # Copy source only — never node_modules, browser profiles, logs, tokens, or the
  # operator's filled-in config*.json (secrets stay out of the runtime tree; registrar
  # creds are supplied via the admin UI "node_registrar_config" setting at runtime).
  run_root sh -c "cd '$REGISTRAR_SOURCE' && tar \
      --exclude=./node_modules --exclude=./logs --exclude=./tokens \
      --exclude=./browser-profile --exclude='./*.log' \
      --exclude=./config.json --exclude=./config.server.json --exclude=./config.local.json \
      --exclude='./config.server.json.bak*' --exclude=./auth.json --exclude=./accounts.json \
      --exclude=./username.json --exclude=./shibai.json \
      -cf - . | tar -xf - -C '$tmp'"
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$tmp"
  run_root rm -rf "$release_install"
  run_root mv "$tmp" "$release_install"

  log "Installing Node registrar dependencies (pulls puppeteer-real-browser; may take a minute)"
  local npm_common="--no-audit --no-fund --omit=dev"
  run_root env HOME="$DATA_DIR" sh -c "cd '$release_install' && '$npm_bin' ci $npm_common"
  run_root chown -R "root:${SERVICE_GROUP}" "$release_install"
  run_root chmod -R u=rwX,g=rX,o= "$release_install"

  if (( REGISTRAR_INSTALL_EXPLICIT == 1 )); then
    tmp="${REGISTRAR_INSTALL}.tmp"
    run_root rm -rf "$tmp"
    run_root cp -a "$release_install" "$tmp"
    run_root rm -rf "$REGISTRAR_INSTALL"
    run_root mv "$tmp" "$REGISTRAR_INSTALL"
  else
    REGISTRAR_INSTALL="$release_install"
  fi
  log "Node registrar ready at ${REGISTRAR_INSTALL}"
}

install_python_registrar() {
  bool_enabled "$WITH_REGISTRATION" || return 0

  local release_source release_venv reauth_source reauth_module_dir registrar_file
  release_source="${RELEASE_DIR%/}/registrar-python"
  release_venv="${RELEASE_DIR%/}/registrar-python-venv"
  reauth_source="${RELEASE_DIR%/}/codex-reauth"
  reauth_module_dir="${reauth_source%/}/codex_register"
  log "Installing immutable Python registrar runtime to ${release_source}"

  run_root install -d -m 0750 -o root -g "$SERVICE_GROUP" "$release_source"
  for registrar_file in protocol_register.py browser_register.py reg_v3.py phone_verify.py requirements.txt; do
    run_root install -m 0640 -o root -g "$SERVICE_GROUP" \
      "${PY_REGISTRAR_SOURCE%/}/${registrar_file}" "${release_source}/${registrar_file}"
  done
  log "Installing immutable Codex OAuth reauth worker to ${reauth_source}"
  run_root install -d -m 0750 -o root -g "$SERVICE_GROUP" "$reauth_source" "$reauth_module_dir"
  run_root install -m 0750 -o root -g "$SERVICE_GROUP" \
    "$CODEX_REAUTH_WORKER_SOURCE" "${reauth_source}/codex_reauth_worker.py"
  run_root install -m 0750 -o root -g "$SERVICE_GROUP" \
    "${PY_REGISTRAR_SOURCE%/}/login_oauth.py" "${reauth_module_dir}/login_oauth.py"
  run_root install -m 0640 -o root -g "$SERVICE_GROUP" \
    "${PY_REGISTRAR_SOURCE%/}/phone_verify.py" "${reauth_module_dir}/phone_verify.py"

  run_root python3 -m venv "$release_venv"
  run_root "$release_venv/bin/pip" install --disable-pip-version-check \
    --requirement "${release_source}/requirements.txt"
  run_root "$release_venv/bin/pip" check
  for registrar_file in protocol_register.py browser_register.py reg_v3.py phone_verify.py; do
    run_root env PYTHONDONTWRITEBYTECODE=1 "$release_venv/bin/python" -c \
      'import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"), filename=sys.argv[1])' \
      "${release_source}/${registrar_file}"
  done
  for registrar_file in "${reauth_source}/codex_reauth_worker.py" "${reauth_module_dir}/login_oauth.py" "${reauth_module_dir}/phone_verify.py"; do
    run_root env PYTHONDONTWRITEBYTECODE=1 "$release_venv/bin/python" -c \
      'import ast, pathlib, sys; ast.parse(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"), filename=sys.argv[1])' \
      "$registrar_file"
  done
  run_root env PYTHONDONTWRITEBYTECODE=1 "$release_venv/bin/python" -c \
    'import curl_cffi, playwright, playwright_stealth, requests, urllib3'
  run_root env PYTHONDONTWRITEBYTECODE=1 "$release_venv/bin/python" -c \
    'import importlib.util, pathlib, sys; p=pathlib.Path(sys.argv[1]); s=importlib.util.spec_from_file_location("codex_reauth_worker", p); m=importlib.util.module_from_spec(s); s.loader.exec_module(m); assert m.make_server' \
    "${reauth_source}/codex_reauth_worker.py"

  run_root chown -R "root:${SERVICE_GROUP}" "$release_source" "$release_venv" "$reauth_source"
  run_root chmod -R u=rwX,g=rX,o= "$release_source" "$release_venv" "$reauth_source"
  log "Python registration workers ready at ${release_source}"
  log "Codex OAuth reauth worker ready at ${reauth_source}"
}

# install_warp provisions the multi-exit WARP fallback pool (wgcf + wireproxy): one
# independent WARP identity per exit, each served as a local SOCKS5 listener by its own
# systemd-managed wireproxy instance (codex-pool-warp@<i>). It also installs a scoped
# sudoers rule so the unprivileged server can restart an exit after re-registering it
# for a fresh IP (the CF-on-WARP recovery step).
install_warp() {
  bool_enabled "$WITH_WARP" || return 0
  [[ "$WARP_EXITS" =~ ^[0-9]+$ && "$WARP_EXITS" -ge 1 ]] || die "--warp-exits must be a positive integer (got: ${WARP_EXITS})"

  local warp_dir script_dst wireproxy_bin
  warp_dir="${WARP_DIR:-${DATA_DIR%/}/warp}"
  script_dst="${warp_dir}/warp-exit.sh"
  wireproxy_bin="${warp_dir}/bin/wireproxy"

  log "Installing WARP fallback pool: ${WARP_EXITS} exits from base port ${WARP_BASE_PORT} (≤${WARP_ACCOUNTS_PER_EXIT} accounts/exit)"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$warp_dir" "${warp_dir}/bin"
  run_root install -m 0755 scripts/warp-exit.sh "$script_dst"

  # Provision identities + wireproxy configs, then chown to the service user so the
  # server can re-register an exit later. wgcf needs outbound HTTPS to Cloudflare.
  run_root env WARP_DIR="$warp_dir" WARP_BASE_PORT="$WARP_BASE_PORT" SERVICE_NAME="$SERVICE_NAME" \
    "$script_dst" provision "$WARP_EXITS" || warn "WARP provisioning reported errors (check outbound connectivity to Cloudflare)"
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$warp_dir"

  if ! { systemd_requested && systemd_running; }; then
    warn "systemd not active; WARP wireproxy instances not registered as services."
    warn "Run each manually: ${wireproxy_bin} -c ${warp_dir}/exit-<i>/wireproxy.conf"
    return 0
  fi

  local tmp unit i
  tmp="$(mktemp)"
  unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-warp@.service"
  cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool WARP exit %i (wireproxy)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
ExecStart=${wireproxy_bin} -c ${warp_dir}/exit-%i/wireproxy.conf
Restart=always
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
  log "Installing systemd unit to ${unit}"
  run_root install -m 0644 "$tmp" "$unit"
  rm -f "$tmp"
  run_root systemctl daemon-reload
  for (( i=1; i<=WARP_EXITS; i++ )); do
    run_root systemctl enable "${SERVICE_NAME}-warp@${i}.service" >/dev/null 2>&1 || true
    if bool_enabled "$START_SERVICE"; then
      run_root systemctl restart "${SERVICE_NAME}-warp@${i}.service" || warn "WARP exit ${i} failed to start"
    fi
  done

  # Scoped sudoers so the server (via warp-exit.sh) can restart an exit after a
  # re-registration. The main service unit drops NoNewPrivileges when WARP is enabled
  # (see install_systemd_unit) so this sudo is permitted.
  local sysctl_bin sudoers
  sysctl_bin="$(command -v systemctl || echo /usr/bin/systemctl)"
  sudoers="/etc/sudoers.d/${SERVICE_NAME}-warp"
  tmp="$(mktemp)"
  printf '%s ALL=(root) NOPASSWD: %s restart %s-warp@*.service\n' "$SERVICE_USER" "$sysctl_bin" "$SERVICE_NAME" >"$tmp"
  if command -v visudo >/dev/null 2>&1 && ! visudo -cf "$tmp" >/dev/null 2>&1; then
    warn "generated sudoers rule failed validation; skipping (auto re-registration will need a manual exit restart)"
  else
    run_root install -m 0440 "$tmp" "$sudoers"
    log "Installed sudoers drop-in ${sudoers}"
  fi
  rm -f "$tmp"
}

open_firewall_port() {
  bool_enabled "$OPEN_FIREWALL" || return 0

  local port
  port="$(listen_port "$LISTEN_ADDR")"
  if [[ ! "$port" =~ ^[0-9]+$ ]]; then
    warn "Cannot infer TCP port from LISTEN_ADDR=${LISTEN_ADDR}; skipping firewall update."
    return 0
  fi

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -qi '^Status: active'; then
    log "Opening TCP port ${port} with ufw"
    run_root ufw allow "${port}/tcp"
    return 0
  fi
  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    log "Opening TCP port ${port} with firewalld"
    run_root firewall-cmd --add-port="${port}/tcp" --permanent
    run_root firewall-cmd --reload
    return 0
  fi

  warn "OPEN_FIREWALL=1 was requested, but no active ufw/firewalld was detected."
  warn "Open TCP port ${port} manually if your host or cloud firewall blocks inbound HTTP."
}

frontend_url_hint() {
  if [[ -n "$PUBLIC_URL" ]]; then
    printf '%s\n' "$PUBLIC_URL"
    return 0
  fi

  local host port
  host="$(listen_host "$LISTEN_ADDR")"
  port="$(listen_port "$LISTEN_ADDR")"
  case "$host" in
    ""|"0.0.0.0"|"::") host="<server-ip>" ;;
  esac
  if [[ "$host" == *:* && "$host" != \[*\] ]]; then
    host="[${host}]"
  fi
  if [[ "$port" =~ ^[0-9]+$ ]]; then
    printf 'http://%s:%s/\n' "$host" "$port"
  else
    printf 'http://%s/\n' "$host"
  fi
}

print_summary() {
  local sidecar_summary migration_summary warp_summary super_instruct_summary frontend_url manual_admin_env admin_token_summary reauth_manual
  if bool_enabled "$WITH_SIDECAR"; then
    sidecar_summary="${SERVICE_NAME}-sidecar.service (${SIDECAR_ADDR})"
  else
    sidecar_summary="disabled"
  fi
  local registration_summary
  if bool_enabled "$WITH_REGISTRATION"; then
    registration_summary="node engine @ ${REGISTRAR_INSTALL}; OAuth reauth @ $(codex_reauth_base_url) (node=${NODE_BIN:-?}, chrome=${CHROME_BIN:-?})"
    reauth_manual="
  ${RELEASE_DIR%/}/registrar-python-venv/bin/python ${RELEASE_DIR%/}/codex-reauth/codex_reauth_worker.py --host $(listen_host "$CODEX_REAUTH_ADDR") --port $(listen_port "$CODEX_REAUTH_ADDR") --concurrency ${CODEX_REAUTH_CONCURRENCY}"
  else
    registration_summary="disabled"
    reauth_manual=""
  fi
  if bool_enabled "$WITH_WARP"; then
    warp_summary="${WARP_EXITS} exits, ports ${WARP_BASE_PORT}-$(( WARP_BASE_PORT + WARP_EXITS - 1 )) (${SERVICE_NAME}-warp@1..${WARP_EXITS}), ≤${WARP_ACCOUNTS_PER_EXIT}/exit"
  else
    warp_summary="disabled"
  fi
  if bool_enabled "$MIGRATE_USER_GROUPS"; then
    migration_summary="enabled (missing account-pool groups will be copied)"
  else
    migration_summary="disabled (account-pool groups stay separate)"
  fi
  case "$SUPER_INSTRUCT_SOURCE_KIND" in
    bundled) super_instruct_summary="bundled; enabled per API-key install choice within user-group policy" ;;
    preserved) super_instruct_summary="preserved from the previously active release" ;;
    *) super_instruct_summary="unavailable (internal Markdown resources are not part of this source)" ;;
  esac
  frontend_url="$(frontend_url_hint)"
  manual_admin_env=""
  admin_token_summary="<empty>"
  if [[ -n "$ADMIN_TOKEN" ]]; then
    manual_admin_env=" CODEX_POOL_ADMIN_TOKEN_FILE=${DATA_DIR%/}/data/keys/admin.token"
    admin_token_summary="<configured in ${DATA_DIR%/}/data/keys/admin.token>"
  fi

  cat <<EOF

Install complete.

Binary:        ${BIN_DIR}/${APP_NAME}
Handoff:       ${BIN_DIR}/${HANDOFF_NAME}
Release:       ${RELEASE_ID} (${RELEASE_DIR})
Context clear: ${BIN_DIR}/codex-pool-clear-context
Config:        ${CONFIG_FILE}
Data:          ${DATA_DIR%/}/data
Database:      ${DATABASE_PATH}
Listen:        ${LISTEN_ADDR}
Frontend:      ${frontend_url}
Handoff svc:  ${HANDOFF_SERVICE_NAME}.service
Compat svc:   ${SERVICE_NAME}.service
Sidecar:       ${sidecar_summary}
Super-Instruct: ${super_instruct_summary}
Registration:  ${registration_summary}
Group migration: ${migration_summary}
WARP:          ${warp_summary}
Admin token:   ${admin_token_summary}

Manual run:
  CODEX_POOL_DATABASE=${DATABASE_PATH} CODEX_POOL_DATA_DIR=${DATA_DIR%/}/data CODEX_POOL_MIGRATE_USER_GROUPS=${MIGRATE_USER_GROUPS} CODEX_POOL_LISTEN_ADDR=${LISTEN_ADDR} CODEX_POOL_SUPER_INSTRUCT_DIR=${APP_DIR%/}/current/super-instruct/codex-skills CODEX_POOL_SUPER_INSTRUCT_BRIDGE_FILE=${APP_DIR%/}/current/super-instruct/bridge.md${manual_admin_env} ${BIN_DIR}/${APP_NAME} --config ${CONFIG_FILE}${reauth_manual}

Useful service commands:
  systemctl status ${HANDOFF_SERVICE_NAME}.service
  systemctl status ${SERVICE_NAME}.socket
  systemctl status ${SERVICE_NAME}-worker@${RELEASE_ID}.service
  systemctl status ${SERVICE_NAME}-reauth.service
  ${BIN_DIR}/codex-pool-rollback
  journalctl -u ${HANDOFF_SERVICE_NAME}.service -f
  systemctl status ${SERVICE_NAME}-sidecar.service
EOF
}

main() {
  apply_port_overrides
  normalize_migrate_user_groups
  ensure_absolute_paths
  validate_deployment_limits
  if (( STATUS_ONLY == 1 )); then
    collect_deployment_storage status
    print_deployment_storage_status
    return 0
  fi
  ensure_project_files
  validate_codex_reauth_settings
  acquire_deploy_lock
  remove_legacy_auxiliary_units
  trap deployment_exit_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
  log "Codex skills compatibility: full official skills/plugins/Browser Use support requires the official Codex account path; custom providers are best-effort."
  cleanup_abandoned_deployment_artifacts
  prune_console_generations || die "console generation pruning could not satisfy its byte budget"
  if [[ -d "$BUILD_DIR" ]]; then
    log "Removing previous release build artifacts ${BUILD_DIR}"
    run_root rm -rf -- "$BUILD_DIR"
  fi
  reclaim_build_caches_under_pressure prebuild
  enforce_deployment_budget prebuild
  ensure_system_deps
  adopt_interrupted_install_pause
  ensure_go
  build_project
  enforce_deployment_budget prebuild
  resolve_requested_egress_ports
  prepare_runtime_layout
  install_binary_and_config
  run_expand_only_migration
  install_sidecar
  install_nodejs
  install_chrome
  install_node_registrar
  install_python_registrar
  install_warp
  enforce_deployment_budget poststage
  printf '%s\n' "$RELEASE_ID" | run_root tee "${RELEASE_DIR%/}/.staged-ok" >/dev/null
  install_systemd_unit
  open_firewall_port
  run_root rm -rf -- "$BUILD_DIR"
  collect_deployment_storage status
  write_deployment_storage_state
  print_summary
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
