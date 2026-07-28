package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestBatchTelemetryOrdersHoldUsageAndSettlement(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := Now()
	holdID, eventID := "hold_ordered", "event_ordered"
	writes := []UsageRecordWrite{{AccountID: "account-a", RouteKeyHash: "route-a", Model: "gpt-5.6", Prompt: 10, Completion: 2, Total: 12,
		Raw: json.RawMessage(`{"total_tokens":12}`), Diagnostics: UsageDiagnostics{UsageEventID: eventID, BillingHoldID: holdID, RouteEpoch: 7}}}
	holds := []BillingHoldWrite{
		{ID: holdID, EventID: eventID, AccountID: "account-a", RouteKeyHash: "route-a", EstimatedTokens: 20, RouteEpoch: 7, CreatedAt: now, Create: true},
		{ID: holdID, Status: "settled_streaming", CreatedAt: now + 1},
	}
	if err := store.BatchWriteTelemetry(ctx, writes, nil, holds, nil); err != nil {
		t.Fatal(err)
	}
	var status, usageState, terminal string
	var expected, recordedAt, rows, routeEpoch int64
	if err := store.db.QueryRowContext(ctx, `SELECT status,usage_expected,usage_recorded_at FROM billing_holds WHERE id=?`, holdID).Scan(&status, &expected, &recordedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT usage_state,terminal_status,route_epoch FROM usage_events WHERE event_id=?`, eventID).Scan(&usageState, &terminal, &routeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_records WHERE usage_event_id=? AND billing_hold_id=?`, eventID, holdID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if status != "settled_streaming" || expected != 1 || recordedAt == 0 || usageState != "real" || terminal != status || routeEpoch != 7 || rows != 1 {
		t.Fatalf("hold=%s expected=%d recorded=%d event=%s/%s epoch=%d rows=%d", status, expected, recordedAt, usageState, terminal, routeEpoch, rows)
	}
}

func TestUsageEventEstimateIsReplacedByLateRealUsage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := Now()
	holdID, eventID := "hold_late", "event_late"
	holds := []BillingHoldWrite{
		{ID: holdID, EventID: eventID, AccountID: "account-late", RouteKeyHash: "route-late", EstimatedTokens: 123, RouteEpoch: 9, CreatedAt: now, Create: true},
		{ID: holdID, Status: "settled_streaming", CreatedAt: now + 1},
	}
	if err := store.BatchWriteTelemetry(ctx, nil, nil, holds, nil); err != nil {
		t.Fatal(err)
	}
	assertUsageEventRow(t, store, eventID, 123, 1, "estimated", 9)
	real := UsageRecordWrite{AccountID: "account-late", RouteKeyHash: "route-late", Model: "gpt-5.6", Prompt: 100, Completion: 5, Total: 105, Cached: 80, CacheRead: 80,
		Raw: json.RawMessage(`{"input_tokens":100,"output_tokens":5,"total_tokens":105}`), Diagnostics: UsageDiagnostics{UsageEventID: eventID, BillingHoldID: holdID, RouteEpoch: 9, UsageProvider: "codex", UsageSource: "upstream", PromptCacheKeyPresent: true}}
	if err := store.BatchInsertUsageRecords(ctx, []UsageRecordWrite{real}); err != nil {
		t.Fatal(err)
	}
	assertUsageEventRow(t, store, eventID, 105, 0, "real", 9)
	var rows, promptCacheKey int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*),MAX(prompt_cache_key_present) FROM usage_records WHERE usage_event_id=?`, eventID).Scan(&rows, &promptCacheKey); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || promptCacheKey != 1 {
		t.Fatalf("late real usage rows=%d prompt_cache_key=%d", rows, promptCacheKey)
	}
}

func TestUsageEventConcurrentDuplicatesAreChargedOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const workers = 32
	write := UsageRecordWrite{AccountID: "account-duplicate", Model: "gpt", Prompt: 90, Completion: 10, Total: 100,
		Raw: json.RawMessage(`{"total_tokens":100}`), Diagnostics: UsageDiagnostics{UsageEventID: "event-duplicate", BillingHoldID: "hold-duplicate", UsageSource: "upstream", RouteEpoch: 11}}
	if err := store.BatchWriteTelemetry(ctx, nil, nil, []BillingHoldWrite{{ID: "hold-duplicate", EventID: "event-duplicate", AccountID: "account-duplicate", EstimatedTokens: 120, RouteEpoch: 11, Create: true}}, nil); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- store.BatchInsertUsageRecords(ctx, []UsageRecordWrite{write})
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var rows, total int64
	if err := store.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(total_tokens),0) FROM usage_records WHERE usage_event_id='event-duplicate'`).Scan(&rows, &total); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || total != 100 {
		t.Fatalf("duplicate event rows=%d total=%d", rows, total)
	}
}

