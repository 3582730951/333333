#!/usr/bin/env bash
set -euo pipefail

TARGET=${1:?usage: ROLLBACK.sh /path/to/independent/git-worktree}
BASELINE_TREE=efd4940f3d3c53890cf7e2682c39694de4566a4e

ROOT=$(git -c safe.directory="$TARGET" -C "$TARGET" rev-parse --show-toplevel)
if [[ "$(cd "$TARGET" && pwd -P)" != "$(cd "$ROOT" && pwd -P)" ]]; then
  printf 'refusing non-root worktree target: %s\n' "$TARGET" >&2
  exit 2
fi

git -c safe.directory="$TARGET" -C "$TARGET" read-tree --reset -u "$BASELINE_TREE"
ACTUAL=$(git -c safe.directory="$TARGET" -C "$TARGET" write-tree)
if [[ "$ACTUAL" != "$BASELINE_TREE" ]]; then
  printf 'rollback tree mismatch: got=%s want=%s\n' "$ACTUAL" "$BASELINE_TREE" >&2
  exit 3
fi
printf 'ROLLBACK_RESULT=restored tree=%s target=%s\n' "$ACTUAL" "$TARGET"
