#!/usr/bin/env bash
set -Eeuo pipefail

ROOT=/root/autodl-tmp/team-lifecycle-patch-verify-20260731
ARCHIVE=/root/autodl-tmp/source-patch-baseline.tar.gz
PATCH=/root/autodl-tmp/team-lifecycle-full-source.patch
MODIFIED_MANIFEST=/root/autodl-tmp/modified-source-files.sha256

rm -rf "$ROOT"
mkdir -p "$ROOT"
tar -xzf "$ARCHIVE" -C "$ROOT"
cd "$ROOT"

find internal web-spa -type f -print0 |
  sort -z |
  xargs -0 sha256sum >baseline-files.sha256

git apply --check "$PATCH"
printf 'PATCH_FORWARD_CHECK=0\n'
git apply "$PATCH"
sha256sum -c "$MODIFIED_MANIFEST" >modified-reopen.log
printf 'MODIFIED_REOPEN_OK=%s\n' "$(wc -l <modified-reopen.log)"

git apply --reverse --check "$PATCH"
printf 'PATCH_REVERSE_CHECK=0\n'
git apply --reverse "$PATCH"
sha256sum -c baseline-files.sha256 >baseline-reopen.log

current="$(find internal web-spa -type f | wc -l)"
baseline="$(wc -l <baseline-files.sha256)"
test "$current" -eq "$baseline"
printf 'PATCH_APPLY_REVERSE_OK=1 BASELINE_FILES=%s\n' "$baseline"
