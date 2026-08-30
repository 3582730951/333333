// admin_accounts.go holds the admin console's account-management REST handlers:
// listing, auth.json/token/cookie import, group assignment, enable/disable/quarantine,
// and deletion. Extracted verbatim from server.go (no behavior change). Imports via
// goimports.
package api

import (
	"codex-account-pool/internal/accountprovider"
	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("X-Response-Contract", "admin.accounts.v1")
	// Server-side pagination: the frontend sends ?page=1&pageSize=20&search=&status=.
	// When page is 0 (old frontend), fall back to the full list for backward compat.
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	authType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("authType")))
	group := strings.TrimSpace(r.URL.Query().Get("group"))
	if authType != "" && authType != "all" && authType != "api_key" && authType != "account" {
		writeError(w, http.StatusBadRequest, errors.New("authType must be api_key, account, or all"))
		return
	}
	if authType == "all" {
		authType = ""
	}
	if page > 0 && pageSize > 0 {
		accounts, total, err := s.store.ListAccountsPageFiltered(r.Context(), pageSize, (page-1)*pageSize, search, status, authType, group)
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
			"contract_version": "admin.accounts.v1",
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
	Provider               string                        `json:"provider"`
	AuthMethod             string                        `json:"auth_method"`
	CredentialMode         string                        `json:"credential_mode,omitempty"`
	AgentIdentityPresent   bool                          `json:"agent_identity_present,omitempty"`
	BillingMode            string                        `json:"billing_mode"`
	APIKeyPresent          bool                          `json:"api_key_present"`
	Capabilities           []storage.ModelCapability     `json:"capabilities"`
	Egress                 *storage.AccountEgressBinding `json:"egress_binding,omitempty"`
	Usage                  *storage.UsageSummaryRow      `json:"usage,omitempty"`
	QuotaSummary           QuotaSummary                  `json:"quota_summary"`
	CodexReauthConfigured  bool                          `json:"codex_reauth_configured,omitempty"`
	CodexReauthAutoEnabled bool                          `json:"codex_reauth_auto_enabled,omitempty"`
	CodexReauthLastStatus  string                        `json:"codex_reauth_last_status,omitempty"`
	KiroAuth               *storage.KiroAuthSummary      `json:"kiro_auth,omitempty"`
	RequestRate            storage.AccountRequestRate    `json:"request_rate"`
}

