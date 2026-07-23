package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registration"
	"codex-account-pool/internal/storage"
)

// ── Email Registration API ──────────────────────────────────────────

// emailRegSettings is stored in the settings table under key "email_registration".
type emailRegSettings struct {
	Count        int    `json:"count"`
	GroupName    string `json:"group_name"`
	EgressPoolID string `json:"egress_pool_id"`
	Concurrency  int    `json:"concurrency"`
}

// handleEmailRegConfig handles GET and POST for /admin/register/email/config
func (s *Server) handleEmailRegConfig(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		stored, _, _ := s.store.GetSetting(r.Context(), "email_registration")
		if stored == "" {
			writeJSON(w, http.StatusOK, emailRegSettings{
				Count:       1,
				Concurrency: 2,
				GroupName:   s.cfg.EmailRegistrationGroup,
			})
			return
		}
		var settings emailRegSettings
		if err := json.Unmarshal([]byte(stored), &settings); err != nil {
			writeJSON(w, http.StatusOK, emailRegSettings{
				Count:       1,
				Concurrency: 2,
				GroupName:   s.cfg.EmailRegistrationGroup,
			})
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case http.MethodPost:
		var settings emailRegSettings
		if err := decodeJSONRequestBody(r.Body, &settings, adminJSONBodyLimit); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if settings.Count < 1 {
			settings.Count = 1
		}
		if settings.Concurrency < 1 {
			settings.Concurrency = 2
		}
		if strings.TrimSpace(settings.GroupName) == "" {
			settings.GroupName = s.cfg.EmailRegistrationGroup
		}
		raw, _ := json.Marshal(settings)
		if err := s.store.SetSetting(r.Context(), "email_registration", string(raw)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, settings)
	default:
		methodNotAllowed(w)
	}
}

// handleEmailRegStart handles POST /admin/register/email/start
func (s *Server) handleEmailRegStart(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}

	var req emailRegSettings
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Count < 1 {
		req.Count = 1
	}
	if req.Concurrency < 1 {
		req.Concurrency = 2
	}
	if strings.TrimSpace(req.GroupName) == "" {
		req.GroupName = s.cfg.EmailRegistrationGroup
	}

	// Check available emails
	idleAccounts, _, err := s.store.ListEmailAccounts(r.Context(), 1, 1, "", "idle")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if len(idleAccounts) == 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no idle email accounts available in the pool. Please import email accounts first"))
		return
	}

	if s.emailReg == nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("email registration orchestrator not initialized"))
		return
	}

	job, err := s.emailReg.StartJob(r.Context(), req.Count, req.GroupName, req.EgressPoolID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id":           job.ID,
		"status":           job.Status,
		"count":            job.Total,
		"group_name":       job.GroupName,
		"egress_pool_id":   job.EgressPoolID,
		"available_emails": len(idleAccounts),
	})
}

// handleEmailRegJobs handles GET /admin/register/email/jobs
func (s *Server) handleEmailRegJobs(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := s.store.DB().QueryContext(r.Context(),
		`SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error, created_at, updated_at
		 FROM registration_jobs WHERE method = 'email' ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type jobRow struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at,omitempty"`
		CompletedAt int64  `json:"completed_at,omitempty"`
		Error       string `json:"error,omitempty"`
		CreatedAt   int64  `json:"created_at"`
		UpdatedAt   int64  `json:"updated_at"`
	}

	var jobs []jobRow
	for rows.Next() {
		var j jobRow
		if err := rows.Scan(&j.ID, &j.Platform, &j.Method, &j.Total, &j.Succeeded, &j.Failed,
			&j.Status, &j.StartedAt, &j.CompletedAt, &j.Error, &j.CreatedAt, &j.UpdatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		jobs = append(jobs, j)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jobs": jobs,
	})
}

