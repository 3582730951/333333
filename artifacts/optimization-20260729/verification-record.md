# Verification Record

Date: 2026-07-29  
Repository: `/workspace`  
Branch: `cache-hit-optimization`  
Baseline commit: `3b484ab1d1148136c724a8dc91ac46c3e89f5ec3`
Preserved Git-index tree: `fd5c6dfecaa01f0922d056ee148edfefab8f7ccf`

All executable tests in this record ran through SSH in:

```text
/root/autodl-tmp/codex-pool-regression-20260729
```

Resource boundary:

```text
taskset -c 190,191
nice -n 15
ionice -c3
GOMAXPROCS=2
go test -p=1
```

## 1. Preserved baseline and final artifact

Baseline binary:

```text
artifacts/optimization-20260729/pool-server-pre-fix
SHA256=d525be4e130037650d30a7d37b482320259bf62483ce32117627afab628289ed
```

Final binary:

```text
artifacts/optimization-20260729/pool-server-final
SHA256=3051663eba78f636f063fc9c69e69885bcff3706ab56ef6ee09661149f7bc21e
```

Preserved source and console baselines:

```text
artifacts/optimization-20260729/source-before.tar.gz
artifacts/optimization-20260729/source-before-sha256.txt
artifacts/optimization-20260729/console-dist-before.tar.gz
artifacts/optimization-20260729/console-dist-before-sha256.txt
```

The repository already contained staged work. The patch was generated against that exact Git index so rollback preserves those staged changes.

## 2. Baseline behavior: diagnostic export

Command:

```bash
CODEX_TEST_CPU_SET=190,191 \
taskset -c 190,191 nice -n 15 ionice -c3 \
bash results/remote-diagnostic-perf.sh \
  results/pool-server-pre-fix pre-fix perf-pre-fix 39081
```

Input:

```text
usage_records=500000
audit_log=200000
database_bytes=125243392
```

Literal result:

```json
{
  "archive_bytes": 3991141,
  "export_elapsed_seconds": 110.714,
  "exported_rows": {
    "audit_log": 200000,
    "usage_records": 500000
  },
  "states": [
    {"at_ms": 5.774, "status": "rendering"},
    {"at_ms": 92539.958, "status": "validating"},
    {"at_ms": 110709.326, "status": "ready"}
  ]
}
```

Exit:

```text
exit_status=0
peak_rss_kib=41792
```

Evidence:

```text
remote-evidence/perf-pre-fix/result.json
remote-evidence/results/perf-pre-fix.log
remote-evidence/perf-pre-fix/pre-fix-diagnostics.zip
```

## 3. Final behavior: diagnostic export

Command:

```bash
CODEX_TEST_CPU_SET=190,191 \
taskset -c 190,191 nice -n 15 ionice -c3 \
bash results/remote-diagnostic-perf.sh \
  results/pool-server-final final perf-final 28793
```

Same input:

```text
usage_records=500000
audit_log=200000
database_bytes=125243392
```

Literal result:

```json
{
  "archive_bytes": 239466,
  "archive_sha256": "5807f7fbaa08ee32efc928ff6f8848ac5ab74995ad738371825510c8719d0110",
  "export_elapsed_seconds": 5.246,
  "exported_rows": {
    "audit_log": 20000,
    "usage_records": 20000
  },
  "manifest_large_table_row_limit": 20000,
  "states": [
    {"at_ms": 8.876, "status": "rendering"},
    {"at_ms": 4121.284, "status": "validating"},
    {"at_ms": 5244.83, "status": "ready"}
  ]
}
```

Exit:

```text
PERF_FINAL_EXIT=0
peak_rss_kib=42408
```

Comparison:

```text
speedup=21.10x
elapsed_reduction=95.26%
archive_reduction=94.00%
source_rows_preserved_in_manifest=700000
exported_rows=40000
```

Evidence:

```text
remote-evidence/perf-final/result.json
remote-evidence/results/perf-final.log
remote-evidence/perf-final/final-diagnostics.zip
```

## 4. Real browser UI download

Command:

```bash
PUPPETEER_CACHE_DIR=toolchains/puppeteer-cache \
LD_LIBRARY_PATH=toolchains/chrome-libs/root/usr/lib/x86_64-linux-gnu \
taskset -c 190,191 nice -n 15 ionice -c3 \
toolchains/node/node remote-ui-diagnostic-download.mjs \
  http://127.0.0.1:28801/console \
  ui-diagnostic-e2e-final/downloads \
  TOKEN \
  ui-diagnostic-e2e-final/result.json
```

Literal output:

```json
{
  "browser": "Chrome/150.0.7871.24",
  "clicked_label": "导出诊断包",
  "filename": "codex-pool-diagnostics-v3-diagjob_98e9eeae67fd4dd58cd5230b0b624b63.zip",
  "bytes": 9415,
  "signature": "504b0304",
  "elapsed_ms": 3087,
  "button_reenabled": true,
  "final_url": "http://127.0.0.1:28801/console/audit"
}
```

Rendered empty-state text:

```text
暂无审计记录
当前筛选条件下没有可展示的审计事件。你可以调整动作筛选或稍后刷新。
```

Exit:

```text
UI_DIAGNOSTIC_E2E_FINAL_EXIT=0
```

Evidence:

```text
remote-evidence/ui-diagnostic-e2e-final/result.json
remote-evidence/ui-diagnostic-e2e-final/downloads/
remote-evidence/results/ui-diagnostic-download-e2e-final.log
```

## 5. WebSocket mapped-session recovery

Before:

```text
5/5 failed
each failure=10.12s to 10.15s
terminal read=i/o timeout
```

After command:

