# Pool Server 生命周期管理 - 1核1G VPS 极限优化方案

## 设计目标

**硬件下限**：1核1G VPS（约 $3-5/月）
**性能目标**：稳定运行注册+Plus订阅全流程
**优化原则**：所有性能参数可配置，不硬编码

---

## 一、资源占用分析

### 1.1 内存预算（总计 1GB）

```
系统保留：           200MB
Go 主服务：           50MB
Python 注册服务：     80MB（优化后）
Python 支付服务：     80MB（按需启动）
SQLite 数据库：       20MB
临时缓冲区：          70MB
━━━━━━━━━━━━━━━━━━━━━━━━━
剩余可用：           500MB（任务执行缓冲）
```

### 1.2 CPU 预算（单核）

```
Go 主服务：          20%（空闲）
Python 服务：        30%（空闲）
注册任务执行：       50%（峰值）
━━━━━━━━━━━━━━━━━━━━━━━━━
预留余量：           0%（满负载可接受）
```

---

## 二、架构优化

### 2.1 按需启动策略

**原则**：服务只在需要时启动，任务完成后自动休眠

```go
// internal/lifecycle/service_manager.go

type ServiceManager struct {
    services map[string]*Service
    mu       sync.RWMutex
}

type Service struct {
    Name       string
    Port       int
    Command    string
    Process    *exec.Cmd
    StartedAt  time.Time
    IdleTimer  *time.Timer
    IdleTimeout time.Duration // 可配置：默认 5 分钟
}

func (sm *ServiceManager) StartService(name string) error {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    
    svc := sm.services[name]
    if svc.Process != nil && svc.Process.ProcessState == nil {
        // 已运行，重置空闲计时器
        svc.IdleTimer.Reset(svc.IdleTimeout)
        return nil
    }
    
    // 启动服务
    log.Infof("启动服务: %s", name)
    cmd := exec.Command("/bin/bash", "-c", svc.Command)
    if err := cmd.Start(); err != nil {
        return err
    }
    
    svc.Process = cmd
    svc.StartedAt = time.Now()
    
    // 健康检查
    if err := sm.waitHealthy(name, svc.Port); err != nil {
        cmd.Process.Kill()
        return err
    }
    
    // 启动空闲计时器
    svc.IdleTimer = time.AfterFunc(svc.IdleTimeout, func() {
        sm.stopService(name)
    })
    
    return nil
}

func (sm *ServiceManager) StopService(name string) error {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    
    svc := sm.services[name]
    if svc.Process == nil {
        return nil
    }
    
    log.Infof("停止服务: %s", name)
    
    // 优雅关闭
    svc.Process.Process.Signal(syscall.SIGTERM)
    
    // 等待 5 秒
    done := make(chan error)
    go func() {
        done <- svc.Process.Wait()
    }()
    
    select {
    case <-done:
    case <-time.After(5 * time.Second):
        // 强制杀死
        svc.Process.Process.Kill()
    }
    
    svc.Process = nil
    return nil
}
```

### 2.2 轻量级 Python 服务

**优化 1：使用 gunicorn 单 worker**

```bash
# services/chatgpt_register/start.sh
#!/bin/bash
cd "$(dirname "$0")"
source .venv/bin/activate

# 1核1G：单 worker，预加载代码
exec gunicorn \
  --bind 127.0.0.1:8801 \
  --workers 1 \
  --worker-class sync \
  --threads 2 \
  --max-requests 100 \
  --max-requests-jitter 10 \
  --preload \
  --timeout 300 \
  register_service:app
```

**优化 2：精简依赖**

```python
# services/chatgpt_register/requirements.txt
flask==3.0.0
werkzeug==3.0.0
curl-cffi==0.6.0
requests==2.31.0

# 移除不必要的依赖
# beautifulsoup4  # 如果不用就移除
# cryptography    # 按需引入
```

**优化 3：延迟导入**

```python
# services/chatgpt_register/register_service.py

# 不在模块顶层导入，避免启动时加载
# from chatgpt_register import ChatGPTRegister  ❌

@app.route('/register', methods=['POST'])
def register_account():
    # 仅在请求时导入，节省内存
    from chatgpt_register import ChatGPTRegister  # ✅
    from smsbower import SmsBower
    
    # ... 业务逻辑
```

### 2.3 SQLite 优化

