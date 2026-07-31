# 后端自动注册、邮箱、Team 生命周期与 Apple UI 验证记录

## 1. 输入与固定基线

- 当前分支：`cache-hit-optimization`
- 基线提交：`390edea477cb4d16b45133096bf1351d7d593db9`
- 旧版复核副本：`legacy-cache-hit-optimization/`
- 执行环境：远端 Linux 原生进程；未使用 Docker。
- 远端工具链：
  - Go `1.25.12`
  - Node `22.23.2`
  - Chromium `150.0.7871.24`
- 两份诊断输入：

```text
dc545343aceac1989ae06d1a4ca7d4c673b73306de97ff911a4138aac3c8bdb7  example_zip/codex-pool-diagnostics-v3-diagjob_5a7505dd2d92451fad81b00ec52882c1.zip
fd3520c5cbb379a7ae01f03d70d6ca8734d5be95360d7e526e39b95560482c5b  example_zip/codex-pool-diagnostics-v3-diagjob_68c09af2bc30401b90c92e4a15bb3e65.zip
```

完整差分：`diagnostics/comparison.md`、`diagnostics/comparison.json`。

## 2. 旧版安装基线

旧提交在独立目录、端口 `34276` 执行：

```bash
SOURCE_DIR=/root/autodl-tmp/legacy-install-upgrade-20260731/old-src \
PHASE=old RELEASE_ID=legacy-cache-hit-optimization \
bash remote-install-version.sh
```

字面结果：

```text
INSTALL_PHASE=old
INSTALL_EXIT=0
SERVICE_RELEASE=legacy-cache-hit-optimization
READY=1
```

直接 SQLite 填充后，在同一配置/数据库运行新版 `install.sh`：

```text
INSTALL_PHASE=new-v2-final
INSTALL_EXIT=0
SERVICE_RELEASE=apple-backend-optimized-v2-20260731
READY=1
```

升级前后逻辑指纹保持：

```text
logical fingerprint before = 9295ea6d34bdefe8e7152905920fef7b8c39e743fb2beb8806c2bc19071af869
logical fingerprint after  = 9295ea6d34bdefe8e7152905920fef7b8c39e743fb2beb8806c2bc19071af869
quick_check                 = ok
integrity_check             = ok
```

旧分支安装、数据、截图、回滚和重部署完整记录：
`legacy-upgrade/verification-record.md`。

## 3. 诊断行为基线与修改后对照

输入：

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

修改后使用同一命令和输入：

```text
HEADROOM_PROBE before=655875 after=393525 hard_max=1048576 target=524288 reclaimed=262350 behavior=proactive-low-watermark
MODIFIED_PROBE_EXIT=0
ROUTING_AUDIT_PROBE non409_requests=3 non409_audits=1 strict409_requests=2 strict409_audits=2
MODIFIED_ROUTING_PROBE_EXIT=0
```

详细记录：`diagnostics/verification-record.md`。

## 4. 后端测试

### 4.1 全量 Go

命令：

```bash
cd /root/autodl-tmp/backend-mail-automation-20260731/src
/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go \
  test ./... -count=1 -timeout=15m
```

字面尾部：

```text
ok   codex-account-pool/internal/registration/teamflow  0.362s
ok   codex-account-pool/internal/storage                9.802s
ok   codex-account-pool/internal/upstream               4.305s
GO_ALL_GREEN_EXIT=0
```

`internal/api` 用时 `201.947s`。完整输出：
`remote-verification-records/go-all-green.literal.log`。

### 4.2 vet 与 race

命令：

```bash
$GO vet ./...
$GO test -race \
  ./internal/storage \
  ./internal/registration/teamflow \
  ./internal/registration/provider/mailbox \
  ./internal/registration/pipeline \
  -count=1 -timeout=10m
```

字面结果：

