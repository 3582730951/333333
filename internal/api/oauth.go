package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

// oauth.go implements the web-login (paste-back) account import flow for both
// providers: the server mints an OAuth Authorization-Code + PKCE login URL, the
// operator opens it in a browser and logs in, and after the upstream redirects
// (the address bar changes, or claude.ai prints a "code#state") the operator
// pastes the redirected URL / code back. We then exchange it for tokens and add
// the account to the pool. The relay runs headless, so there is no localhost
// callback server to catch the redirect — the manual paste IS the callback.
//
// Both authorize/token endpoints, client ids, redirect URIs and scopes come from
// config (config.go defaults mirror the official clients), so a rotation upstream
// is a config edit, not a recompile.

// oauthUserAgent is sent on the token-exchange calls. Matches the UA already used
// by fetchChatGPTSessionToken so the relay presents one consistent browser shape.
const oauthUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	antigravityUserInfoURL     = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
	antigravityControlPlaneURL = "https://cloudcode-pa.googleapis.com"
	antigravityDailyControlURL = "https://daily-cloudcode-pa.googleapis.com"
)

const (
	antigravityRuntimeUserAgent = "antigravity/hub/2.2.1 darwin/arm64"
	antigravityOnboardUserAgent = antigravityRuntimeUserAgent + " google-api-nodejs-client/10.3.0"
	antigravityGoogAPIClient    = "gl-node/22.21.1"
)

// oauthSessionTTL bounds how long a generated login URL stays completable. The
// flow takes a minute or two; 15 min is generous while keeping the in-memory
// store tiny and self-cleaning.
const oauthSessionTTL = 15 * time.Minute

// oauthPending is the server-side half of one in-flight login: the PKCE verifier
// and state must never leave the server, and are matched back up when the
// operator pastes the code.
type oauthPending struct {
	provider          string
	verifier          string
	state             string
	created           time.Time
	reauthAccountID   string
	targetWorkspaceID string
	groupName         string
	egressID          string
	// importEgressID is set only for the deprecated explicit-account outlet input.
	// A group-inherited outlet is intentionally not copied into the account row.
	importEgressID string
}

// oauthStore is a tiny TTL map of in-flight logins keyed by an opaque session id.
// A single relay process serves the whole flow, so process memory is sufficient;
// a restart mid-login just means the operator clicks "generate" again. It is
// goroutine-safe and self-purges expired entries on every access.
type oauthStore struct {
	mu  sync.Mutex
	m   map[string]oauthPending
	ttl time.Duration
}

func newOAuthStore(ttl time.Duration) *oauthStore {
	return &oauthStore{m: make(map[string]oauthPending), ttl: ttl}
}

func (s *oauthStore) put(id string, p oauthPending) { s.putAt(id, p, time.Now()) }

func (s *oauthStore) putAt(id string, p oauthPending, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	p.created = now
	s.m[id] = p
}

// take returns and removes a pending login (single-use). ok is false when the id
// is unknown or has expired.
func (s *oauthStore) take(id string) (oauthPending, bool) { return s.takeAt(id, time.Now()) }

func (s *oauthStore) takeAt(id string, now time.Time) (oauthPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	p, ok := s.m[id]
	if ok {
		delete(s.m, id)
	}
	return p, ok
}

func (s *oauthStore) gcLocked(now time.Time) {
	for k, v := range s.m {
		if now.Sub(v.created) > s.ttl {
			delete(s.m, k)
		}
	}
}

// oauthProviderDesc is the resolved OAuth configuration for one provider.
type oauthProviderDesc struct {
	provider    string // canonical: "codex" | "claude"
	authURL     string
	tokenURL    string
	clientID    string
	redirectURI string
	scope       string
}

