#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
REPO=${1:-$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)}
PATCH=${2:-"$SCRIPT_DIR/service-drain-progress.patch"}

git -C "$REPO" apply --check --reverse "$PATCH"
git -C "$REPO" apply --reverse "$PATCH"
printf 'Rolled back service-drain progress hotfix in %s\n' "$REPO"
