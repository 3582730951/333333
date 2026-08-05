package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestClaudeModelProbeRefreshPolicyDistinguishesEndpointForbidden(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "bare unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, want: true},
		{name: "endpoint forbidden", status: http.StatusForbidden, body: `{"error":{"type":"forbidden","message":"not allowed"}}`},
		{name: "expired forbidden", status: http.StatusForbidden, body: `{"error":"token expired"}`, want: true},
		{name: "missing scope", status: http.StatusForbidden, body: `{"error":"insufficient permissions; missing scope"}`},
		{name: "server error", status: http.StatusServiceUnavailable, body: `{"error":"overloaded"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claudeModelProbeShouldRefresh(test.status, nil, []byte(test.body)); got != test.want {
				t.Fatalf("refresh policy = %v, want %v", got, test.want)
			}
		})
	}
}

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

func TestKiroCatalogCapabilitiesPreserveLiveGPTInputLimit(t *testing.T) {
	caps := kiroCatalogCapabilities("kiro-account", []storage.KiroModelDescriptor{{
		PublicID: "gpt-5.6-sol", UpstreamID: "gpt-5.6-sol",
		MaxInputTokens: 272000, Complete: true,
	}})
	if len(caps) != 1 {
		t.Fatalf("capabilities = %+v, want one row", caps)
	}
	if caps[0].NativeContextWindow != 272000 || caps[0].NativeMaxContextWindow != 272000 {
		t.Fatalf("Kiro live limit was overwritten by Codex contract: %+v", caps[0])
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
