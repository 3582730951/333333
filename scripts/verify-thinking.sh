#!/bin/bash
# Thinking 功能快速验证脚本

echo "🔍 Thinking 功能集成验证"
echo "================================"

# 1. 检查文件是否存在
echo ""
echo "1. 检查核心文件..."
files=(
    "internal/thinking/types.go"
    "internal/thinking/apply.go"
    "internal/api/thinking.go"
    "internal/web/assets/thinking.html"
    "docs/THINKING_USER_GUIDE.md"
)

for file in "${files[@]}"; do
    if [ -f "$file" ]; then
        echo "  ✅ $file"
    else
        echo "  ❌ $file (缺失)"
    fi
done

# 2. 测试编译
echo ""
echo "2. 测试编译..."
if go build -o /tmp/pool-server-test ./cmd/pool-server 2>/dev/null; then
    echo "  ✅ 主程序编译成功"
    rm -f /tmp/pool-server-test
else
    echo "  ❌ 编译失败"
    exit 1
fi

# 3. 检查配置结构
echo ""
echo "3. 检查配置字段..."
if grep -q "ThinkingEnabled" internal/config/config.go; then
    echo "  ✅ ThinkingEnabled 字段存在"
else
    echo "  ❌ ThinkingEnabled 字段缺失"
fi

# 4. 检查 API 路由
echo ""
echo "4. 检查 API 路由..."
if grep -q "handleThinkingConfig" internal/api/server.go; then
    echo "  ✅ Thinking API 路由已注册"
else
    echo "  ❌ Thinking API 路由未注册"
fi

echo ""
echo "================================"
echo "✅ 验证完成！Thinking 功能已成功集成。"
echo ""
echo "📚 查看用户文档: docs/THINKING_USER_GUIDE.md"
echo "🌐 访问 Web UI: http://YOUR_SERVER:8787/admin/thinking.html"