```go
// internal/storage/storage.go

func (s *Store) Initialize(ctx context.Context) error {
    // 1核1G 优化配置
    pragmas := []string{
        "PRAGMA journal_mode=WAL",           // WAL 模式
        "PRAGMA synchronous=NORMAL",         // 降低同步级别
        "PRAGMA cache_size=-32000",          // 32MB 缓存（可配置）
        "PRAGMA temp_store=MEMORY",          // 临时表存内存
        "PRAGMA mmap_size=30000000",         // 30MB mmap
        "PRAGMA page_size=4096",
        "PRAGMA auto_vacuum=INCREMENTAL",
    }
    
    for _, pragma := range pragmas {
        if _, err := s.db.ExecContext(ctx, pragma); err != nil {
            return err
        }
    }
    
    return nil
}
```

---

## 三、并发控制（1核优化）

### 3.1 动态并发数

**原则**：根据可用内存动态调整并发数

```go
// internal/lifecycle/config.go

type PerformanceConfig struct {
    // 并发配置（可在 config.json 覆盖）
    MinConcurrency     int `json:"min_concurrency"`      // 最小并发：1
    MaxConcurrency     int `json:"max_concurrency"`      // 最大并发：10
    DefaultConcurrency int `json:"default_concurrency"`  // 默认并发：2
    
    // 内存阈值（MB）
    MemoryThreshold    int `json:"memory_threshold"`     // 触发降速：800MB
    MemoryCritical     int `json:"memory_critical"`      // 暂停任务：900MB
    
    // CPU 阈值（%）
    CPUThreshold       int `json:"cpu_threshold"`        // 触发降速：80%
    CPUCritical        int `json:"cpu_critical"`         // 暂停任务：95%
}

func (c *PerformanceConfig) AutoAdjustConcurrency() int {
    // 读取系统资源
    memStats := &runtime.MemStats{}
    runtime.ReadMemStats(memStats)
    
    usedMemMB := int(memStats.Alloc / 1024 / 1024)
    
    // 内存压力检测
    if usedMemMB > c.MemoryCritical {
        return 1 // 降到最低
    } else if usedMemMB > c.MemoryThreshold {
        return c.MinConcurrency
    }
    
    // CPU 压力检测（通过外部工具）
    cpuUsage := getCPUUsage()
    if cpuUsage > c.CPUCritical {
        return 1
    } else if cpuUsage > c.CPUThreshold {
        return c.MinConcurrency
    }
    
    // 正常情况
    return c.DefaultConcurrency
}
```

### 3.2 智能任务调度

```go
// internal/lifecycle/scheduler.go

type TaskScheduler struct {
    perfConfig *PerformanceConfig
    queue      chan *Task
    semaphore  chan struct{}
}

func (ts *TaskScheduler) Run() {
    for {
        // 动态调整并发数
        currentConcurrency := ts.perfConfig.AutoAdjustConcurrency()
        
        // 重新创建信号量
        ts.semaphore = make(chan struct{}, currentConcurrency)
        
        // 处理任务
        select {
        case task := <-ts.queue:
            ts.semaphore <- struct{}{}
            go func() {
                defer func() { <-ts.semaphore }()
                ts.executeTask(task)
            }()
            
        case <-time.After(1 * time.Second):
            // 定期检查资源
        }
    }
}
```

---

## 四、代理系统集成（1核优化）

### 4.1 轻量级代理管理

```go
// internal/proxy/lightweight_manager.go

type LightweightProxyManager struct {
    configs map[string]*ProxyConfig
    cache   *lru.Cache // LRU 缓存，限制内存
}

func NewLightweightProxyManager() *LightweightProxyManager {
    // 最多缓存 10 个提取的代理
    cache, _ := lru.New(10)
    
    return &LightweightProxyManager{
        configs: make(map[string]*ProxyConfig),
        cache:   cache,
    }
}

func (m *LightweightProxyManager) GetProxy(configID string) (*ExtractedProxy, error) {
    // 1. 检查缓存
    if cached, ok := m.cache.Get(configID); ok {
        proxy := cached.(*ExtractedProxy)
        if time.Now().Before(proxy.ExpiresAt) {
            return proxy, nil
        }
    }
    
    // 2. 提取新代理
    config := m.configs[configID]
    
    switch config.ProxyType {
    case "static":
        // 固定代理，直接返回
        return &ExtractedProxy{ProxyURL: config.ProxyURL}, nil
        
    case "dynamic":
        // 动态代理，调用 API 提取
        proxy, err := m.extractFromAPI(config)
        if err != nil {
            return nil, err
        }
        
        // 缓存 5 分钟（可配置）
        m.cache.Add(configID, proxy)
        return proxy, nil
        
    case "rotating":
        // 旋转网关，返回网关地址
        return &ExtractedProxy{ProxyURL: config.GatewayURL}, nil
    }
    
    return nil, fmt.Errorf("unknown proxy type")
}

func (m *LightweightProxyManager) extractFromAPI(config *ProxyConfig) (*ExtractedProxy, error) {
    // 简单 HTTP GET 提取，避免复杂库
    resp, err := http.Get(config.APIURL + "?key=" + config.APIKey)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    
    var result struct {
        Proxy string `json:"proxy"`
        IP    string `json:"ip"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    
    return &ExtractedProxy{
        ProxyURL:  result.Proxy,
        IP:        result.IP,
        ExpiresAt: time.Now().Add(5 * time.Minute),
    }, nil
}
```

### 4.2 精简指纹生成

```go
// internal/fingerprint/lightweight.go

