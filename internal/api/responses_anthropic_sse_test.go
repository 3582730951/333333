package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/prompt"
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

func TestResponsesStreamToAnthropicSSESerializesParallelToolLifecycle(t *testing.T) {
	stream := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","arguments":""}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","input":""}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"tool_search_call","arguments":{}}}` + "\n\n" +
		"event: response.custom_tool_call_input.delta\n" +
		`data: {"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"patch"}` + "\n\n" +
		"event: response.custom_tool_call_input.done\n" +
		`data: {"type":"response.custom_tool_call_input.done","output_index":1,"input":"patch"}` + "\n\n" +
		"event: response.custom_tool_call_input.done\n" +
		`data: {"type":"response.custom_tool_call_input.done","output_index":1,"input":"patch"}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":\"partial\"}"}` + "\n\n" +
		"event: response.function_call_arguments.done\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}` + "\n\n" +
		"event: response.function_call_arguments.done\n" +
		`data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"path\":\"a.go\"}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"wire_read","arguments":"{\"path\":\"a.go\"}"}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"wire_read","arguments":"{\"path\":\"a.go\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_parallel","status":"completed","usage":{"input_tokens":3,"output_tokens":3},"output":[{"type":"function_call","output_index":0,"id":"fc_read","call_id":"call_read","name":"wire_read","arguments":"{\"path\":\"a.go\"}"},{"type":"custom_tool_call","output_index":1,"id":"ctc_patch","call_id":"call_patch","name":"apply_patch","input":"patch"},{"type":"tool_search_call","output_index":2,"id":"tsc_search","call_id":"call_search","arguments":{"limit":900719925474099312345}},{"type":"function_call","output_index":3,"id":"fc_write","call_id":"call_write","name":"wire_write","arguments":"{\"path\":\"b.go\",\"content\":\"ok\"}"}]}}` + "\n\n"

	recorder := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(
		recorder,
		strings.NewReader(stream),
		"gpt-5.6-sol",
		map[string]string{"wire_read": "Read", "wire_write": "Write"},
		nil,
		streamrewrite.New(nil),
	)
	got := recorder.Body.String()

	openIndex := -1
	nextIndex := 0
	starts := make([]string, 0, 4)
	ids := make([]string, 0, 4)
	stops := 0
	for _, frame := range strings.Split(got, "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if data == "" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("decode Anthropic SSE frame: %v\n%s", err, data)
		}
		switch event.Type {
		case "content_block_start":
			if openIndex >= 0 {
				t.Fatalf("content block %d started while block %d was open:\n%s", event.Index, openIndex, got)
			}
			if event.Index != nextIndex {
				t.Fatalf("content block index = %d, want unique sequential index %d:\n%s", event.Index, nextIndex, got)
			}
			openIndex = event.Index
			nextIndex++
			if event.Block.Type == "tool_use" {
				starts = append(starts, event.Block.Name)
				ids = append(ids, event.Block.ID)
			}
		case "content_block_delta":
			if openIndex != event.Index {
				t.Fatalf("delta for block %d while block %d was open:\n%s", event.Index, openIndex, got)
			}
		case "content_block_stop":
			if openIndex != event.Index {
				t.Fatalf("stop for block %d while block %d was open:\n%s", event.Index, openIndex, got)
			}
			openIndex = -1
			stops++
		}
	}
	if openIndex >= 0 {
		t.Fatalf("content block %d remained open:\n%s", openIndex, got)
	}
	if strings.Join(starts, ",") != "Read,apply_patch,tool_search,Write" {
		t.Fatalf("tool start order = %v, want registration order", starts)
	}
	if strings.Join(ids, ",") != "call_read,call_patch,call_search,call_write" {
		t.Fatalf("tool IDs = %v, want each call exactly once", ids)
	}
	if stops != 4 {
		t.Fatalf("content block stops = %d, want 4 (duplicate done events must be idempotent):\n%s", stops, got)
	}
	for _, want := range []string{
		`"partial_json":"{\"path\":\"a.go\"}"`,
		`"partial_json":"{\"input\":\"patch\"}"`,
		`"partial_json":"{\"limit\":900719925474099312345}"`,
		`"partial_json":"{\"path\":\"b.go\",\"content\":\"ok\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("serialized parallel tools missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `\"path\":\"partial\"`) || strings.Contains(got, `"name":""`) || strings.Contains(got, `"id":""`) {
		t.Fatalf("incomplete arguments or pre-hydration tool identity leaked:\n%s", got)
	}
}

