package api

import (
	"net/http"
	"strings"
)

func (s *Server) handleSharedEndpoint(w http.ResponseWriter, r *http.Request) {
	target, policy, ok := s.resolveSharedEndpointTarget(w, r)
	if !ok {
		return
	}

	// A built-in provider selected from a stronger wire-protocol signal must also
	// win when its concrete passthrough handler resolves downstream policy. Keep
	// the caller's request immutable and install that result only on an internal
	// clone. Custom passthrough consumes the already-resolved policy directly.
	// Neither upstream passthrough allowlist forwards X-Pool-Provider.
	selectedRequest := r
	if (target == "claude" || target == "codex") &&
		strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		selectedRequest = r.Clone(r.Context())
		selectedRequest.Header = r.Header.Clone()
		selectedRequest.Header.Set("X-Pool-Provider", target)
	}

	switch {
	case target == "claude":
		s.handleAnthropicPassthrough(w, selectedRequest)
	case target == "codex":
		s.handleCodexPassthrough(w, selectedRequest)
	case target == "kiro":
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"Kiro does not expose this passthrough endpoint",
			[]string{"kiro_unsupported_endpoint:" + r.URL.Path},
			"official_claude_passthrough", "kiro", "Route Files, Skills, Agents and related endpoints to provider_hint=\"claude\".")
	case strings.HasPrefix(target, "custom:"):
		s.handleCustomProviderPassthrough(w, r, strings.TrimPrefix(target, "custom:"), policy)
	default:
		s.writeCapabilityUnavailable(w, http.StatusBadRequest,
			"shared endpoint provider is ambiguous",
			[]string{"shared_endpoint:" + r.URL.Path},
			"official_codex_or_official_claude",
			"shared_endpoint:auto",
			"Set the downstream key provider_hint to codex or claude, send X-Pool-Provider, or include Anthropic/OpenAI provider headers.")
	}
}

func (s *Server) resolveSharedEndpointTarget(w http.ResponseWriter, r *http.Request) (string, downstreamPolicy, bool) {
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return "", downstreamPolicy{}, false
	}
	if explicit := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); explicit != "" {
		hint, valid := normalizeProviderHint(explicit)
		if !valid {
			s.writeCapabilityUnavailable(w, http.StatusBadRequest,
				"invalid X-Pool-Provider value",
				[]string{"shared_endpoint:" + r.URL.Path},
				"valid_provider_hint",
				"shared_endpoint:"+explicit,
				"Use X-Pool-Provider: codex, claude, kiro, or custom:<provider_id>.")
			return "", downstreamPolicy{}, false
		}
		if strings.HasPrefix(hint, "custom:") && pol.UserGroupID != "" {
			var routeProvider string
			pol, routeProvider, ok = s.resolveSharedEndpointUserGroup(w, r, pol)
			if !ok {
				return "", downstreamPolicy{}, false
			}
			if routeProvider != "" && !strings.EqualFold(routeProvider, hint) {
				s.writeCapabilityUnavailable(
					w,
					http.StatusConflict,
					"the user-group route does not select this custom provider",
					[]string{"shared_endpoint:" + r.URL.Path},
					routeProvider,
					hint,
					"Select a custom provider configured on the current user group.",
				)
				return "", downstreamPolicy{}, false
			}
		}
		if isClaudeOnlySharedEndpoint(r.URL.Path) &&
			hint != "kiro" && !strings.HasPrefix(hint, "custom:") {
			// These routes historically entered the Anthropic passthrough
			// directly. Preserve that built-in behavior; only an explicit
			// custom provider adds a new destination (Kiro keeps its existing
			// capability error).
			return "claude", pol, true
		}
		return hint, pol, true
	}

	if pol.UserGroupID != "" {
		var routeProvider string
		pol, routeProvider, ok = s.resolveSharedEndpointUserGroup(w, r, pol)
		if !ok {
			return "", downstreamPolicy{}, false
		}
		if strings.HasPrefix(routeProvider, "custom:") {
			return routeProvider, pol, true
		}
	}
	// A custom provider policy is an explicit upstream choice, while Anthropic-*
	// and OpenAI-* headers identify only the wire protocol. Honor the selected
	// relay before protocol-family detection so Claude Code can reach a custom
	// Anthropic relay instead of being silently diverted to the built-in route.
	if strings.HasPrefix(pol.ProviderHint, "custom:") {
		return pol.ProviderHint, pol, true
	}
	if isClaudeOnlySharedEndpoint(r.URL.Path) {
		if pol.ProviderHint == "kiro" {
			return "kiro", pol, true
		}
		return "claude", pol, true
	}
	if hasHeaderPrefix(r.Header, "Anthropic-") {
		return "claude", pol, true
	}
	if hasHeaderPrefix(r.Header, "OpenAI-") {
		return "codex", pol, true
	}
	if sharedEndpointHasCodexClientSignal(r) {
		return "codex", pol, true
	}
	if pol.ProviderHint != "" && pol.ProviderHint != "auto" {
		return pol.ProviderHint, pol, true
	}
	// A URL plus one pool API key is the zero-configuration Codex contract.
	// Claude Code always supplies Anthropic protocol headers on these surfaces,
	// while a generic client may provide no provider-specific header at all.
	return "codex", pol, true
}

// resolveSharedEndpointUserGroup performs the no-model user-group decision once
// and returns both its physical group and provider result. The resolved policy is
// passed into the custom handler, avoiding a second binding claim, a second API-key
// usage update, and a race where a policy edit could select a different relay
// between dispatch and scheduler selection.
func (s *Server) resolveSharedEndpointUserGroup(
	w http.ResponseWriter,
	r *http.Request,
	pol downstreamPolicy,
) (downstreamPolicy, string, bool) {
	routeGroup, routeProvider, err := resolveUserGroupRoute(r.Context(), s.store, pol, r, nil)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", err.Error())
		return downstreamPolicy{}, "", false
	}
	if routeGroup != "" {
		pol.Group = routeGroup
	}
	if routeProvider != "" {
		pol.ProviderHint = routeProvider
	}
	return pol, routeProvider, true
}

func isClaudeOnlySharedEndpoint(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	for _, prefix := range []string{"/v1/agents", "/v1/environments", "/v1/sessions"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
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

func sharedEndpointHasCodexClientSignal(r *http.Request) bool {
	if r == nil {
		return false
	}
	if hasHeaderPrefix(r.Header, "X-Codex-") || isOfficialCodexCLIRequest(r) {
		return true
	}
	return recognizedCodexClientName(r.Header.Get("Originator")) ||
		recognizedCodexClientName(r.Header.Get("User-Agent"))
}

func recognizedCodexClientName(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if slash := strings.IndexByte(value, '/'); slash >= 0 {
		value = value[:slash]
	}
	switch value {
	case "codex_cli_rs",
		"codex_exec",
		"codex-tui",
		"codex_vscode",
		"codex desktop",
		"codex_desktop",
		"codex-app-server",
		"codex_app_server",
		"codex-mcp-server",
		"codex_mcp_server",
		"codex-cli":
		return true
	default:
		return false
	}
}
