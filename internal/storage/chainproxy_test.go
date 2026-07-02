package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestEgressChainProxyRoundTrip(t *testing.T) {
	store := openTest(t)
	ctx := context.Background()
	in := EgressProfile{ID: "warp-1-ja3", Name: "warp-1-ja3", Type: "curl_cffi_sidecar", Endpoint: "http://127.0.0.1:8790", ChainProxy: "socks5h://127.0.0.1:40000", Health: "healthy"}
	if err := store.UpsertEgressProfile(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetEgressProfile(ctx, "warp-1-ja3")
	if err != nil {
		t.Fatal(err)
	}
	if got.ChainProxy != in.ChainProxy {
		t.Fatalf("chain_proxy not persisted: got %q want %q", got.ChainProxy, in.ChainProxy)
	}
}

func TestAddStandbyEgressIdempotent(t *testing.T) {
	store := openTest(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{ID: "acc-1", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	added, err := store.AddStandbyEgress(ctx, "acc-1", "warp-1")
	if err != nil || !added {
		t.Fatalf("first add should report added: %v %v", added, err)
	}
	added, err = store.AddStandbyEgress(ctx, "acc-1", "warp-1")
	if err != nil || added {
		t.Fatalf("second add should be a no-op: %v %v", added, err)
	}
	if _, err := store.AddStandbyEgress(ctx, "acc-1", "warp-2"); err != nil {
		t.Fatal(err)
	}
	b, _ := store.GetEgressBinding(ctx, "acc-1")
	ids := b.StandbyIDs()
	if len(ids) != 2 || ids[0] != "warp-1" || ids[1] != "warp-2" {
		t.Fatalf("unexpected standby list: %v", ids)
	}
	// Adding the primary egress as standby is a no-op (already the primary).
	added, _ = store.AddStandbyEgress(ctx, "acc-1", DefaultDirectEgressID)
	if added {
		t.Fatalf("adding the primary egress as standby should be a no-op")
	}
}
