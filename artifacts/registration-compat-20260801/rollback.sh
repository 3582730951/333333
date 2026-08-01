#!/usr/bin/env bash
set -Eeuo pipefail

ARTIFACT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKSPACE_ROOT="${1:-${WORKSPACE_ROOT:-$(cd "${ARTIFACT_ROOT}/../.." && pwd)}}"
BASELINE_ARCHIVE="${ARTIFACT_ROOT}/delivery/rollback-baseline.tar.gz"
MODIFIED_LIST="${ARTIFACT_ROOT}/delivery/modified-files.txt"
NEW_LIST="${ARTIFACT_ROOT}/delivery/new-files.txt"
DELETED_LIST="${ARTIFACT_ROOT}/delivery/deleted-files.txt"
EXPECTED_ARCHIVE_SHA256="b428a46cac3602ae083c440d585100dc06a818f7b81fa95553ff51fe226071ef"

fail() {
  printf "rollback error: %s\n" "$*" >&2
  exit 1
}

safe_relative_path() {
  local value="$1"
  [[ -n "$value" && "$value" != /* && "$value" != ".." && "$value" != ../* && "$value" != *"/../"* && "$value" != *"/.." ]]
}

[[ -d "$WORKSPACE_ROOT" ]] || fail "workspace root does not exist: $WORKSPACE_ROOT"
for required in "$BASELINE_ARCHIVE" "$MODIFIED_LIST" "$NEW_LIST" "$DELETED_LIST"; do
  [[ -r "$required" ]] || fail "missing artifact: $required"
done

actual_sha="$(sha256sum "$BASELINE_ARCHIVE" | awk "{print \$1}")"
[[ "$actual_sha" == "$EXPECTED_ARCHIVE_SHA256" ]] || fail "baseline archive checksum mismatch: $actual_sha"

tmp="$(mktemp -d)"
trap "rm -rf \"$tmp\"" EXIT
tar -xzf "$BASELINE_ARCHIVE" -C "$tmp"

removed=0
while IFS= read -r relative || [[ -n "$relative" ]]; do
  [[ -z "$relative" ]] && continue
  safe_relative_path "$relative" || fail "unsafe new-file path: $relative"
  rm -f -- "$WORKSPACE_ROOT/$relative"
  removed=$((removed + 1))
done < "$NEW_LIST"

restored=0
for list in "$MODIFIED_LIST" "$DELETED_LIST"; do
  while IFS= read -r relative || [[ -n "$relative" ]]; do
    [[ -z "$relative" ]] && continue
    safe_relative_path "$relative" || fail "unsafe baseline path: $relative"
    [[ -f "$tmp/$relative" || -L "$tmp/$relative" ]] || fail "baseline payload missing: $relative"
    mkdir -p -- "$(dirname "$WORKSPACE_ROOT/$relative")"
    rm -f -- "$WORKSPACE_ROOT/$relative"
    cp -a -- "$tmp/$relative" "$WORKSPACE_ROOT/$relative"
    restored=$((restored + 1))
  done < "$list"
done

while IFS= read -r relative || [[ -n "$relative" ]]; do
  [[ -z "$relative" ]] && continue
  cmp -s -- "$tmp/$relative" "$WORKSPACE_ROOT/$relative" || fail "restored content mismatch: $relative"
done < <(cat "$MODIFIED_LIST" "$DELETED_LIST")
while IFS= read -r relative || [[ -n "$relative" ]]; do
  [[ -z "$relative" ]] && continue
  [[ ! -e "$WORKSPACE_ROOT/$relative" && ! -L "$WORKSPACE_ROOT/$relative" ]] || fail "new file still present: $relative"
done < "$NEW_LIST"

printf "rollback complete: restored=%d removed=%d workspace=%s baseline_sha256=%s\n" \
  "$restored" "$removed" "$WORKSPACE_ROOT" "$actual_sha"
