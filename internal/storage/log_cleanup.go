package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const sqliteIncrementalVacuumSetting = "sqlite_incremental_vacuum_enabled"

// LogRecordCounts describes the disposable observability/history rows managed by
// the unified log-retention policy. Active billing holds are deliberately excluded:
// they are live accounting state rather than logs.
type LogRecordCounts struct {
	AuditLog               int64 `json:"audit_log"`
	CFEvents               int64 `json:"cf_events"`
	UsageRecords           int64 `json:"usage_records"`
	UsageEvents            int64 `json:"usage_events"`
	RegistrationTaskEvents int64 `json:"registration_task_events"`
	LifecycleTaskLogs      int64 `json:"lifecycle_task_logs"`
	LifecycleEvents        int64 `json:"lifecycle_events"`
	ProxyUsageRecords      int64 `json:"proxy_usage_records"`
	TerminalBillingHolds   int64 `json:"terminal_billing_holds"`
}

func (c LogRecordCounts) Total() int64 {
	return c.AuditLog + c.CFEvents + c.UsageRecords + c.UsageEvents + c.RegistrationTaskEvents +
		c.LifecycleTaskLogs + c.LifecycleEvents + c.ProxyUsageRecords + c.TerminalBillingHolds
}

type LogClearResult struct {
	Deleted                     LogRecordCounts `json:"deleted"`
	PreservedActiveBillingHolds int64           `json:"preserved_active_billing_holds"`
}

