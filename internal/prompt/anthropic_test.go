package prompt

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatCompletionToAnthropic(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"temperature":0.5,"stream":true}`)
	out, err := ChatCompletionToAnthropic(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["system"] != "be brief" {
		t.Fatalf("system hoist: %v", m["system"])
	}
	if m["max_tokens"] == nil {
		t.Fatalf("max_tokens must be set (Anthropic requires it)")
	}
	if m["stream"] != true {
		t.Fatalf("stream not carried: %v", m["stream"])
	}
	msgs, _ := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("system message should not remain in messages: %v", msgs)
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "user" || first["content"] != "hi" {
		t.Fatalf("converted message: %v", first)
	}
}

func TestAnthropicToChatCompletion(t *testing.T) {
	raw := []byte(`{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hello"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":4}}`)
	out, err := AnthropicToChatCompletion(raw, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["object"] != "chat.completion" {
		t.Fatalf("object: %v", m["object"])
	}
	choice := m["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason mapping: %v", choice["finish_reason"])
	}
	if choice["message"].(map[string]interface{})["content"] != "hello" {
		t.Fatalf("content: %v", choice["message"])
	}
	if m["usage"].(map[string]interface{})["total_tokens"].(float64) != 14 {
		t.Fatalf("total_tokens: %v", m["usage"])
	}
}

func TestChatToolsAndToolCallsConversion(t *testing.T) {
	raw := []byte(`{"model":"claude-x","tools":[{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],"tool_choice":"required","messages":[{"role":"user","content":"weather?"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`)
	out, err := ChatCompletionToAnthropic(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tool0 := m["tools"].([]interface{})[0].(map[string]interface{})
	if tool0["name"] != "get_weather" || tool0["input_schema"] == nil {
		t.Fatalf("function tool not converted to Anthropic shape: %v", tool0)
	}
	if tc, _ := m["tool_choice"].(map[string]interface{}); tc["type"] != "any" {
		t.Fatalf("tool_choice required→any: %v", m["tool_choice"])
	}
	msgs := m["messages"].([]interface{})
	asst := msgs[1].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if asst["type"] != "tool_use" || asst["name"] != "get_weather" {
		t.Fatalf("assistant tool_calls→tool_use: %v", asst)
	}
	if inp, _ := asst["input"].(map[string]interface{}); inp["city"] != "SF" {
		t.Fatalf("tool_use input not parsed: %v", asst["input"])
	}
	tr := msgs[2].(map[string]interface{})["content"].([]interface{})[0].(map[string]interface{})
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "call_1" {
		t.Fatalf("tool message→tool_result: %v", tr)
	}
}

func TestChatCompletionToAnthropicPreservesMultimodalContent(t *testing.T) {
	raw := []byte(`{"model":"claude-x","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}},{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERi0="}}]}]}`)
	out, err := ChatCompletionToAnthropic(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]interface{})
	blocks, ok := msgs[0].(map[string]interface{})["content"].([]interface{})
	if !ok {
		t.Fatalf("multimodal content should be converted to Anthropic blocks, got %T: %v", msgs[0].(map[string]interface{})["content"], msgs[0].(map[string]interface{})["content"])
	}
	if blocks[0].(map[string]interface{})["text"] != "look" {
		t.Fatalf("text block not preserved: %v", blocks[0])
	}
	imgData := blocks[1].(map[string]interface{})
	if imgData["type"] != "image" {
		t.Fatalf("data image not converted: %v", imgData)
	}
	imgDataSrc := imgData["source"].(map[string]interface{})
	if imgDataSrc["type"] != "base64" || imgDataSrc["media_type"] != "image/png" || imgDataSrc["data"] != "AAAA" {
		t.Fatalf("data image source = %v", imgDataSrc)
	}
	imgURL := blocks[2].(map[string]interface{})["source"].(map[string]interface{})
	if imgURL["type"] != "url" || imgURL["url"] != "https://example.com/cat.jpg" {
		t.Fatalf("url image source = %v", imgURL)
	}
	doc := blocks[3].(map[string]interface{})
	if doc["type"] != "document" {
		t.Fatalf("file not converted to document: %v", doc)
	}
	docSrc := doc["source"].(map[string]interface{})
	if docSrc["type"] != "base64" || docSrc["media_type"] != "application/pdf" || docSrc["data"] != "JVBERi0=" {
		t.Fatalf("document source = %v", docSrc)
	}
}

func TestEnsureAnthropicCacheControl(t *testing.T) {
	// Compat-converted body: plain-string system + plain-string user content with
	// no cache_control anywhere → should gain a breakpoint at the system tail.
	// The latest user turn is volatile and must not be marked as a fallback; doing so
	// creates cache writes that rarely turn into reads when the next question differs.
	raw := []byte(`{"model":"claude-x","system":"big system","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}`)
	out := EnsureAnthropicCacheControl(raw, "")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys, ok := m["system"].([]interface{})
	if !ok || len(sys) == 0 || sys[0].(map[string]interface{})["cache_control"] == nil {
		t.Fatalf("no cache_control on system tail: %v", m["system"])
	}
	if content := m["messages"].([]interface{})[0].(map[string]interface{})["content"]; content != "hello" {
		t.Fatalf("latest user turn should remain unmarked and unchanged: %v", content)
	}
	diag := InspectAnthropicCacheControl(out)
	if diag.BreakpointCount != 1 || diag.LatestUserCacheControl {
		t.Fatalf("cache diagnostics = %+v, want one stable marker and no latest-user marker", diag)
	}

	// A body already at the 4-breakpoint limit must be left unchanged (never exceed).
	full := []byte(`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}},{"type":"text","text":"c","cache_control":{"type":"ephemeral"}}]}]}`)
	if n := cacheControlCount(t, EnsureAnthropicCacheControl(full, "")); n != 4 {
		t.Fatalf("must not exceed 4 breakpoints, got %d", n)
	}

	// The extended (1h) TTL is propagated onto injected breakpoints.
	out3 := EnsureAnthropicCacheControl([]byte(`{"system":"s","messages":[{"role":"user","content":"x"}]}`), "1h")
	if !strings.Contains(string(out3), `"ttl":"1h"`) {
		t.Fatalf("1h ttl not applied: %s", out3)
	}
}

