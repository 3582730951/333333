package upstream

import (
	"net/http"
	"strings"
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

func TestCustomCodexCLISidecarSuppressesBrowserDefaultHeaders(t *testing.T) {
	var capture sidecarCapture
	sidecar := newFakeSidecar(t, &capture)
	defer sidecar.Close()

	client := NewClient(sidecarEngineConfig())
	response, err := client.Do(nilContext(t), Request{
		Provider:         "custom-codex",
		BaseURL:          "https://custom-codex.example/v1",
		TransportProfile: storage.CustomProviderTransportCodexCLI,
		DownstreamPath:   "/responses",
		Body:             testBody([]byte(`{"model":"gpt","stream":true}`)),
		Account:          storage.Account{ID: "acc-custom-codex", Provider: "custom-codex"},
		Token:            storage.AccountToken{OpenAIAPIKey: "custom-provider-key"},
		Egress:           storage.EgressProfile{ID: "sidecar", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if capture.defaultHeaders == nil || *capture.defaultHeaders {
		t.Fatalf("custom Codex CLI sidecar must pin default_headers=false, got %v", capture.defaultHeaders)
	}
	if ua := capture.headers.Get("User-Agent"); !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("custom Codex CLI User-Agent = %q", ua)
	}
}
