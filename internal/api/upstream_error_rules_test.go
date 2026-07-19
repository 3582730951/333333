package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func TestAdminUpstreamErrorRulesCRUDAndTestEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"ok"}`)) })
	createBody := `{"name":"Claude quota","enabled":true,"priority":10,"providers":["claude"],"entrypoints":["claude_messages"],"model_patterns":["claude-sonnet-*"],"status_codes":[429],"body_keywords":["quota"],"match_mode":"all","account_action":"cooldown_recheck","downstream_action":"custom_error","response_status":503,"custom_message":"请稍后重试","cooldown_seconds":1800,"prefer_retry_after":true}`
	code, raw := grpReq(t, h, http.MethodPost, "/admin/upstream-error-rules", createBody)
	if code != http.StatusOK {
		t.Fatalf("create = %d: %s", code, raw)
	}
	var created storage.UpstreamErrorRule
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.MatchMode != "all" || created.AccountAction != "cooldown_recheck" {
		t.Fatalf("bad created rule: %+v", created)
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/upstream-error-rules", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d: %s", code, raw)
	}
	var list []storage.UpstreamErrorRule
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("bad list: %+v", list)
	}

	patchBody := `{"enabled":false,"priority":3,"downstream_action":"pass"}`
	code, raw = grpReq(t, h, http.MethodPatch, "/admin/upstream-error-rules/"+created.ID, patchBody)
	if code != http.StatusOK {
		t.Fatalf("patch = %d: %s", code, raw)
	}
	var patched storage.UpstreamErrorRule
	if err := json.Unmarshal(raw, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Enabled || patched.Priority != 3 || patched.DownstreamAction != "pass" {
		t.Fatalf("bad patch: %+v", patched)
	}

	// Re-enable for the matcher preview endpoint.
	code, raw = grpReq(t, h, http.MethodPatch, "/admin/upstream-error-rules/"+created.ID, `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("reenable = %d: %s", code, raw)
	}
	testBody := `{"provider":"claude","entrypoint":"claude_messages","model":"claude-sonnet-4.5","status":429,"body":"quota reached"}`
	code, raw = grpReq(t, h, http.MethodPost, "/admin/upstream-error-rules/test", testBody)
	if code != http.StatusOK {
		t.Fatalf("test = %d: %s", code, raw)
	}
	var preview map[string]interface{}
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	if preview["matched"] != true {
		t.Fatalf("expected match: %v", preview)
	}
	if got := preview["downstream_action"]; got != "pass" {
		t.Fatalf("downstream_action=%v", got)
	}

	code, raw = grpReq(t, h, http.MethodDelete, "/admin/upstream-error-rules/"+created.ID, "")
	if code != http.StatusOK {
		t.Fatalf("delete = %d: %s", code, raw)
	}
}

func TestValidateHideSafetyBufferingRuleUsesProtocolSafeDefaults(t *testing.T) {
	rule := storage.UpstreamErrorRule{
		DownstreamAction:    "hide_safety_buffering",
		BodyKeywords:        []string{"This request requires additional safety checks"},
		AccountAction:       "cooldown",
		FilterAccountAction: true,
	}
	if err := validateUpstreamErrorRule(&rule); err != nil {
		t.Fatal(err)
	}
	if len(rule.BodyKeywords) != 0 || rule.AccountAction != "none" || rule.FilterAccountAction {
		t.Fatalf("unsafe specialized rule normalization: %+v", rule)
	}
}

func TestCodexHideSafetyBufferingRulePreservesCompletedStream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_safe"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"ok","safety_buffering":{"reasons":["user_risk"]}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_safe","model":"gpt-5.5","status":"completed"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n"))
	})
	acc := h.importAccount(t, "safe", "up-safe", "tok-safe")
	setTestCapability(t, h, acc, "gpt-5.5", 1024)
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID:               "hide-safety-buffering",
		Name:             "hide safety buffering",
		Enabled:          true,
		Priority:         1,
		Providers:        []string{"chatgpt"},
		Entrypoints:      []string{"responses"},
		AccountAction:    "none",
		DownstreamAction: "hide_safety_buffering",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	got := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	if strings.Contains(got, "safety_buffering") {
		t.Fatalf("safety field leaked: %s", got)
	}
	for _, want := range []string{"response.created", "response.output_text.delta", "ok", "response.completed", "resp_safe"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %q: %s", want, got)
		}
	}
}

