#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GO_BIN="${GO_BIN:-/tmp/codex-go1.24.1/go/bin/go}"
PG_PRIMARY_PORT="${PG_PRIMARY_PORT:-55442}"
PG_STANDBY_PORT="${PG_STANDBY_PORT:-55443}"
REDIS_PORT="${REDIS_PORT:-56389}"
RUN_ID="codex-pool-ha-$$"
NETWORK="$RUN_ID"
PG_PRIMARY="$RUN_ID-pg-primary"
PG_STANDBY="$RUN_ID-pg-standby"
REDIS="$RUN_ID-redis"
PG_PRIMARY_VOLUME="$RUN_ID-pg-primary-data"
PG_STANDBY_VOLUME="$RUN_ID-pg-standby-data"
REDIS_VOLUME="$RUN_ID-redis-data"
PG_USER="codex_test"
PG_PASSWORD="codex_test_password"
PG_DATABASE="codex_test"

cleanup() {
  if [[ "${KEEP_CLUSTER:-0}" == "1" ]]; then
    echo "cluster retained: primary=$PG_PRIMARY standby=$PG_STANDBY redis=$REDIS network=$NETWORK"
    return
  fi
  docker rm -f "$PG_PRIMARY" "$PG_STANDBY" "$REDIS" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker volume rm "$PG_PRIMARY_VOLUME" "$PG_STANDBY_VOLUME" "$REDIS_VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

wait_postgres() {
  local container="$1"
  for _ in $(seq 1 200); do
    if docker exec "$container" psql -U "$PG_USER" -d "$PG_DATABASE" -tAc "SELECT 1" 2>/dev/null | grep -qx 1; then return; fi
    sleep 0.1
  done
  docker logs "$container" >&2 || true
  return 1
}

wait_redis() {
  for _ in $(seq 1 200); do
    if docker exec "$REDIS" redis-cli ping 2>/dev/null | grep -q PONG; then return; fi
    sleep 0.1
  done
  docker logs "$REDIS" >&2 || true
  return 1
}

for command in docker sha256sum; do
  command -v "$command" >/dev/null || { echo "required command missing: $command" >&2; exit 1; }
done
[[ -x "$GO_BIN" ]] || { echo "Go binary is not executable: $GO_BIN" >&2; exit 1; }

docker network create "$NETWORK" >/dev/null
docker volume create "$PG_PRIMARY_VOLUME" >/dev/null
docker volume create "$PG_STANDBY_VOLUME" >/dev/null
docker volume create "$REDIS_VOLUME" >/dev/null

docker run --detach --name "$PG_PRIMARY" --network "$NETWORK" --network-alias pg-primary \
  -p "127.0.0.1:$PG_PRIMARY_PORT:5432" -v "$PG_PRIMARY_VOLUME:/var/lib/postgresql/data" \
  -e "POSTGRES_USER=$PG_USER" -e "POSTGRES_PASSWORD=$PG_PASSWORD" -e "POSTGRES_DB=$PG_DATABASE" \
  postgres:16-alpine -c wal_level=replica -c hot_standby=on -c max_wal_senders=10 -c max_replication_slots=10 >/dev/null
wait_postgres "$PG_PRIMARY"
docker exec "$PG_PRIMARY" psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DATABASE" \
  -c "CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD '$PG_PASSWORD'" >/dev/null
docker exec "$PG_PRIMARY" sh -ec \
  "printf '%s\n' 'host replication replicator 0.0.0.0/0 scram-sha-256' >> /var/lib/postgresql/data/pg_hba.conf"
docker exec "$PG_PRIMARY" psql -v ON_ERROR_STOP=1 -U "$PG_USER" -d "$PG_DATABASE" -c "SELECT pg_reload_conf()" >/dev/null

docker run --rm --user postgres --entrypoint sh --network "$NETWORK" \
  -e "PGPASSWORD=$PG_PASSWORD" -v "$PG_STANDBY_VOLUME:/var/lib/postgresql/data" postgres:16-alpine -ec \
  'rm -rf /var/lib/postgresql/data/*; pg_basebackup -h pg-primary -D /var/lib/postgresql/data -U replicator -Fp -Xs -P -R; chmod 700 /var/lib/postgresql/data' >/dev/null
docker run --detach --name "$PG_STANDBY" --network "$NETWORK" --network-alias pg-standby \
  -p "127.0.0.1:$PG_STANDBY_PORT:5432" -v "$PG_STANDBY_VOLUME:/var/lib/postgresql/data" postgres:16-alpine >/dev/null
wait_postgres "$PG_STANDBY"
if [[ "$(docker exec "$PG_STANDBY" psql -U "$PG_USER" -d "$PG_DATABASE" -tAc 'SELECT pg_is_in_recovery()')" != "t" ]]; then
  echo "standby did not enter recovery mode" >&2
  exit 1
fi

docker run --detach --name "$REDIS" --network "$NETWORK" \
  -p "127.0.0.1:$REDIS_PORT:6379" -v "$REDIS_VOLUME:/data" redis:7-alpine \
  redis-server --appendonly yes --appendfsync always >/dev/null
wait_redis

PRIMARY_DSN="postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$PG_PRIMARY_PORT/$PG_DATABASE?sslmode=disable"
STANDBY_DSN="postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$PG_STANDBY_PORT/$PG_DATABASE?sslmode=disable"
FAILOVER_DSN="postgres://$PG_USER:$PG_PASSWORD@127.0.0.1:$PG_PRIMARY_PORT,127.0.0.1:$PG_STANDBY_PORT/$PG_DATABASE?sslmode=disable&target_session_attrs=read-write&connect_timeout=2"
REDIS_URL="redis://127.0.0.1:$REDIS_PORT/0"

cd "$ROOT_DIR"
GOTOOLCHAIN=local TEST_POSTGRES_DSN="$PRIMARY_DSN" "$GO_BIN" test ./internal/storage -run TestSQLitePostgresMigrationIntegration -count=1
GOTOOLCHAIN=local TEST_POSTGRES_DSN="$PRIMARY_DSN" TEST_POSTGRES_RESTART_CONTAINER="$PG_PRIMARY" "$GO_BIN" test ./internal/storage \
  -run 'TestPostgresIntegration(InitAndCoreRoundTrip|ActiveActiveUsageIdempotency|RecoversAfterDurableRestart)$' -count=1
GOTOOLCHAIN=local TEST_REDIS_URL="$REDIS_URL" TEST_REDIS_RESTART_CONTAINER="$REDIS" "$GO_BIN" test ./internal/scheduler \
  -run 'TestRedisLease(CoordinatorAtomicAcrossNodes|CoordinatorFailsClosedAfterEpochLoss|CoordinatorSurvivesDurableRestart|ExpiresAfterNodeExit|RenewalKeepsLiveNodeFenced)$' -count=1
GOTOOLCHAIN=local TEST_POSTGRES_FAILOVER_DSN="$FAILOVER_DSN" TEST_POSTGRES_STANDBY_DSN="$STANDBY_DSN" \
  TEST_POSTGRES_PRIMARY_CONTAINER="$PG_PRIMARY" TEST_POSTGRES_STANDBY_CONTAINER="$PG_STANDBY" \
  "$GO_BIN" test ./internal/storage -run TestPostgresIntegrationPromotesPhysicalStandby -count=1

echo "cluster failover acceptance passed: PostgreSQL physical promotion, Active-Active usage idempotency, Redis AOF restart, fencing, renewal, node exit"
