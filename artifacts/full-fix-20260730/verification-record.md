# Full Fix Verification Record — 2026-07-30

## Immutable source states

- Baseline commit: `3b484ab1d1148136c724a8dc91ac46c3e89f5ec3`
- Source branch: `cache-hit-optimization`
- Preserved original workspace tree: `045ca893e746ec72cc684db96bec730168f4ff6c`
- Preserved original index tree: `fd5c6dfecaa01f0922d056ee148edfefab8f7ccf`
- Modified source tree: `5e4a67224cbb3ddeabb9ea8a9bc6f1dbc1197aff`
- Repository index before/after assembly: `fd5c6dfecaa01f0922d056ee148edfefab8f7ccf` (unchanged)

## Four delivery roles

1. Modified artifacts: `modified-source.tar.gz`, `bin/pool-server-final-v7`, `bin/gateway-final-v7`
2. Patch/diff: `baseline-to-final.patch`
3. This verification record: `verification-record.md`
4. Runnable rollback: `rollback.sh`

Preserved input: `original-state.tar.gz`. Integrity manifests:
`original-state.sha256`, `modified-source.sha256`, `binary-sha256.txt`,
`evidence-sha256.txt`, and `delivery-sha256.txt`.

## Baseline behavior — exact remote input/output

Command:

```bash
CODEX_TEST_CPU_SET=190,191 taskset -c 190,191 nice -n 15 ionice -c3 \
  bash results/remote-diagnostic-perf.sh \
  results/pool-server-pre-fix pre-fix perf-pre-fix 39081
```

Input: `usage_records=500000`, `audit_log=200000`,
`database_bytes=125243392`.

Literal result (`exit_status=0`, `peak_rss_kib=41792`):

```json
{
  "archive_bytes": 3991141,
  "archive_sha256": "2c704ddf6f10340681c07b9d748c9f2dbf3a3e45a598c88b02213b2a6a1d1171",
  "database_bytes": 125243392,
  "export_elapsed_seconds": 110.714,
  "exported_rows": {
    "audit_log": 200000,
    "usage_records": 500000
  },
  "label": "pre-fix",
  "load_elapsed_seconds": 2.312,
  "manifest_large_table_row_limit": null,
  "manifest_source_row_counts": null,
  "manifest_truncated_tables": null,
  "source_counts": {
    "audit_log": 200000,
    "usage_records": 500000
  },
  "states": [
    {
      "at_ms": 5.774,
      "status": "rendering"
    },
    {
      "at_ms": 92539.958,
      "status": "validating"
    },
    {
      "at_ms": 110709.326,
      "status": "ready"
    }
  ]
}
```

The supplied VPS diagnostic additionally recorded a real job still in
`snapshotting` after at least 272 seconds, a 1,102,053,376-byte partial physical
snapshot, a 2,284,853,152-byte WAL, and only 3.2 GB free. The archive itself
passed CRC and internal SHA-256 checks; the old task never reached `ready`.

## Modified behavior — exact constrained-remote records

### Diagnostic export/download

Same 500k/200k performance input, literal result:

```json
{
  "archive_bytes": 239466,
  "archive_sha256": "5807f7fbaa08ee32efc928ff6f8848ac5ab74995ad738371825510c8719d0110",
  "database_bytes": 125243392,
  "export_elapsed_seconds": 5.246,
  "exported_rows": {
    "audit_log": 20000,
    "usage_records": 20000
  },
  "label": "final",
  "load_elapsed_seconds": 2.279,
  "manifest_large_table_row_limit": 20000,
  "manifest_source_row_counts": {
    "affinity_bindings.csv": 0,
    "audit_log.csv": 200000,
    "billing_holds.csv": 0,
    "cf_events.csv": 0,
    "codex_upstream_attempts.csv": 0,
    "codex_upstream_attempts_daily.csv": 0,
    "diagnostic_events.csv": 0,
    "usage_records.csv": 500000
  },
  "manifest_truncated_tables": {
    "audit_log.csv": {
      "exported_rows": 20000,
      "omitted_rows": 180000,
      "selection": "most_recent",
      "source_rows": 200000
    },
    "usage_records.csv": {
      "exported_rows": 20000,
      "omitted_rows": 480000,
      "selection": "most_recent",
      "source_rows": 500000
    }
  },
  "source_counts": {
    "audit_log": 200000,
    "usage_records": 500000
  },
  "states": [
    {
      "at_ms": 8.876,
      "status": "rendering"
    },
    {
      "at_ms": 4121.284,
      "status": "validating"
    },
    {
      "at_ms": 5244.83,
      "status": "ready"
    }
  ]
}
```

