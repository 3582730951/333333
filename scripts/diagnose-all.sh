#!/bin/bash
# Session 31 完整诊断工具 - 检查所有常见问题

set -e

DB=${1:-/var/lib/codex-pool/pool.sqlite3}
NOW=$(date +%s)

echo "🔍 Pool Server 完整诊断报告"
echo "============================="
echo "时间: $(date)"
echo "数据库: $DB"
echo ""

# 1. 账号状态
echo "## 1️⃣  账号状态总览"
echo ""
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    COUNT(*) as total_accounts,
    SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) as active,
    SUM(CASE WHEN status = 'banned' THEN 1 ELSE 0 END) as banned,
    SUM(CASE WHEN quarantine_until > $NOW THEN 1 ELSE 0 END) as quarantined
FROM accounts;
EOF
echo ""

# 2. 冷却状态
echo "## 2️⃣  当前冷却中的账号"
echo ""
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    a.id,
    a.label,
    a.provider,
    datetime(b.cooldown_until, 'unixepoch') AS cooldown_expires,
    (b.cooldown_until - $NOW) AS seconds_remaining
FROM accounts a
JOIN account_egress_bindings b ON a.id = b.account_id
WHERE b.cooldown_until > $NOW
ORDER BY b.cooldown_until DESC;
EOF

COOLING_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM accounts a JOIN account_egress_bindings b ON a.id = b.account_id WHERE b.cooldown_until > $NOW;")
if [ "$COOLING_COUNT" = "0" ]; then
    echo "✅ 当前没有冷却中的账号"
fi
echo ""

# 3. 出口绑定状态
echo "## 3️⃣  账号出口绑定状态"
echo ""
BINDING_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM account_egress_bindings;")
ACCOUNT_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM accounts WHERE status='active';")
echo "活跃账号数: $ACCOUNT_COUNT"
echo "已绑定出口数: $BINDING_COUNT"
if [ "$BINDING_COUNT" = "0" ]; then
    echo "⚠️  警告: 没有账号绑定到出口！这会导致统计失效"
fi
echo ""

# 4. Usage 统计
echo "## 4️⃣  Usage 统计状态"
echo ""
USAGE_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM usage_records;")
echo "总 usage 记录数: $USAGE_COUNT"
if [ "$USAGE_COUNT" = "0" ]; then
    echo "⚠️  警告: 没有 usage 记录！"
    echo "   可能原因:"
    echo "   1. 从未有请求经过"
    echo "   2. 账号未绑定出口"
    echo "   3. 统计逻辑被禁用"
else
    echo ""
    echo "最近 5 条 usage 记录:"
    sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    substr(account_id, 1, 20) AS account,
    model,
    prompt_tokens,
    completion_tokens,
    total_tokens
FROM usage_records
ORDER BY created_at DESC
LIMIT 5;
EOF
fi
echo ""

# 5. 审计日志检查
echo "## 5️⃣  审计日志分析（最近 1 小时）"
echo ""
ONE_HOUR_AGO=$((NOW - 3600))
AUDIT_COUNT=$(sqlite3 "$DB" "SELECT COUNT(*) FROM audit_log WHERE created_at > $ONE_HOUR_AGO;")
echo "最近 1 小时审计事件数: $AUDIT_COUNT"

if [ "$AUDIT_COUNT" -gt 0 ]; then
    echo ""
    echo "事件分类统计:"
    sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    action,
    COUNT(*) as count
FROM audit_log
WHERE created_at > $ONE_HOUR_AGO
GROUP BY action
ORDER BY count DESC;
EOF
fi
echo ""

# 6. 配置检查
echo "## 6️⃣  关键配置检查"
echo ""

# 读取配置文件
CONFIG_FILE="/var/lib/codex-pool/config.json"
if [ -f "$CONFIG_FILE" ]; then
    RATE_LIMIT_GUARD=$(grep -o '"rate_limit_guard_enabled"[[:space:]]*:[[:space:]]*[^,}]*' "$CONFIG_FILE" | awk -F: '{print $2}' | tr -d ' ')
    echo "rate_limit_guard_enabled: ${RATE_LIMIT_GUARD:-未设置}"

    if [ "$RATE_LIMIT_GUARD" = "true" ]; then
        echo "⚠️  主动冷却已启用 - Session 31c 建议设为 false"
    elif [ "$RATE_LIMIT_GUARD" = "false" ]; then
        echo "✅ 主动冷却已禁用（推荐）"
    fi
