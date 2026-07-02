# 动态代理与指纹浏览器整合方案

## 需求分析

管理员需要为注册任务配置代理策略：

1. **固定 IP 代理**：每个账号使用同一个 IP
2. **动态 IP 代理**：每注册一个账号换一个新 IP
3. **指纹浏览器整合**：动态 IP 时，配合指纹浏览器隔离(只是学习指纹浏览器的思路他是怎么获取动态ip的)

---

## 一、代理类型定义

### 1.1 代理分类

```go
type ProxyType string

const (
    ProxyTypeStatic   ProxyType = "static"    // 固定 IP
    ProxyTypeDynamic  ProxyType = "dynamic"   // 动态 IP（每次提取新IP）
    ProxyTypeRotating ProxyType = "rotating"  // 旋转网关（自动轮换）
)

type ProxyProvider string

const (
    ProxyProviderHTTP      ProxyProvider = "http"        // HTTP代理
    ProxyProviderSocks5    ProxyProvider = "socks5"      // SOCKS5代理
    ProxyProviderAPI       ProxyProvider = "api_extract" // API提取代理
    ProxyProviderRotating  ProxyProvider = "rotating_gateway" // 旋转网关
)
```

### 1.2 数据库 Schema

```sql
-- 代理池配置表（扩展现有 egress_profiles）
CREATE TABLE IF NOT EXISTS proxy_configs (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    proxy_type TEXT NOT NULL,        -- static, dynamic, rotating
    proxy_provider TEXT NOT NULL,    -- http, socks5, api_extract, rotating_gateway
    
    -- 固定代理配置
    proxy_url TEXT,                  -- http://user:pass@host:port
    
    -- 动态代理配置（API提取）
    api_url TEXT,                    -- 提取API地址
    api_key TEXT,                    -- API Key
    api_params TEXT,                 -- JSON: {"region": "US", "protocol": "socks5"}
    
    -- 旋转网关配置
    gateway_url TEXT,                -- socks5://host:port （每次请求自动换IP）
    
    -- 指纹浏览器配置
    fingerprint_enabled BOOLEAN DEFAULT 0,
    fingerprint_mode TEXT,           -- none, per_account, per_task
    
    -- 统计
    total_used INTEGER DEFAULT 0,
    success_count INTEGER DEFAULT 0,
    fail_count INTEGER DEFAULT 0,
    last_used_at INTEGER,
    
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- 代理使用记录
CREATE TABLE IF NOT EXISTS proxy_usage_records (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    proxy_config_id TEXT NOT NULL,
    account_id TEXT,
    task_id TEXT,
    extracted_ip TEXT,               -- 提取到的实际IP
    used_at INTEGER NOT NULL,
    success BOOLEAN,
    error_message TEXT,
    FOREIGN KEY (proxy_config_id) REFERENCES proxy_configs(id)
);
```

---

## 二、动态代理实现

### 2.1 API 提取代理

支持主流代理商的 API：

```go
// internal/proxy/extractor.go

type ProxyExtractor interface {
    Extract(ctx context.Context) (*ExtractedProxy, error)
}

type ExtractedProxy struct {
    ProxyURL  string    // socks5://user:pass@ip:port
    IP        string    // 实际IP
    ExpiresAt time.Time // 过期时间
}

// Luminati / BrightData
type LuminatiExtractor struct {
    Username string
    Password string
    Zone     string
}

func (e *LuminatiExtractor) Extract(ctx context.Context) (*ExtractedProxy, error) {
    // 格式: http://user-zone-{zone}:pass@host:port
    // 每次连接自动换IP
    proxyURL := fmt.Sprintf("http://%s-zone-%s:%s@zproxy.lum-superproxy.io:22225",
        e.Username, e.Zone, e.Password)
    
    // 获取实际出口IP
    ip, err := e.checkIP(proxyURL)
    if err != nil {
        return nil, err
    }
    
    return &ExtractedProxy{
        ProxyURL:  proxyURL,
        IP:        ip,
        ExpiresAt: time.Now().Add(10 * time.Minute),
    }, nil
}

// 自定义API提取
type APIExtractor struct {
    APIURL string
    APIKey string
    Params map[string]string
}

func (e *APIExtractor) Extract(ctx context.Context) (*ExtractedProxy, error) {
    // 调用提取API
    req, _ := http.NewRequestWithContext(ctx, "GET", e.APIURL, nil)
    q := req.URL.Query()
    q.Set("key", e.APIKey)
    for k, v := range e.Params {
        q.Set(k, v)
    }
    req.URL.RawQuery = q.Encode()
    
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    
    var result struct {
        ProxyURL string `json:"proxy"`
        IP       string `json:"ip"`
        Expire   int64  `json:"expire"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, err
    }
    
    return &ExtractedProxy{
        ProxyURL:  result.ProxyURL,
        IP:        result.IP,
        ExpiresAt: time.Unix(result.Expire, 0),
    }, nil
}
```

### 2.2 代理管理器

```go
// internal/proxy/manager.go

