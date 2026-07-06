package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
)

func testCodexIDToken(t *testing.T, userID, workspaceID, email, plan string) string {
	t.Helper()
	claims := map[string]interface{}{
		"https://api.openai.com/profile": map[string]interface{}{"email": email},
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_user_id":    userID,
			"chatgpt_account_id": workspaceID,
			"chatgpt_plan_type":  plan,
		},
	}
	raw, _ := json.Marshal(claims)
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func TestCodexAuthorizeURLIncludesAllowedWorkspaceIDOnlyForCodex(t *testing.T) {
	cfg := config.Default()
	s := &Server{cfg: cfg}
	codex, err := s.oauthProvider("codex")
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, codex.authorizeURLWithOptions("CHAL", "STATE", oauthAuthorizeOptions{AllowedWorkspaceID: "workspace-teacher"}))
	if got := q.Get("allowed_workspace_id"); got != "workspace-teacher" {
		t.Fatalf("allowed_workspace_id = %q, want workspace-teacher", got)
	}
	if q.Get("originator") != "codex_cli_rs" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("codex PKCE/Codex params missing: %v", q)
	}
	claude, err := s.oauthProvider("claude")
	if err != nil {
		t.Fatal(err)
	}
	cq := mustQuery(t, claude.authorizeURLWithOptions("CHAL", "STATE", oauthAuthorizeOptions{AllowedWorkspaceID: "workspace-teacher"}))
	if got := cq.Get("allowed_workspace_id"); got != "" {
		t.Fatalf("claude authorize must ignore allowed_workspace_id, got %q", got)
	}
}

func TestManualCodexOAuthCompleteUpdatesOriginalAccountAndRejectsWorkspaceMismatch(t *testing.T) {
	var tokenHits int
	idTokenTeacher := testCodexIDToken(t, "user-teacher", "workspace-teacher", "teacher@example.internal", "plus")
	idTokenWrong := testCodexIDToken(t, "user-teacher", "workspace-wrong", "teacher@example.internal", "plus")
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenHits++
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse token form: %v", err)
		}
		if r.Form.Get("code") == "WRONG" {
			_, _ = w.Write([]byte(`{"access_token":"at-wrong","refresh_token":"rt-wrong","id_token":"` + idTokenWrong + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","id_token":"` + idTokenTeacher + `"}`))
	}))
	defer oauth.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.CodexOAuthTokenURL = oauth.URL
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{Config: cfg, Store: h.store, Scheduler: scheduler.New(h.store, cfg), Upstream: upstream.NewClient(cfg), Planner: virtual.NewPlanner(h.store, cfg)})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	orig := storage.Account{ID: "acc-original", Label: "Original", GroupName: config.DefaultGroupName, UpstreamAccountID: "workspace-teacher", Status: "auth_expired", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, orig, storage.AccountToken{AccessToken: "at-old"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: orig.ID, LoginEmail: "teacher@example.internal", TargetWorkspaceID: "workspace-teacher", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}

	// First prove the K12 workspace guard refuses to write mismatched tokens.
	code, body := codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(orig.ID)+"/codex-reauth/oauth/start", `{"target_workspace_id":"workspace-teacher"}`)
	if code != http.StatusOK {
		t.Fatalf("oauth start status=%d body=%s", code, body)
	}
	var start struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(body), &start); err != nil {
		t.Fatal(err)
	}
	code, body = codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(orig.ID)+"/codex-reauth/oauth/complete", `{"session_id":"`+start.SessionID+`","redirected":"http://localhost:1455/auth/callback?code=WRONG"}`)
	if code != http.StatusConflict || !strings.Contains(body, "workspace") {
		t.Fatalf("workspace mismatch status/body = %d %s", code, body)
	}
	oldTok, _ := h.store.GetToken(ctx, orig.ID)
	if oldTok.AccessToken != "at-old" {
		t.Fatalf("mismatched OAuth overwrote token: %+v", oldTok)
	}

	// Then a matching workspace updates the original account row instead of creating a duplicate.
	code, body = codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(orig.ID)+"/codex-reauth/oauth/start", `{"target_workspace_id":"workspace-teacher"}`)
	if code != http.StatusOK {
		t.Fatalf("oauth start 2 status=%d body=%s", code, body)
	}
	if err := json.Unmarshal([]byte(body), &start); err != nil {
		t.Fatal(err)
	}
	code, body = codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(orig.ID)+"/codex-reauth/oauth/complete", `{"session_id":"`+start.SessionID+`","redirected":"http://localhost:1455/auth/callback?code=OK"}`)
	if code != http.StatusOK {
		t.Fatalf("oauth complete status=%d body=%s", code, body)
	}
	gotTok, _ := h.store.GetToken(ctx, orig.ID)
	if gotTok.AccessToken != "at-new" || gotTok.RefreshToken != "rt-new" || gotTok.IDTokenRaw == "" {
		t.Fatalf("original token not updated: %+v", gotTok)
	}
	gotAcc, _ := h.store.GetAccount(ctx, orig.ID)
	if gotAcc.Status != "active" || gotAcc.UpstreamAccountID != "workspace-teacher" || gotAcc.ChatGPTUserID != "user-teacher" {
		t.Fatalf("original account metadata/status not updated: %+v", gotAcc)
	}
	if tokenHits != 2 {
		t.Fatalf("token hits = %d, want 2", tokenHits)
	}
}

