package scheduler

import (
	"context"
	"fmt"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func addAcrossTestAccounts(t *testing.T, store *storage.Store, group, prefix, model string, count int) []string {
	t.Helper()
	ctx := context.Background()
	if err := store.CreateGroup(ctx, storage.Group{Name: group}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("%s-%02d", prefix, index)
		account := storage.Account{ID: id, Label: id, GroupName: group, Provider: "codex", Status: "active"}
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
		if model != "" {
			if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
				AccountID: id, ModelSlug: model, AvailabilityState: "verified", Source: "select_across_test",
			}}); err != nil {
				t.Fatal(err)
			}
		}
		ids = append(ids, id)
	}
	return ids
}

func acrossChoices(model string, groups ...string) []RouteChoice {
	choices := make([]RouteChoice, 0, len(groups))
	for _, group := range groups {
		choices = append(choices, RouteChoice{ChoiceKey: group, Route: Route{Group: group, Provider: "codex", Model: model, SkipWait: true}})
	}
	return choices
}

func TestSelectAcrossFairnessAndCapacityRatio(t *testing.T) {
	t.Run("equal-1000", func(t *testing.T) {
		store := testStore(t)
		const model = "exact-across-equal"
		addAcrossTestAccounts(t, store, "across-equal-a", "equal-a", model, 2)
		addAcrossTestAccounts(t, store, "across-equal-b", "equal-b", model, 2)
		s := New(store, config.Default())
		defer s.Close()
		counts := map[string]int{}
		leases := make([]Lease, 0, 1000)
		for range 1000 {
			routed, err := s.SelectAcross(context.Background(), acrossChoices(model, "across-equal-a", "across-equal-b"))
			if err != nil {
				t.Fatal(err)
			}
			counts[routed.ChoiceKey]++
			leases = append(leases, routed.Lease)
		}
		for _, lease := range leases {
			lease.Release()
		}
		if delta := counts["across-equal-a"] - counts["across-equal-b"]; delta < -1 || delta > 1 {
			t.Fatalf("equal target leases=%v delta=%d", counts, delta)
		}
	})

	t.Run("five-to-two-first-wave", func(t *testing.T) {
		store := testStore(t)
		const model = "exact-across-ratio"
		addAcrossTestAccounts(t, store, "across-ratio-five", "ratio-five", model, 5)
		addAcrossTestAccounts(t, store, "across-ratio-two", "ratio-two", model, 2)
		s := New(store, config.Default())
		defer s.Close()
		counts := map[string]int{}
		leases := make([]Lease, 0, 7)
		for range 7 {
			routed, err := s.SelectAcross(context.Background(), acrossChoices(model, "across-ratio-five", "across-ratio-two"))
			if err != nil {
				t.Fatal(err)
			}
			counts[routed.ChoiceKey]++
			leases = append(leases, routed.Lease)
		}
		for _, lease := range leases {
			lease.Release()
		}
		if counts["across-ratio-five"] != 5 || counts["across-ratio-two"] != 2 {
			t.Fatalf("first-wave leases=%v", counts)
		}
	})
}

func TestSelectAcrossDeduplicatesMembershipAndFiltersExactCapability(t *testing.T) {
	store := testStore(t)
	const model = "exact-across-filter"
	overlapIDs := addAcrossTestAccounts(t, store, "across-overlap", "overlap", model, 1)
	healthyIDs := addAcrossTestAccounts(t, store, "across-healthy", "healthy", model, 1)
	addAcrossTestAccounts(t, store, "across-wrong-model", "wrong", "different-model", 3)
	s := New(store, config.Default())
	defer s.Close()

	choices := []RouteChoice{
		{ChoiceKey: "overlap-a", Route: Route{Group: "across-overlap", Provider: "codex", Model: model, SkipWait: true}},
		{ChoiceKey: "overlap-b", Route: Route{Group: "across-overlap", Provider: "codex", Model: model, SkipWait: true}},
		{ChoiceKey: "healthy", Route: Route{Group: "across-healthy", Provider: "codex", Model: model, SkipWait: true}},
		{ChoiceKey: "wrong-model", Route: Route{Group: "across-wrong-model", Provider: "codex", Model: model, SkipWait: true}},
	}
	first, err := s.SelectAcross(context.Background(), choices)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := s.SelectAcross(context.Background(), choices)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	seen := map[string]bool{first.Account.ID: true, second.Account.ID: true}
	if !seen[overlapIDs[0]] || !seen[healthyIDs[0]] || len(seen) != 2 {
		t.Fatalf("overlap inflated physical capacity: first=%s/%s second=%s/%s", first.ChoiceKey, first.Account.ID, second.ChoiceKey, second.Account.ID)
	}
	if first.ChoiceKey == "wrong-model" || second.ChoiceKey == "wrong-model" {
		t.Fatalf("exact capability filter selected wrong-model target")
	}
}

func TestRouteChoiceContextFollowsAtomicClaimWinner(t *testing.T) {
	store := testStore(t)
	const model = "exact-context-claim"
	addAcrossTestAccounts(t, store, "claim-left", "claim-left", model, 3)
	rightIDs := addAcrossTestAccounts(t, store, "claim-right", "claim-right", model, 1)
	s := New(store, config.Default())
	defer s.Close()
	ctx, state := WithRouteChoices(context.Background(), acrossChoices(model, "claim-left", "claim-right"), func(_ context.Context, _ string) (string, error) {
		return "claim-right", nil
	})
	lease, err := s.Select(ctx, Route{Group: "claim-left", Provider: "codex", Model: model, SkipWait: true})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if !state.Used() || state.SelectedChoice() != "claim-right" || lease.Account.ID != rightIDs[0] {
		t.Fatalf("claim winner not followed: used=%v choice=%q account=%q", state.Used(), state.SelectedChoice(), lease.Account.ID)
	}
	retry, err := s.Select(ctx, Route{
		Group: "claim-left", Provider: "codex", Model: model, SkipWait: true,
		Exclude: map[string]bool{rightIDs[0]: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer retry.Release()
	if state.ClaimedChoice() != "claim-right" || state.SelectedChoice() != "claim-left" || retry.Account.GroupName != "claim-left" {
		t.Fatalf("request-local retry did not rerun across targets: claimed=%q selected=%q account=%+v", state.ClaimedChoice(), state.SelectedChoice(), retry.Account)
	}
}

func TestSelectAcrossPersistsOnlyTrueConversationAffinity(t *testing.T) {
	store := testStore(t)
	const model = "exact-across-affinity"
	addAcrossTestAccounts(t, store, "affinity-across", "affinity", model, 1)
	s := New(store, config.Default())
	defer s.Close()

	stable := routing.AffinityFromKey("stable-prefix", "cache_prefix_hash")
	stableChoices := acrossChoices(model, "affinity-across")
	stableChoices[0].Route.Affinity = stable
	lease, err := s.SelectAcross(context.Background(), stableChoices)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if _, err := store.GetAffinityBinding(context.Background(), stable.Hash); !storage.NotFound(err) {
		t.Fatalf("stable prefix created persistent affinity: %v", err)
	}

	conversation := routing.AffinityFromKey("thread", routing.CodexRootThreadAffinitySource)
	conversationChoices := acrossChoices(model, "affinity-across")
	conversationChoices[0].Route.Affinity = conversation
	lease, err = s.SelectAcross(context.Background(), conversationChoices)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if binding, err := store.GetAffinityBinding(context.Background(), conversation.Hash); err != nil || binding.AccountID == "" {
		t.Fatalf("true conversation affinity missing: binding=%+v err=%v", binding, err)
	}
}
