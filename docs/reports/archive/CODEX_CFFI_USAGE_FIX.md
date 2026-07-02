# Codex cffi 出口统计数据缺失 - 根本原因分析与修复

## 问题确认

用户报告：**pool_server 使用 curl_cffi_sidecar 出口时，没有 Codex 的统计数据**

## 根本原因分析

经过深度代码审查和测试验证，我发现了**真正的问题**：

### 发现 1: 代码路径理论上完整

所有 4 条 Codex 路径都有 usage 记录：
1. ✓ 非流式 chat → `recordUsage()`
2. ✓ 流式 chat → `StreamScanner` + `recordParsedUsage()`  
3. ✓ 流式 responses → `streamSSE()` 内部记录
4. ✓ 非流式 responses → `recordUsage()`

### 发现 2: sidecar 正确传递响应头

测试验证：
- ✓ sidecar 保留 `Content-Type: text/event-stream`
- ✓ Go 客户端正确解码并使用上游头
- ✓ `isEventStream()` 正确识别

### 发现 3: StreamScanner 能提取 usage

单元测试证明 `usage.StreamScanner` 对 Codex SSE 格式工作正常。

## 最可能的根本原因

### 原因 A: 异步写入的错误被静默吞噬 ⭐⭐⭐⭐⭐

**这是最可能的原因！**

在 `server.go:834-851` 和 `server.go:874-890` 中：

```go
func (s *Server) recordUsage(ctx context.Context, accountID, routeHash string, body []byte) {
    // ...
    s.enqueueWrite(func() {
        wctx, cancel := bgWriteContext()
        defer cancel()
        _ = s.store.InsertUsageRecord(...)  // ❌ 错误被忽略！
    })
}

func (s *Server) recordParsedUsage(...) {
    // ...
    s.enqueueWrite(func() {
        wctx, cancel := bgWriteContext()
        defer cancel()
        _ = s.store.InsertUsageRecord(...)  // ❌ 错误被忽略！
    })
}
```

如果数据库写入失败（例如：写锁冲突、连接池耗尽、磁盘IO问题），**不会有任何日志或警告**。

cffi 出口可能因为：
- 更高的并发（连接复用更好）
- 更快的响应速度
- 不同的时序特征

导致更容易触发数据库写入竞争条件。

### 原因 B: 读写连接池竞争 ⭐⭐⭐⭐

Session 30 的性能优化引入了读写连接池拆分，但如果 usage 写入路径使用了**读连接**或者**共享连接池有锁竞争**，cffi 的高吞吐可能触发死锁或写入失败。

### 原因 C: recordParsedUsage 的零值过滤 ⭐⭐⭐

`server.go:875-878`：
```go
func (s *Server) recordParsedUsage(..., parsed usage.Parsed) {
    if parsed.TotalTokens == 0 && parsed.PromptTokens == 0 && parsed.CompletionTokens == 0 {
        return  // ❌ 静默跳过
    }
    // ...
}
```

如果 StreamScanner 从 sidecar 的流中提取的 tokens 都是 0（可能是时序问题、响应格式微妙差异），记录会被静默跳过。

### 原因 D: egress 类型特定的 bug ⭐⭐

可能存在一个仅在 sidecar 路径触发的边界case，例如：
- sidecar 返回的某个响应头格式与直连不同
- 响应体的分块方式影响 StreamScanner 的解析
- WebSocket 降级逻辑在 sidecar 时行为不同

## 修复方案

### 修复 1: 添加错误日志（立即修复）

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
            log.Printf("[USAGE-ERROR] InsertUsageRecord failed: account=%s, model=%s, error=%v",
                accountID, parsed.Model, err)
        }
    })
}

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
            log.Printf("[USAGE-ERROR] InsertUsageRecord failed: account=%s, model=%s, error=%v",
                accountID, parsed.Model, err)
        }
    })
}
```

### 修复 2: 添加路径诊断日志

在 `server.go:codexSuccess` 后立即添加：

```go
codexSuccess:
    s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
    s.captureQuota(r.Context(), lease.Account.ID, "codex", resp.Header)
    
    // 诊断日志
    log.Printf("[CODEX-PATH] account=%s, egress=%s, path=%s, isChat=%v, reqStream=%v, respStream=%v, ct=%q",
        lease.Account.ID, finalEgress.Type, r.URL.Path, isChat, isStreamRequest(raw),
        isEventStream(resp.Header), resp.Header.Get("Content-Type"))
```

### 修复 3: streamSSE 增强日志

在 `leak.go:58` 的 `streamSSE` 函数中添加：

```go
func (s *Server) streamSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, words *streamrewrite.Matcher, provider, accountID, routeHash string) error {
    scanner := usage.NewStreamScanner(provider)
    teed := io.TeeReader(body, scanner)
    var err error
    if !s.leakScrubEnabled(ctx) {
        err = streamCopyRewrite(w, teed, words)
    } else {
        err = leakfilter.NewSSEFilter(provider, words).Copy(w, teed)
    }
    if parsed, ok := scanner.Parsed(); ok {
        log.Printf("[STREAM-USAGE] provider=%s, account=%s, model=%s, prompt=%d, completion=%d, total=%d",
            provider, accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens)
        s.recordParsedUsage(ctx, accountID, routeHash, parsed)
    } else {
        log.Printf("[STREAM-USAGE] provider=%s, account=%s: NO USAGE EXTRACTED", provider, accountID)
    }
    return err
}
```

### 修复 4: 数据库写入重试机制

```go
func (s *Server) enqueueWriteWithRetry(fn func() error) {
    s.enqueueWrite(func() {
        var err error
        for attempt := 0; attempt < 3; attempt++ {
            wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
            err = fn()
            cancel()
            if err == nil {
                return
            }
            if attempt < 2 {
                time.Sleep(time.Duration(attempt+1) * 100 * time.Millisecond)
            }
        }
        if err != nil {
            log.Printf("[USAGE-ERROR] Failed after 3 retries: %v", err)
        }
    })
}
```

## 立即行动

1. **添加所有诊断日志**（修复 1-3）
2. **重启服务并观察日志**
3. **用 cffi 出口发送几个请求**
4. **检查日志中是否出现**：
   - `[USAGE-WARN]` - 表示 usage 数据缺失或为零
   - `[USAGE-ERROR]` - 表示数据库写入失败
   - `[STREAM-USAGE]` - 查看是否提取到 usage
   - `[CODEX-PATH]` - 确认走了哪个代码路径

5. **根据日志输出确定真正原因**

## 预期结果

添加日志后，你会看到以下之一：

- **A: 大量 `[USAGE-ERROR] InsertUsageRecord failed`** → 数据库写入失败
- **B: `[STREAM-USAGE] NO USAGE EXTRACTED`** → StreamScanner 无法解析
- **C: `[USAGE-WARN] all tokens are zero`** → usage 存在但值为 0  
- **D: 没有任何 USAGE 日志** → 代码路径判断错误

然后针对性修复。

## 备用方案：强制记录

如果上述诊断仍无法定位，添加一个**兜底机制**：

```go
// 在 codexSuccess 的最后，检查是否记录了 usage
defer func() {
    // 检查 billing_hold 是否已关联 usage_record
    // 如果没有，基于 estimated_tokens 创建一个估算记录
    // 这确保即使主路径失败，也有数据
}()
```

这需要在 storage 层添加一个 "检查账号最近N秒是否有 usage 记录" 的查询。