Final isolated runtime smoke command/output:

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

### Final regression delta (429 failover and Kiro catalog wire pagination)

```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/api ./internal/kiro -run TestCodexStatelessContinuationFailsOverOn429\|TestRefreshModelCatalogPartialFailurePreservesLastGood -count=3 -timeout=5m -v
=== RUN   TestCodexStatelessContinuationFailsOverOn429
2026/07/30 14:52:56 [RATE-LIMIT-DETECT] matched rate/quota signal status=429 body_len=72
2026/07/30 14:52:56 [RATE-LIMIT] COOLDOWN: account=acc_e43a60c14f27b366, status=429, duration=1800s, reason=usage_limit
2026/07/30 14:52:56 [USAGE-WARN] account=acc_8922c4d5c74d553d: no usage in body (len=73), using billing_hold estimate=55
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.18s)
=== RUN   TestCodexStatelessContinuationFailsOverOn429
2026/07/30 14:52:56 [RATE-LIMIT-DETECT] matched rate/quota signal status=429 body_len=72
2026/07/30 14:52:56 [RATE-LIMIT] COOLDOWN: account=acc_e43a60c14f27b366, status=429, duration=1800s, reason=usage_limit
2026/07/30 14:52:56 [USAGE-WARN] account=acc_8922c4d5c74d553d: no usage in body (len=73), using billing_hold estimate=55
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.14s)
=== RUN   TestCodexStatelessContinuationFailsOverOn429
2026/07/30 14:52:56 [RATE-LIMIT-DETECT] matched rate/quota signal status=429 body_len=72
2026/07/30 14:52:56 [RATE-LIMIT] COOLDOWN: account=acc_e43a60c14f27b366, status=429, duration=1800s, reason=usage_limit
2026/07/30 14:52:56 [USAGE-WARN] account=acc_8922c4d5c74d553d: no usage in body (len=73), using billing_hold estimate=55
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.16s)
PASS
ok  	codex-account-pool/internal/api	0.594s
=== RUN   TestRefreshModelCatalogPartialFailurePreservesLastGood
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.03s)
=== RUN   TestRefreshModelCatalogPartialFailurePreservesLastGood
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.03s)
=== RUN   TestRefreshModelCatalogPartialFailurePreservesLastGood
--- PASS: TestRefreshModelCatalogPartialFailurePreservesLastGood (0.04s)
PASS
ok  	codex-account-pool/internal/kiro	0.119s
EXIT_STATUS=0
```

The 429 test was first forced to timeout at 12s. Its goroutine dump SHA-256
`9d63ca78d8da6f4bcdbb2c7e54fbacd1280470eb03d2c80ab1c13fc38c54eb5a`
shows the handler at `tryAutoConsumeCodexResetCredit -> pollOneCodexQuota ->
DoRaw` before failover. The final branch selects an already-healthy distinct
account first; three runs returned in 0.18/0.14/0.16s. When no replacement is
available, reset-credit behavior remains covered by:

```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/api -run TestCodexResetCredit\|TestParseCodexResetCredits\|TestCodexStatelessContinuationFailsOverOn429 -count=1 -timeout=10m -v
=== RUN   TestParseCodexResetCredits
=== RUN   TestParseCodexResetCredits/snake_count_zero_is_known
=== RUN   TestParseCodexResetCredits/camel_count
=== RUN   TestParseCodexResetCredits/credits_fallback_counts_available_entries
=== RUN   TestParseCodexResetCredits/numeric_string_count
=== RUN   TestParseCodexResetCredits/partial_numeric_string_is_unknown
=== RUN   TestParseCodexResetCredits/fractional_count_is_unknown
=== RUN   TestParseCodexResetCredits/usage_fallback_snake
=== RUN   TestParseCodexResetCredits/usage_fallback_camel_zero_is_known
=== RUN   TestParseCodexResetCredits/invalid_is_unknown
--- PASS: TestParseCodexResetCredits (0.00s)
    --- PASS: TestParseCodexResetCredits/snake_count_zero_is_known (0.00s)
    --- PASS: TestParseCodexResetCredits/camel_count (0.00s)
    --- PASS: TestParseCodexResetCredits/credits_fallback_counts_available_entries (0.00s)
    --- PASS: TestParseCodexResetCredits/numeric_string_count (0.00s)
    --- PASS: TestParseCodexResetCredits/partial_numeric_string_is_unknown (0.00s)
    --- PASS: TestParseCodexResetCredits/fractional_count_is_unknown (0.00s)
    --- PASS: TestParseCodexResetCredits/usage_fallback_snake (0.00s)
    --- PASS: TestParseCodexResetCredits/usage_fallback_camel_zero_is_known (0.00s)
    --- PASS: TestParseCodexResetCredits/invalid_is_unknown (0.00s)
=== RUN   TestCodexResetCreditHeadersKeepCapturedDesktopFingerprint
--- PASS: TestCodexResetCreditHeadersKeepCapturedDesktopFingerprint (0.00s)
=== RUN   TestCodexStatelessContinuationFailsOverOn429
2026/07/30 14:54:00 [RATE-LIMIT-DETECT] matched rate/quota signal status=429 body_len=72
2026/07/30 14:54:00 [RATE-LIMIT] COOLDOWN: account=acc_e43a60c14f27b366, status=429, duration=1800s, reason=usage_limit
2026/07/30 14:54:00 [USAGE-WARN] account=acc_8922c4d5c74d553d: no usage in body (len=73), using billing_hold estimate=55
--- PASS: TestCodexStatelessContinuationFailsOverOn429 (0.14s)
PASS
ok  	codex-account-pool/internal/api	0.197s
EXIT_STATUS=0
```

