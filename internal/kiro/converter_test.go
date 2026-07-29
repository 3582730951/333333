package kiro

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/prompt"
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

func TestConvertAnthropicKiroGPTUsesPlainGenerationEnvelope(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"max_tokens":321,
		"thinking":{"type":"enabled","budget_tokens":2048},
		"output_config":{"effort":"max"},
		"messages":[{"role":"user","content":"hello"}]
	}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "gpt-affinity", ConversionOptions{
		DefaultThinking: true,
		ForceMaxQuality: true,
		ContextWindow:   272000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-5.6-sol" || got.ThinkingEnabled || got.ThinkingEffort != "" || got.MaxOutputTokens != 0 || got.ContextWindow != 272000 {
		t.Fatalf("GPT conversion envelope=%+v", got)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["additionalModelRequestFields"]; exists {
		t.Fatalf("GPT request carried Claude-only additional model fields: %s", got.Body)
	}
	if strings.Contains(string(got.Body), `"thinking"`) || strings.Contains(string(got.Body), `"output_config"`) || strings.Contains(string(got.Body), `"max_tokens"`) {
		t.Fatalf("GPT request carried Claude-only generation envelope: %s", got.Body)
	}
	if !strings.Contains(string(got.Body), `"modelId":"gpt-5.6-sol"`) {
		t.Fatalf("GPT model id was not retained: %s", got.Body)
	}
}

func TestConvertAnthropicUsesExactLiveCatalogDescriptor(t *testing.T) {
	raw := []byte(`{"model":"opus","messages":[{"role":"user","content":"hello"}]}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "catalog-affinity", ConversionOptions{
		ForceMaxQuality:           true,
		ContextWindow:             1_000_000,
		CatalogPublicModel:        "claude-opus-5",
		CatalogUpstreamModel:      "amazon.kiro.claude-opus-5-v1",
		MaxOutputTokens:           128_000,
		AdaptiveThinkingKnown:     true,
		AdaptiveThinkingSupported: true,
		EffortKnown:               true,
		MaxThinkingEffort:         "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "amazon.kiro.claude-opus-5-v1" || got.MaxOutputTokens != 128_000 || got.ThinkingEffort != "high" {
		t.Fatalf("conversion=%+v", got)
	}
	var root map[string]any
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	state := root["conversationState"].(map[string]any)
	current := state["currentMessage"].(map[string]any)["userInputMessage"].(map[string]any)
	if current["modelId"] != "amazon.kiro.claude-opus-5-v1" {
		t.Fatalf("wire model=%v body=%s", current["modelId"], got.Body)
	}
	fields := root["additionalModelRequestFields"].(map[string]any)
	if fields["max_tokens"] != float64(128_000) {
		t.Fatalf("catalog output limit was not authoritative: %v", fields)
	}
	output := fields["output_config"].(map[string]any)
	if output["effort"] != "high" {
		t.Fatalf("catalog effort was not authoritative: %v", output)
	}
}

func TestConvertAnthropicOpus5ThinkingAliasUsesExactGeneration5Contract(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-5-thinking","messages":[{"role":"user","content":"hello"}]}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "opus5-thinking", ConversionOptions{
		DefaultThinking: true,
		ForceMaxQuality: true,
		ContextWindow:   1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-5" || got.ContextWindow != 1_000_000 ||
		!got.ThinkingEnabled || got.MaxOutputTokens != 128_000 {
		t.Fatalf("Opus 5 thinking alias conversion=%+v", got)
	}
	var root map[string]any
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	fields := root["additionalModelRequestFields"].(map[string]any)
	thinking := fields["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" || strings.Contains(string(got.Body), "<thinking>") {
		t.Fatalf("Opus 5 did not use native adaptive thinking: %s", got.Body)
	}
	if canonical, ok := capability.KiroCanonicalModel("claude-opus-4-5-20251101"); !ok || canonical != "claude-opus-4.5" {
		t.Fatalf("Opus 5 mapping attracted Opus 4.5: %q %v", canonical, ok)
	}
}

func TestIsClaudeCodeCompactionRequestRequiresDedicatedSignature(t *testing.T) {
	instruction := claudeCodeCompactionInstruction + ", preserving technical details."
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "string system",
			raw:  `{"model":"claude-opus-4-8","system":"` + claudeCodeCompactionSystem + `","messages":[{"role":"user","content":"` + instruction + `"}]}`,
			want: true,
		},
		{
			name: "dedicated system survives changed instruction",
			raw:  `{"model":"claude-opus-4-8","system":"` + claudeCodeCompactionSystem + `","messages":[{"role":"user","content":"Summarize all earlier turns now."}]}`,
			want: true,
		},
		{
			name: "billing plus compaction system blocks",
			raw:  `{"model":"claude-opus-4-8","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215;"},{"type":"text","text":"` + claudeCodeCompactionSystem + `"}],"messages":[{"role":"assistant","content":"old"},{"role":"user","content":[{"type":"text","text":"` + instruction + `"}]}]}`,
			want: true,
		},
		{
			name: "ordinary user mentions compact",
			raw:  `{"model":"claude-opus-4-8","system":"You are Claude Code","messages":[{"role":"user","content":"` + instruction + `"}]}`,
		},
		{
			name: "cache sharing compaction markers",
			raw:  `{"model":"claude-opus-4-8","system":"You are Claude Code","messages":[{"role":"user","content":"` + instruction + `\n\n` + claudeCodeCompactionReminder + `"}]}`,
			want: true,
		},
		{
			name: "carried tool definitions do not hide compaction",
			raw:  `{"model":"claude-opus-4-8","system":"` + claudeCodeCompactionSystem + `","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"` + instruction + `"}]}`,
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsClaudeCodeCompactionRequest([]byte(test.raw)); got != test.want {
				t.Fatalf("compaction detection=%v want=%v raw=%s", got, test.want, test.raw)
			}
		})
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

