#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/codex-vps-diagnostics-test.XXXXXX")"
trap 'find "$TMP" -depth -delete 2>/dev/null || true' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

runtime="$TMP/runtime"
failure_dir="$runtime/deploy-failures"
archive="$TMP/support.tar.gz"
extract_dir="$TMP/extracted"
mkdir -p "$failure_dir" "$extract_dir"

printf '%s\n' \
  'captured_at=2026-08-16T00:32:15Z' \
  'release=old-failure' \
  '[sqlite-locks]' \
  'database=/var/lib/codex-pool/pool.sqlite3' \
  'old-pool-server 4242 POSIX READ pool.sqlite3' \
  'Authorization: Bearer diagnostic-secret-123456789' \
  'operator@example.com' \
  >"$failure_dir/worker-old.log"
sleep 0.01
printf '%s\n' \
  'captured_at=2026-08-16T00:33:15Z' \
  'release=new-failure' \
  '[sqlite-locks]' \
  'probe_error=database is locked' \
  'credential sk-diagnosticsecret123456' \
  'new-operator@example.com' \
  >"$failure_dir/sqlite-lock-new.log"

CODEX_POOL_DATA_DIR="$runtime" \
  bash "$ROOT/scripts/collect-vps-diagnostics.sh" --no-app \
    --base-url http://127.0.0.1:9 --output "$archive" >/dev/null
tar -xzf "$archive" -C "$extract_dir"
bundle="$extract_dir/codex-pool-vps-diagnostics"
[[ -f "$bundle/service/deploy-failure-1.txt" ]] ||
  fail "newest installer failure was not exported"
grep -Fq '[sqlite-locks]' "$bundle/service/deploy-failure-1.txt" ||
  fail "exported deployment report omitted lock evidence"
grep -Fq 'database is locked' "$bundle/service/deploy-failure-1.txt" ||
  fail "exported deployment report omitted the SQLite error"
if grep -Fq 'sk-diagnosticsecret123456' "$bundle/service/deploy-failure-1.txt"; then
  fail "diagnostic bundle leaked an API token"
fi
if grep -Fq 'new-operator@example.com' "$bundle/service/deploy-failure-1.txt"; then
  fail "diagnostic bundle leaked an email address"
fi
grep -Fq '<REDACTED_API_TOKEN>' "$bundle/service/deploy-failure-1.txt" ||
  fail "API token was removed without a visible redaction marker"
grep -Fq '<EMAIL>' "$bundle/service/deploy-failure-1.txt" ||
  fail "email was removed without a visible redaction marker"
[[ -f "$bundle/service/deploy-failure-2.txt" ]] ||
  fail "bounded deployment history omitted the preceding worker failure"
if find "$bundle" -type f -name '*.sqlite3' -print -quit | grep -q .; then
  fail "support bundle copied the SQLite database"
fi
(
  cd "$bundle"
  sha256sum -c checksums.sha256 >/dev/null
)

printf 'PASS: failed-startup evidence is exported, bounded, checksummed, and redacted\n'
