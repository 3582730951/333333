#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/backend-mail-automation-production-20260731}"
SOURCE_INSTALL="${SOURCE_INSTALL:-/root/autodl-tmp/backend-mail-automation-install-20260731}"

MAIN_CONFIG=/root/autodl-tmp/cpupg-20260730/etc/config.json
FRONT_CONFIG=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json
MAIN_DATABASE=/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3
FRONT_DATABASE=/root/autodl-tmp/frontend-ui-shot-20260731/runtime/pool.sqlite3
MAIN_ORIGINAL_SOURCE=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/team-lifecycle-final/codex-pool-server
FRONT_ORIGINAL_SOURCE=/root/autodl-tmp/frontend-ui-shot-20260731/releases/team-lifecycle-final/codex-pool-server
MAIN_ORIGINAL_RELEASE=team-lifecycle-final-main
FRONT_ORIGINAL_RELEASE=team-lifecycle-final-frontend

SOURCE_BINARY="${SOURCE_INSTALL}/prefix/bin/codex-pool-server"
SOURCE_RELEASE="${SOURCE_INSTALL}/prefix/lib/codex-pool/releases/backend-mail-automation-install-20260731"
MAIN_MODIFIED=/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/backend-mail-automation-final/codex-pool-server
FRONT_MODIFIED=/root/autodl-tmp/frontend-ui-shot-20260731/releases/backend-mail-automation-final/codex-pool-server
MAIN_MODIFIED_RELEASE=backend-mail-automation-final-main
FRONT_MODIFIED_RELEASE=backend-mail-automation-final-frontend

BACKUP="${ROOT}/backup"
WORKER_RELEASE="${ROOT}/worker-release"
RUNTIME="${ROOT}/runtime"
LOGS="${ROOT}/logs"
RECORDS="${ROOT}/records"
WORKER_PORT=8802

mkdir -p "$BACKUP" "$RUNTIME" "$LOGS" "$RECORDS"

find_service_pids() {
  python3 - "$1" <<'PY'
import os
import sys

config = sys.argv[1]
for entry in os.scandir("/proc"):
    if not entry.name.isdigit():
        continue
    try:
        args = [
            item.decode(errors="replace")
            for item in open(f"/proc/{entry.name}/cmdline", "rb").read().split(b"\0")
            if item
        ]
    except (FileNotFoundError, PermissionError):
        continue
    if config in args and "--config" in args and "--release-id" in args:
        print(entry.name)
PY
}

find_worker_pids() {
  python3 - "$WORKER_PORT" <<'PY'
import os
import sys

port = sys.argv[1]
for entry in os.scandir("/proc"):
    if not entry.name.isdigit():
        continue
    try:
        args = [
            item.decode(errors="replace")
            for item in open(f"/proc/{entry.name}/cmdline", "rb").read().split(b"\0")
            if item
        ]
    except (FileNotFoundError, PermissionError):
        continue
    text = " ".join(args)
    if "codex_reauth_worker.py" in text and "--port" in args and port in args:
        print(entry.name)
PY
}

stop_by_pids() {
  local label=$1 pids=$2
  if [[ -z "$pids" ]]; then
    echo "STOP ${label} already-stopped"
    return 0
  fi
  kill -TERM $pids 2>/dev/null || true
  for _ in $(seq 1 120); do
    local alive=
    for pid in $pids; do
      if kill -0 "$pid" 2>/dev/null; then alive="${alive} ${pid}"; fi
    done
    [[ -z "$alive" ]] && { echo "STOP ${label} pids=${pids} state=stopped"; return 0; }
    sleep .25
  done
  kill -KILL $pids 2>/dev/null || true
  echo "STOP ${label} pids=${pids} state=killed"
}

stop_services() {
  stop_by_pids main "$(find_service_pids "$MAIN_CONFIG")"
  stop_by_pids frontend "$(find_service_pids "$FRONT_CONFIG")"
}

stop_worker() {
  stop_by_pids reauth "$(find_worker_pids)"
}

backup_database() {
  python3 - "$1" "$2" <<'PY'
import os
import sqlite3
import sys

source, destination = sys.argv[1:3]
if os.path.exists(destination):
    raise SystemExit(0)
source_connection = sqlite3.connect(source)
destination_connection = sqlite3.connect(destination)
source_connection.backup(destination_connection)
destination_connection.close()
source_connection.close()
os.chmod(destination, 0o600)
PY
}

