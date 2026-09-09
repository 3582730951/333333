package scheduler

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func newBalanceContinuityScheduler(t *testing.T) *Scheduler {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(filepath.Join(t.TempDir(), "continuity.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exit-a", "exit-b", "exit-c"} {
		if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{ID: id, Name: id, Type: "http_proxy", Endpoint: "http://" + id + ".invalid", Health: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CreateGroup(ctx, storage.Group{Name: "balance", EgressIDs: []string{"exit-a"}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"account-a", "account-b"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "balance", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: id, ModelSlug: "continuity-model", AvailabilityState: "verified", Source: "continuity_test"}}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Default()
	cfg.DefaultGroup = "balance"
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	t.Cleanup(s.Close)
	return s
}

func balanceContinuityContext() context.Context {
	return WithDynamicPoolBalance(context.Background(), DynamicPoolBalancePolicy{
		Enabled: true, RPMThreshold: 10, UserGroupID: "balance-users",
		AgentClass: storage.AgentClassRoot, Fresh: true, OnlyAccountPoolTier: true,
		EgressRPMBalanceEnabled: true, EgressRPMBalanceThreshold: 10,
		EgressRPMBalanceEgressIDs: []string{"exit-a", "exit-b"},
	})
}

func TestCombinedRPMBalancePreservesSessionContinuity(t *testing.T) {
	for _, across := range []bool{false, true} {
		name := "Select"
		if across {
			name = "SelectAcross"
		}
		t.Run(name, func(t *testing.T) {
			s := newBalanceContinuityScheduler(t)
			ctx := balanceContinuityContext()
			s.SetDynamicPoolBalanceSnapshot(storage.Now(), map[string]storage.AccountRootRate{"account-a": {RootRPM: 20}, "account-b": {RootRPM: 0}})
			s.SetEgressRPMBalanceSnapshot(storage.Now(), map[string]int64{"exit-a": 20, "exit-b": 0})
			route := Route{Group: "balance", Provider: "codex", Model: "continuity-model", SkipWait: true, Affinity: routing.AffinityFromKey("thread:balance", "thread_id")}
			choices := []RouteChoice{{ChoiceKey: "balance", Route: route}}
			first, err := s.SelectAcross(ctx, choices)
			if err != nil {
				t.Fatal(err)
			}
			first.Release()
			if first.Account.ID != "account-b" || first.Egress.ID != "exit-b" {
				t.Fatalf("fresh session selected %s/%s; want low-RPM account-b/exit-b", first.Account.ID, first.Egress.ID)
			}
			before := s.Metrics().DynamicPoolBalanceSelections
			if before == 0 {
				t.Fatal("fresh session did not use account balancing")
			}

			// Simulate both the load changing and an operator editing group egress.
			if err := s.store.UpdateGroup(ctx, storage.Group{Name: "balance", EgressIDs: []string{"exit-c"}}); err != nil {
				t.Fatal(err)
			}
			s.InvalidateAccountCache()
			s.InvalidateEgressCache()
			s.SetDynamicPoolBalanceSnapshot(storage.Now(), map[string]storage.AccountRootRate{"account-a": {RootRPM: 0}, "account-b": {RootRPM: 20}})
			s.SetEgressRPMBalanceSnapshot(storage.Now(), map[string]int64{"exit-a": 0, "exit-b": 20})
			var next Lease
			if across {
				routed, selectErr := s.SelectAcross(ctx, choices)
				next, err = routed.Lease, selectErr
			} else {
				next, err = s.Select(ctx, route)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer next.Release()
			if next.Account.ID != first.Account.ID || next.Egress.ID != first.Egress.ID || next.Binding.CookieJarKey != first.Binding.CookieJarKey {
				t.Fatalf("session moved from %s/%s/%s to %s/%s/%s", first.Account.ID, first.Egress.ID, first.Binding.CookieJarKey, next.Account.ID, next.Egress.ID, next.Binding.CookieJarKey)
			}
			if s.Metrics().DynamicPoolBalanceSelections != before {
				t.Fatal("existing session was rebalanced")
			}
		})
	}
}

func TestRPMBalanceRespectsExplicitSessionPins(t *testing.T) {
	s := newBalanceContinuityScheduler(t)
	ctx := balanceContinuityContext()
	s.SetDynamicPoolBalanceSnapshot(storage.Now(), map[string]storage.AccountRootRate{"account-a": {RootRPM: 20}, "account-b": {RootRPM: 0}})
	s.SetEgressRPMBalanceSnapshot(storage.Now(), map[string]int64{"exit-a": 20, "exit-b": 0})
	ctx, _ = WithRouteChoices(ctx, []RouteChoice{{ChoiceKey: "balance", Route: Route{Group: "balance"}}}, nil)
	lease, err := s.Select(ctx, Route{Group: "balance", Provider: "codex", Model: "continuity-model", RequiredAccountID: "account-a", RequiredEgressID: "exit-a", ServerSideState: true, SkipWait: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Account.ID != "account-a" || lease.Egress.ID != "exit-a" {
		t.Fatalf("explicit session pin moved to %s/%s", lease.Account.ID, lease.Egress.ID)
	}
}

func TestCoarseAffinityDoesNotPinHistoricalEgress(t *testing.T) {
	s := newBalanceContinuityScheduler(t)
	ctx := context.Background()
	affinity := routing.AffinityFromKey("prefix:balance", "cache_prefix_hash")
	if _, err := s.upsertAffinityResult(ctx, storage.AffinityBinding{RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source, AccountID: "account-b", Provider: "codex", Model: "continuity-model", EgressID: "exit-b"}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Select(ctx, Route{Group: "balance", Provider: "codex", Model: "continuity-model", Affinity: affinity, SkipWait: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Egress.ID != "exit-a" {
		t.Fatalf("coarse cache hint pinned old egress %s", lease.Egress.ID)
	}
}

type busyOnceContinuityCoordinator struct {
	LeaseCoordinator
	requests []LeaseRequest
}

func (c *busyOnceContinuityCoordinator) TryAcquire(ctx context.Context, request LeaseRequest) (CoordinatedLease, leaseBlockReason, error) {
	c.requests = append(c.requests, request)
	if len(c.requests) == 1 {
		return nil, leaseBlockConcurrency, nil
	}
	return c.LeaseCoordinator.TryAcquire(ctx, request)
}

func TestConversationEgressSurvivesStickyRetry(t *testing.T) {
	fixture := newBalanceContinuityScheduler(t)
	coordinator := &busyOnceContinuityCoordinator{LeaseCoordinator: newLocalLeaseCoordinator()}
	s := NewWithLeaseCoordinator(fixture.store, fixture.Config(), coordinator)
	defer s.Close()
	ctx := balanceContinuityContext()
	affinity := routing.ResponseAffinityKey("response-before-wait")
	if _, err := s.upsertAffinityResult(ctx, storage.AffinityBinding{RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source, AccountID: "account-b", Provider: "codex", Model: "continuity-model", EgressID: "exit-b"}); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Select(ctx, Route{Group: "balance", Provider: "codex", Model: "continuity-model", Affinity: affinity, SkipWait: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if len(coordinator.requests) != 2 {
		t.Fatalf("expected a busy attempt followed by retry; got %d", len(coordinator.requests))
	}
	for _, request := range coordinator.requests {
		if request.AccountID != "account-b" || len(request.Resources) == 0 || request.Resources[0].ID != "exit-b" {
			t.Fatalf("sticky retry changed session identity: %+v", request)
		}
	}
}
