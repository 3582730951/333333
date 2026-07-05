#!/usr/bin/env bash
# install.sh — friendly entry point for a FIRST-TIME install.
#
# The real, authoritative installer is scripts/install.sh. It:
#   - builds the Go gateway WITH the embedded admin UI (-> codex-pool-server),
#   - installs the curl_cffi sidecar, gopay, and the lifecycle services (register/payment),
#   - writes the systemd units + activation socket and starts the service,
#   - preserves an existing config + the accounts SQLite DB.
#
# This wrapper just forwards to it so `./install.sh` and `./update.sh` always use the
# SAME, correct build/install path (the previous standalone install.sh built a different
# binary name and an incompatible config, which then failed to start — that is fixed by
# delegating here). It also runs the same stale-source self-heal that update.sh does, so a
# folder re-uploaded over an older release still compiles.
#
# First install (binds 0.0.0.0:8787, full feature set):
#     sudo ./install.sh
# Update an existing deploy instead (backs up + preserves accounts):
#     sudo ./update.sh
#
# Any scripts/install.sh flag passes straight through, e.g.:
#     sudo ./install.sh --minimal
#     sudo ./install.sh --listen-addr 127.0.0.1:8787 --without-warp --no-tests
#     ./install.sh --help            # full option list
#
# Just want a local dev binary, no systemd/root? Skip this script and run:
#     go build -o pool-server ./cmd/pool-server
set -Eeuo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$PROJECT_ROOT"

[[ -f scripts/install.sh ]] || {
  printf 'ERROR: scripts/install.sh not found next to install.sh — run this from inside the project folder\n' >&2
  exit 1
}

# Self-heal: drop source files removed in newer releases but left behind by an additive
# re-upload (same list update.sh uses), so the build does not fail with "redeclared".
STALE_SOURCES=(
  "internal/api/lifecycle_routes.go"   # consolidated into internal/api/lifecycle.go
)
for f in "${STALE_SOURCES[@]}"; do
  [[ -n "$f" && -e "${PROJECT_ROOT}/${f}" ]] || continue
  rm -f "${PROJECT_ROOT}/${f}" 2>/dev/null \
    && printf '==> 清理历史残留源码（增量上传残留）：%s\n' "$f"
done

echo "================================"
echo " Pool Server 安装 / Install"
echo "================================"
echo "==> 转交给规范安装器 scripts/install.sh（构建内嵌UI的二进制、安装 sidecar/gopay/生命周期服务、写入 systemd）"
echo "==> Codex skills 兼容提示：完整官方 skills/plugins/Browser Use 体验请使用官方 Codex 账号通道；第三方 API 通道为 best-effort。"
echo

exec bash scripts/install.sh "$@"
