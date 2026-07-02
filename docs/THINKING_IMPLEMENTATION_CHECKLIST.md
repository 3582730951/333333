# Thinking 功能完整实施清单

## ✅ 已完成的设计文档

### 1. 技术架构与集成方案
📄 **文件**: `/workspace/pool_server/docs/THINKING_INTEGRATION_PLAN.md`

**内容概览**:
- ✅ other_cpa thinking 模块深度分析（8个核心文件）
- ✅ Claude/Codex 两大提供商格式详解
- ✅ 配置层设计（3级优先级：global → provider → model）
- ✅ 转发路径注入点设计（anthropic.go + client.go）
- ✅ 代码结构设计（internal/thinking/ 包）
- ✅ 6 阶段实施计划（核心移植 → 配置 → 集成 → API → UI → 测试）
- ✅ 性能优化策略（gjson 零拷贝，<5ms 延迟）
- ✅ 风险评估与缓解措施

### 2. 前端 UI 详细设计
📄 **文件**: `/workspace/pool_server/docs/THINKING_FRONTEND_UI_DESIGN.md`

**内容概览**:
- ✅ 完整 HTML 结构（导航项 + 配置面板）
- ✅ CSS 样式表（600+ 行，包含滑块/卡片/预览框）
- ✅ JavaScript 交互逻辑（1000+ 行，完整功能）
  - 配置加载/保存
  - 提供商/模型覆盖管理
  - 实时预览
  - 表单验证
  - Toast 通知
- ✅ API 端点设计（GET/POST /admin/thinking*）
- ✅ 用户体验优化（实时反馈、智能默认、移动端适配）
- ✅ 完整集成清单（文件修改 + 测试清单）

---

## 📋 实施任务清单

### Phase 1: 核心逻辑移植 ⏳
**目标**: 将 other_cpa 的 thinking 包移植到 Go

**待创建文件**:
```
internal/thinking/
├── types.go          # ThinkingMode, ThinkingConfig, ProviderApplier
├── convert.go        # ConvertLevelToBudget, ConvertBudgetToLevel
├── validate.go       # ValidateConfig, clampBudget, clampLevel
├── apply.go          # ApplyThinking, extractThinkingConfig
├── suffix.go         # ParseSuffix, ParseNumericSuffix, ParseLevelSuffix
├── claude.go         # Claude applier (adaptive + budget_tokens)
├── codex.go          # Codex applier (reasoning.effort)
├── errors.go         # ThinkingError
└── config.go         # ResolveConfig (优先级逻辑)
```

**关键依赖**:
- `github.com/tidwall/gjson` - JSON 路径查询
- `github.com/tidwall/sjson` - JSON 修改
- `internal/registry` - 模型能力信息

**验收标准**:
- [ ] 所有类型和函数从 other_cpa 完整移植
- [ ] 单元测试覆盖率 >80%
- [ ] 与 registry.ModelInfo 集成测试通过

### Phase 2: 配置层实现 ⏳
**目标**: 实现配置管理和解析

**待修改文件**:
```
internal/config/
├── config.go         # 扩展 Config 结构体
└── thinking.go       # ThinkingOverride 结构体
```

**配置字段**:
```go
type Config struct {
    // ... 现有字段 ...
    
    ThinkingEnabled      bool                       `json:"thinking_enabled"`
    ThinkingDefaultMode  string                     `json:"thinking_default_mode"`
    ThinkingDefaultLevel string                     `json:"thinking_default_level"`
    ThinkingDefaultBudget int                       `json:"thinking_default_budget"`
    ThinkingProviders    map[string]ThinkingOverride `json:"thinking_providers"`
    ThinkingModels       map[string]ThinkingOverride `json:"thinking_models"`
}

type ThinkingOverride struct {
    Mode   string `json:"mode"`
    Level  string `json:"level,omitempty"`
    Budget int    `json:"budget,omitempty"`
}
```

**验收标准**:
- [ ] 配置文件解析正确
- [ ] 优先级逻辑正确（model > provider > global）
- [ ] 配置验证（无效值报错）
- [ ] 热更新支持（修改后立即生效）

### Phase 3: 转发路径集成 ⏳
**目标**: 在 Claude/Codex 转发前注入 thinking

**待修改文件**:
```
internal/upstream/
├── anthropic.go      # doClaude 注入点
└── client.go         # Do 函数 Codex 注入点
```

