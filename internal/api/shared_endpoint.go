package api

import (
	"net/http"
	"strings"
)

func (s *Server) handleSharedEndpoint(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveSharedEndpointTarget(w, r)
	if !ok {
		return
	}
	switch {
	case target == "claude":
		s.handleAnthropicPassthrough(w, r)
	case target == "codex":
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"OpenAI/Codex shared endpoint is not available on the selected upstream route",
			[]string{"openai_shared_endpoint:" + r.URL.Path},
			"official_codex_shared_endpoint",
			"codex_shared_endpoint",
			"Use an upstream that supports this OpenAI endpoint directly, or set provider_hint=\"claude\" for Claude Code passthrough requests.")
	case strings.HasPrefix(target, "custom:"):
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"custom providers do not advertise this shared endpoint through the pool",
			[]string{"custom_shared_endpoint:" + r.URL.Path},
			"custom_provider_native_endpoint",
			"custom_shared_endpoint:"+strings.TrimPrefix(target, "custom:"),
			"Call the custom provider directly for provider-specific files/skills APIs, or route official client features to official accounts.")
	default:
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"shared endpoint provider is ambiguous",
			[]string{"shared_endpoint:" + r.URL.Path},
			"official_codex_or_official_claude",
			"shared_endpoint:auto",
			"Set the downstream key provider_hint to codex or claude, send X-Pool-Provider, or include Anthropic/OpenAI provider headers.")
	}
}

func (s *Server) resolveSharedEndpointTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return "", false
	}
	if explicit := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); explicit != "" {
		hint, valid := normalizeProviderHint(explicit)
		if !valid {
			s.writeCapabilityUnavailable(w, http.StatusBadRequest,
				"invalid X-Pool-Provider value",
				[]string{"shared_endpoint:" + r.URL.Path},
				"valid_provider_hint",
				"shared_endpoint:"+explicit,
				"Use X-Pool-Provider: codex, claude, or custom:<provider_id>.")
			return "", false
		}
		return hint, true
	}
	if hasHeaderPrefix(r.Header, "Anthropic-") {
		return "claude", true
	}
	if hasHeaderPrefix(r.Header, "OpenAI-") {
		return "codex", true
	}
	if pol.ProviderHint != "" && pol.ProviderHint != "auto" {
		return pol.ProviderHint, true
	}
	return "auto", true
}

func hasHeaderPrefix(h http.Header, prefix string) bool {
	prefix = strings.ToLower(prefix)
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), prefix) {
			return true
		}
	}
	return false
}
