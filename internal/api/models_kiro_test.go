package api

import (
	"context"
	"encoding/json"
	"io"
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
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
		t.Fatal(err)
	}
	caps := capability.StaticKiroModels(account.ID)
	for i := range caps {
		caps[i].AvailabilityState = capability.AvailabilityVerified
		caps[i].Source = "kiro_runtime"
	}
	if err := h.store.UpsertCapabilities(context.Background(), caps); err != nil {
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

func TestPublicModelsOnlyIncludesVerifiedActiveRoutableAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	accounts := []storage.Account{
		{ID: "active", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "quarantined", GroupName: "cyber", Provider: "codex", Status: "active", QuarantineUntil: storage.Now() + 3600},
		{ID: "unverified", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "unsupported", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "inactive", GroupName: "cyber", Provider: "codex", Status: "inactive"},
		{ID: "no-egress", GroupName: "cyber", Provider: "codex", Status: "active"},
	}
	for _, account := range accounts {
		if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
		if account.ID != "no-egress" {
			if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
				t.Fatal(err)
			}
		}
		state := capability.AvailabilityVerified
		if account.ID == "unverified" {
			state = capability.AvailabilityUnverified
		} else if account.ID == "unsupported" {
			state = capability.AvailabilityUnsupported
		}
		if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: account.ID, ModelSlug: "gpt-" + account.ID, AvailabilityState: state, NativeContextWindow: 128000, Source: "probe"}}); err != nil {
			t.Fatal(err)
		}
	}

	response, err := http.Get(h.pool.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "gpt-active" {
		t.Fatalf("non-routable capability polluted public list: %s", raw)
	}
}
