# Thinking 深度思考功能集成方案

## 执行摘要

将 other_cpa 的 **thinking 模块**（深度思考/extended thinking）集成到 pool_server，让管理员能为转发到上游的请求配置 AI 的思考强度。

### 核心价值
- **可配置的思考强度**: 管理员可全局/per-account/per-model 配置 thinking 级别
- **透明的上游注入**: 在转发层自动注入 thinking 配置，对下游客户端透明
- **多提供商支持**: Claude (budget_tokens + adaptive effort) 和 Codex (reasoning.effort)
- **智能转换**: 自动处理级别↔预算的转换、范围限制、模型能力适配

---

## 一、技术分析

### 1.1 other_cpa thinking 架构概览

**核心模块**（位于 `/workspace/other_cpa/CLIProxyAPI/internal/thinking/`）:

```
thinking/
├── types.go           # 核心类型定义
│   ├── ThinkingMode: Budget/Level/None/Auto
│   ├── ThinkingConfig: {Mode, Budget, Level}
│   └── ProviderApplier 接口
├── convert.go         # 级别↔预算转换
│   ├── ConvertLevelToBudget: minimal→512, low→1024, medium→8192, high→24576, xhigh→32768
│   └── ConvertBudgetToLevel: 反向映射
├── validate.go        # 配置验证和规范化
│   ├── ValidateConfig: 模型能力检测 + 自动转换 + 范围限制
│   ├── clampBudget/clampLevel: 智能限制到模型支持范围
│   └── convertAutoToMidRange: Auto→具体值
├── apply.go           # 统一入口
│   ├── ApplyThinking: 主入口函数
│   ├── extractThinkingConfig: 从请求体提取现有配置
│   └── StripThinkingConfig: 移除不支持的配置
├── suffix.go          # 模型名后缀解析
│   └── ParseSuffix: "claude-opus-4-8(16384)" → budget=16384
├── strip.go           # 配置清理
└── provider/          # 提供商特定实现
    ├── claude/apply.go    # thinking.type + budget_tokens / adaptive + effort
    ├── codex/apply.go     # reasoning.effort
    ├── openai/apply.go    # reasoning_effort
    └── gemini/apply.go    # generationConfig.thinkingConfig
```

**关键设计模式**:
1. **统一抽象**: ThinkingConfig 统一表示所有提供商的思考配置
2. **提供商适配器**: ProviderApplier 接口让每个提供商自定义应用逻辑
3. **智能转换**: 自动处理 Budget↔Level 转换（如 Codex 只支持 Level，Claude 支持 Budget）
4. **模型感知**: 基于 registry.ModelInfo 的能力元数据进行验证

### 1.2 Claude 思考格式

**两种模式**:

1. **手动预算模式** (claude-sonnet-4-5, opus-4-5):
```json
{
  "thinking": {
    "type": "enabled",      // 或 "disabled"
    "budget_tokens": 16384  // 思考的 token 预算
  },
  "max_tokens": 4096        // 必须 > budget_tokens
}
```

2. **自适应模式** (claude-opus-4-8, sonnet-4-6):
```json
{
  "thinking": {
    "type": "adaptive"
  },
  "output_config": {
    "effort": "high"  // low/medium/high/max
  }
}
```

### 1.3 Codex 思考格式

**级别模式** (gpt-5.x):
```json
{
  "reasoning": {
    "effort": "high"  // low/medium/high
  }
}
```

---

## 二、集成架构设计

### 2.1 配置层设计

#### 配置文件结构 (`config.yaml`)

```yaml
# 全局默认 thinking 配置
thinking:
  enabled: true                # 是否启用 thinking 注入
  default_mode: "auto"         # none/auto/level/budget
  default_level: "medium"      # minimal/low/medium/high/xhigh
  default_budget: 8192         # 仅在 default_mode=budget 时生效
  
  # 提供商级别覆盖
  providers:
    claude:
      mode: "level"            # 优先使用 adaptive effort
      level: "high"
      # budget: 16384          # 如果需要手动预算模式
    codex:
      mode: "level"
      level: "medium"
  
  # 模型级别覆盖（最高优先级）
  models:
    "claude-opus-4-8":
      mode: "level"
      level: "max"             # Opus 支持 max
    "gpt-5.2":
      mode: "level"
      level: "high"
```

