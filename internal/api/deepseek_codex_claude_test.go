package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestDeepSeekCodexClaudeRewrite(t *testing.T) {
	cases := []struct {
		model    string
		want     string
		rewrite  bool
	}{
		{"gpt-5.6", "deepseek-chat", true},
		{"gpt-5.6-sol", "deepseek-chat", true},
		{"gpt-4o", "deepseek-chat", true},
		{"claude-sonnet-4-5", "deepseek-chat", true},
		{"claude-haiku-4-5", "deepseek-chat", true},
		{"claude-opus-4-6", "deepseek-reasoner", true},
		{"claude-3-opus", "deepseek-reasoner", true},
		{"o4-mini", "deepseek-reasoner", true},
		{"deepseek-chat", "", false},
		{"deepseek-reasoner", "", false},
		{"some-custom-model", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := deepseekCodexClaudeRewrite(tc.model)
		if ok != tc.rewrite || got != tc.want {
			t.Fatalf("deepseekCodexClaudeRewrite(%q) = (%q,%v), want (%q,%v)", tc.model, got, ok, tc.want, tc.rewrite)
		}
	}
}

func TestProviderIsDeepSeek(t *testing.T) {
	deepSeek := func(models []string, mappings map[string]string) storage.CustomProvider {
		return storage.CustomProvider{ID: "deepseek", Name: "DeepSeek", Models: models, ModelMappings: mappings}
	}
	if !providerIsDeepSeek(deepSeek([]string{"deepseek-chat", "deepseek-reasoner"}, nil)) {
		t.Fatal("id deepseek must be detected")
	}
	if !providerIsDeepSeek(storage.CustomProvider{ID: "ds-relay", Models: []string{"deepseek-chat"}}) {
		t.Fatal("all-DeepSeek model set must be detected")
	}
	if !providerIsDeepSeek(storage.CustomProvider{ID: "relay", ModelMappings: map[string]string{"gpt-5.6": "deepseek-chat"}}) {
		t.Fatal("DeepSeek mapping value must be detected")
	}
	if providerIsDeepSeek(storage.CustomProvider{ID: "openrouter", Models: []string{"deepseek-chat", "gpt-5.6"}}) {
		t.Fatal("mixed model set must not be treated as a DeepSeek provider")
	}
	if providerIsDeepSeek(storage.CustomProvider{ID: "anthropic", Models: []string{"claude-sonnet-4-5"}}) {
		t.Fatal("non-DeepSeek provider detected as DeepSeek")
	}
}

func TestApplyCustomProviderModelMappingDeepSeekBuiltinFallback(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6","input":[]}`)
	deepSeek := storage.CustomProvider{ID: "deepseek", Models: []string{"deepseek-chat", "deepseek-reasoner"}}

	// Stock codex model with no operator mapping → built-in rewrite to deepseek-chat.
	body, model, mapped := applyCustomProviderModelMapping(deepSeek, raw, "gpt-5.6")
	if !mapped || model != "deepseek-chat" {
		t.Fatalf("gpt-5.6 = (%q,%v), want deepseek-chat", model, mapped)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil || payload["model"] != "deepseek-chat" {
		t.Fatalf("body model not rewritten: %v (%s)", err, body)
	}

	// Claude opus → deepseek-reasoner.
	if _, model, mapped = applyCustomProviderModelMapping(deepSeek, raw, "claude-opus-4-6"); !mapped || model != "deepseek-reasoner" {
		t.Fatalf("claude-opus = (%q,%v), want deepseek-reasoner", model, mapped)
	}

	// Already-DeepSeek model passes through unchanged.
	_, model, mapped = applyCustomProviderModelMapping(deepSeek, raw, "deepseek-chat")
	if mapped {
		t.Fatalf("deepseek-chat should pass through unmapped, got (%q,%v)", model, mapped)
	}

	// Operator mapping wins over the built-in table.
	mappedProvider := storage.CustomProvider{ID: "deepseek", Models: []string{"deepseek-chat"}, ModelMappings: map[string]string{"gpt-5.6": "deepseek-reasoner"}}
	_, model, mapped = applyCustomProviderModelMapping(mappedProvider, raw, "gpt-5.6")
	if !mapped || model != "deepseek-reasoner" {
		t.Fatalf("operator mapping ignored: (%q,%v), want deepseek-reasoner", model, mapped)
	}

	// Non-DeepSeek provider is untouched by the built-in table.
	claude := storage.CustomProvider{ID: "claude-relay", Models: []string{"claude-sonnet-4-5"}}
	body, model, mapped = applyCustomProviderModelMapping(claude, raw, "gpt-5.6")
	if mapped || model != "gpt-5.6" || !bytes.Equal(body, raw) {
		t.Fatalf("non-deepseek provider affected: (%q,%v)", model, mapped)
	}
}
