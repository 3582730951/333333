# Kiro CLI 2.15.2 协议兼容性核验

日期：2026-07-30  
范围：协议兼容、请求正确性、并发可靠性与账号稳定性  
原始凭证、账号标识、profile ARN、UUID、提示词、工作目录、工具名与自由文本均已脱敏

## 1. 结论

已依据签名 Linux x86_64 Kiro CLI 2.15.2 的实际请求，逐字段对齐三条业务操作：

- `GenerateAssistantResponse`
- `ListAvailableModels`
- `GetUsageLimits`

对齐项包括 method、官方 service plane、path、query、JSON body、`X-Amz-Target`、普通 `User-Agent`、`X-Amz-User-Agent` 的 operation feature、Content-Type、Accept、压缩声明、SDK request metadata、token type、opt-out 与消息 `origin`。

此前影响兼容性的实质问题有：

1. 把所有操作都发往旧 `q.<region>.amazonaws.com/<operation>` 形式，而 CLI 2.15.2 已把推理放在 `runtime.<region>.kiro.dev/`、模型/用量放在 `management.<region>.kiro.dev/`；
2. catalog 使用 GET、把分页 token 放进 query、body 为空；
3. usage 使用 GET，并发送 `origin=AI_EDITOR&resourceType=AGENTIC_REQUEST`；
4. 推理 current/history 用户消息使用 `AI_EDITOR`；
5. `X-Amz-User-Agent` 的 `m/F` 与 `m/F,C` operation feature，以及 `md/appVersion` 所在 header，与 CLI 字面值有差异。

当前实现已经逐项修正。协议对齐会消除由客户端自相矛盾、错误 service plane 和不存在的请求组合造成的兼容性异常。账号规则、订阅资格、区域、配额和上游处置仍由 Kiro 决定，协议一致性本身不构成零风险或永久可用保证。

## 2. 证据与完整性

交付目录：`artifacts/full-fix-20260730/kiro/`

| 文件 | SHA-256 |
|---|---|
| `capture-integrity.json` | `e215e7305e5c3ca737e98090fbf70ddce5e9eaa6f10e0ff7b2944b9caa9abd3f` |
| `kiro-cli-2.15.2-wire-sanitized.jsonl` | `863288023297572d8a54924154d11f58f0898e85d08557d9e0a09e0fa91381bf` |
| `kiro-cli-2.15.2-usage-sanitized.jsonl` | `65476b7f6e33f9daee4b8de2192e813aa4ea910b7ac0bae391c6eb45c47f4922` |
| `sanitization-verification.json` | `fce27e24eff9c5cc0b426b4985e5defee743c6cef42cd9b8ef9ef13aac57e233` |
| `source-sha256.txt` | `fdd004d0153afacad9722b7abbab7f97d2060e563340b43020bed245f079837f` |
| `static-wire-comparison.json` | `746ab85f8d8411081feb67a59a6a2059b30b94513d7e56d2a446a75016f7f328` |
| `SHA256SUMS` | `82d00458e0ae23d9b14d9f4f016007071fe302ca985e8d8a378d4bc10f22aafc` |

源 capture 哈希：

- 主 wire capture：`299877b87c07c0337ef85a5a4a0cb099d9b2524170f165c98a85e9daee40582d`
- `/usage` capture：`07077df8dca9d8efbe0f256dd6afd0ce3aac08fd6d0e43fdc3ac3f61fbc28058`

原始 capture 保留在隔离的远端回归目录，未复制进交付物。脱敏文件保留 routing/header/protocol enum、JSON 结构、正文总字节数与整段 body SHA-256；credential、profile、ID、自由文本和工具名被替换。`sanitization-verification.json` 对 12 条记录完成 JSON 解析、manifest 校验和 credential/identity 模式扫描，命中数均为 0，12 条 Authorization 值均为脱敏标记。

