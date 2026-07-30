# Full Fix Verification Record — 2026-07-30

Repository: `/workspace`  
Branch: `cache-hit-optimization`  
All executable validation ran only in the isolated remote regression root with CPU 190-191, nice 15, idle I/O priority, `GOMAXPROCS=2`, and Go package parallelism 1.

## Source states and four delivery roles

```text
ARTIFACT_ASSEMBLY_COMMAND=/root/.local/bin/rtk bash artifacts/full-fix-20260730/assemble-delivery.sh /workspace
ARTIFACT_ASSEMBLY_COMMAND_EXIT=0
SOURCE_BRANCH=cache-hit-optimization
BASELINE_COMMIT=3b484ab1d1148136c724a8dc91ac46c3e89f5ec3
ORIGINAL_EXPECTED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
ORIGINAL_RECONSTRUCTED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
MODIFIED_TREE=627a3b0cb3e949b33846269b77fe035fedd0d4a9
HISTORICAL_ORIGINAL_INDEX_TREE=fd5c6dfecaa01f0922d056ee148edfefab8f7ccf
ASSEMBLY_INDEX_EXPECTED_TREE=b9ce758fefe8abe6600f6e11d71ea19afcfbb889
PREEXISTING_PATCH_SHA256=7cf8a1e4da4ee4fb36e367fe174f8376ea05187b5d8e1a9852ac8321ac82b484
REPOSITORY_INDEX_TREE_BEFORE=b9ce758fefe8abe6600f6e11d71ea19afcfbb889
REPOSITORY_INDEX_TREE_AFTER=b9ce758fefe8abe6600f6e11d71ea19afcfbb889
FINAL_SOURCE_FILES=1005
NEW_SOURCE_FILES_INCLUDED=0
UNTRACKED_FILES_EXCLUDED=0
PATCH_CHANGED_PATHS=207
PATCH_STATIC_DIFF_CHECK_EXIT=0
SOURCE_ARCHIVE_GENERATION_EXIT=0
POOL_SERVER_FINAL_V7_SHA256=da80b57d113f886e008eb0914efd0de83966edbd456b3ec8a335e0c83495b467
GATEWAY_FINAL_V7_SHA256=6bfbba594ba36910c89bfe2f67d32a6ac2a8cf8bf42ca5d91a7e5d4f801525b9
LOCAL_EXECUTABLE_TESTS=not-run-by-assembler
REMOTE_ROLE_EXECUTION_HARNESS=verify-delivery-artifacts.sh
```

1. Modified artifacts: `modified-source.tar.gz`, `bin/pool-server-final-v7`, `bin/gateway-final-v7`
2. Patch/diff: `baseline-to-final.patch`
3. Verification: `verification-record.md` plus `remote-artifact-verification.txt`
4. Runnable rollback: `rollback.sh`

Preserved original: `original-state.tar.gz`. Full source/evidence hashes: `original-state.sha256`, `modified-source.sha256`, `binary-sha256.txt`, `evidence-sha256.txt`, `delivery-sha256.txt`.

## Baseline behavior — diagnostic export

Command:
```bash
CODEX_TEST_CPU_SET=190,191 taskset -c 190,191 nice -n 15 ionice -c3 bash results/remote-diagnostic-perf.sh results/pool-server-pre-fix pre-fix perf-pre-fix 39081
```
Input and literal result:
```text
usage_records=500000
audit_log=200000
database_bytes=125243392
archive_bytes=3991141
export_elapsed_seconds=110.714
exported_usage_records=500000
exported_audit_log=200000
terminal_state=ready
exit_status=0
peak_rss_kib=41792
```
Evidence: `../optimization-20260729/verification-record.md`; preserved pre-fix ZIP/binary and SHA manifests are under `../optimization-20260729/`.

## Modified behavior — final v7 diagnostic download

