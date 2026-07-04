package api

import (
	"net/http"
	"strconv"
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
	if err := s.store.EnsureUsageDailyResetAudit(r.Context(), now); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	win, err := s.resolveAdminUsageWindow(r, now, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	models, err := s.store.UsageByModelWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if models == nil {
		models = []storage.UserUsageRow{}
	}
	writeJSON(w, http.StatusOK, mergeWindowFields(map[string]interface{}{"since": win.EffectiveStartAt, "now": win.EffectiveUntilAt, "models": models}, win))
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
	report, err := s.store.CacheUsageMetricsWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if bucket != 3600 {
		if buckets, berr := s.store.CacheUsageBucketsWindow(r.Context(), win.EffectiveStartAt, win.storageUntilAt(), bucket); berr == nil {
			report.ByTimeBucket = buckets
		} else {
			writeError(w, http.StatusInternalServerError, berr)
			return
		}
	}
	writeJSON(w, http.StatusOK, mergeWindowFields(map[string]interface{}{
		"since":                  win.EffectiveStartAt,
		"now":                    win.EffectiveUntilAt,
		"summary":                report.Summary,
		"by_account":             report.ByAccount,
		"by_model":               report.ByModel,
		"by_api_key":             report.ByAPIKey,
		"by_account_model":       report.ByAccountModel,
		"by_route":               report.ByRoute,
		"by_route_account_model": report.ByRouteAccountModel,
		"by_time_bucket":         report.ByTimeBucket,
	}, win))
}
