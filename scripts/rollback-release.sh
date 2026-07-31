#!/usr/bin/env bash
set -Eeuo pipefail

SERVICE_NAME="${SERVICE_NAME:-codex-pool}"
APP_DIR="${APP_DIR:-/usr/local/lib/codex-pool}"
DATA_DIR="${DATA_DIR:-/var/lib/codex-pool}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-300}"
WORKER_DESTROY_TIMEOUT="${WORKER_DESTROY_TIMEOUT:-30}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/var/lock/codex-pool-install.lock}"
HANDOFF_CONTROL_SOCKET="${HANDOFF_CONTROL_SOCKET:-${DATA_DIR%/}/run/handoff-control.sock}"
CODEX_REAUTH_URL="${CODEX_REAUTH_URL:-http://127.0.0.1:8802}"
HANDOFF_PAUSED=0
KEEP_ADMISSION_PAUSED=0

[[ "$(id -u)" -eq 0 ]] || { echo "run rollback as root" >&2; exit 1; }
command -v flock >/dev/null
[[ "$HEALTH_TIMEOUT" =~ ^[1-9][0-9]*$ && "$DRAIN_TIMEOUT" =~ ^[0-9]+$ && "$WORKER_DESTROY_TIMEOUT" =~ ^[0-9]+$ ]] || { echo "invalid timeout" >&2; exit 1; }
exec 9>"$DEPLOY_LOCK_FILE"
flock -n 9 || { echo "another deployment is running" >&2; exit 1; }

resume_admission() {
  local payload
  [[ "$HANDOFF_PAUSED" == 1 ]] || return 0
  payload="$(curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
    --unix-socket "$HANDOFF_CONTROL_SOCKET" \
    http://localhost/_codex_pool/handoff/resume 2>/dev/null || true)"
  [[ "$payload" == *'"admission_paused":false'* ]] || return 1
  HANDOFF_PAUSED=0
}

atomic_link() {
  local target="$1" link="$2" tmp
  tmp="${link}.next.$$"
  ln -s "$target" "$tmp"
  mv -Tf "$tmp" "$link"
}

