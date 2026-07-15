#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: clear-context-journal.sh [options]

Locate MiCliProxy/codex-pool SQLite databases, delete every encrypted context
journal row, truncate the WAL and VACUUM the database.

Options:
  --database PATH   Operate on an explicit database (repeatable).
  --config PATH     Read database_path from a config JSON file (repeatable).
  --scan-root PATH  Recursively inspect SQLite files below PATH (repeatable).
  --dry-run         List verified context databases without changing them.
  --yes, -y         Skip the destructive confirmation prompt.
  --help, -h        Show this help.

Without explicit paths the script checks CODEX_POOL_DATABASE, running
codex-pool-server command lines, standard config locations, and common install
roots. Use --scan-root / only when a full-disk scan is really required.
EOF
}

command -v python3 >/dev/null 2>&1 || {
  echo "error: python3 is required (the standard sqlite3 module is used)" >&2
  exit 2
}

declare -a databases=() configs=() roots=()
dry_run=0
assume_yes=0
while (($#)); do
  case "$1" in
    --database) databases+=("${2:?missing path after --database}"); shift 2 ;;
    --config) configs+=("${2:?missing path after --config}"); shift 2 ;;
    --scan-root) roots+=("${2:?missing path after --scan-root}"); shift 2 ;;
    --dry-run) dry_run=1; shift ;;
    --yes|-y) assume_yes=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "error: unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

exec python3 - "$dry_run" "$assume_yes" "${#databases[@]}" "${databases[@]}" "${#configs[@]}" "${configs[@]}" "${#roots[@]}" "${roots[@]}" <<'PY'
import json
import os
import sqlite3
import sys
from pathlib import Path

args = iter(sys.argv[1:])
dry_run = next(args) == "1"
assume_yes = next(args) == "1"
databases = [next(args) for _ in range(int(next(args)))]
configs = [next(args) for _ in range(int(next(args)))]
scan_roots = [next(args) for _ in range(int(next(args)))]

script_cwd = Path.cwd()
candidate_configs = [Path(p) for p in configs]
candidate_configs += [
    Path("/etc/codex-pool/config.json"),
    Path("/var/lib/codex-pool/config.json"),
    script_cwd / "config.json",
    script_cwd / "config.server.json",
    script_cwd / "config.local.json",
]

# Discover --config arguments from running pool-server processes without needing
# systemctl (works in containers and minimal installations too).
proc = Path("/proc")
if proc.is_dir():
    for entry in proc.iterdir():
        if not entry.name.isdigit():
            continue
        try:
            words = (entry / "cmdline").read_bytes().split(b"\0")
            decoded = [word.decode(errors="ignore") for word in words if word]
        except (OSError, PermissionError):
            continue
        if not any("codex-pool-server" in word or "pool-server" in word for word in decoded):
            continue
        for index, word in enumerate(decoded[:-1]):
            if word == "--config":
                candidate_configs.append(Path(decoded[index + 1]))
            elif word.startswith("--config="):
                candidate_configs.append(Path(word.split("=", 1)[1]))

candidates = [Path(p).expanduser() for p in databases]
env_database = os.environ.get("CODEX_POOL_DATABASE", "").strip()
if env_database:
    candidates.append(Path(env_database).expanduser())

for config in candidate_configs:
    try:
        payload = json.loads(config.read_text(encoding="utf-8"))
        value = str(payload.get("database_path", "")).strip()
        if value:
            path = Path(value).expanduser()
            candidates.append(path if path.is_absolute() else config.parent / path)
    except (OSError, ValueError, TypeError):
        pass

if not databases and not scan_roots:
    scan_roots = [
        Path("/var/lib/codex-pool"), Path("/opt/codex-pool"),
        Path("/srv/codex-pool"), script_cwd,
    ]
else:
    scan_roots = [Path(root).expanduser() for root in scan_roots]

suffixes = {".sqlite", ".sqlite3", ".db"}
for root in scan_roots:
    if not root.exists():
        continue
    if root.is_file():
        candidates.append(root)
        continue
    for directory, dirnames, filenames in os.walk(root):
        dirnames[:] = [name for name in dirnames if name not in {".git", "node_modules", "proc", "sys", "dev"}]
        for filename in filenames:
            path = Path(directory) / filename
            if path.suffix.lower() in suffixes:
                candidates.append(path)

verified = []
seen = set()
for raw_path in candidates:
    try:
        path = raw_path.resolve(strict=True)
    except OSError:
        continue
    if path in seen or not path.is_file():
        continue
    seen.add(path)
    try:
        uri = f"file:{path.as_posix()}?mode=ro"
        with sqlite3.connect(uri, uri=True, timeout=2) as db:
            row = db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='context_journal'").fetchone()
            if not row:
                continue
            count = int(db.execute("SELECT COUNT(*) FROM context_journal").fetchone()[0])
        verified.append((path, count))
    except (sqlite3.Error, OSError):
        continue

if not verified:
    print("No SQLite database containing context_journal was found.", file=sys.stderr)
    print("Hint: pass --database /path/to/pool.sqlite3 or --scan-root /path.", file=sys.stderr)
    raise SystemExit(1)

print("Verified context journal databases:")
for path, count in verified:
    sizes = sum(p.stat().st_size for p in (path, Path(str(path)+"-wal"), Path(str(path)+"-shm")) if p.exists())
    print(f"  {path}  rows={count}  allocated_bytes={sizes}")

if dry_run:
    print("Dry run: no data changed.")
    raise SystemExit(0)
if not assume_yes:
    answer = input("Delete ALL encrypted context journals above and reclaim SQLite space? [y/N] ").strip().lower()
    if answer not in {"y", "yes"}:
        print("Cancelled.")
        raise SystemExit(0)

failed = False
for path, _ in verified:
    before = sum(p.stat().st_size for p in (path, Path(str(path)+"-wal"), Path(str(path)+"-shm")) if p.exists())
    try:
        with sqlite3.connect(path, timeout=30, isolation_level=None) as db:
            db.execute("PRAGMA busy_timeout=30000")
            db.execute("BEGIN IMMEDIATE")
            deleted = int(db.execute("SELECT COUNT(*) FROM context_journal").fetchone()[0])
            db.execute("DELETE FROM context_journal")
            db.execute("COMMIT")
            checkpoint = db.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
            if checkpoint and int(checkpoint[0]) != 0:
                raise sqlite3.OperationalError(f"WAL checkpoint busy: {checkpoint}")
            db.execute("VACUUM")
            db.execute("PRAGMA optimize")
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        after = sum(p.stat().st_size for p in (path, Path(str(path)+"-wal"), Path(str(path)+"-shm")) if p.exists())
        print(f"OK {path}: deleted={deleted} before_bytes={before} after_bytes={after} reclaimed_bytes={max(0, before-after)}")
    except (sqlite3.Error, OSError) as exc:
        failed = True
        print(f"FAILED {path}: {exc}", file=sys.stderr)

raise SystemExit(1 if failed else 0)
PY
