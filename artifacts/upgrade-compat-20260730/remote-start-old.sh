#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"

if test -f "$ROOT/old.pid" && kill -0 "$(cat "$ROOT/old.pid")" 2>/dev/null; then
  kill "$(cat "$ROOT/old.pid")"
  for _ in $(seq 1 50); do
    kill -0 "$(cat "$ROOT/old.pid")" 2>/dev/null || break
    sleep 0.1
  done
fi

nohup env \
  CODEX_POOL_DATABASE="$ROOT/state/pool.sqlite3" \
  CODEX_POOL_MIGRATE_USER_GROUPS=0 \
  CODEX_POOL_LISTEN_ADDR="127.0.0.1:$PORT" \
  "$ROOT/prefix/bin/codex-pool-server" \
  --config "$ROOT/etc/config.json" \
  >"$ROOT/logs/old-server.log" 2>&1 </dev/null &
pid=$!
printf '%s\n' "$pid" >"$ROOT/old.pid"

for _ in $(seq 1 120); do
  if curl -fsS "http://127.0.0.1:$PORT/readyz" >"$ROOT/logs/old-ready.json"; then
    printf 'old_server_pid=%s\n' "$pid"
    cat "$ROOT/logs/old-ready.json"
    exit 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$ROOT/logs/old-server.log" >&2
    exit 1
  fi
  sleep 0.25
done

cat "$ROOT/logs/old-server.log" >&2
exit 1
