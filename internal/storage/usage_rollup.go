package storage

import (
	"context"
	"database/sql"
	"fmt"
)

const usageHourlyRollupVersion = "usage_hourly_rollup_v1"

const usageHourlyRollupSchema = `
CREATE TABLE IF NOT EXISTS usage_hourly_rollups(
  bucket INTEGER NOT NULL,
  account_id TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  usage_provider TEXT NOT NULL DEFAULT '',
  usage_source TEXT NOT NULL DEFAULT '',
  estimated INTEGER NOT NULL DEFAULT 0,
  cache_unreported INTEGER NOT NULL DEFAULT 0,
  requests INTEGER NOT NULL DEFAULT 0,
  real_requests INTEGER NOT NULL DEFAULT 0,
  hit_requests INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_5m_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_reported_requests INTEGER NOT NULL DEFAULT 0,
  kiro_credits REAL NOT NULL DEFAULT 0,
  kiro_credits_present INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(bucket, account_id, model, usage_provider, usage_source, estimated, cache_unreported)
);
CREATE INDEX IF NOT EXISTS idx_usage_hourly_rollups_bucket ON usage_hourly_rollups(bucket);
CREATE INDEX IF NOT EXISTS idx_usage_hourly_rollups_model_bucket ON usage_hourly_rollups(model, bucket);
CREATE INDEX IF NOT EXISTS idx_usage_hourly_rollups_account_bucket ON usage_hourly_rollups(account_id, bucket);
`

// usageRollupSelect aggregates an exact raw usage range into the durable hourly
// grain. It is shared by the one-time migration and the rare UPDATE/DELETE repair
// triggers; ordinary INSERT traffic uses one O(1) upsert below.
func usageRollupSelect(where string) string {
	return `SELECT (created_at / 3600) * 3600, account_id,
COALESCE(NULLIF(TRIM(model),''),'unknown'), COALESCE(usage_provider,''), COALESCE(usage_source,''), estimated,
CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 1 ELSE 0 END,
COUNT(*),
SUM(CASE WHEN estimated=0 AND NOT (usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0) THEN 1 ELSE 0 END),
SUM(CASE WHEN (CASE WHEN cache_read_tokens>0 THEN cache_read_tokens ELSE cached_tokens END)>0 THEN 1 ELSE 0 END),
SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens), SUM(cached_tokens),
SUM(CASE WHEN cache_read_tokens>0 THEN cache_read_tokens ELSE cached_tokens END),
SUM(cache_creation_tokens),
SUM(CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 0 WHEN cache_total_input_tokens>0 THEN cache_total_input_tokens ELSE prompt_tokens END),
SUM(CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 0 WHEN cache_miss_tokens>0 THEN cache_miss_tokens ELSE MAX(prompt_tokens-(CASE WHEN cache_read_tokens>0 THEN cache_read_tokens ELSE cached_tokens END),0) END),
SUM(cache_creation_5m_tokens), SUM(cache_creation_1h_tokens), SUM(cache_creation_present),
SUM(kiro_credits), SUM(kiro_credits_present)
FROM usage_records ` + where + `
GROUP BY (created_at / 3600) * 3600, account_id, COALESCE(NULLIF(TRIM(model),''),'unknown'), COALESCE(usage_provider,''), COALESCE(usage_source,''), estimated,
CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 1 ELSE 0 END`
}

func usageRollupInsertValues(prefix string) string {
	p := prefix + "."
	return `(((` + p + `created_at / 3600) * 3600), ` + p + `account_id,
COALESCE(NULLIF(TRIM(` + p + `model),''),'unknown'), COALESCE(` + p + `usage_provider,''), COALESCE(` + p + `usage_source,''), ` + p + `estimated,
CASE WHEN ` + p + `usage_provider='kiro' AND ` + p + `cache_read_present=0 AND ` + p + `cache_creation_present=0 THEN 1 ELSE 0 END,
1,
CASE WHEN ` + p + `estimated=0 AND NOT (` + p + `usage_provider='kiro' AND ` + p + `cache_read_present=0 AND ` + p + `cache_creation_present=0) THEN 1 ELSE 0 END,
CASE WHEN (CASE WHEN ` + p + `cache_read_tokens>0 THEN ` + p + `cache_read_tokens ELSE ` + p + `cached_tokens END)>0 THEN 1 ELSE 0 END,
` + p + `prompt_tokens, ` + p + `completion_tokens, ` + p + `total_tokens, ` + p + `cached_tokens,
CASE WHEN ` + p + `cache_read_tokens>0 THEN ` + p + `cache_read_tokens ELSE ` + p + `cached_tokens END,
` + p + `cache_creation_tokens,
CASE WHEN ` + p + `usage_provider='kiro' AND ` + p + `cache_read_present=0 AND ` + p + `cache_creation_present=0 THEN 0 WHEN ` + p + `cache_total_input_tokens>0 THEN ` + p + `cache_total_input_tokens ELSE ` + p + `prompt_tokens END,
CASE WHEN ` + p + `usage_provider='kiro' AND ` + p + `cache_read_present=0 AND ` + p + `cache_creation_present=0 THEN 0 WHEN ` + p + `cache_miss_tokens>0 THEN ` + p + `cache_miss_tokens ELSE MAX(` + p + `prompt_tokens-(CASE WHEN ` + p + `cache_read_tokens>0 THEN ` + p + `cache_read_tokens ELSE ` + p + `cached_tokens END),0) END,
` + p + `cache_creation_5m_tokens, ` + p + `cache_creation_1h_tokens, ` + p + `cache_creation_present,
` + p + `kiro_credits, ` + p + `kiro_credits_present)`
}