type ProxyManager struct {
    store    *storage.Store
    cache    sync.Map // proxyConfigID -> *ExtractedProxy
    mu       sync.RWMutex
}

func (m *ProxyManager) GetProxy(configID string, forceNew bool) (*ExtractedProxy, error) {
    config, err := m.store.GetProxyConfig(configID)
    if err != nil {
        return nil, err
    }
    
    switch config.ProxyType {
    case "static":
        // 固定代理，直接返回
        return &ExtractedProxy{
            ProxyURL: config.ProxyURL,
        }, nil
        
    case "dynamic":
        // 动态代理，每次提取新IP
        if !forceNew {
            // 检查缓存
            if cached, ok := m.cache.Load(configID); ok {
                proxy := cached.(*ExtractedProxy)
                if time.Now().Before(proxy.ExpiresAt) {
                    return proxy, nil
                }
            }
        }
        
        // 提取新代理
        extractor := m.createExtractor(config)
        proxy, err := extractor.Extract(context.Background())
        if err != nil {
            return nil, err
        }
        
        // 缓存
        if !forceNew {
            m.cache.Store(configID, proxy)
        }
        
        // 记录使用
        m.recordUsage(configID, proxy)
        
        return proxy, nil
        
    case "rotating":
        // 旋转网关，返回网关地址（每次请求自动换IP）
        return &ExtractedProxy{
            ProxyURL: config.GatewayURL,
        }, nil
        
    default:
        return nil, fmt.Errorf("unknown proxy type: %s", config.ProxyType)
    }
}

func (m *ProxyManager) createExtractor(config *ProxyConfig) ProxyExtractor {
    switch config.ProxyProvider {
    case "api_extract":
        params := make(map[string]string)
        json.Unmarshal([]byte(config.APIParams), &params)
        return &APIExtractor{
            APIURL: config.APIURL,
            APIKey: config.APIKey,
            Params: params,
        }
    // ... 其他类型
    default:
        return nil
    }
}
```

---

## 三、指纹浏览器整合

### 3.1 指纹隔离策略

```go
type FingerprintMode string

const (
    FingerprintNone       FingerprintMode = "none"        // 不使用指纹
    FingerprintPerAccount FingerprintMode = "per_account" // 每账号一个指纹
    FingerprintPerTask    FingerprintMode = "per_task"    // 每任务一个指纹
)

type FingerprintProfile struct {
    UserAgent     string
    Platform      string
    Language      string
    Resolution    string
    Timezone      string
    WebGL         string
    Canvas        string
    AudioContext  string
}
```

### 3.2 指纹生成器

```go
// internal/fingerprint/generator.go

type FingerprintGenerator struct {
    seed int64
}

func (g *FingerprintGenerator) Generate() *FingerprintProfile {
    r := rand.New(rand.NewSource(g.seed))
    
    // 随机生成浏览器指纹
    userAgents := []string{
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
        // ... 更多
    }
    
    return &FingerprintProfile{
        UserAgent:  userAgents[r.Intn(len(userAgents))],
        Platform:   "Win32",
        Language:   "en-US,en;q=0.9",
        Resolution: "1920x1080",
        Timezone:   "America/New_York",
        // ... 更多指纹参数
    }
}
```

### 3.3 与注册服务整合

```go
// internal/registration/orchestrator.go

