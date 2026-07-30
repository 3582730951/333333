package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

func TestRouteBindingLookupTouchesAtMostHourlyAndKeepsActiveRoute(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertAccount(ctx, Account{
		ID: "retention-account", GroupName: "cyber", Provider: "codex", Status: "active",
	}, AccountToken{AccessToken: "retention-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUserGroup(ctx, UserGroup{
		ID: "retention-group", Name: "Retention Group",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUserGroupTargetBinding(ctx, UserGroupTargetBinding{
		UserGroupID: "retention-group", AffinityKey: "active-conversation",
		Target: TargetRef{Kind: TargetKindAccountPoolGroup, ID: "cyber"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, AffinityBinding{
		RouteKeyHash: "active-conversation", RouteKey: "active-conversation",
		Source: "session_id", AccountID: "retention-account",
		Provider: "codex", Model: "gpt-5.6-sol", EgressID: DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}

	sevenDaysAgo := Now() - 7*24*60*60
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE user_group_target_bindings SET updated_at=? WHERE affinity_key='active-conversation'`,
		sevenDaysAgo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE affinity_bindings SET updated_at=? WHERE route_key_hash='active-conversation'`,
		sevenDaysAgo); err != nil {
		t.Fatal(err)
	}

	groupBinding, found, err := store.GetUserGroupTargetBinding(
		ctx, "retention-group", "active-conversation", "",
	)
	if err != nil || !found || groupBinding.Target.ID != "cyber" {
		t.Fatalf("active group binding=%+v found=%v err=%v", groupBinding, found, err)
	}
	affinity, err := store.GetAffinityBinding(ctx, "active-conversation")
	if err != nil || affinity.AccountID != "retention-account" {
		t.Fatalf("active affinity=%+v err=%v", affinity, err)
	}
	firstGroupTouch, firstAffinityTouch := groupBinding.UpdatedAt, affinity.UpdatedAt
	if firstGroupTouch <= sevenDaysAgo || firstAffinityTouch <= sevenDaysAgo {
		t.Fatalf("active lookup was not touched group=%d affinity=%d", firstGroupTouch, firstAffinityTouch)
	}

	// A second request in the same hour observes the identical route without
	// advancing either timestamp, avoiding a write per downstream request.
	groupBinding, found, err = store.GetUserGroupTargetBinding(
		ctx, "retention-group", "active-conversation", "",
	)
	if err != nil || !found {
		t.Fatalf("second group lookup found=%v err=%v", found, err)
	}
	affinity, err = store.GetAffinityBinding(ctx, "active-conversation")
	if err != nil {
		t.Fatal(err)
	}
	if groupBinding.UpdatedAt != firstGroupTouch || affinity.UpdatedAt != firstAffinityTouch {
		t.Fatalf("hourly touch was not throttled group=%d/%d affinity=%d/%d",
			firstGroupTouch, groupBinding.UpdatedAt, firstAffinityTouch, affinity.UpdatedAt)
	}

	cleaned, err := store.CleanupInactiveRouteBindings(ctx, maxRouteBindingCleanupSize)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Total() != 0 {
		t.Fatalf("active seven-day route was removed: %+v", cleaned)
	}
	again, err := store.GetAffinityBinding(ctx, "active-conversation")
	if err != nil || again.AccountID != affinity.AccountID || again.Epoch != affinity.Epoch {
		t.Fatalf("active route drifted after cleanup: before=%+v after=%+v err=%v", affinity, again, err)
	}
}

func TestCleanupInactiveRouteBindingsIsBoundedAndPreservesAliasesAndFutureExpiry(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertAccount(ctx, Account{
		ID: "cleanup-account", GroupName: "cyber", Provider: "codex", Status: "active",
	}, AccountToken{AccessToken: "cleanup-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUserGroup(ctx, UserGroup{
		ID: "cleanup-group", Name: "Cleanup Group",
	}); err != nil {
		t.Fatal(err)
	}
	old := Now() - routeBindingRetentionSeconds - 60
	for index := 0; index < 3; index++ {
		if err := store.UpsertUserGroupTargetBinding(ctx, UserGroupTargetBinding{
			UserGroupID: "cleanup-group", AffinityKey: fmt.Sprintf("old-group-%d", index),
			Target: TargetRef{Kind: TargetKindAccountPoolGroup, ID: "cyber"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx,
			`UPDATE user_group_target_bindings SET updated_at=? WHERE user_group_id='cleanup-group' AND affinity_key=?`,
			old, fmt.Sprintf("old-group-%d", index)); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 3; index++ {
		hash := fmt.Sprintf("old-affinity-%d", index)
		if err := store.UpsertAffinityBinding(ctx, AffinityBinding{
			RouteKeyHash: hash, RouteKey: hash, Source: "session_id",
			AccountID: "cleanup-account", Provider: "codex", Model: "gpt-5.6-sol",
			EgressID: DefaultDirectEgressID,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(ctx,
			`UPDATE affinity_bindings SET updated_at=? WHERE route_key_hash=?`, old, hash); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, AffinityBinding{
		RouteKeyHash: "old-alias", RouteKey: "resp_old_alias", Source: "previous_response_id",
		AccountID: "cleanup-account", Provider: "codex", Model: "gpt-5.6-sol",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE affinity_aliases SET updated_at=? WHERE route_key_hash='old-alias'`, old); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, AffinityBinding{
		RouteKeyHash: "future-expiry", RouteKey: "future-expiry", Source: "resource_id",
		AccountID: "cleanup-account", Provider: "codex", Model: "gpt-5.6-sol",
		ExpiresAt: Now() + 24*60*60,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE affinity_bindings SET updated_at=? WHERE route_key_hash='future-expiry'`, old); err != nil {
		t.Fatal(err)
	}

	generation := store.AffinityGeneration()
	first, err := store.CleanupInactiveRouteBindings(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total() != 4 || first.UserGroupTargetBindings != 3 || first.AffinityBindings != 1 {
		t.Fatalf("first bounded cleanup=%+v, want 3 group + 1 affinity", first)
	}
	if store.AffinityGeneration() != generation+1 {
		t.Fatalf("affinity generation=%d, want %d", store.AffinityGeneration(), generation+1)
	}
	second, err := store.CleanupInactiveRouteBindings(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if second.Total() != 2 || second.AffinityBindings != 2 {
		t.Fatalf("second cleanup=%+v, want remaining two affinity rows", second)
	}
	if _, err := store.GetAffinityBinding(ctx, "old-alias"); err != nil {
		t.Fatalf("alias was removed by inactive primary cleanup: %v", err)
	}
	if _, err := store.GetAffinityBinding(ctx, "future-expiry"); err != nil {
		t.Fatalf("future-expiring primary binding was removed: %v", err)
	}
}

func TestRouteBindingLookupReportsCleanupRaceAsMiss(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	// This regression guard primarily documents the public miss contract used by
	// the router after bounded cleanup: deleted rows must not turn into malformed
	// target records or a non-sql sentinel.
	binding, found, err := store.GetUserGroupTargetBinding(ctx, "missing", "missing", "")
	if err != nil || found || binding.UserGroupID != "" {
		t.Fatalf("missing group binding=%+v found=%v err=%v", binding, found, err)
	}
	if _, err := store.GetAffinityBinding(ctx, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing affinity err=%v, want sql.ErrNoRows", err)
	}
}
