package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func guardRateLimitTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: apiTestStore(t), cfg: config.Default()}
}

// A successful cooldown trial proves the stale quota signal wrong: the upstream
// served traffic, so the binding cooldown must be lifted immediately. Without
// this, a trial-proven account would keep being excluded until the old cooldown
// expiry — the very latency the trial exists to remove.
func TestGuardRateLimitTrialClearsBindingCooldown(t *testing.T) {
	s := guardRateLimitTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-trial", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetBindingCooldown(ctx, account.ID, storage.Now()+3600); err != nil {
		t.Fatal(err)
	}

	s.guardRateLimitForAccount(ctx, account, http.Header{}, true)

	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 {
		t.Fatalf("trial success did not clear binding cooldown: cooldown_until=%d", binding.CooldownUntil)
	}
}

// A non-trial success must keep the cooldown untouched: ordinary traffic already
// respects a real cooldown, and only a trial (which selected a cooldown-stuck
// account on purpose) is allowed to clear it.
func TestGuardRateLimitNonTrialKeepsCooldown(t *testing.T) {
	s := guardRateLimitTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-plain", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.SetBindingCooldown(ctx, account.ID, storage.Now()+3600); err != nil {
		t.Fatal(err)
	}

	s.guardRateLimitForAccount(ctx, account, http.Header{}, false)

	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil <= storage.Now() {
		t.Fatalf("non-trial success cleared cooldown_until=%d, want it preserved", binding.CooldownUntil)
	}
}

// End to end: with the primary group holding no accounts and a configured
// fallback chain, a request must serve from the fallback group instead of
// failing with an empty-pool error. This is the production symptom — 120+
// requests an hour to two empty groups, every one a 503 — fixed by the group
// fallback layer.
func TestIntelligentRoutingGroupFallbackServesWhenPrimaryEmpty(t *testing.T) {
	var calls atomic.Int32
	var reachedFallback atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("ChatGPT-Account-ID") == "up-fb" {
			reachedFallback.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-fb","object":"response","model":"gpt","status":"completed","output":[]}`))
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.GroupFallbacks = map[string][]string{"cyber": {"cyber-fb"}}
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: "acc-fb", Label: "acc-fb", GroupName: "cyber-fb", Provider: "codex", Status: "active", UpstreamAccountID: "up-fb",
	}, storage.AccountToken{AccessToken: "access-fb"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: "acc-fb", ModelSlug: "gpt", AvailabilityState: "verified", Source: "intelligent-routing-test",
	}}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback request status = %d body=%s", resp.StatusCode, out)
	}
	if calls.Load() == 0 {
		t.Fatalf("no upstream request captured")
	}
	if !reachedFallback.Load() {
		t.Fatalf("request did not reach the fallback account up-fb; captured=%+v", h.requests())
	}
}
