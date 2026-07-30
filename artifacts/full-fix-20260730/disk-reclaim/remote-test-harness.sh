#!/usr/bin/env bash
set -uo pipefail

ROOT=/root/autodl-tmp/codex-pool-regression-20260730/disk-reclaim-fixture
CASE="$ROOT/case"
SCRIPT="$ROOT/reclaim-disk-space.sh"
LOG="$ROOT/remote-verification.log"
OUT="$ROOT/.command-output"
R=/usr/local/bin/rtk
LOW=("$R" taskset -c 190,191 nice -n 15 ionice -c3)

mkdir -p "$ROOT"
exec >"$LOG" 2>&1

run_logged() {
  local label=$1
  shift
  printf '\n===== %s =====\nCOMMAND:' "$label"
  printf ' %q' "$@"
  printf '\n'
  set +e
  "$@" >"$OUT" 2>&1
  local status=$?
  set -e
  while IFS= read -r line || [[ -n $line ]]; do
    printf '%s\n' "$line"
  done <"$OUT"
  printf 'EXIT_STATUS=%d\n' "$status"
  return "$status"
}

set -e
run_logged "reset isolated fixture" "${LOW[@]}" rm -rf "$CASE"
run_logged "create fixture directories" "${LOW[@]}" mkdir -p \
  "$CASE/data/diagnostics" "$CASE/data/spool" "$CASE/data/tmp/browser" \
  "$CASE/data/logs" "$CASE/backups"

run_logged "create SQLite fixture and baseline hashes" "${LOW[@]}" python3 - "$CASE" <<'PY'
import base64, hashlib, json, os, sqlite3, sys, time
from pathlib import Path

