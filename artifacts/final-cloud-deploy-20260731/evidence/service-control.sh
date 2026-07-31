#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/final-apple-email-deploy-20260731
SOURCE=/root/autodl-tmp/legacy-install-upgrade-20260731/prefix/lib/codex-pool/releases/apple-backend-optimized-v2-20260731/codex-pool-server
BACKUP="$ROOT/rollback"
LOGS="$ROOT/logs"
RECORDS="$ROOT/records"

MAIN_CFG=/root/autodl-tmp/cpupg-20260730/etc/config.json
FRONT_CFG=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json
MAIN_DB=/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3
FRONT_DB=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/pool.sqlite3

MAIN_PREVIOUS=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/backend-headroom-apple-final/codex-pool-server
FRONT_PREVIOUS=/root/autodl-tmp/frontend-ui-shot-20260731/releases/backend-headroom-apple-final/codex-pool-server
MAIN_FINAL=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/apple-email-overflow-final/codex-pool-server
FRONT_FINAL=/root/autodl-tmp/frontend-ui-shot-20260731/releases/apple-email-overflow-final/codex-pool-server

MAIN_PREVIOUS_RELEASE=backend-headroom-final-main
FRONT_PREVIOUS_RELEASE=backend-headroom-final-frontend
MAIN_FINAL_RELEASE=apple-email-overflow-final-main
FRONT_FINAL_RELEASE=apple-email-overflow-final-frontend
FINAL_BINARY_SHA=429f98e8fb44b62b2fe4f71ca8745923dd6feac83629964e2ec82876e8cd9046
FINAL_CONSOLE_SHA=c56c968e9703f066dca5e601fb836c1665ff332843512d98ea769a308ed0037f

find_pids() {
  python3 - "$1" <<'PY'
import os
import sys

config = sys.argv[1]
for entry in os.scandir("/proc"):
    if not entry.name.isdigit():
        continue
    try:
        args = open(f"/proc/{entry.name}/cmdline", "rb").read().split(b"\0")
    except (FileNotFoundError, PermissionError):
        continue
    text = [arg.decode(errors="replace") for arg in args if arg]
    if config in text and "--config" in text and "--release-id" in text:
        print(entry.name)
PY
}

stop_service() {
  local config="$1"
  local pids
  pids="$(find_pids "$config")"
  [[ -z "$pids" ]] && return 0
  kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 120); do
    [[ -z "$(find_pids "$config")" ]] && return 0
    sleep 0.25
  done
  pids="$(find_pids "$config")"
  [[ -z "$pids" ]] || kill -KILL $pids 2>/dev/null || true
}

wait_ready() {
  python3 - "$1" "$2" <<'PY'
import json
import sys
import time
import urllib.request

url, expected = sys.argv[1:3]
last = ""
for _ in range(160):
    try:
        with urllib.request.urlopen(url, timeout=1) as response:
            data = json.load(response)
            last = json.dumps(data, sort_keys=True)
            if response.status == 200 and data.get("ready") and data.get("release_id") == expected:
                print(last)
                raise SystemExit(0)
    except Exception as error:
        last = type(error).__name__
    time.sleep(0.25)
print(last)
raise SystemExit(1)
PY
}

start_service() {
  local binary="$1" config="$2" release="$3" port="$4" name="$5"
  nohup "$binary" --config "$config" --release-id "$release" \
    >"$LOGS/${name}.literal.log" 2>&1 </dev/null &
  printf '%s\n' "$!" >"$RECORDS/${name}.pid"
  wait_ready "http://127.0.0.1:${port}/readyz" "$release" \
    >"$RECORDS/${name}.ready.json"
  printf 'STARTED=%s PID=%s RELEASE=%s\n' "$name" "$(cat "$RECORDS/${name}.pid")" "$release"
}

backup_db() {
  python3 - "$1" "$2" <<'PY'
import os
import sqlite3
import sys

source, destination = sys.argv[1:3]
os.makedirs(os.path.dirname(destination), exist_ok=True)
if os.path.exists(destination):
    raise SystemExit(0)
src = sqlite3.connect(source)
dst = sqlite3.connect(destination)
with dst:
    src.backup(dst)
dst.close()
src.close()
os.chmod(destination, 0o600)
PY
}