Command and literal output:
```text
COMMAND=CODEX_TEST_CPU_SET=190,191 /root/autodl-tmp/codex-pool-regression-20260730/remote-diagnostic-v5.sh /root/autodl-tmp/codex-pool-regression-20260730/bin/pool-server-final-v7 /root/autodl-tmp/codex-pool-regression-20260730/diagnostic-smoke-v7 19447
HEALTHZ_EXIT=0
{
  "archive_bytes": 9451,
  "archive_sha256": "f877149d5af2fef7785756f4320c7e5c98289a4fc78da25ba0e12da226bdf418",
  "create_http_status": 202,
  "download_headers": {
    "Cache-Control": "no-store",
    "Content-Disposition": "attachment; filename=\"codex-pool-diagnostics-v3-diagjob_e207fb2c92ef4976af4917ce7089609a.zip\"",
    "Content-Type": "application/zip"
  },
  "download_http_status": 200,
  "elapsed_ms": 62.943,
  "job_id": "diagjob_e207fb2c92ef4976af4917ce7089609a",
  "states": [
    {
      "elapsed_ms": 5.737,
      "state": "rendering"
    },
    {
      "elapsed_ms": 57.579,
      "state": "ready"
    }
  ],
  "status_cache_control": "no-store",
  "status_http_status": 200,
  "terminal_state": "ready",
  "zip_member_count": 30,
  "zip_test": "ok"
}
DIAGNOSTIC_DOWNLOAD_EXIT=0
2026/07/30 15:00:21 startup: deferred storage migrations completed in 1ms
DEFERRED_MIGRATION_EXIT=0
SERVER_GRACEFUL_EXIT=0
EXIT_STATUS=0
```
Evidence SHA-256: `6336bb9a949f6838e6fe7f0611dd8ca885b6dbbd705f54573a14fad30d1d751e`. The downloaded ZIP SHA-256 is `f877149d5af2fef7785756f4320c7e5c98289a4fc78da25ba0e12da226bdf418`.

## Final binary build and smoke

```text
POOL_BUILD_COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go build -p=1 -trimpath -o /root/autodl-tmp/codex-pool-regression-20260730/bin/pool-server-final-v7 ./cmd/pool-server
POOL_BUILD_EXIT_STATUS=0
GATEWAY_BUILD_COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go build -p=1 -trimpath -o /root/autodl-tmp/codex-pool-regression-20260730/bin/gateway-final-v7 ./cmd/gateway
GATEWAY_BUILD_EXIT_STATUS=0
da80b57d113f886e008eb0914efd0de83966edbd456b3ec8a335e0c83495b467  /root/autodl-tmp/codex-pool-regression-20260730/bin/pool-server-final-v7
6bfbba594ba36910c89bfe2f67d32a6ac2a8cf8bf42ca5d91a7e5d4f801525b9  /root/autodl-tmp/codex-pool-regression-20260730/bin/gateway-final-v7
/root/autodl-tmp/codex-pool-regression-20260730/bin/pool-server-final-v7: ELF 64-bit LSB executable, x86-64, version 1 (SYSV), dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2, BuildID[sha1]=e6661cddd01ae005ea4d44363ee6f22e7260b4a5, for GNU/Linux 3.2.0, with debug_info, not stripped
/root/autodl-tmp/codex-pool-regression-20260730/bin/gateway-final-v7:     ELF 64-bit LSB executable, x86-64, version 1 (SYSV), dynamically linked, interpreter /lib64/ld-linux-x86-64.so.2, BuildID[sha1]=78129336c6a7f1feb0249d621cba97d951ef4b7e, with debug_info, not stripped
POOL_SELF_TEST_COMMAND=/root/autodl-tmp/codex-pool-regression-20260730/bin/pool-server-final-v7 --self-test
codex-pool-server self-test ok
POOL_SELF_TEST_EXIT_STATUS=0
GATEWAY_SELF_TEST_COMMAND=HOME=/root/autodl-tmp/codex-pool-regression-20260730/gateway-self-test-home /root/autodl-tmp/codex-pool-regression-20260730/bin/gateway-final-v7 trust-ca --help
Usage of /root/autodl-tmp/codex-pool-regression-20260730/bin/gateway-final-v7:
  -print-commands
    	Print manual commands
GATEWAY_SELF_TEST_EXIT_STATUS=0
EXIT_STATUS=0
```
Evidence SHA-256: `8d14451a7c8acd71549c85ecb22e8ce9cf944a917a37462b172b6a823abdb78e`.

## One-MiB Codex / one-million-token Claude account-switch stress

Each test ran three times. Each run used eight concurrent downstream threads, switched upstream accounts, rebuilt the prior context, and asserted exact native tool-call/tool-result pairing and isolation.

