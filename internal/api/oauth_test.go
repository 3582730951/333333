package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	authparse "codex-account-pool/internal/auth"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

func TestGeneratePKCE(t *testing.T) {
	v, c, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("verifier not base64url-no-pad: %v", err)
	}
	if len(raw) != 64 {
		t.Errorf("verifier decoded length = %d, want 64", len(raw))
	}
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if c != want {
		t.Errorf("challenge = %q, want S256(verifier) = %q", c, want)
	}
	if strings.ContainsAny(v, "=+/") || strings.ContainsAny(c, "=+/") {
		t.Errorf("PKCE values must be base64url-no-pad: v=%q c=%q", v, c)
	}
	if v2, _, _ := generatePKCE(); v == v2 {
		t.Errorf("two PKCE verifiers should differ")
	}
}

func TestParseRedirected(t *testing.T) {
	cases := []struct {
		name, in, code, state string
	}{
		{"openai localhost redirect", "http://localhost:1455/auth/callback?code=ABC&state=XYZ", "ABC", "XYZ"},
		{"anthropic console redirect", "https://console.anthropic.com/oauth/code/callback?code=ABC&state=XYZ", "ABC", "XYZ"},
		{"claude localhost redirect", "http://localhost:54545/callback?code=ABC&state=XYZ", "ABC", "XYZ"},
		{"url fragment plain state", "http://localhost:54545/callback?code=ABC#XYZ", "ABC", "XYZ"},
		{"url fragment params", "http://localhost:54545/callback#code=ABC&state=XYZ", "ABC", "XYZ"},
		{"encoded code state", "http://localhost:54545/callback?code=ABC%23XYZ", "ABC", "XYZ"},
		{"query string", "code=ABC&state=XYZ", "ABC", "XYZ"},
		{"leading question query", "?code=ABC&state=XYZ", "ABC", "XYZ"},
		{"code#state", "ABC#XYZ", "ABC", "XYZ"},
		{"bare code", "ABC", "ABC", ""},
		{"bare padded code", "ABC=", "ABC=", ""},
		{"whitespace trimmed", "  ABC#XYZ  ", "ABC", "XYZ"},
		{"url without state", "http://localhost:1455/auth/callback?code=ABC", "ABC", ""},
		{"quoted callback", `"http://localhost:51121/oauth-callback?code=ABC&state=XYZ"`, "ABC", "XYZ"},
		{"backtick callback", "`http://localhost:51121/oauth-callback?code=ABC&state=XYZ`", "ABC", "XYZ"},
		{"html escaped callback", "http://localhost:51121/oauth-callback?code=ABC&amp;state=XYZ", "ABC", "XYZ"},
		{"localhost without scheme", "localhost:51121/oauth-callback?code=ABC&state=XYZ", "ABC", "XYZ"},
		{"browser error page text", "This site cannot be reached\nhttp://localhost:51121/oauth-callback?code=ABC&state=XYZ\nERR_CONNECTION_REFUSED", "ABC", "XYZ"},
		{"unparseable prose", "This is not an OAuth callback", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state := parseRedirected(tc.in)
			if code != tc.code || state != tc.state {
				t.Errorf("parseRedirected(%q) = (%q,%q), want (%q,%q)", tc.in, code, state, tc.code, tc.state)
			}
		})
	}
}

func TestParseOAuthRedirectedPreservesAuthorizationError(t *testing.T) {
	parsed := parseOAuthRedirected("http://localhost:51121/oauth-callback?error=access_denied&error_description=User+cancelled&state=XYZ")
	if parsed.Code != "" || parsed.State != "XYZ" || parsed.Error != "access_denied" || parsed.ErrorDescription != "User cancelled" {
		t.Fatalf("parsed authorization error = %+v", parsed)
	}
	if err := oauthCallbackFailure(parsed); err == nil || !strings.Contains(err.Error(), "access_denied") || !strings.Contains(err.Error(), "User cancelled") {
		t.Fatalf("authorization error was not surfaced: %v", err)
	}
}

func TestOAuthStoreSingleUse(t *testing.T) {
	st := newOAuthStore(time.Hour)
	st.put("s1", oauthPending{provider: "codex", verifier: "v", state: "st"})
	peeked, ok := st.get("s1")
	if !ok || peeked.provider != "codex" {
		t.Fatalf("get(s1) = %+v, ok=%v; want pending session", peeked, ok)
	}
	if _, ok := st.get("s1"); !ok {
		t.Fatal("get must not consume the pending session")
	}
	p, ok := st.take("s1")
	if !ok || p.provider != "codex" || p.verifier != "v" || p.state != "st" {
		t.Fatalf("take(s1) = %+v, ok=%v; want the stored pending", p, ok)
	}
	if _, ok := st.take("s1"); ok {
		t.Errorf("take(s1) twice should miss (single-use)")
	}
	if _, ok := st.take("unknown"); ok {
		t.Errorf("take(unknown) should miss")
	}
}

