# 账户额度耗尽不自动切换问题 - 根因分析

## 问题描述
用户报告：账户额度耗尽后不会自动切换到池内其他账户

## 代码流程分析

### 1. 成功响应时的额度检查 (guardRateLimit)

**位置**: `internal/api/isolate.go:280-287`

```go
func (s *Server) guardRateLimit(ctx context.Context, accountID string, header http.Header) {
    if !s.cfg.RateLimitGuardEnabled {  // ❌ 关键：需要配置启用
        return
    }
    if cd := exhaustedCooldown(header, storage.Now()); cd > 0 {
        _ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+cd)
    }
}
```

**触发条件**：
1. ✅ 配置 `RateLimitGuardEnabled = true` （**可能未启用**）
2. ✅ 响应头包含 rate-limit 信息
3. ✅ `isExhausted()` 判断所有维度都耗尽

### 2. 额度耗尽判断逻辑 (isExhausted)

**位置**: `internal/api/ratelimit.go:143-168`

```go
func isExhausted(header http.Header) bool {
    // OpenAI/Codex: 两个维度都必须为0
    reqRemain := getRemaining(header, "x-ratelimit-remaining-requests")
    tokRemain := getRemaining(header, "x-ratelimit-remaining-tokens")
    if reqRemain.present && tokRemain.present {
        return reqRemain.value <= 0 && tokRemain.value <= 0  // 必须BOTH为0
    }
    
    // Anthropic: 检查 unified 或 granular
    unified := getRemaining(header, "anthropic-ratelimit-unified-remaining")
    if unified.present {
        return unified.value <= 0
    }
    
    // ❌ 关键：没有 rate-limit headers → 返回 false (不耗尽)
    return false
}
```

**Session 31c 的关键发现**：
> ChatGPT backend-api 可能**不返回** `x-ratelimit-*` headers

### 3. 调度器的账户选择 (selectFresh)

**位置**: `internal/scheduler/scheduler.go:100-166`

```go
func (s *Scheduler) selectFresh(ctx context.Context, route Route) (Lease, error) {
    accounts, err := s.store.ListActiveAccountsByGroup(ctx, route.Group)
    // ...
    for _, account := range accounts {
        if account.QuarantineUntil > now {
            continue  // ✅ 跳过隔离账户
        }
        // ...
        binding, err := s.store.GetEgressBinding(ctx, account.ID)
        egress, ok := s.selectEgress(ctx, binding, now, egressCache)
        if !ok {
            continue  // ✅ 跳过冷却中的 egress
        }
        // ...
    }
}
```

**selectEgress 的冷却检查**：

```go
func (s *Scheduler) selectEgress(ctx context.Context, binding storage.AccountEgressBinding, now int64, ...) {
    if binding.CooldownUntil <= now {  // ✅ 检查账户绑定冷却
        if egress, err := s.egressProfile(ctx, binding.PrimaryEgressID, ...); err == nil && EgressHealthy(egress, now) {
            return egress, true
        }
    }
    // 尝试 standby egress...
}
```

### 4. 失败响应时的冷却 (benchOnLimit)

**位置**: `internal/api/isolate.go:317-326`

```go
func (s *Server) benchOnLimit(ctx context.Context, accountID string, status int, header http.Header, body []byte) {
    cd := usageLimitCooldown(status, body)  // 检查响应体关键词
    if cd == 0 {
        return
    }
    if s.cfg.RateLimitGuardEnabled && header != nil {
        cd = limitCooldownSeconds(header, now, cd)
    }
    _ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+cd)
}
```

**usageLimitCooldown 的检测**：

```go
func usageLimitCooldown(status int, body []byte) int64 {
    lb := strings.ToLower(string(body))
    for _, sig := range []string{"usage limit", "usage_limit", "quota", 
        "insufficient_quota", "exceeded your current", "rate_limit_exceeded", 
        "too many requests"} {
        if strings.Contains(lb, sig) {
            return 1800  // 30分钟冷却
        }
    }
    if status == 429 {
        return 60  // 1分钟冷却
    }
    return 0
}
```

## 问题根因

### 根因 1: RateLimitGuardEnabled 未启用 ⭐⭐⭐⭐⭐

**最可能的原因！**

如果配置中 `RateLimitGuardEnabled = false`（默认值），则：
- ✅ `guardRateLimit()` 直接返回，不检查额度
- ✅ 只有在 **429 错误** 或 **响应体包含限额关键词** 时才冷却
- ✅ 账户会一直使用，直到上游主动拒绝

### 根因 2: Codex backend-api 无 rate-limit headers ⭐⭐⭐⭐

Session 31c 诊断发现：
> ChatGPT backend-api 响应可能**不返回** `x-ratelimit-*` headers

如果没有 headers：
- ✅ `isExhausted()` 返回 `false`
- ✅ `exhaustedCooldown()` 返回 `0`
- ✅ 账户不会被主动冷却
- ✅ 必须等到实际 429 错误才切换

### 根因 3: 单维度耗尽不触发切换 ⭐⭐⭐

Session 31b 修复后的逻辑要求 **ALL** 维度耗尽：

```go
// 必须 requests=0 AND tokens=0
return reqRemain.value <= 0 && tokRemain.value <= 0
```

如果只有 `tokens=0` 但 `requests=100`，不会触发冷却。

**问题**：用户可能期望 tokens 耗尽就切换，但代码认为"还有 requests 配额可以发小请求"。

### 根因 4: 被动检测延迟 ⭐⭐

即使启用了 `guardRateLimit`，检测是在 **成功响应** 后：

