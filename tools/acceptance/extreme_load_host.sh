#!/usr/bin/env bash
set -euo pipefail

BASE_DIR="${BASE_DIR:-$(pwd)}"
EXTREME_LOAD_BIN="${EXTREME_LOAD_BIN:-$BASE_DIR/bin/extreme-load}"
POOL_SERVER_BIN="${POOL_SERVER_BIN:-$BASE_DIR/bin/pool-server}"
TIKTOKEN_CACHE_DIR="${TIKTOKEN_CACHE_DIR:-$BASE_DIR/bin}"
FIXTURE_COUNT="${FIXTURE_COUNT:-256}"
TARGET_TOKENS="${TARGET_TOKENS:-1000000}"
FIXTURE_PROFILE="${FIXTURE_PROFILE:-mixed-agent}"
TARGET_RPS="${TARGET_RPS:-100}"
MINIMUM_ACHIEVED_RPS="${MINIMUM_ACHIEVED_RPS:-$TARGET_RPS}"
LOAD_DURATION="${LOAD_DURATION:-10s}"
LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-256}"
FIXTURE_WORKERS="${FIXTURE_WORKERS:-16}"
CPU_SET="${CPU_SET:-0,1}"
MEMORY_LIMIT_KIB="${MEMORY_LIMIT_KIB:-2097152}"
POOL_GOMAXPROCS="${POOL_GOMAXPROCS:-2}"
BODY_MEMORY_THRESHOLD_BYTES="${BODY_MEMORY_THRESHOLD_BYTES:-8388608}"
DEFAULT_BODY_MEMORY_BUDGET_BYTES="$((MEMORY_LIMIT_KIB * 1024 / 4))"
if (( DEFAULT_BODY_MEMORY_BUDGET_BYTES > 536870912 )); then
  DEFAULT_BODY_MEMORY_BUDGET_BYTES=536870912
elif (( DEFAULT_BODY_MEMORY_BUDGET_BYTES < 67108864 )); then
  DEFAULT_BODY_MEMORY_BUDGET_BYTES=67108864
fi
BODY_MEMORY_BUDGET_BYTES="${BODY_MEMORY_BUDGET_BYTES:-$DEFAULT_BODY_MEMORY_BUDGET_BYTES}"
HEAP_PROFILE_KIB="${HEAP_PROFILE_KIB:-0}"
MOCK_PORT="${MOCK_PORT:-29443}"
POOL_PORT="${POOL_PORT:-28787}"
ADMIN_TOKEN="${ADMIN_TOKEN:-extreme-host-acceptance-admin-token-20260727}"
RUN_ROOT="${RUN_ROOT:-$BASE_DIR/run}"
RUN_DIR="$(mktemp -d "$RUN_ROOT/extreme.XXXXXX")"
RUN_OWNER_UID="$(stat -c '%u' "$RUN_DIR")"
RUN_OWNER_GID="$(stat -c '%g' "$RUN_DIR")"
FIXTURE_DIR="${REUSE_FIXTURE_DIR:-$RUN_DIR/fixtures}"
MOCK_PID=""
POOL_PID=""
RESOURCE_PID=""
RUNTIME_PID=""
CPU_PROFILE_PATH=""
if [[ "${CAPTURE_CPU_PROFILE:-0}" == "1" ]]; then
  CPU_PROFILE_PATH="$RUN_DIR/cpu.pprof"
fi

cleanup() {
  for pid in "$RUNTIME_PID" "$RESOURCE_PID" "$POOL_PID" "$MOCK_PID"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; fi
  done
  for pid in "$RUNTIME_PID" "$RESOURCE_PID" "$POOL_PID" "$MOCK_PID"; do
    if [[ -n "$pid" ]]; then wait "$pid" 2>/dev/null || true; fi
  done
  if [[ "${KEEP_ARTIFACTS:-1}" == "1" ]]; then
    echo "acceptance artifacts retained at $RUN_DIR"
  elif [[ "$RUN_DIR" == "$RUN_ROOT"/extreme.* ]]; then
    find "$RUN_DIR" -depth -delete
  fi
}
trap cleanup EXIT INT TERM

