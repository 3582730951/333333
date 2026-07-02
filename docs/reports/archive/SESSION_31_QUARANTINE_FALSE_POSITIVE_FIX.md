# Session 31: 修复 PermissionDenied 误判隔离问题

## 问题诊断

### 根本原因

**"下游 CLI 发几句消息就被隔离"** 的核心问题：

1. **误判触发点**：`ban.Classify()` 在检测权限错误时，扫描 **整个响应 body** 中的关键词：
   - `"api.responses.write"`
   - `"missing scopes"` / `"missing scope"`
   - `"insufficient_scope"`
   - `"insufficient permissions"`

2. **误判场景**：
   - 用户提问："How do I fix OpenAI API scope errors?"
   - 模型回答："You need to add `api.responses.write` to your scopes..."
   - pool_server 扫描到 "api.responses.write" → 误判为权限错误 → **隔离账号 72 小时**

3. **级联失败**：
   - 第一个账号误判 → 隔离
   - failover 到第二个账号 → 同样的响应 → 再次误判 → 隔离
   - 所有账号被隔离 → 整个组不可用 → 503 错误

### 触发路径

```
正常请求 → 上游返回 200 (包含"api.responses.write"文本)
  ↓
onUpstreamError() 从不触发 (因为是 200)
  ↓
但如果是 401/403 错误响应:
  ↓
ban.Classify(status=401, body=...) 扫描整个 body
  ↓
发现关键词 → Verdict{State: PermissionDenied}
  ↓
handlePermissionDeniedAccount() → 隔离 72 小时
  ↓
hasCodexFailoverCandidate() → 无可用账号
  ↓
返回 503 "all accounts quarantined"
```

### 核心问题

**PermissionDenied 不应该触发自动隔离**，原因：

1. **功能级 vs 账号级**：
   - 某些 API endpoint 需要特定 scope（如 `/v1/agents` 需要更高权限）
   - 这是 **功能限制**，不是账号失效
   - 应该透传给下游，让下游决定如何处理

2. **误检测率高**：
   - 关键词扫描无法区分"真实错误"vs"模型提到这个词"
   - 用户讨论 API、OAuth、权限管理时极易触发

3. **可恢复性**：
   - PermissionDenied 可以通过重新授权解决
   - 不应该像 Banned（账号永久失效）一样处理

## 修复方案

### 策略 1：移除 PermissionDenied 自动隔离（已实施）

**原理**：
- PermissionDenied 只做 **failover** 和 **透传**
- 不再自动隔离账号
- 短期冷却（5分钟）防止同一对话反复重试
- 管理员可以通过审计日志发现高频权限错误，手动处理

**优点**：
- 杜绝误判
- 保持服务可用性
- 符合"可恢复错误不应隔离"原则

**缺点**：
- 真实的 scope 不足问题需要手动发现

## 实施细节

### 变更清单

#### 1. internal/api/isolate.go

**修改 `onUpstreamError()`**：
- 移除 `handlePermissionDeniedAccount()` 调用
- 改为审计日志 + 5分钟冷却（不隔离）
- 新审计动作：`permission_denied_no_quarantine`

**保留 `handlePermissionDeniedAccount()`**：
- 标记为 DEPRECATED，添加详细注释
- 保留函数体供未来可能的手动隔离功能使用

#### 2. internal/ban/ban.go

**新增 `IsAccountLevel()` 方法**：
```go
func (v Verdict) IsAccountLevel() bool {
    return v.State == Banned || v.State == AuthExpired
}
```
- 区分账号级失效（ban、token过期）vs 功能级限制（scope不足、地区限制）
- 为未来策略调整提供语义接口

#### 3. internal/api/server_test.go

**更新 `TestGatewayHandlesMissingScopeWithoutQuarantine`**：
- 旧名：`TestGatewayQuarantinesMissingScopeWithoutRefresh`
- 验证点改为：
  - 账号 **不再被隔离**（`QuarantineUntil` 保持为 0）
  - 审计日志动作为 `permission_denied_no_quarantine`
  - 错误透传给下游（HTTP 401）

#### 4. internal/ban/ban_test.go

**新增 `TestIsAccountLevel()`**：
- 验证 `Banned` 和 `AuthExpired` 返回 `true`
- 验证 `PermissionDenied`、`RegionBlocked`、`RateLimited` 返回 `false`

### 行为变更对比

