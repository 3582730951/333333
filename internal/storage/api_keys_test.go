package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAPIKeyProviderHintPersists(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := APIKey{
		KeyHash:      "hash-provider-hint",
		Label:        "codex installer",
		KeyType:      "downstream",
		GroupName:    "cyber",
		ProviderHint: "codex",
		Enabled:      true,
		Secret:       "cap_provider_hint",
	}
	if err := s.UpsertAPIKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LookupAPIKey(context.Background(), key.KeyHash)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("key not found")
	}
	if got.ProviderHint != "codex" {
		t.Fatalf("provider_hint = %q, want codex", got.ProviderHint)
	}
	keys, err := s.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 || keys[0].ProviderHint != "codex" {
		t.Fatalf("list did not preserve provider_hint: %#v", keys)
	}
}