func TestAdminUpstreamErrorRuleModelOptionsMergeSources(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"ok"}`)) })
	ctx := context.Background()
	if err := h.store.UpsertCustomProvider(ctx, storage.CustomProvider{ID: "openrouter", Name: "OpenRouter", BaseURL: "https://openrouter.ai/api/v1", Enabled: true, Models: []string{"openrouter/auto"}}); err != nil {
		t.Fatal(err)
	}
	acc := h.importAccount(t, "cap", "up-cap", "tok-cap")
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: acc, ModelSlug: "gpt-5.5", Source: "test"}}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/upstream-error-rules/model-options", "")
	if code != http.StatusOK {
		t.Fatalf("model-options=%d: %s", code, raw)
	}
	body := string(raw)
	for _, want := range []string{"chatgpt", "claude", "openrouter", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "claude-sonnet", "openrouter/auto"} {
		if !strings.Contains(body, want) {
			t.Fatalf("model-options missing %q in %s", want, body)
		}
	}
}

func TestCodexUpstreamErrorRuleFailoverOnNonDefaultStatus(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{"error":{"message":"vendor-specific temporary block"}}`))
		case "Bearer access-b":
			bCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok-from-b"}`))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	accB := h.importAccount(t, "b", "upstream-b", "access-b")
	setTestCapability(t, h, accA, "gpt-5.5", 1024)
	setTestCapability(t, h, accB, "gpt-5.5", 1024)
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{ID: "failover-418", Name: "failover", Enabled: true, Priority: 1, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, ModelPatterns: []string{"gpt-5.5"}, StatusCodes: []int{http.StatusTeapot}, AccountAction: "cooldown_recheck", DownstreamAction: "failover", CooldownSeconds: 45}); err != nil {
		t.Fatal(err)
	}
	reqBody := `{"model":"gpt-5.5","prompt_cache_key":"rule-failover","input":"hi"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bCalled {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("rule failover did not succeed: status=%d bCalled=%v body=%s", resp.StatusCode, bCalled, body)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accA)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.RecheckPending || binding.CooldownUntil <= storage.Now() {
		t.Fatalf("cooldown_recheck not applied: %+v", binding)
	}
}

func TestCodexUpstreamErrorRuleFailoverOnWebSocketSSEUsageLimit(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = w.Write([]byte("event: error\n" +
				`data: {"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":12090},"status_code":429,"headers":{"X-Codex-Primary-Used-Percent":"100","X-Codex-Secondary-Used-Percent":"94","X-Codex-Primary-Reset-After-Seconds":"12091"}}` + "\n\n" +
				"data: [DONE]\n\n"))
		case "Bearer access-b":
			bCalled = true
			_, _ = w.Write([]byte("event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"ok-from-b"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_b","model":"gpt-5.5","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n" +
				"data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	accB := h.importAccount(t, "b", "upstream-b", "access-b")
	setTestCapability(t, h, accA, "gpt-5.5", 1024)
	setTestCapability(t, h, accB, "gpt-5.5", 1024)
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID:               "ws-usage-limit",
		Name:             "WebSocket usage limit",
		Enabled:          true,
		Priority:         1,
		Providers:        []string{"chatgpt"},
		Entrypoints:      []string{"responses"},
		BodyKeywords:     []string{"The usage limit has been reached"},
		AccountAction:    "builtin",
		DownstreamAction: "failover",
	}); err != nil {
		t.Fatal(err)
	}
	reqBody := `{"model":"gpt-5.5","stream":true,"prompt_cache_key":"ws-rule-failover","input":"hi"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bCalled || !strings.Contains(string(body), "ok-from-b") {
		t.Fatalf("SSE rule failover did not succeed: status=%d bCalled=%v body=%s", resp.StatusCode, bCalled, body)
	}
	if strings.Contains(string(body), "usage_limit_reached") || strings.Contains(string(body), "access-a") {
		t.Fatalf("account A limit event leaked downstream: %s", body)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accA)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.RecheckPending || binding.CooldownUntil < storage.Now()+3500 {
		t.Fatalf("embedded Codex reset window was not applied: %+v", binding)
	}
}

func TestCodexUpstreamErrorRuleFailoverOnSSEClient400(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = io.WriteString(w, "event: error\n"+
				`data: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`+"\n\n"+
				"data: [DONE]\n\n")
		case "Bearer access-b":
			_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"ok-from-b"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_b","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n"+
				"data: [DONE]\n\n")
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accountA := h.importAccount(t, "a", "upstream-a", "access-a")
	accountB := h.importAccount(t, "b", "upstream-b", "access-b")
	setTestCapability(t, h, accountA, "gpt-5.6-sol", 1024)
	setTestCapability(t, h, accountB, "gpt-5.6-sol", 1024)
	// This transform rule deliberately sorts before the failover rule, matching the
	// diagnostic bundle. It must not shadow terminal-error decisions.
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID: "hide-safety", Name: "hide safety", Enabled: true, Priority: 1,
		Providers: []string{"chatgpt"}, Entrypoints: []string{"responses"},
		AccountAction: "none", DownstreamAction: "hide_safety_buffering",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID: "failover-client-400", Name: "fail over unsupported ChatGPT model", Enabled: true, Priority: 2,
		Providers: []string{"chatgpt"}, Entrypoints: []string{"responses"}, StatusCodes: []int{400},
		BodyKeywords: []string{"using Codex with a ChatGPT account."}, MatchMode: "all",
		AccountAction: "cooldown_recheck", DownstreamAction: "failover", CooldownSeconds: 1800,
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"client-400-rule","input":"hi"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "ok-from-b") || strings.Contains(string(responseBody), "ChatGPT account") {
		t.Fatalf("client 400 rule failover status=%d body=%s", resp.StatusCode, responseBody)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-a" || requests[1].Auth != "Bearer access-b" {
		t.Fatalf("client 400 did not switch upstream exactly once: %+v", requests)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if !binding.RecheckPending || binding.CooldownUntil <= storage.Now() {
		t.Fatalf("configured account action was not applied: %+v", binding)
	}
}

