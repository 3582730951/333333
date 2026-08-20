package usage

import (
	"io"
	"strings"
	"testing"
)

func TestParseCachedTokens(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"gpt","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":80}}}`))
	if parsed.Model != "gpt" || parsed.PromptTokens != 100 || parsed.CachedTokens != 80 {
		t.Fatalf("unexpected usage: %+v", parsed)
	}
	if parsed.CacheMissTokens != 20 || parsed.CacheTotalInputTokens != 100 {
		t.Fatalf("openai cache miss/total input wrong: %+v", parsed)
	}
}

func TestParseOpenAICacheWriteTokens(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"gpt-5.6","usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":55}}}`))
	if parsed.CacheReadTokens != 40 || parsed.CacheCreationTokens != 55 {
		t.Fatalf("openai cache read/write not normalized: %+v", parsed)
	}
	if parsed.CacheMissTokens != 60 || parsed.CacheTotalInputTokens != 100 {
		t.Fatalf("openai cache input breakdown changed: %+v", parsed)
	}
}

func TestParseOpenAIChatCacheWriteTokens(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"gpt-5.6","usage":{"prompt_tokens":80,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":32,"cache_write_tokens":17}}}`))
	if parsed.CacheReadTokens != 32 || parsed.CacheCreationTokens != 17 {
		t.Fatalf("chat cache read/write not normalized: %+v", parsed)
	}
}

func TestParseAnthropicUsage(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"claude-x","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":40,"cache_creation_input_tokens":75,"cache_creation":{"ephemeral_5m_input_tokens":25,"ephemeral_1h_input_tokens":50}}}`))
	if parsed.PromptTokens != 100 || parsed.CompletionTokens != 20 {
		t.Fatalf("anthropic tokens: %+v", parsed)
	}
	if parsed.CachedTokens != 40 {
		t.Fatalf("cache_read_input_tokens not parsed: %+v", parsed)
	}
	if parsed.CacheReadTokens != 40 || parsed.CacheCreationTokens != 75 {
		t.Fatalf("anthropic cache read/create not split: %+v", parsed)
	}
	if parsed.CacheMissTokens != 100 || parsed.CacheTotalInputTokens != 215 {
		t.Fatalf("anthropic cache miss/total input wrong: %+v", parsed)
	}
	if parsed.CacheCreation5mTokens != 25 || parsed.CacheCreation1hTokens != 50 {
		t.Fatalf("anthropic cache creation ttl split wrong: %+v", parsed)
	}
	if parsed.TotalTokens != 235 {
		t.Fatalf("total not derived from input+output: %+v", parsed)
	}
}

func TestParseAnthropicCacheCreationIsNotReportedAsHit(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"claude-x","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":75}}`))
	if parsed.CachedTokens != 0 || parsed.CacheReadTokens != 0 || parsed.CacheCreationTokens != 75 {
		t.Fatalf("cache creation must not be counted as a read hit: %+v", parsed)
	}
	if parsed.CacheMissTokens != 100 || parsed.CacheTotalInputTokens != 175 {
		t.Fatalf("anthropic cache-write-only total input wrong: %+v", parsed)
	}
}

func TestParseDeepSeekCacheUsage(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"deepseek-chat","usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`))
	if parsed.Model != "deepseek-chat" || parsed.PromptTokens != 100 || parsed.CompletionTokens != 20 || parsed.TotalTokens != 120 {
		t.Fatalf("deepseek tokens not parsed: %+v", parsed)
	}
	if parsed.CachedTokens != 80 {
		t.Fatalf("prompt_cache_hit_tokens not mapped to cached tokens: %+v", parsed)
	}
	if !strings.Contains(string(parsed.RawUsage), `"prompt_cache_miss_tokens":20`) {
		t.Fatalf("raw usage should preserve miss tokens: %s", parsed.RawUsage)
	}
}

func TestParseDeepSeekCacheUsageDerivesPromptTokens(t *testing.T) {
	parsed := ParseResponse([]byte(`{"model":"deepseek-chat","usage":{"completion_tokens":3,"prompt_cache_hit_tokens":11,"prompt_cache_miss_tokens":5}}`))
	if parsed.PromptTokens != 16 {
		t.Fatalf("prompt tokens should derive from deepseek hit+miss tokens: %+v", parsed)
	}
	if parsed.CachedTokens != 11 || parsed.TotalTokens != 19 {
		t.Fatalf("deepseek derived totals wrong: %+v", parsed)
	}
}

// feedStream tees an SSE transcript through the scanner exactly as the relay does
// (io.TeeReader into the scanner while the stream is "copied" to a discard sink),
// in small chunks to exercise the partial-line buffering.
func feedStream(t *testing.T, provider, transcript string) (Parsed, bool) {
	t.Helper()
	sc := NewStreamScanner(provider)
	r := io.TeeReader(strings.NewReader(transcript), sc)
	buf := make([]byte, 7) // deliberately tiny so frames split across reads
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	return sc.Parsed()
}