### Lossless context compression and concurrent account switching

Input matrix: eight independent downstreams; Codex uses a byte-visible 1 MiB
prompt within the fixed 372,000-token GPT-5.6 contract; Claude uses 1,000,000
`" a"` token-like units. Every run switches account A→B and checks exact
SHA-256 context recovery, tool call/output ordering, tool/call IDs, a 27-digit
integer, isolation from the other seven sessions, compressed-at-rest storage,
and idempotent legacy migration.

Exact command/result record:

```text
COMMAND=GOMAXPROCS=2 taskset -c 190,191 nice -n 15 ionice -c3 go test -p=1 ./internal/storage ./internal/api -run TestGoalAndVirtualContextCompressionIsLosslessAtRest\|TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce\|TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs\|TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults -count=3 -timeout=25m -v
=== RUN   TestGoalAndVirtualContextCompressionIsLosslessAtRest
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.09s)
=== RUN   TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.09s)
=== RUN   TestGoalAndVirtualContextCompressionIsLosslessAtRest
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.07s)
=== RUN   TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.08s)
=== RUN   TestGoalAndVirtualContextCompressionIsLosslessAtRest
--- PASS: TestGoalAndVirtualContextCompressionIsLosslessAtRest (0.07s)
=== RUN   TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce
--- PASS: TestDeferredContextCompressionMigratesLegacyRowsExactlyOnce (0.09s)
PASS
ok  	codex-account-pool/internal/storage	0.513s
=== RUN   TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:30 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:31 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.77s)
=== RUN   TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.79s)
=== RUN   TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs
2026/07/30 14:53:35 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:35 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:35 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:36 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:36 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:36 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:36 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:36 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:37 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.69s)
=== RUN   TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.91s)
=== RUN   TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:41 [USAGE-WARN] account=acc_e639cd5c4d6a1288: no usage in body (len=256), using billing_hold estimate=262209
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
2026/07/30 14:53:42 [USAGE-WARN] account=acc_7bd457d2d4652795: no usage in body (len=110), using billing_hold estimate=262244
--- PASS: TestCodexOneMiBContextConcurrentAccountSwitchPreservesToolPairs (1.79s)
=== RUN   TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults
--- PASS: TestClaudeOneMillionTokenConcurrentAccountSwitchPreservesToolUseResults (3.90s)
PASS
ok  	codex-account-pool/internal/api	17.018s
EXIT_STATUS=0
```

### Final binaries

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

### Frontend and account/provider surfaces

Exact remote command: `npm run verify` after `npm ci`; exit 0. Literal summary:

```text
UI regression contract check passed.
Test Files  10 passed (10)
Tests       60 passed (60)
✓ 2787 modules transformed.
✓ built in 699ms
EXIT_STATUS=0
```

The 60 browser/unit tests include diagnostic Blob download cleanup, error/timeout
termination, one-account JSON export, multi-account ZIP export, local upload and
legacy archive import. Go coverage for all credential shapes/versions, atomic
replacement, custom Claude relay routing, administrator test, no-group URL+key
configuration, maintained Claude model-table discovery, and target-model mapping
ran in the v5 full sweep. That sweep compiled and ran every package; all packages
were green except the two explicitly isolated regressions above. Both final v7
regressions then passed three times, and both final binaries were rebuilt.