`static-wire-comparison.json` 从脱敏 capture 提取三条 operation 的逐字段记录，并对当前 source snapshot 执行 16 项静态一致性断言；16 项全部为 true。对应源文件哈希固定在 `source-sha256.txt`。

`/usage` 是完全离线的本地 TLS 拦截：CLI 使用仅供本地 capture 的假 key，所有 443 连接都重定向到本地 TLS server，没有向外部业务服务发送该请求。

## 3. CLI 实际请求矩阵

### 3.1 公共 header

CLI 2.15.2 业务请求的公共字段：

```text
Content-Type: application/x-amz-json-1.0
Accept: */*
Accept-Encoding: gzip
Amz-Sdk-Request: attempt=1; max=3
Amz-Sdk-Invocation-Id: <每次请求的新 UUID>
Authorization: Bearer <已脱敏>
TokenType: API_KEY
X-Amzn-Codewhisperer-Optout: false
```

management 普通 User-Agent：

```text
aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererruntime/0.1.17975 os/linux lang/rust/1.92.0 md/appVersion-2.15.2 app/AmazonQ-For-CLI
```

streaming 普通 User-Agent：

```text
aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.17975 os/linux lang/rust/1.92.0 md/appVersion-2.15.2 app/AmazonQ-For-CLI
```

`X-Amz-User-Agent` 与普通 User-Agent 故意不同：

- Generate/Usage：同一 SDK 基础串，后缀为 `m/F app/AmazonQ-For-CLI`
- Catalog：同一 SDK 基础串，后缀为 `m/F,C app/AmazonQ-For-CLI`
- `md/appVersion-2.15.2` 只在普通 User-Agent；不出现在 `X-Amz-User-Agent`

### 3.2 `GenerateAssistantResponse`

```text
POST https://runtime.us-east-1.kiro.dev/
X-Amz-Target: AmazonCodeWhispererStreamingService.GenerateAssistantResponse
```

capture body：

- UTF-8 字节：33,784
- SHA-256：`3b6a9beb5eda19a0a0345c8f2e8e306e08823194262d71963aa9a2147c77d31c`
- `conversationState.agentTaskType = "vibe"`
- `conversationState.chatTriggerType = "MANUAL"`
- `currentMessage.userInputMessage.modelId = "auto"`
- current 用户消息 `origin = "KIRO_CLI"`
- history 中每个用户消息 `origin = "KIRO_CLI"`
- history 结构为一个用户消息和一个 assistant 消息
- current message context 携带 14 个 tool specification

提示词、history 内容、工作目录、conversation/tool ID、工具说明和 schema 文本均在交付 capture 中脱敏，结构与字节证据保留。

### 3.3 `ListAvailableModels`

首屏请求：

```text
POST https://management.us-east-1.kiro.dev/?origin=KIRO_CLI
X-Amz-Target: AmazonCodeWhispererService.ListAvailableModels
Body: {"origin":"KIRO_CLI"}
```

body：

- UTF-8 字节：21
- SHA-256：`f56f84fa1f521ca5bf64ddbab8fa9c67be24d63eac78964d5b50b0c7484352ea`

分页规则：

- query 始终只放 `origin=KIRO_CLI`；
- 第一页 body 没有 `nextToken`；
- 后续页 body 为 `{"origin":"KIRO_CLI","nextToken":"<token>"}`；
- profile ARN 继续放在 `X-Amzn-Kiro-Profile-Arn` header，而不是 query；
- 只有全部页面解析成功后才原子替换 last-good catalog；
- 同 scope 的并发后台 catalog refresh 使用 singleflight 去重，这不是推理限速。

### 3.4 `GetUsageLimits`

```text
POST https://management.us-east-1.kiro.dev/?origin=KIRO_CLI&isEmailRequired=false
X-Amz-Target: AmazonCodeWhispererService.GetUsageLimits
Body: {"origin":"KIRO_CLI","isEmailRequired":false}
```

