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
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

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

// get returns a pending login without consuming it. Callers use it for parsing,
// ownership, and state validation before take atomically claims the valid callback.
func (s *oauthStore) get(id string) (oauthPending, bool) { return s.getAt(id, time.Now()) }

func (s *oauthStore) getAt(id string, now time.Time) (oauthPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(now)
	p, ok := s.m[id]
	return p, ok
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
	raw, _ := d.buildAuthorizeURLWithOptions(challenge, state, opts)
	return raw
}

// buildAuthorizeURLWithOptions validates the operator-configured endpoints and
// merges OAuth parameters into any existing query string. The old string
// concatenation produced `...?tenant=x?client_id=...` for configured endpoints
// that already carried a query, yielding a link that looked generated in the UI
// but could never be used.
func (d oauthProviderDesc) buildAuthorizeURLWithOptions(challenge, state string, opts oauthAuthorizeOptions) (string, error) {
	authEndpoint, err := url.Parse(strings.TrimSpace(d.authURL))
	if err != nil || authEndpoint.Host == "" || (authEndpoint.Scheme != "https" && authEndpoint.Scheme != "http") || authEndpoint.Fragment != "" {
		return "", errors.New("OAuth authorization endpoint must be an absolute HTTP(S) URL without a fragment")
	}
	if strings.TrimSpace(d.clientID) == "" {
		return "", errors.New("OAuth client id is empty")
	}
	tokenEndpoint, err := url.Parse(strings.TrimSpace(d.tokenURL))
	if err != nil || tokenEndpoint.Host == "" || (tokenEndpoint.Scheme != "https" && tokenEndpoint.Scheme != "http") || tokenEndpoint.Fragment != "" {
		return "", errors.New("OAuth token endpoint must be an absolute HTTP(S) URL without a fragment")
	}
	redirectURI := strings.TrimSpace(d.redirectURI)
	redirect, err := url.Parse(redirectURI)
	if err != nil || redirect.Host == "" || (redirect.Scheme != "https" && redirect.Scheme != "http") || redirect.Fragment != "" {
		return "", errors.New("OAuth redirect URI must be an absolute HTTP(S) URL without a fragment")
	}
	if strings.TrimSpace(state) == "" {
		return "", errors.New("OAuth state is empty")
	}
	if d.provider != "antigravity" && strings.TrimSpace(challenge) == "" {
		return "", errors.New("OAuth PKCE challenge is empty")
	}
	if d.provider == "claude" {
		// URLSearchParams insertion order is visible to the authorization endpoint.
		// Claude Code 2.1.226 emits this exact sequence from hqo().
		params := authEndpoint.Query()
		for _, key := range []string{"code", "client_id", "response_type", "redirect_uri", "scope", "code_challenge", "code_challenge_method", "state"} {
			params.Del(key)
		}
		ordered := []string{
			"code=" + url.QueryEscape("true"),
			"client_id=" + url.QueryEscape(strings.TrimSpace(d.clientID)),
			"response_type=" + url.QueryEscape("code"),
			"redirect_uri=" + url.QueryEscape(redirectURI),
			"scope=" + url.QueryEscape(d.scope),
			"code_challenge=" + url.QueryEscape(challenge),
			"code_challenge_method=" + url.QueryEscape("S256"),
			"state=" + url.QueryEscape(state),
		}
		if extras := params.Encode(); extras != "" {
			ordered = append(ordered, extras)
		}
		authEndpoint.RawQuery = strings.Join(ordered, "&")
		return authEndpoint.String(), nil
	}

	params := authEndpoint.Query()
	for key, value := range map[string]string{
		"client_id":     strings.TrimSpace(d.clientID),
		"response_type": "code",
		"redirect_uri":  redirectURI,
		"scope":         d.scope,
		"state":         state,
	} {
		params.Set(key, value)
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
	}
	authEndpoint.RawQuery = params.Encode()
	return authEndpoint.String(), nil
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

type oauthRedirected struct {
	Code             string
	State            string
	Error            string
	ErrorDescription string
}

// parseRedirected preserves the original helper contract for callers that only
// need code and state. New completion handlers use parseOAuthRedirected so an
// upstream authorization error is not mistaken for a malformed or bare code.
func parseRedirected(raw string) (code, state string) {
	parsed := parseOAuthRedirected(raw)
	return parsed.Code, parsed.State
}

// parseOAuthRedirected accepts the callback forms operators can realistically
// paste from a browser: a URL, a raw query, code#state, a bare code, a quoted or
// HTML-escaped value, surrounding browser error text containing the URL, or the
// complete Claude success-page text containing "Paste this into Claude Code:".
func parseOAuthRedirected(raw string) oauthRedirected {
	raw = trimOAuthCallbackWrapper(html.UnescapeString(strings.TrimSpace(raw)))
	if raw == "" {
		return oauthRedirected{}
	}
	candidates := make([]string, 0, 3)
	if embedded := embeddedOAuthCallbackURL(raw); embedded != "" && embedded != raw {
		candidates = append(candidates, embedded)
	}
	if authenticationCode := embeddedOAuthAuthenticationCode(raw); authenticationCode != "" && authenticationCode != raw {
		candidates = append(candidates, authenticationCode)
	}
	candidates = append(candidates, raw)
	for _, candidate := range candidates {
		if parsed, recognized := parseOAuthCallbackCandidate(candidate); recognized {
			return parsed
		}
	}
	return oauthRedirected{}
}

func parseOAuthCallbackCandidate(raw string) (oauthRedirected, bool) {
	raw = trimOAuthCallbackWrapper(strings.TrimSpace(raw))
	if raw == "" {
		return oauthRedirected{}, false
	}
	lower := strings.ToLower(raw)
	isURL := strings.Contains(lower, "://") || strings.HasPrefix(lower, "localhost:") || strings.HasPrefix(lower, "127.0.0.1:") || strings.HasPrefix(lower, "[::1]:")
	if isURL {
		candidate := raw
		if !strings.Contains(lower, "://") {
			candidate = "http://" + candidate
		}
		u, err := url.Parse(candidate)
		if err != nil || strings.TrimSpace(u.Host) == "" {
			return oauthRedirected{}, true
		}
		parsed := oauthRedirectedFromValues(u.Query())
		fragment := strings.TrimSpace(u.Fragment)
		if fragment != "" {
			if values, err := url.ParseQuery(fragment); err == nil {
				mergeOAuthRedirected(&parsed, oauthRedirectedFromValues(values))
			}
			if parsed.State == "" && parsed.Code != "" && !strings.Contains(fragment, "=") {
				parsed.State = fragment
			}
		}
		normalizeOAuthRedirected(&parsed)
		return parsed, true
	}
	if strings.Contains(raw, "=") {
		values, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err == nil && oauthCallbackValuesPresent(values) {
			parsed := oauthRedirectedFromValues(values)
			normalizeOAuthRedirected(&parsed)
			return parsed, true
		}
		if strings.HasPrefix(raw, "?") || strings.Contains(raw, "&") {
			return oauthRedirected{}, true
		}
	}
	if strings.IndexFunc(raw, unicode.IsSpace) >= 0 || strings.ContainsAny(raw, "<>\"'`") {
		return oauthRedirected{}, false
	}
	code, state := splitCodeAndState(raw)
	return oauthRedirected{Code: code, State: state}, true
}

func oauthCallbackValuesPresent(values url.Values) bool {
	for _, key := range []string{"code", "state", "error", "error_description"} {
		if _, ok := values[key]; ok {
			return true
		}
	}
	return false
}

func oauthRedirectedFromValues(values url.Values) oauthRedirected {
	return oauthRedirected{
		Code:             strings.TrimSpace(values.Get("code")),
		State:            strings.TrimSpace(values.Get("state")),
		Error:            strings.TrimSpace(values.Get("error")),
		ErrorDescription: strings.TrimSpace(values.Get("error_description")),
	}
}

func mergeOAuthRedirected(dst *oauthRedirected, src oauthRedirected) {
	if dst.Code == "" {
		dst.Code = src.Code
	}
	if dst.State == "" {
		dst.State = src.State
	}
	if dst.Error == "" {
		dst.Error = src.Error
	}
	if dst.ErrorDescription == "" {
		dst.ErrorDescription = src.ErrorDescription
	}
}

func normalizeOAuthRedirected(parsed *oauthRedirected) {
	parsed.Code = strings.TrimSpace(parsed.Code)
	parsed.State = strings.TrimSpace(parsed.State)
	parsed.Error = strings.TrimSpace(parsed.Error)
	parsed.ErrorDescription = strings.TrimSpace(parsed.ErrorDescription)
	code, inlineState := splitCodeAndState(parsed.Code)
	parsed.Code = code
	if parsed.State == "" {
		parsed.State = inlineState
	}
}

func trimOAuthCallbackWrapper(raw string) string {
	for {
		raw = strings.TrimSpace(raw)
		if len(raw) < 2 {
			return raw
		}
		first, last := raw[0], raw[len(raw)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') || (first == '`' && last == '`') {
			raw = raw[1 : len(raw)-1]
			continue
		}
		return raw
	}
}

func embeddedOAuthCallbackURL(raw string) string {
	lower := strings.ToLower(raw)
	start := -1
	for _, marker := range []string{"https://", "http://", "localhost:", "127.0.0.1:", "[::1]:"} {
		if i := strings.Index(lower, marker); i >= 0 && (start < 0 || i < start) {
			start = i
		}
	}
	if start < 0 {
		return ""
	}
	tail := raw[start:]
	end := len(tail)
	for i, r := range tail {
		if unicode.IsSpace(r) || strings.ContainsRune("<>\"'`", r) {
			end = i
			break
		}
	}
	return strings.TrimRight(tail[:end], ".,;)")
}

// embeddedOAuthAuthenticationCode extracts the value rendered by Claude's
// platform.claude.com OAuth success page. Operators commonly copy the whole block:
//
//	Authentication code
//	Paste this into Claude Code: CODE#STATE
//
// rather than selecting only CODE#STATE. Keep this label-specific so arbitrary
// prose is not reinterpreted as a credential. The returned token is fed back into
// parseOAuthCallbackCandidate, which applies the normal code#state validation.
func embeddedOAuthAuthenticationCode(raw string) string {
	lower := strings.ToLower(raw)
	for _, marker := range []string{"paste this into claude code", "authentication code"} {
		start := strings.Index(lower, marker)
		if start < 0 {
			continue
		}
		tail := raw[start+len(marker):]
		tail = strings.TrimLeftFunc(tail, func(r rune) bool {
			return unicode.IsSpace(r) || r == ':' || r == '：'
		})
		if tail == "" {
			continue
		}
		token := strings.Fields(tail)[0]
		token = strings.Trim(token, "\"'`<>()[]{}")
		if token == "" {
			continue
		}
		if parsed, recognized := parseOAuthCallbackCandidate(token); recognized && parsed.Code != "" {
			return token
		}
	}
	return ""
}

func oauthCallbackFailure(parsed oauthRedirected) error {
	code := compactOAuthErrorText(parsed.Error, 100)
	description := compactOAuthErrorText(parsed.ErrorDescription, 300)
	if code == "" && description == "" {
		return nil
	}
	if code == "" {
		code = "authorization_error"
	}
	if description == "" {
		return fmt.Errorf("OAuth 授权失败 (%s)，请重新打开登录链接完成授权", code)
	}
	return fmt.Errorf("OAuth 授权失败 (%s): %s", code, description)
}

func compactOAuthErrorText(value string, maxRunes int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes])
	}
	return value
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
//	POST /admin/oauth/start  {provider:"codex"|"claude"|"antigravity"}
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
	authURL, err := desc.buildAuthorizeURLWithOptions(challenge, state, oauthAuthorizeOptions{AllowedWorkspaceID: req.AllowedWorkspaceID})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("invalid OAuth provider configuration: %w", err))
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
		"auth_url":   authURL,
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
	redirected := parseOAuthRedirected(req.Redirected)
	if redirected.Code == "" && redirected.Error == "" && redirected.ErrorDescription == "" {
		writeError(w, http.StatusBadRequest, errors.New("未能从粘贴内容中解析出授权码（请粘贴完整回调网址，或 Claude 页面显示的 Authentication code）"))
		return
	}
	pend, ok := s.oauth.get(req.SessionID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("登录会话已过期或不存在，请重新生成登录链接"))
		return
	}
	// CSRF: when the paste carried a state, it must match the one we issued.
	if redirected.State != "" && pend.state != "" && redirected.State != pend.state {
		writeError(w, http.StatusBadRequest, errors.New("state 不匹配，可能不是本次登录的回调，请重新登录"))
		return
	}
	if err := oauthCallbackFailure(redirected); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	desc, err := s.oauthProvider(pend.provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	groupName := strings.TrimSpace(req.GroupName)
	importEgressID := requestedImportEgressID(req.EgressID, req.PrimaryEgressID)
	var antigravityEgress storage.EgressProfile
	if pend.provider == "antigravity" {
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
		antigravityEgress, groupName, err = s.resolveAntigravityOAuthEgress(r.Context(), groupName, routeEgressID)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	claimed, ok := s.oauth.take(req.SessionID)
	if !ok {
		writeError(w, http.StatusConflict, errors.New("登录回调正在处理或已被使用，请重新生成登录链接"))
		return
	}
	pend = claimed
	var parsed authparse.ParsedAuth
	switch pend.provider {
	case "codex":
		parsed, err = s.exchangeCodexCode(r.Context(), desc, redirected.Code, pend.verifier)
	case "claude":
		// Run the code→token exchange from the SAME exit the imported account will be
		// bound to. Minting it from the relay host and then using it from the account's
		// proxy would make the credential's origin IP permanently inconsistent.
		var claudeEgress storage.EgressProfile
		claudeEgress, err = s.claudeImportOAuthEgress(r.Context(), groupName, importEgressID)
		if err == nil {
			parsed, err = s.exchangeClaudeCode(r.Context(), desc, redirected.Code, firstNonEmpty(redirected.State, pend.state), pend.verifier,
				claudeEgress, "oauth:claude:"+req.SessionID)
		}
	case "antigravity":
		parsed, err = s.exchangeAntigravityCode(r.Context(), redirected.Code, desc.redirectURI, antigravityEgress, "oauth:antigravity:"+req.SessionID)
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

// oauthHTTPClient is the host-direct client for token endpoints that are NOT the relayed
// inference hosts: auth.openai.com, chatgpt.com's session endpoint, and the operator's own
// reauth worker. A finite timeout guards against a hung exchange.
//
// It must NOT be used for Anthropic. A token call from here would put the account's
// credential lifecycle on the relay host's IP and Go TLS fingerprint while inference uses
// the bound egress and native fingerprint. Anthropic OAuth goes through
// doClaudeOAuthRequest → upstream.DoAnthropicOAuth.
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

// exchangeCodexRefreshToken turns a standalone RT into the normal Codex auth
// shape. This is used by the admin importer and Sub2API-compatible Hub, so an
// operator can add an account without first extracting an access token.
func (s *Server) exchangeCodexRefreshToken(ctx context.Context, refreshToken string) (authparse.ParsedAuth, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return authparse.ParsedAuth{}, errors.New("refresh_token required")
	}
	d, err := s.oauthProvider("codex")
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {d.clientID},
		"scope":         {d.scope},
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return authparse.ParsedAuth{}, fmt.Errorf("openai refresh-token exchange failed (%d): %s", resp.StatusCode, bodySnippet(body, 300))
	}
	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return authparse.ParsedAuth{}, fmt.Errorf("parse refresh-token response: %w", err)
	}
	parsed, err := authparse.ParseOAuthCodex(tr.AccessToken, firstNonEmpty(tr.RefreshToken, refreshToken), tr.IDToken)
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	parsed.Provider = "codex"
	return parsed, nil
}