type accountImportResponse struct {
	storage.Account
	Duplicate      bool     `json:"duplicate,omitempty"`
	Updated        bool     `json:"updated,omitempty"`
	ImportStatus   string   `json:"import_status,omitempty"`
	CredentialMode string   `json:"credential_mode,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

type authJSONImportRequest struct {
	Label           string          `json:"label"`
	GroupName       string          `json:"group_name"`
	EgressID        string          `json:"egress_id"`
	PrimaryEgressID string          `json:"primary_egress_id"`
	AuthJSON        json.RawMessage `json:"auth_json"`
	AuthJSONText    string          `json:"auth_json_text"`
	SessionCookie   string          `json:"session_cookie"`
}

func (s *Server) accountViews(ctx context.Context, accounts []storage.Account) ([]accountView, error) {
	if len(accounts) == 0 {
		return []accountView{}, nil
	}
	accountIDs := make([]string, 0, len(accounts))
	for _, account := range accounts {
		accountIDs = append(accountIDs, account.ID)
	}
	var (
		providers      map[string]string
		capabilities   map[string][]storage.ModelCapability
		bindings       map[string]storage.AccountEgressBinding
		groups         []storage.Group
		usages         map[string]storage.UsageSummaryRow
		tokens         map[string]storage.AccountToken
		quotaSnapshots map[string][]storage.AccountRateLimit
		reauthConfigs  map[string]storage.AccountCodexReauthConfig
		kiroSummaries  map[string]storage.KiroAuthSummary
		requestRates   map[string]storage.AccountRequestRate
	)
	loadCtx, cancelLoads := context.WithCancel(ctx)
	defer cancelLoads()
	loads := []func() error{
		func() (err error) { providers, err = s.store.ResolveAccountProviders(loadCtx, accounts); return err },
		func() (err error) {
			capabilities, err = s.store.ListCapabilitiesSummaryByAccountIDs(loadCtx, accountIDs)
			return err
		},
		func() (err error) {
			bindings, err = s.store.ListEgressBindingsByAccountIDs(loadCtx, accountIDs)
			return err
		},
		func() (err error) { groups, err = s.store.ListGroups(loadCtx); return err },
		func() (err error) { usages, err = s.store.UsageSummaryByAccountIDs(loadCtx, accountIDs); return err },
		func() (err error) { tokens, err = s.store.ListTokensByAccountIDs(loadCtx, accountIDs); return err },
		func() (err error) {
			quotaSnapshots, err = s.store.ListAccountRateLimitsByAccountIDs(loadCtx, accountIDs)
			return err
		},
		func() (err error) {
			reauthConfigs, err = s.store.ListCodexReauthConfigPublicByAccountIDs(loadCtx, accountIDs)
			return err
		},
		func() (err error) {
			kiroSummaries, err = s.store.KiroAuthSummariesByAccountIDs(loadCtx, accountIDs)
			return err
		},
		func() error {
			var rateErr error
			requestRates, _, rateErr = s.AccountRequestRates(loadCtx, accountIDs)
			if rateErr != nil {
				log.Printf("account list request rates degraded: %v", rateErr)
			}
			return nil
		},
	}
	var loadWG sync.WaitGroup
	var loadErr error
	var loadErrOnce sync.Once
	loadWG.Add(len(loads))
	for _, load := range loads {
		go func() {
			defer loadWG.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					supervisor.LogPanic("admin-account-view-load", panicValue)
					loadErrOnce.Do(func() {
						loadErr = fmt.Errorf("account view load panic: %v", panicValue)
						cancelLoads()
					})
				}
			}()
			if err := load(); err != nil {
				loadErrOnce.Do(func() {
					loadErr = err
					cancelLoads()
				})
			}
		}()
	}
	loadWG.Wait()
	if loadErr != nil {
		return nil, loadErr
	}
	now := storage.Now()
	quotaSummaries := make([]QuotaSummary, len(accounts))
	for index, account := range accounts {
		var token *storage.AccountToken
		if value, ok := tokens[account.ID]; ok {
			token = &value
		}
		quotaSummaries[index] = BuildQuotaSummary(account, token, quotaSnapshots[account.ID], now)
	}
	if err := s.attachQuotaWindowEstimates(ctx, accounts, quotaSummaries, now); err != nil {
		log.Printf("account list quota window estimates degraded: %v", err)
	}
	groupEgresses := make(map[string]string, len(groups))
	for _, group := range groups {
		id := strings.TrimSpace(group.DefaultEgressID)
		for _, candidate := range group.EgressIDs {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				id = candidate
				break
			}
		}
		if id == "" {
			id = storage.DefaultDirectEgressID
		}
		groupEgresses[group.Name] = id
	}
	out := make([]accountView, 0, len(accounts))
	for index, account := range accounts {
		var token *storage.AccountToken
		if t, ok := tokens[account.ID]; ok {
			token = &t
		}
		summary := quotaSummaries[index]
		view := accountView{
			Account:      account,
			Provider:     providers[account.ID],
			Capabilities: capabilities[account.ID],
			QuotaSummary: summary,
			RequestRate:  requestRates[account.ID],
		}
		if token != nil {
			view.AuthMethod = accountprovider.EffectiveAuthMethod(view.Provider, *token)
			view.CredentialMode = token.CredentialMode
			view.AgentIdentityPresent = accountprovider.IsAgentIdentity(*token)
			view.BillingMode = accountprovider.BillingMode(view.Provider, *token)
			view.APIKeyPresent = accountprovider.UsesAPIKey(view.Provider, *token) && accountprovider.Credential(view.Provider, *token) != ""
		}
		if binding, ok := bindings[account.ID]; ok {
			if !strings.EqualFold(strings.TrimSpace(binding.BindingScope), storage.EgressBindingScopeAccount) {
				binding.BindingScope = storage.EgressBindingScopeGroup
				binding.PrimaryEgressID = firstNonEmpty(groupEgresses[account.GroupName], storage.DefaultDirectEgressID)
				binding.StandbyEgressIDs = ""
				binding.CookieJarKey = account.ID + ":" + binding.PrimaryEgressID
			}
			view.Egress = &binding
		}
		if usage, ok := usages[account.ID]; ok {
			view.Usage = &usage
		}
		if cfg, ok := reauthConfigs[account.ID]; ok {
			view.CodexReauthConfigured = true
			view.CodexReauthAutoEnabled = cfg.AutoEnabled
			view.CodexReauthLastStatus = cfg.LastStatus
		}
		if providers[account.ID] == "kiro" {
			if summary, ok := kiroSummaries[account.ID]; ok {
				view.KiroAuth = &summary
				// Kiro keeps its credential shape in a separate encrypted table. Surface
				// that method through the common account contract so filtering and badges
				// do not mislabel a ksk_ key as an OAuth account.
				if view.AuthMethod == "" {
					view.AuthMethod = strings.ToLower(strings.TrimSpace(summary.AuthMethod))
				}
				if view.AuthMethod == accountprovider.AuthMethodAPIKey {
					view.APIKeyPresent = summary.HasAPIKey
					view.BillingMode = accountprovider.BillingModePayAsYouGo
				}
			}
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
	var req authJSONImportRequest
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw := []byte(req.AuthJSON)
	if len(raw) == 0 && req.AuthJSONText != "" {
		raw = []byte(req.AuthJSONText)
	}
	doc, err := authparse.ParseImportDocument(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if doc.Format != authparse.ImportFormatSingle {
		s.adminImportAuthDocument(w, r, req, doc)
		return
	}
	parsed := doc.Entries[0].Parsed
	sessionCookie, err := normalizeImportedSessionCookie(req.SessionCookie, parsed)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	warnings := importedAuthWarnings(parsed, sessionCookie)
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
	token := accountTokenFromParsed(parsed, parsed.RefreshToken)
	if strings.TrimSpace(account.Provider) == "" {
		if inferred := accountprovider.InferProviderFromToken(token); inferred != accountprovider.UnknownProvider {
			account.Provider = inferred
		}
	}
	if token.AuthMethod == "" {
		token.AuthMethod = accountprovider.EffectiveAuthMethod(account.Provider, token)
	}
	credential := accountprovider.Credential(account.Provider, token)
	if (account.Provider == "codex" || account.Provider == "claude") &&
		(accountprovider.UsesAPIKey(account.Provider, token) || accountprovider.LooksLikeAPIKey(account.Provider, credential)) {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "built-in provider API keys must be imported with /admin/accounts/import-key and confirm_cost:true")
		return
	}
	if existing, err := s.findExistingImportedAccount(r.Context(), parsed); err == nil {
		updatedAccount, updated, updateErr := s.updateExistingExternalChatGPTTokens(r.Context(), existing, parsed, sessionCookie)
		if updateErr != nil {
			writeError(w, http.StatusBadRequest, updateErr)
			return
		}
		status := "duplicate"
		if updated {
			status = "updated"
		}
		writeJSON(w, http.StatusOK, accountImportResponse{
			Account: updatedAccount, Duplicate: !updated, Updated: updated, ImportStatus: status,
			CredentialMode: parsed.CredentialMode, Warnings: warnings,
		})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.UpsertAccount(r.Context(), account, token); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.storeImportedSessionCookie(r.Context(), account.ID, sessionCookie); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.bindImportedAccountPrimaryEgress(r.Context(), account.ID, requestedImportEgressID(req.EgressID, req.PrimaryEgressID)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.seedImportedAccountCapabilities(r.Context(), account); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, accountImportResponse{
		Account: account, ImportStatus: "imported", CredentialMode: parsed.CredentialMode, Warnings: warnings,
	})
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
	sessionCookie, err := normalizeImportedSessionCookie("", parsed)
	if err != nil {
		return storage.Account{}, err
	}
	if strings.TrimSpace(provider) == "" {
		shape := storage.AccountToken{
			AccessToken: parsed.AccessToken, RefreshToken: refreshToken,
			OpenAIAPIKey: parsed.OpenAIAPIKey, IDTokenRaw: parsed.IDTokenRaw,
			Scopes: strings.Join(parsed.Scopes, " "),
		}
		if inferred := accountprovider.InferProviderFromToken(shape); inferred != accountprovider.UnknownProvider {
			provider = inferred
		}
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
	token := accountTokenFromParsed(parsed, refreshToken)
	token.LastRefresh = lastRefresh
	token.AuthMethod = accountprovider.EffectiveAuthMethod(account.Provider, token)
	if parsed.CredentialMode == authparse.CredentialModeChatGPTAuthTokens && strings.TrimSpace(refreshToken) == "" {
		token.AuthMethod = accountprovider.AuthMethodAccessToken
	}
	if existing, err := s.findExistingImportedAccount(ctx, parsed); err == nil {
		updatedAccount, _, updateErr := s.updateExistingExternalChatGPTTokens(ctx, existing, parsed, sessionCookie)
		return updatedAccount, updateErr
	} else if !errors.Is(err, sql.ErrNoRows) {
		return storage.Account{}, err
	}
	var persistErr error
	if strings.EqualFold(provider, "antigravity") {
		persistErr = s.store.UpsertAccountWithAntigravityCredentials(ctx, account, token, storage.AntigravityCredentials{
			AccountID: account.ID, Email: account.Email, ProjectID: parsed.AntigravityProjectID,
			AccessToken: parsed.AccessToken, RefreshToken: refreshToken, ExpiresAt: parsed.ExpiresAt,
			BaseURL: parsed.AntigravityBaseURL, UserAgent: parsed.AntigravityUserAgent,
		})
	} else {
		persistErr = s.store.UpsertAccount(ctx, account, token)
	}
	if persistErr != nil {
		return storage.Account{}, persistErr
	}
	if err := s.storeImportedSessionCookie(ctx, account.ID, sessionCookie); err != nil {
		return storage.Account{}, err
	}
	if err := s.bindImportedAccountPrimaryEgress(ctx, account.ID, egressID); err != nil {
		return storage.Account{}, err
	}
	if err := s.seedImportedAccountCapabilities(ctx, account); err != nil {
		return storage.Account{}, err
	}
	s.probeImportedAccountAsync(account)
	return account, nil
}

// findExistingImportedAccount keeps Agent Identity imports compatible with rows
// created before its stable key included chatgpt_user_id. A workspace/account ID
// may be shared by multiple users, so the fallback always requires both fields
// and confirms the stored credential mode before reporting a duplicate.
func (s *Server) findExistingImportedAccount(ctx context.Context, parsed authparse.ParsedAuth) (storage.Account, error) {
	if existing, err := s.store.GetAccount(ctx, parsed.AccountID); err == nil || !errors.Is(err, sql.ErrNoRows) {
		return existing, err
	}
	shape := storage.AccountToken{
		CredentialMode: parsed.CredentialMode, AgentRuntimeID: parsed.AgentRuntimeID, AgentPrivateKey: parsed.AgentPrivateKey,
	}
	if !accountprovider.IsAgentIdentity(shape) || strings.TrimSpace(parsed.UpstreamAccountID) == "" || strings.TrimSpace(parsed.ChatGPTUserID) == "" {
		return storage.Account{}, sql.ErrNoRows
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return storage.Account{}, err
	}
	for _, candidate := range accounts {
		if candidate.UpstreamAccountID != parsed.UpstreamAccountID || candidate.ChatGPTUserID != parsed.ChatGPTUserID {
			continue
		}
		token, tokenErr := s.store.GetToken(ctx, candidate.ID)
		if tokenErr != nil {
			if errors.Is(tokenErr, sql.ErrNoRows) {
				continue
			}
			return storage.Account{}, tokenErr
		}
		if accountprovider.IsAgentIdentity(token) {
			return candidate, nil
		}
	}
	return storage.Account{}, sql.ErrNoRows
}

// seedImportedAccountCapabilities closes the import-to-first-request race. Static
// provider capabilities are unverified discovery hints; the detached live probe
// replaces them with the account's authoritative model catalog when available.
func (s *Server) seedImportedAccountCapabilities(ctx context.Context, account storage.Account) error {
	var caps []storage.ModelCapability
	switch strings.ToLower(strings.TrimSpace(account.Provider)) {
	case "claude":
		caps = capability.StaticClaudeModels(account.ID)
	case "kiro":
		caps = capability.StaticKiroModels(account.ID)
	case "", "codex":
		caps = capability.StaticCodexModels(account.ID)
	default:
		provider, ok, err := s.store.GetCustomProvider(ctx, account.Provider)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		seen := map[string]struct{}{}
		add := func(model string) {
			model = strings.TrimSpace(model)
			if model == "" {
				return
			}
			if _, exists := seen[model]; exists {
				return
			}
			seen[model] = struct{}{}
			caps = append(caps, storage.ModelCapability{
				AccountID: account.ID, ModelSlug: model,
				AvailabilityState:             capability.AvailabilityUnverified,
				EffectiveContextWindowPercent: 100,
				Visibility:                    "list",
				Source:                        "custom_static_unverified:" + provider.ID,
				LastProbeAt:                   storage.Now(),
			})
		}
		for _, model := range provider.Models {
			add(model)
		}
		for source, target := range provider.ModelMappings {
			if strings.TrimSpace(source) != "*" {
				add(source)
			}
			add(target)
		}
		if provider.AutoDiscoverModels &&
			provider.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages &&
			len(caps) == 0 {
			for _, model := range capability.ClaudeProbeModelTable() {
				add(model)
			}
		}
	}
	return s.store.UpsertCapabilities(ctx, caps)
}

// probeImportedAccountAsync refreshes the synchronous floor without delaying the
// import response. Detached context keeps the probe alive after the HTTP handler
// returns, while the timeout bounds cleanup on shutdown/network failure.
func (s *Server) probeImportedAccountAsync(account storage.Account) {
	s.launchRuntimeTask("account-import-probe", s.cfg.RequestTimeout(), func(ctx context.Context) {
		_, _ = s.probeAccountModels(ctx, account)
	})
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
	provider := accountprovider.InferProviderFromToken(storage.AccountToken{AccessToken: strings.TrimSpace(req.AccessToken)})
	if (provider == "codex" || provider == "claude") && accountprovider.LooksLikeAPIKey(provider, req.AccessToken) {
		writePoolCodeError(w, http.StatusBadRequest, "cost_confirmation_required", "upstream API keys must be imported with /admin/accounts/import-key and confirm_cost:true")
		return
	}
	parsed, err := authparse.ParseAccessToken(req.AccessToken, req.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.RefreshToken) != "" {
		parsed.CredentialMode = ""
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
	if strings.HasPrefix(action, "codex-reauth") {
		if token, err := s.store.GetToken(r.Context(), accountID); err == nil && (accountprovider.UsesAPIKey("codex", token) || accountprovider.IsAgentIdentity(token)) {
			writePoolCodeError(w, http.StatusBadRequest, "reauth_not_applicable", "this account does not use Codex OAuth reauthentication")
			return
		}
	}
	switch action {
	case "capacity":
		s.adminAccountCapacity(w, r, accountID)
	case "kiro":
		if len(parts) == 3 && parts[2] == "cache-probe" {
			s.adminKiroCacheProbe(w, r, accountID)
			return
		}
		http.NotFound(w, r)
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
	case "codex-reauth-config":
		s.adminCodexReauthConfig(w, r, accountID)
	case "codex-reauth-status":
		s.adminCodexReauthStatus(w, r, accountID)
	case "codex-reauth":
		s.adminCodexReauthAction(w, r, accountID, parts[2:])
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
	case "clear-cooldown":
		s.adminClearCooldown(w, r, accountID)
	case "rate-limit-controls":
		s.adminSetAccountRateLimitControls(w, r, accountID)
	case "force-codex-429":
		s.adminSetAccountForceCodex429(w, r, accountID)
	case "routing-policy":
		s.adminSetAccountRoutingPolicy(w, r, accountID)
	case "group":
		s.adminSetAccountGroup(w, r, accountID)
	case "delete":
		s.adminDeleteAccount(w, r, accountID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) adminCodexReauthAction(w http.ResponseWriter, r *http.Request, accountID string, parts []string) {
	if token, err := s.store.GetToken(r.Context(), accountID); err == nil && (accountprovider.UsesAPIKey("codex", token) || accountprovider.IsAgentIdentity(token)) {
		writePoolCodeError(w, http.StatusBadRequest, "reauth_not_applicable", "this account does not use Codex OAuth reauthentication")
		return
	}
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	switch parts[0] {
	case "run":
		s.adminCodexReauthRun(w, r, accountID)
	case "oauth":
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		switch parts[1] {
		case "start":
			s.adminCodexReauthOAuthStart(w, r, accountID)
		case "complete":
			s.adminCodexReauthOAuthComplete(w, r, accountID)
		default:
			http.NotFound(w, r)
		}
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
	for _, batch := range accountGroupAssignmentBatches(req.IDs) {
		updatedIDs, err := s.store.SetAccountsGroup(r.Context(), batch, group)
		if err != nil {
			// Keep the endpoint's established best-effort contract: a failure that
			// affects one row must not discard successful assignments in the chunk.
			for _, id := range batch {
				if err := s.store.SetAccountGroup(r.Context(), id, group); err == nil {
					updated++
				}
			}
			continue
		}
		succeeded := make(map[string]struct{}, len(updatedIDs))
		for _, id := range updatedIDs {
			succeeded[id] = struct{}{}
		}
		// Count successful input occurrences, rather than unique rows, matching
		// the previous per-ID loop when callers send duplicate IDs.
		for _, id := range batch {
			if _, ok := succeeded[id]; ok {
				updated++
			}
		}
	}
	if updated > 0 && s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"group": group, "accounts_updated": updated})
}

func accountGroupAssignmentBatches(accountIDs []string) [][]string {
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	batches := make([][]string, 0, (len(ids)+storage.AccountGroupBatchSize-1)/storage.AccountGroupBatchSize)
	for start := 0; start < len(ids); start += storage.AccountGroupBatchSize {
		end := start + storage.AccountGroupBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[start:end])
	}
	return batches
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
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	if status != "active" && s.cursorProxy != nil {
		s.cursorProxy.Stop(accountID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "status": status})
}

// adminSetAccountRateLimitControls updates the account-local override exposed by
// the account drawer. It deliberately does not clear stored cooldown/quarantine
// state: switching it back off restores the normal protections immediately.
func (s *Server) adminSetAccountRateLimitControls(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req struct {
		IgnoreRateLimitControls bool `json:"ignore_rate_limit_controls"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.store.GetAccount(r.Context(), accountID); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.store.SetAccountIgnoreRateLimitControls(r.Context(), accountID, req.IgnoreRateLimitControls); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.RefreshAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "set_ignore_rate_limit_controls",
		State:     "manual",
		Reason:    strconv.FormatBool(req.IgnoreRateLimitControls),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":                 accountID,
		"ignore_rate_limit_controls": req.IgnoreRateLimitControls,
	})
}

