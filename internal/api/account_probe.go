// account_probe.go holds model-capability probing (codex/claude/custom), the
// background probe loop, and the token-refresh handlers (codex + claude). Extracted
// verbatim from server.go (no behavior change). Imports via goimports.
package api

import (
	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) probeAccountModels(ctx context.Context, account storage.Account) ([]storage.ModelCapability, error) {
	token, err := s.store.GetToken(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	binding, err := s.store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil {
		return nil, err
	}
	return s.probeAccountModelsWithDeps(ctx, account, token, binding, egress)
}

func (s *Server) probeAccountModelsWithDeps(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile) ([]storage.ModelCapability, error) {
	switch provider := s.accountProvider(account, token); provider {
	case "claude":
		return s.probeClaudeModels(ctx, account, token, binding, egress)
	case "kiro":
		caps := capability.StaticKiroModels(account.ID)
		if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
			return nil, err
		}
		cred, err := s.store.GetKiroCredentials(ctx, account.ID)
		if err != nil {
			return nil, err
		}
		cfg := s.effectiveKiroConfig(ctx)
		endpointHash, err := kirowire.EndpointHash(cred.Endpoint, firstNonEmpty(cred.APIRegion, cfg.KiroDefaultAPIRegion, "us-east-1"), cfg.KiroEndpointAllowlist)
		if err != nil {
			return nil, err
		}
		models := make([]string, 0, len(caps))
		for _, model := range caps {
			models = append(models, model.ModelSlug)
		}
		if err := s.store.EnsureKiroRuntimeModels(ctx, account.ID, endpointHash, models); err != nil {
			return nil, err
		}
		return caps, nil
	case "codex":
		// fall through to the Codex /models probe below
	default:
		// Custom OpenAI-compatible provider (DeepSeek, …).
		return s.probeCustomModels(ctx, account, token, binding, egress, provider)
	}
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := s.upstream.Do(ctx, upstream.Request{
			Method:         http.MethodGet,
			DownstreamPath: capability.ProbePath(s.cfg.ClientVersion),
			Headers:        http.Header{},
			Account:        account,
			Token:          token,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
			// Report the same current version on the UA/`version` header as in the
			// ?client_version= query, so the probe request is internally coherent. The
			// ChatGPT /models backend gates the catalog by client_version; a stale value
			// here is exactly why the newest models never came back.
			CodexClientVersion: s.cfg.ClientVersion,
		})
		if err != nil {
			log.Printf("codex model probe %s: %v; using static model set", account.ID, err)
			return s.upsertStaticCodexModels(ctx, account.ID)
		}
		raw, err := upstream.DrainAndClose(resp.Body)
		if err != nil {
			log.Printf("codex model probe %s: read body: %v; using static model set", account.ID, err)
			return s.upsertStaticCodexModels(ctx, account.ID)
		}
		if resp.StatusCode >= 400 {
			v := ban.Classify(false, resp.StatusCode, resp.Header, raw)
			if attempt == 0 && v.State == ban.AuthExpired {
				if refreshed, rerr := s.refreshCodexToken(ctx, token); rerr == nil && refreshed.Refreshed {
					token = refreshed.Token
					continue
				} else if rerr != nil {
					log.Printf("codex model probe %s: refresh failed: %v", account.ID, rerr)
					s.handleCodexRefreshFailure(ctx, account, refreshed, rerr, "model_probe")
				}
			}
			snippet := string(raw)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			log.Printf("codex model probe %s: upstream %d (%s); using static model set", account.ID, resp.StatusCode, snippet)
			return s.upsertStaticCodexModels(ctx, account.ID)
		}
		caps, err := capability.Parse(account.ID, raw, capability.ETagFromHeader(resp.Header))
		if err != nil {
			log.Printf("codex model probe %s: parse: %v; using static model set", account.ID, err)
			return s.upsertStaticCodexModels(ctx, account.ID)
		}
		if len(caps) == 0 {
			log.Printf("codex model probe %s: empty model set; using static model set", account.ID)
			return s.upsertStaticCodexModels(ctx, account.ID)
		}
		caps = capability.MergeCodexStatic(account.ID, caps)
		if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
			return nil, err
		}
		return caps, nil
	}
	return s.upsertStaticCodexModels(ctx, account.ID)
}