const usageRollupColumns = `(bucket,account_id,model,usage_provider,usage_source,estimated,cache_unreported,requests,real_requests,hit_requests,prompt_tokens,completion_tokens,total_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,cache_input_tokens,cache_miss_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,cache_creation_reported_requests,kiro_credits,kiro_credits_present)`

const usageRollupUpsert = ` ON CONFLICT(bucket,account_id,model,usage_provider,usage_source,estimated,cache_unreported) DO UPDATE SET
requests=requests+excluded.requests, real_requests=real_requests+excluded.real_requests,
hit_requests=hit_requests+excluded.hit_requests, prompt_tokens=prompt_tokens+excluded.prompt_tokens,
completion_tokens=completion_tokens+excluded.completion_tokens, total_tokens=total_tokens+excluded.total_tokens,
cached_tokens=cached_tokens+excluded.cached_tokens, cache_read_tokens=cache_read_tokens+excluded.cache_read_tokens,
cache_creation_tokens=cache_creation_tokens+excluded.cache_creation_tokens, cache_input_tokens=cache_input_tokens+excluded.cache_input_tokens,
cache_miss_tokens=cache_miss_tokens+excluded.cache_miss_tokens, cache_creation_5m_tokens=cache_creation_5m_tokens+excluded.cache_creation_5m_tokens,
cache_creation_1h_tokens=cache_creation_1h_tokens+excluded.cache_creation_1h_tokens,
cache_creation_reported_requests=cache_creation_reported_requests+excluded.cache_creation_reported_requests,
kiro_credits=kiro_credits+excluded.kiro_credits, kiro_credits_present=kiro_credits_present+excluded.kiro_credits_present`

func (s *Store) initUsageHourlyRollups(ctx context.Context) error {
	// The trigger implementation deliberately uses SQLite's lightweight local
	// aggregation primitives. PostgreSQL keeps using the exact raw-query path
	// until its own concurrent-refresh implementation is enabled.
	if s == nil || s.driver != "sqlite" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, usageHourlyRollupSchema); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, usageHourlyRollupVersion).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if version != "1" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM usage_hourly_rollups`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO usage_hourly_rollups `+usageRollupColumns+` `+usageRollupSelect("")); err != nil {
			return fmt.Errorf("backfill usage hourly rollup: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, usageHourlyRollupVersion, "1", Now()); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}

	insertTrigger := `CREATE TRIGGER IF NOT EXISTS usage_hourly_rollup_insert AFTER INSERT ON usage_records BEGIN
INSERT INTO usage_hourly_rollups ` + usageRollupColumns + ` VALUES ` + usageRollupInsertValues("NEW") + usageRollupUpsert + `; END;`
	updateTrigger := `CREATE TRIGGER IF NOT EXISTS usage_hourly_rollup_update AFTER UPDATE ON usage_records BEGIN
DELETE FROM usage_hourly_rollups WHERE bucket IN ((OLD.created_at/3600)*3600,(NEW.created_at/3600)*3600);
INSERT INTO usage_hourly_rollups ` + usageRollupColumns + ` ` + usageRollupSelect(`WHERE (created_at/3600)*3600 IN ((OLD.created_at/3600)*3600,(NEW.created_at/3600)*3600)`) + `; END;`
	deleteTrigger := `CREATE TRIGGER IF NOT EXISTS usage_hourly_rollup_delete AFTER DELETE ON usage_records BEGIN
DELETE FROM usage_hourly_rollups WHERE bucket=(OLD.created_at/3600)*3600;
INSERT INTO usage_hourly_rollups ` + usageRollupColumns + ` ` + usageRollupSelect(`WHERE (created_at/3600)*3600=(OLD.created_at/3600)*3600`) + `; END;`
	for index, statement := range []string{insertTrigger, updateTrigger, deleteTrigger} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create usage hourly trigger %d: %w", index, err)
		}
	}
	return nil
}

