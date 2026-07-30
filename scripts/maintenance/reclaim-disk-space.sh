#!/usr/bin/env bash
#
# One-shot, account/context/log preserving disk reclamation for codex-pool.
#
# The command is intentionally a dry-run unless --apply is present.  An apply
# run must be quiescent: either let this script stop one explicitly named
# systemd unit, or stop every process that has the SQLite database open first.
#
# SQLite is the primary deployment path and receives an automatic full backup,
# exact SHA-256 invariants, one atomic cleanup transaction, quick_check,
# WAL truncation and VACUUM.  PostgreSQL uses pg_dump plus one transaction and
# VACUUM; callers must explicitly attest that writers are quiesced.
set -Eeuo pipefail

umask 077

PROGRAM=${0##*/}
MODE=dry-run
DRIVER=sqlite
DRIVER_EXPLICIT=0
DATA_DIR=
DATABASE_PATH=
POSTGRES_DSN=
CONFIG_PATH=
BACKUP_DIR=
RETENTION_DAYS=
STALE_FILE_HOURS=24
SERVICE_NAME=
STOP_SERVICE=0
ASSUME_QUIESCED=0
OPTIMIZE_CONFIG=0
ROLLBACK_PATH=
SERVICE_WAS_ACTIVE=0
LOCK_FD=
ONE_CLICK=0

usage() {
  cat <<'EOF'
Usage:
  reclaim-disk-space.sh [options]                 # dry-run (default)
  reclaim-disk-space.sh --apply [options]         # perform reclamation
  reclaim-disk-space.sh --one-click [options]     # apply + stop/restart + optimize
  reclaim-disk-space.sh --apply --rollback FILE [options]

Database selection:
  --config FILE              Read data_dir/database_path/storage_driver/DSN.
  --data-dir DIR             Persistent data root.
  --db FILE                  SQLite database path.
  --postgres-dsn DSN         PostgreSQL DSN (never written to reports).
  --driver sqlite|postgres   Explicit storage driver.

Safety and retention:
  --retention-days N         Historical-log cutoff; default is the stored
                             reg_log_retention_days setting, then 7.
  --stale-file-hours N       Minimum age for spool/browser temp removal (24).
  --backup-dir DIR           Backup/archive destination.  Prefer another disk.
  --service UNIT             Exact systemd unit owned by this deployment.
  --stop-service             Stop UNIT for --apply and restart it on exit.
  --assume-quiesced          Assert all writers are already stopped.  Required
                             for PostgreSQL apply without --stop-service.
  --optimize-config          Atomically write conservative space bounds to the
                             JSON config; never lowers a bound below live data.
  --one-click                Equivalent to --apply --stop-service
                             --optimize-config; requires exact --service UNIT.
  --apply                    Mutate.  Without this flag no file is changed.
  --rollback FILE            Restore a SQLite .sqlite3 or .sqlite3.gz backup.
  -h, --help                 Show this help.

Always preserved and verified:
  * account rows, credentials, cookies, group membership and egress bindings;
  * goal/context/session payloads and the virtual context ledger;
  * logs inside the retention window; older logs are losslessly JSONL+gzip
    archived and checksummed before their database rows are removed;
  * keys, users/API keys, groups, providers, egress and all static settings.

Reclaimed:
  * expired/stale route and model-discovery caches;
  * expired user/maintenance leases and terminal reauth history;
  * diagnostic artifacts/rows and stale spool/browser/snapshot files;
  * archived log rows older than the configured retention window;
  * SQLite free pages and WAL via VACUUM + TRUNCATE checkpoint.

The usage journal and host-wide/system logs are never touched.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*"
}

need_value() {
  (($# >= 2)) || die "$1 requires a value"
}

while (($#)); do
  case "$1" in
    --apply) MODE=apply; shift ;;
    --config) need_value "$@"; CONFIG_PATH=$2; shift 2 ;;
    --data-dir) need_value "$@"; DATA_DIR=$2; shift 2 ;;
    --db) need_value "$@"; DATABASE_PATH=$2; shift 2 ;;
    --postgres-dsn) need_value "$@"; POSTGRES_DSN=$2; DRIVER=postgres; shift 2 ;;
    --driver) need_value "$@"; DRIVER=$2; DRIVER_EXPLICIT=1; shift 2 ;;
    --backup-dir) need_value "$@"; BACKUP_DIR=$2; shift 2 ;;
    --retention-days) need_value "$@"; RETENTION_DAYS=$2; shift 2 ;;
    --stale-file-hours) need_value "$@"; STALE_FILE_HOURS=$2; shift 2 ;;
    --service) need_value "$@"; SERVICE_NAME=$2; shift 2 ;;
    --stop-service) STOP_SERVICE=1; shift ;;
    --assume-quiesced) ASSUME_QUIESCED=1; shift ;;
    --optimize-config) OPTIMIZE_CONFIG=1; shift ;;
    --one-click)
      ONE_CLICK=1
      MODE=apply
      STOP_SERVICE=1
      OPTIMIZE_CONFIG=1
      shift
      ;;
    --rollback) need_value "$@"; ROLLBACK_PATH=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

[[ $STALE_FILE_HOURS =~ ^[0-9]+$ ]] && ((STALE_FILE_HOURS >= 1)) ||
  die "--stale-file-hours must be a positive integer"
if [[ -n $RETENTION_DAYS ]]; then
  [[ $RETENTION_DAYS =~ ^[0-9]+$ ]] &&
    ((RETENTION_DAYS >= 1 && RETENTION_DAYS <= 3650)) ||
    die "--retention-days must be between 1 and 3650"
fi
((STOP_SERVICE == 0)) || [[ -n $SERVICE_NAME ]] ||
  die "--stop-service requires an exact --service UNIT"

command -v python3 >/dev/null 2>&1 || die "python3 is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"
command -v gzip >/dev/null 2>&1 || die "gzip is required"
command -v flock >/dev/null 2>&1 || die "flock is required"

# Read only missing CLI values from JSON.  NUL separators keep paths lossless.
if [[ -n $CONFIG_PATH ]]; then
  [[ -f $CONFIG_PATH && ! -L $CONFIG_PATH ]] ||
    die "config is not a regular non-symlink file: $CONFIG_PATH"
  mapfile -d '' -t CONFIG_VALUES < <(
    python3 - "$CONFIG_PATH" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as handle:
    value = json.load(handle)
for key in ("data_dir", "database_path", "storage_driver", "postgres_dsn"):
    sys.stdout.write(str(value.get(key, "") or ""))
    sys.stdout.write("\0")
PY
  )
  [[ -n $DATA_DIR ]] || DATA_DIR=${CONFIG_VALUES[0]:-}
  [[ -n $DATABASE_PATH ]] || DATABASE_PATH=${CONFIG_VALUES[1]:-}
  if ((DRIVER_EXPLICIT == 0)) && [[ -n ${CONFIG_VALUES[2]:-} ]]; then
    DRIVER=${CONFIG_VALUES[2]}
  fi
  [[ -n $POSTGRES_DSN ]] || POSTGRES_DSN=${CONFIG_VALUES[3]:-}
fi

[[ $DRIVER == sqlite || $DRIVER == postgres ]] ||
  die "--driver/storage_driver must be sqlite or postgres"
((OPTIMIZE_CONFIG == 0)) || [[ -n $CONFIG_PATH ]] ||
  die "--optimize-config requires --config FILE"

if [[ -z $DATA_DIR ]]; then
  if [[ $DRIVER == sqlite && -n $DATABASE_PATH ]]; then
    DATA_DIR="$(dirname "$DATABASE_PATH")/data"
  else
    DATA_DIR=data
  fi
fi
DATA_DIR=$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$DATA_DIR")
if [[ $DRIVER == sqlite ]]; then
  [[ -n $DATABASE_PATH ]] || DATABASE_PATH="$DATA_DIR/../pool.sqlite3"
  DATABASE_PATH=$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$DATABASE_PATH")
  [[ -f $DATABASE_PATH && ! -L $DATABASE_PATH ]] ||
    die "SQLite database is not a regular non-symlink file: $DATABASE_PATH"
else
  [[ -n $POSTGRES_DSN ]] || die "PostgreSQL requires --postgres-dsn or postgres_dsn in --config"
  command -v psql >/dev/null 2>&1 || die "psql is required for PostgreSQL"
  command -v pg_dump >/dev/null 2>&1 || die "pg_dump is required for PostgreSQL"
  command -v pg_restore >/dev/null 2>&1 || die "pg_restore is required for PostgreSQL"
fi

if [[ -z $BACKUP_DIR ]]; then
  BACKUP_DIR="$(dirname "$DATA_DIR")/codex-pool-maintenance-backups"
fi
BACKUP_DIR=$(python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$BACKUP_DIR")
[[ ! -L $BACKUP_DIR ]] || die "backup directory may not be a symlink: $BACKUP_DIR"
case "$BACKUP_DIR/" in
  "$DATA_DIR/diagnostics/"*|"$DATA_DIR/spool/"*|"$DATA_DIR/tmp/browser/"*)
    die "backup directory may not be inside diagnostics, spool, or browser temp" ;;
