# 八项修复与全前端 UI/UX 验证记录

## 1. 对象、基线与输入

- 工作树：`/workspace`
- 分支：`cache-hit-optimization`
- 基线/HEAD：`a14151d862aaf74d21d68f759ab6b2aecd0f2744`
- 执行日期：2026-07-31 至 2026-08-01（America/New_York）
- 实施约束：`AGENTS.md` → `RTK.md`，并严格按 `docs/plan/1.txt` 的“先审计、建基线、渐进实施、可回滚、最终回归”执行。

最终交付对象：

| 角色 | 绝对路径 | 内容 |
|---|---|---|
| 修改源码包 | `/workspace/artifacts/eight-issue-fix-20260731/modified-source.tar.gz` | 121 个最终存在文件 + 文件级 SHA-256 + 49 个删除路径清单 |
| 完整补丁 | `/workspace/artifacts/eight-issue-fix-20260731/complete.patch` | 170 个 diff，包含跟踪修改/删除、新文件、被 Git 忽略的报告/手册 |
| 验证记录 | `/workspace/artifacts/eight-issue-fix-20260731/verification.md` | 本文件 |
| 可执行回滚 | `/workspace/artifacts/eight-issue-fix-20260731/rollback.sh` | 只恢复 98 个基线路径并删除 72 个本次新增文件 |

## 2. 八项需求的行为证据

| # | 修改后行为 | 定向证据 |
|---|---|---|
| 1 | 邮箱池兼容数组、`data/items/rows`、新旧分页名和空时间；错误对象和 HTML 仍会带 request ID 失败 | `contracts.test.ts` 的 current/legacy email pool 与 malformed/error 用例 |
| 2 | 生成链接复制优先 Clipboard API，HTTP 下同步 textarea 降级，最后选中可见 URL | `browser-clipboard.test.ts` 3/3，`oauth-copy.test.tsx` 2/2 |
| 3 | 已安装实例转更新；快照去重，按数量/年龄/字节上限轮转；旧受管源文件被收敛 | `bash scripts/test-upgrade-safety.sh` |
| 4 | 新库不再生成 DeepSeek/SiliconFlow；旧历史默认仅在未修改且未引用时清理 | `TestFreshStoreDoesNotSeedExampleCustomProviders`、`TestLegacyProviderSeedCleanupIsConservative` |
| 5 | `custom_providers.routes_json` 支持单供应商多下游入口路由，旧 base URL/协议仍是默认回退 | `TestCustomProviderSelectsEndpointByDownstreamPath`、routes round-trip/reject tests、`provider-routes.test.ts` |
| 6 | 亲和键与 Cookie jar 包含 downstream scope + route fingerprint，同 key 稳定、不同 key 隔离 | `TestCustomProviderDownstreamNamespacesAffinityAndCookies` |
| 7 | 仓库内 Cloudflare Worker + D1 + Wrangler + 部署脚本；注册/住宅 IP/邮箱/Team 为可复制粘贴步骤 | Worker 4/4，`contracts.test.ts` deployment-command 用例，运维手册 |
| 8 | Team Lifecycle API 返回 readiness，UI 改为顺序就绪清单、预设和日常/高级分层 | Team Lifecycle Go tests，presentation 2/2，全路由截图 |

定向命令：

```bash
/root/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go test -v ./internal/api ./internal/storage -run 'Test(CloudflareMailbox|AdminProviders|CustomProvider|TeamLifecycle|FreshStore|LegacyProviderSeed)'
npm --prefix web-spa test -- --reporter=verbose tests/contracts.test.ts tests/browser-clipboard.test.ts tests/oauth-copy.test.tsx tests/provider-routes.test.ts tests/team-lifecycle-presentation.test.tsx
```

字面结果：

```text
ok  codex-account-pool/internal/api
ok  codex-account-pool/internal/storage
PASS
Test Files  5 passed (5)
Tests  37 passed (37)
```

退出状态：`0`。完整输出：`verification-eight-issues-go.log`、`verification-eight-issues-frontend-final.log`。

## 3. 前端基线与修改后对比

### 3.1 基线

精确命令：

```bash
UI_REVIEW_RECORD_ONLY=1 npm --prefix web-spa run capture:ui-review
```

输入：固定 seed `ui-review-v6.1-fixed-seed`，30 规范页面，light/dark，1440×900 / 1280×720 / 390×844 / 360×800，长值、空、加载、错误、权限、下载失败 fixture。

字面结果（`ui-baseline/summary.json`）：

```text
exit_status=1
canonical_page_captures=240
canonical_page_failures=240
table_text_overflow_pages=20
table_text_overflow_items=24
clipped_control_pages=240
clipped_control_items=322
page_overflow_pages=0
sibling_overlap_pages=0
fatal_stage=audit export failure state text wait
```

基线失败由 28px 中文标题字形裁切和表格长值越界触发；最后的状态 fixture 还等待了不稳定的错误正文。

### 3.2 修改后

精确命令：

```bash
npm --prefix web-spa run capture:ui-review
```

输入与基线相同，额外完整走到下载成功。字面结果（`ui-final/final/summary.json`）：

