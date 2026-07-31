#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/backend-mail-automation-install-20260731}"
SOURCE_DIR="${SOURCE_DIR:-/root/autodl-tmp/backend-mail-automation-20260731/src}"
GO_ROOT="${GO_ROOT:-/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12}"
NODE_ROOT="${NODE_ROOT:-/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64}"
CHROME_BIN="${CHROME_BIN:-/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome}"
PORT="${PORT:-34277}"
REAUTH_PORT="${REAUTH_PORT:-34279}"
RELEASE_ID="${RELEASE_ID:-backend-mail-automation-install-20260731}"

mkdir -p "$ROOT"/{logs,records,run,etc,data,prefix,systemd}
export PATH="${GO_ROOT}/bin:${NODE_ROOT}/bin:${PATH}"
export GOTOOLCHAIN=local
export SERVICE_NAME="codex-pool-backend-mail-install"
export HANDOFF_SERVICE_NAME="${SERVICE_NAME}-handoff"
export SERVICE_USER=root
export SERVICE_GROUP=root
export INSTALL_PREFIX="${ROOT}/prefix"
export BIN_DIR="${ROOT}/prefix/bin"
export APP_DIR="${ROOT}/prefix/lib/codex-pool"
export CONFIG_DIR="${ROOT}/etc"
export CONFIG_FILE="${ROOT}/etc/config.json"
export DATA_DIR="${ROOT}/data"
export DATABASE_PATH="${ROOT}/data/pool.sqlite3"
export SYSTEMD_DIR="${ROOT}/systemd"
export BUILD_DIR="${ROOT}/build"
export DEPLOY_LOCK_FILE="${ROOT}/run/install.lock"
export HANDOFF_CONTROL_SOCKET="${ROOT}/data/run/handoff-control.sock"
export HANDOFF_PAUSE_STATE="${ROOT}/data/run/admission-paused.json"
export LISTEN_ADDR="127.0.0.1:${PORT}"
export RELEASE_ID_OVERRIDE="$RELEASE_ID"
export RUN_TESTS=0
export INSTALL_SYSTEMD=0
export START_SERVICE=0
export WITH_SIDECAR=0
export WITH_REGISTRATION=1
export WITH_WARP=0
export MIGRATE_USER_GROUPS=0
export OPEN_FIREWALL=0
export INSTALL_GO=0
export GO_BIN="${GO_ROOT}/bin/go"
export SKIP_OS_PACKAGES=1
export NODE_BIN="${NODE_ROOT}/bin/node"
export CHROME_BIN
export REGISTRAR_INSTALL="${ROOT}/data/registrar"
export CODEX_REAUTH_ADDR="127.0.0.1:${REAUTH_PORT}"
export CODEX_REAUTH_CONCURRENCY=2

TOKEN_FILE="${ROOT}/records/admin.token"
if [[ ! -s "$TOKEN_FILE" ]]; then
  umask 077
  openssl rand -hex 24 >"$TOKEN_FILE"
fi
export ADMIN_TOKEN
ADMIN_TOKEN="$(cat "$TOKEN_FILE")"

cd "$SOURCE_DIR"
set +e
./install.sh \
  --full \
  --without-sidecar \
  --with-registration \
  --without-warp \
  --no-systemd \
  --no-start \
  --no-tests \
  --without-go-install \
  --no-open-firewall \
  --no-migrate-user-groups \
  --listen-addr "$LISTEN_ADDR" \
  > >(tee "${ROOT}/logs/install.literal.log") \
  2> >(tee "${ROOT}/logs/install.stderr.log" >&2)
status=$?
set -e
printf '%s\n' "$status" >"${ROOT}/records/install.status"
(( status == 0 )) || exit "$status"

release_dir="${APP_DIR}/releases/${RELEASE_ID}"
test -x "${BIN_DIR}/codex-pool-server"
test -x "${release_dir}/registrar-python-venv/bin/python"
test -x "${release_dir}/codex-reauth/codex_reauth_worker.py"
test -x "${release_dir}/codex-reauth/codex_register/login_oauth.py"
test -r "${release_dir}/codex-reauth/codex_register/phone_verify.py"
test -d "${release_dir}/registrar-node/node_modules"
python3 - "$CONFIG_FILE" "$CODEX_REAUTH_ADDR" <<'PY'
import json, sys
path, address = sys.argv[1:]
config = json.load(open(path, encoding="utf-8"))
expected = "http://" + address
assert config["codex_reauth_worker_url"] == expected, (config["codex_reauth_worker_url"], expected)
print("CONFIG_REAUTH_URL=" + expected)
PY

pkill -F "${ROOT}/run/server.pid" 2>/dev/null || true
pkill -F "${ROOT}/run/reauth.pid" 2>/dev/null || true
nohup "${release_dir}/registrar-python-venv/bin/python" \
  "${release_dir}/codex-reauth/codex_reauth_worker.py" \
  --host 127.0.0.1 --port "$REAUTH_PORT" --concurrency 2 \
  >"${ROOT}/logs/reauth.log" 2>&1 </dev/null &
echo $! >"${ROOT}/run/reauth.pid"
nohup "${BIN_DIR}/codex-pool-server" --config "$CONFIG_FILE" --release-id "$RELEASE_ID" \
  >"${ROOT}/logs/server.log" 2>&1 </dev/null &
echo $! >"${ROOT}/run/server.pid"

python3 - "$PORT" "$REAUTH_PORT" "$RELEASE_ID" <<'PY'
import json, sys, time, urllib.request
port, reauth_port, release = sys.argv[1:]
targets = [
    (f"http://127.0.0.1:{port}/readyz", lambda d: d.get("ready") and d.get("release_id") == release),
    (f"http://127.0.0.1:{reauth_port}/healthz", lambda d: d == {"ready": True, "service": "codex-reauth-worker"}),
]
for url, accept in targets:
    last = ""
    for _ in range(160):
        try:
            with urllib.request.urlopen(url, timeout=1) as response:
                data = json.load(response)
            if response.status == 200 and accept(data):
                print("READY " + url + " " + json.dumps(data, sort_keys=True))
                break
            last = repr(data)
        except Exception as error:
            last = type(error).__name__ + ": " + str(error)
        time.sleep(0.25)
    else:
        raise SystemExit("health timeout " + url + " last=" + last)
PY

sha256sum "${BIN_DIR}/codex-pool-server" >"${ROOT}/records/installed-binary.sha256"
printf 'INSTALL_CURRENT_EXIT=0\nRELEASE_ID=%s\nPORT=%s\nREAUTH_PORT=%s\n' \
  "$RELEASE_ID" "$PORT" "$REAUTH_PORT"
