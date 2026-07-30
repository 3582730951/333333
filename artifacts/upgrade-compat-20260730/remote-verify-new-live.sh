#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"
BASE="http://127.0.0.1:$PORT"
OUT="$ROOT/logs/post-upgrade"
mkdir -p "$OUT"

curl -fsS "$BASE/readyz" >"$OUT/ready.json"
for resource in accounts egress-profiles egress-pools groups user-groups api-keys providers settings config system metrics; do
  curl -fsS "$BASE/admin/$resource" >"$OUT/$resource.json"
done

mapfile -t ACCOUNT_IDS <"$ROOT/logs/seed-account-ids.txt"
for index in 0 1; do
  curl -fsS "$BASE/admin/accounts/${ACCOUNT_IDS[$index]}/egress-binding" \
    >"$OUT/account-$index-egress-binding.json"
done

python3 - "$ROOT" "$OUT" <<'PY'
import hashlib, json, pathlib, sqlite3, sys
root=pathlib.Path(sys.argv[1])
out=pathlib.Path(sys.argv[2])

def load(name):
    return json.load(open(out/name))

ready=load("ready.json")
assert ready["ok"] is True and ready["ready"] is True, ready
assert ready["release_id"]=="new-goal-context-fix", ready

raw={name:(out/name).read_text() for name in [
    "accounts.json","egress-profiles.json","egress-pools.json","groups.json",
    "user-groups.json","api-keys.json","providers.json"
]}
for value in ["alpha@example.internal","beta@example.internal","legacy-team"]:
    assert value in raw["accounts.json"], (value,raw["accounts.json"])
for value in ["legacy-direct-us","legacy-http-exit","legacy-sidecar-exit"]:
    assert value in raw["egress-profiles.json"], (value,raw["egress-profiles.json"])
assert "legacy-registration-pool" in raw["egress-pools.json"]
assert "legacy-team" in raw["groups.json"]
assert "Legacy downstream users" in raw["user-groups.json"]
assert "Legacy Codex downstream" in raw["api-keys.json"]
assert "legacy-relay" in raw["providers.json"]
assert "fixture-access-alpha" not in raw["accounts.json"]
assert "fixture-refresh-alpha" not in raw["accounts.json"]

binding0=load("account-0-egress-binding.json")
binding1=load("account-1-egress-binding.json")
assert binding0["primary_egress_id"]=="legacy-direct-us", binding0
assert binding1["primary_egress_id"]=="legacy-http-exit", binding1
assert "legacy-http-exit" in binding0["standby_egress_ids"], binding0
assert "legacy-direct-us" in binding1["standby_egress_ids"], binding1
assert binding0["sidecar_egress_id"]=="legacy-sidecar-exit", binding0
assert binding1["sidecar_egress_id"]=="legacy-sidecar-exit", binding1

settings=load("settings.json")
assert settings["conversation_isolation"] is True, settings
assert settings["web_search_enabled"] is True, settings
assert settings["identity_os_source"]=="diverse", settings

config_rows={row["key"]:row for row in load("config.json")}
assert config_rows["goal_continuity_enabled"]["value"] is True
assert config_rows["codex_session_mapping_enabled"]["value"] is True

config_path=root/"etc/config.json"
config=json.load(open(config_path))
expected_config={
    "node_id":"legacy-cloud-upgrade-fixture",
    "goal_continuity_enabled":True,
    "codex_session_mapping_enabled":True,
    "codex_install_model":"gpt-5.6-sol",
    "codex_install_effort":"xhigh",
    "codex_install_approval_policy":"never",
    "codex_install_sandbox_mode":"danger-full-access",
    "web_search_enabled":True,
    "identity_os_source":"diverse",
}
for key,value in expected_config.items():
    assert config[key]==value,(key,config.get(key),value)

baseline={}
for line in (root/"backup-pre-upgrade/baseline.sha256").read_text().splitlines():
    digest,path=line.split(maxsplit=1)
    baseline[path]=digest
config_hash=hashlib.sha256(config_path.read_bytes()).hexdigest()
baseline_config=[v for k,v in baseline.items() if k.endswith("/config.json")][0]
assert config_hash==baseline_config,(config_hash,baseline_config)

db=sqlite3.connect(root/"state/pool.sqlite3")
assert db.execute("pragma integrity_check").fetchone()[0]=="ok"
tables={r[0] for r in db.execute(
    "select name from sqlite_master where type='table' and name not like 'sqlite_%'"
)}
for required in [
    "accounts","egress_profiles","egress_pools","egress_pool_members",
    "groups","api_keys","user_groups","custom_providers","goal_session",
    "goal_alias","codex_session_binding","codex_session_alias",
]:
    assert required in tables,(required,sorted(tables))

