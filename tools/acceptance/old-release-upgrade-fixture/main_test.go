package main

import (
	"context"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestFixtureSnapshotDetectsOldDataChanges(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "pool.sqlite3")
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		store.Close()
		t.Fatalf("initialize store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close initialized store: %v", err)
	}

	db, err := openFixtureDB(databasePath)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer db.Close()
	if err := seedFixture(db, 137); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}
	snapshot, err := readFixtureSnapshot(db)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if snapshot.Accounts != 2 || snapshot.UsageRows != 137 || snapshot.CodexUsageRows != 110 || snapshot.KiroUsageRows != 27 {
		t.Fatalf("unexpected snapshot counts: %+v", snapshot)
	}
	snapshotPath := filepath.Join(t.TempDir(), "before.json")
	if err := writeSnapshotAtomic(snapshotPath, snapshot); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if err := verifyFixtureSnapshot(db, snapshotPath); err != nil {
		t.Fatalf("unchanged fixture failed verification: %v", err)
	}
	if _, err := db.Exec(`UPDATE context_journal SET encrypted_payload='lost' WHERE response_id='old-response'`); err != nil {
		t.Fatalf("mutate context fixture: %v", err)
	}
	if err := verifyFixtureSnapshot(db, snapshotPath); err == nil {
		t.Fatal("context loss was not detected by the old-release snapshot")
	}
}
