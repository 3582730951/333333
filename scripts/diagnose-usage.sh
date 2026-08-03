#!/bin/bash
# 出口统计诊断工具

DB="/var/lib/codex-pool/pool.sqlite3"

echo "📊 出口使用统计诊断"
echo "===================="
echo ""

echo "## 1️⃣  所有出口配置"
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT id, name, type, endpoint, health, stream_capable
FROM egress_profiles
ORDER BY id;
EOF
echo ""

echo "## 2️⃣  账号与出口的绑定关系"
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    a.id AS account_id,
    a.label,
    a.provider,
    b.primary_egress_id,
    b.standby_egress_ids
FROM accounts a
LEFT JOIN account_egress_bindings b ON a.id = b.account_id
WHERE a.status = 'active'
LIMIT 10;
EOF
echo ""

echo "## 3️⃣  Usage 记录统计"
echo "总记录数:"
sqlite3 "$DB" "SELECT COUNT(*) FROM usage_records;"

echo ""
echo "最近 10 条 usage 记录:"
sqlite3 "$DB" <<EOF
.mode column
.headers on
SELECT
    datetime(created_at, 'unixepoch') AS time,
    account_id,
    model,
    input_tokens,
    output_tokens
FROM usage_records
ORDER BY created_at DESC
LIMIT 10;
EOF
echo ""

echo "## 4️⃣  检查代码中的 usage 记录逻辑"
echo ""
echo "查找 recordUsage 调用点..."
grep -rn "recordUsage\|RecordUsage" /workspace/pool_server/internal/api/*.go | grep -v test | head -10
echo ""

echo "## 5️⃣  检查是否是流式请求统计问题"
echo ""
echo "流式请求的 usage 统计需要 StreamScanner..."
grep -rn "StreamScanner\|usage.Stream" /workspace/pool_server/internal/api/*.go | head -5
echo ""

echo "## 💡 可能的原因："
echo ""
echo "1. **流式请求未统计** - Session 17b 修复了这个问题"
echo "2. **特定出口路径缺少 recordUsage 调用**"
echo "3. **Sidecar 出口的统计路径不同**"
echo "4. **非聊天请求（如健康检查）不统计**"
echo ""