// upsertStaticCodexModels stores (and returns) the curated current-generation Codex
// catalog for an account. It is the fallback when the live ChatGPT /models probe is
// unavailable, so a transient probe failure never leaves a Codex account with an
// empty or stale model set. A probe failure NEVER bans the account.
func (s *Server) upsertStaticCodexModels(ctx context.Context, accountID string) ([]storage.ModelCapability, error) {
	caps := capability.StaticCodexModels(accountID)
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// probeClaudeModels populates a Claude account's model capabilities. It first
// asks Anthropic's GET /v1/models through the account (so the advertised set
// reflects what the account can actually use), and falls back to a curated
// static current-generation model set when that endpoint is unavailable — an
// OAuth (sk-ant-oat) token is scoped to inference and may be rejected by the
// models-listing endpoint. The reference relay (CLIProxyAPI) ships Claude models
// as a static registry for exactly this reason, so the fallback is faithful, not
// a guess. Either way the UI and /v1/models stop showing an empty/failed set.
// A probe failure here NEVER bans the account: capability discovery is advisory.
func (s *Server) probeClaudeModels(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile) ([]storage.ModelCapability, error) {
	var err error
	token, err = s.prepareClaudeToken(ctx, account, token, "model_probe_preflight")
	if err != nil {
		log.Printf("claude model probe %s: refresh wait: %v; using static model set", account.ID, err)
		return s.upsertStaticClaudeModels(ctx, account.ID)
	}
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         http.MethodGet,
			Provider:       "claude",
			DownstreamPath: "/v1/models",
			Headers:        http.Header{},
			Account:        account,
			Token:          t,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
		}
	}
	resp, err := s.upstream.Do(ctx, requestForToken(token))
	if err == nil {
		raw, derr := upstream.DrainAndClose(resp.Body)
		switch {
		case derr != nil:
			log.Printf("claude model probe %s: read body: %v; using static model set", account.ID, derr)
		case resp.StatusCode >= 400:
			if claudeAuthError(resp.StatusCode, resp.Header, raw) && claudeTokenCanRefresh(token) {
				if refreshed, rerr := s.forceRefreshClaudeToken(ctx, account, "auth_error"); rerr == nil {
					token = refreshed
					if retryResp, retryErr := s.upstream.Do(ctx, requestForToken(token)); retryErr == nil {
						raw, derr = upstream.DrainAndClose(retryResp.Body)
						resp = retryResp
						if derr == nil && resp.StatusCode < 400 {
							if caps, perr := capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header)); perr == nil && len(caps) > 0 {
								caps = capability.MergeClaudeStatic(account.ID, caps)
								if uerr := s.store.UpsertCapabilities(ctx, caps); uerr != nil {
									return nil, uerr
								}
								return caps, nil
							}
						}
					}
				}
			}
			snippet := string(raw)
			if len(snippet) > 200 {
				snippet = snippet[:200]
			}
			log.Printf("claude model probe %s: upstream %d (%s); using static model set", account.ID, resp.StatusCode, snippet)
		default:
			if caps, perr := capability.ParseClaudeModels(account.ID, raw, capability.ETagFromHeader(resp.Header)); perr == nil && len(caps) > 0 {
				// Union the curated current-gen set as a floor: a live probe is
				// authoritative for what the account exposes, but Anthropic's
				// /v1/models can lag a freshly shipped model (e.g. claude-opus-4-8),
				// so without this floor the newest model would stay hidden even on a
				// "successful" probe. MergeClaudeStatic keeps probe entries on conflict.
				caps = capability.MergeClaudeStatic(account.ID, caps)
				if uerr := s.store.UpsertCapabilities(ctx, caps); uerr != nil {
					return nil, uerr
				}
				return caps, nil
			} else if perr != nil {
				log.Printf("claude model probe %s: parse: %v; using static model set", account.ID, perr)
			}
		}
	} else {
		log.Printf("claude model probe %s: %v; using static model set", account.ID, err)
	}
	return s.upsertStaticClaudeModels(ctx, account.ID)
}