#### 配置优先级（从高到低）

1. **请求体中的显式配置**: 如果下游已经设置了 thinking，保留它
2. **模型名后缀**: `claude-opus-4-8(high)` 或 `gpt-5.2(16384)`
3. **模型级配置**: `thinking.models["claude-opus-4-8"]`
4. **提供商级配置**: `thinking.providers["claude"]`
5. **全局默认**: `thinking.default_*`

### 2.2 代码结构设计

```
pool_server/
└── internal/
    ├── thinking/              # 新增包
    │   ├── types.go          # 核心类型（移植自 other_cpa）
    │   ├── convert.go        # 级别↔预算转换
    │   ├── validate.go       # 配置验证
    │   ├── apply.go          # 统一应用入口
    │   ├── suffix.go         # 后缀解析
    │   ├── claude.go         # Claude applier
    │   ├── codex.go          # Codex applier
    │   └── config.go         # 配置管理（读取 config.yaml）
    ├── config/
    │   └── thinking.go       # Config 结构体扩展
    ├── upstream/
    │   ├── anthropic.go      # 注入点：doClaude 调用前
    │   └── client.go         # 注入点：doCodex 调用前
    └── api/
        └── thinking.go       # 管理接口：GET/POST /admin/thinking
```

### 2.3 转发路径注入点

#### Claude 路径

```go
// anthropic.go: doClaude 函数
func (c *Client) doClaude(ctx context.Context, spec Request) (*Response, error) {
    // ... 现有代码 ...
    
    // === 注入点：应用 thinking 配置 ===
    if !spec.PassThrough && c.cfg.ThinkingEnabled {
        spec.Body = c.applyThinkingConfig(spec.Body, "claude", spec.Model, spec.Account)
    }
    
    // 继续现有的转发逻辑（sidecar/direct 两条路径都已处理好 body）
    if spec.Egress.Type == "curl_cffi_sidecar" && !c.cfg.ClaudeForceDirect {
        // ... postViaSidecar ...
    }
    // ... 或 doHTTP ...
}
```

#### Codex 路径

```go
// client.go: Do 函数中，识别 Codex 请求时
func (c *Client) Do(ctx context.Context, spec Request) (*Response, error) {
    switch spec.Provider {
    case "codex":
        // === 注入点：应用 thinking 配置 ===
        if c.cfg.ThinkingEnabled && spec.Method == http.MethodPost {
            spec.Body = c.applyThinkingConfig(spec.Body, "codex", spec.Model, spec.Account)
        }
        return c.doCodex(ctx, spec)
    case "claude":
        return c.doClaude(ctx, spec)
    // ...
    }
}
```

### 2.4 与现有系统的协调

#### 与 cloak 虚拟化的关系

- **cloak**: 处理 billing-header 的虚拟化（session id, device id 等）
- **thinking**: 处理 thinking 字段的注入/修改
- **执行顺序**: `thinking → cloak` （thinking 先修改 body，cloak 再虚拟化）

```go
// anthropic.go 中的顺序
spec.Body = c.applyThinkingConfig(spec.Body, "claude", spec.Model, spec.Account)  // 1. thinking
spec.Body = cloak.Virtualize(spec.Body, id, ...)                                   // 2. cloak
```

**无冲突**: 两者操作不同的 JSON 字段，互不干扰。

---

## 三、实施计划

### Phase 1: 核心移植（Task #4）

**目标**: 将 other_cpa 的 thinking 逻辑移植到 Go

**文件清单**:
```
internal/thinking/
├── types.go          # ThinkingMode, ThinkingConfig, ProviderApplier
├── convert.go        # ConvertLevelToBudget, ConvertBudgetToLevel
├── validate.go       # ValidateConfig, clampBudget, clampLevel
├── apply.go          # ApplyThinking, extractThinkingConfig
├── suffix.go         # ParseSuffix, ParseNumericSuffix, ParseLevelSuffix
├── claude.go         # Claude applier
├── codex.go          # Codex applier
└── errors.go         # ThinkingError
```

**关键实现**:
1. 使用 `tidwall/gjson` 和 `tidwall/sjson` 处理 JSON（与 other_cpa 一致）
2. 依赖现有的 `internal/registry` 包获取模型能力信息
3. 单元测试覆盖所有转换和验证逻辑

