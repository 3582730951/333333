package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/usagejournal"
)

func TestTelemetryBarrierCommitsPriorWrites(t *testing.T) {
	h := newHarness(t, nil)
	h.app.enqueueAudit(storage.AuditLogRow{Action: "barrier_before", Reason: "test", CreatedAt: storage.Now()})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if timedOut, err := h.app.flushTelemetry(ctx); err != nil {
		t.Fatal(err)
	} else if timedOut {
		t.Fatal("telemetry barrier timed out")
	}
	rows, err := h.store.ListAuditLog(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].Action != "barrier_before" {
		t.Fatalf("barrier did not commit prior audit: rows=%+v err=%v", rows, err)
	}
}

func TestAsyncWriterRecoversPanicAndContinues(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	defer s.FlushWrites()

	done := make(chan struct{})
	s.enqueueWrite(func() {
		panic("bad async write")
	})
	s.enqueueWrite(func() {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("async writer did not continue after a panicking write")
	}
}

func TestAsyncWriterRecoversInlineAfterFlush(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	s.FlushWrites()

	mustNotPanic(t, func() {
		s.enqueueWrite(func() {
			panic("inline after flush")
		})
	})
}

func TestAsyncWriterRecoversInlineBudgetOverflow(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	defer s.FlushWrites()

	mustNotPanic(t, func() {
		s.enqueueWriteSized(asyncWriteBudgetBytes+1, func() {
			panic("budget overflow")
		})
	})

	if got := atomic.LoadInt64(&s.asyncBytes); got != 0 {
		t.Fatalf("async byte budget leaked after overflow fallback: got %d", got)
	}
}

func TestAsyncWriterRecoversInlineQueueFull(t *testing.T) {
	s := &Server{
		asyncWrites: make(chan func(), 1),
	}
	s.asyncWrites <- func() {}
	defer close(s.asyncWrites)

	mustNotPanic(t, func() {
		s.enqueueWrite(func() {
			panic("inline queue full")
		})
	})
}

func TestUsageJournalReplaysCrashAndRemainsIdempotent(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "journal-replay.sqlite3")
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	journalDir, err := usageJournalDirectory(cfg, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := usagejournal.Open(journalDir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	holdID, eventID := "hold-crash", "usage-hold-crash"
	records := []usagejournal.Record{
		{Hold: &storage.BillingHoldWrite{ID: holdID, EventID: eventID, RouteKeyHash: "route", AccountID: "account", EstimatedTokens: 3, RouteEpoch: 7, Create: true}},
		{Usage: &storage.UsageRecordWrite{AccountID: "account", RouteKeyHash: "route", Model: "gpt-5.6-sol", Prompt: 2, Completion: 1, Total: 3, Raw: json.RawMessage(`{"total_tokens":3}`), Diagnostics: storage.UsageDiagnostics{UsageEventID: eventID, BillingHoldID: holdID, RouteEpoch: 7}}},
		{Hold: &storage.BillingHoldWrite{ID: holdID, Status: "settled"}},
	}
	for _, record := range records {
		if _, err = journal.Append(record); err != nil {
			t.Fatal(err)
		}
	}
	if err = journal.Close(); err != nil {
		t.Fatal(err)
	}

	for iteration := 0; iteration < 2; iteration++ {
		app := NewServer(Dependencies{Config: cfg, Store: store})
		var usageRows, eventRows int
		var status string
		if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records WHERE usage_event_id=?`, eventID).Scan(&usageRows); err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_events WHERE event_id=?`, eventID).Scan(&eventRows); err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRow(`SELECT status FROM billing_holds WHERE id=?`, holdID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if usageRows != 1 || eventRows != 1 || status != "settled" {
			t.Fatalf("iteration=%d usage=%d events=%d status=%q", iteration, usageRows, eventRows, status)
		}
		metrics := app.usageJournalMetrics()
		if metrics["pending_records"] != uint64(0) {
			t.Fatalf("iteration=%d journal metrics=%+v", iteration, metrics)
		}
		app.FlushWrites()
	}
}

func TestDeferredRuntimeStartLeavesStandbyJournalAndWritersStopped(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "standby.sqlite3")
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(t.TempDir(), "standby-journal")
	cfg := config.Default()
	cfg.UsageJournalEnabled = true
	cfg.UsageJournalDir = journalDir
	app := NewServer(Dependencies{Config: cfg, Store: store, DeferRuntimeStart: true})
	if app.usageJournal != nil || app.asyncWrites != nil || app.usageWrites != nil || app.goalCompactionQueue != nil {
		t.Fatal("standby construction started a journal or background writer")
	}
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Fatalf("standby journal path exists before activation: %v", err)
	}
	if err := app.StartRuntime(); err != nil {
		t.Fatal(err)
	}
	defer app.FlushWrites()
	if app.usageJournal == nil || app.asyncWrites == nil || app.usageWrites == nil || app.goalCompactionQueue == nil {
		t.Fatal("active runtime did not start all durable writers")
	}
	if info, err := os.Stat(journalDir); err != nil || !info.IsDir() {
		t.Fatalf("active journal path = info:%v err:%v", info, err)
	}
}

func TestUsageJournalConcurrentEnqueueAndShutdownDoesNotLoseEvents(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "journal-shutdown.sqlite3")
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := usagejournal.Open(filepath.Join(t.TempDir(), "journal"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, usageJournal: journal, usageJournalStop: make(chan struct{})}
	s.startAsyncWriter()
	const writes = 64
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(writes)
	for i := 0; i < writes; i++ {
		i := i
		go func() {
			defer workers.Done()
			<-start
			eventID := "shutdown-event-" + string(rune('A'+i))
			s.enqueueUsage(storage.UsageRecordWrite{Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`), Diagnostics: storage.UsageDiagnostics{UsageEventID: eventID}})
		}()
	}
	close(start)
	s.FlushWrites()
	workers.Wait()
	var rows int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records WHERE usage_event_id LIKE 'shutdown-event-%'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != writes {
		t.Fatalf("usage rows=%d want=%d", rows, writes)
	}
}

func TestUsageJournalQueueOverflowRemainsDurableWithoutInlineWrite(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "overflow.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := usagejournal.Open(filepath.Join(t.TempDir(), "journal"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	s := &Server{store: store, usageJournal: journal, usageWrites: make(chan telemetryWrite, 1), usageJournalWake: make(chan struct{}, 1)}
	s.usageWrites <- telemetryWrite{audit: &storage.AuditLogRow{Action: "occupy_queue"}}
	done := make(chan struct{})
	go func() {
		s.enqueueUsage(storage.UsageRecordWrite{Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`), Diagnostics: storage.UsageDiagnostics{UsageEventID: "overflow-event"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("journaled overflow blocked on the full in-memory queue")
	}
	var rows int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records WHERE usage_event_id='overflow-event'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("overflow unexpectedly wrote inline: rows=%d", rows)
	}
	if err = s.replayUsageJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records WHERE usage_event_id='overflow-event'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("durable overflow replay rows=%d", rows)
	}
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("unexpected panic: %v", recovered)
		}
	}()
	fn()
}
