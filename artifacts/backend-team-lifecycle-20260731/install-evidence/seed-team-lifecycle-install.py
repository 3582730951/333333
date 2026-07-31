#!/usr/bin/env python3
import json
import os
import sqlite3
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(os.environ["TEAM_FIXTURE_ROOT"])
DATABASE_PATH = Path(os.environ.get("TEAM_FIXTURE_DATABASE", ROOT / "data/pool.sqlite3"))
BASE = os.environ["TEAM_FIXTURE_BASE_URL"].rstrip("/")
TOKEN = Path(os.environ.get("TEAM_FIXTURE_TOKEN_FILE", ROOT / "records/admin.token")).read_text().strip()
EVIDENCE_PATH = Path(
    os.environ.get(
        "TEAM_FIXTURE_EVIDENCE",
        ROOT / "records/team-lifecycle-api-verification.json",
    )
)


def request(method, path, payload=None, idempotency_key=""):
    raw = None if payload is None else json.dumps(payload).encode()
    headers = {"Authorization": f"Bearer {TOKEN}"}
    if raw is not None:
        headers["Content-Type"] = "application/json"
    if idempotency_key:
        headers["Idempotency-Key"] = idempotency_key
    req = urllib.request.Request(BASE + path, data=raw, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=20) as response:
            return response.status, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:
        body = error.read()
        raise RuntimeError(f"{method} {path}: HTTP {error.code} {body[:300]!r}") from error


workspace_payload = {
    "id": "teamws-apple-lifecycle-demo",
    "name": "Apple Lifecycle Room · Cloud fixture",
    "parent_account_id": "parent-account-reference-cloud-fixture",
    "workspace_ref": "workspace-reference-cloud-fixture",
    "connector_kind": "fixture",
    "max_members": 12,
    "status": "active",
}
workspace_status, workspace = request(
    "POST", "/admin/team-lifecycle/workspaces", workspace_payload
)
if workspace_status != 201:
    raise RuntimeError(f"workspace status {workspace_status}")

shadow_status, shadow_result = request(
    "POST",
    "/admin/team-lifecycle/workflows",
    {
        "workspace_id": workspace["id"],
        "parent_account_id": workspace["parent_account_id"],
        "child_account_id": "shadow-child-account-reference",
        "replacement_method": "protocol_v2",
        "rotate_threshold_percent": 1,
        "max_attempts": 5,
        "shadow_mode": True,
    },
    "cloud-shadow-plan-v1",
)
if shadow_status != 201:
    raise RuntimeError(f"shadow workflow status {shadow_status}")
shadow_id = shadow_result["workflow"]["id"]

shadow = None
for _ in range(30):
    _, shadow = request("GET", f"/admin/team-lifecycle/workflows/{shadow_id}")
    if shadow["state"] == "review_required":
        break
    time.sleep(0.25)
if shadow is None or shadow["state"] != "review_required":
    raise RuntimeError(f"shadow workflow did not settle: {shadow}")
if shadow["error_class"] != "shadow_plan_ready":
    raise RuntimeError(f"unexpected shadow result: {shadow}")

now = int(time.time())
rows = [
    {
        "id": "teamwf-active-cloud-fixture",
        "key": "cloud-active-cycle-v1",
        "child": "child-account-with-an-extremely-long-reference-for-responsive-layout-cloud-fixture",
        "state": "active",
        "resume": "",
        "path": "access_reference",
        "membership": "membership-reference-active",
        "credential": "account_auth_tokens:child-cloud-active",
        "challenge": "",
        "imported": "account-cloud-active",
        "method": "protocol_v2",
        "replacement": "",
        "quota": 75,
        "attempt": 0,
        "next": now + 86400,
        "error": "",
        "shadow": 0,
        "completed": 0,
    },
    {
        "id": "teamwf-retry-cloud-fixture",
        "key": "cloud-retry-cycle-v1",
        "child": "child-account-oauth-retry-reference",
        "state": "retry_wait",
        "resume": "oauth_login",
        "path": "oauth",
        "membership": "membership-reference-retry",
        "credential": "",
        "challenge": "",
        "imported": "",
        "method": "browser_v3",
        "replacement": "",
        "quota": -1,
        "attempt": 2,
        "next": now + 86400,
        "error": "connector_timeout",
        "shadow": 0,
        "completed": 0,
    },
    {
        "id": "teamwf-completed-cloud-fixture",
        "key": "cloud-completed-cycle-v1",
        "child": "child-account-rotated-reference",
        "state": "completed",
        "resume": "",
        "path": "oauth",
        "membership": "membership-reference-completed",
        "credential": "account_auth_tokens:child-cloud-completed",
        "challenge": "challenge-reference-completed",
        "imported": "account-cloud-completed",
        "method": "browser_v3",
        "replacement": "registration-job-reference-completed",
        "quota": 100,
        "attempt": 0,
        "next": 0,
        "error": "",
        "shadow": 0,
        "completed": now - 30,
    },
]