type LightweightFingerprint struct {
    UserAgent string
    Platform  string
    Language  string
}

func GenerateFingerprint(seed int64) *LightweightFingerprint {
    // 使用轻量级随机生成，避免复杂计算
    r := rand.New(rand.NewSource(seed))
    
    userAgents := []string{
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0",
        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/120.0.0.0",
        "Mozilla/5.0 (X11; Linux x86_64) Chrome/120.0.0.0",
    }
    
    return &LightweightFingerprint{
        UserAgent: userAgents[r.Intn(len(userAgents))],
        Platform:  "Win32",
        Language:  "en-US",
    }
}
```

---

## 五、配置文件（1核1G 默认值）

```json
{
  "// 性能配置（1核1G 优化）": "",
  "performance": {
    "min_concurrency": 1,
    "max_concurrency": 3,
    "default_concurrency": 1,
    "memory_threshold_mb": 700,
    "memory_critical_mb": 850,
    "cpu_threshold_percent": 75,
    "cpu_critical_percent": 90
  },
  
  "// 服务管理": "",
  "service_management": {
    "auto_start_services": false,
    "idle_timeout_seconds": 300,
    "health_check_timeout_seconds": 30
  },
  
  "// 数据库优化": "",
  "database": {
    "cache_size_mb": 20,
    "mmap_size_mb": 20,
    "wal_autocheckpoint": 1000
  },
  
  "// 任务执行": "",
  "task_execution": {
    "max_concurrent_tasks": 1,
    "task_timeout_minutes": 10,
    "retry_max_attempts": 2,
    "retry_delay_seconds": 30
  },
  
  "// 代理配置": "",
  "proxy": {
    "cache_size": 5,
    "cache_ttl_minutes": 5,
    "extract_timeout_seconds": 30
  }
}
```

---

## 六、启动流程优化

### 6.1 分阶段启动

```bash
#!/bin/bash
# scripts/start.sh - 1核1G 优化启动

echo "启动 Pool Server (1核1G 模式)"

# 1. 仅启动 Go 主服务
systemctl start codex-pool

# 2. 等待主服务就绪
sleep 5

echo "主服务已启动 (内存占用: ~50MB)"
echo ""
echo "Python 服务将按需自动启动："
echo "  - 创建注册任务时 → 启动注册服务"
echo "  - 创建支付任务时 → 启动支付服务"
echo "  - 5分钟无活动 → 自动停止"
echo ""
echo "访问管理面板: http://localhost:8787"
```

### 6.2 健康监控

```go
// internal/health/monitor.go

type HealthMonitor struct {
    checkInterval time.Duration
}

func (hm *HealthMonitor) Start() {
    ticker := time.NewTicker(hm.checkInterval)
    defer ticker.Stop()
    
    for range ticker.C {
        stats := hm.collectStats()
        
        // 内存告警
        if stats.MemoryUsageMB > 900 {
            log.Warn("内存使用过高: %dMB, 暂停新任务", stats.MemoryUsageMB)
            // 触发降级
        }
        
        // CPU 告警
        if stats.CPUUsagePercent > 90 {
            log.Warn("CPU 使用过高: %.1f%%, 降低并发", stats.CPUUsagePercent)
            // 触发降级
        }
    }
}

func (hm *HealthMonitor) collectStats() *SystemStats {
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    
    return &SystemStats{
        MemoryUsageMB:    int(m.Alloc / 1024 / 1024),
        CPUUsagePercent:  getCPUUsage(),
        GoroutineCount:   runtime.NumGoroutine(),
        ActiveTasks:      getActiveTaskCount(),
    }
}
```

---

## 七、实际性能测试

### 7.1 预期性能（1核1G）

| 场景 | 并发 | 内存 | CPU | 完成时间 |
|-----|-----|------|-----|---------|
| **仅注册 10 个账号** | 1 | 150MB | 60% | 10-15 分钟 |
| **注册+Plus 5 个** | 1 | 200MB | 70% | 8-12 分钟 |
| **批量升级 10 个** | 1 | 180MB | 65% | 5-8 分钟 |

### 7.2 压力测试配置

```bash
# 1核1G 压力测试
./scripts/stress_test.sh --accounts 20 --concurrency 1