func (s *Server) upsertStaticClaudeModels(ctx context.Context, accountID string) ([]storage.ModelCapability, error) {
	caps := capability.StaticClaudeModels(accountID)
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

// accountProvider resolves an account's upstream provider, preferring the explicit
// provider column and falling back to the credential-shape heuristic for legacy rows
// imported before the provider column existed.
func (s *Server) accountProvider(account storage.Account, token storage.AccountToken) string {
	return accountprovider.EffectiveProvider(account.Provider, token, true)
}

// probeCustomModels discovers a custom OpenAI-compatible provider's models for an
// account. When auto-discovery is on it GETs {base}/models (OpenAI shape, parsed by
// capability.Parse) and unions the result with the provider's manual model list; the
// newly discovered ids are also persisted back into the provider record so routing and
// the admin model-list inputs reflect them. The manual list is the fallback when
// discovery is unavailable. Capabilities are tagged source "custom:<id>" so /v1/models
// advertises them natively (never the Codex virtual-2M window). Never bans the account.
func (s *Server) probeCustomModels(ctx context.Context, account storage.Account, token storage.AccountToken, binding storage.AccountEgressBinding, egress storage.EgressProfile, providerID string) ([]storage.ModelCapability, error) {
	prov, ok, err := s.store.GetCustomProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	seen := map[string]bool{}
	var ordered []string
	add := func(slug string) {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			return
		}
		seen[slug] = true
		ordered = append(ordered, slug)
	}
	for _, m := range prov.Models {
		add(m)
	}
	var discovered []string
	if prov.AutoDiscoverModels && strings.TrimSpace(prov.BaseURL) != "" {
		resp, derr := s.upstream.Do(ctx, upstream.Request{
			Method:         http.MethodGet,
			Provider:       providerID,
			BaseURL:        prov.BaseURL,
			DownstreamPath: "/models",
			Account:        account,
			Token:          token,
			Egress:         egress,
			CookieJarKey:   binding.CookieJarKey,
		})
		if derr != nil {
			log.Printf("custom model probe %s (%s): %v; using manual model list", account.ID, providerID, derr)
		} else {
			raw, rerr := upstream.DrainAndClose(resp.Body)
			switch {
			case rerr != nil:
				log.Printf("custom model probe %s (%s): read body: %v", account.ID, providerID, rerr)
			case resp.StatusCode >= 400:
				log.Printf("custom model probe %s (%s): upstream %d (%s)", account.ID, providerID, resp.StatusCode, bodySnippet(raw, 160))
			default:
				if caps, perr := capability.Parse(account.ID, raw, ""); perr == nil {
					for _, c := range caps {
						discovered = append(discovered, c.ModelSlug)
						add(c.ModelSlug)
					}
				} else {
					log.Printf("custom model probe %s (%s): parse: %v", account.ID, providerID, perr)
				}
			}
		}
	}
	// Persist newly discovered models into the provider's list so routing + the admin UI
	// reflect them (auto-discovery feeds the model list the operator can then prune).
	if len(discovered) > 0 {
		have := map[string]bool{}
		for _, m := range prov.Models {
			have[strings.TrimSpace(m)] = true
		}
		changed := false
		for _, d := range discovered {
			d = strings.TrimSpace(d)
			if d != "" && !have[d] {
				have[d] = true
				prov.Models = append(prov.Models, d)
				changed = true
			}
		}
		if changed {
			_ = s.store.UpsertCustomProvider(ctx, prov)
		}
	}
	now := storage.Now()
	caps := make([]storage.ModelCapability, 0, len(ordered))
	for _, slug := range ordered {
		caps = append(caps, storage.ModelCapability{
			AccountID:                     account.ID,
			ModelSlug:                     slug,
			EffectiveContextWindowPercent: 100,
			Visibility:                    "list",
			Source:                        "custom:" + providerID,
			LastProbeAt:                   now,
		})
	}
	if len(caps) == 0 {
		return nil, nil
	}
	if err := s.store.UpsertCapabilities(ctx, caps); err != nil {
		return nil, err
	}
	return caps, nil
}

