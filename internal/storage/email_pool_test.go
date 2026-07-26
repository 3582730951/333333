package storage

import (
	"context"
	"path/filepath"
	"testing"
)

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
