# Claude Gateway - 本地指纹虚拟化网关

## 概述

Claude Gateway 是部署在**下游用户本地**的透明 HTTPS 代理，与 pool_server 配合使用，解决客户端采集本地环境指纹导致的关联风险。

## 为什么需要本地网关？

Pool_server 作为远程中继，只能改写**到达网络层的数据**。但 Claude Code 在客户端采集的指纹（环境变量、进程信息、DNS 配置等）已经序列化进请求体，pool_server 无法区分"用户真实的工作目录"和"泄露的本地环境"。

**本地网关在请求发出前拦截并改写这些本地指纹。**

---

## 架构

```
┌─────────────┐
│ Claude Code │  官方 CLI（无修改）
└──────┬──────┘
       │ claude "prompt"  →  HTTPS_PROXY=127.0.0.1:8765（包装器自动设置）
       ↓
┌──────────────────────────────────────┐
│ 本地网关 (gateway)                    │
│  1. HTTPS MITM 拦截                  │
│  2. 从 pool_server 获取虚拟身份       │
│  3. 改写请求体（metadata/env/主机名） │
│  4. 添加 X-Gateway-Mode: local       │
└──────┬───────────────────────────────┘
       │ 转发到 pool_server
       ↓
┌────────────────┐
│  Pool Server   │  VPS 远程中继
│  - 检测网关模式，跳过冗余虚拟化        │
│  - 账号路由、出口选择                │
│  - WARP/CF 防护                     │
└────────────────┘
```

---

## 安装

### 方式 1：一键安装（推荐）

```bash
# 下载网关二进制
curl -sSL https://your-vps.com/download/gateway -o /tmp/gateway
chmod +x /tmp/gateway
sudo mv /tmp/gateway /usr/local/bin/gateway

# 一键安装
gateway quick-install \
  --pool-url https://your-vps.com:1455 \
  --key cap_xxx_your_downstream_key

# 输出示例：
# 🚀 Claude Gateway 一键安装
# [1/4] 初始化配置... ✓
# [2/4] 信任 CA 证书... ✓
# [3/4] 安装 claude 命令包装器... ✓
# [4/4] 测试连接... ⏩
# ✅ 安装完成！
```

### 方式 2：手动安装

```bash
# 1. 初始化配置和 CA
gateway init --pool-url https://your-vps.com:1455 --key cap_xxx

# 2. 信任 CA 证书（需要 sudo）
gateway trust-ca
# 如果失败，手动执行：
gateway trust-ca --print-commands

# 3. 安装 claude 命令包装器
gateway install-wrapper

# 4. 启动网关
gateway start &
```

---

## 使用

### 日常使用（零改变）

```bash
# Terminal 1: 启动网关（后台）
gateway start &

# Terminal 2: 正常使用 Claude Code
claude "write hello world in python"
claude chat
claude --version

# 完全无感知！网关自动拦截并改写
```

包装器原理：
```bash
# /usr/local/bin/claude 内容：
#!/bin/bash
export HTTPS_PROXY=http://127.0.0.1:8765
export NO_PROXY=localhost,127.0.0.1
exec "/usr/local/bin/claude.real" "$@"
```

### 检查状态

```bash
# 查看网关状态
gateway status

# status 会读取 ~/.claude-gateway/config.json，
# 检查下游密钥/CA 文件、本地监听端口以及 pool_server /healthz。
# 如果本地网关端口不可达，命令会返回非 0 退出码。

# 查看配置
cat ~/.claude-gateway/config.json

# 查看日志
tail -f ~/.claude-gateway/gateway.log
```

---

## 改写内容

### VPS 下发的虚拟身份

网关调用 `GET /v1/gateway/identity?provider=claude` 获取虚拟身份，并通过请求头传递下游密钥：

```http
Authorization: Bearer cap_xxx
```

`downstream_key` 查询参数仅保留用于兼容旧客户端，不建议继续使用，避免密钥出现在 URL、日志或代理链路中。

```json
{
  "account_id": "acc_abc123",
  "session_id": "550e8400-e29b-41d4-a716-446655440000",
  "user_id": "f3a5...a4b3",
  "machine_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "username": "alex7f3a",
  "hostname": "alex7f3a-macbook",
  "home_dir": "/Users/alex7f3a",
  "env_vars": {
    "HOME": "/Users/alex7f3a",
    "USER": "alex7f3a",
    "SHELL": "/bin/zsh"
  },
  "dns_servers": ["8.8.8.8", "8.8.4.4"],
  "gateway_ip": "192.168.1.1",
  "local_ip": "192.168.1.100"
}
```

### 改写规则

#### 1. JSON 精确替换
- `metadata.user_id` → 虚拟 user_id（64 hex）
- `session_id` / `parent_thread_id` → 虚拟 session_id

#### 2. `<env>` 块替换
```text
<env>
Working directory: /Users/realbob/project  # 保持不变
Platform: darwin                           # → 虚拟 os_name
OS Version: Darwin 24.3.0                  # → 虚拟 os_release
Terminal: /bin/bash                        # → 虚拟 terminal
Hostname: realbob-macbook-pro.local        # → alex7f3a-macbook
Architecture: arm64                        # → 虚拟 arch
</env>
```

