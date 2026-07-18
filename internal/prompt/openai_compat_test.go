package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustUnmarshal(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return m
}

func TestResponsesRequestToChatCompletion(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "instructions":"You are a coding agent.",
	  "stream":true,
	  "max_output_tokens":1024,
	  "input":[
	    {"role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"sunny"}
	  ],
	  "tools":[{"type":"function","name":"get_weather","description":"w","parameters":{"type":"object"}}],
	  "tool_choice":"auto"
	}`)
	out, err := ResponsesRequestToChatCompletion(in)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	if m["model"] != "deepseek-chat" || m["stream"] != true {
		t.Fatalf("model/stream wrong: %v", m)
	}
	if m["max_tokens"].(float64) != 1024 {
		t.Fatalf("max_tokens not mapped from max_output_tokens: %v", m["max_tokens"])
	}
	msgs := m["messages"].([]interface{})
	if len(msgs) != 4 { // system + user + assistant(tool_call) + tool
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Fatalf("instructions not hoisted to system: %v", msgs[0])
	}
	asst := msgs[2].(map[string]interface{})
	if asst["role"] != "assistant" || asst["tool_calls"] == nil {
		t.Fatalf("function_call not -> assistant tool_calls: %v", asst)
	}
	tool := msgs[3].(map[string]interface{})
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "sunny" {
		t.Fatalf("function_call_output not -> tool message: %v", tool)
	}
	tools := m["tools"].([]interface{})
	fn := tools[0].(map[string]interface{})["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Fatalf("tool not flattened to chat shape: %v", tools)
	}
}

func TestResponsesRequestToChatCompletionOmitsHostedResponsesTools(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "input":[{"role":"user","content":"search"}],
	  "tools":[{"type":"web_search_preview"}]
	}`)
	converted, err := ResponsesRequestToChatCompletionBridge(in)
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, converted.Body)
	if _, exists := root["tools"]; exists {
		t.Fatalf("hosted tool leaked into Chat request: %v", root["tools"])
	}
	if got := strings.Join(converted.CompatibilityLosses, ","); got != LossResponsesHostedToolOmitted {
		t.Fatalf("compatibility losses = %q", got)
	}
}

func TestResponsesRequestToChatCompletionPreservesUnknownHistoryAsJSON(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "input":[
	    {"role":"user","content":"hi"},
	    {"type":"web_search_call","id":"ws_1","status":"completed"}
	  ]
	}`)
	converted, err := ResponsesRequestToChatCompletionBridge(in)
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, converted.Body)
	messages := root["messages"].([]interface{})
	if len(messages) != 2 || !strings.Contains(messages[1].(map[string]interface{})["content"].(string), `"responses_history_item"`) {
		t.Fatalf("unknown history was not preserved as JSON: %v", messages)
	}
	if got := strings.Join(converted.CompatibilityLosses, ","); got != LossResponsesHistoryItemJSON {
		t.Fatalf("compatibility losses = %q", got)
	}
}

func TestResponsesRequestToChatCompletionOmitsIncludeWithDiagnostic(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "input":"hi",
	  "include":["web_search_call.results"]
	}`)
	converted, err := ResponsesRequestToChatCompletionBridge(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(converted.CompatibilityLosses, ","); got != LossResponsesIncludeOmitted {
		t.Fatalf("compatibility losses = %q", got)
	}
}

func TestChatCompletionToResponsesResponse(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-1","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`)
	out, err := ChatCompletionToResponsesResponse(in, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	if m["object"] != "response" || m["status"] != "completed" {
		t.Fatalf("envelope wrong: %v", m)
	}
	out2 := m["output"].([]interface{})
	if len(out2) != 2 {
		t.Fatalf("want message+function_call output, got %d: %v", len(out2), out2)
	}
	msg := out2[0].(map[string]interface{})
	if msg["type"] != "message" {
		t.Fatalf("first item not message: %v", msg)
	}
	if txt := msg["content"].([]interface{})[0].(map[string]interface{})["text"]; txt != "hello" {
		t.Fatalf("text not carried: %v", txt)
	}
	fc := out2[1].(map[string]interface{})
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["name"] != "f" {
		t.Fatalf("function_call item wrong: %v", fc)
	}
	u := m["usage"].(map[string]interface{})
	if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 3 {
		t.Fatalf("usage not mapped: %v", u)
	}
}

func TestChatCompletionToResponsesResponseMapsDeepSeekCacheUsage(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-1","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":3,"total_tokens":103,"prompt_cache_hit_tokens":64,"prompt_cache_miss_tokens":36}}`)
	out, err := ChatCompletionToResponsesResponse(in, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	u := m["usage"].(map[string]interface{})
	if u["input_tokens"].(float64) != 100 || u["output_tokens"].(float64) != 3 || u["total_tokens"].(float64) != 103 {
		t.Fatalf("usage totals not mapped: %v", u)
	}
	details := u["input_tokens_details"].(map[string]interface{})
	if details["cached_tokens"].(float64) != 64 {
		t.Fatalf("deepseek cache hit not mapped to cached_tokens: %v", u)
	}
	if u["prompt_cache_hit_tokens"].(float64) != 64 || u["prompt_cache_miss_tokens"].(float64) != 36 {
		t.Fatalf("deepseek cache fields not preserved: %v", u)
	}
}