func (s *Server) hydrateRefreshTokenImport(ctx context.Context, parsed authparse.ParsedAuth) (authparse.ParsedAuth, error) {
	if strings.TrimSpace(parsed.AccessToken) != "" || strings.TrimSpace(parsed.RefreshToken) == "" {
		return parsed, nil
	}
	if strings.EqualFold(strings.TrimSpace(parsed.Provider), "codex") || parsed.Provider == "" {
		return s.exchangeCodexRefreshToken(ctx, parsed.RefreshToken)
	}
	return parsed, nil
}

// claudeImportOAuthEgress resolves the egress a Claude login/import should use for its
// token exchange: the egress the account is about to be bound to. The exchange hits the
// same Anthropic account perimeter used by inference, so an unresolved future exit fails
// closed instead of minting the credential from the relay IP.
func (s *Server) claudeImportOAuthEgress(ctx context.Context, groupName, requestedEgressID string) (storage.EgressProfile, error) {
	egressID, err := s.resolveImportPrimaryEgressForGroup(ctx, requestedEgressID, groupName)
	if err != nil || strings.TrimSpace(egressID) == "" {
		s.auditClaudeOAuthEgressDegraded(ctx, "import_egress_unresolved", err)
		if err == nil {
			err = errors.New("empty import egress")
		}
		return storage.EgressProfile{}, fmt.Errorf("claude import egress unavailable: %w", err)
	}
	egress, err := s.store.GetEgressProfile(ctx, egressID)
	if err != nil {
		s.auditClaudeOAuthEgressDegraded(ctx, "import_egress_lookup_failed", err)
		return storage.EgressProfile{}, fmt.Errorf("claude import egress unavailable: %w", err)
	}
	return egress, nil
}

// exchangeClaudeCode runs the Anthropic authorization-code → tokens exchange
// (JSON). The access token is an opaque sk-ant-oat string. The exchange travels on the
// account's future egress and fingerprint (see upstream.DoAnthropicOAuth).
func (s *Server) exchangeClaudeCode(ctx context.Context, d oauthProviderDesc, code, state, verifier string, egress storage.EgressProfile, cookieJarKey string) (authparse.ParsedAuth, error) {
	if parsedCode, parsedState := splitCodeAndState(code); parsedCode != "" {
		code = parsedCode
		if parsedState != "" {
			state = parsedState
		}
	}
	raw, err := marshalClaudeOAuthJSON(claudeOAuthAuthorizationCodeGrant{
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  d.redirectURI,
		ClientID:     d.clientID,
		CodeVerifier: verifier,
		State:        state,
	})
	if err != nil {
		return authparse.ParsedAuth{}, err
	}
	resp, err := s.doClaudeOAuthRequest(ctx, egress, storage.Account{}, cookieJarKey, d.tokenURL, raw)
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