func TestConvertAnthropicBudgetsForcedOutputAgainstRemainingContext(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"keep the complete current request"}]}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "budget", ConversionOptions{ForceMaxQuality: true, ContextWindow: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxOutputTokens >= 128_000 || got.MaxOutputTokens < kiroContextMinOutput {
		t.Fatalf("max output was not fitted to context: %+v", got)
	}
	if got.HistoryMessagesDropped != 0 || !containsString(got.CompatibilityLosses, LossMaxOutputReducedForContext) {
		t.Fatalf("current input should be preserved while output reserve shrinks: %+v", got)
	}
	if got.EstimatedInputTokens+got.MaxOutputTokens > got.ContextWindow {
		t.Fatalf("planned request exceeds window: input=%d output=%d window=%d", got.EstimatedInputTokens, got.MaxOutputTokens, got.ContextWindow)
	}
}

func TestConvertAnthropicRequiresCompactRatherThanDroppingHistory(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-8",
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("old-history-", 10_000)},
			map[string]any{"role": "assistant", "content": "old-answer"},
			map[string]any{"role": "user", "content": "recent-history"},
			map[string]any{"role": "assistant", "content": "recent-answer"},
			map[string]any{"role": "user", "content": "current-message"},
		},
	})
	_, err := ConvertAnthropicRequestWithOptions(raw, "trim-budget", ConversionOptions{ForceMaxQuality: true, ContextWindow: 30_000})
	var contextErr *ContextLengthError
	if !errors.As(err, &contextErr) || !strings.HasPrefix(err.Error(), claudeCodePromptTooLongPrefix) || !strings.Contains(err.Error(), "tokens > 30000") || strings.Contains(err.Error(), "请运行 /compact") {
		t.Fatalf("oversized history error = %v, want Claude Code auto-compact protocol", err)
	}
}