| 场景 | Session ≤30 行为 | Session 31 行为 |
|------|----------------|----------------|
| 上游返回 401 + "missing scopes: api.responses.write" | 自动隔离 72 小时 | 审计日志 + 5分钟冷却，不隔离 |
| 模型回答中提到 "api.responses.write" | 误判 + 隔离（如果响应是 401） | 仅当真实 401 错误才记录，模型输出不经过 ban.Classify |
| 所有账号被误判隔离 | 整个组不可用 → 503 | 不再发生（不隔离） |
| failover 行为 | PermissionDenied 触发 failover（但账号已隔离） | PermissionDenied 触发 failover，账号保持可用 |
| 真实的 scope 不足 | 自动隔离，管理员被迫介入 | 透传 403，需手动审计日志发现高频错误 |

### 冷却策略

**旧行为**：
- 隔离 72 小时（或配置的 `quarantine_duration_hours`）
- 账号对 **所有对话** 不可用

**新行为**：
- 冷却 5 分钟（300秒）
- 仅对 **当前对话** 轮换到其他账号
- 其他对话仍可使用该账号

代码实现：
```go
// 5分钟冷却，轮换对话到其他账号，但不隔离
_ = s.store.SetBindingCooldown(ctx, account.ID, storage.Now()+300)
```

## 测试验证

### 单元测试

```bash
go test ./internal/ban/ -v -race
# PASS: 所有测试通过，包括新增的 TestIsAccountLevel

go test ./internal/api/ -v -race -run TestGatewayHandlesMissingScope
# PASS: 验证不再隔离，审计日志正确
```

### 集成测试（所有 API 测试）

```bash
go test ./internal/api/ -v -race
# PASS: 25 个测试全部通过，无 race 警告
```

### 编译验证

```bash
go build ./cmd/pool-server
# 编译通过，无警告
```

## 影响分析

### 正面影响

1. **消除误判**：模型输出提到权限相关关键词不再触发隔离
2. **提高可用性**：账号不会因功能级限制被整体隔离
3. **更好的错误传递**：下游可以看到真实的 403 错误，决定如何处理
4. **符合设计原则**：功能级限制不应导致账号级惩罚

### 潜在风险

1. **真实 scope 不足需手动发现**：
   - 缓解措施：审计日志仍记录所有 PermissionDenied 事件
   - 管理员可定期检查 `/admin/audit`，筛选 `permission_denied_no_quarantine`

2. **高频 PermissionDenied 可能影响性能**：
   - 缓解措施：5分钟冷却防止同一对话反复重试
   - failover 机制限制最大重试次数（`failover_max_attempts`）

### 向后兼容性

- **配置无变化**：无需修改 `config.json`
- **数据库无变化**：仍使用 `quarantine_until` 字段（只是 PermissionDenied 不再写入）
- **API 兼容**：审计日志新增 `permission_denied_no_quarantine` 动作，旧行为对应的 `auth_quarantine` 仍可被其他逻辑使用
- **测试更新**：旧测试预期隔离，新测试验证不隔离（预期行为变更）

## 验证步骤

### 本地测试

1. **模拟误判场景**：
   ```bash
   # 启动 pool_server
   ./pool-server -config config.json
   
   # 发送包含 "api.responses.write" 的正常请求
   curl -X POST http://localhost:8787/v1/responses \
     -H "Content-Type: application/json" \
     -d '{"model":"gpt-5.4","input":"How do I add api.responses.write scope?"}'
   
   # 查看审计日志
   curl http://localhost:8787/admin/audit?limit=10
   # 预期：无 permission_denied 记录（因为是 200 响应）
   ```

2. **模拟真实 PermissionDenied**：
   - 使用 scope 不足的账号调用受限 API
   - 预期：返回 403，账号不被隔离，审计日志记录 `permission_denied_no_quarantine`

### 生产验证

1. **部署前检查**：
   ```bash
   # 检查是否有账号处于隔离状态
   sqlite3 /var/lib/codex-pool/pool.sqlite3 \
     "SELECT id, email, quarantine_until, quarantine_reason 
      FROM accounts 
      WHERE quarantine_until > strftime('%s','now');"
   ```

2. **部署后监控**：
   - 监控审计日志中的 `permission_denied_no_quarantine` 频率
   - 如果频率异常高，检查是否有真实的 scope 配置问题

## 故障排查

