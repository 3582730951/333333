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
	meta, err := s.store.UsageCompleteness(ctx, snapshotAt)
	meta.TelemetryFlushTimedOut = timedOut
	// Telemetry is durable in the journal before this barrier runs. A busy SQLite
	// materializer therefore makes the view partial, not unavailable. Returning a
	// stable read snapshot keeps the control plane alive under writer pressure and
	// lets the normal replayer catch up without losing usage records.
	if timedOut || flushErr != nil {
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

// scopedUsageWindowRequest lets aggregate consumers retain different time
// windows for individual views without paying for separate HTTP requests. The
// unscoped since/until values remain the fallback for existing callers.
func scopedUsageWindowRequest(r *http.Request, scope string) *http.Request {
	query := r.URL.Query()
	changed := false
	for _, bound := range []string{"since", "until"} {
		if values, ok := query[scope+"_"+bound]; ok {
			query[bound] = append([]string(nil), values...)
			changed = true
		}
	}
	if !changed {
		return r
	}
	clone := r.Clone(r.Context())
	clonedURL := *r.URL
	clonedURL.RawQuery = query.Encode()
	clone.URL = &clonedURL
	return clone
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
	if s.usageDashboardCache != nil {
		s.usageDashboardCache.Serve(w, r, s.adminUsageDashboardFresh)
		return
	}
	s.adminUsageDashboardFresh(w, r)
}

func (s *Server) adminUsageDashboardFresh(w http.ResponseWriter, r *http.Request) {
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
	timeseriesWin, err := s.resolveAdminUsageWindow(scopedUsageWindowRequest(r, "timeseries"), now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	modelsWin, err := s.resolveAdminUsageWindow(scopedUsageWindowRequest(r, "models"), now, false)
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
	allowPartial := false
	if raw := strings.TrimSpace(r.URL.Query().Get("allow_partial")); raw != "" {
		allowPartial, err = strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("allow_partial must be a boolean"))
			return
		}
	}
	modelsUntil := modelsWin.storageUntilAt()
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
	type dashboardTask struct {
		section string
		run     func() error
	}
	var (
		accounts           []storage.UsageSummaryRow
		timeseries         []storage.UsageBucket
		models             interface{}
		series             interface{}
		modelSeries        interface{}
		realtime           storage.CacheUsageReport
		stable             storage.CacheUsageReport
		customCacheBuckets []storage.CacheUsageBucket
		tasks              []dashboardTask
	)
	addTask := func(section string, run func() error) {
		tasks = append(tasks, dashboardTask{section: section, run: run})
	}
	addTask("accounts", func() error {
		var queryErr error
		accounts, queryErr = s.store.UsageSummaryWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
		return queryErr
	})
	addTask("timeseries", func() error {
		var queryErr error
		timeseries, queryErr = s.store.UsageTimeseriesWindow(r.Context(), timeseriesWin.EffectiveStartAt, timeseriesWin.storageUntilAt(), bucket)
		return queryErr
	})
	addTask("models", func() error {
		var queryErr error
		if dimension == "provider_model" {
			var rows []storage.ProviderModelUsageRow
			rows, queryErr = s.store.UsageByProviderModelWindow(r.Context(), modelsWin.EffectiveStartAt, modelsUntil)
			models = nonNilProviderModelUsageRows(rows)
		} else {
			var rows []storage.UserUsageRow
			rows, queryErr = s.store.UsageByModelWindow(r.Context(), modelsWin.EffectiveStartAt, modelsUntil)
			models = nonNilUserUsageRows(rows)
		}
		return queryErr
	})
	addTask("timeseries", func() error {
		var queryErr error
		if seriesDimension == "provider_model" {
			series, modelSeries, queryErr = s.store.UsageProviderModelSeriesWindow(r.Context(), timeseriesWin.EffectiveStartAt, timeseriesWin.storageUntilAt(), bucket, seriesLimit)
		} else if seriesDimension == "model" {
			series, modelSeries, queryErr = s.store.UsageModelSeriesWindow(r.Context(), timeseriesWin.EffectiveStartAt, timeseriesWin.storageUntilAt(), bucket, seriesLimit)
		}
		return queryErr
	})
	addTask("cache", func() error {
		var queryErr error
		realtime, queryErr = s.store.CacheUsageMetricsWindowFields(r.Context(), cacheWin.EffectiveStartAt, cacheWin.storageUntilAt(), 200, cacheFields)
		return queryErr
	})
	addTask("cache", func() error {
		var queryErr error
		stable, queryErr = s.store.CacheUsageMetricsWindowFields(r.Context(), cacheWin.EffectiveStartAt, stableUntil, 0, stableFields)
		return queryErr
	})
	if bucket != 3600 && includeCacheField("by_time_bucket") {
		addTask("cache", func() error {
			var queryErr error
			customCacheBuckets, queryErr = s.store.CacheUsageBucketsWindow(r.Context(), cacheWin.EffectiveStartAt, cacheWin.storageUntilAt(), bucket)
			return queryErr
		})
	}
	type dashboardQueryError struct {
		section string
		err     error
	}
	var wg sync.WaitGroup
	queryErrors := make(chan dashboardQueryError, len(tasks))
	for _, task := range tasks {
		wg.Add(1)
		go func(task dashboardTask) {
			defer wg.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					supervisor.LogPanic("usage-dashboard-query", panicValue)
					queryErrors <- dashboardQueryError{section: task.section, err: errors.New("usage dashboard query failed")}
				}
			}()
			if queryErr := task.run(); queryErr != nil {
				queryErrors <- dashboardQueryError{section: task.section, err: queryErr}
			}
		}(task)
	}
	wg.Wait()
	close(queryErrors)
	failedSections := make(map[string]bool)
	var firstQueryError error
	for queryErr := range queryErrors {
		failedSections[queryErr.section] = true
		if firstQueryError == nil {
			firstQueryError = queryErr.err
		}
	}
	if firstQueryError != nil && !allowPartial {
		writeError(w, http.StatusInternalServerError, firstQueryError)
		return
	}
	if firstQueryError != nil {
		// Telemetry completeness is only one way an aggregate can be partial. If an
		// independently queried section failed, advertise the whole response as partial
		// as well as naming the unavailable section below.
		meta.PartialData = true
	}
	body := map[string]interface{}{
		"timeseries_effective_start_at": timeseriesWin.EffectiveStartAt, "timeseries_effective_until_at": timeseriesWin.EffectiveUntilAt,
		"models_effective_start_at": modelsWin.EffectiveStartAt, "models_effective_until_at": modelsWin.EffectiveUntilAt,
	}
	if !failedSections["accounts"] {
		body["accounts"] = nonNilUsageSummary(accounts)
	}
	if !failedSections["timeseries"] {
		markUsageBucketsPartial(timeseries, bucket, meta.UsageCompleteThroughAt)
		body["timeseries"] = nonNilUsageBuckets(timeseries)
		if seriesDimension != "" {
			body["series_dimension"] = seriesDimension
			body["series"] = series
			body["model_series"] = modelSeries
		}
	}
	if !failedSections["models"] {
		body["models"] = models
	}
	if !failedSections["cache"] {
		if customCacheBuckets != nil {
			realtime.ByTimeBucket = customCacheBuckets
		}
		markCacheBucketsPartial(realtime.ByTimeBucket, bucket, meta.UsageCompleteThroughAt)
		cacheBody := usageDashboardCacheBody(realtime, stable.Summary, cacheFields)
		body["cache"] = mergeCompletenessFields(mergeWindowFields(cacheBody, cacheWin), meta)
		body["stable_cache"] = stable
		body["summary"] = realtime.Summary
		body["stable_summary"] = stable.Summary
	}
	if allowPartial {
		unavailableSections := make([]string, 0, len(failedSections))
		for _, section := range []string{"accounts", "timeseries", "models", "cache"} {
			if failedSections[section] {
				unavailableSections = append(unavailableSections, section)
			}
		}
		body["unavailable_sections"] = unavailableSections
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
