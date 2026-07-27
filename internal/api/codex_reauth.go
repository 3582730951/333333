package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
)

type codexReauthConfigRequest struct {
	LoginEmail        string `json:"login_email"`
	Password          string `json:"password"`
	OTPURL            string `json:"otp_url"`
	TargetWorkspaceID string `json:"target_workspace_id"`
	AutoEnabled       bool   `json:"auto_enabled"`
}

type codexReauthWorkerRequest struct {
	Email             string `json:"email"`
	Password          string `json:"password,omitempty"`
	OTPURL            string `json:"otp_url,omitempty"`
	Proxy             string `json:"proxy,omitempty"`
	TargetWorkspaceID string `json:"target_workspace_id,omitempty"`
	CookieHeader      string `json:"cookie_header,omitempty"`
}

type codexReauthWorkerResponse struct {
	Status        string `json:"status"`
	Code          string `json:"code"`
	Error         string `json:"error"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	IDToken       string `json:"id_token"`
	SessionCookie string `json:"session_cookie"`
	Email         string `json:"email"`
	UserID        string `json:"user_id"`
	WorkspaceID   string `json:"workspace_id"`
	PlanType      string `json:"plan_type"`
}

func (s *Server) adminCodexReauthConfig(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	account, ok, err := s.codexReauthEligibleAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts"))
		return
	}
	var req codexReauthConfigRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	existing, hadExisting, _ := s.store.GetCodexReauthConfig(r.Context(), account.ID)
	password := req.Password
	if password == "" && hadExisting {
		password = existing.Password
	}
	otpURL := strings.TrimSpace(req.OTPURL)
	if otpURL == "" && hadExisting {
		otpURL = existing.OTPURL
	}
	cfg := storage.AccountCodexReauthConfig{
		AccountID:         account.ID,
		LoginEmail:        strings.TrimSpace(req.LoginEmail),
		Password:          password,
		OTPURL:            otpURL,
		TargetWorkspaceID: strings.TrimSpace(req.TargetWorkspaceID),
		AutoEnabled:       req.AutoEnabled,
		LastStatus:        "configured",
	}
	if cfg.TargetWorkspaceID == "" {
		cfg.TargetWorkspaceID = strings.TrimSpace(account.UpstreamAccountID)
	}
	if err := s.store.UpsertCodexReauthConfig(r.Context(), cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	pub, _, err := s.store.GetCodexReauthConfigPublic(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, pub)
}

func (s *Server) adminCodexReauthStatus(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	account, ok, err := s.codexReauthEligibleAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts"))
		return
	}
	cfg, configured, err := s.store.GetCodexReauthConfigPublic(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	latest, hasLatest, err := s.store.LatestCodexReauthJob(r.Context(), account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	jobs, err := s.store.ListCodexReauthJobs(r.Context(), account.ID, 10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]interface{}{
		"account_id": account.ID,
		"configured": configured,
		"config":     cfg,
		"jobs":       jobs,
	}
	if hasLatest {
		out["latest_job"] = latest
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) adminCodexReauthRun(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, ok, err := s.codexReauthEligibleAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts"))
		return
	}
	job, _, err := s.store.EnqueueCodexReauthJob(r.Context(), account.ID, "manual_run")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp, status, err := s.runCodexReauthJob(r.Context(), job.ID)
	if err != nil {
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) adminCodexReauthOAuthStart(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, ok, err := s.codexReauthEligibleAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts"))
		return
	}
	var req struct {
		TargetWorkspaceID string `json:"target_workspace_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	targetWorkspaceID := strings.TrimSpace(req.TargetWorkspaceID)
	if targetWorkspaceID == "" {
		if cfg, found, _ := s.store.GetCodexReauthConfig(r.Context(), account.ID); found {
			targetWorkspaceID = strings.TrimSpace(cfg.TargetWorkspaceID)
		}
	}
	desc, err := s.oauthProvider("codex")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	verifier, challenge, err := generatePKCE()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	state, err := randomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	sid, err := randomToken(18)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.oauth.put(sid, oauthPending{provider: "codex", verifier: verifier, state: state, reauthAccountID: account.ID, targetWorkspaceID: targetWorkspaceID})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id":          sid,
		"provider":            desc.provider,
		"target_workspace_id": targetWorkspaceID,
		"auth_url":            desc.authorizeURLWithOptions(challenge, state, oauthAuthorizeOptions{AllowedWorkspaceID: targetWorkspaceID}),
		"expires_in":          int(s.oauth.ttl.Seconds()),
	})
}

