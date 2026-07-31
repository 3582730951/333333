#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' '--- listening services ---'
ss -ltnp | grep -E ':(34273|34274|8802|34277|34279)\b' || true

printf '%s\n' '--- process contracts ---'
python3 - <<'PY'
import os

needles = (
    "34273",
    "34274",
    "cpupg-20260730/etc/config.json",
    "frontend-ui-shot-20260731/runtime/config.json",
    "codex_reauth_worker.py",
)
for entry in os.scandir("/proc"):
    if not entry.name.isdigit():
        continue
    try:
        args = [
            value.decode(errors="replace")
            for value in open(f"/proc/{entry.name}/cmdline", "rb").read().split(b"\0")
            if value
        ]
    except (FileNotFoundError, PermissionError):
        continue
    text = " ".join(args)
    if any(needle in text for needle in needles):
        print(entry.name, text)
PY

for spec in '34273 main' '34274 frontend'; do
  read -r port label <<<"$spec"
  printf '%s_ready=' "$label"
  curl -fsS "http://127.0.0.1:${port}/readyz"
  echo
done

printf '%s\n' '--- config release-relevant fields (secrets omitted) ---'
python3 - <<'PY'
import json
import os

paths = (
    "/root/autodl-tmp/cpupg-20260730/etc/config.json",
    "/root/autodl-tmp/frontend-ui-shot-20260731/runtime/config.json",
)
keys = (
    "listen_addr",
    "database_path",
    "codex_reauth_worker_url",
    "registration_enabled",
    "registration_default_method",
    "registration_max_concurrency",
)
for path in paths:
    config = json.load(open(path, encoding="utf-8"))
    print(path)
    for key in keys:
        print(" ", key, repr(config.get(key)))
    print(" bytes", os.path.getsize(path))
PY

printf '%s\n' '--- database integrity/counts ---'
python3 - <<'PY'
import os
import sqlite3

paths = (
    "/root/autodl-tmp/cpupg-20260730/state/pool.sqlite3",
    "/root/autodl-tmp/frontend-ui-shot-20260731/runtime/pool.sqlite3",
)
for path in paths:
    connection = sqlite3.connect(path)
    tables = {
        row[0]
        for row in connection.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table'"
        )
    }
    print(
        path,
        "bytes",
        os.path.getsize(path),
        "integrity",
        connection.execute("PRAGMA integrity_check").fetchone()[0],
    )
    for table in (
        "accounts",
        "email_pool",
        "team_lifecycle_workflows",
        "provider_settings",
    ):
        count = (
            connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
            if table in tables
            else "missing"
        )
        print(" ", table, count)
    connection.close()
PY
