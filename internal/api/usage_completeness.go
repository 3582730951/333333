package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

func (s *Server) currentUsageCompleteness(ctx context.Context, snapshotAt int64) (storage.UsageCompleteness, error) {
	flushCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	timedOut, flushErr := s.flushTelemetry(flushCtx)
	cancel()
	if flushErr != nil {
		return storage.UsageCompleteness{}, flushErr
	}
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
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
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
	bucket, _ := strconv.ParseInt(r.URL.Query().Get("bucket"), 10, 64)
	if bucket <= 0 {
		bucket = 3600
	}
	dimension := strings.TrimSpace(r.URL.Query().Get("dimension"))
	if dimension == "" {
		dimension = "model"
	}
	if dimension != "model" && dimension != "provider_model" {
		writeError(w, http.StatusBadRequest, errors.New("dimension must be model or provider_model"))
		return
	}
	seriesDimension := strings.TrimSpace(r.URL.Query().Get("series_dimension"))
	if seriesDimension != "" && seriesDimension != "model" && seriesDimension != "provider_model" {
		writeError(w, http.StatusBadRequest, errors.New("series_dimension must be model or provider_model"))
		return
	}
	seriesLimit := 6
	if raw := strings.TrimSpace(r.URL.Query().Get("series_limit")); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n <= 0 || n > 20 {
			writeError(w, http.StatusBadRequest, errors.New("series_limit must be an integer from 1 to 20"))
			return
		}
		seriesLimit = n
	}
	cacheFields, err := parseCacheUsageFields(r.URL.Query().Get("fields"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	modelsUntil := win.storageUntilAt()
	if meta.UsageCompleteThroughAt < modelsUntil {
		modelsUntil = meta.UsageCompleteThroughAt
	}
	includeCacheField := func(name string) bool { return len(cacheFields) == 0 || cacheFields[name] }
	stableUntil := cacheWin.storageUntilAt()
	if meta.UsageCompleteThroughAt < stableUntil {
		stableUntil = meta.UsageCompleteThroughAt
	}
	stableFields := cacheFields
	if cacheFields != nil {
		stableFields = map[string]bool{"summary": true}
	}

	// These aggregates are independent read-only snapshots. The store has a WAL
	// read pool, so execute them concurrently instead of serially scanning the same
	// usage window roughly ten times before the page can render.
	var (
		accounts           []storage.UsageSummaryRow
		timeseries         []storage.UsageBucket
		models             interface{}
		series             interface{}
		modelSeries        interface{}
		realtime           storage.CacheUsageReport
		stable             storage.CacheUsageReport
		customCacheBuckets []storage.CacheUsageBucket
		tasks              []func() error
	)
	tasks = append(tasks,
		func() error {
			var queryErr error
			accounts, queryErr = s.store.UsageSummaryWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
			return queryErr
		},
		func() error {
			var queryErr error
			timeseries, queryErr = s.store.UsageTimeseriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket)
			return queryErr
		},
		func() error {
			var queryErr error
			if dimension == "provider_model" {
				var rows []storage.ProviderModelUsageRow
				rows, queryErr = s.store.UsageByProviderModelWindow(r.Context(), win.EffectiveStartAt, modelsUntil)
				models = nonNilProviderModelUsageRows(rows)
			} else {
				var rows []storage.UserUsageRow
				rows, queryErr = s.store.UsageByModelWindow(r.Context(), win.EffectiveStartAt, modelsUntil)
				models = nonNilUserUsageRows(rows)
			}
			return queryErr
		},
		func() error {
			var queryErr error
			if seriesDimension == "provider_model" {
				series, modelSeries, queryErr = s.store.UsageProviderModelSeriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket, seriesLimit)
			} else if seriesDimension == "model" {
				series, modelSeries, queryErr = s.store.UsageModelSeriesWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket, seriesLimit)
			}
			return queryErr
		},
		func() error {
			var queryErr error
			realtime, queryErr = s.store.CacheUsageMetricsWindowFields(r.Context(), cacheWin.EffectiveStartAt, cacheWin.storageUntilAt(), 200, cacheFields)
			return queryErr
		},
		func() error {
			var queryErr error
			stable, queryErr = s.store.CacheUsageMetricsWindowFields(r.Context(), cacheWin.EffectiveStartAt, stableUntil, 0, stableFields)
			return queryErr
		},
	)
	if bucket != 3600 && includeCacheField("by_time_bucket") {
		tasks = append(tasks, func() error {
			var queryErr error
			customCacheBuckets, queryErr = s.store.CacheUsageBucketsWindow(r.Context(), cacheWin.EffectiveStartAt, cacheWin.storageUntilAt(), bucket)
			return queryErr
		})
	}
	var wg sync.WaitGroup
	queryErrors := make(chan error, len(tasks))
	for _, task := range tasks {
		wg.Add(1)
		go func(run func() error) {
			defer wg.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					supervisor.LogPanic("usage-dashboard-query", panicValue)
					queryErrors <- errors.New("usage dashboard query failed")
				}
			}()
			if queryErr := run(); queryErr != nil {
				queryErrors <- queryErr
			}
		}(task)
	}
	wg.Wait()
	close(queryErrors)
	if queryErr := <-queryErrors; queryErr != nil {
		writeError(w, http.StatusInternalServerError, queryErr)
		return
	}
	if customCacheBuckets != nil {
		realtime.ByTimeBucket = customCacheBuckets
	}
	markUsageBucketsPartial(timeseries, bucket, meta.UsageCompleteThroughAt)
	markCacheBucketsPartial(realtime.ByTimeBucket, bucket, meta.UsageCompleteThroughAt)
	cacheBody := usageDashboardCacheBody(realtime, stable.Summary, cacheFields)
	cacheBody = mergeCompletenessFields(mergeWindowFields(cacheBody, cacheWin), meta)
	body := map[string]interface{}{
		"accounts": nonNilUsageSummary(accounts), "timeseries": nonNilUsageBuckets(timeseries), "models": models,
		"cache": cacheBody, "stable_cache": stable, "summary": realtime.Summary, "stable_summary": stable.Summary,
	}
	if seriesDimension != "" {
		body["series_dimension"] = seriesDimension
		body["series"] = series
		body["model_series"] = modelSeries
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(body, win), meta))
}

