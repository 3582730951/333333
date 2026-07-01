#!/bin/bash
# Session 31 限额冷却诊断工具
# 用于分析账号进入"限额冷却"状态的具体原因

set -e

DB=${1:-/var/lib/codex-pool/pool.sqlite3}

if [ ! -f "$DB" ]; then
    echo "❌ 数据库文件不存在: $DB"
    echo "用法: $0 [数据库路径]"
    exit 1
fi

echo "📊 Session 31 限额冷却诊断报告"
echo "================================"
echo ""

# 1. 当前冷却中的账号
echo "## 1️⃣  当前冷却中的账号"
echo ""
NOW=$(date +%s)
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    a.id AS account_id,
    a.label,
    a.provider,
    datetime(b.cooldown_until, 'unixepoch') AS cooldown_until,
    (b.cooldown_until - $NOW) AS remaining_seconds
FROM accounts a
JOIN account_egress_bindings b ON a.id = b.account_id
WHERE b.cooldown_until > $NOW
ORDER BY b.cooldown_until DESC;
EOF
echo ""

# 2. 最近的冷却触发事件（审计日志）
echo "## 2️⃣  最近 10 次冷却触发事件"
echo ""
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    account_label,
    action,
    state,
    reason,
    substr(detail, 1, 80) AS detail_snippet
FROM audit_log
WHERE action IN (
    'permission_denied_no_quarantine',
    'rate_limited',
    'auth_expired',
    'ban_quarantine'
)
ORDER BY created_at DESC
LIMIT 10;
EOF
echo ""

# 3. 冷却触发原因统计（最近 1 小时）
echo "## 3️⃣  冷却触发原因统计（最近 1 小时）"
echo ""
ONE_HOUR_AGO=$((NOW - 3600))
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    action,
    state,
    COUNT(*) AS count
FROM audit_log
WHERE created_at > $ONE_HOUR_AGO
  AND action IN (
      'permission_denied_no_quarantine',
      'rate_limited',
      'auth_expired'
  )
GROUP BY action, state
ORDER BY count DESC;
EOF
echo ""

# 4. 检查是否是主动冷却（guardRateLimit）
echo "## 4️⃣  主动冷却 (guardRateLimit) 状态"
echo ""
sqlite3 "$DB" <<EOF
SELECT
    json_extract(value, '$.rate_limit_guard_enabled') AS enabled
FROM system_config
WHERE key = 'server_config'
LIMIT 1;
EOF | while read enabled; do
    if [ "$enabled" = "true" ]; then
        echo "⚠️  主动冷却已启用 (rate_limit_guard_enabled: true)"
        echo "   这可能导致误判！建议改为 false"
    elif [ "$enabled" = "false" ]; then
        echo "✅ 主动冷却已禁用 (rate_limit_guard_enabled: false)"
    else
        echo "ℹ️  未找到配置（使用默认值 false）"
    fi
done
echo ""

# 5. Codex headers 缺失诊断
echo "## 5️⃣  Codex Rate-Limit Headers 诊断"
echo ""
sqlite3 "$DB" <<EOF
SELECT COUNT(*)
FROM audit_log
WHERE action = 'codex_no_ratelimit_headers'
  AND created_at > $ONE_HOUR_AGO;
EOF | while read count; do
    if [ "$count" -gt 0 ]; then
        echo "⚠️  最近 1 小时有 $count 次 Codex 响应缺少 rate-limit headers"
        echo "   这是 ChatGPT backend-api 的正常现象"
        echo "   主动冷却在此情况下无效（但已默认禁用）"
    else
        echo "ℹ️  最近 1 小时未检测到 Codex headers 缺失"
    fi
done
echo ""

# 6. 被动冷却来源分析
echo "## 6️⃣  被动冷却来源分析（最近 10 次）"
echo ""
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    account_label,
    CASE
        WHEN action = 'permission_denied_no_quarantine' THEN 'PermissionDenied (5min)'
        WHEN state = 'rate_limited' THEN 'Rate Limited (30-60min)'
        WHEN state = 'auth_expired' THEN 'Auth Expired'
        ELSE action
    END AS cooldown_type,
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
echo ""

# 7. 总结建议
echo "## 📋 诊断总结"
echo ""
echo "### 冷却类型说明："
echo ""
echo "1. **PermissionDenied (5分钟冷却)**"
echo "   - 触发：上游返回 401/403 + 'missing scopes' 等"
echo "   - Session 31a 修复：不再隔离，只冷却 5 分钟"
echo "   - 建议：检查账号 OAuth scope 是否完整"
echo ""
echo "2. **Rate Limited (30-60分钟冷却)**"
echo "   - 触发：上游返回 429 或 'usage limit' 错误"
echo "   - 这是**被动检测**，始终有效"
echo "   - 建议：增加账号池容量或等待限额重置"
echo ""
echo "3. **主动冷却（guardRateLimit）**"
echo "   - 触发：成功响应但 headers 显示 remaining=0"
echo "   - Session 31c：默认禁用（ChatGPT backend-api 无 headers）"
echo "   - 建议：保持禁用状态"
echo ""
echo "### 下一步操作："
echo ""
echo "- 如果看到大量 PermissionDenied → 检查账号 scope 配置"
echo "- 如果看到大量 Rate Limited → 增加账号数量或监控使用频率"
echo "- 如果 rate_limit_guard_enabled=true → 改为 false（避免误判）"
echo ""
