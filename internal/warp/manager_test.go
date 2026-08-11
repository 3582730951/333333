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

func TestEnsurePoolCreatesSelectableProfilesWithoutMutatingAccountBinding(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	cfg := config.Config{
		WarpEnabled: true, WarpExitCount: 2, WarpExitBasePort: 40000,
		DefaultSidecarEndpoint: "http://127.0.0.1:8790",
	}
	m := NewManager(cfg, store, nil, nil)
	if err := m.EnsurePool(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if got, err := m.AssignCFAccount(ctx, "acc-1"); err != nil || got != "" {
		t.Fatalf("automatic WARP assignment must remain disabled, got %q %v", got, err)
	}
	binding, err := store.GetEgressBinding(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if binding.BindingScope != storage.EgressBindingScopeGroup || len(binding.StandbyIDs()) != 0 {
		t.Fatalf("WARP pool mutated account outlet binding: %+v", binding)
	}
	eg, err := store.GetEgressProfile(ctx, "warp-1-ja3")
	if err != nil {
		t.Fatal(err)
	}
	if eg.Type != "curl_cffi_sidecar" || eg.ChainProxy != "socks5h://127.0.0.1:40000" {
		t.Fatalf("operator-selectable JA3 variant should chain through WARP SOCKS: %+v", eg)
	}
	if _, err := store.GetEgressProfile(ctx, "warp-2"); err != nil {
		t.Fatalf("second operator-selectable WARP profile missing: %v", err)
	}
}