func (s *Server) adminProbeModels(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	caps, err := s.probeAccountModels(r.Context(), account)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "capabilities": caps})
}

// StartBackground launches the periodic model-capability probe sweep. It returns
// immediately; the sweep runs until ctx is cancelled (on server shutdown). When
// ModelProbeIntervalHours is 0 the background refresh is disabled (imports and the
// manual admin probe still work).
func (s *Server) StartBackground(ctx context.Context) {
	// Cooldown→health-recheck loop runs independently of the model-probe sweep (it
	// must work even when model probing is disabled).
	s.startRecheckLoop(ctx)
	s.startBillingHoldExpiryLoop(ctx)
	s.startLogRetentionLoop(ctx)
	interval := time.Duration(s.cfg.ModelProbeIntervalHours) * time.Hour
	if interval <= 0 {
		return
	}
	supervisor.Go(ctx, "model-probe", func(ctx context.Context) {
		s.probeAllAccounts(ctx)
		s.probeLoop(ctx, interval)
	})
}

func (s *Server) probeLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeAllAccounts(ctx)
		}
	}
}

// probeAllAccounts re-probes every active account's upstream model list, spreading
// the probes out (small stagger) to avoid a thundering herd against the upstream
// and the shared egress. Every blocking point selects on ctx.Done() so the sweep
// never blocks shutdown.
func (s *Server) probeAllAccounts(ctx context.Context) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		log.Printf("model probe sweep: list accounts: %v", err)
		return
	}
	active := activeModelProbeAccounts(accounts)
	if len(active) == 0 {
		log.Printf("model probe sweep: probed=0 failed=0")
		return
	}
	accountIDs := accountIDsFromAccounts(active)
	tokens, err := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("model probe sweep: list tokens: %v", err)
		return
	}
	bindings, err := s.store.ListEgressBindingsByAccountIDs(ctx, accountIDs)
	if err != nil {
		log.Printf("model probe sweep: list egress bindings: %v", err)
		return
	}
	profiles, err := s.store.ListEgressProfiles(ctx)
	if err != nil {
		log.Printf("model probe sweep: list egress profiles: %v", err)
		return
	}
	profilesByID := modelProbeEgressProfilesByID(profiles)
	const stagger = 3 * time.Second
	probed, failed := 0, 0
	for _, account := range active {
		if ctx.Err() != nil {
			return
		}
		token, ok := tokens[account.ID]
		if !ok {
			failed++
			continue
		}
		binding, ok := bindings[account.ID]
		if !ok {
			failed++
			continue
		}
		egress, ok := profilesByID[binding.PrimaryEgressID]
		if !ok {
			failed++
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout())
		_, perr := s.probeAccountModelsWithDeps(cctx, account, token, binding, egress)
		cancel()
		if perr != nil {
			failed++
		} else {
			probed++
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(stagger):
		}
	}
	log.Printf("model probe sweep: probed=%d failed=%d", probed, failed)
}

func activeModelProbeAccounts(accounts []storage.Account) []storage.Account {
	active := make([]storage.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.Status == "active" {
			active = append(active, account)
		}
	}
	return active
}

