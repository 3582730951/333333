#!/usr/bin/env bash
# Verify that the embedded SPA and the managed upload manifest describe one
# complete release.  This runs before stale-source pruning so an additive upload
# can never retain a new index.html while deleting its new hashed assets.
set -Eeuo pipefail

ROOT="${PROJECT_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST="${MANAGED_SOURCE_MANIFEST:-${ROOT}/scripts/managed-source-manifest.txt}"
DIST="${ROOT}/internal/console/dist"
INDEX="${DIST}/index.html"

fail() {
  printf 'ERROR: embedded console release is incomplete: %s\n' "$*" >&2
  exit 1
}

[[ -s "$MANIFEST" ]] || fail "managed source manifest is missing: ${MANIFEST}"
[[ -s "$INDEX" ]] || fail "index.html is missing: ${INDEX}"
grep -Fqx 'internal/console/dist/index.html' "$MANIFEST" ||
  fail 'index.html is not managed; regenerate scripts/managed-source-manifest.txt'

declared="$(mktemp)"
actual="$(mktemp)"
references="$(mktemp)"
trap 'rm -f "$declared" "$actual" "$references"' EXIT

grep '^internal/console/dist/' "$MANIFEST" | LC_ALL=C sort -u >"$declared"
[[ -s "$declared" ]] || fail 'manifest contains no embedded console files'

while IFS= read -r relative; do
  [[ -n "$relative" ]] || continue
  [[ -f "${ROOT}/${relative}" ]] || fail "manifest file is absent: ${relative}"
done <"$declared"

# Vite emits double-quoted src/href attributes. Ignore data URLs and external
# resources; every /console/ reference must be both shipped and allow-listed.
grep -Eo '(src|href)="/console/[^"?#]+"' "$INDEX" |
  sed -E 's/^(src|href)="([^"]+)"$/\2/' |
  LC_ALL=C sort -u >"$references" || true
[[ -s "$references" ]] || fail 'index.html contains no local /console/ assets'

while IFS= read -r url; do
  logical="${url#/console/}"
  case "/${logical}/" in
    *'/../'*|*'/./'*) fail "unsafe asset reference in index.html: ${url}" ;;
  esac
  relative="internal/console/dist/${logical}"
  grep -Fqx "$relative" "$MANIFEST" ||
    fail "index.html references an unmanaged asset: ${relative}"
  [[ -s "${ROOT}/${relative}" ]] ||
    fail "index.html references a missing or empty asset: ${relative}"
done <"$references"

# A release checkout/build must match exactly. During an additive VPS upload,
# old hashed files may still exist until prune-managed-source.sh removes them;
# that caller opts into allowing only those harmless extras.
case "${CONSOLE_RELEASE_ALLOW_STALE:-0}" in
  1|true|TRUE|yes|YES|on|ON) ;;
  *)
    find "$DIST" -type f -print |
      sed "s#^${ROOT%/}/##" |
      LC_ALL=C sort -u >"$actual"
    if ! cmp -s "$declared" "$actual"; then
      diff -u "$declared" "$actual" >&2 || true
      fail 'dist tree and managed source manifest differ'
    fi
    ;;
esac

printf 'embedded console release verified: %s files, %s entry assets\n' \
  "$(wc -l <"$declared")" "$(wc -l <"$references")"
