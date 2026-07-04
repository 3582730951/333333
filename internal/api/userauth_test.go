package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
)

// userauth_test.go exercises the multi-user portal auth end-to-end through the real
// relay handlers: register/login/logout, the first-user-is-admin bootstrap, role
// gating of /admin/*, double-submit CSRF on session-authenticated mutations, and
// admin_token back-compat.

func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// doReq issues a request with optional JSON body + headers and decodes a JSON object.
func doReq(t *testing.T, c *http.Client, method, u, body string, headers map[string]string) (*http.Response, map[string]interface{}) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return resp, m
}

func csrfFor(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	u, _ := url.Parse(base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == csrfCookieName {
			return ck.Value
		}
	}
	t.Fatal("no cp_csrf cookie was issued")
	return ""
}

func TestAuthFirstUserIsAdminRegisterLoginLogout(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	c := jarClient(t)

	resp, body := doReq(t, c, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter","name":"Boss"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status=%d body=%v", resp.StatusCode, body)
	}
	if body["role"] != "admin" {
		t.Fatalf("first registered user must be admin, got %v", body["role"])
	}

	resp, body = doReq(t, c, http.MethodGet, h.pool.URL+"/auth/me", "", nil)
	if resp.StatusCode != http.StatusOK || body["role"] != "admin" || body["via"] != "session" {
		t.Fatalf("/auth/me via session wrong: %d %v", resp.StatusCode, body)
	}

	resp, _ = doReq(t, c, http.MethodPost, h.pool.URL+"/auth/logout", "{}", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logout status=%d", resp.StatusCode)
	}
	// Open deployment (no admin_token): after logout /auth/me reports unauthenticated.
	_, body = doReq(t, c, http.MethodGet, h.pool.URL+"/auth/me", "", nil)
	if body["authed"] != false {
		t.Fatalf("after logout expected authed=false, got %v", body)
	}

	// Wrong password rejected; correct password re-establishes a session.
	resp, _ = doReq(t, c, http.MethodPost, h.pool.URL+"/auth/login", `{"email":"admin@x.io","password":"nope"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password should be 401, got %d", resp.StatusCode)
	}
	resp, body = doReq(t, c, http.MethodPost, h.pool.URL+"/auth/login", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil)
	if resp.StatusCode != http.StatusOK || body["role"] != "admin" {
		t.Fatalf("login failed: %d %v", resp.StatusCode, body)
	}
}

func TestAuthRegisterRejectsOversizedJSON(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	c := jarClient(t)

	resp, body := doReq(t, c, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"big@x.io","password":"hunter2hunter"}`+strings.Repeat(" ", adminJSONBodyLimit), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized register status=%d body=%v", resp.StatusCode, body)
	}
	errBody, _ := body["error"].(map[string]interface{})
	if msg, _ := errBody["message"].(string); !strings.Contains(msg, "request body too large") {
		t.Fatalf("oversized register error = %v, want size error", body)
	}

	resp, body = doReq(t, c, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil)
	if resp.StatusCode != http.StatusOK || body["role"] != "admin" {
		t.Fatalf("normal register after oversized body failed: %d %v", resp.StatusCode, body)
	}
}

func TestAuthSecondUserIsNormalAndDeniedAdmin(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	admin := jarClient(t)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first register %d", resp.StatusCode)
	}
	user := jarClient(t)
	resp, body := doReq(t, user, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"u@x.io","password":"password123"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second register %d", resp.StatusCode)
	}
	if body["role"] != "user" {
		t.Fatalf("second user should be 'user', got %v", body["role"])
	}
	// Normal user's session is rejected by an admin endpoint...
	if resp, _ := doReq(t, user, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin on /admin/accounts should be 403, got %d", resp.StatusCode)
	}
	// ...while the admin session is allowed.
	if resp, _ := doReq(t, admin, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin session on /admin/accounts should be 200, got %d", resp.StatusCode)
	}
}

