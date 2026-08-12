package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUsageHourlyRollupLeavesPostgresOnExactRawPath(t *testing.T) {
	store := &Store{driver: "postgres"}
	if err := store.initUsageHourlyRollups(context.Background()); err != nil {
		t.Fatalf("postgres rollup initialization should be a no-op: %v", err)
	}
}

func TestUsageHourlyRollupCombinesExactWindowEdgesAndRepairsUpdates(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const base int64 = 1_800_000_000 // exact UTC hour
	insert := func(created, prompt, total int64) int64 {
		result, err := store.DB().ExecContext(ctx, `INSERT INTO usage_records(account_id,model,usage_provider,prompt_tokens,total_tokens,cache_read_tokens,cache_total_input_tokens,created_at) VALUES('account-a','gpt-5.6','codex',?,?,2,?,?)`, prompt, total, prompt, created)
		if err != nil {
			t.Fatalf("insert usage at %d: %v", created, err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert(base-30, 5, 5)
	fullID := insert(base+10, 10, 10)
	insert(base+3600+10, 20, 20)
	insert(base+7200+30, 7, 7)

	rows, err := store.UsageTimeseriesWindow(ctx, base-60, base+7200+60, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("timeseries buckets=%d rows=%+v", len(rows), rows)
	}
	for index, want := range []int64{5, 10, 20, 7} {
		if rows[index].TotalTokens != want {
			t.Fatalf("bucket %d total=%d want=%d", index, rows[index].TotalTokens, want)
		}
	}

	if _, err := store.DB().ExecContext(ctx, `UPDATE usage_records SET prompt_tokens=15,total_tokens=15 WHERE id=?`, fullID); err != nil {
		t.Fatal(err)
	}
	rows, err = store.UsageTimeseriesWindow(ctx, base-60, base+7200+60, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if rows[1].TotalTokens != 15 {
		t.Fatalf("updated rollup total=%d want=15", rows[1].TotalTokens)
	}
	cacheRows, err := store.CacheUsageBucketsWindow(ctx, base-60, base+7200+60, 3600)
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, row := range cacheRows {
		requests += row.Requests
	}
	if requests != 4 {
		t.Fatalf("cache rollup requests=%d want=4 rows=%+v", requests, cacheRows)
	}
	summary, err := store.cacheUsageSummary(ctx, base-60, base+7200+60)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 4 || summary.PromptTokens != 47 || summary.CacheReadTokens != 8 {
		t.Fatalf("cache summary=%+v", summary)
	}
	stats, err := store.UsageHourlyRollupStats(ctx)
	if err != nil || stats.Rows != 4 {
		t.Fatalf("rollup stats=%+v err=%v", stats, err)
	}
}

func TestUsageHourlyRollupHistoricalBackfillWaitsForDeferredRunner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pool.sqlite3")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `
DROP TRIGGER usage_hourly_rollup_insert;
DROP TRIGGER usage_hourly_rollup_update;
DROP TRIGGER usage_hourly_rollup_delete;
DELETE FROM settings WHERE key='usage_hourly_rollup_v1';
DELETE FROM usage_hourly_rollups;
INSERT INTO usage_records(account_id,model,prompt_tokens,total_tokens,created_at)
VALUES('legacy-a','gpt-test',10,10,1800000010),('legacy-b','gpt-test',20,20,1800003610)`); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err := store.usageHourlyRollupReady(ctx)
	if err != nil || ready {
		t.Fatalf("rollup unexpectedly marked ready before deferred migration: ready=%t err=%v", ready, err)
	}
	stats, err := store.UsageHourlyRollupStats(ctx)
	if err != nil || stats.Rows != 0 {
		t.Fatalf("startup performed historical rollup: stats=%+v err=%v", stats, err)
	}
	rows, err := store.UsageTimeseriesWindow(ctx, 1_800_000_000, 1_800_007_200, 3600)
	if err != nil || len(rows) != 2 || rows[0].TotalTokens != 10 || rows[1].TotalTokens != 20 {
		t.Fatalf("raw fallback while migration is pending: rows=%+v err=%v", rows, err)
	}

	if err = store.backfillUsageHourlyRollups(ctx); err != nil {
		t.Fatal(err)
	}
	ready, err = store.usageHourlyRollupReady(ctx)
	if err != nil || !ready {
		t.Fatalf("deferred rollup did not commit marker: ready=%t err=%v", ready, err)
	}
	stats, err = store.UsageHourlyRollupStats(ctx)
	if err != nil || stats.Rows != 2 {
		t.Fatalf("deferred rollup stats=%+v err=%v", stats, err)
	}
}