// adminSetAccountForceCodex429 updates the account-local "强制卡429" opt-in
// exposed by the account drawer. Like the rate-limit-controls override it does
// not clear stored cooldown/quarantine state; switching it back off restores the
// normal protections immediately. The flag only takes effect at runtime for
// OpenAI OAuth Codex requests (see codexAttempt); API-key accounts ignore it.
func (s *Server) adminSetAccountForceCodex429(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ForceCodex429 bool `json:"force_codex_429"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.store.GetAccount(r.Context(), accountID); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.store.SetAccountForceCodex429(r.Context(), accountID, req.ForceCodex429); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.RefreshAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "set_force_codex_429",
		State:     "manual",
		Reason:    strconv.FormatBool(req.ForceCodex429),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":      accountID,
		"force_codex_429": req.ForceCodex429,
	})
}

// adminSetAccountRoutingPolicy updates only fresh-selection share and the
// replay-safe same-credential attempt cap. It cannot move an existing sticky or
// native session and cannot enable retries for unsafe/stateful request classes.
func (s *Server) adminSetAccountRoutingPolicy(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost && r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	var req struct {
		RoutingWeight    int `json:"routing_weight"`
		RetryMaxAttempts int `json:"retry_max_attempts"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if req.RoutingWeight < 1 || req.RoutingWeight > 1000 {
		writeError(w, http.StatusBadRequest, errors.New("routing_weight must be between 1 and 1000"))
		return
	}
	if req.RetryMaxAttempts < 0 || req.RetryMaxAttempts > 3 {
		writeError(w, http.StatusBadRequest, errors.New("retry_max_attempts must be between 0 and 3"))
		return
	}
	if err := s.store.SetAccountRoutingPolicy(r.Context(), accountID, req.RoutingWeight, req.RetryMaxAttempts); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.RefreshAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID, AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action: "set_routing_policy", State: "manual",
		Detail: fmt.Sprintf("routing_weight=%d retry_max_attempts=%d", req.RoutingWeight, req.RetryMaxAttempts),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id": accountID, "routing_weight": req.RoutingWeight, "retry_max_attempts": req.RetryMaxAttempts,
	})
}

