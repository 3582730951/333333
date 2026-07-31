#!/usr/bin/env python3
"""Populate the final native install directly through SQLite with UI review data."""

from __future__ import annotations

import hashlib
import json
import os
import sqlite3
import time
from pathlib import Path


ROOT = Path(os.environ.get("ROOT", "/root/autodl-tmp/backend-mail-automation-install-20260731"))
DB = Path(os.environ.get("DATABASE_PATH", ROOT / "data" / "pool.sqlite3"))
BACKUPS = ROOT / "backups"
RECORDS = ROOT / "records"
NOW = int(time.time())

BACKUPS.mkdir(parents=True, exist_ok=True)
RECORDS.mkdir(parents=True, exist_ok=True)


def columns(conn: sqlite3.Connection, table: str) -> set[str]:
    return {row[1] for row in conn.execute(f'PRAGMA table_info("{table}")')}


def put(conn: sqlite3.Connection, table: str, values: dict, *, replace: bool = True) -> None:
    available = columns(conn, table)
    if not available:
        raise RuntimeError(f"required table is missing: {table}")
    filtered = {key: value for key, value in values.items() if key in available}
    names = ", ".join(f'"{key}"' for key in filtered)
    marks = ", ".join("?" for _ in filtered)
    verb = "INSERT OR REPLACE" if replace else "INSERT"
    conn.execute(
        f'{verb} INTO "{table}" ({names}) VALUES ({marks})',
        tuple(filtered.values()),
    )


def backup(conn: sqlite3.Connection, destination: Path) -> None:
    if destination.exists():
        destination.unlink()
    with sqlite3.connect(destination) as target:
        conn.backup(target)


conn = sqlite3.connect(DB, timeout=30)
conn.execute("PRAGMA foreign_keys=ON")
conn.execute("PRAGMA busy_timeout=30000")
conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
before_backup = BACKUPS / "before-direct-seed.sqlite3"
if not before_backup.exists():
    backup(conn, before_backup)

groups = [
    "核心生产 · 北美高容量",
    "产品设计 · Apple 体验复核",
    "生命周期自动化 · 同域邮箱轮换",
    "研究与分析 · 超长工作组名称用于响应式布局压力验证",
]
for index, name in enumerate(groups):
    put(conn, "groups", {
        "name": name,
        "system_prompt": "",
        "prompt_mode": "prepend",
        "created_at": NOW - 86400 * (30 - index),
        "updated_at": NOW - index * 1200,
    })

account_ids: list[str] = []
providers = ["codex", "claude", "kiro", "antigravity"]
statuses = ["active", "active", "active", "quarantined", "disabled"]
plans = ["team", "pro", "business", "free"]
for index in range(16):
    account_id = f"final-apple-account-{index + 1:02d}"
    account_ids.append(account_id)
    provider = providers[index % len(providers)]
    status = statuses[index % len(statuses)]
    local = (
        f"child-{index + 1:02d}-same-domain-lifecycle-automation-"
        f"with-an-extremely-long-identity"
    )
    put(conn, "accounts", {
        "id": account_id,
        "label": (
            f"Apple 数据可视化与自动注册验证账号 {index + 1:02d} · "
            f"very-long-account-name-for-responsive-layout-{index + 1:02d}"
        ),
        "group_name": groups[index % len(groups)],
        "upstream_account_id": f"workspace-member-reference-{index + 1:02d}",
        "chatgpt_user_id": f"fixture-user-{index + 1:02d}",
        "email": f"{local}@automation-mail.example.test",
        "plan_type": plans[index % len(plans)],
        "provider": provider,
        "status": status,
        "quarantine_until": NOW + 7200 if status == "quarantined" else 0,
        "quarantine_reason": "scheduled fixture review" if status == "quarantined" else "",
        "created_at": NOW - 86400 * (20 - index),
        "updated_at": NOW - index * 180,
    })
    used = [0.75, 9.5, 23.0, 47.5, 71.0, 88.0][index % 6]
    put(conn, "account_rate_limits", {
        "account_id": account_id,
        "provider": provider,
        "model": "gpt-5.2-codex",
        "limiter_type": "primary",
        "source": "direct_sql_final_fixture",
        "used_percent": used,
        "limit_tokens": 1_000_000,
        "remaining_tokens": int(1_000_000 * (100 - used) / 100),
        "limit_requests": 10_000,
        "remaining_requests": int(10_000 * (100 - used) / 100),
        "reset_at": NOW + 3600 * (index + 1),
        "status": "healthy" if status == "active" else status,
        "raw_json": "{}",
        "updated_at": NOW - index * 60,
    })
    put(conn, "account_model_capabilities", {
        "account_id": account_id,
        "model_slug": "gpt-5.2-codex",
        "availability_state": "available" if status == "active" else "unverified",
        "context_1m_state": "supported",
        "context_1m_source": "direct_sql_final_fixture",
        "native_context_window": 200_000,
        "native_max_context_window": 1_000_000,
        "effective_context_window_percent": 100 - index,
        "auto_compact_token_limit": 180_000,
        "visibility": "public",
        "source": "direct_sql_final_fixture",
        "last_probe_at": NOW - index * 90,
    })

