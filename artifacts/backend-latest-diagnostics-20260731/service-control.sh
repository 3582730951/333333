#!/usr/bin/env bash
set -euo pipefail

ROOT=/root/autodl-tmp/backend-latest-fix-20260731
BACKUP="$ROOT/deployment-backup"
BUILT="$ROOT/bin/codex-pool-server"
MAIN_CFG=/root/autodl-tmp/cpupg-20260730/etc/config.json
FRONT_CFG=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json
MAIN_DB=/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3
FRONT_DB=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/pool.sqlite3
MAIN_OLD=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/new-goal-context-fix/codex-pool-server
FRONT_OLD=/root/autodl-tmp/frontend-ui-shot-20260731/optimized/codex-pool-server
MAIN_NEW=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/backend-headroom-apple-final/codex-pool-server
FRONT_NEW=/root/autodl-tmp/frontend-ui-shot-20260731/releases/backend-headroom-apple-final/codex-pool-server

find_pids() {
  python3 - "$1" <<'PY'
import os,sys
config=sys.argv[1]
for entry in os.scandir("/proc"):
    if not entry.name.isdigit():
        continue
    try:
        args=open(f"/proc/{entry.name}/cmdline","rb").read().split(b"\0")
    except (FileNotFoundError,PermissionError):
        continue
    text=[arg.decode(errors="replace") for arg in args if arg]
    if config in text and "--config" in text and "--release-id" in text:
        print(entry.name)
PY
}

stop_service() {
  local config=$1 pids
  pids=$(find_pids "$config")
  [[ -z "$pids" ]] && return 0
  kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 120); do
    [[ -z "$(find_pids "$config")" ]] && return 0
    sleep .25
  done
  pids=$(find_pids "$config")
  [[ -z "$pids" ]] || kill -KILL $pids 2>/dev/null || true
}

backup_db() {
  python3 - "$1" "$2" <<'PY'
import os,sqlite3,sys
source,destination=sys.argv[1:3]
os.makedirs(os.path.dirname(destination),exist_ok=True)
if os.path.exists(destination):
    raise SystemExit(0)
src=sqlite3.connect(source)
dst=sqlite3.connect(destination)
with dst:
    src.backup(dst)
dst.close()
src.close()
os.chmod(destination,0o600)
PY
}

restore_db() {
  python3 - "$1" "$2" <<'PY'
import os,shutil,sys
source,destination=sys.argv[1:3]
temp=destination+".rollback-tmp"
shutil.copy2(source,temp)
os.replace(temp,destination)
for suffix in ("-wal","-shm"):
    try: os.remove(destination+suffix)
    except FileNotFoundError: pass
PY
}

wait_ready() {
  python3 - "$1" "$2" <<'PY'
import json,sys,time,urllib.request
url,expected=sys.argv[1:3]
last=""
for _ in range(160):
    try:
        with urllib.request.urlopen(url,timeout=1) as response:
            data=json.load(response)
            last=json.dumps(data,sort_keys=True)
            if response.status==200 and data.get("ready") and data.get("release_id")==expected:
                print(last)
                raise SystemExit(0)
    except Exception as error:
        last=type(error).__name__
    time.sleep(.25)
print(last)
raise SystemExit(1)
PY
}

start_service() {
  local binary=$1 config=$2 release=$3 port=$4 name=$5
  nohup "$binary" --config "$config" --release-id "$release" \
    >"$ROOT/logs/$name.log" 2>&1 </dev/null &
  echo $! >"$ROOT/runtime/$name.pid"
  wait_ready "http://127.0.0.1:$port/readyz" "$release"
}