```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/storage ./internal/api -run TestGoalAndVirtualContextCompressionIsLosslessAtRest\|TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce\|TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs\|TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults -count=3 -timeout=25m -v
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.09s)
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.09s)
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.07s)
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.08s)
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.07s)
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.09s)
PASS
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.77s)
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.79s)
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.69s)
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.91s)
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.79s)
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.90s)
PASS
EXIT_STATUS=0
```
Evidence SHA-256: `7e99ef23bbd858b9207cc7850b493c4602ed1245b7e2f74bffbe1ac28f807f44`.

## v5 full-suite audit and v7 closure

Exact v5 command:
```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./... -count=1 -timeout=50m
```
Literal terminal findings:
```text
--- FAIL: TestCodexStatelessContinuationFailsOverOn429 (30.15s)
FAIL    codex-account-pool/internal/api    511.276s
--- FAIL: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.03s)
FAIL    codex-account-pool/internal/kiro    0.278s
EXIT_STATUS=1
```
All other packages in that complete run emitted `ok` or `[no test files]`; full literal log SHA-256: `380679771021d5b37ef567caeb581ecd2bbb4b22ccd81a3f35e00ab85bdf6600`. The Kiro failure was an obsolete fixture expectation. A captured goroutine stack proved the 30-second failure was reset-credit post-success handling, not upstream account switching (SHA-256 `9d63ca78d8da6f4bcdbb2c7e54fbacd1280470eb03d2c80ab1c13fc38c54eb5a`). v7 removes that blocking path.

Final repeat evidence:
```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/api ./internal/kiro -run TestCodexStatelessContinuationFailsOverOn429\|TestRefreshModelCatalogPartialFailurePreservesLastGood -count=3 -timeout=5m -v
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.18s)
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.14s)
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.16s)
PASS
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.03s)
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.03s)
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.04s)
PASS
EXIT_STATUS=0
```
Evidence SHA-256: `12e8816586ffe5ea1b17b1de8efc1f627d6511a0734987c731afd3ee3da85d34`.

Reset-credit regression matrix:
```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/api -run TestCodexResetCredit\|TestParseCodexResetCredits\|TestCodexStatelessContinuationFailsOverOn429 -count=1 -timeout=10m -v
--- PASS: TestParseCodexResetCredits (0.00s)
--- PASS: TestCodexResetCreditHeadersKeepCapturedDesktopFingerprint (0.00s)
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.14s)
PASS
EXIT_STATUS=0
```
Evidence SHA-256: `1c9a7226e92ec86016c3d67312157c1b9be999c39844b90a022e2cedcd52a7eb`.

## Frontend install, tests, typecheck, and production build

Commands:
```text
npm ci
npm run verify
```
Literal terminal result:
```text
npm ci: 356 packages, 0 vulnerabilities, EXIT_STATUS=0
Test Files  10 passed (10)
Tests       60 passed (60)
tsc --noEmit: EXIT_STATUS=0
vite v8.1.4: 2787 modules transformed
built in 699ms
npm run verify: EXIT_STATUS=0
```
Logs: `remote-tests/frontend-npm-ci.log` SHA-256 `5ef631162988ec848e0352a3ffeea304d8c12c8ab000763c6c71d13ba0e4115a`; `remote-tests/frontend-npm-verify-final.log` SHA-256 `3d07277e299e9423e19c0c4316645f8fd3883c01872947bfd43806a38034266b`. Canonical console archive SHA-256 `ec6fa3607b0b3e1348588970017732b8bbde4c0efb64736f7ef0d15b03ac81a5`; reopened tree SHA-256 `787c4876f09a6fdf0e06ac1cbef93e0ad22845b727ef3c99f40b5e99ac34b102` (`console-dist-sync-record.md`).

## Disk reclaim preservation / rollback

Command family:
```text
reclaim-disk-space.sh                       # dry run
reclaim-disk-space.sh --apply ... --assume-quiesced
reclaim-disk-space.sh --apply --rollback BACKUP ...
```
Literal verified result:
```text
dry_run_exact_match=true
accounts_sha256_before_after=83729194369589f009b2d126aff5825bc25941e59bb75aafea3bcc953068527f
contexts_sha256_before_after=d6e11815c181209015ed294bb8b9d4f2e13cac347c267e9d32c387cfcbc01082
retained_logs_sha256=d4ce47f22fcddfc8823a52782e96ca6c99ce3f1a058764fcbbd3cae39fa8a847
database_bytes_before=8765440
database_bytes_after=368640
files_bytes_reclaimed=4194304
archive_rows=19
sqlite_quick_check=ok
rollback_rows=65
rollback_sha256=7d971b36d769b26c5afa20247133339b12664c04b4a9a55d19ef1f7f0231a4d6
ALL_REMOTE_DISK_RECLAIM_TESTS_PASSED
```
Full commands/literal output: `disk-reclaim/remote-verification.txt` SHA-256 `3ac4b8e7212269978b32503b0d62c401174fabbca6a189185fc66f1934ac41ea`.

