package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func failureStreakTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{store: apiTestStore(t), cfg: config.Default()}
}

// A plain 5xx never benched an account: usageLimitCooldown returns 0 for anything
// that is not a 429 or a quota signal. Production diagnostics show one provider
// account returning 1512 consecutive 503s across 14 hours while remaining a
// first-choice candidate. After the threshold-th consecutive 5xx the account must be
// benched for recheck.
func TestConsecutiveUpstreamFailuresBenchAccount(t *testing.T) {
	s := failureStreakTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-dead-relay", GroupName: "cyber", Provider: "custom", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}

	threshold := s.cfg.AccountFailureStreakThreshold
	if threshold <= 1 {
		t.Fatalf("threshold = %d, want > 1 so the pre-threshold assertion is meaningful", threshold)
	}
	for i := 1; i < threshold; i++ {
		if benched := s.benchOnFailureStreak(ctx, account, http.StatusServiceUnavailable); benched {
			t.Fatalf("benched after %d/%d failures, want no bench before the threshold", i, threshold)
		}
		binding, err := s.store.GetEgressBinding(ctx, account.ID)
		if err != nil {
			t.Fatal(err)
		}
		if binding.RecheckPending || binding.CooldownUntil > storage.Now() {
			t.Fatalf("account benched early after %d failures: %+v", i, binding)
		}
	}
	if benched := s.benchOnFailureStreak(ctx, account, http.StatusServiceUnavailable); !benched {
		t.Fatalf("no bench after %d consecutive 5xx", threshold)
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Bench-for-recheck, not a bare cooldown: a dead upstream must pass a liveness
	// probe before it re-enters the pool.
	if !binding.RecheckPending {
		t.Fatalf("binding.RecheckPending = false, want a recheck-gated bench: %+v", binding)
	}
	if binding.CooldownUntil <= storage.Now() {
		t.Fatalf("binding.CooldownUntil = %d, want a future cooldown", binding.CooldownUntil)
	}
}

// One good response ends the streak, so an account with intermittent 5xx among
// successes is never benched by the breaker.
func TestUpstreamSuccessResetsFailureStreak(t *testing.T) {
	s := failureStreakTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-flaky", GroupName: "cyber", Provider: "custom", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	threshold := s.cfg.AccountFailureStreakThreshold
	for round := 0; round < 4; round++ {
		for i := 1; i < threshold; i++ {
			if benched := s.benchOnFailureStreak(ctx, account, http.StatusBadGateway); benched {
				t.Fatalf("round %d: benched at %d/%d failures", round, i, threshold)
			}
		}
		s.guardRateLimitForAccount(ctx, account, http.Header{}, false)
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RecheckPending || binding.CooldownUntil > storage.Now() {
		t.Fatalf("interleaved successes still benched the account: %+v", binding)
	}
}

// A non-5xx response also resets the streak: 4xx has its own remediation (or none),
// and must not accumulate toward a server-error bench.
func TestNon5xxResetsFailureStreak(t *testing.T) {
	s := failureStreakTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-4xx", GroupName: "cyber", Provider: "custom", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	threshold := s.cfg.AccountFailureStreakThreshold
	for i := 1; i < threshold; i++ {
		s.benchOnFailureStreak(ctx, account, http.StatusInternalServerError)
	}
	if benched := s.benchOnFailureStreak(ctx, account, http.StatusNotFound); benched {
		t.Fatal("a 404 benched the account through the 5xx breaker")
	}
	if benched := s.benchOnFailureStreak(ctx, account, http.StatusInternalServerError); benched {
		t.Fatal("streak survived a non-5xx response")
	}
}

func TestQuotaWrappedIn5xxDoesNotTripFailureStreak(t *testing.T) {
	s := failureStreakTestServer(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-quota-5xx", GroupName: "cyber", Provider: "custom", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	for range s.cfg.AccountFailureStreakThreshold * 2 {
		s.onUpstreamError(ctx, account, http.StatusServiceUnavailable, nil, []byte(`{"error":"quota exceeded"}`))
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RecheckPending || binding.CooldownUntil <= storage.Now() {
		t.Fatalf("quota-wrapped 5xx should have only a live cooldown: %+v", binding)
	}
}

// The per-account operator override opts out of every automatic bench, including
// this one.
func TestIgnoreRateLimitControlsSkipsFailureStreakBench(t *testing.T) {
	s := failureStreakTestServer(t)
	ctx := context.Background()
	account := storage.Account{
		ID: "acc-override", GroupName: "cyber", Provider: "custom", Status: "active",
		IgnoreRateLimitControls: true,
	}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < s.cfg.AccountFailureStreakThreshold*3; i++ {
		if benched := s.benchOnFailureStreak(ctx, account, http.StatusServiceUnavailable); benched {
			t.Fatal("ignore_rate_limit_controls account was benched by the failure-streak breaker")
		}
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RecheckPending || binding.CooldownUntil > storage.Now() {
		t.Fatalf("override account benched: %+v", binding)
	}
}

// threshold=0 is the explicit off switch.
func TestFailureStreakBreakerDisabled(t *testing.T) {
	s := failureStreakTestServer(t)
	s.cfg.AccountFailureStreakThreshold = 0
	ctx := context.Background()
	account := storage.Account{ID: "acc-off", GroupName: "cyber", Provider: "custom", Status: "active"}
	if err := s.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if benched := s.benchOnFailureStreak(ctx, account, http.StatusServiceUnavailable); benched {
			t.Fatal("breaker benched with threshold=0")
		}
	}
}

// Two isolated failures far apart are not an outage: an idle gap restarts the count.
func TestFailureStreakExpiresWhenIdle(t *testing.T) {
	table := &failureStreakTable{}
	start := time.Now()
	if got := table.observe("acc", true, start); got != 1 {
		t.Fatalf("first observe = %d, want 1", got)
	}
	if got := table.observe("acc", true, start.Add(time.Second)); got != 2 {
		t.Fatalf("second observe = %d, want 2", got)
	}
	next := start.Add(time.Second + failureStreakEntryTTL + time.Second)
	if got := table.observe("acc", true, next); got != 1 {
		t.Fatalf("observe after idle TTL = %d, want the streak to restart at 1", got)
	}
}

// A slow trickle spaced just under the idle TTL must not accumulate to a bench over
// hours: the streak is also bounded by how long it took to build.
func TestFailureStreakWindowBoundsSlowTrickle(t *testing.T) {
	table := &failureStreakTable{}
	now := time.Now()
	step := failureStreakEntryTTL - time.Minute
	max := 0
	for i := 0; i < 12; i++ {
		if got := table.observe("acc", true, now); got > max {
			max = got
		}
		now = now.Add(step)
	}
	// 12 failures spread over ~1.8 hours, each within the idle TTL of the previous one.
	// The window cap must keep the streak far below that count.
	if max > 3 {
		t.Fatalf("max streak over 12 trickled failures = %d, want the window to cap it", max)
	}
}

func TestFailureStreakTableIsBounded(t *testing.T) {
	table := &failureStreakTable{}
	now := time.Now()
	for i := 0; i < failureStreakMaxEntries*2; i++ {
		table.observe(accountIDForIndex(i), true, now)
	}
	table.mu.Lock()
	size := len(table.entries)
	table.mu.Unlock()
	if size > failureStreakMaxEntries {
		t.Fatalf("table size = %d, want <= %d", size, failureStreakMaxEntries)
	}
}

func accountIDForIndex(i int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 8)
	for n := i; ; n /= len(digits) {
		out = append(out, digits[n%len(digits)])
		if n < len(digits) {
			break
		}
	}
	return "acc-" + string(out)
}
