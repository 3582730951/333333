#!/bin/bash
# ChatGPT 账号池生命周期管理 - 一键实施脚本
#
# 本脚本整合三个开源项目到 pool_server：
# 1. chatgpt-auto-register (注册引擎)
# 2. aBaiAutoplus (支付引擎 + 架构)
# 3. GuJumpgate (checkout 转换服务)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SERVICES_DIR="$PROJECT_ROOT/services"

echo "=========================================="
echo "  ChatGPT 账号池生命周期管理 - 实施脚本"
echo "=========================================="
echo ""

# 颜色输出
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# 检查依赖
check_dependencies() {
    info "检查系统依赖..."

    if ! command -v python3 &> /dev/null; then
        error "Python 3 未安装，请先安装 Python 3.10+"
    fi

    PYTHON_VERSION=$(python3 --version | awk '{print $2}')
    info "Python 版本: $PYTHON_VERSION"

    if ! command -v go &> /dev/null; then
        warn "Go 未安装，将只安装 Python 服务"
    else
        GO_VERSION=$(go version | awk '{print $3}')
        info "Go 版本: $GO_VERSION"
    fi
}

# 步骤 1: 复制注册引擎代码
setup_registration_service() {
    info "设置注册引擎服务..."

    REG_SRC="$PROJECT_ROOT/../other_gpt/chatgpt_auto_register/chatgpt-auto-register"
    REG_DEST="$SERVICES_DIR/chatgpt_register"

    if [ ! -d "$REG_SRC" ]; then
        error "源码目录不存在: $REG_SRC"
    fi

    mkdir -p "$REG_DEST"

    # 复制核心文件
    info "  复制核心代码..."
    cp "$REG_SRC/chatgpt_register.py" "$REG_DEST/" || error "复制 chatgpt_register.py 失败"
    cp "$REG_SRC/sentinel.py" "$REG_DEST/" || error "复制 sentinel.py 失败"
    cp "$REG_SRC/smsbower.py" "$REG_DEST/" || error "复制 smsbower.py 失败"
    cp "$REG_SRC/phone_sms.py" "$REG_DEST/" || error "复制 phone_sms.py 失败"
    cp "$REG_SRC/auth.py" "$REG_DEST/" || warn "auth.py 不存在，跳过"

    # 创建 requirements.txt
    cat > "$REG_DEST/requirements.txt" <<EOF
flask>=3.0.0
flask-cors>=4.0.0
curl-cffi>=0.6.0
requests>=2.31.0
pyjwt>=2.8.0
beautifulsoup4>=4.12.0
cryptography>=41.0.0
EOF

    # 创建 HTTP API 服务
    cat > "$REG_DEST/register_service.py" <<'PYEOF'
#!/usr/bin/env python3
"""
ChatGPT 账号注册 HTTP 服务
"""
import os
import sys
import logging
import traceback
from flask import Flask, request, jsonify
from flask_cors import CORS

# 导入注册模块
try:
    from chatgpt_register import ChatGPTRegister
    from smsbower import SmsBower
except ImportError as e:
    print(f"导入失败: {e}")
    print("请确保核心文件已复制到当前目录")
    sys.exit(1)

logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

app = Flask(__name__)
CORS(app)

@app.route('/health', methods=['GET'])
def health():
    return jsonify({"status": "ok", "service": "chatgpt_register"})

@app.route('/register', methods=['POST'])
def register_account():
    """
    注册 ChatGPT 账号

    Request:
    {
        "sms_provider": "smsbower",
        "sms_config": {"api_key": "...", "country": "151", "service": "dr"},
        "proxy": "http://...",
        "password": "...",
        "name": "John",
        "birthdate": "1990-01-01"
    }

    Response:
    {
        "ok": true,
        "email": "...",
        "password": "...",
        "phone": "...",
        "session_token": "...",
        "access_token": "...",
        "account_id": "..."
    }
    """
    try:
        data = request.json
        logger.info(f"收到注册请求: {data.get('sms_provider')}")

        if not data.get('sms_config'):
            return jsonify({"ok": False, "error": "缺少 sms_config"}), 400

        # 初始化 SMS
        sms_config = data['sms_config']
        sms = SmsBower(
            api_key=sms_config['api_key'],
            country=sms_config.get('country', '151'),
            service=sms_config.get('service', 'dr')
        )

        # 初始化注册器
        register = ChatGPTRegister(
            proxy=data.get('proxy'),
            password=data.get('password', ''),
            name=data.get('name', 'A'),
            birthdate=data.get('birthdate', '2000-01-01')
        )

        # 执行注册
        logger.info("开始注册...")
        result = register.register_one(sms)

        logger.info(f"注册完成: {result.get('ok')}")
        return jsonify(result)

    except Exception as e:
        logger.error(f"注册失败: {str(e)}")
        logger.error(traceback.format_exc())
        return jsonify({
            "ok": False,
            "error": str(e),
            "traceback": traceback.format_exc()
        }), 500

if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8801))
    host = os.environ.get('HOST', '127.0.0.1')
    logger.info(f"启动注册服务: {host}:{port}")
    app.run(host=host, port=port, debug=False)