1. 请求发送到账户 A
2. 上游返回 200 + `remaining=0`
3. `guardRateLimit` 检测并冷却
4. **但这个请求已经成功了**
5. 下一个请求才会跳过账户 A

用户可能期望**提前检测**（发送前），但实际是**事后检测**（响应后）。

## 修复方案

### 修复 1: 检查并启用 RateLimitGuardEnabled ⭐⭐⭐⭐⭐

**立即修复**

检查配置文件：

```bash
grep -i "rate.*limit.*guard" config.yaml
```

如果没有或为 `false`，添加/修改：

```yaml
rate_limit_guard_enabled: true
```

### 修复 2: 添加主动额度检查日志

在 `guardRateLimit` 中添加日志，确认是否被调用：

```go
func (s *Server) guardRateLimit(ctx context.Context, accountID string, header http.Header) {
    if !s.cfg.RateLimitGuardEnabled {
        log.Printf("[RATE-GUARD] DISABLED: account=%s (set rate_limit_guard_enabled=true to enable)", accountID)
        return
    }
    
    if cd := exhaustedCooldown(header, storage.Now()); cd > 0 {
        log.Printf("[RATE-GUARD] COOLDOWN: account=%s, duration=%ds, reason=exhausted", accountID, cd)
        _ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+cd)
    } else {
        // 诊断：记录未触发的原因
        if isExhausted(header) {
            log.Printf("[RATE-GUARD] exhausted but no reset time in headers: account=%s", accountID)
        }
    }
}
```

### 修复 3: 为 Codex 添加备用检测机制

由于 Codex backend-api 可能不返回 headers，添加响应体检测：

```go
func (s *Server) guardRateLimitWithBody(ctx context.Context, accountID, provider string, header http.Header, body []byte) {
    // 先尝试标准 header 检测
    if s.cfg.RateLimitGuardEnabled {
        if cd := exhaustedCooldown(header, storage.Now()); cd > 0 {
            log.Printf("[RATE-GUARD] header-based cooldown: account=%s, duration=%ds", accountID, cd)
            _ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+cd)
            return
        }
    }
    
    // Codex 备用：检查响应体中的限额信号
    if provider == "codex" && body != nil {
        lb := strings.ToLower(string(body))
        if strings.Contains(lb, "rate limit") || strings.Contains(lb, "quota") || 
           strings.Contains(lb, "usage limit") {
            log.Printf("[RATE-GUARD] body-based cooldown: account=%s, found limit signal in 200 response", accountID)
            _ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+300) // 5分钟冷却
        }
    }
}
```

### 修复 4: 单维度耗尽选项（可选）

如果用户需要 tokens 耗尽就切换：

```go
// 添加配置选项
type Config struct {
    // ...
    RateLimitStrictMode bool // true = 任一维度耗尽就切换
}

func isExhausted(header http.Header, strictMode bool) bool {
    reqRemain := getRemaining(header, "x-ratelimit-remaining-requests")
    tokRemain := getRemaining(header, "x-ratelimit-remaining-tokens")
    
    if reqRemain.present && tokRemain.present {
        if strictMode {
            // 严格模式：任一维度耗尽就切换
            return reqRemain.value <= 0 || tokRemain.value <= 0
        } else {
            // 默认：两个维度都耗尽才切换
            return reqRemain.value <= 0 && tokRemain.value <= 0
        }
    }
    // ...
}
```

### 修复 5: 添加诊断端点

```go
func (s *Server) adminRateLimitDiag(w http.ResponseWriter, r *http.Request) {
    if !s.adminAllowed(w, r) {
        return
    }
    
    accounts, _ := s.store.ListAccounts(r.Context())
    now := storage.Now()
    
    type accountDiag struct {
        ID              string `json:"id"`
        InCooldown      bool   `json:"in_cooldown"`
        CooldownSeconds int64  `json:"cooldown_seconds"`
        LastQuota       string `json:"last_quota"`
    }
    
    var diag []accountDiag
    for _, acc := range accounts {
        binding, _ := s.store.GetEgressBinding(r.Context(), acc.ID)
        quota, _ := s.store.GetAccountRateLimit(r.Context(), acc.ID)
        
        cd := int64(0)
        if binding.CooldownUntil > now {
            cd = binding.CooldownUntil - now
        }
        
        diag = append(diag, accountDiag{
            ID:              acc.ID,
            InCooldown:      cd > 0,
            CooldownSeconds: cd,
            LastQuota:       fmt.Sprintf("requests=%d, tokens=%d", quota.RemainingRequests, quota.RemainingTokens),
        })
    }
    
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]interface{}{
        "rate_limit_guard_enabled": s.cfg.RateLimitGuardEnabled,
        "accounts":                 diag,
    })
}
```

## 立即行动

1. **检查配置**
   ```bash
   grep -i "rate_limit_guard" config.yaml
   ```

2. **如果未启用，添加配置**
   ```yaml
   rate_limit_guard_enabled: true
   ```

3. **重启服务**
   ```bash
   systemctl restart pool-server
   ```

4. **测试验证**
   - 使用一个账户发送请求直到额度耗尽
   - 观察日志中是否有 `[RATE-GUARD] COOLDOWN`
   - 检查下一个请求是否自动切换到其他账户

5. **查看实时冷却状态**
   ```bash
   # 添加诊断端点后
   curl http://localhost:8080/admin/ratelimit-diag -H "Authorization: Bearer $ADMIN_TOKEN"
   ```

## 预期结果

修复后：
- ✅ 账户额度耗尽时自动进入冷却
- ✅ 调度器跳过冷却账户，选择池内其他账户
- ✅ 日志明确显示冷却和切换事件
- ✅ 即使 Codex 无 headers，也能通过 429 检测切换
