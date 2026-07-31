#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import sqlite3
from pathlib import Path


ROOT = Path(os.environ.get("ROOT", "/root/autodl-tmp/legacy-install-upgrade-20260731"))
PHASE = os.environ.get("PHASE", "verification")
DB = ROOT / "data" / "pool.sqlite3"
CONFIG = ROOT / "etc" / "config.json"
APP = ROOT / "prefix" / "lib" / "codex-pool"
CURRENT = APP / "current"


def sha(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def columns(conn: sqlite3.Connection, table: str) -> set[str]:
    return {row[1] for row in conn.execute(f'PRAGMA table_info("{table}")')}


def stable_rows(conn: sqlite3.Connection, table: str, selected: list[str]) -> list[list]:
    available = columns(conn, table)
    fields = [field for field in selected if field in available]
    order = ", ".join(f'"{field}"' for field in fields)
    return [list(row) for row in conn.execute(f'SELECT {order} FROM "{table}" ORDER BY {order}')]


conn = sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=30)
quick_check = conn.execute("PRAGMA quick_check").fetchone()[0]
integrity_check = conn.execute("PRAGMA integrity_check").fetchone()[0]
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
marker = conn.execute(
    "SELECT value FROM settings WHERE key='upgrade_fixture_marker'"
).fetchone()[0]
schema_tables = conn.execute(
    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'"
).fetchone()[0]
conn.close()

current_target = CURRENT.resolve()
release_json = json.loads((current_target / "release.json").read_text())
result = {
    "phase": PHASE,
    "quick_check": quick_check,
    "integrity_check": integrity_check,
    "logical_fingerprint_sha256": logical_sha,
    "counts": counts,
    "fixture_marker": marker,
    "schema_tables": schema_tables,
    "config_sha256": sha(CONFIG),
    "database_sha256": sha(DB),
    "current_release": current_target.name,
    "release_manifest": release_json,
    "server_binary_sha256": sha(current_target / "codex-pool-server"),
}
record = ROOT / "records" / f"data-{PHASE}.json"
record.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(json.dumps(result, ensure_ascii=False, separators=(",", ":")))

if quick_check != "ok" or integrity_check != "ok":
    raise SystemExit(1)