restore_database() {
  python3 - "$1" "$2" <<'PY'
import os
import shutil
import sys

source, destination = sys.argv[1:3]
temporary = destination + ".backend-mail-rollback-tmp"
shutil.copy2(source, temporary)
os.replace(temporary, destination)
for suffix in ("-wal", "-shm"):
    try:
        os.remove(destination + suffix)
    except FileNotFoundError:
        pass
PY
}

wait_json() {
  python3 - "$1" "$2" "$3" <<'PY'
import json
import sys
import time
import urllib.request

url, field, expected = sys.argv[1:4]
last = ""
for _ in range(200):
    try:
        with urllib.request.urlopen(url, timeout=1) as response:
            body = json.load(response)
            status = response.status
        last = json.dumps(body, sort_keys=True)
        actual = body if field == "." else body.get(field)
        if status == 200 and str(actual).lower() == expected.lower():
            print("READY", url, last)
            raise SystemExit(0)
    except Exception as error:
        last = f"{type(error).__name__}: {error}"
    time.sleep(.25)
raise SystemExit(f"health timeout {url} last={last}")
PY
}

start_worker() {
  stop_worker
  nohup "$WORKER_RELEASE/registrar-python-venv/bin/python" \
    "$WORKER_RELEASE/codex-reauth/codex_reauth_worker.py" \
    --host 127.0.0.1 --port "$WORKER_PORT" --concurrency 2 \
    >"$LOGS/reauth.log" 2>&1 </dev/null &
  echo $! >"$RUNTIME/reauth.pid"
  wait_json "http://127.0.0.1:${WORKER_PORT}/healthz" ready true
}

start_service() {
  local binary=$1 config=$2 release=$3 port=$4 label=$5
  nohup "$binary" --config "$config" --release-id "$release" \
    >"$LOGS/${label}.log" 2>&1 </dev/null &
  echo $! >"$RUNTIME/${label}.pid"
  wait_json "http://127.0.0.1:${port}/readyz" release_id "$release"
}

database_summary() {
  python3 - "$1" <<'PY'
import hashlib
import json
import os
import sqlite3
import sys

path = sys.argv[1]
connection = sqlite3.connect(path)
tables = {
    row[0]
    for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table'")
}
counts = {}
for table in (
    "accounts",
    "email_pool",
    "registration_jobs",
    "team_lifecycle_workflows",
    "provider_settings",
):
    counts[table] = (
        connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
        if table in tables
        else None
    )
payload = {
    "integrity": connection.execute("PRAGMA integrity_check").fetchone()[0],
    "counts": counts,
}
payload["semantic_sha256"] = hashlib.sha256(
    json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
).hexdigest()
print(json.dumps(payload, sort_keys=True))
connection.close()
PY
}

verify_http_surface() {
  local phase=$1 full=$2
  python3 - "$phase" "$full" "$MAIN_CONFIG" "$FRONT_CONFIG" "$RECORDS" <<'PY'
import json
import pathlib
import sys
import urllib.error
import urllib.request

phase, full, main_config_path, front_config_path, records = sys.argv[1:6]
full = full == "1"
services = (
    ("main", "http://127.0.0.1:34273", main_config_path),
    ("frontend", "http://127.0.0.1:34274", front_config_path),
)
result = {"phase": phase, "services": {}}
for name, base, config_path in services:
    config = json.load(open(config_path, encoding="utf-8"))
    token = config["admin_token"]
    paths = [("/console/", False), ("/admin/accounts?page=1&pageSize=1", True)]
    if full:
        paths += [
            ("/admin/register/readiness", True),
            ("/admin/team-lifecycle/stats", True),
            ("/admin/email-pool/cloudflare", True),
        ]
    service = {"paths": {}}
    for path, authenticated in paths:
        request = urllib.request.Request(base + path)
        if authenticated:
            request.add_header("Authorization", "Bearer " + token)
        try:
            with urllib.request.urlopen(request, timeout=8) as response:
                body = response.read()
                status = response.status
        except urllib.error.HTTPError as error:
            body = error.read()
            status = error.code
        service["paths"][path] = {"status": status, "bytes": len(body)}
        if status != 200:
            raise SystemExit(f"{phase} {name} {path} returned {status}")
        if path == "/console/" and b'id="root"' not in body:
            raise SystemExit(f"{phase} {name} console root missing")
    result["services"][name] = service
destination = pathlib.Path(records) / f"surface-{phase}.json"
destination.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
print("SURFACE_VERIFY", json.dumps(result, sort_keys=True))
PY
}

