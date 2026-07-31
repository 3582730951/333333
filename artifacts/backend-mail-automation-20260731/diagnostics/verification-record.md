# 后端诊断、修复、验证与部署记录

## 1. 输入与约束

- 执行环境：远端 Linux，原生进程；全程未使用 Docker。
- 本地仅用于源码编辑、静态分析和证据归档；构建、Go 测试、SQLite 行为回归、运行时验证和浏览器截图全部在远端执行。
- 两份输入诊断包：
  - `codex-pool-diagnostics-v3-diagjob_5a7505dd2d92451fad81b00ec52882c1.zip`
    - 生成时间：`2026-07-30T21:37:13Z`
    - SHA-256：`dc545343aceac1989ae06d1a4ca7d4c673b73306de97ff911a4138aac3c8bdb7`
    - ZIP CRC：通过
  - `codex-pool-diagnostics-v3-diagjob_68c09af2bc30401b90c92e4a15bb3e65.zip`
    - 生成时间：`2026-07-30T23:45:34Z`
    - SHA-256：`fd3520c5cbb379a7ae01f03d70d6ca8734d5be95360d7e526e39b95560482c5b`
    - ZIP CRC：通过
- 完整机器可读对比：`comparison.json`
- 人工结论：`comparison.md`

## 2. 根因证据

两次快照的目标连续性存储都精确停在 `268435116` 字节；配置硬上限为
`268435456` 字节，只剩 `340` 字节。较新快照相对前一快照的 7,701 秒内，
新增 2,986 次 `error_code=storage_budget`，约 23.265 次/分钟。

原实现每 15 秒调用：

```text
EnforceGoalStorageBudget(configured_hard_max)
```

存储层只在 `used > max` 时回收；而前台提交在 `projected > max` 时拒绝。因此
`used <= max` 但任何真实新段都放不下时，后台永远不动作，形成硬上限前死区。

另一个可确认问题是重复路由不可用审计。较新增量窗口有 3,447 条路由结果，
其中成功 3,256、429 为 63、5xx 为 128；逐次事实已经保存在有界
`route_attempts`，再为同一非 409 结果重复写审计会缩短支持包的有效窗口。

CPA ambiguous 从 108 增至 310，但支持包不含原始客户端层级字段。本批次不猜测
身份关系，不修改 CPA、计费持有或上游重试语义。

## 3. 修改

### 目标存储低水位

- 硬上限继续用于前台提交，配置兼容性不变。
- 磁盘守护维护目标：
  - 保留量为硬上限的 1/8；
  - 最小 8 MiB、最大 128 MiB；
  - 小容量配置保留一半。
- 256 MiB 配置对应：
  - 硬上限：`268435456`
  - 维护目标：`234881024`
  - 保留余量：`33554432`
- 只回收存储层原有规则允许的终态、无有效 live/compacting/awaiting-tool lease
  的历史；每次仍受原 8 MiB/64 行边界限制。
- 新增可观测字段：
  - `goal_bytes_reclaimed`
  - `goal_storage_target_bytes`
  - `goal_storage_reserve_bytes`
  - 诊断包 `goal_policy.storage_maintenance_*`

### 路由审计

- 相同非 409 诊断键每 30 秒最多写 1 条审计，最多跟踪 2,048 个键。
- `route_attempts` 仍逐次保存。
- HTTP 状态、响应体、`Retry-After`、路由决策均未改变。
- 严格 409 继续逐次写审计。
- `/admin/system`、`diagnostic_summary.json` 和 `runtime_storage.json` 新增聚合指标。

### 修改文件

- `internal/api/disk_guard.go`
- `internal/api/diagnose.go`
- `internal/api/server.go`
- `internal/api/diagnostic_runtime.go`
- `internal/api/diagnostics_export.go`
- `internal/api/system.go`
- `internal/api/disk_guard_test.go`
- `internal/api/diagnostics_export_test.go`
- `internal/api/routing_audit_test.go`

## 4. 基线与修改后行为

同一远端 Go 工具链、同一 SQLite 内存夹具、同一输入：

```text
storage_before=655875
hard_max=1048576
maintenance_target=524288
non409_requests=3
strict409_requests=2
```

基线命令：

```bash
GO=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go
$GO test ./internal/api -run '^TestGoalStorageHeadroomBehaviorProbe$' -count=1 -v
$GO test ./internal/api -run '^TestRoutingAuditBehaviorProbe$' -count=1 -v
```