func TestOAuthCompleteValidationDoesNotConsumeSession(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("invalid callback must not reach an upstream")
	})
	tests := []struct {
		name       string
		redirected string
	}{
		{name: "unparseable paste", redirected: "This is not an OAuth callback"},
		{name: "state mismatch", redirected: "http://localhost:51121/oauth-callback?code=ABC&state=WRONG"},
		{name: "authorization denied", redirected: "http://localhost:51121/oauth-callback?error=access_denied&error_description=User+cancelled&state=EXPECTED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessionID := "session-" + strings.ReplaceAll(tt.name, " ", "-")
			h.app.oauth.put(sessionID, oauthPending{provider: "antigravity", state: "EXPECTED"})
			payload, _ := json.Marshal(map[string]string{"session_id": sessionID, "redirected": tt.redirected})
			req := httptest.NewRequest(http.MethodPost, "/admin/oauth/complete", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.app.adminOAuthComplete(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"invalid_request"`) ||
				strings.Contains(w.Body.String(), "WRONG") || strings.Contains(w.Body.String(), "access_denied") {
				t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
			}
			if _, ok := h.app.oauth.get(sessionID); !ok {
				t.Fatal("invalid callback consumed the OAuth session")
			}
		})
	}
}

func TestOAuthStoreExpiry(t *testing.T) {
	st := newOAuthStore(time.Hour)
	base := time.Now()
	st.putAt("fresh", oauthPending{provider: "claude"}, base)
	st.putAt("stale", oauthPending{provider: "claude"}, base.Add(-2*time.Hour))
	if _, ok := st.takeAt("stale", base); ok {
		t.Errorf("expired session should be purged")
	}
	if _, ok := st.takeAt("fresh", base); !ok {
		t.Errorf("non-expired session should be returned")
	}
}

func TestOAuthProviderDescriptors(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	s := &Server{cfg: cfg}

	codex, err := s.oauthProvider("codex")
	if err != nil {
		t.Fatalf("oauthProvider(codex): %v", err)
	}
	if codex.authURL != config.DefaultCodexOAuthAuthURL ||
		codex.tokenURL != config.DefaultCodexOAuthTokenURL ||
		codex.clientID != config.DefaultCodexOAuthClientID ||
		codex.redirectURI != config.DefaultCodexOAuthRedirectURI {
		t.Errorf("codex descriptor did not pick up config defaults: %+v", codex)
	}

	q := mustQuery(t, codex.authorizeURL("CHAL", "STATE"))
	for k, want := range map[string]string{
		"client_id":                  config.DefaultCodexOAuthClientID,
		"response_type":              "code",
		"redirect_uri":               config.DefaultCodexOAuthRedirectURI,
		"scope":                      config.DefaultCodexOAuthScope,
		"state":                      "STATE",
		"code_challenge":             "CHAL",
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "codex_cli_rs",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("codex authorize param %s = %q, want %q", k, got, want)
		}
	}
	// The generated URL must be byte-for-byte the real Codex CLI authorize request.
	// Two params previously broke this: a `prompt=login` the real client never sends,
	// and a trailing `api.responses.write` scope this client_id isn't granted. Either
	// makes auth.openai.com reject the request and error-redirect to localhost:1455
	// (the "open link → instantly jumps, no login screen" bug). Ground truth: the
	// official Codex CLI link and codex-rs build_authorize_url.
	if q.Get("prompt") != "" {
		t.Errorf("codex authorize must not send prompt=%q (real client omits it)", q.Get("prompt"))
	}
	if strings.Contains(q.Get("scope"), "api.responses.write") {
		t.Errorf("codex authorize scope must not include api.responses.write (not granted to this client_id): %q", q.Get("scope"))
	}
	// The connectors scopes must be present — these are exactly what the real
	// Codex CLI requests; api.responses.write is intentionally NOT among them.
	if !strings.Contains(q.Get("scope"), "api.connectors.read") ||
		!strings.Contains(q.Get("scope"), "api.connectors.invoke") {
		t.Errorf("codex authorize scope missing required connectors scopes: %q", q.Get("scope"))
	}

	claude, err := s.oauthProvider("anthropic") // alias
	if err != nil {
		t.Fatalf("oauthProvider(anthropic): %v", err)
	}
	if claude.provider != "claude" || claude.clientID != config.DefaultClaudeOAuthClientID {
		t.Errorf("claude descriptor wrong: %+v", claude)
	}
	if claude.tokenURL != "https://api.anthropic.com/v1/oauth/token" {
		t.Errorf("claude token endpoint = %q, want api.anthropic.com endpoint", claude.tokenURL)
	}
	cq := mustQuery(t, claude.authorizeURL("CHAL", "STATE"))
	if cq.Get("code") != "true" {
		t.Errorf("claude authorize must set code=true, got %q", cq.Get("code"))
	}
	if cq.Get("codex_cli_simplified_flow") != "" {
		t.Errorf("claude authorize must not carry codex flags")
	}

	antigravity, err := s.oauthProvider("antigravity")
	if err != nil {
		t.Fatalf("oauthProvider(antigravity): %v", err)
	}
	if antigravity.redirectURI != config.DefaultAntigravityOAuthRedirectURI {
		t.Fatalf("antigravity redirect URI = %q", antigravity.redirectURI)
	}
	agq := mustQuery(t, antigravity.authorizeURL("MUST_NOT_APPEAR", "STATE"))
	if agq.Get("code_challenge") != "" || agq.Get("code_challenge_method") != "" {
		t.Fatalf("antigravity OAuth must not send PKCE: %v", agq)
	}
	for _, scope := range []string{"cloud-platform", "userinfo.email", "userinfo.profile", "cclog", "experimentsandconfigs"} {
		if !strings.Contains(agq.Get("scope"), scope) {
			t.Fatalf("antigravity scope missing %q: %q", scope, agq.Get("scope"))
		}
	}

	if _, err := s.oauthProvider("bogus"); err == nil {
		t.Errorf("unknown provider should error")
	}
}

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn oauthRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func oauthJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestExchangeAntigravityDiscoversIdentityAndProject(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AntigravityOAuthTokenURL = "https://oauth.test/token"
	originalClient := apiExternalHTTPClient
	t.Cleanup(func() { apiExternalHTTPClient = originalClient })
	requests := []string{}
	apiExternalHTTPClient = &http.Client{Transport: oauthRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.String())
		if req.Header.Get("X-Forwarded-For") != "" || req.Header.Get("X-Pool-Account") != "" {
			t.Fatalf("downstream/proxy identity header leaked: %v", req.Header)
		}
		switch req.URL.String() {
		case "https://oauth.test/token":
			body, _ := io.ReadAll(req.Body)
			form, parseErr := url.ParseQuery(string(body))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if form.Get("code") != "oauth-code" || form.Get("redirect_uri") != config.DefaultAntigravityOAuthRedirectURI {
				t.Fatalf("token form = %v", form)
			}
			if form.Get("code_verifier") != "" {
				t.Fatalf("antigravity token exchange sent PKCE verifier: %v", form)
			}
			return oauthJSONResponse(http.StatusOK, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"scope":"scope-a scope-b"}`), nil
		case antigravityUserInfoURL:
			if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer access" {
				t.Fatalf("userinfo request = %s %v", req.Method, req.Header)
			}
			return oauthJSONResponse(http.StatusOK, `{"email":"Admin@Example.com"}`), nil
		case antigravityControlPlaneURL + "/v1internal:loadCodeAssist":
			body, _ := io.ReadAll(req.Body)
			if g := strings.TrimSpace(string(body)); !strings.Contains(g, `"ideType":"ANTIGRAVITY"`) {
				t.Fatalf("loadCodeAssist body = %s", g)
			}
			return oauthJSONResponse(http.StatusOK, `{"cloudaicompanionProject":"project-123"}`), nil
		default:
			t.Fatalf("unexpected OAuth request: %s", req.URL)
			return nil, nil
		}
	})}

	parsed, err := (&Server{cfg: cfg}).exchangeAntigravityCode(context.Background(), "oauth-code", config.DefaultAntigravityOAuthRedirectURI, storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}, "oauth-test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Provider != "antigravity" || parsed.Email != "Admin@Example.com" || parsed.AntigravityProjectID != "project-123" {
		t.Fatalf("parsed auth = %+v", parsed)
	}
	if parsed.AccountID == "" || parsed.AccountID != stableAntigravityAccountIDForTest(t, parsed.Email) {
		t.Fatalf("unstable account id = %q", parsed.AccountID)
	}
	if len(requests) != 3 {
		t.Fatalf("request sequence = %v", requests)
	}
}

