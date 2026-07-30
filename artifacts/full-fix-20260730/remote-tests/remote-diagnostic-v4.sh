#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 POOL_SERVER_BINARY RUN_DIR PORT" >&2
  exit 2
fi

binary="$(readlink -f "$1")"
run_dir="$2"
port="$3"
cpu_set="${CODEX_TEST_CPU_SET:-190,191}"
admin_token="isolated-diagnostic-smoke-token"

rm -rf "$run_dir"
install -d -m 700 "$run_dir" "$run_dir/data"
cat >"$run_dir/config.json" <<JSON
{
  "listen_addr": "127.0.0.1:${port}",
  "database_path": "${run_dir}/pool.sqlite3",
  "data_dir": "${run_dir}/data",
  "admin_token": "${admin_token}",
  "identity_secret": "isolated-diagnostic-smoke-identity",
  "model_probe_interval_hours": 0
}
JSON

GOMAXPROCS=2 taskset -c "$cpu_set" nice -n 15 ionice -c3 \
  "$binary" --config "$run_dir/config.json" --deployment-role active \
  >"$run_dir/server.log" 2>&1 &
server_pid=$!
cleanup() {
  kill -INT "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 1 300); do
  if curl -fsS --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$run_dir/server.log" >&2
    exit 1
  fi
  sleep 0.1
done
curl -fsS --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null
printf 'HEALTHZ_EXIT=0\n'

taskset -c "$cpu_set" nice -n 15 ionice -c3 python3 - "$run_dir" "$port" "$admin_token" <<'PY'
import hashlib
import json
import sys
import time
import urllib.request
import zipfile
from pathlib import Path

run_dir = Path(sys.argv[1])
port = int(sys.argv[2])
token = sys.argv[3]
base = f"http://127.0.0.1:{port}"
json_headers = {
    "Authorization": f"Bearer {token}",
    "Content-Type": "application/json",
}

def request(path, method="GET", body=None, timeout=15):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        base + path, data=data, method=method,
        headers=json_headers if body is not None else {"Authorization": f"Bearer {token}"},
    )
    return urllib.request.urlopen(req, timeout=timeout)

started = time.perf_counter()
with request("/admin/diagnostics/jobs", "POST", {}) as response:
    create_status = response.status
    created = json.load(response)
job_id = created["job"]["id"]

states = []
last_state = None
deadline = started + 120
while True:
    with request(f"/admin/diagnostics/jobs/{job_id}") as response:
        status_http = response.status
        status_cache_control = response.headers.get("Cache-Control")
        job = json.load(response)
    state = job["status"]
    if state != last_state:
        states.append({
            "state": state,
            "elapsed_ms": round((time.perf_counter() - started) * 1000, 3),
        })
        last_state = state
    if state == "ready":
        break
    if state in {"failed", "cancelled", "expired"}:
        raise RuntimeError(f"diagnostic job terminal state: {job}")
    if time.perf_counter() >= deadline:
        raise TimeoutError(f"diagnostic job timed out in state {state}")
    time.sleep(0.05)

archive = run_dir / "diagnostics-v4.zip"
with request(f"/admin/diagnostics/jobs/{job_id}/download", timeout=60) as response:
    download_status = response.status
    content_type = response.headers.get("Content-Type", "")
    content_disposition = response.headers.get("Content-Disposition", "")
    cache_control = response.headers.get("Cache-Control", "")
    archive.write_bytes(response.read())

if create_status not in (200, 201, 202):
    raise AssertionError(f"create HTTP status {create_status}")
if status_http != 200:
    raise AssertionError(f"status HTTP status {status_http}")
if status_cache_control != "no-store":
    raise AssertionError(f"status Cache-Control={status_cache_control!r}")
if download_status != 200:
    raise AssertionError(f"download HTTP status {download_status}")
if not content_type.lower().startswith("application/zip"):
    raise AssertionError(f"download Content-Type={content_type!r}")
if not content_disposition.lower().startswith("attachment;"):
    raise AssertionError(f"download Content-Disposition={content_disposition!r}")
if cache_control != "no-store":
    raise AssertionError(f"download Cache-Control={cache_control!r}")
if archive.read_bytes()[:4] != b"PK\x03\x04":
    raise AssertionError("downloaded artifact lacks ZIP local-file signature")

with zipfile.ZipFile(archive) as bundle:
    bad_member = bundle.testzip()
    members = sorted(bundle.namelist())
    if "manifest.json" not in members:
        raise AssertionError(f"manifest.json absent from ZIP: {members}")
if bad_member is not None:
    raise AssertionError(f"corrupt ZIP member: {bad_member}")

result = {
    "archive_bytes": archive.stat().st_size,
    "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
    "create_http_status": create_status,
    "download_headers": {
        "Cache-Control": cache_control,
        "Content-Disposition": content_disposition,
        "Content-Type": content_type,
    },
    "download_http_status": download_status,
    "elapsed_ms": round((time.perf_counter() - started) * 1000, 3),
    "job_id": job_id,
    "states": states,
    "status_cache_control": status_cache_control,
    "status_http_status": status_http,
    "terminal_state": state,
    "zip_member_count": len(members),
    "zip_test": "ok",
}
(run_dir / "result.json").write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
print(json.dumps(result, indent=2, sort_keys=True))
print("DIAGNOSTIC_DOWNLOAD_EXIT=0")
PY

for _ in $(seq 1 200); do
  if grep -Fq "startup: deferred storage migrations completed" "$run_dir/server.log"; then
    break
  fi
  sleep 0.05
done
grep -F "startup: deferred storage migrations completed" "$run_dir/server.log"
if grep -Eiq 'deferred storage migration.*(fail|error|panic)' "$run_dir/server.log"; then
  echo "deferred migration failure marker found" >&2
  exit 1
fi
printf 'DEFERRED_MIGRATION_EXIT=0\n'

kill -INT "$server_pid"
wait "$server_pid"
trap - EXIT
printf 'SERVER_GRACEFUL_EXIT=0\n'