## Kiro CLI fidelity and diagnostic analysis

Sanitized exact CLI 2.15.2 wire/usage captures and integrity checks are in `kiro/`; `kiro/SHA256SUMS` verifies every capture. Final Kiro compatibility report SHA-256 `4d4146af2f7396c8b203607f9e5a76574b2b087bd6acde17b58ec1d10a213257`. Final diagnostic report SHA-256 `c5fe2454e4d04847ba8bd25e09d087501661a28e9ddb12cc4258e511d831fc61`; diagnostic source manifest SHA-256 `c3a50ec29fc50381e71553ef60c20a2e60b7e076bf22817795e8e18b5c531a23`. No raw authorization value is included.

## Remote artifact reopen / patch / rollback / reapply

Command:
```bash
/usr/local/bin/rtk taskset -c 190,191 nice -n 15 ionice -c3 bash artifacts/full-fix-20260730/verify-delivery-artifacts.sh REMOTE_ROOT REMOTE_ARTIFACT_DIR
```
Literal output:
```text
REMOTE_ARTIFACT_HARNESS_VERSION=1
REMOTE_CPU_ALLOWED_LIST=190-191
REMOTE_NICE_LEVEL=15
REMOTE_IO_CLASS=idle
REMOTE_EXECUTION_POLICY=verified
REMOTE_WORK_ROOT_SCOPE=verified
baseline-to-final.patch: OK
original-state.tar.gz: OK
original-tree-manifest.jsonl: OK
original-tree.txt: OK
original-index-tree.txt: OK
baseline-commit.txt: OK
modified-source.tar.gz: OK
modified-tree-manifest.jsonl: OK
modified-tree.txt: OK
modified-source-new-files.txt: OK
modified-source-exclusions.txt: OK
bin/pool-server-final-v7: OK
bin/gateway-final-v7: OK
console-dist-after-sync-validation.txt: OK
console-dist-after-sync.sha256tree: OK
console-dist-after-sync.tar.gz: OK
console-dist-after-sync.tar.gz.sha256: OK
console-dist-sync-record.md: OK
diagnostic-analysis.md: OK
diagnostic-archive-integrity.json: OK
diagnostic-source-sha256.txt: OK
disk-reclaim/apply-result.json: OK
disk-reclaim/baseline.json: OK
disk-reclaim/remote-test-harness.sh: OK
disk-reclaim/remote-verification.log: OK
disk-reclaim/remote-verification.log.sha256: OK
disk-reclaim/remote-verification.txt: OK
disk-reclaim/remote-verification.txt.sha256: OK
disk-reclaim/verification.json: OK
frontend-dist-verified.tar.gz: OK
frontend-dist-verified.tar.gz.sha256: OK
kiro/SHA256SUMS: OK
kiro/capture-integrity.json: OK
kiro/kiro-cli-2.15.2-usage-sanitized.jsonl: OK
kiro/kiro-cli-2.15.2-wire-sanitized.jsonl: OK
kiro/sanitization-verification.json: OK
kiro/source-sha256.txt: OK
kiro/static-wire-comparison.json: OK
kiro-cli-compatibility.md: OK
remote-tests/build-v7.log: OK
remote-tests/context-stress-v7-count3.log: OK
remote-tests/diagnostic-smoke-v7-result.json: OK
remote-tests/diagnostic-smoke-v7-server.log: OK
remote-tests/diagnostic-smoke-v7.log: OK
remote-tests/diagnostic-smoke-v7.zip: OK
remote-tests/frontend-npm-ci.log: OK
remote-tests/frontend-npm-verify-final.log: OK
remote-tests/go-test-all-v5.log: OK
remote-tests/kiro-header-targeted-v5.log: OK
remote-tests/remote-diagnostic-v4.sh: OK
remote-tests/v6-stateless429-timeout-stack.log: OK
remote-tests/v7-reset-credit-targeted.log: OK
remote-tests/v7-two-regressions-count3.log: OK
baseline-commit.txt: OK
original-tree.txt: OK
original-index-tree.txt: OK
original-state.tar.gz: OK
original-state.sha256: OK
original-tree-manifest.jsonl: OK
modified-tree.txt: OK
modified-source.tar.gz: OK
modified-source.sha256: OK
modified-tree-manifest.jsonl: OK
modified-source-new-files.txt: OK
modified-source-exclusions.txt: OK
baseline-to-final.patch: OK
baseline-to-final.files.txt: OK
baseline-to-final.stat.txt: OK
baseline-to-final.patch.sha256: OK
rollback-baseline-scope.jsonl: OK
rollback-final-scope.jsonl: OK
rollback.sh: OK
binary-sha256.txt: OK
evidence-sha256.txt: OK
bin/pool-server-final-v7: OK
bin/gateway-final-v7: OK
artifact-assembly.txt: OK
verification-record.md: OK
verify-delivery-artifacts.sh: OK
assemble-delivery.sh: OK
DELIVERY_SHA256_EXIT=0
POOL_SERVER_SELF_TEST_OUTPUT=codex-pool-server self-test ok
POOL_SERVER_SELF_TEST_EXIT=0
GATEWAY_USAGE_FIRST_LINE=Claude Gateway - Local MITM proxy for pool_server
GATEWAY_USAGE_EXPECTED_EXIT=1
BINARY_SMOKE_EXIT=0
ROLLBACK_SCRIPT_REOPEN_EXIT=0
VERIFICATION_RECORD_REOPEN_EXIT=0
ORIGINAL_ARCHIVE_LIST_EXIT=0
MODIFIED_ARCHIVE_LIST_EXIT=0
ARCHIVE_EXCLUSION_POLICY=verified
ORIGINAL_ARCHIVE_EXTRACT_EXIT=0
MODIFIED_ARCHIVE_EXTRACT_EXIT=0
MANIFEST_FILES=979
MANIFEST_SHA256=verified
ORIGINAL_MANIFEST_EXIT=0
MANIFEST_FILES=1004
MANIFEST_SHA256=verified
MODIFIED_MANIFEST_EXIT=0
ORIGINAL_EXPECTED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
ORIGINAL_REOPENED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
MODIFIED_EXPECTED_TREE=627a3b0cb3e949b33846269b77fe035fedd0d4a9
MODIFIED_REOPENED_TREE=627a3b0cb3e949b33846269b77fe035fedd0d4a9
PATCH_CHECK_EXIT=0
PATCH_APPLY_EXIT=0
PATCH_APPLIED_TREE=627a3b0cb3e949b33846269b77fe035fedd0d4a9
baseline-to-final.patch: OK
ROLLBACK_SCOPE_CONTENT=verified
ROLLBACK_EXIT=0
ROLLBACK_REPO=REMOTE_ISOLATED_FIXTURE
ROLLBACK_STATE=original
ROLLBACK_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
ROLLBACK_FORWARD_PREFLIGHT_EXIT=0
ROLLBACK_REAPPLY_EXIT=0
ROLLBACK_REAPPLIED_TREE=627a3b0cb3e949b33846269b77fe035fedd0d4a9
c49996e1c21264326206e3ccd10cc426bd37060af54d7fa0a0739066425dbab8  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/modified-source.tar.gz
f29ed34a7bdd4c0e52a0682cd843ec0f7c8b5ecc0ceb56c43e0f43509ee5c786  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/baseline-to-final.patch
bdd2c371890a569ec8e851195a406ff93d383f192e25acf57516079fd2eda81d  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/rollback.sh
46afcb5e5afa1f651c0bafe7f94ccd1f8a6095d40a5520376b66511cc76f7faa  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/original-state.tar.gz
da80b57d113f886e008eb0914efd0de83966edbd456b3ec8a335e0c83495b467  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/bin/pool-server-final-v7
6bfbba594ba36910c89bfe2f67d32a6ac2a8cf8bf42ca5d91a7e5d4f801525b9  /root/autodl-tmp/codex-pool-regression-20260730/delivery-final-v7/artifacts/full-fix-20260730/bin/gateway-final-v7
VERIFICATION_RECORD_SHA256=verified-by-delivery-manifest
FOUR_ROLES_REOPENED=verified
REMOTE_ARTIFACT_VERIFICATION_EXIT=0
```
