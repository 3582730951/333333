# ChatGPT Team 空间管理功能设计方案

## 📋 需求分析

### 核心需求
1. **母号管理子号**: 母账号拉取子账号进入Team空间
2. **额度监控**: 监控子号的Codex额度使用情况
3. **自动轮换**: 子号额度用光后自动踢出，拉入新子号
4. **OAuth登录**: 获取Codex OAuth token用于API调用
5. **接码支持**: 支持自动接收验证码
6. **令牌模式**: 支持直接使用令牌登录（应对风控）

---

## 🔍 现有技术分析

### 搜索结果总结
基于我的搜索，ChatGPT Team的管理功能有以下限制：

1. **官方API限制**: 
   - OpenAI没有提供公开的Team成员管理API
   - 需要通过Web界面或内部API进行操作

2. **可行的技术方案**:
   - 使用浏览器自动化（Puppeteer/Playwright）模拟操作
   - 逆向工程ChatGPT内部API
   - 使用Cookie/Token进行认证

---

## 🏗️ 架构设计

### 1. 数据库结构

```sql
-- Team空间管理表
CREATE TABLE IF NOT EXISTS team_workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  parent_account_id TEXT NOT NULL,  -- 母账号ID
  workspace_id TEXT NOT NULL,        -- Team工作区ID
  max_members INTEGER DEFAULT 10,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Team成员管理表
CREATE TABLE IF NOT EXISTS team_members (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  account_id TEXT NOT NULL,          -- 子账号ID
  email TEXT NOT NULL,
  invite_status TEXT NOT NULL,       -- pending/active/removed
  codex_quota_used INTEGER DEFAULT 0,
  codex_quota_limit INTEGER DEFAULT 0,
  last_activity_at INTEGER,
  added_at INTEGER NOT NULL,
  removed_at INTEGER,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id),
  FOREIGN KEY(account_id) REFERENCES accounts(id)
);

-- 子账号池表
CREATE TABLE IF NOT EXISTS child_account_pool (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL,
  password TEXT,                     -- 加密存储
  oauth_token TEXT,                  -- OAuth token
  oauth_refresh_token TEXT,
  token_expires_at INTEGER,
  status TEXT NOT NULL,              -- available/in_use/quota_exhausted/banned
  sms_receive_method TEXT,           -- auto/manual
  sms_api_config TEXT,               -- 接码平台配置JSON
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 轮换历史表
CREATE TABLE IF NOT EXISTS member_rotation_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL,
  removed_account_id TEXT,
  removed_reason TEXT,
  added_account_id TEXT,
  success INTEGER NOT NULL,
  error_message TEXT,
  created_at INTEGER NOT NULL
);
```

### 2. 核心模块

#### 2.1 Team管理器 (TeamManager)
```go
type TeamManager struct {
    store     *storage.Store
    browser   *BrowserAutomation
    smsClient *SMSReceiver
}

// 创建Team空间
func (tm *TeamManager) CreateWorkspace(parentAccountID, name string) (*TeamWorkspace, error)

// 添加成员
func (tm *TeamManager) AddMember(workspaceID, childAccountID string) error

// 移除成员
func (tm *TeamManager) RemoveMember(workspaceID, memberID string) error

// 监控额度
func (tm *TeamManager) MonitorQuota(workspaceID string) error

// 自动轮换
func (tm *TeamManager) AutoRotate(workspaceID string) error
```

#### 2.2 浏览器自动化 (BrowserAutomation)
```go
type BrowserAutomation struct {
    browser *rod.Browser
}

// 登录ChatGPT
func (ba *BrowserAutomation) Login(email, password string, smsReceiver *SMSReceiver) error

// 邀请成员
func (ba *BrowserAutomation) InviteMember(workspaceID, email string) error

// 移除成员
func (ba *BrowserAutomation) KickMember(workspaceID, memberID string) error

// 获取OAuth Token
func (ba *BrowserAutomation) GetOAuthToken() (string, error)

// 检查额度
func (ba *BrowserAutomation) CheckQuota(accountID string) (used, limit int, error)
```

#### 2.3 接码服务 (SMSReceiver)
```go
type SMSReceiver struct {
    provider string  // sms-activate, 5sim, etc.
    apiKey   string
}

// 获取手机号
func (sr *SMSReceiver) GetPhoneNumber(country string) (string, error)

// 接收验证码
func (sr *SMSReceiver) ReceiveCode(phoneNumber string, timeout time.Duration) (string, error)

// 释放号码
func (sr *SMSReceiver) ReleaseNumber(phoneNumber string) error
```