**验收标准**:
- [ ] 所有类型和函数从 other_cpa 完整移植
- [ ] 单元测试通过（级别转换、预算限制、模型能力检测）
- [ ] 与 registry 集成（读取 ModelInfo.Thinking）

### Phase 2: 配置层（Task #3）

**目标**: 实现配置管理和解析

**实现**:
```go
// internal/config/config.go 扩展
type Config struct {
    // ... 现有字段 ...
    
    ThinkingEnabled      bool            `json:"thinking_enabled"`
    ThinkingDefaultMode  string          `json:"thinking_default_mode"`
    ThinkingDefaultLevel string          `json:"thinking_default_level"`
    ThinkingDefaultBudget int            `json:"thinking_default_budget"`
    ThinkingProviders    map[string]ThinkingOverride `json:"thinking_providers"`
    ThinkingModels       map[string]ThinkingOverride `json:"thinking_models"`
}

type ThinkingOverride struct {
    Mode   string `json:"mode"`
    Level  string `json:"level"`
    Budget int    `json:"budget"`
}
```

```go
// internal/thinking/config.go
func ResolveConfig(cfg config.Config, provider, model string, account Account) ThinkingConfig {
    // 1. 检查模型后缀: model(16384) 或 model(high)
    // 2. 检查模型级配置
    // 3. 检查提供商级配置
    // 4. 返回全局默认
}
```

**验收标准**:
- [ ] 配置文件解析正确
- [ ] 优先级逻辑正确
- [ ] 配置验证（无效值报错）

### Phase 3: 转发集成（Task #5）

**目标**: 在 Claude/Codex 转发路径注入 thinking

**修改文件**:
```
internal/upstream/
├── anthropic.go      # doClaude 注入点
└── client.go         # Do 函数 Codex 注入点
```

**实现**:
```go
// upstream/anthropic.go
func (c *Client) applyThinkingConfig(body []byte, provider, model string, account Account) []byte {
    if !c.cfg.ThinkingEnabled {
        return body
    }
    
    // 1. 解析配置（后缀优先）
    config := thinking.ResolveConfig(c.cfg, provider, model, account)
    
    // 2. 应用 thinking
    modelInfo := registry.LookupModelInfo(model, provider)
    result, err := thinking.ApplyThinking(body, config, modelInfo, provider)
    if err != nil {
        log.WithError(err).Warn("thinking: failed to apply, passthrough")
        return body
    }
    
    return result
}
```

**验收标准**:
- [ ] Claude 三条路径（sidecar/direct/单账号）都应用 thinking
- [ ] Codex POST 路径应用 thinking
- [ ] 与 cloak 虚拟化无冲突
- [ ] 错误时优雅降级（返回原 body）

### Phase 4: 管理接口（Task #3 续）

**目标**: 提供 Web UI 可调用的管理接口

**接口设计**:
```
GET  /admin/thinking          # 获取当前配置
POST /admin/thinking          # 更新配置
GET  /admin/thinking/preview  # 预览某个请求会应用的 thinking
```

**请求/响应示例**:
```json
// GET /admin/thinking
{
  "enabled": true,
  "default_mode": "level",
  "default_level": "medium",
  "providers": {
    "claude": {"mode": "level", "level": "high"},
    "codex": {"mode": "level", "level": "medium"}
  },
  "models": {
    "claude-opus-4-8": {"mode": "level", "level": "max"}
  }
}

// POST /admin/thinking/preview
Request: {
  "provider": "claude",
  "model": "claude-opus-4-8",
  "body": {"messages": [...]}
}
Response: {
  "resolved_config": {"mode": "level", "level": "max"},
  "applied_body": {"thinking": {"type": "adaptive"}, "output_config": {"effort": "max"}, "messages": [...]}
}
```

**验收标准**:
- [ ] 配置可通过 API 读取和修改
- [ ] 修改立即生效（无需重启）
- [ ] 预览接口正确显示应用结果

### Phase 5: 前端 UI（Task #3 续）

**目标**: 在管理界面添加 thinking 配置面板

**位置**: `/admin` 页面新增 "Thinking 配置" 标签页

