#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
source "$ROOT/install-env.sh"
export GO_BIN="$GO_INSTALL_ROOT/go1.25.12/bin/go"
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=sum.golang.google.cn

old_pid="$(cat "$ROOT/old.pid")"
if kill -0 "$old_pid" 2>/dev/null; then
  kill "$old_pid"
  for _ in $(seq 1 100); do
    kill -0 "$old_pid" 2>/dev/null || break
    sleep 0.1
  done
fi
if kill -0 "$old_pid" 2>/dev/null; then
  echo "old worker did not stop: $old_pid" >&2
  exit 1
fi

BACKUP="$ROOT/backup-pre-upgrade"
rm -rf "$BACKUP"
mkdir -p "$BACKUP"
cp -a "$ROOT/etc" "$BACKUP/etc"
cp -a "$ROOT/state" "$BACKUP/state"
readlink "$ROOT/prefix/lib/codex-pool/current" >"$BACKUP/current-link.txt"
if test -L "$ROOT/prefix/lib/codex-pool/previous"; then
  readlink "$ROOT/prefix/lib/codex-pool/previous" >"$BACKUP/previous-link.txt"
fi
sha256sum \
  "$ROOT/etc/config.json" \
  "$ROOT/state/pool.sqlite3" \
  "$ROOT/prefix/bin/codex-pool-server" \
  >"$BACKUP/baseline.sha256"

set +e
(
  export BUILD_DIR="$ROOT/build-new"
  export RELEASE_ID_OVERRIDE=new-goal-context-fix
  cd "$ROOT/new-src"
  ./install.sh \
    --minimal \
    --no-systemd \
    --no-start \
    --no-tests \
    --no-migrate-user-groups \
    --listen-addr "127.0.0.1:$PORT"
) 2>&1 | tee "$ROOT/logs/new-install.log"
status=${PIPESTATUS[0]}
set -e
printf '%s\n' "$status" >"$ROOT/logs/new-install.exit"
test "$status" -eq 0

baseline_config="$(awk '$2 ~ /config.json$/ {print $1}' "$BACKUP/baseline.sha256")"
baseline_db="$(awk '$2 ~ /pool.sqlite3$/ {print $1}' "$BACKUP/baseline.sha256")"
current_config="$(sha256sum "$ROOT/etc/config.json" | awk '{print $1}')"
current_db="$(sha256sum "$ROOT/state/pool.sqlite3" | awk '{print $1}')"
test "$current_config" = "$baseline_config"
test "$current_db" = "$baseline_db"

current_target="$(readlink "$ROOT/prefix/lib/codex-pool/current")"
case "$current_target" in
  *new-goal-context-fix) ;;
  *) echo "unexpected current target: $current_target" >&2; exit 1 ;;
esac
test -x "$ROOT/prefix/lib/codex-pool/releases/old-0873de57/codex-pool-server"
previous_target="$(readlink "$ROOT/prefix/lib/codex-pool/previous" 2>/dev/null || true)"

{
  echo "upgrade_install_exit=$status"
  echo "config_sha256_before=$baseline_config"
  echo "config_sha256_after_install=$current_config"
  echo "database_sha256_before=$baseline_db"
  echo "database_sha256_after_install=$current_db"
  echo "current_release=$current_target"
  echo "previous_release=${previous_target:-not-created-in-no-systemd-mode}"
  echo "rollback_release=$ROOT/prefix/lib/codex-pool/releases/old-0873de57"
  echo "new_binary_sha256=$(sha256sum "$ROOT/prefix/bin/codex-pool-server" | awk '{print $1}')"
  echo "old_binary_sha256=$(sha256sum "$ROOT/prefix/lib/codex-pool/releases/old-0873de57/codex-pool-server" | awk '{print $1}')"
} | tee "$ROOT/logs/upgrade-install-verification.txt"
