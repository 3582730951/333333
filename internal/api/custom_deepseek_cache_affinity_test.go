package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func TestCustomProviderAffinityUsesOriginalResponsesCacheKey(t *testing.T) {
	original := []byte(`{"model":"deepseek-v4-flash","prompt_cache_key":"codex-root-cache","instructions":"stable system","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(original))
	req.Header.Set("Authorization", "Bearer downstream-key")
	expected := routing.ExtractAffinityKey(req, original)
	if expected.Source != "prompt_cache_key" || expected.Hash == "" {
		t.Fatalf("original affinity = %+v", expected)
	}

	pinned := withCustomProviderDownstreamAffinity(req, original, "codex")
	diagnostics := usageDiagnosticsFromCtx(pinned.Context())
	if !diagnostics.PromptCacheKeyPresent || diagnostics.PromptCacheKeySource != "downstream" ||
		diagnostics.AffinitySource != "prompt_cache_key" {
		t.Fatalf("request diagnostics = %+v", diagnostics)
	}
	bridge, err := prompt.ResponsesRequestToChatCompletionBridge(original)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bridge.Body, []byte("prompt_cache_key")) {
		t.Fatalf("test precondition failed: bridge retained Responses-only cache key: %s", bridge.Body)
	}
	got := customProviderProtocolAffinity(pinned, bridge.Body, "codex")
	if got.Hash != expected.Hash || got.Source != expected.Source {
		t.Fatalf("converted affinity = %+v, want original %+v", got, expected)
	}
}

func TestCustomProviderAffinityUsesClaudeCodeSessionBeforeBridgePrefix(t *testing.T) {
	original := []byte(`{"model":"deepseek-v4-flash","max_tokens":4096,"system":"stable system","messages":[{"role":"user","content":"first turn"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(original))
	req.Header.Set("Authorization", "Bearer downstream-key")
	req.Header.Set("X-Claude-Code-Session-Id", "claude-root-session")
	expected := routing.ExtractClaudeAffinityKey(req, original)
	if expected.Source != "x-claude-code-session-id" || expected.Hash == "" {
		t.Fatalf("original affinity = %+v", expected)
	}

	pinned := withCustomProviderDownstreamAffinity(req, original, "claude")
	if diagnostics := usageDiagnosticsFromCtx(pinned.Context()); diagnostics.AffinitySource != "x-claude-code-session-id" {
		t.Fatalf("request diagnostics = %+v", diagnostics)
	}
	converted, err := prompt.AnthropicRequestToChatCompletion(original)
	if err != nil {
		t.Fatal(err)
	}
	got := customProviderProtocolAffinity(pinned, converted, "claude")
	if got.Hash != expected.Hash || got.Source != expected.Source {
		t.Fatalf("converted affinity = %+v, want original %+v", got, expected)
	}
}

func TestCustomProviderCodexRootAndSubagentShareScopedAccountAffinity(t *testing.T) {
	provider := storage.CustomProvider{
		ID:                     "deepseek-1",
		BaseURL:                "https://api.deepseek.com",
		UpstreamProtocol:       storage.CustomProviderProtocolChatCompletions,
		ResolvedRouteID:        "default",
		ResolvedDownstreamPath: storage.CustomProviderDownstreamResponses,
	}
	root := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	root.Header.Set("Authorization", "Bearer same-downstream-key")
	root.Header.Set("Thread-Id", "codex-family-root")
	child := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	child.Header.Set("Authorization", "Bearer same-downstream-key")
	child.Header.Set("X-Codex-Parent-Thread-Id", "codex-family-root")

	rootBase := customProviderProtocolAffinity(root, []byte(`{"model":"deepseek-v4-pro","input":"root"}`), "codex")
	childBase := customProviderProtocolAffinity(child, []byte(`{"model":"deepseek-v4-flash","input":"child"}`), "codex")
	if rootBase.Source != routing.CodexRootThreadAffinitySource || rootBase.Hash != childBase.Hash {
		t.Fatalf("root=%+v child=%+v", rootBase, childBase)
	}
	rootScoped := customProviderScopedAffinity(root, provider, rootBase)
	childScoped := customProviderScopedAffinity(child, provider, childBase)
	if rootScoped.Hash == "" || rootScoped.Hash != childScoped.Hash {
		t.Fatalf("scoped root=%+v child=%+v", rootScoped, childScoped)
	}
}
