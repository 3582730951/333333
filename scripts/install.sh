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
WITH_GOPAY="${WITH_GOPAY:-1}"
WITH_WARP="${WITH_WARP:-0}"
WITH_LIFECYCLE="${WITH_LIFECYCLE:-1}"
MIGRATE_USER_GROUPS="${MIGRATE_USER_GROUPS:-0}"
# Node registration engine (other_new_gpt_register): installs Node.js + a headless
# Chrome + Xvfb and the puppeteer-real-browser registrar so the pool can auto-register
# accounts on a no-display cloud VPS. Default on; disable with --without-registration.
WITH_REGISTRATION="${WITH_REGISTRATION:-1}"
NODE_MIN_MAJOR="${NODE_MIN_MAJOR:-18}"
NODE_INSTALL_MAJOR="${NODE_INSTALL_MAJOR:-20}"
REGISTRAR_SOURCE="${REGISTRAR_SOURCE:-${PROJECT_ROOT}/other_new_gpt_register}"
REGISTRAR_INSTALL="${REGISTRAR_INSTALL:-${DATA_DIR%/}/registrar}"
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
GO_INSTALL_VERSION="${GO_INSTALL_VERSION:-1.23.5}"
GO_INSTALL_ROOT="${GO_INSTALL_ROOT:-/usr/local}"
BUILD_DIR="${BUILD_DIR:-${PROJECT_ROOT}/.build}"
SKIP_OS_PACKAGES="${SKIP_OS_PACKAGES:-0}"
SIDECAR_ADDR="${SIDECAR_ADDR:-127.0.0.1:8790}"
GOPAY_SOURCE_DIR="${GOPAY_SOURCE_DIR:-${PROJECT_ROOT}/gopay/plus}"
# HEALTH_TIMEOUT bounds private-worker and post-switch /readyz gates.
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-300}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/var/lock/codex-pool-install.lock}"
HANDOFF_CONTROL_SOCKET="${HANDOFF_CONTROL_SOCKET:-${DATA_DIR%/}/run/handoff-control.sock}"
HANDOFF_PAUSE_STATE="${HANDOFF_PAUSE_STATE:-${DATA_DIR%/}/run/admission-paused.json}"
RELEASE_ID=""
RELEASE_DIR=""
PREVIOUS_RELEASE_ID=""
HANDOFF_PAUSED=0
LEGACY_SERVICE_ACTIVE=0
ACTIVATION_PENDING=0
ACTIVATION_OLD_RELEASE=""
ACTIVATION_OLD_SOCKET=""
ACTIVATION_NEW_RELEASE=""
# Set by install_sidecar: 1 when the sidecar source changed (or first install), 0 when
# unchanged — lets the restart block skip a needless sidecar restart that would sever
# in-flight upstream streams.
SIDECAR_CHANGED=1
OLD_WORKER_DRAINED=1

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
if [[ -n "${GOPAY_INSTALL_DIR:-}" ]]; then
  GOPAY_INSTALL_DIR_EXPLICIT=1
else
  GOPAY_INSTALL_DIR_EXPLICIT=0
  GOPAY_INSTALL_DIR="${DATA_DIR%/}/gopay/plus"
fi
if [[ -n "${GOPAY_VENV:-}" ]]; then
  GOPAY_VENV_EXPLICIT=1
else
  GOPAY_VENV_EXPLICIT=0
  GOPAY_VENV="${DATA_DIR%/}/gopay-venv"
fi

