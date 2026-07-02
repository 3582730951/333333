//go:build ignore

// Standalone diagnostic program (run with `go run ./tools/diagnostics/codex/test_codex_cffi_diagnostic.go`),
// excluded from the normal build/test/vet like `go run ./tools/diagnostics/codex/test_codex_usage.go`.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/usage"
)

// 模拟 sidecar 返回的响应
func mockSidecarResponse() *http.Response {
	// 模拟一个真实的 Codex SSE 流
	sseBody := `event: response.created
data: {"type":"response.created","response":{"id":"resp_123","model":"gpt-5.5","usage":null}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_123","model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}}

data: [DONE]

`

	// 构造 sidecar 的响应头
	upstreamHeaders := map[string][]string{
		"Content-Type":  {"text/event-stream"},
		"Cache-Control": {"no-cache"},
	}
	headersJSON, _ := json.Marshal(upstreamHeaders)
	headersB64 := base64.StdEncoding.EncodeToString(headersJSON)

	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Sidecar-Upstream-Status":      {"200"},
			"X-Sidecar-Upstream-Headers-B64": {headersB64},
			"Content-Type":                   {"text/event-stream"}, // sidecar 也设置这个
			"Connection":                     {"close"},
		},
		Body: io.NopCloser(strings.NewReader(sseBody)),
	}

	return resp
}

// 模拟 upstream.Client.doSidecar 的响应头处理
func processSidecarResponse(resp *http.Response) *upstream.Response {
	header := resp.Header.Clone()

	// 这是 client.go:586-594 的逻辑
	if encoded := header.Get("x-sidecar-upstream-headers-b64"); encoded != "" {
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			var upstreamHeaders http.Header
			if json.Unmarshal(decoded, &upstreamHeaders) == nil {
				log.Printf("替换前 header: %v", header)
				header = upstreamHeaders
				log.Printf("替换后 header: %v", header)
			}
		}
	}

	return &upstream.Response{
		StatusCode: resp.StatusCode,
		Header:     header,
		Body:       resp.Body,
	}
}

// 检查是否是事件流
func isEventStream(h http.Header) bool {
	ct := h.Get("Content-Type")
	result := strings.Contains(strings.ToLower(ct), "text/event-stream")
	log.Printf("isEventStream check: Content-Type=%q, result=%v", ct, result)
	return result
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds | log.Lshortfile)

	fmt.Println("=== 诊断 Codex cffi 出口 usage 记录问题 ===\n")

	// 步骤 1: 模拟 sidecar 返回的响应
	fmt.Println("步骤 1: 模拟 sidecar 响应")
	sidecarResp := mockSidecarResponse()
	fmt.Printf("Sidecar 返回的原始 Content-Type: %s\n", sidecarResp.Header.Get("Content-Type"))
	fmt.Printf("Sidecar 的 x-sidecar-upstream-headers-b64: %s\n\n",
		sidecarResp.Header.Get("X-Sidecar-Upstream-Headers-B64"))

	// 步骤 2: 模拟 Go 客户端处理响应头
	fmt.Println("步骤 2: Go 客户端处理响应头")
	upstreamResp := processSidecarResponse(sidecarResp)
	fmt.Printf("处理后的 Content-Type: %s\n\n", upstreamResp.Header.Get("Content-Type"))

	// 步骤 3: 检查是否被识别为事件流
	fmt.Println("步骤 3: 检查流式响应判断")
	if !isEventStream(upstreamResp.Header) {
		fmt.Println("✗ 问题确认: 响应未被识别为事件流!")
		fmt.Println("  这意味着不会进入 streamSSE 路径")
		fmt.Println("  因此不会创建 StreamScanner，也不会记录 usage\n")
	} else {
		fmt.Println("✓ 响应被正确识别为事件流\n")
	}

	// 步骤 4: 测试 usage 提取
	fmt.Println("步骤 4: 测试 usage 提取")
	if isEventStream(upstreamResp.Header) {
		scanner := usage.NewStreamScanner("codex")
		body, _ := io.ReadAll(io.TeeReader(upstreamResp.Body, scanner))
		fmt.Printf("读取的 body 长度: %d bytes\n", len(body))

		if parsed, ok := scanner.Parsed(); ok {
			fmt.Printf("✓ 成功提取 usage:\n")
			fmt.Printf("  Model: %s\n", parsed.Model)
			fmt.Printf("  Prompt tokens: %d\n", parsed.PromptTokens)
			fmt.Printf("  Completion tokens: %d\n", parsed.CompletionTokens)
			fmt.Printf("  Total tokens: %d\n\n", parsed.TotalTokens)
		} else {
			fmt.Println("✗ 未能提取 usage 数据\n")
		}
	}

	// 步骤 5: 检查数据库记录条件
	fmt.Println("步骤 5: 检查 recordParsedUsage 条件")
	fmt.Println("代码路径分析:")
	fmt.Println("  server.go:643 - if isEventStream(resp.Header)")
	fmt.Println("  server.go:644 - if isChat")
	fmt.Println("  server.go:652-656 - 创建 StreamScanner + recordParsedUsage")
	fmt.Println("  server.go:660-667 - 否则调用 streamSSE (内部也会 recordParsedUsage)")
	fmt.Println()

	// 实际问题诊断
	fmt.Println("=== 根因分析 ===")
	fmt.Println("理论上 sidecar 路径应该工作，因为:")
	fmt.Println("1. ✓ sidecar 正确传递 Content-Type: text/event-stream")
	fmt.Println("2. ✓ Go 客户端正确解码并使用上游响应头")
	fmt.Println("3. ✓ isEventStream() 会返回 true")
	fmt.Println("4. ✓ 会进入 streamSSE 或 responsesStreamToChatSSE 路径")
	fmt.Println("5. ✓ StreamScanner 能正确提取 usage")
	fmt.Println()
	fmt.Println("但是，如果实际运行时没有统计数据，可能的原因:")
	fmt.Println("A. sidecar 返回的实际响应头缺少 Content-Type")
	fmt.Println("B. 响应不是 SSE 格式（WebSocket?）")
	fmt.Println("C. StreamScanner 没有从响应中找到 usage 数据")
	fmt.Println("D. recordParsedUsage 被跳过（tokens 都是 0）")
	fmt.Println("E. 数据库写入失败（enqueueWrite 异步写入）")
	fmt.Println()

	fmt.Println("=== 下一步: 添加实时诊断 ===")
	fmt.Println("建议在 server.go 中添加日志:")
	fmt.Println("1. 在 doWithCFRetry 后记录 resp.Header")
	fmt.Println("2. 在 isEventStream 判断处记录结果")
	fmt.Println("3. 在 streamSSE 中记录 scanner.Parsed() 结果")
	fmt.Println("4. 在 recordParsedUsage 中记录实际写入的数据")
}
