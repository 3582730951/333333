# 网关一键安装（用户端）

## 🚀 方式 1：带 API Key 安装（推荐）

**管理员提供专属链接：**

```bash
curl -fsSL "https://your-vps.com/install-gateway.sh?key=cap_xxx_your_key" | bash
```

脚本会**自动填充你的 API Key**，无需手动输入。

---

## 🔧 方式 2：手动输入 API Key

```bash
curl -fsSL https://your-vps.com/install-gateway.sh | bash
```

脚本会提示输入 API Key：
```
[3/5] 配置网关...
请输入下游 API Key (cap_xxx): cap_xxx_your_key
```

---

## 📋 安装步骤

脚本自动完成：

1. ✅ 检测操作系统（macOS/Linux/Windows）
2. ✅ 下载对应网关二进制
3. ✅ 安装到 `/usr/local/bin/gateway`
4. ✅ 使用你的 API Key 初始化配置
5. ✅ 生成并信任 CA 证书
6. ✅ 安装 `claude` 命令包装器

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
  ✓ 使用 API Key: cap_xxx_your_key
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
```

---

## 🔒 多组支持

**为什么需要识别 API Key？**

不同用户可能属于不同的路由组（如 `group_a`、`group_b`），每组有不同的：
- 账号池
- 出口配置
- 模型权限

**方式 1** 的链接包含你的专属 API Key，确保：
- 网关连接到正确的路由组
- 使用对应组的账号池
- 应用组的配置策略

---

## 🛠️ 使用

### 启动网关

```bash
gateway start &
```

### 使用 Claude Code

```bash
claude "write hello world in python"
```

**完全无感知！** 网关自动拦截并改写。

---

## 📞 获取安装链接

联系管理员获取专属链接：

```
https://your-vps.com/install-gateway.sh?key=cap_xxx_your_key
```

或使用通用链接（需手动输入 Key）：

```
https://your-vps.com/install-gateway.sh
```

---

## ❓ 常见问题

### Q: 我没有 API Key 怎么办？

A: 联系 pool_server 管理员申请。管理员会分配 `cap_` 开头的密钥。

### Q: 可以多台机器共用一个 Key 吗？

A: 可以！同一个 API Key 可以在多台机器安装，都会使用同一组账号池。

### Q: API Key 会泄露吗？

A: 
- 链接只在首次安装时使用
- Key 保存在本地配置 `~/.claude-gateway/config.json`
- 网关只向你的 VPS 发送请求

### Q: 如何更换 API Key？

A:
```bash
gateway init --pool-url https://your-vps.com:1455 --key cap_new_key
gateway start &
```

---

## 🎯 管理员：如何生成用户专属链接

在 pool_server 管理后台：

1. 创建下游 Key：`cap_xxx`
2. 生成安装链接：
   ```
   https://your-vps.com/install-gateway.sh?key=cap_xxx
   ```
3. 发给用户

**用户执行一条命令即可完成安装！**
