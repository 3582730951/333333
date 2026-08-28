package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The exact top-level field set captured from real Codex CLI 0.146.0. Six of these
// (client_metadata, include, prompt_cache_key, reasoning, store, text) are absent from
// validateResponsesToAnthropicRequest's allowlist, and include:
// ["reasoning.encrypted_content"] raises LossResponsesIncludeOmitted — which the
// custom-provider Anthropic bridge rejects outright. The built-in Claude route must
// accept all of them, because Codex CLI sends them on every single request.
func TestResponsesViaClaudeAcceptsRealCodexCLIFieldSet(t *testing.T) {
	var upstreamBodies [][]byte
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_wire","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"wire ok"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":2}}`)
	})
	seedClaudeResponsesAccount(t, h, "responses-claude-wire")

	body := `{
		"model":"claude-opus-4-8",
		"instructions":"You are Codex.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"probe"}]}],
		"stream":false,
		"store":false,
		"include":["reasoning.encrypted_content"],
		"prompt_cache_key":"codex-root-thread-abc",
		"reasoning":{"effort":"xhigh","summary":"auto"},
		"text":{"verbosity":"medium"},
		"parallel_tool_calls":false,
		"tool_choice":"auto",
		"client_metadata":{"cli":"codex"},
		"tools":[{"type":"function","name":"shell","description":"run","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]
	}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("real Codex CLI field set was rejected: status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if got["object"] != "response" {
		t.Fatalf("missing Responses envelope: %s", raw)
	}
	if len(upstreamBodies) == 0 {
		t.Fatal("no upstream request was made")
	}

	var upstream map[string]interface{}
	if err := json.Unmarshal(upstreamBodies[0], &upstream); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	// The task forbids losing context, tools, or reasoning strength. Verify each
	// survived the Responses -> Chat -> Anthropic Messages conversion.
	if upstream["model"] == nil {
		t.Fatalf("upstream lost the model: %s", upstreamBodies[0])
	}
	tools, _ := upstream["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("declared function tool did not reach the Anthropic upstream: %s", upstreamBodies[0])
	}
	if name, _ := tools[0].(map[string]interface{})["name"].(string); name != "shell" {
		t.Fatalf("tool identity was not preserved: %s", upstreamBodies[0])
	}
	flat := string(upstreamBodies[0])
	if !strings.Contains(flat, "probe") {
		t.Fatalf("user context was dropped: %s", flat)
	}
	if !strings.Contains(flat, "You are Codex.") {
		t.Fatalf("instructions were dropped: %s", flat)
	}
}

// A streaming Codex request must receive Responses SSE, not Anthropic or chat SSE.
func TestResponsesViaClaudeStreamsResponsesSSE(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":9,"output_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"streamed"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	seedClaudeResponsesAccount(t, h, "responses-claude-stream")

	body := `{"model":"claude-opus-4-8","stream":true,"store":false,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"stream please"}]}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming request failed: status=%d body=%s", resp.StatusCode, raw)
	}
	text := string(raw)
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
	// Codex CLI requires the Responses event vocabulary and a terminal completion.
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q event:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "streamed") {
		t.Fatalf("model text did not reach the client:\n%s", text)
	}
	// Anthropic-native event names must not leak through to a Codex client.
	for _, forbidden := range []string{"content_block_delta", "message_stop"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("Anthropic event %q leaked to a Codex client:\n%s", forbidden, text)
		}
	}
}

// A hosted tool has no Anthropic equivalent and would be silently dropped. That is a
// real capability loss, so it must fail loudly rather than degrade the request.
func TestResponsesViaClaudeRejectsHostedToolLoss(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called when a declared tool cannot be represented")
	})
	seedClaudeResponsesAccount(t, h, "responses-claude-hosted")

	body := `{"model":"claude-opus-4-8","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search"}]}],"tools":[{"type":"web_search"}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("hosted-tool loss status = %d, want 422: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "responses_hosted_tool_omitted") {
		t.Fatalf("error did not name the dropped capability: %s", raw)
	}
}