func (o *Orchestrator) processOneAccount(task *Task, index int) *Result {
    // 1. 获取代理
    proxy, err := o.proxyManager.GetProxy(
        task.Config.ProxyConfigID,
        task.Config.ProxyType == "dynamic", // 动态代理强制提取新IP
    )
    if err != nil {
        return &Result{Success: false, Error: err}
    }
    
    // 2. 生成指纹（如果启用）
    var fingerprint *FingerprintProfile
    if task.Config.FingerprintEnabled {
        generator := &FingerprintGenerator{seed: time.Now().UnixNano() + int64(index)}
        fingerprint = generator.Generate()
    }
    
    // 3. 调用注册服务
    regResult, err := o.registrationClient.Register(RegisterRequest{
        ProxyURL:    proxy.ProxyURL,
        Fingerprint: fingerprint,
        // ... 其他参数
    })
    
    return regResult
}
```

---

## 四、管理面板实现

### 4.1 代理配置页面

```html
<!-- /admin/proxy-configs -->

<div class="proxy-config-page">
  <h2>代理配置</h2>
  
  <!-- 代理列表 -->
  <table>
    <thead>
      <tr>
        <th>名称</th>
        <th>类型</th>
        <th>提供商</th>
        <th>使用次数</th>
        <th>成功率</th>
        <th>操作</th>
      </tr>
    </thead>
    <tbody>
      <tr v-for="proxy in proxies">
        <td>{{ proxy.name }}</td>
        <td>
          <span v-if="proxy.proxy_type === 'static'">固定IP</span>
          <span v-if="proxy.proxy_type === 'dynamic'">动态IP</span>
          <span v-if="proxy.proxy_type === 'rotating'">旋转网关</span>
        </td>
        <td>{{ proxy.proxy_provider }}</td>
        <td>{{ proxy.total_used }}</td>
        <td>{{ (proxy.success_count / proxy.total_used * 100).toFixed(1) }}%</td>
        <td>
          <button @click="editProxy(proxy)">编辑</button>
          <button @click="testProxy(proxy)">测试</button>
          <button @click="deleteProxy(proxy)">删除</button>
        </td>
      </tr>
    </tbody>
  </table>
  
  <!-- 添加代理按钮 -->
  <button @click="showAddDialog = true">+ 添加代理配置</button>
  
  <!-- 添加/编辑对话框 -->
  <dialog v-if="showAddDialog">
    <h3>{{ editingProxy ? '编辑' : '添加' }}代理配置</h3>
    
    <form @submit="saveProxy">
      <!-- 基本信息 -->
      <label>名称：<input v-model="form.name" required></label>
      
      <!-- 代理类型 -->
      <label>代理类型：
        <select v-model="form.proxy_type" @change="onProxyTypeChange">
          <option value="static">固定IP代理</option>
          <option value="dynamic">动态IP代理（API提取）</option>
          <option value="rotating">旋转网关代理</option>
        </select>
      </label>
      
      <!-- 固定代理配置 -->
      <div v-if="form.proxy_type === 'static'">
        <label>代理地址：
          <input v-model="form.proxy_url" 
                 placeholder="http://user:pass@host:port"
                 required>
        </label>
        <p class="hint">支持 http://, https://, socks5://, socks5h://</p>
      </div>
      
      <!-- 动态代理配置 -->
      <div v-if="form.proxy_type === 'dynamic'">
        <label>API地址：
          <input v-model="form.api_url" 
                 placeholder="https://api.proxy.com/extract"
                 required>
        </label>
        
        <label>API Key：
          <input v-model="form.api_key" 
                 type="password"
                 required>
        </label>
        
        <label>API参数（JSON）：
          <textarea v-model="form.api_params" 
                    placeholder='{"region": "US", "protocol": "socks5"}'></textarea>
        </label>
        
        <p class="hint">每注册一个账号提取一个新IP</p>
      </div>
      
      <!-- 旋转网关配置 -->
      <div v-if="form.proxy_type === 'rotating'">
        <label>网关地址：
          <input v-model="form.gateway_url" 
                 placeholder="socks5://user:pass@gateway:port"
                 required>
        </label>
        <p class="hint">每次请求自动轮换IP，无需提取</p>
      </div>
      
      <!-- 指纹浏览器配置 -->
      <fieldset>
        <legend>指纹浏览器</legend>
        
        <label>
          <input type="checkbox" v-model="form.fingerprint_enabled">
          启用指纹隔离
        </label>
        
        <div v-if="form.fingerprint_enabled">
          <label>隔离模式：
            <select v-model="form.fingerprint_mode">
              <option value="per_account">每账号独立指纹</option>
              <option value="per_task">每任务共享指纹</option>
            </select>
          </label>
          
          <p class="hint">
            <strong>每账号独立：</strong>每个账号生成不同的浏览器指纹<br>
            <strong>每任务共享：</strong>同一任务的所有账号使用相同指纹
          </p>
        </div>
      </fieldset>
      
      <!-- 按钮 -->
      <div class="buttons">
        <button type="button" @click="showAddDialog = false">取消</button>
        <button type="submit">保存</button>
      </div>
    </form>
  </dialog>
