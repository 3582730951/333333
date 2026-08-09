# 2026-08-09 Claude OAuth、Codex 五版本指纹与 CF 恢复审计

## 审计基线

- 项目分支：`cache-hit-optimization`
- 本地/远端最新提交：`b4863db70c24ddf113bc004448c91ad55b796cc9`
- 提交时间：`2026-08-09T21:44:04+08:00`
- 提交标题：`feat: sync Codex 0.144.5 and repair tool context failover`
- `example_zip`：`codex-pool-diagnostics-v3-diagjob_1a1fc96f2d4e498c90088cbbd54b25ad.zip`
- ZIP SHA-256：`33cc82c6085b7be71a45719f79a3c4ad188e3a787fee06a3d679092887fc2d6d`

GitHub 的最新稳定 Codex release 是 `0.147.0`；本次以官方稳定 tag 的源码逐版本比较，
没有拿 alpha/main 的未来字段倒灌到稳定客户端。官方入口：

- <https://github.com/openai/codex/releases>
- <https://github.com/openai/codex/blob/rust-v0.147.0/codex-rs/core/src/responses_metadata.rs>
- <https://github.com/openai/codex/blob/rust-v0.147.0/codex-rs/core/src/client.rs>
- <https://github.com/openai/codex/blob/rust-v0.147.0/codex-rs/codex-api/src/requests/headers.rs>
- <https://github.com/openai/codex/blob/rust-v0.147.0/codex-rs/login/src/auth/default_client.rs>

## Codex 五版本差异矩阵

| 下游版本 | 官方提交 | `prompt_cache_key` 默认来源 | `code_mode_tool_names` | `parent_turn_id` |
|---|---|---|---|---|
| 0.144.6 | `5d1fbf26c43abc65a203928b2e31561cb039e06d` | thread | 无 | 无 |
| 0.145.0 | `25af12f7e61572b0bc18ddb1008be543b91519b0` | session | 无 | 无 |
| 0.146.0 | `e363b08c9175ac1cbe5893615dd2cb9ddf95043b` | session | 仅完整 client metadata；直接 header 有界化 | 无 |
| 0.146.1 | `79b4f03d35962b005b007a015113b38930711665` | session | 同 0.146.0 | 无 |
| 0.147.0 | `be6e8eac029b183056b7e4402879f15d2c85f61b` | session | 同 0.146.x | 有 |

实现结果：

1. 请求入口只解析一次有效版本，优先级为“显式模型探测覆盖 > 被支持且 UA/version 一致的下游版本 > 账号/配置默认值”。
2. 同一个冻结版本同时驱动 `User-Agent`、`version`、HTTP/WS `client_metadata` 和 cache key；热更新不会让一条请求出现混合版本。
3. 仅 UUID 形态的官方自动 cache key 会按版本重建；用户自定义 key 原样保留。
4. `0.146+` 的工具名映射只留在完整 metadata，直接兼容 header 不携带无界 map。
5. `0.147` 的 parent turn 只发送账号虚拟化 UUID，原始下游 parent id 不上游透传。
6. 官方 Rust 客户端没有浏览器 `sec-ch-*` / `sec-fetch-*` / `accept-language` 组合；Codex OAuth、API key 与 custom Codex CLI sidecar 均关闭默认浏览器应用头注入，只保留所选 TLS/HTTP2 engine。

## Claude OAuth 粘贴问题

前后端现在接受三种内容：完整 callback URL、单独 authentication code、Claude 成功页整段
`Authentication code / Paste this into Claude Code` 文本。UI 改为多行粘贴框并明确说明；后端从
整段文本提取 code 后继续原有 PKCE/state 校验。

## example_zip 结论

导出与当前远端 commit 一致，但 manifest 标记 `+dirty`。关键统计如下：

- `codex_upstream_attempts.csv` 共 20,000 行：`status=200` 9,955，状态转换 `status=0` 10,036，`400` 4，`503` 5。
- 状态分布：`terminal_success` 4,956，`attempted` 5,006，`transport_attempted` 5,006，`response_headers` 5,004，WS probe/transport error 11，HTTPS fallback context recovery 11。
- 631 条 route attempt 最终下游状态全部为 200；629 条无 terminal error，2 条 server error 被后续路径恢复。
- cache capability：`hit_observed` 18,444、`reported` 428、`unreported` 1,128；16,596 条保留下游 cache key，7 条使用官方稳定前缀；compatibility loss 非空记录为 0。
- `cf_events.csv` 为 0 行，因此该 ZIP 不能证明真实 CF 通过率，只能用于验证“无 CF 误触发记录”。
- 最近有 2 条 `admin.oauth.complete` 503；新的 Claude 粘贴解析与 UI 正好覆盖用户报告的完成阶段输入问题。
- 12 条 `refresh_token_invalidated` 隔离是上游明确返回 session ended，和指纹/代理改写无关。

## CF 求解器升级

对齐 FlareSolverr `v3.5.0`（官方 release 含 Turnstile 修复）：

- 优先完整 session 生命周期；代理认证在 `sessions.create` 绑定，request 不重复/泄漏代理字段。
- `socks5h` 规范化为 Chromium/FlareSolverr 接受的 `socks5`。
- `maxTimeout=90000`、解题后等待 2 秒、保留媒体加载；第一次仍为 challenge 时换全新浏览器重试一次。
- 校验最终 HTTP 状态、host、challenge marker、UA、cookie domain/path/expiry 和 `cf_clearance`。
- session 命令不支持时兼容旧 stateless API；带认证的代理不降级到无法承载认证的 stateless 路径。
- 浏览器解题结果先进入临时 jar，再以真实 Codex/Claude 应用头、同一 egress 请求模型端点；Codex 会逐一验证五个兼容版本的 UA/version。只有全部应用重放不再命中 CF 才提升为正式 cookie 并清 cooldown，否则继续 WARP 重注册换 IP。
- 修复 Claude direct/proxy jar 错用 Codex host namespace 的问题。

官方依据：

- <https://github.com/FlareSolverr/FlareSolverr/releases/tag/v3.5.0>
- <https://github.com/FlareSolverr/FlareSolverr/blob/v3.5.0/README.md#-sessionscreate>
- <https://github.com/FlareSolverr/FlareSolverr/blob/v3.5.0/README.md#-requestget>

FlareSolverr 明确要求复用 clearance 时同时复用 solver UA；因此本项目不把浏览器 UA 写进
Codex/Claude 应用请求，而用应用形态重放作为 promotion gate，避免出现“Mozilla UA + Codex/Claude
协议头”的混合指纹。
