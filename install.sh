#!/usr/bin/env bash
# install.sh — friendly entry point for a FIRST-TIME install.
#
# The real, authoritative installer is scripts/install.sh. It:
#   - builds the Go gateway WITH the embedded admin UI (-> codex-pool-server),
#   - installs the curl_cffi sidecar and registration workers,
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
#     sudo ./install.sh --migrate-user-groups       # skip the interactive question
#     sudo ./install.sh --no-migrate-user-groups    # keep account-pool groups separate
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
echo "==> 转交给规范安装器 scripts/install.sh（构建内嵌 UI 的二进制、安装 sidecar/注册 worker、写入 systemd）"
echo "==> Codex skills 兼容提示：完整官方 skills/plugins/Browser Use 体验请使用官方 Codex 账号通道；第三方 API 通道为 best-effort。"
echo

# The legacy compatibility migration copies every account-pool group into a
# same-named user-facing routing group. That is useful for some upgrades, but
# recreates deliberately removed user groups on the next service start. Ask on
# the friendly entry point and default to keeping the two group layers separate.
# Automation can bypass the question with either documented flag or by setting
# MIGRATE_USER_GROUPS=0/1.
migration_choice_set=0
help_requested=0
for arg in "$@"; do
  case "$arg" in
    --migrate-user-groups|--no-migrate-user-groups) migration_choice_set=1 ;;
    -h|--help) help_requested=1 ;;
  esac
done

if [[ "$help_requested" == "0" && "$migration_choice_set" == "0" && ! -v MIGRATE_USER_GROUPS ]]; then
  migration_answer=""
  if [[ -t 0 ]]; then
    printf '是否将现有账号池分组迁移为同名用户分组？这可能会创建多个用户分组。 [y/N]: '
    IFS= read -r migration_answer || true
  else
    echo "==> 非交互执行：默认跳过账号池分组→用户分组迁移"
  fi
  case "$migration_answer" in
    y|Y|yes|YES|Yes|是) set -- "$@" --migrate-user-groups ;;
    *) set -- "$@" --no-migrate-user-groups ;;
  esac
fi

exec bash scripts/install.sh "$@"
