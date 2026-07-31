#!/usr/bin/env python3
"""Populate the isolated upgrade fixture directly through SQLite."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import sqlite3
import time
from pathlib import Path


ROOT = Path(os.environ.get("ROOT", "/root/autodl-tmp/legacy-install-upgrade-20260731"))
DB = ROOT / "data" / "pool.sqlite3"
BACKUPS = ROOT / "backups"
RECORDS = ROOT / "records"
NOW = int(time.time())

BACKUPS.mkdir(parents=True, exist_ok=True)
RECORDS.mkdir(parents=True, exist_ok=True)


def columns(conn: sqlite3.Connection, table: str) -> set[str]:
    return {row[1] for row in conn.execute(f'PRAGMA table_info("{table}")')}


def insert(conn: sqlite3.Connection, table: str, values: dict, *, replace: bool = True) -> None:
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


def stable_rows(conn: sqlite3.Connection, table: str, selected: list[str]) -> list[list]:
    available = columns(conn, table)
    fields = [field for field in selected if field in available]
    order = ", ".join(f'"{field}"' for field in fields)
    return [list(row) for row in conn.execute(f'SELECT {order} FROM "{table}" ORDER BY {order}')]


conn = sqlite3.connect(DB, timeout=30)
conn.row_factory = sqlite3.Row
conn.execute("PRAGMA foreign_keys=ON")
conn.execute("PRAGMA busy_timeout=30000")
conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")
backup(conn, BACKUPS / "old-empty.sqlite3")

groups = [
    "核心生产 · 北美高容量",
    "产品设计 · Apple 体验复核",
    "研究与分析 · 超长名称工作组用于布局压力验证",
    "生命周期自动化 · 邮箱与账号池",
]
for index, name in enumerate(groups):
    insert(
        conn,
        "groups",
        {
            "name": name,
            "system_prompt": "",
            "prompt_mode": "prepend",
            "force_model": "",
            "force_effort": "",
            "created_at": NOW - 86400 * (20 - index),
            "updated_at": NOW - 3600 * index,
        },
    )

providers = ["codex", "claude", "kiro", "antigravity"]
statuses = ["active", "active", "active", "quarantined", "disabled"]
plans = ["team", "pro", "business", "free"]
account_ids: list[str] = []
for index in range(16):
    account_id = f"upgrade-demo-account-{index + 1:02d}"
    account_ids.append(account_id)
    long_label = (
        f"北美产品设计与数据可视化验证账号 {index + 1:02d} · "
        f"very-long-account-label-for-install-upgrade-{index + 1:02d}"
    )
    email = (
        f"frontend-upgrade-observability-{index + 1:02d}-"
        f"long-mailbox-identity@subdomain-{index % 4}.example.test"
    )
    provider = providers[index % len(providers)]
    status = statuses[index % len(statuses)]
    insert(
        conn,
        "accounts",
        {
            "id": account_id,
            "label": long_label,
            "group_name": groups[index % len(groups)],
            "upstream_account_id": f"fixture-upstream-{index + 1:02d}",
            "chatgpt_user_id": f"fixture-user-{index + 1:02d}",
            "email": email,
            "plan_type": plans[index % len(plans)],
            "provider": provider,
            "status": status,
            "quarantine_until": NOW + 7200 if status == "quarantined" else 0,
            "quarantine_reason": "fixture scheduled review" if status == "quarantined" else "",
            "created_at": NOW - 86400 * (15 - index),
            "updated_at": NOW - 300 * index,
        },
    )
    insert(
        conn,
        "account_rate_limits",
        {
            "account_id": account_id,
            "provider": provider,
            "model": "gpt-5.2-codex" if index % 2 == 0 else "claude-sonnet-4-5",
            "limiter_type": "primary",
            "source": "upgrade_fixture",
            "used_percent": float(8 + index * 5),
            "limit_tokens": 1_000_000,
            "remaining_tokens": 920_000 - index * 43_000,
            "limit_requests": 10_000,
            "remaining_requests": 9_600 - index * 410,
            "reset_at": NOW + 3600 * (index + 1),
            "status": "healthy" if status == "active" else status,
            "raw_json": "{}",
            "updated_at": NOW - index * 60,
        },
    )
    for model_index, model in enumerate(("gpt-5.2-codex", "claude-sonnet-4-5")):
        insert(
            conn,
            "account_model_capabilities",
            {
                "account_id": account_id,
                "model_slug": model,
                "availability_state": "available" if status == "active" else "unverified",
                "context_1m_state": "supported" if model_index else "unknown",
                "context_1m_source": "upgrade_fixture",
                "native_context_window": 200_000,
                "native_max_context_window": 1_000_000,
                "effective_context_window_percent": 100 - index,
                "auto_compact_token_limit": 180_000,
                "visibility": "public",
                "source": "upgrade_fixture",
                "last_probe_at": NOW - index * 120,
            },
        )

models = ["gpt-5.2-codex", "claude-sonnet-4-5", "gemini-2.5-pro", "kiro-auto"]
for index in range(336):
    created_at = NOW - (index % (24 * 14)) * 3600
    prompt = 950 + (index * 137) % 11_000
    completion = 230 + (index * 73) % 3_200
    cached = int(prompt * ((index % 8) / 10))
    insert(
        conn,
        "usage_records",
        {
            "usage_event_id": f"upgrade-fixture-usage-{index + 1:04d}",
            "account_id": account_ids[index % len(account_ids)],
            "route_key_hash": f"fixture-route-{index % 23:02d}",
            "model": models[index % len(models)],
            "prompt_tokens": prompt,
            "completion_tokens": completion,
            "total_tokens": prompt + completion,
            "cached_tokens": cached,
            "cache_read_tokens": cached,
            "cache_creation_tokens": max(prompt - cached, 0),
            "usage_provider": providers[index % len(providers)],
            "usage_source": "direct_sql_upgrade_fixture",
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
            "created_at": created_at,
        },
        replace=False,
    )

for index in range(18):
    status = ["idle", "in_use", "used", "error"][index % 4]
    insert(
        conn,
        "email_pool",
        {
            "id": f"upgrade-email-{index + 1:02d}",
            "email": (
                f"apple-layout-long-mailbox-name-{index + 1:02d}-"
                f"observability@regional-subdomain-{index % 5}.example.test"
            ),
            "password": "",
            "client_id": "",
            "refresh_token": "",
            "status": status,
            "group_name": groups[index % len(groups)],
            "error_message": "fixture retry scheduled" if status == "error" else "",
            "last_used_at": NOW - index * 500,
            "created_at": NOW - 86400 * (index + 1),
            "updated_at": NOW - index * 300,
        },
    )

for index in range(48):
    insert(
        conn,
        "audit_log",
        {
            "account_id": account_ids[index % len(account_ids)],
            "account_label": f"升级复核账号 {index % len(account_ids) + 1:02d}",
            "action": ["route", "quota_refresh", "lifecycle_check", "registration"][index % 4],
            "state": ["success", "success", "queued", "warning"][index % 4],
            "reason": "direct_sql_upgrade_fixture",
            "detail": json.dumps(
                {"fixture": True, "sequence": index + 1, "install_phase": "legacy"},
                ensure_ascii=False,
                separators=(",", ":"),
            ),
            "created_at": NOW - index * 240,
        },
        replace=False,
    )

for index in range(6):
    job_id = f"upgrade-registration-job-{index + 1:02d}"
    succeeded = 9 + index
    failed = index % 3
    total = succeeded + failed
    insert(
        conn,
        "registration_jobs",
        {
            "id": job_id,
            "platform": ["chatgpt", "claude", "kiro"][index % 3],
            "method": ["protocol", "browser"][index % 2],
            "total": total,
            "succeeded": succeeded,
            "failed": failed,
            "status": "completed",
            "config_json": '{"fixture":true}',
            "started_at": NOW - 7200 - index * 600,
            "completed_at": NOW - 6600 - index * 600,
            "created_at": NOW - 7500 - index * 600,
            "updated_at": NOW - 6600 - index * 600,
        },
    )
    for record_index in range(3):
        insert(
            conn,
            "registration_records",
            {
                "id": f"{job_id}-record-{record_index + 1}",
                "job_id": job_id,
                "account_id": account_ids[(index * 3 + record_index) % len(account_ids)],
                "email": (
                    f"registration-result-{index + 1:02d}-{record_index + 1:02d}"
                    "@mailbox.example.test"
                ),
                "phone": "",
                "tier": plans[(index + record_index) % len(plans)],
                "cost_usd": round(0.12 + index * 0.03, 2),
                "duration_seconds": 31 + record_index * 12,
                "status": "success",
                "detail_json": '{"fixture":true}',
                "created_at": NOW - 6400 - index * 600 - record_index * 60,
            },
        )

for index in range(8):
    insert(
        conn,
        "lifecycle_tasks",
        {
            "id": f"upgrade-lifecycle-task-{index + 1:02d}",
            "task_type": ["health_check", "token_refresh", "quota_sync", "account_review"][index % 4],
            "platform": providers[index % len(providers)],
            "status": ["completed", "running", "pending", "completed"][index % 4],
            "config_json": '{"fixture":true}',
            "target_count": 16,
            "completed_count": 12 + index % 4,
            "success_count": 11 + index % 3,
            "failed_count": index % 2,
            "result_json": '{"source":"direct_sql"}',
            "created_at": NOW - 10_000 - index * 900,
            "started_at": NOW - 9_700 - index * 900,
            "finished_at": NOW - 9_000 - index * 900 if index % 4 in (0, 3) else 0,
            "created_by": "upgrade_fixture",
            "priority": index % 3,
        },
    )

for key, value in {
    "upgrade_fixture_marker": "cache-hit-optimization-to-optimized",
    "upgrade_fixture_seed_version": "20260731-v1",
    "upgrade_fixture_expected_accounts": str(len(account_ids)),
}.items():
    insert(conn, "settings", {"key": key, "value": value, "updated_at": NOW})

conn.commit()
conn.execute("PRAGMA wal_checkpoint(TRUNCATE)")

fingerprint_input = {
    "groups": stable_rows(conn, "groups", ["name"]),
    "accounts": stable_rows(conn, "accounts", ["id", "label", "email", "provider", "status"]),
    "email_pool": stable_rows(conn, "email_pool", ["id", "email", "status", "group_name"]),
    "settings": [
        list(row)
        for row in conn.execute(
            "SELECT key, value FROM settings WHERE key LIKE 'upgrade_fixture_%' ORDER BY key"
        )
    ],
}
logical_sha = hashlib.sha256(
    json.dumps(fingerprint_input, ensure_ascii=False, separators=(",", ":")).encode()
).hexdigest()

tracked_tables = [
    "groups",
    "accounts",
    "account_rate_limits",
    "account_model_capabilities",
    "usage_records",
    "email_pool",
    "audit_log",
    "registration_jobs",
    "registration_records",
    "lifecycle_tasks",
]
counts = {
    table: conn.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
    for table in tracked_tables
}
manifest = {
    "seed_method": "direct_sqlite",
    "database": str(DB),
    "logical_fingerprint_sha256": logical_sha,
    "counts": counts,
    "longest_account_label": conn.execute("SELECT MAX(LENGTH(label)) FROM accounts").fetchone()[0],
    "longest_email": conn.execute("SELECT MAX(LENGTH(email)) FROM email_pool").fetchone()[0],
    "fixture_marker": conn.execute(
        "SELECT value FROM settings WHERE key='upgrade_fixture_marker'"
    ).fetchone()[0],
}
(RECORDS / "seed-manifest.json").write_text(
    json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
)
backup(conn, BACKUPS / "old-seeded.sqlite3")
conn.close()

print(json.dumps(manifest, ensure_ascii=False, separators=(",", ":")))
