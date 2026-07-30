#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"
BASE="http://127.0.0.1:$PORT"
OUT="$ROOT/logs/seed"
mkdir -p "$OUT"

request() {
  local name=$1 method=$2 path=$3 allowed=$4 body=${5-}
  local response="$OUT/$name.json" code
  if test -n "$body"; then
    code="$(curl -sS -o "$response" -w '%{http_code}' \
      -X "$method" -H 'Content-Type: application/json' \
      --data-binary "$body" "$BASE$path")"
  else
    code="$(curl -sS -o "$response" -w '%{http_code}' \
      -X "$method" "$BASE$path")"
  fi
  printf '%-30s %s %s -> %s\n' "$name" "$method" "$path" "$code" | tee -a "$OUT/requests.log"
  case ",$allowed," in
    *",$code,"*) ;;
    *) cat "$response" >&2; exit 1 ;;
  esac
}

request egress-direct POST /admin/egress-profiles 200 '{
  "id":"legacy-direct-us",
  "name":"Legacy direct US",
  "type":"direct",
  "region":"US",
  "ip_mode":"static_residential",
  "stream_capable":true,
  "health":"healthy",
  "max_concurrency":4,
  "detect_region":false
}'

request egress-http POST /admin/egress-profiles 200 '{
  "id":"legacy-http-exit",
  "name":"Legacy HTTP exit",
  "type":"http_proxy",
  "endpoint":"http://fixture-user:fixture-pass@127.0.0.1:18999",
  "region":"SG",
  "ip_mode":"datacenter",
  "provider_key":"legacy-proxy",
  "stream_capable":true,
  "health":"healthy",
  "max_concurrency":3,
  "detect_region":false,
  "dynamic_config_json":{"fixture":"pre-upgrade","rotation":"manual"}
}'

request egress-sidecar POST /admin/egress-profiles 200 '{
  "id":"legacy-sidecar-exit",
  "name":"Legacy registration sidecar",
  "type":"curl_cffi_sidecar",
  "endpoint":"http://127.0.0.1:18790",
  "region":"JP",
  "ip_mode":"static_residential",
  "provider_key":"legacy-sidecar",
  "stream_capable":true,
  "health":"healthy",
  "max_concurrency":2,
  "detect_region":false
}'

request egress-pool POST /admin/egress-pools 200 '{
  "id":"legacy-registration-pool",
  "name":"Legacy registration exits",
  "purpose":"registration",
  "assignment_strategy":"sticky_least_used"
}'

request egress-pool-member POST /admin/egress-pools/legacy-registration-pool/members 200 '{
  "egress_id":"legacy-sidecar-exit",
  "enabled":true,
  "capacity":7
}'

request account-group POST /admin/groups 200 '{
  "name":"legacy-team",
  "egress_ids":["legacy-direct-us","legacy-http-exit"],
  "default_egress_id":"legacy-direct-us"
}'

request group-egress-policy POST /admin/groups/legacy-team/egress-policy 200 '{
  "registration_pool_id":"legacy-registration-pool",
  "assignment_strategy":"sticky_least_used"
}'

request accounts-import POST /admin/accounts/import-auth-json 200 '{
  "auth_json":[
    {
      "type":"codex",
      "access_token":"fixture-access-alpha",
      "refresh_token":"fixture-refresh-alpha",
      "id_token":"legacy-placeholder",
      "account_id":"legacy-workspace-alpha",
      "email":"alpha@example.internal",
      "plan_type":"plus"
    },
    {
      "type":"codex",
      "access_token":"fixture-access-beta",
      "refresh_token":"fixture-refresh-beta",
      "id_token":"legacy-placeholder",
      "account_id":"legacy-workspace-beta",
      "email":"beta@example.internal",
      "plan_type":"team"
    }
  ],
  "group_name":"legacy-team"
}'

python3 - "$OUT/accounts-import.json" "$OUT/account-ids.txt" <<'PY'
import json, sys
d=json.load(open(sys.argv[1]))
ids=[]
for item in d.get("items", []):
    value=item.get("account_id") or item.get("id")
    if value:
        ids.append(value)
if len(ids) != 2:
    raise SystemExit(f"expected two imported account ids, got {ids}: {d}")
open(sys.argv[2],"w").write("\n".join(ids)+"\n")
PY
mapfile -t ACCOUNT_IDS <"$OUT/account-ids.txt"

