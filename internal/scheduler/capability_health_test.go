package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// A benched (recheck-pending) account must be reported as recheck_pending, never as
// model_unsupported. The capability query used to health-gate itself, so the only
// provider account in a group falling into cooldown made the scheduler announce that
// the operator's admin-verified model was unsupported — 716 production audit rows of
// `Routing rejected normalized model "…" … model_unsupported=1` for a model whose
// capability row said `verified`. Capability and availability are separate questions
// and must produce separate counters.
func TestBenchedCapableAccountIsNotReportedModelUnsupported(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, account := range []storage.Account{
		{ID: "acc-provider", Label: "acc-provider", GroupName: "cyber", Provider: "custom", Status: "active"},
		// A same-model capable account on a provider this route does not allow. It keeps
		// the capability map non-empty (the scheduler ignores an empty map entirely), so
		// the model gate is genuinely exercised against the benched account.
		{ID: "acc-other-provider", Label: "acc-other-provider", GroupName: "cyber", Provider: "claude", Status: "active"},
	} {
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: "acc-provider", ModelSlug: "gpt-5.6-sol",
		AvailabilityState: capability.AvailabilityVerified, Source: "custom_admin_test",
		NativeMaxContextWindow: 200000,
	}, {
		AccountID: "acc-other-provider", ModelSlug: "gpt-5.6-sol",
		AvailabilityState: capability.AvailabilityVerified, Source: "custom_admin_test",
		NativeMaxContextWindow: 200000,
	}}); err != nil {
		t.Fatal(err)
	}
	// The account is capable of the model and benched for recheck at the same time.
	if err := store.BenchBindingForRecheck(ctx, "acc-provider", storage.Now()+600); err != nil {
		t.Fatal(err)
	}

	capable, err := store.AccountsWithModelAndContext(ctx, "cyber", "gpt-5.6-sol", "")
	if err != nil {
		t.Fatal(err)
	}
	if !capable["acc-provider"] {
		t.Fatalf("capability lookup dropped a benched-but-capable account: %+v", capable)
	}

	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	_, err = s.Select(ctx, Route{Group: "cyber", Provider: "custom", ExplicitProvider: true, Model: "gpt-5.6-sol", SkipWait: true})
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T, want *NoAccountError", err)
	}
	if noAccount.Counters.ModelUnsupported != 0 {
		t.Fatalf("benched account counted as model_unsupported: %+v", noAccount.Counters)
	}
	if noAccount.Counters.RecheckPending != 1 {
		t.Fatalf("counters = %+v, want recheck_pending=1", noAccount.Counters)
	}
}

// A capable account that is merely cooling must still be a cooldown wait target.
// shortestCooldown/shortestCooldownBatch skip accounts outside the capability map,
// and that map used to exclude cooling accounts — so "every account is cooling"
// silently became "no capable account" and the request failed immediately instead
// of waiting out a cooldown that was seconds from elapsing.
func TestCoolingCapableAccountRemainsCooldownWaitTarget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, account := range []storage.Account{
		{ID: "acc-cooling", Label: "acc-cooling", GroupName: "cyber", Provider: "custom", Status: "active"},
		// Keeps the capability map non-empty without being a candidate for this
		// provider-scoped wait computation.
		{ID: "acc-other-provider", Label: "acc-other-provider", GroupName: "cyber", Provider: "claude", Status: "active"},
	} {
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: "acc-cooling", ModelSlug: "gpt-5.6-sol",
		AvailabilityState: capability.AvailabilityVerified, Source: "custom_admin_test",
		NativeMaxContextWindow: 200000,
	}, {
		AccountID: "acc-other-provider", ModelSlug: "gpt-5.6-sol",
		AvailabilityState: capability.AvailabilityVerified, Source: "custom_admin_test",
		NativeMaxContextWindow: 200000,
	}}); err != nil {
		t.Fatal(err)
	}
	// Plain cooldown (no recheck flag): this account becomes usable the moment the
	// cooldown elapses, which is exactly what makes it a legitimate wait target.
	if err := store.SetBindingCooldown(ctx, "acc-cooling", storage.Now()+120); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())

	if wait, found := s.shortestCooldown(ctx, "cyber", "custom", "gpt-5.6-sol"); !found || wait <= 0 {
		t.Fatalf("shortestCooldown = (%v, %v), want a positive wait for the cooling capable account", wait, found)
	}

	accounts, err := store.ListActiveAccountsWithEgress(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	wait, found := s.shortestCooldownBatch(ctx, "cyber", "custom", "gpt-5.6-sol", accounts, nil)
	if !found || wait <= 0 || wait > 121*time.Second {
		t.Fatalf("shortestCooldownBatch = (%v, %v), want a positive wait <= 121s", wait, found)
	}
}
