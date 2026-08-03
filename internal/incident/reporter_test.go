package incident

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

func newIncidentTestStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestPrimaryFailureFallsBackAndReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "exception-events")
	first, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := supervisor.RegisterEventCallback(first.CallbackOptions("incident-restart-test"))
	if err != nil {
		t.Fatal(err)
	}

	const rawSecret = "Bearer raw-secret-token@example.test"
	supervisor.Report(supervisor.Event{
		Type: "panic", Severity: "error", Module: "api.fixture", Operation: "trigger",
		ErrorClass: "fixtureError", RequestID: "REQ-0123456789ABCDEF",
		Route: "fixture.panic", Status: 503, Recovered: true,
		Message: rawSecret, Panic: rawSecret,
	})
	registration.Unregister()
	if snapshot := first.Snapshot(); snapshot.Pending != 1 || snapshot.FallbackWrites != 1 || snapshot.PrimaryConfigured {
		t.Fatalf("fallback snapshot: %#v", snapshot)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var journalPath string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "event-") {
			journalPath = filepath.Join(dir, entry.Name())
		}
	}
	if journalPath == "" {
		t.Fatal("fallback journal record was not created")
	}
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), rawSecret) || strings.Contains(string(raw), "raw-secret") {
		t.Fatalf("raw error leaked into fallback journal: %s", raw)
	}
	if info, err := os.Stat(journalPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fallback mode info=%v err=%v", info, err)
	}

	store := newIncidentTestStore(t)
	second, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := second.Replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != 1 || second.Snapshot().Pending != 0 {
		t.Fatalf("replay count=%d snapshot=%#v", replayed, second.Snapshot())
	}
	var eventType, entityType, entityAlias, detail string
	var gap int
	if err = store.DB().QueryRowContext(ctx, `
SELECT event_type,entity_type,entity_alias,detail_json,diagnostic_gap
FROM diagnostic_events WHERE event_type='panic'`).Scan(&eventType, &entityType, &entityAlias, &detail, &gap); err != nil {
		t.Fatal(err)
	}
	if eventType != "panic" || entityType != "request" || entityAlias != "REQ-0123456789ABCDEF" || gap != 0 {
		t.Fatalf("persisted event=%q/%q/%q gap=%d", eventType, entityType, entityAlias, gap)
	}
	for _, expected := range []string{`"component":"api.fixture"`, `"operation":"trigger"`, `"delivery":"fallback_replayed"`, `"route":"fixture.panic"`} {
		if !strings.Contains(detail, expected) {
			t.Fatalf("persisted detail missing %q: %s", expected, detail)
		}
	}
	if strings.Contains(detail, rawSecret) || strings.Contains(detail, "raw-secret") {
		t.Fatalf("raw error leaked into diagnostic event: %s", detail)
	}
}

func TestPrimaryCallbackPersistsWithoutJournal(t *testing.T) {
	store := newIncidentTestStore(t)
	reporter, err := Open(filepath.Join(t.TempDir(), "exception-events"), store)
	if err != nil {
		t.Fatal(err)
	}
	event := supervisor.Event{
		ID: "SEVT-0123456789ABCDEF0123456789ABCDEF", TimeUnix: time.Now().Unix(),
		Type: "http_error", Severity: "error", Module: "http-request",
		Operation: "serve_http", ErrorClass: "http_status_5xx",
		Fingerprint: "sha256:0123456789abcdef0123456789abcdef",
		RequestID:   "REQ-FEDCBA9876543210", Route: "admin.fixture", Status: 500,
	}
	if err = reporter.PrimaryCallback(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if snapshot := reporter.Snapshot(); snapshot.Pending != 0 || snapshot.FallbackWrites != 0 {
		t.Fatalf("primary unexpectedly used journal: %#v", snapshot)
	}
	var count int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM diagnostic_events WHERE id=?`, event.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted count=%d err=%v", count, err)
	}
}

func TestCorruptJournalProducesPersistentDiagnosticGap(t *testing.T) {
	store := newIncidentTestStore(t)
	dir := filepath.Join(t.TempDir(), "exception-events")
	reporter, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "event-SEVT-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA.json")
	if err = os.WriteFile(path, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = reporter.Replay(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = reporter.Replay(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = store.DB().QueryRow(`SELECT COUNT(*) FROM diagnostic_events WHERE event_type='exception_journal_gap' AND diagnostic_gap=1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("gap count=%d err=%v", count, err)
	}
	if reporter.Snapshot().CorruptRecords != 1 {
		t.Fatalf("corrupt snapshot: %#v", reporter.Snapshot())
	}
}

func TestConcurrentFallbackReplayIsLossless(t *testing.T) {
	const eventCount = 64
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "exception-events")
	reporter, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	registration, err := supervisor.RegisterEventCallback(reporter.CallbackOptions("incident-concurrent-test"))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < eventCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer supervisor.RecoverEvent(supervisor.Event{Module: "incident-test", Operation: "concurrent_report"})
			supervisor.Report(supervisor.Event{
				Type: "error", Module: "fixture.concurrent", Operation: "write",
				ErrorClass: "fixture_error", Message: fmt.Sprintf("raw-secret-%d", i),
			})
		}()
	}
	wg.Wait()
	registration.Unregister()
	if snapshot := reporter.Snapshot(); snapshot.Pending != eventCount || snapshot.FallbackWrites != eventCount {
		t.Fatalf("concurrent fallback snapshot: %#v", snapshot)
	}

	store := newIncidentTestStore(t)
	restarted, err := Open(dir, store)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.Replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != eventCount || restarted.Snapshot().Pending != 0 {
		t.Fatalf("replayed=%d snapshot=%#v", replayed, restarted.Snapshot())
	}
	var count int
	if err = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM diagnostic_events WHERE event_type='error'`).Scan(&count); err != nil || count != eventCount {
		t.Fatalf("persisted concurrent events=%d err=%v", count, err)
	}
}
