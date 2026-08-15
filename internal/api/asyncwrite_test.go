package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestFlushWritesCancelsAndJoinsRuntimeTasks(t *testing.T) {
	h := newHarness(t, nil)
	started := make(chan struct{})
	stopped := make(chan struct{})
	if !h.app.launchRuntimeTask("test-runtime-task", time.Minute, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(stopped)
	}) {
		t.Fatal("runtime task was rejected before shutdown")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime task did not start")
	}
	h.app.FlushWrites()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("FlushWrites returned without joining the cancelled runtime task")
	}
	if h.app.launchRuntimeTask("late-runtime-task", time.Second, func(context.Context) {}) {
		t.Fatal("runtime task was admitted after shutdown")
	}
}

func TestUsageJournalReplaysCrashAndRemainsIdempotent(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "journal-replay.sqlite3")
	store, err := storage.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
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
		{UpstreamAttempt: &storage.CodexUpstreamAttempt{EventID: "attempt-crash", TreeID: "tree-crash", AccountID: "account", EgressID: storage.DefaultDirectEgressID, State: "terminal_success", StatusCode: 200, ExpiresAt: storage.Now() + 60}},
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
		var usageRows, eventRows, attemptRows int
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
		if err = store.DB().QueryRow(`SELECT COUNT(*) FROM codex_upstream_attempt WHERE tree_id='tree-crash' AND state='terminal_success'`).Scan(&attemptRows); err != nil {
			t.Fatal(err)
		}
		if usageRows != 1 || eventRows != 1 || attemptRows != 1 || status != "settled" {
			t.Fatalf("iteration=%d usage=%d events=%d attempts=%d status=%q", iteration, usageRows, eventRows, attemptRows, status)
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

func TestUsageJournalReplayHonorsShutdownCancellation(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "cancelled-replay.sqlite3"))
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
	if _, err = journal.Append(usagejournal.Record{Usage: &storage.UsageRecordWrite{
		Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`),
		Diagnostics: storage.UsageDiagnostics{UsageEventID: "cancelled-replay-event"},
	}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, usageJournal: journal}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.replayUsageJournal(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled replay err=%v, want context.Canceled", err)
	}
	snapshot, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Pending != 1 {
		t.Fatalf("cancelled replay acknowledged durable record: %+v", snapshot)
	}
}

func TestNonContiguousTelemetryBatchDefersGapReplayToDedicatedWorker(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "gap-replay.sqlite3"))
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
	records := make([]storage.UsageRecordWrite, 3)
	sequences := make([]uint64, len(records))
	for index := range records {
		records[index] = storage.UsageRecordWrite{
			Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`),
			Diagnostics: storage.UsageDiagnostics{UsageEventID: fmt.Sprintf("gap-event-%d", index)},
		}
		sequences[index], err = journal.Append(usagejournal.Record{Usage: &records[index]})
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: store, usageJournal: journal, usageJournalWake: make(chan struct{}, 1)}
	if err = s.persistTelemetryBatchContext(context.Background(), []telemetryWrite{{
		usage: &records[2], journalSeq: sequences[2],
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AckedSequence != 0 || snapshot.Pending != 3 {
		t.Fatalf("gap batch advanced FIFO cursor or synchronously replayed: %+v", snapshot)
	}
	select {
	case <-s.usageJournalWake:
	default:
		t.Fatal("gap batch did not wake the dedicated journal replayer")
	}
	var materialized int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&materialized); err != nil {
		t.Fatal(err)
	}
	if materialized != 1 {
		t.Fatalf("gap batch materialized rows=%d, want only its idempotent row", materialized)
	}
	if err = s.replayUsageJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&materialized); err != nil {
		t.Fatal(err)
	}
	if materialized != 3 {
		t.Fatalf("dedicated replay materialized rows=%d, want 3", materialized)
	}
}

