# Session 31 部署和初始化指南

## 🎯 当前状态

根据诊断结果：
- ✅ pool-server 正在运行
- ✅ 数据库文件存在（416K）
- ❌ 数据库为空（0 个账号）
- ❌ Session 31 修复未部署

---

## 🚀 完整部署步骤

### 步骤 1：编译新版本（包含 Session 31 修复）

```bash
cd /workspace/pool_server

# 编译
go build -v ./cmd/pool-server

# 检查编译结果
ls -lh pool-server
```

### 步骤 2：零停机部署

```bash
# 使用 update.sh 脚本部署
./update.sh

# 或手动部署
sudo systemctl stop pool-server
sudo cp pool-server /usr/local/bin/pool-server
sudo systemctl start pool-server

# 检查状态
sudo systemctl status pool-server
```

### 步骤 3：初始化账号

**通过前端添加账号**：

1. 访问 `http://localhost:8787/admin/`
2. 点击 "Accounts" → "Add Account"
3. 添加 Claude 或 ChatGPT 账号

**或通过 API**：

```bash
# 添加 Claude 账号
curl -X POST http://localhost:8787/admin/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "claude",
    "email": "your-email@example.com",
    "label": "Claude Account 1",
    "token": "your-session-key"
  }'

# 添加 ChatGPT 账号
curl -X POST http://localhost:8787/admin/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "codex",
    "email": "your-email@example.com",
    "label": "GPT Account 1",
    "token": "your-access-token"
  }'
```

### 步骤 4：验证部署

```bash
# 运行完整诊断
./scripts/diagnose-all.sh

# 应该看到：
# ✅ Session 31a: 已部署
# ✅ Session 31c: 已部署
# ✅ 有活跃账号
# ✅ 账号已绑定出口
```

### 步骤 5：测试统计功能

```bash
# 发送测试请求
curl -X POST http://localhost:8787/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-api-key" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
    "max_tokens": 100,
    "messages": [{"role": "user", "content": "Hello"}]
  }'

# 检查 usage 统计
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM usage_records;"

# 应该 > 0
```

---

## 🔧 针对"走某个出口没有统计"的诊断

### 问题根源

**账号必须绑定到出口才能统计 usage**

`account_egress_bindings` 表为空 → usage 统计失效

### 自动修复

重启 pool-server 会自动为所有账号创建绑定：

```bash
sudo systemctl restart pool-server

# 等待 5 秒
sleep 5

# 验证绑定
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM account_egress_bindings;"
# 应该 = 账号数量
```

### 手动检查绑定

```bash
# 查看账号与出口的绑定关系
sqlite3 /var/lib/codex-pool/pool.sqlite3 <<EOF
.mode column
.headers on
SELECT
    a.id,
    a.label,
    a.provider,
    b.primary_egress_id,
    CASE
        WHEN b.primary_egress_id IS NULL THEN '❌ 未绑定'
        ELSE '✅ 已绑定'
    END as binding_status
FROM accounts a
LEFT JOIN account_egress_bindings b ON a.id = b.account_id
WHERE a.status = 'active';
EOF
```

### 出口类型说明

| 出口 ID | 类型 | 用途 | 统计支持 |
|---------|------|------|---------|
| `egress_direct` | direct | 直连上游 | ✅ 支持 |
| `egress_sidecar` | curl_cffi_sidecar | 通过 sidecar | ✅ 支持 |

**两种出口都支持 usage 统计！**

---

## 📊 验证统计是否正常

### 1. 检查统计配置

```bash
# 查看代码中的 recordUsage 调用
grep -rn "recordUsage" /workspace/pool_server/internal/api/*.go | wc -l
# 应该有多个调用点
```

### 2. 检查流式统计

```bash
# 流式请求的统计需要 StreamScanner
grep -rn "StreamScanner" /workspace/pool_server/internal/api/*.go
# 应该看到 Claude、Codex、Custom 路径都有
```

### 3. 实时监控统计

