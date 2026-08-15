package scheduler

import (
	"context"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func TestRoutingWeightShapesFreshLoadButNeverMovesStickySession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, account := range []storage.Account{
		{ID: "weighted-high", GroupName: "cyber", Provider: "codex", Status: "active", RoutingWeight: 300},
		{ID: "weighted-normal", GroupName: "cyber", Provider: "codex", Status: "active", RoutingWeight: 100},
	} {
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token-" + account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	scheduler := New(store, config.Default())
	var leases []Lease
	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		lease, err := scheduler.Select(ctx, Route{Group: "cyber", Provider: "codex"})
		if err != nil {
			t.Fatalf("fresh select %d: %v", i, err)
		}
		counts[lease.Account.ID]++
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		lease.Release()
	}
	if counts["weighted-high"] < 27 || counts["weighted-high"] > 33 || counts["weighted-normal"] == 0 {
		t.Fatalf("weighted load did not converge near 3:1: %v", counts)
	}

	affinity := routing.AffinityKey{Key: "sticky-root", Hash: "sticky-root-hash", Source: "session_id"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source,
		AccountID: "weighted-normal", Provider: "codex", EgressID: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}
	sticky, err := scheduler.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: affinity})
	if err != nil {
		t.Fatal(err)
	}
	defer sticky.Release()
	if sticky.Account.ID != "weighted-normal" {
		t.Fatalf("routing weight moved sticky session to %q", sticky.Account.ID)
	}
}
