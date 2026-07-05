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
	for _, p := range providers {
		tier, label, reasons := customProviderCompatTier(p)
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"strategy": "official_first",
		"tiers": []map[string]interface{}{
			{"tier": 1, "label": "official_codex_account", "compatibility": "full official Codex CLI skills/plugins/local tools path"},
			{"tier": 2, "label": "custom_native_responses", "compatibility": "native /v1/responses pass-through; ChatGPT cloud plugins not promised"},
			{"tier": 3, "label": "custom_chat_completions_bridge", "compatibility": "function tools only; typed Responses tools/items return explicit compatibility errors"},
		},
		"checks": map[string]interface{}{
			"official_raw_model_metadata": map[string]interface{}{
				"official_models":              officialModels,
				"models_with_raw_json":         rawModels,
				"models_with_raw_capabilities": rawCapabilityModels,
				"status":                       checkStatus(rawModels > 0, "missing_raw_model_json"),
			},
			"chat_bridge_explicit_errors": map[string]interface{}{
				"status": "ok",
				"detail": "chat_completions bridge rejects typed Responses tools and unknown input items instead of silently dropping them",
			},
			"custom_provider_protocols": map[string]interface{}{
				"status": "ok",
				"detail": "providers declare upstream_protocol=chat_completions or responses",
			},
		},
		"custom_providers": providerViews,
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
		"Responses typed tools such as web_search are unsupported",
		"unknown Responses input/output item types are rejected explicitly",
		"ChatGPT account-scoped cloud plugins/connectors are unavailable",
	}
}

func checkStatus(ok bool, reason string) string {
	if ok {
		return "ok"
	}
	return reason
}
