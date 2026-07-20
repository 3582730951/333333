package storage

import (
	"context"
	"encoding/json"
	"testing"
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