```text
ok codex-account-pool/internal/storage                           12.400s
ok codex-account-pool/internal/registration/teamflow              1.427s
ok codex-account-pool/internal/registration/provider/mailbox       1.066s
ok codex-account-pool/internal/registration/pipeline               1.200s
GO_VET_RACE_EXIT=0
```

API 并发/生命周期/邮箱精确命令与结果：

```text
COMMAND=/root/autodl-tmp/cpupg-20260730/toolchains/go1.25.12/bin/go test -race ./internal/api -run ^(TestCloudflareMailboxProfileEncryptedDefaultsAndProbe|TestCloudflareMailboxUnsettingAndDeleteClearDefaults|TestTeamLifecycleAdminAPIIdempotencyAndNoSecretFields|TestTeamLifecycleAPIRequiresIdempotencyKey|TestRunBoundedConcurrency|TestRunBoundedCancellation|TestRunBoundedLimitFloor|TestRegistrationRuntimeStopCancelsAndWaitsForJobs|TestRegistrationPersistenceContextIsDetachedButBounded|TestRegistrationProviderReloadUsesAtomicPipelineSnapshots|TestProcessBatchRecordsWorkerPanicAsFailure|TestNativeTeamLifecycleConnectorInvitePATImportAndRemove|TestNativeTeamLifecycleCredentialRejectionFallsBackToOAuth|TestRegisteredReplacementEnqueuesNextExecuteWorkflow)$ -count=1 -timeout=10m
ok   codex-account-pool/internal/api  8.691s
GO_API_RACE_EXACT_EXIT=0
```

完整输出：

- `remote-verification-records/go-vet-race.literal.log`
- `remote-verification-records/go-api-race-exact.literal.log`

## 5. 前端测试与构建

命令：

```bash
cd /root/autodl-tmp/backend-mail-automation-20260731/src/web-spa
npm run verify
```

该命令依次执行全部静态契约、真实 visual smoke、TypeScript、Vitest 和生产构建。
字面结果：

```text
Test Files  17 passed (17)
Tests       78 passed (78)
2789 modules transformed.
✓ built in 488ms
FRONTEND_VERIFY_GREEN_EXIT=0
```

关键构建产物：

```text
CloudflareMailbox-FruMoXg3.js   9.52 kB | gzip 2.84 kB
TeamLifecycle-EpVx7Zra.js      14.99 kB | gzip 4.25 kB
Dashboard-Cx-QW6u9.js          16.18 kB | gzip 4.65 kB
Registration-BG6AAUPm.js       20.73 kB | gzip 6.51 kB
```

完整输出：`remote-verification-records/frontend-verify-green.literal.log`。

## 6. Python OAuth Worker 与安装器

静态/单元命令：

```bash
bash -n scripts/install.sh scripts/rollback-release.sh
cd services
python3 -m unittest codex_reauth_worker_test.py
source ../scripts/install.sh
render_runtime_config | python3 -m json.tool
```

字面结果：

```text
.....
Ran 5 tests in 1.661s
OK
RENDERED_REAUTH_URL=http://127.0.0.1:18802
INSTALLER_REAUTH_STATIC_EXIT=0
```

完整记录：
`remote-verification-records/installer-reauth-static.literal.log`。

## 7. 原生完整安装

执行：

```bash
ROOT=/root/autodl-tmp/backend-mail-automation-install-20260731 \
SOURCE_DIR=/root/autodl-tmp/backend-mail-automation-20260731/src \
bash remote-install-current.sh
```

内部调用：

```bash
./install.sh --full --without-sidecar --with-registration --without-warp \
  --no-systemd --no-start --no-tests --without-go-install \
  --no-open-firewall --no-migrate-user-groups \
  --listen-addr 127.0.0.1:34277
```

字面结果：

```text
codex-pool-server self-test ok
codex-pool-handoff self-test ok
Registration: node engine ...; OAuth reauth @ http://127.0.0.1:34279
CONFIG_REAUTH_URL=http://127.0.0.1:34279
READY http://127.0.0.1:34277/readyz ... "release_id":"backend-mail-automation-install-20260731" ...
READY http://127.0.0.1:34279/healthz {"ready": true, "service": "codex-reauth-worker"}
INSTALL_CURRENT_EXIT=0
```

