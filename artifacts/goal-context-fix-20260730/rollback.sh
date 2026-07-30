#!/usr/bin/env bash
set -euo pipefail

ROOT="${1:-$(pwd)}"
SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PATCH_FILE="${2:-$SCRIPT_DIR/patch/goal-context-fix.patch}"
EXPECTED_PATCH_SHA256="049050ae0f4d4f2b2b687b68146b41cc47f51f0b25aa6f90f190b1cd93daa70d"

actual_sha256="$(sha256sum "$PATCH_FILE" | awk '{print $1}')"
if [ "$actual_sha256" != "$EXPECTED_PATCH_SHA256" ]; then
  printf 'rollback patch checksum mismatch: got=%s want=%s\n' \
    "$actual_sha256" "$EXPECTED_PATCH_SHA256" >&2
  exit 2
fi

cd "$ROOT"
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git apply --reverse --check "$PATCH_FILE"
  git apply --reverse "$PATCH_FILE"
else
  patch --batch --silent -p1 --dry-run --reverse < "$PATCH_FILE"
  patch --batch --silent -p1 --reverse < "$PATCH_FILE"
fi

for added in \
  internal/api/downstream_client_namespace.go \
  scripts/analyze_goal_context_diagnostics.py
do
  if [ -e "$added" ]; then
    printf 'rollback retained added path: %s\n' "$added" >&2
    exit 3
  fi
done

printf 'ROLLBACK_OK root=%s patch_sha256=%s\n' "$ROOT" "$actual_sha256"
