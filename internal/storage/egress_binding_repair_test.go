package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInitRepairsMissingEgressBindingUsingGroupDefault(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pool.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, EgressProfile{
		ID:                "egress_group_default",
		Name:              "group default",
		Type:              "http_proxy",
		Endpoint:          "http://127.0.0.1:8080",
		StreamCapable:     true,
		Health:            "healthy",
		MaxConcurrency:    16,
		DynamicConfigJSON: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.DefaultEgressID = "egress_group_default"
	group.EgressIDs = []string{"egress_group_default"}
	if err := store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, Account{ID: "acc-missing-binding", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, "DELETE FROM account_egress_bindings WHERE account_id = ?", "acc-missing-binding"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Init(ctx); err != nil {
		t.Fatal(err)
	}
	binding, err := reopened.GetEgressBinding(ctx, "acc-missing-binding")
	if err != nil {
		t.Fatal(err)
	}
	if binding.PrimaryEgressID != "egress_group_default" || binding.CookieJarKey != "acc-missing-binding:egress_group_default" {
		t.Fatalf("binding = %+v, want repaired group default", binding)
	}
}

func TestUpsertAccountDefaultsToGroupEgress(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, EgressProfile{ID: "egress_group_default", Name: "group default", Type: "direct", StreamCapable: true, Health: "healthy", DynamicConfigJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.DefaultEgressID = "egress_group_default"
	group.EgressIDs = []string{"egress_group_default"}
	if err := store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, Account{ID: "acc-new", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetEgressBinding(ctx, "acc-new")
	if err != nil {
		t.Fatal(err)
	}
	if binding.PrimaryEgressID != "egress_group_default" {
		t.Fatalf("primary egress = %q, want group default", binding.PrimaryEgressID)
	}
}
