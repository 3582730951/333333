package api

import (
	"strings"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/storage"
)

// deepseek_codex_claude.go is the built-in DeepSeek model rewrite for codex and
// Claude Code clients. DeepSeek is a custom provider: it is routable for all three
// entrypoints, but a codex/claude-code client keeps requesting its stock model
// (gpt-5.6, claude-sonnet-4-5, …), which a DeepSeek upstream rejects. When routing
// resolves to a DeepSeek provider, this table rewrites the stock codex/claude model
// to the provider's native deepseek-chat / deepseek-reasoner.
//
// It is deliberately a FALLBACK: an operator-authored model_mapping always wins, and
// a model that is already a DeepSeek model passes through unchanged. It applies only
// on the explicitly selected custom-provider path (provider_hint custom:<id>); auto
// mode model matching is untouched, so a group that also has real codex/claude
// accounts never has its stock-model traffic hijacked by a DeepSeek provider.

// providerIsDeepSeek reports whether a custom provider serves DeepSeek models. The
// id/name are the primary signal; a relay whose advertised model set is entirely
// DeepSeek (a dedicated gateway under a custom id) and mapping values naming DeepSeek
// models also count.
func providerIsDeepSeek(provider storage.CustomProvider) bool {
	id := strings.ToLower(strings.TrimSpace(provider.ID))
	name := strings.ToLower(strings.TrimSpace(provider.Name))
	if strings.Contains(id, "deepseek") || strings.Contains(name, "deepseek") {
		return true
	}
	if len(provider.Models) > 0 {
		allDeepSeek := true
		for _, m := range provider.Models {
			if !prompt.IsDeepSeekModel(m) {
				allDeepSeek = false
				break
			}
		}
		if allDeepSeek {
			return true
		}
	}
	// Mapping VALUES name the upstream model; keys are downstream names and are not
	// evidence of a DeepSeek upstream.
	for _, v := range provider.ModelMappings {
		if prompt.IsDeepSeekModel(v) {
			return true
		}
	}
	return false
}

// deepseekCodexClaudeRewrite maps a stock codex/claude model to the DeepSeek model
// a DeepSeek upstream accepts. The second return reports whether a rewrite applies.
// Fast/tiered families (gpt, claude sonnet/haiku) map to deepseek-chat; top reasoning
// families (o-series, claude opus) map to deepseek-reasoner. DeepSeek models and
// unknown models pass through unmapped.
func deepseekCodexClaudeRewrite(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || strings.Contains(m, "deepseek") {
		return "", false
	}
	switch {
	case strings.HasPrefix(m, "gpt-"):
		return "deepseek-chat", true
	case strings.HasPrefix(m, "o1"), strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"):
		return "deepseek-reasoner", true
	case strings.HasPrefix(m, "claude-opus"), strings.HasPrefix(m, "claude-3-opus"):
		return "deepseek-reasoner", true
	case strings.HasPrefix(m, "claude-"):
		return "deepseek-chat", true
	default:
		return "", false
	}
}