func TestCodexSSEClient400WithoutRulePassesThrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\n"+
			`data: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"ordinary client error"}}`+"\n\n"+
			"data: [DONE]\n\n")
	})
	accountA := h.importAccount(t, "a", "upstream-a", "access-a")
	accountB := h.importAccount(t, "b", "upstream-b", "access-b")
	setTestCapability(t, h, accountA, "gpt-5.5", 1024)
	setTestCapability(t, h, accountB, "gpt-5.5", 1024)
	body := `{"model":"gpt-5.5","stream":true,"prompt_cache_key":"plain-client-400","input":"hi"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "ordinary client error") {
		t.Fatalf("ordinary client 400 status=%d body=%s", resp.StatusCode, responseBody)
	}
	if requests := h.requests(); len(requests) != 1 || requests[0].Auth != "Bearer access-a" {
		t.Fatalf("ordinary client 400 was unexpectedly retried: %+v", requests)
	}
}

func TestCodexChatStreamingWebSocketUsageLimitRetriesBeforeHTTP200(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = w.Write([]byte("event: error\n" +
				`data: {"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"},"status_code":429}` + "\n\n" +
				"data: [DONE]\n\n"))
		case "Bearer access-b":
			bCalled = true
			_, _ = w.Write([]byte("event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"chat-ok-from-b"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_b","model":"gpt-5.5","status":"completed"}}` + "\n\n" +
				"data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	accB := h.importAccount(t, "b", "upstream-b", "access-b")
	setTestCapability(t, h, accA, "gpt-5.5", 1024)
	setTestCapability(t, h, accB, "gpt-5.5", 1024)
	reqBody := `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bCalled || !strings.Contains(string(body), "chat-ok-from-b") {
		t.Fatalf("chat SSE failover did not succeed: status=%d bCalled=%v body=%s", resp.StatusCode, bCalled, body)
	}
	if strings.Contains(string(body), "usage_limit_reached") {
		t.Fatalf("account A limit event leaked into chat stream: %s", body)
	}
}

func TestCodexUpstreamErrorRuleCustomErrorOverridesDefault(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"raw quota detail"}}`))
	})
	acc := h.importAccount(t, "a", "up-a", "tok-a")
	setTestCapability(t, h, acc, "gpt-5.5", 1024)
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{ID: "custom", Name: "custom", Enabled: true, Priority: 1, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, ModelPatterns: []string{"gpt-5.5"}, StatusCodes: []int{429}, AccountAction: "none", DownstreamAction: "custom_error", ResponseStatus: 503, CustomMessage: "上游繁忙，请稍后"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(stringMust(json.Marshal(out))), "上游繁忙") {
		t.Fatalf("custom message missing: %+v", out)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("account_action=none should not cooldown: %+v", binding)
	}
}

func TestIdleStreamRuleWritesHeartbeatsAndReturns(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"ok"}`)) })
	rec := &flushRecorder{header: http.Header{}}
	done := make(chan struct{})
	go func() {
		h.app.writeIdleStreamForRule(rec, storage.UpstreamErrorRule{IdleSeconds: 1, IdlePingSeconds: 1, CustomMessage: "holding"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("idle stream did not finish after finite idle_seconds")
	}
	if rec.status != http.StatusOK {
		t.Fatalf("status=%d", rec.status)
	}
	if ct := rec.header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	if !strings.Contains(rec.body.String(), ": holding") {
		t.Fatalf("heartbeat missing: %q", rec.body.String())
	}
}

type flushRecorder struct {
	header http.Header
	status int
	body   strings.Builder
}

func (r *flushRecorder) Header() http.Header         { return r.header }
func (r *flushRecorder) WriteHeader(code int)        { r.status = code }
func (r *flushRecorder) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *flushRecorder) Flush()                      {}

func stringMust(b []byte, err error) string {
	if err != nil {
		panic(err)
	}
	return string(b)
}

var _ = bufio.ErrFinalToken