#### 2.4 令牌管理器 (TokenManager)
```go
type TokenManager struct {
    store *storage.Store
}

// 刷新Token
func (tm *TokenManager) RefreshToken(accountID string) error

// 验证Token
func (tm *TokenManager) ValidateToken(token string) (bool, error)

// 检测风控
func (tm *TokenManager) DetectRiskControl(accountID string) (bool, error)
```

---

## 🔧 实现步骤

### Phase 1: 基础架构 (Week 1)
1. ✅ 设计数据库结构
2. ⬜ 实现数据库迁移
3. ⬜ 创建基础的Team管理器接口

### Phase 2: 浏览器自动化 (Week 2)
1. ⬜ 集成rod/playwright
2. ⬜ 实现登录流程（含2FA）
3. ⬜ 实现邀请/移除成员功能
4. ⬜ 实现OAuth token提取

### Phase 3: 接码集成 (Week 3)
1. ⬜ 集成常见接码平台API
2. ⬜ 实现自动接收验证码
3. ⬜ 添加号码池管理

### Phase 4: 额度监控 (Week 4)
1. ⬜ 实现额度查询接口
2. ⬜ 添加定时监控任务
3. ⬜ 实现告警机制

### Phase 5: 自动轮换 (Week 5)
1. ⬜ 实现轮换策略
2. ⬜ 添加轮换日志
3. ⬜ 实现失败重试

### Phase 6: 管理界面 (Week 6)
1. ⬜ 添加Team管理API端点
2. ⬜ 创建管理界面
3. ⬜ 添加监控仪表板

---

## 🛡️ 风控应对策略

### 1. Token被风控
- **检测**: 定期验证token有效性
- **应对**: 自动切换到浏览器模式
- **备份**: 保持多个活跃token

### 2. 账号被封
- **检测**: 监控登录失败率
- **应对**: 立即移出pool并标记
- **预防**: 限制单账号操作频率

### 3. 验证码挑战
- **应对**: 使用接码平台
- **备份**: 人工介入接口
- **优化**: 保持活跃session减少验证

---

## 📊 监控指标

### 关键指标
1. **成员状态**
   - 活跃成员数
   - 待添加成员数
   - 已移除成员数

2. **额度使用**
   - 总额度
   - 已使用额度
   - 剩余额度
   - 使用趋势

3. **轮换效率**
   - 轮换成功率
   - 平均轮换时间
   - 失败重试次数

4. **账号健康**
   - 可用账号数
   - 被封账号数
   - Token有效率

---

## 🚀 API设计

### Team管理API

```http
# 创建Team空间
POST /admin/team/workspaces
{
  "name": "My Team",
  "parent_account_id": "acc_xxx",
  "max_members": 10
}

# 添加成员
POST /admin/team/workspaces/{workspace_id}/members
{
  "account_id": "acc_yyy"
}

# 移除成员
DELETE /admin/team/workspaces/{workspace_id}/members/{member_id}

# 监控额度
GET /admin/team/workspaces/{workspace_id}/quota

# 触发轮换
POST /admin/team/workspaces/{workspace_id}/rotate

# 获取轮换历史
GET /admin/team/workspaces/{workspace_id}/rotation-log
```

### 子账号池API

```http
# 添加子账号
POST /admin/child-accounts
{
  "email": "user@example.com",
  "password": "encrypted_password",
  "sms_receive_method": "auto"
}

# 获取可用账号
GET /admin/child-accounts?status=available

# 更新账号状态
PATCH /admin/child-accounts/{account_id}
{
  "status": "quota_exhausted"
}
```

---

## ⚠️ 注意事项

### 法律和道德
1. **合规性**: 确保符合OpenAI服务条款
2. **隐私保护**: 妥善保管账号凭证
3. **使用限制**: 遵守使用配额限制

### 技术风险
1. **API变化**: OpenAI可能随时更改内部API
2. **反爬虫**: 需要应对各种反自动化措施
3. **封号风险**: 频繁操作可能导致账号被封

### 运维建议
1. **备份**: 定期备份账号数据
2. **监控**: 实时监控系统运行状态
3. **告警**: 设置关键事件告警

---

## 📝 下一步行动

1. **Review此设计方案**
2. **实现数据库迁移**
3. **开发浏览器自动化模块**
4. **集成接码平台**
5. **测试完整流程**

---

**创建时间**: 2026-07-28
**状态**: 设计阶段
**优先级**: 高
