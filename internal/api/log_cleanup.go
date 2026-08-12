package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	// Keep the existing storage key so upgrades preserve the value already exposed
	// by the Logging settings page. Its scope is now all disposable log records.
	logRetentionDaysSettingKey = "reg_log_retention_days"
	logCleanupLastVacuumKey    = "log_cleanup_last_vacuum_at"
	defaultLogRetentionDays    = 7
	logCleanupBatchSize        = 1000
	logCleanupInterval         = 24 * time.Hour
)

func (s *Server) logRetentionDays(ctx context.Context) int {
	days := s.settingInt(ctx, logRetentionDaysSettingKey, defaultLogRetentionDays)
	if days < 1 {
		return defaultLogRetentionDays
	}
	if days > 90 {
		return 90
	}
	return days
}

func (s *Server) runLogRetentionCleanup(ctx context.Context, now int64) (storage.LogRecordCounts, error) {
	if now <= 0 {
		now = storage.Now()
	}
	cutoff := now - int64(s.logRetentionDays(ctx))*24*60*60
	return s.store.PurgeLogRecordsBefore(ctx, cutoff, logCleanupBatchSize)
}

func (s *Server) maintainLogRetention(ctx context.Context) {
	now := storage.Now()
	counts, err := s.runLogRetentionCleanup(ctx, now)
	if err != nil {
		log.Printf("[LOG-CLEANUP] retention sweep failed: %v", err)
		return
	}
	if counts.Total() == 0 {
		return
	}
	log.Printf("[LOG-CLEANUP] retention_days=%d deleted=%d counts=%+v", s.logRetentionDays(ctx), counts.Total(), counts)
	// Deleted SQLite pages are immediately reusable. VACUUM, however, takes an
	// exclusive writer for a potentially multi-gigabyte rewrite; running it from a
	// live worker can starve the active-role lease and every billing write. Keep the
	// online sweep bounded to row batches plus a WAL checkpoint. Operators can still
	// request an explicit compacting clear through DELETE /admin/logs.
	checkpointCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.store.CheckpointLogStorage(checkpointCtx); err != nil {
		log.Printf("[LOG-CLEANUP] WAL checkpoint failed: %v", err)
	}
}

func (s *Server) startLogRetentionLoop(ctx context.Context) {
	supervisor.Go(ctx, "log-retention", func(ctx context.Context) {
		s.maintainLogRetention(ctx)
		ticker := time.NewTicker(logCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.maintainLogRetention(ctx)
			}
		}
	})
}

func (s *Server) adminLogRecords(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	// Ensure usage rows queued before the destructive request reach SQLite first;
	// rows created by genuinely later inference requests remain valid new history.
	s.WaitForAsyncWrites()
	result, err := s.store.ClearLogRecords(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reclaimed := true
	reclaimWarning := ""
	if err := s.store.ReclaimLogStorage(r.Context()); err != nil {
		reclaimed = false
		reclaimWarning = err.Error()
	} else if err := s.store.SetSetting(r.Context(), logCleanupLastVacuumKey, strconv.FormatInt(storage.Now(), 10)); err != nil {
		// The file was already compacted; failure to persist the maintenance
		// timestamp should not falsely report that disk space was not reclaimed.
		reclaimWarning = "space reclaimed, but the maintenance timestamp was not saved: " + err.Error()
	}
	log.Printf("[LOG-CLEANUP] manual deleted=%d preserved_active_billing_holds=%d reclaimed=%t", result.Deleted.Total(), result.PreservedActiveBillingHolds, reclaimed)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":                             true,
		"deleted":                        result.Deleted,
		"deleted_total":                  result.Deleted.Total(),
		"preserved_active_billing_holds": result.PreservedActiveBillingHolds,
		"space_reclaimed":                reclaimed,
		"reclaim_warning":                reclaimWarning,
		"retention_days":                 s.logRetentionDays(r.Context()),
	})
}
