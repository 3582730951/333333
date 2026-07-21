package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// adminGoals deliberately exposes only lifecycle and occupancy metadata.  Checkpoint
// bodies, tool results, summaries, aliases and credentials stay in encrypted storage
// and never pass through this handler or diagnostics exports.
func (s *Server) adminGoals(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	goals, err := s.store.ListGoalSessions(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var payloadBytes int64
	for _, goal := range goals {
		if detail, detailErr := s.store.GetGoalDetail(r.Context(), goal.ID); detailErr == nil {
			payloadBytes += detail.PayloadBytes
		}
	}
	metrics, metricsErr := s.store.GoalContinuityMetrics(r.Context())
	if metricsErr != nil {
		writeError(w, http.StatusInternalServerError, metricsErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"engine":                   "v2",
		"goals":                    goals,
		"listed":                   len(goals),
		"listed_payload_bytes":     payloadBytes,
		"metrics":                  metrics,
		"retention_days":           s.settingInt(r.Context(), "goal_retention_days", s.cfg.GoalRetentionDays),
		"storage_max_mb":           s.settingInt(r.Context(), "goal_storage_max_mb", s.cfg.GoalStorageMaxMB),
		"continuity_enabled":       s.goalContinuityEnabled(r.Context()),
		"plaintext_fields_exposed": false,
	})
}

// adminGoalAction handles GET /admin/goals/{id} and DELETE
// /admin/goals/{id}/cleanup.  Cleanup is intentionally refused for a live run or an
// unresolved tool wait; an operator cannot accidentally evict active context.
func (s *Server) adminGoalAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/goals/"), "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		writeError(w, http.StatusNotFound, errors.New("goal id required"))
		return
	}
	id := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		detail, err := s.store.GetGoalDetail(r.Context(), id)
		if err != nil {
			if errors.Is(err, storage.ErrGoalNotFound) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"engine": "v2", "goal": detail, "plaintext_fields_exposed": false})
		return
	}
	if len(parts) == 2 && parts[1] == "cleanup" && r.Method == http.MethodDelete {
		if err := s.store.DeleteGoalSafely(r.Context(), id); err != nil {
			switch {
			case errors.Is(err, storage.ErrGoalNotFound):
				writeError(w, http.StatusNotFound, err)
			case errors.Is(err, storage.ErrGoalActiveCannotBePurged):
				writePoolCodeError(w, http.StatusConflict, "goal_cleanup_active_forbidden", "Active goals and pending tool results cannot be removed.")
			default:
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "goal_id": id, "engine": "v2"})
		return
	}
	methodNotAllowed(w)
}
