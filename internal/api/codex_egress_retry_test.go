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

func TestCodexTransportFailureDoesNotRotateLegacyStandby(t *testing.T) {
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
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 0 {
		t.Fatalf("legacy standby reached upstream: %+v", requests)
	}
	rows, err := h.store.ListCodexUpstreamAttemptDiagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statesByEgress := map[string]map[string]bool{}
	for _, row := range rows {
		if row.AccountID != accountID {
			continue
		}
		if statesByEgress[row.EgressID] == nil {
			statesByEgress[row.EgressID] = map[string]bool{}
		}
		statesByEgress[row.EgressID][row.State] = true
	}
	if !statesByEgress["unreachable-primary"]["transport_attempted"] || !statesByEgress["unreachable-primary"]["egress_failure"] {
		t.Fatalf("primary transport outcome attribution=%v rows=%+v", statesByEgress, rows)
	}
	if len(statesByEgress[storage.DefaultDirectEgressID]) != 0 {
		t.Fatalf("legacy standby received diagnostics despite being ignored: %v rows=%+v", statesByEgress, rows)
	}
	outcomes, err := h.store.ListRecentCodexEgressOutcomes(context.Background(), storage.Now()-60)
	if err != nil {
		t.Fatal(err)
	}
	if primary := outcomes["unreachable-primary"]; primary.Attempts != 1 || primary.Successes != 0 {
		t.Fatalf("primary classified outcome=%+v, want one exit failure", primary)
	}
	if _, used := outcomes[storage.DefaultDirectEgressID]; used {
		t.Fatalf("legacy standby received an outcome: %+v", outcomes[storage.DefaultDirectEgressID])
	}
}

func TestCodexRetryable5xxDoesNotRotateLegacyStandby(t *testing.T) {
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
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if first := <-order; first != "primary" {
		t.Fatalf("egress order starts with %q, want primary", first)
	}
	select {
	case extra := <-order:
		t.Fatalf("request rotated to legacy standby %q", extra)
	default:
	}
	requests := h.requests()
	if len(requests) != 0 {
		t.Fatalf("legacy standby reached upstream: %+v", requests)
	}
	outcomes, err := h.store.ListRecentCodexEgressOutcomes(context.Background(), storage.Now()-60)
	if err != nil {
		t.Fatal(err)
	}
	if primary := outcomes["failing-primary"]; primary.Attempts != 1 || primary.Successes != 0 {
		t.Fatalf("retry-triggering 5xx outcome=%+v, want exactly one primary failure", primary)
	}
	if _, used := outcomes[storage.DefaultDirectEgressID]; used {
		t.Fatalf("legacy standby received an outcome: %+v", outcomes[storage.DefaultDirectEgressID])
	}
}

func TestCodexOrdinary5xxNeverReachesLegacyEdgeStandby(t *testing.T) {
	primaryProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"ordinary upstream 5xx"}}`)
	}))
	defer primaryProxy.Close()
	edgeHits := make(chan struct{}, 1)
	edgeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		edgeHits <- struct{}{}
		w.Header().Set("cf-ray", "edge-only-standby")
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"challenge"}`)
	}))
	defer edgeProxy.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"resp_after_edge","status":"completed","output_text":"ok"}`)
	})
	accountID := h.importAccount(t, "edge-only-standby", "upstream-edge-only-standby", "access-edge-only-standby")
	for _, profile := range []storage.EgressProfile{
		{ID: "ordinary-5xx-primary", Type: "http_proxy", Endpoint: primaryProxy.URL, Health: "healthy", StreamCapable: true, MaxConcurrency: 10},
		{ID: "edge-only-5xx-standby", Type: "http_proxy", Endpoint: edgeProxy.URL, Health: "healthy", StreamCapable: true, MaxConcurrency: 10},
	} {
		if err := h.store.UpsertEgressProfile(context.Background(), profile); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: "ordinary-5xx-primary", StandbyEgressIDs: "edge-only-5xx-standby," + storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"retry edge"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	select {
	case <-edgeHits:
		t.Fatal("ordinary 5xx rotated to legacy edge standby")
	default:
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("final legacy direct standby reached upstream: %+v", requests)
	}
	outcomes, err := h.store.ListRecentCodexEgressOutcomes(context.Background(), storage.Now()-60)
	if err != nil {
		t.Fatal(err)
	}
	if primary := outcomes["ordinary-5xx-primary"]; primary.Attempts != 1 || primary.Successes != 0 {
		t.Fatalf("ordinary primary 5xx outcome=%+v", primary)
	}
	if _, used := outcomes["edge-only-5xx-standby"]; used {
		t.Fatalf("legacy edge standby received an outcome: %+v", outcomes["edge-only-5xx-standby"])
	}
	if _, used := outcomes[storage.DefaultDirectEgressID]; used {
		t.Fatalf("legacy direct standby received an outcome: %+v", outcomes[storage.DefaultDirectEgressID])
	}
}
