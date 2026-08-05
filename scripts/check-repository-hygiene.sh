#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

max_bytes="${MAX_TRACKED_FILE_BYTES:-10485760}"
if [[ ! "$max_bytes" =~ ^[1-9][0-9]*$ ]]; then
  echo "MAX_TRACKED_FILE_BYTES must be a positive integer" >&2
  exit 2
fi

failed=0
while IFS= read -r -d '' tracked; do
  # A staged deletion remains in git ls-files until the index is refreshed.
  [[ -e "$tracked" || -L "$tracked" ]] || continue

  if [[ -L "$tracked" ]]; then
    echo "tracked symlink is forbidden: $tracked" >&2
    failed=1
    continue
  fi

  if [[ -d "$tracked" ]]; then
    if [[ "$(git ls-files --stage -- "$tracked")" == 160000\ * ]]; then
      continue
    fi
    echo "tracked path is unexpectedly a directory: $tracked" >&2
    failed=1
    continue
  fi

  size="$(wc -c < "$tracked")"
  if (( size > max_bytes )); then
    echo "tracked file exceeds ${max_bytes} bytes: $tracked ($size bytes)" >&2
    failed=1
  fi

  lower="${tracked,,}"
  case "$lower" in
    *.exe|*.dll|*.dylib|*.so|*.a|*.o|*.bin|*.zip|*.tar|*.tar.gz|*.tgz|*.gz|*.bz2|*.xz|*.7z|*.rar|*.sqlite|*.sqlite3|*.db|*.wasm)
      echo "tracked binary/archive/database extension is forbidden: $tracked" >&2
      failed=1
      ;;
  esac
done < <(git ls-files -z)

# Git's binary detector is content-based. Empty files do not appear here and are
# harmless; every non-empty binary blob must be kept out of source control.
while IFS= read -r -d '' binary; do
  [[ -s "$binary" ]] || continue
  echo "tracked binary content is forbidden: $binary" >&2
  failed=1
done < <(git grep -z -IL -e '' -- . || true)

if (( failed != 0 )); then
  exit 1
fi

echo "repository hygiene: ok"