func TestConvertAnthropicOversizedCompactionRequestsAutomaticPartialRetry(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"model":  "claude-opus-4-8",
		"system": claudeCodeCompactionSystem,
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("old-history-", 10_000)},
			map[string]any{"role": "user", "content": claudeCodeCompactionInstruction},
		},
	})
	_, err := ConvertAnthropicRequestWithOptions(raw, "compact-budget", ConversionOptions{ForceMaxQuality: true, ContextWindow: 30_000, Compaction: true})
	var contextErr *ContextLengthError
	if !errors.As(err, &contextErr) || !contextErr.Compaction || !strings.HasPrefix(err.Error(), "Prompt is too long:") || !strings.Contains(err.Error(), "tokens > 30000") || !strings.Contains(err.Error(), "automatically retry compaction") || strings.Contains(err.Error(), "请运行 /compact") {
		t.Fatalf("oversized compaction error = %v", err)
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

func TestConvertAnthropicMapsCacheControlsToKiroCachePoints(t *testing.T) {
	raw := []byte(`{
  "model":"claude-sonnet-4-6",
  "system":[{"type":"text","text":"stable system","cache_control":{"type":"ephemeral","ttl":"1h"}}],
  "messages":[
    {"role":"user","content":[{"type":"text","text":"history","cache_control":{"type":"ephemeral"}}]},
    {"role":"assistant","content":"answer"},
    {"role":"user","content":[{"type":"text","text":"current","cache_control":{"type":"ephemeral"}}]}
  ],
  "tools":[{"name":"lookup","description":"d","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]
}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "cache-session", ConversionOptions{EnableCachePoints: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.CachePointCount != 4 || containsString(got.CompatibilityLosses, LossCacheControlNotForwarded) {
		t.Fatalf("cache conversion diagnostics = %+v", got)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	state := root["conversationState"].(map[string]interface{})
	current := state["currentMessage"].(map[string]interface{})["userInputMessage"].(map[string]interface{})
	if point, _ := current["cachePoint"].(map[string]interface{}); point["type"] != "default" || point["ttl"] != nil {
		t.Fatalf("current cachePoint = %#v", point)
	}
	tools := current["userInputMessageContext"].(map[string]interface{})["tools"].([]interface{})
	if len(tools) != 2 || tools[0].(map[string]interface{})["toolSpecification"] == nil || tools[1].(map[string]interface{})["cachePoint"] == nil {
		t.Fatalf("tool cachePoint was not inserted after its tool: %#v", tools)
	}
	history := state["history"].([]interface{})
	ack := history[1].(map[string]interface{})["assistantResponseMessage"].(map[string]interface{})
	if ack["cachePoint"] == nil {
		t.Fatalf("system cachePoint was not attached after acknowledgement: %#v", history)
	}
	historyUser := history[2].(map[string]interface{})["userInputMessage"].(map[string]interface{})
	if historyUser["cachePoint"] == nil {
		t.Fatalf("history block marker was not folded onto its Kiro message: %#v", historyUser)
	}
	if len(got.BodyWithoutCachePoints) == 0 || strings.Contains(string(got.BodyWithoutCachePoints), "cachePoint") {
		t.Fatalf("fallback body still contains cachePoint: %s", got.BodyWithoutCachePoints)
	}
	removed, changed, err := RemoveKiroCachePoints(got.Body)
	if err != nil {
		t.Fatal(err)
	}
	withoutPoints, err := canonicalJSON(got.BodyWithoutCachePoints)
	if err != nil {
		t.Fatal(err)
	}
	removedCanonical, err := canonicalJSON(removed)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || string(removedCanonical) != string(withoutPoints) {
		t.Fatalf("cachePoint insertion changed non-cache context:\nwith points removed=%s\nfallback=%s", removedCanonical, withoutPoints)
	}
}

func TestKiroRollingCachePlanningPreservesConvertedConversation(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4.8","system":"stable system","tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"large previous request"},{"role":"assistant","content":"previous response"},{"role":"user","content":"current request"}]}`)
	marked := prompt.EnsureAnthropicCacheControlWithOptions(raw, prompt.AnthropicCacheControlOptions{
		Policy: "max_hit", LatestTailWrite: true, PreferRecentTurnRead: true,
	})
	cached, err := ConvertAnthropicRequestWithOptions(marked, "rolling-context", ConversionOptions{
		ForceMaxQuality: true, EnableCachePoints: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := ConvertAnthropicRequestWithOptions(raw, "rolling-context", ConversionOptions{ForceMaxQuality: true})
	if err != nil {
		t.Fatal(err)
	}
	if cached.CachePointCount != 4 {
		t.Fatalf("cache points=%d breakpoints=%+v", cached.CachePointCount, cached.CachePointBreakpoints)
	}
	wantBreakpoints := []KiroCachePointBreakpoint{
		{Section: "tools", ToolIndex: 0}, {Section: "system"},
		{Section: "messages", MessageIndex: 0}, {Section: "messages", MessageIndex: 2},
	}
	if encoded, _ := json.Marshal(cached.CachePointBreakpoints); string(encoded) != mustJSON(t, wantBreakpoints) {
		t.Fatalf("breakpoints=%s want=%s", encoded, mustJSON(t, wantBreakpoints))
	}
	stripped, changed, err := RemoveKiroCachePoints(cached.Body)
	if err != nil {
		t.Fatal(err)
	}
	strippedCanonical, err := canonicalJSON(stripped)
	if err != nil {
		t.Fatal(err)
	}
	baselineCanonical, err := canonicalJSON(baseline.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || string(strippedCanonical) != string(baselineCanonical) {
		t.Fatalf("rolling cache markers changed Kiro context:\nstripped=%s\nbaseline=%s", strippedCanonical, baselineCanonical)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestConvertAnthropicCachePointCapUsesToolsSystemMessagesOrder(t *testing.T) {
	raw := []byte(`{
  "model":"claude-sonnet-4-6",
  "system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral"}}],
  "messages":[{"role":"user","content":[{"type":"text","text":"current","cache_control":{"type":"ephemeral"}}]}],
  "tools":[
    {"name":"a","cache_control":{"type":"ephemeral"}},
    {"name":"b","cache_control":{"type":"ephemeral"}},
    {"name":"c","cache_control":{"type":"ephemeral"}}
  ]
}`)
	got, err := ConvertAnthropicRequestWithOptions(raw, "cache-cap", ConversionOptions{EnableCachePoints: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.CachePointCount != 4 || !containsString(got.CompatibilityLosses, LossCacheControlNotForwarded) {
		t.Fatalf("cap diagnostics = count=%d losses=%v", got.CachePointCount, got.CompatibilityLosses)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got.Body, &root); err != nil {
		t.Fatal(err)
	}
	state := root["conversationState"].(map[string]interface{})
	current := state["currentMessage"].(map[string]interface{})["userInputMessage"].(map[string]interface{})
	if current["cachePoint"] != nil {
		t.Fatalf("messages must be dropped after tools+system consume four slots: %#v", current)
	}
	if count := strings.Count(string(got.Body), `"cachePoint"`); count != 4 {
		t.Fatalf("cachePoint count in body = %d: %s", count, got.Body)
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
