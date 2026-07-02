package storage

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/secretbox"
)

// TestTokenEncryptionAtRest proves the secret is ciphertext on disk (a reader without
// the key sees an enc:v1: blob) while a keyed reader transparently gets the plaintext.
func TestTokenEncryptionAtRest(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "enc.sqlite3")

	// Writer with encryption enabled.
	w, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(ctx); err != nil {
		t.Fatal(err)
	}
	w.SetTokenEncryptionKey([]byte("deployment-secret"))
	if err := w.UpsertAccount(ctx,
		Account{ID: "acc-1", GroupName: "cyber", Status: "active"},
		AccountToken{AccessToken: "plain-AT", RefreshToken: "plain-RT"}); err != nil {
		t.Fatal(err)
	}
	// Keyed read → plaintext.
	got, err := w.GetToken(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "plain-AT" || got.RefreshToken != "plain-RT" {
		t.Fatalf("keyed read = %q/%q, want plain-AT/plain-RT", got.AccessToken, got.RefreshToken)
	}
	w.Close()

	// Reopen WITHOUT a key: the stored value must be an encrypted blob, not the secret.
	r, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := r.GetToken(ctx, "acc-1")
	if err != nil {
		t.Fatal(err)
	}
	if raw.AccessToken == "plain-AT" {
		t.Fatal("access token is plaintext at rest — encryption did not apply")
	}
	if !secretbox.IsSealed(raw.AccessToken) {
		t.Fatalf("at-rest value not sealed: %q", raw.AccessToken)
	}
}

func TestListTokensByAccountIDsDecryptsAndFilters(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "batch.sqlite3")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("deployment-secret"))
	for _, row := range []struct {
		account Account
		token   AccountToken
	}{
		{Account{ID: "acc-a", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "at-a", RefreshToken: "rt-a"}},
		{Account{ID: "acc-b", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "at-b", OpenAIAPIKey: "key-b"}},
		{Account{ID: "acc-c", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "at-c"}},
	} {
		if err := store.UpsertAccount(ctx, row.account, row.token); err != nil {
			t.Fatalf("upsert %s: %v", row.account.ID, err)
		}
	}

	got, err := store.ListTokensByAccountIDs(ctx, []string{"acc-a", "acc-b", "missing", "acc-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("tokens = %#v, want two requested existing rows", got)
	}
	if got["acc-a"].AccessToken != "at-a" || got["acc-a"].RefreshToken != "rt-a" {
		t.Fatalf("acc-a token = %#v, want decrypted access/refresh", got["acc-a"])
	}
	if got["acc-b"].AccessToken != "at-b" || got["acc-b"].OpenAIAPIKey != "key-b" {
		t.Fatalf("acc-b token = %#v, want decrypted access/api key", got["acc-b"])
	}
	if _, ok := got["acc-c"]; ok {
		t.Fatalf("loaded unrequested token: %#v", got["acc-c"])
	}
}

// TestEncryptExistingTokensMigration verifies legacy plaintext rows are upgraded.
func TestEncryptExistingTokensMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mig.sqlite3")

	// Write a row with encryption OFF (simulates a pre-encryption pool).
	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertAccount(ctx,
		Account{ID: "acc-1", GroupName: "cyber", Status: "active"},
		AccountToken{AccessToken: "legacy-AT"}); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	// Reopen WITH a key and run the migration.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	s.SetTokenEncryptionKey([]byte("k"))
	n, err := s.EncryptExistingTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migration upgraded %d rows, want 1", n)
	}
	// Still readable as plaintext through the keyed store...
	got, err := s.GetToken(ctx, "acc-1")
	if err != nil || got.AccessToken != "legacy-AT" {
		t.Fatalf("post-migration read = %q,%v; want legacy-AT", got.AccessToken, err)
	}
	// ...and a second migration pass is a no-op (idempotent).
	if n2, _ := s.EncryptExistingTokens(ctx); n2 != 0 {
		t.Fatalf("second migration upgraded %d rows, want 0 (idempotent)", n2)
	}
}

func TestAPIKeySecretEncryptionAtRest(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "apikey.sqlite3")

	w, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Init(ctx); err != nil {
		t.Fatal(err)
	}
	w.SetTokenEncryptionKey([]byte("deployment-secret"))
	if err := w.UpsertAPIKey(ctx, APIKey{KeyHash: "hash-1", Label: "client", Enabled: true, Secret: "cap_plain"}); err != nil {
		t.Fatal(err)
	}
	got, found, err := w.LookupAPIKey(ctx, "hash-1")
	if err != nil || !found {
		t.Fatalf("keyed lookup found=%v err=%v", found, err)
	}
	if got.Secret != "cap_plain" {
		t.Fatalf("keyed secret = %q, want cap_plain", got.Secret)
	}
	w.Close()

	r, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Init(ctx); err != nil {
		t.Fatal(err)
	}
	raw, found, err := r.LookupAPIKey(ctx, "hash-1")
	if err != nil || !found {
		t.Fatalf("raw lookup found=%v err=%v", found, err)
	}
	if raw.Secret == "cap_plain" {
		t.Fatal("api key secret is plaintext at rest — encryption did not apply")
	}
	if !secretbox.IsSealed(raw.Secret) {
		t.Fatalf("at-rest api key secret not sealed: %q", raw.Secret)
	}
}

func TestEncryptExistingTokensMigratesAPIKeySecrets(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "apikey-mig.sqlite3")

	legacy, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := legacy.UpsertAPIKey(ctx, APIKey{KeyHash: "hash-1", Label: "client", Enabled: true, Secret: "cap_legacy"}); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	s.SetTokenEncryptionKey([]byte("deployment-secret"))
	n, err := s.EncryptExistingTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("migration upgraded %d rows, want 1", n)
	}
	got, found, err := s.LookupAPIKey(ctx, "hash-1")
	if err != nil || !found || got.Secret != "cap_legacy" {
		t.Fatalf("post-migration lookup = %+v found=%v err=%v", got, found, err)
	}
	if n2, _ := s.EncryptExistingTokens(ctx); n2 != 0 {
		t.Fatalf("second migration upgraded %d rows, want 0", n2)
	}
}