func TestExchangeAntigravityUsesSelectedEgressForWholeLogin(t *testing.T) {
	cfg := config.Default()
	cfg.RequestTimeoutSeconds = 5
	originalUserInfo := antigravityUserInfoURL
	originalControl := antigravityControlPlaneURL
	originalDaily := antigravityDailyControlURL
	t.Cleanup(func() {
		antigravityUserInfoURL = originalUserInfo
		antigravityControlPlaneURL = originalControl
		antigravityDailyControlURL = originalDaily
	})
	cfg.AntigravityOAuthTokenURL = "http://google.test/token"
	antigravityUserInfoURL = "http://google.test/userinfo"
	antigravityControlPlaneURL = "http://cloudcode.test"
	antigravityDailyControlURL = "http://daily-cloudcode.test"

	var requests []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.String())
		switch r.URL.String() {
		case cfg.AntigravityOAuthTokenURL:
			_, _ = io.WriteString(w, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
		case antigravityUserInfoURL:
			_, _ = io.WriteString(w, `{"email":"admin@example.com"}`)
		case antigravityControlPlaneURL + "/v1internal:loadCodeAssist":
			_, _ = io.WriteString(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case antigravityDailyControlURL + "/v1internal:onboardUser":
			if r.Header.Get("X-Goog-Api-Client") != antigravityGoogAPIClient {
				t.Fatalf("onboard X-Goog-Api-Client = %q", r.Header.Get("X-Goog-Api-Client"))
			}
			_, _ = io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":"project-proxied"}}`)
		default:
			t.Fatalf("unexpected proxy target %s", r.URL)
		}
	}))
	defer proxy.Close()

	server := &Server{cfg: cfg, upstream: upstream.NewClient(cfg)}
	egress := storage.EgressProfile{ID: "oauth-proxy", Type: "http_proxy", Endpoint: proxy.URL}
	parsed, err := server.exchangeAntigravityCode(context.Background(), "oauth-code", config.DefaultAntigravityOAuthRedirectURI, egress, "oauth-egress-test")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AntigravityProjectID != "project-proxied" {
		t.Fatalf("project = %q", parsed.AntigravityProjectID)
	}
	want := []string{
		"POST " + cfg.AntigravityOAuthTokenURL,
		"GET " + antigravityUserInfoURL,
		"POST " + antigravityControlPlaneURL + "/v1internal:loadCodeAssist",
		"POST " + antigravityDailyControlURL + "/v1internal:onboardUser",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("request sequence = %#v, want %#v", requests, want)
	}
}

