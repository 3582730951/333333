#!/usr/bin/env python3

import hashlib
import json
import os
import sqlite3
import sys
import urllib.request


expected = sys.argv[1:]
services = [
    ("main", 34273, "/root/autodl-tmp/cpupg-20260730/etc/config.json"),
    (
        "frontend",
        34274,
        "/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json",
    ),
]
if len(expected) != len(services):
    raise SystemExit("usage: verify_release_state.py MAIN_RELEASE FRONTEND_RELEASE")


def process_for(config_path):
    for entry in os.scandir("/proc"):
        if not entry.name.isdigit():
            continue
        try:
            args = open(f"/proc/{entry.name}/cmdline", "rb").read().split(b"\0")
        except (FileNotFoundError, PermissionError):
            continue
        text = [value.decode(errors="replace") for value in args if value]
        if config_path in text and "--config" in text and "--release-id" in text:
            executable = os.path.realpath(f"/proc/{entry.name}/exe")
            return int(entry.name), executable
    raise RuntimeError(f"service process not found for {config_path}")


for (name, port, config_path), wanted in zip(services, expected):
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/readyz", timeout=5) as response:
        ready = json.load(response)
    if ready.get("release_id") != wanted or not ready.get("ready"):
        raise RuntimeError(f"{name} readiness mismatch: {ready}")
    config = json.load(open(config_path))
    database = sqlite3.connect(
        "file:" + config["database_path"] + "?mode=ro", uri=True
    )
    pid, executable = process_for(config_path)
    result = {
        "pid": pid,
        "release_id": ready["release_id"],
        "ready": ready["ready"],
        "binary": executable,
        "binary_sha256": hashlib.sha256(open(executable, "rb").read()).hexdigest(),
        "accounts": database.execute("SELECT COUNT(*) FROM accounts").fetchone()[0],
        "goal_sessions": database.execute(
            "SELECT COUNT(*) FROM goal_session"
        ).fetchone()[0],
        "goal_storage_bytes": database.execute(
            "SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session"
        ).fetchone()[0],
    }
    print(name, json.dumps(result, sort_keys=True))
