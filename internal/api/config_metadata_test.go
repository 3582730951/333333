package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestConfigMetadataHasSinglePlacement(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/config", "")
	if code != http.StatusOK {
		t.Fatalf("GET /admin/config = %d: %s", code, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	validPlacement := map[string]bool{configPlacementAI: true, configPlacementSystem: true, configPlacementFeature: true}
	for _, row := range rows {
		key, _ := row["key"].(string)
		if key == "" || seen[key] {
			t.Fatalf("config key must have one display location: %q", key)
		}
		seen[key] = true
		placement, _ := row["placement"].(string)
		if !validPlacement[placement] {
			t.Fatalf("%s placement = %q", key, placement)
		}
		if placement == configPlacementAI {
			if domain, _ := row["domain"].(string); domain == "" {
				t.Fatalf("AI setting %s has no domain", key)
			}
		}
		if row["section"] == "" || row["scope"] == "" || row["order"] == nil {
			t.Fatalf("%s missing metadata: %#v", key, row)
		}
	}
}

func TestConfigMetadataFiltersAIDomain(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, raw := grpReq(t, h, http.MethodGet, "/admin/config?placement=ai_settings&domain=codex", "")
	if code != http.StatusOK {
		t.Fatalf("filtered GET /admin/config = %d: %s", code, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected Codex AI settings")
	}
	for _, row := range rows {
		if row["placement"] != configPlacementAI || row["domain"] != "codex" {
			t.Fatalf("unexpected filtered row: %#v", row)
		}
	}
}