stop_pool_gracefully() {
  for pid in "$RUNTIME_PID" "$RESOURCE_PID"; do
    if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; fi
  done
  for pid in "$RUNTIME_PID" "$RESOURCE_PID"; do
    if [[ -n "$pid" ]]; then wait "$pid" 2>/dev/null || true; fi
  done
  RUNTIME_PID=""
  RESOURCE_PID=""
  if [[ -z "$POOL_PID" ]]; then return 0; fi
  kill -TERM "$POOL_PID" 2>/dev/null || true
  for _ in $(seq 1 300); do
    if ! kill -0 "$POOL_PID" 2>/dev/null; then break; fi
    sleep 0.1
  done
  if kill -0 "$POOL_PID" 2>/dev/null; then
    echo "pool did not complete its bounded graceful drain" >&2
	# SIGQUIT gives the retained acceptance artifact a complete Go goroutine dump,
	# making shutdown regressions diagnosable even when the bounded drain is stuck.
	kill -QUIT "$POOL_PID" 2>/dev/null || true
	sleep 1
	tail -300 "$RUN_DIR/pool.log" >&2 || true
    return 1
  fi
  wait "$POOL_PID"
  POOL_PID=""
}

for command in taskset curl openssl python3 awk find du sha256sum; do
  command -v "$command" >/dev/null || { echo "required command missing: $command" >&2; exit 1; }
done
POOL_LAUNCH=(taskset -c "$CPU_SET")
if [[ "$(id -u)" != "$RUN_OWNER_UID" || "$(id -g)" != "$RUN_OWNER_GID" ]]; then
  [[ "$(id -u)" == "0" ]] || { echo "run directory owner $RUN_OWNER_UID:$RUN_OWNER_GID does not match caller and caller cannot change identity" >&2; exit 1; }
  command -v setpriv >/dev/null || { echo "setpriv is required to launch the pool as run directory owner $RUN_OWNER_UID:$RUN_OWNER_GID" >&2; exit 1; }
  POOL_LAUNCH=(setpriv --reuid="$RUN_OWNER_UID" --regid="$RUN_OWNER_GID" --clear-groups taskset -c "$CPU_SET")
fi
[[ -x "$EXTREME_LOAD_BIN" ]] || { echo "extreme-load binary is not executable: $EXTREME_LOAD_BIN" >&2; exit 1; }
[[ -x "$POOL_SERVER_BIN" ]] || { echo "pool-server binary is not executable: $POOL_SERVER_BIN" >&2; exit 1; }
EXTREME_LOAD_SHA="$(sha256sum "$EXTREME_LOAD_BIN" | awk '{print $1}')"
POOL_SERVER_SHA="$(sha256sum "$POOL_SERVER_BIN" | awk '{print $1}')"
[[ -z "${EXPECTED_EXTREME_LOAD_SHA:-}" || "$EXTREME_LOAD_SHA" == "$EXPECTED_EXTREME_LOAD_SHA" ]] || { echo "extreme-load checksum mismatch: $EXTREME_LOAD_SHA" >&2; exit 1; }
[[ -z "${EXPECTED_POOL_SERVER_SHA:-}" || "$POOL_SERVER_SHA" == "$EXPECTED_POOL_SERVER_SHA" ]] || { echo "pool-server checksum mismatch: $POOL_SERVER_SHA" >&2; exit 1; }
echo "acceptance binaries: pool-server=$POOL_SERVER_SHA extreme-load=$EXTREME_LOAD_SHA"
mkdir -p "$RUN_DIR/fixtures" "$RUN_DIR/spool" "$RUN_DIR/journal"

openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
  -keyout "$RUN_DIR/mock.key" -out "$RUN_DIR/mock.crt" \
  -subj "/CN=127.0.0.1" \
  -addext "subjectAltName=IP:127.0.0.1" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign" >/dev/null 2>&1

if [[ -z "${REUSE_FIXTURE_DIR:-}" ]]; then
  TIKTOKEN_CACHE_DIR="$TIKTOKEN_CACHE_DIR" "$EXTREME_LOAD_BIN" generate -dir "$FIXTURE_DIR" -count "$FIXTURE_COUNT" -tokens "$TARGET_TOKENS" -tolerance 0.005 -model gpt-5.6-sol -encoding o200k_base -profile "$FIXTURE_PROFILE" -workers "$FIXTURE_WORKERS"
