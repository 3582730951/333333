package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestMessagesRoutesGPTToBuiltInCodexNonStreaming(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("Codex bridge upstream path = %s", r.URL.Path)
		}
		upstreamStream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_msg_codex","model":"gpt-5.6-sol"}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"bridge ok"}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_msg_codex","model":"gpt-5.6-sol","usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14,"input_tokens_details":{"cached_tokens":4}}}}` + "\n\n"
		raw := serveCodexResponsesFixture(t, w, r, upstreamStream)
		if len(raw) > 0 {
			var body map[string]interface{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode Codex bridge request: %v", err)
			}
			if body["model"] != "gpt-5.6-sol" || body["stream"] != true {
				t.Fatalf("unexpected Responses request: %s", raw)
			}
			if body["store"] != false || body["instructions"] == nil {
				t.Fatalf("native Codex request defaults missing: %s", raw)
			}
			include, _ := body["include"].([]interface{})
			if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
				t.Fatalf("encrypted reasoning was not requested: %s", raw)
			}
			input, _ := body["input"].([]interface{})
			if len(input) != 1 || input[0].(map[string]interface{})["type"] != "message" {
				t.Fatalf("request passed through a Chat intermediate: %s", raw)
			}
			if _, present := body["max_output_tokens"]; present {
				t.Fatalf("ChatGPT Codex upstream rejects max_output_tokens: %s", raw)
			}
			reasoning, _ := body["reasoning"].(map[string]interface{})
			if reasoning["effort"] != "xhigh" {
				t.Fatalf("Claude Code effort missing from Responses request: %s", raw)
			}
			tools, _ := body["tools"].([]interface{})
			if len(tools) != 1 || tools[0].(map[string]interface{})["name"] != "read_file" {
				t.Fatalf("Anthropic function tool not converted to Responses: %s", raw)
			}
		}
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
	if usage["input_tokens"] != float64(8) || usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("Anthropic usage conversion wrong: %s", raw)
	}
}

func TestMessagesCodexBuiltInAgentInheritsParentModel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamStream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_agent_bridge","model":"gpt-5.6-sol"}}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_agent","call_id":"call_agent","name":"Agent","arguments":"{\"description\":\"scan repo\",\"prompt\":\"inspect files\",\"subagent_type\":\"general-purpose\",\"model\":\"haiku\"}"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_agent_bridge","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":4,"output_tokens":2}}}` + "\n\n"
		raw := serveCodexResponsesFixture(t, w, r, upstreamStream)
		var request map[string]interface{}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatalf("decode Agent bridge request: %v\n%s", err, raw)
		}
		tools := request["tools"].([]interface{})
		if len(tools) != 2 {
			t.Fatalf("Claude Code tools were dropped before Codex: %s", raw)
		}
		agentProperties := tools[0].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
		if _, leaked := agentProperties["model"]; leaked {
			t.Fatalf("built-in Agent.model reached Codex: %v", agentProperties)
		}
		workflowProperties := tools[1].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
		if _, ok := workflowProperties["model"]; !ok {
			t.Fatalf("unrelated Workflow.model was removed: %v", workflowProperties)
		}
	})
	h.importAccount(t, "messages-codex-agent", "upstream-messages-codex-agent", "access-messages-codex-agent")

	body := `{
	  "model":"gpt-5.6-sol",
	  "stream":false,
	  "messages":[{"role":"user","content":"delegate"}],
	  "tools":[
	    {
	      "name":"Agent",
	      "description":"Launch a new agent",
	      "input_schema":{
	        "type":"object",
	        "properties":{
	          "description":{"type":"string"},
	          "prompt":{"type":"string"},
	          "subagent_type":{"type":"string"},
	          "model":{"type":"string","enum":["sonnet","opus","haiku","fable"]},
	          "run_in_background":{"type":"boolean"}
	        },
	        "required":["description","prompt"],
	        "additionalProperties":false
	      }
	    },
	    {
	      "name":"Workflow",
	      "input_schema":{"type":"object","properties":{"name":{"type":"string"},"model":{"type":"string"}},"required":["name"]}
	    }
	  ]
	}`
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Agent bridge status=%d body=%s", resp.StatusCode, raw)
	}
	var message map[string]interface{}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("decode Agent bridge response: %v\n%s", err, raw)
	}
	content := message["content"].([]interface{})
	toolUse := content[0].(map[string]interface{})
	input := toolUse["input"].(map[string]interface{})
	if toolUse["name"] != "Agent" || input["prompt"] != "inspect files" || input["subagent_type"] != "general-purpose" {
		t.Fatalf("Agent tool call was damaged: %s", raw)
	}
	if _, leaked := input["model"]; leaked {
		t.Fatalf("Agent.model would force Claude Haiku instead of inheriting Codex: %s", raw)
	}
}

func TestMessagesRoutesGPTToBuiltInCodexStreamingTools(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("Codex bridge upstream path = %s", r.URL.Path)
		}
		upstreamStream := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_stream_bridge","model":"gpt-5.6-sol"}}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}` + "\n\n" +
			"event: response.reasoning_summary_text.delta\n" +
			`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"I inspected the input."}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"I inspected the input."}]}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"checking"}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":""}}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"path\":\"a.go\"}"}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"a.go\"}"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_stream_bridge","model":"gpt-5.6-sol","usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13}}}` + "\n\n"
		raw := serveCodexResponsesFixture(t, w, r, upstreamStream)
		if strings.Contains(string(raw), `"max_output_tokens"`) {
			t.Fatalf("ChatGPT Codex upstream rejects max_output_tokens: %s", raw)
		}
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
		`"type":"thinking"`,
		`"type":"signature_delta"`,
		`"signature":"pool-openai-reasoning-v1:`,
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

// serveCodexResponsesFixture supports both official Responses-over-WebSocket and
// SSE. gpt-5.6-sol selects WebSocket, so this also verifies that the native Claude
// bridge sends the complete Responses payload in the initial create frame.
func serveCodexResponsesFixture(t *testing.T, w http.ResponseWriter, r *http.Request, sse string) []byte {
	t.Helper()
	if !websocket.IsWebSocketUpgrade(r) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
		return raw
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		t.Errorf("upgrade Codex fixture websocket: %v", err)
		return nil
	}
	defer conn.Close()
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Errorf("read Codex Responses create frame: %v", err)
		return nil
	}
	for _, frame := range strings.Split(sse, "\n\n") {
		_, data := sseFrameEventData([]byte(frame))
		if len(data) == 0 {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			t.Errorf("write Codex Responses fixture event: %v", err)
			break
		}
	}
	return raw
}
