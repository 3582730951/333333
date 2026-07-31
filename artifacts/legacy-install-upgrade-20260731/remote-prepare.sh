#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"

if [[ -f "${ROOT}/run/pid" ]]; then
  pid="$(cat "${ROOT}/run/pid" 2>/dev/null || true)"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in {1..50}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
  fi
fi

rm -rf "$ROOT"
mkdir -p "$ROOT"/{uploads,old-src,new-src,prefix,etc,data,systemd,logs,run,backups,screenshots,records}
chmod 0700 "$ROOT" "$ROOT/etc" "$ROOT/data" "$ROOT/run" "$ROOT/backups"
printf 'REMOTE_ROOT=%s\n' "$ROOT"
