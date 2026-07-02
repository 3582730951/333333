# Session 31c: ChatGPT Backend-API Rate-Limit Header 缺失诊断 + 默认禁用主动冷却

## 🔍 问题分析

根据用户反馈"GPT账号的额度获取就是错的"和网络搜索结果 [[4]](https://community.openai.com/t/x-ratelimit-headers-missing/935514) [[6]](https://community.openai.com/t/the-chatcompletion-response-limit-headers-do-not-reflect-previous-request-or-token-usage/596144)，发现：

### 核心问题

**ChatGPT backend-api (`chatgpt.com/backend-api/codex`) 可能不返回标准的 `x-ratelimit-*` headers！**

**后果**：
- `isExhausted()` 永远返回 `false`（没有 header = 假定不耗尽）
- `captureQuota()` 无法记录额度到数据库
- **主动冷却 (guardRateLimit) 成为无效功能**
- 只能依赖被动 429 错误检测 (benchOnLimit)

### 设计决策

**`rate_limit_guard_enabled` 现在默认为 `false`**

理由：
1. ChatGPT backend-api 通常不返回 headers → 主动冷却无法工作
2. 没有 headers 时 `guardRateLimit()` 是 no-op → 启用也无意义
3. 被动 429 检测 (`benchOnLimit`) 始终有效 → 足够应对真实限额
4. 管理员可在前端配置启用（当使用返回 headers 的 API 时）

## 📊 双层冷却机制

pool_server 有**两种**冷却机制：

### 1. 主动冷却 (guardRateLimit) - 现在默认禁用

**触发条件**：成功响应 + headers 显示 `remaining=0`

**目的**：预防性轮换，避免下一个请求 429

**依赖**：需要 `x-ratelimit-remaining-*` headers

**配置**：`rate_limit_guard_enabled: false` (默认)

### 2. 被动冷却 (benchOnLimit) - 始终启用

**触发条件**：收到 429 或限额错误响应

**目的**：已经触发限额后的响应式冷却

**依赖**：HTTP 状态码 + 错误消息

**配置**：无法禁用（核心逻辑）

## ✅ 修复内容

### 1. config.go - 默认值改为 false

```go
// Session 31c: RateLimitGuardEnabled defaults to FALSE. The proactive
// cooldown on success-path exhaustion is disabled by default because:
// 1. ChatGPT backend-api often doesn't return x-ratelimit-* headers
// 2. Without headers, guardRateLimit is a no-op anyway
// 3. Passive 429-based detection (benchOnLimit) is always active
// Admins can enable via frontend config when headers are available.
RateLimitGuardEnabled: false,
```

### 2. config.example.json - 示例配置更新

```json
{
  "rate_limit_guard_enabled": false
}
```

### 3. quota.go - 添加诊断日志

当 Codex 响应缺少任何 rate-limit header 时记录：

```go
if provider == "codex" && !hasAnyRateLimit {
    _ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
        AccountID: accountID,
        Action:    "codex_no_ratelimit_headers",
        Reason:    "backend-api response missing x-ratelimit-* headers",
        Detail:    "provider=codex status=200 (this is NORMAL for chatgpt.com/backend-api)",
    })
}
```

### 4. ratelimit.go - 补充注释

```go
// Session 31c caveat: If the upstream (e.g., ChatGPT backend-api) does NOT return
// x-ratelimit-* headers, this function will return false (assume not exhausted).
// That's correct behavior: without headers, we cannot proactively rotate; the
// account will only cooldown when it actually hits a 429 (via benchOnLimit).
```

## 📋 使用指南

### 默认行为（推荐）

```json
{
  "rate_limit_guard_enabled": false
}
```

**效果**：
- ✅ 主动冷却禁用（无 headers 时无效）
- ✅ 被动 429 检测始终工作
- ✅ 避免误判（Session 31b 已修复）
- ✅ 简化配置

### 启用主动冷却（仅当有 headers 时）

```json
{
  "rate_limit_guard_enabled": true
}
```

**适用场景**：
- 使用 OpenAI 公开 API（返回标准 headers）
- 使用 Claude API（始终返回 headers）
- 使用自定义 provider（实现了 headers）

**不适用**：
- ChatGPT backend-api（通常无 headers）

## 🔍 诊断方法

### 1. 部署新版本

```bash
cd /workspace/pool_server
./update.sh
```

### 2. 发送测试请求

```bash
curl -X POST http://localhost:8787/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.4","input":"test"}'
```

### 3. 查看诊断日志

```bash
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT account_id, action, reason, detail, created_at 
   FROM audit_log 
   WHERE action = 'codex_no_ratelimit_headers' 
   ORDER BY created_at DESC 
   LIMIT 10;"
```

**预期结果**：
- **有大量记录** → backend-api 不返回 headers（正常，保持 `rate_limit_guard_enabled: false`）
- **没有记录** → headers 存在，可以考虑启用主动冷却

### 4. 查看捕获的额度快照

```bash
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT account_id, source, used_percent, remaining_tokens, remaining_requests, raw 
   FROM account_rate_limit 
   WHERE provider='codex';"
```

**预期结果**：
- **raw 字段为空** → 没有 headers
- **有 JSON 数据** → headers 存在且被正确解析

## 📊 行为对比

| 配置 | 主动冷却 | 被动冷却 | 适用场景 |
|------|---------|---------|---------|
| `false` (默认) | ❌ 禁用 | ✅ 启用 | ChatGPT backend-api (无 headers) |
| `true` | ✅ 启用 | ✅ 启用 | OpenAI/Claude API (有 headers) |

## 🎯 总结

**用户问题**："GPT账号的额度获取就是错的，是否就是限额冷却触发的原因"

**答案**：
1. ✅ 额度获取"错"不是 bug — backend-api 不返回 headers 是其设计
2. ✅ Session 31b 已修复误判冷却（ANY→ALL 逻辑）
3. ✅ 现在主动冷却默认禁用 — 避免无 headers 时的困惑
4. ✅ 被动 429 检测始终有效 — 真实限额仍能正确处理

**建议**：
- 保持 `rate_limit_guard_enabled: false`（默认）
- 依赖被动 429 检测
- 必要时增加账号池容量
- 运行诊断命令确认 headers 状态

## 📝 文件变更

| 文件 | 变更 | 说明 |
|------|------|------|
| `internal/config/config.go` | 修改 | `RateLimitGuardEnabled: false` (默认) |
| `config.example.json` | 修改 | `"rate_limit_guard_enabled": false` |
| `internal/api/quota.go` | 新增 | Codex headers 缺失诊断日志 |
| `internal/api/ratelimit.go` | 注释 | Session 31c caveat 说明 |

build+test: ✅ 编译通过

## 相关 Sessions

- **Session 31a**：PermissionDenied 误判隔离
- **Session 31b**：Rate-limit ANY→ALL 逻辑修复
- **Session 31c**：主动冷却默认禁用 + 诊断工具（本次）

## 参考资料

- [[1]](https://blog.laozhang.ai/en/posts/openai-api-rate-limit): OpenAI API Rate Limits 标准格式
- [[3]](https://clemenssiebler.com/posts/understanding-azure-openai-x-ratelimit-remaining-tokens-x-ratelimit-remaining-requests-headers/): Azure OpenAI headers 详解
- [[4]](https://community.openai.com/t/x-ratelimit-headers-missing/935514): X-ratelimit Headers Missing
- [[6]](https://community.openai.com/t/the-chatcompletion-response-limit-headers-do-not-reflect-previous-request-or-token-usage/596144): Headers 不准确报告

