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

	forced, err := ConvertAnthropicRequestWithOptions(disabled, "a", ConversionOptions{ForceMaxQuality: true})
	if err != nil {
		t.Fatal(err)
	}
	var forcedBody map[string]interface{}
	if err := json.Unmarshal(forced.Body, &forcedBody); err != nil {
		t.Fatal(err)
	}
	fields := forcedBody["additionalModelRequestFields"].(map[string]interface{})
	thinking := fields["thinking"].(map[string]interface{})
	effort := fields["output_config"].(map[string]interface{})
	if thinking["type"] != "adaptive" || effort["effort"] != "max" || fields["max_tokens"] != float64(128000) {
		t.Fatalf("forced max-quality fields=%#v", fields)
	}
	if !forced.ThinkingEnabled || forced.ThinkingEffort != "max" || forced.MaxOutputTokens != 128000 || !containsString(forced.CompatibilityLosses, LossThinkingForcedAdaptive) {
		t.Fatalf("forced max-quality conversion=%+v", forced)
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

func TestConvertAnthropicPreservesPrefillThinkingUnknownBlocksAndLosses(t *testing.T) {
	raw := []byte(`{
  "model":"claude-opus-4-9-20270101",
  "system":[{"type":"text","text":" first "},{"type":"future_system","value":9007199254740993,"cache_control":{"type":"ephemeral"}}],
  "messages":[
    {"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"future_block","constant":9007199254740993},{"type":"tool_use","id":9007199254740993,"name":"tool","input":{"constant":9007199254740993}}]},
    {"role":"assistant","content":"tail prefill"}
  ],
  "tools":[{"name":"tool","description":"d","input_schema":{"type":"object","properties":{"n":{"const":9007199254740993}}}}],
  "output_config":{"format":{"type":"object","properties":{"n":{"const":9007199254740993}}}}
}`)
	got, err := ConvertAnthropicRequest(raw, "prefill-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-4.9" {
		t.Fatalf("concrete unknown model drifted to another version: %q", got.Model)
	}
	wantLosses := []string{LossAssistantFirstPadded, LossAssistantPrefillEmulated, LossCacheControlNotForwarded, LossOutputFormatPromptEmulated, LossThinkingHistoryTextualized, LossUnpairedToolUseTextualized, LossUnsupportedBlockTextualized}
	for _, want := range wantLosses {
		if !containsString(got.CompatibilityLosses, want) {
			t.Fatalf("missing loss %q in %v", want, got.CompatibilityLosses)
		}
	}
	body := string(got.Body)
	for _, want := range []string{`"content":"Continue."`, `\u003cthinking\u003e\nprivate\n\u003c/thinking\u003e`, `9007199254740993`, `tail prefill`, `future_block`} {
		if !strings.Contains(body, want) {
			t.Fatalf("converted body lost %q: %s", want, body)
		}
	}
}

func TestConvertAnthropicPairedToolIDsAndRecursiveSchemaAreStable(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"assistant","content":[{"type":"tool_use","id":9007199254740993,"name":"calc","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":9007199254740993,"is_error":true,"content":[{"type":"text","text":"failed"}]},{"type":"text","text":"continue"}]}],"tools":[{"name":"calc","description":"d","input_schema":{"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"}}}},"$ref":"#/$defs/node"}}]}`)
	got, err := ConvertAnthropicRequest(raw, "tool-session")
	if err != nil {
		t.Fatal(err)
	}
	body := string(got.Body)
	if !strings.Contains(body, `"toolUseId":"9007199254740993"`) || !strings.Contains(body, `"n":9007199254740993`) || !strings.Contains(body, `"status":"error"`) {
		t.Fatalf("tool identity/input/status was reconstructed lossily: %s", body)
	}
	if len(body) > 100_000 {
		t.Fatalf("recursive schema expansion was unbounded: %d bytes", len(body))
	}
	if !containsString(got.CompatibilityLosses, LossToolSchemaRefBounded) || strings.Contains(body, `"$ref"`) {
		t.Fatalf("recursive schema cycle was not bounded transparently: losses=%v body=%s", got.CompatibilityLosses, body)
	}
}

func TestConvertAnthropicWebSearchUsesLatestUserTurn(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"old query"},{"role":"assistant","content":"ok"},{"role":"user","content":"Perform a web search for the query: current query"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	got, err := ConvertAnthropicRequest(raw, "search-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.WebSearch == nil || got.WebSearch.Query != "current query" {
		t.Fatalf("web search query = %+v", got.WebSearch)
	}
}

func TestConvertAnthropicLongToolDescriptionRecordsHash(t *testing.T) {
	description := strings.Repeat("界", kiroToolDescriptionRunes+1)
	raw, _ := json.Marshal(map[string]any{"model": "claude-sonnet-4-6", "messages": []any{map[string]any{"role": "user", "content": "x"}}, "tools": []any{map[string]any{"name": "long", "description": description}}})
	got, err := ConvertAnthropicRequest(raw, "long-tool")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(got.CompatibilityLosses, LossToolDescriptionTruncated) || len(got.ToolDescriptionHashes["long"]) != 64 {
		t.Fatalf("truncation diagnostics missing: losses=%v hashes=%v", got.CompatibilityLosses, got.ToolDescriptionHashes)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