type UsageRollupStats struct {
	Rows        int64 `json:"rows"`
	FirstBucket int64 `json:"first_bucket"`
	LastBucket  int64 `json:"last_bucket"`
}

func (s *Store) UsageHourlyRollupStats(ctx context.Context) (UsageRollupStats, error) {
	var out UsageRollupStats
	if s == nil || s.driver != "sqlite" {
		return out, nil
	}
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(bucket),0),COALESCE(MAX(bucket),0) FROM usage_hourly_rollups`).Scan(&out.Rows, &out.FirstBucket, &out.LastBucket)
	return out, err
}

func fullUsageRollupBounds(since, until int64) (int64, int64, bool) {
	fullStart := ((since + 3599) / 3600) * 3600
	fullUntil := (until / 3600) * 3600
	return fullStart, fullUntil, fullStart < fullUntil
}

func (s *Store) usageTimeseriesHourlyWindow(ctx context.Context, since, until, bucketSeconds int64) ([]UsageBucket, error) {
	if s == nil || s.driver != "sqlite" {
		return nil, nil
	}
	fullStart, fullUntil, ok := fullUsageRollupBounds(since, until)
	if !ok || bucketSeconds%3600 != 0 {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (source_bucket / ?) * ? AS output_bucket,
       SUM(requests),SUM(prompt_tokens),SUM(completion_tokens),SUM(cached_tokens),
       SUM(cache_read_tokens),SUM(cache_creation_tokens),SUM(total_tokens)
FROM (
  SELECT bucket AS source_bucket,SUM(requests) AS requests,SUM(prompt_tokens) AS prompt_tokens,
         SUM(completion_tokens) AS completion_tokens,SUM(cached_tokens) AS cached_tokens,
         SUM(cache_read_tokens) AS cache_read_tokens,SUM(cache_creation_tokens) AS cache_creation_tokens,
         SUM(total_tokens) AS total_tokens
  FROM usage_hourly_rollups WHERE estimated=0 AND bucket>=? AND bucket<? GROUP BY bucket
  UNION ALL
  SELECT (created_at/3600)*3600,COUNT(*),SUM(prompt_tokens),SUM(completion_tokens),SUM(cached_tokens),
         SUM(CASE WHEN cache_read_tokens>0 THEN cache_read_tokens ELSE cached_tokens END),
         SUM(cache_creation_tokens),SUM(total_tokens)
  FROM usage_records
  WHERE estimated=0 AND created_at>=? AND created_at<? AND (created_at<? OR created_at>=?)
  GROUP BY (created_at/3600)*3600
) source
GROUP BY output_bucket ORDER BY output_bucket`, bucketSeconds, bucketSeconds, fullStart, fullUntil, since, until, fullStart, fullUntil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageBucket{}
	for rows.Next() {
		var bucket UsageBucket
		if err := rows.Scan(&bucket.Bucket, &bucket.Requests, &bucket.PromptTokens, &bucket.CompletionTokens, &bucket.CachedTokens, &bucket.CacheReadTokens, &bucket.CacheCreationTokens, &bucket.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, bucket)
	}
	return out, rows.Err()
}

