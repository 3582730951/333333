#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
BACKUP="$ROOT/backup-pre-upgrade"

baseline_config="$(awk '$2 ~ /config.json$/ {print $1}' "$BACKUP/baseline.sha256")"
baseline_db="$(awk '$2 ~ /pool.sqlite3$/ {print $1}' "$BACKUP/baseline.sha256")"
current_config="$(sha256sum "$ROOT/etc/config.json" | awk '{print $1}')"
current_db="$(sha256sum "$ROOT/state/pool.sqlite3" | awk '{print $1}')"
test "$current_config" = "$baseline_config"
test "$current_db" = "$baseline_db"
test "$(cat "$ROOT/logs/new-install.exit")" = 0

current_target="$(readlink "$ROOT/prefix/lib/codex-pool/current")"
case "$current_target" in
  *new-goal-context-fix) ;;
  *) echo "unexpected current target: $current_target" >&2; exit 1 ;;
esac
old_release="$ROOT/prefix/lib/codex-pool/releases/old-0873de57"
test -x "$old_release/codex-pool-server"

{
  echo "upgrade_install_exit=0"
  echo "config_sha256_before=$baseline_config"
  echo "config_sha256_after_install=$current_config"
  echo "database_sha256_before=$baseline_db"
  echo "database_sha256_after_install=$current_db"
  echo "current_release=$current_target"
  echo "previous_release=not-created-in-no-systemd-mode"
  echo "rollback_release=$old_release"
  echo "new_binary_sha256=$(sha256sum "$ROOT/prefix/bin/codex-pool-server" | awk '{print $1}')"
  echo "old_binary_sha256=$(sha256sum "$old_release/codex-pool-server" | awk '{print $1}')"
} | tee "$ROOT/logs/upgrade-install-verification.txt"
