package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestAffinityAliasUsesTTLWithoutGrowingPrimaryBindings(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for _, id := range []string{"affinity-a", "affinity-b"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}

	before := Now()
	alias := AffinityBinding{RouteKeyHash: "response-hash", RouteKey: "resp_1", Source: "previous_response_id", AccountID: "affinity-a", Provider: "codex", Model: "gpt-5", EgressID: "direct", Epoch: 4}
	if err := store.UpsertAffinityBinding(ctx, alias); err != nil {
		t.Fatal(err)
	}
	var primaryRows, aliasRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM affinity_bindings WHERE route_key_hash=?`, alias.RouteKeyHash).Scan(&primaryRows); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM affinity_aliases WHERE route_key_hash=?`, alias.RouteKeyHash).Scan(&aliasRows); err != nil {
		t.Fatal(err)
	}
	if primaryRows != 0 || aliasRows != 1 {
		t.Fatalf("previous-response storage primary=%d aliases=%d", primaryRows, aliasRows)
	}
	got, err := store.GetAffinityBinding(ctx, alias.RouteKeyHash)
	if err != nil {
		t.Fatal(err)
	}
	retention := int64((7 * 24 * time.Hour) / time.Second)
	if got.AccountID != alias.AccountID || got.Epoch != alias.Epoch || got.ExpiresAt < before+retention || got.ExpiresAt > Now()+retention {
		t.Fatalf("stored alias=%+v", got)
	}

	alias.Epoch = 99
	alias.RouteKey = "resp_1_refreshed"
	if err := store.UpsertAffinityBinding(ctx, alias); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetAffinityBinding(ctx, alias.RouteKeyHash)
	if err != nil || got.Epoch != 4 || got.RouteKey != alias.RouteKey {
		t.Fatalf("idempotent alias update=%+v err=%v", got, err)
	}
	alias.AccountID = "affinity-b"
	if err := store.UpsertAffinityBinding(ctx, alias); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetAffinityBinding(ctx, alias.RouteKeyHash)
	if err != nil || got.AccountID != "affinity-b" || got.Epoch != 5 {
		t.Fatalf("rebound alias=%+v err=%v", got, err)
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE affinity_aliases SET expires_at=? WHERE route_key_hash=?`, Now()-1, alias.RouteKeyHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetAffinityBinding(ctx, alias.RouteKeyHash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired alias err=%v, want no rows", err)
	}
	deleted, err := store.CleanupAffinityAliases(ctx, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("cleanup deleted=%d err=%v", deleted, err)
	}
}

func TestAffinityBindingEpochChangesOnlyWhenRoutingIdentityChanges(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for _, id := range []string{"epoch-a", "epoch-b"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	binding := AffinityBinding{RouteKeyHash: "stable-hash", RouteKey: "stable", Source: "prompt_cache_key", AccountID: "epoch-a", Provider: "codex", Model: "gpt-5", EgressID: "direct", Epoch: 7}
	if err := store.UpsertAffinityBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	assertEpoch := func(want int64) {
		t.Helper()
		got, err := store.GetAffinityBinding(ctx, binding.RouteKeyHash)
		if err != nil || got.Epoch != want {
			t.Fatalf("binding=%+v err=%v want epoch=%d", got, err, want)
		}
	}
	binding.Epoch = 100
	binding.RouteKey = "updated-display-value"
	binding.Source = "stable_prefix"
	if err := store.UpsertAffinityBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	assertEpoch(7)
	for i, mutate := range []func(){
		func() { binding.AccountID = "epoch-b" },
		func() { binding.Provider = "claude" },
		func() { binding.Model = "claude-opus" },
		func() { binding.EgressID = "proxy-1" },
	} {
		mutate()
		if err := store.UpsertAffinityBinding(ctx, binding); err != nil {
			t.Fatal(err)
		}
		assertEpoch(int64(8 + i))
	}
}

func TestAffinityLegacyAliasCompatibilityCleanupAndAccountDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for _, id := range []string{"legacy-a", "legacy-b"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	updatedAt := Now() - 60
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO affinity_bindings(route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,0)`, "legacy-hash", "resp_legacy", "previous_response_id", "legacy-a", "codex", "gpt-5", "direct", 2, updatedAt, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.GetAffinityBinding(ctx, "legacy-hash")
	if err != nil || legacy.ExpiresAt != updatedAt+int64((7*24*time.Hour)/time.Second) {
		t.Fatalf("legacy binding=%+v err=%v", legacy, err)
	}
	if err := store.UpsertAffinityBinding(ctx, AffinityBinding{RouteKeyHash: "legacy-hash", RouteKey: "resp_new", Source: "previous_response_id", AccountID: "legacy-b", Provider: "codex", Model: "gpt-5", EgressID: "direct", Epoch: 10}); err != nil {
		t.Fatal(err)
	}
	resolved, err := store.GetAffinityBinding(ctx, "legacy-hash")
	if err != nil || resolved.AccountID != "legacy-b" || resolved.RouteKey != "resp_new" {
		t.Fatalf("alias did not override legacy row: %+v err=%v", resolved, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE affinity_bindings SET expires_at=? WHERE route_key_hash='legacy-hash'`, Now()-1); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.CleanupAffinityAliases(ctx, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("legacy cleanup deleted=%d err=%v", deleted, err)
	}

	if err := store.UpsertAffinityBinding(ctx, AffinityBinding{RouteKeyHash: "delete-hash", RouteKey: "resp_delete", Source: "previous_response_id", AccountID: "legacy-b"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAccount(ctx, "legacy-b"); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM affinity_aliases WHERE account_id='legacy-b'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("account delete retained %d aliases", remaining)
	}
}
