package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestStorageInitRetriesAcrossActiveWorkerSQLiteWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pool.sqlite3")
	active, err := storage.Open(path)
	if err != nil {
		t.Fatalf("open active store: %v", err)
	}
	t.Cleanup(func() { _ = active.Close() })
	if err := active.Init(ctx); err != nil {
		t.Fatalf("init active store: %v", err)
	}
	if _, err := active.DB().ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('lock-test','before',1)`); err != nil {
		t.Fatalf("seed lock row: %v", err)
	}

	activeTx, err := active.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin active worker transaction: %v", err)
	}
	if _, err := activeTx.ExecContext(ctx, `UPDATE settings SET value='held' WHERE key='lock-test'`); err != nil {
		_ = activeTx.Rollback()
		t.Fatalf("hold active worker write lock: %v", err)
	}
	t.Cleanup(func() { _ = activeTx.Rollback() })

	// Use a short contender busy timeout so the test exercises the process-level
	// retry loop without sleeping for production's five-second SQLite timeout.
	contender, err := sql.Open("sqlite3", path+"?_busy_timeout=20&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open staged worker connection: %v", err)
	}
	contender.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = contender.Close() })

	released := make(chan struct{})
	go func() {
		time.Sleep(120 * time.Millisecond)
		_ = activeTx.Commit()
		close(released)
	}()

	var attempts atomic.Int32
	err = initStorageWithLockRetryPolicy(ctx, func(ctx context.Context, _ func(string)) error {
		attempts.Add(1)
		_, execErr := contender.ExecContext(ctx, `UPDATE settings SET value='staged' WHERE key='lock-test'`)
		return execErr
	}, nil, nil, storageInitRetryPolicy{
		MaxWait:        2 * time.Second,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("staged worker did not recover after active write completed: %v", err)
	}
	<-released
	if got := attempts.Load(); got < 2 {
		t.Fatalf("attempts=%d, want at least 2 to prove lock retry", got)
	}
	var value string
	if err := contender.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='lock-test'`).Scan(&value); err != nil {
		t.Fatalf("read final value: %v", err)
	}
	if value != "staged" {
		t.Fatalf("final value=%q, want staged", value)
	}
}

func TestStorageInitDoesNotRetryNonLockFailure(t *testing.T) {
	want := errors.New("schema checksum mismatch")
	var attempts atomic.Int32
	err := initStorageWithLockRetryPolicy(context.Background(), func(context.Context, func(string)) error {
		attempts.Add(1)
		return want
	}, nil, nil, storageInitRetryPolicy{MaxWait: time.Second, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts=%d, want 1", got)
	}
}

func TestStorageInitLockRetryIsBounded(t *testing.T) {
	started := time.Now()
	err := initStorageWithLockRetryPolicy(context.Background(), func(context.Context, func(string)) error {
		return errors.New("database is locked")
	}, nil, nil, storageInitRetryPolicy{
		MaxWait:        30 * time.Millisecond,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     10 * time.Millisecond,
	})
	if err == nil || !isSQLiteLockError(err) {
		t.Fatalf("error=%v, want wrapped SQLite lock error", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded retry took %s", elapsed)
	}
}
