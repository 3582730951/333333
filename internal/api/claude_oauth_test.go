package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
)

func rebuildHarnessForClaudeOAuth(t *testing.T, h *testHarness, oauthURL string) {
	t.Helper()
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.ClaudeUpstreamBaseURL = h.upstream.URL
	cfg.ClaudeOAuthTokenURL = oauthURL
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     h.store,
		Scheduler: scheduler.New(h.store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(h.store, cfg),
	})
	h.app = app
	h.pool = httptest.NewServer(app)
	t.Cleanup(h.pool.Close)
}

func upsertClaudeOAuthAccount(t *testing.T, h *testHarness, access, refresh string, expiresAt int64) storage.Account {
	t.Helper()
	acc := storage.Account{
		ID:        "claude-oauth",
		Label:     "Claude OAuth",
		GroupName: config.DefaultGroupName,
		Email:     "claude@example.com",
		PlanType:  "max",
		Provider:  "claude",
		Status:    "active",
	}
	tok := storage.AccountToken{
		AccessToken:        access,
		RefreshToken:       refresh,
		ExpiresAt:          expiresAt,
		Scopes:             "user:profile user:inference user:sessions:claude_code",
		OAuthRateLimitTier: "tier_4",
	}
	if err := h.store.UpsertAccount(context.Background(), acc, tok); err != nil {
		t.Fatal(err)
	}
	return acc
}

func TestPollOneClaudeQuotaUsesOAuthUsageEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat-live" {
			t.Fatalf("auth = %q", got)
		}
		if got := r.Header.Get("Anthropic-Beta"); got != "oauth-2025-04-20" {
			t.Fatalf("anthropic-beta = %q", got)
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "claude-cli/") {
			t.Fatalf("user-agent = %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"subscription_type":"max",
			"rate_limit_tier":"tier_4",
			"windows":{
				"5h":{"used_percent":40,"limit_window_seconds":18000,"reset_after_seconds":600,"limit_tokens":1000,"remaining_tokens":600},
				"7d":{"used_percent":25,"limit_window_seconds":604800,"reset_after_seconds":86400,"limit_tokens":7000,"remaining_tokens":5250},
				"oauth_app":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":300},
				"opus":{"used_percent":75,"limit_window_seconds":18000,"reset_after_seconds":120},
				"sonnet":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":200}
			}
		}`))
	})
	acc := upsertClaudeOAuthAccount(t, h, "sk-ant-oat-live", "refresh-live", storage.Now()+3600)

	if err := h.app.pollOneClaudeQuota(context.Background(), acc, mustToken(t, h, acc.ID), storage.EgressProfile{ID: storage.DefaultDirectEgressID, Type: "direct"}); err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.ListAccountRateLimits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]storage.AccountRateLimit{}
	for _, row := range rows {
		seen[row.LimiterType] = row
	}
	for _, want := range []string{"5h_oauth_usage", "7d_oauth_usage", "oauth_app"} {
		if _, ok := seen[want]; !ok {
			t.Fatalf("missing limiter %q in %#v", want, rows)
		}
	}
	for _, forbidden := range []string{"opus", "sonnet"} {
		if _, ok := seen[forbidden]; ok {
			t.Fatalf("model-family limiter %q must not be persisted for quota selection: %#v", forbidden, rows)
		}
	}
	if seen["5h_oauth_usage"].UsedPercent != 40 || seen["7d_oauth_usage"].UsedPercent != 25 {
		t.Fatalf("usage windows = %#v", seen)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/quota", "")
	if code != http.StatusOK {
		t.Fatalf("admin quota = %d: %s", code, raw)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 || out[0]["plan_type"] != "max" || out[0]["oauth_rate_limit_tier"] != "tier_4" {
		t.Fatalf("admin quota missing plan/tier: %#v", out)
	}
}

func TestParseClaudeOAuthUsagePreservesZeroTokenCounts(t *testing.T) {
	parsed, err := parseClaudeOAuthUsage([]byte(`{
		"windows":{
			"5h":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":60,"limit_tokens":0,"remaining_tokens":0}
		}
	}`), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Windows) != 1 {
		t.Fatalf("windows = %#v", parsed.Windows)
	}
	if parsed.Windows[0].LimitTokens != 0 || parsed.Windows[0].RemainingTokens != 0 {
		t.Fatalf("zero token counts not preserved: %#v", parsed.Windows[0])
	}
}

func TestParseClaudeOAuthUsageFindsNestedWindows(t *testing.T) {
	parsed, err := parseClaudeOAuthUsage([]byte(`{
		"subscriptionType":"max",
		"rateLimitTier":"tier_4",
		"usage":{
			"model_windows":{
				"sonnet":{"usage_percent":7,"windowSeconds":18000,"resetAfterSeconds":120,"remainingTokens":0}
			}
		}
	}`), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Windows) != 0 {
		t.Fatalf("windows = %#v", parsed.Windows)
	}
}

func TestClaudeRefreshGateSingleflightsExpiredTokenAndDelaysResume(t *testing.T) {
	var refreshCalls int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&refreshCalls, 1) == 1 {
			close(refreshStarted)
		}
		<-releaseRefresh
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer oauth.Close()

	var upstreamCalls int32
	var oldAuthSeen int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		if r.Header.Get("Authorization") == "Bearer sk-ant-oat-old" {
			atomic.StoreInt32(&oldAuthSeen, 1)
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	upsertClaudeOAuthAccount(t, h, "sk-ant-oat-old", "refresh-old", storage.Now()-10)

	const n = 5
	var wg sync.WaitGroup
	errs := make(chan string, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				errs <- err.Error()
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- string(body)
			}
		}()
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		t.Fatalf("upstream called while refresh gate active: %d", got)
	}
	close(releaseRefresh)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if atomic.LoadInt32(&oldAuthSeen) != 0 {
		t.Fatal("old Claude access token reached upstream during refresh")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("requests resumed too quickly after refresh: %s", elapsed)
	}
}

func TestClaudeMessagesRefreshesOnceAfterAuthExpired401(t *testing.T) {
	var refreshCalls int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer oauth.Close()

	var seen []string
	var mu sync.Mutex
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer sk-ant-oat-old" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"token expired"}}`))
			return
		}
		if auth != "Bearer sk-ant-oat-new" {
			t.Fatalf("auth = %q", auth)
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	upsertClaudeOAuthAccount(t, h, "sk-ant-oat-old", "refresh-old", storage.Now()+3600)

	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if len(seen) != 2 || seen[0] != "Bearer sk-ant-oat-old" || seen[1] != "Bearer sk-ant-oat-new" {
		t.Fatalf("expected old token then refreshed token, saw %#v", seen)
	}
}