**UI 组件**:
1. **全局开关**: `启用 Thinking 注入` (checkbox)
2. **默认配置**:
   - 模式: 下拉框 (None/Auto/Level/Budget)
   - 级别: 下拉框 (minimal/low/medium/high/xhigh) [当 mode=level]
   - 预算: 输入框 (512-32768) [当 mode=budget]
3. **提供商覆盖**: 表格，每行一个 provider，可编辑模式/级别/预算
4. **模型覆盖**: 表格，可添加行（model name + 配置）

**交互**:
- 修改后点击 "保存" → `POST /admin/thinking`
- 实时预览：选择 provider/model，显示会应用的最终配置

**验收标准**:
- [ ] UI 组件完整且美观
- [ ] 配置保存成功
- [ ] 实时预览准确

### Phase 6: 测试和文档（Task #6）

**单元测试**:
```
internal/thinking/*_test.go   # 已在 Phase 1 完成
```

**集成测试**:
```go
// tests/api/thinking_test.go
func TestClaudeThinkingInjection(t *testing.T) {
    // 1. 配置 thinking: level=high
    // 2. 发送 /v1/messages 请求
    // 3. 验证上游收到的请求包含 thinking.type="adaptive", output_config.effort="high"
}

func TestCodexThinkingInjection(t *testing.T) {
    // 1. 配置 thinking: level=medium
    // 2. 发送 /v1/responses 请求
    // 3. 验证上游收到的请求包含 reasoning.effort="medium"
}

func TestThinkingSuffixPriority(t *testing.T) {
    // 1. 配置全局 level=low
    // 2. 请求 model="claude-opus-4-8(high)"
    // 3. 验证应用的是 high（后缀优先）
}
```

**手动测试**:
1. Claude Code 客户端请求（观察上游收到的 thinking 配置）
2. 不同模型/提供商的配置覆盖
3. 配置热更新（修改后立即生效）

**文档**:
```markdown
# docs/THINKING.md
- 功能介绍
- 配置说明
- API 参考
- 故障排查

# 更新 README.md
- 新增特性说明
```

**验收标准**:
- [ ] 所有测试通过（单元 + 集成 + 手动）
- [ ] 文档完整且准确
- [ ] 性能影响可接受（<5ms 额外延迟）

---

## 四、技术细节和注意事项

### 4.1 JSON 处理性能

**问题**: 每个请求都要解析和修改 JSON body

**优化**:
1. **按需解析**: 只有启用 thinking 时才解析
2. **流式处理**: 使用 gjson/sjson（零拷贝路径查询）
3. **缓存检测**: 快速检测请求体是否已包含 thinking（避免重复处理）

```go
func needsThinking(body []byte, provider string) bool {
    // 快速路径：如果已有 thinking，跳过
    switch provider {
    case "claude":
        return !gjson.GetBytes(body, "thinking.type").Exists()
    case "codex":
        return !gjson.GetBytes(body, "reasoning.effort").Exists()
    }
    return true
}
```

### 4.2 模型能力数据

**依赖**: `internal/registry` 包的 `ModelInfo.Thinking` 字段

**当前状态**: registry 已有 Thinking 支持（从 other_cpa 移植）

**需要补充**:
```go
// internal/registry/claude.go
var claudeModels = []ModelInfo{
    {
        ID: "claude-opus-4-8",
        Thinking: &ThinkingSupport{
            Levels: []string{"low", "medium", "high", "max"},
            DynamicAllowed: true,
            // 或者：Min: 1024, Max: 32768, ZeroAllowed: true
        },
    },
    // ...
}
```

### 4.3 与现有功能的兼容性

| 功能 | 关系 | 冲突? | 处理 |
|------|------|-------|------|
| cloak 虚拟化 | 操作不同字段 | 否 | thinking 先执行 |
| 指纹伪装 | 只影响请求头 | 否 | 无关 |
| 流式转发 | body 在转发前处理 | 否 | 无关 |
| passthrough 模式 | passthrough 时跳过 thinking | 否 | 增加条件判断 |

### 4.4 错误处理策略

**原则**: Thinking 应用失败不应阻断请求