func TestResponsesStableToolBridgePlanRoundTrip(t *testing.T) {
	in := []byte(`{
	  "model":"chat-model",
	  "input":"use tools",
	  "include":["reasoning.encrypted_content"],
	  "tools":[
	    {"type":"function","name":"plain","parameters":{"type":"object","properties":{"n":{"const":900719925474099312345}}}},
	    {"type":"namespace","name":"filesystem","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},
	    {"type":"namespace","name":"network","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},
	    {"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
	    {"type":"tool_search","execution":"client","description":"find tools","parameters":{"type":"object"}},
	    {"type":"tool_search","execution":"server"},
	    {"type":"web_search_preview"}
	  ],
	  "tool_choice":{"type":"custom","name":"apply_patch"}
	}`)
	converted, err := ResponsesRequestToChatCompletionBridge(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted.Body), "900719925474099312345") {
		t.Fatalf("large schema integer was rounded: %s", converted.Body)
	}
	wantLosses := strings.Join([]string{
		LossResponsesHostedToolOmitted,
		LossResponsesIncludeOmitted,
		LossResponsesServerToolSearchOmitted,
	}, ",")
	if got := strings.Join(converted.CompatibilityLosses, ","); got != wantLosses {
		t.Fatalf("compatibility losses = %q, want %q", got, wantLosses)
	}
	root := mustUnmarshal(t, converted.Body)
	tools := root["tools"].([]interface{})
	if len(tools) != 5 {
		t.Fatalf("Chat tools = %d, want 5: %v", len(tools), tools)
	}
	seen := map[string]bool{}
	for _, rawTool := range tools {
		name := rawTool.(map[string]interface{})["function"].(map[string]interface{})["name"].(string)
		if len(name) > 64 || seen[name] {
			t.Fatalf("invalid/colliding Chat tool name %q", name)
		}
		seen[name] = true
	}
	fsAlias, ok := converted.Plan.ChatName(ResponsesToolFunction, "filesystem", "read")
	if !ok {
		t.Fatal("filesystem namespace alias missing")
	}
	netAlias, _ := converted.Plan.ChatName(ResponsesToolFunction, "network", "read")
	if fsAlias == netAlias || len(fsAlias) > 64 || len(netAlias) > 64 {
		t.Fatalf("namespace aliases collided: %q %q", fsAlias, netAlias)
	}
	customAlias, _ := converted.Plan.ChatName(ResponsesToolCustom, "", "apply_patch")
	searchAlias, _ := converted.Plan.ChatName(ResponsesToolSearch, "", "tool_search")
	choice := root["tool_choice"].(map[string]interface{})["function"].(map[string]interface{})["name"]
	if choice != customAlias {
		t.Fatalf("custom tool choice = %v, want %q", choice, customAlias)
	}

	chatResponse := []byte(`{"id":"chat-tools","choices":[{"message":{"role":"assistant","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"plain","arguments":"{}"}},` +
		`{"id":"c2","type":"function","function":{"name":"` + fsAlias + `","arguments":"{\"path\":\"a\"}"}},` +
		`{"id":"c3","type":"function","function":{"name":"` + customAlias + `","arguments":"{\"input\":\"*** Begin Patch\"}"}},` +
		`{"id":"c4","type":"function","function":{"name":"` + searchAlias + `","arguments":"{\"limit\":900719925474099312345}"}}` +
		`]}}]}`)
	responses, err := ChatCompletionToResponsesResponse(chatResponse, "chat-model", converted.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(responses), "900719925474099312345") {
		t.Fatalf("large tool-search argument was rounded: %s", responses)
	}
	responseRoot := mustUnmarshal(t, responses)
	items := responseRoot["output"].([]interface{})
	if len(items) != 4 {
		t.Fatalf("Responses output = %v", items)
	}
	namespaced := items[1].(map[string]interface{})
	if namespaced["type"] != "function_call" || namespaced["namespace"] != "filesystem" || namespaced["name"] != "read" {
		t.Fatalf("namespace identity not restored: %v", namespaced)
	}
	custom := items[2].(map[string]interface{})
	if custom["type"] != "custom_tool_call" || custom["name"] != "apply_patch" || custom["input"] != "*** Begin Patch" {
		t.Fatalf("custom identity/input not restored: %v", custom)
	}
	search := items[3].(map[string]interface{})
	if search["type"] != "tool_search_call" || search["execution"] != "client" {
		t.Fatalf("tool-search identity not restored: %v", search)
	}
}

