package scheduler

import (
	"context"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func seedPressureCodexAccount(t *testing.T, store *storage.Store, id, group, model string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: group, Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: id, ModelSlug: model, AvailabilityState: "verified", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderPressureSnapshotCountsOnlyRequestedGroupProviderAndModel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const group, model = "gpt-group", "gpt-pressure-test"
	seedPressureCodexAccount(t, store, "codex-a", group, model)
	seedPressureCodexAccount(t, store, "codex-b", group, model)
	seedPressureCodexAccount(t, store, "wrong-model", group, "other-model")
	if err := store.UpsertAccount(ctx, storage.Account{ID: "claude", Label: "claude", GroupName: group, Provider: "claude", Status: "active"}, storage.AccountToken{AccessToken: "claude-token"}); err != nil {
		t.Fatal(err)
	}
	seedPressureCodexAccount(t, store, "other-group", "other-group", model)

	s := New(store, config.Default())
	snapshot, err := s.ProviderPressureSnapshot(ctx, group, "codex", model)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.EligibleAccounts != 2 || snapshot.AvailableAccounts != 2 || snapshot.InFlight != 0 || snapshot.Queued != 0 || snapshot.PressurePercent != 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if snapshot.ShouldAdmitKiroFairly() {
		t.Fatalf("two idle GPT accounts must not admit Kiro: %+v", snapshot)
	}
}

func TestProviderPressureSnapshotTriggersForLowAvailabilityAndOverFiftyPercentPressure(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const group, model = "gpt-group", "gpt-pressure-test"
	seedPressureCodexAccount(t, store, "codex-a", group, model)
	seedPressureCodexAccount(t, store, "codex-b", group, model)

	s := New(store, config.Default())
	first, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := s.Select(ctx, Route{Group: group, Provider: "codex", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()

	snapshot, err := s.ProviderPressureSnapshot(ctx, group, "codex", model)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.InFlight != 2 || snapshot.EligibleAccounts != 2 || snapshot.PressurePercent != 100 {
		t.Fatalf("snapshot=%+v, want two busy accounts / 100%% pressure", snapshot)
	}
	if !snapshot.ShouldAdmitKiroFairly() {
		t.Fatalf("100%% pressure must admit Kiro to the fair pool: %+v", snapshot)
	}

	low := ProviderPressureSnapshot{AvailableAccounts: 1}
	if !low.ShouldAdmitKiroFairly() {
		t.Fatal("fewer than two available accounts under 50% pressure must admit Kiro")
	}
	atThreshold := ProviderPressureSnapshot{AvailableAccounts: 2, PressurePercent: 50}
	if atThreshold.ShouldAdmitKiroFairly() {
		t.Fatal("50%% exactly must not admit Kiro")
	}
	lowAtThreshold := ProviderPressureSnapshot{AvailableAccounts: 1, PressurePercent: 50}
	if lowAtThreshold.ShouldAdmitKiroFairly() {
		t.Fatal("low capacity at exactly 50%% must not admit Kiro")
	}
}

func TestProviderPressureSnapshotIncludesRouteQueue(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const group, model = "gpt-group", "gpt-pressure-test"
	seedPressureCodexAccount(t, store, "codex-a", group, model)
	seedPressureCodexAccount(t, store, "codex-b", group, model)
	s := New(store, config.Default())
	route := Route{Group: group, Provider: "codex", Model: model}
	key, first := s.enqueue(route, "concurrency")
	_, second := s.enqueue(route, "concurrency")
	defer s.removeWaiter(key, first)
	defer s.removeWaiter(key, second)

	snapshot, err := s.ProviderPressureSnapshot(ctx, group, "codex", model)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Queued != 2 || snapshot.PressurePercent != 100 {
		t.Fatalf("snapshot=%+v, want two queued requests / 100%% pressure", snapshot)
	}
	if !snapshot.ShouldAdmitKiroFairly() {
		t.Fatalf("queued pressure must admit Kiro to the fair pool: %+v", snapshot)
	}
}

func TestEligibleCandidateCountHonorsExclusionsAndPersistentCooldowns(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const group, model = "gpt-group", "gpt-pressure-test"
	seedPressureCodexAccount(t, store, "codex-a", group, model)
	seedPressureCodexAccount(t, store, "codex-b", group, model)
	seedPressureCodexAccount(t, store, "codex-c", group, model)
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{AccountID: "codex-b", Provider: "codex", LimiterType: "5h_polled", UsedPercent: 100, Status: "rejected", ResetAt: storage.Now() + 3600, UpdatedAt: storage.Now()}); err != nil {
		t.Fatal(err)
	}

	s := New(store, config.Default())
	count, err := s.EligibleCandidateCount(ctx, Route{Group: group, Provider: "codex", Model: model, Exclude: map[string]bool{"codex-a": true}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("eligible candidates=%d, want only codex-c", count)
	}
}