func TestEnsureAnthropicCacheControlDoesNotMarkSingleVolatileUserTurn(t *testing.T) {
	raw := []byte(`{"model":"claude-x","messages":[{"role":"user","content":"latest volatile user turn"}]}`)
	out := EnsureAnthropicCacheControl(raw, "1h")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if got := cacheControlCount(t, out); got != 0 {
		t.Fatalf("single volatile user turn should not create cache writes, got %d markers: %s", got, out)
	}
	if content := m["messages"].([]interface{})[0].(map[string]interface{})["content"]; content != "latest volatile user turn" {
		t.Fatalf("latest user turn should remain unchanged: %v", content)
	}
}

func TestEnsureAnthropicCacheControlNormalizesTTLOrder(t *testing.T) {
	raw := []byte(`{"tools":[{"name":"Bash","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	out := EnsureAnthropicCacheControl(raw, "")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys := m["system"].([]interface{})[0].(map[string]interface{})
	if _, has := sys["cache_control"].(map[string]interface{})["ttl"]; has {
		t.Fatalf("system ttl should be downgraded after an earlier default/5m marker: %v", sys)
	}
	msg := m["messages"].([]interface{})[0].(map[string]interface{})
	block := msg["content"].([]interface{})[0].(map[string]interface{})
	if _, has := block["cache_control"].(map[string]interface{})["ttl"]; has {
		t.Fatalf("message ttl should be downgraded after an earlier default/5m marker: %v", block)
	}
}

func TestEnsureAnthropicCacheControlPreservesExistingMarker(t *testing.T) {
	raw := []byte(`{"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hello"}]}`)
	out := EnsureAnthropicCacheControl(raw, "")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys := m["system"].([]interface{})
	cc := sys[0].(map[string]interface{})["cache_control"].(map[string]interface{})
	if cc["ttl"] != "1h" {
		t.Fatalf("existing client marker was overwritten: %v", cc)
	}
	if n := cacheControlCount(t, out); n > 4 {
		t.Fatalf("must not exceed 4 breakpoints, got %d", n)
	}
}

func TestEnsureAnthropicCacheControlMarksToolsBeforeVolatileLastUser(t *testing.T) {
	raw := []byte(`{"system":"stable system","tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"latest volatile user turn"}]}`)
	out := EnsureAnthropicCacheControl(raw, "")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	tools := m["tools"].([]interface{})
	if _, ok := tools[len(tools)-1].(map[string]interface{})["cache_control"]; !ok {
		t.Fatalf("tool tail should carry a stable-prefix breakpoint: %v", tools[len(tools)-1])
	}
	content := m["messages"].([]interface{})[0].(map[string]interface{})["content"]
	if content != "latest volatile user turn" {
		t.Fatalf("last volatile user turn should remain unmarked and unchanged: %v", content)
	}
	if n := cacheControlCount(t, out); n != 2 {
		t.Fatalf("cache marker count = %d, want system+tools only", n)
	}
}

