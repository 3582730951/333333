package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A non-streaming bridge request whose upstream stream ends in response.failed takes
// the ResponsesToAnthropicResponse error branch, not the non-2xx branch. That branch
// must also produce an Anthropic envelope, otherwise Claude Code shows the same
// generic "API Error" for a terminal upstream failure.
func TestMessagesCodexTerminalFailureUsesAnthropicEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamStream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_term","model":"gpt-5.6-sol"}}` + "\n\n" +
			"event: response.failed\n" +
			`data: {"type":"response.failed","response":{"id":"resp_term","model":"gpt-5.6-sol","status":"failed","error":{"code":"server_error","message":"upstream gave up"}}}` + "\n\n"
		serveCodexResponsesFixture(t, w, r, upstreamStream)
	})
	h.importAccount(t, "messages-codex-term", "upstream-messages-codex-term", "access-messages-codex-term")

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

	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("terminal-failure body is not JSON: %v\n%s", err, raw)
	}
	// A successful message envelope would mean the failure was silently swallowed.
	if got["type"] == "message" {
		t.Fatalf("terminal failure became a success envelope: %s", raw)
	}
	if got["type"] != "error" {
		t.Fatalf("terminal failure lacks Anthropic envelope type=error (status=%d): %s", resp.StatusCode, raw)
	}
	errObj, ok := got["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("no error object: %s", raw)
	}
	errorType, _ := errObj["type"].(string)
	if !knownAnthropicErrorTypes[errorType] {
		t.Fatalf("error.type %q is not an Anthropic error type: %s", errorType, raw)
	}
	if message, _ := errObj["message"].(string); strings.TrimSpace(message) == "" {
		t.Fatalf("error.message must not be empty: %s", raw)
	}
}