prepare() {
  test -x "$SOURCE_BINARY"
  test -x "$SOURCE_RELEASE/registrar-python-venv/bin/python"
  test -x "$SOURCE_RELEASE/codex-reauth/codex_reauth_worker.py"
  test -x "$MAIN_ORIGINAL_SOURCE"
  test -x "$FRONT_ORIGINAL_SOURCE"

  if [[ ! -f "$BACKUP/prepared" ]]; then
    install -m 0755 "$MAIN_ORIGINAL_SOURCE" "$BACKUP/main-original-codex-pool-server"
    install -m 0755 "$FRONT_ORIGINAL_SOURCE" "$BACKUP/frontend-original-codex-pool-server"
    install -m 0600 "$MAIN_CONFIG" "$BACKUP/main-config.json"
    install -m 0600 "$FRONT_CONFIG" "$BACKUP/frontend-config.json"
    backup_database "$MAIN_DATABASE" "$BACKUP/main.sqlite3"
    backup_database "$FRONT_DATABASE" "$BACKUP/frontend.sqlite3"
    database_summary "$BACKUP/main.sqlite3" >"$BACKUP/main-database-summary.json"
    database_summary "$BACKUP/frontend.sqlite3" >"$BACKUP/frontend-database-summary.json"
    rm -rf "$WORKER_RELEASE"
    mkdir -p "$WORKER_RELEASE"
    cp -a "$SOURCE_RELEASE/registrar-python-venv" "$WORKER_RELEASE/"
    cp -a "$SOURCE_RELEASE/codex-reauth" "$WORKER_RELEASE/"
    touch "$BACKUP/prepared"
  fi

  mkdir -p "$(dirname "$MAIN_MODIFIED")" "$(dirname "$FRONT_MODIFIED")"
  install -m 0755 "$SOURCE_BINARY" "$MAIN_MODIFIED"
  install -m 0755 "$SOURCE_BINARY" "$FRONT_MODIFIED"
  sha256sum \
    "$SOURCE_BINARY" "$MAIN_MODIFIED" "$FRONT_MODIFIED" \
    "$BACKUP/main-original-codex-pool-server" \
    "$BACKUP/frontend-original-codex-pool-server" \
    >"$RECORDS/binaries.sha256"
  echo "PREPARE_OK"
}

write_manifest() {
  python3 - "$ROOT" <<'PY'
import hashlib
import json
import os
import pathlib
import sys
import time

root = pathlib.Path(sys.argv[1])
paths = {
    "modified_main": pathlib.Path("/root/autodl-tmp/cpupg-20260730/prefix/lib/codex-pool/releases/backend-mail-automation-final/codex-pool-server"),
    "modified_frontend": pathlib.Path("/root/autodl-tmp/frontend-ui-shot-20260731/releases/backend-mail-automation-final/codex-pool-server"),
    "original_main": root / "backup/main-original-codex-pool-server",
    "original_frontend": root / "backup/frontend-original-codex-pool-server",
    "original_main_database": root / "backup/main.sqlite3",
    "original_frontend_database": root / "backup/frontend.sqlite3",
    "worker": root / "worker-release/codex-reauth/codex_reauth_worker.py",
}
files = {}
for role, path in paths.items():
    files[role] = {
        "path": str(path),
        "bytes": path.stat().st_size,
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
    }
manifest = {
    "created_at": int(time.time()),
    "original_releases": {
        "main": "team-lifecycle-final-main",
        "frontend": "team-lifecycle-final-frontend",
    },
    "modified_releases": {
        "main": "backend-mail-automation-final-main",
        "frontend": "backend-mail-automation-final-frontend",
    },
    "worker_url": "http://127.0.0.1:8802",
    "files": files,
}
(root / "records/deployment-manifest.json").write_text(
    json.dumps(manifest, indent=2, sort_keys=True) + "\n"
)
print("MANIFEST", json.dumps(manifest, sort_keys=True))
PY
}