database = sqlite3.connect(DATABASE_PATH, timeout=20)
database.execute("PRAGMA busy_timeout=20000")
with database:
    for row in rows:
        database.execute(
            """
INSERT OR REPLACE INTO team_lifecycle_workflows(
 id,idempotency_key,workspace_id,parent_account_id,child_account_id,state,resume_state,
 credential_path,membership_ref,credential_ref,phone_challenge_ref,imported_account_id,
 replacement_method,replacement_job_ref,quota_remaining_bps,rotate_threshold_bps,
 attempt,max_attempts,next_attempt_at,lease_owner,lease_expires_at,error_class,
 shadow_mode,version,created_at,updated_at,completed_at
) VALUES(
 :id,:key,:workspace,:parent,:child,:state,:resume,
 :path,:membership,:credential,:challenge,:imported,
 :method,:replacement,:quota,100,
 :attempt,5,:next,'',0,:error,
 :shadow,7,:created,:updated,:completed
)""",
            {
                **row,
                "workspace": workspace["id"],
                "parent": workspace["parent_account_id"],
                "created": now - 120,
                "updated": now - 10,
            },
        )
        database.execute(
            """
INSERT OR REPLACE INTO team_lifecycle_events(
 workflow_id,sequence,from_state,to_state,event_type,detail_json,created_at
) VALUES(?,1,'',?,'fixture_seed','{}',?)""",
            (row["id"], row["state"], now - 10),
        )
database.close()

_, workflows_payload = request(
    "GET", "/admin/team-lifecycle/workflows?workspace_id=teamws-apple-lifecycle-demo&limit=20"
)
_, stats = request("GET", "/admin/team-lifecycle/stats")
_, events = request(
    "GET", f"/admin/team-lifecycle/workflows/{shadow_id}/events?limit=20"
)
workflows = workflows_payload["items"]
states = {item["state"] for item in workflows}
required_states = {"active", "retry_wait", "review_required", "completed"}
if not required_states.issubset(states):
    raise RuntimeError(f"missing fixture states: {states}")
if stats.get("credential_persistence") != "encrypted_account_reference":
    raise RuntimeError(f"unexpected credential storage summary: {stats}")
if not stats.get("lease_heartbeat"):
    raise RuntimeError(f"lease heartbeat not reported: {stats}")

evidence = {
    "verified_at": int(time.time()),
    "release_id": json.loads(
        urllib.request.urlopen(BASE + "/readyz", timeout=10).read()
    ).get("release_id"),
    "workspace_status": workspace_status,
    "shadow_workflow_status": shadow_status,
    "shadow_final_state": shadow["state"],
    "shadow_error_class": shadow["error_class"],
    "workflow_count": len(workflows),
    "states": sorted(states),
    "state_counts": stats["states"],
    "credential_persistence": stats["credential_persistence"],
    "lease_heartbeat": stats["lease_heartbeat"],
    "rotation_threshold_bps": stats["rotation_threshold_bps"],
    "shadow_event_types": [item["event_type"] for item in events["items"]],
    "raw_secret_fields_in_workflow_response": [
        key
        for item in workflows
        for key in item
        if key in {"access_token", "refresh_token", "password", "phone_number"}
    ],
}
if evidence["raw_secret_fields_in_workflow_response"]:
    raise RuntimeError("workflow API exposed a raw secret field")
EVIDENCE_PATH.parent.mkdir(parents=True, exist_ok=True)
EVIDENCE_PATH.write_text(
    json.dumps(evidence, indent=2) + "\n"
)
print(
    "TEAM_LIFECYCLE_SEEDED=1 "
    f"workflows={len(workflows)} states={','.join(sorted(states))}"
)