基线字面输出：

```text
HEADROOM_PROBE before=655875 after=655875 hard_max=1048576 target=524288 reclaimed=0 behavior=hard-limit-only
BASELINE_PROBE_EXIT=0
ROUTING_AUDIT_PROBE non409_requests=3 non409_audits=3 strict409_requests=2 strict409_audits=2
BASELINE_ROUTING_PROBE_EXIT=0
```

修改后使用完全相同命令和输入，字面输出：

```text
HEADROOM_PROBE before=655875 after=393525 hard_max=1048576 target=524288 reclaimed=262350 behavior=proactive-low-watermark
MODIFIED_PROBE_EXIT=0
ROUTING_AUDIT_PROBE non409_requests=3 non409_audits=1 strict409_requests=2 strict409_audits=2
MODIFIED_ROUTING_PROBE_EXIT=0
```

完整日志：

- `remote-evidence/logs/baseline-headroom-probe.log`
- `remote-evidence/logs/modified-headroom-probe.log`
- `remote-evidence/logs/baseline-routing-audit-probe.log`
- `remote-evidence/logs/modified-routing-audit-probe.log`

## 5. 测试与构建

定向命令：

```bash
$GO test ./internal/api \
  -run 'TestGoalStorageMaintenanceTarget|TestRunSafeDiskCleanupCreatesGoalStorageHeadroomBeforeHardLimit|TestRoutingUnavailableAuditCoalescesOnlyNon409Repetitions|TestAdminDiagnosticsExportAnonymizesBusinessLogs' \
  -count=1 -v
```

字面结果：

```text
--- PASS: TestAdminDiagnosticsExportAnonymizesBusinessLogs
--- PASS: TestGoalStorageMaintenanceTarget
--- PASS: TestRunSafeDiskCleanupCreatesGoalStorageHeadroomBeforeHardLimit
--- PASS: TestRoutingUnavailableAuditCoalescesOnlyNon409Repetitions
PASS
TARGETED_GO_TEST_EXIT=0
```

全量命令与结果：

```bash
$GO test ./... -count=1
FULL_GO_TEST_EXIT=0
```

全量包均通过；`internal/api` 用时 340.080 秒。完整逐包输出见
`remote-evidence/logs/full-go-test.log`。

原生构建命令：

```bash
$GO build -buildvcs=false -trimpath -o bin/codex-pool-server ./cmd/pool-server
$GO build -buildvcs=false -trimpath -o bin/codex-pool-gateway ./cmd/gateway
bin/codex-pool-server --self-test
```

字面结果：

```text
POOL_BUILD_EXIT=0
GATEWAY_BUILD_EXIT=0
codex-pool-server self-test ok
SELF_TEST_EXIT=0
BUILD_PIPELINE_EXIT=0
```

构建产物：

- `remote-evidence/bin/codex-pool-server`
  - SHA-256：`659a51c2f880356df14a56f54338fa5028e5fc85bc2e7f587a124fb8280bea39`
- `remote-evidence/bin/codex-pool-gateway`
  - SHA-256：`737c8edc943d9ed7f553239c6712090abfef4470c8ac7340e088c4e050784b07`

## 6. 原生运行时与诊断包回归

隔离服务监听 `127.0.0.1:34275`，release 为
`backend-headroom-smoke`，就绪状态 200。

`/admin/system` 字面关键值：

```json
{
  "goal_storage_target_bytes": 234881024,
  "goal_storage_reserve_bytes": 33554432,
  "coalesce_limit": 1,
  "coalesce_window_seconds": 30,
  "strict_409_uncoalesced": true,
  "route_attempts_preserved": true
}
```

该服务实际生成并下载一份 v3 诊断包：

```text
CREATE 202 queued
JOB ready
ZIP crc_ok=True
SMOKE_DIAGNOSTICS_EXIT=0
```

ZIP SHA-256：
`849a18c6e18f31f3bd21820b119f5b69d37b6bd3983d293775118e725bb63588`。
重新打开后，`diagnostic_summary.json` 与 `runtime_storage.json` 均含新增字段。

## 7. 部署、回滚与重部署

部署到两个原生服务：