deploy() {
  prepare
  stop_services
  start_worker
  start_service "$MAIN_MODIFIED" "$MAIN_CONFIG" "$MAIN_MODIFIED_RELEASE" 34273 main
  start_service "$FRONT_MODIFIED" "$FRONT_CONFIG" "$FRONT_MODIFIED_RELEASE" 34274 frontend
  verify_http_surface deploy 1
  echo "MAIN_DB $(database_summary "$MAIN_DATABASE")"
  echo "FRONT_DB $(database_summary "$FRONT_DATABASE")"
  write_manifest
  echo "DEPLOY_OK main_pid=$(cat "$RUNTIME/main.pid") frontend_pid=$(cat "$RUNTIME/frontend.pid") worker_pid=$(cat "$RUNTIME/reauth.pid")"
}

rollback() {
  test -f "$BACKUP/prepared"
  stop_services
  stop_worker
  install -m 0600 "$BACKUP/main-config.json" "$MAIN_CONFIG"
  install -m 0600 "$BACKUP/frontend-config.json" "$FRONT_CONFIG"
  restore_database "$BACKUP/main.sqlite3" "$MAIN_DATABASE"
  restore_database "$BACKUP/frontend.sqlite3" "$FRONT_DATABASE"
  start_service "$BACKUP/main-original-codex-pool-server" "$MAIN_CONFIG" "$MAIN_ORIGINAL_RELEASE" 34273 rollback-main
  start_service "$BACKUP/frontend-original-codex-pool-server" "$FRONT_CONFIG" "$FRONT_ORIGINAL_RELEASE" 34274 rollback-frontend
  verify_http_surface rollback 0
  current_main=$(database_summary "$MAIN_DATABASE")
  current_front=$(database_summary "$FRONT_DATABASE")
  expected_main=$(cat "$BACKUP/main-database-summary.json")
  expected_front=$(cat "$BACKUP/frontend-database-summary.json")
  [[ "$current_main" == "$expected_main" ]]
  [[ "$current_front" == "$expected_front" ]]
  echo "ROLLBACK_MAIN_DB $current_main"
  echo "ROLLBACK_FRONT_DB $current_front"
  if find_worker_pids | grep -q .; then
    echo "rollback worker still active" >&2
    return 1
  fi
  echo "ROLLBACK_OK main_pid=$(cat "$RUNTIME/rollback-main.pid") frontend_pid=$(cat "$RUNTIME/rollback-frontend.pid") worker=stopped"
}

redeploy() {
  test -x "$MAIN_MODIFIED"
  test -x "$FRONT_MODIFIED"
  stop_services
  start_worker
  start_service "$MAIN_MODIFIED" "$MAIN_CONFIG" "$MAIN_MODIFIED_RELEASE" 34273 main
  start_service "$FRONT_MODIFIED" "$FRONT_CONFIG" "$FRONT_MODIFIED_RELEASE" 34274 frontend
  verify_http_surface redeploy 1
  echo "MAIN_DB $(database_summary "$MAIN_DATABASE")"
  echo "FRONT_DB $(database_summary "$FRONT_DATABASE")"
  echo "REDEPLOY_OK main_pid=$(cat "$RUNTIME/main.pid") frontend_pid=$(cat "$RUNTIME/frontend.pid") worker_pid=$(cat "$RUNTIME/reauth.pid")"
}

verify() {
  wait_json "http://127.0.0.1:8802/healthz" ready true
  wait_json "http://127.0.0.1:34273/readyz" release_id "$MAIN_MODIFIED_RELEASE"
  wait_json "http://127.0.0.1:34274/readyz" release_id "$FRONT_MODIFIED_RELEASE"
  verify_http_surface final 1
  echo "MAIN_DB $(database_summary "$MAIN_DATABASE")"
  echo "FRONT_DB $(database_summary "$FRONT_DATABASE")"
  echo "FINAL_VERIFY_OK"
}

case "${1:-}" in
  deploy) deploy ;;
  rollback) rollback ;;
  redeploy) redeploy ;;
  verify) verify ;;
  *)
    echo "usage: $0 {deploy|rollback|redeploy|verify}" >&2
    exit 2
    ;;
esac
