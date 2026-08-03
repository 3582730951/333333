package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/secretbox"
)

func TestEmailPoolEmptyListIsJSONSafe(t *testing.T) {
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, total, err := store.ListEmailAccounts(context.Background(), 1, 50, "", "")
	if err != nil || rows == nil || len(rows) != 0 || total != 0 {
		t.Fatalf("empty email pool rows=%#v total=%d err=%v", rows, total, err)
	}
}

func TestEmailPoolMigratesLegacyMissingColumns(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.db.Exec(`CREATE TABLE email_pool(id TEXT PRIMARY KEY, email TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`INSERT INTO email_pool(id,email) VALUES('legacy-email','legacy@example.test')`); err != nil {
		t.Fatal(err)
	}
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, total, err := store.ListEmailAccounts(context.Background(), 1, 50, "", "")
	if err != nil {
		t.Fatalf("list legacy nullable email row: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("email rows total=%d rows=%#v", total, rows)
	}
	if rows[0].Status != "idle" || rows[0].ClientID != "" || rows[0].LastUsedAt != 0 {
		t.Fatalf("legacy defaults not normalized: %+v", rows[0])
	}
	counts, err := store.CountEmailAccountsByStatus(context.Background())
	if err != nil || counts["idle"] != 1 {
		t.Fatalf("legacy status counts=%v err=%v", counts, err)
	}
}

func TestEmailPoolMigratesLegacyStatusAliases(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.Exec(`CREATE TABLE email_pool(
id TEXT PRIMARY KEY,email TEXT,password TEXT,client_id TEXT,refresh_token TEXT,status TEXT,
group_name TEXT,error_message TEXT,last_used_at INTEGER,created_at INTEGER,updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, status string }{{"ready", "ready"}, {"busy", "busy"}, {"failed", "failed"}, {"done", "consumed"}} {
		if _, err := store.db.Exec(`INSERT INTO email_pool(id,email,status) VALUES(?,?,?)`, row.id, row.id+"@example.test", row.status); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{"ready": "idle", "busy": "in_use", "failed": "error", "done": "used"}
	for id, want := range wants {
		account, found, err := store.GetEmailAccount(ctx, id)
		if err != nil || !found || account.Status != want {
			t.Fatalf("%s status=%q found=%v err=%v, want %q", id, account.Status, found, err, want)
		}
	}
}

func TestEmailPoolReadsLegacyNullableRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.db.Exec(`CREATE TABLE email_pool(
id TEXT PRIMARY KEY, email TEXT, password TEXT, client_id TEXT, refresh_token TEXT,
status TEXT, group_name TEXT, error_message TEXT, last_used_at INTEGER,
created_at INTEGER, updated_at INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.Exec(`INSERT INTO email_pool(id,email,password,client_id,refresh_token,status,group_name,error_message,last_used_at,created_at,updated_at)
VALUES('legacy-null','nullable@example.test',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, total, err := store.ListEmailAccounts(context.Background(), 1, 50, "", "")
	if err != nil {
		t.Fatalf("list legacy nullable email row: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Status != "idle" || rows[0].ClientID != "" {
		t.Fatalf("legacy nullable row not normalized: total=%d rows=%+v", total, rows)
	}
}

func TestEmailPoolCredentialsEncryptedAtRest(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("email-pool-encryption-key"))
	account := EmailAccount{
		ID: "mail-secure", Email: "secure@example.test", Password: "mail-password",
		ClientID: "oauth-client", RefreshToken: "oauth-refresh", Status: "idle",
	}
	if err := store.InsertEmailAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	var password, clientID, refreshToken string
	if err := store.DB().QueryRowContext(ctx, `
SELECT password,client_id,refresh_token FROM email_pool WHERE id=?`, account.ID).
		Scan(&password, &clientID, &refreshToken); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"password": password, "client_id": clientID, "refresh_token": refreshToken,
	} {
		if !secretbox.IsSealed(value) || strings.Contains(value, "oauth-") || value == "mail-password" {
			t.Fatalf("%s was not sealed at rest: %q", name, value)
		}
	}
	got, found, err := store.GetEmailAccount(ctx, account.ID)
	if err != nil || !found {
		t.Fatalf("GetEmailAccount found=%v err=%v", found, err)
	}
	if got.Password != account.Password || got.ClientID != account.ClientID ||
		got.RefreshToken != account.RefreshToken {
		t.Fatalf("decrypted email credentials=%+v", got)
	}
}

func TestEncryptExistingTokensMigratesEmailPoolCredentials(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pool.sqlite3")
	legacy, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := legacy.InsertEmailAccount(ctx, EmailAccount{
		ID: "mail-legacy", Email: "legacy@example.test", Password: "legacy-password",
		ClientID: "legacy-client", RefreshToken: "legacy-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store.SetTokenEncryptionKey([]byte("email-pool-migration-key"))
	if count, err := store.EncryptExistingTokens(ctx); err != nil || count != 1 {
		t.Fatalf("EncryptExistingTokens count=%d err=%v", count, err)
	}
	if count, err := store.EncryptExistingTokens(ctx); err != nil || count != 0 {
		t.Fatalf("second migration count=%d err=%v", count, err)
	}
	got, found, err := store.GetEmailAccount(ctx, "mail-legacy")
	if err != nil || !found || got.RefreshToken != "legacy-refresh" {
		t.Fatalf("migrated account=%+v found=%v err=%v", got, found, err)
	}
}

func TestReserveEmailAccountIsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, account := range []EmailAccount{
		{ID: "mail-a", Email: "a@example.test", RefreshToken: "rt-a"},
		{ID: "mail-b", Email: "b@example.test", RefreshToken: "rt-b"},
	} {
		if err := store.InsertEmailAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	results := make(chan string, 12)
	errorsSeen := make(chan error, 12)
	for index := 0; index < 12; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			account, reserveErr := store.ReserveEmailAccount(ctx, "")
			if reserveErr != nil {
				errorsSeen <- reserveErr
				return
			}
			results <- account.ID
		}()
	}
	wg.Wait()
	close(results)
	close(errorsSeen)

	unique := map[string]bool{}
	for id := range results {
		if unique[id] {
			t.Fatalf("email account %q was reserved more than once", id)
		}
		unique[id] = true
	}
	if len(unique) != 2 {
		t.Fatalf("reserved accounts=%v, want both pool entries exactly once", unique)
	}
	for reserveErr := range errorsSeen {
		if reserveErr != sql.ErrNoRows {
			t.Fatalf("unexpected reserve error: %v", reserveErr)
		}
	}
}

func TestBulkInsertEmailAccountsDeduplicatesCaseInsensitively(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	inserted, err := store.BulkInsertEmailAccounts(ctx, []EmailAccount{
		{Email: "Person@Example.test", RefreshToken: "rt-a"},
		{Email: "person@example.test", RefreshToken: "rt-b"},
		{Email: "second@example.test", RefreshToken: "rt-c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 2 {
		t.Fatalf("inserted=%d want=2", inserted)
	}
	accounts, total, err := store.ListEmailAccounts(ctx, 1, 20, "", "")
	if err != nil || total != 2 || len(accounts) != 2 {
		t.Fatalf("accounts=%+v total=%d err=%v", accounts, total, err)
	}
}
