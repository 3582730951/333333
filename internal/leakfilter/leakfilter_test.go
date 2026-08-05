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
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", status)
	}
	low := strings.ToLower(string(out))
	for _, leak := range []string{"usage limit", "switch to another model", "resets_at", "plan_type", "pro"} {
		if strings.Contains(low, leak) {
			t.Fatalf("neutralized body still leaks %q: %s", leak, out)
		}
	}
	if !strings.Contains(low, "server_error") || !strings.Contains(string(out), responsesPublicRetryMessage) {
		t.Fatalf("expected generic server error, got %s", out)
	}
}

func TestIsAuthoritativeCodexUsageLimitAcceptsOnlyFixedStructuredTerminals(t *testing.T) {
	positives := []struct {
		name string
		body string
	}{
		{name: "http_usage_limit_reached", body: `{"error":{"type":"usage_limit_reached","message":"terminal"}}`},
		{name: "http_usage_not_included", body: `{"error":{"type":"usage_not_included"}}`},
		{name: "websocket_usage_limit_reached", body: `{"type":"error","error":{"type":"usage_limit_reached"},"status_code":429}`},
		{name: "websocket_usage_not_included", body: `{"type":"error","error":{"type":"usage_not_included"},"status_code":429}`},
		{name: "sse_insufficient_quota", body: `{"type":"response.failed","response":{"error":{"code":"insufficient_quota","message":"terminal"}}}`},
		{name: "sse_usage_not_included", body: `{"type":"response.failed","response":{"error":{"code":"usage_not_included"}}}`},
	}
	for _, tc := range positives {
		t.Run(tc.name, func(t *testing.T) {
			if !IsAuthoritativeCodexUsageLimit(http.StatusTooManyRequests, []byte(tc.body)) {
				t.Fatalf("fixed terminal was not recognized: %s", tc.body)
			}
		})
	}

	negatives := []struct {
		name   string
		status int
		body   string
	}{
		{name: "fixed_code_at_503", status: http.StatusServiceUnavailable, body: `{"error":{"type":"usage_limit_reached"}}`},
		{name: "fixed_code_at_200", status: http.StatusOK, body: `{"error":{"type":"usage_limit_reached"}}`},
		{name: "generic_http_rate_limit", status: http.StatusTooManyRequests, body: `{"error":{"type":"rate_limit_exceeded"}}`},
		{name: "generic_sse_rate_limit", status: http.StatusTooManyRequests, body: `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`},
		{name: "message_pseudo_match", status: http.StatusTooManyRequests, body: `{"error":{"type":"server_error","message":"usage_limit_reached"}}`},
		{name: "nested_json_string_pseudo_match", status: http.StatusTooManyRequests, body: `{"error":{"message":"{\"error\":{\"type\":\"usage_limit_reached\"}}"}}`},
		{name: "http_wrong_code_field", status: http.StatusTooManyRequests, body: `{"error":{"code":"usage_limit_reached"}}`},
		{name: "http_insufficient_quota_is_not_fixed_type", status: http.StatusTooManyRequests, body: `{"error":{"type":"insufficient_quota"}}`},
		{name: "sse_wrong_type_field", status: http.StatusTooManyRequests, body: `{"type":"response.failed","response":{"error":{"type":"usage_not_included"}}}`},
		{name: "sse_usage_limit_reached_is_not_fixed_code", status: http.StatusTooManyRequests, body: `{"type":"response.failed","response":{"error":{"code":"usage_limit_reached"}}}`},
		{name: "wrong_outer_event", status: http.StatusTooManyRequests, body: `{"type":"response.error","response":{"error":{"code":"insufficient_quota"}}}`},
		{name: "root_lookalike", status: http.StatusTooManyRequests, body: `{"type":"usage_limit_reached"}`},
		{name: "case_changed_code", status: http.StatusTooManyRequests, body: `{"error":{"type":"USAGE_LIMIT_REACHED"}}`},
		{name: "trailing_json", status: http.StatusTooManyRequests, body: `{"error":{"type":"usage_limit_reached"}} {}`},
		{name: "invalid_json", status: http.StatusTooManyRequests, body: `{"error":`},
	}
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			if IsAuthoritativeCodexUsageLimit(tc.status, []byte(tc.body)) {
				t.Fatalf("non-authoritative signal was accepted: status=%d body=%s", tc.status, tc.body)
			}
		})
	}

	oversized := append([]byte(`{"error":{"type":"usage_limit_reached"},"padding":"`), bytes.Repeat([]byte("x"), 64<<10)...)
	oversized = append(oversized, []byte(`"}`)...)
	if IsAuthoritativeCodexUsageLimit(http.StatusTooManyRequests, oversized) {
		t.Fatal("oversized terminal envelope must not become an account-switch trigger")
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
	if !strings.Contains(string(out), `"type":"api_error"`) || !strings.Contains(string(out), responsesPublicRetryMessage) || strings.Contains(string(out), "overloaded_error") {
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

func TestSSEFilterNeutralizesResponseFailedLimit(t *testing.T) {
	stream := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"You've hit your usage limit.\"}}}\n\n"
	got := runSSE(t, "codex", stream, 5)
	if !strings.Contains(got, "event: response.failed") || !strings.Contains(got, `"code":"server_error"`) {
		t.Fatalf("limit response.failed frame was not neutralized: %q", got)
	}
	for _, leak := range []string{"rate_limit_exceeded", "usage limit"} {
		if strings.Contains(strings.ToLower(got), leak) {
			t.Fatalf("neutralized terminal leaked %q: %s", leak, got)
		}
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
	if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) || strings.Contains(got, "usage_limit_reached") {
		t.Fatalf("retryable WebSocket error was not safely terminated: %q", got)
	}
}

func TestRetryableCodexFailureFrameAcceptsResponseError(t *testing.T) {
	frame := []byte("event: response.error\n" +
		`data: {"type":"response.error","error":{"type":"server_error","message":"session blocked by risk control"},"status_code":503}` + "\n\n")
	failure, ok := ParseRetryableCodexFailureFrame(frame)
	if !ok || failure.EventType != "response.error" || failure.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response.error failure = %+v ok=%v", failure, ok)
	}
	if got := runSSE(t, "codex", string(frame), 3); !strings.Contains(got, "response.failed") || strings.Contains(got, "risk control") {
		t.Fatalf("retryable response.error frame was not safely terminated: %q", got)
	}
}

func TestParseCodexFailureFrameRecognizesStructuredContextLengthExceeded(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  int
	}{
		{
			name: "standard_nested_response_failed",
			frame: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp_full","status":"failed","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too many tokens"}}}` + "\n\n",
			want: http.StatusBadRequest,
		},
		{
			name:  "top_level_error",
			frame: "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"too many tokens\"}}\n\n",
			want:  http.StatusBadRequest,
		},
		{
			name:  "top_level_response_error_explicit_413",
			frame: "event: response.error\ndata: {\"type\":\"response.error\",\"status_code\":413,\"error\":{\"code\":\"context_length_exceeded\"}}\n\n",
			want:  http.StatusRequestEntityTooLarge,
		},
		{
			name:  "structured_error_overrides_retryable_status",
			frame: "event: response.failed\ndata: {\"type\":\"response.failed\",\"status\":503,\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"context_length_exceeded\",\"message\":\"server overloaded\"}}}\n\n",
			want:  http.StatusServiceUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			failure, ok := ParseCodexFailureFrame([]byte(tc.frame))
			if !ok || failure.StatusCode != tc.want || failure.RequestError != ResponsesRequestErrorContextLengthExceeded {
				t.Fatalf("failure=%+v ok=%v", failure, ok)
			}
			if failure.ContextError != ResponsesContextErrorNone || failure.BuiltinRetryable {
				t.Fatalf("request size error entered account/context recovery: %+v", failure)
			}
			if retryable, ok := ParseRetryableCodexFailureFrame([]byte(tc.frame)); ok {
				t.Fatalf("request size error became retryable: %+v", retryable)
			}
		})
	}
}

