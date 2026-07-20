package api

import (
	"context"
	"net/http"
	"time"

	"codex-account-pool/internal/storage"
)

func (s *Server) currentUsageCompleteness(ctx context.Context, snapshotAt int64) (storage.UsageCompleteness, error) {
	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	timedOut := s.flushTelemetry(flushCtx)
	cancel()
	meta, err := s.store.UsageCompleteness(ctx, snapshotAt)
	meta.TelemetryFlushTimedOut = timedOut
	if timedOut {
		meta.PartialData = true
	}
	return meta, err
}

func mergeCompletenessFields(body map[string]interface{}, meta storage.UsageCompleteness) map[string]interface{} {
	body["data_snapshot_at"] = meta.DataSnapshotAt
	body["usage_complete_through_at"] = meta.UsageCompleteThroughAt
	body["usage_lag_seconds"] = meta.UsageLagSeconds
	body["pending_usage_requests"] = meta.PendingUsageRequests
	body["partial_data"] = meta.PartialData
	body["telemetry_flush_timed_out"] = meta.TelemetryFlushTimedOut
	body["completeness_gap_count"] = meta.CompletenessGapCount
	return body
}

func markUsageBucketsPartial(rows []storage.UsageBucket, bucketSeconds, watermark int64) {
	for i := range rows {
		rows[i].Partial = rows[i].Bucket+bucketSeconds > watermark
	}
}

func markCacheBucketsPartial(rows []storage.CacheUsageBucket, bucketSeconds, watermark int64) {
	for i := range rows {
		rows[i].Partial = rows[i].Bucket+bucketSeconds > watermark
	}
}

// adminUsageDashboard returns the primary usage views from one flushed snapshot.
// Existing endpoints remain available for compatibility.
func (s *Server) adminUsageDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	meta, err := s.currentUsageCompleteness(r.Context(), now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cacheWin, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	bucket := int64(3600)
	accounts, err := s.store.UsageSummaryWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	timeseries, err := s.store.UsageTimeseriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	markUsageBucketsPartial(timeseries, bucket, meta.UsageCompleteThroughAt)
	modelsUntil := win.storageUntilAt()
	if meta.UsageCompleteThroughAt < modelsUntil {
		modelsUntil = meta.UsageCompleteThroughAt
	}
	models, err := s.store.UsageByModelWindow(r.Context(), win.EffectiveStartAt, modelsUntil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	realtime, err := s.store.CacheUsageMetricsWindow(r.Context(), cacheWin.EffectiveStartAt, cacheWin.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stableUntil := cacheWin.storageUntilAt()
	if meta.UsageCompleteThroughAt < stableUntil {
		stableUntil = meta.UsageCompleteThroughAt
	}
	stable, err := s.store.CacheUsageMetricsWindow(r.Context(), cacheWin.EffectiveStartAt, stableUntil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	markCacheBucketsPartial(realtime.ByTimeBucket, bucket, meta.UsageCompleteThroughAt)
	body := map[string]interface{}{
		"accounts": nonNilUsageSummary(accounts), "timeseries": nonNilUsageBuckets(timeseries), "models": nonNilUserUsageRows(models),
		"cache": realtime, "stable_cache": stable, "summary": realtime.Summary, "stable_summary": stable.Summary,
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(body, win), meta))
}

func nonNilUsageSummary(rows []storage.UsageSummaryRow) []storage.UsageSummaryRow {
	if rows == nil {
		return []storage.UsageSummaryRow{}
	}
	return rows
}
func nonNilUsageBuckets(rows []storage.UsageBucket) []storage.UsageBucket {
	if rows == nil {
		return []storage.UsageBucket{}
	}
	return rows
}
func nonNilUserUsageRows(rows []storage.UserUsageRow) []storage.UserUsageRow {
	if rows == nil {
		return []storage.UserUsageRow{}
	}
	return rows
}
