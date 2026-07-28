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