// oauthProvider resolves a (possibly aliased) provider name to its configured
// OAuth descriptor. Unknown names are rejected.
func (s *Server) oauthProvider(name string) (oauthProviderDesc, error) {
	switch name {
	case "codex", "openai", "chatgpt":
		return oauthProviderDesc{
			provider:    "codex",
			authURL:     s.cfg.CodexOAuthAuthURL,
			tokenURL:    s.cfg.CodexOAuthTokenURL,
			clientID:    s.cfg.CodexOAuthClientID,
			redirectURI: s.cfg.CodexOAuthRedirectURI,
			scope:       s.cfg.CodexOAuthScope,
		}, nil
	case "claude", "anthropic":
		return oauthProviderDesc{
			provider:    "claude",
			authURL:     s.cfg.ClaudeOAuthAuthURL,
			tokenURL:    s.cfg.ClaudeOAuthTokenURL,
			clientID:    s.cfg.ClaudeOAuthClientID,
			redirectURI: s.cfg.ClaudeOAuthRedirectURI,
			scope:       s.cfg.ClaudeOAuthScope,
		}, nil
	case "antigravity", "google":
		return oauthProviderDesc{
			provider:    "antigravity",
			authURL:     s.cfg.AntigravityOAuthAuthURL,
			tokenURL:    s.cfg.AntigravityOAuthTokenURL,
			clientID:    s.cfg.AntigravityOAuthClientID,
			redirectURI: s.cfg.AntigravityOAuthRedirectURI,
			scope:       s.cfg.AntigravityOAuthScope,
		}, nil
	default:
		return oauthProviderDesc{}, fmt.Errorf("unknown provider %q (use codex, claude, or antigravity)", name)
	}
}

// authorizeURL builds the browser login URL with the PKCE challenge + state. The
// provider-specific params mirror the official clients: Codex sends exactly what
// the current Codex CLI sends (other_codex login/src/server.rs build_authorize_url);
// Claude sends code=true so claude.ai also prints the code on screen for the
// paste-back.
type oauthAuthorizeOptions struct {
	AllowedWorkspaceID string
}

func (d oauthProviderDesc) authorizeURL(challenge, state string) string {
	return d.authorizeURLWithOptions(challenge, state, oauthAuthorizeOptions{})
}

func (d oauthProviderDesc) authorizeURLWithOptions(challenge, state string, opts oauthAuthorizeOptions) string {
	params := url.Values{
		"client_id":     {d.clientID},
		"response_type": {"code"},
		"redirect_uri":  {d.redirectURI},
		"scope":         {d.scope},
		"state":         {state},
	}
	if d.provider != "antigravity" {
		params.Set("code_challenge", challenge)
		params.Set("code_challenge_method", "S256")
	}
	if d.provider == "codex" {
		// Make this authorize request BYTE-FOR-BYTE identical to the real Codex CLI
		// (ground truth: codex-rs login/src/server.rs build_authorize_url). Any extra
		// or unrecognized param makes auth.openai.com reject the request and 302 an
		// error straight back to redirect_uri — which the operator sees as "open the
		// link → it instantly jumps to localhost:1455, no login screen".
		//
		// Two params were doing exactly that and are NOT sent by the real client:
		//   - scope's trailing `api.responses.write` — NOT granted to this client_id;
		//     an illegal scope is rejected outright (removed in config.go).
		//   - `prompt=login` — the real CLI omits it; sending it triggered the same
		//     error-redirect. (An earlier session added it to force account choice;
		//     that was the wrong lever and is reverted here.)
		//
		// What the real client DOES send (and we keep): originator=codex_cli_rs,
		// id_token_add_organizations, codex_cli_simplified_flow, plus the
		// api.connectors.read/invoke scope (the corrected default in config).
		params.Set("id_token_add_organizations", "true")
		params.Set("codex_cli_simplified_flow", "true")
		params.Set("originator", identity.CodexOriginator)
		if allowed := strings.TrimSpace(opts.AllowedWorkspaceID); allowed != "" {
			params.Set("allowed_workspace_id", allowed)
		}
	} else if d.provider == "antigravity" {
		params.Set("access_type", "offline")
		params.Set("prompt", "consent")
	} else {
		params.Set("code", "true")
	}
	return d.authURL + "?" + params.Encode()
}