```bash
taskset -c 190,191 nice -n 15 ionice -c3 \
go test -p=1 ./internal/api \
  -run '^TestCodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext$' \
  -count=5 -timeout=5m -v
```

After literal summary:

```text
PASS 5/5
durations=0.15s,0.13s,0.13s,0.13s,0.15s
WEBSOCKET_QUOTA_AFTER_FIX_EXIT=0
```

Evidence:

```text
remote-evidence/results/websocket-quota-flake-rerun.log
remote-evidence/results/websocket-quota-after-fix.log
```

## 6. Daily-use TLS/HTTP2 load

Command inputs:

```text
FIXTURE_COUNT=16
TARGET_TOKENS=32000
TARGET_RPS=10
MINIMUM_ACHIEVED_RPS=8
LOAD_DURATION=8s
LOAD_CONCURRENCY=16
FIXTURE_WORKERS=2
CPU_SET=190,191
MEMORY_LIMIT_KIB=1048576
```

Literal result:

```json
{
  "requests": 80,
  "succeeded": 80,
  "failed": 0,
  "achieved_rps": 10.117349652635186,
  "p50_millis": 6.593808,
  "p95_millis": 11.882925,
  "p99_millis": 23.364471
}
```

Resource output:

```json
{
  "peak_rss_kib": 39116,
  "peak_fds": 15,
  "peak_threads": 8,
  "peak_spool_bytes": 6,
  "peak_goroutines": 26
}
```

Protocol gate:

```json
{"bytes_read":3794650,"http2_requests":80,"requests":80}
```

Exit:

```text
EXTREME_LOAD_DAILY_FINAL_EXIT=0
```

## 7. Metrics

Microbenchmark:

```text
BenchmarkHTTPRequestMetricsObserve-2 = 18.23–18.41 ns/op
0 B/op
0 allocs/op
```

Final binary smoke:

```text
unauthenticated_status=401
authenticated_exit=0
metric_lines=150
route_labels=route="admin",route="auth",route="health",route="inference",route="other",route="user"
```

## 8. Test matrix

```text
go vet ./...
GO_VET_EXACT_FINAL_EXIT=0

go test -p=1 ./... -count=1 -timeout=45m
GO_FULL_TEST_EXACT_FINAL_EXIT=0

go test -race -p=1 ./internal/api -run 'Diagnostic|HTTPRequestMetrics|AdminMetrics|CodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext'
GO_RACE_TARGETED_FINAL_EXIT=0

go test -race -p=1 ./internal/scheduler ./internal/bodysource ./internal/usagejournal
GO_RACE_CORE_EXIT=0

npm run verify
9 test files passed
47 tests passed
typecheck passed
visual smoke passed
production build passed
```

## 9. Rollback execution

Command:

```bash
bash artifacts/optimization-20260729/rollback.sh \
  /root/autodl-tmp/codex-pool-regression-20260729/rollback-validation
```

Literal final output:

```text
ROLLBACK_EXIT=0
ROLLBACK_SOURCE_BASE=verified
ROLLBACK_CONSOLE_FILES=55
ROLLBACK_COMPARE_EXIT=0
ROLLBACK_SOURCE_FILES=11
ROLLBACK_DIST_COMPARE=sha256_verified
```

Rollback verified:

- 11 tracked source files match the preserved Git-index baseline.
- Three added source/test files and the added report are removed.
- All 55 embedded console files match the preserved baseline SHA-256 manifest.
- The forward patch preflight succeeds on the restored tree.

The patch and rebuilt console were then applied to that restored tree and hashed:

```text
PATCH_FORWARD_EXIT=0
PATCH_FORWARD_SOURCE_FILES=15
PATCH_FORWARD_CONSOLE_FILES=55
```

The rollback script was executed once more after the forward application:

```text
ROLLBACK_EXIT=0
ROLLBACK_SOURCE_BASE=verified
ROLLBACK_CONSOLE_FILES=55
```

Evidence:

```text
remote-evidence/results/rollback-validation-final.log
remote-evidence/results/patch-forward-validation.log
remote-evidence/results/rollback-after-forward.log
```

## 10. Shared-server cleanup

After local evidence and binaries were hash-verified:

```text
REMOTE_LISTENER_CHECK=clean
REMOTE_ISOLATED_SIZE_REMOVED=3.9G
REMOTE_CLEANUP_EXIT=0
SSH_CONTROL_CLOSED=1
```

Evidence:

```text
remote-cleanup.txt
```

## 11. Final delivery revalidation

After the live GitHub completion audit corrected the active-project set and
added the explicit original-requirement evidence matrix, the regenerated source
patch and manifests were uploaded to a fresh isolated directory on the shared
server. The executable source and console files were unchanged from the
already-tested final binary. The preserved source and console baselines were
extracted, the patch and final console bundle were applied, every final SHA-256
entry was checked, four report gates were evaluated, and the runnable rollback
was executed and checked against both baseline manifests.

Literal completion markers:

```text
COMPLETION_VALIDATION_BEGIN
RESOURCE_CPU_SET=190,191
RESOURCE_NICE=15
RESOURCE_IO_CLASS=idle
REPORT_GATES_EXIT=0
FORWARD_EXIT=0
ROLLBACK_EXIT=0
ROLLBACK_EXEC_EXIT=0
ROLLBACK_VERIFY_EXIT=0
COMPLETION_VALIDATION_END
REMOTE_COMPLETION_DIR_REMOVED=1
REMOTE_MATCHING_DIRS_REMAINING=0
LOCAL_CONTROL_SOCKET_CLOSED=1
```

Evidence:

```text
delivery-revalidation.txt
delivery-remote-cleanup.txt
```
