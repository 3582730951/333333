#!/usr/bin/env bash
set -Eeuo pipefail

cd "$(dirname "$0")/.."
# shellcheck source=install.sh
source scripts/install.sh

SERVICE_NAME="codex-pool"
DATA_DIR="$(mktemp -d)"
APP_DIR="$DATA_DIR/app"
APP_NAME="codex-pool-server"
PROC_ROOT="$DATA_DIR/proc"
WORKER_DESTROY_TIMEOUT=0
trap 'rm -rf "$DATA_DIR"' EXIT
mkdir -p "$DATA_DIR/run" "$APP_DIR/releases" "$PROC_ROOT"

declare -A unit_state=()
declare -A unit_pid=()
declare -A unit_enabled=()
declare -a root_chown_calls=()

current_unit="${SERVICE_NAME}-worker@release-current.service"
old_unit="${SERVICE_NAME}-worker@release-old.service"
orphan_unit="${SERVICE_NAME}-worker@release-orphan.service"
older_unit="${SERVICE_NAME}-worker@release-older.service"
legacy_unit="${SERVICE_NAME}.service"
unit_state["$current_unit"]="active"
unit_pid["$current_unit"]="222"
unit_enabled["$current_unit"]="1"
unit_state["$old_unit"]="active"
unit_pid["$old_unit"]="111"
unit_enabled["$old_unit"]="1"
unit_state["$older_unit"]="active"
unit_pid["$older_unit"]="444"
unit_enabled["$older_unit"]="1"

log() { :; }
warn() { printf 'test warning: %s\n' "$*" >&2; }
die() { printf 'test failure: %s\n' "$*" >&2; return 1; }

run_root() {
  if [[ "$1" != systemctl ]]; then
    if [[ "$1" == kill ]]; then
      local pid="${!#}"
      rm -rf -- "$PROC_ROOT/$pid"
      return
    fi
    if [[ "$1" == chown ]]; then
      root_chown_calls+=("$*")
      return
    fi
    command "$@"
    return
  fi
  shift
  local action="$1" unit prop
  shift
  case "$action" in
    show)
      unit="$1"
      shift
      prop=""
      while (($#)); do
        case "$1" in
          --property=*) prop="${1#--property=}" ;;
        esac
        shift
      done
      case "$prop" in
        ActiveState) printf '%s\n' "${unit_state[$unit]:-inactive}" ;;
        MainPID) printf '%s\n' "${unit_pid[$unit]:-0}" ;;
      esac
      ;;
    stop|kill)
      unit="${!#}"
      unit_state["$unit"]="inactive"
      unit_pid["$unit"]="0"
      ;;
    disable)
      unit="${!#}"
      unit_enabled["$unit"]="0"
      ;;
    reset-failed)
      unit="${!#}"
      unit_state["$unit"]="inactive"
      ;;
    list-units)
      for unit in "${!unit_state[@]}"; do
        if [[ " $* " == *" --state=active "* && "${unit_state[$unit]}" != active ]]; then
          continue
        fi
        printf '%s loaded %s running test\n' "$unit" "${unit_state[$unit]}"
      done
      ;;
    list-unit-files)
      for unit in "${!unit_enabled[@]}"; do
        [[ "${unit_enabled[$unit]}" == 1 ]] || continue
        printf '%s enabled\n' "$unit"
      done
      ;;
    *)
      printf 'unexpected fake systemctl action: %s\n' "$action" >&2
      return 2
      ;;
  esac
}

old_socket="$DATA_DIR/run/worker-release-old.sock"
: >"$old_socket"
destroy_worker_instance "release-old" "$old_socket"
[[ "${unit_state[$old_unit]}" == inactive && "${unit_pid[$old_unit]}" == 0 ]]
[[ "${unit_enabled[$old_unit]}" == 0 && ! -e "$old_socket" ]]

unit_state["$orphan_unit"]="active"
unit_pid["$orphan_unit"]="333"
unit_enabled["$orphan_unit"]="1"
destroy_stale_worker_instances "release-current"
[[ "${unit_state[$orphan_unit]}" == inactive && "${unit_enabled[$orphan_unit]}" == 0 ]]
[[ "${unit_state[$older_unit]}" == inactive && "${unit_enabled[$older_unit]}" == 0 ]]
[[ "${unit_state[$current_unit]}" == active && "${unit_enabled[$current_unit]}" == 1 ]]
verify_single_active_worker "release-current"

unit_state["$legacy_unit"]="active"
unit_pid["$legacy_unit"]="0"
unit_enabled["$legacy_unit"]="1"
destroy_legacy_worker_process
[[ "${unit_state[$legacy_unit]}" == inactive && "${unit_enabled[$legacy_unit]}" == 0 ]]

