# 限额冷却诊断指南

## ❓ 问题

**前端管理员页面显示账号进入"限额冷却"状态**

---

## 🔍 原因分析

### "限额冷却"有 3 种来源

| 类型 | 触发条件 | 冷却时长 | Session 31 状态 |
|------|---------|---------|----------------|
| **1. PermissionDenied** | 401/403 + "missing scopes" | 5 分钟 | ✅ 31a 修复（不再隔离） |
| **2. 被动 Rate Limit** | 429 或 "usage limit" 错误 | 30-60 分钟 | ✅ 始终有效 |
| **3. 主动 Rate Limit** | 成功响应 + headers remaining=0 | 30 秒 | ✅ 31c 默认禁用 |

### 前端显示逻辑

```javascript
// admin.js
const cd = a.egress_binding.cooldown_until;  // 会话级冷却
const q = a.quarantine_until;                 // 账号级隔离

if (q > now) {
    显示 "隔离"  ← Session 31a 修复后应该很少见
} else if (cd > now) {
    显示 "限额冷却"  ← 你看到的这个！
}
```

### 数据库字段

- **`accounts.quarantine_until`** - 账号隔离（Session 31a 移除自动隔离）
- **`account_egress_bindings.cooldown_until`** - 会话冷却（仍然有效）

---

## 🛠️ 诊断步骤

### 1. 运行诊断脚本

```bash
cd /workspace/pool_server
./scripts/diagnose-cooldown.sh
```

**输出示例**：
```
📊 Session 31 限额冷却诊断报告
================================

## 1️⃣  当前冷却中的账号
account_id              cooldown_until         remaining_seconds
acc_abc123             2026-06-12 12:45:00    280

## 2️⃣  最近 10 次冷却触发事件
time                    account_label    action                              state           reason
2026-06-12 12:40:00    Account-1        permission_denied_no_quarantine     permission_...  api.responses.write
2026-06-12 12:38:00    Account-2        rate_limited                        rate_limited    usage limit

## 3️⃣  冷却触发原因统计（最近 1 小时）
action                                  state           count
permission_denied_no_quarantine         permission_...  15    ← 高频！
rate_limited                           rate_limited     3

## 4️⃣  主动冷却 (guardRateLimit) 状态
✅ 主动冷却已禁用 (rate_limit_guard_enabled: false)

## 5️⃣  Codex Rate-Limit Headers 诊断
⚠️  最近 1 小时有 50 次 Codex 响应缺少 rate-limit headers
```

### 2. 根据诊断结果分析

#### 场景 A：大量 PermissionDenied（高频）

**症状**：
- `permission_denied_no_quarantine` 计数很高
- 冷却时长：5 分钟
- 原因：`api.responses.write` / `missing scopes`

**根本原因**：
- 账号 OAuth scope 不完整
- 或下游调用了需要特殊权限的 API

**解决方案**：
```bash
# 1. 检查账号 scope
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT id, email, provider FROM accounts WHERE id='acc_abc123';"

# 2. 重新授权账号（补全 scope）
# 通过 /admin/oauth/start 重新登录

# 3. 或者接受这是正常现象（某些 API 确实需要更高权限）
```

#### 场景 B：真实限额耗尽（中频）

**症状**：
- `rate_limited` 计数高
- 冷却时长：30-60 分钟
- 原因：`usage limit` / `quota exceeded`

**根本原因**：
- 账号真的达到了使用限额
- 请求频率过高

**解决方案**：
```bash
# 1. 增加账号池容量
# 通过前端 /admin/accounts 添加更多账号

# 2. 检查使用情况
curl http://localhost:8787/admin/usage

# 3. 等待限额重置（通常是小时级或天级）
```

#### 场景 C：主动冷却误判（Session 31c 已修复）

**症状**：
- `rate_limit_guard_enabled: true`
- 但 Codex 无 headers

**根本原因**：
- Session 31b 之前的误判逻辑
- 主动冷却在无 headers 时仍然启用