func TestClaudeInvalidGrantRecoversCredentialRotatedByPreviousWorker(t *testing.T) {
	var h *testHarness
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := h.store.GetTokenFresh(r.Context(), "claude-oauth")
		if err != nil {
			t.Fatalf("load peer credential: %v", err)
		}
		current.AccessToken = "sk-ant-oat-peer"
		current.RefreshToken = "refresh-peer"
		current.ExpiresAt = storage.Now() + 3600
		current.LastRefresh = storage.Now()
		if _, err := h.store.UpdateTokenAfterCredentialRefresh(r.Context(), current); err != nil {
			t.Fatalf("persist peer credential: %v", err)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token already used"}`))
	}))
	defer oauth.Close()

	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[]}`))
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	account := upsertClaudeOAuthAccount(t, h, "sk-ant-oat-old", "refresh-old", storage.Now()-1)

	got, err := h.app.forceRefreshClaudeToken(context.Background(), account, "admin_refresh")
	if err != nil {
		t.Fatalf("rotating-token race was treated as terminal auth failure: %v", err)
	}
	if got.AccessToken != "sk-ant-oat-peer" || got.RefreshToken != "refresh-peer" {
		t.Fatalf("recovered token = access:%q refresh:%q", got.AccessToken, got.RefreshToken)
	}
	storedAccount, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedAccount.Status != "active" {
		t.Fatalf("race recovery quarantined a valid account: %+v", storedAccount)
	}
}

func TestClaudeSuccessfulRefreshReactivatesAuthExpiredAccount(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-recovered","refresh_token":"refresh-recovered","expires_in":3600}`))
	}))
	defer oauth.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat-recovered" {
			t.Fatalf("auth = %q", got)
		}
		_, _ = w.Write([]byte(`{"id":"msg","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	account := upsertClaudeOAuthAccount(t, h, "sk-ant-oat-stale", "refresh-current", storage.Now()+3600)
	if err := h.store.SetAccountStatus(context.Background(), account.ID, "auth_expired"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountQuarantine(context.Background(), account.ID, storage.Now()+3600, "old credential failure"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.BenchBindingForRecheck(context.Background(), account.ID, storage.Now()+300); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	code, raw := grpReq(t, h, http.MethodPost, "/admin/accounts/"+account.ID+"/refresh", "")
	if code != http.StatusOK {
		t.Fatalf("admin refresh = %d: %s", code, raw)
	}
	gotAccount, err := h.store.GetAccount(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAccount.Status != "active" || gotAccount.QuarantineUntil != 0 || gotAccount.QuarantineReason != "" {
		t.Fatalf("recovered account = %+v", gotAccount)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RecheckPending || binding.CooldownUntil != 0 {
		t.Fatalf("recovered binding still benched: %+v", binding)
	}
	token := mustToken(t, h, account.ID)
	if token.AccessToken != "sk-ant-oat-recovered" || token.RefreshToken != "refresh-recovered" || token.ExpiresAt <= storage.Now() {
		t.Fatalf("recovered token = %+v", token)
	}
	var recoveredAudit int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_log WHERE account_id=? AND action='auth_recovered' AND state='active'`, account.ID).Scan(&recoveredAudit); err != nil {
		t.Fatal(err)
	}
	if recoveredAudit != 1 {
		t.Fatalf("auth recovery audit rows = %d, want 1", recoveredAudit)
	}

	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovered account did not rejoin scheduler: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestClaudeMessagesStreamSendsRefreshWaitHeartbeat(t *testing.T) {
	releaseRefresh := make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-releaseRefresh
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer oauth.Close()

	var upstreamCalls int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat-new" {
			t.Fatalf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	upsertClaudeOAuthAccount(t, h, "sk-ant-oat-old", "refresh-old", storage.Now()-10)

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	var resp *http.Response
	select {
	case resp = <-respCh:
	case err := <-errCh:
		close(releaseRefresh)
		t.Fatal(err)
	case <-time.After(500 * time.Millisecond):
		close(releaseRefresh)
		t.Fatal("stream response did not send refresh-wait heartbeat before refresh completed")
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
		close(releaseRefresh)
		t.Fatalf("upstream called before refresh completed: %d", got)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		close(releaseRefresh)
		t.Fatal(err)
	}
	if line != ": claude-refresh-wait\n" {
		close(releaseRefresh)
		t.Fatalf("first SSE line = %q", line)
	}
	close(releaseRefresh)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("stream did not continue after refresh: %s", rest)
	}
}

