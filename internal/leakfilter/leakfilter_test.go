package leakfilter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/streamrewrite"
)

func TestStripLeakHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Request-Id", "req_123")
	h.Set("x-codex-active-limit", "gpt-5.2")
	h.Set("x-codex-primary-used-percent", "98")
	h.Set("openai-model", "gpt-5.2-codex")
	h.Set("x-ratelimit-remaining-requests", "0")
	StripLeakHeaders(h)
	if h.Get("Content-Type") == "" || h.Get("X-Request-Id") == "" {
		t.Fatalf("non-leak headers were removed: %v", h)
	}
	for _, k := range []string{"x-codex-active-limit", "x-codex-primary-used-percent", "openai-model", "x-ratelimit-remaining-requests"} {
		if h.Get(k) != "" {
			t.Fatalf("leak header %q survived: %v", k, h)
		}
	}
}

func TestStripLeakHeadersAnthropic(t *testing.T) {
	h := http.Header{}
	// Benign Anthropic response headers a real client would also see — must pass.
	h.Set("anthropic-version", "2023-06-01")
	h.Set("request-id", "req_abc")
	// Per-account quota + org state — must be stripped (relay signal).
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "742")
	h.Set("anthropic-ratelimit-requests-reset", "2026-06-20T00:00:00Z")
	h.Set("anthropic-ratelimit-tokens-remaining", "98311")
	h.Set("anthropic-ratelimit-unified-remaining", "12")
	h.Set("anthropic-organization-id", "org_01ABC")
	StripLeakHeaders(h)
	if h.Get("anthropic-version") == "" || h.Get("request-id") == "" {
		t.Fatalf("benign Anthropic headers were removed: %v", h)
	}
	for _, k := range []string{
		"anthropic-ratelimit-requests-limit",
		"anthropic-ratelimit-requests-remaining",
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-remaining",
		"anthropic-ratelimit-unified-remaining",
		"anthropic-organization-id",
	} {
		if h.Get(k) != "" {
			t.Fatalf("Anthropic leak header %q survived: %v", k, h)
		}
	}
}

func TestNeutralizeErrorBodyCodexUsageLimit(t *testing.T) {
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"You've hit your usage limit. Switch to another model now.","resets_at":1704067242,"plan_type":"pro"}}`)
	status, out, changed := NeutralizeErrorBody("codex", 429, body)
	if !changed {
		t.Fatal("expected neutralization")
	}
	if status != 429 {
		t.Fatalf("expected 429, got %d", status)
	}
	low := strings.ToLower(string(out))
	for _, leak := range []string{"usage limit", "switch to another model", "resets_at", "plan_type", "pro"} {
		if strings.Contains(low, leak) {
			t.Fatalf("neutralized body still leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(low, "rate_limit_exceeded") {
		t.Fatalf("expected generic rate_limit_exceeded type, got %s", out)
	}
}

func TestNeutralizeErrorBodyClaudeOverloaded(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	status, out, changed := NeutralizeErrorBody("claude", 529, body)
	if !changed {
		t.Fatal("expected neutralization")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", status)
	}
	if !strings.Contains(string(out), `"overloaded_error"`) || !strings.Contains(string(out), `"type":"error"`) {
		t.Fatalf("expected anthropic error envelope, got %s", out)
	}
}

func TestNeutralizeErrorBodyGenericClientErrorUnchanged(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"messages: field required"}}`)
	_, out, changed := NeutralizeErrorBody("codex", 400, body)
	if changed {
		t.Fatalf("genuine client error must not be neutralized, got %s", out)
	}
	if !bytes.Equal(out, body) {
		t.Fatal("body should be unchanged")
	}
}

// chunkReader yields its payload in fixed-size reads to exercise the SSE
// filter's frame-boundary handling.
type chunkReader struct {
	data []byte
	step int
	pos  int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	end := c.pos + c.step
	if end > len(c.data) {
		end = len(c.data)
	}
	n := copy(p, c.data[c.pos:end])
	c.pos += n
	return n, nil
}

