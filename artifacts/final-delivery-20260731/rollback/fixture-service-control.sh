#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
PORT="${PORT:-34276}"
ACTION="${1:?start, stop, or status is required}"
TOKEN_FILE="${ROOT}/records/admin.token"
PID_FILE="${ROOT}/run/pid"

stop_service() {
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid"
    for _ in {1..100}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
}

start_service() {
  local release pid ready=""
  stop_service
  release="$(basename "$(readlink -f "${ROOT}/prefix/lib/codex-pool/current")")"
  nohup env \
    CODEX_POOL_RELEASE_ID="$release" \
    CODEX_POOL_DATABASE="${ROOT}/data/pool.sqlite3" \
    CODEX_POOL_LISTEN_ADDR="127.0.0.1:${PORT}" \
    CODEX_POOL_ADMIN_TOKEN_FILE="$TOKEN_FILE" \
    "${ROOT}/prefix/bin/codex-pool-server" \
    --config "${ROOT}/etc/config.json" \
    >"${ROOT}/logs/service-${release}.literal.log" 2>&1 &
  pid=$!
  printf '%s\n' "$pid" >"$PID_FILE"
  for _ in {1..180}; do
    if payload="$(curl --noproxy '*' -fsS --max-time 2 "http://127.0.0.1:${PORT}/readyz" 2>/dev/null)"; then
      ready="$payload"
      break
    fi
    kill -0 "$pid" 2>/dev/null || {
      tail -80 "${ROOT}/logs/service-${release}.literal.log" >&2
      exit 1
    }
    sleep 1
  done
  [[ -n "$ready" ]] || {
    tail -80 "${ROOT}/logs/service-${release}.literal.log" >&2
    exit 1
  }
  printf '%s\n' "$ready" >"${ROOT}/records/ready-${release}.json"
  printf 'SERVICE_PID=%s\nSERVICE_RELEASE=%s\nREADY=1\n' "$pid" "$release"
}

case "$ACTION" in
  start) start_service ;;
  stop) stop_service; printf 'STOPPED=1\n' ;;
  status)
    pid="$(cat "$PID_FILE" 2>/dev/null || true)"
    if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
      printf 'RUNNING=1\nPID=%s\n' "$pid"
      curl --noproxy '*' -fsS --max-time 2 "http://127.0.0.1:${PORT}/readyz"
      printf '\n'
    else
      printf 'RUNNING=0\n'
      exit 1
    fi
    ;;
  *) printf 'unknown action: %s\n' "$ACTION" >&2; exit 2 ;;
esac