# Lifecycle management services (chatgpt_register + plus_payment)
LIFECYCLE_REGISTER_SOURCE="${LIFECYCLE_REGISTER_SOURCE:-${PROJECT_ROOT}/services/chatgpt_register}"
LIFECYCLE_REGISTER_INSTALL="${LIFECYCLE_REGISTER_INSTALL:-${DATA_DIR%/}/lifecycle/register}"
LIFECYCLE_REGISTER_VENV="${LIFECYCLE_REGISTER_VENV:-${DATA_DIR%/}/lifecycle-register-venv}"
LIFECYCLE_REGISTER_ADDR="${LIFECYCLE_REGISTER_ADDR:-127.0.0.1:8791}"
LIFECYCLE_PAYMENT_SOURCE="${LIFECYCLE_PAYMENT_SOURCE:-${PROJECT_ROOT}/services/plus_payment}"
LIFECYCLE_PAYMENT_INSTALL="${LIFECYCLE_PAYMENT_INSTALL:-${DATA_DIR%/}/lifecycle/payment}"
LIFECYCLE_PAYMENT_VENV="${LIFECYCLE_PAYMENT_VENV:-${DATA_DIR%/}/lifecycle-payment-venv}"
LIFECYCLE_PAYMENT_ADDR="${LIFECYCLE_PAYMENT_ADDR:-127.0.0.1:8792}"
LIFECYCLE_REGISTER_CHANGED=1
LIFECYCLE_PAYMENT_CHANGED=1

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
  --full                    Install all optional bundled Python components (default)
  --minimal                 Install only the Go gateway
  --with-sidecar            Install and manage the curl_cffi sidecar (default)
  --without-sidecar         Do not install the curl_cffi sidecar
  --with-gopay              Install the bundled gopay/plus Python services (default)
  --without-gopay           Do not install the bundled gopay/plus Python services
  --with-lifecycle          Install lifecycle management services (chatgpt_register + plus_payment, default)
  --without-lifecycle       Do not install lifecycle management services
  --with-registration       Install the Node auto-registration engine: Node.js + headless Chrome + Xvfb + the
                            puppeteer-real-browser registrar (other_new_gpt_register). Default. Works on a
                            no-display cloud VPS (Chrome runs headed inside a per-process Xvfb).
  --without-registration    Do not install the Node auto-registration engine
  --migrate-user-groups     Copy missing account-pool groups to same-named user groups on service start
  --no-migrate-user-groups  Keep account-pool groups separate (default)
  --with-warp               Provision the multi-exit WARP CF-fallback pool (wgcf + wireproxy)
  --without-warp            Do not provision WARP (default)
  --warp-exits N            Independent WARP exits to provision (implies --with-warp; default ${WARP_EXITS})
  --cf-solver-url URL       FlareSolverr-compatible cf_clearance solver base URL (enables the solver rung)
  --listen-addr ADDR        HTTP listen address for API + embedded frontend. Default: ${LISTEN_ADDR}
  --admin-token TOKEN       Admin API token. Generated for new externally exposed configs when empty
  --public-url URL          Public frontend URL to print in the install summary
  --open-firewall           Try to open the listen TCP port in ufw/firewalld
  --no-open-firewall        Do not change firewall rules (default)
  --sidecar-addr ADDR       Sidecar listen address. Default: ${SIDECAR_ADDR}
  --without-go-install      Fail instead of installing Go when Go is missing/too old
  --go-version VERSION      Go version to install when needed. Default: ${GO_INSTALL_VERSION}
  -h, --help                Show this help

Environment overrides:
  SERVICE_NAME, SERVICE_USER, SERVICE_GROUP, INSTALL_PREFIX, BIN_DIR, APP_DIR,
  CONFIG_DIR, CONFIG_FILE, DATA_DIR, DATABASE_PATH, SYSTEMD_DIR,
  RUN_TESTS, INSTALL_SYSTEMD, START_SERVICE, WITH_SIDECAR, WITH_GOPAY, WITH_LIFECYCLE,
  MIGRATE_USER_GROUPS,
  WITH_WARP, WARP_EXITS, WARP_BASE_PORT, WARP_ACCOUNTS_PER_EXIT, WARP_DIR, CF_SOLVER_URL,
  LISTEN_ADDR, ADMIN_TOKEN, PUBLIC_URL, OPEN_FIREWALL,
  SIDECAR_ADDR, SIDECAR_VENV, SIDECAR_INSTALL_DIR, SIDECAR_COOKIE_DIR,
  GOPAY_SOURCE_DIR, GOPAY_INSTALL_DIR, GOPAY_VENV,
  LIFECYCLE_REGISTER_SOURCE, LIFECYCLE_REGISTER_INSTALL, LIFECYCLE_REGISTER_VENV, LIFECYCLE_REGISTER_ADDR,
  LIFECYCLE_PAYMENT_SOURCE, LIFECYCLE_PAYMENT_INSTALL, LIFECYCLE_PAYMENT_VENV, LIFECYCLE_PAYMENT_ADDR,
  INSTALL_GO, GO_INSTALL_VERSION, GO_INSTALL_ROOT, GO_BIN, HEALTH_TIMEOUT,
  SKIP_OS_PACKAGES, GO_TARBALL_SHA256,
  WITH_REGISTRATION, NODE_MIN_MAJOR, NODE_INSTALL_MAJOR, NODE_BIN, CHROME_BIN,
  REGISTRAR_SOURCE, REGISTRAR_INSTALL
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
      if [[ "$GOPAY_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        GOPAY_INSTALL_DIR="${DATA_DIR%/}/gopay/plus"
      fi
      if [[ "$GOPAY_VENV_EXPLICIT" == "0" ]]; then
        GOPAY_VENV="${DATA_DIR%/}/gopay-venv"
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
      if [[ "$GOPAY_INSTALL_DIR_EXPLICIT" == "0" ]]; then
        GOPAY_INSTALL_DIR="${DATA_DIR%/}/gopay/plus"
      fi
      if [[ "$GOPAY_VENV_EXPLICIT" == "0" ]]; then
        GOPAY_VENV="${DATA_DIR%/}/gopay-venv"
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
      WITH_GOPAY=1
      WITH_LIFECYCLE=1
      WITH_REGISTRATION=1
      shift
      ;;
    --minimal)
      WITH_SIDECAR=0
      WITH_GOPAY=0
      WITH_LIFECYCLE=0
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
    --with-gopay)
      WITH_GOPAY=1
      shift
      ;;
    --without-gopay)
      WITH_GOPAY=0
      shift
      ;;
    --with-lifecycle)
      WITH_LIFECYCLE=1
      shift
      ;;
    --without-lifecycle)
      WITH_LIFECYCLE=0
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
  local listen_json database_json admin_json sidecar_json gopay_dir_json gopay_python_json
  listen_json="$(json_escape "$LISTEN_ADDR")"
  database_json="$(json_escape "$DATABASE_PATH")"
  admin_json="$(json_escape "$ADMIN_TOKEN")"
  if bool_enabled "$WITH_SIDECAR"; then
    sidecar_json="$(json_escape "http://${SIDECAR_ADDR}")"
  else
    sidecar_json=""
  fi
  if bool_enabled "$WITH_GOPAY"; then
    gopay_dir_json="$(json_escape "$GOPAY_INSTALL_DIR")"
    gopay_python_json="$(json_escape "${GOPAY_VENV}/bin/python")"
  else
    gopay_dir_json="$(json_escape "gopay/plus")"
    gopay_python_json="$(json_escape "python3")"
  fi

  awk \
    -v listen="$listen_json" \
    -v database="$database_json" \
    -v admin="$admin_json" \
    -v sidecar="$sidecar_json" \
    -v gopay_dir="$gopay_dir_json" \
    -v gopay_python="$gopay_python_json" '
      /^[[:space:]]*"listen_addr"[[:space:]]*:/ {
        print "  \"listen_addr\": \"" listen "\","
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
      /^[[:space:]]*"gopay_dir"[[:space:]]*:/ {
        print "  \"gopay_dir\": \"" gopay_dir "\","
        next
      }
      /^[[:space:]]*"gopay_python"[[:space:]]*:/ {
        print "  \"gopay_python\": \"" gopay_python "\","
        next
      }
      { print }
    ' config.example.json
}