func runSSE(t *testing.T, provider string, stream string, step int) string {
	t.Helper()
	var out bytes.Buffer
	f := NewSSEFilter(provider, streamrewrite.New(nil))
	if err := f.Copy(&out, &chunkReader{data: []byte(stream), step: step}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return out.String()
}

func TestSSEFilterDropsCodexRateLimitFrames(t *testing.T) {
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello quota world\"}\n\n" +
		"data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"pro\",\"rate_limits\":{\"primary\":{\"used_percent\":99}}}\n\n" +
		"data: {\"type\":\"response.completed\"}\n\n"
	// Try several chunk sizes so frame boundaries land mid-frame.
	for _, step := range []int{1, 3, 7, 64, 4096} {
		got := runSSE(t, "codex", stream, step)
		if strings.Contains(got, "codex.rate_limits") || strings.Contains(got, "used_percent") {
			t.Fatalf("step=%d: rate-limit frame leaked: %s", step, got)
		}
		if !strings.Contains(got, "hello quota world") {
			t.Fatalf("step=%d: dropped legitimate content frame: %s", step, got)
		}
		if !strings.Contains(got, "response.completed") {
			t.Fatalf("step=%d: dropped terminal frame: %s", step, got)
		}
	}
}

func TestSSEFilterDropsResponseFailedLimit(t *testing.T) {
	stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"You've hit your usage limit.\"}}}\n\n"
	got := runSSE(t, "codex", stream, 5)
	if strings.TrimSpace(got) != "" {
		t.Fatalf("limit response.failed frame should be dropped, got %q", got)
	}
}

func TestRetryableCodexFailureFrameAcceptsWebSocketUsageLimitError(t *testing.T) {
	frame := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"},"status_code":429,"headers":{"X-Codex-Primary-Used-Percent":"100","X-Codex-Primary-Reset-After-Seconds":"12091"}}` + "\n\n")
	failure, ok := ParseRetryableCodexFailureFrame(frame)
	if !ok {
		t.Fatal("WebSocket type:error usage limit was not recognized")
	}
	if failure.EventType != "error" || failure.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("failure = %+v", failure)
	}
	if got := failure.Header.Get("X-Codex-Primary-Reset-After-Seconds"); got != "12091" {
		t.Fatalf("embedded reset header = %q", got)
	}
	if !strings.Contains(string(failure.Body), "The usage limit has been reached") {
		t.Fatalf("failure body = %s", failure.Body)
	}
	got := runSSE(t, "codex", string(frame), 7)
	if strings.TrimSpace(got) != "" {
		t.Fatalf("retryable WebSocket error frame should be filtered, got %q", got)
	}
}

