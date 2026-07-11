package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConvertAnthropicRequestStableAffinityAndLongTool(t *testing.T) {
	name := strings.Repeat("very_long_tool_", 8)
	raw := []byte(`{"model":"claude-opus-4-8-20260701","system":"be exact","messages":[{"role":"assistant","content":"prefill"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}]}],"tools":[{"name":"` + name + `","description":"d","input_schema":{"$defs":{"v":{"type":"string"}},"type":"object","properties":{"x":{"$ref":"#/$defs/v"}}}}]}`)
	a, err := ConvertAnthropicRequest(raw, "affinity-1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ConvertAnthropicRequest(raw, "affinity-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Body) != string(b.Body) {
		t.Fatal("conversion is not stable for the same affinity")
	}
	if a.Model != "claude-opus-4.8" || len(a.ToolNameMap) != 1 {
		t.Fatalf("conversion=%+v", a)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(a.Body, &root); err != nil {
		t.Fatal(err)
	}
	state := root["conversationState"].(map[string]interface{})
	if state["conversationId"] == "" || state["agentContinuationId"] == "" {
		t.Fatalf("state=%v", state)
	}
}
func TestConvertAnthropicRejectsNonClaude(t *testing.T) {
	if _, err := ConvertAnthropicRequest([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"x"}]}`), "a"); err == nil {
		t.Fatal("expected unsupported model")
	}
}

func TestConvertAnthropicPureWebSearchUsesMCPRequest(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":256,"messages":[{"role":"user","content":"Perform a web search for the query: Kiro latest release"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":4}]}`)
	got, err := ConvertAnthropicRequest(raw, "affinity")
	if err != nil {
		t.Fatal(err)
	}
	if got.WebSearch == nil || got.WebSearch.Query != "Kiro latest release" || got.WebSearch.ToolUseID == "" {
		t.Fatalf("web search conversion=%+v", got.WebSearch)
	}
}

func TestConvertAnthropicUsesNativeThinkingAndSystemHistory(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"Use only verified sources."}],"messages":[{"role":"user","content":"Draft the introduction."}]}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "paper-session", ConversionOptions{DefaultThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	fields, _ := root["additionalModelRequestFields"].(map[string]interface{})
	thinking, _ := fields["thinking"].(map[string]interface{})
	if thinking["type"] != "adaptive" {
		t.Fatalf("native thinking fields = %#v", fields)
	}
	state := root["conversationState"].(map[string]interface{})
	current := state["currentMessage"].(map[string]interface{})["userInputMessage"].(map[string]interface{})
	if current["content"] != "Draft the introduction." {
		t.Fatalf("current content polluted by compatibility prompts: %q", current["content"])
	}
	history := state["history"].([]interface{})
	if len(history) != 2 {
		t.Fatalf("system history length = %d, want 2: %#v", len(history), history)
	}
	first := history[0].(map[string]interface{})["userInputMessage"].(map[string]interface{})
	if first["content"] != "Use only verified sources." {
		t.Fatalf("system history content = %q", first["content"])
	}
	if strings.Contains(string(got.Body), "<system>") || strings.Contains(string(got.Body), "<thinking_mode>") {
		t.Fatalf("legacy prompt tags remain in Kiro request: %s", got.Body)
	}
}

func TestConvertAnthropicThinkingExplicitChoiceWinsDefault(t *testing.T) {
	disabled := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"x"}]}`)
	got, err := ConvertAnthropicRequestWithOptions(disabled, "a", ConversionOptions{DefaultThinking: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got.Body), "additionalModelRequestFields") {
		t.Fatalf("explicitly disabled thinking was re-enabled: %s", got.Body)
	}

	enabled := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"enabled","budget_tokens":12000},"messages":[{"role":"user","content":"x"}]}`)
	got, err = ConvertAnthropicRequestWithOptions(enabled, "a", ConversionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.Body), `"additionalModelRequestFields"`) || strings.Contains(string(got.Body), "<thinking_mode>") {
		t.Fatalf("explicit thinking did not use native Kiro fields: %s", got.Body)
	}
}
