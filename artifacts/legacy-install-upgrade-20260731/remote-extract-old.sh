#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
ARCHIVE="${ROOT}/uploads/old-source-cache-hit-optimization.tar.gz"
EXPECTED_SHA="${EXPECTED_SHA:-eedf0eef22ac07bab00179d2e0a1605a37324a09d3bf6cf03598314e587cd71a}"

actual_sha="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
[[ "$actual_sha" == "$EXPECTED_SHA" ]]
rm -rf "${ROOT}/old-src"
mkdir -p "${ROOT}/old-src"
tar -xzf "$ARCHIVE" -C "${ROOT}/old-src"
chmod 0755 "${ROOT}/old-src/install.sh" "${ROOT}/old-src/scripts/install.sh"

printf 'OLD_UPLOAD_SHA256=%s\n' "$actual_sha"
printf 'OLD_INSTALL_SHA256=%s\n' "$(sha256sum "${ROOT}/old-src/install.sh" | awk '{print $1}')"
printf 'OLD_MEMBERS=%s\n' "$(tar -tzf "$ARCHIVE" | wc -l)"
printf 'OLD_SOURCE_READY=1\n'
