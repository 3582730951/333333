package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Quota window estimation follows the calculate_money scheme (sub2api ecosystem):
// each quota poll snapshots the account's used_percent together with the USD cost
// the relay recorded inside that same window cycle. The ratio of cost change to
// percentage change over a cycle infers the window's total dollar value without
// ever consulting a plan list price. Raw samples are pruned once a cycle is
// finalized; only the best-quality summary per cycle is kept.
//
// quotaWindowSchemaSQL is additive and driver-neutral: sqlite executes it in
// Store.init, postgres folds it into the checksummed base migration.
const quotaWindowSchemaSQL = `
CREATE TABLE IF NOT EXISTS quota_window_cycles(
  account_id TEXT NOT NULL,
  window_kind TEXT NOT NULL,
  cycle_start INTEGER NOT NULL,
  window_minutes INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, window_kind),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS quota_window_samples(
  account_id TEXT NOT NULL,
  window_kind TEXT NOT NULL,
  cycle_start INTEGER NOT NULL,
  sample_at INTEGER NOT NULL,
  used_percent REAL NOT NULL,
  cost_usd REAL NOT NULL,
  PRIMARY KEY(account_id, window_kind, cycle_start, sample_at),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_quota_window_samples_cycle ON quota_window_samples(account_id, window_kind, cycle_start, sample_at);
CREATE TABLE IF NOT EXISTS quota_window_estimates(
  account_id TEXT NOT NULL,
  window_kind TEXT NOT NULL,
  cycle_start INTEGER NOT NULL,
  estimate_json TEXT NOT NULL DEFAULT '',
  quality_score REAL NOT NULL DEFAULT -1,
  confidence TEXT NOT NULL DEFAULT '',
  finalized_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, window_kind, cycle_start),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
`

// QuotaWindowSample is one poll-time snapshot of a window cycle: the upstream
// used_percent together with the relay's recorded USD cost inside that cycle.
type QuotaWindowSample struct {
	AccountID   string
	WindowKind  string
	CycleStart  int64
	SampleAt    int64
	UsedPercent float64
	CostUSD     float64
}

// QuotaWindowCycle points at the cycle currently being sampled for an account's
// window kind. cycle_start is the bucketed window start (unix seconds).
type QuotaWindowCycle struct {
	AccountID     string
	WindowKind    string
	CycleStart    int64
	WindowMinutes int64
	UpdatedAt     int64
}

// QuotaWindowEstimateSummary is the finalized per-cycle best estimate kept after
// the raw samples of an expired cycle are pruned.
type QuotaWindowEstimateSummary struct {
	AccountID    string
	WindowKind   string
	CycleStart   int64
	EstimateJSON string
	QualityScore float64
	Confidence   string
	FinalizedAt  int64
}

