#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="${ROOT:-/root/autodl-tmp/legacy-install-upgrade-20260731}"
ARCHIVE="${ROOT}/uploads/new-source-optimized.tar.gz"
EXPECTED_SHA="${EXPECTED_SHA:-db99b5768dd6a55d351cf087a825538f60c523db0ecb45182f4d0b4f6d2eb73f}"
NODE_ROOT="${NODE_ROOT:-/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64}"
export PATH="${NODE_ROOT}/bin:${PATH}"

actual_sha="$(sha256sum "$ARCHIVE" | awk '{print $1}')"
[[ "$actual_sha" == "$EXPECTED_SHA" ]]
rm -rf "${ROOT}/new-src"
mkdir -p "${ROOT}/new-src"
tar -xzf "$ARCHIVE" -C "${ROOT}/new-src"
chmod 0755 "${ROOT}/new-src/install.sh" "${ROOT}/new-src/scripts/install.sh"

old_dist_sha="$(sha256sum "${ROOT}/new-src/internal/console/dist/index.html" | awk '{print $1}')"
cd "${ROOT}/new-src/web-spa"
set +e
npm ci --no-audit --no-fund \
  > >(tee "${ROOT}/logs/new-ui-npm-ci.literal.log") \
  2> >(tee "${ROOT}/logs/new-ui-npm-ci.stderr.log" >&2)
ci_status=$?
set -e
printf '%s\n' "$ci_status" >"${ROOT}/records/new-ui-npm-ci.exit-status"
(( ci_status == 0 )) || exit "$ci_status"

set +e
npm run build \
  > >(tee "${ROOT}/logs/new-ui-build.literal.log") \
  2> >(tee "${ROOT}/logs/new-ui-build.stderr.log" >&2)
build_status=$?
set -e
printf '%s\n' "$build_status" >"${ROOT}/records/new-ui-build.exit-status"
(( build_status == 0 )) || exit "$build_status"

set +e
npm test -- tests/email-pool-responsive.test.tsx \
  > >(tee "${ROOT}/logs/new-ui-email-regression.literal.log") \
  2> >(tee "${ROOT}/logs/new-ui-email-regression.stderr.log" >&2)
test_status=$?
set -e
printf '%s\n' "$test_status" >"${ROOT}/records/new-ui-email-regression.exit-status"
(( test_status == 0 )) || exit "$test_status"

new_dist_sha="$(sha256sum "${ROOT}/new-src/internal/console/dist/index.html" | awk '{print $1}')"
asset_count="$(find "${ROOT}/new-src/internal/console/dist" -type f | wc -l)"
[[ -s "${ROOT}/new-src/internal/console/dist/index.html" ]]
[[ "$asset_count" -gt 10 ]]
[[ "$new_dist_sha" != "$old_dist_sha" ]]

cat >"${ROOT}/records/new-ui-build.json" <<EOF
{
  "source_archive_sha256": "${actual_sha}",
  "npm_ci_exit": ${ci_status},
  "build_exit": ${build_status},
  "email_regression_test_exit": ${test_status},
  "old_dist_index_sha256": "${old_dist_sha}",
  "new_dist_index_sha256": "${new_dist_sha}",
  "asset_files": ${asset_count}
}
EOF

printf 'NEW_UPLOAD_SHA256=%s\n' "$actual_sha"
printf 'OLD_DIST_INDEX_SHA256=%s\n' "$old_dist_sha"
printf 'NEW_DIST_INDEX_SHA256=%s\n' "$new_dist_sha"
printf 'NEW_DIST_ASSETS=%s\n' "$asset_count"
printf 'NEW_SOURCE_READY=1\n'
