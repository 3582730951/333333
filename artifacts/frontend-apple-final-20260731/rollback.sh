#!/usr/bin/env bash
set -euo pipefail

root=/root/autodl-tmp/frontend-ui-shot-20260731
source_root="$root/optimized-src"
artifact="$root/artifacts/frontend-apple-final"
runtime="$root/runtime"
baseline_binary=/root/autodl-tmp/cpupg-20260730/build-new/codex-pool-server

server_pid="$(rtk proxy cat "$runtime/server.pid" 2>/dev/null || true)"
if [[ "$server_pid" =~ ^[0-9]+$ ]]; then
  rtk proxy pkill -TERM -P "$server_pid" 2>/dev/null || true
  rtk proxy kill -TERM "$server_pid" 2>/dev/null || true
fi
rtk proxy sleep 1

rtk proxy cp "$artifact/originals/web-spa/scripts/check-visual-smoke.mjs" "$source_root/web-spa/scripts/check-visual-smoke.mjs"
rtk proxy cp "$artifact/originals/web-spa/src/components/Charts.jsx" "$source_root/web-spa/src/components/Charts.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/components/DisplayPrimitives.jsx" "$source_root/web-spa/src/components/DisplayPrimitives.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/components/ResourceTable.jsx" "$source_root/web-spa/src/components/ResourceTable.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/components/SettingsTabShell.jsx" "$source_root/web-spa/src/components/SettingsTabShell.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/lib/i18n.js" "$source_root/web-spa/src/lib/i18n.js"
rtk proxy cp "$artifact/originals/web-spa/src/pages/Accounts.jsx" "$source_root/web-spa/src/pages/Accounts.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/Dashboard.tsx" "$source_root/web-spa/src/pages/Dashboard.tsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/EmailPool.tsx" "$source_root/web-spa/src/pages/EmailPool.tsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/ModelQuality.jsx" "$source_root/web-spa/src/pages/ModelQuality.jsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/Registration.tsx" "$source_root/web-spa/src/pages/Registration.tsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/SettingsV2.tsx" "$source_root/web-spa/src/pages/SettingsV2.tsx"
rtk proxy cp "$artifact/originals/web-spa/src/pages/System.tsx" "$source_root/web-spa/src/pages/System.tsx"
rtk proxy cp "$artifact/originals/web-spa/src/styles/components.css" "$source_root/web-spa/src/styles/components.css"
rtk proxy cp "$artifact/originals/web-spa/src/styles/layout.css" "$source_root/web-spa/src/styles/layout.css"
rtk proxy cp "$artifact/originals/web-spa/tests/contracts.test.ts" "$source_root/web-spa/tests/contracts.test.ts"
rtk proxy rm -f \
  "$source_root/web-spa/tests/accounts-dashboard-presentation.test.ts" \
  "$source_root/web-spa/tests/compact-operational-records.test.tsx" \
  "$source_root/web-spa/tests/email-pool-responsive.test.tsx" \
  "$source_root/web-spa/tests/model-quality-mobile.test.tsx" \
  "$source_root/web-spa/tests/settings-information-architecture.test.tsx"
rtk proxy rm -f "$runtime/pool.sqlite3-wal" "$runtime/pool.sqlite3-shm"
rtk proxy cp "$artifact/pool.before-edge.sqlite3" "$runtime/pool.sqlite3"

rtk proxy nohup "$baseline_binary" \
  --config "$runtime/config.json" \
  --release-id frontend-apple-rollback \
  > "$runtime/server-frontend-apple-rollback.log" 2>&1 &
rollback_pid=$!
rtk proxy printf '%s\n' "$rollback_pid" > "$runtime/server.pid"
rtk proxy sleep 2
rtk curl -fsS http://127.0.0.1:34274/healthz > "$artifact/rollback-health.json"
rtk proxy grep -q '"ok":true' "$artifact/rollback-health.json"
rtk proxy cp "$artifact/verify-frontend-mode.mjs" "$root/browser-runner/verify-frontend-mode.mjs"
rtk /root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin/node \
  "$root/browser-runner/verify-frontend-mode.mjs" baseline \
  > "$artifact/rollback-behavior.literal.log"
rtk proxy printf 'ROLLBACK_OK pid=%s\n' "$rollback_pid"