body：

- UTF-8 字节：45
- SHA-256：`99571cd39c127f3dc93687c34b0d2477b807f270a4478af493bdb38039058b5f`

这组字段来自 CLI 2.15.2 `/usage` 的离线 capture，取代旧的 GET + `AI_EDITOR` + `resourceType` 组合。

### 3.5 其他观测

- `GetProfile` 使用 management root、POST、body `{}`。
- CLI 还向旧 `q.<region>.amazonaws.com/` 发送 `SendTelemetryEvent`。网关不伪造、不复制下游内容去合成 CLI telemetry；业务推理、catalog 和 usage 使用各自真实 operation。制造不存在的遥测反而会增加协议矛盾。

## 4. 代码映射

### 4.1 service plane 与端点

`internal/kiro/endpoint.go:15-53,94-162`

- 空 endpoint 和旧官方 `https://q.<region>.amazonaws.com[/operation]` 作为兼容输入；
- generation 映射为 `https://runtime.<region>.kiro.dev/`；
- catalog/usage 映射为 `https://management.<region>.kiro.dev/`；
- 显式官方 runtime/management endpoint 也按 operation 归一；
- 管理员明确 allowlist 的非官方/私有 host 保留 authority/base path，并使用兼容 operation suffix；
- host、scheme、region 与 allowlist 在附加 bearer 前验证。

### 4.2 header 与 operation

`internal/kiro/auth.go:238-319`

- CLI 2.15.2、Rust 1.92.0、AWS SDK 1.3.15、service model 0.1.17975 作为一个一致版本组；
- `Headers` 生成公共字段和正确的普通/X-Amz User-Agent；
- `ApplyOperationHeaders` 同时设置精确 `X-Amz-Target` 与 operation-specific `m/F` 或 `m/F,C`；
- API key 使用 `TokenType: API_KEY`，social 使用 `EXTERNAL_IDP`，IDC 保持省略；
- 每个请求生成独立 invocation ID。

### 4.3 catalog、usage、generation 和消息 origin

- `internal/kiro/catalog.go:184-243`：POST、root、query origin、body origin/nextToken、完整分页、last-good 原子替换。
- `internal/kiro/auth.go:190-230`：Usage POST、精确 query/body/target。
- `internal/api/kiro_messages.go:534-600`：官方 runtime root、Generate target、POST。
- `internal/api/kiro_cache_singleflight.go:10-23`：推理侧 cache singleflight 为即时放行的 no-op，不串行化同前缀下游请求。
- `internal/kiro/converter.go:374,395,420,1056`：system/history/current 用户 envelope 全部使用 `KIRO_CLI`。
- 生产代码的 `internal/kiro` 与 Kiro API 路径中已没有 `AI_EDITOR`。
- `internal/api/config_fields.go:374-379`：管理端帮助文案列明 runtime/management/旧 q 兼容；Kiro 代理侧亲和等待固定为 0。

## 5. 稳定性策略：不降低下游体验

当前 Kiro 路径采用以下原则：

1. **不注入代理侧限速或人工等待**：推理请求不加 pacing、批处理延时或“防护 sleep”；`kiro_affinity_wait_millis` 兼容项固定为 0。
2. **不偷偷裁剪上下文**：上游报告 context overflow 时，保留原始 prompt、history、工具 schema 和输出预算，向下游返回结构化 context 信号，由 Claude Code 自己压缩并重交。
3. **不自动重放计费 POST**：成功但空输出、普通 API-key 401 等情形不盲目重放 `GenerateAssistantResponse`，避免双计费和瞬时请求突发。
4. **只合并后台发现探测**：catalog singleflight 只去重同 scope 的重复后台 discovery，不占用或延迟用户推理。
5. **尊重真实 quota 与 Retry-After**：上游 429/503 的可用信息保持给调度/下游，不用伪造成功、改写用量或绕过 provider 控制。
6. **保持 operation 语义真实**：推理、模型发现和 usage 各走其真实 host/target/body；不为“看起来像 CLI”而生成虚假 telemetry。

