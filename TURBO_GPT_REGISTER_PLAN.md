# Turbo GPT 自动化注册整合计划

## 项目分析总结

### 原项目核心流程 (codex-remote-registrar)

**技术栈：**
- Node.js + puppeteer-real-browser
- HeroSMS 接码平台
- Cloudflare Worker + D1 临时邮箱
- OAuth 2.0 PKCE 流程

**注册流程（3个阶段）：**

#### Phase 1: 手机号注册
1. 从 HeroSMS 购买手机号
2. 打开 auth.openai.com 注册页
3. 输入手机号，等待短信验证码
4. 提交验证码完成手机号注册
5. 保存账号到 `accounts.json`

#### Phase 1.5: 首次登录完善资料
1. 使用手机号登录
2. 填写个人信息（姓名、生日）
3. 完成 about-you 流程

#### Phase 2: 绑定临时邮箱
1. 使用手机号登录
2. 创建临时邮箱地址
3. 在 ChatGPT 设置中绑定邮箱
4. 接收邮箱验证码并验证
5. 保存到 `username.json`

#### Phase 3: OAuth 获取 Token
1. 使用邮箱登录
2. 触发 OAuth 授权流程
3. 获取 authorization code
4. 用 PKCE 交换 access_token + refresh_token
5. 保存完整 token JSON 到 `tokens/` 目录

**关键组件：**
- `src/smsProvider.js` - HeroSMS API 封装
- `src/mailProvider.js` - 临时邮箱 API 封装（支持多种后端）
- `src/browserService.js` - Puppeteer 浏览器自动化
- `src/oauthService.js` - OAuth PKCE 流程实现
- `src/yesCaptcha.js` - Turnstile 验证码绕过

---

## 整合方案设计

### 架构设计

```
pool_server/
├── internal/
│   └── turbo_gpt_register/          # 新模块（Go）
│       ├── orchestrator.go          # 任务编排器
│       ├── job.go                   # 单个任务状态机
│       ├── sms_client.go            # HeroSMS Go 客户端
│       ├── mail_client.go           # 临时邮箱 Go 客户端
│       ├── storage.go               # 任务持久化
│       └── models.go                # 数据模型
├── services/
│   └── turbo_gpt_register/          # Node.js 执行器
│       ├── package.json
│       ├── index.js                 # 主入口（简化版）
│       ├── src/
│       │   ├── browserService.js    # 浏览器自动化
│       │   ├── oauthService.js      # OAuth 流程
│       │   ├── smsProvider.js       # SMS 接口
│       │   ├── mailProvider.js      # 邮箱接口
│       │   └── config.js            # 配置加载
│       └── README.md
└── web-spa/
    └── src/
        └── pages/
            └── TurboGptRegister.jsx # 前端管理页面
```

---

## 实施步骤

### M1: 后端基础架构（Go）

#### M1.1 数据模型设计

**表：`turbo_gpt_register_jobs`**
```sql
CREATE TABLE turbo_gpt_register_jobs (
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,  -- pending/phase1/phase1_5/phase2/phase3/completed/failed
  phone TEXT,
  email TEXT,
  password TEXT,
  full_name TEXT,
  birth_date TEXT,
  phone_country_code TEXT,
  phone_country_dial_code TEXT,
  sms_operator TEXT,
  mail_domain TEXT,
  error TEXT,
  phase1_completed_at INTEGER,
  phase2_completed_at INTEGER,
  phase3_completed_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE turbo_gpt_register_tokens (
  job_id TEXT PRIMARY KEY,
  email TEXT NOT NULL,
  access_token TEXT NOT NULL,
  refresh_token TEXT NOT NULL,
  id_token TEXT,
  account_id TEXT,
  expires_at INTEGER NOT NULL,
  raw_json TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  FOREIGN KEY (job_id) REFERENCES turbo_gpt_register_jobs(id)
);

CREATE TABLE turbo_gpt_register_config (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
```

#### M1.2 Go 编排器实现

**文件：`internal/turbo_gpt_register/orchestrator.go`**

核心功能：
- 创建注册任务
- 调用 Node.js 子进程执行各阶段
- 监控任务状态
- 错误重试机制
- 结果持久化
- **SMS 平台切换策略（HeroSMS ↔ SMSbower）**