fi
if [[ "${SKIP_INITIAL_VERIFY:-0}" != "1" ]]; then
  TIKTOKEN_CACHE_DIR="$TIKTOKEN_CACHE_DIR" "$EXTREME_LOAD_BIN" verify -dir "$FIXTURE_DIR" -minimum "$FIXTURE_COUNT" -workers "$FIXTURE_WORKERS"
fi
"$EXTREME_LOAD_BIN" seed -database "$RUN_DIR/pool.sqlite3" -accounts 64

"$EXTREME_LOAD_BIN" mock -listen "127.0.0.1:$MOCK_PORT" -cert "$RUN_DIR/mock.crt" -key "$RUN_DIR/mock.key" -input-tokens "$TARGET_TOKENS" >"$RUN_DIR/mock.log" 2>&1 &
MOCK_PID="$!"
for _ in $(seq 1 100); do
  if curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/healthz" >/dev/null; then break; fi
  sleep 0.1
done
curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/healthz" >/dev/null

cat >"$RUN_DIR/config.json" <<EOF
{
  "listen_addr": "127.0.0.1:$POOL_PORT",
  "data_dir": "$RUN_DIR",
  "database_path": "$RUN_DIR/pool.sqlite3",
  "storage_driver": "sqlite",
  "upstream_base_url": "https://127.0.0.1:$MOCK_PORT/backend-api/codex",
  "openai_api_upstream_base_url": "https://127.0.0.1:$MOCK_PORT/v1",
  "default_group": "cyber",
  "admin_token": "$ADMIN_TOKEN",
  "identity_secret": "extreme-host-acceptance-identity-only",
  "request_timeout_seconds": 600,
  "max_body_bytes": 1073741824,
  "body_v2_enabled": true,
  "body_memory_threshold_bytes": $BODY_MEMORY_THRESHOLD_BYTES,
  "body_memory_budget_bytes": $BODY_MEMORY_BUDGET_BYTES,
  "body_spool_max_bytes": 34359738368,
  "body_disk_reserve_bytes": 0,
  "body_spool_dir": "$RUN_DIR/spool",
  "scheduler_index_enabled": true,
  "usage_journal_enabled": true,
  "usage_journal_dir": "$RUN_DIR/journal",
  "goal_continuity_enabled": false,
  "codex_session_mapping_enabled": true,
  "codex_cpa_strict": true,
  "codex_cache_singleflight_enabled": true,
  "model_probe_interval_hours": 0,
  "resource_headroom_percent": 10
}
EOF

GODEBUG="${POOL_GODEBUG:-}" GOMEMLIMIT="${POOL_GOMEMLIMIT:-off}" CODEX_POOL_HEAP_PROFILE="$RUN_DIR/heap.pprof" CODEX_POOL_CPU_PROFILE="$CPU_PROFILE_PATH" GOMAXPROCS="$POOL_GOMAXPROCS" SSL_CERT_FILE="$RUN_DIR/mock.crt" "${POOL_LAUNCH[@]}" "$POOL_SERVER_BIN" -config "$RUN_DIR/config.json" >"$RUN_DIR/pool.log" 2>&1 &
POOL_PID="$!"
for _ in $(seq 1 200); do
  if curl --silent --fail "http://127.0.0.1:$POOL_PORT/healthz" >/dev/null; then break; fi
  if ! kill -0 "$POOL_PID" 2>/dev/null; then echo "pool exited during startup" >&2; tail -100 "$RUN_DIR/pool.log" >&2; exit 1; fi
  sleep 0.1
done
curl --silent --fail "http://127.0.0.1:$POOL_PORT/healthz" >/dev/null

