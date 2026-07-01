//go:build ignore
// +build ignore

package main

import (
	"io"
	"log"
	"strings"

	"codex-account-pool/internal/usage"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// 测试1: 标准 Codex responses SSE 流
	log.Println("=== TEST 1: Standard Codex SSE ===")
	testCodexSSE()

	// 测试2: 检查 sidecar 可能返回的格式
	log.Println("\n=== TEST 2: Checking if response format differs ===")
	testResponseFormats()
}

func testCodexSSE() {
	sampleStream := `event: response.created
data: {"type":"response.created","response":{"id":"resp_abc123","model":"gpt-5.5","usage":null}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"Hello"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":" world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_abc123","model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":20}}}}

data: [DONE]

`

	scanner := usage.NewStreamScanner("codex")
	_, err := io.Copy(io.Discard, io.TeeReader(strings.NewReader(sampleStream), scanner))
	if err != nil {
		log.Printf("Copy error: %v", err)
	}

	if parsed, ok := scanner.Parsed(); ok {
		log.Printf("✓ SUCCESS: Parsed usage data")
		log.Printf("  Model: %s", parsed.Model)
		log.Printf("  Prompt tokens: %d", parsed.PromptTokens)
		log.Printf("  Completion tokens: %d", parsed.CompletionTokens)
		log.Printf("  Total tokens: %d", parsed.TotalTokens)
		log.Printf("  Cached tokens: %d", parsed.CachedTokens)

		if parsed.Model != "gpt-5.5" || parsed.PromptTokens != 100 || parsed.CompletionTokens != 50 {
			log.Printf("✗ MISMATCH: Expected gpt-5.5/100/50")
		}
	} else {
		log.Printf("✗ FAILED: No usage data extracted!")
	}
}

func testResponseFormats() {
	// 可能的格式变体
	variants := []struct {
		name   string
		stream string
	}{
		{
			name: "Without event prefix",
			stream: `data: {"type":"response.created","response":{"id":"r1","model":"gpt-5.5","usage":null}}

data: {"type":"response.completed","response":{"id":"r1","model":"gpt-5.5","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

data: [DONE]
`,
		},
		{
			name: "Compact JSON",
			stream: `event: response.completed
data: {"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}

`,
		},
		{
			name: "With extra whitespace",
			stream: `event: response.completed

data: {"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}


`,
		},
	}

	for _, v := range variants {
		log.Printf("\nTesting: %s", v.name)
		scanner := usage.NewStreamScanner("codex")
		_, _ = io.Copy(io.Discard, io.TeeReader(strings.NewReader(v.stream), scanner))

		if parsed, ok := scanner.Parsed(); ok {
			log.Printf("  ✓ Extracted: prompt=%d, completion=%d, total=%d",
				parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens)
		} else {
			log.Printf("  ✗ Failed to extract usage")
		}
	}
}
