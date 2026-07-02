# Pool Server 前端使用说明

## ✅ 前端已集成！

前端 UI 已经完全集成到 Pool Server 中，可以直接访问。

---

## 🚀 快速开始

### 1. 启动服务

```bash
cd /workspace/pool_server
./pool_server
```

### 2. 访问前端页面

打开浏览器访问：

- **任务管理页面**: http://localhost:8787/frontend/registration.html
- **代理配置页面**: http://localhost:8787/frontend/proxy-configs.html

---

## 📋 功能说明

### 任务管理页面 (registration.html)

**功能**:
- ✅ 创建新的注册/Plus 任务
- ✅ 查看任务列表
- ✅ 实时查看任务进度
- ✅ 查看任务日志
- ✅ 取消运行中的任务
- ✅ 自动刷新（每 5 秒）

**支持的任务类型**:
- 仅注册 (register)
- 仅升级 Plus (upgrade_plus)
- 注册 + Plus (register_and_plus)

**配置选项**:
- 平台选择 (ChatGPT/Claude)
- 目标数量
- 并发数
- 分组名称
- 代理配置 ID（可选）

### 代理配置页面 (proxy-configs.html)

**功能**:
- ✅ 添加代理配置
- ✅ 查看代理列表
- ✅ 代理类型选择（固定/动态/旋转）
- ✅ 代理提供商支持
- ✅ 指纹浏览器开关

**支持的代理类型**:
- 固定代理 (static)
- 动态代理 (dynamic) - Luminati/Oxylabs
- 旋转网关 (rotating)

---

## 🔌 API 端点

前端会调用以下 API 端点：

### 任务管理 API
```
POST   /admin/lifecycle/tasks           # 创建任务
GET    /admin/lifecycle/tasks           # 获取任务列表
GET    /admin/lifecycle/tasks/:id       # 获取任务详情
GET    /admin/lifecycle/tasks/:id/logs  # 获取任务日志
POST   /admin/lifecycle/tasks/:id/cancel # 取消任务
```

### 静态文件
```
GET    /frontend/registration.html      # 任务管理页面
GET    /frontend/proxy-configs.html     # 代理配置页面
```

---

## 📊 任务状态说明

| 状态 | 说明 | 颜色 |
|------|------|------|
| pending | 等待执行 | 橙色 |
| running | 执行中 | 蓝色 |
| completed | 已完成 | 绿色 |
| failed | 失败 | 红色 |
| cancelled | 已取消 | 灰色 |

---

## 💡 使用示例

### 1. 创建注册任务

```bash
# 在任务管理页面填写：
任务类型: 仅注册
平台: ChatGPT
目标数量: 5
并发数: 2
分组名称: test-group
```

点击"创建任务"后，任务会立即开始执行，并显示在任务列表中。

### 2. 监控任务进度

任务列表会自动刷新（每 5 秒），显示：
- 任务 ID
- 任务状态
- 进度条
- 完成数量 / 目标数量
- 成功 / 失败数量

### 3. 查看任务日志

点击任务卡片中的"📝 查看日志"按钮，会在新窗口打开日志页面，显示：
- 账号序号
- 日志级别 (info/error/warn)
- 日志消息
- 时间戳

---

## 🔧 技术实现

### 前端技术
- 纯 HTML + CSS + JavaScript
- 无需 Node.js 或构建工具
- 原生 Fetch API 调用后端
- 自动刷新机制

### 后端集成
- Go HTTP ServeMux 路由
- RESTful API 设计
- JSON 数据交换
- 静态文件服务

---

## 🎨 界面特点

- ✅ 简洁美观的设计
- ✅ 响应式布局
- ✅ 实时数据更新
- ✅ 进度条可视化
- ✅ 状态颜色区分
- ✅ 友好的错误提示

---

## 🔄 API 调用流程

```
┌─────────┐  HTTP GET   ┌──────────┐  Query DB  ┌──────────┐
│ 浏览器  │ ──────────> │ Go Server│ ─────────> │ SQLite   │
│         │             │          │            │          │
│         │ <────────── │          │ <───────── │          │
└─────────┘  JSON Data  └──────────┘            └──────────┘
```

1. 浏览器发送 HTTP 请求到 Go 服务器
2. Go 服务器查询 SQLite 数据库
3. 返回 JSON 数据给浏览器
4. 浏览器渲染页面

---

## 🐛 故障排查

### 前端无法访问？

1. 确认服务已启动：
```bash
./pool_server
# 应该看到: codex pool server listening on 0.0.0.0:8787
```

2. 确认端口未被占用：
```bash
netstat -tlnp | grep 8787
```

3. 检查防火墙设置

### API 返回错误？

1. 查看服务器日志
2. 检查数据库是否初始化
3. 确认路由配置正确

### 任务不执行？

1. 检查生命周期功能是否启用（config.json）
2. 检查 Python 服务是否启动
3. 查看任务日志了解详情

---

## 📞 获取帮助

- 查看主文档: `README_LIFECYCLE.md`
- 查看 API 文档: `internal/api/admin_lifecycle.go`
- 运行测试: `go test ./... -v`

---

**前端已完全集成，立即可用！** 🎉