(
  peak=0
  while kill -0 "$POOL_PID" 2>/dev/null; do
    rss="$(awk '/^VmRSS:/ {print $2}' "/proc/$POOL_PID/status" 2>/dev/null || true)"
    threads="$(awk '/^Threads:/ {print $2}' "/proc/$POOL_PID/status" 2>/dev/null || true)"
    fds="$(find "/proc/$POOL_PID/fd" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l)"
    spool="$(du -sb "$RUN_DIR/spool" 2>/dev/null | awk '{print $1}')"
    rss="${rss:-0}"
    threads="${threads:-0}"
    spool="${spool:-0}"
    if (( rss > peak )); then peak="$rss"; fi
    printf '{"unix_nano":%s,"rss_kib":%s,"peak_rss_kib":%s,"fds":%s,"threads":%s,"spool_bytes":%s}\n' "$(date +%s%N)" "$rss" "$peak" "$fds" "$threads" "$spool" >>"$RUN_DIR/process-stats.jsonl"
    if (( HEAP_PROFILE_KIB > 0 && rss > HEAP_PROFILE_KIB )) && [[ ! -f "$RUN_DIR/heap-profile-requested" ]]; then
      printf '%s\n' "$rss" >"$RUN_DIR/heap-profile-requested"
      kill -USR1 "$POOL_PID" 2>/dev/null || true
    fi
    if (( rss > MEMORY_LIMIT_KIB )); then
      printf '%s\n' "$rss" >"$RUN_DIR/rss-limit-exceeded"
      kill -TERM "$POOL_PID" 2>/dev/null || true
      exit 1
    fi
    sleep 0.1
  done
) &
RESOURCE_PID="$!"

(
  while kill -0 "$POOL_PID" 2>/dev/null; do
    curl --silent --fail -H "Authorization: Bearer $ADMIN_TOKEN" "http://127.0.0.1:$POOL_PORT/admin/system" >>"$RUN_DIR/runtime-stats.jsonl" || true
    printf '\n' >>"$RUN_DIR/runtime-stats.jsonl"
    sleep 1
  done
) &
RUNTIME_PID="$!"

set +e
TIKTOKEN_CACHE_DIR="$TIKTOKEN_CACHE_DIR" "$EXTREME_LOAD_BIN" run -dir "$FIXTURE_DIR" -minimum "$FIXTURE_COUNT" \
  -endpoint "http://127.0.0.1:$POOL_PORT/v1/responses" \
  -rps "$TARGET_RPS" -minimum-achieved-rps "$MINIMUM_ACHIEVED_RPS" -duration "$LOAD_DURATION" -concurrency "$LOAD_CONCURRENCY" -fixture-workers "$FIXTURE_WORKERS" -hard \
  | tee "$RUN_DIR/load-result.json"
load_status="${PIPESTATUS[0]}"
set -e
if (( load_status != 0 )); then
  if [[ -f "$RUN_DIR/rss-limit-exceeded" ]]; then
    echo "RSS threshold exceeded: $(cat "$RUN_DIR/rss-limit-exceeded") KiB (limit $MEMORY_LIMIT_KIB KiB)" >&2
  fi
  python3 - "$RUN_DIR/process-stats.jsonl" "$RUN_DIR/runtime-stats.jsonl" <<'PY' >&2 || true
import json, sys
process_rows = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
runtime_rows = [json.loads(line) for line in open(sys.argv[2]) if line.strip()]
summary = {
    "peak_rss_kib": max((row["rss_kib"] for row in process_rows), default=0),
    "peak_fds": max((row["fds"] for row in process_rows), default=0),
    "peak_threads": max((row["threads"] for row in process_rows), default=0),
    "peak_spool_bytes": max((row["spool_bytes"] for row in process_rows), default=0),
    "runtime_samples": len(runtime_rows),
}
print(json.dumps(summary, separators=(",", ":")))
PY
	# A performance-gate failure must still exercise the exact same bounded drain
	# path as a passing run. This preserves the journal and a goroutine dump when
	# shutdown itself regresses instead of silently masking it in the EXIT trap.
	set +e
	stop_pool_gracefully
	shutdown_status="$?"
	set -e
	tail -200 "$RUN_DIR/pool.log" >&2
	if (( shutdown_status != 0 )); then
		exit "$shutdown_status"
	fi
	exit "$load_status"
fi
if [[ -f "$RUN_DIR/rss-limit-exceeded" ]]; then
  echo "2 GiB RSS threshold exceeded: $(cat "$RUN_DIR/rss-limit-exceeded") KiB" >&2
  exit 1