func TestRefreshCodexTokenQueuesAutoReauthOnInvalidRefreshAndSkipsClaude(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_invalidated","message":"try signing in again"}}`))
	}))
	defer oauth.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{Config: cfg, Store: h.store, Scheduler: scheduler.New(h.store, cfg), Upstream: upstream.NewClient(cfg), Planner: virtual.NewPlanner(h.store, cfg)})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	codexAcc := storage.Account{ID: "acc-codex-repair", Label: "codex", GroupName: config.DefaultGroupName, Status: "active", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, codexAcc, storage.AccountToken{AccessToken: "at-old", RefreshToken: "rt-old"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: codexAcc.ID, LoginEmail: "c@example.internal", Password: "pw", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}
	res, err := app.refreshCodexToken(ctx, storage.AccountToken{AccountID: codexAcc.ID, AccessToken: "at-old", RefreshToken: "rt-old"})
	if err == nil || res.Reason != "refresh_token_invalidated" {
		t.Fatalf("refresh result err=%v res=%+v", err, res)
	}
	jobs, err := h.store.ListCodexReauthJobs(ctx, codexAcc.ID, 10)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "queued" {
		t.Fatalf("codex job not queued: jobs=%+v err=%v", jobs, err)
	}

	claudeAcc := storage.Account{ID: "acc-claude-no-repair", Label: "claude", GroupName: config.DefaultGroupName, Status: "active", Provider: "claude"}
	if err := h.store.UpsertAccount(ctx, claudeAcc, storage.AccountToken{AccessToken: "sk-ant-oat-old", RefreshToken: "sk-ant-ort-old"}); err != nil {
		t.Fatal(err)
	}
	app.enqueueCodexReauthIfEligible(ctx, claudeAcc.ID, "should_skip")
	jobs, err = h.store.ListCodexReauthJobs(ctx, claudeAcc.ID, 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("claude/static account should not get codex jobs: jobs=%+v err=%v", jobs, err)
	}
}

func TestCodexReauthRunWorkerSuccessUpdatesOriginalAndWorkerFailureKeepsOldToken(t *testing.T) {
	idToken := testCodexIDToken(t, "user-worker", "workspace-worker", "worker@example.internal", "plus")
	var hits int
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/v1/codex/reauth" {
			t.Fatalf("worker path = %s", r.URL.Path)
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode worker req: %v", err)
		}
		if req["email"] != "worker@example.internal" || req["password"] != "pw" || req["otp_url"] != "https://otp.example" || req["target_workspace_id"] != "workspace-worker" {
			t.Fatalf("worker request missing config: %+v", req)
		}
		if hits == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":"failed","error":"bad password"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"succeeded","access_token":"at-worker","refresh_token":"rt-worker","id_token":"` + idToken + `","session_cookie":"__Secure-next-auth.session-token=abc","email":"worker@example.internal","workspace_id":"workspace-worker","plan_type":"plus"}`))
	}))
	defer worker.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.CodexReauthWorkerURL = worker.URL
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{Config: cfg, Store: h.store, Scheduler: scheduler.New(h.store, cfg), Upstream: upstream.NewClient(cfg), Planner: virtual.NewPlanner(h.store, cfg)})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	acc := storage.Account{ID: "acc-worker", Label: "worker", GroupName: config.DefaultGroupName, Status: "auth_expired", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccessToken: "at-old", RefreshToken: "rt-old"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountQuarantine(ctx, acc.ID, storage.Now()+3600, "auth refresh failed"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: acc.ID, LoginEmail: "worker@example.internal", Password: "pw", OTPURL: "https://otp.example", TargetWorkspaceID: "workspace-worker", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}

	code, body := codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(acc.ID)+"/codex-reauth/run", `{}`)
	if code != http.StatusOK {
		t.Fatalf("run status=%d body=%s", code, body)
	}
	gotTok, _ := h.store.GetToken(ctx, acc.ID)
	if gotTok.AccessToken != "at-worker" || gotTok.RefreshToken != "rt-worker" {
		t.Fatalf("worker success did not update token: %+v", gotTok)
	}
	cookie, _ := h.store.GetSessionCookie(ctx, acc.ID)
	if !strings.Contains(cookie, "session-token=abc") {
		t.Fatalf("session cookie not stored: %q", cookie)
	}
	gotAcc, _ := h.store.GetAccount(ctx, acc.ID)
	if gotAcc.Status != "active" || gotAcc.QuarantineUntil != 0 || gotAcc.UpstreamAccountID != "workspace-worker" {
		t.Fatalf("worker success did not clear/metadata account: %+v", gotAcc)
	}

	// Second run fails; the successful token remains in place.
	code, body = codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(acc.ID)+"/codex-reauth/run", `{}`)
	if code != http.StatusBadGateway || !strings.Contains(body, "bad password") {
		t.Fatalf("failed worker status/body = %d %s", code, body)
	}
	gotTok, _ = h.store.GetToken(ctx, acc.ID)
	if gotTok.AccessToken != "at-worker" || gotTok.RefreshToken != "rt-worker" {
		t.Fatalf("worker failure should keep old good token: %+v", gotTok)
	}
}