func TestParseCodexFailureFramePromotesStatuslessCyberPolicyWithoutRetry(t *testing.T) {
	frame := []byte("event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_policy","status":"failed","error":{"type":"invalid_request_error","code":"cyber_policy","message":"policy terminal; try a different model"}}}` + "\n\n")
	failure, ok := ParseCodexFailureFrame(frame)
	if !ok || failure.ErrorCode != "cyber_policy" || failure.StatusCode != http.StatusBadRequest {
		t.Fatalf("statusless cyber_policy failure=%+v ok=%v", failure, ok)
	}
	if failure.BuiltinRetryable || failure.RequestError != ResponsesRequestErrorNone || failure.ContextError != ResponsesContextErrorNone {
		t.Fatalf("cyber_policy entered account retry/context recovery: %+v", failure)
	}
	if retryable, retryOK := ParseRetryableCodexFailureFrame(frame); retryOK {
		t.Fatalf("cyber_policy became retryable: %+v", retryable)
	}
	got := runSSE(t, "codex", string(frame), 3)
	if !strings.Contains(got, `"code":"cyber_policy"`) || !strings.Contains(got, "try a different model") {
		t.Fatalf("cyber_policy terminal was altered: %q", got)
	}
}

func TestParseCodexFailureFrameContextLengthCodeMustBeExactAndStructured(t *testing.T) {
	frames := []string{
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"context_length_exceeded\"}}}\n\n",
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"context_length_exceeded_later\"}}}\n\n",
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"invalid_request_error\"}}}\n\n",
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n",
	}
	for _, frame := range frames {
		failure, ok := ParseCodexFailureFrame([]byte(frame))
		if !ok {
			t.Fatalf("terminal frame was not parsed: %s", frame)
		}
		if failure.RequestError != ResponsesRequestErrorNone || failure.StatusCode != 0 {
			t.Fatalf("unstructured/inexact code matched: %+v frame=%s", failure, frame)
		}
	}
}

func TestSSEFilterPreservesContextLengthExceededForCodexClient(t *testing.T) {
	frame := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"too many tokens"}}}` + "\n\n"
	got := runSSE(t, "codex", frame, 2)
	if !strings.Contains(got, `"code":"context_length_exceeded"`) || !strings.Contains(got, "event: response.failed") {
		t.Fatalf("Codex context signal was changed: %q", got)
	}
}