**解决方案**：
```bash
# 已在 Session 31c 修复：默认 false
# 如果配置是 true，改为 false：
vim /var/lib/codex-pool/config.json
# "rate_limit_guard_enabled": false

# 重启
systemctl restart pool-server
```

---

## ✅ Session 31 修复效果

### 修复前（≤Session 30）

```
用户请求 → 模型提到 "api.responses.write"
  ↓
误判为权限错误
  ↓
隔离账号 72 小时 ← 严重！
  ↓
前端显示 "隔离"
```

### 修复后（Session 31a）

```
用户请求 → 模型提到 "api.responses.write"
  ↓
仅审计日志记录
  ↓
不隔离 ← 修复！
  ↓
前端不显示任何状态
```

### 真实权限错误的处理

```
用户请求 → 上游返回 401 + "missing scopes"
  ↓
检测为真实的 PermissionDenied
  ↓
冷却 5 分钟（不隔离）← Session 31a
  ↓
前端显示 "限额冷却"
  ↓
5 分钟后自动恢复
```

### 真实限额耗尽的处理

```
用户请求 → 上游返回 429 / "usage limit"
  ↓
被动检测（benchOnLimit）
  ↓
冷却 30-60 分钟
  ↓
前端显示 "限额冷却"
  ↓
等待限额重置
```

---

## 🎯 判断是否正常

### ✅ 正常情况

- **偶尔看到限额冷却**（几个账号，几分钟）
- **原因是 PermissionDenied**（5分钟自动恢复）
- **或真实的 429 错误**（账号确实达到限额）

### ⚠️ 需要关注

- **大量账号同时冷却**
- **高频 PermissionDenied**（说明 scope 配置问题）
- **频繁 429 错误**（说明账号池容量不足）

### ❌ 异常情况（Session 31 应该已修复）

- **账号进入"隔离"状态**（现在应该很少见）
- **一发消息就冷却 30 秒**（Session 31b 已修复）
- **主动冷却启用但无 headers**（Session 31c 已修复）

---

## 📋 快速检查清单

```bash
# 1. 查看当前冷却账号
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT a.id, a.label, datetime(b.cooldown_until, 'unixepoch') 
   FROM accounts a 
   JOIN account_egress_bindings b ON a.id = b.account_id 
   WHERE b.cooldown_until > strftime('%s','now');"

# 2. 查看最近冷却原因
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT datetime(created_at, 'unixepoch'), account_label, action, reason 
   FROM audit_log 
   WHERE action IN ('permission_denied_no_quarantine', 'rate_limited') 
   ORDER BY created_at DESC LIMIT 10;"

# 3. 检查配置
grep rate_limit_guard /var/lib/codex-pool/config.json
# 应该是: "rate_limit_guard_enabled": false

# 4. 手动清除冷却（仅测试用）
curl -X POST http://localhost:8787/admin/accounts/{account_id}/clear-cooldown
```

---

## 💡 建议

### 短期

1. **运行诊断脚本**：`./scripts/diagnose-cooldown.sh`
2. **查看审计日志**：确定高频冷却原因
3. **如果是 PermissionDenied**：检查账号 scope 配置
4. **如果是 Rate Limited**：考虑增加账号

### 长期

1. **监控冷却频率**：设置告警阈值
2. **定期检查审计日志**：发现模式
3. **优化账号池容量**：根据实际使用情况调整
4. **保持配置正确**：`rate_limit_guard_enabled: false`

---

## 📚 相关文档

- `SESSION_31_SUMMARY.md` - Session 31 完整总结
- `SESSION_31A_QUARANTINE_FALSE_POSITIVE_FIX.md` - PermissionDenied 详解
- `SESSION_31B_RATELIMIT_FIX.md` - Rate-limit 逻辑修复
- `SESSION_31C_CODEX_RATELIMIT_DIAGNOSIS.md` - 主动冷却配置指南
