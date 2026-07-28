package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestClaudeMessagesAutoRoutesExactGroupModelsToAntigravity(t *testing.T) {
	var upstreamCalls atomic.Int32
	observedModels := make(chan string, 4)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatalf("decode Antigravity envelope: %v body=%s", err, body)
		}
		observedModels <- envelope.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"response": {
				"candidates": [{"content":{"role":"model","parts":[{"text":"routed"}]},"finishReason":"STOP"}],
				"usageMetadata": {"promptTokenCount":4,"candidatesTokenCount":2}
			}
		}`)
	})
	ctx := context.Background()
	unsupported := storage.Account{ID: "claude-unsupported-in-mixed-group", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := h.store.UpsertAccount(ctx, unsupported, storage.AccountToken{OpenAIAPIKey: "sk-ant-unsupported"}); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "antigravity-auto-route", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(ctx, storage.AntigravityCredentials{
		AccountID: account.ID, ProjectID: "project", AccessToken: "access",
		ExpiresAt: time.Now().Add(2 * time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}
	models := []string{"claude-opus-4-6-thinking", "gemini-3-flash"}
	caps := make([]storage.ModelCapability, 0, len(models)*2)
	for _, model := range models {
		caps = append(caps, storage.ModelCapability{
			AccountID: account.ID, ModelSlug: model, AvailabilityState: "verified",
			Source: "antigravity_model_probe",
		}, storage.ModelCapability{
			AccountID: unsupported.ID, ModelSlug: model, AvailabilityState: "unsupported",
			Source: "claude_runtime_rejected",
		})
	}
	if err := h.store.UpsertCapabilities(ctx, caps); err != nil {
		t.Fatal(err)
	}

	const sessionID = "mixed-group-model-switch"
	for _, model := range models {
		body := []byte(`{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		affinity := routing.ExtractClaudeAffinityKey(req, body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Pool-Resolved-Provider") != "antigravity" {
			t.Fatalf("model=%s status=%d provider=%q body=%s", model, resp.StatusCode, resp.Header.Get("X-Pool-Resolved-Provider"), raw)
		}
		if observed := <-observedModels; observed != model {
			t.Fatalf("model=%s reached Antigravity as %q", model, observed)
		}
		binding, err := h.store.GetAffinityBinding(ctx, affinity.Hash)
		if err != nil || binding.AccountID != account.ID || binding.Provider != "antigravity" || binding.Model != model {
			t.Fatalf("model=%s exact affinity binding=%+v err=%v", model, binding, err)
		}
	}

	before := upstreamCalls.Load()
	unknownBody := `{"model":"gemini-unverified","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	unknownReq, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(unknownBody))
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownReq.Header.Set("anthropic-version", "2023-06-01")
	unknownReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
	unknownResp, err := http.DefaultClient.Do(unknownReq)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, unknownResp.Body)
	_ = unknownResp.Body.Close()
	if unknownResp.StatusCode == http.StatusOK || upstreamCalls.Load() != before {
		t.Fatalf("unknown Antigravity model status=%d calls=%d->%d", unknownResp.StatusCode, before, upstreamCalls.Load())
	}
}

func TestAntigravityGeminiHandlerRecordsAttributedCacheUsage(t *testing.T) {
	const (
		accountID = "antigravity-usage"
		model     = "gemini-2.5-pro"
		keyHash   = "downstream-key-hash"
		userID    = "portal-user"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" {
			t.Fatalf("antigravity upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer antigravity-access-token" {
			t.Fatalf("antigravity authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"response": {
				"candidates": [{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],
				"usageMetadata": {"promptTokenCount":12,"candidatesTokenCount":3,"cachedContentTokenCount":5}
			}
		}`)
	})
	ctx := context.Background()
	account := storage.Account{ID: accountID, Label: "Antigravity usage", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(ctx, storage.AntigravityCredentials{
		AccountID:   accountID,
		ProjectID:   "test-project",
		AccessToken: "antigravity-access-token",
		ExpiresAt:   time.Now().Add(2 * time.Hour).Unix(),
		BaseURL:     h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}

	raw := []byte(`{"model":"` + model + `","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "antigravity-thread")
	affinity := routing.ExtractAffinityKey(req, raw)
	if affinity.Hash == "" {
		t.Fatal("expected test request to produce an affinity hash")
	}
	req = req.WithContext(withDownstreamKey(req.Context(), downstreamPolicy{KeyHash: keyHash, UserID: userID}))
	w := httptest.NewRecorder()

	if got := h.app.antigravityMessagesWithLease(w, req, raw, model, scheduler.Lease{Account: account}, map[string]bool{}); got != outcomeDone {
		t.Fatalf("handler outcome = %v", got)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("handler status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"text":"hello"`) {
		t.Fatalf("translated response = %s", w.Body.String())
	}

	var (
		gotRouteHash, gotKeyHash, gotUserID, gotProvider, gotSource, gotAffinitySource string
		gotCached, gotCacheRead                                                        int64
		gotCacheReadPresent                                                            int
	)
	if err := h.store.DB().QueryRowContext(ctx, `
		SELECT route_key_hash, api_key_hash, user_id, usage_provider, usage_source,
		       affinity_source, cached_tokens, cache_read_tokens, cache_read_present
		FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, accountID).Scan(
		&gotRouteHash, &gotKeyHash, &gotUserID, &gotProvider, &gotSource,
		&gotAffinitySource, &gotCached, &gotCacheRead, &gotCacheReadPresent,
	); err != nil {
		t.Fatal(err)
	}
	if gotRouteHash != affinity.Hash || gotKeyHash != keyHash || gotUserID != userID {
		t.Fatalf("usage identity = route %q key %q user %q", gotRouteHash, gotKeyHash, gotUserID)
	}
	if gotProvider != "antigravity" || gotSource != "upstream" || gotAffinitySource != affinity.Source {
		t.Fatalf("usage diagnostics = provider %q source %q affinity %q", gotProvider, gotSource, gotAffinitySource)
	}
	if gotCached != 5 || gotCacheRead != 5 || gotCacheReadPresent != 1 {
		t.Fatalf("cache usage = cached %d read %d present %d", gotCached, gotCacheRead, gotCacheReadPresent)
	}

	binding, err := h.store.GetAffinityBinding(ctx, affinity.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RouteKey != affinity.Key || binding.AccountID != accountID {
		t.Fatalf("affinity binding = %+v", binding)
	}
}

func TestAntigravityHandlerRetriesBeforeCommitAndExcludesAccount(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "http status", status: http.StatusServiceUnavailable, body: `{"error":{"message":"service unavailable"}}`},
		{name: "embedded SSE error", status: http.StatusOK, contentType: "text/event-stream", body: "data: {\"error\":{\"code\":429,\"message\":\"quota\"}}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			account := storage.Account{ID: "antigravity-retry", GroupName: "cyber", Provider: "antigravity", Status: "active"}
			if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
				t.Fatal(err)
			}
			if err := h.store.UpsertAntigravityCredentials(context.Background(), storage.AntigravityCredentials{
				AccountID: account.ID, ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
			}); err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"model":"claude-opus-4-8","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
			w := httptest.NewRecorder()
			exclude := map[string]bool{}
			if got := h.app.antigravityMessagesWithLease(w, req, raw, "claude-opus-4-8", scheduler.Lease{Account: account}, exclude); got != outcomeRetry {
				t.Fatalf("outcome = %v, want retry; body=%s", got, w.Body.String())
			}
			if !exclude[account.ID] {
				t.Fatalf("failed account was not excluded: %+v", exclude)
			}
			if w.Body.Len() != 0 {
				t.Fatalf("upstream error leaked downstream: %s", w.Body.String())
			}
		})
	}
}

func TestAntigravityRetriesAreBoundedAndPermanent4xxIsPreserved(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantCalls   int32
		wantOutcome attemptOutcome
		wantStatus  int
		wantExclude bool
	}{
		{name: "transient bounded", status: http.StatusServiceUnavailable, body: `{"error":{"message":"temporarily unavailable"}}`, wantCalls: 4, wantOutcome: outcomeRetry, wantStatus: http.StatusOK, wantExclude: true},
		{name: "permanent preserved", status: http.StatusBadRequest, body: `{"error":{"message":"invalid exact model input"}}`, wantCalls: 1, wantOutcome: outcomeDone, wantStatus: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			account := storage.Account{ID: "antigravity-classifier", GroupName: "cyber", Provider: "antigravity", Status: "active"}
			if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
				t.Fatal(err)
			}
			if err := h.store.UpsertAntigravityCredentials(context.Background(), storage.AntigravityCredentials{
				AccountID: account.ID, ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
			}); err != nil {
				t.Fatal(err)
			}
			raw := []byte(`{"model":"gemini-exact","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
			w := httptest.NewRecorder()
			exclude := map[string]bool{}
			got := h.app.antigravityMessagesWithLease(w, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw)), raw, "gemini-exact", scheduler.Lease{Account: account}, exclude)
			if got != tt.wantOutcome || calls.Load() != tt.wantCalls || w.Code != tt.wantStatus || exclude[account.ID] != tt.wantExclude {
				t.Fatalf("outcome=%v calls=%d status=%d exclude=%v body=%s", got, calls.Load(), w.Code, exclude, w.Body.String())
			}
			if tt.status == http.StatusBadRequest && w.Body.String() != tt.body {
				t.Fatalf("permanent body=%s want=%s", w.Body.String(), tt.body)
			}
		})
	}
}

