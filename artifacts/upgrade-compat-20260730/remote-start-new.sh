#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"

if test -f "$ROOT/new.pid" && kill -0 "$(cat "$ROOT/new.pid")" 2>/dev/null; then
  kill "$(cat "$ROOT/new.pid")"
  for _ in $(seq 1 50); do
    kill -0 "$(cat "$ROOT/new.pid")" 2>/dev/null || break
    sleep 0.1
  done
fi

nohup env \
  CODEX_POOL_DATABASE="$ROOT/state/pool.sqlite3" \
  CODEX_POOL_MIGRATE_USER_GROUPS=0 \
  CODEX_POOL_LISTEN_ADDR="127.0.0.1:$PORT" \
  "$ROOT/prefix/bin/codex-pool-server" \
  --config "$ROOT/etc/config.json" \
  --release-id new-goal-context-fix \
  >"$ROOT/logs/new-server.log" 2>&1 </dev/null &
pid=$!
printf '%s\n' "$pid" >"$ROOT/new.pid"

for _ in $(seq 1 160); do
  if curl -fsS "http://127.0.0.1:$PORT/readyz" >"$ROOT/logs/new-ready.json"; then
    python3 - "$ROOT/logs/new-ready.json" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
assert d["ok"] is True and d["ready"] is True, d
assert d["release_id"] == "new-goal-context-fix", d
print("new_server_ready",json.dumps(d,sort_keys=True))
PY
    printf 'new_server_pid=%s\n' "$pid"
    exit 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$ROOT/logs/new-server.log" >&2
    exit 1
  fi
  sleep 0.25
done

cat "$ROOT/logs/new-server.log" >&2
exit 1