**核心代码**:
```go
// anthropic.go: doClaude 注入点（第 63 行附近）
func (c *Client) doClaude(ctx context.Context, spec Request) (*Response, error) {
    // ... 现有代码 ...
    
    // === 新增：应用 thinking 配置 ===
    if !spec.PassThrough && c.cfg.ThinkingEnabled {
        spec.Body = c.applyThinkingConfig(spec.Body, "claude", spec.Model, spec.Account)
    }
    
    // 继续现有逻辑...
    if spec.Egress.Type == "curl_cffi_sidecar" && !c.cfg.ClaudeForceDirect {
        // ... postViaSidecar ...
    }
    // ...
}

// 新增辅助函数
func (c *Client) applyThinkingConfig(body []byte, provider, model string, account Account) []byte {
    if !c.cfg.ThinkingEnabled {
        return body
    }
    
    config := thinking.ResolveConfig(c.cfg, provider, model, account)
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
- [ ] 日志记录应用结果

### Phase 4: 管理 API 实现 ⏳
**目标**: 提供配置的 CRUD 接口

**待创建文件**:
```
internal/api/
└── thinking.go       # GET/POST /admin/thinking*
```

**API 端点**:
```
GET  /admin/thinking           # 获取当前配置
POST /admin/thinking           # 保存配置
POST /admin/thinking/preview   # 预览应用结果
```

**验收标准**:
- [ ] GET 接口返回完整配置
- [ ] POST 接口验证并保存配置
- [ ] Preview 接口正确应用 thinking 并返回结果
- [ ] 权限控制（需要 admin_token）
- [ ] 错误处理和日志记录

### Phase 5: 前端 UI 实现 ⏳
**目标**: 在管理界面添加 Thinking 配置页

**待修改文件**:
```
internal/web/assets/
├── index.html        # 添加导航项 + section
├── app.css           # 添加 thinking 样式
└── app.js            # 添加 thinking JS 逻辑
```

**UI 组件**:
- 全局开关（启用/禁用）
- 默认配置（模式/级别/预算）
- 提供商覆盖表格
- 模型覆盖表格
- 实时预览面板

**验收标准**:
- [ ] UI 组件完整且美观
- [ ] 交互逻辑正常（切换/保存/预览）
- [ ] 配置保存成功后显示通知
- [ ] 实时预览准确显示应用结果
- [ ] 移动端响应式布局正常

### Phase 6: 测试和文档 ⏳
**目标**: 全面测试和文档

**单元测试**:
```
internal/thinking/
├── convert_test.go
├── validate_test.go
├── apply_test.go
├── claude_test.go
└── codex_test.go
```

**集成测试**:
```
tests/api/
└── thinking_test.go    # 端到端测试
```

**文档**:
```
docs/
├── THINKING.md         # 用户文档（配置说明、使用指南）
└── README.md           # 更新特性列表
```

**验收标准**:
- [ ] 单元测试覆盖率 >80%
- [ ] 集成测试通过（Claude/Codex 两条路径）
- [ ] 手动测试通过（Claude Code 客户端）
- [ ] 性能测试通过（<5ms 额外延迟）
- [ ] 文档完整且准确

---

## 📊 进度追踪

| Phase | 任务 | 状态 | 文件数 | 预估工作量 |
|-------|------|------|--------|------------|
| 1 | 核心移植 | ⏳ 待开始 | 9 个文件 | 6-8 小时 |
| 2 | 配置层 | ⏳ 待开始 | 2 个文件 | 2-3 小时 |
| 3 | 转发集成 | ⏳ 待开始 | 2 个文件 | 3-4 小时 |
| 4 | 管理 API | ⏳ 待开始 | 1 个文件 | 2-3 小时 |
| 5 | 前端 UI | ⏳ 待开始 | 3 个文件 | 4-6 小时 |
| 6 | 测试文档 | ⏳ 待开始 | 6+ 个文件 | 4-6 小时 |
| **总计** | | | **23+ 个文件** | **21-30 小时** |

---

## 🎯 快速参考

### 级别→预算映射表
```
minimal  →     512 tokens
low      →   1,024 tokens
medium   →   8,192 tokens  (推荐默认)
high     →  24,576 tokens
xhigh    →  32,768 tokens
max      → 128,000 tokens  (仅 Opus 支持)
```

### 配置优先级
```
请求体显式配置 > 模型名后缀 > 模型级配置 > 提供商级配置 > 全局默认
```

### 转发路径注入点
```
Claude:  anthropic.go:doClaude() → applyThinkingConfig() → sidecar/direct
Codex:   client.go:Do() → applyThinkingConfig() → doCodexResponsesWebSocket()
```

---

## ✅ 下一步行动

**推荐顺序**:
1. ✅ **审阅设计文档** - 确认架构和 UI 设计
2. ⏳ **Phase 1: 核心移植** - 创建 internal/thinking/ 包
3. ⏳ **Phase 2: 配置层** - 扩展 config.Config
4. ⏳ **Phase 3: 转发集成** - 注入 anthropic.go + client.go
5. ⏳ **Phase 4-6**: API + UI + 测试

**问题确认**:
- [ ] 配置存储方式：config.yaml vs 数据库？
- [ ] 热更新机制：文件监听 vs 手动重载？
- [ ] 前端框架：纯 JS vs Vue/React？（当前设计：纯 JS）
- [ ] 测试覆盖率目标：80% vs 更高？

---

**文档版本**: 1.0  
**最后更新**: 2026-06-09  
**作者**: Claude (Kiro AI)  
**状态**: 设计完成，等待实施确认