</div>
```

### 4.2 任务创建时选择代理

```html
<!-- 创建注册任务对话框中添加 -->

<fieldset>
  <legend>代理配置</legend>
  
  <label>选择代理：
    <select v-model="taskForm.proxy_config_id">
      <option value="">不使用代理（直连）</option>
      <option v-for="proxy in proxyConfigs" :value="proxy.id">
        {{ proxy.name }} 
        ({{ proxy.proxy_type === 'static' ? '固定' : '动态' }})
        - 成功率 {{ proxy.success_rate }}%
      </option>
    </select>
  </label>
  
  <div v-if="selectedProxy">
    <p class="proxy-info">
      <strong>类型：</strong>
      <span v-if="selectedProxy.proxy_type === 'static'">
        固定IP - 所有账号使用同一IP
      </span>
      <span v-if="selectedProxy.proxy_type === 'dynamic'">
        动态IP - 每个账号自动提取新IP
      </span>
      <span v-if="selectedProxy.proxy_type === 'rotating'">
        旋转网关 - 每次请求自动换IP
      </span>
    </p>
    
    <p v-if="selectedProxy.fingerprint_enabled" class="fingerprint-info">
      ✓ 已启用指纹隔离 
      ({{ selectedProxy.fingerprint_mode === 'per_account' ? '每账号独立' : '每任务共享' }})
    </p>
  </div>
</fieldset>
```

---

## 五、API 端点

```go
// internal/api/admin_proxy.go

// GET /admin/proxy-configs
// 列出所有代理配置
func (h *Handler) ListProxyConfigs(c *gin.Context) {
    configs, err := h.store.ListProxyConfigs()
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    c.JSON(200, configs)
}

// POST /admin/proxy-configs
// 创建代理配置
func (h *Handler) CreateProxyConfig(c *gin.Context) {
    var req struct {
        Name                string `json:"name"`
        ProxyType           string `json:"proxy_type"`
        ProxyProvider       string `json:"proxy_provider"`
        ProxyURL            string `json:"proxy_url"`
        APIURL              string `json:"api_url"`
        APIKey              string `json:"api_key"`
        APIParams           string `json:"api_params"`
        GatewayURL          string `json:"gateway_url"`
        FingerprintEnabled  bool   `json:"fingerprint_enabled"`
        FingerprintMode     string `json:"fingerprint_mode"`
    }
    
    if err := c.BindJSON(&req); err != nil {
        c.JSON(400, gin.H{"error": err.Error()})
        return
    }
    
    config := &ProxyConfig{
        ID:                  generateID(),
        Name:                req.Name,
        ProxyType:           req.ProxyType,
        ProxyProvider:       req.ProxyProvider,
        ProxyURL:            req.ProxyURL,
        APIURL:              req.APIURL,
        APIKey:              req.APIKey,
        APIParams:           req.APIParams,
        GatewayURL:          req.GatewayURL,
        FingerprintEnabled:  req.FingerprintEnabled,
        FingerprintMode:     req.FingerprintMode,
        CreatedAt:           time.Now().Unix(),
        UpdatedAt:           time.Now().Unix(),
    }
    
    if err := h.store.InsertProxyConfig(config); err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    
    c.JSON(200, config)
}

