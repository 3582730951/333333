#!/usr/bin/env bash
set -euo pipefail

ROOT=/root/autodl-tmp/frontend-ui-shot-20260731
NODE=/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin/node
NODE_MODULES="$ROOT/browser-runner/node_modules"
SCRIPT="$ROOT/browser-runner/capture-matrix-apple-final.mjs"
LOG="$ROOT/runtime/matrix-apple-final.log"
PID_FILE="$ROOT/runtime/matrix-apple-final.pid"

rtk rm -rf "$ROOT/screenshots/matrix-apple-final"
rtk rm -f "$ROOT/artifacts/ui-matrix-apple-final-report.json" "$ROOT/artifacts/ui-matrix-apple-final-report.md" "$LOG"
rtk mkdir -p "$ROOT/screenshots/matrix-apple-final" "$ROOT/artifacts"
rtk proxy nohup /usr/bin/env \
  "PATH=/root/miniconda3/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  "NODE_PATH=$NODE_MODULES" \
  "$NODE" "$SCRIPT" >"$LOG" 2>&1 &
matrix_pid=$!
rtk proxy sh -c "printf '%s\n' '$matrix_pid' > '$PID_FILE'"
rtk echo "matrix_pid=$matrix_pid"