func TestChatCompletionsViaClaudeStreamSendsRefreshWaitHeartbeat(t *testing.T) {
	releaseRefresh := make(chan struct{})
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-releaseRefresh
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"sk-ant-oat-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer oauth.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-ant-oat-new" {
			t.Fatalf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	rebuildHarnessForClaudeOAuth(t, h, oauth.URL)
	upsertClaudeOAuthAccount(t, h, "sk-ant-oat-old", "refresh-old", storage.Now()-10)

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	respCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	var resp *http.Response
	select {
	case resp = <-respCh:
	case err := <-errCh:
		close(releaseRefresh)
		t.Fatal(err)
	case <-time.After(500 * time.Millisecond):
		close(releaseRefresh)
		t.Fatal("OpenAI-compatible stream did not send refresh-wait heartbeat before refresh completed")
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		close(releaseRefresh)
		t.Fatal(err)
	}
	if line != ": claude-refresh-wait\n" {
		close(releaseRefresh)
		t.Fatalf("first SSE line = %q", line)
	}
	close(releaseRefresh)
	rest, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(rest), "chat.completion.chunk") || strings.Contains(string(rest), "content_block_delta") {
		t.Fatalf("OpenAI-compatible stream did not continue as chat SSE after refresh: %s", rest)
	}
}

func mustToken(t *testing.T, h *testHarness, accountID string) storage.AccountToken {
	t.Helper()
	token, err := h.store.GetToken(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