func TestStreamScannerCodexResponses(t *testing.T) {
	// A trimmed Codex /v1/responses SSE: deltas then the terminal response.completed.
	transcript := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"gpt-5.5","usage":null}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"gpt-5.5","usage":{"input_tokens":100,"output_tokens":42,"total_tokens":142,"input_tokens_details":{"cached_tokens":60}}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	p, ok := feedStream(t, "codex", transcript)
	if !ok {
		t.Fatal("expected usage from codex stream")
	}
	if p.Model != "gpt-5.5" || p.PromptTokens != 100 || p.CompletionTokens != 42 || p.TotalTokens != 142 || p.CachedTokens != 60 {
		t.Fatalf("codex stream usage = %+v", p)
	}
}

func TestStreamScannerCodexCacheWriteTokens(t *testing.T) {
	transcript := "data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6\",\"usage\":{\"input_tokens\":100,\"output_tokens\":4,\"total_tokens\":104,\"input_tokens_details\":{\"cached_tokens\":25,\"cache_write_tokens\":70}}}}\n\n"
	p, ok := feedStream(t, "codex", transcript)
	if !ok || p.CacheReadTokens != 25 || p.CacheCreationTokens != 70 {
		t.Fatalf("codex stream cache read/write = %+v, ok=%v", p, ok)
	}
}

func TestStreamScannerOpenAIChatCacheWriteTokens(t *testing.T) {
	transcript := "data: {\"model\":\"gpt-5.6\",\"usage\":{\"prompt_tokens\":90,\"completion_tokens\":4,\"total_tokens\":94,\"prompt_tokens_details\":{\"cached_tokens\":30,\"cache_write_tokens\":50}}}\n\n"
	p, ok := feedStream(t, "openai_chat", transcript)
	if !ok || p.CacheReadTokens != 30 || p.CacheCreationTokens != 50 {
		t.Fatalf("chat stream cache read/write = %+v, ok=%v", p, ok)
	}
}

func TestStreamScannerClaudeMessages(t *testing.T) {
	// A trimmed Anthropic /v1/messages SSE: message_start (input/cache) then
	// message_delta frames whose final output_tokens is the running total.
	transcript := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-opus-4-8","usage":{"input_tokens":200,"cache_read_input_tokens":50,"cache_creation_input_tokens":21,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"yo"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":88,"cache_creation_input_tokens":34}}`,
		``,
	}, "\n")
	p, ok := feedStream(t, "claude", transcript)
	if !ok {
		t.Fatal("expected usage from claude stream")
	}
	if p.Model != "claude-opus-4-8" || p.PromptTokens != 200 || p.CachedTokens != 50 {
		t.Fatalf("claude stream input/cache = %+v", p)
	}
	if p.CacheReadTokens != 50 || p.CacheCreationTokens != 34 {
		t.Fatalf("claude stream cache read/create = %+v", p)
	}
	if p.CacheMissTokens != 200 || p.CacheTotalInputTokens != 284 {
		t.Fatalf("claude stream cache miss/total input = %+v", p)
	}
	if p.CompletionTokens != 88 { // last message_delta wins
		t.Fatalf("claude stream output = %d, want 88 (final delta)", p.CompletionTokens)
	}
	if p.TotalTokens != 372 { // ordinary input + cache read + cache creation + output
		t.Fatalf("claude stream total = %d, want 372", p.TotalTokens)
	}
}

func TestStreamScannerOpenAIChatDeepSeekCacheUsage(t *testing.T) {
	transcript := strings.Join([]string{
		`data: {"id":"c1","model":"deepseek-chat","choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: {"id":"c1","model":"deepseek-chat","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`,
		``,
	}, "\n")
	p, ok := feedStream(t, "openai_chat", transcript)
	if !ok {
		t.Fatal("expected usage from deepseek chat stream")
	}
	if p.Model != "deepseek-chat" || p.PromptTokens != 100 || p.CompletionTokens != 7 || p.TotalTokens != 107 || p.CachedTokens != 64 {
		t.Fatalf("deepseek stream usage = %+v", p)
	}
	if !strings.Contains(string(p.RawUsage), `"prompt_cache_hit_tokens":64`) {
		t.Fatalf("raw usage should keep deepseek cache fields: %s", p.RawUsage)
	}
}

func TestStreamScannerNoUsageNoRecord(t *testing.T) {
	transcript := "event: ping\ndata: {\"type\":\"ping\"}\n\ndata: [DONE]\n\n"
	if _, ok := feedStream(t, "codex", transcript); ok {
		t.Fatal("a stream with no usage frame must report no usage")
	}
}

func TestStreamScannerFinalUsageWithoutNewline(t *testing.T) {
	p, ok := feedStream(t, "codex", `data: {"type":"response.completed","response":{"model":"gpt","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`)
	if !ok || p.TotalTokens != 5 {
		t.Fatalf("unterminated final usage lost: ok=%v parsed=%+v", ok, p)
	}
}
