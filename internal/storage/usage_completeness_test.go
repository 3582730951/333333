package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestKiroCreditsAndCacheReportingAggregation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	diag := UsageDiagnostics{UsageProvider: "kiro", UsageSource: "estimated", Estimated: true, KiroCredits: 1.25, KiroCreditsPresent: true}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "kiro-a", "route", "", "", "claude-opus-4.8", 100, 5, 105, 0, 0, 0, json.RawMessage(`{"estimated":true,"kiro_credits":1.25}`), diag); err != nil {
		t.Fatal(err)
	}
	report, err := store.CacheUsageMetricsWindow(ctx, 0, usageWindowOpenEndedUntil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ByProvider) != 1 {
		t.Fatalf("by_provider=%+v", report.ByProvider)
	}
	row := report.ByProvider[0]
	if row.Provider != "kiro" || row.KiroCredits != 1.25 || row.KiroCreditsReportedRequests != 1 || row.CacheReportingState != "unreported" {
		t.Fatalf("kiro aggregate=%+v", row)
	}
}

func TestUsageCompletenessUsesEarliestPendingHold(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	snapshot := Now()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_holds(id, account_id, status, usage_expected, created_at, updated_at) VALUES('pending','a','held',1,?,?)`, snapshot-30, snapshot-30); err != nil {
		t.Fatal(err)
	}
	meta, err := store.UsageCompleteness(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PendingUsageRequests != 1 || meta.UsageCompleteThroughAt != snapshot-30 || !meta.PartialData {
		t.Fatalf("meta=%+v", meta)
	}
}

func TestUsageCompletenessIsReadOnlyAndBackgroundReconcileIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	snapshot := Now()
	createdAt := snapshot - 3*60*60
	if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_holds(id, account_id, status, usage_expected, created_at, updated_at) VALUES('stale','a','settled',1,?,?)`, createdAt, createdAt); err != nil {
		t.Fatal(err)
	}

	meta, err := store.UsageCompleteness(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PendingUsageRequests != 0 || meta.CompletenessGapCount != 1 || !meta.PartialData {
		t.Fatalf("read-only completeness=%+v", meta)
	}
	var status string
	var auditRows int
	if err = store.db.QueryRowContext(ctx, `SELECT status FROM billing_holds WHERE id='stale'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='usage_missing'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if status != "settled" || auditRows != 0 {
		t.Fatalf("UsageCompleteness mutated storage: status=%q audit_rows=%d", status, auditRows)
	}

	changed, err := store.ReconcileUsageMissing(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("first reconcile changed=%d, want 1", changed)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT status FROM billing_holds WHERE id='stale'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='usage_missing'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if status != "usage_missing" || auditRows != 1 {
		t.Fatalf("reconciled status=%q audit_rows=%d", status, auditRows)
	}
	changed, err = store.ReconcileUsageMissing(ctx, snapshot)
	if err != nil || changed != 0 {
		t.Fatalf("second reconcile changed=%d err=%v", changed, err)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='usage_missing'`).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 1 {
		t.Fatalf("idempotent reconcile audit_rows=%d", auditRows)
	}
}

func TestUsageCompletenessRemainsAvailableDuringConcurrentWrite(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	snapshot := Now()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO billing_holds(id, account_id, status, usage_expected, created_at, updated_at) VALUES('locked','a','held',1,?,?)`, snapshot-30, snapshot-30); err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE billing_holds SET updated_at=? WHERE id='locked'`, snapshot); err != nil {
		t.Fatal(err)
	}

	readCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	meta, err := store.UsageCompleteness(readCtx, snapshot)
	if err != nil {
		t.Fatalf("read-only completeness blocked behind writer: %v", err)
	}
	if meta.PendingUsageRequests != 1 {
		t.Fatalf("meta=%+v", meta)
	}
}