func (s *Server) adminClearQuarantine(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if isKiroSuspensionQuarantine(account) {
		writePoolCodeError(w, http.StatusConflict, "kiro_health_probe_required", "AWS User ID suspension quarantine can only be cleared by a successful administrator Kiro auth and inference health test")
		return
	}
	if isProviderAPIKeyInferenceQuarantine(account.QuarantineReason) {
		writePoolCodeError(w, http.StatusConflict, "provider_api_key_health_probe_required", "API-key inference quarantine can only be cleared by a successful administrator health test with confirm_cost:true")
		return
	}
	if err := s.store.SetAccountQuarantine(r.Context(), accountID, 0, ""); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.RefreshAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "clear_quarantine",
		State:     "manual",
		Reason:    "cleared by admin",
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "quarantine_until": 0})
}

func (s *Server) adminClearCooldown(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	rateLimitSnapshotsCleared, err := s.store.ClearAccountCooldown(r.Context(), accountID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.scheduler != nil {
		s.scheduler.RefreshAccountCache()
		s.scheduler.NotifyStateChanged()
	}
	_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "clear_cooldown",
		State:     "manual",
		Reason:    "cleared by admin",
		Detail:    fmt.Sprintf("rate_limit_snapshots_cleared=%d", rateLimitSnapshotsCleared),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"account_id":                   accountID,
		"cooldown_until":               0,
		"recheck_pending":              false,
		"rate_limit_snapshots_cleared": rateLimitSnapshotsCleared,
	})
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
	if s.cursorProxy != nil {
		s.cursorProxy.Stop(accountID)
	}
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "deleted": true})
}

// probeAccountModels fetches one account's upstream-supported model list and
// upserts it into the capability store. It is the shared core used by the manual
// admin probe, the on-import probe, and the periodic background sweep, so all
// three stay consistent. Codex accounts are probed via the ChatGPT /models
// endpoint (reporting a current client_version so the version-gated catalog returns
// today's models), with a curated static fallback when the probe is unavailable;
// Claude accounts are probed via Anthropic's /v1/models; a successful live catalog
// is authoritative, while an unavailable catalog leaves only unverified static
// discovery hints (see probeClaudeModels). Accounts of other providers are skipped
// (returns nil, nil). A probe failure NEVER bans the account.
