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
done
mkdir -p "$APP_DIR/releases/.staging-release-interrupted"
ln -s "$APP_DIR/releases/release-old" "$APP_DIR/previous"
mkdir -p "$PROC_ROOT/555" "$PROC_ROOT/666"
ln -s "$APP_DIR/releases/release-orphan/$APP_NAME" "$PROC_ROOT/555/exe"
ln -s "$APP_DIR/releases/release-current/$APP_NAME" "$PROC_ROOT/666/exe"

reclaim_superseded_install_resources "release-current"
[[ ! -e "$PROC_ROOT/555" && -e "$PROC_ROOT/666/exe" ]]
[[ -d "$APP_DIR/releases/release-current" && -d "$APP_DIR/releases/release-old" ]]
[[ ! -e "$APP_DIR/releases/release-orphan" && ! -e "$APP_DIR/releases/release-older" ]]
[[ ! -e "$APP_DIR/releases/.staging-release-interrupted" ]]

printf '%s\n' "single-worker installer lifecycle: OK"
