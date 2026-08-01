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

	"codex-account-pool/internal/registration/pipeline"
	"codex-account-pool/internal/storage"

	"github.com/google/uuid"
)

type legacyTurboCreateRequest struct {
	FullName             string                 `json:"full_name,omitempty"`
	BirthDate            string                 `json:"birth_date,omitempty"`
	PhoneCountryCode     string                 `json:"phone_country_code,omitempty"`
	PhoneCountryDialCode string                 `json:"phone_country_dial_code,omitempty"`
	MailDomain           string                 `json:"mail_domain,omitempty"`
	AutoImport           bool                   `json:"auto_import"`
	Start                bool                   `json:"start"`
	Config               map[string]interface{} `json:"config,omitempty"`
}

func (s *Server) handleTurboRegistrationCompatibility(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/turbo-gpt-register/"), "/")
	switch {
	case path == "jobs":
		s.handleLegacyTurboJobs(w, r)
	case strings.HasPrefix(path, "jobs/"):
		s.handleLegacyTurboJobAction(w, r, strings.Trim(strings.TrimPrefix(path, "jobs/"), "/"))
	case path == "config":
		s.handleLegacyTurboConfig(w, r)
	default:
		writeError(w, http.StatusNotFound, errors.New("turbo registration compatibility route not found"))
	}
}

func (s *Server) handleLegacyTurboJobs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit, _ := strconv.Atoi(firstNonEmpty(r.URL.Query().Get("limit"), r.URL.Query().Get("pageSize"), r.URL.Query().Get("page_size")))
		jobs, err := s.store.ListTurboGPTRegisterJobs(r.Context(), strings.TrimSpace(r.URL.Query().Get("status")), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for index := range jobs {
			jobs[index] = s.syncLegacyTurboJob(r.Context(), jobs[index])
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"jobs": jobs, "compatibility": "unified_registration", "canonical_endpoint": "/admin/register/batch",
		})
	case http.MethodPost:
		request, err := decodeLegacyTurboCreateRequest(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		now := storage.Now()
		configJSON, _ := json.Marshal(canonicalizeAutomationConfig(request.Config))
		job := storage.TurboGPTRegisterJob{
			ID:     "tgr_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			Status: "pending", Phase: "unified", FullName: strings.TrimSpace(request.FullName),
			BirthDate: strings.TrimSpace(request.BirthDate), PhoneCountryCode: strings.TrimSpace(request.PhoneCountryCode),
			PhoneCountryDialCode: strings.TrimSpace(request.PhoneCountryDialCode), MailDomain: strings.TrimSpace(request.MailDomain),
			ConfigJSON: string(configJSON), ResultJSON: "{}", AutoImport: request.AutoImport,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.store.CreateTurboGPTRegisterJob(r.Context(), job); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if request.Start {
			job, err = s.startLegacyTurboJob(r.Context(), job)
			if err != nil {
				writeError(w, registrationStartHTTPStatus(err), err)
				return
			}
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		methodNotAllowed(w)
	}
}

func decodeLegacyTurboCreateRequest(reader io.Reader) (legacyTurboCreateRequest, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, adminJSONBodyLimit+1))
	if err != nil {
		return legacyTurboCreateRequest{}, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return legacyTurboCreateRequest{}, errors.New("turbo registration request exceeds request limit")
	}
	var request legacyTurboCreateRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, err
	}
	// Accept camel-case rows from early SPA builds without expanding the durable
	// representation; current snake_case values retain precedence.
	var aliases map[string]interface{}
	_ = json.Unmarshal(raw, &aliases)
	if request.FullName == "" {
		request.FullName, _ = registrationString(firstLegacyValue(aliases, "fullName", "name"))
	}
	if request.BirthDate == "" {
		request.BirthDate, _ = registrationString(firstLegacyValue(aliases, "birthDate"))
	}
	if request.PhoneCountryCode == "" {
		request.PhoneCountryCode, _ = registrationString(firstLegacyValue(aliases, "phoneCountryCode", "country"))
	}
	if request.PhoneCountryDialCode == "" {
		request.PhoneCountryDialCode, _ = registrationString(firstLegacyValue(aliases, "phoneCountryDialCode", "dialCode"))
	}
	if request.MailDomain == "" {
		request.MailDomain, _ = registrationString(firstLegacyValue(aliases, "mailDomain", "mailbox_domain"))
	}
	return request, nil
}

