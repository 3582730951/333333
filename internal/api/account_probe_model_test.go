package api

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestProbeModelCodexDefaultsToGPT56Sol(t *testing.T) {
	store := apiTestStore(t)
	cfg := config.Default()
	app := &Server{cfg: cfg, store: store}

	got := app.probeModel(context.Background(), "acc", "codex")
	if got != "gpt-5.6-sol" {
		t.Fatalf("probe model = %q, want gpt-5.6-sol", got)
	}
}

func TestProbeModelCodexNeverFallsBackToCodexDedicatedModel(t *testing.T) {
	store := apiTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc", GroupName: "cyber", Status: "active", Provider: "codex"}, storage.AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: "acc", ModelSlug: "gpt-5-codex", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CodexInstallModel = "gpt-5-codex"
	app := &Server{cfg: cfg, store: store}

	got := app.probeModel(ctx, "acc", "codex")
	if got != "gpt-5.6-sol" {
		t.Fatalf("probe model = %q, want gpt-5.6-sol", got)
	}
}

func TestProbeModelCodexUsesConfiguredChatGPTModel(t *testing.T) {
	store := apiTestStore(t)
	cfg := config.Default()
	cfg.CodexInstallModel = "gpt-5.4"
	app := &Server{cfg: cfg, store: store}

	got := app.probeModel(context.Background(), "acc", "codex")
	if got != "gpt-5.4" {
		t.Fatalf("probe model = %q, want configured gpt-5.4", got)
	}
}

func apiTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
