# ChatGPT 账号池完整生命周期管理方案

## 项目定位

这是一个**商业化账号池管理平台**，整合三个开源项目的能力：
1. **chatgpt-auto-register**：协议注册 ChatGPT 账号
2. **aBaiAutoplus**：多平台账号管理 + Plus 订阅
3. **GuJumpgate**：浏览器扩展辅助工具

提供给管理员的功能：
- ✅ **注册 → 入池**：注册 Free 账号直接放入账号池
- ✅ **注册 → Plus → 入池**：注册后自动升级 Plus 再入池
- ✅ **已有账号 → Plus**：对池内账号批量升级 Plus
- ✅ **生命周期管理**：自动检测、刷新、隔离失效账号

**关键特性：默认关闭，管理员选择启用**

---

## 一、系统架构

```
┌─────────────────────────────────────────────────────────────┐
│                  Pool Server (Go Gateway)                   │
│  - 核心网关功能（/v1/messages, /v1/responses）              │
│  - 账号池管理（导入、分组、出口）                            │
│  - 管理面板（现有功能）                                      │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│              账号生命周期管理模块（新增）                    │
├─────────────────────────────────────────────────────────────┤
│  Go Backend (internal/)                                     │
│  ├── registration/   注册编排服务                           │
│  ├── subscription/   订阅编排服务                           │
│  └── lifecycle/      生命周期管理                           │
│                                                              │
│  Python Services (services/) - 可选启用                     │
│  ├── chatgpt_register/  注册引擎 (HTTP API)                 │
│  ├── plus_payment/      支付引擎 (HTTP API)                 │
│  └── checkout_converter/ GuJumpgate的checkout转换服务      │
│                                                              │
│  Browser Extension (可选)                                   │
│  └── GuJumpgate/        浏览器辅助工具                      │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│                   第三方服务集成                             │
│  - SMS 接码平台 (SmsBower, HeroSMS, SMSPool)               │
│  - 邮箱服务 (Outlook, MoeMail, Cloudflare Worker)          │
│  - 支付账号池 (GoPay, PayPal)                              │
└─────────────────────────────────────────────────────────────┘
```

---

## 二、核心功能流程

### 流程 A：注册 → 入池

```
管理员操作：
1. 配置供应商（邮箱、SMS）
2. 创建注册任务
   - 数量：10 个
   - 分组：cyber
   - 流程：仅注册
3. 启动任务

系统执行：
1. Python 服务调用注册引擎
2. 获取邮箱 → 获取手机号 → 注册 → 验证
3. 获得 session_token + access_token
4. 自动导入 pool_server 账号池
5. 状态：active, plan_type: free
```

### 流程 B：注册 → Plus → 入池

```
管理员操作：
1. 配置供应商（邮箱、SMS、支付）
2. 创建注册任务
   - 数量：10 个
   - 分组：cyber
   - 流程：注册 + 升级 Plus
   - 支付方式：GoPay / PayPal
3. 启动任务

系统执行：
1. 执行流程 A（注册）
2. 对每个注册成功的账号：
   a. 生成 Stripe checkout 链接
   b. 调用支付引擎（GoPay/PayPal）
   c. 完成支付
3. 更新账号状态：plan_type: plus
4. 放入账号池
```

### 流程 C：池内账号 → Plus

```
管理员操作：
1. 在账号列表选择账号
2. 批量升级 → Plus
3. 选择支付方式

系统执行：
1. 读取账号 token
2. 生成支付链接
3. 调用支付引擎
4. 更新账号状态
```

---

## 三、数据库 Schema

### 3.1 扩展现有 accounts 表

```sql
-- 已有字段保持不变，新增：
ALTER TABLE accounts ADD COLUMN registration_method TEXT DEFAULT 'manual';
-- 值：manual（手动导入）, auto_register（自动注册）, oauth（OAuth）

ALTER TABLE accounts ADD COLUMN phone TEXT;
-- 注册手机号

ALTER TABLE accounts ADD COLUMN subscription_status TEXT DEFAULT 'unknown';
-- 值：unknown, free, plus, team, enterprise

ALTER TABLE accounts ADD COLUMN subscription_expires_at INTEGER;
-- Plus 过期时间

ALTER TABLE accounts ADD COLUMN last_validity_check_at INTEGER;
-- 最后一次有效性检查时间

ALTER TABLE accounts ADD COLUMN registration_task_id TEXT;
-- 关联的注册任务 ID
```

