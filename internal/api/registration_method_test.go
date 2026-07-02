package api

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestResolveRegisterMethod locks the engine-selection precedence: an explicit request
// method wins; else the admin "default_register_method" setting; else the boot default.
// This is what makes a registration trigger automatically use the Node engine (→ produce
// auth.json → import to the pool) without the caller naming it.
func TestResolveRegisterMethod(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "p.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h := NewHandler(store, nil, "node", 1, nil)
	ctx := context.Background()

	if got := h.resolveMethod(ctx, "protocol_v2"); got != "protocol_v2" {
		t.Fatalf("explicit method = %q, want protocol_v2", got)
	}
	if got := h.resolveMethod(ctx, ""); got != "node" {
		t.Fatalf("default method = %q, want node (boot default)", got)
	}
	if err := store.SetSetting(ctx, "default_register_method", "browser_v3"); err != nil {
		t.Fatal(err)
	}
	if got := h.resolveMethod(ctx, ""); got != "browser_v3" {
		t.Fatalf("setting override = %q, want browser_v3", got)
	}
	if got := h.resolveMethod(ctx, "node"); got != "node" {
		t.Fatalf("explicit over setting = %q, want node", got)
	}
}