func TestEnsureAnthropicCacheControlMarksNativeClaudeStablePrefixes(t *testing.T) {
	raw := []byte(`{"model":"claude","system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.159.abc; cc_entrypoint=cli; cch=12345;"},` +
		`{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},` +
		`{"type":"text","text":"stable project instructions"}` +
		`],"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}}],` +
		`"messages":[` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>\nAs you answer the user's questions, you can use the following context:\n# repo\nstable files\n</system-reminder>\n\n"},` +
		`{"type":"text","text":"first real request"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":[{"type":"text","text":"different final question"}]}` +
		`]}`)
	out := EnsureAnthropicCacheControl(raw, "1h")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys := m["system"].([]interface{})
	if _, has := sys[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("billing block must not be marked: %v", sys[0])
	}
	if cc, ok := sys[2].(map[string]interface{})["cache_control"].(map[string]interface{}); !ok || cc["ttl"] != "1h" {
		t.Fatalf("last non-billing system block should be marked with 1h: %v", sys[2])
	}
	tools := m["tools"].([]interface{})
	if _, has := tools[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("only last tool should be marked: %v", tools[0])
	}
	if cc, ok := tools[1].(map[string]interface{})["cache_control"].(map[string]interface{}); !ok || cc["ttl"] != "1h" {
		t.Fatalf("last tool should be marked with 1h: %v", tools[1])
	}
	msgs := m["messages"].([]interface{})
	firstBlocks := msgs[0].(map[string]interface{})["content"].([]interface{})
	if cc, ok := firstBlocks[0].(map[string]interface{})["cache_control"].(map[string]interface{}); !ok || cc["ttl"] != "1h" {
		t.Fatalf("native auto-context prefix should be marked with 1h: %v", firstBlocks[0])
	}
	if cc, ok := firstBlocks[1].(map[string]interface{})["cache_control"].(map[string]interface{}); !ok || cc["ttl"] != "1h" {
		t.Fatalf("second-to-last user turn should be marked with 1h: %v", firstBlocks[1])
	}
	lastBlocks := msgs[2].(map[string]interface{})["content"].([]interface{})
	if _, has := lastBlocks[0].(map[string]interface{})["cache_control"]; has {
		t.Fatalf("volatile final user turn must not be marked: %v", lastBlocks[0])
	}
	if n := cacheControlCount(t, out); n != 4 {
		t.Fatalf("cache marker count = %d, want tool+system+auto-context+history", n)
	}
}

func TestEnsureAnthropicCacheControlMarksSecondToLastUserTurn(t *testing.T) {
	raw := []byte(`{"model":"claude","messages":[` +
		`{"role":"user","content":"first stable question"},` +
		`{"role":"assistant","content":[{"type":"text","text":"answer"}]},` +
		`{"role":"user","content":"final volatile question"}` +
		`]}`)
	out := EnsureAnthropicCacheControl(raw, "")
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]interface{})
	firstBlocks := msgs[0].(map[string]interface{})["content"].([]interface{})
	if _, ok := firstBlocks[0].(map[string]interface{})["cache_control"]; !ok {
		t.Fatalf("second-to-last user turn should be marked: %v", firstBlocks[0])
	}
	if last := msgs[2].(map[string]interface{})["content"]; last != "final volatile question" {
		t.Fatalf("final user turn should remain unmarked and unchanged: %v", last)
	}
}

func cacheControlCount(t *testing.T, body []byte) int {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return countCacheControl(m["system"]) + countCacheControl(m["tools"]) + countCacheControlMessages(m["messages"])
}

func TestAnthropicToolUseToChatToolCalls(t *testing.T) {
	raw := []byte(`{"id":"msg_1","model":"claude-x","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":2}}`)
	out, err := AnthropicToChatCompletion(raw, "claude-x")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	choice := m["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason: %v", choice["finish_reason"])
	}
	tc0 := choice["message"].(map[string]interface{})["tool_calls"].([]interface{})[0].(map[string]interface{})
	fn := tc0["function"].(map[string]interface{})
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "SF") {
		t.Fatalf("tool_use→tool_call: %v", tc0)
	}
}
