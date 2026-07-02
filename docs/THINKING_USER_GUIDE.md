# Thinking 深度思考功能 - 用户文档

## 概述

Thinking 是 Pool Server 的深度思考功能，允许您精细控制 AI 模型的推理深度和质量。通过配置不同的思考级别或预算，您可以在响应速度和推理质量之间取得最佳平衡。

---

## 核心概念

### 思考模式（Mode）

Thinking 支持四种模式：

1. **Level（级别模式）** - 使用离散的思考级别
   - `minimal` - 最小思考（512 tokens）
   - `low` - 低级思考（1024 tokens）
   - `medium` - 中等思考（8192 tokens）⭐ 推荐
   - `high` - 高级思考（24576 tokens）
   - `xhigh` - 超高级思考（32768 tokens）
   - `max` - 最大思考（128000 tokens）

2. **Budget（预算模式）** - 使用精确的 token 数量
   - 范围：0 - 200,000
   - 推荐：512 - 128,000
   - 示例：`8192` 表示使用 8192 个 tokens 进行思考

3. **Auto（自动模式）** - 由模型自动决定思考深度
   - 适用于不确定最佳思考量的场景
   - Claude 4.6+ 支持自适应思考

4. **None（禁用模式）** - 完全禁用思考
   - 适用于简单查询或需要快速响应的场景

### 配置优先级

Thinking 配置遵循以下优先级（从高到低）：

1. **模型名后缀** - `claude-opus-4-8(high)`
2. **模型级覆盖** - 配置中的 `thinking_models`
3. **提供商级覆盖** - 配置中的 `thinking_providers`
4. **全局默认** - 配置中的 `thinking_default_*`

---

## 配置方法

### 方法 1: 配置文件

编辑 `config.json`:

```json
{
  "thinking_enabled": true,
  "thinking_default_mode": "level",
  "thinking_default_level": "medium",
  "thinking_default_budget": 8192,
  
  "thinking_providers": {
    "claude": {
      "mode": "level",
      "level": "high"
    },
    "codex": {
      "mode": "level",
      "level": "medium"
    }
  },
  
  "thinking_models": {
    "claude-opus-4-8": {
      "mode": "level",
      "level": "max"
    },
    "gpt-5.2": {
      "mode": "budget",
      "budget": 16384
    }
  }
}
```

### 方法 2: 管理 API

**获取当前配置:**
```bash
curl -H "X-Admin-Token: YOUR_TOKEN" \
  http://localhost:8787/admin/thinking
```

**保存配置（热更新）:**
```bash
curl -X POST -H "X-Admin-Token: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "enabled": true,
    "default_mode": "level",
    "default_level": "high",
    "providers": {},
    "models": {
      "claude-opus-4-8": {"mode": "level", "level": "max"}
    }
  }' \
  http://localhost:8787/admin/thinking
```

**预览应用结果:**
```bash
curl -X POST -H "X-Admin-Token: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "provider": "claude",
    "model": "claude-opus-4-8",
    "body": {"messages": [{"role": "user", "content": "test"}]}
  }' \
  http://localhost:8787/admin/thinking/preview
```

### 方法 3: Web UI

访问 `http://localhost:8787/admin/thinking.html` 使用可视化界面配置。

### 方法 4: 模型名后缀

在请求时直接指定：

```bash
curl -X POST http://localhost:8787/v1/messages \
  -d '{
    "model": "claude-opus-4-8(high)",
    "messages": [
      {"role": "user", "content": "解释量子纠缠"}
    ]
  }'
```

支持的后缀格式：
- `model(high)` - 使用 high 级别
- `model(16384)` - 使用 16384 token 预算
- `model(auto)` - 使用自动模式
- `model(none)` - 禁用思考

---

## 使用场景

### 简单查询（Minimal/Low）
```json
{
  "model": "claude-opus-4-8(low)",
  "messages": [{"role": "user", "content": "今天天气怎么样？"}]
}
```
**适用于**: 简单问答、信息检索、快速响应

### 标准任务（Medium）⭐
```json
{
  "model": "claude-opus-4-8(medium)",
  "messages": [{"role": "user", "content": "写一篇关于人工智能的文章"}]
}
```
**适用于**: 日常写作、代码生成、数据分析

### 复杂推理（High/XHigh）
```json
{
  "model": "claude-opus-4-8(high)",
  "messages": [{"role": "user", "content": "设计一个分布式系统架构"}]
}
```
**适用于**: 系统设计、复杂算法、深度分析

### 专家级任务（Max）
```json
{
  "model": "claude-opus-4-8(max)",
  "messages": [{"role": "user", "content": "证明黎曼猜想"}]
}
```
**适用于**: 数学证明、科学研究、创新设计

---

## 性能考虑

### Token 消耗

思考级别越高，消耗的 tokens 越多：

| 级别 | 预算 (tokens) | 相对速度 | 推荐场景 |
|------|---------------|----------|----------|
| Minimal | 512 | ⚡⚡⚡⚡⚡ | 简单查询 |
| Low | 1,024 | ⚡⚡⚡⚡ | 日常对话 |
| Medium | 8,192 | ⚡⚡⚡ | 标准任务 |
| High | 24,576 | ⚡⚡ | 复杂推理 |
| XHigh | 32,768 | ⚡ | 专业分析 |
| Max | 128,000 | 🐢 | 专家任务 |

### 成本优化建议

