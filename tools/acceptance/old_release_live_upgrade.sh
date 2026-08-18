#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.25.12-toolchain/go/bin/go}"
OLD_RELEASE_REPOSITORY="${OLD_RELEASE_REPOSITORY:-https://github.com/3582730951/333333}"
# cache-hit-optimization currently points at the candidate revision. Its parent is
# the exact old release from the production failure report and is therefore pinned.
OLD_RELEASE_SHA="${OLD_RELEASE_SHA:-d76f7ce8545f0fa03fb4a202394f483625cea030}"
OLD_RELEASE_SOURCE="${OLD_RELEASE_SOURCE:-}"
USAGE_ROWS="${USAGE_ROWS:-50000}"
REPRO_TIMEOUT="${REPRO_TIMEOUT:-25}"
STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-60}"
LONG_INFLIGHT_SECONDS="${LONG_INFLIGHT_SECONDS:-35}"
KEEP_ARTIFACTS="${KEEP_ARTIFACTS:-0}"
ARTIFACT_DIR="${ARTIFACT_DIR:-}"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

[[ -x "$GO_BIN" ]] || fail "Go toolchain is unavailable: $GO_BIN"
[[ "$OLD_RELEASE_SHA" =~ ^[0-9a-f]{40}$ ]] || fail "OLD_RELEASE_SHA must be a full 40-character commit"
[[ "$USAGE_ROWS" =~ ^[1-9][0-9]*$ ]] || fail "USAGE_ROWS must be positive"
[[ "$REPRO_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || fail "REPRO_TIMEOUT must be positive"
[[ "$STARTUP_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || fail "STARTUP_TIMEOUT must be positive"
[[ "$LONG_INFLIGHT_SECONDS" =~ ^[0-9]+$ ]] || fail "LONG_INFLIGHT_SECONDS must be non-negative"
(( LONG_INFLIGHT_SECONDS == 0 || LONG_INFLIGHT_SECONDS > 30 )) ||
  fail "LONG_INFLIGHT_SECONDS must be zero or greater than the former 30s drain deadline"
require_command curl
require_command tar
require_command install
require_command python3

owned_artifacts=0
if [[ -z "$ARTIFACT_DIR" ]]; then
  ARTIFACT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/codex-old-live-upgrade.XXXXXX")"
  owned_artifacts=1
else
  mkdir -p "$ARTIFACT_DIR"
  ARTIFACT_DIR="$(cd "$ARTIFACT_DIR" && pwd -P)"
fi

old_pid=""
candidate_pid=""
handoff_pid=""
long_inflight_pid=""

terminate_pid() {
  local pid="$1" timeout="${2:-30}" started="$SECONDS"
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  while kill -0 "$pid" 2>/dev/null && (( SECONDS - started < timeout )); do
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    return 1
  fi
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  local status="$?"
  set +e
  if ! terminate_pid "$candidate_pid" 5; then
    kill -KILL "$candidate_pid" 2>/dev/null || true
  fi
  if ! terminate_pid "$old_pid" 5; then
    kill -KILL "$old_pid" 2>/dev/null || true
  fi
  if ! terminate_pid "$handoff_pid" 5; then
    kill -KILL "$handoff_pid" 2>/dev/null || true
  fi
  if ! terminate_pid "$long_inflight_pid" 2; then
    kill -KILL "$long_inflight_pid" 2>/dev/null || true
  fi
  if (( status == 0 )) && [[ "$KEEP_ARTIFACTS" != "1" && "$owned_artifacts" == "1" ]]; then
    find "$ARTIFACT_DIR" -depth -delete 2>/dev/null || true
  else
    printf 'Artifacts: %s\n' "$ARTIFACT_DIR" >&2
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

wait_for_file() {
  local path="$1" timeout="$2" started="$SECONDS"
  while (( SECONDS - started < timeout )); do
    [[ -e "$path" ]] && return 0
    sleep 0.1
  done
  return 1
}

wait_for_log() {
  local path="$1" pattern="$2" timeout="$3" started="$SECONDS"
  while (( SECONDS - started < timeout )); do
    grep -Fq "$pattern" "$path" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

unix_health() {
  local socket="$1" endpoint="$2"
  curl --noproxy '*' --silent --show-error --max-time 2 \
    --unix-socket "$socket" "http://localhost${endpoint}" 2>/dev/null
}

wait_for_health() {
  local socket="$1" endpoint="$2" needle="$3" timeout="$4" started="$SECONDS" payload
  while (( SECONDS - started < timeout )); do
    payload="$(unix_health "$socket" "$endpoint" || true)"
    [[ "$payload" == *"$needle"* ]] && return 0
    sleep 0.1
  done
  return 1
}

wait_for_url() {
  local url="$1" needle="$2" timeout="$3" started="$SECONDS" payload
  while (( SECONDS - started < timeout )); do
    payload="$(curl --noproxy '*' --silent --show-error --max-time 2 "$url" 2>/dev/null || true)"
    [[ "$payload" == *"$needle"* ]] && return 0
    sleep 0.1
  done
  return 1
}

handoff_post() {
  local control_socket="$1" action="$2"
  curl --noproxy '*' --silent --show-error --max-time 3 --request POST \
    --unix-socket "$control_socket" \
    "http://localhost/_codex_pool/handoff/${action}?reason=acceptance&release=candidate"
}

old_source="$ARTIFACT_DIR/old-source"
old_tar="$ARTIFACT_DIR/old-source.tar.gz"
mkdir -p "$old_source"
if [[ -n "$OLD_RELEASE_SOURCE" ]]; then
  [[ -d "$OLD_RELEASE_SOURCE" ]] || fail "OLD_RELEASE_SOURCE is not a directory: $OLD_RELEASE_SOURCE"
  cp -a "$OLD_RELEASE_SOURCE/." "$old_source/"
else
  archive_url="https://codeload.github.com/${OLD_RELEASE_REPOSITORY#https://github.com/}/tar.gz/${OLD_RELEASE_SHA}"
  printf 'Downloading pinned old release %s\n' "$OLD_RELEASE_SHA"
  curl -fsSL --retry 3 --connect-timeout 15 -o "$old_tar" "$archive_url"
  tar -xzf "$old_tar" -C "$old_source" --strip-components=1
fi
[[ -f "$old_source/go.mod" && -f "$old_source/cmd/pool-server/main.go" ]] ||
  fail "downloaded old release is incomplete"

install -m 0644 \
  "$ROOT/tools/acceptance/old-release-upgrade-fixture/lock_injector_template.go" \
  "$old_source/cmd/pool-server/zz_acceptance_lock_injector.go"

old_binary="$ARTIFACT_DIR/old-pool-server"
candidate_binary="$ARTIFACT_DIR/candidate-pool-server"
handoff_binary="$ARTIFACT_DIR/candidate-pool-handoff"
fixture_binary="$ARTIFACT_DIR/old-release-upgrade-fixture"
printf 'Building old release with deterministic same-PID lock injector\n'
(
  cd "$old_source"
  CGO_ENABLED=1 "$GO_BIN" build -buildvcs=false -tags acceptance_old_release_injector \
    -o "$old_binary" ./cmd/pool-server
)
printf 'Building current candidate and data verifier\n'
(
  cd "$ROOT"
  CGO_ENABLED=1 "$GO_BIN" build -buildvcs=false -o "$candidate_binary" ./cmd/pool-server
  CGO_ENABLED=1 "$GO_BIN" build -buildvcs=false -o "$handoff_binary" ./cmd/pool-handoff
  CGO_ENABLED=1 "$GO_BIN" build -buildvcs=false -o "$fixture_binary" \
    ./tools/acceptance/old-release-upgrade-fixture
)

runtime="$ARTIFACT_DIR/runtime"
database="$runtime/pool.sqlite3"
data_dir="$runtime/data"
old_socket="$runtime/old-worker.sock"
candidate_socket="$runtime/candidate-worker.sock"
active_link="$runtime/active-worker.sock"
handoff_control_socket="$runtime/handoff-control.sock"
handoff_pause_state="$runtime/admission-paused.json"
old_log="$ARTIFACT_DIR/old-worker.log"
handoff_log="$ARTIFACT_DIR/handoff.log"
repro_log="$ARTIFACT_DIR/candidate-reproduction.log"
recovered_log="$ARTIFACT_DIR/candidate-recovered.log"
snapshot="$ARTIFACT_DIR/pre-upgrade-snapshot.json"
arm_file="$runtime/arm-old-lock"
lock_ready_file="$runtime/old-lock-ready"
mkdir -p "$runtime" "$data_dir"
ln -s "$old_socket" "$active_link"
handoff_port="$(python3 - <<'PY'
import socket
sock = socket.socket()
sock.bind(("127.0.0.1", 0))
print(sock.getsockname()[1])
sock.close()
PY
)"

env \
  CODEX_POOL_DATABASE="$database" \
  CODEX_POOL_DATA_DIR="$data_dir" \
  CODEX_POOL_LISTEN_ADDR=127.0.0.1:0 \
  CODEX_POOL_ACCEPTANCE_LOCK_ARM_FILE="$arm_file" \
  CODEX_POOL_ACCEPTANCE_LOCK_READY_FILE="$lock_ready_file" \
  "$old_binary" --release-id "${OLD_RELEASE_SHA:0:12}" --deployment-role auto \
    --unix-socket "$old_socket" >"$old_log" 2>&1 &
old_pid=$!
wait_for_health "$old_socket" /readyz '"ready":true' "$STARTUP_TIMEOUT" ||
  fail "pinned old worker did not become ready; see $old_log"

"$handoff_binary" --listen "127.0.0.1:${handoff_port}" --backend-link "$active_link" \
  --control-socket "$handoff_control_socket" --pause-state "$handoff_pause_state" \
  --instance-id acceptance-old-upgrade >"$handoff_log" 2>&1 &
handoff_pid=$!
wait_for_health "$handoff_control_socket" /handoffz '"ready":true' "$STARTUP_TIMEOUT" ||
  fail "independent handoff control plane did not become ready; see $handoff_log"
wait_for_url "http://127.0.0.1:${handoff_port}/readyz" '"ready":true' "$STARTUP_TIMEOUT" ||
  fail "handoff did not route to the pinned old worker"

# The old process remains active while representative Codex/Kiro usage, affinity,
# Goal v2, context-journal, and root/child session data are added.
"$fixture_binary" seed --database "$database" --rows "$USAGE_ROWS"
"$fixture_binary" snapshot --database "$database" --snapshot "$snapshot"
old_health="$(unix_health "$old_socket" /readyz || true)"
[[ "$old_health" == *'"inflight":0'* ]] || fail "old worker was not idle before lock injection: $old_health"

: >"$arm_file"
wait_for_file "$lock_ready_file" "$STARTUP_TIMEOUT" ||
  fail "old worker did not acquire the deterministic SQLite lock; see $old_log"
grep -Fq "pid=${old_pid}" "$lock_ready_file" ||
  fail "SQLite lock was not owned by the old worker PID"

env \
  CODEX_POOL_DATABASE="$database" \
  CODEX_POOL_DATA_DIR="$data_dir" \
  CODEX_POOL_LISTEN_ADDR=127.0.0.1:0 \
  "$candidate_binary" --release-id candidate-reproduction --deployment-role standby \
    --unix-socket "$candidate_socket" >"$repro_log" 2>&1 &
candidate_pid=$!
wait_for_log "$repro_log" 'storage is locked by another worker' "$REPRO_TIMEOUT" ||
  fail "current candidate did not reproduce the old-generation SQLite lock; see $repro_log"
[[ ! -S "$candidate_socket" ]] || fail "locked candidate exposed /standbyz before storage was safe"
kill -0 "$old_pid" 2>/dev/null || fail "old worker died during candidate reproduction"
old_health="$(unix_health "$old_socket" /livez || true)"
[[ "$old_health" == *'"ok":true'* ]] || fail "old worker liveness was lost during reproduction: $old_health"
handoff_state="$(unix_health "$handoff_control_socket" /handoffz || true)"
[[ "$handoff_state" == *'"inflight":0'* ]] ||
  fail "independent handoff reported requests during reproduction: $handoff_state"

terminate_pid "$candidate_pid" 15 || fail "failed reproduction candidate did not terminate gracefully"
candidate_pid=""
kill -0 "$old_pid" 2>/dev/null || fail "failed candidate disturbed the active old worker"

# This is the installer recovery invariant: admissions are paused by the real
# handoff, established requests are proven zero, and only the identified old PID is
# gracefully stopped. No unknown holder is killed.
long_inflight_ready="$runtime/long-inflight-ready"
if (( LONG_INFLIGHT_SECONDS > 0 )); then
  python3 - "$handoff_port" "$LONG_INFLIGHT_SECONDS" "$long_inflight_ready" <<'PY' &
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
  long_inflight_pid=$!
  wait_for_file "$long_inflight_ready" 10 || fail "long in-flight request did not reach handoff"
  wait_for_health "$handoff_control_socket" /handoffz '"inflight":1' 10 || {
    long_state="$(unix_health "$handoff_control_socket" /handoffz || true)"
    fail "handoff did not count the long request before pause: $long_state"
  }
fi
pause_response="$(handoff_post "$handoff_control_socket" pause || true)"
[[ "$pause_response" == *'"admission_paused":true'* ]] ||
  fail "independent handoff did not acknowledge the admission pause: $pause_response"
if (( LONG_INFLIGHT_SECONDS > 0 )); then
  sleep 31
  kill -0 "$old_pid" 2>/dev/null || fail "old worker was stopped at the former 30s drain deadline"
  handoff_state="$(unix_health "$handoff_control_socket" /handoffz || true)"
  [[ "$handoff_state" == *'"admission_paused":true'* && "$handoff_state" == *'"inflight":1'* ]] ||
    fail "long request was cut at the former 30s drain deadline: $handoff_state"
  wait "$long_inflight_pid" || fail "long in-flight client failed"
  long_inflight_pid=""
  wait_for_health "$handoff_control_socket" /handoffz '"inflight":0' 15 ||
    fail "handoff did not release the naturally completed long request"
fi
handoff_state="$(unix_health "$handoff_control_socket" /handoffz || true)"
[[ "$handoff_state" == *'"admission_paused":true'* && "$handoff_state" == *'"inflight":0'* ]] ||
  fail "refusing recovery without a paused zero-inflight handoff: $handoff_state"
terminate_pid "$old_pid" 30 || fail "old worker did not gracefully release SQLite"
old_pid=""

env \
  CODEX_POOL_DATABASE="$database" \
  CODEX_POOL_DATA_DIR="$data_dir" \
  CODEX_POOL_LISTEN_ADDR=127.0.0.1:0 \
  "$candidate_binary" --release-id candidate-recovered --deployment-role auto \
    --unix-socket "$candidate_socket" >"$recovered_log" 2>&1 &
candidate_pid=$!
wait_for_health "$candidate_socket" /standbyz '"standby_ready":true' "$STARTUP_TIMEOUT" ||
  fail "candidate did not start after safe old-worker quiesce; see $recovered_log"
next_link="$runtime/active-worker.next"
ln -s "$candidate_socket" "$next_link"
mv -Tf "$next_link" "$active_link"
wait_for_url "http://127.0.0.1:${handoff_port}/readyz" '"release_id":"candidate-recovered"' "$STARTUP_TIMEOUT" ||
  fail "independent handoff did not route to the recovered candidate"
resume_response="$(handoff_post "$handoff_control_socket" resume || true)"
[[ "$resume_response" == *'"admission_paused":false'* ]] ||
  fail "independent handoff did not resume admission: $resume_response"
"$fixture_binary" verify --database "$database" --snapshot "$snapshot"

python3 - "$ARTIFACT_DIR/report.json" "$OLD_RELEASE_SHA" "$USAGE_ROWS" "$LONG_INFLIGHT_SECONDS" <<'PY'
import json
import sys
from pathlib import Path

path, old_sha, usage_rows, long_inflight_seconds = sys.argv[1:]
report = {
    "format": "codex-pool-old-release-live-upgrade",
    "old_release": old_sha,
    "usage_rows": int(usage_rows),
    "old_worker_running_during_seed": True,
    "same_pid_sqlite_lock_reproduced": True,
    "candidate_socket_blocked_while_locked": True,
    "old_worker_survived_failed_candidate": True,
    "independent_handoff_paused": True,
    "long_inflight_seconds": int(long_inflight_seconds),
    "long_inflight_survived_former_30s_deadline": int(long_inflight_seconds) == 0 or int(long_inflight_seconds) > 30,
    "inflight_before_quiesce": 0,
    "graceful_old_worker_stop": True,
    "candidate_ready_after_lock_release": True,
    "atomic_backend_switch_ready": True,
    "independent_handoff_resumed": True,
    "pre_post_snapshot_equal": True,
}
Path(path).write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
PY

printf 'PASS: old=%s usage=%s lock reproduced, safely quiesced, candidate ready, snapshot unchanged\n' \
  "$OLD_RELEASE_SHA" "$USAGE_ROWS"
if [[ "$KEEP_ARTIFACTS" == "1" || "$owned_artifacts" == "0" ]]; then
  printf 'Report: %s/report.json\n' "$ARTIFACT_DIR"
fi