```go
result, err := thinking.ApplyThinking(body, config, modelInfo, provider)
if err != nil {
    log.WithFields(log.Fields{
        "provider": provider,
        "model": model,
        "error": err.Error(),
    }).Warn("thinking: failed to apply, passthrough original body")
    return body  // 降级：使用原始 body
}
return result
```

**日志级别**:
- DEBUG: 应用成功（记录配置和结果）
- WARN: 应用失败（记录错误，使用原始 body）
- INFO: 配置变更（管理员修改配置）

---

## 五、配置示例

### 5.1 保守配置（最小化思考成本）

```yaml
thinking:
  enabled: true
  default_mode: "level"
  default_level: "low"
  
  models:
    # 只对 Opus 启用高强度思考
    "claude-opus-4-8":
      mode: "level"
      level: "high"
```

### 5.2 激进配置（最大化思考质量）

```yaml
thinking:
  enabled: true
  default_mode: "level"
  default_level: "high"
  
  providers:
    claude:
      mode: "level"
      level: "max"      # Opus 会用 max
    codex:
      mode: "level"
      level: "high"
```

### 5.3 手动预算控制

```yaml
thinking:
  enabled: true
  default_mode: "budget"
  default_budget: 8192
  
  models:
    "claude-opus-4-8":
      mode: "budget"
      budget: 16384     # Opus 专用高预算
```

---

## 六、验收清单

### 功能完整性
- [ ] Claude 支持 budget_tokens 和 adaptive effort 两种模式
- [ ] Codex 支持 reasoning.effort
- [ ] 模型后缀优先级正确
- [ ] 配置优先级正确（model > provider > global）
- [ ] 级别↔预算自动转换
- [ ] 模型能力验证和范围限制

### 性能
- [ ] 单次 thinking 应用 <5ms
- [ ] 不影响现有转发延迟
- [ ] 配置热更新无性能抖动

### 可靠性
- [ ] thinking 失败时优雅降级
- [ ] 与 cloak/fingerprint 无冲突
- [ ] passthrough 模式正确跳过
- [ ] 错误日志清晰可排查

### 可用性
- [ ] 配置文件易读易写
- [ ] 管理界面直观
- [ ] 文档完整（配置说明、API 文档）
- [ ] 故障排查指南

---

## 七、后续扩展

### 7.1 动态调整

**场景**: 根据账号负载动态调整 thinking 强度

```yaml
thinking:
  dynamic_adjustment:
    enabled: true
    high_load_level: "low"      # 负载高时降低
    low_load_level: "high"      # 负载低时提高
    load_threshold: 0.8         # 负载阈值
```

### 7.2 用户级配置

**场景**: 不同下游用户使用不同 thinking 强度

```yaml
thinking:
  users:
    "user-premium":
      level: "max"
    "user-free":
      level: "low"
```

### 7.3 成本追踪

**扩展**: 记录 thinking 消耗的额外 token

```sql
-- usage 表新增字段
ALTER TABLE usage ADD COLUMN thinking_tokens INTEGER DEFAULT 0;
```

---

## 八、风险和缓解

| 风险 | 影响 | 概率 | 缓解措施 |
|------|------|------|----------|
| JSON 解析影响性能 | 中 | 低 | 使用 gjson 零拷贝；按需解析 |
| 配置错误导致请求失败 | 高 | 中 | 错误时降级到原始 body；配置验证 |
| 模型能力数据不完整 | 中 | 中 | 默认保守配置；定期更新 registry |
| 与现有系统冲突 | 高 | 低 | 充分测试；字段隔离 |

---

## 九、参考资料

### 源代码
- `/workspace/other_cpa/CLIProxyAPI/internal/thinking/` - 完整参考实现
- `/workspace/pool_server/internal/upstream/anthropic.go` - Claude 转发路径
- `/workspace/pool_server/internal/cloak/` - 虚拟化系统

### 官方文档
- Claude Thinking API: https://docs.anthropic.com/claude/docs/thinking
- OpenAI Reasoning: https://platform.openai.com/docs/guides/reasoning

### 内部文档
- Memory: `pool-server-*` - 所有历史改动记录
- CLAUDE.md - 项目指令

---

**文档版本**: 1.0  
**创建日期**: 2026-06-09  
**作者**: Claude (Kiro AI)  
**状态**: 设计完成，待实施