### 3.2 新增表

```sql
-- 邮箱供应商
CREATE TABLE IF NOT EXISTS mailbox_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    provider_type TEXT NOT NULL,
    enabled INTEGER DEFAULT 1,
    is_default INTEGER DEFAULT 0,
    config_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- SMS 供应商
CREATE TABLE IF NOT EXISTS sms_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    provider_type TEXT NOT NULL,
    enabled INTEGER DEFAULT 1,
    is_default INTEGER DEFAULT 0,
    config_json TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 注册/订阅任务
CREATE TABLE IF NOT EXISTS lifecycle_tasks (
    id TEXT PRIMARY KEY,
    task_type TEXT NOT NULL, -- register, upgrade_plus, register_and_plus
    platform TEXT NOT NULL DEFAULT 'chatgpt',
    status TEXT NOT NULL, -- pending, running, completed, failed, cancelled
    config_json TEXT NOT NULL,
    target_count INTEGER DEFAULT 0,
    completed_count INTEGER DEFAULT 0,
    success_count INTEGER DEFAULT 0,
    failed_count INTEGER DEFAULT 0,
    result_json TEXT,
    created_at INTEGER NOT NULL,
    started_at INTEGER,
    finished_at INTEGER,
    created_by TEXT -- 创建者（管理员 ID）
);

-- 任务日志
CREATE TABLE IF NOT EXISTS lifecycle_task_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    level TEXT NOT NULL,
    message TEXT NOT NULL,
    timestamp INTEGER NOT NULL,
    FOREIGN KEY (task_id) REFERENCES lifecycle_tasks(id)
);

-- GoPay 账号池
CREATE TABLE IF NOT EXISTS gopay_accounts (
    id TEXT PRIMARY KEY,
    phone TEXT NOT NULL UNIQUE,
    pin TEXT NOT NULL,
    access_token TEXT,
    refresh_token TEXT,
    balance_idr INTEGER DEFAULT 0,
    status TEXT NOT NULL, -- active, exhausted, invalid
    usage_count INTEGER DEFAULT 0,
    last_used_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- PayPal 账号池
CREATE TABLE IF NOT EXISTS paypal_accounts (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE,
    password TEXT NOT NULL,
    region TEXT, -- jp, us
    status TEXT NOT NULL,
    balance_usd REAL DEFAULT 0,
    usage_count INTEGER DEFAULT 0,
    last_used_at INTEGER,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 生命周期事件
CREATE TABLE IF NOT EXISTS lifecycle_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id TEXT NOT NULL,
    event_type TEXT NOT NULL, -- registered, upgraded, check_valid, check_invalid, refreshed
    event_data TEXT,
    timestamp INTEGER NOT NULL,
    FOREIGN KEY (account_id) REFERENCES accounts(id)
);
```

---

## 四、Go 后端实现

### 4.1 配置文件扩展

`config.json` 新增：

```json
{
  // ... 现有配置 ...
  
  "lifecycle_management_enabled": false,
  "registration_service_enabled": false,
  "registration_service_url": "http://127.0.0.1:8801",
  "payment_service_enabled": false,
  "payment_service_url": "http://127.0.0.1:8802",
  "checkout_converter_enabled": false,
  "checkout_converter_url": "http://127.0.0.1:8803",
  
  "lifecycle_check_enabled": false,
  "lifecycle_check_interval_hours": 6,
  "token_refresh_enabled": false,
  "token_refresh_interval_hours": 12,
  "auto_quarantine_invalid": true
}
```

### 4.2 API 端点设计

#### 供应商管理

