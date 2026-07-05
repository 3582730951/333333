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
	for _, want := range []string{"chatgpt", "claude", "openrouter", "gpt-5.5", "claude-sonnet", "openrouter/auto"} {
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
