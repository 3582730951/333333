#!/usr/bin/env bash
set -Eeuo pipefail

export PATH=/root/autodl-tmp/jce_cloud_tools_20260730/node-v22.23.2-linux-x64/bin:/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin:$PATH
SOURCE=/root/autodl-tmp/dark-mode-fix-20260731/src
VERIFY=/root/autodl-tmp/dark-mode-fix-20260731/verification

cd "$SOURCE/web-spa"
npm test -- --run 2>&1 | tee "$VERIFY/frontend-test-final-exact.log"
npm run typecheck 2>&1 | tee "$VERIFY/frontend-typecheck-final-exact.log"
npm run build 2>&1 | tee "$VERIFY/frontend-build-final-exact.log"

cd "$SOURCE"
go test ./internal/storage ./internal/registration/teamflow ./internal/api \
  -run 'TeamLifecycle|Lifecycle|Engine|PoolAdapter|RegistrationProviderReloadUsesAtomicPipelineSnapshots' \
  -count=1 2>&1 | tee "$VERIFY/team-lifecycle-targeted-final.log"
go test -race ./internal/api \
  -run RegistrationProviderReloadUsesAtomicPipelineSnapshots \
  -count=1 2>&1 | tee "$VERIFY/registration-race-final.log"
