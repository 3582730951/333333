# 前后端、生命周期自动化、安装与正式部署验证记录

## 1. 执行约束与输入

- 所有构建、测试、数据库写入、服务运行与浏览器截图均在远端 Linux 原生进程完成；
  未使用 Docker。
- 本地仅执行源码读取/编辑、补丁生成与证据归档。
- 设计基准：`docs/plan/1.txt`。
- 两份最新诊断包均完成 CRC、时序和指标对比：
  - `diagjob_5a7505dd...zip`，SHA-256
    `dc545343aceac1989ae06d1a4ca7d4c673b73306de97ff911a4138aac3c8bdb7`
  - `diagjob_68c09af2...zip`，SHA-256
    `fd3520c5cbb379a7ae01f03d70d6ca8734d5be95360d7e526e39b95560482c5b`
- 完整诊断分析继续保存在
  `../backend-latest-diagnostics-20260731/comparison.md` 与
  `verification-record.md`。

## 2. 修改行为

### 生命周期分支

```text
credential_ref present
  -> credential_path=access_reference
  -> credential login
  -> import
  -> 跳过 OAuth 与条件验证

credential_ref absent
  -> credential_path=oauth
  -> OAuth
  -> connector 返回 requires_phone=true 时进入条件验证
  -> import
```

两条分支随后统一进入 `active -> quota_check`；当
`quota_remaining_bps <= rotate_threshold_bps`（默认 `100` bps，即 1%）时，
进入 `removing_member -> enqueueing_replacement -> completed`。补位通过现有
registration Handler，选择 `protocol_v2` 或 `browser_v3`。

### 数据与恢复字段

- `state` / `resume_state`
- `credential_path`
- `quota_remaining_bps` / `rotate_threshold_bps`
- `version`
- `lease_owner` / `lease_expires_at`
- `attempt` / `max_attempts` / `next_attempt_at`
- `replacement_method` / `replacement_job_ref`
- `shadow_mode`

## 3. 后端验证

远端命令：

```bash
go test ./... -count=1
```

字面结果：

```text
internal/registration/teamflow  PASS
internal/storage                PASS
internal/api                    PASS
all remaining packages         PASS
real                            5m31.914s
GO_FULL_EXIT=0
```

完整逐包输出：`verification/go-test-all.log`。

定向命令：

```bash
go test ./internal/storage ./internal/registration/teamflow ./internal/api \
  -run 'TeamLifecycle|Lifecycle|Engine|PoolAdapter|RegistrationProviderReloadUsesAtomicPipelineSnapshots' \
  -count=1
```

字面结果：

```text
ok codex-account-pool/internal/storage
ok codex-account-pool/internal/registration/teamflow
ok codex-account-pool/internal/api
TARGETED_EXIT=0
```

Race detector：

```bash
go test -race ./internal/api \
  -run RegistrationProviderReloadUsesAtomicPipelineSnapshots -count=1
```

字面结果：

```text
ok codex-account-pool/internal/api 1.779s
RACE_EXIT=0
```

首次全量测试只暴露 supervisor 子协程 panic 边界不足；加入协调器与心跳边界后，
重新执行完整 `go test ./...` 得到上述通过结果。

## 4. 前端验证

最终精确源码远端命令：

```bash
npm test -- --run
npm run typecheck
npm run build
```

字面结果：

```text
Test Files  17 passed (17)
Tests       78 passed (78)
TYPECHECK_EXIT=0
2788 modules transformed
TeamLifecycle-CgOHqkf4.js 13.86 kB / gzip 3.98 kB
BUILD_EXIT=0
```

日志：

- `verification/frontend-test-final-exact.log`
- `verification/frontend-typecheck-final-exact.log`
- `verification/frontend-build-final-exact.log`

截图验收：

```text
TEAM_LIFECYCLE_VISUAL total=4 passed=4 issues=0
DARK_MODE_SUMMARY total=72 passed=72 issues=0
PRODUCTION_TEAM_LIFECYCLE total=4 passed=4 issues=0
```

每次检查均包含主题值、页面 ready、HTTP >=400、console/page error、根节点横向
溢出、长标识、阈值与页面关键内容。

## 5. 当前源码 `install.sh` 原生安装

隔离目录与端口：

```text
ROOT=/root/autodl-tmp/team-lifecycle-install-20260731
LISTEN_ADDR=127.0.0.1:34277
RELEASE_ID=team-lifecycle-final
```

执行：

```bash
./install.sh --minimal --no-systemd --no-start --no-tests \
  --without-go-install --no-open-firewall --no-migrate-user-groups \
  --listen-addr 127.0.0.1:34277
```

字面结果：

```text
codex-pool-server self-test ok
codex-pool-handoff self-test ok
Install complete.
INSTALL_PHASE=current-final
INSTALL_EXIT=0
```

