package api

import (
	"regexp"
	"strings"

	"codex-account-pool/internal/capability"
)

// ResolvedInferenceSurface is the single typed eligibility decision used by
// GPT/Codex-only terminal features. Call it with the provider/model that actually
// produced the terminal attempt, after route and force-model resolution.
type ResolvedInferenceSurface struct {
	Provider        string
	Model           string
	Family          string
	Protocol        string
	NativeCodex     bool
	ServerSideState bool
	Eligible        bool
	RejectReason    string
}

var resolvedGPTModelPattern = regexp.MustCompile(`^(?:gpt(?:-[0-9][a-z0-9._-]*)?|chatgpt(?:-[0-9][a-z0-9._-]*)?|codex(?:-[a-z0-9._-]+)?|o(?:1|3|4)(?:[-.][a-z0-9._-]+)?)$`)

// ResolveInferenceSurface fails closed. It never decides a caller's vendor
// identity; it only describes the final upstream surface for session rollover.
func ResolveInferenceSurface(provider, model, protocol string, serverSideState bool) ResolvedInferenceSurface {
	result := ResolvedInferenceSurface{
		Provider:        strings.ToLower(strings.TrimSpace(provider)),
		Model:           capability.NormalizeCodexModelAlias(model),
		Protocol:        strings.ToLower(strings.TrimSpace(protocol)),
		ServerSideState: serverSideState,
	}
	if result.Provider == "chatgpt" {
		result.Provider = "codex"
	}
	switch result.Provider {
	case "codex", "openai":
	default:
		result.RejectReason = "provider_not_codex"
		return result
	}
	if result.Protocol != "responses" && result.Protocol != "responses_websocket" {
		result.RejectReason = "protocol_not_native_responses"
		return result
	}
	if !serverSideState {
		result.RejectReason = "server_state_unavailable"
		return result
	}
	if !capability.IsCatalogCodexModel(result.Model) && !resolvedGPTModelPattern.MatchString(result.Model) {
		result.RejectReason = "model_not_gpt_codex"
		return result
	}
	result.Family = "gpt"
	if strings.HasPrefix(result.Model, "codex") {
		result.Family = "codex"
	}
	result.NativeCodex = true
	result.Eligible = true
	return result
}
