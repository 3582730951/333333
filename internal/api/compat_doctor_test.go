package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminSkillsCompatDoctorReportsOfficialRawAndProviderTiers(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	if err := h.store.UpsertAccount(context.Background(), storage.Account{
		ID:        "acc-codex",
		Label:     "codex",
		GroupName: "cyber",
		Provider:  "codex",
		Status:    "active",
	}, storage.AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	if err := h.store.UpsertCapabilities(context.Background(), []storage.ModelCapability{{
		AccountID:                     "acc-codex",
		ModelSlug:                     "gpt-future",
		NativeMaxContextWindow:        272000,
		EffectiveContextWindowPercent: 100,
		Visibility:                    "list",
		RawModelJSON:                  `{"id":"gpt-future","capabilities":{"tools":["function","web_search_preview"]}}`,
		Source:                        "probe",
		LastProbeAt:                   storage.Now(),
	}}); err != nil {
		t.Fatalf("seed capabilities: %v", err)
	}
	if err := h.store.UpsertCustomProvider(context.Background(), storage.CustomProvider{
		ID:                 "native",
		Name:               "Native",
		BaseURL:            "https://example.com/v1",
		UpstreamProtocol:   storage.CustomProviderProtocolResponses,
		Enabled:            true,
		AutoDiscoverModels: false,
		Models:             []string{"native-model"},
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	resp, err := http.Get(h.pool.URL + "/admin/compat/skills")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("doctor status = %d", resp.StatusCode)
	}
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["strategy"] != "official_first" {
		t.Fatalf("strategy = %#v", root["strategy"])
	}
	checks := root["checks"].(map[string]interface{})
	raw := checks["official_raw_model_metadata"].(map[string]interface{})
	if raw["models_with_raw_capabilities"].(float64) != 1 {
		t.Fatalf("raw capability check wrong: %#v", raw)
	}
	codexAccounts := checks["official_codex_accounts"].(map[string]interface{})
	if codexAccounts["status"] != "ok" || codexAccounts["active_accounts"].(float64) != 1 {
		t.Fatalf("codex account check wrong: %#v", codexAccounts)
	}
	shared := checks["shared_endpoint_dispatch"].(map[string]interface{})
	if shared["status"] != "ok" {
		t.Fatalf("shared endpoint dispatch check wrong: %#v", shared)
	}
	runtime := checks["claude_runtime_mode"].(map[string]interface{})
	if runtime["default_runtime"] != "compat" {
		t.Fatalf("claude runtime default wrong: %#v", runtime)
	}
	if selection, _ := runtime["model_selection"].(string); !strings.Contains(selection, "Claude Code") || !strings.Contains(selection, "force_model") {
		t.Fatalf("claude runtime doctor must explain client/server model ownership: %#v", runtime)
	}
	providers := root["custom_providers"].([]interface{})
	var sawNative bool
	for _, p := range providers {
		pm := p.(map[string]interface{})
		if pm["id"] == "native" {
			sawNative = true
			if pm["tier"].(float64) != 2 || pm["upstream_protocol"] != "responses" {
				t.Fatalf("native provider tier wrong: %#v", pm)
			}
		}
	}
	if !sawNative {
		t.Fatalf("native provider missing from doctor: %#v", providers)
	}
	if _, ok := root["recent_incompatibilities"].([]interface{}); !ok {
		t.Fatalf("recent incompatibilities missing: %#v", root["recent_incompatibilities"])
	}
}
