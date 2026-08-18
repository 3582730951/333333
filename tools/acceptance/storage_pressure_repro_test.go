package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

// This acceptance test mirrors the cardinalities in
// example_zip/codex-pool-diagnostics-v3.zip. It is opt-in because the fixture is
// intentionally tens of MiB and performs hundreds of committed cleanup batches.
func TestDiagnosticScaleStoragePressureKeepsForegroundWritesLive(t *testing.T) {
	if os.Getenv("CODEX_RUN_STORAGE_PRESSURE_TEST") != "1" {
		t.Skip("set CODEX_RUN_STORAGE_PRESSURE_TEST=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pressure.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	const attemptRows = 125_391
	const expiredContexts = 64
	now := storage.Now()
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	attemptStatement, err := tx.PrepareContext(ctx, `INSERT INTO codex_upstream_attempt(
tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	for index := 0; index < attemptRows; index++ {
		if _, err = attemptStatement.ExecContext(ctx, fmt.Sprintf("pressure-tree-%06d", index),
			"pressure-account", "pressure-egress", 1, "response_headers", 429, now-60, now-1); err != nil {
			attemptStatement.Close()
			tx.Rollback()
			t.Fatal(err)
		}
	}
	attemptStatement.Close()
	contextStatement, err := tx.PrepareContext(ctx, `INSERT INTO context_journal(
response_id,affinity_hash,account_id,encrypted_payload,created_at,expires_at) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	payload := strings.Repeat("x", 1<<20)
	for index := 0; index < expiredContexts; index++ {
		if _, err = contextStatement.ExecContext(ctx, fmt.Sprintf("pressure-context-%03d", index),
			"", "", payload, now-60, now-1); err != nil {
			contextStatement.Close()
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if _, err = contextStatement.ExecContext(ctx, "live-context", "", "", "live-payload", now, now+3600); err != nil {
		contextStatement.Close()
		tx.Rollback()
		t.Fatal(err)
	}
	contextStatement.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var foregroundWrites atomic.Int64
	var foregroundMaxNanos atomic.Int64
	foregroundDone := make(chan struct{})
	var foregroundWG sync.WaitGroup
	foregroundWG.Add(1)
	go func() {
		defer foregroundWG.Done()
		for sequence := int64(0); ; sequence++ {
			select {
			case <-foregroundDone:
				return
			case <-ctx.Done():
				return
			default:
			}
			started := time.Now()
			writeErr := store.SetSetting(ctx, "pressure_foreground", fmt.Sprintf("%d", sequence))
			elapsed := time.Since(started).Nanoseconds()
			for previous := foregroundMaxNanos.Load(); elapsed > previous &&
				!foregroundMaxNanos.CompareAndSwap(previous, elapsed); previous = foregroundMaxNanos.Load() {
			}
			if writeErr != nil {
				return
			}
			foregroundWrites.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	maximumCleanup := time.Duration(0)
	cleanup := func() {
		t.Helper()
		started := time.Now()
		if _, cleanupErr := store.CleanupContextJournal(ctx); cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		if _, cleanupErr := store.CleanupCodexSessionMappings(ctx); cleanupErr != nil {
			t.Fatal(cleanupErr)
		}
		if elapsed := time.Since(started); elapsed > maximumCleanup {
			maximumCleanup = elapsed
		}
	}
	passes := (attemptRows + 255) / 256
	for pass := 0; pass < passes; pass++ {
		cleanup()
	}
	cleanup() // idempotent retry must not duplicate the daily aggregate.
	close(foregroundDone)
	foregroundWG.Wait()

	var expiredAttempts, liveContexts, aggregated int64
	if err = store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM codex_upstream_attempt WHERE expires_at<=?`, now).Scan(&expiredAttempts); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM context_journal WHERE response_id='live-context' AND expires_at>?`, now).Scan(&liveContexts); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(SUM(attempt_count),0) FROM codex_upstream_attempt_daily`).Scan(&aggregated); err != nil {
		t.Fatal(err)
	}
	if expiredAttempts != 0 || liveContexts != 1 || aggregated != attemptRows {
		t.Fatalf("expired_attempts=%d live_contexts=%d aggregated=%d", expiredAttempts, liveContexts, aggregated)
	}
	if foregroundWrites.Load() == 0 {
		t.Fatal("bounded maintenance starved every foreground write")
	}
	if maximumCleanup > 5*time.Second || time.Duration(foregroundMaxNanos.Load()) > 5*time.Second {
		t.Fatalf("latency cleanup_max=%s foreground_max=%s writes=%d",
			maximumCleanup, time.Duration(foregroundMaxNanos.Load()), foregroundWrites.Load())
	}
	if err = store.CheckWritable(ctx); err != nil {
		t.Fatalf("database was not writable after pressure cleanup: %v", err)
	}
	t.Logf("attempt_rows=%d context_bytes=%d passes=%d foreground_writes=%d cleanup_max=%s foreground_max=%s",
		attemptRows, expiredContexts*(1<<20), passes+1, foregroundWrites.Load(), maximumCleanup,
		time.Duration(foregroundMaxNanos.Load()))
}
