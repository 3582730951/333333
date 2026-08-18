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
declare -a root_systemctl_calls=()

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
  root_systemctl_calls+=("$*")
  if [[ -n "${SQLITE_TEST_SYSTEMCTL_LOG:-}" ]]; then
    printf '%s\n' "$*" >>"$SQLITE_TEST_SYSTEMCTL_LOG"
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
    stop)
      unit="${!#}"
      unit_state["$unit"]="inactive"
      unit_pid["$unit"]="0"
      ;;
    kill)
      unit="${!#}"
      if [[ " $* " == *" --signal=SIGUSR1 "* ]]; then
        [[ "${unit_state[$unit]:-inactive}" == active ]] || return 1
      else
        unit_state["$unit"]="inactive"
        unit_pid["$unit"]="0"
      fi
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
  [[ -f "$APP_DIR/releases/$RELEASE_ID/super-instruct/bridge.md" ]] || die "Super-Instruct M1 bridge missing"
  cmp -s super-instruct/bridge.md "$APP_DIR/releases/$RELEASE_ID/super-instruct/bridge.md" || die "installed Super-Instruct M1 bridge changed"
  expected="chown -R root:${SERVICE_GROUP} $APP_DIR/releases/.staging-${RELEASE_ID}/super-instruct"
  found=0
  for call in "${root_chown_calls[@]}"; do
    [[ "$call" == "$expected" ]] && found=1
  done
  [[ "$found" == 1 ]] || die "Super-Instruct release bundle was not assigned to ${SERVICE_GROUP}: ${root_chown_calls[*]-}"
  [[ "$(cat "$CONFIG_FILE")" == "$config_before" ]] || die "existing config was changed during install"
}

test_fresh_config_preserves_codex_client_policy() {
  local rendered help_text
  # A legacy environment value must not create a cloud-installer-wide gate.
  WITH_SUPER_INSTRUCT=1
  rendered="$(render_runtime_config)"
  python3 - "$rendered" <<'PY'
import json
import sys

config = json.loads(sys.argv[1])
assert config["codex_install_model"], "fresh install must retain its managed model"
assert config["super_instruct_local_enabled"] is False, "fresh install unexpectedly enabled local Super-Instruct"
for key in (
    "codex_install_effort",
    "codex_install_approval_policy",
    "codex_install_sandbox_mode",
):
    assert config[key] == "", f"fresh install unexpectedly overrides {key}: {config[key]!r}"
PY

  help_text="$(usage)"
  [[ "$help_text" != *"--with-super-instruct"* ]] || die "cloud installer still advertises a global Super-Instruct switch"
  [[ "$help_text" != *"--without-super-instruct"* ]] || die "cloud installer still advertises a global Super-Instruct switch"
  ! grep -q 'Enable headless local Super-Instruct' scripts/install.sh || die "cloud installer still prompts for global Super-Instruct mode"
  ! grep -q 'CODEX_POOL_SUPER_INSTRUCT_LOCAL_ENABLED' scripts/install.sh || die "cloud installer still exports a global Super-Instruct override"
  unset WITH_SUPER_INSTRUCT
}