for release in release-current release-old release-orphan release-older; do
  mkdir -p "$APP_DIR/releases/$release"
  : >"$APP_DIR/releases/$release/$APP_NAME"
  : >"$APP_DIR/releases/$release/$HANDOFF_NAME"
done
mkdir -p "$APP_DIR/releases/.staging-release-interrupted"
ln -s "$APP_DIR/releases/release-old" "$APP_DIR/previous"
mkdir -p "$PROC_ROOT/555" "$PROC_ROOT/666" "$PROC_ROOT/777"
ln -s "$APP_DIR/releases/release-orphan/$APP_NAME" "$PROC_ROOT/555/exe"
ln -s "$APP_DIR/releases/release-current/$APP_NAME" "$PROC_ROOT/666/exe"
ln -s "$APP_DIR/releases/release-older/$HANDOFF_NAME" "$PROC_ROOT/777/exe"


test_release_super_instruct_permissions() {
  local fixture
  fixture="$(mktemp -d "$DATA_DIR/release-perms.XXXXXX")"
  local APP_DIR="$fixture/app"
  local BIN_DIR="$fixture/bin"
  local BUILD_DIR="$fixture/build"
  local CONFIG_FILE="$fixture/etc/config.json"
  local DATA_DIR="$fixture/data"
  local LISTEN_ADDR="127.0.0.1:8787"
  local ADMIN_TOKEN=""
  local RELEASE_ID="release-perms"
  local RELEASE_DIR=""
  local PREVIOUS_RELEASE_ID=""
  local expected found call

  mkdir -p "$APP_DIR/releases" "$APP_DIR/bin" "$BIN_DIR" "$BUILD_DIR/gateway-bin" "$(dirname "$CONFIG_FILE")" "$DATA_DIR/data/keys"
  cat >"$CONFIG_FILE" <<'EOF'
{
  "codex_install_model": "existing-model",
  "codex_install_effort": "ultra",
  "codex_install_approval_policy": "on-request",
  "codex_install_sandbox_mode": "workspace-write",
  "existing_extension": {"features": ["keep-me"]}
}
EOF
  local config_before
  config_before="$(cat "$CONFIG_FILE")"
  cat >"$BUILD_DIR/$APP_NAME" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == "--self-test" ]] && exit 0
exit 0
EOF
  cat >"$BUILD_DIR/$HANDOFF_NAME" <<'EOF'
#!/usr/bin/env bash
[[ "${1:-}" == "--self-test" ]] && exit 0
exit 0
EOF
  cat >"$BUILD_DIR/gateway-bin/gateway-test" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$BUILD_DIR/$APP_NAME" "$BUILD_DIR/$HANDOFF_NAME" "$BUILD_DIR/gateway-bin/gateway-test"

  root_chown_calls=()
  install_binary_and_config

  [[ -d "$APP_DIR/releases/$RELEASE_ID/super-instruct/codex-skills" ]] || die "Super-Instruct release bundle missing"
  expected="chown -R root:${SERVICE_GROUP} $APP_DIR/releases/.staging-${RELEASE_ID}/super-instruct"
  found=0
  for call in "${root_chown_calls[@]}"; do
    [[ "$call" == "$expected" ]] && found=1
  done
  [[ "$found" == 1 ]] || die "Super-Instruct release bundle was not assigned to ${SERVICE_GROUP}: ${root_chown_calls[*]-}"
  [[ "$(cat "$CONFIG_FILE")" == "$config_before" ]] || die "existing config was changed during install"
}

test_fresh_config_preserves_codex_client_policy() {
  local rendered
  rendered="$(render_runtime_config)"
  python3 - "$rendered" <<'PY'
import json
import sys

config = json.loads(sys.argv[1])
assert config["codex_install_model"], "fresh install must retain its managed model"
for key in (
    "codex_install_effort",
    "codex_install_approval_policy",
    "codex_install_sandbox_mode",
):
    assert config[key] == "", f"fresh install unexpectedly overrides {key}: {config[key]!r}"
PY
}
reclaim_superseded_install_resources "release-current"
[[ ! -e "$PROC_ROOT/555" && -e "$PROC_ROOT/666/exe" ]]
[[ -L "$PROC_ROOT/777/exe" ]]
[[ -d "$APP_DIR/releases/release-current" && -d "$APP_DIR/releases/release-old" ]]
[[ ! -e "$APP_DIR/releases/release-orphan" && ! -e "$APP_DIR/releases/release-older" ]]
[[ ! -e "$APP_DIR/releases/.staging-release-interrupted" ]]

test_release_super_instruct_permissions
test_fresh_config_preserves_codex_client_policy

printf '%s\n' "single-worker installer lifecycle: OK"
