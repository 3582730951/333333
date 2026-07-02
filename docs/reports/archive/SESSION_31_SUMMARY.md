# Session 31 完整修复总结

## 🎯 三重修复 + 诊断工具

---

## ✅ Session 31a: PermissionDenied 误判隔离

**问题**：模型输出提到 "api.responses.write" 等关键词 → 误判为权限错误 → 自动隔离账号 72 小时 → 级联失败导致整个账号池不可用

**修复**：
- 移除 `handlePermissionDeniedAccount()` 自动隔离调用
- 改为审计日志 (`permission_denied_no_quarantine`) + 5分钟冷却（仅对当前对话）
- 新增 `IsAccountLevel()` 方法区分账号级 vs 功能级错误

**文件**：
- `internal/api/isolate.go`
- `internal/ban/ban.go`
- `internal/api/server_test.go`
- `internal/ban/ban_test.go`

---

## ✅ Session 31b: Rate-limit 误判冷却

**问题**：`anyRemainingZero()` 使用 **ANY 逻辑** → 只要任意一个维度（tokens OR requests）为 0 就触发冷却 → "一发消息就限额冷却"

**高频误判场景**：
- `tokens=0, requests=100` → 旧逻辑冷却 30s，实际还能发 100 个小请求
- `unified=0, granular 正常` → Bootstrap 残留值触发误判

**修复**：
- `anyRemainingZero()` → `isExhausted()`
- **ANY → ALL 逻辑**：所有维度都为 0 才算耗尽
- Granular 优先于 unified（防止 bootstrap 误判）
- 新增 13 个测试用例覆盖所有场景

**文件**：
- `internal/api/ratelimit.go`
- `internal/api/ratelimit_test.go` (新增)

---

## ✅ Session 31c: 主动冷却默认禁用 + 诊断工具

**问题**：用户反馈"GPT账号的额度获取就是错的，是否就是限额冷却触发的原因"

**核心发现**：
- ChatGPT backend-api (`chatgpt.com/backend-api/codex`) **通常不返回 `x-ratelimit-*` headers**
- 根据 GitHub 社区多个报告，这是 backend-api 的设计特性
- 主动冷却 (`guardRateLimit`) 依赖 headers，无 headers 时是无效功能

**修复**：
- `rate_limit_guard_enabled` 默认改为 `false`（由管理员在前端配置启用）
- 添加 Codex headers 缺失诊断日志（`codex_no_ratelimit_headers` 审计事件）
- 补充注释说明无 headers 时依赖被动 429 检测是正确行为

**双层冷却机制**：

| 机制 | 触发条件 | 依赖 | 默认状态 |
|------|---------|------|---------|
| **主动冷却** (guardRateLimit) | 成功响应 + remaining=0 | x-ratelimit-* headers | **禁用** |
| **被动冷却** (benchOnLimit) | 429 错误 | HTTP 状态码 | **启用** |

**文件**：
- `internal/config/config.go`
- `config.example.json`
- `internal/api/quota.go`
- `internal/api/ratelimit.go`

---

## 📊 行为对比总览

| 场景 | 旧行为（≤Session 30） | 新行为（Session 31） |
|------|---------------------|-------------------|
| 模型输出提到 "api.responses.write" | ❌ 隔离 72h | ✅ 不触发（200不走检测） |
| 上游 401 + "missing scopes" | ❌ 隔离 72h | ✅ 审计 + 5分钟冷却 |
| `tokens=0, requests=100` | ❌ 冷却 30s | ✅ 不冷却 |
| `tokens=0, requests=0` | ✅ 冷却 | ✅ 冷却（正确） |
| `unified=0, granular 正常` | ❌ 冷却 | ✅ 不冷却（用 granular） |
| ChatGPT backend-api (无 headers) | ⚠️ 主动冷却启用但无效 | ✅ 主动冷却默认禁用 |

---

## ✅ 验证结果

```bash
✅ go test ./internal/ban/ -race
   PASS: TestClassify, TestIsAccountLevel

✅ go test ./internal/api/ -race -run "TestIsExhausted|TestExhausted|TestReset"
   PASS: 13 个场景全部通过
   - tokens=0 but requests=100 → NOT exhausted (Session 31b fix)
   - unified=0 but granular available → NOT exhausted (Session 31b fix)

✅ go test ./internal/api/ -race
   PASS: 所有核心测试通过 (32.225s)

✅ go build ./cmd/pool-server
   Binary size: 25M
```