完整输出：
`remote-verification-records/install-current-wrapper.literal.log`。

## 8. 直接 SQLite 填充

服务停止后执行：

```bash
ROOT=/root/autodl-tmp/backend-mail-automation-install-20260731 \
python3 seed-current-sqlite.py
```

首次数据把 14 天 usage 放入默认 7 天保留窗口之外；服务正确清理为 168 条。种子改成
6 天窗口、备份只创建一次、审计写入幂等后重新执行。最终字面结果：

```text
DB_VERIFY {"accounts": 16, "cloudflare_profiles": 3, "email_pool": 20,
 "fixture_audits": 56, "oauth_paths": 4, "pat_paths": 2,
 "quota_at_or_below_1_percent": 3, "usage_records": 288, "workflows": 8}
DB_INTEGRITY=ok
BACKUP before-direct-seed.sqlite3 integrity=ok accounts=0
BACKUP after-direct-seed.sqlite3 integrity=ok accounts=16
DIRECT_SEED_RERUN_EXIT=0
```

最长账号邮箱 `102` 字符，最长账号标签 `73` 字符，最长邮箱池地址 `98` 字符。
完整记录：
`remote-final/records/direct-seed-rerun.literal.log`。

## 9. 明暗模式与布局截图

输入矩阵：

- 路由：总览、账号池、邮箱池、自动注册、Team 生命周期、Cloudflare 邮箱、
  系统监控、设置。
- 视口：`1440×1000`、`390×844`。
- 主题：light、dark。
- 检查：`data-page-ready`、主题、fixture、1% 阈值、图表、同源 HTTP >=400、
  console/page error、文档横向溢出、表格子元素越界。

命令：

```bash
BASE_URL=http://127.0.0.1:34277 \
CONFIG_FILE=/root/autodl-tmp/backend-mail-automation-install-20260731/etc/config.json \
OUTPUT_ROOT=/root/autodl-tmp/backend-mail-automation-install-20260731/screenshots/final \
CHROME_BIN=/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome \
node scripts/capture-final-ui.mjs
```

字面结果：

```text
FINAL_UI_VISUAL total=32 passed=32 issues=0 screenshots=33
FINAL_UI_VISUAL_RERUN_EXIT=0
```

主题实际 canvas：

```text
light = rgb(245, 245, 247)
dark  = rgb(28, 28, 30)
```

报告：`remote-final/screenshots/final/final-ui-visual-report.json`。

## 10. 生产部署、真实回滚与重部署

生产控制脚本：
`production-control.sh`。

### 部署

```bash
/root/autodl-tmp/backend-mail-automation-production-20260731/production-control.sh deploy
```

字面结果：

```text
PREPARE_OK
READY http://127.0.0.1:8802/healthz {"ready": true, "service": "codex-reauth-worker"}
READY http://127.0.0.1:34273/readyz ... "release_id":"backend-mail-automation-final-main" ...
READY http://127.0.0.1:34274/readyz ... "release_id":"backend-mail-automation-final-frontend" ...
SURFACE_VERIFY ... /console/ 200 ... /admin/register/readiness 200 ...
  /admin/team-lifecycle/stats 200 ... /admin/email-pool/cloudflare 200 ...
DEPLOY_OK
FINAL_LOGGED_DEPLOY_EXIT=0
```

### 回滚

```bash
/root/autodl-tmp/backend-mail-automation-production-20260731/production-control.sh rollback
```

字面结果：

```text
release_id":"team-lifecycle-final-main"
release_id":"team-lifecycle-final-frontend"
ROLLBACK_MAIN_DB ... "integrity":"ok" ...
ROLLBACK_FRONT_DB ... "integrity":"ok" ...
ROLLBACK_OK ... worker=stopped
ROLLBACK_PIPELINE_EXIT=0
```

