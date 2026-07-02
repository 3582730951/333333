# 网关一键安装指南（用户端）

## 🚀 快速安装

打开终端，执行一条命令：

```bash
curl -fsSL https://your-vps.com/install-gateway.sh | bash
```

**替换 `your-vps.com` 为你的 VPS 地址！**

---

## 📋 安装步骤

脚本会自动完成：

1. ✅ 检测你的操作系统（macOS/Linux/Windows）
2. ✅ 从 VPS 下载对应的网关二进制
3. ✅ 安装到 `/usr/local/bin/gateway`
4. ✅ 生成自签名 CA 证书
5. ✅ 信任 CA（可能需要输入密码）
6. ✅ 安装 `claude` 命令包装器（如果已安装 Claude Code）

---

## 🎬 示例输出

```
🚀 Claude Gateway 自动安装脚本
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
[1/5] 下载网关...
  ✓ 下载完成
[2/5] 安装到 /usr/local/bin...
  ✓ 已安装
[3/5] 配置网关...
请输入 Pool Server URL [默认: https://your-vps.com:1455]: 
请输入下游 API Key (cap_xxx): cap_abc123_your_key
[4/5] 初始化配置和 CA...
  ✓ 配置已保存: ~/.claude-gateway/config.json
  ✓ CA 已生成: ~/.claude-gateway/ca-cert.pem
[5/5] 信任 CA 证书...
  ✓ CA 已信任
  ✓ 包装器已安装
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
✅ 安装完成！

使用方法:
  1. 启动网关: gateway start &
  2. 使用 Claude: claude "your prompt"

查看状态: gateway status
查看日志: tail -f ~/.claude-gateway/gateway.log
```

---

## 🔧 使用

### 启动网关

```bash
gateway start &
```

网关会在后台运行（监听 `127.0.0.1:8765`）。

### 使用 Claude Code

```bash
claude "write hello world in python"
claude chat
```

**完全无感知！** 网关自动拦截请求并改写本地指纹。

---

## 🛠️ 手动安装（如果脚本失败）

### 1. 下载二进制

```bash
# 直接下载（自动检测系统）
curl -fsSL https://your-vps.com/download/gateway -o /tmp/gateway
chmod +x /tmp/gateway
sudo mv /tmp/gateway /usr/local/bin/gateway
```

### 2. 初始化

```bash
gateway init \
  --pool-url https://your-vps.com:1455 \
  --key cap_xxx_your_key
```

### 3. 信任 CA

```bash
gateway trust-ca
```

如果失败，查看手动指令：
```bash
gateway trust-ca --print-commands
```

### 4. 安装包装器

```bash
gateway install-wrapper
```

### 5. 启动网关

```bash
gateway start &
```

---

## ❓ 常见问题

### Q: 如何获取 API Key？

A: 联系 pool_server 管理员获取 `cap_` 开头的密钥。

### Q: CA 信任失败怎么办？

A: 运行 `gateway trust-ca --print-commands` 查看手动指令，复制执行即可。

**macOS 示例**：
```bash
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  ~/.claude-gateway/ca-cert.pem
```

### Q: 网关启动后 claude 命令无响应？

A: 检查 CA 是否已信任：
```bash
# macOS
security find-certificate -c "Claude Gateway" -a

# Linux
ls /usr/local/share/ca-certificates/claude-gateway.crt
```

### Q: 如何卸载？

A: 
```bash
gateway uninstall
sudo rm /usr/local/bin/gateway
rm -rf ~/.claude-gateway
```

### Q: 支持哪些操作系统？

A: 
- ✅ macOS (Intel / Apple Silicon)
- ✅ Linux (amd64 / arm64)
- ✅ Windows (amd64)

---

## 🔐 安全说明

1. **CA 证书**：仅本地生成，私钥不离开你的机器
2. **代理范围**：只拦截 `api.anthropic.com` 和 `chatgpt.com`
3. **其他程序**：浏览器、curl 等不受影响
4. **监听地址**：`127.0.0.1`（仅本机访问）

---

## 📞 需要帮助？

1. 查看日志：`tail -f ~/.claude-gateway/gateway.log`
2. 测试连接：`gateway test`
3. 联系管理员

---

## 🎯 工作原理

```
你的电脑                         VPS
┌──────────────┐
│ claude "..."  │
└──────┬───────┘
       │ ① 自动走代理（包装器设置）
       ↓
┌──────────────────┐
│ 本地网关         │
│ 127.0.0.1:8765   │
│ ② 改写本地指纹    │
└──────┬───────────┘
       │ ③ 转发
       ↓
    ┌──────────────┐
    │ Pool Server  │  你的 VPS
    │ :1455        │
    └──────────────┘
```

1. **包装器**：`claude` 命令自动设置 `HTTPS_PROXY=127.0.0.1:8765`
2. **网关拦截**：改写请求中的用户名、主机名、环境变量等
3. **转发到 VPS**：pool_server 再进行账号路由和出口选择
