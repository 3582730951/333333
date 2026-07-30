#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=${ROOT:-/root/autodl-tmp/cpupg-20260730}
ACTION=${1:-old}
source "$ROOT/install-env.sh"
APP_DIR="$ROOT/prefix/lib/codex-pool"

case "$ACTION" in
  old)
    RELEASE_ID=old-0873de57
    TARGET="$APP_DIR/releases/$RELEASE_ID"
    LOG="$ROOT/logs/rollback-old-server.log"
    ;;
  new)
    RELEASE_ID=new-goal-context-fix
    TARGET="$APP_DIR/releases/$RELEASE_ID"
    LOG="$ROOT/logs/rollback-new-server.log"
    ;;
  *)
    echo "usage: $0 old|new" >&2
    exit 64
    ;;
esac
test -x "$TARGET/codex-pool-server"

for pid_file in "$ROOT/new.pid" "$ROOT/old.pid" "$ROOT/active.pid"; do
  test -f "$pid_file" || continue
  pid="$(cat "$pid_file")"
  if kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in $(seq 1 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -0 "$pid" 2>/dev/null && {
      echo "worker did not stop: $pid" >&2
      exit 1
    }
  fi
done

next="$APP_DIR/.current.next.$$"
ln -s "$TARGET" "$next"
mv -Tf "$next" "$APP_DIR/current"

nohup env \
  CODEX_POOL_DATABASE="$ROOT/state/pool.sqlite3" \
  CODEX_POOL_MIGRATE_USER_GROUPS=0 \
  CODEX_POOL_LISTEN_ADDR="127.0.0.1:$PORT" \
  "$TARGET/codex-pool-server" \
  --config "$ROOT/etc/config.json" \
  --release-id "$RELEASE_ID" \
  >"$LOG" 2>&1 </dev/null &
pid=$!
printf '%s\n' "$pid" >"$ROOT/active.pid"
if test "$ACTION" = new; then
  printf '%s\n' "$pid" >"$ROOT/new.pid"
else
  printf '%s\n' "$pid" >"$ROOT/old.pid"
fi

for _ in $(seq 1 160); do
  if payload="$(curl -fsS "http://127.0.0.1:$PORT/readyz" 2>/dev/null)"; then
    python3 - "$payload" "$RELEASE_ID" <<'PY'
import json,sys
d=json.loads(sys.argv[1])
assert d["ok"] is True and d["ready"] is True,d
assert d["release_id"]==sys.argv[2],(d,sys.argv[2])
print(json.dumps(d,sort_keys=True))
PY
    printf 'active_release=%s pid=%s current=%s\n' \
      "$RELEASE_ID" "$pid" "$(readlink "$APP_DIR/current")"
    exit 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    cat "$LOG" >&2
    exit 1
  fi
  sleep 0.25
done
cat "$LOG" >&2
exit 1