```go
package turbo_gpt_register

type Orchestrator struct {
    store *storage.Store
    nodeScriptPath string
    config Config
    smsClient SMSClient  // 支持多平台
}

func (o *Orchestrator) CreateJob(ctx context.Context) (*Job, error)
func (o *Orchestrator) RunPhase1(ctx context.Context, jobID string) error
func (o *Orchestrator) RunPhase2(ctx context.Context, jobID string) error
func (o *Orchestrator) RunPhase3(ctx context.Context, jobID string) error
func (o *Orchestrator) GetJobStatus(ctx context.Context, jobID string) (*Job, error)
func (o *Orchestrator) ListJobs(ctx context.Context, filter JobFilter) ([]*Job, error)
func (o *Orchestrator) GetBestSMSCountry(ctx context.Context, service string) (*SMSCountryOption, error)
```

#### M1.3 Admin API 端点

**文件：`internal/api/turbo_gpt_register.go`**

```go
// POST /admin/turbo-gpt-register/jobs
func (s *Server) adminTurboGptRegisterCreateJob(w http.ResponseWriter, r *http.Request)

// GET /admin/turbo-gpt-register/jobs
func (s *Server) adminTurboGptRegisterListJobs(w http.ResponseWriter, r *http.Request)

// GET /admin/turbo-gpt-register/jobs/:id
func (s *Server) adminTurboGptRegisterGetJob(w http.ResponseWriter, r *http.Request)

// POST /admin/turbo-gpt-register/jobs/:id/retry
func (s *Server) adminTurboGptRegisterRetryJob(w http.ResponseWriter, r *http.Request)

// POST /admin/turbo-gpt-register/jobs/:id/advance
func (s *Server) adminTurboGptRegisterAdvanceJob(w http.ResponseWriter, r *http.Request)

// DELETE /admin/turbo-gpt-register/jobs/:id
func (s *Server) adminTurboGptRegisterDeleteJob(w http.ResponseWriter, r *http.Request)

// GET /admin/turbo-gpt-register/config
// PUT /admin/turbo-gpt-register/config
```

---

### M2: Node.js 执行器整合

#### M2.1 简化原项目代码

**保留核心模块：**
- `src/browserService.js` - 浏览器自动化
- `src/oauthService.js` - OAuth 流程
- `src/smsProvider.js` - SMS 接口
- `src/mailProvider.js` - 邮箱接口
- `src/yesCaptcha.js` - 验证码处理

**删除/重构：**
- 移除批量循环逻辑（由 Go 编排器控制）
- 移除文件持久化（改为 stdout JSON 输出）
- 简化配置加载（从环境变量/stdin 读取）

#### M2.2 新的入口文件

**文件：`services/turbo_gpt_register/index.js`**

```javascript
// 接受命令行参数：
// node index.js phase1 --job-id=xxx
// node index.js phase2 --job-id=xxx --phone=xxx --password=xxx
// node index.js phase3 --job-id=xxx --email=xxx --password=xxx

async function main() {
  const phase = process.argv[2];
  const args = parseArgs(process.argv.slice(3));
  
  const config = loadConfigFromEnv();
  const result = {};
  
  try {
    switch (phase) {
      case 'phase1':
        result = await runPhase1(config, args);
        break;
      case 'phase2':
        result = await runPhase2(config, args);
        break;
      case 'phase3':
        result = await runPhase3(config, args);
        break;
      default:
        throw new Error(`Unknown phase: ${phase}`);
    }
    
    // 输出结果到 stdout（JSON）
    console.log(JSON.stringify({ success: true, data: result }));
    process.exit(0);
  } catch (error) {
    console.error(JSON.stringify({ success: false, error: error.message, stack: error.stack }));
    process.exit(1);
  }
}

main();
```

#### M2.3 环境变量配置

Go 编排器通过环境变量传递配置：

```bash
HERO_SMS_API_KEY=xxx
HERO_SMS_SERVICE=dr
HERO_SMS_COUNTRY=46
PHONE_COUNTRY_CODE=SE
MAIL_PROVIDER=cloudflare-worker
MAIL_BASE_URL=https://xxx.workers.dev
MAIL_ADMIN_TOKEN=xxx
MAIL_DOMAIN=xxx.com
PROXY_HOST=127.0.0.1
PROXY_PORT=7897
BROWSER_USER_DATA_DIR=/tmp/turbo-gpt-register/browser-profile
CHROME_PATH=/usr/bin/google-chrome-stable
REG_HEADLESS=0
```

---

### M3: 前端管理页面

#### M3.1 页面设计

**文件：`web-spa/src/pages/TurboGptRegister.jsx`**

**功能模块：**

1. **配置面板**
   - HeroSMS 配置（API Key、服务代码、国家）
   - 邮箱配置（Provider、域名、Token）
   - 代理配置
   - 浏览器配置

