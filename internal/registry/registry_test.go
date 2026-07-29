package registry

import "testing"

func TestLookupCurrentClaudeAdaptiveModels(t *testing.T) {
	for _, model := range []string{
		"claude-fable-5",
		"claude-opus-5[1m]",
		"claude-sonnet-5-thinking",
		"claude-opus-4.8",
	} {
		info := LookupModelInfo(model, "Claude")
		if info == nil || info.Thinking == nil || !info.Thinking.DynamicAllowed ||
			len(info.Thinking.Levels) == 0 || info.MaxCompletionTokens != 128000 {
			t.Fatalf("LookupModelInfo(%q) = %+v", model, info)
		}
	}
}

func TestLookupModelInfoReturnsDefensiveCopy(t *testing.T) {
	first := LookupModelInfo("claude-opus-5", "claude")
	if first == nil {
		t.Fatal("claude-opus-5 missing")
	}
	first.Thinking.Levels[0] = "mutated"
	second := LookupModelInfo("claude-opus-5", "claude")
	if second == nil || second.Thinking.Levels[0] != "low" {
		t.Fatalf("registry state was mutated: %+v", second)
	}
	if got := LookupModelInfo("claude-opus-5", "codex"); got != nil {
		t.Fatalf("cross-provider lookup returned %+v", got)
	}
}
