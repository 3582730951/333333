package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

// adminUsageByModel returns usage aggregated per model since `since` (default last 7d),
// so the admin UI can render the per-model cache-hit-rate (cache read / cache input)
// view with a distinct color per model. Admin-gated, GET only.
func (s *Server) adminUsageByModel(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	completeness, err := s.currentUsageCompleteness(r.Context(), now.Unix())
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
	stableUntil := win.storageUntilAt()
	if completeness.UsageCompleteThroughAt < stableUntil {
		stableUntil = completeness.UsageCompleteThroughAt
	}
	dimension := strings.TrimSpace(r.URL.Query().Get("dimension"))
	if dimension == "" {
		dimension = "model"
	}
	if dimension != "model" && dimension != "provider_model" {
		writeError(w, http.StatusBadRequest, errors.New("dimension must be model or provider_model"))
		return
	}
	var models interface{}
	if dimension == "provider_model" {
		models, err = s.store.UsageByProviderModelWindow(r.Context(), win.EffectiveStartAt, stableUntil)
	} else {
		models, err = s.store.UsageByModelWindow(r.Context(), win.EffectiveStartAt, stableUntil)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if models == nil {
		if dimension == "provider_model" {
			models = []storage.ProviderModelUsageRow{}
		} else {
			models = []storage.UserUsageRow{}
		}
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(map[string]interface{}{"since": win.EffectiveStartAt, "now": win.EffectiveUntilAt, "dimension": dimension, "models": models}, win), completeness))
}

// adminUsageCache returns cache-hit diagnostics since `since` (default last 24h).
// It exposes request hit-rate, token hit-rate, and estimated-usage share across
// several dimensions without leaking full downstream API-key hashes.
func (s *Server) adminUsageCache(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	completeness, err := s.currentUsageCompleteness(r.Context(), now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	bucket := int64(3600)
	if v := r.URL.Query().Get("bucket"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			bucket = n
		}
	}
	fields, err := parseCacheUsageFields(r.URL.Query().Get("fields"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report, err := s.store.CacheUsageMetricsWindowFields(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), 200, fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	includeAll := len(fields) == 0
	includeField := func(name string) bool { return includeAll || fields[name] }
	if bucket != 3600 && includeField("by_time_bucket") {
		if buckets, berr := s.store.CacheUsageBucketsWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket); berr == nil {
			report.ByTimeBucket = buckets
		} else {
			writeError(w, http.StatusInternalServerError, berr)
			return
		}
	}
	markCacheBucketsPartial(report.ByTimeBucket, bucket, completeness.UsageCompleteThroughAt)
	stableUntil := win.storageUntilAt()
	if completeness.UsageCompleteThroughAt < stableUntil {
		stableUntil = completeness.UsageCompleteThroughAt
	}
	stableSummary, err := s.store.CacheUsageMetricsWindowFields(r.Context(), win.EffectiveStartAt, stableUntil, 0, map[string]bool{"summary": true})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	body := map[string]interface{}{
		"since": win.EffectiveStartAt,
		"now":   win.EffectiveUntilAt,
	}
	if includeField("summary") {
		body["summary"] = report.Summary
		body["stable_summary"] = stableSummary.Summary
	}
	if includeField("by_account") {
		body["by_account"] = nonNilCacheUsageRows(report.ByAccount)
	}
	if includeField("by_model") {
		body["by_model"] = nonNilCacheUsageRows(report.ByModel)
	}
	if includeField("by_api_key") {
		body["by_api_key"] = nonNilCacheUsageRows(report.ByAPIKey)
	}
	if includeField("by_account_model") {
		body["by_account_model"] = nonNilCacheUsageRows(report.ByAccountModel)
	}
	if includeField("by_provider") {
		body["by_provider"] = nonNilCacheUsageRows(report.ByProvider)
	}
	if includeField("by_provider_model") {
		body["by_provider_model"] = nonNilCacheUsageRows(report.ByProviderModel)
	}
	if includeField("by_route") {
		body["by_route"] = nonNilCacheUsageRows(report.ByRoute)
	}
	if includeField("by_route_account_model") {
		body["by_route_account_model"] = nonNilCacheUsageRows(report.ByRouteAccountModel)
	}
	if includeField("by_time_bucket") {
		body["by_time_bucket"] = nonNilCacheUsageBuckets(report.ByTimeBucket)
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(body, win), completeness))
}

func nonNilCacheUsageRows(rows []storage.CacheUsageMetricRow) []storage.CacheUsageMetricRow {
	if rows == nil {
		return []storage.CacheUsageMetricRow{}
	}
	return rows
}

func nonNilCacheUsageBuckets(rows []storage.CacheUsageBucket) []storage.CacheUsageBucket {
	if rows == nil {
		return []storage.CacheUsageBucket{}
	}
	return rows
}

func parseCacheUsageFields(raw string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	allowed := map[string]bool{
		"summary":                true,
		"by_account":             true,
		"by_model":               true,
		"by_api_key":             true,
		"by_account_model":       true,
		"by_provider":            true,
		"by_provider_model":      true,
		"by_route":               true,
		"by_route_account_model": true,
		"by_time_bucket":         true,
	}
	out := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		field := strings.TrimSpace(part)
		if field == "" {
			continue
		}
		if !allowed[field] {
			return nil, errors.New("unknown cache usage field: " + field)
		}
		out[field] = true
	}
	if len(out) == 0 {
		return nil, errors.New("fields must include at least one field")
	}
	return out, nil
}
