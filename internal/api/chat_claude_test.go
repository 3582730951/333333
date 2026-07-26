package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/streamrewrite"
)

func TestAnthropicStreamToChatSSEText(t *testing.T) {
	anthSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_9"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	anthropicStreamToChatSSE(rec, strings.NewReader(anthSSE), "claude-x", streamrewrite.New(nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("no chunk objects:\n%s", body)
	}
	if !strings.Contains(body, `"content":"Hel"`) || !strings.Contains(body, `"content":"lo"`) {
		t.Fatalf("text deltas not converted:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("missing finish_reason:\n%s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", body)
	}
}

func TestAnthropicStreamToChatSSEToolUse(t *testing.T) {
	anthSSE := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	anthropicStreamToChatSSE(rec, strings.NewReader(anthSSE), "claude-x", streamrewrite.New(nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `"get_weather"`) {
		t.Fatalf("tool_use not converted to tool_calls:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("missing tool_calls finish_reason:\n%s", body)
	}
}

func TestAnthropicStreamToChatSSEErrorIsNotReportedAsSuccessfulDone(t *testing.T) {
	anthSSE := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_err"}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}`,
		``,
		`event: error`,
		`data: {"type":"error","error":{"type":"api_error","message":"Kiro stream failed"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	rec := httptest.NewRecorder()
	anthropicStreamToChatSSE(rec, strings.NewReader(anthSSE), "gpt-5.6-sol", streamrewrite.New(nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"server_error"`) || !strings.Contains(body, publicRetryMessage) {
		t.Fatalf("stream error did not become a public terminal:\n%s", body)
	}
	if strings.Contains(body, "Kiro stream failed") {
		t.Fatalf("upstream stream error leaked:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") || strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("failed stream was incorrectly completed:\n%s", body)
	}
}