func (s *Server) adminCodexReauthOAuthComplete(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, ok, err := s.codexReauthEligibleAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts"))
		return
	}
	var req struct {
		SessionID  string `json:"session_id"`
		Redirected string `json:"redirected"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.Redirected) == "" {
		writeError(w, http.StatusBadRequest, errors.New("session_id and redirected are required"))
		return
	}
	redirected := parseOAuthRedirected(req.Redirected)
	if redirected.Code == "" && redirected.Error == "" && redirected.ErrorDescription == "" {
		writeError(w, http.StatusBadRequest, errors.New("未能从粘贴内容中解析出授权码"))
		return
	}
	pend, ok := s.oauth.get(req.SessionID)
	if !ok || pend.provider != "codex" || pend.reauthAccountID != account.ID {
		writeError(w, http.StatusBadRequest, errors.New("登录会话已过期或不属于该账号，请重新生成登录链接"))
		return
	}
	if redirected.State != "" && pend.state != "" && redirected.State != pend.state {
		writeError(w, http.StatusBadRequest, errors.New("state 不匹配，可能不是本次登录的回调，请重新登录"))
		return
	}
	if err := oauthCallbackFailure(redirected); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	desc, err := s.oauthProvider("codex")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	claimed, ok := s.oauth.take(req.SessionID)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("登录回调正在处理或已被使用，请重新生成登录链接"))
		return
	}
	parsed, err := s.exchangeCodexCode(r.Context(), desc, redirected.Code, claimed.verifier)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if err := validateCodexTargetWorkspace(parsed, claimed.targetWorkspaceID); err != nil {
		_ = s.store.UpdateCodexReauthConfigStatus(r.Context(), account.ID, storage.CodexReauthJobWorkspaceMismatch, err.Error())
		writeError(w, http.StatusConflict, err)
		return
	}
	updated, err := s.applyCodexReauthParsed(r.Context(), account.ID, parsed, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.store.UpdateCodexReauthConfigStatus(r.Context(), account.ID, storage.CodexReauthJobSucceeded, "")
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) codexReauthEligibleAccount(ctx context.Context, accountID string) (storage.Account, bool, error) {
	account, err := s.store.GetAccount(ctx, strings.TrimSpace(accountID))
	if err != nil {
		return storage.Account{}, false, err
	}
	providers, err := s.store.ResolveAccountProviders(ctx, []storage.Account{account})
	if err != nil {
		return storage.Account{}, false, err
	}
	return account, providers[account.ID] == "codex", nil
}

func (s *Server) enqueueCodexReauthIfEligible(ctx context.Context, accountID, reason string) bool {
	account, ok, err := s.codexReauthEligibleAccount(ctx, accountID)
	if err != nil || !ok {
		return false
	}
	cfg, found, err := s.store.GetCodexReauthConfig(ctx, account.ID)
	if err != nil {
		return false
	}
	if !found || !cfg.AutoEnabled {
		job, created, err := s.store.EnqueueCodexReauthJob(ctx, account.ID, firstNonEmpty(reason, "needs_manual"))
		if err == nil && created {
			_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobNeedsManual, "codex reauth config missing or disabled")
		}
		if found {
			_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobNeedsManual, "auto reauth disabled")
		}
		return false
	}
	job, _, err := s.store.EnqueueCodexReauthJob(ctx, account.ID, firstNonEmpty(reason, "auth_expired"))
	if err != nil {
		return false
	}
	_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, job.Status, "")
	return true
}

func (s *Server) runCodexReauthJob(ctx context.Context, jobID int64) (map[string]interface{}, int, error) {
	job, found, err := s.store.GetCodexReauthJob(ctx, jobID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !found {
		return nil, http.StatusNotFound, errors.New("codex reauth job not found")
	}
	account, ok, err := s.codexReauthEligibleAccount(ctx, job.AccountID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !ok {
		return nil, http.StatusBadRequest, errors.New("codex reauth is only available for Codex/ChatGPT accounts")
	}
	cfg, configured, err := s.store.GetCodexReauthConfig(ctx, account.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if !configured {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobNeedsManual, "codex reauth config missing")
		return nil, http.StatusBadRequest, errors.New("codex reauth config missing")
	}
	workerURL := strings.TrimRight(strings.TrimSpace(s.cfg.CodexReauthWorkerURL), "/")
	if workerURL == "" {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobNeedsManual, "codex reauth worker url not configured")
		return nil, http.StatusBadRequest, errors.New("codex reauth worker url not configured")
	}
	_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobRunning, "")
	cookie, _ := s.store.GetSessionCookie(ctx, account.ID)
	payload := codexReauthWorkerRequest{
		Email:             cfg.LoginEmail,
		Password:          cfg.Password,
		OTPURL:            cfg.OTPURL,
		TargetWorkspaceID: cfg.TargetWorkspaceID,
		CookieHeader:      cookie,
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, workerURL+"/v1/codex/reauth", bytes.NewReader(raw))
	if err != nil {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobFailed, err.Error())
		return nil, http.StatusInternalServerError, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobFailed, err.Error())
		_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobFailed, err.Error())
		return nil, http.StatusBadGateway, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var wr codexReauthWorkerResponse
	_ = json.Unmarshal(body, &wr)
	if resp.StatusCode >= 400 || strings.EqualFold(wr.Status, storage.CodexReauthJobFailed) || strings.EqualFold(wr.Status, storage.CodexReauthJobNeedsManual) {
		msg := firstNonEmpty(wr.Error, string(body), resp.Status)
		if resp.StatusCode == http.StatusConflict || strings.EqualFold(wr.Code, storage.CodexReauthJobWorkspaceMismatch) {
			_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobWorkspaceMismatch, msg)
			_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobWorkspaceMismatch, msg)
			return nil, http.StatusConflict, errors.New(msg)
		}
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobFailed, msg)
		_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobFailed, msg)
		return nil, http.StatusBadGateway, errors.New(msg)
	}
	parsed, err := authparse.ParseOAuthCodex(wr.AccessToken, wr.RefreshToken, wr.IDToken)
	if err != nil {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobFailed, err.Error())
		_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobFailed, err.Error())
		return nil, http.StatusBadGateway, err
	}
	if parsed.Email == "" {
		parsed.Email = strings.TrimSpace(wr.Email)
	}
	if parsed.UpstreamAccountID == "" {
		parsed.UpstreamAccountID = strings.TrimSpace(wr.WorkspaceID)
	}
	if parsed.PlanType == "" {
		parsed.PlanType = strings.TrimSpace(wr.PlanType)
	}
	if err := validateCodexTargetWorkspace(parsed, cfg.TargetWorkspaceID); err != nil {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobWorkspaceMismatch, err.Error())
		_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobWorkspaceMismatch, err.Error())
		return nil, http.StatusConflict, err
	}
	updated, err := s.applyCodexReauthParsed(ctx, account.ID, parsed, wr.SessionCookie)
	if err != nil {
		_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobFailed, err.Error())
		_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobFailed, err.Error())
		return nil, http.StatusInternalServerError, err
	}
	_ = s.store.UpdateCodexReauthJobStatus(ctx, job.ID, storage.CodexReauthJobSucceeded, "")
	_ = s.store.UpdateCodexReauthConfigStatus(ctx, account.ID, storage.CodexReauthJobSucceeded, "")
	return map[string]interface{}{"account": updated, "job_id": job.ID, "status": storage.CodexReauthJobSucceeded}, http.StatusOK, nil
}

func validateCodexTargetWorkspace(parsed authparse.ParsedAuth, targetWorkspaceID string) error {
	targetWorkspaceID = strings.TrimSpace(targetWorkspaceID)
	if targetWorkspaceID == "" {
		return nil
	}
	got := strings.TrimSpace(parsed.UpstreamAccountID)
	if got == "" {
		return fmt.Errorf("workspace mismatch: id_token has no chatgpt_account_id; expected %s", targetWorkspaceID)
	}
	if got != targetWorkspaceID {
		return fmt.Errorf("workspace mismatch: id_token chatgpt_account_id %s != target %s", got, targetWorkspaceID)
	}
	return nil
}

func (s *Server) applyCodexReauthParsed(ctx context.Context, accountID string, parsed authparse.ParsedAuth, sessionCookie string) (storage.Account, error) {
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return storage.Account{}, err
	}
	if parsed.UpstreamAccountID != "" {
		account.UpstreamAccountID = parsed.UpstreamAccountID
	}
	if parsed.ChatGPTUserID != "" {
		account.ChatGPTUserID = parsed.ChatGPTUserID
	}
	if parsed.Email != "" {
		account.Email = parsed.Email
	}
	if parsed.PlanType != "" {
		account.PlanType = parsed.PlanType
	}
	account.Provider = "codex"
	account.Status = "active"
	account.IsFedramp = parsed.IsFedramp
	lastRefresh := parsed.LastRefresh
	if lastRefresh == 0 {
		lastRefresh = storage.Now()
	}
	token := storage.AccountToken{
		AccountID:    account.ID,
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		OpenAIAPIKey: parsed.OpenAIAPIKey,
		IDTokenRaw:   parsed.IDTokenRaw,
		LastRefresh:  lastRefresh,
		ExpiresAt:    parsed.ExpiresAt,
		Scopes:       strings.Join(parsed.Scopes, " "),
		CreatedAt:    storage.Now(),
	}
	token.AuthMethod = accountprovider.AuthMethodOAuth
	if err := s.store.UpsertAccount(ctx, account, token); err != nil {
		return storage.Account{}, err
	}
	_ = s.store.SetAccountQuarantine(ctx, account.ID, 0, "")
	_ = s.store.SetAccountStatus(ctx, account.ID, "active")
	if strings.TrimSpace(sessionCookie) != "" {
		_ = s.store.SetSessionCookie(ctx, account.ID, sessionCookie)
	}
	return s.store.GetAccount(ctx, account.ID)
}