func TestCodexReauthStatusDoesNotExposeSecrets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	ctx := context.Background()
	acc := storage.Account{ID: "acc-status", Label: "status", GroupName: config.DefaultGroupName, Status: "active", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: acc.ID, LoginEmail: "status@example.internal", Password: "pw-secret", OTPURL: "https://otp-secret", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}
	code, body := codexReauthReq(t, h, http.MethodGet, "/admin/accounts/"+url.PathEscape(acc.ID)+"/codex-reauth-status", "")
	if code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", code, body)
	}
	if strings.Contains(body, "pw-secret") || strings.Contains(body, "otp-secret") || strings.Contains(body, "password\"") || strings.Contains(body, "otp_url\"") {
		t.Fatalf("status leaked secrets: %s", body)
	}
	if !strings.Contains(body, "password_configured") || !strings.Contains(body, "otp_url_configured") {
		t.Fatalf("status missing configured booleans: %s", body)
	}
}

func codexReauthReq(t *testing.T, h *testHarness, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.pool.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func TestAccountsListShowsCodexReauthConfiguredWithoutSecrets(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	ctx := context.Background()
	acc := storage.Account{ID: "acc-list-reauth", Label: "list", GroupName: config.DefaultGroupName, Status: "active", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: acc.ID, LoginEmail: "list@example.internal", Password: "pw-list", OTPURL: "https://otp-list", AutoEnabled: true, LastStatus: "configured"}); err != nil {
		t.Fatal(err)
	}
	code, body := codexReauthReq(t, h, http.MethodGet, "/admin/accounts?page=1&pageSize=20", "")
	if code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", code, body)
	}
	if strings.Contains(body, "pw-list") || strings.Contains(body, "otp-list") || strings.Contains(body, "password\"") || strings.Contains(body, "otp_url\"") {
		t.Fatalf("accounts list leaked reauth secret fields: %s", body)
	}
	if !strings.Contains(body, "codex_reauth_configured") || !strings.Contains(body, "codex_reauth_auto_enabled") {
		t.Fatalf("accounts list missing public reauth flags: %s", body)
	}
}

func TestCodexReauthRunWorkerWorkspaceMismatchReturnsConflict(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"status":"failed","code":"workspace_mismatch","error":"id_token chatgpt_account_id workspace-wrong != target workspace-teacher"}`))
	}))
	defer worker.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.CodexReauthWorkerURL = worker.URL
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{Config: cfg, Store: h.store, Scheduler: scheduler.New(h.store, cfg), Upstream: upstream.NewClient(cfg), Planner: virtual.NewPlanner(h.store, cfg)})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	acc := storage.Account{ID: "acc-worker-mismatch", Label: "worker", GroupName: config.DefaultGroupName, Status: "auth_expired", Provider: "codex"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccessToken: "at-old", RefreshToken: "rt-old"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: acc.ID, LoginEmail: "teacher@example.internal", Password: "pw", TargetWorkspaceID: "workspace-teacher", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}
	code, body := codexReauthReq(t, h, http.MethodPost, "/admin/accounts/"+url.PathEscape(acc.ID)+"/codex-reauth/run", `{}`)
	if code != http.StatusConflict || !strings.Contains(body, "workspace") {
		t.Fatalf("worker mismatch status/body = %d %s", code, body)
	}
	jobs, err := h.store.ListCodexReauthJobs(ctx, acc.ID, 10)
	if err != nil || len(jobs) == 0 || jobs[0].Status != storage.CodexReauthJobWorkspaceMismatch {
		t.Fatalf("workspace mismatch job not recorded: jobs=%+v err=%v", jobs, err)
	}
}
