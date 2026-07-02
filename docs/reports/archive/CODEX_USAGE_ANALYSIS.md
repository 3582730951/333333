# Codex cffi 出口 usage 统计缺失问题 - 深度分析报告

## 问题描述
用户报告：pool_server 在使用 curl_cffi_sidecar 出口时，没有 Codex 的统计数据。

## 代码路径分析

### 理论上的完整覆盖
根据 server.go:614-685 的代码，所有 Codex 请求都应该记录 usage：

1. **isChat && !stream** (line 618-640)
   - 非流式 chat/completions 请求
   - 调用 `recordUsage()`
   - ✓ 有统计

2. **isChat && stream** (line 644-658)
   - 流式 chat/completions 请求  
   - 创建 `StreamScanner` + 调用 `recordParsedUsage()`
   - ✓ 有统计

3. **!isChat && stream** (line 660-667)
   - 流式 /v1/responses 请求
   - 调用 `streamSSE()` → 内部创建 `StreamScanner` + 调用 `recordParsedUsage()`
   - ✓ 有统计

4. **!isChat && !stream** (line 669-684)
   - 非流式 /v1/responses 请求
   - 调用 `recordUsage()`
   - ✓ 有统计

### sidecar 响应头传递验证

测试结果显示：
- ✓ sidecar 正确保留 `Content-Type: text/event-stream`
- ✓ Go client 正确解码 `x-sidecar-upstream-headers-b64`
- ✓ `isEventStream()` 正确识别流式响应
- ✓ `StreamScanner` 能正确提取 usage 数据

## 可能的根本原因

### 假设 1: WebSocket 路径绕过
**最可能的原因**

检查 server.go:502-511：
```go
codexUseWebSocket := ...
if codexUseWebSocket && s.cfg.CodexPreferSidecarJA3OverWS && 
   !forceCodexResponsesWebSocket(r.Context()) &&
   strings.EqualFold(lease.Egress.Type, "curl_cffi_sidecar") {
    codexUseWebSocket = false  // sidecar 账号降级到 HTTP/SSE
}
```

但如果配置了 `CodexPreferSidecarJA3OverWS = false`，或者模型强制 WS，sidecar 账号仍可能走 WebSocket。

**关键发现**：WebSocket 路径 (`doCodexResponsesWebSocket`) 可能没有正确记录 usage！

让我检查 WebSocket 实现：

### 假设 2: 非流式 responses 路径的 usage 解析失败
line 669-670：
```go
responseBody, _ := io.ReadAll(resp.Body)
s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
```

`recordUsage` 调用 `usage.ParseResponse(body)`，如果 Codex 的非流式响应格式不匹配，可能返回空 usage。

### 假设 3: 异步写入失败被静默吞噬
line 872-890 `recordParsedUsage` 使用 `enqueueWrite` 异步写入，错误被忽略：
```go
s.enqueueWrite(func() {
    _ = s.store.InsertUsageRecord(...)  // 错误被忽略
})
```

如果数据库写入失败（锁冲突、磁盘满等），不会有任何日志。

### 假设 4: 路径判断条件
如果请求既不是 chat，也没有被识别为 event-stream，且不是标准的 responses body 格式，可能落入一个"无统计"的角落case。

## 诊断步骤

### 第1步：确认实际请求路径
添加日志到 server.go:614：
```go
codexSuccess:
    log.Printf("[USAGE-DEBUG] account=%s, egress=%s, isChat=%v, isStream=%v, ct=%q, path=%s",
        lease.Account.ID, lease.Egress.Type, isChat, 
        isEventStream(resp.Header), resp.Header.Get("Content-Type"),
        r.URL.Path)
```

### 第2步：检查 WebSocket 路径
查找 `doCodexResponsesWebSocket` 实现，验证是否记录 usage。

### 第3步：验证数据库写入
在 `enqueueWrite` 中添加错误日志：
```go
err := s.store.InsertUsageRecord(...)
if err != nil {
    log.Printf("[USAGE-ERROR] Failed to insert: %v", err)
}
```

### 第4步：检查实际的响应格式
在每个 usage 记录点添加：
```go
if parsed.TotalTokens == 0 {
    log.Printf("[USAGE-WARN] Zero tokens: account=%s, model=%s, raw=%s",
        accountID, parsed.Model, string(parsed.RawUsage))
}
```

## 立即修复方案

即使不知道确切原因，可以添加一个**兜底统计**机制：

1. 在 `doWithCFRetry` 返回后，检查响应是否已被统计
2. 如果没有，基于 billing_hold 的 estimated_tokens 创建一个估算记录
3. 添加显式的诊断日志到所有 usage 记录点

## 建议的代码修改

见下一个文件：usage_fix.patch
