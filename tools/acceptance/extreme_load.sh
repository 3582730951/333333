#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.25.12/go/bin/go}"
FIXTURE_COUNT="${FIXTURE_COUNT:-256}"
TARGET_TOKENS="${TARGET_TOKENS:-1000000}"
TARGET_RPS="${TARGET_RPS:-100}"
MINIMUM_ACHIEVED_RPS="${MINIMUM_ACHIEVED_RPS:-$TARGET_RPS}"
LOAD_DURATION="${LOAD_DURATION:-10s}"
LOAD_CONCURRENCY="${LOAD_CONCURRENCY:-256}"
FIXTURE_WORKERS="${FIXTURE_WORKERS:-16}"
REQUIRE_CGROUP="${REQUIRE_CGROUP:-1}"
MOCK_PORT="${MOCK_PORT:-19443}"
POOL_PORT="${POOL_PORT:-18787}"
CONTAINER_NAME="codex-pool-extreme-$$"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/codex-pool-extreme.XXXXXX")"
MOCK_PID=""
STATS_PID=""
POOL_LOG_PID=""

cleanup() {
  if [[ -n "$STATS_PID" ]]; then kill "$STATS_PID" 2>/dev/null || true; fi
  if [[ -n "$POOL_LOG_PID" ]]; then kill "$POOL_LOG_PID" 2>/dev/null || true; wait "$POOL_LOG_PID" 2>/dev/null || true; fi
  docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if [[ -n "$MOCK_PID" ]]; then kill "$MOCK_PID" 2>/dev/null || true; wait "$MOCK_PID" 2>/dev/null || true; fi
  if [[ "${KEEP_ARTIFACTS:-0}" == "1" ]]; then
    echo "acceptance artifacts retained at $RUN_DIR"
  elif [[ "$RUN_DIR" == "${TMPDIR:-/tmp}"/codex-pool-extreme.* ]]; then
    rm -rf -- "$RUN_DIR"
  fi
}
trap cleanup EXIT INT TERM

mkdir -p "$RUN_DIR/fixtures" "$RUN_DIR/spool" "$RUN_DIR/journal"
DOCKER_LIMIT_ARGS=(--cpus 2 --memory 2g --memory-swap 2g)
POOL_COMMAND=(/accept/pool-server -config /accept/config.json)
LIMIT_MODE="cgroup"
if ! docker run --rm "${DOCKER_LIMIT_ARGS[@]}" ubuntu:22.04 true >/dev/null 2>&1; then
  if [[ "$REQUIRE_CGROUP" == "1" ]]; then
    echo "host cgroup hierarchy cannot apply Docker CPU+memory limits; hard acceptance requires a domain cgroup v2 host" >&2
    exit 1
  fi
  DOCKER_LIMIT_ARGS=(--cpuset-cpus 0,1)
  POOL_COMMAND=(/bin/bash -lc 'ulimit -v 2097152; exec /accept/pool-server -config /accept/config.json')
  LIMIT_MODE="cpuset+RLIMIT_AS"
fi

openssl req -x509 -newkey rsa:2048 -sha256 -nodes -days 1 \
  -keyout "$RUN_DIR/mock.key" -out "$RUN_DIR/mock.crt" \
  -subj "/CN=127.0.0.1" \
  -addext "subjectAltName=IP:127.0.0.1" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,digitalSignature,keyEncipherment,keyCertSign" >/dev/null 2>&1

cd "$ROOT_DIR"
GOTOOLCHAIN=local "$GO_BIN" build -o "$RUN_DIR/extreme-load" ./cmd/extreme-load
GOTOOLCHAIN=local "$GO_BIN" build -o "$RUN_DIR/pool-server" ./cmd/pool-server

"$RUN_DIR/extreme-load" generate -dir "$RUN_DIR/fixtures" -count "$FIXTURE_COUNT" -tokens "$TARGET_TOKENS" -tolerance 0.005 -model gpt-5.6-sol -encoding o200k_base -workers "$FIXTURE_WORKERS"
"$RUN_DIR/extreme-load" verify -dir "$RUN_DIR/fixtures" -minimum "$FIXTURE_COUNT" -workers "$FIXTURE_WORKERS"
"$RUN_DIR/extreme-load" seed -database "$RUN_DIR/pool.sqlite3" -accounts 64

