#!/usr/bin/env bash
#
# Zero-configuration, application-scoped disk reclamation.
#
# Run:
#   sudo ./scripts/maintenance/reclaim-space-now.sh
#
# It discovers the installed config, the exact SQLite owner service (when one
# exists), and a suitable backup directory, then delegates all preservation,
# backup, verification, rollback and reclamation work to
# reclaim-disk-space.sh.  It never performs host-wide cleanup.
set -Eeuo pipefail

umask 077

PROGRAM=${0##*/}
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
RECLAIM_SCRIPT="$SCRIPT_DIR/reclaim-disk-space.sh"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 0 ]] || die "$PROGRAM takes no arguments; run it directly"
[[ $EUID -eq 0 ]] || die "run with sudo: sudo $0"
[[ -x $RECLAIM_SCRIPT ]] || die "missing executable: $RECLAIM_SCRIPT"
command -v python3 >/dev/null 2>&1 || die "python3 is required"

config_path=${CODEX_POOL_CONFIG:-}
if [[ -n $config_path ]]; then
  [[ -f $config_path && ! -L $config_path ]] ||
    die "CODEX_POOL_CONFIG is not a regular non-symlink file: $config_path"
else
  for candidate in \
    /etc/codex-pool/config.json \
    /var/lib/codex-pool/config.json \
    "$SCRIPT_DIR/../../config.json"
  do
    if [[ -f $candidate && ! -L $candidate ]]; then
      config_path=$candidate
      break
    fi
  done
fi

database_path=
data_dir=
driver=sqlite
if [[ -n $config_path ]]; then
  mapfile -d '' -t values < <(
    python3 - "$config_path" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as handle:
    cfg = json.load(handle)
for key in ("database_path", "data_dir", "storage_driver"):
    sys.stdout.write(str(cfg.get(key, "") or ""))
    sys.stdout.write("\0")
PY
  )
  database_path=${values[0]:-}
  data_dir=${values[1]:-}
  driver=${values[2]:-sqlite}
fi

extra_args=()
if [[ -n $config_path ]]; then
  extra_args+=(--config "$config_path")
else
  candidates=()
  for candidate in \
    "$PWD/var/data/pool.sqlite3" \
    "$PWD/pool.sqlite3" \
    "$PWD/data/pool.sqlite3" \
    /var/lib/codex-pool/pool.sqlite3
  do
    [[ -f $candidate && ! -L $candidate ]] && candidates+=("$candidate")
  done
  ((${#candidates[@]} == 1)) ||
    die "no unique codex-pool config/database was discovered"
  database_path=${candidates[0]}
  data_dir=$(dirname "$database_path")/data
  extra_args+=(--db "$database_path" --data-dir "$data_dir")
fi

if [[ $driver == sqlite ]]; then
  [[ -n $database_path ]] ||
    database_path="$(dirname "${data_dir:-/var/lib/codex-pool/data}")/pool.sqlite3"
  database_path=$(python3 -c \
    'import os,sys; print(os.path.abspath(sys.argv[1]))' "$database_path")
  [[ -f $database_path && ! -L $database_path ]] ||
    die "SQLite database was not found: $database_path"
fi

backup_dir=${CODEX_POOL_BACKUP_DIR:-}
if [[ -z $backup_dir && -d /mnt/backup && -w /mnt/backup ]]; then
  backup_dir=/mnt/backup/codex-pool
fi
[[ -z $backup_dir ]] || extra_args+=(--backup-dir "$backup_dir")

if [[ $driver != sqlite ]]; then
  # PostgreSQL has no local DB descriptor from which to prove service
  # ownership.  Its configured deployment must already be quiescent.
  exec "$RECLAIM_SCRIPT" --apply --assume-quiesced --optimize-config \
    "${extra_args[@]}"
fi

open_pids=()
if command -v fuser >/dev/null 2>&1; then
  while IFS= read -r pid; do
    [[ $pid =~ ^[0-9]+$ ]] && open_pids+=("$pid")
  done < <(
    fuser "$database_path" "$database_path-wal" "$database_path-shm" \
      2>/dev/null | tr ' ' '\n' | sed '/^$/d' | sort -nu
  )
else
  for fd in /proc/[0-9]*/fd/*; do
    [[ -e $fd ]] || continue
    target=$(readlink "$fd" 2>/dev/null || true)
    case "$target" in
      "$database_path"|"$database_path-wal"|"$database_path-shm")
        pid=${fd#/proc/}; open_pids+=("${pid%%/*}") ;;
    esac
  done
  if ((${#open_pids[@]})); then
    mapfile -t open_pids < <(printf '%s\n' "${open_pids[@]}" | sort -nu)
  fi
fi

if ((${#open_pids[@]} == 0)); then
  printf '零配置回收：数据库无写入进程，开始备份、清理和完整验证。\n'
  if [[ -n $config_path ]]; then
    exec "$RECLAIM_SCRIPT" --apply --assume-quiesced --optimize-config \
      "${extra_args[@]}"
  fi
  exec "$RECLAIM_SCRIPT" --apply --assume-quiesced "${extra_args[@]}"
fi

command -v systemctl >/dev/null 2>&1 ||
  die "database is active but systemctl is unavailable"
units=()
for pid in "${open_pids[@]}"; do
  [[ -r /proc/$pid/cgroup ]] ||
    die "database owner PID disappeared during discovery: $pid"
  unit=$(sed -En 's#^.*/([^/]+\.service)(/.*)?$#\1#p' \
    "/proc/$pid/cgroup" | tail -n 1)
  [[ -n $unit ]] ||
    die "database owner PID $pid is not uniquely owned by a systemd service"
  [[ $(systemctl show "$unit" -p LoadState --value 2>/dev/null) == loaded ]] ||
    die "database owner service is not loaded: $unit"
  units+=("$unit")
done
mapfile -t units < <(printf '%s\n' "${units[@]}" | sort -u)
((${#units[@]} == 1)) ||
  die "database is shared by multiple services; automatic stop is not safe"

printf '零配置回收：将短暂停止并自动恢复 %s，账号和上下文保持不动。\n' \
  "${units[0]}"
if [[ -n $config_path ]]; then
  exec "$RECLAIM_SCRIPT" --one-click --service "${units[0]}" \
    "${extra_args[@]}"
fi
exec "$RECLAIM_SCRIPT" --apply --stop-service --service "${units[0]}" \
  "${extra_args[@]}"
