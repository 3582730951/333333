package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/storage"
)

const legacyEmailRegistrationSetting = "email_registration"

type legacyEmailRegistrationConfig struct {
	Count             int    `json:"count"`
	GroupName         string `json:"group_name"`
	EgressPoolID      string `json:"egress_pool_id"`
	Concurrency       int    `json:"concurrency"`
	Compatibility     string `json:"compatibility,omitempty"`
	CanonicalEndpoint string `json:"canonical_endpoint,omitempty"`
}

func (h *Handler) legacyEmailRegistrationSettings(ctx context.Context) (legacyEmailRegistrationConfig, bool) {
	if h == nil || h.store == nil {
		return legacyEmailRegistrationConfig{}, false
	}
	stored, ok, err := h.store.GetSetting(ctx, legacyEmailRegistrationSetting)
	if err != nil || !ok || strings.TrimSpace(stored) == "" {
		return legacyEmailRegistrationConfig{}, false
	}
	decoded, err := decodeLegacyEmailRegistrationConfig(strings.NewReader(stored))
	return decoded, err == nil
}

func (s *Server) handleEmailRegistrationCompatibility(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/register/email/"), "/")
	switch {
	case path == "config":
		s.handleLegacyEmailRegistrationConfig(w, r)
	case path == "start":
		s.handleLegacyEmailRegistrationStart(w, r)
	case path == "jobs":
		s.handleLegacyEmailRegistrationJobs(w, r)
	case path == "job/status":
		s.handleLegacyEmailRegistrationJobStatus(w, r)
	case path == "job/events":
		s.handleLegacyEmailRegistrationEvents(w, r)
	case path == "job/events/sse":
		s.handleLegacyEmailRegistrationEventsSSE(w, r)
	case strings.HasPrefix(path, "job/"):
		s.handleLegacyEmailRegistrationJobAction(w, r, strings.Trim(strings.TrimPrefix(path, "job/"), "/"))
	default:
		writeError(w, http.StatusNotFound, errors.New("email registration compatibility route not found"))
	}
}

func (s *Server) legacyEmailRegistrationConfig(ctx context.Context) legacyEmailRegistrationConfig {
	settings := legacyEmailRegistrationConfig{Count: 1, Concurrency: 2}
	if s != nil {
		settings.GroupName = firstNonEmpty(s.cfg.RegistrationDefaultGroup, s.cfg.DefaultGroup, config.DefaultGroupName)
		settings.EgressPoolID = strings.TrimSpace(s.cfg.RegistrationEgressPoolID)
		if s.cfg.RegistrationConcurrency > 0 {
			settings.Concurrency = s.cfg.RegistrationConcurrency
		}
	}
	if stored, ok, _ := s.store.GetSetting(ctx, legacyEmailRegistrationSetting); ok && strings.TrimSpace(stored) != "" {
		if decoded, err := decodeLegacyEmailRegistrationConfig(strings.NewReader(stored)); err == nil {
			if decoded.Count > 0 {
				settings.Count = decoded.Count
			}
			if decoded.Concurrency > 0 {
				settings.Concurrency = decoded.Concurrency
			}
			settings.GroupName = firstNonEmpty(decoded.GroupName, settings.GroupName)
			settings.EgressPoolID = firstNonEmpty(decoded.EgressPoolID, settings.EgressPoolID)
		}
	}
	// Current settings always win over the compatibility blob.
	settings.GroupName = s.firstSettingString(ctx, settings.GroupName, "registration_default_group", "reg_default_group")
	settings.EgressPoolID = s.firstSettingString(ctx, settings.EgressPoolID, "registration_egress_pool_id", "reg_default_egress")
	if raw := s.firstSettingString(ctx, "", "registration_concurrency"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			settings.Concurrency = value
		}
	}
	settings.Compatibility = "unified_registration"
	settings.CanonicalEndpoint = "/admin/register/batch"
	return settings
}