models = ["gpt-5.2-codex", "claude-sonnet-4-5", "gemini-2.5-pro", "kiro-auto"]
for index in range(288):
    prompt = 900 + (index * 137) % 12_000
    completion = 210 + (index * 79) % 3_600
    cached = int(prompt * ((index % 9) / 10))
    put(conn, "usage_records", {
        "usage_event_id": f"final-fixture-usage-{index + 1:04d}",
        "account_id": account_ids[index % len(account_ids)],
        "route_key_hash": f"fixture-route-{index % 29:02d}",
        "model": models[index % len(models)],
        "prompt_tokens": prompt,
        "completion_tokens": completion,
        "total_tokens": prompt + completion,
        "cached_tokens": cached,
        "cache_read_tokens": cached,
        "cache_creation_tokens": max(prompt - cached, 0),
        "usage_provider": providers[index % len(providers)],
        "usage_source": "direct_sql_final_fixture",
        "cache_read_present": 1,
        "cache_creation_present": 1,
        "cache_capability": "verified",
        "estimated": 0,
        "cache_miss_tokens": max(prompt - cached, 0),
        "cache_total_input_tokens": prompt,
        "requested_model": models[index % len(models)],
        "resolved_model": models[index % len(models)],
        "model_override_source": "none",
        "raw_usage_json": "{}",
        # Keep every fixture inside the server's default seven-day retention
        # window so the post-start cleanup pass leaves the visual series intact.
        "created_at": NOW - (index % (24 * 6)) * 3600,
    })

for index in range(20):
    status = ["idle", "in_use", "used", "error"][index % 4]
    put(conn, "email_pool", {
        "id": f"final-email-{index + 1:02d}",
        "email": (
            f"apple-layout-long-mailbox-{index + 1:02d}-registration-and-"
            f"lifecycle-observability@automation-mail.example.test"
        ),
        "password": "",
        "client_id": "",
        "refresh_token": "",
        "status": status,
        "group_name": groups[index % len(groups)],
        "error_message": "bounded retry is scheduled" if status == "error" else "",
        "last_used_at": NOW - index * 420,
        "created_at": NOW - 86400 * (index + 1),
        "updated_at": NOW - index * 240,
    })

mailbox_profiles = [
    ("cf_automation_primary", "Cloudflare · 生产同域邮箱", "https://mail-primary.example.test", "automation-mail.example.test", 100, "healthy", 84),
    ("cf_automation_backup", "Cloudflare · 灾备同域邮箱（超长名称验证）", "https://mail-backup.example.test", "backup-mail.example.test", 80, "healthy", 137),
    ("cf_automation_review", "Cloudflare · 配置待复核", "https://mail-review.example.test", "review-mail.example.test", 60, "unhealthy", 1200),
]
for index, (key, name, api_url, domain, priority, health, latency) in enumerate(mailbox_profiles):
    put(conn, "provider_settings", {
        "id": f"provider-{key}",
        "provider_type": "mailbox",
        "provider_key": key,
        "display_name": name,
        "enabled": 0 if health == "unhealthy" else 1,
        "priority": priority,
        "config_json": json.dumps({
            "adapter": "cloudflare_temp_email",
            "api_url": api_url,
            "domain": domain,
            "custom_domain": True,
            "same_domain_capable": True,
            "compatibility": "dreamhunter-v1",
        }, separators=(",", ":")),
        "auth_json": "{}",
        "created_at": NOW - 86400 * (10 - index),
        "updated_at": NOW - index * 1800,
    })
    put(conn, "mailbox_provider_health", {
        "provider_key": key,
        "last_status": health,
        "last_checked_at": NOW - index * 600,
        "latency_ms": latency,
        "success_count": 120 - index * 20,
        "failure_count": index * 3,
        "consecutive_failures": 4 if health == "unhealthy" else 0,
        "last_error_class": "timeout" if health == "unhealthy" else "",
        "updated_at": NOW - index * 600,
    })