1. **默认使用 Medium** - 平衡质量和成本
2. **根据任务类型调整** - 简单查询用 Low，复杂任务用 High
3. **使用模型后缀** - 针对特定请求临时调整
4. **监控使用情况** - 查看 `/admin/usage` 了解实际消耗

---

## 故障排除

### 问题：配置不生效

**症状**: 修改配置后没有看到效果

**解决方案**:
1. 检查 `thinking_enabled` 是否为 `true`
2. 确认模型支持 thinking（查看模型文档）
3. 查看日志：`grep "thinking:" /var/log/pool-server.log`
4. 使用预览 API 验证配置应用

### 问题：返回错误 "thinking not supported"

**症状**: API 返回思考不支持错误

**解决方案**:
1. 确认模型版本支持 thinking（Claude 4.6+, GPT-5+）
2. 检查模型名是否正确
3. 尝试使用其他思考级别

### 问题：响应速度慢

**症状**: 请求响应时间过长

**解决方案**:
1. 降低思考级别（从 high 降到 medium 或 low）
2. 使用更小的预算值
3. 对简单查询禁用思考

### 问题：质量不符合预期

**症状**: 响应质量低于预期

**解决方案**:
1. 提高思考级别（从 medium 升到 high）
2. 增加预算值
3. 使用 auto 模式让模型自动决定

---

## 最佳实践

### 1. 分层配置

```json
{
  "thinking_enabled": true,
  "thinking_default_mode": "level",
  "thinking_default_level": "medium",
  
  "thinking_providers": {
    "claude": {"mode": "level", "level": "high"}
  },
  
  "thinking_models": {
    "claude-opus-4-8": {"mode": "level", "level": "max"},
    "claude-sonnet-4-6": {"mode": "level", "level": "medium"}
  }
}
```

- **全局**: 保守默认（medium）
- **提供商**: 根据能力调整（Claude 用 high）
- **模型**: 精细化控制（Opus 用 max，Sonnet 用 medium）

### 2. 使用后缀动态调整

```javascript
// 根据任务复杂度动态选择
function getModel(complexity) {
  const base = 'claude-opus-4-8';
  const levels = {
    simple: 'low',
    normal: 'medium',
    complex: 'high',
    expert: 'max'
  };
  return `${base}(${levels[complexity]})`;
}

// 使用
const model = getModel('complex'); // "claude-opus-4-8(high)"
```

### 3. 监控和优化

定期检查使用情况：
```bash
curl -H "X-Admin-Token: YOUR_TOKEN" \
  http://localhost:8787/admin/usage/timeseries
```

根据数据调整配置：
- 如果成本过高 → 降低默认级别
- 如果质量不足 → 提高关键模型的级别

### 4. A/B 测试

对比不同思考级别的效果：

```bash
# 测试 Medium
curl -X POST http://localhost:8787/v1/messages \
  -d '{"model": "claude-opus-4-8(medium)", "messages": [...]}'

# 测试 High
curl -X POST http://localhost:8787/v1/messages \
  -d '{"model": "claude-opus-4-8(high)", "messages": [...]}'

# 对比质量和耗时
```

---

## API 参考

### GET /admin/thinking

获取当前配置。

**请求头**:
- `X-Admin-Token`: Admin token

**响应**:
```json
{
  "enabled": true,
  "default_mode": "level",
  "default_level": "medium",
  "default_budget": 8192,
  "providers": {...},
  "models": {...}
}
```

### POST /admin/thinking

保存配置（热更新，无需重启）。

**请求头**:
- `X-Admin-Token`: Admin token
- `Content-Type`: application/json

**请求体**:
```json
{
  "enabled": true,
  "default_mode": "level",
  "default_level": "high",
  "default_budget": 8192,
  "providers": {},
  "models": {}
}
```

**响应**:
```json
{
  "status": "ok"
}
```

### POST /admin/thinking/preview

预览配置应用结果。

**请求头**:
- `X-Admin-Token`: Admin token
- `Content-Type`: application/json

**请求体**:
```json
{
  "provider": "claude",
  "model": "claude-opus-4-8",
  "body": {"messages": [...]}
}
```

**响应**:
```json
{
  "source": "model override",
  "resolved_config": {
    "mode": "level",
    "level": "max"
  },
  "applied_body": {...}
}
```

---

## 附录

### 支持的模型

| 提供商 | 模型 | Thinking 支持 | 默认能力 |
|--------|------|----------------|----------|
| Claude | claude-opus-4-8 | ✅ | Level + Budget |
| Claude | claude-sonnet-4-6 | ✅ | Level (adaptive) |
| Codex | gpt-5.2 | ✅ | Level only |
| Codex | gpt-5.4 | ✅ | Level only |

### 配置验证规则

- `default_mode`: 必须是 `level`, `budget`, `auto`, 或 `none`
- `default_level`: 必须是 `minimal`, `low`, `medium`, `high`, `xhigh`, 或 `max`
- `default_budget`: 必须在 0 - 200,000 范围内
- Provider/Model 覆盖遵循相同规则

### 日志示例

```
DEBUG thinking: configuration applied successfully provider=claude model=claude-opus-4-8 mode=level
INFO thinking: configuration updated enabled=true mode=level
WARN thinking: failed to apply configuration, using original body
```

---

**文档版本**: 1.0  
**最后更新**: 2026-06-09  
**适用版本**: Pool Server v1.0+
