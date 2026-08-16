# DeepSeek 缓存与 Codex / Claude Code 接入

本文记录不牺牲模型质量、推理强度或上下文完整性的缓存策略。实现依据
[DeepSeek 思考模式](https://api-docs.deepseek.com/guides/thinking_mode)、
[上下文缓存](https://api-docs.deepseek.com/guides/kv_cache)、
[Coding Agents 接入](https://api-docs.deepseek.com/guides/coding_agents)和
[deepseek-harness](https://github.com/deepseek-ai/deepseek-harness)；核对的 harness
版本为 `47f943859bef60e4160492346772ded9b24f765a`（2026-08-13）。

## 不变量

- 不裁剪或概括客户端提供的历史，不降低模型、reasoning effort、最大上下文或输出预算。
- system、工具定义和既有 messages 保持顺序；新轮次只追加在尾部。
- 不向不认识缓存参数的供应商猜测性注入 `cache_control` 或 `prompt_cache_key`。
- 账号切换只能使用完整、自包含的已恢复上下文；不能为命中缓存把严格粘性会话迁走。
- 普通回答的旧 reasoning 不回放；带工具调用的 assistant 轮必须完整回放
  `reasoning_content`，并与该轮全部并行工具调用保持在同一个 assistant message 中。

## 官方 harness 中可直接复用的做法

`deepseek-harness` 没有通过缩短上下文或降低推理档位换取缓存。它采用以下三个可验证约束：

1. DeepSeek 请求序列化始终保持 system → 既有 messages 的顺序，工具数组按注册顺序输出；
   可选字段不存在时直接省略，不在不同轮次间交替发送 `null` 和缺省值。
2. 压缩请求完整重放当前会话的 system、tools 和待压缩历史，只把压缩指令作为最后一条
   user message 追加。这样压缩调用本身复用刚刚发送过的前缀，不会为 summarizer 重建另一套
   system prompt。
3. 官方真实 API E2E 使用“首轮工具调用 + 工具结果续轮 + 普通 follow-up”，并断言冷请求之后
   每次 `cacheReadTokens > 0`；同时验证工具结果仍出现在最终答案，避免以破坏上下文换取假命中。

Pool 对应地在任何协议桥接之前保存原始下游亲和键。Codex 的 `prompt_cache_key`、root/parent
thread，及 Claude Code 的 `X-Claude-Code-Session-Id` 都从原始请求提取，再绑定到同一上游
API Key；桥接后不重新按已变化的 JSON 前缀选账号。额度耗尽时仍只以完整、自包含历史切换
账号，缓存冷启动可以接受，丢上下文不可以接受。

## 各协议的安全策略

| 下游 / 上游 | 缓存策略 | 上下文保护 |
|---|---|---|
| Codex → 官方 Responses | 保留稳定 `prompt_cache_key` 和原始前缀；高吞吐时按稳定会话键分片，不逐请求随机化 | 原生 reasoning item、工具 item 和 compact 语义不降级 |
| Claude Code → Anthropic Messages | 保持 tools → system → messages 的稳定层级；只使用协议支持的 cache control | thinking/signature、tool_use/tool_result 原样配对 |
| Codex → DeepSeek Chat | DeepSeek 缓存自动生效，不添加缓存专用参数；Responses reasoning 使用带版本的 opaque carrier 往返 | 工具轮完整恢复 `reasoning_content`，tool-call `content` 强制为非 null 空字符串 |
| Claude Code → DeepSeek 官方 API | `/v1/messages` 自动走 `https://api.deepseek.com/anthropic` 原生 Messages 路径 | 避免经 Chat 中间格式损失 thinking、typed block 或未来字段 |
| 其他 Chat/Responses 供应商 | 仅使用其声明的协议和路由 | 不套用 DeepSeek 专属字段；不为缓存改变请求语义 |

DeepSeek 的上下文缓存由服务端自动识别相同前缀，并通过
`prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` 报告结果。没有需要客户端设置的
缓存 key。稳定的 system、tools、历史顺序和只追加新消息，才是可控的命中条件。

## DeepSeek V4 工具轮规则

官方 API 的思考模式默认启用，支持 `reasoning_effort=high|max`；兼容输入中的
`low/medium/xhigh` 会分别映射为 `high/high/max`。Pool 额外把 Codex 的非标准
`ultra` 收敛到供应商最高的 `max`，不会降低为较弱档位。

截至 2026-08-15，官方更新日志已说明旧别名 `deepseek-chat` / `deepseek-reasoner`
在 2026-07-24 后停止提供；新配置应使用 `deepseek-v4-pro` / `deepseek-v4-flash`，
不要用旧别名的短期路由结果评估 V4 缓存能力。

V4 思考模式不支持 `tool_choice`。Pool 仅在精确的官方 `api.deepseek.com` Chat
端点删除语义等价的 `tool_choice:auto`；`required` 或指定函数等显式选择会返回可诊断的
400，而不是静默改变客户端意图。第三方代理和非 V4 模型不应用这条规则。

官方要求工具轮后续请求同时满足：

1. 完整回传该 assistant 工具轮的 `reasoning_content`；
2. assistant `content` 不能是 null，纯工具轮使用 `""`；
3. 并行 tool calls 仍属于同一 assistant 轮；
4. 普通、无工具的旧 reasoning 可省略，避免无意义地扩大和碎片化缓存前缀。

流式响应同样适用。Pool 会先累计完整 reasoning，再为 Codex 生成 Responses reasoning
item，或为 Claude Code 生成 thinking + signature；下一轮转换回 Chat 时恢复原文。

## 配置官方 DeepSeek API Key

在管理端“供应商”创建自定义供应商，推荐字段如下：

```json
{
  "id": "deepseek-v4",
  "name": "DeepSeek V4",
  "base_url": "https://api.deepseek.com",
  "upstream_protocol": "chat_completions",
  "transport_profile": "generic",
  "enabled": true,
  "auto_discover_models": true,
  "models": [
    "deepseek-v4-pro",
    "deepseek-v4-flash",
    "deepseek-v4-pro[1m]"
  ]
}
```

随后通过管理 UI 导入 API Key，或调用：

```bash
curl -sS http://127.0.0.1:8787/admin/accounts/import-key \
  -H 'content-type: application/json' \
  -d '{"provider_id":"deepseek-v4","api_key":"<DEEPSEEK_API_KEY>","label":"DeepSeek V4","group_name":"cyber"}'
```

不要把真实 key 写入仓库、诊断包或测试 fixture。

### Codex 使用 DeepSeek

Codex 仍以 `wire_api = "responses"` 连接 Pool，并选择 `deepseek-v4-pro` 或
`deepseek-v4-flash`。DeepSeek 没有原生 Responses 端点，因此 Pool 只在这一条路径执行
Responses ↔ Chat 桥接，并保留 reasoning/tool round-trip。官方 Codex hosted tools 没有
Chat 等价物时仍会通过现有 compatibility-loss 机制明确报告；客户端 function、custom 和
client tool-search 保持可用。

### Claude Code 使用 DeepSeek

Claude Code 连接 Pool 的 `/v1/messages`，模型选择 `deepseek-v4-pro[1m]`，effort 选择
`max`。当供应商 Base URL 精确为 `https://api.deepseek.com` 或其 `/v1` 兼容形式、且操作员
没有配置显式 messages route 时，Pool 自动改用官方 `/anthropic/messages`。显式 route
始终优先，代理域名不会被重写。

主 CLI 与子 agent 即使选择 Pro/Flash 不同模型，也应使用同一个下游 key 和客户端原生
session/thread 标识。Pool 的粘性键会将同一会话族绑定到同一上游 API Key；独立新会话仍可
在压力下公平选择账号。不要为子 agent 生成逐请求随机 cache key。

## 验证与诊断

- 观察 usage 中 `prompt_cache_hit_tokens`、`prompt_cache_miss_tokens` 和归一化后的
  `cache_read_input_tokens`；不要只按“请求是否出现过命中”判断。
- 真实缓存验收应采用一次冷请求、一次工具结果续轮和一次普通 follow-up；冷请求之后每次都应
  有正的 cache-read token。缓存是 best-effort，单次未命中不能作为上下文丢失证据。
- 400 且发生在工具续轮时，优先检查 `reasoning_content` 是否完整、tool-call content 是否
  非 null、显式 `tool_choice` 是否被客户端强制设置。
- 上下文稳定性验收同时计算首尾 marker/hash，并验证 reasoning → tool call → tool result 的
  顺序；缓存率高不能替代内容完整性检查。
- 诊断包必须保留 usage、路由/账号切换、兼容性损失和上下文恢复事件，但继续擦除 API Key、
  Authorization、Cookie 和模型 reasoning 正文。
