#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
SOURCE_DIR="${SOURCE_DIR:?SOURCE_DIR is required}"
PHASE="${PHASE:?PHASE is required}"
RELEASE_ID="${RELEASE_ID:?RELEASE_ID is required}"
PORT="${PORT:-34276}"
GO_ROOT="${GO_ROOT:-/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12}"
NODE_ROOT="${NODE_ROOT:-/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64}"
TOKEN_FILE="${ROOT}/records/admin.token"

export PATH="${GO_ROOT}/bin:${NODE_ROOT}/bin:${PATH}"
export SERVICE_NAME="codex-pool-upgrade-fixture"
export HANDOFF_SERVICE_NAME="codex-pool-upgrade-fixture-handoff"
export SERVICE_USER="root"
export SERVICE_GROUP="root"
export INSTALL_PREFIX="${ROOT}/prefix"
export BIN_DIR="${ROOT}/prefix/bin"
export APP_DIR="${ROOT}/prefix/lib/codex-pool"
export CONFIG_DIR="${ROOT}/etc"
export CONFIG_FILE="${ROOT}/etc/config.json"
export DATA_DIR="${ROOT}/data"
export DATABASE_PATH="${ROOT}/data/pool.sqlite3"
export SYSTEMD_DIR="${ROOT}/systemd"
export BUILD_DIR="${ROOT}/build-${PHASE}"
export DEPLOY_LOCK_FILE="${ROOT}/run/install.lock"
export HANDOFF_CONTROL_SOCKET="${ROOT}/data/run/handoff-control.sock"
export HANDOFF_PAUSE_STATE="${ROOT}/data/run/admission-paused.json"
export LISTEN_ADDR="127.0.0.1:${PORT}"
export RELEASE_ID_OVERRIDE="$RELEASE_ID"
export RUN_TESTS=0
export INSTALL_SYSTEMD=0
export START_SERVICE=0
export WITH_SIDECAR=0
export WITH_REGISTRATION=0
export WITH_WARP=0
export MIGRATE_USER_GROUPS=0
export OPEN_FIREWALL=0
export INSTALL_GO=0
export GO_BIN="${GO_ROOT}/bin/go"
export SKIP_OS_PACKAGES=1

if [[ ! -s "$TOKEN_FILE" ]]; then
  umask 077
  openssl rand -hex 24 >"$TOKEN_FILE"
fi
export ADMIN_TOKEN
ADMIN_TOKEN="$(cat "$TOKEN_FILE")"

cd "$SOURCE_DIR"
set +e
./install.sh \
  --minimal \
  --no-systemd \
  --no-start \
  --no-tests \
  --without-go-install \
  --no-open-firewall \
  --no-migrate-user-groups \
  --listen-addr "$LISTEN_ADDR" \
  > >(tee "${ROOT}/logs/install-${PHASE}.literal.log") \
  2> >(tee "${ROOT}/logs/install-${PHASE}.stderr.log" >&2)
status=$?
set -e
printf '%s\n' "$status" >"${ROOT}/records/install-${PHASE}.exit-status"
printf 'INSTALL_PHASE=%s\nINSTALL_EXIT=%s\n' "$PHASE" "$status"
exit "$status"