```bash
# 监控 usage_records 表
watch -n 2 'sqlite3 /var/lib/codex-pool/pool.sqlite3 "SELECT COUNT(*) as total, datetime(MAX(created_at), '\''unixepoch'\'') as last_record FROM usage_records;"'

# 发送请求后应该看到 total 增加
```

### 4. 查看最近的统计记录

```bash
sqlite3 /var/lib/codex-pool/pool.sqlite3 <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    substr(account_id, 1, 25) AS account,
    model,
    prompt_tokens,
    completion_tokens,
    total_tokens
FROM usage_records
ORDER BY created_at DESC
LIMIT 10;
EOF
```

---

## 🎯 针对"限额冷却"的说明

### 正常的冷却来源

| 来源 | 触发条件 | 冷却时长 | 是否正常 |
|------|---------|---------|---------|
| PermissionDenied | 真实权限错误 | 5 分钟 | ✅ 正常 |
| Rate Limited | 真实限额错误 | 30-60 分钟 | ✅ 正常 |
| 主动冷却 | headers remaining=0 | 30 秒 | ⚠️ 默认禁用 |

### 查看冷却原因

```bash
# 查看最近的冷却事件
sqlite3 /var/lib/codex-pool/pool.sqlite3 <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    account_label,
    action,
    reason
FROM audit_log
WHERE action IN (
    'permission_denied_no_quarantine',
    'rate_limited',
    'auth_expired'
)
ORDER BY created_at DESC
LIMIT 10;
EOF
```

### 检查配置

```bash
# 确认主动冷却已禁用
grep rate_limit_guard /etc/codex-pool/config.json

# 应该是: "rate_limit_guard_enabled": false
```

---

## 📋 快速检查清单

```bash
# 1. 检查 pool-server 运行状态
sudo systemctl status pool-server

# 2. 检查账号数量
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM accounts WHERE status='active';"

# 3. 检查出口绑定
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM account_egress_bindings;"

# 4. 检查 usage 统计
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM usage_records;"

# 5. 检查 Session 31 部署
sqlite3 /var/lib/codex-pool/pool.sqlite3 \
  "SELECT COUNT(*) FROM audit_log WHERE action LIKE '%permission_denied%';"

# 6. 运行完整诊断
./scripts/diagnose-all.sh
```

---

## 🆘 常见问题

### Q1: "账号未绑定到出口"

**解决**：重启 pool-server
```bash
sudo systemctl restart pool-server
```

### Q2: "usage 统计为 0"

**原因**：
1. 账号未绑定出口 → 见 Q1
2. 从未有实际请求 → 发送测试请求
3. 统计逻辑被禁用 → 检查代码

**测试**：
```bash
curl -X POST http://localhost:8787/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: your-key" \
  -d '{"model":"claude-3-5-sonnet-20241022","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}'
```

### Q3: "看到限额冷却但不知道原因"

**诊断**：
```bash
./scripts/diagnose-cooldown.sh
```

### Q4: "Session 31 修复未部署"

**解决**：
```bash
cd /workspace/pool_server
go build ./cmd/pool-server
./update.sh
```

---

## 📚 相关文档

- `SESSION_31_SUMMARY.md` - Session 31 完整总结
- `COOLDOWN_DIAGNOSIS_GUIDE.md` - 冷却诊断指南
- `scripts/diagnose-all.sh` - 完整诊断脚本
- `scripts/diagnose-cooldown.sh` - 冷却诊断脚本
- `scripts/diagnose-usage.sh` - 统计诊断脚本

---

## 🎉 部署后预期效果

- ✅ 不再有误判隔离（PermissionDenied 只冷却 5 分钟）
- ✅ 不再有误判冷却（tokens=0 但 requests>0 不冷却）
- ✅ 主动冷却默认禁用（避免无 headers 时的困惑）
- ✅ Usage 统计正常工作（所有出口都支持）
- ✅ 审计日志记录所有事件（便于排查）
