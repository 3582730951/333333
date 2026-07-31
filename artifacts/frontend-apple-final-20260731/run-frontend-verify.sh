#!/usr/bin/env bash
set -uo pipefail

ROOT=/root/autodl-tmp/frontend-ui-shot-20260731
SOURCE="$ROOT/optimized-src"
ARTIFACT="$ROOT/artifacts/frontend-apple-final"
NODE_BIN=/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin
LOG="$ARTIFACT/frontend-verify.literal.log"
STATUS_FILE="$ARTIFACT/frontend-verify.exit-status"

rtk env "PATH=$NODE_BIN:$PATH" npm --prefix "$SOURCE/web-spa" run verify >"$LOG" 2>&1
verify_status=$?
rtk proxy sh -c "printf '%s\n' '$verify_status' > '$STATUS_FILE'"
rtk tail -n 40 "$LOG"
rtk echo "FRONTEND_VERIFY_EXIT=$verify_status"
exit "$verify_status"
