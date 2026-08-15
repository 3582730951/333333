package capability

import "testing"

func TestRemoteFallbackModelsCannotShrinkBundledQualityContract(t *testing.T) {
	SetRemoteCodexModels(nil)
	t.Cleanup(func() { SetRemoteCodexModels(nil) })
	SetRemoteCodexModels([]RemoteCodexModel{
		{Slug: "gpt-5.6-sol", ContextWindow: 128000, MaxContextWindow: 128000, AutoCompactTokenLimit: 1, ReasoningLevels: []string{"low"}},
		{Slug: "gpt-new", ContextWindow: 256000, MaxContextWindow: 256000, ReasoningLevels: []string{"medium", "high"}},
	})
	bundled, ok := codexStaticModelForSlug("gpt-5.6-sol")
	if !ok {
		t.Fatal("bundled model disappeared")
	}
	if bundled.window != GPT56ContextWindow || bundled.maxWindow != GPT56ContextWindow {
		t.Fatalf("remote manifest shrank bundled context: %+v", bundled)
	}
	if bundled.autoCompactTokenLimit != GPT56AutoCompactTokenLimit {
		t.Fatalf("remote manifest forced premature compaction: %+v", bundled)
	}
	foundUltra := false
	for _, effort := range bundled.reasoningLevels {
		if effort == "ultra" {
			foundUltra = true
		}
	}
	if !foundUltra || !bundled.preferWebSocket || !bundled.responsesLite {
		t.Fatalf("remote manifest shrank bundled capabilities: %+v", bundled)
	}
	if added, ok := codexStaticModelForSlug("gpt-new"); !ok || added.window != 256000 {
		t.Fatalf("remote model was not added: %+v ok=%v", added, ok)
	}

	sources := make(map[string]string)
	for _, model := range StaticCodexModels("account-1") {
		sources[model.ModelSlug] = model.Source
	}
	if got := sources["gpt-5.6-sol"]; got != "codex_compatibility_manifest" {
		t.Fatalf("overridden model source = %q, want compatibility manifest", got)
	}
	if got := sources["gpt-new"]; got != "codex_compatibility_manifest" {
		t.Fatalf("added model source = %q, want compatibility manifest", got)
	}
	if got := sources["gpt-5.6-terra"]; got != "codex_static" {
		t.Fatalf("untouched bundled model source = %q, want codex_static", got)
	}
}