restore_db() {
  python3 - "$1" "$2" <<'PY'
import os
import shutil
import sys

source, destination = sys.argv[1:3]
temp = destination + ".final-deploy-restore"
shutil.copy2(source, temp)
os.replace(temp, destination)
for suffix in ("-wal", "-shm"):
    try:
        os.remove(destination + suffix)
    except FileNotFoundError:
        pass
PY
}

data_summary() {
  python3 - "$1" "$2" <<'PY'
import hashlib
import json
import sqlite3
import sys

database, output = sys.argv[1:3]
conn = sqlite3.connect(f"file:{database}?mode=ro", uri=True)
tables = {row[0] for row in conn.execute(
    "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
)}
tracked = [
    "accounts", "groups", "email_pool", "usage_records",
    "registration_jobs", "lifecycle_tasks",
]
counts = {
    table: conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
    for table in tracked if table in tables
}
identity = {}
for table in ("accounts", "groups", "email_pool"):
    if table not in tables:
        continue
    key = "name" if table == "groups" else "id"
    rows = [row[0] for row in conn.execute(f'SELECT "{key}" FROM "{table}" ORDER BY "{key}"')]
    identity[table] = hashlib.sha256(
        json.dumps(rows, separators=(",", ":")).encode()
    ).hexdigest()
result = {
    "quick_check": conn.execute("PRAGMA quick_check").fetchone()[0],
    "integrity_check": conn.execute("PRAGMA integrity_check").fetchone()[0],
    "counts": counts,
    "identity_fingerprints": identity,
}
conn.close()
open(output, "w").write(json.dumps(result, indent=2) + "\n")
print(json.dumps(result, separators=(",", ":")))
PY
}

assert_data_equal() {
  python3 - "$1" "$2" <<'PY'
import json
import sys

before, after = [json.load(open(path)) for path in sys.argv[1:3]]
assert before["quick_check"] == after["quick_check"] == "ok"
assert before["integrity_check"] == after["integrity_check"] == "ok"
assert before["counts"] == after["counts"]
assert before["identity_fingerprints"] == after["identity_fingerprints"]
print("DATA_ASSERTED=1")
PY
}

console_sha() {
  curl --noproxy '*' -fsS "http://127.0.0.1:$1/console/" | sha256sum | awk '{print $1}'
}

config_sha() {
  sha256sum "$1" | awk '{print $1}'
}