esac

cleanup_shell() {
  local status=$?
  if ((SERVICE_WAS_ACTIVE)); then
    if ! systemctl start "$SERVICE_NAME"; then
      printf 'ERROR: failed to restart service %s\n' "$SERVICE_NAME" >&2
      status=1
    else
      printf 'Service restarted: %s\n' "$SERVICE_NAME"
    fi
  fi
  exit "$status"
}
trap cleanup_shell EXIT

if [[ $MODE == apply ]]; then
  mkdir -p "$BACKUP_DIR"
  chmod 700 "$BACKUP_DIR"
fi

LOCK_PARENT=$DATA_DIR
[[ -d $LOCK_PARENT ]] || LOCK_PARENT=$(dirname "$DATABASE_PATH")
if [[ $MODE == apply ]]; then
  mkdir -p "$LOCK_PARENT"
  exec {LOCK_FD}>"$LOCK_PARENT/.codex-pool-reclaim.lock"
  flock -n "$LOCK_FD" || die "another disk reclamation is already running"
else
  [[ -d $LOCK_PARENT ]] || die "lock parent does not exist in dry-run: $LOCK_PARENT"
fi

if ((STOP_SERVICE)); then
  command -v systemctl >/dev/null 2>&1 || die "systemctl is required by --stop-service"
  if systemctl is-active --quiet "$SERVICE_NAME"; then
    if [[ $MODE == apply ]]; then
      note "Stopping service: $SERVICE_NAME"
      systemctl stop "$SERVICE_NAME"
      SERVICE_WAS_ACTIVE=1
    else
      note "DRY-RUN: service $SERVICE_NAME is active; --apply would stop it."
    fi
  fi
fi