这些措施最大化减少由代理自身造成的异常请求组合、重复计费、突发重试和不可解释流量，同时保持下游工具调用与上下文体验。

## 6. API key 与模型上下文边界

Kiro 官方文档说明：

- API key 支持所有 non-interactive Kiro CLI 功能；
- headless 模式使用 `KIRO_API_KEY` 与 `chat --no-interactive`；
- 官方网络清单同时列出 US/EU 的 runtime 和 management service plane。

来源：

- [Kiro CLI authentication](https://kiro.dev/docs/cli/authentication/)
- [Kiro CLI headless mode](https://kiro.dev/docs/cli/headless/)
- [Kiro firewall/proxy endpoints](https://kiro.dev/docs/privacy-and-security/firewalls/)
- [Kiro CLI commands](https://kiro.dev/docs/cli/reference/cli-commands/)

上下文窗口必须区分两个合同：

- Kiro 官方模型页当前列出的 GPT-5.6 Sol/Terra/Luna 为 **272K**；
- Claude Opus 4.6/4.7/4.8/5 与 Sonnet 4.6/5 列为 **1M**；
- pool 对 Codex GPT-5.6 的 **372K** 下游合同是另一条路由能力，不应被宣称为 Kiro GPT 的上游能力。

来源：[Kiro models](https://kiro.dev/docs/models/)

实际调度应以账号级 live catalog 能力为准。这样既满足 1M Claude 测试，又不会把 372K Codex 配置错误地套到 Kiro GPT 请求上。

## 7. 测试覆盖

精确 wire/兼容回归包括：

- `TestOperationEndpointsMatchKiroCLIAndTranslateLegacyOfficialHost`
- `TestHeadersMatchOfficialKiroCLI2152WireProfile`
- `TestUsageLimitsMatchesOfficialKiroCLI2152WireRequest`
- `TestRefreshModelCatalogRequiresCompletePagination`
- `TestConvertAnthropicUsesNativeThinkingAndSystemHistory`
- `TestAutoKiroGPTLowCapacityBridgesChatAndResponses`

覆盖维度：

- 默认区域、EU 区域、旧 q 官方 endpoint、显式 runtime/management、allowlisted custom host/base path；
- Generate/List/Usage 的 method/path/query/target；
- 普通与 X-Amz User-Agent 字面值及 operation feature；
- catalog 第一页/第二页原始 JSON body；
- profile header、不把 nextToken 放 query；
- current/history origin；
- GPT bridge 的 Generate target 与 CLI origin；
- API-key/social/IDC token type。

静态核验结果：

- 相关 Go 文件 `gofmt` 后保持格式；
- `git diff --check` 通过；
- 生产 Kiro 路径搜索 `AI_EDITOR` 为 0；
- 脱敏证据 `SHA256SUMS` 全部通过。

远端 v5 header/operation 定向回归结果：

```text
PASS=5
EXIT=0
log_sha256=1d62008aa6080b6eab9c9bf42b792abab7a91e1fc2a85c8c6ae159cf891af0fb
```

通过项：

- `TestHeadersMatchOfficialKiroCLI2152WireProfile`
- `TestHeadersUseRuntimeServiceAndAuthSpecificTokenType`
- `TestUsageLimitsMatchesOfficialKiroCLI2152WireRequest`
- `TestRefreshModelCatalogRequiresCompletePagination`
- `TestOperationEndpointsMatchKiroCLIAndTranslateLegacyOfficialHost`

日志交付路径：`artifacts/full-fix-20260730/remote-tests/kiro-header-targeted-v5.log`

### 7.1 429 后固定 30 秒阻塞回归

独立回归曾稳定复现：账号 A 返回可切换的 429 后，账号 B 明明健康且已经声明同一模型能力，下游仍需等待约 30 秒才完成切换。12 秒处抓取的 goroutine stack 将响应内阻塞链固定为：

```text
server.go -> tryAutoConsumeCodexResetCredit
          -> pollOneCodexQuota
          -> upstream.DoRaw (quota/reset-credit endpoint dial)
```

这不是代理限速，也不是 Kiro catalog singleflight；请求在切换前同步探测原账号的 quota/reset-credit 辅助端点，而该端点耗尽了自身约 30 秒的 upstream timeout。

当前 `internal/api/server.go:2047-2056,3629-3644` 在尝试 reset-credit 前先确认是否已有不同的健康候选账号。对可移动、可 failover 的 429，只要账号 B 已可用，就跳过辅助探测并立即切换；只有没有健康替代账号时，才保留原地 reset-credit 恢复。这消除了无收益等待，不引入 pacing、限速、裁剪或降级。

远端 v7 重复三次的结果：

```text
TestCodexStatelessContinuationFailsOverOn429:
  0.18s / 0.14s / 0.16s
TestRefreshModelCatalogPartialFailurePreservesLastGood:
  0.03s / 0.03s / 0.04s
PASS
EXIT_STATUS=0
```

证据：

- 超时现场 stack：`artifacts/full-fix-20260730/remote-tests/v6-stateless429-timeout-stack.log`，SHA-256 `9d63ca78d8da6f4bcdbb2c7e54fbacd1280470eb03d2c80ab1c13fc38c54eb5a`
- 修复后 count=3 日志：`artifacts/full-fix-20260730/remote-tests/v7-two-regressions-count3.log`，SHA-256 `12e8816586ffe5ea1b17b1de8efc1f627d6511a0734987c731afd3ee3da85d34`
- reset-credit 保留路径定向日志：`artifacts/full-fix-20260730/remote-tests/v7-reset-credit-targeted.log`，SHA-256 `1c9a7226e92ec86016c3d67312157c1b9be999c39844b90a022e2cedcd52a7eb`

### 7.2 长上下文、工具 ID 与并发账号切换

同一远端环境将以下四项连续执行三轮：

- 8 路并发 GPT-5.6，每路 1 MiB 模型可见上下文，处于固定 372K context 合同内；
- 8 路并发 Claude Code，每路 1,000,000 个 token-like 单元并声明 1M beta；
- A→B 账号切换后完整恢复 Codex `custom_tool_call/custom_tool_call_output`；
- A→B 账号切换后完整恢复 Claude `tool_use/tool_result`，并同时验证 at-rest 压缩与 legacy migration 幂等。

三轮均未出现上下文 digest、工具 ID/顺序、跨下游串线或大整数参数错误。结果为 `internal/storage PASS (0.513s)`、`internal/api PASS (17.018s)`、`EXIT_STATUS=0`。

日志：`artifacts/full-fix-20260730/remote-tests/context-stress-v7-count3.log`，SHA-256 `7e99ef23bbd858b9207cc7850b493c4602ed1245b7e2f74bffbe1ac28f807f44`。

上述回归均在共享服务器的低优先级、限 CPU 回归目录执行；本地未运行 Go 构建或测试。

## 8. 运维判定

部署后用管理端的模型发现与 usage probe 验证：

1. management 请求到达目标 service plane；
2. generation 请求到达 runtime service plane；
3. 自定义 allowlisted 中转保留配置 host/base path；
4. catalog 完整分页后才更新；
5. quota/401/403/429/503 分类保持真实；
6. 下游高并发期间没有代理注入的固定等待，且同会话账号切换由现有上下文/工具 ID 连续性测试约束。

协议字段已经逐项与 capture 对齐；后续 Kiro CLI 升级时应重新 capture，并把版本组、service model、operation feature 和 endpoint 作为一个整体升级，避免拼接出真实 CLI 从未发送过的混合指纹。
