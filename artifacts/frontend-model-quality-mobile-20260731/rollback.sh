#!/usr/bin/env bash
set -euo pipefail

root=/root/autodl-tmp/frontend-ui-shot-20260731
source_root="$root/optimized-src"
patch="$root/artifacts/model-quality-mobile-change.patch"
runtime="$root/runtime"
baseline_binary=/root/autodl-tmp/cpupg-20260730/build-new/codex-pool-server

rtk proxy git -C "$source_root" apply --reverse --check "$patch"
rtk proxy git -C "$source_root" apply --reverse "$patch"

current_pid="$(rtk proxy cat "$runtime/server.pid")"
rtk proxy kill -TERM "$current_pid"
rtk proxy sleep 1
rtk proxy nohup "$baseline_binary" \
  --config "$runtime/config.json" \
  --release-id ui-demo-shot-rollback \
  > "$runtime/server-rollback.log" 2>&1 &
rollback_pid=$!
rtk proxy printf '%s\n' "$rollback_pid" > "$runtime/server.pid"
rtk proxy sleep 2
rtk curl -fsS http://127.0.0.1:34274/healthz > "$root/artifacts/rollback-health.json"
rtk proxy grep -q '"ok":true' "$root/artifacts/rollback-health.json"
rtk sha256sum \
  "$source_root/web-spa/src/pages/ModelQuality.jsx" \
  "$source_root/web-spa/tests/contracts.test.ts" \
  > "$root/artifacts/rollback-source-sha256.txt"
rtk proxy printf 'ROLLBACK_OK pid=%s\n' "$rollback_pid"
