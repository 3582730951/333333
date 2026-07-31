#!/usr/bin/env bash
set -euo pipefail

root=/root/autodl-tmp/traffic-fallback-20260731
source_dir="$root/src"
binary="$root/bin/codex-pool-server"
candidate="$root/bin/codex-pool-server.next"
go=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go
record="$root/records"
runtime="$root/runtime"
release=traffic-fallback-review-20260731

cd "$source_dir"
"$go" build -trimpath -o "$candidate" ./cmd/pool-server
"$candidate" --self-test | tee "$record/server-self-test-zindex.log"
sha256sum "$candidate" | tee "$record/server-zindex.sha256"
chmod 0755 "$candidate"
mv -f "$candidate" "$binary"

old_pid="$(cat "$runtime/server.pid")"
kill "$old_pid"
for _ in $(seq 1 100); do
  if ! kill -0 "$old_pid" 2>/dev/null; then
    break
  fi
  sleep 0.05
done
if kill -0 "$old_pid" 2>/dev/null; then
  echo "old server did not stop: $old_pid" >&2
  exit 1
fi

nohup "$binary" \
  --config "$runtime/config.json" \
  --release-id "$release" \
  >"$runtime/server.log" 2>&1 &
new_pid=$!
printf '%s\n' "$new_pid" >"$runtime/server.pid"

ready=
for _ in $(seq 1 120); do
  if ready="$(curl -fsS http://127.0.0.1:34318/readyz 2>/dev/null)"; then
    break
  fi
  sleep 0.1
done
if [[ -z "$ready" ]]; then
  cat "$runtime/server.log" >&2
  exit 1
fi
printf '%s\n' "$ready" | tee "$record/ready-zindex.json"
printf 'REBUILD_RESTART_OK=1\nOLD_PID=%s\nNEW_PID=%s\n' "$old_pid" "$new_pid" |
  tee "$record/rebuild-restart-zindex.status"
