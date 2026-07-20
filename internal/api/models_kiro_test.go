package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

func TestAnthropicModelsEndpointAdvertisesClaudeFacingKiroModelID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := storage.Account{
		ID: "kiro-model-catalog", GroupName: "cyber", Provider: "kiro", PlanType: "KIRO PRO", Status: "active",
	}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(context.Background(), capability.StaticKiroModels(account.ID)); err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/models", nil)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, model := range body.Data {
		ids[model.ID] = true
	}
	if !ids["claude-opus-4-8"] || ids["claude-opus-4.8"] {
		t.Fatalf("unexpected Anthropic Kiro model catalog: %+v", ids)
	}
}