func TestResponsesStreamToAnthropicSSEPreservesServerWebSearch(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_web","model":"gpt-5.6-sol"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_123","status":"in_progress"}}` + "\n\n" +
		"event: response.web_search_call.searching\n" +
		`data: {"type":"response.web_search_call.searching","item_id":"ws_123","output_index":0}` + "\n\n" +
		"event: response.web_search_call.completed\n" +
		`data: {"type":"response.web_search_call.completed","item_id":"ws_123","output_index":0}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"current weather"},"results":[{"title":"Forecast","url":"https://example.com/weather","page_age":"today","encrypted_content":"opaque-search-state"}]}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_123","status":"completed","action":{"type":"search","query":"current weather"}}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"It is sunny."}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_web","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

	recorder := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(recorder, strings.NewReader(stream), "gpt-5.6-sol", nil, nil, streamrewrite.New(nil))
	got := recorder.Body.String()
	toolUseID := prompt.ResponsesWebSearchToolUseID("ws_123")
	for _, want := range []string{
		`"type":"server_tool_use"`,
		`"id":"` + toolUseID + `"`,
		`"name":"web_search"`,
		`"partial_json":"{\"query\":\"current weather\"}"`,
		`"type":"web_search_tool_result"`,
		`"tool_use_id":"` + toolUseID + `"`,
		`"url":"https://example.com/weather"`,
		`"encrypted_content":"opaque-search-state"`,
		`"server_tool_use":{"web_search_requests":1}`,
		`"stop_reason":"end_turn"`,
		`"text":"It is sunny."`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("web search SSE missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, `"type":"server_tool_use"`) != 1 ||
		strings.Count(got, `"type":"web_search_tool_result"`) != 1 {
		t.Fatalf("duplicate web search events were not idempotent:\n%s", got)
	}
	if useAt, resultAt, textAt := strings.Index(got, `"type":"server_tool_use"`), strings.Index(got, `"type":"web_search_tool_result"`), strings.Index(got, `"text":"It is sunny."`); useAt < 0 || resultAt < useAt || textAt < resultAt {
		t.Fatalf("web search use/result/text ordering is invalid:\n%s", got)
	}
	assertAnthropicSSEBlockLifecycle(t, got)
}

func TestResponsesStreamToAnthropicSSEHydratesTerminalWebSearchWithClientTool(t *testing.T) {
	stream := "event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_read","call_id":"call_read","name":"Read","arguments":"{\"file_path\":\"a.go\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_terminal_web","status":"completed","usage":{"input_tokens":3,"output_tokens":2},"output":[{"type":"web_search_call","output_index":0,"id":"ws_terminal","status":"completed","action":{"type":"search","query":"release notes"}},{"type":"function_call","output_index":1,"id":"fc_read","call_id":"call_read","name":"Read","arguments":"{\"file_path\":\"a.go\"}"}]}}` + "\n\n"

	recorder := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(recorder, strings.NewReader(stream), "gpt-5.6-sol", nil, nil, streamrewrite.New(nil))
	got := recorder.Body.String()
	for _, want := range []string{
		`"type":"server_tool_use"`,
		`"partial_json":"{\"query\":\"release notes\"}"`,
		`"type":"web_search_tool_result"`,
		`"type":"tool_use"`,
		`"name":"Read"`,
		`"partial_json":"{\"file_path\":\"a.go\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("terminal web/client tool bridge missing %q:\n%s", want, got)
		}
	}
	assertAnthropicSSEBlockLifecycle(t, got)
}

func assertAnthropicSSEBlockLifecycle(t *testing.T, stream string) {
	t.Helper()
	openIndex := -1
	nextIndex := 0
	for _, frame := range strings.Split(stream, "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
				break
			}
		}
		if data == "" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("decode Anthropic SSE frame: %v\n%s", err, data)
		}
		switch event.Type {
		case "content_block_start":
			if openIndex >= 0 {
				t.Fatalf("content block %d started while %d remained open:\n%s", event.Index, openIndex, stream)
			}
			if event.Index != nextIndex {
				t.Fatalf("content block index=%d want=%d:\n%s", event.Index, nextIndex, stream)
			}
			openIndex = event.Index
			nextIndex++
		case "content_block_delta":
			if event.Index != openIndex {
				t.Fatalf("delta index=%d while open=%d:\n%s", event.Index, openIndex, stream)
			}
		case "content_block_stop":
			if event.Index != openIndex {
				t.Fatalf("stop index=%d while open=%d:\n%s", event.Index, openIndex, stream)
			}
			openIndex = -1
		}
	}
	if openIndex >= 0 {
		t.Fatalf("content block %d remained open:\n%s", openIndex, stream)
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

func TestResponsesStreamToAnthropicSSERecognizesStableToolKinds(t *testing.T) {
	stream := "event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_custom","name":"apply_patch","input":""}}` + "\n\n" +
		"event: response.custom_tool_call_input.delta\n" +
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"hello"}` + "\n\n" +
		"event: response.custom_tool_call_input.done\n" +
		`data: {"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_1","input":"hello"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"tool_search_call","id":"tsc_1","call_id":"call_search","execution":"client","arguments":{}}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"tool_search_call","id":"tsc_1","call_id":"call_search","execution":"client","arguments":{"limit":900719925474099312345}}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_read","namespace":"filesystem","name":"read","arguments":"{\"path\":\"a\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_tools","status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n"

	recorder := httptest.NewRecorder()
	responsesStreamToAnthropicSSE(recorder, strings.NewReader(stream), "gpt", nil, nil, streamrewrite.New(nil))
	got := recorder.Body.String()
	for _, want := range []string{
		`"name":"apply_patch"`,
		`"partial_json":"{\"input\":\"hello\"}"`,
		`"name":"tool_search"`,
		"900719925474099312345",
		`"name":"filesystem__read_`,
		`\"path\":\"a\"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stable Anthropic tool bridge missing %q:\n%s", want, got)
		}
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
