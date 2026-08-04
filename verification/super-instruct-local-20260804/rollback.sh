#!/usr/bin/env bash
set -euo pipefail
ART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TARGET="${1:-$(pwd)}"
if [[ ! -f "$TARGET/go.mod" ]]; then
  printf 'rollback target lacks go.mod: %s\n' "$TARGET" >&2
  exit 2
fi
while IFS=$'\t' read -r mode rel; do
  [[ -n "$rel" ]] || continue
  case "/$rel/" in *"/../"*|*"/./"*) printf 'unsafe rollback path: %s\n' "$rel" >&2; exit 2;; esac
  mkdir -p "$(dirname "$TARGET/$rel")"
  install -m "$mode" "$ART_DIR/original/$rel" "$TARGET/$rel"
done < "$ART_DIR/baseline-modes.tsv"
while IFS= read -r rel; do
  [[ -n "$rel" ]] || continue
  case "/$rel/" in *"/../"*|*"/./"*) printf 'unsafe rollback path: %s\n' "$rel" >&2; exit 2;; esac
  if [[ -e "$TARGET/$rel" ]]; then unlink "$TARGET/$rel"; fi
done < "$ART_DIR/added-files.txt"
printf 'rollback restored %s baseline files and removed %s task-added files in %s\n' \
  "$(wc -l < "$ART_DIR/baseline-files.txt")" "$(wc -l < "$ART_DIR/added-files.txt")" "$TARGET"