else
    echo "ℹ️  配置文件不存在: $CONFIG_FILE"
fi
echo ""

# 7. Session 31 修复状态检测
echo "## 7️⃣  Session 31 修复部署状态"
echo ""

# 检查是否有 Session 31a 的审计动作
HAS_31A=$(sqlite3 "$DB" "SELECT COUNT(*) FROM audit_log WHERE action = 'permission_denied_no_quarantine' LIMIT 1;")
if [ "$HAS_31A" = "0" ]; then
    echo "❌ Session 31a: 未部署（缺少 permission_denied_no_quarantine 审计日志）"
else
    echo "✅ Session 31a: 已部署"
fi

# 检查是否有 Session 31c 的诊断日志
HAS_31C=$(sqlite3 "$DB" "SELECT COUNT(*) FROM audit_log WHERE action = 'codex_no_ratelimit_headers' LIMIT 1;")
if [ "$HAS_31C" = "0" ]; then
    echo "❌ Session 31c: 未部署（缺少 codex_no_ratelimit_headers 诊断）"
else
    echo "✅ Session 31c: 已部署"
fi

# 检查二进制编译时间
BINARY="/usr/local/bin/pool-server"
if [ -f "$BINARY" ]; then
    BINARY_TIME=$(stat -c %Y "$BINARY" 2>/dev/null || stat -f %m "$BINARY" 2>/dev/null)
    BINARY_DATE=$(date -d @$BINARY_TIME 2>/dev/null || date -r $BINARY_TIME 2>/dev/null)
    echo "二进制编译时间: $BINARY_DATE"
fi
echo ""

# 8. 问题诊断建议
echo "## 📋 诊断总结和建议"
echo ""

if [ "$COOLING_COUNT" -gt 0 ]; then
    echo "⚠️  当前有 $COOLING_COUNT 个账号在冷却中"
    echo "   → 运行: ./scripts/diagnose-cooldown.sh 查看详细原因"
    echo ""
fi

if [ "$BINDING_COUNT" = "0" ]; then
    echo "❌ 严重问题: 账号未绑定到出口"
    echo "   → 这会导致:"
    echo "     • Usage 统计失效"
    echo "     • 冷却机制失效"
    echo "     • 会话隔离失效"
    echo "   → 解决: 重启 pool-server 会自动创建绑定"
    echo ""
fi

if [ "$USAGE_COUNT" = "0" ]; then
    echo "⚠️  没有 usage 记录"
    echo "   → 可能原因:"
    echo "     • 从未有实际请求经过"
    echo "     • 账号未绑定出口（见上）"
    echo "   → 测试: curl -X POST http://localhost:8787/v1/messages ..."
    echo ""
fi

if [ "$HAS_31A" = "0" ] || [ "$HAS_31C" = "0" ]; then
    echo "❌ Session 31 修复未完全部署"
    echo "   → 需要部署新版本"
    echo "   → 步骤:"
    echo "     cd /workspace/pool_server"
    echo "     go build ./cmd/pool-server"
    echo "     ./update.sh"
    echo ""
fi

if [ "$RATE_LIMIT_GUARD" = "true" ]; then
    echo "⚠️  建议修改配置"
    echo "   → rate_limit_guard_enabled: false"
    echo "   → 原因: ChatGPT backend-api 通常无 headers，主动冷却无效"
    echo ""
fi

if [ "$COOLING_COUNT" = "0" ] && [ "$BINDING_COUNT" -gt 0 ] && [ "$USAGE_COUNT" -gt 0 ]; then
    echo "✅ 系统状态正常"
    echo ""
fi

echo "完整文档: docs/operations/cooldown-diagnosis-guide.md"
echo ""
