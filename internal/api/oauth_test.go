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

	"codex-account-pool/internal/config"
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
		{"whitespace trimmed", "  ABC#XYZ  ", "ABC", "XYZ"},
		{"url without state", "http://localhost:1455/auth/callback?code=ABC", "ABC", ""},
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

func TestOAuthStoreSingleUse(t *testing.T) {
	st := newOAuthStore(time.Hour)
	st.put("s1", oauthPending{provider: "codex", verifier: "v", state: "st"})
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

	if _, err := s.oauthProvider("bogus"); err == nil {
		t.Errorf("unknown provider should error")
	}
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