func TestSSEFilterDoesNotDropAssistantModelSwitchText(t *testing.T) {
	stream := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"You can switch to another model manually."}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_text","status":"completed"}}` + "\n\n"
	got := runSSE(t, "codex", stream, 1)
	if !strings.Contains(got, "switch to another model") || !strings.Contains(got, "response.completed") {
		t.Fatalf("ordinary assistant text was removed: %s", got)
	}
}

func TestSSEFilterNeutralizesContextFailureAsResponsesTerminal(t *testing.T) {
	frame := "event: error\r\n" +
		`data: {"type":"error","status":400,"error":{"type":"previous_response_not_found","message":"Previous response resp_private was not found."}}` + "\r\n\r\n"
	got := runSSE(t, "codex", frame, 1)
	if !strings.Contains(got, "event: response.failed") || !strings.Contains(got, `"code":"server_error"`) {
		t.Fatalf("context error did not become a Responses terminal: %q", got)
	}
	if strings.Contains(got, "previous_response_not_found") || strings.Contains(got, "resp_private") {
		t.Fatalf("context details leaked in terminal: %q", got)
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

func TestRetryableCodexFailureFrameAcceptsEncryptedFunctionOutputStatus(t *testing.T) {
	frame := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."},"status":400}` + "\n\n")
	failure, ok := ParseRetryableCodexFailureFrame(frame)
	if !ok || failure.StatusCode != http.StatusBadRequest || failure.ContextError != ResponsesContextErrorEncryptedFunctionOutput {
		t.Fatalf("encrypted function output failure = %+v ok=%v", failure, ok)
	}
}

func TestRetryableCodexFailureFrameAcceptsStatuslessEncryptedFunctionOutput(t *testing.T) {
	for _, frame := range [][]byte{
		[]byte("event: error\n" +
			`data: {"type":"error","error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."}}` + "\n\n"),
		[]byte("event: response.failed\n" +
			`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."}}}` + "\n\n"),
	} {
		failure, ok := ParseRetryableCodexFailureFrame(frame)
		if !ok || failure.StatusCode != http.StatusBadRequest || failure.ContextError != ResponsesContextErrorEncryptedFunctionOutput {
			t.Fatalf("statusless encrypted function output failure = %+v ok=%v frame=%s", failure, ok, frame)
		}
	}
	nearMatch := []byte("event: error\n" +
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded while parsing."}}` + "\n\n")
	if failure, ok := ParseCodexFailureFrame(nearMatch); !ok || failure.StatusCode != 0 || failure.ContextError != ResponsesContextErrorNone || failure.BuiltinRetryable {
		t.Fatalf("statusless near-match was promoted: %+v ok=%v", failure, ok)
	}
}

func TestRetryableCodexFailureFrameAcceptsWrappedOrphanStatusAndStatusCode(t *testing.T) {
	inner := `{"error":{"type":"invalid_request_error","message":"No tool call found for function call output with call_id call_wrapped."}}`
	for _, tc := range []struct {
		name, event, typ, field string
	}{
		{name: "http_sse_status", event: "response.failed", typ: "response.failed", field: "status"},
		{name: "websocket_status_code", event: "error", typ: "error", field: "status_code"},
		{name: "websocket_response_error", event: "response.error", typ: "response.error", field: "status_code"},
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
		{name: "websocket_response_error", event: "response.error", typ: "response.error", field: "status_code"},
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

func TestEncryptedFunctionOutputErrorRecognizesOnlyExactStructuredMessage(t *testing.T) {
	direct := []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"Encrypted function output content could not be decrypted or decoded."}}`)
	if got := DetectResponsesContextError(http.StatusBadRequest, direct); got != ResponsesContextErrorEncryptedFunctionOutput {
		t.Fatalf("encrypted function output error kind = %q", got)
	}

	inner, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": "Encrypted function output content could not be decrypted or decoded."},
	})
	wrapped, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": string(inner)},
	})
	if got := DetectResponsesContextError(http.StatusBadRequest, wrapped); got != ResponsesContextErrorEncryptedFunctionOutput {
		t.Fatalf("wrapped encrypted function output error kind = %q", got)
	}

	for _, body := range [][]byte{
		[]byte(`{"error":{"message":"Encrypted reasoning content could not be decrypted or decoded."}}`),
		[]byte(`{"error":{"message":"Encrypted function output content could not be decrypted or decoded while parsing."}}`),
	} {
		if got := DetectResponsesContextError(http.StatusBadRequest, body); got != ResponsesContextErrorNone {
			t.Fatalf("near-match error was classified as %q: %s", got, body)
		}
	}
	if got := DetectResponsesContextError(http.StatusServiceUnavailable, direct); got != ResponsesContextErrorNone {
		t.Fatalf("non-400 encrypted function output error was classified as %q", got)
	}
}

func TestResponsesContextErrorNeutralizationNeverLeaksObservedWrapper(t *testing.T) {
	const callID = "call_GQqnpD0cS3uxvXlgWBSD974z"
	body := []byte(`{"error":{"message":"{\n\"type\": \"error\",\n\"error\": {\n\"type\": \"invalid_request_error\",\n\"message\": \"No tool call found for custom tool call output with call_id ` + callID + `.\",\n\"param\": \"input\"\n},\n\"status\": 400\n}"},"status":400,"type":"error"}`)
	status, neutral, changed := NeutralizeResponsesContextErrorBody(http.StatusBadRequest, body)
	if !changed || status != http.StatusServiceUnavailable {
		t.Fatalf("neutralization changed=%v status=%d body=%s", changed, status, neutral)
	}
	for _, leak := range []string{"No tool call found", callID, `invalid_request_error`, `\"param\"`} {
		if bytes.Contains(neutral, []byte(leak)) {
			t.Fatalf("neutralized HTTP body leaked %q: %s", leak, neutral)
		}
	}
	if !bytes.Contains(neutral, []byte(`"type":"server_error"`)) {
		t.Fatalf("neutralized HTTP body is not a stable server error: %s", neutral)
	}

	frame := append([]byte("event: error\ndata: "), body...)
	frame = append(frame, []byte("\n\n")...)
	neutralFrame, changed := NeutralizeResponsesContextErrorSSEFrame(frame)
	if !changed {
		t.Fatalf("observed SSE wrapper was not neutralized: %s", frame)
	}
	for _, leak := range []string{"No tool call found", callID, `invalid_request_error`} {
		if bytes.Contains(neutralFrame, []byte(leak)) {
			t.Fatalf("neutralized SSE frame leaked %q: %s", leak, neutralFrame)
		}
	}
	if !bytes.Contains(neutralFrame, []byte(`event: response.failed`)) ||
		!bytes.Contains(neutralFrame, []byte(`"status":"failed"`)) ||
		!bytes.Contains(neutralFrame, []byte(`"code":"server_error"`)) {
		t.Fatalf("neutralized SSE frame is not a stable terminal error: %s", neutralFrame)
	}
	filtered := runSSE(t, "codex", string(frame), 7)
	if strings.Contains(filtered, "No tool call found") || strings.Contains(filtered, callID) || !strings.Contains(filtered, "server_error") {
		t.Fatalf("Codex SSE filter did not enforce context safety: %s", filtered)
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

func TestResponsesContextErrorRecognizesUnsupportedPreviousResponseID(t *testing.T) {
	inner := `{"detail":"Unsupported parameter:\nprevious_response_id"}`
	wrapped, _ := json.Marshal(map[string]interface{}{
		"error":  map[string]interface{}{"type": "upstream_error", "message": inner},
		"status": 400,
		"type":   "error",
	})
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{name: "direct_detail", body: []byte(inner)},
		{name: "message_wrapped_detail", body: wrapped},
		{name: "quoted_parameter", body: []byte(`{"error":{"message":"Unsupported parameter: 'previous_response_id'."}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectResponsesContextError(http.StatusBadRequest, tc.body); got != ResponsesContextErrorPreviousResponseNotFound {
				t.Fatalf("kind = %q, body=%s", got, tc.body)
			}
		})
	}
	for _, body := range [][]byte{
		[]byte(`{"detail":"Unsupported parameter: max_output_tokens"}`),
		[]byte(`{"detail":"Unsupported parameter: previous_response_id_extra"}`),
		[]byte(`{"detail":"Unsupported parameter: previous_response_id for this model"}`),
	} {
		if got := DetectResponsesContextError(http.StatusBadRequest, body); got != ResponsesContextErrorNone {
			t.Fatalf("unrelated unsupported parameter matched as %q: %s", got, body)
		}
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
		if !strings.Contains(got, `"type":"api_error"`) || !strings.Contains(got, responsesPublicRetryMessage) || strings.Contains(got, "rate_limit_error") {
			t.Fatalf("step=%d: expected generic api_error type, got %s", step, got)
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
	if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) {
		t.Fatalf("model-switch error lost its terminal: %s", got)
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
		if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) {
			t.Fatalf("step=%d: server-overload error lost its terminal: %s", step, got)
		}
	}
}

func TestSSEFilterDropsSlowDown(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"slow_down\",\"message\":\"Please slow down.\"}}}\n\n"
	got := runSSE(t, "codex", stream, 7)
	if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) || strings.Contains(got, "slow_down") {
		t.Fatalf("slow_down response.failed was not safely terminated: %q", got)
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
