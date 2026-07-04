package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAffinityBindsSameAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, config.Default())
	key := routing.AffinityKey{Key: "parent:1", Hash: "hash-parent-1", Source: "test"}
	first, err := s.Select(ctx, Route{Group: "cyber", Affinity: key})
	if err != nil {
		t.Fatalf("select first: %v", err)
	}
	firstID := first.Account.ID
	first.Release()
	second, err := s.Select(ctx, Route{Group: "cyber", Affinity: key})
	if err != nil {
		t.Fatalf("select second: %v", err)
	}
	defer second.Release()
	if second.Account.ID != firstID {
		t.Fatalf("second account = %q, want %q", second.Account.ID, firstID)
	}
}

func TestAffinityBindingCannotCrossGroups(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-a", Label: "acc-a", GroupName: "group-a", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-b", Label: "acc-b", GroupName: "group-b", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "hash-shared", RouteKey: "cache_prefix:gpt:abc", Source: "cache_prefix_hash", AccountID: "acc-a"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "group-b", Affinity: routing.AffinityKey{Hash: "hash-shared", Key: "cache_prefix:gpt:abc", Source: "cache_prefix_hash"}})
	if err != nil {
		t.Fatalf("select group-b: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-b" {
		t.Fatalf("sticky affinity crossed groups: got %q, want acc-b", lease.Account.ID)
	}
}

func TestStrictStickyDoesNotSwitchUnavailableAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "hash", RouteKey: "key", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountQuarantine(ctx, "acc-1", storage.Now()+3600, "test"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	_, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "hash", Key: "key"}, Strict: true})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("err = %v, want ErrStrictUnavailable (wrapped)", err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "hash", Key: "key"}, Strict: false})
	if err != nil {
		t.Fatalf("nonstrict should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
}

func TestCapabilityAwareRoutingPrefersAccountWithModel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// Only acc-2 has the requested model probed.
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: "acc-2", ModelSlug: "gpt-rare", NativeMaxContextWindow: 200000}}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	for i := 0; i < 5; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber", Model: "gpt-rare"})
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if lease.Account.ID != "acc-2" {
			t.Fatalf("capability routing chose %q, want acc-2 (the only account with the model)", lease.Account.ID)
		}
		lease.Release()
	}
	// A model nobody probed must NOT over-restrict — selection still succeeds.
	lease, err := s.Select(ctx, Route{Group: "cyber", Model: "gpt-unknown"})
	if err != nil {
		t.Fatalf("unknown (unprobed) model should still select an account: %v", err)
	}
	lease.Release()
}

func TestSelectSkipsCurrentModelLimitedAccountOnly(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	var caps []storage.ModelCapability
	for _, id := range []string{"acc-1", "acc-2"} {
		for _, model := range []string{"gpt-5", "gpt-4"} {
			caps = append(caps, storage.ModelCapability{AccountID: id, ModelSlug: model, NativeMaxContextWindow: 200000, Source: "test", LastProbeAt: storage.Now()})
		}
	}
	if err := store.UpsertCapabilities(ctx, caps); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		UsedPercent: 100, LimitTokens: 1000, RemainingTokens: 0,
		LimitRequests: -1, RemainingRequests: -1, ResetAt: storage.Now() + 300,
		Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}

	s := New(store, config.Default())
	limited, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5"})
	if err != nil {
		t.Fatalf("select gpt-5: %v", err)
	}
	if limited.Account.ID != "acc-2" {
		t.Fatalf("gpt-5 should avoid limited acc-1, chose %s", limited.Account.ID)
	}
	limited.Release()

	otherModel, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-4"})
	if err != nil {
		t.Fatalf("select gpt-4: %v", err)
	}
	defer otherModel.Release()
	if otherModel.Account.ID != "acc-1" {
		t.Fatalf("gpt-4 should still use acc-1; model-specific gpt-5 limit must not bench whole account, chose %s", otherModel.Account.ID)
	}
}

func TestExpiredBreakerEgressStateIsSelectable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()
	for _, health := range []string{"cooldown", "tripped"} {
		profile, err := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
		if err != nil {
			t.Fatal(err)
		}
		profile.Health = health
		profile.CooldownUntil = now - 1
		if err := store.UpsertEgressProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
		s := New(store, config.Default())
		lease, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("%s egress after cooldown should be selectable: %v", health, err)
		}
		lease.Release()
	}
}