python_runtime_enabled() {
  bool_enabled "$WITH_SIDECAR" || bool_enabled "$WITH_GOPAY"
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
    base_pkgs=(ca-certificates curl tar build-essential sqlite3)
    sidecar_pkgs=(python3 python3-venv python3-pip)
    log "Installing OS packages with apt-get"
    run_root env DEBIAN_FRONTEND=noninteractive apt-get update
    if python_runtime_enabled; then
      run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${base_pkgs[@]}"
    fi
  elif command -v dnf >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar gcc gcc-c++ make sqlite)
    sidecar_pkgs=(python3 python3-pip)
    log "Installing OS packages with dnf"
    if python_runtime_enabled; then
      run_root dnf install -y "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root dnf install -y "${base_pkgs[@]}"
    fi
  elif command -v yum >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar gcc gcc-c++ make sqlite)
    sidecar_pkgs=(python3 python3-pip)
    log "Installing OS packages with yum"
    if python_runtime_enabled; then
      run_root yum install -y "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root yum install -y "${base_pkgs[@]}"
    fi
  elif command -v apk >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar build-base sqlite)
    sidecar_pkgs=(python3 py3-pip py3-virtualenv)
    log "Installing OS packages with apk"
    if python_runtime_enabled; then
      run_root apk add --no-cache "${base_pkgs[@]}" "${sidecar_pkgs[@]}"
    else
      run_root apk add --no-cache "${base_pkgs[@]}"
    fi
  elif command -v pacman >/dev/null 2>&1; then
    base_pkgs=(ca-certificates curl tar base-devel sqlite)
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
  if bool_enabled "$WITH_SIDECAR"; then
    [[ -f sidecar/curl_cffi_sidecar.py ]] || die "sidecar/curl_cffi_sidecar.py not found"
    [[ -f sidecar/requirements.txt ]] || die "sidecar/requirements.txt not found"
  fi
  if bool_enabled "$WITH_GOPAY"; then
    [[ -d "$GOPAY_SOURCE_DIR" ]] || die "GoPay bundle not found: ${GOPAY_SOURCE_DIR}. Add gopay/plus or unset --with-gopay."
    [[ -f "${GOPAY_SOURCE_DIR%/}/orchestrator.py" ]] || die "GoPay orchestrator.py not found in ${GOPAY_SOURCE_DIR}"
    [[ -f "${GOPAY_SOURCE_DIR%/}/plus_gopay_links/payment_server.py" ]] || die "GoPay payment_server.py not found in ${GOPAY_SOURCE_DIR}/plus_gopay_links"
    [[ -f "${GOPAY_SOURCE_DIR%/}/requirements.txt" ]] || die "GoPay requirements.txt not found in ${GOPAY_SOURCE_DIR}"
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
  require_absolute_path "GOPAY_SOURCE_DIR" "$GOPAY_SOURCE_DIR"
  require_absolute_path "SIDECAR_INSTALL_DIR" "$SIDECAR_INSTALL_DIR"
  require_absolute_path "SIDECAR_VENV" "$SIDECAR_VENV"
  require_absolute_path "SIDECAR_COOKIE_DIR" "$SIDECAR_COOKIE_DIR"
  require_absolute_path "GOPAY_INSTALL_DIR" "$GOPAY_INSTALL_DIR"
  require_absolute_path "GOPAY_VENV" "$GOPAY_VENV"
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