### Disk reclaim literal invariants

Command: `reclaim-disk-space.sh --apply ... --retention-days 7
--stale-file-hours 1 --optimize-config --assume-quiesced`; exit 0.

```json
{
  "accounts_sha256": "83729194369589f009b2d126aff5825bc25941e59bb75aafea3bcc953068527f",
  "contexts_sha256": "d6e11815c181209015ed294bb8b9d4f2e13cac347c267e9d32c387cfcbc01082",
  "retained_logs_sha256": "d4ce47f22fcddfc8823a52782e96ca6c99ce3f1a058764fcbbd3cae39fa8a847",
  "archived_rows": 19,
  "database_bytes_before": 8765440,
  "database_bytes_after": 368640,
  "files_bytes_reclaimed": 4194304,
  "sqlite_quick_check": "ok"
}
```

Runnable rollback restored `rows=65`, logical SHA-256
`7d971b36d769b26c5afa20247133339b12664c04b4a9a55d19ef1f7f0231a4d6`,
account/context counts, and the original config SHA-256; exit 0.

## Remote artifact reopen / patch / rollback / reapply

Command:

```bash
/usr/local/bin/rtk taskset -c 190,191 nice -n 15 ionice -c3 \
  bash artifacts/full-fix-20260730/verify-delivery-artifacts.sh \
  /root/autodl-tmp/codex-pool-regression-20260730 \
  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730
```

Input archive SHA-256:
`63ff47abe0ef245c08a250be727f651b74f2c52ea40f13f97b50baad65e49d32`.
Literal output SHA-256:
`20f078896eeab88a3ab4b5c0cecac451cd6aaaee6d08b10d37b9b707917e7a46`.

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
MANIFEST_FILES=1003
MANIFEST_SHA256=verified
MODIFIED_MANIFEST_EXIT=0
ORIGINAL_EXPECTED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
ORIGINAL_REOPENED_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
MODIFIED_EXPECTED_TREE=5e4a67224cbb3ddeabb9ea8a9bc6f1dbc1197aff
MODIFIED_REOPENED_TREE=5e4a67224cbb3ddeabb9ea8a9bc6f1dbc1197aff
PATCH_CHECK_EXIT=0
PATCH_APPLY_EXIT=0
PATCH_APPLIED_TREE=5e4a67224cbb3ddeabb9ea8a9bc6f1dbc1197aff
baseline-to-final.patch: OK
ROLLBACK_SCOPE_CONTENT=verified
ROLLBACK_EXIT=0
ROLLBACK_REPO=REMOTE_ISOLATED_FIXTURE
ROLLBACK_STATE=original
ROLLBACK_TREE=045ca893e746ec72cc684db96bec730168f4ff6c
ROLLBACK_FORWARD_PREFLIGHT_EXIT=0
ROLLBACK_REAPPLY_EXIT=0
ROLLBACK_REAPPLIED_TREE=5e4a67224cbb3ddeabb9ea8a9bc6f1dbc1197aff
0f997108cfc10bf935c39e386c464f8b3e3723180fda6c6c3d0b1128d1034a13  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/modified-source.tar.gz
d62bace0c35ef4c3ccb680c0a02a192b795dbea6b7d4361d553d60615326b9ad  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/baseline-to-final.patch
bdd2c371890a569ec8e851195a406ff93d383f192e25acf57516079fd2eda81d  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/rollback.sh
46afcb5e5afa1f651c0bafe7f94ccd1f8a6095d40a5520376b66511cc76f7faa  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/original-state.tar.gz
da80b57d113f886e008eb0914efd0de83966edbd456b3ec8a335e0c83495b467  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/bin/pool-server-final-v7
6bfbba594ba36910c89bfe2f67d32a6ac2a8cf8bf42ca5d91a7e5d4f801525b9  /root/autodl-tmp/codex-pool-regression-20260730/artifacts/full-fix-20260730/bin/gateway-final-v7
VERIFICATION_RECORD_SHA256=verified-by-delivery-manifest
FOUR_ROLES_REOPENED=verified
REMOTE_ARTIFACT_VERIFICATION_EXIT=0
```

## Runtime constraints

All executable verification ran only in the designated isolated regression root
on the shared server: CPUs 190-191, nice 15, idle I/O priority, `GOMAXPROCS=2`,
Go package parallelism 1. No production service, account database, global package,
or host-wide cache was changed.