```
GET    /admin/providers/mailbox           列出邮箱供应商
POST   /admin/providers/mailbox           添加邮箱供应商
PUT    /admin/providers/mailbox/:id       更新
DELETE /admin/providers/mailbox/:id       删除
POST   /admin/providers/mailbox/:id/test  测试连接

GET    /admin/providers/sms               列出 SMS 供应商
POST   /admin/providers/sms               添加 SMS 供应商
PUT    /admin/providers/sms/:id           更新
DELETE /admin/providers/sms/:id           删除
POST   /admin/providers/sms/:id/test      测试连接
```

#### 任务管理

```
POST   /admin/lifecycle/tasks             创建任务
GET    /admin/lifecycle/tasks             列出所有任务
GET    /admin/lifecycle/tasks/:id         查询任务详情
POST   /admin/lifecycle/tasks/:id/cancel  取消任务
GET    /admin/lifecycle/tasks/:id/logs    实时日志流 (SSE)
DELETE /admin/lifecycle/tasks/:id         删除任务记录
```

#### 账号操作

```
POST   /admin/accounts/batch-upgrade      批量升级 Plus
POST   /admin/accounts/batch-check        批量检查有效性
POST   /admin/accounts/:id/upgrade        单个账号升级
GET    /admin/accounts/:id/lifecycle      查询生命周期事件
```

#### 支付账号池

```
GET    /admin/gopay-pool                  查询 GoPay 池状态
POST   /admin/gopay-pool                  添加 GoPay 账号
DELETE /admin/gopay-pool/:id              删除
POST   /admin/gopay-pool/register         自动注册 GoPay（调用服务）

GET    /admin/paypal-pool                 查询 PayPal 池状态
POST   /admin/paypal-pool                 添加 PayPal 账号
DELETE /admin/paypal-pool/:id             删除
```

#### 系统状态

```
GET    /admin/lifecycle/status            生命周期服务状态
GET    /admin/lifecycle/statistics        统计信息
```

### 4.3 Go 代码结构

```
internal/
├── registration/
│   ├── service.go          # 注册服务主入口
│   ├── engine_client.go    # 调用 Python 注册引擎
│   ├── task.go             # 任务编排
│   └── providers.go        # 供应商管理
│
├── subscription/
│   ├── service.go          # 订阅服务主入口
│   ├── payment_client.go   # 调用 Python 支付引擎
│   ├── gopay.go            # GoPay 池管理
│   ├── paypal.go           # PayPal 池管理
│   └── upgrade.go          # 升级逻辑
│
├── lifecycle/
│   ├── manager.go          # 生命周期管理器
│   ├── checker.go          # 有效性检查
│   ├── refresher.go        # Token 刷新
│   └── scheduler.go        # 定时任务
│
└── api/
    ├── admin_providers.go      # 供应商 API
    ├── admin_lifecycle.go      # 生命周期 API
    └── admin_payment_pool.go   # 支付池 API
```

---

## 五、Python 服务实现

### 5.1 注册服务 (services/chatgpt_register/)

**来源**：复制 `chatgpt-auto-register` 核心代码

**功能**：
- HTTP API 封装注册流程
- 支持多 SMS 平台
- 支持多邮箱渠道
- 返回注册结果（token、email、phone）

**API 端点**：
```
GET  /health
POST /register
POST /test-sms
POST /sentinel/solve
```

### 5.2 支付服务 (services/plus_payment/)

**来源**：复制 `aBaiAutoplus` 的支付相关代码

**功能**：
- 生成 Stripe checkout 链接
- GoPay 自动支付
- PayPal 自动支付

**API 端点**：
```
GET  /health
POST /generate-plus-link
POST /gopay-pay
POST /gopay-register
POST /paypal-pay
```

### 5.3 Checkout 转换服务 (services/checkout_converter/)

**来源**：直接使用 `GuJumpgate/services/checkout-converter`

**功能**：
- 独立部署的 checkout 转换服务
- 使用 curl_cffi 模拟浏览器
- 生成各种格式的支付链接

**API 端点**：
```
GET  /healthz
POST /api/checkout
```

---

## 六、前端 UI 设计

### 6.1 新增页面