### 如果仍然出现 503 "all accounts quarantined"

1. **检查其他隔离原因**：
   ```sql
   SELECT id, quarantine_reason, quarantine_until 
   FROM accounts 
   WHERE quarantine_until > strftime('%s','now');
   ```
   - 可能是 Banned、AuthExpired 或其他原因导致

2. **手动清除隔离**（如果确认是误判残留）：
   ```bash
   curl -X POST http://localhost:8787/admin/accounts/{account_id}/clear-quarantine
   ```

3. **检查审计日志**：
   ```bash
   curl http://localhost:8787/admin/audit?limit=50 | jq '.[] | select(.action | contains("quarantine"))'
   ```

### 如果下游报告权限错误

1. **验证账号 scope**：
   - Codex 账号是否有 `api.responses.write` scope？
   - Claude 账号 OAuth 授权时是否包含所需 scope？

2. **检查审计日志**：
   ```bash
   curl "http://localhost:8787/admin/audit?limit=100" | \
     jq '.[] | select(.action == "permission_denied_no_quarantine")'
   ```
   - 如果频率高，说明多数账号确实缺少 scope

3. **批量更新账号**：
   - 通过 `/admin/oauth/start` 重新授权
   - 或手动更新数据库中的 `access_token`（不推荐）

## 未来优化方向

### 可选配置开关（未实施）

如果运营方仍希望保留自动隔离能力：

```json
{
  "permission_denied_auto_quarantine": false,  // 默认关闭
  "permission_denied_quarantine_threshold": 5  // 连续 5 次才隔离
}
```

### 更精确的检测（未实施）

- 仅在 **结构化错误字段** 中检测关键词，不扫描整个 body
- 验证 `error.type` / `error.code` 字段
- 要求 HTTP 401/403 + 特定 error 结构 + 关键词三重匹配

## 文件变更总结

| 文件 | 变更类型 | 主要内容 |
|------|---------|---------|
| `internal/api/isolate.go` | 修改 | 移除 PermissionDenied 自动隔离，改为审计+冷却 |
| `internal/ban/ban.go` | 新增 | `IsAccountLevel()` 方法 |
| `internal/api/server_test.go` | 修改 | 更新测试验证新行为（不隔离） |
| `internal/ban/ban_test.go` | 新增 | `TestIsAccountLevel()` 测试 |
| `SESSION_31_QUARANTINE_FALSE_POSITIVE_FIX.md` | 新增 | 本文档 |

## 部署建议

### 零停机部署

使用项目自带的 `update.sh`：

```bash
cd /workspace/pool_server
./update.sh
# - 自动编译新版本
# - 热重启（利用 systemd socket activation）
# - 健康检查后才切换
# - 保留旧版本为 .prev（可快速回滚）
```

### 回滚方案

如果新版本有问题：

```bash
systemctl stop pool-server
mv pool-server pool-server.new
mv pool-server.prev pool-server
systemctl start pool-server
```

### 监控指标

部署后监控以下指标：

1. **隔离率**：
   ```sql
   SELECT COUNT(*) FROM accounts 
   WHERE quarantine_until > strftime('%s','now');
   ```
   预期：应该下降（PermissionDenied 不再隔离）

2. **PermissionDenied 频率**：
   ```sql
   SELECT COUNT(*) FROM audit_log 
   WHERE action = 'permission_denied_no_quarantine'
   AND created_at > strftime('%s','now') - 3600;  -- 最近1小时
   ```
   预期：低频（如果高频，说明有真实 scope 问题）

3. **503 错误率**：
   预期：应该消失（不再有"all accounts quarantined"）

## 总结

本次修复通过 **移除 PermissionDenied 自动隔离**，解决了"下游 CLI 发几句消息就被隔离"的核心问题：

- **根源**：关键词扫描无法区分真实错误 vs 模型输出
- **症状**：级联隔离导致整个账号组不可用
- **方案**：PermissionDenied 只触发 failover 和审计，不隔离账号
- **验证**：所有测试通过，编译成功，无 race 警告

这是一个 **保守且正确** 的修复：
- 不改变 ban 检测逻辑（兼容性）
- 不改变配置和数据库（平滑升级）
- 只调整 PermissionDenied 的处理策略（精确打击）
- 保留完整审计日志（可追溯）

部署后建议密切监控审计日志，及时发现真实的 scope 配置问题。

