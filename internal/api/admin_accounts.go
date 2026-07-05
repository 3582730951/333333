// admin_accounts.go holds the admin console's account-management REST handlers:
// listing, auth.json/token/cookie import, group assignment, enable/disable/quarantine,
// and deletion. Extracted verbatim from server.go (no behavior change). Imports via
// goimports.
package api

import (
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	// Server-side pagination: the frontend sends ?page=1&pageSize=20&search=&status=.
	// When page is 0 (old frontend), fall back to the full list for backward compat.
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if page > 0 && pageSize > 0 {
		accounts, total, err := s.store.ListAccountsPage(r.Context(), pageSize, (page-1)*pageSize, search, status)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out, err := s.accountViews(r.Context(), accounts)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"accounts": out, "total": total, "page": page, "pageSize": pageSize,
		})
		return
	}
	// Legacy full-list path (no page param).
	accounts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out, err := s.accountViews(r.Context(), accounts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type accountView struct {
	storage.Account
	Provider     string                        `json:"provider"`
	Capabilities []storage.ModelCapability     `json:"capabilities"`
	Egress       *storage.AccountEgressBinding `json:"egress_binding,omitempty"`
	Usage        *storage.UsageSummaryRow      `json:"usage,omitempty"`
	QuotaSummary QuotaSummary                  `json:"quota_summary"`
}

type accountImportResponse struct {
	storage.Account
	Duplicate    bool   `json:"duplicate,omitempty"`
	ImportStatus string `json:"import_status,omitempty"`
}

func (s *Server) accountViews(ctx context.Context, accounts []storage.Account) ([]accountView, error) {
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	providers, err := s.store.ResolveAccountProviders(ctx, accounts)
	if err != nil {
		return nil, err
	}
	capabilities, err := s.store.ListCapabilitiesByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	usages, err := s.store.UsageSummaryByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	tokens, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	quotaSnapshots, err := s.store.ListAccountRateLimitsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	now := storage.Now()
	out := make([]accountView, 0, len(accounts))
	for _, account := range accounts {
		var token *storage.AccountToken
		if t, ok := tokens[account.ID]; ok {
			token = &t
		}
		view := accountView{
			Account:      account,
			Provider:     providers[account.ID],
			Capabilities: capabilities[account.ID],
			QuotaSummary: BuildQuotaSummary(account, token, quotaSnapshots[account.ID], now),
		}
		if binding, ok := bindings[account.ID]; ok {
			view.Egress = &binding
		}
		if usage, ok := usages[account.ID]; ok {
			view.Usage = &usage
		}
		out = append(out, view)
	}
	return out, nil
}

func (s *Server) adminAccountsSummary(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	summary, err := s.store.AccountPoolSummary(r.Context(), storage.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) adminImportAuthJSON(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Label           string          `json:"label"`
		GroupName       string          `json:"group_name"`
		EgressID        string          `json:"egress_id"`
		PrimaryEgressID string          `json:"primary_egress_id"`
		AuthJSON        json.RawMessage `json:"auth_json"`
		AuthJSONText    string          `json:"auth_json_text"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw := []byte(req.AuthJSON)
	if len(raw) == 0 && req.AuthJSONText != "" {
		raw = []byte(req.AuthJSONText)
	}
	parsed, err := authparse.ParseAuthJSON(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Label == "" {
		req.Label = firstNonEmpty(parsed.Name, parsed.Email, parsed.UpstreamAccountID, parsed.AccountID)
	}
	if req.GroupName == "" {
		req.GroupName = s.cfg.DefaultGroup
	}
	account := storage.Account{
		ID:                parsed.AccountID,
		Label:             req.Label,
		GroupName:         req.GroupName,
		UpstreamAccountID: parsed.UpstreamAccountID,
		ChatGPTUserID:     parsed.ChatGPTUserID,
		Email:             parsed.Email,
		PlanType:          parsed.PlanType,
		Provider:          parsed.Provider,
		Status:            "active",
		IsFedramp:         parsed.IsFedramp,
	}
	lastRefresh := parsed.LastRefresh
	if lastRefresh == 0 {
		lastRefresh = storage.Now()
	}
	token := storage.AccountToken{
		AccessToken:        parsed.AccessToken,
		RefreshToken:       parsed.RefreshToken,
		OpenAIAPIKey:       parsed.OpenAIAPIKey,
		IDTokenRaw:         parsed.IDTokenRaw,
		LastRefresh:        lastRefresh,
		ExpiresAt:          parsed.ExpiresAt,
		Scopes:             strings.Join(parsed.Scopes, " "),
		OAuthRateLimitTier: parsed.OAuthRateLimitTier,
	}
	if existing, err := s.store.GetAccount(r.Context(), account.ID); err == nil {
		writeJSON(w, http.StatusOK, accountImportResponse{Account: existing, Duplicate: true, ImportStatus: "duplicate"})
		return
	}
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bindImportedAccountPrimaryEgress(r.Context(), account.ID, requestedImportEgressID(req.EgressID, req.PrimaryEgressID)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, accountImportResponse{Account: account, ImportStatus: "imported"})
}

func (s *Server) saveImportedAccount(ctx context.Context, parsed authparse.ParsedAuth, label, groupName, refreshToken, provider, egressID string) (storage.Account, error) {
	if label == "" {
		label = firstNonEmpty(parsed.Name, parsed.Email, parsed.UpstreamAccountID, parsed.AccountID)
	}
	if groupName == "" {
		groupName = s.cfg.DefaultGroup
	}
	if refreshToken == "" {
		refreshToken = parsed.RefreshToken
	}
	if provider == "" {
		provider = parsed.Provider
	}
	account := storage.Account{
		ID:                parsed.AccountID,
		Label:             label,
		GroupName:         groupName,
		UpstreamAccountID: parsed.UpstreamAccountID,
		ChatGPTUserID:     parsed.ChatGPTUserID,
		Email:             parsed.Email,
		PlanType:          parsed.PlanType,
		Provider:          provider,
		Status:            "active",
		IsFedramp:         parsed.IsFedramp,
	}
	lastRefresh := parsed.LastRefresh
	if lastRefresh == 0 {
		lastRefresh = storage.Now()
	}
	token := storage.AccountToken{
		AccessToken:        parsed.AccessToken,
		RefreshToken:       refreshToken,
		OpenAIAPIKey:       parsed.OpenAIAPIKey,
		IDTokenRaw:         parsed.IDTokenRaw,
		LastRefresh:        lastRefresh,
		ExpiresAt:          parsed.ExpiresAt,
		Scopes:             strings.Join(parsed.Scopes, " "),
		OAuthRateLimitTier: parsed.OAuthRateLimitTier,
	}
	if existing, err := s.store.GetAccount(ctx, account.ID); err == nil {
		return existing, nil
	}
	if err := s.store.UpsertAccount(ctx, account, token); err != nil {
		return storage.Account{}, err
	}
	if err := s.bindImportedAccountPrimaryEgress(ctx, account.ID, egressID); err != nil {
		return storage.Account{}, err
	}
	// Probe the new account's upstream model list once, in the background, so the
	// advertised /v1/models union reflects it without waiting for the periodic
	// sweep. Detached context + fire-and-forget so it never blocks the import
	// response; it no-ops harmlessly if the egress binding isn't set yet.
	go func(acc storage.Account) {
		defer supervisor.Recover("account-import-probe")
		cctx, cancel := context.WithTimeout(context.Background(), s.cfg.RequestTimeout())
		defer cancel()
		_, _ = s.probeAccountModels(cctx, acc)
	}(account)
	return account, nil
}

// adminImportToken imports a ChatGPT account from a bare access token ("AT"
// import) — no auth.json and no phone/organization verification required.
func (s *Server) adminImportToken(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Label           string `json:"label"`
		GroupName       string `json:"group_name"`
		EgressID        string `json:"egress_id"`
		PrimaryEgressID string `json:"primary_egress_id"`
		AccessToken     string `json:"access_token"`
		RefreshToken    string `json:"refresh_token"`
		AccountID       string `json:"account_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	parsed, err := authparse.ParseAccessToken(req.AccessToken, req.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.saveImportedAccount(r.Context(), parsed, req.Label, req.GroupName, req.RefreshToken, "", requestedImportEgressID(req.EgressID, req.PrimaryEgressID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// adminImportCookie imports a ChatGPT account from a chatgpt.com session cookie:
// the server exchanges it for an access token via /api/auth/session and stores
// the cookie so the token can be silently re-minted on refresh.
func (s *Server) adminImportCookie(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Label           string `json:"label"`
		GroupName       string `json:"group_name"`
		EgressID        string `json:"egress_id"`
		PrimaryEgressID string `json:"primary_egress_id"`
		CookieHeader    string `json:"cookie_header"`
		AccountID       string `json:"account_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.CookieHeader) == "" {
		writeError(w, http.StatusBadRequest, errors.New("cookie_header required"))
		return
	}
	accessToken, err := fetchChatGPTSessionToken(r.Context(), req.CookieHeader)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	parsed, err := authparse.ParseAccessToken(accessToken, req.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.saveImportedAccount(r.Context(), parsed, req.Label, req.GroupName, "", "", requestedImportEgressID(req.EgressID, req.PrimaryEgressID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SetSessionCookie(r.Context(), account.ID, req.CookieHeader); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

// fetchChatGPTSessionToken exchanges a chatgpt.com session cookie for the
// account access token via the same endpoint the web app uses.
func fetchChatGPTSessionToken(ctx context.Context, cookieHeader string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/api/auth/session", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chatgpt session fetch failed: status %d", resp.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return "", errors.New("chatgpt session response had no accessToken (cookie expired or invalid)")
	}
	return parsed.AccessToken, nil
}

func (s *Server) adminAccountAction(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	accountID, action := parts[0], parts[1]
	switch action {
	case "probe-models":
		s.adminProbeModels(w, r, accountID)
	case "refresh":
		s.adminRefresh(w, r, accountID)
	case "egress-binding":
		s.adminEgressBinding(w, r, accountID)
	case "identity":
		s.adminIdentity(w, r, accountID)
	case "sessions":
		s.adminSessions(w, r, accountID)
	case "health-test":
		s.adminHealthTest(w, r, accountID)
	case "browser-repair":
		s.adminBrowserRepair(w, r, accountID)
	case "disable":
		s.adminSetAccountStatus(w, r, accountID, "disabled")
	case "enable":
		s.adminSetAccountStatus(w, r, accountID, "active")
	case "clear-quarantine":
		s.adminClearQuarantine(w, r, accountID)
	case "group":
		s.adminSetAccountGroup(w, r, accountID)
	case "delete":
		s.adminDeleteAccount(w, r, accountID)
	default:
		http.NotFound(w, r)
	}
}

// adminSetAccountGroup reassigns a single account to a different (existing) group.
// POST/PATCH /admin/accounts/<id>/group  {"group":"<name>"}
func (s *Server) adminSetAccountGroup(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Group string `json:"group"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		writeError(w, http.StatusBadRequest, errors.New("group required"))
		return
	}
	if _, err := s.store.GetGroup(r.Context(), group); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("group %q does not exist", group))
		return
	}
	if err := s.store.SetAccountGroup(r.Context(), accountID, group); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "group": group})
}

