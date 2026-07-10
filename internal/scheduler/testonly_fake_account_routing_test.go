package scheduler

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// fakeQuotaAccountID deliberately lives in a _test.go file. The Go toolchain never
// includes this fixture, its credential, or its quota snapshot in a normal/release
// build; it exists only inside the scheduler test binary.
const fakeQuotaAccountID = "acc-z-fake-quota-test-only"

func insertFakeQuotaAccount(t *testing.T, store *storage.Store, group, model string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{
		ID:        fakeQuotaAccountID,
		Label:     "fake quota account (test only)",
		GroupName: group,
		Provider:  "codex",
		Status:    "active",
		CreatedAt: 2,
	}, storage.AccountToken{AccessToken: "fake-token-never-sent-test-only"}); err != nil {
		t.Fatalf("insert fake account: %v", err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID:              fakeQuotaAccountID,
		ModelSlug:              model,
		NativeMaxContextWindow: 372000,
		Source:                 "test-only-fixture",
		LastProbeAt:            storage.Now(),
	}}); err != nil {
		t.Fatalf("insert fake account capability: %v", err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         fakeQuotaAccountID,
		Provider:          "codex",
		Model:             model,
		LimiterType:       "tokens",
		Source:            "test-only-fixture",
		UsedPercent:       1,
		LimitTokens:       1_000_000,
		RemainingTokens:   990_000,
		LimitRequests:     10_000,
		RemainingRequests: 9_900,
		ResetAt:           storage.Now() + 3600,
		Status:            "allowed",
	}); err != nil {
		t.Fatalf("insert fake account quota: %v", err)
	}
}

func TestSchedulerRoutesToTestOnlyFakeAccountWithAvailableQuota(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const (
		group = "fake-routing-test"
		model = "gpt-5.6-sol"
	)

	// A normal account and the fake account are both eligible. Keeping the first
	// lease in flight makes the least-loaded scheduler route the concurrent request
	// to the fake account, which proves the fixture participates in real selection.
	if err := store.UpsertAccount(ctx, storage.Account{
		ID:        "acc-a-normal-test",
		Label:     "normal test account",
		GroupName: group,
		Provider:  "codex",
		Status:    "active",
		CreatedAt: 1,
	}, storage.AccountToken{AccessToken: "normal-token-test-only"}); err != nil {
		t.Fatalf("insert normal account: %v", err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID:              "acc-a-normal-test",
		ModelSlug:              model,
		NativeMaxContextWindow: 372000,
		Source:                 "test-only-fixture",
		LastProbeAt:            storage.Now(),
	}}); err != nil {
		t.Fatalf("insert normal capability: %v", err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         "acc-a-normal-test",
		Provider:          "codex",
		Model:             model,
		LimiterType:       "tokens",
		Source:            "test-only-fixture",
		UsedPercent:       1,
		LimitTokens:       1_000_000,
		RemainingTokens:   990_000,
		LimitRequests:     10_000,
		RemainingRequests: 9_900,
		ResetAt:           storage.Now() + 3600,
		Status:            "allowed",
	}); err != nil {
		t.Fatalf("insert normal quota: %v", err)
	}
	insertFakeQuotaAccount(t, store, group, model)

	s := New(store, config.Default())
	first, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatalf("select normal account: %v", err)
	}
	defer first.Release()
	if first.Account.ID != "acc-a-normal-test" {
		t.Fatalf("first account = %q, want normal account", first.Account.ID)
	}

	second, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatalf("select concurrent account: %v", err)
	}
	defer second.Release()
	if second.Account.ID != fakeQuotaAccountID {
		t.Fatalf("concurrent account = %q, want test-only fake account", second.Account.ID)
	}

	if _, limited, err := store.AccountRateLimitCooldownUntil(ctx, fakeQuotaAccountID, "codex", model, storage.Now()); err != nil {
		t.Fatalf("read fake quota: %v", err)
	} else if limited {
		t.Fatal("fake account's positive test quota was treated as exhausted")
	}

	// Flip only the fake snapshot to exhausted and build a fresh scheduler. The
	// same account must now disappear from routing, proving the earlier selection
	// was quota-aware rather than merely accepting any active database row.
	first.Release()
	second.Release()
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         fakeQuotaAccountID,
		Provider:          "codex",
		Model:             model,
		LimiterType:       "tokens",
		Source:            "test-only-fixture",
		UsedPercent:       100,
		LimitTokens:       1_000_000,
		RemainingTokens:   0,
		LimitRequests:     10_000,
		RemainingRequests: 9_900,
		ResetAt:           storage.Now() + 3600,
		Status:            "rejected",
	}); err != nil {
		t.Fatalf("exhaust fake quota: %v", err)
	}
	third, err := New(store, config.Default()).Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatalf("select after exhausting fake quota: %v", err)
	}
	defer third.Release()
	if third.Account.ID != "acc-a-normal-test" {
		t.Fatalf("exhausted fake account remained routable: selected %q", third.Account.ID)
	}
}

