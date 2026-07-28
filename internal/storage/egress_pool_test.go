package storage

import (
	"context"
	"testing"
)

func TestEgressPoolSchemaAndProfileMetadataRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	profile := EgressProfile{
		ID:                "res-dyn-br",
		Name:              "dynamic BR residential",
		Type:              "http_proxy",
		Endpoint:          "http://user:pass@example.com:8000",
		IPMode:            "dynamic_residential",
		ProviderKey:       "cliproxy",
		DynamicConfigJSON: `{"rotation":"sid","api_base":"https://api.example.test"}`,
		StreamCapable:     true,
		Health:            "healthy",
		MaxConcurrency:    8,
	}
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatalf("upsert egress profile: %v", err)
	}
	got, err := store.GetEgressProfile(ctx, profile.ID)
	if err != nil {
		t.Fatalf("get egress profile: %v", err)
	}
	if got.IPMode != profile.IPMode || got.ProviderKey != profile.ProviderKey || got.DynamicConfigJSON != profile.DynamicConfigJSON {
		t.Fatalf("profile metadata = (%q,%q,%q), want (%q,%q,%q)",
			got.IPMode, got.ProviderKey, got.DynamicConfigJSON,
			profile.IPMode, profile.ProviderKey, profile.DynamicConfigJSON)
	}

	pool := EgressPool{ID: "pool_reg_dyn", Name: "registration dynamic", Purpose: "registration", AssignmentStrategy: "sticky_least_used"}
	if err := store.UpsertEgressPool(ctx, pool); err != nil {
		t.Fatalf("upsert egress pool: %v", err)
	}
	if err := store.UpsertEgressPoolMember(ctx, EgressPoolMember{PoolID: pool.ID, EgressID: profile.ID, Enabled: true, Capacity: 3}); err != nil {
		t.Fatalf("upsert pool member: %v", err)
	}
	if err := store.UpsertGroupEgressPolicy(ctx, GroupEgressPolicy{
		GroupName:          "cyber",
		RegistrationPoolID: pool.ID,
		RuntimePoolID:      pool.ID,
		AssignmentStrategy: "sticky_least_used",
	}); err != nil {
		t.Fatalf("upsert group policy: %v", err)
	}

	pools, err := store.ListEgressPools(ctx)
	if err != nil {
		t.Fatalf("list pools: %v", err)
	}
	if len(pools) != 1 || pools[0].ID != pool.ID || len(pools[0].Members) != 1 || pools[0].Members[0].Egress.ID != profile.ID {
		t.Fatalf("pools = %#v, want pool with hydrated member profile", pools)
	}
	policy, err := store.GetGroupEgressPolicy(ctx, "cyber")
	if err != nil {
		t.Fatalf("get group policy: %v", err)
	}
	if policy.RegistrationPoolID != pool.ID || policy.RuntimePoolID != pool.ID || policy.AssignmentStrategy != "sticky_least_used" {
		t.Fatalf("policy = %#v, want both pools and sticky_least_used", policy)
	}
}

func TestAssignAccountToEgressPoolStickyLeastUsed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"egress_a", "egress_b"} {
		if err := store.UpsertEgressProfile(ctx, EgressProfile{
			ID: id, Name: id, Type: "curl_cffi_sidecar", Endpoint: "http://127.0.0.1:8790",
			StreamCapable: true, Health: "healthy", MaxConcurrency: 16,
		}); err != nil {
			t.Fatalf("upsert profile %s: %v", id, err)
		}
	}
	if err := store.UpsertEgressPool(ctx, EgressPool{ID: "pool_runtime", Purpose: "runtime", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"egress_a", "egress_b"} {
		if err := store.UpsertEgressPoolMember(ctx, EgressPoolMember{PoolID: "pool_runtime", EgressID: id, Enabled: true, Capacity: 10}); err != nil {
			t.Fatalf("member %s: %v", id, err)
		}
	}
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, Account{ID: id, GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "tok-" + id}); err != nil {
			t.Fatalf("upsert account %s: %v", id, err)
		}
	}
	if err := store.UpsertEgressProfile(ctx, EgressProfile{
		ID: "transport-sidecar", Type: CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8791", Health: "healthy",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(ctx, AccountEgressBinding{
		AccountID: "acc-1", PrimaryEgressID: DefaultDirectEgressID, SidecarEgressID: "transport-sidecar",
	}); err != nil {
		t.Fatal(err)
	}

	b1, err := store.AssignAccountToEgressPool(ctx, "acc-1", "pool_runtime")
	if err != nil {
		t.Fatalf("assign acc-1: %v", err)
	}
	b1Again, err := store.AssignAccountToEgressPool(ctx, "acc-1", "pool_runtime")
	if err != nil {
		t.Fatalf("reassign acc-1: %v", err)
	}
	if b1Again.PrimaryEgressID != b1.PrimaryEgressID {
		t.Fatalf("sticky assignment changed from %q to %q", b1.PrimaryEgressID, b1Again.PrimaryEgressID)
	}
	if b1.SidecarEgressID != "transport-sidecar" || b1Again.SidecarEgressID != "transport-sidecar" {
		t.Fatalf("pool assignment erased account sidecar: first=%+v again=%+v", b1, b1Again)
	}

	b2, err := store.AssignAccountToEgressPool(ctx, "acc-2", "pool_runtime")
	if err != nil {
		t.Fatalf("assign acc-2: %v", err)
	}
	if b2.PrimaryEgressID == b1.PrimaryEgressID {
		t.Fatalf("least-used assignment put acc-2 on %q too; want the other healthy member", b2.PrimaryEgressID)
	}
}

func TestUpsertAccountIgnoresGroupRuntimePoolForNewBindings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertEgressProfile(ctx, EgressProfile{
		ID: "runtime_sidecar", Name: "runtime sidecar", Type: "curl_cffi_sidecar", Endpoint: "http://127.0.0.1:8790",
		StreamCapable: true, Health: "healthy", MaxConcurrency: 16,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressPool(ctx, EgressPool{ID: "pool_runtime", Purpose: "runtime", AssignmentStrategy: "sticky_least_used"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressPoolMember(ctx, EgressPoolMember{PoolID: "pool_runtime", EgressID: "runtime_sidecar", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertGroupEgressPolicy(ctx, GroupEgressPolicy{
		GroupName: "cyber", RuntimePoolID: "pool_runtime", AssignmentStrategy: "sticky_least_used",
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.UpsertAccount(ctx, Account{ID: "acc-runtime", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	binding, err := store.GetEgressBinding(ctx, "acc-runtime")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if binding.PrimaryEgressID != DefaultDirectEgressID {
		t.Fatalf("primary egress = %q, want default direct egress despite legacy group runtime policy", binding.PrimaryEgressID)
	}
}

func TestListActiveAccountsWithEgressPreservesStoredBinding(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "stored-binding-account", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"stored-egress", "group-egress"} {
		if err := store.UpsertEgressProfile(ctx, EgressProfile{
			ID: id, Type: "http_proxy", Endpoint: "http://" + id + ".example:8080", Health: "healthy", MaxConcurrency: 4,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertEgressBinding(ctx, AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: "stored-egress"}); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.EgressIDs = []string{"group-egress", "stored-egress"}
	if err := store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ListActiveAccountsWithEgress(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Binding.PrimaryEgressID != "stored-egress" || rows[0].Egress.ID != "stored-egress" {
		t.Fatalf("stored binding was overwritten by group order: %+v", rows)
	}
}