request account-alpha-egress POST "/admin/accounts/${ACCOUNT_IDS[0]}/egress-binding" 200 '{
  "primary_egress_id":"legacy-direct-us",
  "standby_egress_ids":["legacy-http-exit"],
  "sidecar_egress_id":"legacy-sidecar-exit"
}'

request account-beta-egress POST "/admin/accounts/${ACCOUNT_IDS[1]}/egress-binding" 200 '{
  "primary_egress_id":"legacy-http-exit",
  "standby_egress_ids":["legacy-direct-us"],
  "sidecar_egress_id":"legacy-sidecar-exit"
}'

request user-group POST /admin/user-groups 201 '{
  "name":"Legacy downstream users",
  "targets":[{"kind":"account_pool_group","id":"legacy-team"}]
}'
USER_GROUP_ID="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["id"])' "$OUT/user-group.json")"

request downstream-key POST /admin/api-keys 201 "{
  \"label\":\"Legacy Codex downstream\",
  \"user_group_id\":\"$USER_GROUP_ID\",
  \"force_model\":\"gpt-5.6-sol\",
  \"force_effort\":\"xhigh\"
}"

request provider POST /admin/providers 200 '{
  "id":"legacy-relay",
  "name":"Legacy local relay",
  "base_url":"http://127.0.0.1:19001/v1",
  "upstream_protocol":"responses",
  "enabled":false,
  "auto_discover_models":false,
  "models":["fixture-model-a","fixture-model-b"]
}'

request settings PATCH /admin/settings 200 '{
  "conversation_isolation":true,
  "web_search_enabled":true,
  "identity_os_source":"diverse",
  "goal_continuity_enabled":true
}'

for resource in accounts egress-profiles egress-pools groups user-groups api-keys providers settings; do
  request "list-${resource}" GET "/admin/${resource}" 200
done

cp -a "$ROOT/etc/config.json" "$ROOT/etc/config.pre-customization.json"
python3 - "$ROOT/etc/config.json" <<'PY'
import json, sys
p=sys.argv[1]
with open(p) as f:
    d=json.load(f)
d.update({
    "node_id":"legacy-cloud-upgrade-fixture",
    "goal_continuity_enabled":True,
    "codex_session_mapping_enabled":True,
    "codex_install_model":"gpt-5.6-sol",
    "codex_install_effort":"xhigh",
    "codex_install_approval_policy":"never",
    "codex_install_sandbox_mode":"danger-full-access",
    "web_search_enabled":True,
    "identity_os_source":"diverse",
})
with open(p,"w") as f:
    json.dump(d,f,indent=2,ensure_ascii=False)
    f.write("\n")
PY

{
  sha256sum "$ROOT/etc/config.pre-customization.json"
  sha256sum "$ROOT/etc/config.json"
  sha256sum "$ROOT/state/pool.sqlite3"
} | tee "$ROOT/logs/pre-upgrade-file-hashes.txt"

python3 - "$ROOT/state/pool.sqlite3" "$ROOT/logs/pre-upgrade-db-snapshot.json" <<'PY'
import json, sqlite3, sys
db=sqlite3.connect(sys.argv[1])
db.row_factory=sqlite3.Row
tables=[r[0] for r in db.execute(
    "select name from sqlite_master where type='table' and name not like 'sqlite_%' order by name"
)]
snapshot={"tables":{}}
for table in tables:
    quoted='"'+table.replace('"','""')+'"'
    count=db.execute(f"select count(*) from {quoted}").fetchone()[0]
    columns=[r[1] for r in db.execute(f"pragma table_info({quoted})")]
    snapshot["tables"][table]={"count":count,"columns":columns}
snapshot["fixture_expectations"]={
    "account_emails":["alpha@example.internal","beta@example.internal"],
    "account_group":"legacy-team",
    "egress_ids":["legacy-direct-us","legacy-http-exit","legacy-sidecar-exit"],
    "egress_pool":"legacy-registration-pool",
    "provider":"legacy-relay",
    "downstream_key_label":"Legacy Codex downstream",
}
json.dump(snapshot,open(sys.argv[2],"w"),indent=2,sort_keys=True)
PY

echo "seeded_accounts=${ACCOUNT_IDS[*]}" | tee "$ROOT/logs/seed-summary.txt"
echo "user_group_id=$USER_GROUP_ID" | tee -a "$ROOT/logs/seed-summary.txt"
echo "config_sha256=$(sha256sum "$ROOT/etc/config.json" | awk '{print $1}')" | tee -a "$ROOT/logs/seed-summary.txt"