func TestRetryableCodexFailureFrameRejectsWebSocketClientError(t *testing.T) {
	frame := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"invalid cache request"},"status_code":400}` + "\n\n")
	if failure, ok := ParseRetryableCodexFailureFrame(frame); ok {
		t.Fatalf("genuine client error must not be retried: %+v", failure)
	}
	failure, ok := ParseCodexFailureFrame(frame)
	if !ok || failure.StatusCode != http.StatusBadRequest || failure.BuiltinRetryable {
		t.Fatalf("client error must remain available to operator rules without becoming built-in retryable: %+v ok=%v", failure, ok)
	}
	if got := runSSE(t, "codex", string(frame), 5); !strings.Contains(got, "invalid cache request") {
		t.Fatalf("genuine client error was dropped: %q", got)
	}
}

func TestRetryableCodexFailureFrameAcceptsOrphanedToolOutputStatus(t *testing.T) {
	frame := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id call_orphan."},"status":400}` + "\n\n")
	failure, ok := ParseRetryableCodexFailureFrame(frame)
	if !ok {
		t.Fatal("orphaned tool output carried in status:400 was not recognized")
	}
	if failure.StatusCode != http.StatusBadRequest || failure.ContextError != ResponsesContextErrorOrphanedToolOutput {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRetryableCodexFailureFrameAcceptsWrappedOrphanStatusAndStatusCode(t *testing.T) {
	inner := `{"error":{"type":"invalid_request_error","message":"No tool call found for function call output with call_id call_wrapped."}}`
	for _, tc := range []struct {
		name, event, typ, field string
	}{
		{name: "http_sse_status", event: "response.failed", typ: "response.failed", field: "status"},
		{name: "websocket_status_code", event: "error", typ: "error", field: "status_code"},
	} {
		payload, _ := json.Marshal(map[string]interface{}{
			"type":   tc.typ,
			tc.field: 400,
			"error": map[string]interface{}{
				"type": "invalid_request_error", "message": inner,
			},
		})
		frame := append([]byte("event: "+tc.event+"\ndata: "), payload...)
		frame = append(frame, []byte("\n\n")...)
		failure, ok := ParseRetryableCodexFailureFrame(frame)
		if !ok || failure.StatusCode != http.StatusBadRequest || failure.ContextError != ResponsesContextErrorOrphanedToolOutput {
			t.Fatalf("%s wrapped orphan failure = %+v, ok=%v, payload=%s", tc.name, failure, ok, payload)
		}
	}
}

func TestRetryableCodexFailureFrameAcceptsPreviousResponseNotFoundStatusFields(t *testing.T) {
	for _, tc := range []struct {
		name, event, typ, field string
	}{
		{name: "http_sse_status", event: "response.failed", typ: "response.failed", field: "status"},
		{name: "websocket_status_code", event: "error", typ: "error", field: "status_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner, _ := json.Marshal(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type": "previous_response_not_found", "message": "Previous response resp_missing was not found.",
				},
				"status": 400,
			})
			payload, _ := json.Marshal(map[string]interface{}{
				"type":   tc.typ,
				tc.field: 400,
				"error": map[string]interface{}{
					"type": "upstream_error", "message": string(inner),
				},
			})
			frame := append([]byte("event: "+tc.event+"\ndata: "), payload...)
			frame = append(frame, []byte("\n\n")...)
			failure, ok := ParseRetryableCodexFailureFrame(frame)
			if !ok || failure.StatusCode != http.StatusBadRequest || failure.ContextError != ResponsesContextErrorPreviousResponseNotFound {
				t.Fatalf("failure = %+v, ok=%v, payload=%s", failure, ok, payload)
			}
		})
	}
}

func TestOrphanedToolOutputErrorRecognizesBoundedJSONWrappers(t *testing.T) {
	for _, kind := range []string{
		"function call output",
		"custom tool call output",
		"tool search output",
		"mcp tool call output",
	} {
		t.Run(kind, func(t *testing.T) {
			inner, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{"message": "No tool call found for " + kind + " with call_id call_1."},
			})
			outer, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{"message": string(inner)},
			})
			if !IsOrphanedToolCallOutputError(http.StatusBadRequest, outer) {
				t.Fatalf("wrapped %s was not recognized: %s", kind, outer)
			}
		})
	}

	inner := `{"error":{"message":"No tool call found for custom tool call output with call_id call_2."}}`
	for i := 0; i < 3; i++ {
		wrapped, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": inner}})
		inner = string(wrapped)
	}
	if IsOrphanedToolCallOutputError(http.StatusBadRequest, []byte(inner)) {
		t.Fatal("an error nested beyond two message wrappers must not match")
	}
	if IsOrphanedToolCallOutputError(http.StatusBadRequest, []byte(`{"error":{"message":"{malformed"}}`)) {
		t.Fatal("malformed nested JSON must not match")
	}
	if IsOrphanedToolCallOutputError(http.StatusBadRequest, []byte(`{"error":{"message":"No tool call found for another reason; function call output with call_id call_3."}}`)) {
		t.Fatal("a message that only mentions the target phrase later must not match")
	}
	oversized := append([]byte(`{"error":{"message":"No tool call found for function call output with call_id call_3.`), bytes.Repeat([]byte("x"), 64<<10)...)
	oversized = append(oversized, []byte(`"}}`)...)
	if IsOrphanedToolCallOutputError(http.StatusBadRequest, oversized) {
		t.Fatal("an error beyond the 64 KiB budget must not match")
	}
}