#### 1. 供应商配置页 (`/admin/providers`)

**布局**：
```
┌─────────────────────────────────────────────┐
│ 供应商配置                                   │
├─────────────────────────────────────────────┤
│ 📧 邮箱供应商                                │
│ ┌─────────────────────────────────────────┐ │
│ │ ☑ MoeMail (默认)     [测试] [编辑] [删除] │ │
│ │ ☐ Outlook           [测试] [编辑] [删除] │ │
│ └─────────────────────────────────────────┘ │
│ [+ 添加邮箱供应商]                           │
│                                              │
│ 📱 SMS 供应商                                │
│ ┌─────────────────────────────────────────┐ │
│ │ ☑ SmsBower (默认)   [测试] [编辑] [删除] │ │
│ │ ☐ HeroSMS          [测试] [编辑] [删除] │ │
│ └─────────────────────────────────────────┘ │
│ [+ 添加 SMS 供应商]                          │
└─────────────────────────────────────────────┘
```

#### 2. 账号注册页 (`/admin/registration`)

**布局**：
```
┌─────────────────────────────────────────────┐
│ 账号注册                                     │
├─────────────────────────────────────────────┤
│ [创建注册任务]                               │
│                                              │
│ 任务列表                                     │
│ ┌─────────────────────────────────────────┐ │
│ │ ID: task-001                             │ │
│ │ 类型: 注册 + 升级 Plus                   │ │
│ │ 状态: ████████░░ 80% (8/10)              │ │
│ │ 成功: 8 | 失败: 0                        │ │
│ │ [查看日志] [取消]                         │ │
│ └─────────────────────────────────────────┘ │
│ ┌─────────────────────────────────────────┐ │
│ │ ID: task-002                             │ │
│ │ 类型: 仅注册                             │ │
│ │ 状态: ✓ 已完成 (10/10)                   │ │
│ │ 成功: 10 | 失败: 0                       │ │
│ │ [查看结果] [删除]                         │ │
│ └─────────────────────────────────────────┘ │
└─────────────────────────────────────────────┘
```

**创建任务对话框**：
```
┌─────────────────────────────────────┐
│ 创建注册任务                         │
├─────────────────────────────────────┤
│ 任务类型：                          │
│ ◉ 仅注册（Free 账号）               │
│ ○ 注册 + 升级 Plus                  │
│                                      │
│ 数量：[10]                           │
│ 目标分组：[cyber ▼]                 │
│                                      │
│ 邮箱供应商：[MoeMail ▼]             │
│ SMS 供应商：[SmsBower ▼]            │
│                                      │
│ 支付方式：（仅当选择升级时）         │
│ ○ GoPay  ○ PayPal日区  ○ PayPal美区 │
│                                      │
│ 并发数：[3] （1-10）                 │
│                                      │
│ [取消] [开始执行]                    │
└─────────────────────────────────────┘
```

#### 3. Plus 订阅管理页 (`/admin/subscription`)

**布局**：
```
┌─────────────────────────────────────────────┐
│ Plus 订阅管理                                │
├─────────────────────────────────────────────┤
│ 💳 支付账号池状态                            │
│ ┌─────────────────────────────────────────┐ │
│ │ GoPay:  5 个可用 | 余额总计: 500,000 IDR │ │
│ │ PayPal: 3 个可用 | 余额总计: $45.00 USD  │ │
│ └─────────────────────────────────────────┘ │
│                                              │
│ 📊 订阅统计                                  │
│ ┌─────────────────────────────────────────┐ │
│ │ Free:  120 │ Plus: 80 │ 过期: 5          │ │
│ └─────────────────────────────────────────┘ │
│                                              │
│ 🚀 快速操作                                  │
│ [批量升级选中账号]                           │
│ [自动注册 GoPay 账号]                        │
│ [添加 PayPal 账号]                           │
└─────────────────────────────────────────────┘
```

#### 4. 账号列表扩展

在现有账号列表添加列：

```
ID | 标签 | 邮箱 | 注册方式 | 手机号 | 订阅状态 | 过期时间 | 操作
```