func assertUsageEventRow(t *testing.T, store *Store, eventID string, wantTotal int64, wantEstimated int, wantState string, wantEpoch int64) {
	t.Helper()
	var rows, estimated int
	var total, routeEpoch int64
	var state string
	if err := store.db.QueryRow(`SELECT COUNT(*),MAX(total_tokens),MAX(estimated),MAX(route_epoch) FROM usage_records WHERE usage_event_id=?`, eventID).Scan(&rows, &total, &estimated, &routeEpoch); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT usage_state FROM usage_events WHERE event_id=?`, eventID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || total != wantTotal || estimated != wantEstimated || state != wantState || routeEpoch != wantEpoch {
		t.Fatalf("event=%s rows=%d total=%d estimated=%d state=%s epoch=%d", eventID, rows, total, estimated, state, routeEpoch)
	}
}

func TestBillingFailureSettlementClearsUsageExpectation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	holds := []BillingHoldWrite{
		{ID: "hold_failed", EventID: "event_failed", AccountID: "account-failed", EstimatedTokens: 50, Create: true},
		{ID: "hold_failed", Status: "stream_probe_failed"},
	}
	if err := store.BatchWriteTelemetry(ctx, nil, nil, holds, nil); err != nil {
		t.Fatal(err)
	}
	var expected, usageRows int
	if err := store.db.QueryRow(`SELECT usage_expected FROM billing_holds WHERE id='hold_failed'`).Scan(&expected); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE billing_hold_id='hold_failed'`).Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if expected != 0 || usageRows != 0 {
		t.Fatalf("failed hold expected=%d usage_rows=%d", expected, usageRows)
	}
}

func TestRepairLegacyUsageEventsMatchesDiagnosticStateMachine(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := Now() - 100
	statuses := map[string]int{"mapped_session_risk_rotating": 16, "stream_interrupted_compensated": 16, "stream_mapped_session_risk_rotating": 2, "stream_probe_failed": 12}
	index := 0
	for status, count := range statuses {
		for range count {
			index++
			id := fmt.Sprintf("legacy_false_%02d", index)
			if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_holds(id,account_id,estimated_tokens,status,usage_expected,created_at,updated_at) VALUES(?,?,100,?,1,?,?)`, id, "account", status, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("legacy_success_%d", i)
		if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_holds(id,account_id,estimated_tokens,status,usage_expected,created_at,updated_at) VALUES(?,?,?, 'settled_streaming',1,?,?)`, id, "account", 200+i, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.repairLegacyUsageEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var falsePending, recovered, duplicateRows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM billing_holds WHERE id LIKE 'legacy_false_%' AND usage_expected<>0`).Scan(&falsePending); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE billing_hold_id LIKE 'legacy_success_%' AND estimated=1 AND raw_usage_json LIKE '%recovered_from_hold%'`).Scan(&recovered); err != nil {
		t.Fatal(err)
	}
	if err := store.repairLegacyUsageEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE billing_hold_id LIKE 'legacy_success_%'`).Scan(&duplicateRows); err != nil {
		t.Fatal(err)
	}
	if falsePending != 0 || recovered != 4 || duplicateRows != 4 {
		t.Fatalf("false_pending=%d recovered=%d rows_after_replay=%d", falsePending, recovered, duplicateRows)
	}
}
