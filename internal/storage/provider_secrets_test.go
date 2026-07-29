package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/secretbox"
)

func TestProviderSecretsMigrateToDomainSeparatedAuthJSON(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "provider-secrets.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	key := secretbox.DeriveKey([]byte("provider-secret-test-master"))
	if err := store.SetTokenMasterKey(key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO provider_settings(
  id,provider_type,provider_key,display_name,enabled,priority,
  config_json,auth_json,created_at,updated_at
) VALUES('sms1','sms','herosms','HeroSMS',1,1,?,'{}',0,0)`,
		`{"api_key":"secret-api-key","service":"dr"}`); err != nil {
		t.Fatal(err)
	}

	if n, err := store.EncryptExistingTokens(ctx); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("migrated rows = %d, want 1", n)
	}

	var configJSON, authJSON string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT config_json,auth_json FROM provider_settings WHERE id='sms1'`).
		Scan(&configJSON, &authJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(configJSON, "secret-api-key") || strings.Contains(authJSON, "secret-api-key") {
		t.Fatalf("provider plaintext remained at rest: config=%s auth=%s", configJSON, authJSON)
	}
	var stored map[string]string
	if err := json.Unmarshal([]byte(authJSON), &stored); err != nil {
		t.Fatal(err)
	}
	if !secretbox.IsSealed(stored["api_key"]) {
		t.Fatalf("api_key was not sealed: %q", stored["api_key"])
	}
	store.EnableStrictEncryption()
	plain, err := store.OpenProviderAuthJSON("sms", "herosms", authJSON)
	if err != nil {
		t.Fatal(err)
	}
	if plain["api_key"] != "secret-api-key" {
		t.Fatalf("decrypted api_key mismatch")
	}
}

func TestProviderSecretDomainRejectsCrossProviderCiphertext(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "provider-domain.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetTokenMasterKey(secretbox.DeriveKey([]byte("provider-domain-master"))); err != nil {
		t.Fatal(err)
	}
	raw, err := store.SealProviderAuthJSON("sms", "herosms", map[string]string{"api_key": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenProviderAuthJSON("sms", "smsbower", raw); err == nil {
		t.Fatal("cross-provider ciphertext unexpectedly decrypted")
	}
}
