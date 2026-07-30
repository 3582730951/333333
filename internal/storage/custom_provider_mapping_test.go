package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCustomProviderModelMappingsPersistAndNormalize(t *testing.T) {
	store := newTestStore(t)
	provider := CustomProvider{
		ID: "relay", Name: "Relay", BaseURL: "https://relay.example/v1",
		UpstreamProtocol: CustomProviderProtocolAnthropicMessages,
		Enabled:          true,
		ModelMappings: map[string]string{
			" CLAUDE-SONNET-5 ": " relay-sonnet ",
			"":                  "ignored",
			"claude-empty":      " ",
		},
	}
	if err := store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetCustomProvider(t.Context(), provider.ID)
	if err != nil || !ok {
		t.Fatalf("reload provider: found=%v err=%v", ok, err)
	}
	if len(got.ModelMappings) != 1 || got.ModelMappings["claude-sonnet-5"] != "relay-sonnet" {
		t.Fatalf("stored mappings = %#v", got.ModelMappings)
	}
}

func TestCustomProviderModelMappingsRejectCaseFoldConflicts(t *testing.T) {
	store := newTestStore(t)
	err := store.UpsertCustomProvider(t.Context(), CustomProvider{
		ID: "conflict", BaseURL: "https://relay.example/v1", Enabled: true,
		ModelMappings: map[string]string{
			"CLAUDE-SONNET-5": "relay-a",
			"claude-sonnet-5": "relay-b",
		},
	})
	if err == nil {
		t.Fatal("case-folded mapping conflict was accepted")
	}
}

func TestCustomProviderModelMappingsMigrateFromPreviousSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-provider.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if _, err := store.DB().ExecContext(ctx, `
CREATE TABLE custom_providers(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  base_url TEXT NOT NULL DEFAULT '',
  upstream_protocol TEXT NOT NULL DEFAULT 'chat_completions',
  transport_profile TEXT NOT NULL DEFAULT 'generic',
  egress_ids TEXT NOT NULL DEFAULT '[]',
  enabled INTEGER NOT NULL DEFAULT 1,
  auto_discover_models INTEGER NOT NULL DEFAULT 1,
  models_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO custom_providers(
  id, name, base_url, upstream_protocol, transport_profile, egress_ids,
  enabled, auto_discover_models, models_json, created_at, updated_at
) VALUES(
  'legacy-relay', 'Legacy Relay', 'https://relay.example/v1',
  'anthropic_messages', 'generic', '[]', 1, 1, '[]', 1, 1
);`); err != nil {
		t.Fatal(err)
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetCustomProvider(ctx, "legacy-relay")
	if err != nil || !ok {
		t.Fatalf("reload migrated provider: found=%v err=%v", ok, err)
	}
	if got.ModelMappings == nil || len(got.ModelMappings) != 0 {
		t.Fatalf("legacy mapping default = %#v, want empty object", got.ModelMappings)
	}
	got.ModelMappings["claude-sonnet-5"] = "relay-sonnet"
	if err := store.UpsertCustomProvider(ctx, got); err != nil {
		t.Fatal(err)
	}
	reloaded, ok, err := store.GetCustomProvider(ctx, got.ID)
	if err != nil || !ok || reloaded.ModelMappings["claude-sonnet-5"] != "relay-sonnet" {
		t.Fatalf("mapping after migration = %#v found=%v err=%v", reloaded.ModelMappings, ok, err)
	}
}
