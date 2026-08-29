#!/usr/bin/env bash
# Remove repository-owned source files left by additive uploads. The manifest is
# generated from the release commit, so files dropped by a newer release no longer
# influence compilation. Runtime data, configs, plugins, artifacts and dependencies
# are outside the managed roots and are never traversed.
set -Eeuo pipefail

ROOT="${PROJECT_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST="${MANAGED_SOURCE_MANIFEST:-${ROOT}/scripts/managed-source-manifest.txt}"

case "${PRUNE_MANAGED_SOURCE:-1}" in
  0|false|FALSE|no|NO|off|OFF)
    printf '==> 跳过受管源码清理（PRUNE_MANAGED_SOURCE=0）\n'
    exit 0
    ;;
esac

if [[ ! -s "$MANIFEST" ]]; then
  printf 'WARN: 受管源码清单不存在，跳过清理：%s\n' "$MANIFEST" >&2
  exit 0
fi

if grep -Eq '(^/|(^|/)\.\.(/|$)|[[:cntrl:]])' "$MANIFEST"; then
  printf 'ERROR: 受管源码清单包含不安全路径：%s\n' "$MANIFEST" >&2
  exit 1
fi

# Validate the new SPA closure before removing anything. Additive uploads may
# legitimately contain old hashed chunks, so those extras are allowed only for
# this pre-prune check; every file declared by the new release and every asset
# referenced by its index.html must already be present.
if [[ -f "${ROOT}/scripts/verify-console-release.sh" ]]; then
  PROJECT_ROOT="$ROOT" \
    MANAGED_SOURCE_MANIFEST="$MANIFEST" \
    CONSOLE_RELEASE_ALLOW_STALE=1 \
    bash "${ROOT}/scripts/verify-console-release.sh"
fi

managed_roots=(
  cmd internal services sidecar modules scripts deploy web-spa/src web-spa/scripts
  workers/node-registrar/src
)
candidate_file="$(mktemp)"
trap 'rm -f "$candidate_file"' EXIT

{
  for relative_root in "${managed_roots[@]}"; do
    [[ -d "${ROOT}/${relative_root}" ]] || continue
    find "${ROOT}/${relative_root}" -type f \
      ! -path '*/test/*' ! -path '*/tests/*' ! -path '*/testdata/*' \
      ! -path '*/cmd/extreme-load/*' \
      ! -name '*_test.*' ! -name 'test_*.*' ! -name 'test-*.*' \
      ! -name '*-test.*' ! -name '*.test.*' ! -name '*_spec.*' \
      ! -name 'spec_*.*' ! -name 'spec-*.*' ! -name '*-spec.*' \
      ! -name '*.spec.*' ! -name '*selftest*' \
      ! -name 'playwright.config.*' ! -name 'vitest.config.*' \
      ! -name 'jest.config.*' ! -path '*/scripts/ci.sh' \
      \( -name '*.go' -o -name '*.py' -o -name '*.js' -o -name '*.jsx' \
         -o -name '*.ts' -o -name '*.tsx' -o -name '*.css' -o -name '*.json' \
         -o -name '*.jsonc' -o -name '*.mjs' -o -name '*.sql' -o -name '*.sh' \) -print
  done
  # The embedded SPA is a release artifact, not ordinary source: manage every
  # file regardless of extension (HTML, fonts, images, source maps, and chunks).
  if [[ -d "${ROOT}/internal/console/dist" ]]; then
    find "${ROOT}/internal/console/dist" -type f -print
  fi
} | sed "s#^${ROOT%/}/##" | sort -u >"$candidate_file"

removed=0
while IFS= read -r relative; do
  [[ -n "$relative" ]] || continue
  if ! grep -Fqx -- "$relative" "$MANIFEST"; then
    rm -f -- "${ROOT}/${relative}"
    printf '==> 清理新版清单外的历史受管源码：%s\n' "$relative"
    removed=$((removed + 1))
  fi
done <"$candidate_file"

printf '==> 受管源码同步完成：移除 %d 个旧文件；配置、数据与插件目录未触碰\n' "$removed"
