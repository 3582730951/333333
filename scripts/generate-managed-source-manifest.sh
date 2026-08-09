#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${1:-${ROOT}/scripts/managed-source-manifest.txt}"
cd "$ROOT"

list_candidates() {
  if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git ls-files --cached
    # Release builds normally have no untracked source. Including it makes local
    # release verification deterministic before the final commit is assembled.
    git ls-files --others --exclude-standard
    return
  fi

  # Source archives intentionally have no .git directory. Keep `npm run build`
  # usable there by deriving the same bounded release candidates from disk.
  local root
  for root in cmd internal services sidecar scripts deploy web-spa/src web-spa/scripts workers/node-registrar/src super-instruct; do
    [[ -d "$root" ]] || continue
    find "$root" \( -type f -o -type l \) -print
  done
}

list_candidates | awk '
  /^super-instruct\/LICENSE$/ { print; next }
  /^internal\/console\/dist\// { print; next }
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
