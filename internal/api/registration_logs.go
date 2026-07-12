// registration_logs.go — structured registration event logs query, export, and retention.
package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleJobLogs returns the structured event log for a registration job.
// GET /admin/register/job/{jobID}/logs
func (h *Handler) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/admin/register/job/")
	jobID = strings.TrimSuffix(jobID, "/logs")
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing job id"))
		return
	}

	level := strings.TrimSpace(r.URL.Query().Get("level"))
	limit := 500
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}

	query := `SELECT id, task_id, level, message, detail_json, created_at FROM registration_task_events WHERE task_id=?`
	args := []interface{}{jobID}
	if level != "" {
		query += ` AND level=?`
		args = append(args, level)
	}
	query += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)

	rows, err := h.store.DB().QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type event struct {
		ID         int64                  `json:"id"`
		TaskID     string                 `json:"task_id"`
		Level      string                 `json:"level"`
		Message    string                 `json:"message"`
		DetailJSON map[string]interface{} `json:"detail_json"`
		CreatedAt  int64                  `json:"created_at"`
	}
	events := []event{}
	for rows.Next() {
		var e event
		var detailRaw string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Level, &e.Message, &detailRaw, &e.CreatedAt); err != nil {
			continue
		}
		if detailRaw != "" && detailRaw != "{}" {
			_ = json.Unmarshal([]byte(detailRaw), &e.DetailJSON)
		}
		events = append(events, e)
	}

	// Also include the job row for context.
	jobRow := h.store.DB().QueryRowContext(r.Context(),
		`SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error, created_at FROM registration_jobs WHERE id=?`, jobID)
	var j struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at"`
		CompletedAt int64  `json:"completed_at"`
		Error       string `json:"error"`
		CreatedAt   int64  `json:"created_at"`
	}
	_ = jobRow.Scan(&j.ID, &j.Platform, &j.Method, &j.Total, &j.Succeeded, &j.Failed, &j.Status, &j.StartedAt, &j.CompletedAt, &j.Error, &j.CreatedAt)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job":    j,
		"events": events,
		"count":  len(events),
	})
}

// handleJobLogsExport returns a gzip-compressed JSON file of the full structured
// event log for a registration job, suitable for download and AI analysis.
// GET /admin/register/job/{jobID}/logs/export?format=gzip
func (h *Handler) handleJobLogsExport(w http.ResponseWriter, r *http.Request) {
	jobID := strings.TrimPrefix(r.URL.Path, "/admin/register/job/")
	jobID = strings.TrimSuffix(jobID, "/logs/export")
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("missing job id"))
		return
	}

	query := `SELECT id, task_id, level, message, detail_json, created_at FROM registration_task_events WHERE task_id=? ORDER BY id ASC`
	rows, err := h.store.DB().QueryContext(r.Context(), query, jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type event struct {
		ID         int64                  `json:"id"`
		TaskID     string                 `json:"task_id"`
		Level      string                 `json:"level"`
		Message    string                 `json:"message"`
		DetailJSON map[string]interface{} `json:"detail_json,omitempty"`
		CreatedAt  int64                  `json:"created_at"`
	}
	events := []event{}
	for rows.Next() {
		var e event
		var detailRaw string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Level, &e.Message, &detailRaw, &e.CreatedAt); err != nil {
			continue
		}
		if detailRaw != "" && detailRaw != "{}" {
			e.DetailJSON = map[string]interface{}{}
			_ = json.Unmarshal([]byte(detailRaw), &e.DetailJSON)
		}
		events = append(events, e)
	}

	// Also grab the job row.
	jobRow := h.store.DB().QueryRowContext(r.Context(),
		`SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error, created_at FROM registration_jobs WHERE id=?`, jobID)
	var j struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at"`
		CompletedAt int64  `json:"completed_at"`
		Error       string `json:"error"`
		CreatedAt   int64  `json:"created_at"`
	}
	_ = jobRow.Scan(&j.ID, &j.Platform, &j.Method, &j.Total, &j.Succeeded, &j.Failed, &j.Status, &j.StartedAt, &j.CompletedAt, &j.Error, &j.CreatedAt)

	export := map[string]interface{}{
		"job":         j,
		"events":      events,
		"count":       len(events),
		"exported_at": time.Now().Unix(),
	}

	filename := fmt.Sprintf("reg-logs-%s-%d.json.gz", jobID, time.Now().Unix())

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	gw := gzip.NewWriter(w)
	defer gw.Close()
	enc := json.NewEncoder(gw)
	enc.SetIndent("", "  ")
	_ = enc.Encode(export)
}

// LogRetentionCleanup is retained as a compatibility entry point for registration
// callers. The server-level retention loop now invokes the same unified policy for
// registration events and the other disposable observability tables.
func (h *Handler) LogRetentionCleanup(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	days := 7
	if v, ok := h.setting(ctx, "reg_log_retention_days"); ok {
		trimmed := strings.TrimSpace(v)
		if n, err := strconv.Atoi(trimmed); err == nil && n > 0 {
			days = n
		} else {
			logInvalidRegistrationSetting("reg_log_retention_days", trimmed, "positive integer")
		}
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	_, _ = h.store.PurgeLogRecordsBefore(ctx, cutoff, logCleanupBatchSize)
}
