# 导航集成修复 (Navigation Integration Fix)

## 问题 (Issue)
用户反馈：访问 http://168.143.109.26:8787/ 看不到注册管理界面。

**根本原因**：`registration.html` 和 `proxy-configs.html` 已嵌入二进制且可访问（HTTP 200），但主控制台的侧边栏导航（`app.js` 中的 `NAV_ADMIN` 数组）没有对应的入口，导致用户无法通过 UI 访问。

## 解决方案 (Solution)

### 修改的文件

#### 1. `internal/web/assets/i18n.js`
添加了两个新的导航翻译键：

```javascript
// 中文
"nav.lifecycle": "生命周期管理"
"nav.proxies": "代理配置"

// English
"nav.lifecycle": "Lifecycle"
"nav.proxies": "Proxy Config"
```

#### 2. `internal/web/assets/app.js`
在 `NAV_ADMIN` 数组中添加了两个新条目：

```javascript
const NAV_ADMIN = [
  // ... existing entries ...
  { v: "gopay", ic: "card", key: "nav.gopay" },
  { v: "lifecycle", ic: "refresh", key: "nav.lifecycle" },    // ✅ 新增
  { v: "proxies", ic: "shuffle", key: "nav.proxies" },        // ✅ 新增
  { v: "org", ic: "briefcase", key: "nav.org" },
  { v: "settings", ic: "gear", key: "nav.settings" },
];
```

在 `loadView()` 函数的路由映射中添加了导航处理：

```javascript
lifecycle: () => window.location.href = "/registration.html",
proxies: () => window.location.href = "/proxy-configs.html",
```

## 部署 (Deployment)

### 方法 1：完整更新（推荐）
```bash
cd /workspace/pool_server
sudo ./update.sh
```

### 方法 2：手动替换二进制
```bash
# 1. 本地编译新二进制
cd /workspace/pool_server
go build -o codex-pool ./cmd/pool-server

# 2. 上传到服务器（替换 <your-server>）
scp codex-pool root@168.143.109.26:/tmp/

# 3. 在服务器上替换并重启
ssh root@168.143.109.26
sudo systemctl stop codex-pool
sudo mv /tmp/codex-pool /usr/local/bin/codex-pool
sudo chmod +x /usr/local/bin/codex-pool
sudo systemctl start codex-pool
```

### 方法 3：远程快速编译部署
```bash
# 在服务器上直接编译（需要 Go 环境）
ssh root@168.143.109.26
cd /path/to/pool_server
go build -o /tmp/codex-pool ./cmd/pool-server
sudo systemctl stop codex-pool
sudo mv /tmp/codex-pool /usr/local/bin/codex-pool
sudo systemctl start codex-pool
sudo systemctl status codex-pool
```

## 验证 (Verification)

### 1. 检查二进制是否包含新导航
```bash
strings codex-pool | grep "nav.lifecycle"
# 应该输出: "nav.lifecycle": "生命周期管理"
```

### 2. 访问控制台
打开浏览器访问：http://168.143.109.26:8787/

管理员登录后，在左侧导航栏应该能看到：
- **生命周期管理** (Lifecycle) — 点击跳转到 `/registration.html`
- **代理配置** (Proxy Config) — 点击跳转到 `/proxy-configs.html`

### 3. 检查页面是否正常加载
```bash
curl -I http://168.143.109.26:8787/registration.html
# 应该返回: HTTP/1.1 200 OK

curl -I http://168.143.109.26:8787/proxy-configs.html
# 应该返回: HTTP/1.1 200 OK
```

## 导航位置 (Navigation Position)

新导航项插入在 **GoPay 订阅** 和 **租户/项目** 之间：

```
管理端 (Admin)
├── 概览 (Overview)
├── 账号 (Accounts)
├── ...
├── GoPay 订阅 (GoPay)
├── ✅ 生命周期管理 (Lifecycle)      ← 新增
├── ✅ 代理配置 (Proxy Config)        ← 新增
├── 租户/项目 (Tenants/Projects)
└── 接入/设置 (Setup/Settings)
```

## 图标说明 (Icons)

- **生命周期管理**: 使用 `refresh` 图标（循环箭头，象征账号生命周期）
- **代理配置**: 使用 `shuffle` 图标（交叉箭头，象征网络路由）

## 技术细节 (Technical Details)

### 为什么使用 `window.location.href` 而不是 SPA 路由？

`registration.html` 和 `proxy-configs.html` 是**独立的完整 HTML 页面**（非 SPA 内嵌视图），它们：
1. 有自己的 `<html>`、`<head>`、`<body>` 结构
2. 包含完整的 JavaScript 逻辑和样式
3. 通过 `/admin/lifecycle/*` API 直接与后端通信

因此使用**全页导航**（`window.location.href`）是正确的选择，而非在主 SPA 中内联渲染。

## 回滚 (Rollback)

如果需要移除这些导航项：

```bash
# 恢复到之前的二进制
sudo systemctl stop codex-pool
sudo cp /usr/local/bin/codex-pool.prev /usr/local/bin/codex-pool
sudo systemctl start codex-pool
```

## 文件清单 (File Checklist)

- ✅ `internal/web/assets/i18n.js` — 添加翻译键
- ✅ `internal/web/assets/app.js` — 添加导航项和路由
- ✅ `internal/web/assets/registration.html` — 已存在（嵌入）
- ✅ `internal/web/assets/proxy-configs.html` — 已存在（嵌入）
- ✅ 编译验证通过（15MB 二进制，包含所有前端资源）

---

**状态**: ✅ 已修复，待部署  
**构建时间**: 2026-06-07 10:02 UTC  
**二进制大小**: 15M  
**影响范围**: 仅前端导航，无后端变更