func TestAntigravitySafetyStopIsHiddenWithoutAccountSwitch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"SAFETY\"}]}}\n\n")
	})
	account := storage.Account{ID: "antigravity-safety", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(context.Background(), storage.AntigravityCredentials{
		AccountID: account.ID, ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"claude-opus-4-8","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	exclude := map[string]bool{}
	if got := h.app.antigravityMessagesWithLease(w, req, raw, "claude-opus-4-8", scheduler.Lease{Account: account}, exclude); got != outcomeDone {
		t.Fatalf("outcome = %v", got)
	}
	if len(exclude) != 0 {
		t.Fatalf("safety-only response switched accounts: %+v", exclude)
	}
	if body := w.Body.String(); !strings.Contains(body, "event: message_stop") || strings.Contains(strings.ToLower(body), "safety") {
		t.Fatalf("safety response was not silently terminated: %s", body)
	}
}

func TestAntigravityUnsafeConversionReturnsLocal422(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unsafe request must not reach upstream")
	})
	account := storage.Account{ID: "antigravity-conversion", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(context.Background(), storage.AntigravityCredentials{
		AccountID: account.ID, ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"claude-opus-4-8","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	if got := h.app.antigravityMessagesWithLease(w, req, raw, "claude-opus-4-8", scheduler.Lease{Account: account}, map[string]bool{}); got != outcomeDone {
		t.Fatalf("outcome = %v", got)
	}
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "unsupported_protocol_conversion") {
		t.Fatalf("status/body = %d %s", w.Code, w.Body.String())
	}
}

func TestAntigravityAccountModelProbeIsDynamicAndRetainsLastCatalog(t *testing.T) {
	var fail atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("probe path = %q", r.URL.Path)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"models":{"claude-sonnet-5-2":{"displayName":"Claude Sonnet 5.2","maxTokens":1000000,"maxOutputTokens":64000},"gemini-3.2-pro":{"displayName":"Gemini 3.2 Pro","maxTokens":1048576}}}`)
	})
	ctx := context.Background()
	account := storage.Account{ID: "antigravity-model-probe", GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(ctx, storage.AntigravityCredentials{
		AccountID: account.ID, ProjectID: "project", AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}
	caps, err := h.app.probeAccountModels(ctx, account)
	if err != nil || len(caps) != 2 {
		t.Fatalf("caps=%+v err=%v", caps, err)
	}
	byModel := map[string]storage.ModelCapability{}
	for _, cap := range caps {
		byModel[cap.ModelSlug] = cap
	}
	if cap := byModel["claude-sonnet-5-2"]; cap.AvailabilityState != "verified" || cap.Context1MState != "supported" || cap.Source != "antigravity_model_probe" {
		t.Fatalf("dynamic Claude capability = %+v", cap)
	}

	fail.Store(true)
	retained, err := h.app.probeAccountModels(ctx, account)
	if err != nil || len(retained) != 2 {
		t.Fatalf("failed probe did not retain prior catalog: caps=%+v err=%v", retained, err)
	}
	if retained[0].Source != "antigravity_model_probe" || retained[1].Source != "antigravity_model_probe" {
		t.Fatalf("failed probe replaced authority with fallback: %+v", retained)
	}
}

func TestAntigravityLivenessUsesNativeModelCatalog(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("liveness used unexpected upstream path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer health-access" {
			t.Fatalf("liveness authorization = %q", got)
		}
		_, _ = io.WriteString(w, `{"models":{"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6","maxTokens":250000},"gemini-3.1-pro":{"displayName":"Gemini 3.1 Pro","maxTokens":1048576}}}`)
	})
	ctx := context.Background()
	account := storage.Account{ID: "antigravity-health", GroupName: "antigravity", Provider: "antigravity", Status: "active"}
	token := storage.AccountToken{}
	if err := h.store.UpsertAccountWithAntigravityCredentials(ctx, account, token, storage.AntigravityCredentials{
		ProjectID: "health-project", AccessToken: "health-access", RefreshToken: "health-refresh",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}

	result := h.app.probeAccountLiveness(ctx, account, token)
	if result.Err != nil || !result.Alive || !result.Ready || result.Status != http.StatusOK {
		t.Fatalf("liveness result = %+v", result)
	}
	if result.Provider != "antigravity" || result.ProbeScope != "account_auth_models" || !result.ModelChecked || result.Model != "claude-sonnet-4-6" {
		t.Fatalf("liveness classification = %+v", result)
	}
	if strings.Contains(string(result.Body), "health-access") || !strings.Contains(string(result.Body), `"model_count":2`) {
		t.Fatalf("liveness response was unsafe or incomplete: %s", result.Body)
	}
}

func TestAdminRefreshAntigravityUsesOAuthRefresh(t *testing.T) {
	var refreshCalls atomic.Int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
			t.Fatalf("refresh form = %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer oauth.Close()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("admin token refresh must not use the inference upstream")
	})
	h.app.cfg.AntigravityOAuthTokenURL = oauth.URL
	ctx := context.Background()
	account := storage.Account{ID: "antigravity-refresh", GroupName: "antigravity", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccountWithAntigravityCredentials(ctx, account, storage.AccountToken{}, storage.AntigravityCredentials{
		ProjectID: "refresh-project", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+account.ID+"/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("refresh status/body = %d %s", resp.StatusCode, raw)
	}
	creds, err := h.store.GetAntigravityCredentials(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls.Load() != 1 || creds.AccessToken != "fresh-access" || creds.RefreshToken != "fresh-refresh" || creds.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("refresh calls=%d credentials=%+v", refreshCalls.Load(), creds)
	}
}
