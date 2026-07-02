# PayPal Plus Automation — VPS Deployment Guide

## 概述

PayPal Plus 自动化使用 **headless Chrome** 完成支付流程，**完全兼容无桌面环境的云 VPS**（无需 X11/Wayland）。

---

## 🖥️ 系统要求

### 操作系统
- Ubuntu 22.04+ / Debian 11+
- RHEL 9 / Rocky Linux / AlmaLinux
- Docker (alpine + chrome 镜像)

### 资源要求
- **CPU**: 2+ cores (推荐)
- **内存**: 2GB+ (Chrome headless 需 ~500MB)
- **磁盘**: /tmp 至少 1GB 可用（截图调试）
- **网络**: 出站 HTTPS (443) 到 chatgpt.com, paypal.com, stripe.com

---

## 📦 安装 Chrome（VPS 环境）

### 方式 1: 自动化脚本（推荐）

```bash
cd /workspace/pool_server
sudo bash scripts/install_chrome_headless.sh
```

脚本会：
1. 检测 OS (Ubuntu/Debian/RHEL)
2. 添加 Google Chrome 仓库
3. 安装 Chrome stable + 字体
4. 验证 headless 模式

### 方式 2: 手动安装

#### Ubuntu/Debian
```bash
sudo apt-get update
sudo apt-get install -y wget gnupg2
wget -q -O - https://dl.google.com/linux/linux_signing_key.pub | sudo apt-key add -
echo "deb [arch=amd64] http://dl.google.com/linux/chrome/deb/ stable main" | sudo tee /etc/apt/sources.list.d/google-chrome.list
sudo apt-get update
sudo apt-get install -y google-chrome-stable fonts-liberation fonts-noto-cjk
```

#### RHEL/CentOS/Rocky
```bash
sudo tee /etc/yum.repos.d/google-chrome.repo << 'EOF'
[google-chrome]
name=google-chrome
baseurl=http://dl.google.com/linux/chrome/rpm/stable/x86_64
enabled=1
gpgcheck=1
gpgkey=https://dl.google.com/linux/linux_signing_key.pub
EOF
sudo yum install -y google-chrome-stable liberation-fonts google-noto-cjk-fonts
```

### 验证安装

```bash
google-chrome --version
# 应输出: Google Chrome 13x.x.xxxx.xx

# 测试 headless 模式
google-chrome --headless --disable-gpu --no-sandbox --dump-dom https://www.google.com
```

---

## 🐳 Docker 部署（推荐生产环境）

### Dockerfile 示例

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /build
COPY . .
RUN go build -o pool-server ./cmd/pool-server

FROM alpine:3.19
# 安装 Chrome (使用 chromium 作为替代)
RUN apk add --no-cache chromium chromium-chromedriver \
    ca-certificates \
    font-liberation \
    font-noto-cjk

# 创建非 root 用户（安全）
RUN adduser -D -u 1000 pooluser
WORKDIR /app
COPY --from=builder /build/pool-server .
COPY --from=builder /build/scripts ./scripts

# Chrome 需要 /tmp 可写
RUN mkdir -p /tmp && chmod 1777 /tmp

# 切换到非 root 用户
USER pooluser

ENV ROD_CHROME_BIN=/usr/bin/chromium-browser
EXPOSE 8080

CMD ["./pool-server"]
```

### docker-compose.yml

```yaml
version: '3.8'
services:
  pool-server:
    build: .
    ports:
      - "8080:8080"
    environment:
      - ADMIN_TOKEN=your_secure_token
      - ROD_CHROME_BIN=/usr/bin/chromium-browser
      - ROD_HEADLESS=true
    volumes:
      - ./data:/app/data
      - /dev/shm:/dev/shm  # 重要：共享内存（避免 Chrome 崩溃）
    shm_size: 2gb          # 或使用此参数（二选一）
    restart: unless-stopped
```

**关键配置**：
- `shm_size: 2gb` 或 `--shm-size=2g` — Chrome 需要足够的共享内存
- `/dev/shm` 挂载（二选一）

---

## ⚙️ 配置 PayPal 自动化

### 1. 在管理后台配置 PayPal 凭证

访问 `/admin/register` → **注册配置** → **支付方式** → **PayPal Plus**：

```json
{
  "paypal_email": "your-paypal@example.com",
  "paypal_password": "your-paypal-password"
}
```

**安全提示**：
- 使用专用 PayPal 测试账号（不要用主账号）
- 凭证存储在 `provider_settings` 表（加密推荐）
- 生产环境建议使用环境变量注入

### 2. 环境变量方式（推荐生产）

```bash
export PAYPAL_EMAIL="your-paypal@example.com"
export PAYPAL_PASSWORD="your-secure-password"
```

在 `cmd/pool-server/main.go` 中修改：

```go
paypalSettings := map[string]interface{}{
    "paypal_email":    os.Getenv("PAYPAL_EMAIL"),
    "paypal_password": os.Getenv("PAYPAL_PASSWORD"),
}
paymentMgr.Register(payment.NewPaypalProvider(store, paypalSettings))
```

---

## 🚀 使用流程

### API 调用

```bash
# 1. 创建注册任务（自动生成账号）
curl -X POST http://localhost:8080/admin/register/batch \
  -H "Authorization: Bearer YOUR_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "count": 1,
    "plan": "plus",
    "payment_method": "paypal"
  }'