destroy_worker() {
  local release="$1" socket="${2:-}" unit state pid destroyed=0
  [[ "$release" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || return 1
  unit="${SERVICE_NAME}-worker@${release}.service"
  systemctl stop --no-block "$unit" >/dev/null 2>&1 || true
  for ((attempt=0; attempt<WORKER_DESTROY_TIMEOUT*5; attempt++)); do
    state="$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
    pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
    if [[ ( -z "$state" || "$state" == inactive || "$state" == failed ) && ( -z "$pid" || "$pid" == 0 ) ]]; then
      destroyed=1
      break
    fi
    sleep 0.2
  done
  if [[ "$destroyed" != 1 ]]; then
    systemctl kill --kill-who=all --signal=SIGKILL "$unit" >/dev/null 2>&1 || true
    pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -KILL "$pid" >/dev/null 2>&1 || true
    systemctl stop "$unit" >/dev/null 2>&1 || true
    # systemd can report "deactivating" briefly after the cgroup is already
    # signalled. Reap and re-check rather than misclassifying that transition as
    # an immortal second worker.
    for ((attempt=0; attempt<50; attempt++)); do
      state="$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
      pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
      if [[ ( -z "$state" || "$state" == inactive || "$state" == failed ) && ( -z "$pid" || "$pid" == 0 ) ]]; then
        destroyed=1
        break
      fi
      sleep 0.2
    done
  fi
  systemctl disable "$unit" >/dev/null 2>&1 || true
  systemctl reset-failed "$unit" >/dev/null 2>&1 || true
  [[ -z "$socket" ]] || rm -f -- "$socket"
  state="$(systemctl show "$unit" --property=ActiveState --value 2>/dev/null || true)"
  pid="$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)"
  [[ ( -z "$state" || "$state" == inactive || "$state" == failed ) && ( -z "$pid" || "$pid" == 0 ) ]]
}

cleanup() {
  local status="$?"
  if [[ "$HANDOFF_PAUSED" == 1 && "$KEEP_ADMISSION_PAUSED" != 1 ]]; then
    set +e
    resume_admission || echo "handoff admission resume failed during rollback cleanup" >&2
    set -e
  elif [[ "$HANDOFF_PAUSED" == 1 ]]; then
    echo "handoff admission remains paused because rollback could not prove a single active worker" >&2
  fi
  return "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

current="$(readlink "${APP_DIR%/}/current")"
previous="$(readlink "${APP_DIR%/}/previous")"
[[ -n "$current" && -d "$current" ]] || { echo "current release is missing" >&2; exit 1; }
[[ -n "$previous" && -d "$previous" ]] || { echo "previous release is missing" >&2; exit 1; }

previous_id="${previous##*/}"
previous_socket="${DATA_DIR%/}/run/worker-${previous_id}.sock"
current_id="${current##*/}"
current_socket="${DATA_DIR%/}/run/worker-${current_id}.sock"
[[ "$previous_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ && "$current_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ && "$previous_id" != "$current_id" ]] ||
  { echo "invalid rollback release ids" >&2; exit 1; }
systemctl start "${SERVICE_NAME}-worker@${previous_id}.service"

ready=0
for ((i=0; i<HEALTH_TIMEOUT; i++)); do
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$previous_socket" http://localhost/standbyz 2>/dev/null || true)"
  if [[ "$payload" == *'"standby_ready":true'* && "$payload" == *"\"release_id\":\"${previous_id}\""* ]]; then
    ready=1
    break
  fi
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$previous_socket" http://localhost/readyz 2>/dev/null || true)"
  if [[ "$payload" == *'"ready":true'* && "$payload" == *"\"release_id\":\"${previous_id}\""* ]]; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  destroy_worker "$previous_id" "$previous_socket" || true
  echo "previous worker did not become ready" >&2
  exit 1
fi
if ! systemctl enable "${SERVICE_NAME}-worker@${previous_id}.service" >/dev/null; then
  destroy_worker "$previous_id" "$previous_socket" || true
  echo "rollback worker could not be enabled for reboot" >&2
  exit 1
fi

pause_payload="$(curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
  --unix-socket "$HANDOFF_CONTROL_SOCKET" \
  "http://localhost/_codex_pool/handoff/pause?reason=rollback&release=${previous_id}" 2>/dev/null || true)"
if [[ "$pause_payload" != *'"admission_paused":true'* ]]; then
  destroy_worker "$previous_id" "$previous_socket" || true
  echo "handoff did not pause request admission" >&2
  exit 1
fi
HANDOFF_PAUSED=1

# The backend link is the traffic linearization point. Established streams retain
# their old Unix connection; only requests arriving after this rename use previous.
if ! atomic_link "$previous_socket" "${DATA_DIR%/}/run/active-worker.sock"; then
  destroy_worker "$previous_id" "$previous_socket" || true
  resume_admission || true
  echo "failed to switch rollback traffic" >&2
  exit 1
fi

ready=0
for ((i=0; i<HEALTH_TIMEOUT; i++)); do
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$previous_socket" http://localhost/readyz 2>/dev/null || true)"
  if [[ "$payload" == *'"ready":true'* && "$payload" == *"\"release_id\":\"${previous_id}\""* ]]; then
    ready=1
    break
  fi
  sleep 1
done
if [[ "$ready" != 1 ]]; then
  atomic_link "$current_socket" "${DATA_DIR%/}/run/active-worker.sock"
  destroy_worker "$previous_id" "$previous_socket" || true
  resume_admission || true
  echo "previous worker did not acquire the active fencing lease; current release restored" >&2
  exit 1
fi

if ! atomic_link "$previous" "${APP_DIR%/}/current" ||
    ! atomic_link "$current" "${APP_DIR%/}/previous"; then
  atomic_link "$current_socket" "${DATA_DIR%/}/run/active-worker.sock" || true
  atomic_link "$current" "${APP_DIR%/}/current" || true
  atomic_link "$previous" "${APP_DIR%/}/previous" || true
  destroy_worker "$previous_id" "$previous_socket" || true
  resume_admission || true
  echo "failed to publish rollback release links; original release restored" >&2
  exit 1
fi
resume_failed=0
resume_admission || {
  resume_failed=1
  echo "rollback traffic switched; admission resume will be retried after superseded-worker destruction" >&2
}

inflight="unknown"
for ((i=0; i<DRAIN_TIMEOUT; i++)); do
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$current_socket" http://localhost/readyz 2>/dev/null || true)"
  inflight="$(printf '%s' "$payload" | sed -n 's/.*"inflight":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
  if [[ "$inflight" == 0 ]]; then
    break
  fi
  sleep 1
done
if [[ "$inflight" != 0 ]]; then
  echo "release ${current_id} still has ${inflight:-unknown} in-flight connection(s); enforcing rollback drain deadline" >&2
fi

if ! destroy_worker "$current_id" "$current_socket"; then
  echo "superseded worker ${SERVICE_NAME}-worker@${current_id}.service is still running; aborting rollback to restore the original single worker" >&2
  restore_ok=1
  atomic_link "$current_socket" "${DATA_DIR%/}/run/active-worker.sock" || restore_ok=0
  atomic_link "$current" "${APP_DIR%/}/current" || restore_ok=0
  atomic_link "$previous" "${APP_DIR%/}/previous" || restore_ok=0
  systemctl enable "${SERVICE_NAME}-worker@${current_id}.service" >/dev/null || restore_ok=0
  systemctl start "${SERVICE_NAME}-worker@${current_id}.service" >/dev/null || restore_ok=0
  ready=0
  if [[ "$restore_ok" == 1 ]]; then
    for ((i=0; i<HEALTH_TIMEOUT; i++)); do
      payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$current_socket" http://localhost/readyz 2>/dev/null || true)"
      if [[ "$payload" == *'"ready":true'* && "$payload" == *"\"release_id\":\"${current_id}\""* ]]; then
        ready=1
        break
      fi
      sleep 1
    done
  fi
  [[ "$ready" == 1 ]] || restore_ok=0
  destroy_worker "$previous_id" "$previous_socket" || restore_ok=0
  mapfile -t restored_workers < <(systemctl list-units --type=service --state=active --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null | awk '{print $1}')
  [[ "${#restored_workers[@]}" == 1 && "${restored_workers[0]}" == "${SERVICE_NAME}-worker@${current_id}.service" ]] || restore_ok=0
  if [[ "$restore_ok" == 1 ]]; then
    resume_admission || { KEEP_ADMISSION_PAUSED=1; echo "original worker restored but admission resume failed" >&2; exit 1; }
    echo "rollback aborted; original release ${current_id} restored and rollback candidate ${previous_id} destroyed" >&2
    exit 1
  fi
  KEEP_ADMISSION_PAUSED=1
  echo "rollback invariant is unresolved; handoff admission is held closed for operator inspection" >&2
  exit 1
fi

# Remove any orphan instance left by an older interrupted installer. A rollback
# is complete only when the selected release is the sole live worker.
mapfile -t loaded_workers < <(
  {
    systemctl list-units --type=service --all --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
    systemctl list-unit-files --type=service --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null || true
  } | awk '{print $1}' | grep -F "${SERVICE_NAME}-worker@" | grep -E '@[^[:space:]]+\.service$' | sort -u
)
for unit in "${loaded_workers[@]}"; do
  [[ -n "$unit" && "$unit" != "${SERVICE_NAME}-worker@${previous_id}.service" ]] || continue
  release="${unit#${SERVICE_NAME}-worker@}"
  release="${release%.service}"
  [[ "$release" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] ||
    { echo "malformed stale worker unit: ${unit}" >&2; exit 1; }
  destroy_worker "$release" "${DATA_DIR%/}/run/worker-${release}.sock" ||
    { echo "stale worker ${unit} is still running" >&2; exit 1; }
done

mapfile -t active_workers < <(systemctl list-units --type=service --state=active --plain --no-legend "${SERVICE_NAME}-worker@*.service" 2>/dev/null | awk '{print $1}')
[[ "${#active_workers[@]}" == 1 && "${active_workers[0]}" == "${SERVICE_NAME}-worker@${previous_id}.service" ]] ||
  { echo "single-worker invariant failed after rollback: ${active_workers[*]:-none}" >&2; exit 1; }
if [[ "$resume_failed" == 1 ]]; then
  resume_admission || { echo "single worker is active but handoff admission remains paused" >&2; exit 1; }
fi
if systemctl cat "${SERVICE_NAME}-sidecar.service" >/dev/null 2>&1; then
  systemctl restart "${SERVICE_NAME}-sidecar.service" || true
fi
if systemctl cat "${SERVICE_NAME}-reauth.service" >/dev/null 2>&1; then
  if [[ -x "${APP_DIR%/}/current/codex-reauth/codex_reauth_worker.py" &&
        -x "${APP_DIR%/}/current/registrar-python-venv/bin/python" ]]; then
    systemctl restart "${SERVICE_NAME}-reauth.service"
    reauth_ready=0
    for ((i=0; i<HEALTH_TIMEOUT; i++)); do
      payload="$(curl --noproxy '*' --silent --max-time 2 "${CODEX_REAUTH_URL%/}/healthz" 2>/dev/null || true)"
      if [[ "$payload" == *'"ready":true'* ]]; then
        reauth_ready=1
        break
      fi
      sleep 1
    done
    [[ "$reauth_ready" == 1 ]] || {
      echo "rolled-back Codex reauth worker did not become ready" >&2
      exit 1
    }
  else
    systemctl stop "${SERVICE_NAME}-reauth.service" >/dev/null 2>&1 || true
  fi
fi
echo "rolled back to release ${previous_id}; release ${current_id} destroyed; exactly one worker remains"