func (s *Server) firstSettingString(ctx context.Context, fallback string, keys ...string) string {
	for _, key := range keys {
		if value, ok, err := s.store.GetSetting(ctx, key); err == nil && ok {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return strings.TrimSpace(fallback)
}

func (s *Server) handleLegacyEmailRegistrationConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.legacyEmailRegistrationConfig(r.Context()))
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		settings, err := decodeLegacyEmailRegistrationConfig(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		defaults := s.legacyEmailRegistrationConfig(r.Context())
		if settings.Count < 1 {
			settings.Count = defaults.Count
		}
		if settings.Concurrency < 1 {
			settings.Concurrency = defaults.Concurrency
		}
		settings.GroupName = firstNonEmpty(settings.GroupName, defaults.GroupName, config.DefaultGroupName)
		settings.EgressPoolID = firstNonEmpty(settings.EgressPoolID, defaults.EgressPoolID)
		canonicalBlob := legacyEmailRegistrationConfig{
			Count: settings.Count, GroupName: settings.GroupName,
			EgressPoolID: settings.EgressPoolID, Concurrency: settings.Concurrency,
		}
		raw, _ := json.Marshal(canonicalBlob)
		if err := s.store.SetSettings(r.Context(), map[string]string{
			legacyEmailRegistrationSetting: string(raw),
			"registration_default_group":   settings.GroupName,
			"reg_default_group":            settings.GroupName,
			"registration_egress_pool_id":  settings.EgressPoolID,
			"reg_default_egress":           settings.EgressPoolID,
			"registration_concurrency":     strconv.Itoa(settings.Concurrency),
			"default_mailbox_provider":     "email_pool",
			"reg_default_mailbox":          "email_pool",
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		canonicalBlob.Compatibility = "unified_registration"
		canonicalBlob.CanonicalEndpoint = "/admin/register/batch"
		writeJSON(w, http.StatusOK, canonicalBlob)
	default:
		methodNotAllowed(w)
	}
}

func decodeLegacyEmailRegistrationConfig(reader io.Reader) (legacyEmailRegistrationConfig, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, adminJSONBodyLimit+1))
	if err != nil {
		return legacyEmailRegistrationConfig{}, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return legacyEmailRegistrationConfig{}, errors.New("email registration config exceeds request limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	values := map[string]interface{}{}
	if err := decoder.Decode(&values); err != nil {
		return legacyEmailRegistrationConfig{}, err
	}
	var out legacyEmailRegistrationConfig
	if out.Count, err = registrationInt(firstLegacyValue(values, "count", "total", "amount")); err != nil {
		return out, registrationFieldError("count", err)
	}
	if out.Concurrency, err = registrationInt(firstLegacyValue(values, "concurrency", "workers", "worker_count", "workerCount")); err != nil {
		return out, registrationFieldError("concurrency", err)
	}
	if out.GroupName, err = registrationString(firstLegacyValue(values, "group_name", "groupName", "group")); err != nil {
		return out, registrationFieldError("group_name", err)
	}
	if out.EgressPoolID, err = registrationString(firstLegacyValue(values,
		"egress_pool_id", "egressPoolId", "registration_egress_pool_id", "registration_pool_id", "registrationPoolId")); err != nil {
		return out, registrationFieldError("egress_pool_id", err)
	}
	return out, nil
}

func firstLegacyValue(values map[string]interface{}, keys ...string) interface{} {
	value, _ := firstRegistrationValue(values, keys...)
	return value
}

func (s *Server) handleLegacyEmailRegistrationStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.diskGuardPausesBackground() {
		writePublicServiceUnavailable(w)
		return
	}
	if s.regHandler == nil {
		writeError(w, http.StatusNotImplemented, errors.New("registration not initialized"))
		return
	}
	requested, err := decodeLegacyEmailRegistrationConfig(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defaults := s.legacyEmailRegistrationConfig(r.Context())
	if requested.Count < 1 {
		requested.Count = defaults.Count
	}
	requested.GroupName = firstNonEmpty(requested.GroupName, defaults.GroupName)
	requested.EgressPoolID = firstNonEmpty(requested.EgressPoolID, defaults.EgressPoolID)
	if requested.Concurrency > 0 {
		if err := s.store.SetSetting(r.Context(), "registration_concurrency", strconv.Itoa(requested.Concurrency)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	_, available, err := s.store.ListEmailAccounts(r.Context(), 1, 1, "", "idle")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if available == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no idle email accounts are available in the pool"))
		return
	}
	jobID, err := s.regHandler.StartJob(r.Context(), pipeline.RegisterRequest{
		Platform: "chatgpt", Method: "protocol_v2", Count: requested.Count,
		GroupName: requested.GroupName, RegistrationEgressPoolID: requested.EgressPoolID,
		IdentityMode: "email", MailboxProvider: "email_pool",
	})
	if err != nil {
		writeError(w, registrationStartHTTPStatus(err), err)
		return
	}
	w.Header().Set("Location", "/admin/register/jobs/"+jobID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id": jobID, "status": "queued", "count": requested.Count,
		"group_name": requested.GroupName, "egress_pool_id": requested.EgressPoolID,
		"available_emails": available, "compatibility": "unified_registration",
	})
}

func registrationStartHTTPStatus(err error) int {
	switch {
	case errors.Is(err, errPaymentFeatureRemoved):
		return http.StatusGone
	case errors.Is(err, errRegistrationDisabled):
		return http.StatusForbidden
	case errors.Is(err, errRegistrationCanaryRequired):
		return http.StatusConflict
	case errors.Is(err, errRegistrationNotReady):
		return http.StatusServiceUnavailable
	case errors.Is(err, errInvalidRegisterRequest):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

type legacyEmailRegistrationJob struct {
	ID              string `json:"id"`
	Platform        string `json:"platform"`
	Method          string `json:"method"`
	CanonicalMethod string `json:"canonical_method,omitempty"`
	Total           int    `json:"total"`
	Succeeded       int    `json:"succeeded"`
	Failed          int    `json:"failed"`
	Status          string `json:"status"`
	StartedAt       int64  `json:"started_at,omitempty"`
	CompletedAt     int64  `json:"completed_at,omitempty"`
	Error           string `json:"error,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

func scanLegacyEmailRegistrationJob(scan func(...interface{}) error) (legacyEmailRegistrationJob, error) {
	var job legacyEmailRegistrationJob
	var storedMethod string
	err := scan(&job.ID, &job.Platform, &storedMethod, &job.Total, &job.Succeeded, &job.Failed,
		&job.Status, &job.StartedAt, &job.CompletedAt, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if err == nil {
		job.Method = "email"
		job.CanonicalMethod = normalizeRegistrationMethodAlias(storedMethod)
		if job.CanonicalMethod == "email" {
			job.CanonicalMethod = "protocol_v2"
		}
	}
	return job, err
}

const legacyEmailRegistrationJobColumns = `id,COALESCE(platform,''),COALESCE(method,''),COALESCE(total,0),COALESCE(succeeded,0),COALESCE(failed,0),COALESCE(status,''),COALESCE(started_at,0),COALESCE(completed_at,0),COALESCE(error,''),COALESCE(created_at,0),COALESCE(updated_at,0)`

func (s *Server) handleLegacyEmailRegistrationJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit, _ := strconv.Atoi(firstNonEmpty(r.URL.Query().Get("limit"), r.URL.Query().Get("pageSize"), r.URL.Query().Get("page_size")))
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := `SELECT ` + legacyEmailRegistrationJobColumns + ` FROM registration_jobs WHERE method IN ('email','protocol_v2','browser_v3')`
	args := []interface{}{}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" && status != "all" {
		query += ` AND status=?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.store.DB().QueryContext(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer rows.Close()
	jobs := make([]legacyEmailRegistrationJob, 0)
	for rows.Next() {
		job, err := scanLegacyEmailRegistrationJob(rows.Scan)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": jobs, "compatibility": "unified_registration"})
}

func (s *Server) handleLegacyEmailRegistrationJobStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobID := firstNonEmpty(r.URL.Query().Get("id"), r.URL.Query().Get("job_id"), r.URL.Query().Get("jobId"))
	if jobID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id parameter is required"))
		return
	}
	job, err := scanLegacyEmailRegistrationJob(s.store.DB().QueryRowContext(r.Context(),
		`SELECT `+legacyEmailRegistrationJobColumns+` FROM registration_jobs WHERE id=?`, jobID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("registration job not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

type legacyEmailRegistrationEvent struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"task_id"`
	Level     string `json:"level"`
	Message   string `json:"message"`
	Detail    string `json:"detail_json,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (s *Server) legacyEmailRegistrationEvents(ctx context.Context, jobID string, afterID int64) ([]legacyEmailRegistrationEvent, error) {
	rows, err := s.store.DB().QueryContext(ctx, `SELECT id,task_id,level,message,COALESCE(detail_json,''),created_at
FROM registration_task_events WHERE task_id=? AND id>? ORDER BY id`, jobID, afterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]legacyEmailRegistrationEvent, 0)
	for rows.Next() {
		var event legacyEmailRegistrationEvent
		if err := rows.Scan(&event.ID, &event.TaskID, &event.Level, &event.Message, &event.Detail, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func legacyRegistrationJobID(r *http.Request) string {
	return firstNonEmpty(r.URL.Query().Get("id"), r.URL.Query().Get("job_id"), r.URL.Query().Get("jobId"))
}

func (s *Server) handleLegacyEmailRegistrationEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobID := legacyRegistrationJobID(r)
	if jobID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id parameter is required"))
		return
	}
	events, err := s.legacyEmailRegistrationEvents(r.Context(), jobID, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": jobID, "events": events})
}

func (s *Server) handleLegacyEmailRegistrationEventsSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	jobID := legacyRegistrationJobID(r)
	if jobID == "" {
		writeError(w, http.StatusBadRequest, errors.New("id parameter is required"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastID int64
	for {
		events, err := s.legacyEmailRegistrationEvents(r.Context(), jobID, lastID)
		if err != nil {
			writeRegistrationSSEError(w, err)
			flusher.Flush()
			return
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", data)
			lastID = event.ID
		}
		var status string
		if err := s.store.DB().QueryRowContext(r.Context(), `SELECT status FROM registration_jobs WHERE id=?`, jobID).Scan(&status); err != nil {
			writeRegistrationSSEError(w, err)
			flusher.Flush()
			return
		}
		if status != "queued" && status != "pending" && status != "running" {
			fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", status)
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) handleLegacyEmailRegistrationJobAction(w http.ResponseWriter, r *http.Request, jobID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	if jobID == "" || strings.Contains(jobID, "/") {
		writeError(w, http.StatusBadRequest, errors.New("job id is required"))
		return
	}
	if s.regHandler != nil {
		s.regHandler.mu.Lock()
		cancel := s.regHandler.jobCancels[jobID]
		s.regHandler.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	result, err := s.store.DB().ExecContext(r.Context(), `UPDATE registration_jobs SET status='cancelled',completed_at=?,updated_at=? WHERE id=? AND status IN ('queued','pending','running')`, storage.Now(), storage.Now(), jobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		var status string
		if err := s.store.DB().QueryRowContext(r.Context(), `SELECT status FROM registration_jobs WHERE id=?`, jobID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("registration job not found"))
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeError(w, http.StatusConflict, fmt.Errorf("registration job is %s", status))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"job_id": jobID, "status": "cancelled", "cancelled": true})
}