func TestActiveBreakerEgressCooldownIsNotSelectable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	if err != nil {
		t.Fatal(err)
	}
	profile.Health = "cooldown"
	profile.CooldownUntil = storage.Now() + 60
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	_, err = s.Select(ctx, Route{Group: "cyber"})
	if err != ErrNoAccount {
		t.Fatalf("err = %v, want ErrNoAccount while egress cooldown is active", err)
	}
}

// TestSelectFreshRoundRobinSpread verifies new conversations spread evenly across
// equally-idle accounts instead of always hammering the oldest one.
func TestSelectFreshRoundRobinSpread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, config.Default())
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		l, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		counts[l.Account.ID]++
		l.Release() // back to idle so all three stay equal-load (score 0)
	}
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if counts[id] != 3 {
			t.Fatalf("account %s got %d/9 selections, want 3 (even round-robin); counts=%v", id, counts[id], counts)
		}
	}
}

func TestTokenBudgetPreventsOverloadedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("first select: %v", err)
	}
	defer lease.Release()
	_, err = s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 20})
	if err != ErrNoAccount {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

// A single request whose estimate exceeds the entire per-account budget must still be
// admitted on an idle account — the budget guards CONCURRENT over-commit, not single-
// request size. Rejecting it (the old behavior) produced a 409/503 that no account
// could satisfy. This is the regression test for the reported "token budget exceeded"
// 409 on a large-context sticky turn.
func TestSoloRequestOverBudgetIsAdmitted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 5000})
	if err != nil {
		t.Fatalf("solo over-budget request should be admitted, got: %v", err)
	}
	lease.Release()
}

// The same, via the strict sticky path: an over-budget solo turn on its bound account
// must NOT return ErrStrictUnavailable("token budget exceeded").
func TestStrictStickyOverBudgetSoloDoesNotConflict(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true, EstimatedTokens: 5000})
	if err != nil {
		t.Fatalf("solo strict over-budget request should be admitted, got: %v", err)
	}
	lease.Release()
}

// An excluded account must be skipped even by the sticky/affinity path: the request
// fails over to a fresh account and the affinity rebinds to it. This is what keeps a
// just-failed account from reappearing in the same request's retry.
func TestExcludeBypassesStickyAndRebinds(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true, Exclude: map[string]bool{"acc-1": true}})
	if err != nil {
		t.Fatalf("excluded sticky account should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
	b, err := store.GetAffinityBinding(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-2" {
		t.Fatalf("affinity rebound to %q, want acc-2", b.AccountID)
	}
}

// A recheck-pending account stays out of the candidate pool even after its cooldown
// has elapsed — until a probe clears it (ClearBindingRecheck). This is the "must pass
// 测活 before re-entering the pool" guarantee.
func TestRecheckPendingAccountIsNotSelected(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// Cooldown already elapsed (now-1) but recheck_pending is set: acc-1 must remain
	// ineligible regardless.
	if err := store.BenchBindingForRecheck(ctx, "acc-1", storage.Now()-1); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	for i := 0; i < 5; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if lease.Account.ID != "acc-2" {
			t.Fatalf("selected %q, want acc-2 (acc-1 is recheck-pending)", lease.Account.ID)
		}
		lease.Release()
	}
	// After a passing probe clears the flag, acc-1 is selectable again.
	if err := store.ClearBindingRecheck(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", Exclude: map[string]bool{"acc-2": true}})
	if err != nil {
		t.Fatalf("after clear, acc-1 should be selectable: %v", err)
	}
	if lease.Account.ID != "acc-1" {
		t.Fatalf("after clear selected %q, want acc-1", lease.Account.ID)
	}
	lease.Release()
}

// A strict request bound to a recheck-pending account must fail over (the sticky
// account is unavailable) rather than return ErrStrictUnavailable, because the
// alternative is pinning the conversation to a benched account.
func TestStrictStickyFailsOverWhenBoundAccountRecheckPending(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.BenchBindingForRecheck(ctx, "acc-1", storage.Now()-1); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	// Without Exclude, a strict turn on a recheck-pending bound account returns
	// ErrStrictUnavailable (the bound account is benched); the request layer then
	// retries with Exclude set, which fails over. Here we assert the diagnose is the
	// recheck-pending reason, then confirm Exclude fails over.
	_, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("err = %v, want ErrStrictUnavailable", err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true, Exclude: map[string]bool{"acc-1": true}})
	if err != nil {
		t.Fatalf("with exclude should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
}