root = Path(sys.argv[1])
db = root / "pool.sqlite3"
data = root / "data"
now = int(time.time())
old = now - 20 * 86400
future = now + 20 * 86400
c = sqlite3.connect(db)
c.execute("PRAGMA journal_mode=WAL")
c.execute("PRAGMA foreign_keys=ON")
c.executescript("""
CREATE TABLE accounts(id TEXT PRIMARY KEY,label TEXT,group_name TEXT,provider TEXT,status TEXT,created_at INTEGER,updated_at INTEGER);
CREATE TABLE account_auth_tokens(account_id TEXT PRIMARY KEY,access_token TEXT,refresh_token TEXT);
CREATE TABLE account_session_cookies(account_id TEXT PRIMARY KEY,cookie TEXT,updated_at INTEGER);
CREATE TABLE account_egress_bindings(account_id TEXT PRIMARY KEY,primary_egress_id TEXT,updated_at INTEGER);
CREATE TABLE account_group_memberships(account_id TEXT,group_name TEXT,is_primary INTEGER,PRIMARY KEY(account_id,group_name));
CREATE TABLE account_lifecycle_status(account_id TEXT PRIMARY KEY,validity_status TEXT,updated_at INTEGER);
CREATE TABLE account_rate_limits(account_id TEXT,provider TEXT,model TEXT,limiter_type TEXT,raw_json TEXT,updated_at INTEGER,PRIMARY KEY(account_id,provider,model,limiter_type));
CREATE TABLE codex_reset_credit_consumptions(account_id TEXT,seven_day_reset_at INTEGER,status TEXT,PRIMARY KEY(account_id,seven_day_reset_at));
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT,updated_at INTEGER);
CREATE TABLE context_journal(response_id TEXT PRIMARY KEY,affinity_hash TEXT,account_id TEXT,encrypted_payload TEXT,created_at INTEGER,expires_at INTEGER);
CREATE TABLE virtual_context_ledger(id INTEGER PRIMARY KEY,route_key_hash TEXT,account_id TEXT,content TEXT,created_at INTEGER);
CREATE TABLE goal_session(id TEXT PRIMARY KEY,protocol TEXT,state TEXT,current_checkpoint_id TEXT,encrypted_working_state TEXT,storage_bytes INTEGER,expires_at INTEGER,created_at INTEGER,updated_at INTEGER);
CREATE TABLE goal_alias(alias_hash TEXT PRIMARY KEY,alias_type TEXT,goal_id TEXT,created_at INTEGER);
CREATE TABLE goal_checkpoint(id TEXT PRIMARY KEY,goal_id TEXT,sequence INTEGER,through_segment_sequence INTEGER,payload_hash TEXT,payload_bytes INTEGER,encrypted_payload TEXT,format_version INTEGER,created_at INTEGER);
CREATE TABLE goal_segment(id TEXT PRIMARY KEY,goal_id TEXT,sequence INTEGER,payload_hash TEXT,payload_bytes INTEGER,encrypted_payload TEXT,format_version INTEGER,state TEXT,created_at INTEGER);
CREATE TABLE goal_payload_chunk(goal_id TEXT,payload_kind TEXT,segment_sequence INTEGER,chunk_index INTEGER,payload_hash TEXT,payload_bytes INTEGER,encrypted_payload TEXT,created_at INTEGER,PRIMARY KEY(goal_id,payload_kind,segment_sequence,chunk_index));
CREATE TABLE goal_run(id TEXT PRIMARY KEY,goal_id TEXT,state TEXT,lease_expires_at INTEGER,updated_at INTEGER);
CREATE TABLE codex_session_binding(id TEXT PRIMARY KEY,tree_id TEXT,state TEXT,encrypted_identity TEXT,expires_at INTEGER);
CREATE TABLE codex_session_alias(alias_hash TEXT,binding_id TEXT,expires_at INTEGER,PRIMARY KEY(alias_hash,binding_id));
CREATE TABLE codex_instruction_snapshot(tree_id TEXT PRIMARY KEY,encrypted_instructions TEXT,expires_at INTEGER);
CREATE TABLE audit_log(id INTEGER PRIMARY KEY,detail TEXT,created_at INTEGER);
CREATE TABLE cf_events(id INTEGER PRIMARY KEY,message TEXT,created_at INTEGER);
CREATE TABLE usage_records(id INTEGER PRIMARY KEY,raw_usage_json TEXT,created_at INTEGER);
CREATE TABLE billing_holds(id TEXT PRIMARY KEY,status TEXT,updated_at INTEGER);
CREATE TABLE usage_events(event_id TEXT PRIMARY KEY,hold_id TEXT,detail TEXT,updated_at INTEGER);
CREATE TABLE registration_task_events(id INTEGER PRIMARY KEY,message TEXT,created_at INTEGER);
CREATE TABLE lifecycle_task_logs(id INTEGER PRIMARY KEY,message TEXT,timestamp INTEGER);
CREATE TABLE lifecycle_events(id INTEGER PRIMARY KEY,event_data TEXT,timestamp INTEGER);
CREATE TABLE proxy_usage_records(id INTEGER PRIMARY KEY,error_message TEXT,created_at INTEGER);
CREATE TABLE model_quality_runs(id INTEGER PRIMARY KEY,error_message TEXT,created_at INTEGER);
CREATE TABLE member_rotation_log(id INTEGER PRIMARY KEY,error_message TEXT,created_at INTEGER);
CREATE TABLE quota_check_log(id INTEGER PRIMARY KEY,check_method TEXT,created_at INTEGER);
CREATE TABLE diagnostic_jobs(id TEXT PRIMARY KEY,status TEXT);
CREATE TABLE diagnostic_download_leases(lease_id TEXT PRIMARY KEY,job_id TEXT);
CREATE TABLE diagnostic_events(id TEXT PRIMARY KEY,detail_json TEXT);
CREATE TABLE storage_resources(id TEXT PRIMARY KEY,resource_type TEXT,retention_class TEXT);
CREATE TABLE affinity_aliases(route_key_hash TEXT PRIMARY KEY,expires_at INTEGER);
CREATE TABLE affinity_bindings(route_key_hash TEXT PRIMARY KEY,expires_at INTEGER,updated_at INTEGER);
CREATE TABLE user_group_target_bindings(user_group_id TEXT,affinity_key TEXT,model TEXT,updated_at INTEGER,PRIMARY KEY(user_group_id,affinity_key,model));
CREATE TABLE antigravity_cache_entries(id INTEGER PRIMARY KEY,expires_at INTEGER);
CREATE TABLE kiro_model_catalog(account_id TEXT,capability_key TEXT,upstream_id TEXT,expires_at INTEGER,PRIMARY KEY(account_id,capability_key,upstream_id));
CREATE TABLE kiro_probe_state(account_id TEXT,capability_key TEXT,expires_at INTEGER,PRIMARY KEY(account_id,capability_key));
CREATE TABLE kiro_runtime_capabilities(account_id TEXT,endpoint_hash TEXT,model TEXT,updated_at INTEGER,PRIMARY KEY(account_id,endpoint_hash,model));
CREATE TABLE account_model_capabilities(account_id TEXT,model_slug TEXT,last_probe_at INTEGER,PRIMARY KEY(account_id,model_slug));
CREATE TABLE account_model_catalog_status(account_id TEXT PRIMARY KEY,last_probe_at INTEGER);
CREATE TABLE user_sessions(token_hash TEXT PRIMARY KEY,expires_at INTEGER);
CREATE TABLE maintenance_leases(lease_name TEXT PRIMARY KEY,expires_at INTEGER);
CREATE TABLE account_codex_reauth_jobs(id INTEGER PRIMARY KEY,status TEXT,updated_at INTEGER);
CREATE TABLE codex_upstream_attempt(id INTEGER PRIMARY KEY,expires_at INTEGER,detail TEXT);
CREATE TABLE codex_upstream_attempt_daily(day_start INTEGER,account_id TEXT,egress_id TEXT,state TEXT,status_code INTEGER,expires_at INTEGER,PRIMARY KEY(day_start,account_id,egress_id,state,status_code));
""")
c.execute("INSERT INTO accounts VALUES(?,?,?,?,?,?,?)", ("acc-1","primary","cyber","codex","active",old,now))
c.execute("INSERT INTO account_auth_tokens VALUES(?,?,?)", ("acc-1","access-secret","refresh-secret"))
c.execute("INSERT INTO account_session_cookies VALUES(?,?,?)", ("acc-1","session-cookie",now))
c.execute("INSERT INTO account_egress_bindings VALUES(?,?,?)", ("acc-1","egress-1",now))
c.execute("INSERT INTO account_group_memberships VALUES(?,?,?)", ("acc-1","cyber",1))
c.execute("INSERT INTO account_lifecycle_status VALUES(?,?,?)", ("acc-1","healthy",now))
c.execute("INSERT INTO account_rate_limits VALUES(?,?,?,?,?,?)", ("acc-1","codex","gpt","requests",'{"remaining":9}',now))
c.execute("INSERT INTO codex_reset_credit_consumptions VALUES(?,?,?)", ("acc-1",now+86400,"complete"))
c.executemany("INSERT INTO settings VALUES(?,?,?)", [
    ("reg_log_retention_days","7",now),
    ("goal_continuity_enabled","true",now),
])
response = "resp-current"
alias = hashlib.sha256(("response_id\0" + response).encode()).hexdigest()
c.execute("INSERT INTO context_journal VALUES(?,?,?,?,?,?)", (response,"affinity","acc-1","encrypted-context",now,future))
c.execute("INSERT INTO virtual_context_ledger VALUES(?,?,?,?,?)", (1,"route","acc-1","current-ledger",now))
c.execute("INSERT INTO goal_session VALUES(?,?,?,?,?,?,?,?,?)", ("goal-1","codex","ready","cp-1","working",128,future,now,now))
c.execute("INSERT INTO goal_alias VALUES(?,?,?,?)", (alias,"response_id","goal-1",now))
c.execute("INSERT INTO goal_checkpoint VALUES(?,?,?,?,?,?,?,?,?)", ("cp-1","goal-1",1,0,"hash",10,"",2,now))
c.execute("INSERT INTO goal_segment VALUES(?,?,?,?,?,?,?,?,?)", ("seg-1","goal-1",1,"hash",10,"",2,"committed",now))
c.execute("INSERT INTO goal_payload_chunk VALUES(?,?,?,?,?,?,?,?)", ("goal-1","checkpoint",0,0,"hash",10,"checkpoint-chunk",now))
c.execute("INSERT INTO goal_payload_chunk VALUES(?,?,?,?,?,?,?,?)", ("goal-1","segment",1,0,"hash",10,"segment-chunk",now))
c.execute("INSERT INTO goal_run VALUES(?,?,?,?,?)", ("run-1","goal-1","completed",0,now))
c.execute("INSERT INTO codex_session_binding VALUES(?,?,?,?,?)", ("bind-1","tree-1","active","identity",future))
c.execute("INSERT INTO codex_session_alias VALUES(?,?,?)", ("alias","bind-1",future))
c.execute("INSERT INTO codex_instruction_snapshot VALUES(?,?,?)", ("tree-1","instructions",future))
large = "X" * (8 << 20)
for table, time_col in [
    ("audit_log","created_at"), ("cf_events","created_at"),
    ("registration_task_events","created_at"), ("lifecycle_task_logs","timestamp"),
    ("lifecycle_events","timestamp"), ("proxy_usage_records","created_at"),
    ("model_quality_runs","created_at"), ("member_rotation_log","created_at"),
    ("quota_check_log","created_at"),
]:
    payload_col = {
        "audit_log":"detail","cf_events":"message","registration_task_events":"message",
        "lifecycle_task_logs":"message","lifecycle_events":"event_data",
        "proxy_usage_records":"error_message","model_quality_runs":"error_message",
        "member_rotation_log":"error_message","quota_check_log":"check_method",
    }[table]
    c.execute(f"INSERT INTO {table}(id,{payload_col},{time_col}) VALUES(1,?,?)", ("old-"+table,old))
    c.execute(f"INSERT INTO {table}(id,{payload_col},{time_col}) VALUES(2,?,?)", ("current-"+table,now))