func TestTelemetryBarrierReplaysDeferredJournalGap(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "barrier-gap.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	journal, err := usagejournal.Open(filepath.Join(t.TempDir(), "journal"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store}
	s.startAsyncWriter()
	// Install the journal after the writer starts so this test has no dedicated
	// replayer: the flush barrier itself is solely responsible for filling seq=1.
	s.usageJournal = journal
	s.usageJournalWake = make(chan struct{}, 1)
	t.Cleanup(s.FlushWrites)
	first := storage.UsageRecordWrite{
		Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`),
		Diagnostics: storage.UsageDiagnostics{UsageEventID: "barrier-gap-first"},
	}
	if _, err = journal.Append(usagejournal.Record{Usage: &first}); err != nil {
		t.Fatal(err)
	}
	second := storage.UsageRecordWrite{
		Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`),
		Diagnostics: storage.UsageDiagnostics{UsageEventID: "barrier-gap-second"},
	}
	s.enqueueUsage(second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if timedOut, flushErr := s.flushTelemetry(ctx); timedOut || flushErr != nil {
		t.Fatalf("flush timed_out=%v err=%v", timedOut, flushErr)
	}
	var materialized int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records WHERE usage_event_id LIKE 'barrier-gap-%'`).Scan(&materialized); err != nil {
		t.Fatal(err)
	}
	if materialized != 2 {
		t.Fatalf("barrier returned before deferred FIFO gap replay: rows=%d", materialized)
	}
}

func TestTelemetryReplayDeferralIsPressureBounded(t *testing.T) {
	base := usagejournal.Snapshot{Pending: 1, Bytes: 1}
	if shouldDeferTelemetryReplay(telemetryReplayActiveThreshold, false, base) {
		t.Fatal("threshold request load unexpectedly deferred replay")
	}
	if !shouldDeferTelemetryReplay(telemetryReplayActiveThreshold+1, false, base) {
		t.Fatal("high request load did not defer a bounded durable backlog")
	}
	if !shouldDeferTelemetryReplay(0, true, base) {
		t.Fatal("recent journal activity did not protect live SQLite traffic from replay")
	}
	if shouldDeferTelemetryReplay(telemetryReplayActiveThreshold+1, true, usagejournal.Snapshot{Pending: 0, Bytes: 1}) {
		t.Fatal("empty journal unexpectedly reported replay deferral")
	}
	if shouldDeferTelemetryReplay(telemetryReplayActiveThreshold+1, true, usagejournal.Snapshot{Pending: 1, Bytes: telemetryReplayDeferMaxBytes}) {
		t.Fatal("byte ceiling did not force replay")
	}
	if shouldDeferTelemetryReplay(telemetryReplayActiveThreshold+1, true, usagejournal.Snapshot{Pending: telemetryReplayDeferMaxRecords, Bytes: 1}) {
		t.Fatal("record ceiling did not force replay")
	}
}

func TestLiveUsageJournalReplayCommitsOnlyOneBoundedBatch(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "live-replay.sqlite3"))
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
	for index := 0; index < usageJournalOnlineReplayBatchMax+1; index++ {
		usage := storage.UsageRecordWrite{
			Model: "test", Total: 1, Raw: json.RawMessage(`{"total_tokens":1}`),
			Diagnostics: storage.UsageDiagnostics{UsageEventID: fmt.Sprintf("live-replay-%d", index)},
		}
		if _, err = journal.Append(usagejournal.Record{Usage: &usage}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: store, usageJournal: journal}
	if err = s.replayUsageJournalOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := journal.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AckedSequence != usageJournalOnlineReplayBatchMax || snapshot.Pending != 1 {
		t.Fatalf("live replay was not one bounded batch: %+v", snapshot)
	}
	if err = s.replayUsageJournal(context.Background()); err != nil {
		t.Fatal(err)
	}
	var materialized int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM usage_records`).Scan(&materialized); err != nil {
		t.Fatal(err)
	}
	if materialized != usageJournalOnlineReplayBatchMax+1 {
		t.Fatalf("materialized rows=%d, want %d", materialized, usageJournalOnlineReplayBatchMax+1)
	}
}

func TestTelemetryPressureSplitDefersOnlyJournalCoveredEffects(t *testing.T) {
	audit := &storage.AuditLogRow{Action: "not_journaled"}
	batch := []telemetryWrite{
		{journalSeq: 1, usage: &storage.UsageRecordWrite{Model: "journaled"}},
		{journalSeq: 2, upstreamAttempt: &storage.CodexUpstreamAttempt{EventID: "journaled"}},
		{apiKeyHash: "key"},
		{journalSeq: 3, audit: audit, hold: &storage.BillingHoldWrite{ID: "composite"}},
	}
	immediate, deferred := splitDeferredJournalWrites(batch)
	if deferred != 2 {
		t.Fatalf("deferred=%d, want 2", deferred)
	}
	if len(immediate) != 2 || immediate[0].apiKeyHash != "key" || immediate[1].audit != audit {
		t.Fatalf("immediate writes=%+v", immediate)
	}
}

func TestBillingHoldBackstopDoesNotDuplicateExplicitSettlement(t *testing.T) {
	s := &Server{usageWrites: make(chan telemetryWrite, 4)}
	s.billingEstimates.Store("hold-explicit", int64(100))
	if err := s.settleBillingHold(context.Background(), "hold-explicit", "settled"); err != nil {
		t.Fatal(err)
	}
	if err := s.settleBillingHoldIfHeld(context.Background(), "hold-explicit", "abandoned"); err != nil {
		t.Fatal(err)
	}
	if got := len(s.usageWrites); got != 1 {
		t.Fatalf("explicit settlement queued %d records, want 1", got)
	}
	write := <-s.usageWrites
	s.usagePending.Done()
	if write.hold == nil || write.hold.Status != "settled" || write.hold.IfHeld {
		t.Fatalf("explicit settlement write=%+v", write.hold)
	}

	s.billingEstimates.Store("hold-backstop", int64(100))
	if err := s.settleBillingHoldIfHeld(context.Background(), "hold-backstop", "abandoned"); err != nil {
		t.Fatal(err)
	}
	if err := s.settleBillingHoldIfHeld(context.Background(), "hold-backstop", "abandoned"); err != nil {
		t.Fatal(err)
	}
	if got := len(s.usageWrites); got != 1 {
		t.Fatalf("backstop settlement queued %d records, want 1", got)
	}
	write = <-s.usageWrites
	s.usagePending.Done()
	if write.hold == nil || write.hold.Status != "abandoned" || !write.hold.IfHeld {
		t.Fatalf("backstop settlement write=%+v", write.hold)
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
