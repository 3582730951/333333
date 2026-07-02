#!/bin/bash
# End-to-End Registration Test

set -e

echo "================================"
echo "端到端注册流程测试"
echo "================================"

API_BASE="http://localhost:8787"

# 1. 创建注册任务
echo ""
echo "1️⃣ 创建注册任务..."
TASK_RESPONSE=$(curl -s -X POST "${API_BASE}/admin/lifecycle/tasks" \
  -H "Content-Type: application/json" \
  -d '{
    "task_type": "register",
    "platform": "chatgpt",
    "target_count": 1,
    "group_name": "test-e2e",
    "concurrency": 1
  }')

TASK_ID=$(echo $TASK_RESPONSE | grep -o '"task_id":"[^"]*"' | cut -d'"' -f4)

if [ -z "$TASK_ID" ]; then
    echo "❌ 创建任务失败"
    echo "响应: $TASK_RESPONSE"
    exit 1
fi

echo "✅ 任务创建成功: $TASK_ID"

# 2. 等待任务完成
echo ""
echo "2️⃣ 等待任务完成..."
for i in {1..30}; do
    sleep 2
    TASK_INFO=$(curl -s "${API_BASE}/admin/lifecycle/tasks/${TASK_ID}")
    STATUS=$(echo $TASK_INFO | grep -o '"status":"[^"]*"' | cut -d'"' -f4)

    echo "   状态: $STATUS (${i}次检查)"

    if [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ]; then
        break
    fi
done

# 3. 检查结果
echo ""
echo "3️⃣ 检查任务结果..."
TASK_INFO=$(curl -s "${API_BASE}/admin/lifecycle/tasks/${TASK_ID}")
echo "$TASK_INFO" | python3 -m json.tool 2>/dev/null || echo "$TASK_INFO"

SUCCESS_COUNT=$(echo $TASK_INFO | grep -o '"success_count":[0-9]*' | cut -d':' -f2)

if [ "$SUCCESS_COUNT" -gt 0 ]; then
    echo ""
    echo "✅ 测试通过！成功注册 $SUCCESS_COUNT 个账号"
    exit 0
else
    echo ""
    echo "❌ 测试失败！没有成功注册账号"
    exit 1
fi
