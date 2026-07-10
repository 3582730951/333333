package api

import (
	"testing"

	"codex-account-pool/internal/storage"
)

func TestShouldInjectCodexHostedWebSearch(t *testing.T) {
	tests := []struct {
		name  string
		model string
		token storage.AccountToken
		body  string
		want  bool
	}{
		{
			name:  "native OAuth Responses Lite omits hosted search",
			model: "gpt-5.6-sol",
			token: storage.AccountToken{AccessToken: "oauth-access"},
			body:  `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`,
			want:  false,
		},
		{
			name:  "classic OAuth body keeps hosted search and classic contract",
			model: "gpt-5.6-sol",
			token: storage.AccountToken{AccessToken: "oauth-access"},
			body:  `{"model":"gpt-5.6-sol","input":"hi"}`,
			want:  true,
		},
		{
			name:  "OAuth classic Responses keeps hosted search",
			model: "gpt-5.5",
			token: storage.AccountToken{AccessToken: "oauth-access"},
			body:  `{"model":"gpt-5.5","input":"hi"}`,
			want:  true,
		},
		{
			name:  "API key Responses Lite keeps hosted search",
			model: "gpt-5.6-sol",
			token: storage.AccountToken{OpenAIAPIKey: "sk-openai"},
			body:  `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`,
			want:  true,
		},
		{
			name:  "imported API key mirrored into access token keeps hosted search",
			model: "gpt-5.6-sol",
			token: storage.AccountToken{AccessToken: "sk-openai", OpenAIAPIKey: "sk-openai"},
			body:  `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`,
			want:  true,
		},
		{
			name:  "OAuth evidence wins over mirrored auxiliary key",
			model: "gpt-5.6-sol",
			token: storage.AccountToken{AccessToken: "oauth-access", OpenAIAPIKey: "oauth-access", RefreshToken: "refresh"},
			body:  `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldInjectCodexHostedWebSearch(tt.model, tt.token, []byte(tt.body)); got != tt.want {
				t.Fatalf("shouldInjectCodexHostedWebSearch(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}
