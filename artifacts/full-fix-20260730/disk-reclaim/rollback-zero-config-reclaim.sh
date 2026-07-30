#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO=${1:-$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)}
PATCH=${2:-"$SCRIPT_DIR/zero-config-reclaim.patch"}

[[ -f $PATCH ]] || {
  printf 'ERROR: patch not found: %s\n' "$PATCH" >&2
  exit 1
}

git -C "$REPO" apply --check --reverse "$PATCH"
git -C "$REPO" apply --reverse "$PATCH"
printf 'Rolled back zero-config disk reclamation hotfix in %s\n' "$REPO"