回滚后数据库语义 SHA-256 与部署前备份一致：

```text
main     6c860e8e956d80af5504e1d86fd2cd165908197a7c904292479a7765c6910984
frontend 1057cb21e027693ce80e392bb0912b8f6587f4851e01ab1e9a898db11c356956
```

### 重部署和最终状态

```bash
/root/autodl-tmp/backend-mail-automation-production-20260731/production-control.sh redeploy
/root/autodl-tmp/backend-mail-automation-production-20260731/production-control.sh verify
```

字面结果：

```text
REDEPLOY_OK
REDEPLOY_PIPELINE_EXIT=0
FINAL_VERIFY_OK
FINAL_VERIFY_PIPELINE_EXIT=0
```

最终两个生产 release：

```text
backend-mail-automation-final-main
backend-mail-automation-final-frontend
```

生产二进制 SHA-256：

```text
b2c0d6d718f0b880a3901768e2ce7007bda19cd2349b779d3693ec958e2b5543
```

完整记录：

- `remote-production/records/deploy.literal.log`
- `remote-production/records/rollback.literal.log`
- `remote-production/records/redeploy.literal.log`
- `remote-production/records/final-verify.literal.log`
- `remote-production/records/final-live-verify.literal.log`
- `remote-production/records/deployment-manifest.json`

交付封装前再次在远端运行同一 `production-control.sh verify`，字面结果为：

```text
READY http://127.0.0.1:8802/healthz {"ready": true, "service": "codex-reauth-worker"}
READY http://127.0.0.1:34273/readyz ... "release_id": "backend-mail-automation-final-main" ...
READY http://127.0.0.1:34274/readyz ... "release_id": "backend-mail-automation-final-frontend" ...
FINAL_VERIFY_OK
REMOTE_FINAL_LIVE_VERIFY_STATUS=0
```

## 11. 补丁与产物复核

补丁生成自基线提交，包含 68 个修改文件和 30 个新增源码/测试文件，共 98 个文件。

复核命令：

```bash
git archive HEAD | tar -x -C .run/backend-mail-patch-verify-20260731
git apply --check \
  --directory=.run/backend-mail-patch-verify-20260731 \
  artifacts/backend-mail-automation-20260731/backend-mail-automation.patch
git apply \
  --directory=.run/backend-mail-patch-verify-20260731 \
  artifacts/backend-mail-automation-20260731/backend-mail-automation.patch
while read file; do
  cmp -s "$file" ".run/backend-mail-patch-verify-20260731/$file"
done < artifacts/backend-mail-automation-20260731/source-files.manifest
```

字面结果：

```text
PATCH_VERIFY_EXIT=0
FILES=98
BYTES=656838
SHA256=31ca8f4bc03d2ecff99d0f927fdac283da0d8af3567aa0b6e090e486c8f85ae5
```

修改产物：

```text
b2c0d6d718f0b880a3901768e2ce7007bda19cd2349b779d3693ec958e2b5543  modified/codex-pool-server-linux-amd64
bd0d2d0f99845c2f9d0ac391bdbd7b0ba817e367eef3e569efed7e9add4a445e  modified/codex_reauth_worker.py
d2bf23105ca2d2fdb0cb75dcb4e6e3d2b3e189a7982b694b48320d4ea9dabe73  modified/backend-mail-automation-source-files.tar.gz
```

下载的二进制哈希与两个生产 release、隔离安装 release 完全一致，并已经实际运行。

## 12. 四个交付角色

1. 修改产物：`modified/codex-pool-server-linux-amd64`
2. 补丁：`backend-mail-automation.patch`
3. 验证记录：`verification-record.md`
4. 可运行回滚：`production-control.sh rollback`

原始二进制、配置和两份 SQLite 已保存在远端：

```text
/root/autodl-tmp/backend-mail-automation-production-20260731/backup/
```

`production-control.sh rollback` 已真实执行；随后又执行 `redeploy`，最终保持新版本。
