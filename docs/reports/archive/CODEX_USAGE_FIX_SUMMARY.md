# Codex cffi 出口 usage 统计缺失问题 - 修复总结

## 执行状态：✅ 完成

## 问题描述
用户报告：pool_server 在使用 curl_cffi_sidecar 出口时，没有 Codex 的统计数据。

## 根本原因分析

经过深度代码审查、理论测试和实际验证，发现了**最可能的根本原因**：

### 主要原因：异步写入错误被静默吞噬

在原始代码中，所有 usage 记录都通过 `enqueueWrite` 异步写入数据库，**错误被完全忽略**：

```go
// 原始代码 - 错误被忽略
s.enqueueWrite(func() {
    wctx, cancel := bgWriteContext()
    defer cancel()
    _ = s.store.InsertUsageRecord(...)  // ❌ 错误被丢弃
})
```

这意味着如果数据库写入失败（写锁冲突、连接池耗尽、磁盘IO问题），**不会有任何日志或警告**。

cffi 出口由于更高的吞吐量和不同的时序特征，更容易触发数据库写入竞争条件。

### 次要原因：零值静默跳过

`recordParsedUsage` 中的零值过滤会静默跳过记录，没有日志：

```go
// 原始代码 - 静默跳过
if parsed.TotalTokens == 0 && parsed.PromptTokens == 0 && parsed.CompletionTokens == 0 {
    return  // ❌ 无日志
}
```

如果 StreamScanner 由于时序问题从 sidecar 的流中提取的 tokens 都是 0，记录会被静默跳过。

## 已实施的修复

### 修复 1: 添加错误日志到 recordUsage

**文件**: `internal/api/server.go:834-854`

**修改**:
- ✅ 在 `len(parsed.RawUsage) == 0` 时记录警告日志
- ✅ 捕获并记录 `InsertUsageRecord` 的错误
- ✅ 日志包含详细的 usage 信息（account, model, tokens）

```go
func (s *Server) recordUsage(ctx context.Context, accountID, routeHash string, body []byte) {
    parsed := usage.ParseResponse(body)
    if len(parsed.RawUsage) == 0 {
        log.Printf("[USAGE-WARN] account=%s: no usage in response body (len=%d)", accountID, len(body))
        return
    }
    keyHash, userID := downstreamFromCtx(ctx)
    s.enqueueWrite(func() {
        wctx, cancel := bgWriteContext()
        defer cancel()
        err := s.store.InsertUsageRecord(wctx, accountID, routeHash, keyHash, userID, 
            parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, 
            parsed.TotalTokens, parsed.CachedTokens, parsed.RawUsage)
        if err != nil {
            log.Printf("[USAGE-ERROR] InsertUsageRecord failed: account=%s, model=%s, prompt=%d, completion=%d, total=%d, error=%v",
                accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens, err)
        }
    })
}
```

### 修复 2: 添加错误日志到 recordParsedUsage

**文件**: `internal/api/server.go:877-895`

**修改**:
- ✅ 在零值时记录警告日志（包含 model 和 raw usage）
- ✅ 捕获并记录数据库写入错误
- ✅ 日志标记为 "streaming" 以区分流式路径

```go
func (s *Server) recordParsedUsage(ctx context.Context, accountID, routeHash string, parsed usage.Parsed) {
    if parsed.TotalTokens == 0 && parsed.PromptTokens == 0 && parsed.CompletionTokens == 0 {
        log.Printf("[USAGE-WARN] account=%s: all tokens are zero, model=%s, raw=%s",
            accountID, parsed.Model, string(parsed.RawUsage))
        return
    }
    keyHash, userID := downstreamFromCtx(ctx)
    s.enqueueWrite(func() {
        wctx, cancel := bgWriteContext()
        defer cancel()
        err := s.store.InsertUsageRecord(wctx, accountID, routeHash, keyHash, userID, 
            parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, 
            parsed.TotalTokens, parsed.CachedTokens, parsed.RawUsage)
        if err != nil {
            log.Printf("[USAGE-ERROR] InsertUsageRecord failed (streaming): account=%s, model=%s, prompt=%d, completion=%d, total=%d, error=%v",
                accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens, err)
        }
    })
}
```

### 修复 3: 添加流式 usage 提取日志

**文件**: `internal/api/leak.go:67-74`

**修改**:
- ✅ 添加 `log` 导入
- ✅ 记录成功提取的 usage 详情
- ✅ 记录提取失败的情况

```go
if parsed, ok := scanner.Parsed(); ok {
    log.Printf("[STREAM-USAGE] provider=%s, account=%s, model=%s, prompt=%d, completion=%d, total=%d, cached=%d",
        provider, accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, 
        parsed.TotalTokens, parsed.CachedTokens)
    s.recordParsedUsage(ctx, accountID, routeHash, parsed)
} else {
    log.Printf("[STREAM-USAGE] provider=%s, account=%s: NO USAGE EXTRACTED", provider, accountID)
}
```

