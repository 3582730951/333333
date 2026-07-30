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

# The old legacy settings endpoint exposes only a small compatibility subset.
# The registry-backed /admin/config endpoint owns the remaining runtime fields.
request setting-conversation PATCH /admin/settings 200 '{"conversation_isolation":true}'
request config-web-search PATCH /admin/config 200 '{"web_search_enabled":true}'
request config-identity-os PATCH /admin/config 200 '{"identity_os_source":"diverse"}'
request config-goal PATCH /admin/config 200 '{"goal_continuity_enabled":true}'

for resource in accounts egress-profiles egress-pools groups user-groups api-keys providers settings config; do
  request "list-${resource}" GET "/admin/${resource}" 200
done

if ! test -e "$ROOT/etc/config.pre-customization.json"; then
  cp -a "$ROOT/etc/config.json" "$ROOT/etc/config.pre-customization.json"
fi
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

cp "$OUT/account-ids.txt" "$ROOT/logs/seed-account-ids.txt"
python3 - "$OUT/user-group.json" "$OUT/downstream-key.json" "$ROOT/logs/seed-summary.json" <<'PY'
import hashlib, json, sys
ug=json.load(open(sys.argv[1]))
key=json.load(open(sys.argv[2]))
summary={
    "user_group_id":ug["id"],
    "downstream_key_hash":key["key_hash"],
    "downstream_key_label":key["label"],
}
json.dump(summary,open(sys.argv[3],"w"),indent=2,sort_keys=True)
PY

echo "config_sha256=$(sha256sum "$ROOT/etc/config.json" | awk '{print $1}')"
echo "database_sha256=$(sha256sum "$ROOT/state/pool.sqlite3" | awk '{print $1}')"