post={"ready":ready,"config_sha256":config_hash,"integrity_check":"ok","tables":{}}
for table in sorted(tables):
    q='"'+table.replace('"','""')+'"'
    post["tables"][table]={
        "count":db.execute(f"select count(*) from {q}").fetchone()[0],
        "columns":[r[1] for r in db.execute(f"pragma table_info({q})")],
    }
json.dump(post,open(out/"post-upgrade-db-snapshot.json","w"),indent=2,sort_keys=True)
print("live_upgrade_data_verified",json.dumps({
    "release":ready["release_id"],
    "accounts":2,
    "egress_profiles":3,
    "egress_pool":"legacy-registration-pool",
    "provider":"legacy-relay",
    "config_sha256":config_hash,
    "sqlite_integrity":"ok",
},sort_keys=True))
PY

# Exercise the deployed, generated Codex-only one-click configuration script.
KEY="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["key"])' "$ROOT/logs/seed/downstream-key.json")"
ENCODED_KEY="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1],safe=""))' "$KEY")"
curl -fsS "$BASE/file/$ENCODED_KEY?client=codex" >"$OUT/setup-pool-codex.sh"
bash -n "$OUT/setup-pool-codex.sh"

TEST_HOME="$ROOT/codex-client-home"
rm -rf "$TEST_HOME"
mkdir -p "$TEST_HOME/.codex"
cat >"$TEST_HOME/.codex/config.toml" <<'EOF'
model = "legacy-model"
model_provider = "legacy-provider"
model_context_window = 123456
model_auto_compact_token_limit = 100000
custom_root = "preserve-me"

[features]
goals = false
multi_agent = true

[mcp_servers.keep]
command = "keep-mcp"

[model_providers.poolserver]
name = "old pool"
base_url = "https://old.invalid/v1"
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = "old-key"
EOF
printf '%s\n' '{"provider":"keep-cache"}' >"$TEST_HOME/.codex/models_cache.json"
HOME="$TEST_HOME" CODEX_HOME="$TEST_HOME/.codex" bash "$OUT/setup-pool-codex.sh" \
  >"$OUT/setup-pool-codex.output.txt"

python3 - "$TEST_HOME" "$BASE" "$KEY" "$OUT/codex-installer-verification.json" <<'PY'
import glob, hashlib, json, pathlib, sys
home=pathlib.Path(sys.argv[1])
base=sys.argv[2]
key=sys.argv[3]
config=(home/".codex/config.toml").read_text()
checks={
    "model":config.count('model = "gpt-5.6-sol"')==1,
    "provider":config.count('model_provider = "poolserver"')==1,
    "effort":config.count('model_reasoning_effort = "xhigh"')==1,
    "approval":config.count('approval_policy = "never"')==1,
    "sandbox":config.count('sandbox_mode = "danger-full-access"')==1,
    "goal_experimental":config.count("goals = true")==1,
    "url":config.count(f'base_url = "{base}/v1"')==1,
    "api_key":config.count(f'experimental_bearer_token = "{key}"')==1,
    "preserved_context_window":config.count("model_context_window = 123456")==1,
    "preserved_auto_compact":config.count("model_auto_compact_token_limit = 100000")==1,
    "preserved_custom_root":config.count('custom_root = "preserve-me"')==1,
    "preserved_multi_agent":config.count("multi_agent = true")==1,
    "preserved_mcp":config.count('command = "keep-mcp"')==1,
    "model_cache_unchanged":(home/".codex/models_cache.json").read_text()=='{"provider":"keep-cache"}\n',
    "no_token_file":not (home/".codex/pool-token").exists(),
    "no_client_id_file":not (home/".codex/pool-client-id").exists(),
    "no_claude_tree":not (home/".claude").exists(),
    "backup_created":len(glob.glob(str(home/".codex/config.toml.bak.*")))==1,
}
assert all(checks.values()),(checks,config)
result={
    "checks":checks,
    "config_sha256":hashlib.sha256(config.encode()).hexdigest(),
    "managed_files":sorted(str(p.relative_to(home)) for p in home.rglob("*") if p.is_file()),
}
json.dump(result,open(sys.argv[4],"w"),indent=2,sort_keys=True)
print("codex_one_click_verified",json.dumps(result,sort_keys=True))
PY

{
  echo "ready=$(cat "$OUT/ready.json")"
  echo "config_sha256=$(sha256sum "$ROOT/etc/config.json" | awk '{print $1}')"
  echo "database_sha256_after_start=$(sha256sum "$ROOT/state/pool.sqlite3" | awk '{print $1}')"
  echo "new_pid=$(cat "$ROOT/new.pid")"
  echo "codex_setup_sha256=$(sha256sum "$OUT/setup-pool-codex.sh" | awk '{print $1}')"
} | tee "$OUT/live-verification.txt"