c.execute("INSERT INTO usage_records VALUES(?,?,?)", (1,large,old))
c.execute("INSERT INTO usage_records VALUES(?,?,?)", (2,'{"current":true}',now))
c.execute("INSERT INTO billing_holds VALUES(?,?,?)", ("old-terminal","settled",old))
c.execute("INSERT INTO billing_holds VALUES(?,?,?)", ("active-hold","held",old))
c.execute("INSERT INTO usage_events VALUES(?,?,?,?)", ("old-event","old-terminal","old",old))
c.execute("INSERT INTO usage_events VALUES(?,?,?,?)", ("active-event","active-hold","active",old))
c.execute("INSERT INTO usage_events VALUES(?,?,?,?)", ("current-event","","current",now))
c.execute("INSERT INTO diagnostic_jobs VALUES(?,?)", ("diag-1","ready"))
c.execute("INSERT INTO diagnostic_download_leases VALUES(?,?)", ("lease-1","diag-1"))
c.execute("INSERT INTO diagnostic_events VALUES(?,?)", ("event-1",'{"diagnostic":true}'))
c.execute("INSERT INTO storage_resources VALUES(?,?,?)", ("resource-1","diagnostic_artifact","diagnostic_artifact_24h"))
c.execute("INSERT INTO affinity_aliases VALUES(?,?)", ("expired",old))
c.execute("INSERT INTO affinity_bindings VALUES(?,?,?)", ("stale",0,old-20*86400))
c.execute("INSERT INTO user_group_target_bindings VALUES(?,?,?,?)", ("ug","key","gpt",old-20*86400))
c.execute("INSERT INTO antigravity_cache_entries VALUES(?,?)", (1,old))
c.execute("INSERT INTO kiro_model_catalog VALUES(?,?,?,?)", ("acc-1","cap","up",old))
c.execute("INSERT INTO kiro_probe_state VALUES(?,?,?)", ("acc-1","cap",old))
c.execute("INSERT INTO kiro_runtime_capabilities VALUES(?,?,?,?)", ("acc-1","endpoint","model",old-20*86400))
c.execute("INSERT INTO account_model_capabilities VALUES(?,?,?)", ("acc-1","stale-model",old))
c.execute("INSERT INTO account_model_catalog_status VALUES(?,?)", ("acc-1",old))
c.execute("INSERT INTO user_sessions VALUES(?,?)", ("expired-session",old))
c.execute("INSERT INTO maintenance_leases VALUES(?,?)", ("expired-lease",old))
c.execute("INSERT INTO account_codex_reauth_jobs VALUES(?,?,?)", (1,"completed",old))
c.execute("INSERT INTO codex_upstream_attempt VALUES(?,?,?)", (1,old,"expired"))
c.execute("INSERT INTO codex_upstream_attempt_daily VALUES(?,?,?,?,?,?)", (old,"acc-1","egress-1","ok",200,old))
c.commit()
c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
c.close()