// handleEmailRegJobStatus handles GET /admin/register/email/job/status?id=...
func (s *Server) handleEmailRegJobStatus(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id parameter is required"))
		return
	}

	var j struct {
		ID          string `json:"id"`
		Platform    string `json:"platform"`
		Method      string `json:"method"`
		Total       int    `json:"total"`
		Succeeded   int    `json:"succeeded"`
		Failed      int    `json:"failed"`
		Status      string `json:"status"`
		StartedAt   int64  `json:"started_at,omitempty"`
		CompletedAt int64  `json:"completed_at,omitempty"`
		Error       string `json:"error,omitempty"`
		CreatedAt   int64  `json:"created_at"`
		UpdatedAt   int64  `json:"updated_at"`
	}
	err := s.store.DB().QueryRowContext(r.Context(),
		`SELECT id, platform, method, total, succeeded, failed, status, started_at, completed_at, error, created_at, updated_at
		 FROM registration_jobs WHERE id = ?`, jobID,
	).Scan(&j.ID, &j.Platform, &j.Method, &j.Total, &j.Succeeded, &j.Failed,
		&j.Status, &j.StartedAt, &j.CompletedAt, &j.Error, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// handleEmailRegJobEvents handles GET /admin/register/email/job/events?id=...
func (s *Server) handleEmailRegJobEvents(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id parameter is required"))
		return
	}

	rows, err := s.store.DB().QueryContext(r.Context(),
		`SELECT id, task_id, level, message, detail_json, created_at
		 FROM registration_task_events WHERE task_id = ? ORDER BY id ASC`, jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()

	type eventRow struct {
		ID        int64  `json:"id"`
		TaskID    string `json:"task_id"`
		Level     string `json:"level"`
		Message   string `json:"message"`
		Detail    string `json:"detail_json,omitempty"`
		CreatedAt int64  `json:"created_at"`
	}

	var events []eventRow
	for rows.Next() {
		var e eventRow
		if err := rows.Scan(&e.ID, &e.TaskID, &e.Level, &e.Message, &e.Detail, &e.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		events = append(events, e)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id": jobID,
		"events": events,
	})
}

// handleEmailRegJobAction handles POST /admin/register/email/job/{id}
func (s *Server) handleEmailRegJobAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	// Path: /admin/register/email/job/{id}
	jobID := strings.TrimPrefix(r.URL.Path, "/admin/register/email/job/")
	jobID = strings.TrimSuffix(jobID, "/")
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("job id is required"))
		return
	}
	switch r.Method {
	case http.MethodPost:
		now := storage.Now()
		_, err := s.store.DB().ExecContext(r.Context(),
			`UPDATE registration_jobs SET status = 'cancelled', updated_at = ? WHERE id = ? AND status = 'running'`,
			now, jobID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"job_id": jobID,
			"status": "cancelled",
		})
	default:
		methodNotAllowed(w)
	}
}

// handleEmailRegJobEventsSSE streams registration events as SSE.
func (s *Server) handleEmailRegJobEventsSSE(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	jobID := strings.TrimSpace(r.URL.Query().Get("id"))
	if jobID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id parameter is required"))
		return
	}
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
		return
	}

	var lastID int64
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		rows, err := s.store.DB().QueryContext(r.Context(),
			`SELECT id, task_id, level, message, detail_json, created_at
			 FROM registration_task_events WHERE task_id = ? AND id > ? ORDER BY id ASC`, jobID, lastID)
		if err != nil {
			return
		}
		hasData := false
		for rows.Next() {
			hasData = true
			var e struct {
				ID        int64  `json:"id"`
				TaskID    string `json:"task_id"`
				Level     string `json:"level"`
				Message   string `json:"message"`
				Detail    string `json:"detail_json"`
				CreatedAt int64  `json:"created_at"`
			}
			if err := rows.Scan(&e.ID, &e.TaskID, &e.Level, &e.Message, &e.Detail, &e.CreatedAt); err != nil {
				rows.Close()
				return
			}
			data, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", data)
			lastID = e.ID
		}
		rows.Close()
		if hasData {
			flusher.Flush()
		}
		// Check if job is done
		var status string
		if err := s.store.DB().QueryRowContext(r.Context(),
			`SELECT status FROM registration_jobs WHERE id = ?`, jobID).Scan(&status); err != nil {
			return
		}
		if status != "running" {
			fmt.Fprintf(w, "event: done\ndata: {\"status\":\"%s\"}\n\n", status)
			flusher.Flush()
			return
		}
	}
}

// newEmailRegOrchestrator creates the email registration orchestrator from config.
func newEmailRegOrchestrator(store *storage.Store, cfg *config.Config) *registration.EmailRegOrchestrator {
	return registration.NewEmailRegOrchestrator(store, registration.EmailRegConfig{
		Enabled:      cfg.EmailRegistrationEnabled,
		Concurrency:  cfg.EmailRegistrationConcurrency,
		TimeoutSecs:  cfg.EmailRegistrationTimeoutSeconds,
		DefaultGroup: cfg.EmailRegistrationGroup,
		EgressPoolID: cfg.EmailRegistrationEgressPoolID,
	})
}
