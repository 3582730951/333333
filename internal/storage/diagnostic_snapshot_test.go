package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestDiagnosticSnapshotDoesNotCopyEntireSQLiteDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A production database can be multiple gigabytes while the exported logical
	// tables are small. Inflate an unrelated table enough that materializing a
	// second database would be observable without making this test expensive.
	if _, err = store.DB().ExecContext(context.Background(), `
CREATE TABLE snapshot_copy_regression(payload BLOB);
INSERT INTO snapshot_copy_regression(payload) VALUES(zeroblob(8388608))`); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.BeginDiagnosticSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	matches, err := filepath.Glob(filepath.Join(dir, ".diagnostic-snapshot-*.sqlite3*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		for _, match := range matches {
			if info, statErr := os.Stat(match); statErr == nil {
				t.Logf("unexpected snapshot copy %s (%d bytes)", match, info.Size())
			}
		}
		t.Fatalf("diagnostic snapshot copied the physical SQLite database: %v", matches)
	}
	view, err := snapshot.Store(store)
	if err != nil {
		t.Fatal(err)
	}
	var bytes int64
	if err = view.ReadDB().QueryRowContext(context.Background(),
		`SELECT length(payload) FROM snapshot_copy_regression`).Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	if bytes != 8388608 {
		t.Fatalf("snapshot payload length=%d", bytes)
	}
}

func TestDiagnosticSnapshotCancellationReleasesSQLiteWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	snapshot, err := store.BeginDiagnosticSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 64; index++ {
		if err = store.InsertAuditLog(context.Background(), AuditLogRow{
			Action: "snapshot_wal_write",
			State:  "complete",
			Detail: "bounded",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err = snapshot.Close(); err != nil {
		t.Fatalf("close cancelled snapshot: %v", err)
	}

	var busy, logFrames, checkpointed int
	if err = store.DB().QueryRowContext(context.Background(),
		`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if busy != 0 {
		t.Fatalf("cancelled diagnostic snapshot still pins WAL: busy=%d log=%d checkpointed=%d",
			busy, logFrames, checkpointed)
	}
	if info, statErr := os.Stat(path + "-wal"); statErr == nil && info.Size() != 0 {
		t.Fatalf("WAL was not truncated after snapshot close: %d bytes", info.Size())
	}
}

func TestCleanupLegacyDiagnosticSnapshotsRemovesOnlyRegularSnapshotFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("open-fd-safe legacy cleanup is Linux-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, ".diagnostic-snapshot-stale.sqlite3")
	journal := legacy + "-journal"
	if err = os.WriteFile(legacy, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journal, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside")
	if err = os.WriteFile(outside, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".diagnostic-snapshot-link.sqlite3")
	if err = os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	removed, err := store.CleanupLegacyDiagnosticSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed=%d, want 2", removed)
	}
	for _, removedPath := range []string{legacy, journal} {
		if _, statErr := os.Lstat(removedPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("legacy snapshot still exists: %s (%v)", removedPath, statErr)
		}
	}
	if raw, readErr := os.ReadFile(outside); readErr != nil || string(raw) != "preserve" {
		t.Fatalf("cleanup changed symlink target: raw=%q err=%v", raw, readErr)
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Fatalf("cleanup removed symlink: %v", statErr)
	}
}

func TestCleanupLegacyDiagnosticSnapshotsPreservesOpenFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("open-fd-safe legacy cleanup is Linux-specific")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pool.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, ".diagnostic-snapshot-active.sqlite3")
	journal := legacy + "-journal"
	if err = os.WriteFile(legacy, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journal, []byte("sidecar"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := os.Open(legacy)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.CleanupLegacyDiagnosticSnapshots()
	if err != nil {
		active.Close()
		t.Fatal(err)
	}
	if removed != 0 {
		active.Close()
		t.Fatalf("removed open snapshot files=%d", removed)
	}
	for _, preserved := range []string{legacy, journal} {
		if _, statErr := os.Stat(preserved); statErr != nil {
			active.Close()
			t.Fatalf("active snapshot family member was removed: %s: %v", preserved, statErr)
		}
	}
	if err = active.Close(); err != nil {
		t.Fatal(err)
	}
	removed, err = store.CleanupLegacyDiagnosticSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed closed snapshot files=%d, want 2", removed)
	}
	for _, removedPath := range []string{legacy, journal} {
		if _, statErr := os.Stat(removedPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("closed snapshot family member still exists: %s: %v", removedPath, statErr)
		}
	}
}
