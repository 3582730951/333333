#!/usr/bin/env python3

import json
import sqlite3
import urllib.request


SERVICES = [
    ("main", 34273, "/root/autodl-tmp/cpupg-20260730/etc/config.json"),
    (
        "frontend",
        34274,
        "/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json",
    ),
]


for name, port, config_path in SERVICES:
    config = json.load(open(config_path))
    base = f"http://127.0.0.1:{port}"
    with urllib.request.urlopen(base + "/readyz", timeout=5) as response:
        ready = json.load(response)
    request = urllib.request.Request(
        base + "/admin/system",
        headers={"Authorization": "Bearer " + config["admin_token"]},
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        system = json.load(response)
    with urllib.request.urlopen(base + "/console/", timeout=5) as response:
        console_status = response.status
    database = sqlite3.connect(
        "file:" + config["database_path"] + "?mode=ro", uri=True
    )
    goal_bytes = database.execute(
        "SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session"
    ).fetchone()[0]
    reclaimed = database.execute(
        "SELECT COUNT(*) FROM audit_log WHERE action='goal_storage_reclaimed'"
    ).fetchone()[0]
    print(
        name,
        json.dumps(
            {
                "ready_status": 200,
                "release_id": ready["release_id"],
                "console_status": console_status,
                "goal_storage_bytes": goal_bytes,
                "goal_reclaimed_audits": reclaimed,
                "disk_guard": {
                    key: system["disk_guard"][key]
                    for key in (
                        "level",
                        "database_writable",
                        "goal_storage_target_bytes",
                        "goal_storage_reserve_bytes",
                        "goal_bytes_reclaimed",
                    )
                },
                "routing_audit": system["routing_audit"],
            },
            sort_keys=True,
        ),
    )