write_manifest() {
  python3 - "$BACKUP" "$MAIN_NEW" "$FRONT_NEW" <<'PY'
import hashlib,json,os,sys,time
backup,main_new,front_new=sys.argv[1:4]
paths={
 "main_original_binary":backup+"/main-original.bin",
 "frontend_original_binary":backup+"/frontend-original.bin",
 "main_database":backup+"/main.sqlite3",
 "frontend_database":backup+"/frontend.sqlite3",
 "main_config":backup+"/main-config.json",
 "frontend_config":backup+"/frontend-config.json",
 "main_modified_binary":main_new,
 "frontend_modified_binary":front_new,
}
items={}
for name,path in paths.items():
    items[name]={"path":path,"bytes":os.path.getsize(path),"sha256":hashlib.sha256(open(path,"rb").read()).hexdigest()}
with open(backup+"/manifest.json","w") as f:
    json.dump({"created_at":int(time.time()),"files":items},f,indent=2)
print(json.dumps(items,indent=2))
PY
}

deploy() {
  mkdir -p "$BACKUP" "$(dirname "$MAIN_NEW")" "$(dirname "$FRONT_NEW")" "$ROOT/logs" "$ROOT/runtime"
  [[ -f "$BACKUP/main-original.bin" ]] || cp -p "$MAIN_OLD" "$BACKUP/main-original.bin"
  [[ -f "$BACKUP/frontend-original.bin" ]] || cp -p "$FRONT_OLD" "$BACKUP/frontend-original.bin"
  [[ -f "$BACKUP/main-config.json" ]] || cp -p "$MAIN_CFG" "$BACKUP/main-config.json"
  [[ -f "$BACKUP/frontend-config.json" ]] || cp -p "$FRONT_CFG" "$BACKUP/frontend-config.json"
  install -m 0755 "$BUILT" "$MAIN_NEW"
  install -m 0755 "$BUILT" "$FRONT_NEW"
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  backup_db "$MAIN_DB" "$BACKUP/main.sqlite3"
  backup_db "$FRONT_DB" "$BACKUP/frontend.sqlite3"
  if ! start_service "$MAIN_NEW" "$MAIN_CFG" backend-headroom-final-main 34273 backend-main ||
     ! start_service "$FRONT_NEW" "$FRONT_CFG" backend-headroom-final-frontend 34274 backend-frontend; then
    rollback
    return 1
  fi
  write_manifest
  echo "DEPLOY_OK main_pid=$(cat "$ROOT/runtime/backend-main.pid") frontend_pid=$(cat "$ROOT/runtime/backend-frontend.pid")"
}

rollback() {
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  cp -p "$BACKUP/main-config.json" "$MAIN_CFG"
  cp -p "$BACKUP/frontend-config.json" "$FRONT_CFG"
  restore_db "$BACKUP/main.sqlite3" "$MAIN_DB"
  restore_db "$BACKUP/frontend.sqlite3" "$FRONT_DB"
  start_service "$BACKUP/main-original.bin" "$MAIN_CFG" new-goal-context-fix 34273 rollback-main
  start_service "$BACKUP/frontend-original.bin" "$FRONT_CFG" frontend-apple-final 34274 rollback-frontend
  echo "ROLLBACK_OK main_pid=$(cat "$ROOT/runtime/rollback-main.pid") frontend_pid=$(cat "$ROOT/runtime/rollback-frontend.pid")"
}

redeploy() {
  stop_service "$MAIN_CFG"
  stop_service "$FRONT_CFG"
  start_service "$MAIN_NEW" "$MAIN_CFG" backend-headroom-final-main 34273 backend-main
  start_service "$FRONT_NEW" "$FRONT_CFG" backend-headroom-final-frontend 34274 backend-frontend
  echo "REDEPLOY_OK main_pid=$(cat "$ROOT/runtime/backend-main.pid") frontend_pid=$(cat "$ROOT/runtime/backend-frontend.pid")"
}

case "${1:-}" in
  deploy) deploy ;;
  rollback) rollback ;;
  redeploy) redeploy ;;
  *) echo "usage: $0 {deploy|rollback|redeploy}" >&2; exit 2 ;;
esac
