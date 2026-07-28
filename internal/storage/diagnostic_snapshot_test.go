package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDiagnosticSnapshotAllowsWritesAndKeepsStableView(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "snapshot.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.BeginDiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID() == "" {
		t.Fatal("snapshot id is empty")
	}
	view, err := snapshot.Store(store)
	if err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.InsertAuditLog(context.Background(), AuditLogRow{Action: "after_snapshot", State: "complete"})
	}()
	select {
	case err = <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WAL writer was blocked by read-only diagnostic snapshot")
	}
	var rows int
	if err = view.ReadDB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_log WHERE action='after_snapshot'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("snapshot observed concurrent write: rows=%d", rows)
	}
	if err = store.ReadDB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_log WHERE action='after_snapshot'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("live reader did not observe concurrent write: rows=%d", rows)
	}
	if err = snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}
