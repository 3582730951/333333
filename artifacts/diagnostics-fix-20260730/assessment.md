# 新诊断包问题对照与修复结论（2026-07-30）

## 输入

- `diagjob_69a...zip`：SHA-256 `96d4afe4f6de724e376427c65321540c816ab544d76d7673ee9661305d31618f`
- `diagjob_f43...zip`：SHA-256 `dcc15f9c066dc312e8f5f7ae5e0eb9b7a709585a9112b11674fe8a546f7a4e5d`
- 两包 CRC、文件清单均通过；较新的包约晚 8,333 秒。

## 确认的问题

| 问题 | 旧包 | 新包 | 结论与修复 |
|---|---:|---:|---|
| Goal 持久化因 `storage_budget` 降级 | 809 | 4,386 | 确认。当前工作区已把官方压缩/编辑终端作为权威历史替换，而不是继续追加旧链；满预算时允许严格缩小的原子替换，并后台回收。 |
| GPT-5.6 缺失上游 usage 的估算超过 372K | 10 行，最大 1,204,000 | 17 行，最大 7,378,934 | 确认。所有超窗行均为 `estimated=1`，真实上游 usage 超窗为 0。本轮把内部调度/账单兜底估算限制到模型公开的 372K 合同，不改请求体或下游响应。 |
| 诊断包看不到实际 Goal 存储上限 | 缺失 | 缺失 | 确认。本轮在 `diagnostic_summary.json.goal_policy` 导出快照一致的有效非敏感策略、来源与字节上限。 |
| CPA/Goal 命名空间基数过低 | 2 namespace / 796 tree | 2 namespace / 1,504 tree | 风险确认。当前工作区已自动组合 API key 与 CLI 原生 Session/Conversation/Thread 标识，不要求 CLI 增加配置，并保留精确旧别名迁移。 |
| 模型回退审计放大 | 3,057 fallback / 3,069 rejection | 3,855 fallback / 3,855 rejection；另有 5,137 条 audit 未导出 | 确认。私有跨组探测之前每次写两条审计，挤满 20K 支持窗口。本轮对私有探测不落终端审计，并保留一条真正终端事件；`model_capability_rejected` 只用于上游真实 `model_not_found`。透明跨组成功仍为 HTTP 200。 |

## 快照中的运行状态（未作为代码缺陷修改）

- Claude 池仅有的账号在窗口末尾进入 `auth_expired`，其 5 小时使用率曾到 100%；这解释了最终无可用候选。另一个精确支持 `claude-opus-4-6-thinking` 的 Antigravity 账号位于不同账号池组。
- 多个 Codex Plus 账号处于额度冷却；大量 `routing_unavailable` 与快照候选状态一致。
- 新包仅 2 个 `held`，均为新鲜 hold；`expired_unsettled=0`，没有陈旧账单锁。
- 本地请求体存储拒绝计数全为 0，文件系统可用空间约 14.45 GB；不是磁盘或 spool 容量问题。

## 下游 CLI 不变性

1. GPT-5.6 修复只改变内部 `EstimatedTokens`，原始 JSON、模型字段、SSE/JSON 响应均不改写。
2. Goal 命名空间从 CLI 已有原生头部自动推导；CLI 仍只需 URL 和 API key。
3. Goal 持久化失败保持服务端审计降级，不覆盖已经成功的上游终端。
4. 跨组候选的私有错误继续留在 attempt buffer，下一候选成功时下游只收到成功响应。
5. Goal 策略仅进入管理员诊断 ZIP，不进入任何模型请求或 CLI 配置。

## 对应回归

- `TestGoalHistoryReplacementReclaimsStorageAtBudgetBoundary`
- `TestClaudeCodeCompactionReplacesDurableGoalHistory`
- `TestGoalContinuityScopesSharedKeyByNativeClientSession`
- `TestGoalNamespaceUsesNativeClaudeAndCodexHeadersWithoutExtraConfig`
- `TestCodexEstimatedTokensWithMetaCapsOnlyFixedContextModels`
- `TestAdminDiagnosticsExportAnonymizesBusinessLogs`
- `TestCapabilityFallbackProbeDoesNotPolluteTerminalAudit`
- `TestClaudeMessagesUserGroupFallsThroughCodexTierToExactAntigravityModel`

完整证据见 `verification.md`、`targeted-verification.log` 与 `full-verification.log`。
