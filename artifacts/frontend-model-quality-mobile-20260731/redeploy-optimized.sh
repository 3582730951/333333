#!/usr/bin/env bash
set -euo pipefail

root=/root/autodl-tmp/frontend-ui-shot-20260731
source_root="$root/optimized-src"
patch="$root/artifacts/model-quality-mobile-change.patch"
runtime="$root/runtime"
optimized_binary="$root/optimized/codex-pool-server"

rtk proxy git -C "$source_root" apply --check "$patch"
rtk proxy git -C "$source_root" apply "$patch"

current_pid="$(rtk proxy cat "$runtime/server.pid")"
rtk proxy kill -TERM "$current_pid"
rtk proxy sleep 1
rtk proxy nohup "$optimized_binary" \
  --config "$runtime/config.json" \
  --release-id ui-demo-mobile-v2 \
  > "$runtime/server-optimized.log" 2>&1 &
optimized_pid=$!
rtk proxy printf '%s\n' "$optimized_pid" > "$runtime/server.pid"
rtk proxy sleep 2
rtk curl -fsS http://127.0.0.1:34274/healthz > "$root/artifacts/redeploy-health.json"
rtk proxy grep -q '"ok":true' "$root/artifacts/redeploy-health.json"
rtk sha256sum \
  "$source_root/web-spa/src/pages/ModelQuality.jsx" \
  "$source_root/web-spa/tests/contracts.test.ts" \
  "$source_root/web-spa/tests/model-quality-mobile.test.tsx" \
  > "$root/artifacts/redeploy-source-sha256.txt"
rtk proxy printf 'REDEPLOY_OK pid=%s\n' "$optimized_pid"
