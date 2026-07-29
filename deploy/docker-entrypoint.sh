#!/bin/sh
set -eu

run_dir=/var/lib/codex-pool/run
secret_dir=/run/secrets
release_id="${CODEX_POOL_RELEASE_ID:-docker}"
worker_socket="${run_dir}/worker-${release_id}.sock"
active_link="${run_dir}/active-worker.sock"
worker=/usr/local/lib/codex-pool/releases/docker/codex-pool-server

if [ -f "${secret_dir}/codex_pool_master_key" ] && [ -z "${CODEX_POOL_MASTER_KEY_FILE:-}" ]; then
  export CODEX_POOL_MASTER_KEY_FILE="${secret_dir}/codex_pool_master_key"
fi
if [ -f "${secret_dir}/codex_pool_identity_key" ] && [ -z "${CODEX_POOL_IDENTITY_KEY_FILE:-}" ]; then
  export CODEX_POOL_IDENTITY_KEY_FILE="${secret_dir}/codex_pool_identity_key"
fi
if [ -f "${secret_dir}/codex_pool_diagnostic_alias_key" ] && [ -z "${CODEX_POOL_DIAGNOSTIC_ALIAS_KEY_FILE:-}" ]; then
  export CODEX_POOL_DIAGNOSTIC_ALIAS_KEY_FILE="${secret_dir}/codex_pool_diagnostic_alias_key"
fi
if [ -f "${secret_dir}/codex_pool_admin_token" ] && [ -z "${CODEX_POOL_ADMIN_TOKEN_FILE:-}" ]; then
  export CODEX_POOL_ADMIN_TOKEN_FILE="${secret_dir}/codex_pool_admin_token"
fi

mkdir -p "$run_dir"
rm -f "$worker_socket"
"$worker" "$@" --release-id "$release_id" --deployment-role active --unix-socket "$worker_socket" &
worker_pid=$!

i=0
while [ "$i" -lt 120 ]; do
  if curl --noproxy '*' -fsS --max-time 1 --unix-socket "$worker_socket" http://localhost/readyz >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$worker_pid" 2>/dev/null; then
    wait "$worker_pid"
    exit $?
  fi
  i=$((i + 1))
  sleep 1
done
[ "$i" -lt 120 ] || { echo "worker readiness timeout" >&2; kill "$worker_pid"; exit 1; }

ln -sfn "$worker_socket" "${active_link}.next"
mv -Tf "${active_link}.next" "$active_link"

/usr/local/bin/codex-pool-handoff \
  --listen "${CODEX_POOL_LISTEN_ADDR:-0.0.0.0:8787}" \
  --backend-link "$active_link" \
  --control-socket "${run_dir}/handoff-control.sock" \
  --pause-state "${run_dir}/admission-paused.json" \
  --instance-id "$release_id" &
handoff_pid=$!

stopping=0
terminate() {
  stopping=1
  kill -TERM "$handoff_pid" "$worker_pid" 2>/dev/null || true
}
trap terminate TERM INT HUP

# POSIX sh has no portable wait -n. Monitor both children so failure of either tears
# down its peer; the trap makes PID 1 forward container stop signals to both first.
while kill -0 "$handoff_pid" 2>/dev/null && kill -0 "$worker_pid" 2>/dev/null; do
  sleep 1 &
  wait $! || true
done
if [ "$stopping" -eq 0 ]; then
  kill -TERM "$handoff_pid" "$worker_pid" 2>/dev/null || true
fi
handoff_status=0
worker_status=0
wait "$handoff_pid" || handoff_status=$?
wait "$worker_pid" || worker_status=$?
[ "$stopping" -eq 0 ] || exit 0
[ "$handoff_status" -eq 0 ] && [ "$worker_status" -eq 0 ]
