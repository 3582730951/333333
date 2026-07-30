#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/cpupg-20260730
OLD_ARCHIVE=/root/autodl-tmp/old-source-slim.tar.gz
NEW_ARCHIVE=/root/autodl-tmp/new-source-slim.tar.gz
OLD_SHA=089d84e1d5e24419aaf78854a2aee42b99bf801d0f7950c8d5dca114cb2ed944
NEW_SHA=d634fca1d6adcabcbb9b52e2696752e863adee5367ff08adb19f4a3a156eb7bb

test "$(sha256sum "$OLD_ARCHIVE" | awk '{print $1}')" = "$OLD_SHA"
test "$(sha256sum "$NEW_ARCHIVE" | awk '{print $1}')" = "$NEW_SHA"

case "$ROOT" in
  /root/autodl-tmp/cpupg-*) ;;
  *) echo "unexpected isolated root: $ROOT" >&2; exit 64 ;;
esac
rm -rf -- "$ROOT"
mkdir -p "$ROOT"/{logs,systemd}
tar -xzf "$OLD_ARCHIVE" -C "$ROOT"
tar -xzf "$NEW_ARCHIVE" -C "$ROOT"

PORT="$(python3 - <<'PY'
import socket
with socket.socket() as s:
    s.bind(("127.0.0.1", 0))
    print(s.getsockname()[1])
PY
)"
printf '%s\n' "$PORT" >"$ROOT/port"

cat >"$ROOT/install-env.sh" <<EOF
export ROOT='$ROOT'
export SERVICE_USER=root
export SERVICE_GROUP=root
export INSTALL_PREFIX='$ROOT/prefix'
export CONFIG_DIR='$ROOT/etc'
export DATA_DIR='$ROOT/state'
export SYSTEMD_DIR='$ROOT/systemd'
export DEPLOY_LOCK_FILE='$ROOT/install.lock'
export GO_INSTALL_ROOT='$ROOT/toolchains'
export SKIP_OS_PACKAGES=1
export INSTALL_SYSTEMD=0
export START_SERVICE=0
export WITH_SIDECAR=0
export WITH_REGISTRATION=0
export WITH_WARP=0
export PORT='$PORT'
EOF

(
  source "$ROOT/install-env.sh"
  export BUILD_DIR="$ROOT/build-old"
  export RELEASE_ID_OVERRIDE=old-0873de57
  cd "$ROOT/old-src"
  ./install.sh \
    --minimal \
    --no-systemd \
    --no-start \
    --no-tests \
    --no-migrate-user-groups \
    --listen-addr "127.0.0.1:$PORT"
) 2>&1 | tee "$ROOT/logs/old-install.log"
status=${PIPESTATUS[0]}
printf '%s\n' "$status" >"$ROOT/logs/old-install.exit"
test "$status" -eq 0

{
  echo "hostname=$(hostname)"
  echo "port=$PORT"
  echo "old_archive_sha256=$OLD_SHA"
  echo "new_archive_sha256=$NEW_SHA"
  echo "config=$(realpath "$ROOT/etc/config.json")"
  echo "binary=$(realpath "$ROOT/prefix/bin/codex-pool-server")"
  "$ROOT/prefix/bin/codex-pool-server" --version 2>&1 || true
} | tee "$ROOT/logs/stage1-summary.txt"
