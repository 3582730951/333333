#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  printf 'usage: %s TARGET BACKUP\n' "${0##*/}" >&2
  exit 2
}

target=${1:-}
backup=${2:-}
[[ -n "$target" && -n "$backup" ]] || usage
[[ -f "$backup" ]] || { printf 'backup does not exist: %s\n' "$backup" >&2; exit 1; }

target_dir=$(dirname -- "$target")
[[ -d "$target_dir" ]] || { printf 'target directory does not exist: %s\n' "$target_dir" >&2; exit 1; }
tmp=$(mktemp "$target_dir/.rollback.XXXXXX")
cleanup() { rm -f -- "$tmp"; }
trap cleanup EXIT

# Copy to a same-directory temporary file and atomically replace only the
# explicitly named target. No recursive or broad path operation is performed.
cp -- "$backup" "$tmp"
if [[ -e "$target" ]]; then
  chmod --reference="$target" "$tmp" 2>/dev/null || true
fi
mv -f -- "$tmp" "$target"
trap - EXIT