prepare_runtime_layout() {
  local config_parent database_parent
  config_parent="$(dirname "$CONFIG_FILE")"
  database_parent="$(dirname "$DATABASE_PATH")"

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
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$DATA_DIR"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "${DATA_DIR}/run"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$database_parent"
  if bool_enabled "$WITH_SIDECAR"; then
    run_root install -d -m 0755 "$SIDECAR_INSTALL_DIR"
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$SIDECAR_COOKIE_DIR"
  fi
  if bool_enabled "$WITH_GOPAY"; then
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$(dirname "$GOPAY_INSTALL_DIR")"
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
  run_root install -d -m 0755 "$staging" "$staging/gateway-bin" "$staging/sidecar"
  run_root install -m 0755 "${BUILD_DIR}/${APP_NAME}" "$staging/${APP_NAME}"
  run_root install -m 0755 "${BUILD_DIR}/${HANDOFF_NAME}" "$staging/${HANDOFF_NAME}"
  run_root install -m 0755 "${BUILD_DIR}/gateway-bin"/gateway-* "$staging/gateway-bin/"
  if [[ -f sidecar/curl_cffi_sidecar.py ]]; then
    run_root install -m 0755 sidecar/curl_cffi_sidecar.py "$staging/sidecar/"
    run_root install -m 0644 sidecar/requirements.txt "$staging/sidecar/"
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

acquire_deploy_lock() {
  command -v flock >/dev/null 2>&1 || die "flock is required for serialized deployments"
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

wait_worker_ready() {
  local socket="$1" expected="$2" max="$3" started="$SECONDS" payload
  while (( SECONDS - started < max )); do
    payload="$(curl --noproxy '*' --silent --show-error --max-time 2 --unix-socket "$socket" http://localhost/readyz 2>/dev/null || true)"
    if [[ "$payload" == *'"ready":true'* && "$payload" == *"\"release_id\":\"${expected}\""* ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

worker_inflight() {
  local socket="$1" payload
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$socket" http://localhost/readyz 2>/dev/null || true)"
  printf '%s' "$payload" | sed -n 's/.*"inflight":[[:space:]]*\([0-9][0-9]*\).*/\1/p'
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
}

resume_handoff_admission() {
  local payload
  (( HANDOFF_PAUSED == 1 )) || return 0
  payload="$(handoff_control_post resume 2>/dev/null || true)"
  if [[ "$payload" == *'"admission_paused":false'* ]]; then
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

rollback_pending_activation() {
  (( ACTIVATION_PENDING == 1 )) || return 0
  warn "Rolling back the incomplete traffic switch before reopening admission"
  if [[ -n "$ACTIVATION_OLD_RELEASE" && -n "$ACTIVATION_OLD_SOCKET" ]]; then
    atomic_symlink "$ACTIVATION_OLD_RELEASE" "${APP_DIR%/}/current" || return 1
    atomic_symlink "$ACTIVATION_OLD_SOCKET" "${DATA_DIR%/}/run/active-worker.sock" || return 1
  fi
  if [[ -n "$ACTIVATION_NEW_RELEASE" ]]; then
    run_root systemctl stop "${SERVICE_NAME}-worker@${ACTIVATION_NEW_RELEASE}.service" >/dev/null 2>&1 || true
  fi
  ACTIVATION_PENDING=0
}

deployment_exit_cleanup() {
  local status="$?"
  if (( ACTIVATION_PENDING == 1 )); then
    set +e
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
    run_root systemctl stop --no-block "${SERVICE_NAME}.service" || true
    OLD_WORKER_DRAINED=0
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
  local old_socket old_release inflight started health_url had_handoff=0
  [[ "$DRAIN_TIMEOUT" =~ ^[0-9]+$ ]] || die "DRAIN_TIMEOUT must be a non-negative integer: ${DRAIN_TIMEOUT}"
  (( ${#new_socket} < 104 )) || die "worker Unix socket path is too long: ${new_socket}"
  old_socket="$(run_root readlink "${DATA_DIR%/}/run/active-worker.sock" 2>/dev/null || true)"
  old_release="$(run_root readlink "${APP_DIR%/}/current" 2>/dev/null || true)"

  log "Starting staged worker ${RELEASE_ID} on ${new_socket}"
  if ! run_root systemctl start "${SERVICE_NAME}-worker@${RELEASE_ID}.service"; then
    run_root systemctl --no-pager --full status "${SERVICE_NAME}-worker@${RELEASE_ID}.service" >&2 || true
    die "could not start staged worker ${RELEASE_ID}; active release was not changed"
  fi
  if ! wait_worker_ready "$new_socket" "$RELEASE_ID" "$HEALTH_TIMEOUT"; then
    run_root systemctl --no-pager --full status "${SERVICE_NAME}-worker@${RELEASE_ID}.service" >&2 || true
    run_root systemctl stop "${SERVICE_NAME}-worker@${RELEASE_ID}.service" || true
    die "staged worker ${RELEASE_ID} did not pass /readyz; active release was not changed"
  fi

  ACTIVATION_PENDING=1
  ACTIVATION_OLD_RELEASE="$old_release"
  ACTIVATION_OLD_SOCKET="$old_socket"
  ACTIVATION_NEW_RELEASE="$RELEASE_ID"
  # The new worker is fully ready before the admission barrier closes. Requests which
  # began earlier remain pinned to the old worker; only not-yet-admitted requests wait.
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

  resume_handoff_admission || die "release switched but queued request admission did not resume"
  ACTIVATION_PENDING=0
  log "Release ${RELEASE_ID} is active; stable public listener was not restarted"

  if [[ -n "$old_socket" && "$old_socket" != "$new_socket" ]]; then
    OLD_WORKER_DRAINED=0
    started="$SECONDS"
    while (( SECONDS - started < DRAIN_TIMEOUT )); do
      inflight="$(worker_inflight "$old_socket")"
      if [[ "$inflight" == "0" ]]; then
        OLD_WORKER_DRAINED=1
        old_release="${old_socket##*/worker-}"
        old_release="${old_release%.sock}"
        log "Previous worker ${old_release} drained; stopping it"
        run_root systemctl stop "${SERVICE_NAME}-worker@${old_release}.service" || true
        return 0
      fi
      sleep 1
    done
    warn "previous worker still has ${inflight:-unknown} in-flight connection(s); leaving it alive to finish without interruption"
  fi
}

install_systemd_unit() {
  if ! systemd_requested; then
    warn "Skipping systemd unit install"
    atomic_symlink "$RELEASE_DIR" "${APP_DIR%/}/current"
    return 0
  fi

  local database_parent read_write_paths tmp unit handoff_unit worker_unit sidecar_unit extra_env admin_env no_new_priv warp_dir_env legacy_pid
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
  if bool_enabled "$WITH_GOPAY"; then
    extra_env="${extra_env}
Environment=\"CODEX_POOL_GOPAY_DIR=${GOPAY_INSTALL_DIR}\"
Environment=\"CODEX_POOL_GOPAY_PYTHON=${GOPAY_VENV}/bin/python\""
  fi
  if bool_enabled "$WITH_SIDECAR"; then
    extra_env="${extra_env}
Environment=\"CODEX_POOL_DEFAULT_SIDECAR_ENDPOINT=http://${SIDECAR_ADDR}\""
  fi
  if bool_enabled "$WITH_REGISTRATION"; then
    # Point the Go orchestrator at the installed Node registrar + Node binary; Chrome is
    # auto-detected by puppeteer-real-browser but we pass CHROME_PATH as a backstop. The
    # orchestrator strips DISPLAY/REG_HEADLESS itself, so headed-in-Xvfb works with no
    # display on the server.
    extra_env="${extra_env}
Environment=\"CODEX_REG_NODE_DIR=${REGISTRAR_INSTALL}\""
    if [[ -n "${NODE_BIN:-}" ]]; then
      extra_env="${extra_env}
Environment=\"CODEX_REG_NODE=${NODE_BIN}\""
    fi
    if [[ -n "${CHROME_BIN:-}" ]]; then
      extra_env="${extra_env}
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
Environment=\"CODEX_POOL_ADMIN_TOKEN=${ADMIN_TOKEN}\""
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
Environment="CODEX_POOL_RELEASE_ID=%i"${admin_env}${extra_env}
ExecStart=${APP_DIR%/}/releases/%i/${APP_NAME} --config ${CONFIG_FILE} --release-id %i --unix-socket ${DATA_DIR%/}/run/worker-%i.sock
Restart=on-failure
RestartSec=3
TimeoutStopSec=300
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

  # Lifecycle management services (chatgpt_register + plus_payment)
  if bool_enabled "$WITH_LIFECYCLE"; then
    # Register service
    if [[ -d "$LIFECYCLE_REGISTER_INSTALL" ]]; then
      tmp="$(mktemp)"
      local register_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-register.service"
      cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool Lifecycle Register Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${LIFECYCLE_REGISTER_INSTALL}
Environment="FLASK_HOST=127.0.0.1"
Environment="FLASK_PORT=${LIFECYCLE_REGISTER_ADDR##*:}"
ExecStart=${LIFECYCLE_REGISTER_VENV}/bin/python ${LIFECYCLE_REGISTER_INSTALL}/register_service.py
Restart=always
RestartSec=3
TimeoutStopSec=30
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
      log "Installing systemd unit to ${register_unit}"
      run_root install -m 0644 "$tmp" "$register_unit"
      rm -f "$tmp"
    fi

    # Payment service
    if [[ -d "$LIFECYCLE_PAYMENT_INSTALL" ]]; then
      tmp="$(mktemp)"
      local payment_unit="${SYSTEMD_DIR%/}/${SERVICE_NAME}-payment.service"
      cat >"$tmp" <<EOF
[Unit]
Description=Codex Pool Lifecycle Payment Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_GROUP}
WorkingDirectory=${LIFECYCLE_PAYMENT_INSTALL}
Environment="FLASK_HOST=127.0.0.1"
Environment="FLASK_PORT=${LIFECYCLE_PAYMENT_ADDR##*:}"
ExecStart=${LIFECYCLE_PAYMENT_VENV}/bin/python ${LIFECYCLE_PAYMENT_INSTALL}/payment_service.py
Restart=always
RestartSec=3
TimeoutStopSec=30
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF
      log "Installing systemd unit to ${payment_unit}"
      run_root install -m 0644 "$tmp" "$payment_unit"
      rm -f "$tmp"
    fi
  fi

  if systemd_running; then
    run_root systemctl daemon-reload
    run_root systemctl enable "${SERVICE_NAME}.socket" >/dev/null 2>&1 || true
    run_root systemctl enable "${HANDOFF_SERVICE_NAME}.service" >/dev/null 2>&1 || true
    run_root systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
    if bool_enabled "$WITH_SIDECAR"; then
      run_root systemctl enable "${SERVICE_NAME}-sidecar.service" >/dev/null 2>&1 || true
    fi
    if bool_enabled "$WITH_LIFECYCLE"; then
      if [[ -f "${SYSTEMD_DIR%/}/${SERVICE_NAME}-register.service" ]]; then
        run_root systemctl enable "${SERVICE_NAME}-register.service" >/dev/null 2>&1 || true
      fi
      if [[ -f "${SYSTEMD_DIR%/}/${SERVICE_NAME}-payment.service" ]]; then
        run_root systemctl enable "${SERVICE_NAME}-payment.service" >/dev/null 2>&1 || true
      fi
    fi

    if bool_enabled "$START_SERVICE"; then
      # Start and self-test the private A/B worker first. activate_staged_release then
      # atomically moves only new requests; the old worker remains alive until its
      # HTTP/SSE/WebSocket in-flight counter reaches zero.
      activate_staged_release
      run_root systemctl --no-pager --full status "${HANDOFF_SERVICE_NAME}.service" || true

      # Restart the sidecar only when its source changed (or it is not running): an
      # unchanged sidecar is left alone so its in-flight upstream streams are never cut.
		if bool_enabled "$WITH_SIDECAR"; then
			if [[ "${SIDECAR_CHANGED:-1}" == "1" && "${OLD_WORKER_DRAINED:-1}" != "1" ]]; then
				warn "Previous worker still streams through the existing sidecar; deferring the sidecar version switch to avoid severing HTTP/SSE/WS traffic"
			elif [[ "${SIDECAR_CHANGED:-1}" == "1" ]] || ! run_root systemctl is-active --quiet "${SERVICE_NAME}-sidecar.service"; then
          log "Restarting ${SERVICE_NAME}-sidecar.service"
          run_root systemctl restart "${SERVICE_NAME}-sidecar.service" || true
          run_root systemctl --no-pager --full status "${SERVICE_NAME}-sidecar.service" || true
        else
          log "Sidecar unchanged; leaving ${SERVICE_NAME}-sidecar.service running (no stream cut)"
        fi
      fi

      # Restart lifecycle services when their source changed (or not running)
      if bool_enabled "$WITH_LIFECYCLE"; then
        if [[ -f "${SYSTEMD_DIR%/}/${SERVICE_NAME}-register.service" ]]; then
          if [[ "${LIFECYCLE_REGISTER_CHANGED:-1}" == "1" ]] || ! run_root systemctl is-active --quiet "${SERVICE_NAME}-register.service"; then
            log "Restarting ${SERVICE_NAME}-register.service"
            run_root systemctl restart "${SERVICE_NAME}-register.service" || true
            run_root systemctl --no-pager --full status "${SERVICE_NAME}-register.service" || true
          else
            log "Register service unchanged; leaving ${SERVICE_NAME}-register.service running"
          fi
        fi
        if [[ -f "${SYSTEMD_DIR%/}/${SERVICE_NAME}-payment.service" ]]; then
          if [[ "${LIFECYCLE_PAYMENT_CHANGED:-1}" == "1" ]] || ! run_root systemctl is-active --quiet "${SERVICE_NAME}-payment.service"; then
            log "Restarting ${SERVICE_NAME}-payment.service"
            run_root systemctl restart "${SERVICE_NAME}-payment.service" || true
            run_root systemctl --no-pager --full status "${SERVICE_NAME}-payment.service" || true
          else
            log "Payment service unchanged; leaving ${SERVICE_NAME}-payment.service running"
          fi
        fi
      fi
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

install_gopay() {
  bool_enabled "$WITH_GOPAY" || return 0

  local tmp
  tmp="${GOPAY_INSTALL_DIR}.tmp"
  log "Installing GoPay bundle to ${GOPAY_INSTALL_DIR}"
  run_root rm -rf "$tmp"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp"
  run_root cp -a "${GOPAY_SOURCE_DIR%/}/." "$tmp/"
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$tmp"
  run_root rm -rf "$GOPAY_INSTALL_DIR"
  run_root mv "$tmp" "$GOPAY_INSTALL_DIR"

  log "Preparing GoPay virtualenv at ${GOPAY_VENV}"
  run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$GOPAY_VENV"
  run_root python3 -m venv "$GOPAY_VENV"
  run_root "${GOPAY_VENV}/bin/pip" install --upgrade pip
  run_root "${GOPAY_VENV}/bin/pip" install -r "${GOPAY_INSTALL_DIR}/requirements.txt"
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$GOPAY_INSTALL_DIR" "$GOPAY_VENV"
}

# install_lifecycle_services installs the lifecycle management Python services
# (chatgpt_register + plus_payment) that support automated account registration
# and Plus subscription management.
install_lifecycle_services() {
  bool_enabled "$WITH_LIFECYCLE" || return 0

  # Install chatgpt_register service
  if [[ -d "$LIFECYCLE_REGISTER_SOURCE" ]]; then
    local tmp
    tmp="${LIFECYCLE_REGISTER_INSTALL}.tmp"
    log "Installing lifecycle register service to ${LIFECYCLE_REGISTER_INSTALL}"
    run_root rm -rf "$tmp"
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp"
    run_root cp -a "${LIFECYCLE_REGISTER_SOURCE%/}/." "$tmp/"
    run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$tmp"

    # Check if source changed (for smart restart)
    if [[ -d "$LIFECYCLE_REGISTER_INSTALL" ]]; then
      local old_hash new_hash
      old_hash=$(run_root find "$LIFECYCLE_REGISTER_INSTALL" -type f -exec sha256sum {} \; 2>/dev/null | sort | sha256sum | awk '{print $1}')
      new_hash=$(run_root find "$tmp" -type f -exec sha256sum {} \; 2>/dev/null | sort | sha256sum | awk '{print $1}')
      [[ "$old_hash" == "$new_hash" ]] && LIFECYCLE_REGISTER_CHANGED=0 || LIFECYCLE_REGISTER_CHANGED=1
    else
      LIFECYCLE_REGISTER_CHANGED=1
    fi

    run_root rm -rf "$LIFECYCLE_REGISTER_INSTALL"
    run_root mv "$tmp" "$LIFECYCLE_REGISTER_INSTALL"

    log "Preparing lifecycle register virtualenv at ${LIFECYCLE_REGISTER_VENV}"
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$LIFECYCLE_REGISTER_VENV"
    run_root python3 -m venv "$LIFECYCLE_REGISTER_VENV"
    run_root "${LIFECYCLE_REGISTER_VENV}/bin/pip" install --upgrade pip setuptools wheel
    run_root "${LIFECYCLE_REGISTER_VENV}/bin/pip" install -r "${LIFECYCLE_REGISTER_INSTALL}/requirements.txt"
    run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$LIFECYCLE_REGISTER_INSTALL" "$LIFECYCLE_REGISTER_VENV"
  else
    warn "Lifecycle register source not found at ${LIFECYCLE_REGISTER_SOURCE}, skipping"
  fi

  # Install plus_payment service
  if [[ -d "$LIFECYCLE_PAYMENT_SOURCE" ]]; then
    local tmp
    tmp="${LIFECYCLE_PAYMENT_INSTALL}.tmp"
    log "Installing lifecycle payment service to ${LIFECYCLE_PAYMENT_INSTALL}"
    run_root rm -rf "$tmp"
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$tmp"
    run_root cp -a "${LIFECYCLE_PAYMENT_SOURCE%/}/." "$tmp/"
    run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$tmp"

    # Check if source changed (for smart restart)
    if [[ -d "$LIFECYCLE_PAYMENT_INSTALL" ]]; then
      local old_hash new_hash
      old_hash=$(run_root find "$LIFECYCLE_PAYMENT_INSTALL" -type f -exec sha256sum {} \; 2>/dev/null | sort | sha256sum | awk '{print $1}')
      new_hash=$(run_root find "$tmp" -type f -exec sha256sum {} \; 2>/dev/null | sort | sha256sum | awk '{print $1}')
      [[ "$old_hash" == "$new_hash" ]] && LIFECYCLE_PAYMENT_CHANGED=0 || LIFECYCLE_PAYMENT_CHANGED=1
    else
      LIFECYCLE_PAYMENT_CHANGED=1
    fi

    run_root rm -rf "$LIFECYCLE_PAYMENT_INSTALL"
    run_root mv "$tmp" "$LIFECYCLE_PAYMENT_INSTALL"

    log "Preparing lifecycle payment virtualenv at ${LIFECYCLE_PAYMENT_VENV}"
    run_root install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$LIFECYCLE_PAYMENT_VENV"
    run_root python3 -m venv "$LIFECYCLE_PAYMENT_VENV"
    run_root "${LIFECYCLE_PAYMENT_VENV}/bin/pip" install --upgrade pip setuptools wheel
    run_root "${LIFECYCLE_PAYMENT_VENV}/bin/pip" install -r "${LIFECYCLE_PAYMENT_INSTALL}/requirements.txt"
    run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$LIFECYCLE_PAYMENT_INSTALL" "$LIFECYCLE_PAYMENT_VENV"
  else
    warn "Lifecycle payment source not found at ${LIFECYCLE_PAYMENT_SOURCE}, skipping"
  fi
}

# ── Node auto-registration engine (other_new_gpt_register) ──────────────────────────
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
  local npm_bin tmp
  npm_bin="$(find_npm)" || die "npm not found after Node install"

  tmp="${REGISTRAR_INSTALL}.tmp"
  log "Installing Node registrar to ${REGISTRAR_INSTALL}"
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
  run_root rm -rf "$REGISTRAR_INSTALL"
  run_root mv "$tmp" "$REGISTRAR_INSTALL"

  log "Installing Node registrar dependencies (pulls puppeteer-real-browser; may take a minute)"
  local npm_common="--no-audit --no-fund --omit=dev"
  if [[ -f "${REGISTRAR_INSTALL}/package-lock.json" ]]; then
    run_root env HOME="$DATA_DIR" sh -c "cd '$REGISTRAR_INSTALL' && '$npm_bin' ci $npm_common" \
      || run_root env HOME="$DATA_DIR" sh -c "cd '$REGISTRAR_INSTALL' && '$npm_bin' install $npm_common"
  else
    run_root env HOME="$DATA_DIR" sh -c "cd '$REGISTRAR_INSTALL' && '$npm_bin' install $npm_common"
  fi
  run_root chown -R "${SERVICE_USER}:${SERVICE_GROUP}" "$REGISTRAR_INSTALL"
  log "Node registrar ready at ${REGISTRAR_INSTALL}"
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
  local sidecar_summary gopay_summary lifecycle_summary migration_summary warp_summary frontend_url manual_admin_env
  if bool_enabled "$WITH_SIDECAR"; then
    sidecar_summary="${SERVICE_NAME}-sidecar.service (${SIDECAR_ADDR})"
  else
    sidecar_summary="disabled"
  fi
  if bool_enabled "$WITH_GOPAY"; then
    gopay_summary="${GOPAY_INSTALL_DIR}"
  else
    gopay_summary="disabled"
  fi
  if bool_enabled "$WITH_LIFECYCLE"; then
    lifecycle_summary="${SERVICE_NAME}-register.service (${LIFECYCLE_REGISTER_ADDR}), ${SERVICE_NAME}-payment.service (${LIFECYCLE_PAYMENT_ADDR})"
  else
    lifecycle_summary="disabled"
  fi
  local registration_summary
  if bool_enabled "$WITH_REGISTRATION"; then
    registration_summary="node engine @ ${REGISTRAR_INSTALL} (node=${NODE_BIN:-?}, chrome=${CHROME_BIN:-?})"
  else
    registration_summary="disabled"
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
  frontend_url="$(frontend_url_hint)"
  manual_admin_env=""
  if [[ -n "$ADMIN_TOKEN" ]]; then
    manual_admin_env=" CODEX_POOL_ADMIN_TOKEN=${ADMIN_TOKEN}"
  fi

  cat <<EOF

Install complete.

Binary:        ${BIN_DIR}/${APP_NAME}
Handoff:       ${BIN_DIR}/${HANDOFF_NAME}
Release:       ${RELEASE_ID} (${RELEASE_DIR})
Context clear: ${BIN_DIR}/codex-pool-clear-context
Config:        ${CONFIG_FILE}
Data:          ${DATA_DIR}
Database:      ${DATABASE_PATH}
Listen:        ${LISTEN_ADDR}
Frontend:      ${frontend_url}
Handoff svc:  ${HANDOFF_SERVICE_NAME}.service
Compat svc:   ${SERVICE_NAME}.service
Sidecar:       ${sidecar_summary}
GoPay:         ${gopay_summary}
Lifecycle:     ${lifecycle_summary}
Registration:  ${registration_summary}
Group migration: ${migration_summary}
WARP:          ${warp_summary}
Admin token:   ${ADMIN_TOKEN:-<empty>}

Manual run:
  CODEX_POOL_DATABASE=${DATABASE_PATH} CODEX_POOL_MIGRATE_USER_GROUPS=${MIGRATE_USER_GROUPS} CODEX_POOL_LISTEN_ADDR=${LISTEN_ADDR}${manual_admin_env} ${BIN_DIR}/${APP_NAME} --config ${CONFIG_FILE}

Useful service commands:
  systemctl status ${HANDOFF_SERVICE_NAME}.service
  systemctl status ${SERVICE_NAME}.socket
  systemctl status ${SERVICE_NAME}-worker@${RELEASE_ID}.service
  ${BIN_DIR}/codex-pool-rollback
  journalctl -u ${HANDOFF_SERVICE_NAME}.service -f
  systemctl status ${SERVICE_NAME}-sidecar.service
  systemctl status ${SERVICE_NAME}-register.service
  systemctl status ${SERVICE_NAME}-payment.service
EOF
}

main() {
  normalize_migrate_user_groups
  ensure_project_files
  ensure_absolute_paths
  acquire_deploy_lock
  trap deployment_exit_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap 'exit 129' HUP
  log "Codex skills compatibility: full official skills/plugins/Browser Use support requires the official Codex account path; custom providers are best-effort."
  ensure_system_deps
  adopt_interrupted_install_pause
  ensure_go
  build_project
  prepare_runtime_layout
  install_binary_and_config
  install_sidecar
  install_gopay
  install_lifecycle_services
  install_nodejs
  install_chrome
  install_node_registrar
  install_warp
  printf '%s\n' "$RELEASE_ID" | run_root tee "${RELEASE_DIR%/}/.staged-ok" >/dev/null
  install_systemd_unit
  open_firewall_port
  print_summary
}

main "$@"
