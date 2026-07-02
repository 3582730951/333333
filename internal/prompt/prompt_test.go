package prompt

import (
	"encoding/json"
	"testing"
)

func TestEnsureResponsesPromptCacheRetention(t *testing.T) {
	// Adds the field when absent.
	raw := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"hi"}]}`)
	got := EnsureResponsesPromptCacheRetention(raw, "24h")
	m := mustUnmarshal(t, got)
	if m["prompt_cache_retention"] != "24h" {
		t.Fatalf("retention = %v, want 24h", m["prompt_cache_retention"])
	}
	// A downstream-supplied value wins (no override).
	raw2 := []byte(`{"model":"gpt-5-codex","prompt_cache_retention":"in_memory","input":[]}`)
	got2 := EnsureResponsesPromptCacheRetention(raw2, "24h")
	m2 := mustUnmarshal(t, got2)
	if m2["prompt_cache_retention"] != "in_memory" {
		t.Fatalf("retention = %v, want in_memory (downstream wins)", m2["prompt_cache_retention"])
	}
	// Empty retention is a no-op (byte-identical).
	if got3 := EnsureResponsesPromptCacheRetention(raw, ""); string(got3) != string(raw) {
		t.Fatalf("empty retention must be a no-op")
	}
	// Model gating for the OpenAI families that currently support extended prompt
	// cache retention.
	for _, mdl := range []string{"gpt-5", "gpt-5-codex", "gpt-5.1", "gpt-5.1-codex-max", "gpt-5.5", "gpt-5.5-chat-latest", "gpt-4.1", "gpt-4.1-mini"} {
		if !SupportsExtendedPromptCache(mdl) {
			t.Fatalf("%s should support extended cache", mdl)
		}
	}
	for _, mdl := range []string{"gpt-4o", "gpt-4o-mini", "o3"} {
		if SupportsExtendedPromptCache(mdl) {
			t.Fatalf("%s should NOT support extended cache", mdl)
		}
	}
}

func TestResponsesEmptyPromptKeepsRawBody(t *testing.T) {
	raw := []byte(`{"model":"gpt","input":[{"role":"user","content":"hello"}]}`)
	got, changed, err := InjectResponsesSystemPrompt(raw, "")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if changed {
		t.Fatalf("changed = true, want false")
	}
	if string(got) != string(raw) {
		t.Fatalf("body changed:\n%s", got)
	}
}

func TestResponsesPromptPrependsInstructions(t *testing.T) {
	raw := []byte(`{"model":"gpt","instructions":"downstream","input":"hi"}`)
	got, changed, err := InjectResponsesSystemPrompt(raw, "cyber")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !changed {
		t.Fatalf("changed = false")
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("json: %v", err)
	}
	if root["instructions"] != "cyber\n\ndownstream" {
		t.Fatalf("instructions = %#v", root["instructions"])
	}
}

func TestChatPromptPrependsExistingSystem(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"system","content":"existing"},{"role":"user","content":"hi"}]}`)
	got, _, err := InjectChatSystemPrompt(raw, "cyber")
	if err != nil {
		t.Fatalf("inject: %v", err)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(got, &root)
	messages := root["messages"].([]interface{})
	first := messages[0].(map[string]interface{})
	if first["content"] != "cyber\n\nexisting" {
		t.Fatalf("system content = %#v", first["content"])
	}
}

func TestChatCompletionToResponsesPlainTextUnchanged(t *testing.T) {
	raw := []byte(`{"model":"gpt","messages":[{"role":"user","content":"hi"}],"n":2}`)
	out, err := ChatCompletionToResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(out, &root)
	if _, ok := root["messages"]; ok {
		t.Fatal("messages should be renamed to input")
	}
	if _, ok := root["n"]; ok {
		t.Fatal("n should be dropped")
	}
	input := root["input"].([]interface{})
	if len(input) != 1 || input[0].(map[string]interface{})["content"] != "hi" {
		t.Fatalf("plain text input not preserved: %v", input)
	}
}

func TestChatCompletionToResponsesTranslatesTools(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get","arguments":"{\"x\":1}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"42"}
	],"tools":[{"type":"function","function":{"name":"get","description":"d","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"get"}}}`)
	out, err := ChatCompletionToResponses(raw)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(out, &root)

	input := root["input"].([]interface{})
	var sawCall, sawOutput bool
	for _, it := range input {
		m := it.(map[string]interface{})
		switch m["type"] {
		case "function_call":
			if m["call_id"] == "call_1" && m["name"] == "get" && m["arguments"] == `{"x":1}` {
				sawCall = true
			}
		case "function_call_output":
			if m["call_id"] == "call_1" && m["output"] == "42" {
				sawOutput = true
			}
		}
	}
	if !sawCall || !sawOutput {
		t.Fatalf("tool_call/tool result not translated to Responses items: %v", input)
	}
	tool0 := root["tools"].([]interface{})[0].(map[string]interface{})
	if tool0["type"] != "function" || tool0["name"] != "get" || tool0["parameters"] == nil {
		t.Fatalf("tool not flattened to Responses shape: %v", tool0)
	}
	if _, nested := tool0["function"]; nested {
		t.Fatalf("tool should not keep nested function: %v", tool0)
	}
	tc := root["tool_choice"].(map[string]interface{})
	if tc["type"] != "function" || tc["name"] != "get" {
		t.Fatalf("tool_choice not flattened: %v", tc)
	}
}

func TestResponsesToChatCompletionSurfacesToolCalls(t *testing.T) {
	raw := []byte(`{"id":"resp_1","model":"gpt","output":[{"type":"function_call","call_id":"call_1","name":"get","arguments":"{\"x\":1}"}]}`)
	out, err := ResponsesToChatCompletion(raw, "", "gpt")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	_ = json.Unmarshal(out, &root)
	choice := root["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason should be tool_calls: %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]interface{})
	tcs := msg["tool_calls"].([]interface{})
	tc0 := tcs[0].(map[string]interface{})
	if tc0["id"] != "call_1" {
		t.Fatalf("tool_call id missing: %v", tc0)
	}
	if tc0["function"].(map[string]interface{})["name"] != "get" {
		t.Fatalf("tool_call name missing: %v", tc0)
	}
}