PYEOF

    chmod +x "$REG_DEST/register_service.py"

    # 创建虚拟环境
    info "  创建 Python 虚拟环境..."
    cd "$REG_DEST"
    python3 -m venv .venv
    source .venv/bin/activate
    pip install --upgrade pip > /dev/null
    pip install -r requirements.txt || warn "依赖安装可能有问题，请手动检查"
    deactivate

    info "  ✓ 注册服务设置完成"
}

# 步骤 2: 复制支付引擎代码
setup_payment_service() {
    info "设置支付引擎服务..."

    PAY_SRC="$PROJECT_ROOT/../other_gpt/chatgpt_auto_register/chatgpt-auto-register"
    PAY_DEST="$SERVICES_DIR/plus_payment"

    mkdir -p "$PAY_DEST"

    # 复制支付相关文件
    info "  复制支付代码..."
    for file in gopay_pay.py gopay_register.py payment_protocol.py stripe_http.py paypal_http.py; do
        if [ -f "$PAY_SRC/$file" ]; then
            cp "$PAY_SRC/$file" "$PAY_DEST/"
            info "    复制 $file"
        else
            warn "    $file 不存在，跳过"
        fi
    done

    # 创建 requirements.txt
    cat > "$PAY_DEST/requirements.txt" <<EOF
flask>=3.0.0
flask-cors>=4.0.0
curl-cffi>=0.6.0
requests>=2.31.0
playwright>=1.40.0
cryptography>=41.0.0
EOF

    # 创建 HTTP API 服务
    cat > "$PAY_DEST/payment_service.py" <<'PYEOF'
#!/usr/bin/env python3
"""
ChatGPT Plus 支付 HTTP 服务
"""
import os
import logging
from flask import Flask, request, jsonify
from flask_cors import CORS

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = Flask(__name__)
CORS(app)

@app.route('/health', methods=['GET'])
def health():
    return jsonify({"status": "ok", "service": "plus_payment"})

@app.route('/generate-plus-link', methods=['POST'])
def generate_plus_link():
    """生成 Plus 支付链接"""
    try:
        data = request.json
        logger.info("生成 Plus 支付链接...")

        # TODO: 实现 Stripe checkout 逻辑
        return jsonify({
            "ok": False,
            "error": "待实现 - 需要整合 stripe_http.py 逻辑"
        }), 501

    except Exception as e:
        logger.error(f"生成链接失败: {e}")
        return jsonify({"ok": False, "error": str(e)}), 500

@app.route('/gopay-pay', methods=['POST'])
def gopay_pay():
    """GoPay 支付"""
    try:
        data = request.json
        logger.info("处理 GoPay 支付...")

        # TODO: 实现 GoPay 支付逻辑
        return jsonify({
            "ok": False,
            "error": "待实现 - 需要整合 gopay_pay.py 逻辑"
        }), 501

    except Exception as e:
        logger.error(f"支付失败: {e}")
        return jsonify({"ok": False, "error": str(e)}), 500

if __name__ == '__main__':
    port = int(os.environ.get('PORT', 8802))
    host = os.environ.get('HOST', '127.0.0.1')
    logger.info(f"启动支付服务: {host}:{port}")
    app.run(host=host, port=port, debug=False)
PYEOF

    chmod +x "$PAY_DEST/payment_service.py"

    # 创建虚拟环境
    info "  创建 Python 虚拟环境..."
    cd "$PAY_DEST"
    python3 -m venv .venv
    source .venv/bin/activate
    pip install --upgrade pip > /dev/null
    pip install -r requirements.txt || warn "依赖安装可能有问题，请手动检查"
    deactivate

    info "  ✓ 支付服务设置完成"
}

