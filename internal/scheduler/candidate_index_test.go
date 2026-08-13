package scheduler

import (
	"context"
	"fmt"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCandidateIndexKeepsNormalSelectionConstantWork(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for i := 0; i < 128; i++ {
		id := fmt.Sprintf("indexed-%03d", i)
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := New(store, config.Default())
	for i := 0; i < 2; i++ {
		lease, err := scheduler.Select(ctx, Route{Group: "cyber", Provider: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
	}
	metrics := scheduler.Metrics()
	if metrics.CandidateIndexBuilds != 1 || metrics.CandidateFallbacks != 0 || metrics.CandidateEvaluations != 4 {
		t.Fatalf("candidate index metrics=%+v", metrics)
	}
}

func TestCandidateIndexFallsBackOnlyWhenBothSamplesAreUnavailable(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("fallback-%d", i)
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := New(store, config.Default())
	route := Route{Group: "cyber", Provider: "codex", Exclude: map[string]bool{}}
	for i := 0; i < 7; i++ {
		route.Exclude[fmt.Sprintf("fallback-%d", i)] = true
	}
	lease, err := scheduler.Select(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Account.ID != "fallback-7" || scheduler.Metrics().CandidateFallbacks == 0 {
		t.Fatalf("lease=%s metrics=%+v", lease.Account.ID, scheduler.Metrics())
	}
}

func TestStructuralCandidateCountIgnoresTransientAvailability(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	const (
		group = "structural-count"
		model = "gpt-5.6-sol"
	)
	if err := store.CreateGroup(ctx, storage.Group{Name: group}); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: "structural-cooling", GroupName: group, Provider: "codex", Status: "active",
		QuarantineUntil: storage.Now() + 3600,
	}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token-structural"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: account.ID, ModelSlug: model, AvailabilityState: "verified", Source: "test",
	}}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	defer s.Close()
	route := Route{Group: group, Provider: "codex", Model: model}
	structural, err := s.StructuralCandidateCount(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := s.EligibleCandidateCount(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	if structural != 1 || eligible != 0 {
		t.Fatalf("structural=%d eligible=%d, want 1/0 for transient quarantine", structural, eligible)
	}
	before := s.RouteStructureVersion()
	s.RefreshAccountCache()
	if afterTransient := s.RouteStructureVersion(); afterTransient != before {
		t.Fatalf("transient refresh advanced structural version: before=%d after=%d", before, afterTransient)
	}
	s.InvalidateAccountCache()
	if after := s.RouteStructureVersion(); after <= before {
		t.Fatalf("route state version did not advance: before=%d after=%d", before, after)
	}
}

func TestCandidateIndexRollbackUsesFullScan(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("legacy-%02d", i)
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.SchedulerIndexEnabled = false
	scheduler := New(store, cfg)
	lease, err := scheduler.Select(ctx, Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	metrics := scheduler.Metrics()
	if metrics.CandidateIndexBuilds != 0 || metrics.CandidateEvaluations != 16 {
		t.Fatalf("rollback metrics=%+v", metrics)
	}
}
