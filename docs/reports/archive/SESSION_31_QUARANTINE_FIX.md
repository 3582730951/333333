# 隔离系统优化 — Session 31

## 问题分析

用户遇到的问题：
```
unexpected status 503 Service Unavailable: no available account for this request 
(group=cyber, provider=any, model=gpt-5.5). 
Group "cyber" has 2 account(s) but all are quarantined or disabled
```

**根本原因**：账号被自动隔离（quarantine），且存在三个系统性缺陷：
1. **健康测试通过后不会自动清除隔离** — 即使账号恢复正常，仍需手动干预
2. **隔离时长过长** — 硬编码 30 天，对于临时性问题（如 token 过期）过于严苛
3. **缺少手动清除机制** — 管理员无法直接解除隔离，只能等待超时

## 修复内容

### 1. 可配置的隔离时长（默认 72 小时）

**新增配置字段** (`internal/config/config.go`)：
```go
// QuarantineDurationHours 控制账号隔离时长（默认 72h，旧版硬编码 30 天）
// 设置为 0 可禁用自动隔离（仅手动审查）
QuarantineDurationHours int `json:"quarantine_duration_hours"`
```

**默认值**：`72` 小时（3 天），而非之前的 30 天

**使用方法**：
- 在 `config.json` 中设置 `"quarantine_duration_hours": 24` 改为 1 天
- 设置为 `0` 完全禁用自动隔离
- 设置为 `1` 用于快速测试（1 小时）

**修改位置**：
- `internal/api/isolate.go` 的 `handleBannedAccount` 和 `handlePermissionDeniedAccount`
- 旧代码：`storage.Now()+int64((30*24*time.Hour)/time.Second)` → 硬编码 30 天
- 新代码：`storage.Now() + int64(s.cfg.QuarantineDurationHours*3600)` → 可配置

### 2. 健康测试自动清除隔离（默认启用）

**新增配置字段**：
```go
// HealthTestClearsQuarantine: 当为 true（默认），成功的健康测试（alive=true）
// 会自动清除该账号的隔离状态，无需手动干预
HealthTestClearsQuarantine bool `json:"health_test_clears_quarantine"`
```

**默认值**：`true`（启用）

**行为**：
- 当健康测试返回 `alive=true`（非 ban 状态）时，自动调用 `SetAccountQuarantine(ctx, accountID, 0, "")`
- 操作员只需点击"测试存活"，通过即自动解除隔离
- 设置为 `false` 可要求严格的手动审查流程

**修改位置**：`internal/api/server.go` 的 `adminHealthTest` 函数

### 3. 手动清除隔离按钮

**新增 API 端点**：
```
POST /admin/accounts/{id}/clear-quarantine
```

**前端按钮**：
- 位置：管理后台 → 账号列表 → 账号详情抽屉 → 操作按钮行
- 显示条件：仅当账号当前处于隔离状态时显示（`quarantine_until > now`）
- 样式：黄色警告按钮（`btn sm warn`）
- 文本：中文"清除隔离" / 英文"Clear quarantine"

**实现**：
- `internal/api/server.go`：新增 `adminClearQuarantine` 函数
- 注册路由：`/admin/accounts/{id}/clear-quarantine` 映射到 `case "clear-quarantine"`
- 写入审计日志：`action="clear_quarantine", state="manual", reason="cleared by admin"`
- `internal/web/assets/admin.js`：新增 `clearQuarantine(id)` 函数

## 测试覆盖

新增 3 个测试（`internal/api/quarantine_test.go`）：

1. **TestHealthTestClearsQuarantine**：验证健康测试成功后自动清除隔离
2. **TestClearQuarantineEndpoint**：验证手动清除端点工作正常
3. **TestQuarantineDurationConfigurable**：验证配置字段控制隔离时长

所有测试均通过（包括 race detector）。

## 使用指南

### 场景 1：账号被隔离后快速恢复

**旧流程**（Session 30 及之前）：
1. 账号因 token 过期被隔离 30 天
2. 管理员修复问题（重新登录、更新 scope）
3. **无法立即恢复** — 必须等待 30 天超时，或手动修改数据库

**新流程**（Session 31）：
1. 账号因 token 过期被隔离
2. 管理员修复问题（重新登录、更新 scope）
3. 点击"测试存活" → 通过 → **自动清除隔离** ✓

### 场景 2：强制手动清除

如果不想等待健康测试，可直接清除：
1. 管理后台 → 账号列表 → 点击被隔离的账号
2. 在抽屉的操作按钮行，找到**黄色"清除隔离"按钮**
3. 确认对话框 → 隔离立即清除

### 场景 3：调整隔离策略

在 `config.json` 中：

```json
{
  "quarantine_duration_hours": 24,
  "health_test_clears_quarantine": true,
  "ban_auto_delete": false
}
```

- **短期隔离**：`quarantine_duration_hours: 24`（1 天）适合临时性问题
- **禁用自动隔离**：`quarantine_duration_hours: 0` + `ban_auto_delete: false`，完全手动审查
- **严格审查流程**：`health_test_clears_quarantine: false`，要求管理员显式点击"清除隔离"

## 向后兼容性

- **默认行为改进**：30 天 → 72 小时（更合理）
- **配置可恢复旧行为**：设置 `quarantine_duration_hours: 720`（30 天）+ `health_test_clears_quarantine: false`
- **数据库无变更**：仍使用 `accounts.quarantine_until` 字段
- **API 完全兼容**：新增端点，未修改现有端点

## 文件清单

| 文件 | 修改内容 |
|------|---------|
| `internal/config/config.go` | 新增 2 个配置字段 + 默认值 |
| `internal/api/isolate.go` | 隔离时长改为可配置（删除硬编码 30 天） |
| `internal/api/server.go` | 健康测试自动清除隔离 + 新增手动清除端点 |
| `internal/web/assets/admin.js` | 新增"清除隔离"按钮 + `clearQuarantine()` 函数 |
| `internal/api/quarantine_test.go` | 新增 3 个单元测试 |

## 验证方法

**构建与测试**：
```bash
go build ./...                    # 编译通过
go test ./internal/api/ -v        # 所有测试通过
go test -race ./internal/api/     # race detector 干净
node --check admin.js             # JS 语法正确
```

**功能验证**：
1. 启动 pool_server
2. 手动将一个账号设置为隔离状态（通过 SQLite 或触发 ban）
3. 管理后台查看该账号 → 应显示黄色"清除隔离"按钮
4. 点击"测试存活" → 如果通过，隔离应自动清除
5. 或直接点击"清除隔离" → 立即解除

## 原有问题的最终解决方案

用户的 503 错误诊断路径：
1. **新的错误消息已明确指出**：`Group "cyber" has 2 account(s) but all are quarantined or disabled`
2. **解决方法**：
   - 查看审计日志（`/admin/audit`）了解隔离原因
   - 如果是临时性问题（token 过期、scope 不足）：重新登录账号 → 点击"测试存活" → 自动恢复
   - 如果是永久性 ban：删除账号或保持隔离
   - 紧急情况：直接点击"清除隔离"按钮
