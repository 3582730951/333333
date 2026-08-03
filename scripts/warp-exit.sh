#!/usr/bin/env bash
# warp-exit.sh — provision and manage N independent Cloudflare WARP exits using the
# UNOFFICIAL userspace stack (wgcf + wireproxy), NOT the official warp-cli (which gives
# only one exit per host and a 10s proxy timeout that breaks SSE).
#
# Each exit i (1-based) is an independent free WARP identity (wgcf) served as a local
# SOCKS5 listener on port WARP_BASE_PORT+i-1 by its own wireproxy instance — a distinct
# WARP exit IP per exit, so the pool server can pack ≤3 accounts per exit.
#
# Layout (under WARP_DIR, default /var/lib/codex-pool/warp):
#   bin/wgcf, bin/wireproxy            shared binaries
#   exit-<i>/wgcf-account.toml         the WARP identity (re-register → new IP)
#   exit-<i>/wgcf-profile.conf         WireGuard config generated from the identity
#   exit-<i>/wireproxy.conf            wireproxy config (WG + [Socks5] on the exit port)
#
# Each exit runs as the systemd template unit codex-pool-warp@<i>.service, so the pool
# server (running unprivileged) can ask for a re-registration via this script; the
# script uses `sudo -n systemctl` when not already root (install.sh adds a sudoers
# drop-in scoped to exactly these units + this script).
#
# Commands:
#   install-deps                 download wgcf + wireproxy if missing
#   register <i>                 register exit i if absent (idempotent)
#   provision <N>                register exits 1..N + write all configs
#   reregister <i>               new WARP identity (new IP) for exit i + restart it
#   restart <i>                  restart exit i's wireproxy unit
#   verify <i>                   curl the WARP trace through exit i (expects warp=on)
#   write-config <i>             (re)write exit i's wireproxy.conf only
set -Eeuo pipefail

WARP_DIR="${WARP_DIR:-/var/lib/codex-pool/warp}"
WARP_BASE_PORT="${WARP_BASE_PORT:-40000}"
WARP_BIN_DIR="${WARP_BIN_DIR:-${WARP_DIR}/bin}"
WGCF_BIN="${WGCF_BIN:-${WARP_BIN_DIR}/wgcf}"
WIREPROXY_BIN="${WIREPROXY_BIN:-${WARP_BIN_DIR}/wireproxy}"
WGCF_VERSION="${WGCF_VERSION:-2.2.22}"
WIREPROXY_VERSION="${WIREPROXY_VERSION:-1.0.9}"
SERVICE_NAME="${SERVICE_NAME:-codex-pool}"
WARP_UNIT="${WARP_UNIT:-${SERVICE_NAME}-warp@}"

log() { printf '==> warp-exit: %s\n' "$*"; }
warn() { printf 'WARN: warp-exit: %s\n' "$*" >&2; }
die() { printf 'ERROR: warp-exit: %s\n' "$*" >&2; exit 1; }

exit_port() { echo "$(( WARP_BASE_PORT + $1 - 1 ))"; }
exit_dir() { echo "${WARP_DIR}/exit-$1"; }

# systemctl wrapper: direct as root, else via passwordless sudo (install.sh grants a
# scoped sudoers rule). A missing systemd is non-fatal for register/provision (the
# operator may run wireproxy another way), only restart/reregister need it.
sysctl() {
  if [[ "$(id -u)" -eq 0 ]]; then
    systemctl "$@"
  elif command -v sudo >/dev/null 2>&1; then
    sudo -n systemctl "$@"
  else
    return 1
  fi
}

arch_tag() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    armv7l|armv6l) echo arm ;;
    *) die "unsupported arch for warp binaries: $(uname -m)" ;;
  esac
}

