#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${1:-/workspace}"
PATCH_FILE="${2:-${SCRIPT_DIR}/DIFF_FILE}"
EXPECTED_HEAD="b4863db70c24ddf113bc004448c91ad55b796cc9"
EXPECTED_TREE="97caa7525bd56915504b622382678b8d04e530cb"

git -C "$TARGET" rev-parse --is-inside-work-tree >/dev/null
actual_head="$(git -C "$TARGET" rev-parse HEAD)"
if [[ "$actual_head" != "$EXPECTED_HEAD" ]]; then
  printf 'ROLLBACK_RESULT=head_mismatch expected=%s actual=%s\n' "$EXPECTED_HEAD" "$actual_head" >&2
  exit 2
fi

git -C "$TARGET" apply --check --reverse --binary "$PATCH_FILE"
git -C "$TARGET" apply --reverse --index --binary "$PATCH_FILE"

index_tree="$(git -C "$TARGET" write-tree)"
status="$(git -C "$TARGET" status --porcelain=v1 --untracked-files=all)"
if [[ "$index_tree" != "$EXPECTED_TREE" || -n "$status" ]]; then
  printf 'ROLLBACK_RESULT=verification_failed index_tree=%s status=%q\n' "$index_tree" "$status" >&2
  exit 3
fi

printf 'ROLLBACK_RESULT=restored\n'
printf 'HEAD=%s\n' "$actual_head"
printf 'INDEX_TREE=%s\n' "$index_tree"
printf 'STATUS=clean\n'
printf 'EXIT_STATUS=0\n'