func TestUnifiedCodexParentAffinityStaysStickyWhileFakeAbsorbsIndependentLease(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const (
		group           = "fake-affinity-test"
		model           = "gpt-5.6-sol"
		normalAccountID = "acc-a-normal-affinity-test"
	)

	if err := store.UpsertAccount(ctx, storage.Account{
		ID:        normalAccountID,
		Label:     "normal affinity test account",
		GroupName: group,
		Provider:  "codex",
		Status:    "active",
		CreatedAt: 1,
	}, storage.AccountToken{AccessToken: "normal-affinity-token-test-only"}); err != nil {
		t.Fatalf("insert normal account: %v", err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID:              normalAccountID,
		ModelSlug:              model,
		NativeMaxContextWindow: 372000,
		Source:                 "test-only-fixture",
		LastProbeAt:            storage.Now(),
	}}); err != nil {
		t.Fatalf("insert normal capability: %v", err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         normalAccountID,
		Provider:          "codex",
		Model:             model,
		LimiterType:       "tokens",
		Source:            "test-only-fixture",
		UsedPercent:       1,
		LimitTokens:       1_000_000,
		RemainingTokens:   990_000,
		LimitRequests:     10_000,
		RemainingRequests: 9_900,
		ResetAt:           storage.Now() + 3600,
		Status:            "allowed",
	}); err != nil {
		t.Fatalf("insert normal quota: %v", err)
	}
	insertFakeQuotaAccount(t, store, group, model)

	s := New(store, config.Default())
	rootRequest, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	rootRequest.Header.Set("thread-id", "root-with-subagents-test-only")
	rootAffinity := routing.ExtractAffinityKey(rootRequest, []byte(`{"prompt_cache_key":"root-with-subagents-test-only"}`))
	childRequest, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	childRequest.Header.Set("thread-id", "child-test-only")
	childRequest.Header.Set("x-codex-parent-thread-id", "root-with-subagents-test-only")
	childAffinity := routing.ExtractAffinityKey(childRequest, []byte(`{"prompt_cache_key":"child-test-only"}`))
	if rootAffinity.Hash == "" || rootAffinity.Hash != childAffinity.Hash || rootAffinity.Source != routing.CodexRootThreadAffinitySource || childAffinity.Source != routing.CodexRootThreadAffinitySource {
		t.Fatalf("root/child affinities are not canonical: root=%+v child=%+v", rootAffinity, childAffinity)
	}
	rootRoute := Route{
		Group:    group,
		Provider: "codex",
		Model:    model,
		Affinity: rootAffinity,
	}

	root, err := s.Select(ctx, rootRoute)
	if err != nil {
		t.Fatalf("select Codex root thread: %v", err)
	}
	defer root.Release()
	if root.Account.ID != normalAccountID {
		t.Fatalf("root account = %q, want normal account", root.Account.ID)
	}

	// With the root lease still active, the least-loaded independent request must use
	// the idle fake account. This is a real no-affinity selection, not a forced bind.
	independent, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatalf("select independent concurrent lease: %v", err)
	}
	if independent.Account.ID != fakeQuotaAccountID {
		independent.Release()
		t.Fatalf("independent account = %q, want test-only fake account", independent.Account.ID)
	}
	independent.Release()

	// The fake account is idle again while the normal account is still busy. A child
	// agent carrying the root's unified parent affinity must nevertheless stay on the
	// root account, preserving its upstream state and prompt-cache locality.
	childRoute := rootRoute
	childRoute.Affinity = childAffinity
	child, err := s.Select(ctx, childRoute)
	if err != nil {
		t.Fatalf("select Codex child agent: %v", err)
	}
	defer child.Release()
	if child.Account.ID != root.Account.ID {
		t.Fatalf("child account = %q, want root account %q", child.Account.ID, root.Account.ID)
	}
	bound, err := store.GetAffinityBinding(ctx, rootAffinity.Hash)
	if err != nil {
		t.Fatalf("read unified parent affinity: %v", err)
	}
	if bound.AccountID != normalAccountID || bound.Source != rootAffinity.Source {
		t.Fatalf("unified parent affinity = %+v, want account=%q source=%q", bound, normalAccountID, rootAffinity.Source)
	}

	// Reject the fake account while both root and child leases keep the normal account
	// busier. The next no-affinity request must still exclude fake and use normal.
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         fakeQuotaAccountID,
		Provider:          "codex",
		Model:             model,
		LimiterType:       "tokens",
		Source:            "test-only-fixture",
		UsedPercent:       100,
		LimitTokens:       1_000_000,
		RemainingTokens:   0,
		LimitRequests:     10_000,
		RemainingRequests: 9_900,
		ResetAt:           storage.Now() + 3600,
		Status:            "rejected",
	}); err != nil {
		t.Fatalf("reject fake quota: %v", err)
	}
	afterReject, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatalf("select after rejecting fake quota: %v", err)
	}
	defer afterReject.Release()
	if afterReject.Account.ID != normalAccountID {
		t.Fatalf("rejected fake account remained routable: selected %q", afterReject.Account.ID)
	}
}
