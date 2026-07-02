# Pool Server 部署：启用网关下载

## 📦 编译网关二进制（所有平台）

在 pool_server 目录执行：

```bash
bash scripts/build-gateway.sh
```

输出：
```
🔨 预编译网关二进制...
  ✓ 编译 linux/amd64...
  ✓ 编译 linux/arm64...
  ✓ 编译 darwin/amd64...
  ✓ 编译 darwin/arm64...
  ✓ 编译 windows/amd64...
✅ 编译完成！二进制文件：
-rwxr-xr-x  bin/gateway-darwin-amd64
-rwxr-xr-x  bin/gateway-darwin-arm64
-rwxr-xr-x  bin/gateway-linux-amd64
-rwxr-xr-x  bin/gateway-linux-arm64
-rwxr-xr-x  bin/gateway-windows-amd64.exe
```

## 🚀 部署到生产

### 方式 1：预编译（推荐）

```bash
# 在开发机编译
bash scripts/build-gateway.sh

# 上传到 VPS
rsync -av bin/ your-vps:/path/to/pool_server/bin/

# VPS 上重启 pool_server
ssh your-vps "systemctl restart pool-server"
```

### 方式 2：实时编译

Pool server 会在用户首次下载时自动编译。需要：
- VPS 上安装 Go 编译器
- pool_server 目录包含 `cmd/gateway` 源码

## 📡 用户访问端点

启动 pool_server 后，用户可访问：

### 1. 一键安装脚本
```bash
curl -fsSL https://your-vps.com:1455/install-gateway.sh | bash
```

### 2. 直接下载二进制
```bash
curl -fsSL https://your-vps.com:1455/download/gateway -o gateway
```

二进制会根据用户的 User-Agent 自动选择平台。

## 🔒 HTTPS 配置（重要）

CA 信任需要 HTTPS，建议：

1. **使用反向代理**（Nginx/Caddy）
```nginx
server {
    listen 443 ssl;
    server_name your-vps.com;

    ssl_certificate /etc/letsencrypt/live/your-vps.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/your-vps.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:1455;
    }
}
```

2. **或使用自签名证书**
```bash
# 生成证书
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes

# pool_server 配置
{
  "tls_cert": "cert.pem",
  "tls_key": "key.pem"
}
```

## 📝 更新 README

在 pool_server 的 README 添加：

```markdown
## 网关安装（用户端）

一键安装：
\`\`\`bash
curl -fsSL https://your-vps.com/install-gateway.sh | bash
\`\`\`

详见：[网关用户指南](docs/GATEWAY_USER_GUIDE.md)
```

## 🧪 测试端点

```bash
# 测试安装脚本下载
curl -I https://your-vps.com:1455/install-gateway.sh
# 期望：200 OK, Content-Type: text/x-shellscript

# 测试二进制下载（macOS）
curl -I https://your-vps.com:1455/download/gateway \
  -A "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
# 期望：200 OK, Content-Type: application/octet-stream
```

## 📊 监控

### 查看下载日志

Pool server 日志会记录：
```
INFO: Gateway download: client=darwin/amd64, ip=1.2.3.4
```

### 统计下载量

```bash
# 查看 access log
tail -f /var/log/pool-server/access.log | grep "/download/gateway"
```

## 🎯 完整部署检查清单

- [ ] 编译所有平台二进制（`bash scripts/build-gateway.sh`）
- [ ] 二进制上传到 VPS `bin/` 目录
- [ ] HTTPS 配置完成（Let's Encrypt 或自签名）
- [ ] Pool server 重启
- [ ] 测试安装脚本：`curl -fsSL https://your-vps.com/install-gateway.sh | head`
- [ ] 测试二进制下载：`curl -I https://your-vps.com/download/gateway`
- [ ] 更新 README，添加用户安装指引
- [ ] 通知用户新的安装方式

---

**现在用户只需一条命令即可完成全部安装！**
