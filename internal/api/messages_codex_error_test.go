package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Claude Code parses a failure as Anthropic's error envelope: a top-level
// "type":"error" alongside an "error" object. A raw Responses/OpenAI error body has
// no such top-level type, so forwarding it verbatim makes the CLI report a generic
// "API Error" instead of the upstream reason.
func TestMessagesCodexUpstreamErrorUsesAnthropicEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported parameter: reasoning.effort","type":"invalid_request_error","code":"unsupported_parameter"}}`))
	})
	h.importAccount(t, "messages-codex-err", "upstream-messages-codex-err", "access-messages-codex-err")

	body := `{"model":"gpt-5.6-sol","max_tokens":1024,"stream":false,"messages":[{"role":"user","content":"reply"}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("upstream 400 must not become 200: body=%s", raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("downstream error body is not JSON: %v\n%s", err, raw)
	}
	if got["type"] != "error" {
		t.Fatalf("missing Anthropic error envelope type=error: %s", raw)
	}
	errObj, ok := got["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing error object: %s", raw)
	}
	if _, ok := errObj["type"].(string); !ok {
		t.Fatalf("error.type must be a string: %s", raw)
	}
	if message, _ := errObj["message"].(string); strings.TrimSpace(message) == "" {
		t.Fatalf("error.message must not be empty: %s", raw)
	}
}

// A mid-stream upstream failure must still terminate the Anthropic stream with an
// error event. Without it Claude Code sits on an unterminated message_start and
// reports "API Error" when the connection closes.
func TestMessagesCodexStreamingUpstreamErrorTerminatesAnthropicStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached","type":"rate_limit_error"}}`))
	})
	h.importAccount(t, "messages-codex-stream-err", "upstream-messages-codex-stream-err", "access-messages-codex-stream-err")

	body := `{"model":"gpt-5.6-sol","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"reply"}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	text := string(raw)
	if resp.StatusCode == http.StatusOK && strings.Contains(text, "text/event-stream") {
		t.Fatalf("unexpected success envelope: %s", text)
	}
	// Either a non-2xx Anthropic error envelope, or an SSE stream carrying an
	// Anthropic error event, is acceptable. A body with neither leaves the CLI stuck.
	hasErrorEvent := strings.Contains(text, `"type":"error"`) || strings.Contains(text, `"type": "error"`)
	if !hasErrorEvent {
		t.Fatalf("streaming failure carried no Anthropic error event (status=%d): %s", resp.StatusCode, text)
	}
}