fi
kill -0 "$POOL_PID" 2>/dev/null || { echo "pool exited during load" >&2; tail -200 "$RUN_DIR/pool.log" >&2; exit 1; }

MOCK_STATS="$(curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/stats")"
printf '%s\n' "$MOCK_STATS" | tee "$RUN_DIR/mock-stats.json"
python3 - "$MOCK_STATS" <<'PY'
import json, sys
stats = json.loads(sys.argv[1])
if stats["requests"] <= 0 or stats["requests"] != stats["http2_requests"]:
    raise SystemExit(f"protocol gate failed: {stats}")
PY

stop_pool_gracefully
python3 - "$RUN_DIR/load-result.json" "$RUN_DIR/pool.sqlite3" <<'PY'
import json, sqlite3, sys

result = json.load(open(sys.argv[1], encoding="utf-8"))
expected = int(result["succeeded"])
db = sqlite3.connect(sys.argv[2])
checks = {
    "bindings": "SELECT COUNT(*) FROM codex_session_binding",
    "aliases": "SELECT COUNT(*) FROM codex_session_alias",
    "snapshots": "SELECT COUNT(*) FROM codex_instruction_snapshot",
    "holds": "SELECT COUNT(*) FROM billing_holds",
    "usage": "SELECT COUNT(*) FROM usage_events",
    "created_audits": "SELECT COUNT(*) FROM audit_log WHERE action='codex_session_binding_created'",
}
counts = {name: int(db.execute(query).fetchone()[0]) for name, query in checks.items()}
held = int(db.execute("SELECT COUNT(*) FROM billing_holds WHERE status='held'").fetchone()[0])
attempts = int(db.execute("SELECT COUNT(*) FROM codex_upstream_attempt").fetchone()[0])
attempt_ids = int(db.execute("SELECT COUNT(DISTINCT event_id) FROM codex_upstream_attempt").fetchone()[0])
commit_failures = int(db.execute("SELECT COUNT(*) FROM audit_log WHERE action='codex_session_mapping_commit_failed'").fetchone()[0])
for name in ("bindings", "snapshots", "holds", "usage", "created_audits"):
    if counts[name] != expected:
        raise SystemExit(f"durability gate failed: {name}={counts[name]} expected={expected}")
if counts["aliases"] < expected or held != 0 or attempts != attempt_ids or commit_failures != 0:
    raise SystemExit(
        f"durability gate failed: aliases={counts['aliases']} held={held} "
        f"attempts={attempts} distinct_attempt_ids={attempt_ids} commit_failures={commit_failures}"
    )
print(json.dumps({**counts, "held": held, "attempts": attempts, "distinct_attempt_ids": attempt_ids}, separators=(",", ":")))
PY

python3 - "$RUN_DIR/process-stats.jsonl" "$RUN_DIR/runtime-stats.jsonl" "$MEMORY_LIMIT_KIB" "$CPU_SET" <<'PY'
import json, sys
process_rows = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
runtime_rows = [json.loads(line) for line in open(sys.argv[2]) if line.strip()]
peak = max((row["rss_kib"] for row in process_rows), default=0)
if peak > int(sys.argv[3]):
    raise SystemExit(f"RSS gate failed: {peak} KiB")
go_peak = max((row.get("go", {}).get("goroutines", 0) for row in runtime_rows), default=0)
summary = {
    "limit_mode": "taskset+RSS-watchdog",
    "cpu_set": sys.argv[4],
    "peak_rss_kib": peak,
    "peak_fds": max((row["fds"] for row in process_rows), default=0),
    "peak_threads": max((row["threads"] for row in process_rows), default=0),
    "peak_spool_bytes": max((row["spool_bytes"] for row in process_rows), default=0),
    "peak_goroutines": go_peak,
}
print(json.dumps(summary, separators=(",", ":")))
PY
echo "host extreme acceptance passed: cpu_set=$CPU_SET, RSS watchdog=$MEMORY_LIMIT_KIB KiB, SQLite in-node, target=$TARGET_RPS RPS, minimum-achieved=$MINIMUM_ACHIEVED_RPS RPS, $TARGET_TOKENS tokens, TLS/HTTP2 upstream"