```text
exit_status=0
canonical_page_combinations=240
unique_page_combinations=240
page_failures=0
fatal_errors=0
visual_issues=0
png_count=508
state_capture=15
download_capture=1
download_signature=504b0304
```

`visual_issues=0` 同时要求：页面溢出 0、表格文字越界 0、flex/grid 同级重叠 0、图表文字重叠 0、控件裁切 0、空白图表 0。

证据目录：`/workspace/artifacts/eight-issue-fix-20260731/ui-final/final/`，内含 508 张 PNG、fixture、浏览器脚本、结构化日志、summary 和 514 条 SHA-256。

### 3.3 UI 实现和静态资源

```text
production CSS: 132036 B -> 128720 B
dist bytes:     1691330 B -> 1689107 B
source CSS:      160362 B -> 157693 B
gradients:             22 -> 1 (skeleton shimmer)
non-none backdrop:     12 -> 0
DOM median/P95:   268/688 -> 268/688
resource median/P95: 144/150 -> 144/150
```

不把本地导航时序的小幅波动宣称为生产性能改善。可确认的是 DOM 和资源数没有膨胀，CSS 与总构建字节略降。

## 4. 全局回归命令、字面输出与状态

### 前端静态、类型、组件测试和构建

```bash
npm --prefix web-spa run verify
```

```text
UI inventory check passed.
Semi references: 0
Routes: 48
Test Files  21 passed (21)
Tests  100 passed (100)
✓ built in 6.06s
```

退出状态：`0`。完整输出：`verification-frontend-final.log`。

### 端到端、视觉与无障碍

```bash
npm --prefix web-spa run test:e2e
```

```text
Running 54 tests using 1 worker
11 axe WCAG A/AA page cases passed
54 passed (7.2m)
```

退出状态：`0`。完整输出：`verification-e2e-final.log`。

### Go 全仓

```bash
/root/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64/bin/go test ./...
```

```text
ok  codex-account-pool/internal/api
ok  codex-account-pool/internal/storage
ok  codex-account-pool/internal/registration/teamflow
ok  codex-account-pool/internal/upstream
ok  codex-account-pool/internal/web
(所有列出包通过，无测试文件的包标记 [no test files])
```

退出状态：`0`。完整字面输出：`verification-go-final.log`。

### Cloudflare Worker

```bash
npm --prefix deploy/cloudflare-mailbox test
```

```text
tests 4
pass 4
fail 0
```

退出状态：`0`。完整输出：`verification-worker-final.log`。

### 安装/更新/备份/受管源

```bash
bash scripts/test-upgrade-safety.sh
scripts/generate-managed-source-manifest.sh
```

```text
PASS: install dispatch, bounded backups, and managed-source convergence
managed_source_manifest_idempotent=PASS
manifest_lines=943
deleted_paths_in_manifest=0
```

退出状态：`0`。完整输出：`verification-upgrade-final-2.log`、`verification-scripts-final.log`。

### 格式与语法

```text
git_diff_check=PASS
gofmt_check=PASS
json_parse=PASS
shell_syntax=PASS
```

证据：`verification-format-final.log`、`verification-scripts-final.log`。

## 5. 交付物重开、执行和回滚验证

### 源码包

```text
gzip test=PASS
archive_hashes=121
archive_regular_files=125 (121 source + 4 DELIVERY-MANIFEST files)
content_modes=0644
executable_script_modes=0755
```

### 完整补丁

在从基线新建的 detached worktree 中执行：

```bash
git -c core.fileMode=false apply --check --whitespace=nowarn complete.patch
git -c core.fileMode=false apply --whitespace=nowarn complete.patch
```

字面输出：

```text
final_patch_check_exit=0
final_patch_apply_exit=0
final_patch_source_hashes=PASS files=121
patch_check_warnings=0 patch_apply_warnings=0
```

补丁态还实际执行了：定向 Go tests、5 个前端文件/37 tests、TypeScript typecheck、Worker 4 tests 和升级安全测试，全部退出 0。完整输出：`artifact-validation.log`、`final-patch-rollback-validation.log`。

### 回滚

同一 detached worktree 应用最终补丁后执行：

```bash
/workspace/artifacts/eight-issue-fix-20260731/rollback.sh
```

字面输出：

```text
rollback_verified baseline=a14151d862aaf74d21d68f759ab6b2aecd0f2744 tracked_restored=98 added_removed=72
final_rollback_exit=0
final_rollback_tree_matches_baseline=PASS
```

回滚只覆盖本次路径清单，不使用全仓 `reset --hard` 或无边界 clean。

## 6. 交付物 SHA-256（自身哈希见 `artifact-sha256.txt`）

```text
c429a34de06e6cb518517288977dbe8a6f70cb7e0b5e1f93449d1b7ca1ba553d  modified-source.tar.gz
d4bda96d014de5c0faabfe69180bf3c8d1c9156e9d71d67cf4f696f72c129944  complete.patch
dd9bb7db846b0de585472ca051ed78bd7424fafe841bb6293a60e6100104dae5  rollback.sh
```
