#!/usr/bin/env bash
set -euo pipefail

artifact_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo="${1:-}"
if [[ -z "$repo" ]]; then
  repo="$(git -C "$artifact_dir/../.." rev-parse --show-toplevel)"
fi
repo="$(cd -- "$repo" && pwd)"

patch="$artifact_dir/optimization-source.patch"
dist_archive="$artifact_dir/console-dist-before.tar.gz"
dist_manifest="$artifact_dir/console-dist-before-sha256.txt"
source_manifest="$artifact_dir/source-before-sha256.txt"

[[ -f "$patch" && -f "$dist_archive" && -f "$dist_manifest" && -f "$source_manifest" ]]
cd "$repo"

# The reverse preflight makes rollback atomic with respect to source changes.
git apply --check -R "$patch"
git apply -R "$patch"
sha256sum -c "$source_manifest"

rm -rf internal/console/dist
tar -xzf "$dist_archive" -C "$repo"
sha256sum -c "$dist_manifest"

# A forward preflight proves that the restored source is exactly the patch base.
git apply --check "$patch"
[[ ! -e internal/api/request_metrics.go ]]
[[ ! -e internal/api/request_metrics_test.go ]]
[[ ! -e web-spa/tests/browser-download.test.ts ]]
[[ ! -e docs/项目全栈优化审计-2026-07-29.md ]]

printf 'ROLLBACK_EXIT=0\n'
printf 'ROLLBACK_REPO=%s\n' "$repo"
printf 'ROLLBACK_SOURCE_BASE=verified\n'
printf 'ROLLBACK_CONSOLE_FILES=%s\n' "$(wc -l < "$dist_manifest" | tr -d ' ')"