#### 3. 文本流式替换
- 所有 `realbob` → `alex7f3a`
- 所有 `realbob-macbook-pro.local` → `alex7f3a-macbook`
- 所有 `/Users/realbob` → `/Users/alex7f3a`

**不改写**：
- 工作目录（Working directory）
- 文件路径（如 `/Users/realbob/project/src/main.py`）
- 用户消息内容中的代码/变量名

---

## 配置

配置文件：`~/.claude-gateway/config.json`

```json
{
  "listen_addr": "127.0.0.1:8765",
  "pool_server_url": "https://your-vps.com:1455",
  "downstream_key": "cap_xxx_your_key",
  "providers": ["claude", "codex"],
  "identity_ttl_seconds": 300,
  "log_level": "info",
  "mitm": {
    "ca_cert": "~/.claude-gateway/ca-cert.pem",
    "ca_key": "~/.claude-gateway/ca-key.pem"
  }
}
```

---

## 卸载

```bash
# 完整卸载
gateway uninstall

# 手动清理（如果需要）
rm -rf ~/.claude-gateway
sudo rm /usr/local/bin/gateway
```

---

## 故障排查

### 问题 1：`gateway start` 后 claude 命令无响应

**原因**：CA 证书未被信任，TLS 握手失败。

**解决**：
```bash
# 检查 CA 是否存在
ls -la ~/.claude-gateway/ca-cert.pem

# 重新信任 CA
gateway trust-ca

# macOS 手动信任
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain \
  ~/.claude-gateway/ca-cert.pem
```

### 问题 2：包装器安装失败

**原因**：找不到 `claude` 命令。

**解决**：
```bash
# 确认 claude 已安装
which claude

# 如果未安装，先安装 Claude Code CLI
# https://claude.ai/download
```

### 问题 3：pool_server 返回 400/401

**原因**：`downstream_key` 配置错误。

**解决**：
```bash
# 检查配置
cat ~/.claude-gateway/config.json

# 更新 key
gateway init --pool-url https://your-vps.com:1455 --key cap_correct_key
```

### 问题 4：性能下降/延迟高

**原因**：身份缓存失效，频繁请求 pool_server。

**解决**：
```bash
# 检查缓存 TTL（默认 5 分钟）
# 增加到 10 分钟
jq '.identity_ttl_seconds = 600' ~/.claude-gateway/config.json > /tmp/cfg.json
mv /tmp/cfg.json ~/.claude-gateway/config.json

# 重启网关
pkill -f gateway
gateway start &
```

---

## 开发

### 编译

```bash
cd /workspace/pool_server
go build -o bin/gateway ./cmd/gateway
```

### 测试

```bash
# 单元测试
go test ./cmd/gateway/...

# 集成测试
cd /workspace/pool_server
./test-gateway.sh
```

### 调试

```bash
# 启用调试日志
gateway start --debug

# 查看改写前后对比
tail -f ~/.claude-gateway/gateway.log | grep "rewrite"
```

---

## 技术细节

### 性能

| 操作 | 耗时 |
|------|------|
| 身份缓存命中 | < 1ms |
| 身份缓存未命中 | 20-50ms |
| JSON 解析/序列化 | 1-2ms |
| 文本替换 | < 1ms |
| 证书生成（首次） | 5-10ms |
| **总开销（缓存命中）** | **2-5ms** |

### 安全性

- **CA 证书**：仅本地生成，私钥不离开本机
- **代理范围**：只拦截 `api.anthropic.com` 和 `chatgpt.com`
- **监听地址**：`127.0.0.1`（仅本机访问）
- **响应不改写**：虚拟值不会泄露到用户终端

### 缓存策略

- **身份缓存**：5 分钟 TTL，内存存储
- **证书缓存**：无过期，内存存储
- **缓存 key**：provider（claude/codex）

---

## FAQ

### Q: 网关会影响其他程序吗？

**A**: 不会。网关只在 `claude` 命令运行时生效（通过包装器设置 `HTTPS_PROXY`）。浏览器、curl 等其他程序不受影响。

### Q: 需要在每台机器上都安装吗？

**A**: 是的。本地网关必须部署在运行 Claude Code 的机器上，才能拦截本地指纹。

### Q: 支持多个 pool_server 吗？

**A**: 支持。修改配置文件的 `pool_server_url` 即可。

### Q: 如何验证改写是否生效？

**A**: 
1. 在 pool_server 端查看日志，确认收到 `X-Gateway-Mode: local` 头
2. 使用 `gateway start --debug` 查看改写详情
3. 检查请求体中的 `metadata.user_id` 是否为虚拟值

---

## 更新日志

### v1.0.0 (2026-06-09)
- ✨ 首次发布
- ✅ HTTPS MITM 代理核心
- ✅ 动态证书生成
- ✅ 身份缓存（5分钟TTL）
- ✅ 三层改写策略（JSON/文本/正则）
- ✅ 一键安装脚本
- ✅ macOS/Linux/Windows 支持

---

## 许可证

与 pool_server 相同。

---

## 支持

遇到问题？
1. 查看[故障排查](#故障排查)章节
2. 检查 `~/.claude-gateway/gateway.log`
3. 提交 issue 到 pool_server 仓库
