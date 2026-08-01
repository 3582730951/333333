#!/usr/bin/env bash
set -Eeuo pipefail

readonly BASELINE=a14151d862aaf74d21d68f759ab6b2aecd0f2744
readonly TRACKED_PATHS=(
  README.md
  config.example.json
  install.sh
  internal/api/account_archive.go
  internal/api/chat_custom.go
  internal/api/custom_provider_admin_probe.go
  internal/api/custom_provider_passthrough.go
  internal/api/custom_provider_passthrough_test.go
  internal/api/mailbox_config.go
  internal/api/mailbox_config_test.go
  internal/api/providers.go
  internal/api/providers_test.go
  internal/api/team_lifecycle.go
  internal/api/team_lifecycle_test.go
  internal/console/dist/assets/AISettings-Dwono2hq.js
  internal/console/dist/assets/Accounts-DDDFu8A4.js
  internal/console/dist/assets/Audit-eysE7VsS.js
  internal/console/dist/assets/CFEvents-Cub5tXhe.js
  internal/console/dist/assets/Charts-D-e5aPHC.js
  internal/console/dist/assets/CloudflareMailbox-D418qAuC.js
  internal/console/dist/assets/Dashboard-DWsyIPvI.js
  internal/console/dist/assets/DisplayPrimitives-kACayybc.js
  internal/console/dist/assets/Egress-CCSf0mcN.js
  internal/console/dist/assets/EmailPool-CwgFhubG.js
  internal/console/dist/assets/Groups-Bv-aqSa-.js
  internal/console/dist/assets/Keys-DFpBhnYR.js
  internal/console/dist/assets/Login-BVjmVuwU.js
  internal/console/dist/assets/MobileResourceCell-B3r48ltW.js
  internal/console/dist/assets/ModelNameList-DNxT0c7f.js
  internal/console/dist/assets/ModelQuality-D87NdTAm.js
  internal/console/dist/assets/Models-BybiSyth.js
  internal/console/dist/assets/OrderedEgressSelect-Bk0O45bY.js
  internal/console/dist/assets/PageHeader-kER44SC9.js
  internal/console/dist/assets/PortalDashboard-B-EPvqmz.js
  internal/console/dist/assets/PortalKeys-BNGtdw37.js
  internal/console/dist/assets/PortalModels-D3YFHDiy.js
  internal/console/dist/assets/PortalProfile-CtPB-708.js
  internal/console/dist/assets/Providers-B2lNt7Bi.js
  internal/console/dist/assets/Quota-DhXbyIvq.js
  internal/console/dist/assets/Registration-DcvXk2yy.js
  internal/console/dist/assets/ResourceTable-Bom--eCX.js
  internal/console/dist/assets/SettingsV2-DYs5bi5H.js
  internal/console/dist/assets/System-DFXjQlG-.js
  internal/console/dist/assets/TeamLifecycle-BOQBm70o.js
  internal/console/dist/assets/UpstreamErrorRules-B5-gH4ac.js
  internal/console/dist/assets/Usage-MX2oaXVm.js
  internal/console/dist/assets/Users-CUVZg1Hf.js
  internal/console/dist/assets/VendorLogo-BkaW7nB2.js
  internal/console/dist/assets/coerce-CKO7KuQ-.js
  internal/console/dist/assets/csv-CrYfOdau.js
  internal/console/dist/assets/emailPool-DjYjr6Z0.js
  internal/console/dist/assets/events-DbJD138B.js
  internal/console/dist/assets/format-Z3to-0pr.js
  internal/console/dist/assets/index-DsAitSNG.js
  internal/console/dist/assets/index-DvxoBNeT.css
  internal/console/dist/assets/keys-DFk5xiTa.js
  internal/console/dist/assets/queries-D3Gj7_lQ.js
  internal/console/dist/assets/settings-CHa5vYaO.js
  internal/console/dist/assets/system-BL4tZc9t.js
  internal/console/dist/assets/usage-C6aiJTqp.js
  internal/console/dist/assets/useAsyncAction-BKKapFI2.js
  internal/console/dist/assets/useAsyncResource-WCQoa2Ll.js
  internal/console/dist/assets/useMutation-CC5IVoaE.js
  internal/console/dist/index.html
  internal/storage/account_backup.go
  internal/storage/postgres_driver.go
  internal/storage/storage.go
  internal/web/assets/admin-integrations.js
  internal/web/assets/admin.html
  internal/web/assets/admin.js
  internal/web/assets/i18n.js
  update.sh
  web-spa/e2e/accessibility.spec.ts
  web-spa/e2e/visual.spec.ts
  web-spa/scripts/capture-ui-review.mjs
  web-spa/src/App.tsx
  web-spa/src/api.js
  web-spa/src/components/Charts.jsx
  web-spa/src/components/DisplayPrimitives.jsx
  web-spa/src/components/MobileResourceCell.jsx
  web-spa/src/components/OAuthLoginModal.jsx
  web-spa/src/components/PageHeader.jsx
  web-spa/src/features/accounts/api/emailPool.ts
  web-spa/src/lib/browserClipboard.js
  web-spa/src/lib/i18n.js
  web-spa/src/pages/Accounts.jsx
  web-spa/src/pages/CloudflareMailbox.tsx
  web-spa/src/pages/Egress.jsx
  web-spa/src/pages/Providers.jsx
  web-spa/src/pages/Quota.tsx
  web-spa/src/pages/Registration.tsx
  web-spa/src/pages/TeamLifecycle.tsx
  web-spa/src/styles/base.css
  web-spa/src/styles/components.css
  web-spa/src/styles/layout.css
  web-spa/src/styles/tokens.css
  web-spa/tests/contracts.test.ts
  web-spa/tests/team-lifecycle-presentation.test.tsx
)
readonly NEW_PATHS=(
  deploy/cloudflare-mailbox/.gitignore
  deploy/cloudflare-mailbox/README.md
  deploy/cloudflare-mailbox/deploy.sh
  deploy/cloudflare-mailbox/migrations/0001_init.sql
  deploy/cloudflare-mailbox/package.json
  deploy/cloudflare-mailbox/src/index.js
  deploy/cloudflare-mailbox/tests/contract.test.mjs
  deploy/cloudflare-mailbox/wrangler.template.jsonc
  docs/operations/AUTOMATION_AND_TEAM_LIFECYCLE.md
  docs/reports/2026-07-31-eight-issue-audit.md
  docs/reports/2026-07-31-ui-ux-audit-and-benchmark.md
  internal/api/custom_provider_multi_route_test.go
  internal/console/dist/assets/AISettings-BpYnPn1g.js
  internal/console/dist/assets/Accounts-CT2QsOpv.js
  internal/console/dist/assets/Audit-B26-SlIt.js
  internal/console/dist/assets/CFEvents-DPM9Jbr4.js
  internal/console/dist/assets/Charts-A-zH4AIb.js
  internal/console/dist/assets/CloudflareMailbox-C9VU9BTs.js
  internal/console/dist/assets/CopyCodeBlock-BvF4QQYm.js
  internal/console/dist/assets/Dashboard-_61IACAa.js
  internal/console/dist/assets/DisplayPrimitives-QOpExJ6c.js
  internal/console/dist/assets/Egress-BMD8hFTj.js
  internal/console/dist/assets/EmailPool-CzLGaUyf.js
  internal/console/dist/assets/Groups-B-Zxo3Tu.js
  internal/console/dist/assets/Keys-BdVrdmgE.js
  internal/console/dist/assets/Login-B06Yvdia.js
  internal/console/dist/assets/MobileResourceCell-DFLUkZpS.js
  internal/console/dist/assets/ModelNameList-D1bsDgS_.js
  internal/console/dist/assets/ModelQuality-BWRKosv0.js
  internal/console/dist/assets/Models-FTuiB93B.js
  internal/console/dist/assets/OrderedEgressSelect-B4TrmoVX.js
  internal/console/dist/assets/PageHeader-BkzrsmJT.js
  internal/console/dist/assets/PortalDashboard-bBcDQJi4.js
  internal/console/dist/assets/PortalKeys-Cd9vlw1_.js
  internal/console/dist/assets/PortalModels-DWSGgpxB.js
  internal/console/dist/assets/PortalProfile-CjIOi3r5.js
  internal/console/dist/assets/Providers-Bh1lFPKm.js
  internal/console/dist/assets/Quota-EzhFqR1m.js
  internal/console/dist/assets/Registration-D_7dSr8A.js
  internal/console/dist/assets/ResourceTable-BHqZ9kqS.js
  internal/console/dist/assets/SettingsV2-sIQSrC32.js
  internal/console/dist/assets/System-ZVA3ywpR.js
  internal/console/dist/assets/TeamLifecycle-BuF8GiCf.js
  internal/console/dist/assets/UpstreamErrorRules-CZHy1Z2o.js
  internal/console/dist/assets/Usage-5ZdGOy7C.js
  internal/console/dist/assets/Users-BoGNrjG0.js
  internal/console/dist/assets/VendorLogo-uKGb78Xk.js
  internal/console/dist/assets/coerce-Bm0EPr5b.js
  internal/console/dist/assets/csv-C5zW-T24.js
  internal/console/dist/assets/emailPool-unpKNZ4t.js
  internal/console/dist/assets/events-B1a5HEDb.js
  internal/console/dist/assets/format-DvohAj6H.js
  internal/console/dist/assets/index-BU6tRDT8.css
  internal/console/dist/assets/index-DDSJwj0g.js
  internal/console/dist/assets/keys-BRmKeHU7.js
  internal/console/dist/assets/queries-Btjvb1Vs.js
  internal/console/dist/assets/settings-dA2ewaDf.js
  internal/console/dist/assets/system-BrTjiRxu.js
  internal/console/dist/assets/usage-tFFQVaOW.js
  internal/console/dist/assets/useAsyncAction-gho3cWJk.js
  internal/console/dist/assets/useAsyncResource-DC2yIufh.js
  internal/console/dist/assets/useMutation-BjANTEeV.js
  internal/storage/custom_provider_routes_test.go
  internal/storage/custom_provider_seed_test.go
  scripts/generate-managed-source-manifest.sh
  scripts/managed-source-manifest.txt
  scripts/prune-managed-source.sh
  scripts/test-upgrade-safety.sh
  web-spa/src/components/CopyCodeBlock.jsx
  web-spa/tests/browser-clipboard.test.ts
  web-spa/tests/oauth-copy.test.tsx
  web-spa/tests/provider-routes.test.ts
)

