#!/usr/bin/env bash
set -Eeuo pipefail

SERVICE_NAME="${SERVICE_NAME:-codex-pool}"
APP_DIR="${APP_DIR:-/usr/local/lib/codex-pool}"
DATA_DIR="${DATA_DIR:-/var/lib/codex-pool}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-180}"
DRAIN_TIMEOUT="${DRAIN_TIMEOUT:-300}"
DEPLOY_LOCK_FILE="${DEPLOY_LOCK_FILE:-/var/lock/codex-pool-install.lock}"
HANDOFF_CONTROL_SOCKET="${HANDOFF_CONTROL_SOCKET:-${DATA_DIR%/}/run/handoff-control.sock}"
HANDOFF_PAUSED=0

[[ "$(id -u)" -eq 0 ]] || { echo "run rollback as root" >&2; exit 1; }
command -v flock >/dev/null
[[ "$HEALTH_TIMEOUT" =~ ^[1-9][0-9]*$ && "$DRAIN_TIMEOUT" =~ ^[0-9]+$ ]] || { echo "invalid timeout" >&2; exit 1; }
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

cleanup() {
  local status="$?"
  if [[ "$HANDOFF_PAUSED" == 1 ]]; then
    set +e
    resume_admission || echo "handoff admission resume failed during rollback cleanup" >&2
    set -e
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
systemctl start "${SERVICE_NAME}-worker@${previous_id}.service"

ready=0
for ((i=0; i<HEALTH_TIMEOUT; i++)); do
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$previous_socket" http://localhost/readyz 2>/dev/null || true)"
  if [[ "$payload" == *'"ready":true'* && "$payload" == *"\"release_id\":\"${previous_id}\""* ]]; then
    ready=1
    break
  fi
  sleep 1
done
[[ "$ready" == 1 ]] || { echo "previous worker did not become ready" >&2; exit 1; }

pause_payload="$(curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
  --unix-socket "$HANDOFF_CONTROL_SOCKET" \
  "http://localhost/_codex_pool/handoff/pause?reason=rollback&release=${previous_id}" 2>/dev/null || true)"
[[ "$pause_payload" == *'"admission_paused":true'* ]] || { echo "handoff did not pause request admission" >&2; exit 1; }
HANDOFF_PAUSED=1

atomic_link() {
  local target="$1" link="$2" tmp
  tmp="${link}.next.$$"
  ln -s "$target" "$tmp"
  mv -Tf "$tmp" "$link"
}

# The backend link is the traffic linearization point. Established streams retain
# their old Unix connection; only requests arriving after this rename use previous.
atomic_link "$previous" "${APP_DIR%/}/current"
atomic_link "$previous_socket" "${DATA_DIR%/}/run/active-worker.sock"
atomic_link "$current" "${APP_DIR%/}/previous"
resume_admission || { echo "release switched but queued request admission did not resume" >&2; exit 1; }

current_id="${current##*/}"
current_socket="${DATA_DIR%/}/run/worker-${current_id}.sock"
for ((i=0; i<DRAIN_TIMEOUT; i++)); do
  payload="$(curl --noproxy '*' --silent --max-time 2 --unix-socket "$current_socket" http://localhost/readyz 2>/dev/null || true)"
  inflight="$(printf '%s' "$payload" | sed -n 's/.*"inflight":[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
	if [[ "$inflight" == 0 ]]; then
		systemctl stop "${SERVICE_NAME}-worker@${current_id}.service" || true
		if systemctl cat "${SERVICE_NAME}-sidecar.service" >/dev/null 2>&1; then
			systemctl restart "${SERVICE_NAME}-sidecar.service" || true
		fi
    echo "rolled back to release ${previous_id}; release ${current_id} drained and stopped"
    exit 0
  fi
  sleep 1
done
echo "rolled back to release ${previous_id}; release ${current_id} and its current sidecar are still draining and remain running"
