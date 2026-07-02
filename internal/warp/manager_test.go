package warp

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func newStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestExitIndexOf(t *testing.T) {
	cases := map[string]int{
		"warp-1": 1, "warp-12": 12, "warp-3-ja3": 3,
		"egress_direct": 0, "warp-": 0, "warp-x": 0, "warpx": 0, "": 0,
	}
	for id, want := range cases {
		if got := exitIndexOf(id); got != want {
			t.Errorf("exitIndexOf(%q)=%d want %d", id, got, want)
		}
	}
}

func TestAssignCFAccountPacksThreePerExit(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	cfg := config.Config{WarpEnabled: true, WarpExitCount: 2, WarpExitBasePort: 40000, WarpAccountsPerExit: 3}
	m := NewManager(cfg, store, nil, nil)
	if err := m.EnsurePool(ctx); err != nil {
		t.Fatal(err)
	}
	// 2 exits × 3 = 6 slots; the 7th account finds the pool full.
	assigned := map[string]int{}
	empties := 0
	for i := 0; i < 7; i++ {
		id := "acc-" + string(rune('a'+i))
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
		got, err := m.AssignCFAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got == "" {
			empties++
			continue
		}
		assigned[got]++
	}
	if assigned["warp-1"] != 3 || assigned["warp-2"] != 3 {
		t.Fatalf("expected 3 accounts per exit, got %+v", assigned)
	}
	if empties != 1 {
		t.Fatalf("expected exactly 1 account to find a full pool, got %d", empties)
	}
}

func TestAssignCFAccountIdempotent(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	cfg := config.Config{WarpEnabled: true, WarpExitCount: 2, WarpExitBasePort: 40000, WarpAccountsPerExit: 3}
	m := NewManager(cfg, store, nil, nil)
	_ = m.EnsurePool(ctx)
	_ = store.UpsertAccount(ctx, storage.Account{ID: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"})
	first, err := m.AssignCFAccount(ctx, "acc-1")
	if err != nil || first == "" {
		t.Fatalf("first assign: %q %v", first, err)
	}
	second, err := m.AssignCFAccount(ctx, "acc-1")
	if err != nil || second != first {
		t.Fatalf("re-assign should be idempotent: %q != %q (%v)", second, first, err)
	}
	binding, _ := store.GetEgressBinding(ctx, "acc-1")
	if len(binding.StandbyIDs()) != 1 {
		t.Fatalf("expected exactly one standby, got %v", binding.StandbyIDs())
	}
}

func TestAssignCFAccountPicksJA3VariantForSidecarAccount(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	cfg := config.Config{WarpEnabled: true, WarpExitCount: 1, WarpExitBasePort: 40000, WarpAccountsPerExit: 3, DefaultSidecarEndpoint: "http://127.0.0.1:8790"}
	m := NewManager(cfg, store, nil, nil)
	if err := m.EnsurePool(ctx); err != nil {
		t.Fatal(err)
	}
	// A sidecar primary egress means the account is JA3-bound → it must get the JA3
	// variant of the WARP exit (sidecar + chain_proxy), not the plain SOCKS exit.
	_ = store.UpsertEgressProfile(ctx, storage.EgressProfile{ID: "sc", Name: "sc", Type: "curl_cffi_sidecar", Endpoint: "http://127.0.0.1:8790", Health: "healthy"})
	_ = store.UpsertAccount(ctx, storage.Account{ID: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"})
	_ = store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: "acc-1", PrimaryEgressID: "sc"})
	got, err := m.AssignCFAccount(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got != "warp-1-ja3" {
		t.Fatalf("sidecar account should get JA3 warp variant, got %q", got)
	}
	eg, err := store.GetEgressProfile(ctx, "warp-1-ja3")
	if err != nil {
		t.Fatal(err)
	}
	if eg.Type != "curl_cffi_sidecar" || eg.ChainProxy != "socks5h://127.0.0.1:40000" {
		t.Fatalf("ja3 variant should chain through the warp socks: %+v", eg)
	}
}

func TestAssignDisabledIsNoop(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	m := NewManager(config.Config{WarpEnabled: false}, store, nil, nil)
	_ = store.UpsertAccount(ctx, storage.Account{ID: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"})
	got, err := m.AssignCFAccount(ctx, "acc-1")
	if err != nil || got != "" {
		t.Fatalf("disabled manager should be a no-op, got %q %v", got, err)
	}
}