# 步骤 3: 设置 Checkout 转换服务
setup_checkout_converter() {
    info "设置 Checkout 转换服务..."

    CONV_SRC="$PROJECT_ROOT/../other_gpt/chatgpt_auto_register/GuJumpgate/services/checkout-converter"
    CONV_DEST="$SERVICES_DIR/checkout_converter"

    if [ ! -d "$CONV_SRC" ]; then
        warn "  GuJumpgate checkout-converter 不存在，跳过"
        return 0
    fi

    mkdir -p "$CONV_DEST"

    # 直接复制整个目录
    info "  复制 checkout-converter..."
    cp -r "$CONV_SRC"/* "$CONV_DEST/"

    # 创建虚拟环境
    info "  创建 Python 虚拟环境..."
    cd "$CONV_DEST"
    python3 -m venv .venv
    source .venv/bin/activate
    pip install --upgrade pip > /dev/null
    pip install -r requirements.txt || warn "依赖安装可能有问题，请手动检查"
    deactivate

    info "  ✓ Checkout 转换服务设置完成"
}

# 步骤 4: 创建 systemd 服务
create_systemd_services() {
    info "创建 systemd 服务文件..."

    SYSTEMD_DIR="/etc/systemd/system"

    # 注册服务
    cat > "$SYSTEMD_DIR/codex-pool-register.service" <<EOF
[Unit]
Description=Codex Pool Registration Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$SERVICES_DIR/chatgpt_register
ExecStart=$SERVICES_DIR/chatgpt_register/.venv/bin/python register_service.py
Restart=on-failure
RestartSec=10
Environment="HOST=127.0.0.1"
Environment="PORT=8801"

[Install]
WantedBy=multi-user.target
EOF

    # 支付服务
    cat > "$SYSTEMD_DIR/codex-pool-payment.service" <<EOF
[Unit]
Description=Codex Pool Payment Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$SERVICES_DIR/plus_payment
ExecStart=$SERVICES_DIR/plus_payment/.venv/bin/python payment_service.py
Restart=on-failure
RestartSec=10
Environment="HOST=127.0.0.1"
Environment="PORT=8802"

[Install]
WantedBy=multi-user.target
EOF

    # Checkout 转换服务（如果存在）
    if [ -d "$SERVICES_DIR/checkout_converter" ]; then
        cat > "$SYSTEMD_DIR/codex-pool-checkout.service" <<EOF
[Unit]
Description=Codex Pool Checkout Converter Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$SERVICES_DIR/checkout_converter
ExecStart=$SERVICES_DIR/checkout_converter/.venv/bin/python -m uvicorn app:app --host 127.0.0.1 --port 8803
Restart=on-failure
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
    fi

    systemctl daemon-reload

    info "  ✓ systemd 服务文件创建完成"
    info ""
    info "  启动服务命令："
    info "    systemctl start codex-pool-register"
    info "    systemctl start codex-pool-payment"
    info "    systemctl start codex-pool-checkout"
    info ""
    info "  开机自启："
    info "    systemctl enable codex-pool-register"
    info "    systemctl enable codex-pool-payment"
    info "    systemctl enable codex-pool-checkout"
}

# 步骤 5: 创建测试脚本
create_test_scripts() {
    info "创建测试脚本..."

    cat > "$SERVICES_DIR/test_services.sh" <<'TESTEOF'
#!/bin/bash
# 测试所有服务是否正常运行

set -e

echo "=========================================="
echo "  测试生命周期管理服务"
echo "=========================================="
echo ""

# 测试注册服务
echo "[1/3] 测试注册服务..."
if curl -s http://127.0.0.1:8801/health | grep -q '"status":"ok"'; then
    echo "  ✓ 注册服务运行正常"
else
    echo "  ✗ 注册服务异常"
    exit 1
fi

# 测试支付服务
echo "[2/3] 测试支付服务..."
if curl -s http://127.0.0.1:8802/health | grep -q '"status":"ok"'; then
    echo "  ✓ 支付服务运行正常"
else
    echo "  ✗ 支付服务异常"
    exit 1
fi

# 测试 Checkout 服务
echo "[3/3] 测试 Checkout 服务..."
if curl -s http://127.0.0.1:8803/healthz | grep -q 'ok'; then
    echo "  ✓ Checkout 服务运行正常"
else
    echo "  ⚠ Checkout 服务未运行（可选）"
fi

echo ""
echo "=========================================="
echo "  所有服务测试通过！"
echo "=========================================="
TESTEOF

    chmod +x "$SERVICES_DIR/test_services.sh"

    info "  ✓ 测试脚本创建完成: $SERVICES_DIR/test_services.sh"
}

# 主函数
main() {
    check_dependencies

    echo ""
    info "开始实施整合..."
    echo ""

    setup_registration_service
    echo ""

    setup_payment_service
    echo ""

    setup_checkout_converter
    echo ""

    if [ "$EUID" -eq 0 ]; then
        create_systemd_services
        echo ""
    else
        warn "非 root 用户，跳过 systemd 服务创建"
        warn "请使用 sudo 运行本脚本以创建系统服务"
        echo ""
    fi

    create_test_scripts
    echo ""

    info "=========================================="
    info "  整合完成！"
    info "=========================================="
    echo ""
    echo "下一步："
    echo "  1. 启动服务（需要 root）："
    echo "     sudo systemctl start codex-pool-register"
    echo "     sudo systemctl start codex-pool-payment"
    echo ""
    echo "  2. 测试服务："
    echo "     $SERVICES_DIR/test_services.sh"
    echo ""
    echo "  3. 访问管理面板："
    echo "     http://localhost:8787"
    echo ""
    echo "详细文档见:"
    echo "  - docs/commercial_integration_plan.md"
    echo "  - docs/lifecycle_management_commercial_plan.md"
    echo ""
}

# 运行主函数
main "$@"
