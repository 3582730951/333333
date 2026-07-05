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

func TestResponsesRequestToChatCompletionRejectsUnsupportedResponsesTools(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "input":[{"role":"user","content":"search"}],
	  "tools":[{"type":"web_search_preview"}]
	}`)
	_, err := ResponsesRequestToChatCompletion(in)
	if err == nil {
		t.Fatal("expected explicit compatibility error for typed Responses tool")
	}
	if !strings.Contains(err.Error(), `unsupported Responses tool type "web_search_preview"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResponsesRequestToChatCompletionRejectsUnknownInputItems(t *testing.T) {
	in := []byte(`{
	  "model":"deepseek-chat",
	  "input":[
	    {"role":"user","content":"hi"},
	    {"type":"web_search_call","id":"ws_1","status":"completed"}
	  ]
	}`)
	_, err := ResponsesRequestToChatCompletion(in)
	if err == nil {
		t.Fatal("expected explicit compatibility error for unknown Responses input item")
	}
	if !strings.Contains(err.Error(), `unsupported Responses input item type "web_search_call"`) {
		t.Fatalf("unexpected error: %v", err)
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
