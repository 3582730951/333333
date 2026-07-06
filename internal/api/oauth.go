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
	default:
		return oauthProviderDesc{}, fmt.Errorf("unknown provider %q (use codex or claude)", name)
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
		"client_id":             {d.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {d.redirectURI},
		"scope":                 {d.scope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
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
	s.oauth.put(sid, oauthPending{provider: desc.provider, verifier: verifier, state: state})
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
	switch pend.provider {
	case "codex":
		parsed, err = s.exchangeCodexCode(r.Context(), desc, code, pend.verifier)
	case "claude":
		parsed, err = s.exchangeClaudeCode(r.Context(), desc, code, firstNonEmpty(state, pend.state), pend.verifier)
	default:
		err = fmt.Errorf("unknown provider %q", pend.provider)
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	account, err := s.saveImportedAccount(r.Context(), parsed, req.Label, req.GroupName, "", "", requestedImportEgressID(req.EgressID, req.PrimaryEgressID))
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
