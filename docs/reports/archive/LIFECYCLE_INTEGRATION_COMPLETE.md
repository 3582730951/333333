# 生命周期管理系统集成完成报告

## ✅ 集成完成时间
2026-06-07

## 📋 完成的工作

### 1. 前端资源嵌入
- ✅ 将 `frontend/registration.html` 复制到 `internal/web/assets/`
- ✅ 将 `frontend/proxy-configs.html` 复制到 `internal/web/assets/`
- ✅ 前端资源已通过 Go embed 自动嵌入到二进制中
- ✅ 编译验证通过（15MB 二进制包含所有前端资源）

### 2. install.sh 脚本扩展
添加了完整的生命周期服务支持：

#### 新增配置变量
```bash
WITH_LIFECYCLE="${WITH_LIFECYCLE:-1}"  # 默认启用
LIFECYCLE_REGISTER_SOURCE="${PROJECT_ROOT}/services/chatgpt_register"
LIFECYCLE_REGISTER_INSTALL="${DATA_DIR}/lifecycle/register"
LIFECYCLE_REGISTER_VENV="${DATA_DIR}/lifecycle-register-venv"
LIFECYCLE_REGISTER_ADDR="127.0.0.1:8791"
LIFECYCLE_PAYMENT_SOURCE="${PROJECT_ROOT}/services/plus_payment"
LIFECYCLE_PAYMENT_INSTALL="${DATA_DIR}/lifecycle/payment"
LIFECYCLE_PAYMENT_VENV="${DATA_DIR}/lifecycle-payment-venv"
LIFECYCLE_PAYMENT_ADDR="127.0.0.1:8792"
```

#### 新增命令行参数
- `--with-lifecycle` - 安装生命周期服务（默认）
- `--without-lifecycle` - 不安装生命周期服务
- `--full` - 现在包括生命周期服务
- `--minimal` - 排除生命周期服务

#### 新增函数
- `install_lifecycle_services()` - 安装 2 个 Python 服务
  - 智能检测源码变化（SHA256 哈希比对）
  - 创建独立的 Python virtualenv
  - 安装依赖（Flask 3.0、requests 2.31）
  - 设置正确的文件权限

#### 新增 systemd 服务单元
1. **codex-pool-register.service**
   - 描述: Codex Pool Lifecycle Register Service
   - 工作目录: `/var/lib/codex-pool/lifecycle/register`
   - 监听: 127.0.0.1:8791
   - 启动命令: Python + Flask

2. **codex-pool-payment.service**
   - 描述: Codex Pool Lifecycle Payment Service
   - 工作目录: `/var/lib/codex-pool/lifecycle/payment`
   - 监听: 127.0.0.1:8792
   - 启动命令: Python + Flask

#### 服务启动逻辑
- 智能重启：仅当源码变化或服务未运行时才重启
- 零停机：保持运行中的服务在更新时不中断
- 自动启用：systemd enable + start
- 状态监控：安装后显示服务状态

#### 安装摘要更新
```
Lifecycle:     codex-pool-register.service (127.0.0.1:8791), 
               codex-pool-payment.service (127.0.0.1:8792)

Useful service commands:
  systemctl status codex-pool-register.service
  systemctl status codex-pool-payment.service
```

### 3. update.sh 脚本
✅ **无需修改** - update.sh 完全委托给 install.sh，所有功能自动继承

### 4. 目录结构
```
/workspace/pool_server/
├── services/
│   ├── chatgpt_register/          # 注册服务（已存在）
│   │   ├── register_service.py
│   │   ├── requirements.txt
│   │   ├── start.sh
│   │   └── 其他支持文件
│   ├── plus_payment/              # 支付服务（已存在）
│   │   ├── payment_service.py
│   │   ├── requirements.txt
│   │   └── start.sh
├── internal/web/assets/
│   ├── index.html                # 主管理面板（已存在）
│   ├── registration.html         # ✅ 新增：任务管理页面
│   ├── proxy-configs.html        # ✅ 新增：代理配置页面
│   ├── admin.js, app.js, ...     # 其他资源
```

## 🚀 使用方法

### 全新安装（包含生命周期服务）
```bash
cd /workspace/pool_server
sudo ./scripts/install.sh
# 或显式启用
sudo ./scripts/install.sh --with-lifecycle
```