config = {
    "data_dir": str(data),
    "database_path": str(db),
    "storage_driver": "sqlite",
    "goal_continuity_enabled": True,
    "goal_legacy_journal_dual_write": True,
    "goal_retention_days": 7,
    "codex_session_mapping_retention_days": 7,
}
(root / "config.json").write_text(json.dumps(config, indent=2) + "\n")

for path, size, age in [
    (data / "diagnostics" / "bundle.zip", 1 << 20, old),
    (data / "spool" / "stale.body", 1 << 20, old),
    (data / "tmp" / "browser" / "stale.profile", 1 << 20, old),
    (db.parent / ".diagnostic-snapshot-stale.sqlite3", 1 << 20, old),
    (data / "spool" / "current.body", 32, now),
    (data / "logs" / "old.log", 1 << 20, old),
]:
    path.write_bytes((b"Z" * size))
    os.utime(path, (age, age))

def encode(v):
    return {"b64":base64.b64encode(v).decode()} if isinstance(v,bytes) else v
def table_hash(conn, names):
    h=hashlib.sha256(); total=0
    existing={r[0] for r in conn.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    for name in names:
        if name not in existing: continue
        info=list(conn.execute(f'PRAGMA table_info("{name}")'))
        cols=[r[1] for r in info]
        pk=[r[1] for r in sorted([r for r in info if r[5]],key=lambda r:r[5])] or cols
        h.update((name+"|"+json.dumps(cols)+"\n").encode())
        for row in conn.execute(f'SELECT * FROM "{name}" ORDER BY '+",".join('"'+x+'"' for x in pk)):
            h.update((json.dumps([encode(x) for x in row],sort_keys=True,separators=(",",":"))+"\n").encode())
            total+=1
    return {"rows":total,"sha256":h.hexdigest()}
account_tables=["accounts","account_auth_tokens","account_session_cookies","account_egress_bindings","account_group_memberships","account_lifecycle_status","account_rate_limits","codex_reset_credit_consumptions"]
context_tables=["context_journal","virtual_context_ledger","goal_session","goal_alias","goal_checkpoint","goal_segment","goal_payload_chunk","goal_run","codex_session_binding","codex_session_alias","codex_instruction_snapshot"]
all_tables=[r[0] for r in sqlite3.connect(db).execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
conn=sqlite3.connect(db)
baseline={
  "accounts":table_hash(conn,account_tables),
  "contexts":table_hash(conn,context_tables),
  "all_db":table_hash(conn,all_tables),
  "database_bytes":db.stat().st_size,
  "database_sha256":hashlib.sha256(db.read_bytes()).hexdigest(),
  "config_sha256":hashlib.sha256((root/"config.json").read_bytes()).hexdigest(),
}
conn.close()
(root/"baseline.json").write_text(json.dumps(baseline,indent=2,sort_keys=True)+"\n")
print(json.dumps(baseline,indent=2,sort_keys=True))
PY

run_logged "script sha256" "${LOW[@]}" sha256sum "$SCRIPT"
run_logged "bash static syntax" "${LOW[@]}" bash -n "$SCRIPT"
run_logged "help contract" "${LOW[@]}" bash "$SCRIPT" --help

run_logged "capture pre-dry-run state" "${LOW[@]}" python3 - "$CASE" <<'PY'
import hashlib,json,sys
from pathlib import Path
r=Path(sys.argv[1])
items=[]
for p in sorted(r.rglob("*")):
    if p.is_file() and "dry-run" not in p.name and "apply" not in p.name:
        items.append((str(p.relative_to(r)),hashlib.sha256(p.read_bytes()).hexdigest()))
(r/"pre-dry-state.json").write_text(json.dumps(items,sort_keys=True))
print(hashlib.sha256(json.dumps(items,sort_keys=True).encode()).hexdigest())
PY

run_logged "default dry-run" "${LOW[@]}" bash "$SCRIPT" \
  --config "$CASE/config.json" --backup-dir "$CASE/backups" \
  --retention-days 7 --stale-file-hours 1 --optimize-config

run_logged "verify dry-run made zero changes" "${LOW[@]}" python3 - "$CASE" <<'PY'
import hashlib,json,sys
from pathlib import Path
r=Path(sys.argv[1])
before=json.loads((r/"pre-dry-state.json").read_text())
excluded={"pre-dry-state.json"}
after=[]
for p in sorted(r.rglob("*")):
    if p.is_file() and p.name not in excluded and "dry-run" not in p.name and "apply" not in p.name:
        after.append([str(p.relative_to(r)),hashlib.sha256(p.read_bytes()).hexdigest()])
print(json.dumps({"before_files":len(before),"after_files":len(after),"exact_match":before==after},sort_keys=True))
if before != after:
    print("BEFORE_ONLY",sorted(set(map(tuple,before))-set(map(tuple,after))))
    print("AFTER_ONLY",sorted(set(map(tuple,after))-set(map(tuple,before))))
    raise SystemExit(1)
PY

run_logged "apply reclamation" "${LOW[@]}" bash "$SCRIPT" \
  --apply --config "$CASE/config.json" --backup-dir "$CASE/backups" \
  --retention-days 7 --stale-file-hours 1 --optimize-config \
  --assume-quiesced

run_logged "verify apply invariants archives files config and vacuum" "${LOW[@]}" python3 - "$CASE" <<'PY'
import base64,gzip,hashlib,json,sqlite3,sys
from pathlib import Path
r=Path(sys.argv[1]); db=r/"pool.sqlite3"; data=r/"data"
baseline=json.loads((r/"baseline.json").read_text())
reports=sorted((r/"backups").glob("reclaim-*/verification.json"))
assert len(reports)==1,reports
report=json.loads(reports[0].read_text())
assert report["verified"]["accounts_exact_match"]
assert report["verified"]["contexts_exact_match"]
assert report["verified"]["retained_logs_exact_match"]
assert report["verified"]["historical_logs_archived_before_delete"]
assert report["accounts_after"]["sha256"]==report["accounts_before"]["sha256"]
assert report["contexts_after"]["sha256"]==report["contexts_before"]["sha256"]
assert report["logs_after"]["sha256"]==report["logs_retained_before"]["sha256"]
assert report["database_bytes_after"] < baseline["database_bytes"],(report["database_bytes_after"],baseline["database_bytes"])
cfg=json.loads((r/"config.json").read_text())
assert cfg["goal_legacy_journal_dual_write"] is False
assert report["legacy_dual_write_coverage"]["can_disable"] is True
conn=sqlite3.connect(db)
assert conn.execute("PRAGMA quick_check").fetchone()[0]=="ok"
assert conn.execute("SELECT count(*) FROM accounts").fetchone()[0]==1
assert conn.execute("SELECT access_token FROM account_auth_tokens").fetchone()[0]=="access-secret"
assert conn.execute("SELECT cookie FROM account_session_cookies").fetchone()[0]=="session-cookie"
assert conn.execute("SELECT count(*) FROM context_journal").fetchone()[0]==1
assert conn.execute("SELECT count(*) FROM goal_payload_chunk").fetchone()[0]==2
assert conn.execute("SELECT count(*) FROM virtual_context_ledger").fetchone()[0]==1
assert conn.execute("SELECT count(*) FROM usage_records WHERE created_at < strftime('%s','now')-7*86400").fetchone()[0]==0
assert conn.execute("SELECT count(*) FROM usage_records").fetchone()[0]==1
assert conn.execute("SELECT count(*) FROM billing_holds WHERE status='held'").fetchone()[0]==1
assert conn.execute("SELECT count(*) FROM usage_events WHERE event_id='active-event'").fetchone()[0]==1
assert conn.execute("SELECT count(*) FROM diagnostic_jobs").fetchone()[0]==0
assert conn.execute("SELECT value FROM settings WHERE key='goal_legacy_journal_dual_write'").fetchone()[0]=="false"
conn.close()
archives=list(reports[0].parent.glob("history/*.jsonl.gz"))
assert archives
archive_rows=0
for path in archives:
    with gzip.open(path,"rt",encoding="utf-8") as f:
        header=json.loads(f.readline())
        assert header["format"]=="codex-pool-maintenance-jsonl-v1"
        for line in f:
            json.loads(line); archive_rows+=1
assert archive_rows==report["archived_rows"],(archive_rows,report["archived_rows"])
assert not (data/"diagnostics"/"bundle.zip").exists()
assert not (data/"spool"/"stale.body").exists()
assert (data/"spool"/"current.body").exists()
assert not (data/"tmp"/"browser"/"stale.profile").exists()
assert not (r/".diagnostic-snapshot-stale.sqlite3").exists()
assert not (data/"logs"/"old.log").exists()
assert (data/"logs"/"old.log.gz").exists()
backup=Path(report["rollback_backup"])
assert backup.exists()
with gzip.open(backup,"rb") as f:
    assert f.read(16).startswith(b"SQLite format 3")
result={
 "verification":str(reports[0]),
 "rollback_backup":str(backup),
 "database_bytes_before":baseline["database_bytes"],
 "database_bytes_after":report["database_bytes_after"],
 "archive_files":len(archives),
 "archive_rows":archive_rows,
 "accounts_sha256":report["accounts_after"]["sha256"],
 "contexts_sha256":report["contexts_after"]["sha256"],
 "retained_logs_sha256":report["logs_after"]["sha256"],
 "sqlite_quick_check":"ok",
 "dual_write_disabled_after_coverage_proof":True,
}
(r/"apply-result.json").write_text(json.dumps(result,indent=2,sort_keys=True)+"\n")
print(json.dumps(result,indent=2,sort_keys=True))
PY

ROLLBACK=$("${LOW[@]}" python3 - "$CASE" <<'PY'
import json,sys
from pathlib import Path
r=Path(sys.argv[1])
report=json.loads(next((r/"backups").glob("reclaim-*/verification.json")).read_text())
print(report["rollback_backup"])
PY
)
run_logged "rollback full SQLite and config" "${LOW[@]}" bash "$SCRIPT" \
  --apply --config "$CASE/config.json" --backup-dir "$CASE/backups" \
  --rollback "$ROLLBACK" --assume-quiesced

run_logged "verify rollback restored original logical hash and rows" "${LOW[@]}" python3 - "$CASE" <<'PY'
import base64,hashlib,json,sqlite3,sys
from pathlib import Path
r=Path(sys.argv[1]); db=r/"pool.sqlite3"
baseline=json.loads((r/"baseline.json").read_text())
def enc(v):
    return {"b64":base64.b64encode(v).decode()} if isinstance(v,bytes) else v
conn=sqlite3.connect(db)
names=[x[0] for x in conn.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")]
h=hashlib.sha256(); total=0
for name in names:
    info=list(conn.execute(f'PRAGMA table_info("{name}")')); cols=[x[1] for x in info]
    pk=[x[1] for x in sorted([x for x in info if x[5]],key=lambda x:x[5])] or cols
    h.update((name+"|"+json.dumps(cols)+"\n").encode())
    for row in conn.execute(f'SELECT * FROM "{name}" ORDER BY '+",".join('"'+x+'"' for x in pk)):
        h.update((json.dumps([enc(x) for x in row],sort_keys=True,separators=(",",":"))+"\n").encode());total+=1
quick=conn.execute("PRAGMA quick_check").fetchone()[0]
old_usage=conn.execute("SELECT count(*) FROM usage_records WHERE id=1").fetchone()[0]
diagnostics=conn.execute("SELECT count(*) FROM diagnostic_jobs").fetchone()[0]
account=conn.execute("SELECT count(*) FROM accounts").fetchone()[0]
context=conn.execute("SELECT count(*) FROM context_journal").fetchone()[0]
conn.close()
cfg_sha=hashlib.sha256((r/"config.json").read_bytes()).hexdigest()
result={"rows":total,"sha256":h.hexdigest(),"expected":baseline["all_db"],"quick_check":quick,
        "old_usage_rows":old_usage,"diagnostic_jobs":diagnostics,"accounts":account,"contexts":context,
        "config_sha256":cfg_sha,"expected_config_sha256":baseline["config_sha256"]}
print(json.dumps(result,indent=2,sort_keys=True))
assert total==baseline["all_db"]["rows"]
assert h.hexdigest()==baseline["all_db"]["sha256"]
assert quick=="ok" and old_usage==1 and diagnostics==1 and account==1 and context==1
assert cfg_sha==baseline["config_sha256"]
PY

run_logged "verification artifacts hashes" "${LOW[@]}" find "$CASE/backups" -type f -maxdepth 4 -print
run_logged "remote log sha256" "${LOW[@]}" sha256sum "$LOG"
printf '\nALL_REMOTE_DISK_RECLAIM_TESTS_PASSED\n'