sqlite_open_pids() {
  local path=$1
  if command -v fuser >/dev/null 2>&1; then
    fuser "$path" "$path-wal" "$path-shm" 2>/dev/null || true
    return
  fi
  local fd target pid
  for fd in /proc/[0-9]*/fd/*; do
    [[ -e $fd ]] || continue
    target=$(readlink "$fd" 2>/dev/null || true)
    case "$target" in
      "$path"|"$path-wal"|"$path-shm")
        pid=${fd#/proc/}; pid=${pid%%/*}; printf '%s ' "$pid" ;;
    esac
  done
}

if [[ $MODE == apply && $DRIVER == sqlite && $ASSUME_QUIESCED == 0 ]]; then
  OPEN_PIDS=$(sqlite_open_pids "$DATABASE_PATH")
  [[ -z ${OPEN_PIDS//[[:space:]]/} ]] ||
    die "SQLite still has open writer/reader processes (PIDs: $OPEN_PIDS); stop the deployment or use --service UNIT --stop-service"
fi
if [[ $MODE == apply && $DRIVER == postgres &&
      $ASSUME_QUIESCED == 0 && $SERVICE_WAS_ACTIVE == 0 ]]; then
  die "PostgreSQL apply requires --service UNIT --stop-service or --assume-quiesced"
fi

export RECLAIM_MODE=$MODE
export RECLAIM_DRIVER=$DRIVER
export RECLAIM_DATA_DIR=$DATA_DIR
export RECLAIM_DATABASE_PATH=$DATABASE_PATH
export RECLAIM_BACKUP_DIR=$BACKUP_DIR
export RECLAIM_RETENTION_DAYS=$RETENTION_DAYS
export RECLAIM_STALE_FILE_HOURS=$STALE_FILE_HOURS
export RECLAIM_CONFIG_PATH=$CONFIG_PATH
export RECLAIM_OPTIMIZE_CONFIG=$OPTIMIZE_CONFIG
export RECLAIM_ROLLBACK_PATH=$ROLLBACK_PATH

if [[ $DRIVER == sqlite ]]; then
  python3 - <<'PY'
import base64
import contextlib
import datetime as dt
import gzip
import hashlib
import json
import math
import os
import shutil
import shlex
import sqlite3
import stat
import sys
import time
from pathlib import Path

MODE = os.environ["RECLAIM_MODE"]
APPLY = MODE == "apply"
DB = Path(os.environ["RECLAIM_DATABASE_PATH"]).resolve()
DATA = Path(os.environ["RECLAIM_DATA_DIR"]).resolve()
BACKUP_ROOT = Path(os.environ["RECLAIM_BACKUP_DIR"]).resolve()
CONFIG = Path(os.environ["RECLAIM_CONFIG_PATH"]).resolve() if os.environ.get("RECLAIM_CONFIG_PATH") else None
OPTIMIZE_CONFIG = os.environ.get("RECLAIM_OPTIMIZE_CONFIG") == "1"
ROLLBACK = Path(os.environ["RECLAIM_ROLLBACK_PATH"]).resolve() if os.environ.get("RECLAIM_ROLLBACK_PATH") else None
STALE_HOURS = int(os.environ["RECLAIM_STALE_FILE_HOURS"])
NOW = int(time.time())
STAMP = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")

def fail(message):
    raise RuntimeError(message)

def fsync_file(path):
    with open(path, "rb") as handle:
        os.fsync(handle.fileno())

def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    try:
        os.fsync(fd)
    finally:
        os.close(fd)

def hash_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        while True:
            chunk = handle.read(4 << 20)
            if not chunk:
                break
            digest.update(chunk)
    return digest.hexdigest()

def restore_sqlite(backup, destination):
    backup = Path(backup)
    destination = Path(destination)
    parent = destination.parent
    temp = parent / ("." + destination.name + ".restore-" + STAMP)
    with contextlib.suppress(FileNotFoundError):
        temp.unlink()
    if backup.suffix == ".gz":
        with gzip.open(backup, "rb") as source, open(temp, "wb") as target:
            shutil.copyfileobj(source, target, length=4 << 20)
            target.flush()
            os.fsync(target.fileno())
    else:
        shutil.copy2(backup, temp)
        fsync_file(temp)
    check = sqlite3.connect(f"file:{temp}?mode=ro", uri=True)
    try:
        result = check.execute("PRAGMA quick_check").fetchone()
        if not result or result[0] != "ok":
            fail(f"rollback backup failed quick_check: {result!r}")
    finally:
        check.close()
    original = destination.stat()
    os.chmod(temp, stat.S_IMODE(original.st_mode))
    with contextlib.suppress(PermissionError):
        os.chown(temp, original.st_uid, original.st_gid)
    os.replace(temp, destination)
    for suffix in ("-wal", "-shm"):
        with contextlib.suppress(FileNotFoundError):
            Path(str(destination) + suffix).unlink()
    fsync_dir(parent)

if ROLLBACK:
    if not APPLY:
        print(f"DRY-RUN rollback: {ROLLBACK} -> {DB}")
        sys.exit(0)
    if not ROLLBACK.is_file() or ROLLBACK.is_symlink():
        fail(f"rollback input is not a regular non-symlink file: {ROLLBACK}")
    restore_sqlite(ROLLBACK, DB)
    restored_config = None
    if CONFIG is not None:
        config_backup = ROLLBACK.parent / (CONFIG.name + ".before.json")
        if config_backup.is_file() and not config_backup.is_symlink():
            with open(config_backup, "r", encoding="utf-8") as handle:
                json.load(handle)
            temp = CONFIG.parent / ("." + CONFIG.name + ".rollback-" + STAMP)
            shutil.copy2(config_backup, temp)
            os.replace(temp, CONFIG)
            fsync_dir(CONFIG.parent)
            restored_config = str(CONFIG)
    print(f"Rollback verified and restored: {DB}")
    if restored_config:
        print(f"Rollback restored config: {restored_config}")
    sys.exit(0)

DRY_RUN_SNAPSHOT = None
if not APPLY:
    # Pin one SQLite read transaction on the live WAL database. This gives every
    # count/hash query the same logical snapshot while normal writers continue;
    # copying DB/WAL/SHM and requiring their mtimes to stand still made dry-run
    # fail on every busy production pool and also consumed database-sized /tmp
    # space. A non-empty WAL without SHM cannot be opened read-only without
    # SQLite potentially creating a sidecar, so surface that rare stale layout.
    wal_path = Path(str(DB) + "-wal")
    shm_path = Path(str(DB) + "-shm")
    live_wal = wal_path.exists() and wal_path.stat().st_size > 0
    if live_wal and not shm_path.exists():
        fail(
            "non-empty SQLite WAL has no SHM sidecar; start the configured "
            "service once or quiesce writers before dry-run"
        )
    # When there is no live WAL, immutable read mode avoids manufacturing new
    # -wal/-shm files during what must remain a physically read-only preview.
    # With a live WAL, use SQLite's normal read-only WAL path so committed frames
    # remain visible; the already-present SHM coordinates the pinned snapshot.
    uri = f"file:{DB}?mode=ro" if live_wal else f"file:{DB}?mode=ro&immutable=1"
else:
    uri = f"file:{DB}?mode=rw"
con = sqlite3.connect(uri, uri=True, timeout=30, isolation_level=None)
con.row_factory = sqlite3.Row
con.execute("PRAGMA busy_timeout=30000")
if not APPLY:
    con.execute("PRAGMA query_only=ON")
    con.execute("BEGIN")
    # Force snapshot establishment before any table inventory/fingerprint query.
    con.execute("SELECT COUNT(*) FROM sqlite_schema").fetchone()
    DRY_RUN_SNAPSHOT = {
        "kind": "sqlite_read_transaction",
        "journal_mode": str(con.execute("PRAGMA journal_mode").fetchone()[0]),
        "access": "live_wal" if live_wal else "immutable_main",
        "query_only": bool(con.execute("PRAGMA query_only").fetchone()[0]),
        "quick_check": "deferred_until_apply",
    }

def quote_ident(name):
    return '"' + name.replace('"', '""') + '"'

def table_exists(name):
    row = con.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", (name,)
    ).fetchone()
    return row is not None

def columns(name):
    if not table_exists(name):
        return []
    return [row[1] for row in con.execute(f"PRAGMA table_info({quote_ident(name)})")]

def order_clause(name):
    info = list(con.execute(f"PRAGMA table_info({quote_ident(name)})"))
    primary = [row[1] for row in sorted((r for r in info if r[5]), key=lambda r: r[5])]
    chosen = primary or [row[1] for row in info]
    return ", ".join(quote_ident(item) for item in chosen)

def encode_value(value):
    if isinstance(value, bytes):
        return {"$binary_base64": base64.b64encode(value).decode("ascii")}
    if isinstance(value, float):
        if math.isnan(value):
            return {"$float": "nan"}
        if math.isinf(value):
            return {"$float": "inf" if value > 0 else "-inf"}
    return value

def feed_rows(digest, name, where="1=1", params=()):
    if not table_exists(name):
        return 0
    cols = columns(name)
    digest.update(("table:" + name + "\ncolumns:" + json.dumps(cols, separators=(",", ":")) + "\n").encode())
    query = f"SELECT * FROM {quote_ident(name)} WHERE {where} ORDER BY {order_clause(name)}"
    count = 0
    for row in con.execute(query, params):
        payload = [encode_value(row[col]) for col in cols]
        digest.update(json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8"))
        digest.update(b"\n")
        count += 1
    return count

def fingerprint(specs):
    digest = hashlib.sha256()
    total = 0
    details = {}
    for name, where, params in specs:
        count = feed_rows(digest, name, where, params)
        if table_exists(name):
            details[name] = count
            total += count
    return {"rows": total, "sha256": digest.hexdigest(), "tables": details}

def count_inventory(specs):
    total = 0
    details = {}
    for name, where, params in specs:
        if not table_exists(name):
            continue
        count = count_where(name, where, params)
        details[name] = count
        total += count
    return {
        "rows": total,
        "sha256": None,
        "verification": "content_hash_deferred_until_apply",
        "tables": details,
    }

ACCOUNT_TABLES = [
    "accounts",
    "account_auth_tokens",
    "account_kiro_credentials",
    "account_antigravity_credentials",
    "account_session_cookies",
    "account_codex_reauth_config",
    "account_injected_cookies",
    "account_egress_bindings",
    "account_group_memberships",
    "account_lifecycle_status",
    "account_rate_limits",
    "codex_reset_credit_consumptions",
]
CONTEXT_TABLES = [
    "context_journal",
    "virtual_context_ledger",
    "goal_session",
    "goal_alias",
    "goal_checkpoint",
    "goal_segment",
    "goal_payload_chunk",
    "goal_run",
    "codex_session_binding",
    "codex_session_alias",
    "codex_instruction_snapshot",
]

def all_specs(names):
    return [(name, "1=1", ()) for name in names]

stored_days = None
if table_exists("settings"):
    row = con.execute(
        "SELECT value FROM settings WHERE key='reg_log_retention_days'"
    ).fetchone()
    if row:
        with contextlib.suppress(ValueError, TypeError):
            stored_days = int(row[0])
requested_days = os.environ.get("RECLAIM_RETENTION_DAYS", "").strip()
RETENTION_DAYS = int(requested_days) if requested_days else (
    stored_days if stored_days and 1 <= stored_days <= 3650 else 7
)
CUTOFF = NOW - RETENTION_DAYS * 86400
ROUTE_CUTOFF = NOW - 30 * 86400

def legacy_dual_write_coverage():
    result = {
        "goal_continuity_confirmed": False,
        "active_context_rows": 0,
        "covered_context_rows": 0,
        "can_disable": False,
        "reason": "",
    }
    if not OPTIMIZE_CONFIG:
        result["reason"] = "config optimization was not requested"
        return result
    if CONFIG is None or not CONFIG.is_file() or CONFIG.is_symlink():
        result["reason"] = "a regular --config file is required to prove goal continuity"
        return result
    try:
        with open(CONFIG, "r", encoding="utf-8") as handle:
            cfg = json.load(handle)
    except Exception as exc:
        result["reason"] = f"config could not be read: {exc}"
        return result
    continuity_enabled = cfg.get("goal_continuity_enabled") is True
    if table_exists("settings"):
        stored = con.execute(
            "SELECT value FROM settings WHERE key='goal_continuity_enabled'"
        ).fetchone()
        if stored is not None:
            continuity_enabled = str(stored[0] or "").strip().lower() in {
                "1", "true", "yes", "on",
            }
    if not continuity_enabled:
        result["reason"] = "effective goal_continuity_enabled is not confirmed true"
        return result
    result["goal_continuity_confirmed"] = True
    required = {
        "context_journal", "goal_session", "goal_alias", "goal_checkpoint",
        "goal_segment", "goal_payload_chunk",
    }
    missing_tables = sorted(name for name in required if not table_exists(name))
    if missing_tables:
        result["reason"] = "missing goal tables: " + ",".join(missing_tables)
        return result
    rows = list(con.execute(
        "SELECT response_id FROM context_journal WHERE expires_at > ? ORDER BY response_id",
        (NOW,),
    ))
    result["active_context_rows"] = len(rows)
    uncovered = []
    for item in rows:
        response_id = str(item[0] or "").strip()
        alias_hash = hashlib.sha256(
            ("response_id\0" + response_id).encode("utf-8")
        ).hexdigest()
        goal = con.execute(
            """
SELECT s.id, s.current_checkpoint_id
FROM goal_alias a
JOIN goal_session s ON s.id=a.goal_id
WHERE a.alias_hash=? AND a.alias_type='response_id'
  AND s.expires_at>? AND s.state<>'reclaiming'
ORDER BY s.id
""",
            (alias_hash, NOW),
        ).fetchall()
        if len(goal) != 1:
            uncovered.append(alias_hash[:16] + ":alias")
            continue
        goal_id, checkpoint_id = goal[0]
        checkpoint = con.execute(
            """
SELECT through_segment_sequence, encrypted_payload
FROM goal_checkpoint
WHERE id=? AND goal_id=?
""",
            (checkpoint_id, goal_id),
        ).fetchone()
        if checkpoint is None:
            uncovered.append(alias_hash[:16] + ":checkpoint")
            continue
        through, encrypted_checkpoint = int(checkpoint[0] or 0), str(checkpoint[1] or "")
        checkpoint_chunks = int(con.execute(
            "SELECT COUNT(*) FROM goal_payload_chunk "
            "WHERE goal_id=? AND payload_kind='checkpoint' AND segment_sequence=0 "
            "AND encrypted_payload<>''",
            (goal_id,),
        ).fetchone()[0])
        checkpoint_chunk_shape = con.execute(
            "SELECT COALESCE(MIN(chunk_index),0),COALESCE(MAX(chunk_index),-1),COUNT(*) "
            "FROM goal_payload_chunk WHERE goal_id=? AND payload_kind='checkpoint' "
            "AND segment_sequence=0 AND encrypted_payload<>''",
            (goal_id,),
        ).fetchone()
        checkpoint_chunks_complete = (
            checkpoint_chunks > 0
            and int(checkpoint_chunk_shape[0]) == 0
            and int(checkpoint_chunk_shape[1]) + 1 == int(checkpoint_chunk_shape[2])
        )
        if not encrypted_checkpoint and not checkpoint_chunks_complete:
            uncovered.append(alias_hash[:16] + ":checkpoint_payload")
            continue
        if through > 0:
            compacted = list(con.execute(
                """
SELECT segment_sequence,MIN(chunk_index),MAX(chunk_index),COUNT(*),
       SUM(CASE WHEN encrypted_payload='' THEN 1 ELSE 0 END)
FROM goal_payload_chunk
WHERE goal_id=? AND payload_kind='segment'
  AND segment_sequence>0 AND segment_sequence<=?
GROUP BY segment_sequence
ORDER BY segment_sequence
""",
                (goal_id, through),
            ))
            if (
                len(compacted) != through
                or any(
                    int(row[0]) != index
                    or int(row[1]) != 0
                    or int(row[2]) + 1 != int(row[3])
                    or int(row[4] or 0) != 0
                    for index, row in enumerate(compacted, start=1)
                )
            ):
                uncovered.append(alias_hash[:16] + ":compacted_segment_payload")
                continue
        missing_segments = con.execute(
            """
SELECT COUNT(*)
FROM goal_segment seg
WHERE seg.goal_id=? AND seg.sequence>?
  AND seg.state<>'discarded'
  AND (
    (seg.format_version<2 AND COALESCE(seg.encrypted_payload,'')='')
    OR
    (seg.format_version>=2 AND (
      NOT EXISTS (
        SELECT 1 FROM goal_payload_chunk chunk
        WHERE chunk.goal_id=seg.goal_id
          AND chunk.payload_kind='segment'
          AND chunk.segment_sequence=seg.sequence
          AND chunk.encrypted_payload<>''
      )
      OR EXISTS (
        SELECT 1
        FROM goal_payload_chunk chunk
        WHERE chunk.goal_id=seg.goal_id
          AND chunk.payload_kind='segment'
          AND chunk.segment_sequence=seg.sequence
        GROUP BY chunk.goal_id,chunk.segment_sequence
        HAVING MIN(chunk.chunk_index)<>0
            OR MAX(chunk.chunk_index)+1<>COUNT(*)
            OR SUM(CASE WHEN chunk.encrypted_payload='' THEN 1 ELSE 0 END)<>0
      )
    ))
  )
""",
            (goal_id, through),
        ).fetchone()[0]
        if int(missing_segments):
            uncovered.append(alias_hash[:16] + ":segment_payload")
            continue
        result["covered_context_rows"] += 1
    if uncovered:
        result["reason"] = (
            f"{len(uncovered)} active context row(s) lack structurally recoverable "
            "goal coverage; legacy dual-write remains enabled"
        )
        result["uncovered_alias_hash_prefixes"] = uncovered[:20]
        return result
    result["can_disable"] = True
    result["reason"] = (
        "every unexpired context_journal response alias has one live goal with "
        "a checkpoint and all uncompacted segment payloads"
    )
    return result

DUAL_WRITE_PROOF = legacy_dual_write_coverage()

# Each predicate names historical data that is archived before removal.
LOG_RULES = [
    ("audit_log", "created_at < ?", (CUTOFF,)),
    ("cf_events", "created_at < ?", (CUTOFF,)),
    ("usage_records", "created_at < ?", (CUTOFF,)),
    (
        "usage_events",
        "updated_at < ? AND (hold_id='' OR NOT EXISTS "
        "(SELECT 1 FROM billing_holds h WHERE h.id=usage_events.hold_id AND h.status='held'))",
        (CUTOFF,),
    ),
    ("registration_task_events", "created_at < ?", (CUTOFF,)),
    ("lifecycle_task_logs", "timestamp < ?", (CUTOFF,)),
    ("lifecycle_events", "timestamp < ?", (CUTOFF,)),
    ("proxy_usage_records", "created_at < ?", (CUTOFF,)),
    ("billing_holds", "updated_at < ? AND status <> 'held'", (CUTOFF,)),
    ("model_quality_runs", "created_at < ?", (CUTOFF,)),
    ("member_rotation_log", "created_at < ?", (CUTOFF,)),
    ("quota_check_log", "created_at < ?", (CUTOFF,)),
]

def log_specs(retained):
    out = []
    for name, old_where, params in LOG_RULES:
        where = f"NOT ({old_where})" if retained else "1=1"
        out.append((name, where, params if retained else ()))
    return out

def count_where(name, where, params=()):
    if not table_exists(name):
        return 0
    return int(con.execute(
        f"SELECT COUNT(*) FROM {quote_ident(name)} WHERE {where}", params
    ).fetchone()[0])

if APPLY:
    account_before = fingerprint(all_specs(ACCOUNT_TABLES))
    context_before = fingerprint(all_specs(CONTEXT_TABLES))
    logs_all_before = fingerprint(log_specs(False))
    logs_retained_before = fingerprint(log_specs(True))
else:
    # A preview needs exact candidate/count information, not a byte-for-byte scan
    # of multi-GiB context payloads. Full invariant hashes and quick_check are
    # mandatory in --apply/--one-click after the verified backup is created.
    account_before = count_inventory(all_specs(ACCOUNT_TABLES))
    context_before = count_inventory(all_specs(CONTEXT_TABLES))
    logs_all_before = count_inventory(log_specs(False))
    logs_retained_before = count_inventory(log_specs(True))

candidate_counts = {}
for name, where, params in LOG_RULES:
    if table_exists(name):
        candidate_counts[f"log:{name}"] = count_where(name, where, params)

CACHE_RULES = [
    ("affinity_aliases", "expires_at <= ?", (NOW,)),
    (
        "affinity_bindings",
        "(expires_at > 0 AND expires_at <= ?) OR "
        "(expires_at = 0 AND updated_at < ?)",
        (NOW, ROUTE_CUTOFF),
    ),
    ("user_group_target_bindings", "updated_at < ?", (ROUTE_CUTOFF,)),
    ("antigravity_cache_entries", "expires_at > 0 AND expires_at <= ?", (NOW,)),
    ("kiro_model_catalog", "expires_at > 0 AND expires_at <= ?", (NOW,)),
    ("kiro_probe_state", "expires_at > 0 AND expires_at <= ?", (NOW,)),
    ("kiro_runtime_capabilities", "updated_at < ?", (ROUTE_CUTOFF,)),
    ("account_model_capabilities", "last_probe_at < ?", (CUTOFF,)),
    ("account_model_catalog_status", "last_probe_at < ?", (CUTOFF,)),
    ("user_sessions", "expires_at <= ?", (NOW,)),
    ("maintenance_leases", "expires_at <= ?", (NOW,)),
    (
        "account_codex_reauth_jobs",
        "updated_at < ? AND status IN ('completed','failed','cancelled')",
        (CUTOFF,),
    ),
    ("codex_upstream_attempt", "expires_at <= ?", (NOW,)),
    ("codex_upstream_attempt_daily", "expires_at <= ?", (NOW,)),
]
CACHE_ARCHIVE_TABLES = {
    "account_codex_reauth_jobs",
    "codex_upstream_attempt",
    "codex_upstream_attempt_daily",
}
for name, where, params in CACHE_RULES:
    if table_exists(name):
        candidate_counts[f"cache:{name}"] = count_where(name, where, params)

for name in ("diagnostic_download_leases", "diagnostic_events", "diagnostic_jobs"):
    if table_exists(name):
        candidate_counts[f"diagnostic:{name}"] = count_where(name, "1=1")
if table_exists("storage_resources"):
    candidate_counts["diagnostic:storage_resources"] = count_where(
        "storage_resources",
        "resource_type='diagnostic_artifact' OR retention_class='diagnostic_artifact_24h'",
    )

db_bytes_before = DB.stat().st_size
wal = Path(str(DB) + "-wal")
wal_bytes_before = wal.stat().st_size if wal.exists() else 0
filesystem_free_before = shutil.disk_usage(DB.parent).free
summary = {
    "mode": MODE,
    "database": str(DB),
    "data_dir": str(DATA),
    "backup_dir": str(BACKUP_ROOT),
    "retention_days": RETENTION_DAYS,
    "cutoff_epoch": CUTOFF,
    "database_bytes_before": db_bytes_before,
    "wal_bytes_before": wal_bytes_before,
    "filesystem_free_bytes_before": filesystem_free_before,
    "accounts_before": account_before,
    "contexts_before": context_before,
    "logs_all_before": logs_all_before,
    "logs_retained_before": logs_retained_before,
    "candidate_rows": candidate_counts,
    "legacy_dual_write_coverage": DUAL_WRITE_PROOF,
    "dry_run_snapshot": DRY_RUN_SNAPSHOT,
}

def scan_file_candidates():
    paths = []
    stale_before = NOW - STALE_HOURS * 3600
    for role, root, remove_all in (
        ("diagnostics", DATA / "diagnostics", True),
        ("spool", DATA / "spool", False),
        ("browser_tmp", DATA / "tmp" / "browser", False),
    ):
        if not root.exists() or root.is_symlink() or not root.is_dir():
            continue
        for base, dirs, files in os.walk(root, followlinks=False):
            dirs[:] = [d for d in dirs if not (Path(base) / d).is_symlink()]
            for filename in files:
                path = Path(base) / filename
                try:
                    info = path.lstat()
                except FileNotFoundError:
                    continue
                if not stat.S_ISREG(info.st_mode) or path.is_symlink():
                    continue
                if remove_all or int(info.st_mtime) <= stale_before:
                    paths.append((role, path, info.st_size))
    for path in DB.parent.glob(".diagnostic-snapshot-*"):
        try:
            info = path.lstat()
        except FileNotFoundError:
            continue
        if stat.S_ISREG(info.st_mode) and not path.is_symlink() and int(info.st_mtime) <= stale_before:
            paths.append(("legacy_snapshot", path, info.st_size))
    return paths

file_candidates = scan_file_candidates()
summary["candidate_files"] = {
    "count": len(file_candidates),
    "bytes": sum(item[2] for item in file_candidates),
    "by_role": {
        role: {
            "count": sum(1 for item in file_candidates if item[0] == role),
            "bytes": sum(item[2] for item in file_candidates if item[0] == role),
        }
        for role in sorted({item[0] for item in file_candidates})
    },
}

if not APPLY:
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    print("DRY-RUN: no database row, file, or config was changed.")
    con.execute("ROLLBACK")
    con.close()
    sys.exit(0)

run_dir = BACKUP_ROOT / ("reclaim-" + STAMP)
run_dir.mkdir(mode=0o700, parents=True, exist_ok=False)
history_dir = run_dir / "history"
history_dir.mkdir(mode=0o700)
full_backup = run_dir / (DB.name + ".before.sqlite3")
compressed_backup = Path(str(full_backup) + ".gz")
report_path = run_dir / "verification.json"
config_backup = None
committed = False

def backup_database():
    target = sqlite3.connect(full_backup)
    try:
        con.backup(target)
    finally:
        target.close()
    os.chmod(full_backup, 0o600)
    check = sqlite3.connect(f"file:{full_backup}?mode=ro", uri=True)
    try:
        row = check.execute("PRAGMA quick_check").fetchone()
        if not row or row[0] != "ok":
            fail(f"full backup quick_check failed: {row!r}")
    finally:
        check.close()
    fsync_file(full_backup)

def archive_rows(name, where, params, category):
    if not table_exists(name):
        return None
    cols = columns(name)
    query = f"SELECT * FROM {quote_ident(name)} WHERE {where} ORDER BY {order_clause(name)}"
    path = history_dir / f"{category}-{name}.jsonl.gz"
    digest = hashlib.sha256()
    count = 0
    with gzip.open(path, "wt", encoding="utf-8", compresslevel=9, newline="\n") as out:
        header = {
            "format": "codex-pool-maintenance-jsonl-v1",
            "table": name,
            "columns": cols,
            "where": where,
            "cutoff_epoch": CUTOFF,
        }
        out.write(json.dumps(header, ensure_ascii=False, separators=(",", ":")) + "\n")
        for row in con.execute(query, params):
            payload = [encode_value(row[col]) for col in cols]
            encoded = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
            digest.update(encoded.encode("utf-8"))
            digest.update(b"\n")
            out.write(encoded + "\n")
            count += 1
    os.chmod(path, 0o600)
    # Read every byte through gzip and independently count payload records.
    with gzip.open(path, "rt", encoding="utf-8") as check:
        check_header = json.loads(check.readline())
        verified = sum(1 for _ in check)
    if check_header.get("table") != name or verified != count:
        fail(f"archive verification failed for {name}: {verified} != {count}")
    return {
        "table": name,
        "rows": count,
        "sha256_rows": digest.hexdigest(),
        "sha256_gzip": hash_file(path),
        "bytes": path.stat().st_size,
        "path": str(path),
    }

def safe_delete_files(candidates):
    deleted = []
    roots = [DATA / "diagnostics", DATA / "spool", DATA / "tmp" / "browser", DB.parent]
    resolved_roots = [root.resolve() for root in roots if root.exists()]
    for role, path, expected_size in candidates:
        try:
            resolved = path.resolve(strict=True)
            info = path.lstat()
        except FileNotFoundError:
            continue
        if path.is_symlink() or not stat.S_ISREG(info.st_mode):
            continue
        if not any(resolved == root or root in resolved.parents for root in resolved_roots):
            fail(f"refusing file outside managed roots: {resolved}")
        path.unlink()
        deleted.append({"role": role, "path": str(path), "bytes": expected_size})
    for root in (DATA / "diagnostics", DATA / "spool", DATA / "tmp" / "browser"):
        if not root.exists() or root.is_symlink():
            continue
        for base, dirs, _ in os.walk(root, topdown=False, followlinks=False):
            for name in dirs:
                with contextlib.suppress(OSError):
                    (Path(base) / name).rmdir()
    return deleted

def compress_old_file_logs():
    log_root = DATA / "logs"
    if not log_root.exists() or log_root.is_symlink() or not log_root.is_dir():
        return []
    changed = []
    for base, dirs, files in os.walk(log_root, followlinks=False):
        dirs[:] = [d for d in dirs if not (Path(base) / d).is_symlink()]
        for name in files:
            path = Path(base) / name
            if name.endswith(".gz") or path.is_symlink():
                continue
            try:
                info = path.stat()
            except FileNotFoundError:
                continue
            if not stat.S_ISREG(info.st_mode) or int(info.st_mtime) >= CUTOFF:
                continue
            target = Path(str(path) + ".gz")
            if target.exists():
                fail(f"refusing to overwrite existing compressed log: {target}")
            temp = Path(str(target) + ".tmp")
            with open(path, "rb") as source, gzip.open(temp, "wb", compresslevel=9) as out:
                shutil.copyfileobj(source, out, length=4 << 20)
            with gzip.open(temp, "rb") as check:
                while check.read(4 << 20):
                    pass
            os.chmod(temp, stat.S_IMODE(info.st_mode) & 0o600 or 0o600)
            os.replace(temp, target)
            os.utime(target, (info.st_atime, info.st_mtime))
            path.unlink()
            changed.append({
                "source": str(path),
                "target": str(target),
                "before_bytes": info.st_size,
                "after_bytes": target.stat().st_size,
            })
    return changed

def optimize_config():
    global config_backup
    if not OPTIMIZE_CONFIG:
        return None
    if CONFIG is None:
        fail("--optimize-config requires --config")
    if CONFIG.is_symlink() or not CONFIG.is_file():
        fail(f"config is not a regular non-symlink file: {CONFIG}")
    with open(CONFIG, "r", encoding="utf-8") as handle:
        cfg = json.load(handle)
    original = json.dumps(cfg, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    current_context_rows = sum(context_before["tables"].values())
    context_bytes = 0
    if table_exists("context_journal"):
        context_bytes = int(con.execute(
            "SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM context_journal"
        ).fetchone()[0])
    goal_bytes = 0
    if table_exists("goal_session"):
        goal_bytes = int(con.execute(
            "SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session"
        ).fetchone()[0])
    if DUAL_WRITE_PROOF["can_disable"]:
        cfg["goal_legacy_journal_dual_write"] = False
    cfg["goal_retention_days"] = max(7, int(cfg.get("goal_retention_days") or 0))
    cfg["codex_session_mapping_retention_days"] = max(
        7, int(cfg.get("codex_session_mapping_retention_days") or 0)
    )
    cfg["context_journal_max_rows"] = max(50_000, math.ceil(current_context_rows * 1.20))
    cfg["context_journal_max_mb"] = max(200, math.ceil(context_bytes * 1.20 / (1 << 20)))
    cfg["goal_storage_max_mb"] = max(256, math.ceil(goal_bytes * 1.20 / (1 << 20)))
    cfg["usage_journal_segment_bytes"] = 8 << 20
    cfg["body_disk_reserve_bytes"] = max(
        512 << 20, int(cfg.get("body_disk_reserve_bytes") or 0)
    )
    updated = json.dumps(cfg, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    if updated == original:
        return {
            "changed": False,
            "path": str(CONFIG),
            "legacy_dual_write_coverage": DUAL_WRITE_PROOF,
        }
    config_backup = run_dir / (CONFIG.name + ".before.json")
    shutil.copy2(CONFIG, config_backup)
    os.chmod(config_backup, 0o600)
    temp = CONFIG.parent / ("." + CONFIG.name + ".reclaim-" + STAMP)
    with open(temp, "w", encoding="utf-8") as handle:
        json.dump(cfg, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temp, stat.S_IMODE(CONFIG.stat().st_mode))
    os.replace(temp, CONFIG)
    fsync_dir(CONFIG.parent)
    return {
        "changed": True,
        "path": str(CONFIG),
        "backup": str(config_backup),
        "sha256": hashlib.sha256(CONFIG.read_bytes()).hexdigest(),
        "legacy_dual_write_coverage": DUAL_WRITE_PROOF,
    }

try:
    backup_database()
    archives = []
    con.execute("BEGIN IMMEDIATE")
    try:
        for name, where, params in LOG_RULES:
            item = archive_rows(name, where, params, "historical-log")
            if item and item["rows"]:
                archives.append(item)
        for name in ("diagnostic_download_leases", "diagnostic_events", "diagnostic_jobs"):
            item = archive_rows(name, "1=1", (), "diagnostic")
            if item and item["rows"]:
                archives.append(item)
        for name, where, params in CACHE_RULES:
            if name not in CACHE_ARCHIVE_TABLES:
                continue
            item = archive_rows(name, where, params, "terminal-history")
            if item and item["rows"]:
                archives.append(item)
        if table_exists("storage_resources"):
            item = archive_rows(
                "storage_resources",
                "resource_type='diagnostic_artifact' OR retention_class='diagnostic_artifact_24h'",
                (),
                "diagnostic",
            )
            if item and item["rows"]:
                archives.append(item)
        for name, where, params in LOG_RULES:
            if table_exists(name):
                con.execute(f"DELETE FROM {quote_ident(name)} WHERE {where}", params)
        for name, where, params in CACHE_RULES:
            if table_exists(name):
                con.execute(f"DELETE FROM {quote_ident(name)} WHERE {where}", params)
        if OPTIMIZE_CONFIG and DUAL_WRITE_PROOF["can_disable"] and table_exists("settings"):
            con.execute(
                "INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) "
                "ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at",
                ("goal_legacy_journal_dual_write", "false", NOW),
            )
        # Delete children before jobs even on legacy databases with FK enforcement off.
        for name in ("diagnostic_download_leases", "diagnostic_events"):
            if table_exists(name):
                con.execute(f"DELETE FROM {quote_ident(name)}")
        if table_exists("storage_resources"):
            con.execute(
                "DELETE FROM storage_resources "
                "WHERE resource_type='diagnostic_artifact' "
                "OR retention_class='diagnostic_artifact_24h'"
            )
        if table_exists("diagnostic_jobs"):
            con.execute("DELETE FROM diagnostic_jobs")
        # Exact in-transaction invariants: none of these tables may change.
        account_inside = fingerprint(all_specs(ACCOUNT_TABLES))
        context_inside = fingerprint(all_specs(CONTEXT_TABLES))
        logs_retained_inside = fingerprint(log_specs(False))
        if account_inside != account_before:
            fail("account invariant changed inside cleanup transaction")
        if context_inside != context_before:
            fail("context invariant changed inside cleanup transaction")
        if (
            logs_retained_inside["rows"] != logs_retained_before["rows"]
            or logs_retained_inside["sha256"] != logs_retained_before["sha256"]
        ):
            fail("retained-log invariant changed inside cleanup transaction")
        con.execute("COMMIT")
        committed = True
    except Exception:
        with contextlib.suppress(sqlite3.Error):
            con.execute("ROLLBACK")
        raise

    # VACUUM cannot run inside a transaction.  Keep the verified raw backup until
    # all post-commit checks finish so any failure can be restored automatically.
    row = con.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
    if row and row[0] != 0:
        fail(f"WAL checkpoint remained busy: {tuple(row)!r}")
    con.execute("VACUUM")
    con.execute("PRAGMA optimize")
    row = con.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
    if row and row[0] != 0:
        fail(f"post-VACUUM WAL checkpoint remained busy: {tuple(row)!r}")
    row = con.execute("PRAGMA quick_check").fetchone()
    if not row or row[0] != "ok":
        fail(f"post-cleanup quick_check failed: {row!r}")

    account_after = fingerprint(all_specs(ACCOUNT_TABLES))
    context_after = fingerprint(all_specs(CONTEXT_TABLES))
    logs_after = fingerprint(log_specs(False))
    if account_after != account_before:
        fail("post-VACUUM account count/hash changed")
    if context_after != context_before:
        fail("post-VACUUM context count/hash changed")
    if (
        logs_after["rows"] != logs_retained_before["rows"]
        or logs_after["sha256"] != logs_retained_before["sha256"]
    ):
        fail("post-VACUUM retained-log count/hash changed")

    deleted_files = safe_delete_files(file_candidates)
    compressed_logs = compress_old_file_logs()
    config_result = optimize_config()

    # Compress the verified rollback image only after every mutation succeeds.
    with open(full_backup, "rb") as source, gzip.open(
        compressed_backup, "wb", compresslevel=6
    ) as target:
        shutil.copyfileobj(source, target, length=4 << 20)
    with gzip.open(compressed_backup, "rb") as check:
        header = check.read(16)
        if header[:6] != b"SQLite":
            fail("compressed rollback backup does not contain a SQLite header")
        while check.read(4 << 20):
            pass
    os.chmod(compressed_backup, 0o600)
    compressed_backup_hash = hash_file(compressed_backup)
    full_backup.unlink()

    summary.update({
        "database_bytes_after": DB.stat().st_size,
        "wal_bytes_after": wal.stat().st_size if wal.exists() else 0,
        "filesystem_free_bytes_after": shutil.disk_usage(DB.parent).free,
        "filesystem_free_bytes_gained": (
            shutil.disk_usage(DB.parent).free - filesystem_free_before
        ),
        "database_bytes_reclaimed": max(0, db_bytes_before - DB.stat().st_size),
        "accounts_after": account_after,
        "contexts_after": context_after,
        "logs_after": logs_after,
        "archives": archives,
        "archived_rows": sum(item["rows"] for item in archives),
        "deleted_files": {
            "count": len(deleted_files),
            "bytes": sum(item["bytes"] for item in deleted_files),
            "items": deleted_files,
        },
        "compressed_file_logs": compressed_logs,
        "config_optimization": config_result,
        "rollback_backup": str(compressed_backup),
        "rollback_backup_sha256": compressed_backup_hash,
        "rollback_command": (
            f"{Path(sys.argv[0]).name if sys.argv and sys.argv[0] != '-' else 'reclaim-disk-space.sh'} "
            f"--apply --db {shlex.quote(str(DB))} "
            f"--data-dir {shlex.quote(str(DATA))} "
            f"--rollback {shlex.quote(str(compressed_backup))} "
            "--assume-quiesced"
        ),
        "verified": {
            "sqlite_quick_check": "ok",
            "accounts_exact_match": True,
            "contexts_exact_match": True,
            "retained_logs_exact_match": True,
            "historical_logs_archived_before_delete": True,
            "usage_journal_untouched": True,
        },
    })
    with open(report_path, "w", encoding="utf-8") as handle:
        json.dump(summary, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(report_path, 0o600)
    fsync_dir(run_dir)
    print(json.dumps({
        "ok": True,
        "verification": str(report_path),
        "rollback_backup": str(compressed_backup),
        "database_bytes_reclaimed": summary["database_bytes_reclaimed"],
        "files_bytes_reclaimed": summary["deleted_files"]["bytes"],
        "archived_rows": summary["archived_rows"],
        "accounts_sha256": account_after["sha256"],
        "contexts_sha256": context_after["sha256"],
        "retained_logs_sha256": logs_after["sha256"],
    }, ensure_ascii=False, indent=2, sort_keys=True))
except Exception as exc:
    con.close()
    rollback_candidate = full_backup if full_backup.exists() else (
        compressed_backup if compressed_backup.exists() else None
    )
    if committed and rollback_candidate is not None:
        try:
            restore_sqlite(rollback_candidate, DB)
            print("Automatic rollback restored the pre-cleanup SQLite backup.", file=sys.stderr)
        except Exception as rollback_exc:
            print(f"CRITICAL: automatic rollback failed: {rollback_exc}", file=sys.stderr)
    if config_backup and config_backup.exists():
        with contextlib.suppress(Exception):
            shutil.copy2(config_backup, CONFIG)
    print(f"ERROR: SQLite reclamation failed: {exc}", file=sys.stderr)
    sys.exit(1)
finally:
    with contextlib.suppress(Exception):
        con.close()
PY
else
  # PostgreSQL archives old logs first, applies all deletes in one transaction,
  # verifies account/context/retained-log fingerprints, then rewrites the three
  # large history tables so the filesystem receives the freed blocks.
  export PGPASSWORD=${PGPASSWORD:-}
  PG_RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
  PG_RUN_DIR="$BACKUP_DIR/reclaim-$PG_RUN_ID"
  PG_BACKUP="$PG_RUN_DIR/postgres-before.dump"
  PG_REPORT="$PG_RUN_DIR/verification.txt"

  pg_scalar() {
    psql "$POSTGRES_DSN" -X -A -t -v ON_ERROR_STOP=1 -c "$1"
  }
  pg_table_exists() {
    [[ $(pg_scalar "SELECT to_regclass('public.$1') IS NOT NULL") == t ]]
  }
  pg_count() {
    pg_table_exists "$1" || { printf '0'; return; }
    pg_scalar "SELECT COUNT(*) FROM \"$1\" WHERE $2"
  }

  if [[ -n $ROLLBACK_PATH ]]; then
    [[ -f $ROLLBACK_PATH && ! -L $ROLLBACK_PATH ]] ||
      die "rollback dump is not a regular non-symlink file: $ROLLBACK_PATH"
    pg_restore --list "$ROLLBACK_PATH" >/dev/null
    if [[ $MODE != apply ]]; then
      note "DRY-RUN PostgreSQL rollback: $ROLLBACK_PATH"
      exit 0
    fi
    pg_restore --clean --if-exists --single-transaction \
      --dbname="$POSTGRES_DSN" "$ROLLBACK_PATH"
    note "PostgreSQL rollback restored and verified: $ROLLBACK_PATH"
    exit 0
  fi

  PG_RETENTION=${RETENTION_DAYS:-}
  if [[ -z $PG_RETENTION ]] && pg_table_exists settings; then
    PG_RETENTION=$(pg_scalar \
      "SELECT CASE WHEN value ~ '^[0-9]+$' THEN value ELSE '7' END FROM settings WHERE key='reg_log_retention_days'" ||
      true)
  fi
  [[ $PG_RETENTION =~ ^[0-9]+$ ]] || PG_RETENTION=7
  ((PG_RETENTION >= 1 && PG_RETENTION <= 3650)) || PG_RETENTION=7
  PG_NOW=$(date +%s)
  PG_CUTOFF=$((PG_NOW - PG_RETENTION * 86400))

  declare -a PG_LOG_RULES=(
    "audit_log|created_at < $PG_CUTOFF"
    "cf_events|created_at < $PG_CUTOFF"
    "usage_records|created_at < $PG_CUTOFF"
    "usage_events|updated_at < $PG_CUTOFF AND (hold_id='' OR NOT EXISTS (SELECT 1 FROM billing_holds h WHERE h.id=usage_events.hold_id AND h.status='held'))"
    "registration_task_events|created_at < $PG_CUTOFF"
    "lifecycle_task_logs|timestamp < $PG_CUTOFF"
    "lifecycle_events|timestamp < $PG_CUTOFF"
    "proxy_usage_records|created_at < $PG_CUTOFF"
    "billing_holds|updated_at < $PG_CUTOFF AND status <> 'held'"
    "model_quality_runs|created_at < $PG_CUTOFF"
    "member_rotation_log|created_at < $PG_CUTOFF"
    "quota_check_log|created_at < $PG_CUTOFF"
  )
  declare -a PG_DIAGNOSTIC_RULES=(
    "diagnostic_download_leases|TRUE"
    "diagnostic_events|TRUE"
    "storage_resources|resource_type='diagnostic_artifact' OR retention_class='diagnostic_artifact_24h'"
    "diagnostic_jobs|TRUE"
  )

  pg_fingerprint() {
    local table
    {
      for table in accounts account_auth_tokens account_kiro_credentials \
        account_antigravity_credentials account_session_cookies \
        account_codex_reauth_config account_injected_cookies \
        account_egress_bindings account_group_memberships \
        account_lifecycle_status account_rate_limits \
        codex_reset_credit_consumptions; do
        pg_table_exists "$table" || continue
        printf 'table:%s\n' "$table"
        psql "$POSTGRES_DSN" -X -A -t -v ON_ERROR_STOP=1 \
          -c "COPY (SELECT row_to_json(t)::text FROM \"$table\" t ORDER BY row_to_json(t)::text) TO STDOUT"
      done
    } | sha256sum | awk '{print $1}'
  }
  pg_context_fingerprint() {
    local table
    {
      for table in context_journal virtual_context_ledger goal_session goal_alias \
        goal_checkpoint goal_segment goal_payload_chunk goal_run \
        codex_session_binding codex_session_alias codex_instruction_snapshot; do
        pg_table_exists "$table" || continue
        printf 'table:%s\n' "$table"
        psql "$POSTGRES_DSN" -X -A -t -v ON_ERROR_STOP=1 \
          -c "COPY (SELECT row_to_json(t)::text FROM \"$table\" t ORDER BY row_to_json(t)::text) TO STDOUT"
      done
    } | sha256sum | awk '{print $1}'
  }
  pg_log_fingerprint() {
    local retained=$1 rule table where
    {
      for rule in "${PG_LOG_RULES[@]}"; do
        table=${rule%%|*}; where=${rule#*|}
        pg_table_exists "$table" || continue
        [[ $retained == true ]] && where="NOT ($where)" || where=TRUE
        printf 'table:%s\n' "$table"
        psql "$POSTGRES_DSN" -X -A -t -v ON_ERROR_STOP=1 \
          -c "COPY (SELECT row_to_json(src)::text FROM (SELECT * FROM \"$table\" WHERE $where) src ORDER BY row_to_json(src)::text) TO STDOUT"
      done
    } | sha256sum | awk '{print $1}'
  }
  pg_log_count() {
    local retained=$1 rule table where value total=0
    for rule in "${PG_LOG_RULES[@]}"; do
      table=${rule%%|*}; where=${rule#*|}
      [[ $retained == true ]] && where="NOT ($where)" || where=TRUE
      value=$(pg_count "$table" "$where")
      total=$((total + value))
    done
    printf '%s' "$total"
  }

  PG_ACCOUNT_BEFORE=$(pg_fingerprint)
  PG_CONTEXT_BEFORE=$(pg_context_fingerprint)
  PG_LOGS_ALL_BEFORE=$(pg_log_fingerprint false)
  PG_LOGS_RETAINED_BEFORE=$(pg_log_fingerprint true)
  PG_ACCOUNT_COUNT_BEFORE=$(pg_count accounts TRUE)
  PG_CONTEXT_COUNT_BEFORE=0
  for table in context_journal virtual_context_ledger goal_session goal_alias \
    goal_checkpoint goal_segment goal_payload_chunk goal_run \
    codex_session_binding codex_session_alias codex_instruction_snapshot; do
    value=$(pg_count "$table" TRUE)
    PG_CONTEXT_COUNT_BEFORE=$((PG_CONTEXT_COUNT_BEFORE + value))
  done
  PG_LOGS_ALL_COUNT_BEFORE=$(pg_log_count false)
  PG_LOGS_RETAINED_COUNT_BEFORE=$(pg_log_count true)

  note "PostgreSQL mode=$MODE retention_days=$PG_RETENTION"
  note "accounts rows=$PG_ACCOUNT_COUNT_BEFORE sha256=$PG_ACCOUNT_BEFORE"
  note "contexts rows=$PG_CONTEXT_COUNT_BEFORE sha256=$PG_CONTEXT_BEFORE"
  note "logs rows=$PG_LOGS_ALL_COUNT_BEFORE retained_rows=$PG_LOGS_RETAINED_COUNT_BEFORE"
  if [[ $MODE != apply ]]; then
    note "legacy goal journal dual-write remains unchanged: offline PostgreSQL coverage proof was not attempted"
    note "DRY-RUN: no PostgreSQL row, file, or config was changed."
    exit 0
  fi

  mkdir -p "$PG_RUN_DIR/history"
  chmod 700 "$PG_RUN_DIR" "$PG_RUN_DIR/history"
  pg_dump "$POSTGRES_DSN" --format=custom --file="$PG_BACKUP"
  pg_restore --list "$PG_BACKUP" >/dev/null
  chmod 600 "$PG_BACKUP"
  sha256sum "$PG_BACKUP" >"$PG_BACKUP.sha256"

  for rule in "${PG_LOG_RULES[@]}" "${PG_DIAGNOSTIC_RULES[@]}"; do
    table=${rule%%|*}; where=${rule#*|}
    pg_table_exists "$table" || continue
    archive="$PG_RUN_DIR/history/preserved-$table.jsonl.gz"
    {
      printf '{"format":"codex-pool-maintenance-jsonl-v1","table":"%s","cutoff_epoch":%s}\n' "$table" "$PG_CUTOFF"
      psql "$POSTGRES_DSN" -X -A -t -v ON_ERROR_STOP=1 \
        -c "COPY (SELECT row_to_json(src)::text FROM (SELECT * FROM \"$table\" WHERE $where) src ORDER BY row_to_json(src)::text) TO STDOUT"
    } | gzip -9 >"$archive"
    gzip -t "$archive"
    chmod 600 "$archive"
    sha256sum "$archive" >"$archive.sha256"
  done

  PG_SQL="$PG_RUN_DIR/cleanup.sql"
  {
    printf '\\set ON_ERROR_STOP on\nBEGIN;\n'
    for rule in "${PG_LOG_RULES[@]}"; do
      table=${rule%%|*}; where=${rule#*|}
      pg_table_exists "$table" || continue
      printf 'DELETE FROM "%s" WHERE %s;\n' "$table" "$where"
    done
    pg_table_exists affinity_aliases &&
      printf 'DELETE FROM affinity_aliases WHERE expires_at <= %s;\n' "$PG_NOW"
    pg_table_exists affinity_bindings &&
      printf 'DELETE FROM affinity_bindings WHERE (expires_at > 0 AND expires_at <= %s) OR (expires_at=0 AND updated_at < %s);\n' "$PG_NOW" "$((PG_NOW - 30 * 86400))"
    pg_table_exists user_group_target_bindings &&
      printf 'DELETE FROM user_group_target_bindings WHERE updated_at < %s;\n' "$((PG_NOW - 30 * 86400))"
    pg_table_exists antigravity_cache_entries &&
      printf 'DELETE FROM antigravity_cache_entries WHERE expires_at > 0 AND expires_at <= %s;\n' "$PG_NOW"
    pg_table_exists kiro_model_catalog &&
      printf 'DELETE FROM kiro_model_catalog WHERE expires_at > 0 AND expires_at <= %s;\n' "$PG_NOW"
    pg_table_exists kiro_probe_state &&
      printf 'DELETE FROM kiro_probe_state WHERE expires_at > 0 AND expires_at <= %s;\n' "$PG_NOW"
    pg_table_exists user_sessions &&
      printf 'DELETE FROM user_sessions WHERE expires_at <= %s;\n' "$PG_NOW"
    pg_table_exists maintenance_leases &&
      printf 'DELETE FROM maintenance_leases WHERE expires_at <= %s;\n' "$PG_NOW"
    for rule in "${PG_DIAGNOSTIC_RULES[@]}"; do
      table=${rule%%|*}; where=${rule#*|}
      pg_table_exists "$table" || continue
      printf 'DELETE FROM "%s" WHERE %s;\n' "$table" "$where"
    done
    printf 'COMMIT;\n'
  } >"$PG_SQL"
  chmod 600 "$PG_SQL"
  psql "$POSTGRES_DSN" -X -v ON_ERROR_STOP=1 -f "$PG_SQL"

  PG_ACCOUNT_AFTER=$(pg_fingerprint)
  PG_CONTEXT_AFTER=$(pg_context_fingerprint)
  PG_LOGS_AFTER=$(pg_log_fingerprint false)
  PG_ACCOUNT_COUNT_AFTER=$(pg_count accounts TRUE)
  PG_CONTEXT_COUNT_AFTER=0
  for table in context_journal virtual_context_ledger goal_session goal_alias \
    goal_checkpoint goal_segment goal_payload_chunk goal_run \
    codex_session_binding codex_session_alias codex_instruction_snapshot; do
    value=$(pg_count "$table" TRUE)
    PG_CONTEXT_COUNT_AFTER=$((PG_CONTEXT_COUNT_AFTER + value))
  done
  PG_LOGS_COUNT_AFTER=$(pg_log_count false)
  [[ $PG_ACCOUNT_BEFORE == "$PG_ACCOUNT_AFTER" &&
     $PG_ACCOUNT_COUNT_BEFORE == "$PG_ACCOUNT_COUNT_AFTER" ]] ||
    die "PostgreSQL account invariant changed; rollback dump: $PG_BACKUP"
  [[ $PG_CONTEXT_BEFORE == "$PG_CONTEXT_AFTER" &&
     $PG_CONTEXT_COUNT_BEFORE == "$PG_CONTEXT_COUNT_AFTER" ]] ||
    die "PostgreSQL context invariant changed; rollback dump: $PG_BACKUP"
  [[ $PG_LOGS_RETAINED_BEFORE == "$PG_LOGS_AFTER" &&
     $PG_LOGS_RETAINED_COUNT_BEFORE == "$PG_LOGS_COUNT_AFTER" ]] ||
    die "PostgreSQL retained-log invariant changed; rollback dump: $PG_BACKUP"

  for table in audit_log usage_records usage_events; do
    pg_table_exists "$table" || continue
    psql "$POSTGRES_DSN" -X -v ON_ERROR_STOP=1 \
      -c "VACUUM (FULL, ANALYZE) \"$table\"" >/dev/null
  done

  # Filesystem cleanup is confined to this deployment's managed directories.
  export PG_CUTOFF_FOR_FILES=$PG_CUTOFF
  python3 - <<'PY'
import gzip, os, shutil, stat, time
from pathlib import Path
data = Path(os.environ["RECLAIM_DATA_DIR"]).resolve()
now = int(time.time())
stale = now - int(os.environ["RECLAIM_STALE_FILE_HOURS"]) * 3600
for root, all_files in (
    (data / "diagnostics", True),
    (data / "spool", False),
    (data / "tmp" / "browser", False),
):
    if not root.is_dir() or root.is_symlink():
        continue
    for base, dirs, files in os.walk(root, topdown=False, followlinks=False):
        dirs[:] = [d for d in dirs if not (Path(base) / d).is_symlink()]
        for name in files:
            path = Path(base) / name
            try:
                info = path.lstat()
            except FileNotFoundError:
                continue
            if stat.S_ISREG(info.st_mode) and not path.is_symlink() and (
                all_files or int(info.st_mtime) <= stale
            ):
                path.unlink()
        for name in dirs:
            try:
                (Path(base) / name).rmdir()
            except OSError:
                pass
log_root = data / "logs"
cutoff = int(os.environ.get("PG_CUTOFF_FOR_FILES", "0"))
if log_root.is_dir() and not log_root.is_symlink():
    for base, dirs, files in os.walk(log_root, followlinks=False):
        dirs[:] = [d for d in dirs if not (Path(base) / d).is_symlink()]
        for name in files:
            source = Path(base) / name
            if name.endswith(".gz") or source.is_symlink():
                continue
            try:
                info = source.stat()
            except FileNotFoundError:
                continue
            if not stat.S_ISREG(info.st_mode) or int(info.st_mtime) >= cutoff:
                continue
            target = Path(str(source) + ".gz")
            if target.exists():
                continue
            temp = Path(str(target) + ".tmp")
            with open(source, "rb") as inp, gzip.open(temp, "wb", compresslevel=9) as out:
                shutil.copyfileobj(inp, out, length=4 << 20)
            with gzip.open(temp, "rb") as check:
                while check.read(4 << 20):
                    pass
            os.chmod(temp, 0o600)
            os.replace(temp, target)
            os.utime(target, (info.st_atime, info.st_mtime))
            source.unlink()
PY
  if ((OPTIMIZE_CONFIG)); then
    export PG_CONFIG_BACKUP="$PG_RUN_DIR/$(basename "$CONFIG_PATH").before.json"
    python3 - <<'PY'
import json, os, shutil, stat, tempfile
from pathlib import Path
path = Path(os.environ["RECLAIM_CONFIG_PATH"]).resolve()
backup = Path(os.environ["PG_CONFIG_BACKUP"])
if path.is_symlink() or not path.is_file():
    raise SystemExit(f"config is not a regular non-symlink file: {path}")
with open(path, "r", encoding="utf-8") as handle:
    cfg = json.load(handle)
shutil.copy2(path, backup)
os.chmod(backup, 0o600)
# PostgreSQL offline mode deliberately leaves goal_legacy_journal_dual_write
# unchanged because it did not prove response-alias coverage.  These bounds do
# not discard any current data.
cfg["goal_retention_days"] = max(7, int(cfg.get("goal_retention_days") or 0))
cfg["codex_session_mapping_retention_days"] = max(
    7, int(cfg.get("codex_session_mapping_retention_days") or 0)
)
cfg["usage_journal_segment_bytes"] = 8 << 20
cfg["body_disk_reserve_bytes"] = max(
    512 << 20, int(cfg.get("body_disk_reserve_bytes") or 0)
)
fd, temp_name = tempfile.mkstemp(prefix="." + path.name + ".reclaim-", dir=path.parent)
try:
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        json.dump(cfg, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temp_name, stat.S_IMODE(path.stat().st_mode))
    os.replace(temp_name, path)
finally:
    if os.path.exists(temp_name):
        os.unlink(temp_name)
PY
  fi
  {
    printf 'ok=true\n'
    printf 'accounts_before_rows=%s\naccounts_after_rows=%s\n' "$PG_ACCOUNT_COUNT_BEFORE" "$PG_ACCOUNT_COUNT_AFTER"
    printf 'accounts_before_sha256=%s\naccounts_after_sha256=%s\n' "$PG_ACCOUNT_BEFORE" "$PG_ACCOUNT_AFTER"
    printf 'contexts_before_rows=%s\ncontexts_after_rows=%s\n' "$PG_CONTEXT_COUNT_BEFORE" "$PG_CONTEXT_COUNT_AFTER"
    printf 'contexts_before_sha256=%s\ncontexts_after_sha256=%s\n' "$PG_CONTEXT_BEFORE" "$PG_CONTEXT_AFTER"
    printf 'logs_before_rows=%s\nlogs_retained_before_rows=%s\nlogs_after_rows=%s\n' "$PG_LOGS_ALL_COUNT_BEFORE" "$PG_LOGS_RETAINED_COUNT_BEFORE" "$PG_LOGS_COUNT_AFTER"
    printf 'logs_before_sha256=%s\nlogs_retained_before_sha256=%s\nlogs_after_sha256=%s\n' "$PG_LOGS_ALL_BEFORE" "$PG_LOGS_RETAINED_BEFORE" "$PG_LOGS_AFTER"
    printf 'legacy_goal_dual_write=unchanged_coverage_unproven\n'
    printf 'rollback_backup=%s\n' "$PG_BACKUP"
  } >"$PG_REPORT"
  chmod 600 "$PG_REPORT"
  note "PostgreSQL reclamation verified: $PG_REPORT"
  note "Rollback dump: $PG_BACKUP"
fi