func usageDashboardCacheBody(report storage.CacheUsageReport, stableSummary storage.CacheUsageMetricRow, fields map[string]bool) map[string]interface{} {
	all := len(fields) == 0
	want := func(name string) bool { return all || fields[name] }
	body := map[string]interface{}{}
	if want("summary") {
		body["summary"] = report.Summary
		body["stable_summary"] = stableSummary
	}
	if want("by_account") {
		body["by_account"] = nonNilCacheUsageRows(report.ByAccount)
	}
	if want("by_model") {
		body["by_model"] = nonNilCacheUsageRows(report.ByModel)
	}
	if want("by_api_key") {
		body["by_api_key"] = nonNilCacheUsageRows(report.ByAPIKey)
	}
	if want("by_account_model") {
		body["by_account_model"] = nonNilCacheUsageRows(report.ByAccountModel)
	}
	if want("by_provider") {
		body["by_provider"] = nonNilCacheUsageRows(report.ByProvider)
	}
	if want("by_provider_model") {
		body["by_provider_model"] = nonNilCacheUsageRows(report.ByProviderModel)
	}
	if want("by_route") {
		body["by_route"] = nonNilCacheUsageRows(report.ByRoute)
	}
	if want("by_route_account_model") {
		body["by_route_account_model"] = nonNilCacheUsageRows(report.ByRouteAccountModel)
	}
	if want("by_time_bucket") {
		body["by_time_bucket"] = nonNilCacheUsageBuckets(report.ByTimeBucket)
	}
	return body
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

func nonNilProviderModelUsageRows(rows []storage.ProviderModelUsageRow) []storage.ProviderModelUsageRow {
	if rows == nil {
		return []storage.ProviderModelUsageRow{}
	}
	return rows
}