func (s *Server) legacyTurboRegisterRequest(job storage.TurboGPTRegisterJob) pipeline.RegisterRequest {
	configMap := map[string]interface{}{}
	_ = json.Unmarshal([]byte(firstNonEmpty(job.ConfigJSON, "{}")), &configMap)
	configMap = canonicalizeAutomationConfig(configMap)
	method := normalizeRegistrationMethodAlias(strFromConfig(configMap, "register_method", "browser_v3"))
	count := intFromConfig(configMap, "count", 1)
	if count < 1 {
		count = 1
	}
	return pipeline.RegisterRequest{
		Platform: strFromConfig(configMap, "platform", "chatgpt"), Method: method, Count: count,
		GroupName: strFromConfig(configMap, "group", ""), EgressID: strFromConfig(configMap, "egress_id", ""),
		RegistrationEgressPoolID: strFromConfig(configMap, "registration_egress_pool_id", ""),
		IdentityMode:             strFromConfig(configMap, "identity_mode", "email"), Country: firstNonEmpty(strFromConfig(configMap, "country", ""), job.PhoneCountryCode),
		SMSProvider: strFromConfig(configMap, "sms_provider", ""), MailboxProvider: strFromConfig(configMap, "mailbox_provider", ""),
		MailboxDomain: firstNonEmpty(strFromConfig(configMap, "mailbox_domain", ""), job.MailDomain),
		CaptchaSolver: strFromConfig(configMap, "captcha_solver", ""),
	}
}

func (s *Server) startLegacyTurboJob(ctx context.Context, job storage.TurboGPTRegisterJob) (storage.TurboGPTRegisterJob, error) {
	if s.regHandler == nil {
		return job, errRegistrationNotReady
	}
	job = s.syncLegacyTurboJob(ctx, job)
	if job.Status == "running" {
		return job, errors.New("turbo registration job is already running")
	}
	if job.Status == "completed" {
		return job, errors.New("completed turbo registration job cannot be restarted")
	}
	unifiedJobID, err := s.regHandler.StartJob(ctx, s.legacyTurboRegisterRequest(job))
	if err != nil {
		job.Status = "failed"
		job.Error = registrationErrorClass(err)
		job.Attempts++
		_ = s.store.UpdateTurboGPTRegisterJob(context.WithoutCancel(ctx), job)
		return job, err
	}
	mapping, _ := json.Marshal(map[string]interface{}{"unified_job_id": unifiedJobID, "compatibility": "unified_registration"})
	job.Status = "running"
	job.Phase = "unified"
	job.ResultJSON = string(mapping)
	job.Error = ""
	job.Attempts++
	if job.StartedAt == 0 {
		job.StartedAt = storage.Now()
	}
	if err := s.store.UpdateTurboGPTRegisterJob(ctx, job); err != nil {
		return job, err
	}
	return job, nil
}

func legacyTurboUnifiedJobID(job storage.TurboGPTRegisterJob) string {
	var result map[string]interface{}
	if json.Unmarshal([]byte(job.ResultJSON), &result) != nil {
		return ""
	}
	value, _ := result["unified_job_id"].(string)
	return strings.TrimSpace(value)
}

func (s *Server) syncLegacyTurboJob(ctx context.Context, job storage.TurboGPTRegisterJob) storage.TurboGPTRegisterJob {
	unifiedJobID := legacyTurboUnifiedJobID(job)
	if unifiedJobID == "" {
		return job
	}
	var status, errorClass string
	var startedAt, completedAt int64
	err := s.store.DB().QueryRowContext(ctx, `SELECT status,COALESCE(started_at,0),COALESCE(completed_at,0),COALESCE(error,'') FROM registration_jobs WHERE id=?`, unifiedJobID).
		Scan(&status, &startedAt, &completedAt, &errorClass)
	if err != nil {
		return job
	}
	switch status {
	case "queued", "pending", "running":
		job.Status = "running"
	case "completed", "completed_with_review":
		job.Status = "completed"
		job.Phase = "completed"
	case "cancelled":
		job.Status = "cancelled"
	default:
		job.Status = "failed"
	}
	job.StartedAt = startedAt
	job.CompletedAt = completedAt
	job.Error = errorClass
	if job.Status == "completed" {
		var accountID string
		if err := s.store.DB().QueryRowContext(ctx, `SELECT COALESCE(account_id,'') FROM registration_records WHERE job_id=? AND status='success' ORDER BY created_at DESC LIMIT 1`, unifiedJobID).Scan(&accountID); err == nil && accountID != "" {
			job.ImportedAccountID = accountID
			if account, err := s.store.GetAccount(ctx, accountID); err == nil {
				job.Email = account.Email
			}
			if token, err := s.store.GetToken(ctx, accountID); err == nil {
				_ = s.store.UpsertTurboGPTRegisterToken(ctx, storage.TurboGPTRegisterToken{
					JobID: job.ID, Email: job.Email, AccessToken: token.AccessToken,
					RefreshToken: token.RefreshToken, IDToken: token.IDTokenRaw,
					AccountID: accountID, ExpiresAt: token.ExpiresAt,
				})
			}
		}
	}
	_ = s.store.UpdateTurboGPTRegisterJob(context.WithoutCancel(ctx), job)
	return job
}

func (s *Server) handleLegacyTurboJobAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) < 1 || strings.TrimSpace(parts[0]) == "" || len(parts) > 2 {
		writeError(w, http.StatusNotFound, errors.New("turbo registration job not found"))
		return
	}
	jobID := strings.TrimSpace(parts[0])
	job, err := s.store.GetTurboGPTRegisterJob(r.Context(), jobID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("turbo registration job not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	job = s.syncLegacyTurboJob(r.Context(), job)
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, job)
		case http.MethodDelete:
			if job.Status == "running" {
				writeError(w, http.StatusConflict, errors.New("turbo registration job is running"))
				return
			}
			if err := s.store.DeleteTurboGPTRegisterJob(r.Context(), jobID); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"deleted": jobID})
		default:
			methodNotAllowed(w)
		}
		return
	}
	switch parts[1] {
	case "advance", "retry", "start":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if job.Status == "completed" {
			writeError(w, http.StatusConflict, errors.New("completed turbo registration job cannot be retried"))
			return
		}
		if job.Status == "running" {
			writeError(w, http.StatusConflict, errors.New("turbo registration job is already running"))
			return
		}
		job, err = s.startLegacyTurboJob(r.Context(), job)
		if err != nil {
			writeError(w, registrationStartHTTPStatus(err), err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	case "token":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		token, err := s.store.GetTurboGPTRegisterToken(r.Context(), jobID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("turbo registration token not found"))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, token)
	default:
		writeError(w, http.StatusNotFound, errors.New("turbo registration action not found"))
	}
}

func (s *Server) handleLegacyTurboConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		values, err := s.store.GetTurboGPTRegisterConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		for key, settingKey := range map[string]string{
			"registration_enabled": "registration_enabled", "concurrency": "registration_concurrency",
			"timeout": "registration_timeout", "group_name": "registration_default_group",
			"egress_pool_id": "registration_egress_pool_id", "register_method": "default_register_method",
			"mailbox_provider": "default_mailbox_provider", "sms_provider": "default_sms_provider",
			"captcha_provider": "default_captcha_provider",
		} {
			if value, ok, _ := s.store.GetSetting(r.Context(), settingKey); ok {
				values[key] = value
			}
		}
		values["compatibility"] = "unified_registration"
		values["canonical_endpoint"] = "/admin/register/batch"
		writeJSON(w, http.StatusOK, values)
	case http.MethodPut, http.MethodPatch, http.MethodPost:
		values, err := decodeLegacyTurboConfig(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		canonical := canonicalTurboSettings(values)
		if len(canonical) > 0 {
			if err := s.store.SetSettings(r.Context(), canonical); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		for key, value := range values {
			if err := s.store.SetTurboGPTRegisterConfig(r.Context(), key, value); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, values)
	default:
		methodNotAllowed(w)
	}
}

func decodeLegacyTurboConfig(reader io.Reader) (map[string]string, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, adminJSONBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > adminJSONBodyLimit {
		return nil, errors.New("turbo registration config exceeds request limit")
	}
	var source map[string]interface{}
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		switch typed := value.(type) {
		case string:
			out[key] = strings.TrimSpace(typed)
		case float64:
			out[key] = strconv.FormatFloat(typed, 'f', -1, 64)
		case bool:
			out[key] = strconv.FormatBool(typed)
		default:
			return nil, fmt.Errorf("turbo config %s must be a scalar value", key)
		}
	}
	return out, nil
}

func canonicalTurboSettings(values map[string]string) map[string]string {
	out := map[string]string{}
	aliases := map[string][]string{
		"registration_enabled":        {"registration_enabled", "enabled", "email_registration_enabled"},
		"registration_concurrency":    {"registration_concurrency", "concurrency", "workers"},
		"registration_timeout":        {"registration_timeout", "timeout", "timeout_seconds"},
		"registration_default_group":  {"registration_default_group", "group_name", "group"},
		"registration_egress_pool_id": {"registration_egress_pool_id", "egress_pool_id", "registration_pool_id"},
		"default_register_method":     {"default_register_method", "register_method", "registration_method", "engine"},
		"default_mailbox_provider":    {"default_mailbox_provider", "mailbox_provider", "mail_provider", "email_provider"},
		"default_sms_provider":        {"default_sms_provider", "sms_provider", "sms_platform"},
		"default_captcha_provider":    {"default_captcha_provider", "captcha_provider", "captcha_solver"},
	}
	for canonical, keys := range aliases {
		for _, key := range keys {
			if value, ok := values[key]; ok {
				if canonical == "default_register_method" {
					value = normalizeRegistrationMethodAlias(value)
				} else if canonical == "default_mailbox_provider" {
					value = normalizeMailboxProviderAlias(value)
				}
				out[canonical] = strings.TrimSpace(value)
				break
			}
		}
	}
	return out
}