// POST /admin/proxy-configs/:id/test
// 测试代理配置
func (h *Handler) TestProxyConfig(c *gin.Context) {
    configID := c.Param("id")
    
    // 提取代理
    proxy, err := h.proxyManager.GetProxy(configID, true)
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    
    // 测试连接
    ip, err := h.checkProxyIP(proxy.ProxyURL)
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    
    c.JSON(200, gin.H{
        "ok":        true,
        "proxy_url": maskProxyURL(proxy.ProxyURL),
        "ip":        ip,
        "message":   "代理连接成功",
    })
}

// GET /admin/proxy-configs/:id/stats
// 查询代理使用统计
func (h *Handler) GetProxyStats(c *gin.Context) {
    configID := c.Param("id")
    
    stats, err := h.store.GetProxyStats(configID)
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()})
        return
    }
    
    c.JSON(200, stats)
}
```

---

## 六、使用示例

### 场景 1：固定 IP 注册

```
1. 管理员添加代理配置：
   - 名称：US-Fixed-1
   - 类型：固定IP
   - 地址：socks5://user:pass@proxy.com:1080

2. 创建注册任务：
   - 数量：10
   - 代理：US-Fixed-1
   
3. 系统执行：
   - 所有10个账号使用同一个IP注册
```

### 场景 2：动态 IP 注册

```
1. 管理员添加代理配置：
   - 名称：Luminati-US
   - 类型：动态IP（API提取）
   - API: https://api.luminati.io/extract
   - API Key: xxx
   - 参数：{"country": "US"}
   - 指纹隔离：启用（每账号独立）

2. 创建注册任务：
   - 数量：10
   - 代理：Luminati-US
   
3. 系统执行：
   - 注册账号1：提取IP_1，生成指纹_1
   - 注册账号2：提取IP_2，生成指纹_2
   - ...
   - 注册账号10：提取IP_10，生成指纹_10
```

### 场景 3：旋转网关注册

```
1. 管理员添加代理配置：
   - 名称：BrightData-Rotating
   - 类型：旋转网关
   - 网关：socks5://user:pass@gateway.brightdata.com:1080

2. 创建注册任务：
   - 数量：10
   - 代理：BrightData-Rotating
   
3. 系统执行：
   - 所有账号使用同一网关地址
   - 网关自动为每次请求分配不同IP
```

---

## 七、Python 服务适配

注册服务需要支持代理和指纹：

```python
# services/chatgpt_register/register_service.py

@app.route('/register', methods=['POST'])
def register_account():
    data = request.json
    
    # 代理配置
    proxy = data.get('proxy')
    
    # 指纹配置
    fingerprint = data.get('fingerprint')
    
    # 初始化注册器
    register = ChatGPTRegister(
        proxy=proxy,
        user_agent=fingerprint.get('user_agent') if fingerprint else None,
        # ... 其他指纹参数
    )
    
    result = register.register_one(sms)
    return jsonify(result)
```

---

## 八、总结

这个方案提供了：

✅ **灵活的代理策略**
- 固定IP、动态IP、旋转网关三种模式
- 支持主流代理提供商

✅ **指纹浏览器整合**
- 每账号独立指纹
- 每任务共享指纹
- 自动生成随机指纹

✅ **易于管理**
- 图形化配置界面
- 一键测试代理
- 使用统计和成功率追踪

✅ **反检测增强**
- IP + 指纹 双重隔离
- 动态IP确保每账号独立
- 降低批量注册风险