2. **任务列表**
   - 表格显示所有任务
   - 列：ID、状态、手机号、邮箱、阶段、创建时间、操作
   - 状态标签：pending/phase1/phase2/phase3/completed/failed
   - 操作按钮：查看详情、重试、继续、删除

3. **创建任务**
   - 批量创建（指定数量）
   - 单个创建

4. **任务详情弹窗**
   - 显示完整任务信息
   - 阶段进度条
   - 错误信息（如果失败）
   - Token 信息（如果完成）
   - 日志输出

5. **实时状态更新**
   - WebSocket 或轮询
   - 任务状态实时刷新

#### M3.2 UI 组件结构

```jsx
export default function TurboGptRegister() {
  const [jobs, setJobs] = useState([]);
  const [config, setConfig] = useState({});
  const [selectedJob, setSelectedJob] = useState(null);
  const [createModalOpen, setCreateModalOpen] = useState(false);
  
  // 配置管理
  const saveConfig = async (newConfig) => { ... };
  
  // 任务管理
  const createJobs = async (count) => { ... };
  const retryJob = async (jobId) => { ... };
  const advanceJob = async (jobId) => { ... };
  const deleteJob = async (jobId) => { ... };
  
  return (
    <div>
      <PageHeader title="GPT 邮箱自动化注册" />
      
      <ConfigPanel config={config} onSave={saveConfig} />
      
      <JobsTable 
        jobs={jobs}
        onRetry={retryJob}
        onAdvance={advanceJob}
        onDelete={deleteJob}
        onViewDetails={setSelectedJob}
      />
      
      <Button onClick={() => setCreateModalOpen(true)}>
        批量创建任务
      </Button>
      
      <JobDetailModal 
        job={selectedJob}
        onClose={() => setSelectedJob(null)}
      />
      
      <CreateJobModal
        open={createModalOpen}
        onClose={() => setCreateModalOpen(false)}
        onCreate={createJobs}
      />
    </div>
  );
}
```

---

### M4: 集成到现有账号系统

#### M4.1 自动导入已完成任务

**功能：**
- Phase 3 完成后自动调用 `saveImportedAccount`
- 将 token 导入到账号池
- 关联到指定分组

**实现位置：**
`internal/turbo_gpt_register/orchestrator.go`

```go
func (o *Orchestrator) onPhase3Complete(ctx context.Context, job *Job, tokenData TokenData) error {
    // 1. 保存 token 到 turbo_gpt_register_tokens
    if err := o.store.SaveToken(ctx, job.ID, tokenData); err != nil {
        return err
    }
    
    // 2. 调用现有导入逻辑
    parsed := authparse.ParsedAuth{
        Provider:     "codex",
        Email:        job.Email,
        AccessToken:  tokenData.AccessToken,
        RefreshToken: tokenData.RefreshToken,
        ExpiresAt:    tokenData.ExpiresAt,
    }
    
    _, err := o.server.saveImportedAccount(ctx, parsed, 
        job.Email,                    // label
        o.config.DefaultGroupName,    // group_name
        "",                           // note
        "",                           // workspace_id
        o.config.DefaultEgressID,     // egress_id
    )
    
    return err
}
```

#### M4.2 在账号列表显示来源

**修改：**
`web-spa/src/pages/Accounts.jsx`

添加来源标签：
- OAuth 导入
- 手动导入
- **Turbo GPT 注册** ← 新增

---

## 配置管理

### 配置存储

**使用现有 `turbo_gpt_register_config` 表：**

```go
type Config struct {
    // HeroSMS
    HeroSmsApiKey    string
    HeroSmsService   string // "dr"
    HeroSmsCountry   int    // 46
    PhoneCountryCode string // "SE"
    
    // Mail
    MailProvider   string // "cloudflare-worker"
    MailBaseUrl    string
    MailAdminToken string
    MailDomain     string
    MailDomains    []string
    
    // Proxy
    ProxyHost     string
    ProxyPort     int
    ProxyUsername string
    ProxyPassword string
    
    // Browser
    ChromePath         string
    BrowserUserDataDir string
    BrowserHeadless    bool
    
    // Integration
    DefaultGroupName string
    DefaultEgressID  string
    AutoImport       bool
}
```

### 前端配置表单

分组显示：
1. **短信接码配置**
2. **临时邮箱配置**
3. **代理配置**
4. **浏览器配置**
5. **集成配置**

---

## 错误处理与重试

