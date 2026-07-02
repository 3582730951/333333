# 429 立即 Failover 修复

## 问题描述

```
exceeded retry limit, last status: 429 Too Many Requests
```

以及：
```
unexpected status 409 Conflict: strict sticky account unavailable:
account acc_fe0432956fc105e4 on cooldown for 1697s (rate-limited by upstream)
```

**场景**: 上游返回 429 时，系统没有立即切换到其他可用账号，导致客户端重试多次后失败。

## 根本原因

原有的 failover 逻辑：
- 只对非 strict 请求生效
- strict 请求收到 429 时直接返回错误
- 没有立即切换机制

## 修复方案

新增配置项 `force_failover_on_429`（默认 `false`）：

**启用后行为**：
- 上游返回 429 → **立即切换**到其他可用账号
- 无论请求是否 strict（含 `previous_response_id` / `tool_result`）
- 冷却中的账号也会被跳过，选择可用账号

## 修改文件

| 文件 | 修改内容 |
|------|---------|
| `internal/config/config.go` | 新增 `ForceFailoverOn429` 配置项 |
| `internal/api/server.go` | 1. `handleGatewayPost`: 允许 strict 请求在 429 时 failover<br>2. `codexAttempt`: 429 检测逻辑修改<br>3. 200 响应中的 rate limit 也支持 failover |

## 配置方法

在 `config.json` 中添加：

```json
{
  "force_failover_on_429": true,
  "seamless_failover": true,
  "failover_max_attempts": 3
}
```

### 配置项说明

| 配置项 | 值 | 说明 |
|--------|-----|------|
| `force_failover_on_429` | `true` | **启用 429 立即 failover**（推荐） |
| `force_failover_on_429` | `false` | 禁用（原有行为，strict 不 failover） |
| `seamless_failover` | `true` | 启用无缝 failover（必须） |
| `failover_max_attempts` | `3` | 最大尝试账号数（默认 3） |

## 工作流程

### 修复前
```
请求 → 账号 A 返回 429 → 返回错误给客户端 → 客户端重试 → 账号 A 仍然 429 → 循环失败
```

### 修复后
```
请求 → 账号 A 返回 429 → 立即切换到账号 B → 成功响应
```

### Strict 请求示例
```
请求（含 previous_response_id）→ 账号 A 返回 429
  ↓ force_failover_on_429 = true
立即切换到账号 B
  ↓
账号 B 处理请求（可能丢失部分上下文，但能响应）
  ↓
返回成功响应
```

## 日志示例

```
[FAILOVER] force_failover_on_429: strict request failing over on 429, account=acc_xxx
[RATE-LIMIT] COOLDOWN: account=acc_xxx, status=429, duration=60s, reason=usage_limit
```

## 全部配置项总览

```json
{
  "force_failover_on_429": true,
  "seamless_failover": true,
  "failover_max_attempts": 3,
  "strict_sticky_max_cooldown_seconds": 60,
  "cooldown_wait_max_seconds": 30
}
```

| 配置项 | 默认值 | 作用 |
|--------|--------|------|
| `force_failover_on_429` | `false` | 429 时立即切换，即使 strict 请求 |
| `seamless_failover` | `true` | 启用无缝 failover |
| `failover_max_attempts` | `3` | 最大尝试账号数 |
| `strict_sticky_max_cooldown_seconds` | `60` | strict 请求冷却超过此值允许 failover |
| `cooldown_wait_max_seconds` | `30` | 所有账号冷却时等待最短冷却的时间上限 |

## 权衡考量

| 优点 | 风险 |
|------|------|
| 429 时立即切换，客户端无感 | strict 请求切换可能丢失对话上下文 |
| 最大化成功率 | 增加上游请求量（failover 会产生额外请求） |
| 减少客户端重试 | 如果所有账号都 429，仍然会失败 |

## 测试

```bash
# 编译
go build -buildvcs=false -o pool-server ./cmd/gateway/

# 运行测试
go test -buildvcs=false ./...
```

所有测试通过 ✅