"$RUN_DIR/extreme-load" mock -listen "127.0.0.1:$MOCK_PORT" -cert "$RUN_DIR/mock.crt" -key "$RUN_DIR/mock.key" -input-tokens "$TARGET_TOKENS" >"$RUN_DIR/mock.log" 2>&1 &
MOCK_PID="$!"
for _ in $(seq 1 100); do
  if curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/healthz" >/dev/null; then break; fi
  sleep 0.1
done
curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/healthz" >/dev/null

cat >"$RUN_DIR/config.json" <<EOF
{
  "listen_addr": "0.0.0.0:$POOL_PORT",
  "database_path": "/accept/pool.sqlite3",
  "storage_driver": "sqlite",
  "upstream_base_url": "https://127.0.0.1:$MOCK_PORT/backend-api/codex",
  "openai_api_upstream_base_url": "https://127.0.0.1:$MOCK_PORT/v1",
  "default_group": "cyber",
  "admin_token": "extreme-acceptance-admin-token-20260727",
  "identity_secret": "extreme-acceptance-identity-only",
  "request_timeout_seconds": 600,
  "max_body_bytes": 1073741824,
  "body_v2_enabled": true,
  "body_memory_threshold_bytes": 8388608,
  "body_memory_budget_bytes": 268435456,
  "body_spool_max_bytes": 34359738368,
  "body_disk_reserve_bytes": 0,
  "body_spool_dir": "/accept/spool",
  "scheduler_index_enabled": true,
  "usage_journal_enabled": true,
  "usage_journal_dir": "/accept/journal",
  "goal_continuity_enabled": false,
  "codex_session_mapping_enabled": true,
  "codex_cpa_strict": true,
  "codex_cache_singleflight_enabled": true,
  "resource_headroom_percent": 10
}
EOF

docker run --detach --rm --name "$CONTAINER_NAME" \
  "${DOCKER_LIMIT_ARGS[@]}" --pids-limit 4096 --network host \
  -e SSL_CERT_FILE=/accept/mock.crt \
  -v "$RUN_DIR:/accept" ubuntu:22.04 \
  "${POOL_COMMAND[@]}" >"$RUN_DIR/container.id"
docker logs --follow "$CONTAINER_NAME" >"$RUN_DIR/pool.log" 2>&1 &
POOL_LOG_PID="$!"

for _ in $(seq 1 200); do
  if curl --silent --fail "http://127.0.0.1:$POOL_PORT/healthz" >/dev/null; then break; fi
  sleep 0.1
done
curl --silent --fail "http://127.0.0.1:$POOL_PORT/healthz" >/dev/null

(
  while docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; do
    docker stats --no-stream --format '{{json .}}' "$CONTAINER_NAME" || true
    sleep 1
  done
) >"$RUN_DIR/docker-stats.jsonl" &
STATS_PID="$!"

"$RUN_DIR/extreme-load" run -dir "$RUN_DIR/fixtures" -minimum "$FIXTURE_COUNT" \
  -endpoint "http://127.0.0.1:$POOL_PORT/v1/responses" \
  -rps "$TARGET_RPS" -minimum-achieved-rps "$MINIMUM_ACHIEVED_RPS" -duration "$LOAD_DURATION" -concurrency "$LOAD_CONCURRENCY" -fixture-workers "$FIXTURE_WORKERS" -hard \
  | tee "$RUN_DIR/load-result.json"

MOCK_STATS="$(curl --silent --fail --cacert "$RUN_DIR/mock.crt" "https://127.0.0.1:$MOCK_PORT/stats")"
printf '%s\n' "$MOCK_STATS" | tee "$RUN_DIR/mock-stats.json"
python3 - "$MOCK_STATS" <<'PY'
import json, sys
stats = json.loads(sys.argv[1])
if stats["requests"] <= 0 or stats["requests"] != stats["http2_requests"]:
    raise SystemExit(f"protocol gate failed: {stats}")
PY

if [[ "$(docker inspect --format '{{.State.OOMKilled}}' "$CONTAINER_NAME")" != "false" ]]; then
  echo "pool container was OOM-killed" >&2
  exit 1
fi
echo "extreme acceptance passed: limit_mode=$LIMIT_MODE, SQLite in-node, target=$TARGET_RPS RPS, minimum-achieved=$MINIMUM_ACHIEVED_RPS RPS, $TARGET_TOKENS tokens, TLS/HTTP2 upstream"