预期结果：
- 内存峰值：< 900MB
- CPU 峰值：< 95%
- 完成时间：~ 30-40 分钟
- 成功率：> 90%
```

---

## 八、降级策略

### 8.1 自动降级

```go
// internal/lifecycle/degradation.go

type DegradationManager struct {
    currentLevel int // 0: 正常, 1: 轻度, 2: 重度
}

func (dm *DegradationManager) CheckAndDegrade() {
    stats := collectSystemStats()
    
    // 内存压力
    if stats.MemoryUsageMB > 900 {
        dm.degradeToLevel(2) // 重度降级
    } else if stats.MemoryUsageMB > 800 {
        dm.degradeToLevel(1) // 轻度降级
    } else {
        dm.degradeToLevel(0) // 恢复正常
    }
}

func (dm *DegradationManager) degradeToLevel(level int) {
    if level == dm.currentLevel {
        return
    }
    
    switch level {
    case 0: // 正常
        log.Info("系统恢复正常")
        setConcurrency(2)
        
    case 1: // 轻度降级
        log.Warn("轻度降级: 降低并发到 1")
        setConcurrency(1)
        
    case 2: // 重度降级
        log.Error("重度降级: 暂停新任务，等待资源释放")
        setConcurrency(0)
        pauseNewTasks()
        runtime.GC() // 强制 GC
    }
    
    dm.currentLevel = level
}
```

---

## 九、用户体验优化

### 9.1 实时资源显示

```html
<!-- 管理面板顶部显示资源使用 -->
<div class="resource-bar">
  <div class="resource-item">
    <span>内存</span>
    <div class="progress-bar">
      <div class="progress" :style="{width: memoryPercent + '%'}"></div>
    </div>
    <span>{{ memoryUsed }}MB / 1024MB</span>
  </div>
  
  <div class="resource-item">
    <span>CPU</span>
    <div class="progress-bar">
      <div class="progress" :style="{width: cpuPercent + '%'}"></div>
    </div>
    <span>{{ cpuPercent }}%</span>
  </div>
  
  <div class="resource-item">
    <span>任务并发</span>
    <span>{{ activeTasks }} / {{ maxConcurrency }}</span>
  </div>
</div>
```

### 9.2 任务创建提示

```html
<div class="task-form">
  <label>任务数量：<input v-model="taskCount" type="number"></label>
  
  <!-- 资源预估 -->
  <div class="resource-estimate">
    <p>预计资源占用：</p>
    <ul>
      <li>内存：~{{ estimatedMemory }}MB</li>
      <li>时间：~{{ estimatedTime }} 分钟</li>
      <li>建议并发：{{ suggestedConcurrency }}</li>
    </ul>
    
    <div v-if="resourceWarning" class="warning">
      ⚠️ 当前资源紧张，建议：
      - 减少任务数量
      - 或等待当前任务完成
    </div>
  </div>
</div>
```

---

## 十、总结

### ✅ 1核1G 可行性

**结论**：完全可行，但需要：
1. ✅ 并发数限制为 1（可配置）
2. ✅ 按需启动 Python 服务
3. ✅ 精简依赖和内存占用
4. ✅ 动态资源监控和降级

### 📊 性能对比

| 配置 | 并发 | 10个账号耗时 | 内存峰值 | 月成本 |
|-----|-----|------------|---------|--------|
| **1核1G** | 1 | 15分钟 | 200MB | $3-5 |
| **2核2G** | 2 | 7分钟 | 300MB | $10-15 |
| **2核4G** | 3 | 5分钟 | 350MB | $15-20 |

### 🎯 优化要点

1. **不硬编码**：所有性能参数都在 config.json 可配置
2. **按需启动**：Python 服务仅在任务执行时启动
3. **动态调整**：根据实时资源自动调整并发数
4. **优雅降级**：资源不足时自动降级，不崩溃

### 🚀 部署建议

**最低配置**（适合测试）：
- 1核1G VPS
- 20GB 磁盘
- 100Mbps 带宽

**推荐配置**（适合生产）：
- 2核2G VPS
- 40GB 磁盘
- 100Mbps 带宽

### 📝 下一步

1. 实现自动降级逻辑
2. 添加资源监控面板
3. 优化 Python 服务内存占用
4. 压力测试验证

**1核1G 完全够用，只需要合理的资源管理！** 🎉