ROOT="$(git -c safe.directory='*' rev-parse --show-toplevel 2>/dev/null)"
cd "$ROOT"
git_safe() { git -c "safe.directory=$ROOT" "$@"; }
git_safe cat-file -e "${BASELINE}^{commit}"
if ! git_safe merge-base --is-ancestor "$BASELINE" HEAD; then
  printf 'baseline %s is not an ancestor of HEAD\n' "$BASELINE" >&2
  exit 2
fi

git_safe restore --source="$BASELINE" --worktree -- "${TRACKED_PATHS[@]}"
for path in "${NEW_PATHS[@]}"; do
  if [[ -e "$path" || -L "$path" ]]; then
    unlink -- "$path"
  fi
done

# Remove only directories created by this delivery, and only when empty.
for path in "${NEW_PATHS[@]}"; do
  dir="$(dirname "$path")"
  while [[ "$dir" != "." && "$dir" != "/" ]]; do
    rmdir -- "$dir" 2>/dev/null || break
    dir="$(dirname "$dir")"
  done
done

if ! git_safe diff --quiet "$BASELINE" -- "${TRACKED_PATHS[@]}"; then
  printf 'rollback verification failed: tracked delivery paths differ from baseline\n' >&2
  git_safe diff --stat "$BASELINE" -- "${TRACKED_PATHS[@]}" >&2
  exit 3
fi
for path in "${NEW_PATHS[@]}"; do
  if [[ -e "$path" || -L "$path" ]]; then
    printf 'rollback verification failed: added path remains: %s\n' "$path" >&2
    exit 4
  fi
done
printf 'rollback_verified baseline=%s tracked_restored=%s added_removed=%s\n' \
  "$BASELINE" "${#TRACKED_PATHS[@]}" "${#NEW_PATHS[@]}"