test_lossless_upgrade_contract() {
  local activate_body legacy_body reauth_body systemd_body cleanup_body reaper_body schedule_body wait_body capture_body
  activate_body="$(sed -n '/^activate_staged_release()/,/^}/p' scripts/install.sh)"
  legacy_body="$(sed -n '/^destroy_legacy_worker_process()/,/^}/p' scripts/install.sh)"
  reauth_body="$(sed -n '/^activate_codex_reauth_worker()/,/^}/p' scripts/install.sh)"
  systemd_body="$(sed -n '/^install_systemd_unit()/,/^}/p' scripts/install.sh)"
  cleanup_body="$(sed -n '/^deployment_exit_cleanup()/,/^}/p' scripts/install.sh)"
  schedule_body="$(sed -n '/^schedule_release_reaper()/,/^}/p' scripts/install.sh)"
  wait_body="$(sed -n '/^wait_worker_ready()/,/^}/p' scripts/install.sh)"
  capture_body="$(sed -n '/^capture_worker_startup_failure()/,/^}/p' scripts/install.sh)"
  reaper_body="$(cat scripts/reap-old-release.sh)"

  [[ "$activate_body" == *'pause_handoff_admission'* && "$activate_body" == *'resume_handoff_admission'* ]] ||
    die "release activation does not bracket the switch with admission pause/resume"
  [[ "$activate_body" == *'schedule_release_reaper'* && "$activate_body" == *'reclaimed asynchronously'* ]] ||
    die "release activation does not delegate the old worker to a background reaper"
  [[ "$activate_body" == *'resume_handoff_admission'*'schedule_release_reaper'* && "$schedule_body" == *'systemctl enable'* && "$schedule_body" == *'systemctl start --no-block'* ]] ||
    die "old-release drain starts before cutover completes or blocks install.sh"
  [[ "$activate_body" != *'while true; do'* && "$activate_body" != *'worker_inflight'* && "$activate_body" != *'destroy_worker_instance "$old_worker_release"'* ]] ||
    die "install.sh still waits for or destroys the old worker synchronously"
  [[ "$activate_body" == *'pre-initialized staged worker'* && "$activate_body" == *'atomic_symlink "$new_socket"'* ]] ||
    die "release activation does not use the database-free pre-init promotion contract"
  [[ "$activate_body" != *'sqlite_write_probe'* && "$activate_body" != *'wait_handoff_inflight_zero'* && "$activate_body" != *'prepare_sqlite_for_staged_worker'* ]] ||
    die "release activation still requires SQLite preflight or zero established connections"
  [[ "$legacy_body" != *'SIGKILL'* && "$legacy_body" == *'preserving it instead of forcing client-visible errors'* ]] ||
    die "legacy upgrade can still force-kill established connections"
  [[ "$reauth_body" != *'systemctl restart'* && "$reauth_body" == *'next cold start'* ]] ||
    die "upgrade still hot-restarts the active Codex reauth worker"
  [[ "$systemd_body" != *'systemctl restart "${SERVICE_NAME}-sidecar.service"'* && "$systemd_body" == *'Sidecar update staged'* ]] ||
    die "upgrade still hot-restarts the shared sidecar"
  [[ "$systemd_body" == *'${SERVICE_NAME}-reaper@.service'* && "$systemd_body" == *'TimeoutStopSec=infinity'* ]] ||
    die "systemd units do not preserve an unbounded background graceful drain"
  [[ "$systemd_body" != *'reclaim_superseded_install_resources "$RELEASE_ID"'* ]] ||
    die "systemd installation still performs synchronous old-release reclamation"
  [[ "$reaper_body" == *'worker_inflight'* && "$reaper_body" == *'QUIET_SECONDS'* && "$reaper_body" == *'rm -rf -- "$release_dir"'* ]] ||
    die "background reaper does not wait for stable zero inflight and reclaim the release"
  [[ "$reaper_body" != *'SIGKILL'* && "$reaper_body" != *'kill -KILL'* ]] ||
    die "background reaper can force-kill an established request"
  [[ "$cleanup_body" == *'resume_handoff_admission'* ]] ||
    die "installer failure cleanup does not automatically resume queued requests"
  [[ "$wait_body" == *'NRestarts'* && "$wait_body" == *'WORKER_START_RESTART_LIMIT'* && "$wait_body" == *'Waiting for staged worker'* ]] ||
    die "staged-worker health gate does not expose progress or stop a crash loop"
  [[ "$capture_body" == *'journalctl'*'-u "$unit"'* && "$capture_body" == *'[kernel-oom]'* && "$capture_body" == *'deploy-failures'* ]] ||
    die "staged-worker failure capture does not preserve unit journal and OOM evidence"
  [[ "$capture_body" == *'[sqlite-locks]'* ]] ||
    die "staged-worker failure capture omits SQLite lock evidence"
  [[ "$activate_body" == *'capture_worker_startup_failure'*'systemctl stop "$new_unit"'* ]] ||
    die "candidate cleanup runs before its startup evidence is captured"
}

test_background_reaper_contract() {
  local fixture="$DATA_DIR/reaper-fixture" fake_bin="$DATA_DIR/reaper-bin"
  local fake_state="$DATA_DIR/reaper-stopped" fake_log="$DATA_DIR/reaper-systemctl.log"
  local release="release-drained"
  mkdir -p "$fixture/app/releases/$release" "$fixture/app/releases/release-current" "$fixture/data/run" "$fake_bin"
  : >"$fixture/app/releases/$release/$APP_NAME"
  : >"$fixture/data/run/worker-$release.sock"
  ln -s "$fixture/app/releases/release-current" "$fixture/app/current"
  ln -s "$fixture/app/releases/$release" "$fixture/app/previous"

  cat >"$fake_bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >>"${REAPER_FAKE_LOG:?}"
case "${1:-}" in
  show)
    if [[ " $* " == *" --property=ActiveState "* ]]; then
      if [[ -e "${REAPER_FAKE_STATE:?}" ]]; then printf 'inactive\n'; else printf 'active\n'; fi
    elif [[ -e "${REAPER_FAKE_STATE:?}" ]]; then
      printf '0\n'
    else
      printf '4321\n'
    fi
    ;;
  stop)
    : >"${REAPER_FAKE_STATE:?}"
    ;;