func accountIDsFromAccounts(accounts []storage.Account) []string {
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

func modelProbeEgressProfilesByID(profiles []storage.EgressProfile) map[string]storage.EgressProfile {
	out := make(map[string]storage.EgressProfile, len(profiles))
	for _, profile := range profiles {
		if strings.TrimSpace(profile.ID) == "" {
			continue
		}
		out[profile.ID] = profile
	}
	return out
}

func (s *Server) adminRefresh(w http.ResponseWriter, r *http.Request, accountID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	token, err := s.store.GetToken(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	account, err := s.store.GetAccount(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	switch provider := s.accountProvider(account, token); {
	case provider == "claude":
		s.refreshClaude(w, r, token)
		return
	case provider == "kiro":
		cred, err := s.store.GetKiroCredentials(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		binding, err := s.store.GetEgressBinding(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		egress, err := s.store.GetEgressProfile(r.Context(), binding.PrimaryEgressID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		s.kiro.UpdateConfig(s.effectiveKiroConfig(r.Context()))
		if _, _, _, err = s.kiro.Prepare(r.Context(), account, cred, token, egress, true); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": true, "method": "kiro"})
		return
	case upstream.IsCustomProvider(provider):
		// Custom OpenAI-compatible providers authenticate with a static API key — there
		// is nothing to refresh.
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": false, "reason": "custom provider uses a static API key"})
		return
	}
	refreshed, err := s.refreshCodexToken(r.Context(), token)
	if err != nil {
		s.handleCodexRefreshFailure(r.Context(), account, refreshed, err, "admin_refresh")
		if refreshed.StatusCode >= 400 {
			writeRaw(w, refreshed.StatusCode, refreshed.Header, refreshed.Body)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !refreshed.Refreshed {
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": false, "reason": refreshed.Reason})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": accountID, "refreshed": true, "method": refreshed.Method})
}

type codexRefreshResult struct {
	Token               storage.AccountToken
	Refreshed           bool
	Method              string
	Reason              string
	TerminalAuthFailure bool
	StatusCode          int
	Header              http.Header
	Body                []byte
}

func (s *Server) refreshCodexToken(ctx context.Context, token storage.AccountToken) (codexRefreshResult, error) {
	result := codexRefreshResult{Token: token}
	tokenURL := firstNonEmpty(s.cfg.OAuthTokenURL, s.cfg.CodexOAuthTokenURL)
	if tokenURL == "" || token.RefreshToken == "" {
		// Cookie-imported "AT" accounts (no refresh_token) refresh by re-minting
		// the access token from the stored chatgpt.com session cookie.
		if cookie, _ := s.store.GetSessionCookie(ctx, token.AccountID); cookie != "" {
			if accessToken, ferr := fetchChatGPTSessionToken(ctx, cookie); ferr == nil && accessToken != "" {
				token.AccessToken = accessToken
				token.LastRefresh = storage.Now()
				if err := s.store.UpdateToken(ctx, token); err != nil {
					return result, err
				}
				result.Token = token
				result.Refreshed = true
				result.Method = "session_cookie"
				return result, nil
			}
		}
		token.LastRefresh = storage.Now()
		_ = s.store.UpdateToken(ctx, token)
		result.Token = token
		result.Reason = "no refresh_token and no session cookie to re-mint from"
		s.enqueueCodexReauthIfEligible(ctx, token.AccountID, result.Reason)
		return result, nil
	}
	// OAuth refresh (web-login or auth.json accounts). OpenAI requires client_id +
	// scope alongside the refresh_token grant; older configs that only set
	// OAuthTokenURL still work (it takes precedence over the CodexOAuth default).
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", token.RefreshToken)
	form.Set("client_id", s.cfg.CodexOAuthClientID)
	form.Set("scope", firstNonEmpty(s.cfg.CodexOAuthScope, config.DefaultCodexOAuthScope))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgent)
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		result.StatusCode = resp.StatusCode
		result.Header = resp.Header.Clone()
		result.Body = raw
		result.Reason, result.TerminalAuthFailure = codexRefreshFailureReason(resp.StatusCode, raw)
		if result.TerminalAuthFailure {
			if cookie, _ := s.store.GetSessionCookie(ctx, token.AccountID); cookie != "" {
				if accessToken, ferr := fetchChatGPTSessionToken(ctx, cookie); ferr == nil && accessToken != "" {
					token.AccessToken = accessToken
					token.LastRefresh = storage.Now()
					if err := s.store.UpdateToken(ctx, token); err != nil {
						return result, err
					}
					result.Token = token
					result.Refreshed = true
					result.Method = "session_cookie"
					return result, nil
				}
			}
			s.enqueueCodexReauthIfEligible(ctx, token.AccountID, result.Reason)
		}
		return result, fmt.Errorf("openai token refresh failed (%d): %s", resp.StatusCode, bodySnippet(raw, 300))
	}
	var refreshed map[string]interface{}
	if err := json.Unmarshal(raw, &refreshed); err != nil {
		return result, err
	}
	if access, ok := refreshed["access_token"].(string); ok && access != "" {
		token.AccessToken = access
	} else {
		return result, errors.New("openai oauth response had no access_token")
	}
	if refresh, ok := refreshed["refresh_token"].(string); ok && refresh != "" {
		token.RefreshToken = refresh
	}
	token.LastRefresh = storage.Now()
	if err := s.store.UpdateToken(ctx, token); err != nil {
		return result, err
	}
	result.Token = token
	result.Refreshed = true
	result.Method = "openai_oauth"
	return result, nil
}

func codexRefreshFailureReason(status int, body []byte) (string, bool) {
	hay := strings.ToLower(string(body))
	for _, sig := range []string{
		"refresh_token_invalidated",
		"refresh token has been invalidated",
		"refresh_token_expired",
		"refresh_token_reused",
		"invalid_grant",
		"invalid_scope",
		"insufficient_scope",
		"please try signing in again",
	} {
		if strings.Contains(hay, sig) {
			return sig, true
		}
	}
	if (status == http.StatusBadRequest || status == http.StatusUnauthorized) &&
		strings.Contains(hay, "refresh") &&
		(strings.Contains(hay, "invalid") || strings.Contains(hay, "revoked")) {
		return "invalid_refresh_token", true
	}
	return "http_" + strconv.Itoa(status), false
}

func (s *Server) handleCodexRefreshFailure(ctx context.Context, account storage.Account, result codexRefreshResult, err error, source string) {
	reason := firstNonEmpty(result.Reason, "refresh_failed")
	if result.TerminalAuthFailure {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			AccountID:    account.ID,
			AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
			Action:       "auth_quarantine",
			State:        string(ban.AuthExpired),
			Reason:       reason,
			Detail:       fmt.Sprintf("source=%s http=%d error=%v body=%s", source, result.StatusCode, err, bodySnippet(result.Body, 600)),
		})
		_ = s.store.SetAccountQuarantine(ctx, account.ID, storage.Now()+int64((30*24*time.Hour)/time.Second), "auth refresh failed: "+reason+"; re-login required")
		return
	}
	// Non-terminal (transient) refresh failure: bench-for-recheck so the account is
	// pulled from the pool and only restored once a probe (which re-attempts the
	// refresh) confirms it recovered.
	_ = s.store.BenchBindingForRecheck(ctx, account.ID, storage.Now()+int64((5*time.Minute)/time.Second))
}

// refreshClaude refreshes a Claude OAuth (sk-ant-oat) access token via the
// Anthropic OAuth token endpoint. API keys (sk-ant-api) are static, so there is
// nothing to refresh for them.
func (s *Server) refreshClaude(w http.ResponseWriter, r *http.Request, token storage.AccountToken) {
	account, err := s.store.GetAccount(r.Context(), token.AccountID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if !claudeTokenCanRefresh(token) {
		token.LastRefresh = storage.Now()
		_ = s.store.UpdateToken(r.Context(), token)
		writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": token.AccountID, "refreshed": false, "reason": "api key or missing refresh_token; nothing to refresh"})
		return
	}
	if _, err := s.forceRefreshClaudeToken(r.Context(), account, "admin_refresh"); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"account_id": token.AccountID, "refreshed": true, "method": "anthropic_oauth"})
}