install_deps() {
  mkdir -p "$WARP_BIN_DIR"
  local arch; arch="$(arch_tag)"
  if [[ ! -x "$WGCF_BIN" ]]; then
    local url="https://github.com/ViRb3/wgcf/releases/download/v${WGCF_VERSION}/wgcf_${WGCF_VERSION}_linux_${arch}"
    log "downloading wgcf ${WGCF_VERSION} (${arch})"
    curl -fsSL --retry 3 -o "$WGCF_BIN" "$url" || die "wgcf download failed: $url"
    chmod +x "$WGCF_BIN"
  fi
  if [[ ! -x "$WIREPROXY_BIN" ]]; then
    local tmp; tmp="$(mktemp -d)"
    local url="https://github.com/whyvl/wireproxy/releases/download/v${WIREPROXY_VERSION}/wireproxy_linux_${arch}.tar.gz"
    log "downloading wireproxy ${WIREPROXY_VERSION} (${arch})"
    if curl -fsSL --retry 3 -o "${tmp}/wp.tgz" "$url"; then
      tar -C "$tmp" -xzf "${tmp}/wp.tgz" || die "wireproxy extract failed"
      local found; found="$(find "$tmp" -type f -name wireproxy | head -n1)"
      [[ -n "$found" ]] || die "wireproxy binary not found in archive"
      install -m 0755 "$found" "$WIREPROXY_BIN"
    else
      rm -rf "$tmp"; die "wireproxy download failed: $url"
    fi
    rm -rf "$tmp"
  fi
}

# register an exit's WARP identity (idempotent: keeps an existing account.toml).
register_exit() {
  local i="$1" dir; dir="$(exit_dir "$i")"
  mkdir -p "$dir"
  install_deps
  if [[ ! -f "${dir}/wgcf-account.toml" ]]; then
    log "registering WARP identity for exit ${i}"
    ( cd "$dir" && WGCF_ACCEPT_TOS=1 "$WGCF_BIN" register --accept-tos --config "${dir}/wgcf-account.toml" ) \
      || die "wgcf register failed for exit ${i}"
  fi
  ( cd "$dir" && "$WGCF_BIN" generate --config "${dir}/wgcf-account.toml" --profile "${dir}/wgcf-profile.conf" ) \
    || die "wgcf generate failed for exit ${i}"
  write_config "$i"
}

# write_config builds wireproxy.conf = wgcf's WireGuard profile + a [Socks5] listener.
write_config() {
  local i="$1" dir port; dir="$(exit_dir "$i")"; port="$(exit_port "$i")"
  [[ -f "${dir}/wgcf-profile.conf" ]] || die "exit ${i} has no wgcf profile; run register first"
  {
    # wireproxy reads wg-quick style [Interface]/[Peer]; strip DNS (wireproxy resolves
    # via its own stack) and append the SOCKS5 listener bound to localhost only.
    grep -viE '^\s*DNS\s*=' "${dir}/wgcf-profile.conf"
    printf '\n[Socks5]\nBindAddress = 127.0.0.1:%s\n' "$port"
  } > "${dir}/wireproxy.conf"
  log "wrote exit ${i} config (socks5 127.0.0.1:${port})"
}

provision() {
  local n="$1"
  [[ "$n" =~ ^[0-9]+$ && "$n" -ge 1 ]] || die "provision needs a positive count"
  install_deps
  for (( i=1; i<=n; i++ )); do register_exit "$i"; done
  log "provisioned ${n} exits"
}

reregister() {
  local i="$1" dir; dir="$(exit_dir "$i")"
  [[ "$i" =~ ^[0-9]+$ ]] || die "reregister needs an exit index"
  log "re-registering exit ${i} for a fresh WARP IP"
  rm -f "${dir}/wgcf-account.toml" "${dir}/wgcf-profile.conf"
  register_exit "$i"
  restart_exit "$i" || warn "could not restart exit ${i} (no systemd?); restart it manually"
}

restart_exit() {
  local i="$1"
  sysctl restart "${WARP_UNIT}${i}.service"
}

verify_exit() {
  local i="$1" port; port="$(exit_port "$i")"
  curl -fsS --max-time 15 -x "socks5h://127.0.0.1:${port}" https://www.cloudflare.com/cdn-cgi/trace | grep -E '^warp=' \
    || die "exit ${i} did not report warp= (not connected?)"
}

main() {
  local cmd="${1:-}"; shift || true
  case "$cmd" in
    install-deps) install_deps ;;
    register) register_exit "${1:?exit index}" ;;
    provision) provision "${1:?count}" ;;
    reregister) reregister "${1:?exit index}" ;;
    restart) restart_exit "${1:?exit index}" ;;
    write-config) write_config "${1:?exit index}" ;;
    verify) verify_exit "${1:?exit index}" ;;
    *) die "usage: warp-exit.sh {install-deps|register <i>|provision <N>|reregister <i>|restart <i>|write-config <i>|verify <i>}" ;;
  esac
}

main "$@"
