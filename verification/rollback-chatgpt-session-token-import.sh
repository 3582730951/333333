#!/usr/bin/env bash
set -euo pipefail

worktree="${WORKTREE:-/workspace}"
patch_file="${PATCH_FILE:-/workspace/verification/chatgpt-session-token-import.patch}"
expected_patch_sha256="0096a7cb03d9b0380f10bbddb06db1e5e4d34f920f1690e75b9fb0765803095b"

[[ -d "$worktree/.git" || -f "$worktree/.git" ]] || {
  printf 'rollback error: not a Git worktree: %s\n' "$worktree" >&2
  exit 2
}
[[ -f "$patch_file" ]] || {
  printf 'rollback error: patch missing: %s\n' "$patch_file" >&2
  exit 2
}

actual_patch_sha256="$(sha256sum "$patch_file" | awk '{print $1}')"
[[ "$actual_patch_sha256" == "$expected_patch_sha256" ]] || {
  printf 'rollback error: patch SHA-256 mismatch: %s\n' "$actual_patch_sha256" >&2
  exit 3
}

git_cmd=(git -c "safe.directory=$worktree" -C "$worktree")

if "${git_cmd[@]}" apply -R --check "$patch_file" 2>/dev/null; then
  "${git_cmd[@]}" apply -R "$patch_file"
  "${git_cmd[@]}" diff --check
  printf 'rollback applied: %s\n' "$worktree"
elif "${git_cmd[@]}" apply --check "$patch_file" 2>/dev/null; then
  printf 'rollback already present: %s\n' "$worktree"
else
  printf 'rollback error: worktree does not match either patch state: %s\n' "$worktree" >&2
  exit 4
fi