// ClearLogRecords atomically removes all disposable log/history rows. A live
// billing hold is preserved so clearing diagnostics cannot alter in-flight token
// accounting.
func (s *Store) ClearLogRecords(ctx context.Context) (LogClearResult, error) {
	if s == nil || s.db == nil {
		return LogClearResult{}, errors.New("store is not initialized")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return LogClearResult{}, err
	}
	defer tx.Rollback()

	var result LogClearResult
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_holds WHERE status='held'`).Scan(&result.PreservedActiveBillingHolds); err != nil {
		return LogClearResult{}, err
	}
	statements := []struct {
		query string
		count *int64
	}{
		{`DELETE FROM audit_log`, &result.Deleted.AuditLog},
		{`DELETE FROM cf_events`, &result.Deleted.CFEvents},
		{`DELETE FROM usage_records`, &result.Deleted.UsageRecords},
		{`DELETE FROM usage_events
WHERE hold_id='' OR NOT EXISTS (
	SELECT 1 FROM billing_holds h WHERE h.id=usage_events.hold_id AND h.status='held'
)`, &result.Deleted.UsageEvents},
		{`DELETE FROM registration_task_events`, &result.Deleted.RegistrationTaskEvents},
		{`DELETE FROM lifecycle_task_logs`, &result.Deleted.LifecycleTaskLogs},
		{`DELETE FROM lifecycle_events`, &result.Deleted.LifecycleEvents},
		{`DELETE FROM proxy_usage_records`, &result.Deleted.ProxyUsageRecords},
		{`DELETE FROM billing_holds WHERE status <> 'held'`, &result.Deleted.TerminalBillingHolds},
	}
	for _, statement := range statements {
		execResult, execErr := tx.ExecContext(ctx, statement.query)
		if execErr != nil {
			return LogClearResult{}, execErr
		}
		*statement.count, _ = execResult.RowsAffected()
	}
	if err := tx.Commit(); err != nil {
		return LogClearResult{}, err
	}
	return result, nil
}

// PurgeLogRecordsBefore removes expired log rows in small transactions so the
// SQLite writer is yielded between batches. It returns partial counts alongside an
// error if a later table fails; already committed batches remain deleted.
func (s *Store) PurgeLogRecordsBefore(ctx context.Context, cutoff int64, batchSize int) (LogRecordCounts, error) {
	if s == nil || s.db == nil {
		return LogRecordCounts{}, errors.New("store is not initialized")
	}
	if cutoff <= 0 {
		return LogRecordCounts{}, errors.New("log retention cutoff must be positive")
	}
	if batchSize <= 0 {
		batchSize = 1000
	}
	if batchSize > 10_000 {
		batchSize = 10_000
	}
	deleteBatches := func(query string) (int64, error) {
		var total int64
		for {
			result, err := s.db.ExecContext(ctx, query, cutoff, batchSize)
			if err != nil {
				return total, err
			}
			deleted, err := result.RowsAffected()
			if err != nil {
				return total, err
			}
			total += deleted
			if deleted < int64(batchSize) {
				return total, nil
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}

	var counts LogRecordCounts
	steps := []struct {
		query string
		count *int64
	}{
		{`DELETE FROM audit_log WHERE rowid IN (SELECT rowid FROM audit_log WHERE created_at < ? LIMIT ?)`, &counts.AuditLog},
		{`DELETE FROM cf_events WHERE rowid IN (SELECT rowid FROM cf_events WHERE created_at < ? LIMIT ?)`, &counts.CFEvents},
		{`DELETE FROM usage_records WHERE rowid IN (SELECT rowid FROM usage_records WHERE created_at < ? LIMIT ?)`, &counts.UsageRecords},
		{`DELETE FROM usage_events WHERE rowid IN (
SELECT e.rowid FROM usage_events e
WHERE e.updated_at < ?
  AND (e.hold_id='' OR NOT EXISTS (
	  SELECT 1 FROM billing_holds h WHERE h.id=e.hold_id AND h.status='held'
  ))
LIMIT ?)`, &counts.UsageEvents},
		{`DELETE FROM registration_task_events WHERE rowid IN (SELECT rowid FROM registration_task_events WHERE created_at < ? LIMIT ?)`, &counts.RegistrationTaskEvents},
		{`DELETE FROM lifecycle_task_logs WHERE rowid IN (SELECT rowid FROM lifecycle_task_logs WHERE timestamp < ? LIMIT ?)`, &counts.LifecycleTaskLogs},
		{`DELETE FROM lifecycle_events WHERE rowid IN (SELECT rowid FROM lifecycle_events WHERE timestamp < ? LIMIT ?)`, &counts.LifecycleEvents},
		{`DELETE FROM proxy_usage_records WHERE rowid IN (SELECT rowid FROM proxy_usage_records WHERE created_at < ? LIMIT ?)`, &counts.ProxyUsageRecords},
		{`DELETE FROM billing_holds WHERE rowid IN (SELECT rowid FROM billing_holds WHERE status <> 'held' AND updated_at < ? LIMIT ?)`, &counts.TerminalBillingHolds},
	}
	for _, step := range steps {
		deleted, err := deleteBatches(step.query)
		*step.count = deleted
		if err != nil {
			return counts, err
		}
	}
	return counts, nil
}

// CheckpointLogStorage truncates the WAL after a retention sweep. Free pages in
// the main database remain reusable, preventing subsequent log writes from growing
// the file again.
func (s *Store) CheckpointLogStorage(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("store is not initialized")
	}
	if s.driver == "postgres" {
		return nil
	}
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("sqlite WAL checkpoint remained busy (%d frames, %d checkpointed)", logFrames, checkpointed)
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// ReclaimLogStorage compacts the SQLite database after a manual clear or the
// weekly maintenance window. VACUUM is intentionally separate from row deletion:
// callers can report a successful clear even if an active reader delays compaction.
func (s *Store) ReclaimLogStorage(ctx context.Context) error {
	if s != nil && s.driver == "postgres" {
		return nil
	}
	if err := s.CheckpointLogStorage(ctx); err != nil {
		return err
	}
	if s.incrementalVacuumEnabled(ctx) {
		var mode int
		if err := s.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
			return err
		}
		// SQLite only performs incremental_vacuum when the file was explicitly
		// prepared with auto_vacuum=INCREMENTAL (mode 2). Do not silently issue a
		// costly migration VACUUM here; operators must schedule that one-time step.
		if mode == 2 {
			if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
				return err
			}
			return s.CheckpointLogStorage(ctx)
		}
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	return s.CheckpointLogStorage(ctx)
}

func (s *Store) incrementalVacuumEnabled(ctx context.Context) bool {
	if s == nil || s.db == nil {
		return false
	}
	value, ok, err := s.GetSetting(ctx, sqliteIncrementalVacuumSetting)
	if err != nil {
		return false
	}
	if !ok {
		return s.sqliteIncrementalVacuumDefault
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}