func TestOrphanedToolOutputErrorRecognizesObservedProxyWrapper(t *testing.T) {
	// This is the shape emitted by the downstream proxy in production: the actual
	// Codex error is JSON-encoded inside error.message, while the outer response
	// independently carries status/type.
	body := []byte(`{"error":{"message":"{\n\"type\": \"error\",\n\"error\": {\n\"type\": \"invalid_request_error\",\n\"message\": \"No tool call found for custom tool call output with call_id call_LhyrFDALDxT1GOWETYWfgfRz.\",\n\"param\": \"input\"\n},\n\"status\": 400\n}"},"status":400,"type":"error"}`)
	if !IsOrphanedToolCallOutputError(http.StatusBadRequest, body) {
		t.Fatalf("observed nested proxy error was not recognized: %s", body)
	}
}

func TestResponsesContextErrorRecognizesPreviousResponseNotFound(t *testing.T) {
	directType := []byte(`{"error":{"message":"Previous response resp_missing was not found.","type":"previous_response_not_found","param":"previous_response_id"}}`)
	directCode := []byte(`{"error":{"message":"Previous response resp_missing was not found.","type":"invalid_request_error","code":"previous_response_not_found"}}`)
	inner := `{"type":"error","error":{"type":"previous_response_not_found","message":"Previous response resp_missing was not found."},"status":400}`
	wrapped, _ := json.Marshal(map[string]interface{}{
		"error":  map[string]interface{}{"type": "upstream_error", "message": inner},
		"status": 400,
		"type":   "error",
	})
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "direct_type", body: directType},
		{name: "direct_code", body: directCode},
		{name: "message_wrapped", body: wrapped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectResponsesContextError(http.StatusBadRequest, tc.body); got != ResponsesContextErrorPreviousResponseNotFound {
				t.Fatalf("kind = %q, body=%s", got, tc.body)
			}
		})
	}
	if got := DetectResponsesContextError(http.StatusNotFound, directType); got != ResponsesContextErrorNone {
		t.Fatalf("non-400 status matched as %q", got)
	}
}

