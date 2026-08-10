#!/usr/bin/env bash
set -euo pipefail

BASE_COMMIT="0c2aa3dcf8e120a8833cb1f57687ce4dc067774f"
TARGET_ROOT="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

tracked_paths=(
  internal/api/account_probe.go
  internal/api/antigravity_messages.go
  internal/api/claude_context_1m_test.go
  internal/api/messages.go
  internal/auth/auth.go
  internal/auth/auth_test.go
  internal/capability/models.go
  internal/capability/models_test.go
  internal/scheduler/scheduler.go
  internal/upstream/antigravity.go
  internal/upstream/antigravity_test.go
  internal/virtual/virtual.go
  internal/virtual/virtual_test.go
)

new_paths=(
  internal/antigravityidentity/identity.go
  internal/antigravityidentity/identity_test.go
  internal/api/claude_autocompact.go
  internal/api/claude_autocompact_test.go
  internal/jsonview/jsonview.go
  internal/jsonview/jsonview_test.go
  internal/upstream/antigravity_sensitive.go
  verification/claude-1m-antigravity-20260809/01-original-baseline.md
  verification/claude-1m-antigravity-20260809/02-modified-report.md
  verification/claude-1m-antigravity-20260809/03-concise-diff.md
  verification/claude-1m-antigravity-20260809/VERIFICATION.txt
  verification/claude-1m-antigravity-20260809/04-rollback.sh
)

git -c safe.directory="$TARGET_ROOT" -C "$TARGET_ROOT" cat-file -e "$BASE_COMMIT^{commit}"
git -c safe.directory="$TARGET_ROOT" -C "$TARGET_ROOT" checkout "$BASE_COMMIT" -- "${tracked_paths[@]}"

for path in "${new_paths[@]}"; do
  rm -f "$TARGET_ROOT/$path"
done
rmdir "$TARGET_ROOT/internal/antigravityidentity" 2>/dev/null || true
rmdir "$TARGET_ROOT/internal/jsonview" 2>/dev/null || true
rmdir "$TARGET_ROOT/verification/claude-1m-antigravity-20260809" 2>/dev/null || true
rmdir "$TARGET_ROOT/verification" 2>/dev/null || true

git -c safe.directory="$TARGET_ROOT" -C "$TARGET_ROOT" diff --exit-code -- "${tracked_paths[@]}"
git -c safe.directory="$TARGET_ROOT" -C "$TARGET_ROOT" diff --cached --exit-code -- "${tracked_paths[@]}"
for path in "${new_paths[@]}"; do
  test ! -e "$TARGET_ROOT/$path"
done

printf '%s\n' \
  "ROLLBACK_RESULT=restored" \
  "ROLLBACK_COMMIT=$BASE_COMMIT" \
  "ROLLBACK_BRANCH_FIELD=cache-hit-optimization / native_1m_passthrough=false (baseline)" \
  "ROLLBACK_BEHAVIOR=original scheduler and Antigravity request identity restored" \
  "ROLLBACK_STATUS=0"
