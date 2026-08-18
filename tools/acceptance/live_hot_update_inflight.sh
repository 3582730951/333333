#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.25.12/bin/go}"
CONNECTION_SECONDS="${CONNECTION_SECONDS:-15}"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[[ -x "$GO_BIN" ]] || fail "Go toolchain unavailable: $GO_BIN"
[[ "$CONNECTION_SECONDS" =~ ^[1-9][0-9]*$ ]] || fail "CONNECTION_SECONDS must be positive"
for command_name in curl python3; do
  command -v "$command_name" >/dev/null 2>&1 || fail "missing command: $command_name"
done

runtime="$(mktemp -d "${TMPDIR:-/tmp}/codex-live-hot-update.XXXXXX")"
old_pid=""
candidate_pid=""
handoff_pid=""
client_pid=""

terminate_pid() {
  local pid="$1" started="$SECONDS"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  while kill -0 "$pid" 2>/dev/null && (( SECONDS - started < 10 )); do
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  local status="$?"
  set +e
  terminate_pid "$client_pid"
  terminate_pid "$candidate_pid"
  terminate_pid "$old_pid"
  terminate_pid "$handoff_pid"
  if (( status == 0 )); then
    find "$runtime" -depth -delete 2>/dev/null || true
  else
    printf 'Artifacts: %s\n' "$runtime" >&2
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

unix_get() {
  local socket="$1" path="$2"
  curl --noproxy '*' --silent --show-error --max-time 2 \
    --unix-socket "$socket" "http://localhost${path}" 2>/dev/null
}

wait_unix() {
  local socket="$1" path="$2" needle="$3" timeout="$4" started="$SECONDS" payload
  while (( SECONDS - started < timeout )); do
    payload="$(unix_get "$socket" "$path" || true)"
    [[ "$payload" == *"$needle"* ]] && return 0
    sleep 0.1
  done
  return 1
}

wait_url() {
  local url="$1" needle="$2" timeout="$3" started="$SECONDS" payload
  while (( SECONDS - started < timeout )); do
    payload="$(curl --noproxy '*' --silent --show-error --max-time 2 "$url" 2>/dev/null || true)"
    [[ "$payload" == *"$needle"* ]] && return 0
    sleep 0.1
  done
  return 1
}

old_binary="$runtime/old-worker"
candidate_binary="$runtime/candidate-worker"
handoff_binary="$runtime/handoff"
(
  cd "$ROOT"
  "$GO_BIN" build -buildvcs=false -o "$old_binary" ./cmd/pool-server
  cp "$old_binary" "$candidate_binary"
  "$GO_BIN" build -buildvcs=false -o "$handoff_binary" ./cmd/pool-handoff
)

data_dir="$runtime/data"
database="$runtime/pool.sqlite3"
old_socket="$runtime/worker-old.sock"
candidate_socket="$runtime/worker-candidate.sock"
active_link="$runtime/active-worker.sock"
control_socket="$runtime/handoff-control.sock"
pause_state="$runtime/admission-pause.json"
ready_file="$runtime/long-client-ready"
mkdir -p "$data_dir"
ln -s "$old_socket" "$active_link"

handoff_port="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"

env CODEX_POOL_DATABASE="$database" CODEX_POOL_DATA_DIR="$data_dir" \
  CODEX_POOL_LISTEN_ADDR=127.0.0.1:0 \
  "$old_binary" --release-id old-active --deployment-role auto \
  --unix-socket "$old_socket" >"$runtime/old.log" 2>&1 &
old_pid=$!
wait_unix "$old_socket" /readyz '"ready":true' 45 || fail "old worker not ready"

"$handoff_binary" --listen "127.0.0.1:${handoff_port}" --backend-link "$active_link" \
  --control-socket "$control_socket" --pause-state "$pause_state" \
  --instance-id hot-update-inflight >"$runtime/handoff.log" 2>&1 &
handoff_pid=$!
wait_unix "$control_socket" /handoffz '"ready":true' 30 || fail "handoff not ready"
wait_url "http://127.0.0.1:${handoff_port}/readyz" '"release_id":"old-active"' 30 ||
  fail "handoff did not route old worker"

# Keep a real downstream TCP request open while the backend link changes. The
# incomplete body blocks in the old worker without creating provider traffic.
python3 - "$handoff_port" "$CONNECTION_SECONDS" "$ready_file" <<'PY' &
import pathlib
import socket
import sys
import time

port, duration, ready = int(sys.argv[1]), int(sys.argv[2]), pathlib.Path(sys.argv[3])
sock = socket.create_connection(("127.0.0.1", port), timeout=5)
sock.sendall(
    b"POST /v1/responses HTTP/1.1\r\n"
    b"Host: localhost\r\n"
    b"Content-Type: application/json\r\n"
    b"Content-Length: 1048576\r\n"
    b"Connection: close\r\n\r\n{"
)
ready.write_text("connected\n", encoding="utf-8")
time.sleep(duration)
sock.close()
PY
client_pid=$!
for _ in $(seq 1 100); do
  [[ -f "$ready_file" ]] && break
  sleep 0.1
done
[[ -f "$ready_file" ]] || fail "long downstream connection did not start"
wait_unix "$control_socket" /handoffz '"inflight":1' 10 || fail "handoff did not count long connection"

env CODEX_POOL_DATABASE="$database" CODEX_POOL_DATA_DIR="$data_dir" \
  CODEX_POOL_LISTEN_ADDR=127.0.0.1:0 \
  "$candidate_binary" --release-id candidate-active --deployment-role auto \
  --unix-socket "$candidate_socket" >"$runtime/candidate.log" 2>&1 &
candidate_pid=$!
wait_unix "$candidate_socket" /standbyz '"deployment_state":"preinit_standby"' 30 ||
  fail "candidate did not enter database-free preinit standby"

pause_response="$(curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
  --unix-socket "$control_socket" \
  'http://localhost/_codex_pool/handoff/pause?reason=acceptance&release=candidate-active')"
[[ "$pause_response" == *'"admission_paused":true'* ]] || fail "handoff pause failed"
state="$(unix_get "$control_socket" /handoffz || true)"
[[ "$state" == *'"inflight":1'* ]] || fail "connection drained before switch: $state"

next_link="$runtime/active-worker.next"
ln -s "$candidate_socket" "$next_link"
mv -Tf "$next_link" "$active_link"
wait_url "http://127.0.0.1:${handoff_port}/readyz" '"release_id":"candidate-active"' 45 ||
  fail "promoted candidate did not complete full startup"
kill -0 "$client_pid" 2>/dev/null || fail "established downstream connection was cut during promotion"
kill -0 "$old_pid" 2>/dev/null || fail "old worker exited before its established request drained"

resume_response="$(curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
  --unix-socket "$control_socket" 'http://localhost/_codex_pool/handoff/resume')"
[[ "$resume_response" == *'"admission_paused":false'* ]] || fail "handoff resume failed"
wait_url "http://127.0.0.1:${handoff_port}/livez" '"release_id":"candidate-active"' 10 ||
  fail "new requests did not reach candidate"

wait "$client_pid"
client_pid=""
printf 'PASS: candidate promoted and admitted new traffic while established old-worker request remained connected\n'