随后启动安装产物、直接写入隔离 SQLite 并调用新 API：

```text
SERVICE_RELEASE=team-lifecycle-final
READY=1
TEAM_LIFECYCLE_SEEDED=1 workflows=4
states=active,completed,retry_wait,review_required
PRAGMA quick_check=ok
PRAGMA integrity_check=ok
workflow_count=4
SCREENSHOTS=4/4
```

安装器产物：

```text
codex-pool-server.team-lifecycle-final
SHA-256 3b5a7aa6fa106414ff379e79d075ddec56afa9f53872e2688f4a9633e2fd8fd8
```

完整原生安装证据：`team-lifecycle-install-evidence.tar.gz`。

## 6. 正式部署、回滚与再部署

正式服务：

```text
main     127.0.0.1:34273
frontend 127.0.0.1:34274
```

部署前：

```text
release main     = apple-email-overflow-final-main
release frontend = apple-email-overflow-final-frontend
binary SHA       = 429f98e8fb44b62b2fe4f71ca8745923dd6feac83629964e2ec82876e8cd9046
```

先保存原二进制、配置与 SQLite 在线备份，再安装同一个 `install.sh` 产物。

首次部署字面结果：

```text
STARTED=final-main RELEASE=team-lifecycle-final-main
STARTED=final-frontend RELEASE=team-lifecycle-final-frontend
DATA_ASSERTED=1
DATA_ASSERTED=1
TEAM_SCHEMA_ASSERTED=1
TEAM_SCHEMA_ASSERTED=1
DEPLOY_OK=1
```

核心数据保持：

| 服务 | accounts | groups | email_pool | usage_records | registration_jobs |
|---|---:|---:|---:|---:|---:|
| main | 2 | 4 | 0 | 0 | 0 |
| frontend | 14 | 4 | 8 | 288 | 2 |

三类身份指纹（accounts/groups/email_pool）、配置哈希及 SQLite
`quick_check/integrity_check` 在部署前后均一致。

真实回滚：

```text
STARTED=rollback-main RELEASE=apple-email-overflow-final-main
STARTED=rollback-frontend RELEASE=apple-email-overflow-final-frontend
ROLLBACK_BEHAVIOR=previous-release-ready previous-console-restored data-preserved
ROLLBACK_OK=1
```

真实再部署与最终状态：

```text
STARTED=redeploy-main RELEASE=team-lifecycle-final-main
STARTED=redeploy-frontend RELEASE=team-lifecycle-final-frontend
REDEPLOY_BEHAVIOR=final-release-ready final-console-restored data-preserved
REDEPLOY_OK=1
FINAL_STATUS_OK=1
```

最终 API：

```text
credential_persistence=encrypted_account_reference
default_shadow_mode=true
lease_heartbeat=true
rotation_threshold_bps=100
unauthenticated_status=401
```

完整正式部署证据：`team-lifecycle-production-evidence.tar.gz`。远端可执行恢复脚本：
`/root/autodl-tmp/team-lifecycle-production-20260731/production-service-control.sh`；
本地副本：`production-service-control.sh`。

## 7. 补丁正反向验证

补丁：

```text
team-lifecycle-full-source.patch
SHA-256 423514de12022b0ab963d1e867e19edf39817f9d3707c0b3800d8f5d76e09405
```

在远端从 `HEAD` 的受影响文件干净归档重新打开：

```text
PATCH_FORWARD_CHECK=0
MODIFIED_REOPEN_OK=51
PATCH_REVERSE_CHECK=0
PATCH_APPLY_REVERSE_OK=1 BASELINE_FILES=34
```

完整输出：`patch-apply-reverse.literal.log`。

## 8. 四个交付角色

1. **修改产物**：`codex-pool-server.team-lifecycle-final`
2. **补丁**：`team-lifecycle-full-source.patch`
3. **验证记录**：本文件、两个 evidence archive、截图与日志
4. **回滚**：`production-service-control.sh rollback`

四者均已重新打开；二进制经自检和正式运行，补丁经正反应用，验证归档经解包，
回滚脚本已实际执行并再次部署最终版本。

## 9. 临时资源与访问清理

完成所有远端操作后：

```text
TEMP_ENDPOINT_34276=stopped
TEMP_ENDPOINT_34277=stopped
TEMP_PROCESS_MATCHES=0
FINAL_STATUS_OK=1
REMOTE_TEMP_KEY_REMOVED=1
KEY_AUTH_AFTER_REMOVAL_EXIT=255
LOCAL_TEMP_KEY_REMOVED=1
```

隔离运行时配置、数据库和临时管理凭据已删除；正式两个 release 保持运行。
临时 SSH 公钥从远端精确移除，随后启用 `BatchMode` 与 `IdentitiesOnly` 的复核登录
返回 255，本地对应私钥目录也已删除。