for key, value in {
    "reg_default_mailbox": "cf_automation_primary",
    "team_default_mailbox_provider": "cf_automation_primary",
    "team_default_mailbox_domain": "automation-mail.example.test",
    "final_fixture_marker": "backend-mail-automation-20260731",
    "final_fixture_seed_method": "direct_sqlite",
}.items():
    put(conn, "settings", {"key": key, "value": value, "updated_at": NOW})

workspaces = [
    ("team-workspace-primary", "主团队房间 · 同域自动轮换", account_ids[0], "remote-team-primary", 10),
    ("team-workspace-design", "Apple UI 与额度可视化复核团队房间", account_ids[4], "remote-team-design", 8),
]
for workspace_id, name, parent_id, remote_ref, capacity in workspaces:
    put(conn, "team_workspaces", {
        "id": workspace_id,
        "name": name,
        "parent_account_id": parent_id,
        "workspace_id": remote_ref,
        "workspace_type": "native_chatgpt",
        "max_members": capacity,
        "status": "active",
        "mailbox_provider_key": "cf_automation_primary",
        "required_email_domain": "automation-mail.example.test",
        "same_domain_required": 1,
        "created_at": NOW - 86400 * 8,
        "updated_at": NOW - 300,
    })

states = [
    ("active", "personal_access_token", 7500, 0, ""),
    ("active", "codex_oauth", 75, 0, ""),
    ("retry_wait", "codex_oauth", 980, 2, "upstream_transient"),
    ("review_required", "personal_access_token", 100, 1, ""),
    ("completed", "codex_oauth", 0, 0, ""),
    ("queued", "", -1, 0, ""),
    ("oauth_login", "", -1, 1, ""),
    ("phone_verification", "codex_oauth", -1, 1, ""),
]
for index, (state, credential, quota, attempt, error_class) in enumerate(states):
    workspace_id = workspaces[index % len(workspaces)][0]
    workflow_id = f"teamwf-final-{index + 1:02d}"
    child_id = account_ids[index + 1]
    put(conn, "team_members", {
        "id": f"member-final-{index + 1:02d}",
        "workspace_id": workspace_id,
        "account_id": child_id,
        "identity_ref": f"member-reference-with-a-very-long-opaque-value-{index + 1:02d}",
        "display_label": f"自动轮换子账号 {index + 1:02d}",
        "invite_status": "active" if state not in ("queued", "oauth_login") else "pending",
        "quota_remaining_bps": quota,
        "last_activity_at": NOW - index * 400,
        "last_quota_check_at": NOW - index * 300,
        "added_at": NOW - 86400 * (7 - index // 2),
        "removed_at": 0,
    })
    put(conn, "child_account_pool", {
        "id": f"child-pool-final-{index + 1:02d}",
        "account_id": child_id,
        "identity_ref": f"child-account-with-an-extremely-long-reference-{index + 1:02d}",
        "display_label": f"同域邮箱子账号 {index + 1:02d}",
        "status": "in_use" if state == "active" else "available",
        "last_used_at": NOW - index * 500,
        "use_count": index + 1,
        "failure_count": attempt,
        "created_at": NOW - 86400 * 6,
        "updated_at": NOW - index * 200,
    })
    put(conn, "team_lifecycle_workflows", {
        "id": workflow_id,
        "idempotency_key": f"fixture-team-cycle-{index + 1:02d}",
        "workspace_id": workspace_id,
        "parent_account_id": workspaces[index % len(workspaces)][2],
        "child_account_id": (
            f"child-account-with-an-extremely-long-reference-{index + 1:02d}-"
            "for-responsive-layout-verification"
        ),
        "state": state,
        "resume_state": "oauth_login" if state == "retry_wait" else "",
        "credential_path": credential,
        "membership_ref": f"membership-ref-{index + 1:02d}",
        "credential_ref": f"encrypted-account-reference-{index + 1:02d}",
        "phone_challenge_ref": "",
        "imported_account_id": child_id if credential else "",
        "replacement_method": "protocol_v2" if index % 2 == 0 else "browser_v3",
        "replacement_job_ref": "",
        "mailbox_provider_key": "cf_automation_primary",
        "required_email_domain": "automation-mail.example.test",
        "quota_remaining_bps": quota,
        "rotate_threshold_bps": 100,
        "attempt": attempt,
        "max_attempts": 5,
        "next_attempt_at": NOW + 86400 + index * 600,
        "lease_owner": "",
        "lease_expires_at": 0,
        "error_class": error_class,
        "shadow_mode": 0 if state in ("active", "queued") else 1,
        "version": index + 3,
        "created_at": NOW - 86400 * 5 + index * 120,
        "updated_at": NOW - index * 120,
        "completed_at": NOW - 600 if state == "completed" else 0,
    })
    for sequence, (from_state, to_state, event_type) in enumerate([
        ("", "queued", "created"),
        ("queued", "inviting", "transition"),
        ("inviting", state, "fixture_checkpoint"),
    ], 1):
        put(conn, "team_lifecycle_events", {
            "workflow_id": workflow_id,
            "sequence": sequence,
            "from_state": from_state,
            "to_state": to_state,
            "event_type": event_type,
            "detail_json": json.dumps({"fixture": True, "sequence": sequence}, separators=(",", ":")),
            "created_at": NOW - 3600 + sequence * 90 + index,
        })

for index in range(8):
    job_id = f"final-registration-job-{index + 1:02d}"
    put(conn, "registration_jobs", {
        "id": job_id,
        "platform": "chatgpt",
        "method": ["protocol_v2", "browser_v3"][index % 2],
        "total": 14 + index,
        "succeeded": 12 + index,
        "failed": index % 3,
        "status": "completed",
        "config_json": '{"fixture":true,"plan":"free"}',
        "started_at": NOW - 10800 - index * 600,
        "completed_at": NOW - 9800 - index * 600,
        "created_at": NOW - 11000 - index * 600,
        "updated_at": NOW - 9800 - index * 600,
    })

conn.execute("DELETE FROM audit_log WHERE reason = ?", ("direct_sql_final_fixture",))
for index in range(56):
    put(conn, "audit_log", {
        "id": -(10_000 + index),
        "account_id": account_ids[index % len(account_ids)],
        "account_label": f"自动化审计账号 {index % len(account_ids) + 1:02d}",
        "action": ["registration", "team_invite", "quota_check", "member_rotation"][index % 4],
        "state": ["success", "success", "healthy", "queued"][index % 4],
        "reason": "direct_sql_final_fixture",
        "detail": json.dumps({"fixture": True, "sequence": index + 1}, separators=(",", ":")),
        "created_at": NOW - index * 180,
    })

conn.commit()
conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
integrity = conn.execute("PRAGMA integrity_check").fetchone()[0]
tracked = [
    "accounts", "account_rate_limits", "usage_records", "email_pool",
    "provider_settings", "mailbox_provider_health", "team_workspaces",
    "team_members", "child_account_pool", "team_lifecycle_workflows",
    "team_lifecycle_events", "registration_jobs", "audit_log",
]
counts = {table: conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] for table in tracked}
logical = {
    "accounts": [list(row) for row in conn.execute("SELECT id,label,email,status FROM accounts ORDER BY id")],
    "emails": [list(row) for row in conn.execute("SELECT id,email,status FROM email_pool ORDER BY id")],
    "workflows": [list(row) for row in conn.execute(
        "SELECT id,state,credential_path,quota_remaining_bps FROM team_lifecycle_workflows ORDER BY id"
    )],
}
fingerprint = hashlib.sha256(
    json.dumps(logical, ensure_ascii=False, separators=(",", ":")).encode()
).hexdigest()
manifest = {
    "seed_method": "direct_sqlite",
    "database": str(DB),
    "integrity_check": integrity,
    "logical_fingerprint_sha256": fingerprint,
    "counts": counts,
    "longest_account_label": conn.execute("SELECT MAX(LENGTH(label)) FROM accounts").fetchone()[0],
    "longest_account_email": conn.execute("SELECT MAX(LENGTH(email)) FROM accounts").fetchone()[0],
    "longest_pool_email": conn.execute("SELECT MAX(LENGTH(email)) FROM email_pool").fetchone()[0],
    "quota_threshold_fixture_bps": 100,
    "lowest_active_quota_bps": conn.execute(
        "SELECT MIN(quota_remaining_bps) FROM team_lifecycle_workflows WHERE quota_remaining_bps>=0"
    ).fetchone()[0],
}
(RECORDS / "direct-seed-manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
backup(conn, BACKUPS / "after-direct-seed.sqlite3")
conn.close()
print(json.dumps(manifest, ensure_ascii=False, sort_keys=True))
