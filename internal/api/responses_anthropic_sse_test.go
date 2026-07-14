package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/streamrewrite"
)

func TestResponsesStreamToAnthropicSSEPreservesReasoningAndParallelTools(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_native","model":"gpt-5.6-sol"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"Reasoned."}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"Reasoned."}]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"wire_read","arguments":""}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"wire_grep","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_2","output_index":2,"delta":"{\"pattern\":\"TODO\"}"}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"file_path\":\"a.go\",\"pages\":\"\"}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"wire_read","arguments":"{\"file_path\":\"a.go\",\"pages\":\"\"}"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"wire_grep","arguments":"{\"pattern\":\"TODO\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_native","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"

	w := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(w, strings.NewReader(stream), "gpt-5.6-sol", map[string]string{
		"wire_read": "Read", "wire_grep": "Grep",
	}, nil, streamrewrite.New(nil))
	got := w.Body.String()
	for _, want := range []string{
		`"type":"thinking"`,
		`"type":"signature_delta"`,
		`"name":"Read"`,
		`"name":"Grep"`,
		`"partial_json":"{\"file_path\":\"a.go\"}"`,
		`"partial_json":"{\"pattern\":\"TODO\"}"`,
		`"cache_read_input_tokens":3`,
		`"input_tokens":7`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("native Anthropic SSE missing %q:\n%s", want, got)
		}
	}
}

func TestResponsesStreamToAnthropicSSERemovesBuiltInAgentModelOverride(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_agent","model":"gpt-5.6-sol"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_agent","call_id":"call_agent","name":"wire_agent","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_agent","output_index":0,"delta":"{\"description\":\"scan\",\"prompt\":\"inspect\",\"subagent_type\":\"general-purpose\",\"model\":\"haiku\"}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_agent","call_id":"call_agent","name":"wire_agent","arguments":"{\"description\":\"scan\",\"prompt\":\"inspect\",\"subagent_type\":\"general-purpose\",\"model\":\"haiku\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_agent","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n"

	w := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(
		w,
		strings.NewReader(stream),
		"gpt-5.6-sol",
		map[string]string{"wire_agent": "Agent"},
		map[string]bool{"Agent": true},
		streamrewrite.New(nil),
	)
	got := w.Body.String()
	if !strings.Contains(got, `"name":"Agent"`) || !strings.Contains(got, `\"subagent_type\":\"general-purpose\"`) {
		t.Fatalf("Agent tool call was not preserved:\n%s", got)
	}
	if strings.Contains(got, `\"model\":\"haiku\"`) {
		t.Fatalf("Agent model override leaked into Claude Code stream:\n%s", got)
	}
}

func TestCodexSSEToResponseJSONKeepsIncompleteAndFailedTerminals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{
			name: "incomplete",
			stream: "event: response.incomplete\n" +
				`data: {"type":"response.incomplete","response":{"id":"resp_i","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n",
			want: `"status":"incomplete"`,
		},
		{
			name: "failed",
			stream: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp_f","status":"failed","error":{"message":"bad input"}}}` + "\n\n",
			want: `"status":"failed"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(codexSSEToResponseJSON([]byte(tc.stream)))
			if !strings.Contains(got, tc.want) {
				t.Fatalf("terminal response was dropped: %s", got)
			}
		})
	}
}

func TestResponsesStreamToAnthropicSSEUsesRedactedThinkingWithoutSummary(t *testing.T) {
	stream := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_hidden","summary":[]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_hidden","encrypted_content":"opaque","summary":[]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_hidden","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	w := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(w, strings.NewReader(stream), "gpt-5.6-sol", nil, nil, streamrewrite.New(nil))
	got := w.Body.String()
	if !strings.Contains(got, `"type":"redacted_thinking"`) ||
		!strings.Contains(got, `"data":"pool-openai-reasoning-v1:`) ||
		strings.Contains(got, `"type":"signature_delta"`) {
		t.Fatalf("hidden reasoning did not use a replayable redacted block:\n%s", got)
	}
}

func TestCodexSSELedgerCombinesDoneItemsWithDeltaOnlyText(t *testing.T) {
	stream := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque","summary":[]}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"working"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"Read","arguments":"{}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":2,"output_tokens":2}}}` + "\n\n"
	response := codexSSEToResponseJSON([]byte(stream))
	var root map[string]interface{}
	if err := json.Unmarshal(response, &root); err != nil {
		t.Fatalf("decode ledger response: %v\n%s", err, response)
	}
	output := root["output"].([]interface{})
	if len(output) != 3 || output[0].(map[string]interface{})["type"] != "reasoning" ||
		output[1].(map[string]interface{})["type"] != "message" || output[2].(map[string]interface{})["type"] != "function_call" {
		t.Fatalf("delta text was not merged in Responses order: %v", output)
	}
}
