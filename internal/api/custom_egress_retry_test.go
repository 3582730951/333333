package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/storage"
)

func setupCustomEgressRetryProvider(t *testing.T, h *testHarness, providerID, model string, egressIDs []string) string {
	t.Helper()
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: providerID, Name: providerID, BaseURL: h.upstream.URL,
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		EgressIDs:        egressIDs, Enabled: true, Models: []string{model},
	}); err != nil {
		t.Fatal(err)
	}
	accountID := providerID + "-account"
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: accountID, Label: accountID, GroupName: "cyber", Provider: providerID, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "sk-" + providerID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: accountID, ModelSlug: model, Source: "custom_egress_retry_test",
	}}); err != nil {
		t.Fatal(err)
	}
	return accountID
}

func TestCustomProviderLegacyEgressMetadataDoesNotOverrideGroupOutlet(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	appendOrder := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, value)
	}
	primaryProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appendOrder("primary")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack primary proxy: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer primaryProxy.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		appendOrder("group-direct")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dsChatResp)
	})
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID: "custom-transport-primary", Type: "http_proxy", Endpoint: primaryProxy.URL,
		Health: "healthy", StreamCapable: true, MaxConcurrency: 10,
	}); err != nil {
		t.Fatal(err)
	}
	accountID := setupCustomEgressRetryProvider(t, h, "custom-transport", "custom-transport-model", []string{"custom-transport-primary", storage.DefaultDirectEgressID})

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"custom-transport-model","messages":[{"role":"user","content":"retry"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	if strings.Join(gotOrder, ",") != "group-direct" {
		t.Fatalf("egress order = %v", gotOrder)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer sk-custom-transport" {
		t.Fatalf("group outlet changed account or replayed upstream: account=%s requests=%+v", accountID, requests)
	}
}

func TestCustomProviderUsesOnlyFirstGroupEgress(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	newFailingProxy := func(name string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"temporary outlet failure"}}`)
		}))
	}
	primary := newFailingProxy("primary", http.StatusServiceUnavailable)
	defer primary.Close()
	standbyOne := newFailingProxy("standby-1", http.StatusBadGateway)
	defer standbyOne.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, "standby-2")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dsChatResp)
	})
	for _, profile := range []storage.EgressProfile{
		{ID: "custom-group-primary", Type: "http_proxy", Endpoint: primary.URL, Health: "healthy", StreamCapable: true, MaxConcurrency: 10},
		{ID: "custom-group-standby-1", Type: "http_proxy", Endpoint: standbyOne.URL, Health: "healthy", StreamCapable: true, MaxConcurrency: 10},
	} {
		if err := h.store.UpsertEgressProfile(context.Background(), profile); err != nil {
			t.Fatal(err)
		}
	}
	group, err := h.store.GetGroup(context.Background(), "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.EgressIDs = []string{"custom-group-primary", "custom-group-standby-1", storage.DefaultDirectEgressID}
	if err := h.store.UpdateGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	setupCustomEgressRetryProvider(t, h, "custom-group-egress", "custom-group-egress-model", nil)

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"custom-group-egress-model","messages":[{"role":"user","content":"retry"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), `"type":"server_error"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	gotOrder := append([]string(nil), order...)
	mu.Unlock()
	if strings.Join(gotOrder, ",") != "primary" {
		t.Fatalf("single inherited egress order = %v", gotOrder)
	}
	requests := h.requests()
	if len(requests) != 0 {
		t.Fatalf("legacy standby egress reached upstream: requests=%+v", requests)
	}
}