func TestAntigravityOAuthStartPinsGroupInheritedEgress(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	ctx := context.Background()
	if err := h.store.UpsertEgressProfile(ctx, storage.EgressProfile{ID: "oauth-group-primary", Name: "OAuth group primary", Type: "http_proxy", Endpoint: "http://127.0.0.1:18080", Health: "healthy"}); err != nil {
		t.Fatal(err)
	}
	group, err := h.store.GetGroup(ctx, "antigravity")
	if err != nil {
		t.Fatal(err)
	}
	group.EgressIDs = []string{"oauth-group-primary", storage.DefaultDirectEgressID}
	if err := h.store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/admin/oauth/start", "application/json", strings.NewReader(`{"provider":"antigravity","group_name":"antigravity"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("start status %d: %s", resp.StatusCode, raw)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	pending, ok := h.app.oauth.take(result.SessionID)
	if !ok {
		t.Fatal("pending OAuth session not found")
	}
	if pending.groupName != "antigravity" || pending.egressID != "oauth-group-primary" {
		t.Fatalf("pending route = group %q egress %q", pending.groupName, pending.egressID)
	}
	if pending.importEgressID != "" {
		t.Fatalf("group-inherited outlet must not be copied as explicit account binding: %q", pending.importEgressID)
	}
}

func stableAntigravityAccountIDForTest(t *testing.T, email string) string {
	t.Helper()
	parsed, err := authparse.ParseOAuthAntigravity("access", "refresh", strings.ToLower(email), "project", time.Now().Unix()+3600, nil)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.AccountID
}

func TestExchangeClaudeCodeSplitsInlineState(t *testing.T) {
	var got map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-test","refresh_token":"refresh","account":{"email_address":"claude@example.com"}}`))
	}))
	defer ts.Close()

	desc := oauthProviderDesc{
		provider:    "claude",
		tokenURL:    ts.URL,
		clientID:    "client",
		redirectURI: "http://localhost:54545/callback",
	}
	parsed, err := (&Server{}).exchangeClaudeCode(context.Background(), desc, "CODE#INLINE_STATE", "ISSUED_STATE", "VERIFIER")
	if err != nil {
		t.Fatalf("exchangeClaudeCode: %v", err)
	}
	if parsed.AccessToken != "sk-ant-oat-test" || parsed.RefreshToken != "refresh" || parsed.Email != "claude@example.com" {
		t.Fatalf("parsed auth mismatch: %+v", parsed)
	}
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "client",
		"code":          "CODE",
		"state":         "INLINE_STATE",
		"redirect_uri":  "http://localhost:54545/callback",
		"code_verifier": "VERIFIER",
	} {
		if got[k] != want {
			t.Errorf("request %s = %q, want %q", k, got[k], want)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL not parseable: %v", err)
	}
	return u.Query()
}
