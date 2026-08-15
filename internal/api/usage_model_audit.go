package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// adminUsageModelAudit exposes the requested→resolved→actual model chain. It is
// aggregate-only and therefore safe for long retention and diagnostic review.
func (s *Server) adminUsageModelAudit(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	now := time.Now()
	win, err := s.resolveAdminUsageWindow(r, now, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	mismatchOnly := false
	if raw := strings.TrimSpace(r.URL.Query().Get("mismatch")); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "only":
			mismatchOnly = true
		case "0", "false", "no", "all":
		default:
			writeError(w, http.StatusBadRequest, errors.New("mismatch must be true or false"))
			return
		}
	}
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, errors.New("limit must be between 1 and 1000"))
			return
		}
		limit = parsed
	}
	completeness, err := s.currentUsageCompleteness(r.Context(), now.Unix())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	stableUntil := win.storageUntilAt()
	if completeness.UsageCompleteThroughAt < stableUntil {
		stableUntil = completeness.UsageCompleteThroughAt
	}
	summary, err := s.store.ModelAuditWindow(r.Context(), win.EffectiveStartAt, stableUntil, mismatchOnly, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	payload := map[string]interface{}{
		"rows": summary.Rows, "requests": summary.Requests, "mismatches": summary.Mismatches,
		"actual_model_unavailable": summary.ActualModelUnavailable, "mismatch_only": mismatchOnly,
	}
	writeJSON(w, http.StatusOK, mergeCompletenessFields(mergeWindowFields(payload, win), completeness))
}