esac
EOF
  cat >"$fake_bin/curl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' '{"ok":true,"inflight":0}'
EOF
  chmod +x "$fake_bin/systemctl" "$fake_bin/curl"

  PATH="$fake_bin:$PATH" REAPER_FAKE_STATE="$fake_state" REAPER_FAKE_LOG="$fake_log" \
    CODEX_POOL_REAPER_QUIET_SECONDS=0 CODEX_POOL_REAPER_REPORT_SECONDS=1 \
    bash scripts/reap-old-release.sh \
      --service-name "$SERVICE_NAME" --app-dir "$fixture/app" --data-dir "$fixture/data" \
      --lock-file "$fixture/deploy.lock" --app-name "$APP_NAME" \
      --handoff-name "$HANDOFF_NAME" --release "$release" >/dev/null

  [[ ! -e "$fixture/app/releases/$release" && ! -e "$fixture/data/run/worker-$release.sock" ]] ||
    die "background reaper did not remove the drained worker resources"
  [[ ! -e "$fixture/app/previous" && ! -L "$fixture/app/previous" && -d "$fixture/app/releases/release-current" ]] ||
    die "background reaper removed the current release or left a dangling previous link"
  grep -Fq "stop --no-block ${SERVICE_NAME}-worker@${release}.service" "$fake_log" ||
    die "background reaper did not gracefully stop the drained worker"
  ! grep -Eq 'SIGKILL|kill .*KILL' "$fake_log" ||
    die "background reaper force-killed the drained worker"

  release="release-revived"
  mkdir -p "$fixture/app/releases/$release"
  : >"$fixture/app/releases/$release/$APP_NAME"
  : >"$fixture/data/run/worker-$release.sock"
  ln -sfn "$fixture/app/releases/$release" "$fixture/app/current"
  rm -f "$fake_state" "$fake_log"
  : >"$fake_log"
  PATH="$fake_bin:$PATH" REAPER_FAKE_STATE="$fake_state" REAPER_FAKE_LOG="$fake_log" \
    CODEX_POOL_REAPER_QUIET_SECONDS=0 CODEX_POOL_REAPER_REPORT_SECONDS=1 \
    bash scripts/reap-old-release.sh \
      --service-name "$SERVICE_NAME" --app-dir "$fixture/app" --data-dir "$fixture/data" \
      --lock-file "$fixture/deploy.lock" --app-name "$APP_NAME" \
      --handoff-name "$HANDOFF_NAME" --release "$release" >/dev/null
  [[ -d "$fixture/app/releases/$release" && -e "$fixture/data/run/worker-$release.sock" ]] ||
    die "background reaper removed a release that became current during drain"
  ! grep -Fq "stop --no-block ${SERVICE_NAME}-worker@${release}.service" "$fake_log" ||
    die "background reaper stopped a release that became current during drain"
}

test_worker_failure_report_capture() {
  WORKER_FAILURE_REPORT=""
  capture_worker_startup_failure "release-failed" "$(date +%s)" >/dev/null 2>&1
  [[ -n "$WORKER_FAILURE_REPORT" && -f "$WORKER_FAILURE_REPORT" ]] ||
    die "staged-worker failure report was not persisted"
  grep -Fq '[systemctl-show]' "$WORKER_FAILURE_REPORT" ||
    die "failure report omitted systemd state"
  grep -Fq '[unit-journal]' "$WORKER_FAILURE_REPORT" ||
    die "failure report omitted the complete unit journal"
  grep -Fq '[kernel-oom]' "$WORKER_FAILURE_REPORT" ||
    die "failure report omitted kernel OOM evidence"
}

reclaim_superseded_install_resources "release-current"
[[ ! -e "$PROC_ROOT/555" && -e "$PROC_ROOT/666/exe" ]]
[[ -L "$PROC_ROOT/777/exe" ]]
[[ -d "$APP_DIR/releases/release-current" && -d "$APP_DIR/releases/release-old" ]]
[[ ! -e "$APP_DIR/releases/release-orphan" && ! -e "$APP_DIR/releases/release-older" ]]
[[ ! -e "$APP_DIR/releases/.staging-release-interrupted" ]]

test_release_super_instruct_permissions
test_fresh_config_preserves_codex_client_policy
test_lossless_upgrade_contract
test_background_reaper_contract
test_worker_failure_report_capture

printf '%s\n' "single-worker installer lifecycle: OK"