// generatePKCE returns a fresh (verifier, S256 challenge) pair. The verifier is
// 64 random bytes base64url-encoded (no padding); the challenge is the
// base64url-no-pad SHA-256 of the verifier — exactly what the reference clients
// and the OAuth spec require.
func generatePKCE() (verifier, challenge string, err error) {
	verifier, err = randomToken(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// randomToken returns n cryptographically-random bytes, base64url-no-pad encoded.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// parseRedirected extracts the authorization code and (optional) state from
// whatever the operator pasted back, covering both providers' "the URL changed"
// experience:
//   - a full redirect URL (OpenAI redirects to http://localhost:1455/... which
//     fails to load but leaves ?code=&state= in the address bar) -> read query
//     params, and also handle fragment params / "?code=abc#state";
//   - a raw query string ("code=..." or "?code=...");
//   - a "code#state" string (claude.ai prints this when code=true);
//   - a bare code.
func parseRedirected(raw string) (code, state string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			q := u.Query()
			code = strings.TrimSpace(q.Get("code"))
			state = strings.TrimSpace(q.Get("state"))
			if fragment := strings.TrimSpace(u.Fragment); fragment != "" {
				if fq, err := url.ParseQuery(fragment); err == nil {
					if code == "" {
						code = strings.TrimSpace(fq.Get("code"))
					}
					if state == "" {
						state = strings.TrimSpace(fq.Get("state"))
					}
				}
				if state == "" && code != "" && !strings.Contains(fragment, "=") {
					state = fragment
				}
			}
			code, inlineState := splitCodeAndState(code)
			if state == "" {
				state = inlineState
			}
			return code, state
		}
	}
	if strings.Contains(raw, "=") {
		query := strings.TrimPrefix(raw, "?")
		if q, err := url.ParseQuery(query); err == nil {
			code = strings.TrimSpace(q.Get("code"))
			state = strings.TrimSpace(q.Get("state"))
			if code != "" || state != "" {
				code, inlineState := splitCodeAndState(code)
				if state == "" {
					state = inlineState
				}
				return code, state
			}
		}
	}
	return splitCodeAndState(raw)
}

func splitCodeAndState(raw string) (code, state string) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "#"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return raw, ""
}

// adminOAuthStart mints a login URL.
//
//	POST /admin/oauth/start  {provider:"codex"|"claude"}
//	  -> {session_id, provider, auth_url, expires_in}
func (s *Server) adminOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Provider           string `json:"provider"`
		AllowedWorkspaceID string `json:"allowed_workspace_id"`
		GroupName          string `json:"group_name"`
		EgressID           string `json:"egress_id"`
		PrimaryEgressID    string `json:"primary_egress_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	desc, err := s.oauthProvider(strings.ToLower(strings.TrimSpace(req.Provider)))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	verifier, challenge := "", ""
	if desc.provider != "antigravity" {
		verifier, challenge, err = generatePKCE()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
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
	pending := oauthPending{provider: desc.provider, verifier: verifier, state: state}
	if desc.provider == "antigravity" && (strings.TrimSpace(req.GroupName) != "" || requestedImportEgressID(req.EgressID, req.PrimaryEgressID) != "") {
		explicitEgressID := requestedImportEgressID(req.EgressID, req.PrimaryEgressID)
		egress, groupName, resolveErr := s.resolveAntigravityOAuthEgress(r.Context(), req.GroupName, explicitEgressID)
		if resolveErr != nil {
			writeError(w, http.StatusBadRequest, resolveErr)
			return
		}
		pending.groupName = groupName
		pending.egressID = egress.ID
		pending.importEgressID = explicitEgressID
	}
	s.oauth.put(sid, pending)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sid,
		"provider":   desc.provider,
		"auth_url":   desc.authorizeURLWithOptions(challenge, state, oauthAuthorizeOptions{AllowedWorkspaceID: req.AllowedWorkspaceID}),
		"expires_in": int(s.oauth.ttl.Seconds()),
	})
}

// adminOAuthComplete finishes a login: it matches the pasted code back to the
// pending PKCE session, exchanges it for tokens, and adds the account to the pool
// via the shared saveImportedAccount path (provider is inferred from the token).
//
//	POST /admin/oauth/complete  {session_id, redirected, label?, group_name?, egress_id?}
//	  -> the created account
func (s *Server) adminOAuthComplete(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		SessionID       string `json:"session_id"`
		Redirected      string `json:"redirected"`
		Label           string `json:"label"`
		GroupName       string `json:"group_name"`
		EgressID        string `json:"egress_id"`
		PrimaryEgressID string `json:"primary_egress_id"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.Redirected) == "" {
		writeError(w, http.StatusBadRequest, errors.New("session_id and redirected are required"))
		return
	}
	pend, ok := s.oauth.take(req.SessionID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("登录会话已过期或不存在，请重新生成登录链接"))
		return
	}
	code, state := parseRedirected(req.Redirected)
	if code == "" {
		writeError(w, http.StatusBadRequest, errors.New("未能从粘贴内容中解析出授权码（请粘贴登录后地址栏的完整网址，或页面显示的 code）"))
		return
	}
	// CSRF: when the paste carried a state, it must match the one we issued.
	if state != "" && pend.state != "" && state != pend.state {
		writeError(w, http.StatusBadRequest, errors.New("state 不匹配，可能不是本次登录的回调，请重新登录"))
		return
	}
	desc, err := s.oauthProvider(pend.provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var parsed authparse.ParsedAuth
	groupName := strings.TrimSpace(req.GroupName)
	importEgressID := requestedImportEgressID(req.EgressID, req.PrimaryEgressID)
	switch pend.provider {
	case "codex":
		parsed, err = s.exchangeCodexCode(r.Context(), desc, code, pend.verifier)
	case "claude":
		parsed, err = s.exchangeClaudeCode(r.Context(), desc, code, firstNonEmpty(state, pend.state), pend.verifier)
	case "antigravity":
		if pend.groupName != "" {
			groupName = pend.groupName
		}
		if pend.importEgressID != "" {
			importEgressID = pend.importEgressID
		}
		routeEgressID := pend.egressID
		if routeEgressID == "" {
			routeEgressID = importEgressID
		}
		var egress storage.EgressProfile
		egress, groupName, err = s.resolveAntigravityOAuthEgress(r.Context(), groupName, routeEgressID)
		if err == nil {
			parsed, err = s.exchangeAntigravityCode(r.Context(), code, desc.redirectURI, egress, "oauth:antigravity:"+req.SessionID)
		}
	default:
		err = fmt.Errorf("unknown provider %q", pend.provider)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	account, err := s.saveImportedAccount(r.Context(), parsed, req.Label, groupName, "", "", importEgressID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, account)
}

var apiExternalHTTPClient = &http.Client{Timeout: 60 * time.Second}

// oauthHTTPClient is the client used for token exchange. The token endpoints are
// reached directly (the same way the shipped refreshClaude call reaches the
// Anthropic OAuth endpoint); they are not the relayed inference hosts and need no
// proxy/sidecar. A finite timeout guards against a hung exchange.
func oauthHTTPClient() *http.Client {
	return apiExternalHTTPClient
}

// exchangeCodexCode runs the OpenAI authorization-code → tokens exchange
// (form-urlencoded) and parses the resulting id_token for account metadata.
func (s *Server) exchangeCodexCode(ctx context.Context, d oauthProviderDesc, code, verifier string) (authparse.ParsedAuth, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {d.clientID},
		"code":          {code},
		"redirect_uri":  {d.redirectURI},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgent)
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return authparse.ParsedAuth{}, fmt.Errorf("openai token exchange failed (%d): %s", resp.StatusCode, bodySnippet(body, 300))
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return authparse.ParsedAuth{}, fmt.Errorf("parse token response: %w", err)
	}
	return authparse.ParseOAuthCodex(tr.AccessToken, tr.RefreshToken, tr.IDToken)
}