- 主服务 release：`backend-headroom-final-main`
- 前端展示服务 release：`backend-headroom-final-frontend`
- 两者二进制 SHA-256 均为
  `659a51c2f880356df14a56f54338fa5028e5fc85bc2e7f587a124fb8280bea39`

首次部署字面结果：

```text
DEPLOY_OK main_pid=504360 frontend_pid=504403
DEPLOY_PIPELINE_EXIT=0
```

实际执行 `rollback.sh`，数据库由 SQLite backup 恢复，字面结果：

```text
ROLLBACK_OK main_pid=504874 frontend_pid=504913
ROLLBACK_PIPELINE_EXIT=0
ROLLBACK_STATE_VERIFY_EXIT=0
```

回滚后原二进制哈希：

- 主服务：`4028a0fd4c5123b6b571dc1f0b355d463e0e60ed83efa1447b549e06109ff74a`
- 前端服务：`c86ca322cd55a994b6af707677eac1b8586f7d9363a3f6aa115475161307265e`

重部署首轮暴露并修正了脚本缺陷：备份二进制文件名不含
`codex-pool-server`，旧停止器未识别回滚进程。停止条件改为匹配稳定的
`--config` 与 `--release-id` 参数契约。旧服务在修正期间保持健康。

修正后实际执行 `redeploy.sh`：

```text
REDEPLOY_OK main_pid=505861 frontend_pid=505906
REDEPLOY_PIPELINE_EXIT=0
FINAL_RELEASE_STATE_EXIT=0
FINAL_LIVE_FIELDS_EXIT=0
FINAL_LIVE_VERIFY_EXIT=0
```

为确保修改后的停止器、回滚器和重部署器是同一份最终脚本，又完整执行了一轮：

```text
ROLLBACK_OK main_pid=507154 frontend_pid=507195
FINAL_ROLLBACK_PIPELINE_EXIT=0
FINAL_ROLLBACK_VERIFY_EXIT=0
REDEPLOY_OK main_pid=507252 frontend_pid=507299
FINAL_REDEPLOY_PIPELINE_EXIT=0
FINAL_REDEPLOY_STATE_EXIT=0
FINAL_REDEPLOY_FIELDS_EXIT=0
```

最终两个 `/readyz`、`/console/` 均为 200，数据库可写，磁盘级别为 normal。
当前两个部署数据库没有 Goal 会话，因此线上守护没有删除业务数据；
低水位回收效果由隔离 SQLite 前后对照证明。

## 8. 补丁与恢复验证

`backend-fix.patch` SHA-256：
`de7c3159a3d48fc394af7f530c89889cf91c2a62b314f750490a352299629163`。

在远端原始源码副本执行：

```bash
git apply --check backend-fix.patch
git apply backend-fix.patch
# 逐文件 SHA-256 与最终源码比对
git apply --reverse --check backend-fix.patch
git apply --reverse backend-fix.patch
# 逐文件 SHA-256 与原始源码比对
```

字面结果：

```text
PATCH_FORWARD_CHECK=0
PATCH_REVERSE_CHECK=0
PATCH_APPLY_REVERSE_OK
PATCH_PIPELINE_EXIT=0
```

完整逐文件哈希见 `remote-evidence/logs/patch-apply-reverse.log`。

## 9. 最终截图

- PNG：`remote-evidence/runtime/final-dashboard-after-backend.png`
- 报告：`remote-evidence/runtime/final-dashboard-after-backend.json`
- PNG SHA-256：
  `694684d5968b6272c69aef789acb70172eba0bdd08bdbe8f179786cb051bf52f`

浏览器审计：

```text
pageReady=true
bodyScrollWidth=1440
bodyClientWidth=1440
visibleCharts=23
failedResponses=[]
consoleErrors=[]
FINAL_POST_REDEPLOY_SCREENSHOT_EXIT=0
```

前端完整 105 张截图矩阵与 73 项测试的证据继续保存在
`../frontend-apple-final-20260731/`。

## 10. 四个可验证角色

1. 修改产物：`remote-evidence/bin/codex-pool-server`
2. 补丁：`backend-fix.patch`
3. 验证记录：`verification-record.md`
4. 回滚：`rollback.sh`（调用同目录 `service-control.sh`）

部署数据库和配置备份保留在远端
`/root/autodl-tmp/backend-latest-fix-20260731/deployment-backup/`；
配置和数据库未下载到本地证据包，避免复制凭据。
