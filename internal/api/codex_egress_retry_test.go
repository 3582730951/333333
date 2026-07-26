package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCodexTransportFailureExhaustsStandbyBeforeFailover(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_standby","output_text":"ok"}`)
	})
	accountID := h.importAccount(t, "standby-transport", "upstream-standby-transport", "access-standby-transport")
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID: "unreachable-primary", Type: "http_proxy", Endpoint: "http://127.0.0.1:1",
		Health: "healthy", StreamCapable: true, MaxConcurrency: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: "unreachable-primary", StandbyEgressIDs: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"retry"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].AccountID != "upstream-standby-transport" {
		t.Fatalf("standby request changed account or replayed: %+v", requests)
	}
}

func TestCodexRetryable5xxUsesOrderedStandbyOnSameAccount(t *testing.T) {
	order := make(chan string, 2)
	primaryProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order <- "primary"
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"temporary outlet failure"}}`)
	}))
	defer primaryProxy.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		order <- "standby"
		_, _ = io.WriteString(w, `{"id":"resp_standby_5xx","output_text":"ok"}`)
	})
	accountID := h.importAccount(t, "standby-5xx", "upstream-standby-5xx", "access-standby-5xx")
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID: "failing-primary", Type: "http_proxy", Endpoint: primaryProxy.URL,
		Health: "healthy", StreamCapable: true, MaxConcurrency: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: "failing-primary", StandbyEgressIDs: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"retry"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if first, second := <-order, <-order; first != "primary" || second != "standby" {
		t.Fatalf("egress order = [%s %s], want [primary standby]", first, second)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].AccountID != "upstream-standby-5xx" {
		t.Fatalf("standby request changed account or replayed: %+v", requests)
	}
}