批量操作添加：
- 升级到 Plus
- 检查有效性

---

## 七、安装部署

### 7.1 安装脚本扩展

`scripts/install.sh` 支持参数：

```bash
# 最小安装（仅 Go 网关）
sudo scripts/install.sh --minimal

# 完整安装（含所有生命周期管理）
sudo scripts/install.sh --full

# 自定义安装
sudo scripts/install.sh --with-registration --with-payment

# 一键安装所有（推荐）
sudo scripts/install.sh
```

### 7.2 systemd 服务

```bash
# 主服务
systemctl start codex-pool

# 可选服务（默认禁用）
systemctl enable codex-pool-register
systemctl enable codex-pool-payment
systemctl enable codex-pool-checkout

systemctl start codex-pool-register
systemctl start codex-pool-payment
systemctl start codex-pool-checkout
```

### 7.3 配置流程

1. 启动主服务
2. 访问管理面板
3. 在"系统设置"启用生命周期管理
4. 配置供应商（邮箱、SMS、支付）
5. 创建第一个注册任务

---

## 八、使用流程示例

### 场景 1：批量注册 Free 账号

```
1. 管理员登录面板
2. 供应商配置 → 添加 MoeMail + SmsBower
3. 账号注册 → 创建任务
   - 类型：仅注册
   - 数量：50
   - 分组：cyber
4. 启动任务
5. 系统自动执行，50 个账号入池
6. 在账号列表看到新账号（status: active, plan_type: free）
```

### 场景 2：注册并升级 Plus

```
1. 配置 GoPay 账号池（添加 5 个 GoPay 账号）
2. 账号注册 → 创建任务
   - 类型：注册 + 升级 Plus
   - 数量：10
   - 支付：GoPay
3. 启动任务
4. 系统自动：
   - 注册 10 个账号
   - 生成支付链接
   - GoPay 自动支付
   - 账号变为 Plus
5. 10 个 Plus 账号入池可用
```

### 场景 3：池内账号升级

```
1. 账号列表 → 筛选 Free 账号
2. 批量选择 20 个
3. 批量操作 → 升级到 Plus
4. 选择支付方式：PayPal日区
5. 系统自动升级
6. 20 个账号变为 Plus
```

---

## 九、安全与合规

### 9.1 权限控制

- 所有生命周期管理功能需管理员权限
- 普通用户只能使用账号，不能管理
- 敏感操作记录审计日志

### 9.2 数据安全

- 所有密码/Token 加密存储
- 敏感日志脱敏
- 支付账号信息加密

### 9.3 速率控制

- 注册并发数限制
- 支付请求限流
- 防止滥用

### 9.4 错误处理

- 失败自动重试（可配置）
- 详细错误日志
- 失败账号自动隔离

---

## 十、商业化特性

### 10.1 统计报表

- 每日注册成功率
- Plus 转化率
- 供应商性能对比
- 成本统计

### 10.2 预算控制

- 设置每日注册上限
- 支付预算控制
- 余额预警

### 10.3 多租户支持

- 不同租户独立账号池
- 分组隔离
- 配额管理

---

## 十一、技术优势

1. **模块化设计**：核心网关与生命周期管理解耦
2. **可选启用**：默认关闭，管理员选择启用
3. **混合架构**：Go 主服务 + Python 可选服务
4. **易于部署**：一键安装，systemd 管理
5. **可扩展**：支持添加新供应商、新支付方式
6. **监控完善**：实时日志、统计报表、错误追踪

---

## 十二、与三个项目的关系

| 项目 | 使用方式 | 整合位置 |
|-----|---------|----------|
| chatgpt-auto-register | 核心代码复用 | services/chatgpt_register/ |
| aBaiAutoplus | 架构参考 + 支付代码 | services/plus_payment/ + Go 架构 |
| GuJumpgate | checkout-converter 服务 | services/checkout_converter/ |

---

这个方案提供了**完整的商业化账号池管理能力**，管理员可以根据需求灵活选择启用哪些功能，同时保持系统的稳定性和安全性。