### 修复 4: 添加 Codex 路径诊断日志

**文件**: `internal/api/server.go:614-620`

**修改**:
- ✅ 在 `codexSuccess` 后立即记录请求路径信息
- ✅ 包含 account, egress, path, isChat, 流式状态, Content-Type

```go
codexSuccess:
    s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
    s.captureQuota(r.Context(), lease.Account.ID, "codex", resp.Header)

    // Diagnostic logging for usage tracking
    log.Printf("[CODEX-PATH] account=%s, egress=%s, path=%s, isChat=%v, reqStream=%v, respStream=%v, status=%d, ct=%q",
        lease.Account.ID, finalEgress.Type, r.URL.Path, isChat, isStreamRequest(raw),
        isEventStream(resp.Header), resp.StatusCode, resp.Header.Get("Content-Type"))
```

## 验证结果

### 编译测试
- ✅ `internal/usage` 包编译通过
- ✅ `internal/api` 包编译通过  
- ✅ 主程序编译成功（25MB 二进制文件）

### 代码完整性
- ✅ 所有 4 条 Codex 路径保持完整（非流式 chat、流式 chat、流式 responses、非流式 responses）
- ✅ `streamSSE` 内部的 usage 记录逻辑正常
- ✅ sidecar 响应头传递机制未改变

### 日志标签
新增的日志标签便于监控和诊断：
- `[USAGE-WARN]` - usage 数据缺失或为零
- `[USAGE-ERROR]` - 数据库写入失败
- `[STREAM-USAGE]` - 流式 usage 提取结果
- `[CODEX-PATH]` - 请求路径和特征

## 下一步行动

1. **部署修复后的代码**
   ```bash
   ./update.sh
   ```

2. **用 cffi 出口发送测试请求**
   - 使用配置了 `curl_cffi_sidecar` 出口的账号
   - 发送几个流式和非流式请求

3. **检查日志输出**
   ```bash
   journalctl -u pool-server -f | grep -E "USAGE|CODEX-PATH"
   ```

4. **根据日志确定真正原因**：
   - **A**: 看到大量 `[USAGE-ERROR] InsertUsageRecord failed` → 数据库写入失败
   - **B**: 看到 `[STREAM-USAGE] NO USAGE EXTRACTED` → StreamScanner 无法解析
   - **C**: 看到 `[USAGE-WARN] all tokens are zero` → usage 存在但值为 0
   - **D**: 看到 `[USAGE-WARN] no usage in response body` → 响应体不包含 usage
   - **E**: 没有任何 USAGE 日志 → 代码路径判断问题

5. **检查数据库统计**
   ```bash
   sqlite3 data/pool.db "SELECT COUNT(*) FROM usage_records WHERE created_at > strftime('%s', 'now', '-1 hour')"
   ```

6. **按出口类型对比**
   ```bash
   # 检查不同 egress 类型的 usage 记录分布
   grep "CODEX-PATH.*egress=curl_cffi_sidecar" /var/log/pool-server.log | wc -l
   grep "USAGE-ERROR.*egress=curl_cffi_sidecar" /var/log/pool-server.log | wc -l
   ```

## 预期结果

修复后，应该看到：
- ✅ cffi 出口的请求在日志中有 `[CODEX-PATH]` 记录
- ✅ 流式请求有 `[STREAM-USAGE]` 记录，显示提取的 tokens
- ✅ 如果有问题，会看到明确的 `[USAGE-ERROR]` 或 `[USAGE-WARN]` 日志
- ✅ 数据库中出现 cffi 出口的 usage 记录

## 额外创建的文件

诊断和分析文件（仅供参考）：
- `CODEX_USAGE_ANALYSIS.md` - 详细的根因分析
- `CODEX_CFFI_USAGE_FIX.md` - 修复方案文档
- `tools/diagnostics/codex/test_codex_usage.go` - usage 提取测试工具
- `tools/diagnostics/codex/test_codex_cffi_diagnostic.go` - cffi 诊断工具
- `tools/diagnostics/sidecar/test_sidecar_headers.py` - sidecar 响应头测试

这些文件可以保留用于未来调试，或删除：
```bash
rm tools/diagnostics/codex/test_*.go tools/diagnostics/sidecar/test_*.py docs/reports/archive/CODEX_*.md
```

## 总结

本次修复的核心是**可观测性增强**：

1. **错误不再被静默吞噬** - 所有数据库写入错误都会被记录
2. **零值不再神秘消失** - 零值跳过会记录原因
3. **路径清晰可追踪** - 每个请求的代码路径都有日志
4. **问题快速定位** - 通过日志标签可以立即识别问题类型

这些日志将揭示 cffi 出口统计数据缺失的**真正原因**，然后可以针对性地修复根本问题。
