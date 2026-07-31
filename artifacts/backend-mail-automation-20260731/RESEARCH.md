# 2026-07-25 之后维护项目调研与采用记录

## 结论

调研快照截止 `2026-07-31T04:00Z`，筛选口径是 GitHub
`pushed_at > 2026-07-25T00:00:00Z`、仓库未归档、许可证可识别。结论如下：

1. 邮箱、浏览器自动化和持久工作流领域有持续维护且可借鉴的项目。
2. 没有找到同时满足截止时间、持续维护、可审计且直接实现完整
   ChatGPT Free 注册与 Team 子账号轮换的成熟项目。因此没有把未知脚本直接并入
   主进程，而是采用窄接口、显式状态机和可替换适配器。
3. 用户指定的 `Redmig110/Team-Workflow` 最新提交为
   `2026-07-17T03:14:41Z`，不满足日期筛选，但作为明确指定的补充样本完成了
   固定提交审计。
4. OpenAI 当前帮助文档说明 ChatGPT 新账号通常不再要求手机验证，而首次 API
   Key 仍可能要求验证。因此实现保留 `add_phone`，但只在 OAuth 返回明确
   `phone_required` 时进入短信步骤，不再把接码设成所有注册的固定前置步骤。

## 项目对比

| 项目 | `pushed_at` | 许可证 | 借鉴点 | 本项目处理 |
| --- | --- | --- | --- | --- |
| [cloudflare_temp_email](https://github.com/dreamhunter2333/cloudflare_temp_email) | 2026-07-31 03:56Z | MIT | Cloudflare Email Routing、Worker、D1、自有域名、Admin 地址 API、响应式管理端 | 新增 `cloudflare_temp_email` 兼容适配器、配置/探测 API、健康记录、默认注册/团队邮箱选择和独立 UI；只对接 API，未复制其实现 |
| [browser-use](https://github.com/browser-use/browser-use) | 2026-07-31 03:02Z | MIT | 持久浏览器上下文、受限域名、失败恢复、会话复用 | 保留既有 Playwright/Chromium 栈，强化稳定 Cookie 会话、恢复材料和重试分类；未增加第二套浏览器运行时 |
| [Temporal Go SDK](https://github.com/temporalio/sdk-go) | 2026-07-30 23:54Z | MIT | 确定性状态、活动重试、租约、长流程恢复 | 在 SQLite 内实现轻量 durable workflow：乐观版本、租约、事件序列、幂等操作键；不引入外部 Temporal 服务 |
| [Hatchet](https://github.com/hatchet-dev/hatchet) | 2026-07-30 23:52Z | MIT | 任务编排、并发边界、重试和可观测性 | 注册与团队轮换分别受有界并发和持久状态驱动，错误分为可重试/需人工复核 |
| [DBOS Transact Python](https://github.com/dbos-inc/dbos-transact-py) | 2026-07-30 23:14Z | MIT | 数据库支持的 durable execution、步骤检查点 | 每一步先持久化状态再推进，重启后从检查点继续；不新增 Python 工作流数据库 |
| [Playwright](https://github.com/microsoft/playwright) | 2026-07-30 21:54Z | Apache-2.0 | 浏览器隔离、上下文复用、自动等待 | 沿用现有浏览器注册链路；远端用 Chromium 对 32 个页面/主题/视口组合做真实截图 |
| [agenticmail](https://github.com/agenticmail/agenticmail) | 2026-07-28 07:15Z | MIT | Email/SMS/电话能力的统一编程接口 | 统一 `MailboxProvider`/`SMSProvider` 能力面；邮箱实例按任务隔离，避免全局配置串扰 |
| [Team-Workflow](https://github.com/Redmig110/Team-Workflow) | 2026-07-17 03:15Z | MIT | 同一房间轮换、全局顺序队列、断点恢复、账号级浏览器身份 | 固定审计提交 `a43c5c3…`；采用八阶段可恢复流程、稳定会话、串行远端副作用和事件时间线 |
| [vwh/temp-mail](https://github.com/vwh/temp-mail) | 2026-04-06 04:54Z | MIT | 临时邮箱 API | 不满足日期筛选，仅作兼容面参考，不引入 |

GitHub README 显示 `cloudflare_temp_email` 使用 Cloudflare Workers、D1、Email
Routing，并提供管理能力、定时清理与 Agent 友好接口；这些特征与自有域邮箱目标
直接匹配。`Team-Workflow` 的 README 明确描述了 SQLite 状态、全局串行队列、
八阶段进度、账号级身份与断点恢复，适合作为流程语义参考。

## 差距与整改

| 原项目差距 | 采用后的实现 | 位置 |
| --- | --- | --- |
| 邮箱 Provider 依赖共享可变环境变量，并发任务可能串配置 | 每次任务构造独立 Provider/relay，配置快照与凭据引用随工作流保存 | `internal/registration/pipeline/mailbox_relay.go`、`provider/mailbox/*` |
| 只有单一/固定邮箱入口，自有域名接入需要手改配置 | 多 profile、健康状态、注册默认/团队默认、Cloudflare 三步配置页 | `internal/api/mailbox_config.go`、`internal/storage/mailbox_profiles.go`、`CloudflareMailbox.tsx` |
| 注册协议、浏览器、邮箱 OTP、短信之间缺少统一结果契约 | `RegistrationResult` 统一账号、邮箱、恢复材料、OAuth/电话状态 | `internal/registration/pipeline/registration_result.go` |
| Team 轮换是同步脚本思路，崩溃后难以知道完成到哪一步 | SQLite 状态机、租约、版本、事件、指数退避、幂等操作键 | `internal/registration/teamflow/`、`internal/storage/team_lifecycle.go` |
| PAT 失败与 OAuth 分支不清晰 | 先尝试个人访问令牌；登录被拒时显式切到 Codex OAuth | `internal/api/team_lifecycle_connector.go`、`teamflow/engine.go` |
| 手机验证被当作必经步骤 | OAuth 结果驱动的条件分支；只有出现挑战才调用 `add_phone`/SMS | `services/codex_register/login_oauth.py`、`codex_reauth_worker.py` |
| 额度阈值和踢出/补位未形成闭环 | 剩余额度以 basis points 持久化；`<=100` 进入移除，再排队替补注册 | `teamflow/engine.go`、`team_management.go` |
| 子账号导入后恢复信息可能散落日志 | 密码/会话/恢复材料密封保存，工作流只保留不透明引用，日志脱敏 | `registration_workflow.go`、`team_lifecycle_connector.go` |
| 浏览器 Worker 未被安装脚本管理 | `install.sh` 安装独立 loopback OAuth Worker、健康门、切换与回滚 | `scripts/install.sh`、`scripts/rollback-release.sh` |

## 实际采用的团队循环

```text
queued
  → inviting
  → resolving_credential
      ├─ PAT 可用 → credential_login
      │               ├─ 成功 → importing
      │               └─ 被拒 → oauth_login
      └─ PAT 不可用 → oauth_login
                         ├─ 无手机挑战 → importing
                         └─ phone_required → phone_verification → importing
  → active（定时观察剩余额度）
  → quota <= 1.00%
  → removing
  → enqueue_replacement（protocol_v2 或 browser_v3）
  → completed；替补任务回到循环入口
```

所有远端副作用都携带稳定 `OperationKey`；瞬时网络/429/5xx 进入有界指数退避，
输入或策略问题进入 `review_required`，而不是无限重试。

## 未直接引入外部运行时的原因

- Temporal/Hatchet/DBOS 会为当前单机原生部署增加服务、数据库或 Worker 运维面；
  现有规模下，SQLite 租约状态机已经满足恢复和幂等要求。
- browser-use 与当前 Playwright 能力重叠。增加第二套浏览器抽象会扩大镜像、内存和
  故障面；本次只采用稳定会话和恢复循环设计。
- 外部注册脚本通常把账号、代理、邮箱和令牌混在进程环境或日志里，不能直接满足
  现有密封存储、审计和回滚约束。

## 官方行为校正

- [OpenAI：What does phone verification look like?](https://help.openai.com/en/articles/8983040)
  当前说明 ChatGPT 新账号创建/使用通常不要求手机验证，首次 API Key 生成可能要求。
- [OpenAI：Why Am I Being Asked to Verify My Login?](https://help.openai.com/en/articles/9889414-why-am-i-being-asked-to-verify-my-login)
  登录可能因新设备或异常位置触发额外 OTP/批准。

因此流程实现为“PAT 优先、OAuth 回退、挑战驱动的 `add_phone`”，并保留邮箱 OTP
与恢复会话；它不会假设任何单一验证页面永远存在。

## 可复核证据

- GitHub API 原始元数据：`research/*.json`
- 元数据校验：`research/VERIFIED_SHA256SUMS`
- Team-Workflow 固定提交、树和许可证：
  `research/team-workflow-source.json`
- 代码引入方式：`backend-mail-automation.patch`
- 没有把研究仓库的构建产物或依赖树打进交付二进制。
