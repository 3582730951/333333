#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 BINARY LABEL RUN_DIR PORT" >&2
  exit 2
fi

binary="$(readlink -f "$1")"
label="$2"
run_dir="$3"
port="$4"
cpu_set="${CODEX_TEST_CPU_SET:-190,191}"
auth_token="diagnostic-performance-token"

rm -rf "$run_dir"
install -d -m 700 "$run_dir" "$run_dir/data"
cat >"$run_dir/config.json" <<JSON
{
  "listen_addr": "127.0.0.1:${port}",
  "database_path": "${run_dir}/pool.sqlite3",
  "data_dir": "${run_dir}/data",
  "admin_token": "${auth_token}",
  "identity_secret": "diagnostic-performance-identity",
  "model_probe_interval_hours": 0
}
JSON

(
  TIMEFORMAT='server_elapsed_seconds=%R server_user_seconds=%U server_sys_seconds=%S'
  time exec taskset -c "$cpu_set" ionice -c3 nice -n 15 "$binary" \
    --config "$run_dir/config.json" --deployment-role active
) >"$run_dir/server.log" 2>"$run_dir/server-time.log" &
server_pid=$!
monitor_pid=""
cleanup() {
  if [ -n "$monitor_pid" ]; then
    kill "$monitor_pid" 2>/dev/null || true
    wait "$monitor_pid" 2>/dev/null || true
  fi
  kill -INT "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 1 300); do
  if curl -fsS --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$run_dir/server.log" "$run_dir/server-time.log" >&2
    exit 1
  fi
  sleep 0.1
done
curl -fsS --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null

(
  peak=0
  while kill -0 "$server_pid" 2>/dev/null; do
    rss="$(awk '/^VmRSS:/ {print $2}' "/proc/${server_pid}/status" 2>/dev/null || echo 0)"
    if [ "${rss:-0}" -gt "$peak" ]; then
      peak="$rss"
      printf '%s\n' "$peak" >"$run_dir/peak-rss-kib.txt"
    fi
    sleep 0.2
  done
) &
monitor_pid=$!

taskset -c "$cpu_set" ionice -c3 nice -n 15 python3 - "$run_dir" "$port" "$auth_token" "$label" <<'PY'
import hashlib
import json
import sqlite3
import sys
import time
import urllib.request
import zipfile
from pathlib import Path

run_dir = Path(sys.argv[1])
port = int(sys.argv[2])
token = sys.argv[3]
label = sys.argv[4]
database = run_dir / "pool.sqlite3"
base = f"http://127.0.0.1:{port}"
headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

connection = sqlite3.connect(database, timeout=60)
connection.execute("PRAGMA busy_timeout=60000")
now = int(time.time())
load_started = time.perf_counter()
connection.execute(
    """WITH RECURSIVE n(x) AS (
           VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<500000
       )
       INSERT INTO usage_records(
           account_id,model,prompt_tokens,completion_tokens,total_tokens,
           usage_provider,usage_source,created_at
       )
       SELECT 'account-'||(x%100),'gpt-5.6',1200,300,1500,
              'codex','stream',?-(x%2592000)
       FROM n""",
    (now,),
)
connection.commit()
connection.execute(
    """WITH RECURSIVE n(x) AS (
           VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<200000
       )
       INSERT INTO audit_log(
           account_id,account_label,action,state,reason,detail,created_at
       )
       SELECT 'account-'||(x%100),'account-'||(x%100),
              'route','alive','ok','status=200 model=gpt-5.6',
              ?-(x%2592000)
       FROM n""",
    (now,),
)
connection.commit()
source_counts = {
    table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
    for table in ("usage_records", "audit_log")
}
database_bytes = (
    connection.execute("PRAGMA page_count").fetchone()[0]
    * connection.execute("PRAGMA page_size").fetchone()[0]
)
connection.close()
load_elapsed = time.perf_counter() - load_started

def request_json(path, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(base + path, data=data, method=method, headers=headers)
    with urllib.request.urlopen(request, timeout=15) as response:
        return json.load(response)

started = time.perf_counter()
created = request_json("/admin/diagnostics/jobs", "POST", {})
job_id = created["job"]["id"]
states = []
previous = None
deadline = started + 600
while True:
    job = request_json(f"/admin/diagnostics/jobs/{job_id}")
    status = job["status"]
    if status != previous:
        states.append({"status": status, "at_ms": round((time.perf_counter() - started) * 1000, 3)})
        previous = status
    if status == "ready":
        break
    if status in {"failed", "cancelled", "expired"}:
        raise RuntimeError(f"diagnostic job terminal state: {job}")
    if time.perf_counter() >= deadline:
        raise TimeoutError(f"diagnostic job timed out in state {status}")
    time.sleep(0.1)

archive = run_dir / f"{label}-diagnostics.zip"
download = urllib.request.Request(
    base + f"/admin/diagnostics/jobs/{job_id}/download",
    method="GET",
    headers={"Authorization": f"Bearer {token}"},
)
with urllib.request.urlopen(download, timeout=60) as response, archive.open("wb") as output:
    while True:
        chunk = response.read(128 * 1024)
        if not chunk:
            break
        output.write(chunk)
elapsed = time.perf_counter() - started
with zipfile.ZipFile(archive) as bundle:
    bad_member = bundle.testzip()
    manifest = json.loads(bundle.read("manifest.json"))
    audit_rows = sum(1 for _ in bundle.open("audit_log.csv")) - 1
    usage_rows = sum(1 for _ in bundle.open("usage_records.csv")) - 1
if bad_member:
    raise RuntimeError(f"corrupt ZIP member: {bad_member}")

result = {
    "label": label,
    "source_counts": source_counts,
    "database_bytes": database_bytes,
    "load_elapsed_seconds": round(load_elapsed, 3),
    "states": states,
    "export_elapsed_seconds": round(elapsed, 3),
    "archive_bytes": archive.stat().st_size,
    "archive_sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
    "exported_rows": {"audit_log": audit_rows, "usage_records": usage_rows},
    "manifest_source_row_counts": manifest.get("source_row_counts"),
    "manifest_truncated_tables": manifest.get("truncated_tables"),
    "manifest_large_table_row_limit": manifest.get("large_table_row_limit"),
}
(run_dir / "result.json").write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
print(json.dumps(result, indent=2, sort_keys=True))
PY

kill "$monitor_pid" 2>/dev/null || true
wait "$monitor_pid" 2>/dev/null || true
monitor_pid=""
kill -INT "$server_pid"
wait "$server_pid" || true
trap - EXIT
cat "$run_dir/server-time.log"
printf 'peak_rss_kib=%s\n' "$(cat "$run_dir/peak-rss-kib.txt")"