func TestResponsesLiteToolSearchAddsDiscoveredTools(t *testing.T) {
	in := []byte(`{
	  "model":"chat-model",
	  "tools":[{"type":"tool_search","execution":"client","parameters":{"type":"object"}}],
	  "input":[
	    {"type":"additional_tools","tools":[{"type":"function","name":"lite_tool","parameters":{"type":"object"}}]},
	    {"type":"tool_search_call","call_id":"search_1","execution":"client","arguments":{"query":"calendar"}},
	    {"type":"tool_search_output","call_id":"search_1","status":"completed","execution":"client","tools":[
	      {"type":"custom","name":"freeform"},
	      {"type":"namespace","name":"calendar","tools":[{"type":"function","name":"create","parameters":{"type":"object"}}]}
	    ]}
	  ]
	}`)
	converted, err := ResponsesRequestToChatCompletionBridge(in)
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, converted.Body)
	if tools := root["tools"].([]interface{}); len(tools) != 4 {
		t.Fatalf("top-level/Lite/discovered tools were not merged: %v", tools)
	}
	messages := root["messages"].([]interface{})
	if len(messages) != 2 || messages[0].(map[string]interface{})["role"] != "assistant" || messages[1].(map[string]interface{})["role"] != "tool" {
		t.Fatalf("tool-search history was not bridged: %v", messages)
	}
	content := messages[1].(map[string]interface{})["content"].(string)
	for _, want := range []string{`"responses_tool_output"`, `"status":"completed"`, `"execution":"client"`, `"freeform"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("structured tool-search output lost %q: %s", want, content)
		}
	}
	if got := strings.Join(converted.CompatibilityLosses, ","); got != LossResponsesStructuredToolOutputJSON {
		t.Fatalf("compatibility losses = %q", got)
	}
}

func TestResponsesForcedHostedToolChoiceDowngradesToAuto(t *testing.T) {
	converted, err := ResponsesRequestToChatCompletionBridge([]byte(`{
	  "model":"chat-model","input":"search",
	  "tools":[{"type":"web_search_preview"}],
	  "tool_choice":{"type":"web_search_preview"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, converted.Body)
	if root["tool_choice"] != "auto" {
		t.Fatalf("removed hosted tool choice = %v, want auto", root["tool_choice"])
	}
	want := LossResponsesHostedToolOmitted + "," + LossResponsesToolChoiceDowngraded
	if got := strings.Join(converted.CompatibilityLosses, ","); got != want {
		t.Fatalf("compatibility losses = %q, want %q", got, want)
	}
}

func TestAnthropicRequestToChatCompletion(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat","max_tokens":512,"stream":true,
	  "system":"sys prompt",
	  "messages":[
	    {"role":"user","content":"hello"},
	    {"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"tu_1","name":"f","input":{"a":1}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"result text"}]}
	  ],
	  "tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"auto"}
	}`)
	out, err := AnthropicRequestToChatCompletion(in)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	if m["max_tokens"].(float64) != 512 || m["stream"] != true {
		t.Fatalf("scalar fields wrong: %v", m)
	}
	msgs := m["messages"].([]interface{})
	// system + user + assistant(text+tool_call) + tool(result)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]interface{})["role"] != "system" {
		t.Fatalf("system not first: %v", msgs[0])
	}
	asst := msgs[2].(map[string]interface{})
	tcs := asst["tool_calls"].([]interface{})
	if asst["content"] != "calling" || len(tcs) != 1 {
		t.Fatalf("assistant text+tool_use wrong: %v", asst)
	}
	if tcs[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "f" {
		t.Fatalf("tool_use not -> tool_calls: %v", tcs)
	}
	toolMsg := msgs[3].(map[string]interface{})
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_1" || toolMsg["content"] != "result text" {
		t.Fatalf("tool_result not -> tool message: %v", toolMsg)
	}
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice auto not mapped: %v", m["tool_choice"])
	}
	tools := m["tools"].([]interface{})
	if tools[0].(map[string]interface{})["function"].(map[string]interface{})["name"] != "f" {
		t.Fatalf("anthropic tool not -> chat function tool: %v", tools)
	}
}

func TestAnthropicRequestToChatCompletionPreservesClaudeCodeEffort(t *testing.T) {
	in := []byte(`{
	  "model":"gpt-5.6-sol","max_tokens":64000,"stream":true,
	  "output_config":{"effort":"xhigh"},
	  "thinking":{"type":"adaptive"},
	  "messages":[{"role":"user","content":"solve it"}]
	}`)
	chat, err := AnthropicRequestToChatCompletion(in)
	if err != nil {
		t.Fatal(err)
	}
	chatRoot := mustUnmarshal(t, chat)
	if chatRoot["reasoning_effort"] != "xhigh" {
		t.Fatalf("Claude Code effort not mapped to Chat intermediate: %v", chatRoot)
	}
	streamOptions, _ := chatRoot["stream_options"].(map[string]interface{})
	if streamOptions["include_usage"] != true {
		t.Fatalf("stream usage option missing: %v", chatRoot)
	}

	responses, err := ChatCompletionToResponses(chat)
	if err != nil {
		t.Fatal(err)
	}
	responsesRoot := mustUnmarshal(t, responses)
	reasoning, _ := responsesRoot["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("Claude Code effort not mapped to Responses reasoning: %v", responsesRoot)
	}
	if _, leaked := responsesRoot["reasoning_effort"]; leaked {
		t.Fatalf("Chat-only reasoning_effort leaked to Responses: %v", responsesRoot)
	}
}

func TestAnthropicRequestToChatCompletionMapsLegacyThinkingBudget(t *testing.T) {
	in := []byte(`{"model":"gpt-5.6-sol","thinking":{"type":"enabled","budget_tokens":48000},"messages":[{"role":"user","content":"solve it"}]}`)
	out, err := AnthropicRequestToChatCompletion(in)
	if err != nil {
		t.Fatal(err)
	}
	root := mustUnmarshal(t, out)
	if root["reasoning_effort"] != "xhigh" {
		t.Fatalf("legacy thinking budget mapped to %v, want xhigh", root["reasoning_effort"])
	}
}

func TestAnthropicRequestToChatCompletionRejectsTypedServerTools(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "max_tokens":512,
	  "messages":[{"role":"user","content":"search"}],
	  "tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`)
	_, err := AnthropicRequestToChatCompletion(in)
	if err == nil {
		t.Fatal("expected explicit compatibility error for typed Claude server tool")
	}
	if !strings.Contains(err.Error(), `unsupported Claude tool type "web_search_20250305"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestChatCompletionToAnthropicResponse(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-2","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	out, err := ChatCompletionToAnthropicResponse(in, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "end_turn" {
		t.Fatalf("envelope wrong: %v", m)
	}
	content := m["content"].([]interface{})
	if content[0].(map[string]interface{})["text"] != "hi there" {
		t.Fatalf("text not carried: %v", content)
	}
	u := m["usage"].(map[string]interface{})
	if u["input_tokens"].(float64) != 7 || u["output_tokens"].(float64) != 2 {
		t.Fatalf("usage not mapped: %v", u)
	}
}

func TestChatCompletionToAnthropicResponseMapsDeepSeekCacheUsage(t *testing.T) {
	in := []byte(`{"id":"chatcmpl-2","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":70,"completion_tokens":2,"total_tokens":72,"prompt_cache_hit_tokens":50,"prompt_cache_miss_tokens":20}}`)
	out, err := ChatCompletionToAnthropicResponse(in, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	u := m["usage"].(map[string]interface{})
	if u["input_tokens"].(float64) != 70 || u["output_tokens"].(float64) != 2 {
		t.Fatalf("usage totals not mapped: %v", u)
	}
	if u["cache_read_input_tokens"].(float64) != 50 {
		t.Fatalf("deepseek cache hit not mapped to cache_read_input_tokens: %v", u)
	}
	if u["prompt_cache_hit_tokens"].(float64) != 50 || u["prompt_cache_miss_tokens"].(float64) != 20 {
		t.Fatalf("deepseek cache fields not preserved: %v", u)
	}
}

func TestChatCompletionToAnthropicResponseToolUse(t *testing.T) {
	in := []byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_3","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`)
	out, err := ChatCompletionToAnthropicResponse(in, "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshal(t, out)
	if m["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason not tool_use: %v", m["stop_reason"])
	}
	tu := m["content"].([]interface{})[0].(map[string]interface{})
	if tu["type"] != "tool_use" || tu["id"] != "call_3" || tu["name"] != "f" {
		t.Fatalf("tool_use block wrong: %v", tu)
	}
	if input, ok := tu["input"].(map[string]interface{}); !ok || input["a"].(float64) != 1 {
		t.Fatalf("tool input not parsed from arguments json: %v", tu["input"])
	}
}