func (s *Store) cacheUsageHourlyWindow(ctx context.Context, since, until, bucketSeconds int64) ([]CacheUsageBucket, error) {
	if s == nil || s.driver != "sqlite" {
		return nil, nil
	}
	fullStart, fullUntil, ok := fullUsageRollupBounds(since, until)
	if !ok || bucketSeconds%3600 != 0 {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (source_bucket / ?) * ? AS output_bucket,
       SUM(requests),SUM(real_requests),SUM(hit_requests),SUM(prompt_tokens),SUM(cache_input_tokens),
       SUM(cache_miss_tokens),SUM(cache_read_tokens),SUM(cache_creation_tokens),SUM(cache_creation_5m_tokens),
       SUM(cache_creation_1h_tokens),SUM(estimated_requests),SUM(cache_creation_reported_requests)
FROM (
  SELECT bucket AS source_bucket,SUM(requests) AS requests,SUM(real_requests) AS real_requests,
         SUM(hit_requests) AS hit_requests,SUM(prompt_tokens) AS prompt_tokens,SUM(cache_input_tokens) AS cache_input_tokens,
         SUM(cache_miss_tokens) AS cache_miss_tokens,SUM(cache_read_tokens) AS cache_read_tokens,
         SUM(cache_creation_tokens) AS cache_creation_tokens,SUM(cache_creation_5m_tokens) AS cache_creation_5m_tokens,
         SUM(cache_creation_1h_tokens) AS cache_creation_1h_tokens,
         SUM(CASE WHEN estimated>0 THEN requests ELSE 0 END) AS estimated_requests,
         SUM(cache_creation_reported_requests) AS cache_creation_reported_requests
  FROM usage_hourly_rollups WHERE bucket>=? AND bucket<? GROUP BY bucket
  UNION ALL
  SELECT (created_at/3600)*3600,COUNT(*),SUM(`+realUsageRecordSQL+`),
         SUM(CASE WHEN `+cacheReadTokensSQL+`>0 THEN 1 ELSE 0 END),SUM(prompt_tokens),SUM(`+cacheTotalInputTokensSQL+`),
         SUM(`+cacheMissTokensSQL+`),SUM(`+cacheReadTokensSQL+`),SUM(cache_creation_tokens),
         SUM(cache_creation_5m_tokens),SUM(cache_creation_1h_tokens),SUM(`+estimatedUsageRecordSQL+`),SUM(cache_creation_present)
  FROM usage_records
  WHERE created_at>=? AND created_at<? AND (created_at<? OR created_at>=?)
  GROUP BY (created_at/3600)*3600
) source
GROUP BY output_bucket ORDER BY output_bucket`, bucketSeconds, bucketSeconds, fullStart, fullUntil, since, until, fullStart, fullUntil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CacheUsageBucket{}
	for rows.Next() {
		var bucket CacheUsageBucket
		if err := rows.Scan(&bucket.Bucket, &bucket.Requests, &bucket.RealRequests, &bucket.HitRequests, &bucket.PromptTokens, &bucket.CacheInputTokens, &bucket.CacheMissTokens, &bucket.CacheReadTokens, &bucket.CacheCreationTokens, &bucket.CacheCreation5mTokens, &bucket.CacheCreation1hTokens, &bucket.EstimatedRequests, &bucket.CacheCreationReportedRequests); err != nil {
			return nil, err
		}
		finalizeCacheUsageBucket(&bucket)
		out = append(out, bucket)
	}
	return out, rows.Err()
}

func (s *Store) cacheUsageSummaryHourlyWindow(ctx context.Context, since, until int64) (CacheUsageMetricRow, bool, error) {
	if s == nil || s.driver != "sqlite" {
		return CacheUsageMetricRow{}, false, nil
	}
	fullStart, fullUntil, ok := fullUsageRollupBounds(since, until)
	if !ok {
		return CacheUsageMetricRow{}, false, nil
	}
	var row CacheUsageMetricRow
	err := s.rdb.QueryRowContext(ctx, `
SELECT SUM(requests),SUM(real_requests),SUM(hit_requests),SUM(prompt_tokens),SUM(cached_tokens),
       SUM(cache_input_tokens),SUM(cache_miss_tokens),SUM(cache_read_tokens),SUM(cache_creation_tokens),
       SUM(cache_creation_5m_tokens),SUM(cache_creation_1h_tokens),SUM(estimated_requests),
       SUM(real_cache_input_tokens),SUM(real_cache_read_tokens),SUM(cache_creation_reported_requests),
       SUM(actual_requests),SUM(actual_prompt_tokens),SUM(actual_completion_tokens),SUM(actual_total_tokens),
       SUM(estimated_prompt_tokens),SUM(estimated_completion_tokens),SUM(estimated_total_tokens),
       SUM(cache_reported_requests),SUM(cache_unreported_requests),SUM(kiro_credits),SUM(kiro_credits_present)
FROM (
  SELECT COALESCE(SUM(requests),0) AS requests,COALESCE(SUM(real_requests),0) AS real_requests,
         COALESCE(SUM(hit_requests),0) AS hit_requests,COALESCE(SUM(prompt_tokens),0) AS prompt_tokens,
         COALESCE(SUM(cached_tokens),0) AS cached_tokens,COALESCE(SUM(cache_input_tokens),0) AS cache_input_tokens,
         COALESCE(SUM(cache_miss_tokens),0) AS cache_miss_tokens,COALESCE(SUM(cache_read_tokens),0) AS cache_read_tokens,
         COALESCE(SUM(cache_creation_tokens),0) AS cache_creation_tokens,
         COALESCE(SUM(cache_creation_5m_tokens),0) AS cache_creation_5m_tokens,
         COALESCE(SUM(cache_creation_1h_tokens),0) AS cache_creation_1h_tokens,
         COALESCE(SUM(CASE WHEN estimated>0 THEN requests ELSE 0 END),0) AS estimated_requests,
         COALESCE(SUM(CASE WHEN estimated=0 AND cache_unreported=0 THEN cache_input_tokens ELSE 0 END),0) AS real_cache_input_tokens,
         COALESCE(SUM(CASE WHEN estimated=0 AND cache_unreported=0 THEN cache_read_tokens ELSE 0 END),0) AS real_cache_read_tokens,
         COALESCE(SUM(cache_creation_reported_requests),0) AS cache_creation_reported_requests,
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN requests ELSE 0 END),0) AS actual_requests,
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN prompt_tokens ELSE 0 END),0) AS actual_prompt_tokens,
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN completion_tokens ELSE 0 END),0) AS actual_completion_tokens,
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN total_tokens ELSE 0 END),0) AS actual_total_tokens,
         COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0) AS estimated_prompt_tokens,
         COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0) AS estimated_completion_tokens,
         COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0) AS estimated_total_tokens,
         COALESCE(SUM(CASE WHEN usage_provider='kiro' AND cache_unreported=0 THEN requests ELSE 0 END),0) AS cache_reported_requests,
         COALESCE(SUM(CASE WHEN usage_provider='kiro' AND cache_unreported>0 THEN requests ELSE 0 END),0) AS cache_unreported_requests,
         COALESCE(SUM(kiro_credits),0) AS kiro_credits,COALESCE(SUM(kiro_credits_present),0) AS kiro_credits_present
  FROM usage_hourly_rollups WHERE bucket>=? AND bucket<?
  UNION ALL
  SELECT COUNT(*),COALESCE(SUM(`+realUsageRecordSQL+`),0),
         COALESCE(SUM(CASE WHEN `+cacheReadTokensSQL+`>0 THEN 1 ELSE 0 END),0),
         COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(cached_tokens),0),COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
         COALESCE(SUM(`+cacheMissTokensSQL+`),0),COALESCE(SUM(`+cacheReadTokensSQL+`),0),COALESCE(SUM(cache_creation_tokens),0),
         COALESCE(SUM(cache_creation_5m_tokens),0),COALESCE(SUM(cache_creation_1h_tokens),0),COALESCE(SUM(`+estimatedUsageRecordSQL+`),0),
         COALESCE(SUM(`+realCacheInputTokensSQL+`),0),COALESCE(SUM(`+realCacheReadTokensSQL+`),0),COALESCE(SUM(cache_creation_present),0),
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN 1 ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN prompt_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN completion_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN total_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN usage_provider='kiro' AND (cache_read_present>0 OR cache_creation_present>0) THEN 1 ELSE 0 END),0),
         COALESCE(SUM(CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 1 ELSE 0 END),0),
         COALESCE(SUM(kiro_credits),0),COALESCE(SUM(kiro_credits_present),0)
  FROM usage_records
  WHERE created_at>=? AND created_at<? AND (created_at<? OR created_at>=?)
) combined`, fullStart, fullUntil, since, until, fullStart, fullUntil).Scan(
		&row.Requests, &row.RealRequests, &row.HitRequests, &row.PromptTokens, &row.CachedTokens,
		&row.CacheInputTokens, &row.CacheMissTokens, &row.CacheReadTokens, &row.CacheCreationTokens,
		&row.CacheCreation5mTokens, &row.CacheCreation1hTokens, &row.EstimatedRequests,
		&row.realCacheInputTokens, &row.realCacheReadTokens, &row.CacheCreationReportedRequests,
		&row.ActualRequests, &row.ActualPromptTokens, &row.ActualCompletionTokens, &row.ActualTotalTokens,
		&row.EstimatedPromptTokens, &row.EstimatedCompletionTokens, &row.EstimatedTotalTokens,
		&row.CacheReportedRequests, &row.CacheUnreportedRequests, &row.KiroCredits, &row.KiroCreditsReportedRequests,
	)
	if err != nil {
		return CacheUsageMetricRow{}, true, err
	}
	finalizeCacheUsageMetric(&row)
	return row, true, nil
}
