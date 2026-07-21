package accountprovider

import (
	"testing"

	"codex-account-pool/internal/storage"
)

func TestEffectiveProviderPrefersDeclaredThenTokenShapeThenUnknown(t *testing.T) {
	if got := EffectiveProvider("deepseek", storage.AccountToken{}, false); got != "deepseek" {
		t.Fatalf("declared provider = %q, want deepseek", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{AccessToken: "sk-ant-oat-123"}, true); got != "claude" {
		t.Fatalf("claude token provider = %q, want claude", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{AccessToken: "access-token"}, true); got != "codex" {
		t.Fatalf("codex token provider = %q, want codex", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{CredentialMode: "agent_identity", AgentRuntimeID: "runtime", AgentPrivateKey: "private"}, true); got != "codex" {
		t.Fatalf("agent identity provider = %q, want codex", got)
	}
	if got := EffectiveProvider("", storage.AccountToken{}, false); got != "unknown" {
		t.Fatalf("missing token provider = %q, want unknown", got)
	}
}

func TestEffectiveAuthMethodAndCredentialPreserveLegacyShapes(t *testing.T) {
	cases := []struct {
		name, provider              string
		token                       storage.AccountToken
		method, billing, credential string
	}{
		{name: "explicit api key", provider: "codex", token: storage.AccountToken{AuthMethod: AuthMethodAPIKey, AccessToken: "legacy", OpenAIAPIKey: "sk-key"}, method: AuthMethodAPIKey, billing: BillingModePayAsYouGo, credential: "sk-key"},
		{name: "legacy anthropic api key", provider: "claude", token: storage.AccountToken{AccessToken: "sk-ant-api03-key"}, method: AuthMethodAPIKey, billing: BillingModePayAsYouGo, credential: "sk-ant-api03-key"},
		{name: "legacy oauth", provider: "codex", token: storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "refresh"}, method: AuthMethodOAuth, billing: BillingModeSubscription, credential: "oauth-access"},
		{name: "explicit access token", provider: "claude", token: storage.AccountToken{AuthMethod: AuthMethodAccessToken, AccessToken: "opaque"}, method: AuthMethodAccessToken, billing: BillingModeSubscription, credential: "opaque"},
		{name: "agent identity", provider: "codex", token: storage.AccountToken{CredentialMode: "agent_identity", AgentRuntimeID: "runtime", AgentPrivateKey: "private"}, method: AuthMethodOAuth, billing: BillingModeSubscription, credential: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveAuthMethod(tc.provider, tc.token); got != tc.method {
				t.Fatalf("method=%q want=%q", got, tc.method)
			}
			if got := BillingMode(tc.provider, tc.token); got != tc.billing {
				t.Fatalf("billing=%q want=%q", got, tc.billing)
			}
			if got := Credential(tc.provider, tc.token); got != tc.credential {
				t.Fatalf("credential=%q want=%q", got, tc.credential)
			}
		})
	}
}

func TestLegacyOpenAIAPIKeyColumnDoesNotMisclassifyOAuthAccessToken(t *testing.T) {
	token := storage.AccountToken{AccessToken: "eyJhbGciOi.oauth.access", OpenAIAPIKey: "eyJhbGciOi.oauth.access"}
	if got := EffectiveAuthMethod("codex", token); got != AuthMethodAccessToken {
		t.Fatalf("method=%q, want access_token", got)
	}
	token.AccessToken = "sk-proj-platform"
	token.OpenAIAPIKey = token.AccessToken
	if got := EffectiveAuthMethod("codex", token); got != AuthMethodAPIKey {
		t.Fatalf("method=%q, want api_key", got)
	}
}
