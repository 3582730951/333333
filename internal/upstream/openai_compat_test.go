package upstream

import (
	"net/http"
	"testing"

	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

func TestApplyOpenAICompatHeadersUsesAnthropicAuthForMessages(t *testing.T) {
	headers := http.Header{}
	applyOpenAICompatHeaders(headers, Request{
		Headers: http.Header{"Anthropic-Version": []string{"2023-06-01"}, "Anthropic-Beta": []string{"test-beta"}},
		Account: storage.Account{Provider: "claude-relay"},
		Token:   storage.AccountToken{AccessToken: "relay-key"},
	}, false)
	if headers.Get("Authorization") != "Bearer relay-key" || headers.Get("X-Api-Key") != "relay-key" {
		t.Fatalf("anthropic relay auth headers = %#v", headers)
	}
	if headers.Get("Anthropic-Version") != "2023-06-01" || headers.Get("Anthropic-Beta") != "test-beta" {
		t.Fatalf("anthropic headers not preserved: %#v", headers)
	}
	if got, want := headers.Get("User-Agent"), "claude-cli/"+identity.ClaudeCLIVersion+" (external, cli)"; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
}

func TestIsCustomProviderExcludesBuiltIns(t *testing.T) {
	for _, provider := range []string{"", "codex", "claude", "kiro", "antigravity"} {
		if IsCustomProvider(provider) {
			t.Fatalf("built-in provider %q classified as custom", provider)
		}
	}
	if !IsCustomProvider("openrouter") {
		t.Fatal("real custom provider was not classified as custom")
	}
}
