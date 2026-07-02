package api

import (
	"net/http"
	"strconv"
	"time"

	"codex-account-pool/internal/storage"
)

// adminUsageByModel returns usage aggregated per model since `since` (default last 7d),
// so the admin UI can render the per-model cache-hit-rate (cached/prompt) view with a
// distinct color per model. Admin-gated, GET only.
func (s *Server) adminUsageByModel(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now().Unix()
	since := now - 7*86400
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			since = n
		}
	}
	models, err := s.store.UsageByModel(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if models == nil {
		models = []storage.UserUsageRow{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"since": since, "now": now, "models": models})
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
	now := time.Now().Unix()
	since := now - 24*3600
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			since = n
		}
	}
	report, err := s.store.CacheUsageMetrics(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"since":            since,
		"now":              now,
		"summary":          report.Summary,
		"by_account":       report.ByAccount,
		"by_model":         report.ByModel,
		"by_api_key":       report.ByAPIKey,
		"by_account_model": report.ByAccountModel,
	})
}