### 错误分类

1. **可重试错误：**
   - 网络超时
   - 验证码接收超时
   - Cloudflare 挑战
   - 代理连接失败

2. **不可重试错误：**
   - 手机号已被使用
   - 邮箱已被使用
   - HeroSMS 余额不足
   - 配置错误

### 重试策略

```go
type RetryPolicy struct {
    MaxAttempts int
    InitialDelay time.Duration
    MaxDelay time.Duration
    Multiplier float64
}

func (o *Orchestrator) retryWithBackoff(ctx context.Context, fn func() error, policy RetryPolicy) error {
    var lastError error
    delay := policy.InitialDelay
    
    for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
        if err := fn(); err == nil {
            return nil
        } else {
            lastError = err
            if !isRetriable(err) {
                return err
            }
        }
        
        if attempt < policy.MaxAttempts {
            time.Sleep(delay)
            delay = time.Duration(float64(delay) * policy.Multiplier)
            if delay > policy.MaxDelay {
                delay = policy.MaxDelay
            }
        }
    }
    
    return fmt.Errorf("max retries exceeded: %w", lastError)
}
```

---

## 监控与日志

### 日志记录

1. **Go 编排器日志：**
   - 使用现有 `zerolog`
   - 记录任务状态变更
   - 记录 API 调用

2. **Node.js 执行器日志：**
   - 输出到 stderr
   - Go 捕获并保存到数据库
   - 前端可查看

### 指标统计

**新增指标：**
- 总任务数
- 成功率（按阶段）
- 平均完成时间
- HeroSMS 花费统计
- 失败原因分布

---

## 开发里程碑

### 第一阶段：基础架构（1-2天）
- [x] 数据库表设计
- [ ] Go 编排器骨架
- [ ] Admin API 端点
- [ ] Node.js 入口简化

### 第二阶段：核心功能（2-3天）
- [ ] Phase 1 实现（手机号注册）
- [ ] Phase 2 实现（邮箱绑定）
- [ ] Phase 3 实现（OAuth Token）
- [ ] 错误处理与重试

### 第三阶段：前端界面（1-2天）
- [ ] 配置管理页面
- [ ] 任务列表页面
- [ ] 任务详情弹窗
- [ ] 实时状态更新

### 第四阶段：集成与测试（1天）
- [ ] 自动导入到账号池
- [ ] 端到端测试
- [ ] 文档编写

---

## 技术风险与缓解

### 风险1：Puppeteer 在服务器环境的稳定性
**缓解：**
- 使用 `puppeteer-real-browser` 的 headless 模式
- 内存优化配置
- 浏览器实例复用
- 超时保护

### 风险2：Cloudflare Turnstile 绕过失败率
**缓解：**
- 集成 yesCaptcha API
- 多次重试机制
- 人工介入接口（前端手动验证）

### 风险3：HeroSMS 号码占用率高
**缓解：**
- 自动切换国家
- 支持多平台比价（hero-sms + smsbower）
- 占用号黑名单

### 风险4：OpenAI 反自动化检测
**缓解：**
- 真实浏览器指纹（puppeteer-real-browser）
- 代理轮换
- 操作随机延迟
- 限制并发数

---

## 后续优化方向

1. **批量并发控制：**
   - 限制同时运行的任务数
   - 避免 OpenAI 检测

2. **成本优化：**
   - HeroSMS 国家自动选择最低价
   - 失败任务快速止损

3. **多邮箱后端支持：**
   - Guerrilla Mail
   - 1secmail
   - 自建邮箱服务器

4. **Webhook 通知：**
   - 任务完成通知
   - 失败告警

5. **导出功能：**
   - 导出 token JSON
   - 导出到 other_sub2api 格式
   - 批量导出到账号池

---

## 总结

通过以上设计，我们将原 Node.js 项目的核心能力整合到 pool_server，形成一个完整的 GPT 自动化注册模块：

**优势：**
1. ✅ 统一管理界面
2. ✅ 自动导入账号池
3. ✅ 任务状态持久化
4. ✅ 错误重试机制
5. ✅ 配置集中管理
6. ✅ 与现有系统无缝集成

**技术要点：**
- Go 作为编排器和 API 层
- Node.js 作为浏览器自动化执行器
- 清晰的进程间通信（stdin/stdout/env）
- 数据库持久化任务状态
- 前端实时监控任务进度

**下一步行动：**
1. 创建数据库迁移文件
2. 实现 Go 编排器骨架
3. 简化 Node.js 入口
4. 搭建前端页面框架
