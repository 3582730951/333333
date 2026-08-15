package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/streamrewrite"
)

func TestResponsesStreamToChatSSERecognizesStableToolKinds(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_tools","model":"native"}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_custom","name":"apply_patch","input":""}}` + "\n\n" +
		"event: response.custom_tool_call_input.delta\n" +
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"hel"}` + "\n\n" +
		"event: response.custom_tool_call_input.delta\n" +
		`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"lo"}` + "\n\n" +
		"event: response.custom_tool_call_input.done\n" +
		`data: {"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_1","input":"hello"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"tool_search_call","id":"tsc_1","call_id":"call_search","execution":"client","arguments":{}}}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"tool_search_call","id":"tsc_1","call_id":"call_search","execution":"client","arguments":{"limit":900719925474099312345}}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_read","namespace":"filesystem","name":"read","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"item_id":"fc_1","delta":"{\"path\":\"a\"}"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_tools","status":"completed"}}` + "\n\n"

	recorder := httptest.NewRecorder()
	responsesStreamToChatSSE(recorder, strings.NewReader(stream), "native", false, streamrewrite.New(nil))

	names := map[int]string{}
	arguments := map[int]string{}
	emptyToolNameDeltas := 0
	finish := ""
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("decode Chat chunk: %v\n%s", err, line)
		}
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice := choices[0].(map[string]interface{})
		if value, ok := choice["finish_reason"].(string); ok {
			finish = value
		}
		delta, _ := choice["delta"].(map[string]interface{})
		calls, _ := delta["tool_calls"].([]interface{})
		for _, rawCall := range calls {
			call := rawCall.(map[string]interface{})
			index := int(call["index"].(float64))
			function, _ := call["function"].(map[string]interface{})
			if name, ok := function["name"].(string); ok {
				if name == "" {
					emptyToolNameDeltas++
				}
				names[index] = name
			}
			if part, ok := function["arguments"].(string); ok {
				arguments[index] += part
			}
		}
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q\n%s", finish, recorder.Body.String())
	}
	if emptyToolNameDeltas != 0 {
		t.Fatalf("argument deltas repeated an empty tool name %d times: %s", emptyToolNameDeltas, recorder.Body.String())
	}
	if names[0] != "apply_patch" || arguments[0] != `{"input":"hello"}` {
		t.Fatalf("custom tool = name %q args %q", names[0], arguments[0])
	}
	if names[1] != "tool_search" || arguments[1] != `{"limit":900719925474099312345}` {
		t.Fatalf("tool search = name %q args %q", names[1], arguments[1])
	}
	if !strings.HasPrefix(names[2], "filesystem__read_") || len(names[2]) > 64 || arguments[2] != `{"path":"a"}` {
		t.Fatalf("namespace function = name %q args %q", names[2], arguments[2])
	}
}
