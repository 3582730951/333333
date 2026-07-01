#!/bin/bash
# 一键启动所有服务
# 用法: ./start.sh [otp_mode]
# otp_mode: manual(默认) | sms_api | whatsapp

set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

echo "=== GoPay Plus 自动订阅机 ==="

# 检查 config.json
if [ ! -f config.json ]; then
    echo "❌ config.json 不存在，请先复制并编辑："
    echo "   cp config.example.json config.json"
    exit 1
fi

# 检查 Python 依赖
python3 -c "import curl_cffi, grpc" 2>/dev/null || {
    echo "❌ Python 依赖未安装，执行："
    echo "   pip install -r requirements.txt"
    exit 1
}

# 停止旧进程
pkill -f "payment_server.py" 2>/dev/null || true
pkill -f "orchestrator.py" 2>/dev/null || true
pkill -f "node.*to_whatsapp" 2>/dev/null || true
sleep 1

# 启动支付核心
echo "→ 启动 plus_gopay_links (gRPC :50051)..."
cd "$DIR/plus_gopay_links"
python3 payment_server.py --config "$DIR/config.json" --listen :50051 &
PID1=$!
cd "$DIR"

sleep 2

# 启动编排器
echo "→ 启动 orchestrator (:8800)..."
python3 orchestrator.py &
PID2=$!

# 如果是 whatsapp 模式，启动 relay
OTP_MODE=$(python3 -c "import json; print(json.load(open('config.json')).get('otp',{}).get('mode','manual'))")
if [ "$OTP_MODE" = "whatsapp" ]; then
    echo "→ 启动 to_whatsapp (gRPC :50056)..."
    cd "$DIR/to_whatsapp"
    if [ ! -d node_modules ]; then
        echo "  安装 npm 依赖..."
        npm install --production
    fi
    node index.js &
    PID3=$!
    cd "$DIR"
    echo "✅ 三个服务已启动 (PIDs: $PID1, $PID2, $PID3)"
else
    echo "✅ 两个服务已启动 (PIDs: $PID1, $PID2)"
    echo "   OTP 模式: $OTP_MODE"
fi

echo ""
echo "测试: curl http://localhost:8800/health"
echo "订阅: curl -X POST http://localhost:8800/subscribe -H 'Content-Type: application/json' -d '{\"session_token\": \"eyJ...\"}'"
echo ""
echo "按 Ctrl+C 停止所有服务"

# 等待退出
trap "kill $PID1 $PID2 ${PID3:-} 2>/dev/null; echo '已停止'" EXIT
wait
