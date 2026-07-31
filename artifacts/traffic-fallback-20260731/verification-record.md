# 流量兜底验证记录

## 范围与环境

- 分支：`cache-hit-optimization`
- 基线提交：`390edea477cb4d16b45133096bf1351d7d593db9`
- 环境：远端 Linux 原生进程，未使用 Docker；本地应用运行、构建和测试均保持暂停。
- Go：`go1.25.12`
- Node：`v22.23.2`
- npm：`10.9.8`
- 验证服务：`http://127.0.0.1:34318`
- 发布标识：`traffic-fallback-review-20260731`
- 最终二进制 SHA-256：`83c255fba2eccb277b0879d3b89f51e4e69845e2e7f183c597109de197bec80d`

最终就绪字面输出（`records/ready-final-current.json`）：

```json
{"checks":{"storage":true},"deployment_state":"active","fencing_token":3,"inflight":0,"ok":true,"ready":true,"release_id":"traffic-fallback-review-20260731","started_at":"2026-07-31T10:26:25.590780411Z","worker_socket":""}
```

## 输入数据

远端 SQLite 直接填充 4 个用户分组、3 个家族、4 条转换和 7 个模型目录项。
核心输入如下，完整值见 `records/seed-manifest.json`：

```text
gpt-5.6-sol  -> GPT 长名称兜底分组 -> gpt-5.5
gpt-5.*      -> GPT 长名称兜底分组 -> gpt-5.4-codex
claude-opus-* -> Claude 兜底分组   -> claude-sonnet-4-5
gemini-3-pro -> Gemini 兜底分组    -> gemini-3-flash
```

## 后端验证

相关测试命令：

```bash
go test ./internal/storage -run 'Test(UserGroupTrafficFallbackRoundTripValidationAndCycleGuard|InitAddsTrafficFallbackColumnsToLegacyUserGroups)' -count=1
go test ./internal/api -run 'Test(AdminUserGroups.*TrafficFallback|UserGroupTrafficFallback.*|TrafficFallbackCandidates.*)' -count=1
```

字面结果（`records/go-fallback-tests.status`）：

```text
storage=0
api=0
```

覆盖：持久化往返、标准化、旧表迁移、自引用、循环、删除保护、API 往返、
不完整配置、精确规则优先、顺序跨组、模型改写、GPT/Claude/Gemini、手动模型、
诊断头、深度上限和带服务端状态请求不重放。

完整验证命令：

```bash
go test ./... -skip 'TestCleanupLegacyDiagnosticSnapshots(RemovesOnlyRegularSnapshotFiles|PreservesOpenFile)' -count=1 -timeout=15m
go vet ./...
```

字面结果（`records/go-all.status`）：

```text
go_test=0
go_vet=0
```

未跳过的首次包级运行仅有两个既有诊断快照测试受远端 `/proc/<pid>/fd/0`
`permission denied` 影响；它们与本批次无关，原始输出保留于
`records/go-focused-test.log`。除此之外的功能包通过；没有删除断言或降低本批次测试标准。

## 前端验证

最终命令：

```bash
cd web-spa
npm run verify
```

`records/frontend-verify-final.status`：

```text
0
```

`verify` 串行完成静态契约、TypeScript、Vitest 和生产构建。字面摘要：

```text
Test Files  17 passed (17)
Tests       84 passed (84)
vite build  exit 0
```

最终嵌入控制台包：`traffic-fallback-modified-console-dist.tar.gz`，SHA-256：
`e925a101f89301c4069aaee3af0358a882b745d8c502dcb39597168c4b855401`。

## 浏览器与视觉验证

命令：

```bash
RUN_ROOT=/root/autodl-tmp/traffic-fallback-20260731 \
  node artifacts/traffic-fallback-20260731/scripts/capture-traffic-fallback.mjs
```

字面结果（`records/screenshot.log`）：

```text
OK desktop-light-editor
OK desktop-dark-selector
OK mobile-light-mapping
OK mobile-dark-selector
SCREENSHOT_SUMMARY total=4 passed=4 issues=0
```

自动检查确认：无本服务 4xx/5xx、无控制台错误、主题匹配、页面和弹窗无横向
溢出、4 条映射均存在、来源/目标模型可见、打开的二级浮层在视口内并包含分组。
人工复核同时发现并修复了浮层点击穿透；修复后的深色桌面与深色移动截图均显示
真实打开并选中 Claude/Gemini 的二级列表。

截图：

- `screenshots/desktop-light-editor.png`
- `screenshots/desktop-dark-selector.png`
- `screenshots/mobile-light-mapping.png`
- `screenshots/mobile-dark-selector.png`

## 旧库迁移与旧二进制回滚

旧版源码来自此前已验证的 `cache-hit-optimization` 归档。旧二进制先创建不含
新列的 SQLite 和哨兵分组；最终二进制打开同一数据库，再用旧二进制回读升级库。

迁移前：

```text
traffic_fallback_groups_json=absent
traffic_fallback_model_mappings_json=absent
quick_check=ok
integrity_check=ok
```

迁移后及旧版回滚（`records/legacy-feature-upgrade.status`）：

```text
OLD_SCHEMA_FALLBACK_COLUMNS=absent
NEW_SCHEMA_FALLBACK_COLUMNS=present
SENTINEL_PRESERVED=1
DEFAULT_FALLBACK_GROUPS={}
DEFAULT_FALLBACK_MAPPINGS=[]
QUICK_CHECK=ok
INTEGRITY_CHECK=ok
OLD_BINARY_ROLLBACK_READY=1
LEGACY_FEATURE_UPGRADE_OK=1
```

此前完整的“旧版 `install.sh` → 数据填充 → 新版 `install.sh` → 旧版回滚 →
新版再部署”证据位于 `../legacy-install-upgrade-20260731/verification-record.md`；
其中安装状态均为 0。本批次额外验证的是新增两列的真实旧库迁移与反向兼容。

## 补丁与回滚验证

基线和修改文件均已生成 SHA-256 清单。远端临时 Git 夹具实际执行：

```bash
artifacts/traffic-fallback-20260731/rollback.sh rollback
artifacts/traffic-fallback-20260731/rollback.sh restore
```

字面结果（`records/rollback-restore.status`）：

```text
ROLLBACK_EXIT=0
RESTORE_EXIT=0
ROLLBACK_RESTORE_OK=1
```

回滚后逐文件匹配基线源码和 55 个基线控制台文件；恢复后逐文件匹配修改源码和
58 个最终控制台文件。详细逐文件输出见 `records/rollback.literal.log` 和
`records/restore.literal.log`。

## 已验证行为

- **基线行为**：没有跨用户分组流量兜底字段；旧用户分组及原路由行为保持不变。
- **修改行为**：当前分组全部候选出现可重试失败时，按家族和顺序进入目标分组，
  改写目标模型；成功响应携带 `X-Pool-Fallback-From-Group`、
  `X-Pool-Fallback-Group`、`X-Pool-Fallback-Model`。
- **约束行为**：已提交流、服务端会话状态、循环、越深、缺映射、无效目标和删除被
  引用分组均被保护。
- **兼容行为**：旧库迁移保持哨兵数据，旧二进制仍可读取升级后的数据库。
