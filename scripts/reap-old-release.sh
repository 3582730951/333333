#!/usr/bin/env bash
set -Eeuo pipefail

# Runs as a root-owned systemd oneshot after install.sh has atomically switched the
# stable handoff to a new private worker. It deliberately has no drain deadline:
# established HTTP/SSE/WebSocket requests stay on the old worker until completion.

SERVICE_NAME=""
APP_DIR=""
DATA_DIR=""
LOCK_FILE=""
APP_NAME="codex-pool-server"
HANDOFF_NAME="codex-pool-handoff"
RELEASE=""
REPORT_SECONDS="${CODEX_POOL_REAPER_REPORT_SECONDS:-30}"
QUIET_SECONDS="${CODEX_POOL_REAPER_QUIET_SECONDS:-3}"

log() {
  printf 'codex-pool reaper: %s\n' "$*"
}

die() {
  printf 'codex-pool reaper: ERROR: %s\n' "$*" >&2
  exit 1
}

require_value() {
  [[ $# -ge 2 && -n "${2:-}" ]] || die "missing value for $1"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --service-name) require_value "$@"; SERVICE_NAME="$2"; shift 2 ;;
    --app-dir) require_value "$@"; APP_DIR="$2"; shift 2 ;;
    --data-dir) require_value "$@"; DATA_DIR="$2"; shift 2 ;;
    --lock-file) require_value "$@"; LOCK_FILE="$2"; shift 2 ;;
    --app-name) require_value "$@"; APP_NAME="$2"; shift 2 ;;
    --handoff-name) require_value "$@"; HANDOFF_NAME="$2"; shift 2 ;;
    --release) require_value "$@"; RELEASE="$2"; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[[ "$SERVICE_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid service name"
[[ "$APP_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid app name"
[[ "$HANDOFF_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid handoff name"
[[ "$APP_DIR" == /* && "$APP_DIR" != "/" ]] || die "app dir must be a non-root absolute path"
[[ "$DATA_DIR" == /* && "$DATA_DIR" != "/" ]] || die "data dir must be a non-root absolute path"
[[ "$LOCK_FILE" == /* && "$LOCK_FILE" != "/" ]] || die "lock file must be a non-root absolute path"
[[ "$REPORT_SECONDS" =~ ^[0-9]+$ ]] || die "invalid report interval"
[[ "$QUIET_SECONDS" =~ ^[0-9]+$ ]] || die "invalid quiet interval"
if [[ "$RELEASE" != "_legacy" && ! "$RELEASE" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]]; then
  die "invalid release id"
fi
(( REPORT_SECONDS > 0 )) || REPORT_SECONDS=30

disable_self() {
  systemctl disable "${SERVICE_NAME}-reaper@${RELEASE}.service" >/dev/null 2>&1 || true
}

unit_has_process() {
  local unit="$1" pid
  pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]]
}

unit_is_stopped() {
  local unit="$1" state pid
  state="$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
  pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  [[ -z "$pid" || "$pid" == "0" ]] || return 1
  case "$state" in
    ""|inactive|failed) return 0 ;;
    *) return 1 ;;
  esac
}

release_worker_pids() {
  local release_dir="$1" exe target pid
  for exe in /proc/[0-9]*/exe; do
    [[ -L "$exe" ]] || continue
    target="$(readlink "$exe" 2>/dev/null || true)"
    target="${target% (deleted)}"
    [[ "$target" == "${release_dir%/}/${APP_NAME}" ]] || continue
    pid="${exe%/exe}"
    printf '%s\n' "${pid##*/}"
  done
}

worker_inflight() {
  local socket="$1" payload
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$socket" http://localhost/readyz 2>/dev/null || true)"
  printf '%s' "$payload" | sed -n 's/.*"inflight":[[:space:]]*\([0-9][0-9]*\).*/\1/p'
}

wait_for_worker_drain() {
  local unit="$1" socket="$2" release_dir="$3"
  local inflight="" zero_since=0 next_report=$((SECONDS + REPORT_SECONDS)) pids
  while true; do
    inflight="$(worker_inflight "$socket")"
    pids="$(release_worker_pids "$release_dir")"
    if [[ "$inflight" == "0" ]]; then
      if (( zero_since == 0 )); then
        zero_since="$SECONDS"
      fi
      # Requiring a short quiet period closes the tiny gap between handoff admission
      # and the old worker incrementing its local request counter.
      if (( SECONDS - zero_since >= QUIET_SECONDS )); then
        return 0
      fi
    else
      zero_since=0
    fi

    # If neither systemd nor /proc has a worker, the socket is stale and there is
    # nothing left that could own a client request.
    if ! unit_has_process "$unit" && [[ -z "$pids" ]]; then
      return 0
    fi
    if (( SECONDS >= next_report )); then
      log "${unit} still owns ${inflight:-unknown} in-flight request(s); waiting in background"
      next_report=$((next_report + REPORT_SECONDS))
    fi
    sleep 1
  done
}

wait_for_unit_exit() {
  local unit="$1" next_report=$((SECONDS + REPORT_SECONDS))
  while ! unit_is_stopped "$unit"; do
    if (( SECONDS >= next_report )); then
      log "${unit} is still finishing graceful shutdown; no force-kill will be used"
      next_report=$((next_report + REPORT_SECONDS))
    fi
    sleep 1
  done
}

wait_for_release_processes() {
  local release_dir="$1" pids next_report=$((SECONDS + REPORT_SECONDS))
  while pids="$(release_worker_pids "$release_dir")" && [[ -n "$pids" ]]; do
    if (( SECONDS >= next_report )); then
      log "detached old worker process(es) still exiting: ${pids//$'\n'/,}"
      next_report=$((next_report + REPORT_SECONDS))
    fi
    sleep 1
  done
}

acquire_deployment_lock() {
  command -v flock >/dev/null 2>&1 || die "flock is required"
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec 9>>"$LOCK_FILE"
  flock 9
}

release_dir=""
socket=""
if [[ "$RELEASE" == "_legacy" ]]; then
  unit="${SERVICE_NAME}.service"
  previous_target="$(readlink -f "${APP_DIR%/}/previous" 2>/dev/null || true)"
  case "$previous_target" in
    "${APP_DIR%/}/releases/"*)
      candidate="${previous_target##*/}"
      if [[ "$candidate" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ && "$previous_target" == "${APP_DIR%/}/releases/${candidate}" ]]; then
        release_dir="$previous_target"
      fi
      ;;
  esac
  log "waiting for legacy ${unit} to finish its established requests"
  wait_for_unit_exit "$unit"
  acquire_deployment_lock
else
  unit="${SERVICE_NAME}-worker@${RELEASE}.service"
  socket="${DATA_DIR%/}/run/worker-${RELEASE}.sock"
  release_dir="${APP_DIR%/}/releases/${RELEASE}"
  wait_for_worker_drain "$unit" "$socket" "$release_dir"
  acquire_deployment_lock
  current_target="$(readlink -f "${APP_DIR%/}/current" 2>/dev/null || true)"
  if [[ "$release_dir" == "$current_target" ]]; then
    log "${RELEASE} became current again while draining; leaving it untouched"
    disable_self
    exit 0
  fi
  log "${unit} reached a stable zero in-flight count; stopping it gracefully"
  systemctl set-property --runtime "$unit" TimeoutStopUSec=infinity >/dev/null 2>&1 || true
  systemctl stop --no-block "$unit" >/dev/null 2>&1 || true

  # A manually launched copy is not part of the systemd cgroup. It is safe to TERM
  # only after the same release-local inflight endpoint has stayed at zero.
  while IFS= read -r pid; do
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || continue
    kill -TERM "$pid" >/dev/null 2>&1 || true
  done < <(release_worker_pids "$release_dir")
  wait_for_unit_exit "$unit"
  wait_for_release_processes "$release_dir"
fi

systemctl disable "$unit" >/dev/null 2>&1 || true
systemctl reset-failed "$unit" >/dev/null 2>&1 || true
if [[ -n "$socket" ]]; then
  rm -f -- "$socket"
fi

if [[ -n "$release_dir" && -d "$release_dir" ]]; then
  current_target="$(readlink -f "${APP_DIR%/}/current" 2>/dev/null || true)"
  if [[ "$release_dir" == "$current_target" ]]; then
    log "${release_dir} is current; leaving it and its release storage untouched"
    disable_self
    exit 0
  fi

  # A stable handoff intentionally survives upgrades. Unlinking the immutable tree
  # is safe while that binary runs: its executable inode stays valid until its next
  # ordinary restart, and systemd already points at the compatibility symlink.
  for exe in /proc/[0-9]*/exe; do
    [[ -L "$exe" ]] || continue
    target="$(readlink "$exe" 2>/dev/null || true)"
    target="${target% (deleted)}"
    [[ "$target" == "${release_dir%/}/"* ]] || continue
    [[ "$target" == "${release_dir%/}/${HANDOFF_NAME}" ]] && continue
    die "release still has a live process after drain: ${target}"
  done

  previous_target="$(readlink -f "${APP_DIR%/}/previous" 2>/dev/null || true)"
  if [[ "$previous_target" == "$release_dir" ]]; then
    rm -f -- "${APP_DIR%/}/previous"
  fi
  rm -rf -- "$release_dir"
  [[ ! -e "$release_dir" ]] || die "release directory was not removed: ${release_dir}"
  log "reclaimed old release directory ${release_dir}"
fi

log "completed ${RELEASE}: old process, socket, unit state, and release storage reclaimed"
disable_self