// adminAccountsAssignGroup bulk-reassigns accounts to a group.
// POST /admin/accounts/assign-group  {"ids":["..."],"group":"<name>"}
func (s *Server) adminAccountsAssignGroup(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		IDs   []string `json:"ids"`
		Group string   `json:"group"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		writeError(w, http.StatusBadRequest, errors.New("group required"))
		return
	}
	if _, err := s.store.GetGroup(r.Context(), group); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("group %q does not exist", group))
		return
	}
	updated := 0
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if err := s.store.SetAccountGroup(r.Context(), id, group); err == nil {
			updated++
		}
	}
	if updated > 0 && s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"group": group, "accounts_updated": updated})
}

func (s *Server) adminSetAccountStatus(w http.ResponseWriter, r *http.Request, accountID, status string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.store.SetAccountStatus(r.Context(), accountID, status); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "status": status})
}

func (s *Server) adminClearQuarantine(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.store.SetAccountQuarantine(r.Context(), accountID, 0, ""); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "clear_quarantine",
		State:     "manual",
		Reason:    "cleared by admin",
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "quarantine_until": 0})
}

func (s *Server) adminDeleteAccount(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if err := s.store.DeleteAccount(r.Context(), accountID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "deleted": true})
}

// probeAccountModels fetches one account's upstream-supported model list and
// upserts it into the capability store. It is the shared core used by the manual
// admin probe, the on-import probe, and the periodic background sweep, so all
// three stay consistent. Codex accounts are probed via the ChatGPT /models
// endpoint (reporting a current client_version so the version-gated catalog returns
// today's models), with a curated static fallback when the probe is unavailable;
// Claude accounts are probed via Anthropic's /v1/models, unioned with a static
// current-gen floor (see probeClaudeModels). Accounts of other providers are
// skipped (returns nil, nil). A probe failure NEVER bans the account.
