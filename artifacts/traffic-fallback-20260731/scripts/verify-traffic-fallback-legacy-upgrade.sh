#!/usr/bin/env bash
set -euo pipefail

root=/root/autodl-tmp/traffic-fallback-20260731
upgrade="$root/upgrade"
record="$root/records"
old_src="$upgrade/old-src"
old_bin="$upgrade/codex-pool-server.old"
new_bin="$root/bin/codex-pool-server"
go=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go
port=34319
base="http://127.0.0.1:$port"
config="$upgrade/config.json"
database="$upgrade/legacy.sqlite3"
admin_token=traffic-fallback-legacy-upgrade-token
admin_auth=(-H "Authorization: Bearer $admin_token")
resume=${RESUME:-0}
pid=
server_ready=

stop_server() {
  if [[ -n "${pid:-}" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in $(seq 1 100); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.05
    done
  fi
  pid=
}
trap stop_server EXIT

start_server() {
  local binary=$1 release=$2 log=$3
  stop_server
  nohup "$binary" --config "$config" --release-id "$release" >"$log" 2>&1 &
  pid=$!
  local ready=
  for _ in $(seq 1 160); do
    if ready="$(curl -fsS "$base/readyz" 2>/dev/null)"; then
      break
    fi
    kill -0 "$pid" 2>/dev/null || {
      cat "$log" >&2
      return 1
    }
    sleep 0.1
  done
  [[ -n "$ready" ]] || {
    cat "$log" >&2
    return 1
  }
  server_ready="$ready"
}

if [[ "$resume" == 0 ]]; then
  rm -rf "$old_src"
  rm -f "$database" "$database-shm" "$database-wal"
  mkdir -p "$old_src" "$upgrade/data"
  tar -xzf "$upgrade/old-source-cache-hit-optimization.tar.gz" -C "$old_src"

  (
    cd "$old_src"
    "$go" build -trimpath -o "$old_bin" ./cmd/pool-server
  )
  "$old_bin" --self-test | tee "$record/legacy-old-self-test.log"
  sha256sum "$old_bin" | tee "$record/legacy-old-binary.sha256"

  cat >"$config" <<EOF
{
  "listen_addr": "127.0.0.1:$port",
  "data_dir": "$upgrade/data",
  "database_path": "$database",
  "admin_token": "$admin_token",
  "default_group": "cyber",
  "require_downstream_key": false,
  "registration_concurrency": 1,
  "model_probe_interval_hours": 24,
  "diagnostic_retention_days": 1
}
EOF
else
  [[ -x "$old_bin" && -x "$new_bin" && -s "$config" && -s "$database" ]]
fi

if [[ "$resume" != post_migrate ]]; then
  start_server "$old_bin" legacy-cache-hit-optimization "$record/legacy-old-server.log"
  printf '%s\n' "$server_ready" | tee "$record/legacy-old-ready.json"

  create_code="$(curl -sS -o "$record/legacy-old-created-user-group.json" -w '%{http_code}' \
    -X POST "${admin_auth[@]}" -H 'Content-Type: application/json' \
    --data-binary '{"name":"Legacy fallback migration sentinel","targets":[{"kind":"account_pool_group","id":"cyber"}]}' \
    "$base/admin/user-groups")"
  printf '%s\n' "$create_code" >"$record/legacy-old-create.status"
  [[ "$create_code" == 201 ]]
  group_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' \
    "$record/legacy-old-created-user-group.json")"
  curl -fsS "${admin_auth[@]}" "$base/admin/user-groups" >"$record/legacy-old-user-groups.json"
  stop_server

  python3 - "$database" "$group_id" "$record/legacy-schema-before.json" <<'PY'
import json
import sqlite3
import sys

db, group_id, output = sys.argv[1:]
connection = sqlite3.connect(db)
columns = [row[1] for row in connection.execute("PRAGMA table_info(user_groups)")]
row = connection.execute("SELECT id, name FROM user_groups WHERE id=?", (group_id,)).fetchone()
result = {
    "columns": columns,
    "has_traffic_fallback_groups_json": "traffic_fallback_groups_json" in columns,
    "has_traffic_fallback_model_mappings_json": "traffic_fallback_model_mappings_json" in columns,
    "sentinel": list(row) if row else None,
    "quick_check": connection.execute("PRAGMA quick_check").fetchone()[0],
    "integrity_check": connection.execute("PRAGMA integrity_check").fetchone()[0],
}
if result["has_traffic_fallback_groups_json"] or result["has_traffic_fallback_model_mappings_json"]:
    raise SystemExit(f"old schema unexpectedly has fallback columns: {result}")
if not row or row[1] != "Legacy fallback migration sentinel":
    raise SystemExit(f"old sentinel missing: {result}")
if result["quick_check"] != "ok" or result["integrity_check"] != "ok":
    raise SystemExit(f"old database failed integrity checks: {result}")
with open(output, "w") as handle:
    json.dump(result, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
PY
else
  group_id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' \
    "$record/legacy-old-created-user-group.json")"
  [[ -s "$record/legacy-schema-before.json" ]]
fi

start_server "$new_bin" traffic-fallback-schema-upgrade "$record/legacy-new-server.log"
printf '%s\n' "$server_ready" | tee "$record/legacy-new-ready.json"
curl -fsS "${admin_auth[@]}" "$base/admin/user-groups" >"$record/legacy-new-user-groups.json"
stop_server

python3 - "$database" "$group_id" "$record/legacy-schema-after.json" <<'PY'
import json
import sqlite3
import sys

db, group_id, output = sys.argv[1:]
connection = sqlite3.connect(db)
columns = [row[1] for row in connection.execute("PRAGMA table_info(user_groups)")]
row = connection.execute(
    """SELECT id, name, traffic_fallback_groups_json,
              traffic_fallback_model_mappings_json
       FROM user_groups WHERE id=?""",
    (group_id,),
).fetchone()
result = {
    "columns": columns,
    "sentinel": list(row) if row else None,
    "fallback_groups": json.loads(row[2]) if row else None,
    "fallback_mappings": json.loads(row[3]) if row else None,
    "quick_check": connection.execute("PRAGMA quick_check").fetchone()[0],
    "integrity_check": connection.execute("PRAGMA integrity_check").fetchone()[0],
}
required = {"traffic_fallback_groups_json", "traffic_fallback_model_mappings_json"}
if not required.issubset(columns):
    raise SystemExit(f"new schema columns missing: {result}")
if not row or row[1] != "Legacy fallback migration sentinel":
    raise SystemExit(f"sentinel not preserved: {result}")
if result["fallback_groups"] != {} or result["fallback_mappings"] != []:
    raise SystemExit(f"migration defaults incorrect: {result}")
if result["quick_check"] != "ok" or result["integrity_check"] != "ok":
    raise SystemExit(f"upgraded database failed integrity checks: {result}")
with open(output, "w") as handle:
    json.dump(result, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
PY

python3 - "$record/legacy-new-user-groups.json" "$group_id" <<'PY'
import json
import sys

rows = json.load(open(sys.argv[1]))
group_id = sys.argv[2]
group = next((item for item in rows if item.get("id") == group_id), None)
if not group:
    raise SystemExit("upgraded API did not return the sentinel group")
if group.get("traffic_fallback_groups") not in ({}, {"gpt": [], "claude": [], "gemini": []}):
    raise SystemExit(f"unexpected API fallback group default: {group}")
if group.get("traffic_fallback_model_mappings") != []:
    raise SystemExit(f"unexpected API fallback mapping default: {group}")
PY

start_server "$old_bin" legacy-rollback-after-fallback-schema "$record/legacy-rollback-server.log"
printf '%s\n' "$server_ready" | tee "$record/legacy-rollback-ready.json"
curl -fsS "${admin_auth[@]}" "$base/admin/user-groups" >"$record/legacy-rollback-user-groups.json"
stop_server

python3 - "$record/legacy-rollback-user-groups.json" "$group_id" <<'PY'
import json
import sys

rows = json.load(open(sys.argv[1]))
group_id = sys.argv[2]
if not any(item.get("id") == group_id and item.get("name") == "Legacy fallback migration sentinel" for item in rows):
    raise SystemExit("old binary rollback did not preserve/read the sentinel group")
PY

cat >"$record/legacy-feature-upgrade.status" <<EOF
OLD_SCHEMA_FALLBACK_COLUMNS=absent
NEW_SCHEMA_FALLBACK_COLUMNS=present
SENTINEL_PRESERVED=1
DEFAULT_FALLBACK_GROUPS={}
DEFAULT_FALLBACK_MAPPINGS=[]
QUICK_CHECK=ok
INTEGRITY_CHECK=ok
OLD_BINARY_ROLLBACK_READY=1
LEGACY_FEATURE_UPGRADE_OK=1
EOF
cat "$record/legacy-feature-upgrade.status"