func (s *Store) UpsertQuotaWindowCycle(ctx context.Context, c QuotaWindowCycle) error {
	if s == nil {
		return nil
	}
	if c.UpdatedAt == 0 {
		c.UpdatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO quota_window_cycles(account_id, window_kind, cycle_start, window_minutes, updated_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(account_id, window_kind) DO UPDATE SET
 cycle_start = excluded.cycle_start, window_minutes = excluded.window_minutes, updated_at = excluded.updated_at`,
		strings.TrimSpace(c.AccountID), strings.TrimSpace(c.WindowKind), c.CycleStart, c.WindowMinutes, c.UpdatedAt)
	return err
}

func (s *Store) UpsertQuotaWindowSample(ctx context.Context, sample QuotaWindowSample) error {
	if s == nil {
		return nil
	}
	if sample.SampleAt == 0 {
		sample.SampleAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO quota_window_samples(account_id, window_kind, cycle_start, sample_at, used_percent, cost_usd)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, window_kind, cycle_start, sample_at) DO UPDATE SET
 used_percent = excluded.used_percent, cost_usd = excluded.cost_usd`,
		strings.TrimSpace(sample.AccountID), strings.TrimSpace(sample.WindowKind), sample.CycleStart, sample.SampleAt, sample.UsedPercent, sample.CostUSD)
	return err
}

// QuotaWindowSamples returns the samples of one cycle in sample_at order.
func (s *Store) QuotaWindowSamples(ctx context.Context, accountID, windowKind string, cycleStart int64) ([]QuotaWindowSample, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT account_id, window_kind, cycle_start, sample_at, used_percent, cost_usd
FROM quota_window_samples
WHERE account_id = ? AND window_kind = ? AND cycle_start = ?
ORDER BY sample_at`, strings.TrimSpace(accountID), strings.TrimSpace(windowKind), cycleStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaWindowSample
	for rows.Next() {
		var sample QuotaWindowSample
		if err := rows.Scan(&sample.AccountID, &sample.WindowKind, &sample.CycleStart, &sample.SampleAt, &sample.UsedPercent, &sample.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, sample)
	}
	return out, rows.Err()
}

// QuotaWindowCurrentCycle returns the cycle pointer most recently recorded by a
// poll for the account's window kind.
func (s *Store) QuotaWindowCurrentCycle(ctx context.Context, accountID, windowKind string) (QuotaWindowCycle, bool, error) {
	if s == nil {
		return QuotaWindowCycle{}, false, nil
	}
	row := s.rdb.QueryRowContext(ctx, `
SELECT account_id, window_kind, cycle_start, window_minutes, updated_at
FROM quota_window_cycles
WHERE account_id = ? AND window_kind = ?`, strings.TrimSpace(accountID), strings.TrimSpace(windowKind))
	var c QuotaWindowCycle
	err := row.Scan(&c.AccountID, &c.WindowKind, &c.CycleStart, &c.WindowMinutes, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return QuotaWindowCycle{}, false, nil
		}
		return QuotaWindowCycle{}, false, err
	}
	return c, true, nil
}

// ExpiredQuotaWindowCycles lists distinct cycles older than currentCycleStart
// that have samples but no finalized estimate yet, in cycle_start order.
func (s *Store) ExpiredQuotaWindowCycles(ctx context.Context, accountID, windowKind string, currentCycleStart int64) ([]QuotaWindowCycle, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT s.account_id, s.window_kind, s.cycle_start, COALESCE(c.window_minutes, 0), MAX(s.sample_at)
FROM quota_window_samples s
LEFT JOIN quota_window_cycles c ON c.account_id = s.account_id AND c.window_kind = s.window_kind
LEFT JOIN quota_window_estimates e ON e.account_id = s.account_id AND e.window_kind = s.window_kind AND e.cycle_start = s.cycle_start
WHERE s.account_id = ? AND s.window_kind = ? AND s.cycle_start < ? AND e.cycle_start IS NULL
GROUP BY s.account_id, s.window_kind, s.cycle_start, c.window_minutes
ORDER BY s.cycle_start`, strings.TrimSpace(accountID), strings.TrimSpace(windowKind), currentCycleStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaWindowCycle
	for rows.Next() {
		var c QuotaWindowCycle
		var maxSampleAt int64
		if err := rows.Scan(&c.AccountID, &c.WindowKind, &c.CycleStart, &c.WindowMinutes, &maxSampleAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) FinalizeQuotaWindowEstimate(ctx context.Context, summary QuotaWindowEstimateSummary) error {
	if s == nil {
		return nil
	}
	if summary.FinalizedAt == 0 {
		summary.FinalizedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO quota_window_estimates(account_id, window_kind, cycle_start, estimate_json, quality_score, confidence, finalized_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, window_kind, cycle_start) DO UPDATE SET
 estimate_json = excluded.estimate_json, quality_score = excluded.quality_score,
 confidence = excluded.confidence, finalized_at = excluded.finalized_at`,
		strings.TrimSpace(summary.AccountID), strings.TrimSpace(summary.WindowKind), summary.CycleStart,
		summary.EstimateJSON, summary.QualityScore, strings.TrimSpace(summary.Confidence), summary.FinalizedAt)
	return err
}

// AccountUsageCostRow is one usage record returned for window cost summing.
type AccountUsageCostRow struct {
	Model             string
	PromptTokens      int64
	CompletionTokens  int64
	CacheReadTokens   int64
	CacheCreationTokens int64
	Estimated         int64
}

// AccountUsageCostRows lists the token accounting rows of an account inside
// [start, end] that carried any tokens, oldest first.
func (s *Store) AccountUsageCostRows(ctx context.Context, accountID string, start, end int64) ([]AccountUsageCostRow, error) {
	if s == nil {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT model, prompt_tokens, completion_tokens, cache_read_tokens, cache_creation_tokens, estimated
FROM usage_records
WHERE account_id = ? AND created_at >= ? AND created_at <= ? AND total_tokens > 0
ORDER BY created_at`, strings.TrimSpace(accountID), start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountUsageCostRow
	for rows.Next() {
		var row AccountUsageCostRow
		if err := rows.Scan(&row.Model, &row.PromptTokens, &row.CompletionTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.Estimated); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) DeleteQuotaWindowSamples(ctx context.Context, accountID, windowKind string, cycleStart int64) error {
	if s == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
DELETE FROM quota_window_samples
WHERE account_id = ? AND window_kind = ? AND cycle_start = ?`,
		strings.TrimSpace(accountID), strings.TrimSpace(windowKind), cycleStart)
	return err
}
