#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-${ROOT}/scripts/managed-source-manifest.txt}"
cd "$ROOT"

{
  git ls-files --cached
  # Release builds normally have no untracked source. Including it makes local
  # release verification deterministic before the final commit is assembled.
  git ls-files --others --exclude-standard
} | awk '
  /^super-instruct\/LICENSE$/ { print; next }
  /^(cmd|internal|services|sidecar|scripts|deploy|web-spa\/src|web-spa\/scripts|workers\/node-registrar\/src|super-instruct)\// &&
  /\.(go|py|sh|js|jsx|ts|tsx|css|json|jsonc|mjs|sql|md|txt)$/ { print }
' | LC_ALL=C sort -u | while IFS= read -r candidate; do
  # `git ls-files --cached` still reports paths deleted in an uncommitted
  # release worktree. A release manifest describes files that actually ship,
  # otherwise stale hashed bundles remain allow-listed during an update.
  if [[ -f "$candidate" || -L "$candidate" ]]; then
    printf '%s\n' "$candidate"
  fi
done >"${OUT}.tmp"
mv "${OUT}.tmp" "$OUT"
printf 'wrote %s (%s files)\n' "$OUT" "$(wc -l <"$OUT")"