prepare() {
  mkdir -p "$BACKUP" "$LOGS" "$RECORDS" "$(dirname "$MAIN_FINAL")" "$(dirname "$FRONT_FINAL")"
  chmod 0700 "$BACKUP"
  [[ "$(sha256sum "$SOURCE" | awk '{print $1}')" == "$FINAL_BINARY_SHA" ]]
  [[ -f "$BACKUP/main-previous.bin" ]] || cp -p "$MAIN_PREVIOUS" "$BACKUP/main-previous.bin"
  [[ -f "$BACKUP/frontend-previous.bin" ]] || cp -p "$FRONT_PREVIOUS" "$BACKUP/frontend-previous.bin"
  [[ -f "$BACKUP/main-config.json" ]] || cp -p "$MAIN_CFG" "$BACKUP/main-config.json"
  [[ -f "$BACKUP/frontend-config.json" ]] || cp -p "$FRONT_CFG" "$BACKUP/frontend-config.json"
  chmod 0600 "$BACKUP"/*.json
  install -m 0755 "$SOURCE" "$MAIN_FINAL"
  install -m 0755 "$SOURCE" "$FRONT_FINAL"
}

verify_final() {
  local phase="$1"
  local main_console front_console
  main_console="$(console_sha 34273)"
  front_console="$(console_sha 34274)"
  [[ "$main_console" == "$FINAL_CONSOLE_SHA" ]]
  [[ "$front_console" == "$FINAL_CONSOLE_SHA" ]]
  printf '%s\n' "$main_console" >"$RECORDS/${phase}-main-console.sha256"
  printf '%s\n' "$front_console" >"$RECORDS/${phase}-frontend-console.sha256"
  [[ "$(config_sha "$MAIN_CFG")" == "$(config_sha "$BACKUP/main-config.json")" ]]
  [[ "$(config_sha "$FRONT_CFG")" == "$(config_sha "$BACKUP/frontend-config.json")" ]]
  data_summary "$MAIN_DB" "$RECORDS/${phase}-main-data.json"
  data_summary "$FRONT_DB" "$RECORDS/${phase}-frontend-data.json"
  assert_data_equal "$RECORDS/pre-main-data.json" "$RECORDS/${phase}-main-data.json"
  assert_data_equal "$RECORDS/pre-frontend-data.json" "$RECORDS/${phase}-frontend-data.json"
}

deploy() {
  prepare
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  backup_db "$MAIN_DB" "$BACKUP/main.sqlite3"
  backup_db "$FRONT_DB" "$BACKUP/frontend.sqlite3"
  data_summary "$MAIN_DB" "$RECORDS/pre-main-data.json"
  data_summary "$FRONT_DB" "$RECORDS/pre-frontend-data.json"
  if ! start_service "$MAIN_FINAL" "$MAIN_CFG" "$MAIN_FINAL_RELEASE" 34273 final-main ||
     ! start_service "$FRONT_FINAL" "$FRONT_CFG" "$FRONT_FINAL_RELEASE" 34274 final-frontend; then
    rollback
    return 1
  fi
  verify_final deploy
  printf 'DEPLOY_OK=1\n'
}

rollback() {
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  cp -p "$BACKUP/main-config.json" "$MAIN_CFG"
  cp -p "$BACKUP/frontend-config.json" "$FRONT_CFG"
  restore_db "$BACKUP/main.sqlite3" "$MAIN_DB"
  restore_db "$BACKUP/frontend.sqlite3" "$FRONT_DB"
  start_service "$BACKUP/main-previous.bin" "$MAIN_CFG" "$MAIN_PREVIOUS_RELEASE" 34273 rollback-main
  start_service "$BACKUP/frontend-previous.bin" "$FRONT_CFG" "$FRONT_PREVIOUS_RELEASE" 34274 rollback-frontend
  data_summary "$MAIN_DB" "$RECORDS/rollback-main-data.json"
  data_summary "$FRONT_DB" "$RECORDS/rollback-frontend-data.json"
  assert_data_equal "$RECORDS/pre-main-data.json" "$RECORDS/rollback-main-data.json"
  assert_data_equal "$RECORDS/pre-frontend-data.json" "$RECORDS/rollback-frontend-data.json"
  [[ "$(console_sha 34273)" != "$FINAL_CONSOLE_SHA" ]]
  [[ "$(console_sha 34274)" != "$FINAL_CONSOLE_SHA" ]]
  printf 'ROLLBACK_OK=1\n'
}

redeploy() {
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  cp -p "$BACKUP/main-config.json" "$MAIN_CFG"
  cp -p "$BACKUP/frontend-config.json" "$FRONT_CFG"
  restore_db "$BACKUP/main.sqlite3" "$MAIN_DB"
  restore_db "$BACKUP/frontend.sqlite3" "$FRONT_DB"
  start_service "$MAIN_FINAL" "$MAIN_CFG" "$MAIN_FINAL_RELEASE" 34273 redeploy-main
  start_service "$FRONT_FINAL" "$FRONT_CFG" "$FRONT_FINAL_RELEASE" 34274 redeploy-frontend
  verify_final redeploy
  printf 'REDEPLOY_OK=1\n'
}

status() {
  wait_ready http://127.0.0.1:34273/readyz "$MAIN_FINAL_RELEASE"
  wait_ready http://127.0.0.1:34274/readyz "$FRONT_FINAL_RELEASE"
  verify_final status
  printf 'FINAL_STATUS_OK=1\n'
}

case "${1:-}" in
  deploy) deploy ;;
  rollback) rollback ;;
  redeploy) redeploy ;;
  status) status ;;
  *) printf 'usage: %s {deploy|rollback|redeploy|status}\n' "$0" >&2; exit 2 ;;
esac
