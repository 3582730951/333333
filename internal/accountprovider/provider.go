package accountprovider

import (
	"strings"

	"codex-account-pool/internal/storage"
)

const UnknownProvider = "unknown"

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
