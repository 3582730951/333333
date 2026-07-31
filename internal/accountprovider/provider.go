package accountprovider

import (
	"strings"

	"codex-account-pool/internal/agentidentity"
	"codex-account-pool/internal/storage"
)

const UnknownProvider = "unknown"

const (
	AuthMethodOAuth       = "oauth"
	AuthMethodAccessToken = "access_token"
	AuthMethodAPIKey      = "api_key"

	// CredentialModePersonalAccessToken keeps a workspace-scoped Codex personal
	// access token in the encrypted OpenAIAPIKey column while preserving the
	// refreshable ChatGPT OAuth fields beside it. It is bearer authentication,
	// not pay-as-you-go API-key billing.
	CredentialModePersonalAccessToken = "personal_access_token"

	BillingModeSubscription = "subscription"
	BillingModePayAsYouGo   = "pay_as_you_go"
)

// EffectiveProvider resolves the account provider used for routing and diagnostics.
// The explicit account column wins. Legacy rows with no provider fall back to the
// credential shape. If there is no token row at all, keep the provider unknown.
func EffectiveProvider(declared string, token storage.AccountToken, tokenFound bool) string {
	if p := strings.TrimSpace(declared); p != "" {
		return p
	}
	if !tokenFound {
		return UnknownProvider
	}
	return InferProviderFromToken(token)
}

func InferProviderFromToken(token storage.AccountToken) string {
	if IsAgentIdentity(token) {
		return "codex"
	}
	if strings.HasPrefix(token.AccessToken, "sk-ant") || strings.HasPrefix(token.OpenAIAPIKey, "sk-ant") {
		return "claude"
	}
	if strings.TrimSpace(token.AccessToken) == "" &&
		strings.TrimSpace(token.RefreshToken) == "" &&
		strings.TrimSpace(token.OpenAIAPIKey) == "" &&
		strings.TrimSpace(token.IDTokenRaw) == "" {
		return UnknownProvider
	}
	return "codex"
}

func IsAgentIdentity(token storage.AccountToken) bool {
	return strings.EqualFold(strings.TrimSpace(token.CredentialMode), agentidentity.CredentialMode) ||
		(strings.TrimSpace(token.AgentRuntimeID) != "" && strings.TrimSpace(token.AgentPrivateKey) != "")
}

// NormalizeAuthMethod accepts only the three persisted credential classes. An
// empty result means the row predates auth_method and must use the compatibility
// inference in EffectiveAuthMethod.
func NormalizeAuthMethod(method string) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case AuthMethodOAuth:
		return AuthMethodOAuth
	case AuthMethodAccessToken:
		return AuthMethodAccessToken
	case AuthMethodAPIKey:
		return AuthMethodAPIKey
	default:
		return ""
	}
}

// EffectiveAuthMethod resolves the persisted authentication class, retaining a
// credential-shape fallback for databases created before auth_method existed.
// Explicit metadata always wins so business code never has to infer an API key
// from the historical OpenAIAPIKey column name.
func EffectiveAuthMethod(provider string, token storage.AccountToken) string {
	if IsAgentIdentity(token) {
		return AuthMethodOAuth
	}
	if strings.EqualFold(strings.TrimSpace(token.CredentialMode), CredentialModePersonalAccessToken) &&
		strings.TrimSpace(token.OpenAIAPIKey) != "" {
		return AuthMethodAccessToken
	}
	if method := NormalizeAuthMethod(token.AuthMethod); method != "" {
		return method
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	access := strings.TrimSpace(token.AccessToken)
	key := strings.TrimSpace(token.OpenAIAPIKey)
	if key != "" && (access == "" || access == key) &&
		strings.TrimSpace(token.RefreshToken) == "" &&
		strings.TrimSpace(token.IDTokenRaw) == "" &&
		strings.TrimSpace(token.Scopes) == "" {
		// The historical openai_api_key column also held OAuth/access tokens in
		// some auth.json formats. Treat it as proof only when the credential shape
		// matches the built-in provider, or for a custom provider where that column
		// has always represented its API key.
		if provider != "codex" && provider != "claude" && provider != "" {
			return AuthMethodAPIKey
		}
		if LooksLikeAPIKey(provider, key) {
			return AuthMethodAPIKey
		}
	}
	if strings.TrimSpace(token.RefreshToken) == "" && strings.TrimSpace(token.IDTokenRaw) == "" && strings.TrimSpace(token.Scopes) == "" {
		if LooksLikeAPIKey(provider, access) {
			return AuthMethodAPIKey
		}
	}
	if strings.TrimSpace(token.RefreshToken) != "" || strings.TrimSpace(token.IDTokenRaw) != "" || strings.TrimSpace(token.Scopes) != "" {
		return AuthMethodOAuth
	}
	if strings.EqualFold(strings.TrimSpace(provider), "claude") && strings.HasPrefix(access, "sk-ant-oat") {
		return AuthMethodOAuth
	}
	if access != "" {
		return AuthMethodAccessToken
	}
	return AuthMethodAccessToken
}

// LooksLikeAPIKey recognizes provider-native key shapes only as a compatibility
// fallback for records that predate auth_method. New imports persist auth_method
// explicitly and never depend on prefixes.
func LooksLikeAPIKey(provider, credential string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	credential = strings.TrimSpace(credential)
	switch provider {
	case "claude":
		return strings.HasPrefix(credential, "sk-ant-api")
	case "codex":
		return strings.HasPrefix(credential, "sk-") && !strings.HasPrefix(credential, "sk-ant-oat")
	case "":
		return strings.HasPrefix(credential, "sk-ant-api") ||
			(strings.HasPrefix(credential, "sk-") && !strings.HasPrefix(credential, "sk-ant-oat"))
	default:
		return credential != ""
	}
}

// Credential is the provider-aware plaintext credential accessor. Storage keeps
// the legacy encrypted column for compatibility, but callers no longer need to
// know whether a provider key happens to live in OpenAIAPIKey or AccessToken.
func Credential(provider string, token storage.AccountToken) string {
	if strings.EqualFold(strings.TrimSpace(token.CredentialMode), CredentialModePersonalAccessToken) {
		if token := strings.TrimSpace(token.OpenAIAPIKey); token != "" {
			return token
		}
	}
	if EffectiveAuthMethod(provider, token) == AuthMethodAPIKey {
		if key := strings.TrimSpace(token.OpenAIAPIKey); key != "" {
			return key
		}
	}
	if access := strings.TrimSpace(token.AccessToken); access != "" {
		return access
	}
	return strings.TrimSpace(token.OpenAIAPIKey)
}

func UsesAPIKey(provider string, token storage.AccountToken) bool {
	return EffectiveAuthMethod(provider, token) == AuthMethodAPIKey
}

func BillingMode(provider string, token storage.AccountToken) string {
	if UsesAPIKey(provider, token) {
		return BillingModePayAsYouGo
	}
	return BillingModeSubscription
}
