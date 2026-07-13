package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestMessagesRoutesGPTToBuiltInCodexNonStreaming(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("Codex bridge upstream path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode Codex bridge request: %v", err)
		}
		if body["model"] != "gpt-5.6-sol" || body["stream"] != true {
			t.Fatalf("unexpected Responses request: %s", raw)
		}
		reasoning, _ := body["reasoning"].(map[string]interface{})
		if reasoning["effort"] != "xhigh" {
			t.Fatalf("Claude Code effort missing from Responses request: %s", raw)
		}
		tools, _ := body["tools"].([]interface{})
		if len(tools) != 1 || tools[0].(map[string]interface{})["name"] != "read_file" {
			t.Fatalf("Anthropic function tool not converted to Responses: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.created\n"+
				`data: {"type":"response.created","response":{"id":"resp_msg_codex","model":"gpt-5.6-sol"}}`+"\n\n"+
				"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"bridge ok"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_msg_codex","model":"gpt-5.6-sol","usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14,"input_tokens_details":{"cached_tokens":4}}}}`+"\n\n")
	})
	h.importAccount(t, "messages-codex", "upstream-messages-codex", "access-messages-codex")

	body := `{"model":"gpt-5.6-sol","max_tokens":64000,"stream":false,"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"reply"}],"tools":[{"name":"read_file","description":"read","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Messages -> Codex status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("downstream Anthropic JSON invalid: %v\n%s", err, raw)
	}
	if got["type"] != "message" || got["model"] != "gpt-5.6-sol" || got["stop_reason"] != "end_turn" {
		t.Fatalf("Anthropic response envelope wrong: %s", raw)
	}
	content := got["content"].([]interface{})
	if content[0].(map[string]interface{})["text"] != "bridge ok" {
		t.Fatalf("Anthropic response text wrong: %s", raw)
	}
	usage, _ := got["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(12) || usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("Anthropic usage conversion wrong: %s", raw)
	}
}

func TestMessagesRoutesGPTToBuiltInCodexStreamingTools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("Codex bridge upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.created\n"+
				`data: {"type":"response.created","response":{"id":"resp_stream_bridge","model":"gpt-5.6-sol"}}`+"\n\n"+
				"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"checking"}`+"\n\n"+
				"event: response.output_item.added\n"+
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":""}}`+"\n\n"+
				"event: response.function_call_arguments.delta\n"+
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"path\":\"a.go\"}"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_stream_bridge","model":"gpt-5.6-sol","usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13}}}`+"\n\n")
	})
	h.importAccount(t, "messages-codex-stream", "upstream-messages-codex-stream", "access-messages-codex-stream")

	body := `{"model":"gpt-5.6-sol","max_tokens":64000,"stream":true,"output_config":{"effort":"high"},"messages":[{"role":"user","content":"inspect"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("streaming Messages -> Codex status=%d body=%s", resp.StatusCode, raw)
	}
	stream := string(raw)
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"checking"`,
		`"type":"tool_use"`,
		`"name":"read_file"`,
		`"partial_json":"{\"path\":\"a.go\"}"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("Anthropic stream missing %q:\n%s", want, stream)
		}
	}
	if strings.Contains(stream, "response.output_text.delta") || strings.Contains(stream, "chat.completion.chunk") {
		t.Fatalf("intermediate protocol leaked to Claude Code:\n%s", stream)
	}
}

func TestMessagesCodexCountTokensIsLocal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("count_tokens unexpectedly reached upstream: %s", r.URL.Path)
	})
	h.importAccount(t, "messages-codex-count", "upstream-messages-codex-count", "access-messages-codex-count")
	resp, err := http.Post(h.pool.URL+"/v1/messages/count_tokens", "application/json", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"count this"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["input_tokens"].(float64) <= 0 {
		t.Fatalf("local token estimate missing: %#v", got)
	}
}