---

## 🚀 部署指南

### 一键部署

```bash
cd /workspace/pool_server
./update.sh  # 零停机部署（systemd socket activation）
```

### 配置说明

**默认配置（推荐 - ChatGPT backend-api）**：
```json
{
  "rate_limit_guard_enabled": false
}
```

**启用主动冷却（仅当有 headers 时）**：
```json
{
  "rate_limit_guard_enabled": true
}
```

适用于：OpenAI 公开 API、Claude API、自定义 provider

### 诊断命令

**检查 Codex 是否缺少 headers**：
```bash
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT account_id, action, reason, created_at 
   FROM audit_log 
   WHERE action = 'codex_no_ratelimit_headers' 
   ORDER BY created_at DESC LIMIT 10;"
```

**查看捕获的额度快照**：
```bash
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT account_id, source, remaining_tokens, remaining_requests, raw 
   FROM account_rate_limit 
   WHERE provider='codex';"
```

### 监控指标

部署后监控：

1. **隔离账号数** ↓ 应显著下降
2. **冷却频率** ↓ 应显著下降
3. **503 错误率** → 应消失
4. **账号利用率** ↑ 应提升

---

## 📝 文档清单

- **SESSION_31_QUARANTINE_FALSE_POSITIVE_FIX.md** - PermissionDenied 完整分析
- **SESSION_31B_RATELIMIT_FIX.md** - Rate-limit 完整分析
- **SESSION_31C_CODEX_RATELIMIT_DIAGNOSIS.md** - Headers 诊断 + 配置指南
- **SESSION_31_SUMMARY.md** - 本总结文档

内存已更新：
- `pool-server-permission-denied-fix.md`
- `pool-server-ratelimit-exhausted-fix.md`
- `pool-server-codex-ratelimit-diagnosis.md`

---

## 🎯 根本原因总结

三个问题的共同模式：**过于激进的判断逻辑**

1. **PermissionDenied**：扫描整个 body → 无法区分真实错误 vs 模型输出
2. **Rate-limit**：ANY 逻辑（任意维度=0）→ 无法区分部分耗尽 vs 完全耗尽
3. **主动冷却**：默认启用但无 headers → 无效功能导致困惑

三个修复的共同策略：**更精确的判断 + 更合理的默认值**

1. **PermissionDenied**：不再自动隔离，只审计 + 短期冷却
2. **Rate-limit**：ALL 逻辑（所有维度=0）+ granular 优先
3. **主动冷却**：默认禁用，由管理员根据实际情况启用

---

## 🎉 影响

**正面影响**：
- ✅ 消除两大误判（隔离 + 冷却）
- ✅ 提升账号池可用性和利用率
- ✅ 更好的错误传递（透传 403）
- ✅ 更合理的默认配置（主动冷却按需启用）
- ✅ 提供诊断工具（快速定位问题）

**向后兼容性**：
- ✅ 配置文件可选（默认值已更新）
- ✅ 数据库无变化
- ✅ API 无变化（新增审计动作）
- ✅ 测试更新（预期行为变更）

**潜在风险**：
- ⚠️ 真实 scope 不足需手动发现（缓解：审计日志）
- ⚠️ 主动冷却禁用可能略微增加 429（缓解：被动检测始终有效）

---

## 📚 参考资料

**Sources**:
- [[1] OpenAI API Rate Limits](https://blog.laozhang.ai/en/posts/openai-api-rate-limit)
- [[3] Azure OpenAI Headers](https://clemenssiebler.com/posts/understanding-azure-openai-x-ratelimit-remaining-tokens-x-ratelimit-remaining-requests-headers/)
- [[4] X-ratelimit Headers Missing](https://community.openai.com/t/x-ratelimit-headers-missing/935514)
- [[6] Headers Don't Reflect Usage](https://community.openai.com/t/the-chatcompletion-response-limit-headers-do-not-reflect-previous-request-or-token-usage/596144)

---

**部署后使用诊断命令验证 Codex headers 状态，确认修复效果！** 🚀