# 返回: {"job_id": "job_xxx"}

# 2. 查看任务状态
curl http://localhost:8080/admin/register/jobs \
  -H "Authorization: Bearer YOUR_ADMIN_TOKEN"

# 3. 实时日志（SSE）
curl http://localhost:8080/admin/register/job/events?id=job_xxx \
  -H "Authorization: Bearer YOUR_ADMIN_TOKEN"
```

### Web UI

1. 访问 `/admin` → **账号注册**
2. 选择 **Plan = Plus**, **Payment = PayPal**
3. 点击 **创建任务**
4. 查看 **任务列表** 实时进度
5. 点击 **查看日志** 观察浏览器自动化流程

---

## 🔍 调试与故障排查

### 启用截图调试

修改 `paypal_automation.go`：

```go
automation := NewPayPalAutomation(checkoutURL, email, password, nil)
automation.screenshotDir = "/app/data/screenshots" // 持久化目录
```

截图会保存为：
- `paypal-01-checkout-loaded-{timestamp}.png`
- `paypal-02-paypal-clicked-{timestamp}.png`
- `paypal-03-login-filled-{timestamp}.png`
- ...

### 常见问题

#### 1. Chrome 崩溃 "Failed to move to new namespace: PID namespaces"

**原因**: Docker 容器权限不足

**解决**:
```yaml
# docker-compose.yml
security_opt:
  - seccomp=unconfined
cap_add:
  - SYS_ADMIN
```

**或**使用 `--no-sandbox`（降低安全性）：

```go
l := launcher.New().
    Headless(true).
    Set("no-sandbox")  // 仅用于不可信容器环境
```

#### 2. Chrome 无法找到 "command not found"

**原因**: rod 找不到 Chrome 二进制

**解决**:
```bash
export ROD_CHROME_BIN=/usr/bin/google-chrome-stable
# 或 Debian/Alpine: /usr/bin/chromium-browser
```

#### 3. PayPal 2FA 阻塞

**现象**: 自动化在 2FA 步骤超时

**解决方案**:
- **自动化 OTP**: 集成 SMS provider（见下文）
- **手动 OTP**: 暂时返回 URL，操作员手动完成
- **预授权设备**: 使用已信任的 PayPal 账号（无 2FA）

### OTP 自动化（可选）

实现 `OTPProvider` 接口：

```go
type SMSOTPProvider struct {
    smsProvider *sms.SMSBowerProvider
    phoneNumber string
}

func (p *SMSOTPProvider) GetOTP(ctx context.Context, phone string, timeout time.Duration) (string, error) {
    // 轮询 SMS provider 获取 PayPal OTP
    return p.smsProvider.WaitCode(ctx, p.phoneNumber, timeout)
}

// 使用
automation := NewPayPalAutomation(url, email, password, &SMSOTPProvider{...})
```

---

## 📊 性能与限制

### 资源消耗（单次支付）
- **CPU**: 50-80% (2-3分钟峰值)
- **内存**: 600-800MB (Chrome headless)
- **耗时**: 2-5分钟（含 PayPal 登录 + 2FA + 授权）

### 并发建议
- **VPS (2核4G)**: 并发 2-3 个支付流程
- **VPS (4核8G)**: 并发 5-8 个
- 使用任务队列控制并发（避免 Chrome 实例过多）

### 反爬虫对策
- ✅ **Stealth mode** (已启用 `go-rod/stealth`)
- ✅ **User-Agent 伪装**
- ⚠️ PayPal 可能检测 headless 特征（持续监控）
- 推荐：使用代理轮换 + 延迟随机化

---

## 🔐 安全建议

1. **凭证加密**: PayPal 密码存储前使用 AES-256 加密
2. **网络隔离**: Chrome 仅访问必需域名（防火墙白名单）
3. **审计日志**: 记录所有支付操作到 `audit_logs` 表
4. **定期轮换**: 每 30 天更换 PayPal 测试账号
5. **容器安全**: 避免 `--no-sandbox`（除非必需）

---

## 📝 生产部署 Checklist

- [ ] Chrome/Chromium 已安装（`google-chrome --version`）
- [ ] Headless 模式测试通过
- [ ] Docker shm_size ≥ 2GB 或 /dev/shm 挂载
- [ ] PayPal 凭证配置（环境变量或加密存储）
- [ ] 截图目录权限正确（`chmod 777 /app/data/screenshots`）
- [ ] 防火墙允许出站到 paypal.com (443)
- [ ] 监控告警（支付失败率 > 10% 告警）
- [ ] 日志轮转配置（Chrome 日志可能很大）

---

## 🆘 技术支持

遇到问题？检查以下日志：

1. **Pool Server 日志**: `journalctl -u pool-server -f`
2. **Chrome 日志**: 添加 `--enable-logging --v=1` 到 launcher
3. **截图**: 检查 `/app/data/screenshots/paypal-*.png`
4. **网络抓包**: `tcpdump -i any -w /tmp/paypal.pcap host paypal.com`

---

**更新日期**: 2025-06-09  
**兼容版本**: pool-server v1.0+ (Session 27+)
