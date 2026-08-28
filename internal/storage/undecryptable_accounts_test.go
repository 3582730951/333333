package storage

import (
	"context"
	"testing"
)

// CryptoError reports *that* decryption failed and keeps only the first error with no
// identity. After a key rotation — or a per-boot auto-generated key — every affected
// account still lists as configured but presents empty credentials, so the failure
// surfaces as unexplained routing errors rather than as "re-authorize these accounts".
func TestUndecryptableAccountsAreNamedNotJustCounted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	original := make([]byte, 32)
	for i := range original {
		original[i] = byte(i + 1)
	}
	if err := store.SetTokenMasterKey(original); err != nil {
		t.Fatal(err)
	}

	account := Account{ID: "rotated-key-account", Provider: "codex", Status: "active", GroupName: "cyber"}
	if err := store.UpsertAccount(ctx, account, AccountToken{
		AccountID: account.ID, AccessToken: "secret-access", RefreshToken: "secret-refresh",
	}); err != nil {
		t.Fatal(err)
	}

	if got := store.UndecryptableAccountIDs(); len(got) != 0 {
		t.Fatalf("healthy store already reports undecryptable accounts: %v", got)
	}

	// Rotate to a key that cannot open what the previous one sealed.
	rotated := make([]byte, 32)
	for i := range rotated {
		rotated[i] = byte(200 - i)
	}
	if err := store.SetTokenMasterKey(rotated); err != nil {
		t.Fatal(err)
	}

	token, err := store.GetTokenFresh(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "" {
		t.Fatalf("token opened with the wrong key: %q", token.AccessToken)
	}

	ids := store.UndecryptableAccountIDs()
	if len(ids) != 1 || ids[0] != account.ID {
		t.Fatalf("UndecryptableAccountIDs() = %v, want exactly [%s]", ids, account.ID)
	}
}

// A store that can read its own secrets must stay silent, or the signal is noise.
func TestUndecryptableAccountsStaysEmptyOnAHealthyStore(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 3)
	}
	if err := store.SetTokenMasterKey(key); err != nil {
		t.Fatal(err)
	}

	account := Account{ID: "healthy-account", Provider: "codex", Status: "active", GroupName: "cyber"}
	if err := store.UpsertAccount(ctx, account, AccountToken{
		AccountID: account.ID, AccessToken: "secret-access",
	}); err != nil {
		t.Fatal(err)
	}

	token, err := store.GetTokenFresh(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "secret-access" {
		t.Fatalf("round trip lost the secret: %q", token.AccessToken)
	}
	if got := store.UndecryptableAccountIDs(); len(got) != 0 {
		t.Errorf("healthy account reported as undecryptable: %v", got)
	}
}

// An account with no stored secrets is not a decryption failure.
func TestUndecryptableAccountsIgnoresEmptySecrets(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 9)
	}
	if err := store.SetTokenMasterKey(key); err != nil {
		t.Fatal(err)
	}

	account := Account{ID: "no-secret-account", Provider: "codex", Status: "active", GroupName: "cyber"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccountID: account.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTokenFresh(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if got := store.UndecryptableAccountIDs(); len(got) != 0 {
		t.Errorf("an account with no secrets was reported undecryptable: %v", got)
	}
}
