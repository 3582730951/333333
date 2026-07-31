# 2026-07-25 之后仍维护项目的对比与引入矩阵

检索时间：2026-07-31。维护判定采用 GitHub 官方仓库的 `pushed_at` 与提交时间；
只有 2026-07-25 之后存在实际提交/推送的项目进入借鉴集。

| 项目 | 截止后维护证据 | 借鉴点 | 本项目落地 | 引入方式 |
|---|---|---|---|---|
| [openai/codex](https://github.com/openai/codex) | 2026-07-30 `3016671` 支持 automation account plan；仓库 7/31 仍推送 | 显式 plan/能力契约、执行路径可观测 | replacement method、credential path、shadow plan 与事件均显式持久化 | 引入设计契约，零新增运行时依赖 |
| [microsoft/playwright](https://github.com/microsoft/playwright) | 2026-07-30 仍有 runner/trace 更新 | 隔离浏览器上下文、失败截图、trace 思路 | 每主题/设备独立 page；记录 console、HTTP、溢出与全页截图 | 引入验证模式；现有 Puppeteer 夹具保持轻量 |
| [browser-use/browser-use](https://github.com/browser-use/browser-use) | 2026-07-27 发布 0.13.7 / Browser Harness 0.1.8 | 浏览器工作流 harness、失败后可诊断状态 | browser_v3 继续作为可替换执行器；核心只持久化引用和状态 | 引入 harness 边界，不把浏览器库耦合进领域层 |
| [dbos-inc/dbos-transact-golang](https://github.com/dbos-inc/dbos-transact-golang) | 2026-07-30 限制 nested steps，7/29 修正 final result 状态 | durable step、幂等、完成状态一次写入 | 严格状态转换、幂等键、事件序列、CAS version | 原生 SQLite/PG 实现同类模式，避免引入第二套运行时 |
| [hatchet-dev/hatchet](https://github.com/hatchet-dev/hatchet) | 2026-07-30 修复 heartbeat/action listener 网络错误日志 | heartbeat、worker 故障边界、退避 | lease heartbeat、bounded workers、retryable/permanent 分类、panic 边界 | 引入调度模式，保持现有部署简单 |
| [temporalio/sdk-go](https://github.com/temporalio/sdk-go) | 2026-07-28 发布 v1.47.0，含 heartbeat/cancellation 修正 | heartbeat identity、取消、worker stop | lease owner、取消态、协调器停止与恢复 | 引入语义与测试，不增加外部控制面 |

排除项：

- [go-rod/rod](https://github.com/go-rod/rod) 的最新实际推送为 2026-07-15，
  虽然仓库元数据在 7/30 更新，但不满足“7/25 之后仍有代码维护”的筛选条件。

## 差距对比与整改

| 对比能力 | 原项目差距 | 整改结果 |
|---|---|---|
| durable execution | 团队流程只停留在 DDL 草案 | 可恢复 workflow/event/lease/retry/CAS |
| explicit plan | 分支隐含在具体实现 | credential path、replacement method、resume state 显式 |
| browser evidence | 页面级截图有，但新流程无专属验收 | 4 个视口/主题专测 + 72 路由暗色矩阵 |
| heartbeat | 后台轮询无生命周期任务租约 | owner、expires、heartbeat、过期重领 |
| idempotency | 重复请求可能生成重复周期 | `Idempotency-Key` 唯一约束与复用 |
| cancellation | 缺少工作流级停止语义 | cancelled 状态、API 与协调器停止 |
| nested-step discipline | 远程步骤可能交叉重复 | 严格允许迁移表与单步 CAS |
| dependency isolation | 直接引入大型工作流平台会抬高部署成本 | 吸收成熟模式，保持原生单体、SQLite/PG 与现有安装器 |

原始 GitHub API 元数据与提交响应保存在：
`.run/backend-research/repos/`。
