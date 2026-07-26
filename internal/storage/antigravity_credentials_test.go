package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"codex-account-pool/internal/secretbox"
)

func TestUpsertAccountWithAntigravityCredentialsIsAtomicAndEncrypted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	store.SetTokenEncryptionKey([]byte("0123456789abcdef0123456789abcdef"))
	account := Account{ID: "antigravity-account", Label: "Antigravity", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := store.UpsertAccountWithAntigravityCredentials(ctx, account, AccountToken{AccessToken: "generic-access", RefreshToken: "generic-refresh"}, AntigravityCredentials{
		AccountID: account.ID, Email: "admin@example.com", ProjectID: "project", AccessToken: "ag-access", RefreshToken: "ag-refresh", ExpiresAt: 12345,
	}); err != nil {
		t.Fatal(err)
	}

	var genericAccess, antigravityAccess string
	if err := store.DB().QueryRowContext(ctx, `SELECT access_token FROM account_auth_tokens WHERE account_id = ?`, account.ID).Scan(&genericAccess); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT access_token FROM account_antigravity_credentials WHERE account_id = ?`, account.ID).Scan(&antigravityAccess); err != nil {
		t.Fatal(err)
	}
	if genericAccess == "generic-access" || antigravityAccess == "ag-access" || !secretbox.IsSealed(genericAccess) || !secretbox.IsSealed(antigravityAccess) {
		t.Fatalf("tokens were not encrypted at rest: generic=%q antigravity=%q", genericAccess, antigravityAccess)
	}
	credentials, err := store.GetAntigravityCredentials(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccessToken != "ag-access" || credentials.RefreshToken != "ag-refresh" || credentials.ProjectID != "project" {
		t.Fatalf("decrypted credentials = %+v", credentials)
	}

	if _, err := store.DB().ExecContext(ctx, `CREATE TRIGGER reject_antigravity_credentials BEFORE INSERT ON account_antigravity_credentials BEGIN SELECT RAISE(ABORT, 'forced credential failure'); END`); err != nil {
		t.Fatal(err)
	}
	failed := Account{ID: "antigravity-rollback", Label: "Rollback", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := store.UpsertAccountWithAntigravityCredentials(ctx, failed, AccountToken{AccessToken: "must-rollback"}, AntigravityCredentials{AccessToken: "must-rollback"}); err == nil {
		t.Fatal("expected forced credential failure")
	}
	if _, err := store.GetAccount(ctx, failed.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("account insert was not rolled back: %v", err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM account_auth_tokens WHERE account_id = ?`, failed.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("token insert was not rolled back: count=%d err=%v", count, err)
	}
}

func TestEncryptExistingTokensMigratesLegacyAntigravityCredentials(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	account := Account{ID: "legacy-antigravity", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO account_antigravity_credentials(account_id,email,project_id,access_token,refresh_token,expires_at,base_url,user_agent,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		account.ID, "legacy@example.com", "project", "legacy-access", "legacy-refresh", 12345, "", "", Now(), Now()); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("migration-secret"))
	if count, err := store.EncryptExistingTokens(ctx); err != nil || count < 1 {
		t.Fatalf("migration count=%d err=%v", count, err)
	}
	var access, refresh string
	if err := store.DB().QueryRowContext(ctx, `SELECT access_token,refresh_token FROM account_antigravity_credentials WHERE account_id=?`, account.ID).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if !secretbox.IsSealed(access) || !secretbox.IsSealed(refresh) {
		t.Fatalf("legacy credentials remain plaintext: %q %q", access, refresh)
	}
	credentials, err := store.GetAntigravityCredentials(ctx, account.ID)
	if err != nil || credentials.AccessToken != "legacy-access" || credentials.RefreshToken != "legacy-refresh" {
		t.Fatalf("decrypted credentials=%+v err=%v", credentials, err)
	}
	if count, err := store.EncryptExistingTokens(ctx); err != nil || count != 0 {
		t.Fatalf("migration is not idempotent: count=%d err=%v", count, err)
	}
}
