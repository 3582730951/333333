package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInitMigratesLegacyAffinityBindingExpiryBeforeCreatingIndex(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This is the affinity_bindings schema shipped immediately before expires_at
	// was added. Init must add the column before creating an index that uses it.
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE affinity_bindings(
  route_key_hash TEXT PRIMARY KEY,
  route_key TEXT NOT NULL,
  source TEXT NOT NULL,
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  egress_id TEXT NOT NULL DEFAULT '',
  epoch INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy affinity table: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("migrate legacy store: %v", err)
	}

	var columnCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('affinity_bindings') WHERE name='expires_at'`).Scan(&columnCount); err != nil {
		t.Fatalf("inspect migrated column: %v", err)
	}
	if columnCount != 1 {
		t.Fatalf("expires_at column count = %d, want 1", columnCount)
	}

	var indexCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_affinity_bindings_expiry'`).Scan(&indexCount); err != nil {
		t.Fatalf("inspect migrated index: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("expiry index count = %d, want 1", indexCount)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("second init should remain idempotent: %v", err)
	}
}