// exchangeClaudeCode runs the Anthropic authorization-code → tokens exchange
// (JSON). The access token is an opaque sk-ant-oat string.
func (s *Server) exchangeClaudeCode(ctx context.Context, d oauthProviderDesc, code, state, verifier string) (authparse.ParsedAuth, error) {
	if parsedCode, parsedState := splitCodeAndState(code); parsedCode != "" {
		code = parsedCode
		if parsedState != "" {
			state = parsedState
		}
	}
	payload := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     d.clientID,
		"code":          code,
		"redirect_uri":  d.redirectURI,
		"code_verifier": verifier,
	}
	if state != "" {
		payload["state"] = state
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.tokenURL, bytes.NewReader(raw))
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", oauthUserAgent)
	resp, err := oauthHTTPClient().Do(req)
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return authparse.ParsedAuth{}, fmt.Errorf("anthropic token exchange failed (%d): %s", resp.StatusCode, bodySnippet(body, 300))
	}
	var tr struct {
		AccessToken      string   `json:"access_token"`
		RefreshToken     string   `json:"refresh_token"`
		ExpiresIn        int64    `json:"expires_in"`
		ExpiresAt        int64    `json:"expires_at"`
		Scope            string   `json:"scope"`
		Scopes           []string `json:"scopes"`
		SubscriptionType string   `json:"subscription_type"`
		RateLimitTier    string   `json:"rate_limit_tier"`
		Account          struct {
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		ClaudeAiOauth struct {
			SubscriptionType string   `json:"subscriptionType"`
			RateLimitTier    string   `json:"rateLimitTier"`
			Scopes           []string `json:"scopes"`
			ExpiresAt        int64    `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return authparse.ParsedAuth{}, fmt.Errorf("parse token response: %w", err)
	}
	expiresAt := tr.ExpiresAt
	if expiresAt == 0 {
		expiresAt = tr.ClaudeAiOauth.ExpiresAt
	}
	if expiresAt > 1_000_000_000_000 {
		expiresAt /= 1000
	}
	if expiresAt == 0 && tr.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + tr.ExpiresIn
	}
	scopes := tr.Scopes
	if len(scopes) == 0 && tr.Scope != "" {
		scopes = strings.Fields(tr.Scope)
	}
	if len(scopes) == 0 {
		scopes = tr.ClaudeAiOauth.Scopes
	}
	subscriptionType := firstNonEmpty(tr.SubscriptionType, tr.ClaudeAiOauth.SubscriptionType)
	rateLimitTier := firstNonEmpty(tr.RateLimitTier, tr.ClaudeAiOauth.RateLimitTier)
	return authparse.ParseOAuthClaudeMetadata(tr.AccessToken, tr.RefreshToken, tr.Account.EmailAddress, subscriptionType, rateLimitTier, expiresAt, scopes)
}

type antigravityOAuthRawRequest func(context.Context, string, string, http.Header, []byte) (*upstream.Response, error)

func (s *Server) resolveAntigravityOAuthEgress(ctx context.Context, groupName, requestedEgressID string) (storage.EgressProfile, string, error) {
	groupName = strings.TrimSpace(groupName)
	if groupName == "" {
		groupName = strings.TrimSpace(s.cfg.DefaultGroup)
	}
	if groupName == "" {
		groupName = "cyber"
	}
	group, err := s.store.GetGroup(ctx, groupName)
	if err != nil {
		return storage.EgressProfile{}, "", fmt.Errorf("account pool group %q was not found", groupName)
	}
	egressID := strings.TrimSpace(requestedEgressID)
	if egressID == "" && len(group.EgressIDs) > 0 {
		egressID = strings.TrimSpace(group.EgressIDs[0])
	}
	if egressID == "" {
		egressID = storage.DefaultDirectEgressID
	}
	egress, err := s.store.GetEgressProfile(ctx, egressID)
	if err != nil {
		return storage.EgressProfile{}, "", fmt.Errorf("egress %q was not found", egressID)
	}
	return egress, groupName, nil
}

func (s *Server) antigravityOAuthRequester(egress storage.EgressProfile, cookieJarKey string) antigravityOAuthRawRequest {
	return func(ctx context.Context, method, target string, headers http.Header, body []byte) (*upstream.Response, error) {
		if s.upstream != nil {
			return s.upstream.DoRawHTTP1(ctx, egress, method, target, headers, body, cookieJarKey)
		}
		// Focused unit tests and deliberately minimal embedders may omit the upstream
		// client. Production Server construction always takes the egress-aware path.
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = headers.Clone()
		resp, err := oauthHTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		return &upstream.Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: resp.Body}, nil
	}
}

// exchangeAntigravityCode runs the Google OAuth2 authorization-code → tokens exchange
// for Antigravity credentials. Every server-side login request uses the account-pool
// group's selected outlet, matching later model discovery, refresh, and inference.
func (s *Server) exchangeAntigravityCode(ctx context.Context, code, redirectURI string, egress storage.EgressProfile, cookieJarKey string) (authparse.ParsedAuth, error) {
	do := s.antigravityOAuthRequester(egress, cookieJarKey)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.cfg.AntigravityOAuthClientID},
		"client_secret": {s.cfg.AntigravityOAuthClientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/x-www-form-urlencoded")
	headers.Set("Accept", "application/json")
	resp, err := do(ctx, http.MethodPost, s.cfg.AntigravityOAuthTokenURL, headers, []byte(form.Encode()))
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return authparse.ParsedAuth{}, fmt.Errorf("google token exchange failed (%d): %s", resp.StatusCode, bodySnippet(body, 300))
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		TokenType    string `json:"token_type"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return authparse.ParsedAuth{}, fmt.Errorf("parse token response: %w", err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return authparse.ParsedAuth{}, errors.New("google token exchange returned no access_token")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	email, err := fetchAntigravityUserInfo(ctx, tr.AccessToken, do)
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	projectID, err := discoverAntigravityProject(ctx, tr.AccessToken, do)
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	return authparse.ParseOAuthAntigravity(tr.AccessToken, tr.RefreshToken, email, projectID, time.Now().Unix()+expiresIn, strings.Fields(tr.Scope))
}

func fetchAntigravityUserInfo(ctx context.Context, accessToken string, do antigravityOAuthRawRequest) (string, error) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", antigravityRuntimeUserAgent)
	resp, err := do(ctx, http.MethodGet, antigravityUserInfoURL, headers, nil)
	if err != nil {
		return "", fmt.Errorf("antigravity userinfo request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read antigravity userinfo: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("antigravity userinfo failed (%d): %s", resp.StatusCode, bodySnippet(body, 300))
	}
	var info struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("parse antigravity userinfo: %w", err)
	}
	if strings.TrimSpace(info.Email) == "" {
		return "", errors.New("antigravity userinfo returned no email")
	}
	return strings.TrimSpace(info.Email), nil
}

func discoverAntigravityProject(ctx context.Context, accessToken string, do antigravityOAuthRawRequest) (string, error) {
	body, err := antigravityControlPlaneRequest(ctx, accessToken, antigravityControlPlaneURL+"/v1internal:loadCodeAssist", antigravityRuntimeUserAgent, "", map[string]interface{}{
		"metadata": map[string]string{"ideType": "ANTIGRAVITY"},
	}, do)
	if err != nil {
		return "", fmt.Errorf("antigravity loadCodeAssist: %w", err)
	}
	var loaded map[string]interface{}
	if err := json.Unmarshal(body, &loaded); err != nil {
		return "", fmt.Errorf("parse antigravity loadCodeAssist: %w", err)
	}
	if projectID := antigravityProjectID(loaded); projectID != "" {
		return projectID, nil
	}
	tierID := antigravityDefaultTierID(loaded)
	version := "2.2.1"
	for attempt := 0; attempt < 5; attempt++ {
		body, err = antigravityControlPlaneRequest(ctx, accessToken, antigravityDailyControlURL+"/v1internal:onboardUser", antigravityOnboardUserAgent, antigravityGoogAPIClient, map[string]interface{}{
			"tier_id": tierID,
			"metadata": map[string]string{
				"ide_type": "ANTIGRAVITY", "ide_version": version, "ide_name": "antigravity",
			},
		}, do)
		if err != nil {
			return "", fmt.Errorf("antigravity onboardUser: %w", err)
		}
		var onboard map[string]interface{}
		if err := json.Unmarshal(body, &onboard); err != nil {
			return "", fmt.Errorf("parse antigravity onboardUser: %w", err)
		}
		if done, _ := onboard["done"].(bool); done {
			if response, _ := onboard["response"].(map[string]interface{}); response != nil {
				if projectID := antigravityProjectID(response); projectID != "" {
					return projectID, nil
				}
			}
			return "", errors.New("antigravity onboardUser completed without project_id")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", errors.New("antigravity onboardUser did not complete after 5 attempts")
}

func antigravityControlPlaneRequest(ctx context.Context, accessToken, endpoint, userAgent, googAPIClient string, payload interface{}, do antigravityOAuthRawRequest) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	headers.Set("Accept", "*/*")
	headers.Set("Content-Type", "application/json")
	headers.Set("User-Agent", userAgent)
	if googAPIClient != "" {
		headers.Set("X-Goog-Api-Client", googAPIClient)
	}
	resp, err := do(ctx, http.MethodPost, endpoint, headers, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, bodySnippet(body, 300))
	}
	return body, nil
}

func antigravityProjectID(data map[string]interface{}) string {
	for _, key := range []string{"cloudaicompanionProject", "projectId", "project"} {
		switch value := data[key].(type) {
		case string:
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		case map[string]interface{}:
			if id, _ := value["id"].(string); strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	return ""
}

func antigravityDefaultTierID(data map[string]interface{}) string {
	if tiers, ok := data["allowedTiers"].([]interface{}); ok {
		for _, rawTier := range tiers {
			tier, _ := rawTier.(map[string]interface{})
			isDefault, _ := tier["isDefault"].(bool)
			id, _ := tier["id"].(string)
			if isDefault && strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	if tier, _ := data["currentTier"].(map[string]interface{}); tier != nil {
		if id, _ := tier["id"].(string); strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return "free-tier"
}