func TestResponsesContextErrorRejectsInvalidPreviousResponseErrors(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"error":{"type":"invalid_request_error","message":"previous_response_not_found"}}`),
		[]byte(`{"error":{"type":"previous_response_not_found_later","message":"missing"}}`),
		[]byte(`{"error":{"type":"invalid_request_error","code":"previous_response_not_found_extra","message":"missing"}}`),
		[]byte(`{"error":{"type":"invalid_request_error","param":"previous_response_id","message":"Previous response was invalid"}}`),
		[]byte(`{"error":{"message":"{malformed"}}`),
	} {
		if got := DetectResponsesContextError(http.StatusBadRequest, body); got != ResponsesContextErrorNone {
			t.Fatalf("invalid error matched as %q: %s", got, body)
		}
	}

	tooDeep := `{"error":{"type":"previous_response_not_found","message":"missing"}}`
	for i := 0; i < 3; i++ {
		wrapped, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": tooDeep}})
		tooDeep = string(wrapped)
	}
	if got := DetectResponsesContextError(http.StatusBadRequest, []byte(tooDeep)); got != ResponsesContextErrorNone {
		t.Fatalf("over-deep wrapper matched as %q", got)
	}

	oversized := append([]byte(`{"error":{"type":"previous_response_not_found","message":"`), bytes.Repeat([]byte("x"), 64<<10)...)
	oversized = append(oversized, []byte(`"}}`)...)
	if got := DetectResponsesContextError(http.StatusBadRequest, oversized); got != ResponsesContextErrorNone {
		t.Fatalf("oversized error matched as %q", got)
	}
}

func TestRetryableCodexFailureFrameRejectsUnrelatedStatus400(t *testing.T) {
	frame := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"No tool call supplied"},"status":400}` + "\n\n")
	if failure, ok := ParseRetryableCodexFailureFrame(frame); ok {
		t.Fatalf("unrelated status:400 must not be recoverable: %+v", failure)
	}
}

func TestSSEFilterNeutralizesClaudeError(t *testing.T) {
	// A Claude streaming error must be NEUTRALIZED (rewritten to a generic error
	// event), NOT dropped: dropping it leaves an empty/truncated 200 stream that
	// Claude Code reports as "empty or malformed response". The downstream must
	// still receive a well-formed `event: error` frame so it can render + retry.
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This account has hit its usage limit. Switch to another model.\"}}\n\n"
	for _, step := range []int{1, 9, 64, 4096} {
		got := runSSE(t, "claude", stream, step)
		if !strings.Contains(got, "message_start") {
			t.Fatalf("step=%d: dropped legitimate frame: %s", step, got)
		}
		// The error event survives (well-formed), so the stream is not empty.
		if !strings.Contains(got, "event: error") || !strings.Contains(got, "\"type\":\"error\"") {
			t.Fatalf("step=%d: claude error frame was dropped, leaving an empty stream: %q", step, got)
		}
		// ...but the account-revealing detail is gone, replaced by a generic message.
		low := strings.ToLower(got)
		for _, leak := range []string{"usage limit", "switch to another model"} {
			if strings.Contains(low, leak) {
				t.Fatalf("step=%d: neutralized error still leaks %q: %s", step, leak, got)
			}
		}
		if !strings.Contains(got, "rate_limit_error") {
			t.Fatalf("step=%d: expected generic rate_limit_error type, got %s", step, got)
		}
	}
}

func TestSSEFilterKeepsGenuineClaudeClientError(t *testing.T) {
	// A genuine client error mid-stream (invalid_request_error) carries no limit
	// signature and must pass through untouched so the caller sees their real error.
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages: field required\"}}\n\n"
	got := runSSE(t, "claude", stream, 7)
	if !strings.Contains(got, "invalid_request_error") {
		t.Fatalf("genuine claude client error was altered/dropped: %q", got)
	}
}

func TestSSEFilterDropsModelSwitchSuggestion(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"error\":{\"message\":\"You've hit your usage limit for gpt-5.2. Switch to another model now.\"}}\n\n"
	got := runSSE(t, "codex", stream, 11)
	if strings.Contains(strings.ToLower(got), "switch to another model") {
		t.Fatalf("model-switch suggestion leaked: %s", got)
	}
}

func TestSSEFilterScrubsSensitiveWordsInKeptFrames(t *testing.T) {
	words := streamrewrite.NewFromMap(map[string]string{"SECRET": "[redacted]"})
	var out bytes.Buffer
	f := NewSSEFilter("codex", words)
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"the SECRET value\"}\n\n"
	if err := f.Copy(&out, &chunkReader{data: []byte(stream), step: 6}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if strings.Contains(out.String(), "SECRET") || !strings.Contains(out.String(), "[redacted]") {
		t.Fatalf("sensitive word not scrubbed in kept frame: %s", out.String())
	}
}

func TestSSEFilterDropsServerOverloaded(t *testing.T) {
	// "Selected model is at capacity. Please try a different model." arrives as a
	// response.failed frame with error.code server_is_overloaded — a pool-internal
	// model-switch suggestion ("建议切换模型") that must never reach downstream.
	stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n"
	for _, step := range []int{1, 5, 64, 4096} {
		got := strings.ToLower(runSSE(t, "codex", stream, step))
		for _, leak := range []string{"at capacity", "try a different model", "server_is_overloaded"} {
			if strings.Contains(got, leak) {
				t.Fatalf("step=%d: server-overload suggestion leaked %q: %s", step, leak, got)
			}
		}
	}
}

func TestSSEFilterDropsSlowDown(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"slow_down\",\"message\":\"Please slow down.\"}}}\n\n"
	if got := strings.TrimSpace(runSSE(t, "codex", stream, 7)); got != "" {
		t.Fatalf("slow_down response.failed should be dropped, got %q", got)
	}
}

func TestSSEFilterDropsModelVerificationMetadata(t *testing.T) {
	// Model verification (trusted_access_for_cyber) rides inside a response.metadata
	// frame's openai_verification_recommendation — a per-account trust grant that
	// leaks pool state and must be dropped.
	stream := "data: {\"type\":\"response.metadata\",\"metadata\":{\"openai_verification_recommendation\":[\"trusted_access_for_cyber\"]}}\n\n"
	if got := strings.TrimSpace(runSSE(t, "codex", stream, 6)); got != "" {
		t.Fatalf("response.metadata verification frame should be dropped, got %q", got)
	}
}

func TestSSEFilterKeepsGenuineFailedClientError(t *testing.T) {
	// A response.failed that is the CLIENT's fault (invalid request) is not a pool
	// signal and must pass through so the caller sees their real error.
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_request_error\",\"message\":\"messages: field required\"}}}\n\n"
	if got := runSSE(t, "codex", stream, 9); !strings.Contains(got, "invalid_request_error") {
		t.Fatalf("genuine client error response.failed was dropped: %q", got)
	}
}

func TestSSEFilterKeepsContentMentioningModelCapacity(t *testing.T) {
	// Ordinary assistant content can legitimately contain phrases like "try a
	// different model" or "at capacity"; these must NOT be dropped — only the
	// error-context frames are (the unconditional drop is limited to the very
	// specific "switch to another model" usage-limit phrase).
	stream := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"If the GPU is at capacity you could try a different model.\"}\n\n"
	if got := runSSE(t, "codex", stream, 8); !strings.Contains(got, "try a different model") {
		t.Fatalf("legitimate content frame was dropped: %q", got)
	}
}

func TestNeutralizeErrorBodyServerOverloaded(t *testing.T) {
	// A non-streaming overload error (HTTP 503) carrying server_is_overloaded must
	// be neutralized to a generic, account-agnostic body.
	body := []byte(`{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`)
	_, out, changed := NeutralizeErrorBody("codex", 503, body)
	if !changed {
		t.Fatal("expected neutralization of server-overload body")
	}
	low := strings.ToLower(string(out))
	for _, leak := range []string{"at capacity", "try a different model", "server_is_overloaded"} {
		if strings.Contains(low, leak) {
			t.Fatalf("neutralized body still leaks %q: %s", leak, out)
		}
	}
}

func TestNeutralizeResponsesJSONScrubsSoftFailure(t *testing.T) {
	// A HTTP-200 responses object whose top-level status is "failed" with a
	// usage-limit reason must be neutralized (envelope-only inspection).
	body := []byte(`{"id":"resp_1","status":"failed","error":{"code":"usage_limit_reached","message":"You've reached your usage limit. Please switch models."},"output":[]}`)
	out, changed := NeutralizeResponsesJSON(body)
	if !changed {
		t.Fatal("expected neutralization of soft 200 limit failure")
	}
	low := strings.ToLower(string(out))
	for _, leak := range []string{"usage_limit_reached", "reached your", "switch models"} {
		if strings.Contains(low, leak) {
			t.Fatalf("neutralized responses body still leaks %q: %s", leak, out)
		}
	}
}

func TestNeutralizeResponsesJSONLeavesContentUntouched(t *testing.T) {
	// A genuinely completed response whose ASSISTANT OUTPUT happens to mention
	// "usage limit" / "switch models" must NOT be neutralized — only the envelope
	// is inspected, never generated content.
	body := []byte(`{"id":"resp_2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"To raise your usage limit, upgrade your plan or switch models."}]}]}`)
	out, changed := NeutralizeResponsesJSON(body)
	if changed {
		t.Fatalf("completed response with limit-words in content must be untouched, got %s", out)
	}
	if !bytes.Equal(out, body) {
		t.Fatal("body should be byte-identical")
	}
}