### 更新现有部署（自动集成生命周期服务）
```bash
cd /workspace/pool_server
sudo ./update.sh
```

### 仅安装主服务（不安装生命周期）
```bash
sudo ./scripts/install.sh --without-lifecycle
# 或
sudo ./scripts/install.sh --minimal
```

## 📊 安装后的服务列表

运行 `systemctl list-units 'codex-pool*'` 将看到：

```
codex-pool.socket              loaded active listening
codex-pool.service             loaded active running
codex-pool-sidecar.service     loaded active running
codex-pool-register.service    loaded active running   # ✅ 新增
codex-pool-payment.service     loaded active running   # ✅ 新增
```

## 🔗 访问前端

安装完成后，可以通过以下 URL 访问：

```
http://<server-ip>:8787/                     # 主管理面板（账号池）
http://<server-ip>:8787/registration.html   # ✅ 生命周期：任务管理
http://<server-ip>:8787/proxy-configs.html  # ✅ 生命周期：代理配置
```

## 🔍 验证安装

### 检查服务状态
```bash
systemctl status codex-pool-register.service
systemctl status codex-pool-payment.service
```

### 查看日志
```bash
journalctl -u codex-pool-register.service -f
journalctl -u codex-pool-payment.service -f
```

### 测试 API 端点
```bash
# 注册服务健康检查
curl http://127.0.0.1:8791/health

# 支付服务健康检查
curl http://127.0.0.1:8792/health
```

## 📝 配置文件位置

```
/etc/codex-pool/config.json                      # 主配置
/var/lib/codex-pool/lifecycle/register/          # 注册服务安装目录
/var/lib/codex-pool/lifecycle/payment/           # 支付服务安装目录
/var/lib/codex-pool/lifecycle-register-venv/     # 注册服务 Python venv
/var/lib/codex-pool/lifecycle-payment-venv/      # 支付服务 Python venv
```

## ✨ 智能特性

1. **源码变化检测**
   - 使用 SHA256 哈希检测服务代码是否变化
   - 未变化时跳过重启，保持服务连续性

2. **零配置**
   - 默认端口自动分配（8791、8792）
   - virtualenv 自动创建和管理
   - 依赖自动安装

3. **权限隔离**
   - 服务以 `codex-pool` 用户运行
   - ProtectSystem=strict
   - PrivateTmp=true
   - NoNewPrivileges=true

4. **容错设计**
   - 服务源码缺失时跳过安装（带警告）
   - 依赖安装失败不影响主服务
   - 自动重启策略（RestartSec=3）

## 🎯 兼容性

- ✅ 向后兼容：旧版本使用 `--without-lifecycle` 可保持原有行为
- ✅ 前向兼容：未来添加新服务只需扩展 `install_lifecycle_services()`
- ✅ 配置兼容：所有路径通过环境变量可定制

## 📦 依赖清单

### Python 依赖（两个服务相同）
```
flask==3.0.0
werkzeug==3.0.0
requests==2.31.0
```

### 系统依赖
- Python 3.8+（已在 install.sh 中确保）
- python3-venv
- pip

## 🔧 故障排除

### 服务无法启动
```bash
# 检查日志
journalctl -u codex-pool-register.service -n 50

# 手动测试
sudo -u codex-pool /var/lib/codex-pool/lifecycle-register-venv/bin/python \
  /var/lib/codex-pool/lifecycle/register/register_service.py
```

### 依赖安装失败
```bash
# 重新创建 venv
sudo rm -rf /var/lib/codex-pool/lifecycle-register-venv
sudo python3 -m venv /var/lib/codex-pool/lifecycle-register-venv
sudo /var/lib/codex-pool/lifecycle-register-venv/bin/pip install -r \
  /var/lib/codex-pool/lifecycle/register/requirements.txt
```

## 🎉 总结

**完整的生命周期管理系统现在已完全集成到 update.sh + install.sh 中！**

- ✅ 前端嵌入（2 个 HTML 页面）
- ✅ Python 服务安装（2 个 Flask 服务）
- ✅ systemd 单元（2 个服务）
- ✅ 智能重启逻辑
- ✅ 零停机更新
- ✅ 完整的文档和故障排除

**下次运行 `sudo ./update.sh` 时，生命周期服务将自动安装和更新！**
