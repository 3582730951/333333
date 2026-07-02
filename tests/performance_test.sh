#!/bin/bash
# Performance Test - Concurrent Task Execution

set -e

echo "================================"
echo "性能测试 - 并发任务"
echo "================================"

API_BASE="http://localhost:8787"

# 测试配置
TARGET_COUNT=20
CONCURRENCY=5

echo ""
echo "📊 测试参数:"
echo "  - 目标数量: $TARGET_COUNT"
echo "  - 并发数: $CONCURRENCY"
echo ""

# 创建任务
echo "1️⃣ 创建测试任务..."
START_TIME=$(date +%s)

TASK_RESPONSE=$(curl -s -X POST "${API_BASE}/admin/lifecycle/tasks" \
  -H "Content-Type: application/json" \
  -d "{
    \"task_type\": \"register\",
    \"platform\": \"chatgpt\",
    \"target_count\": $TARGET_COUNT,
    \"group_name\": \"perf-test\",
    \"concurrency\": $CONCURRENCY
  }")

TASK_ID=$(echo $TASK_RESPONSE | grep -o '"task_id":"[^"]*"' | cut -d'"' -f4)

if [ -z "$TASK_ID" ]; then
    echo "❌ 创建任务失败"
    exit 1
fi

echo "✅ 任务创建成功: $TASK_ID"

# 监控执行
echo ""
echo "2️⃣ 监控任务执行..."

while true; do
    sleep 2
    TASK_INFO=$(curl -s "${API_BASE}/admin/lifecycle/tasks/${TASK_ID}")
    STATUS=$(echo $TASK_INFO | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
    COMPLETED=$(echo $TASK_INFO | grep -o '"completed_count":[0-9]*' | cut -d':' -f2)
    SUCCESS=$(echo $TASK_INFO | grep -o '"success_count":[0-9]*' | cut -d':' -f2)

    PROGRESS=$((COMPLETED * 100 / TARGET_COUNT))
    echo "  进度: $COMPLETED/$TARGET_COUNT ($PROGRESS%) | 成功: $SUCCESS | 状态: $STATUS"

    if [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ]; then
        break
    fi
done

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

# 输出结果
echo ""
echo "================================"
echo "📈 测试结果"
echo "================================"
echo "总耗时: ${DURATION}秒"
echo "成功数: $SUCCESS"
echo "平均速度: $(echo "scale=2; $SUCCESS / $DURATION" | bc 2>/dev/null || echo "N/A") 个/秒"
echo ""

if [ "$SUCCESS" -gt 0 ]; then
    echo "✅ 性能测试通过"
    exit 0
else
    echo "❌ 性能测试失败"
    exit 1
fi
