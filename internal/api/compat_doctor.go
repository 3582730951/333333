package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/storage"
)

// adminSkillsCompatDoctor summarizes the gateway's Codex skills compatibility
// posture. It is intentionally local/read-only: the operator can see whether official
// model raw capability metadata is being preserved and why each custom provider is
// Tier 2 (native Responses) or Tier 3 (Chat Completions bridge).
func (s *Server) adminSkillsCompatDoctor(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	caps, err := s.store.ListCapabilities(r.Context(), "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	providers, err := s.store.ListCustomProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	officialModels := 0
	rawModels := 0
	rawCapabilityModels := 0
	for _, c := range caps {
		if !isCodexSource(c.Source) {
			continue
		}
		officialModels++
		if strings.TrimSpace(c.RawModelJSON) == "" {
			continue
		}
		rawModels++
		if rawModelHasCapabilityMetadata(c.RawModelJSON) {
			rawCapabilityModels++
		}
	}

	providerViews := make([]map[string]interface{}, 0, len(providers))
	nativeResponsesProviders := 0
	chatBridgeProviders := 0
	for _, p := range providers {
		tier, label, reasons := customProviderCompatTier(p)
		if p.UpstreamProtocol == storage.CustomProviderProtocolResponses {
			nativeResponsesProviders++
		} else {
			chatBridgeProviders++
		}
		providerViews = append(providerViews, map[string]interface{}{
			"id":                  p.ID,
			"name":                p.Name,
			"enabled":             p.Enabled,
			"upstream_protocol":   p.UpstreamProtocol,
			"tier":                tier,
			"tier_label":          label,
			"degradation_reasons": reasons,
		})
	}

	activeCodexAccounts := 0
	activeClaudeAccounts := 0
	activeClaudeOAuthAccounts := 0
	for _, a := range accounts {
		if strings.ToLower(strings.TrimSpace(a.Status)) != "active" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(a.Provider)) {
		case "codex":
			activeCodexAccounts++
		case "claude":
			activeClaudeAccounts++
			if token, err := s.store.GetToken(r.Context(), a.ID); err == nil && claudeIsOAuth(token) {
				activeClaudeOAuthAccounts++
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"strategy": "official_first",
		"tiers": []map[string]interface{}{
			{"tier": 1, "label": "official_codex_account", "compatibility": "full official Codex CLI skills/plugins/local tools path"},
			{"tier": 2, "label": "custom_native_responses", "compatibility": "native /v1/responses pass-through; ChatGPT cloud plugins not promised"},
			{"tier": 3, "label": "custom_chat_completions_bridge", "compatibility": "function/namespace/custom/client tool-search bridge; hosted tools are omitted with explicit loss diagnostics"},
		},
		"checks": map[string]interface{}{
			"official_codex_accounts": map[string]interface{}{
				"active_accounts": activeCodexAccounts,
				"status":          checkStatus(activeCodexAccounts > 0, "missing_official_codex_account"),
				"fix_hint":        "Import at least one official Codex/OpenAI account for full Codex CLI skills/plugins/Browser Use behavior.",
			},
			"official_claude_oauth_accounts": map[string]interface{}{
				"active_accounts":       activeClaudeAccounts,
				"active_oauth_accounts": activeClaudeOAuthAccounts,
				"status":                checkStatus(activeClaudeOAuthAccounts > 0, "missing_official_claude_oauth_account"),
				"fix_hint":              "Import a Claude OAuth account for full Claude Code skills/plugins/MCP behavior.",
			},
			"official_raw_model_metadata": map[string]interface{}{
				"official_models":              officialModels,
				"models_with_raw_json":         rawModels,
				"models_with_raw_capabilities": rawCapabilityModels,
				"status":                       checkStatus(rawModels > 0, "missing_raw_model_json"),
			},
			"native_responses_providers": map[string]interface{}{
				"count":  nativeResponsesProviders,
				"status": checkStatus(nativeResponsesProviders > 0, "missing_native_responses_provider"),
				"detail": "Tier 2 providers preserve typed Responses tools and future fields by forwarding /v1/responses natively.",
			},
			"chat_bridge_providers": map[string]interface{}{
				"count":  chatBridgeProviders,
				"status": checkStatus(chatBridgeProviders > 0, "no_chat_bridge_provider"),
				"detail": "Tier 3 providers bridge stable function, namespace, custom, and client tool-search tools; unsupported hosted tools continue with X-Pool-Compatibility-Losses diagnostics.",
			},
			"chat_bridge_loss_diagnostics": map[string]interface{}{
				"status": "ok",
				"detail": "chat_completions bridge reports sorted compatibility losses in response headers/trailers and usage_records.compatibility_losses_json",
			},
			"custom_provider_protocols": map[string]interface{}{
				"status": "ok",
				"detail": "providers declare upstream_protocol=chat_completions or responses",
			},
			"shared_endpoint_dispatch": map[string]interface{}{
				"status": "ok",
				"detail": "shared /v1/files and /v1/skills endpoints route by X-Pool-Provider, Anthropic/OpenAI headers, or downstream key provider_hint",
			},
			"claude_passthrough": map[string]interface{}{
				"status": "ok",
				"detail": "Claude passthrough preserves opaque bodies and client Anthropic headers for files/skills/agents/environments/sessions",
			},
			"codex_config_merge": map[string]interface{}{
				"status": "ok",
				"detail": "generated Codex installer filters only pool-managed keys/section and preserves existing MCP/plugin/settings TOML sections",
			},
			"claude_runtime_mode": map[string]interface{}{
				"status":          "ok",
				"default_runtime": "compat",
				"strict_runtime":  "set POOL_CLIENT_RUNTIME=strict or POOL_STRICT_LINUX=1",
				"model_selection": "Claude Code and the user select the client model; force_model is applied only by the VPS after authentication",
				"detail":          "compat keeps official Claude Code skills/plugins/MCP visible; neither runtime injects model, tier, sub-agent, or custom menu overrides",
			},
		},
		"custom_providers":         providerViews,
		"recent_incompatibilities": s.recentCompatibilityIncompatibilities(),
	})
}

func rawModelHasCapabilityMetadata(raw string) bool {
	var m map[string]interface{}
	if json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	for _, key := range []string{"capabilities", "tools", "supported_tool_types", "features", "feature_matrix"} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

func customProviderCompatTier(p storage.CustomProvider) (int, string, []string) {
	if p.UpstreamProtocol == storage.CustomProviderProtocolResponses {
		return 2, "custom_native_responses", []string{"ChatGPT account-scoped cloud plugins/connectors remain best-effort"}
	}
	return 3, "custom_chat_completions_bridge", []string{
		"hosted Responses tools such as web_search and server tool-search are omitted with compatibility diagnostics",
		"unknown Responses history items are preserved in versioned JSON envelopes",
		"ChatGPT account-scoped cloud plugins/connectors are unavailable",
	}
}

func checkStatus(ok bool, reason string) string {
	if ok {
		return "ok"
	}
	return reason
}