func TestAuthAdminSessionMutationRequiresCSRF(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	admin := jarClient(t)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register %d", resp.StatusCode)
	}
	// Session-authenticated POST WITHOUT the CSRF header is rejected.
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/admin/providers", `{"id":"acme","name":"Acme","base_url":"https://acme.test/v1"}`, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin POST without CSRF should be 403, got %d", resp.StatusCode)
	}
	// With the double-submit CSRF header it succeeds.
	csrf := csrfFor(t, admin, h.pool.URL)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/admin/providers", `{"id":"acme","name":"Acme","base_url":"https://acme.test/v1"}`, map[string]string{csrfHeaderName: csrf}); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin POST with CSRF should be 200, got %d", resp.StatusCode)
	}
}

func TestAuthAdminTokenBackCompat(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.AdminToken = "cap_secret-token"
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     h.store,
		Scheduler: scheduler.New(h.store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(h.store, cfg),
	})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()
	c := jarClient(t)

	// No credential → admin endpoints + /auth/me are unauthenticated.
	if resp, _ := doReq(t, c, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token on /admin/accounts should be 401, got %d", resp.StatusCode)
	}
	if resp, body := doReq(t, c, http.MethodGet, h.pool.URL+"/auth/me", "", nil); resp.StatusCode != http.StatusUnauthorized || body["authed"] != false {
		t.Fatalf("/auth/me without token: %d %v", resp.StatusCode, body)
	}

	// The configured admin_token is honored on both /auth/me and /admin/*.
	bearer := map[string]string{"Authorization": "Bearer cap_secret-token"}
	if resp, body := doReq(t, c, http.MethodGet, h.pool.URL+"/auth/me", "", bearer); resp.StatusCode != http.StatusOK || body["role"] != "admin" || body["via"] != "admin_token" {
		t.Fatalf("/auth/me with token: %d %v", resp.StatusCode, body)
	}
	if resp, _ := doReq(t, c, http.MethodGet, h.pool.URL+"/admin/accounts", "", bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin token on /admin/accounts should be 200, got %d", resp.StatusCode)
	}
}

// TestOpenModeLocksDownAfterAdminBootstraps confirms the hardening: with no
// admin_token, anonymous /admin is open ONLY until the first admin registers; after
// that, anonymous access is rejected and /auth/me stops reporting open-admin.
func TestOpenModeLocksDownAfterAdminBootstraps(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	anon := jarClient(t)
	// Pre-bootstrap: open.
	if resp, _ := doReq(t, anon, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-bootstrap anonymous /admin should be open (200), got %d", resp.StatusCode)
	}
	if _, me := doReq(t, anon, http.MethodGet, h.pool.URL+"/auth/me", "", nil); me["via"] != "open" {
		t.Fatalf("pre-bootstrap /auth/me should report via=open, got %v", me["via"])
	}
	// Bootstrap an admin.
	admin := jarClient(t)
	if resp, _ := doReq(t, admin, http.MethodPost, h.pool.URL+"/auth/register", `{"email":"admin@x.io","password":"hunter2hunter"}`, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("register admin: %d", resp.StatusCode)
	}
	// Post-bootstrap: anonymous /admin is locked down and /auth/me is no longer open.
	if resp, _ := doReq(t, anon, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-bootstrap anonymous /admin should be 401, got %d", resp.StatusCode)
	}
	if resp, me := doReq(t, anon, http.MethodGet, h.pool.URL+"/auth/me", "", nil); resp.StatusCode != http.StatusUnauthorized || me["via"] == "open" {
		t.Fatalf("post-bootstrap /auth/me should be 401 non-open, got %d %v", resp.StatusCode, me)
	}
	// The admin's own session still works.
	if resp, _ := doReq(t, admin, http.MethodGet, h.pool.URL+"/admin/accounts", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin session /admin should be 200, got %d", resp.StatusCode)
	}
}
