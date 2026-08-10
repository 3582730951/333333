package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkUsageHourlyRollup compares the dashboard's persisted-hour path with
// the legacy raw-record aggregation on the same synthetic 50K-row history.
func BenchmarkUsageHourlyRollup(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "benchmark.sqlite3"))
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	// The synthetic history intentionally predates Store.Init. Disable the
	// production migration cutover so the benchmark window is deterministic at
	// every minute within the current hour.
	if err := store.SetSetting(ctx, "usage_accuracy_cutover_at", "0"); err != nil {
		b.Fatal(err)
	}
	base := (Now() / 3600) * 3600
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO usage_records(account_id,model,usage_provider,usage_source,prompt_tokens,completion_tokens,total_tokens,created_at) VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	for index := 0; index < 50_000; index++ {
		prompt := int64(100 + index%500)
		completion := int64(20 + index%80)
		created := base - int64(index%720)*3600 + int64(index%3000)
		if _, err := statement.ExecContext(ctx, fmt.Sprintf("account-%03d", index%100), fmt.Sprintf("model-%d", index%5), "codex", "upstream", prompt, completion, prompt+completion, created); err != nil {
			b.Fatal(err)
		}
	}
	_ = statement.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	since, until := base-720*3600, base+3600

	b.Run("hourly_rollup", func(b *testing.B) {
		for index := 0; index < b.N; index++ {
			rows, err := store.UsageTimeseriesWindow(ctx, since, until, 3600)
			if err != nil || len(rows) == 0 {
				b.Fatalf("rows=%d err=%v", len(rows), err)
			}
		}
	})
	b.Run("raw_usage_records", func(b *testing.B) {
		for index := 0; index < b.N; index++ {
			rows, err := store.ReadDB().QueryContext(ctx, `SELECT (created_at/3600)*3600,COUNT(*),SUM(prompt_tokens),SUM(completion_tokens),SUM(total_tokens) FROM usage_records WHERE created_at>=? AND created_at<? AND estimated=0 GROUP BY (created_at/3600)*3600 ORDER BY 1`, since, until)
			if err != nil {
				b.Fatal(err)
			}
			count := 0
			for rows.Next() {
				var bucket, requests, prompt, completion, total int64
				if err := rows.Scan(&bucket, &requests, &prompt, &completion, &total); err != nil {
					b.Fatal(err)
				}
				count++
			}
			_ = rows.Close()
			if count == 0 {
				b.Fatal("no raw rows")
			}
		}
	})
}
