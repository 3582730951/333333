package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/virtual"
	"github.com/gorilla/websocket"
)

type capturedRequest struct {
	Path          string
	Method        string
	AccountID     string
	Auth          string
	Beta          string
	Body          string
	TurnState     string
	ClaudeSession string
}

type testHarness struct {
	pool     *httptest.Server
	upstream *httptest.Server
	store    *storage.Store
	captured *[]capturedRequest
	app      *Server
}

func newHarness(t *testing.T, upstreamHandler http.HandlerFunc) *testHarness {
	t.Helper()
	var mu sync.Mutex
	var captured []capturedRequest
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, capturedRequest{
			Path:          r.URL.Path,
			Method:        r.Method,
			AccountID:     r.Header.Get("ChatGPT-Account-ID"),
			Auth:          r.Header.Get("Authorization"),
			Beta:          r.Header.Get("Anthropic-Beta"),
			Body:          string(raw),
			TurnState:     r.Header.Get("X-Codex-Turn-State"),
			ClaudeSession: r.Header.Get("X-Claude-Code-Session-Id"),
		})
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		upstreamHandler(w, r)
	}))
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.UpstreamBaseURL = up.URL + "/backend-api/codex"
	cfg.ClaudeUpstreamBaseURL = up.URL
	cfg.DatabasePath = filepath.Join(t.TempDir(), "unused.sqlite3")
	cfg.StickyWaitMillis = 1
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     store,
		Scheduler: scheduler.New(store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(store, cfg),
	})
	pool := httptest.NewServer(app)
	t.Cleanup(func() {
		pool.Close()
		up.Close()
		_ = store.Close()
	})
	return &testHarness{pool: pool, upstream: up, store: store, captured: &captured, app: app}
}

func (h *testHarness) importAccount(t *testing.T, label, upstreamAccountID, accessToken string) string {
	t.Helper()
	payload := map[string]interface{}{
		"label": label,
		"auth_json": map[string]interface{}{
			"OPENAI_API_KEY": accessToken,
			"tokens": map[string]interface{}{
				"access_token":  accessToken,
				"refresh_token": "refresh-" + label,
				"account_id":    upstreamAccountID,
			},
		},
	}
	raw, _ := json.Marshal(payload)
	resp, err := http.Post(h.pool.URL+"/admin/accounts/import-auth-json", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("import status %d: %s", resp.StatusCode, body)
	}
	var account storage.Account
	if err := json.NewDecoder(resp.Body).Decode(&account); err != nil {
		t.Fatal(err)
	}
	return account.ID
}

func (h *testHarness) requests() []capturedRequest {
	return append([]capturedRequest(nil), (*h.captured)...)
}

func cacheControlCountPrompt(t *testing.T, body []byte) int {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	countList := func(v interface{}) int {
		n := 0
		for _, item := range toInterfaceSlice(v) {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := block["cache_control"]; has {
				n++
			}
		}
		return n
	}
	n := countList(root["system"]) + countList(root["tools"])
	for _, item := range toInterfaceSlice(root["messages"]) {
		msg, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := msg["cache_control"]; has {
			n++
		}
		n += countList(msg["content"])
	}
	return n
}

func toInterfaceSlice(v interface{}) []interface{} {
	arr, _ := v.([]interface{})
	return arr
}

func setTestCapability(t *testing.T, h *testHarness, accountID, model string, nativeWindow int64) {
	t.Helper()
	if err := h.store.UpsertCapabilities(context.Background(), []storage.ModelCapability{{
		AccountID:                     accountID,
		ModelSlug:                     model,
		NativeContextWindow:           nativeWindow,
		NativeMaxContextWindow:        nativeWindow,
		EffectiveContextWindowPercent: 100,
		Visibility:                    "list",
		Source:                        "test",
		LastProbeAt:                   storage.Now(),
	}}); err != nil {
		t.Fatalf("upsert test capability: %v", err)
	}
}

func TestGatewayResponsesRawStreamingAndHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("ChatGPT-Account-ID", "should-not-leak")
		w.Header().Set("x-sidecar-debug", "should-not-leak")
		_, _ = w.Write([]byte("data: {\"ok\":true}\n\n"))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	body := `{"model":"gpt","input":[{"role":"user","content":"hello"}],"prompt_cache_key":"pc-1"}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(got) != "data: {\"ok\":true}\n\n" {
		t.Fatalf("status/body = %d %q", resp.StatusCode, got)
	}
	if resp.Header.Get("ChatGPT-Account-ID") != "" || resp.Header.Get("x-sidecar-debug") != "" {
		t.Fatalf("internal headers leaked: %+v", resp.Header)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	// Body is forwarded intact except the prompt_cache_key, which conversation
	// isolation (default on) namespaces per-account — stable and cache-preserving,
	// so a flagged session cannot be correlated across accounts.
	rb := reqs[0].Body
	if !strings.Contains(rb, `"model":"gpt"`) || !strings.Contains(rb, `"content":"hello"`) {
		t.Fatalf("forwarded body fields changed:\n%s", rb)
	}
	if strings.Contains(rb, `"pc-1"`) || !strings.Contains(rb, `"prompt_cache_key":"cp_`) {
		t.Fatalf("prompt_cache_key not namespaced per account:\n%s", rb)
	}
	if reqs[0].AccountID != "upstream-a" || reqs[0].Auth != "Bearer access-a" {
		t.Fatalf("missing auth headers: %+v", reqs[0])
	}
	h.app.WaitForAsyncWrites()
	var holdStatus string
	if err := h.store.DB().QueryRow(`SELECT status FROM billing_holds LIMIT 1`).Scan(&holdStatus); err != nil {
		t.Fatalf("billing hold missing: %v", err)
	}
	if holdStatus != "settled_streaming" {
		t.Fatalf("billing hold status = %q", holdStatus)
	}
}

func TestCodexResponsesDoesNotVirtualTrimInput(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_no_trim","status":"completed","output_text":"ok"}`))
	})
	acc := h.importAccount(t, "trim", "up-trim", "access-trim")
	setTestCapability(t, h, acc, "gpt", 64)
	large := strings.Repeat("A", 2000)
	body := `{"model":"gpt","conversation_id":"conv-no-trim","input":[{"role":"user","content":"old ` + large + `"},{"role":"assistant","content":"middle ` + large + `"},{"role":"user","content":"current"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if !strings.Contains(reqs[0].Body, "old "+large) || !strings.Contains(reqs[0].Body, "middle "+large) {
		t.Fatalf("proxy trimmed or reconstructed input; upstream body=%s", reqs[0].Body)
	}
}

func TestGatewayAutoPromptCacheKeyForLargeStablePrefix(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"))
	})
	h.importAccount(t, "auto-pck", "upstream-auto-pck", "access-auto-pck")
	instructions := strings.Repeat("stable repository context for automatic prompt cache key. ", 80)
	body := `{"model":"gpt-cache-test","stream":true,"reasoning":{"effort":"high"},"instructions":` + strconv.Quote(instructions) + `,"input":[{"role":"user","content":"final question"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(upstreamBody, `"prompt_cache_key":"cp_`) {
		t.Fatalf("large stable prefix did not get a namespaced prompt_cache_key:\n%s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, `"effort":"high"`) || !strings.Contains(upstreamBody, `"final question"`) {
		t.Fatalf("auto prompt_cache_key changed reasoning or content:\n%s", upstreamBody)
	}
}

func TestGatewayAutoPromptCacheKeyForStablePrefixInsideFinalUserText(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"))
	})
	h.importAccount(t, "auto-pck-text-prefix", "upstream-auto-pck-text-prefix", "access-auto-pck-text-prefix")
	prefix := strings.Repeat("stable repository snapshot line for prompt cache reuse. ", 140)
	for _, question := range []string{"Question: summarize file A", "Question: summarize file B"} {
		body := `{"model":"gpt-cache-test","stream":true,"reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(prefix+question) + `}]}]}`
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured requests = %d", len(reqs))
	}
	key1 := promptCacheKeyFromBody(t, reqs[0].Body)
	key2 := promptCacheKeyFromBody(t, reqs[1].Body)
	if key1 == "" || key2 == "" || key1 != key2 {
		t.Fatalf("prompt_cache_key mismatch: %q vs %q\nfirst: %s\nsecond: %s", key1, key2, reqs[0].Body, reqs[1].Body)
	}
	if !strings.Contains(reqs[0].Body, "Question: summarize file A") || !strings.Contains(reqs[1].Body, "Question: summarize file B") {
		t.Fatalf("auto prompt_cache_key changed user content\nfirst: %s\nsecond: %s", reqs[0].Body, reqs[1].Body)
	}
	if !strings.Contains(reqs[0].Body, `"effort":"high"`) || !strings.Contains(reqs[1].Body, `"effort":"high"`) {
		t.Fatalf("auto prompt_cache_key changed reasoning\nfirst: %s\nsecond: %s", reqs[0].Body, reqs[1].Body)
	}
}

func TestGatewayAutoPromptCacheKeyForStablePrefixInsideTopLevelInputText(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"))
	})
	h.importAccount(t, "auto-pck-input-text-prefix", "upstream-auto-pck-input-text-prefix", "access-auto-pck-input-text-prefix")
	prefix := strings.Repeat("stable repository snapshot line for top level text cache reuse. ", 120)
	for _, question := range []string{"Question: summarize file A", "Question: summarize file B"} {
		body := `{"model":"gpt-cache-test","stream":true,"reasoning":{"effort":"high"},"input":` + strconv.Quote(prefix+question) + `}`
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured requests = %d", len(reqs))
	}
	key1 := promptCacheKeyFromBody(t, reqs[0].Body)
	key2 := promptCacheKeyFromBody(t, reqs[1].Body)
	if key1 == "" || key2 == "" || key1 != key2 {
		t.Fatalf("prompt_cache_key mismatch: %q vs %q\nfirst: %s\nsecond: %s", key1, key2, reqs[0].Body, reqs[1].Body)
	}
	if !strings.Contains(reqs[0].Body, "Question: summarize file A") || !strings.Contains(reqs[1].Body, "Question: summarize file B") {
		t.Fatalf("auto prompt_cache_key changed input text\nfirst: %s\nsecond: %s", reqs[0].Body, reqs[1].Body)
	}
}

func TestGatewayCanonicalAutoPromptCacheAffinityPinsStablePrefixBeforeAccountSelection(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11,\"input_tokens_details\":{\"cached_tokens\":8}}}}\n\n"))
	})
	h.importAccount(t, "auto-affinity-a", "upstream-auto-affinity-a", "access-auto-affinity-a")
	h.importAccount(t, "auto-affinity-b", "upstream-auto-affinity-b", "access-auto-affinity-b")

	prefix := strings.Repeat("stable repository snapshot line for cross-account cache affinity. ", 130)
	for _, question := range []string{"Question: summarize module A", "Question: summarize module B"} {
		body := `{"model":"gpt-cache-test","stream":true,"input":` + strconv.Quote(prefix+question) + `}`
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured requests = %d", len(reqs))
	}
	if reqs[0].AccountID != reqs[1].AccountID {
		t.Fatalf("same stable cache prefix should stay on one warm account, got %s then %s", reqs[0].AccountID, reqs[1].AccountID)
	}
	key1 := promptCacheKeyFromBody(t, reqs[0].Body)
	key2 := promptCacheKeyFromBody(t, reqs[1].Body)
	if key1 == "" || key2 == "" || key1 != key2 || !strings.HasPrefix(key1, "cp_") {
		t.Fatalf("upstream prompt_cache_key should be per-account and stable: %q vs %q", key1, key2)
	}
	h.app.WaitForAsyncWrites()
	var affinitySource, promptCacheKeySource, stablePrefixSource, stablePrefixReason, retentionEffective, retentionSource string
	var promptCacheKeyPresent int
	var stablePrefixBytes int64
	if err := h.store.DB().QueryRow(`SELECT affinity_source, prompt_cache_key_present, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, stable_prefix_bytes, retention_effective, retention_source FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(
		&affinitySource, &promptCacheKeyPresent, &promptCacheKeySource, &stablePrefixSource, &stablePrefixReason, &stablePrefixBytes, &retentionEffective, &retentionSource,
	); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if affinitySource != "cache_prefix_hash" || promptCacheKeyPresent != 1 || promptCacheKeySource != "auto_stable_prefix" {
		t.Fatalf("cache affinity diagnostics wrong: affinity=%q present=%d key_source=%q", affinitySource, promptCacheKeyPresent, promptCacheKeySource)
	}
	if stablePrefixSource != "text_prefix" || stablePrefixReason != "ok" || stablePrefixBytes <= 2048 {
		t.Fatalf("stable prefix diagnostics wrong: source=%q reason=%q bytes=%d", stablePrefixSource, stablePrefixReason, stablePrefixBytes)
	}
	if retentionEffective != "" || retentionSource != "unsupported_current_codex" {
		t.Fatalf("retention diagnostics wrong: effective=%q source=%q", retentionEffective, retentionSource)
	}
}

func TestGatewayAutoPromptCacheKeyForStablePrefixInsideMultimodalToolRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"))
	})
	h.importAccount(t, "auto-pck-mm-tool-prefix", "upstream-auto-pck-mm-tool-prefix", "access-auto-pck-mm-tool-prefix")
	prefix := strings.Repeat("stable multimodal repository snapshot line for cache reuse. ", 130)
	tools := `[
		{"type":"function","name":"inspect_asset","description":"Inspect a referenced asset and return structured findings.","parameters":{"type":"object","properties":{"asset_id":{"type":"string"},"focus":{"type":"string"}},"required":["asset_id"]}},
		{"type":"function","name":"search_repo","description":"Search the stable repository context without changing answer quality.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
	]`
	for _, tc := range []struct {
		image    string
		question string
	}{
		{"AAAA", "Question: inspect image A"},
		{"BBBB", "Question: inspect image B"},
	} {
		body := `{"model":"gpt-cache-test","stream":true,"reasoning":{"effort":"high"},"tools":` + tools + `,"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(prefix) + `},{"type":"input_image","image_url":"data:image/png;base64,` + tc.image + `"},{"type":"input_text","text":` + strconv.Quote(tc.question) + `}]}]}`
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured requests = %d", len(reqs))
	}
	key1 := promptCacheKeyFromBody(t, reqs[0].Body)
	key2 := promptCacheKeyFromBody(t, reqs[1].Body)
	if key1 == "" || key2 == "" || key1 != key2 {
		t.Fatalf("prompt_cache_key mismatch: %q vs %q\nfirst: %s\nsecond: %s", key1, key2, reqs[0].Body, reqs[1].Body)
	}
	for _, req := range reqs {
		if !strings.Contains(req.Body, `"type":"input_image"`) || !strings.Contains(req.Body, `"name":"inspect_asset"`) {
			t.Fatalf("auto prompt_cache_key changed multimodal/tool content:\n%s", req.Body)
		}
		if !strings.Contains(req.Body, `"effort":"high"`) {
			t.Fatalf("auto prompt_cache_key changed reasoning:\n%s", req.Body)
		}
	}
}

func promptCacheKeyFromBody(t *testing.T, body string) string {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("decode upstream body: %v\n%s", err, body)
	}
	key, _ := payload["prompt_cache_key"].(string)
	return key
}

func equalJSONIgnoringCodexTransportMetadata(t *testing.T, want, got string) bool {
	t.Helper()
	var wm map[string]interface{}
	var gm map[string]interface{}
	if err := json.Unmarshal([]byte(want), &wm); err != nil {
		t.Fatalf("decode wanted JSON: %v\n%s", err, want)
	}
	if err := json.Unmarshal([]byte(got), &gm); err != nil {
		t.Fatalf("decode got JSON: %v\n%s", err, got)
	}
	delete(wm, "prompt_cache_key")
	delete(gm, "prompt_cache_key")
	delete(wm, "client_metadata")
	delete(gm, "client_metadata")
	// Native Responses requests are augmented so a later account can replay the
	// complete reasoning context. Treat that mandatory transport field like the
	// other relay metadata while still comparing every input/tool item.
	if _, expected := wm["include"]; !expected && gm["include"] != nil {
		wm["include"] = []interface{}{"reasoning.encrypted_content"}
	}
	return reflect.DeepEqual(wm, gm)
}

func TestGatewayDoesNotAutoPromptCacheKeyForSmallRequest(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"total_tokens\":11}}}\n\n"))
	})
	h.importAccount(t, "small-no-pck", "upstream-small-no-pck", "access-small-no-pck")
	body := `{"model":"gpt-cache-test","stream":true,"input":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(upstreamBody, "prompt_cache_key") {
		t.Fatalf("small request should not get an automatic prompt_cache_key:\n%s", upstreamBody)
	}
}

func TestGatewaySanitizesInjectedPromptCacheRetention(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","model":"gpt-5.5","output_text":"ok","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	})
	h.importAccount(t, "cache-retention", "upstream-cache", "access-cache")
	if err := h.store.SetSetting(context.Background(), "codex_prompt_cache_retention", "not-a-valid-retention"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if strings.Contains(reqs[0].Body, "not-a-valid-retention") || strings.Contains(reqs[0].Body, "prompt_cache_retention") {
		t.Fatalf("invalid operator retention was injected upstream:\n%s", reqs[0].Body)
	}

	resp, err = http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","prompt_cache_retention":"in_memory","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	reqs = h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if strings.Contains(reqs[1].Body, "prompt_cache_retention") || strings.Contains(reqs[1].Body, "not-a-valid-retention") {
		t.Fatalf("HTTP/SSE retention should be stripped cleanly:\n%s", reqs[1].Body)
	}
}

// TestGatewayStreamingRecordsUsage is the regression for the "overview is just a
// demo" report: a streamed /v1/responses completion must record token usage (parsed
// from the response.completed SSE frame), so /admin/usage + /admin/usage/timeseries
// — and the whole admin overview built on them — reflect real traffic instead of
// staying empty. Before the fix, recordUsage ran only on the non-streaming branch.
func TestGatewayStreamingRecordsUsage(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.5\"}}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.5\",\"usage\":{\"input_tokens\":120,\"output_tokens\":34,\"total_tokens\":154,\"input_tokens_details\":{\"cached_tokens\":40}}}}\n\n" +
			"data: [DONE]\n\n"))
	})
	h.importAccount(t, "stream-usage", "upstream-su", "access-su")
	body := `{"model":"gpt","stream":true,"input":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	// Usage rows are recorded asynchronously off the request path; drain the writer so
	// the row this streamed completion enqueued is durable before we assert on it.
	h.app.WaitForAsyncWrites()
	var prompt, completion, total, cached int64
	var model string
	row := h.store.DB().QueryRow(`SELECT model, prompt_tokens, completion_tokens, total_tokens, cached_tokens FROM usage_records ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&model, &prompt, &completion, &total, &cached); err != nil {
		t.Fatalf("no usage record written for a streamed response: %v", err)
	}
	if model != "gpt-5.5" || prompt != 120 || completion != 34 || total != 154 || cached != 40 {
		t.Fatalf("streamed usage mis-recorded: model=%q prompt=%d completion=%d total=%d cached=%d", model, prompt, completion, total, cached)
	}

	// And it surfaces through the admin endpoint the overview reads.
	ar, err := http.Get(h.pool.URL + "/admin/usage")
	if err != nil {
		t.Fatal(err)
	}
	var usageBody struct {
		Rows []map[string]interface{} `json:"rows"`
	}
	_ = json.NewDecoder(ar.Body).Decode(&usageBody)
	ar.Body.Close()
	if len(usageBody.Rows) == 0 {
		t.Fatal("/admin/usage returned empty after a streamed completion — overview would show a demo")
	}
}

func TestGatewayStreamingRecordsUsageWithoutEventStreamContentType(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.4\"}}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":21,\"output_tokens\":9,\"total_tokens\":30,\"input_tokens_details\":{\"cached_tokens\":7}}}}\n\n" +
			"data: [DONE]\n\n"))
	})
	h.importAccount(t, "stream-usage-no-ct", "upstream-su-no-ct", "access-su-no-ct")
	body := `{"model":"gpt-5.4","stream":true,"input":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("gateway should normalize streamed content type, got %q", resp.Header.Get("Content-Type"))
	}

	h.app.WaitForAsyncWrites()
	var prompt, completion, total, cached int64
	var model string
	row := h.store.DB().QueryRow(`SELECT model, prompt_tokens, completion_tokens, total_tokens, cached_tokens FROM usage_records ORDER BY id DESC LIMIT 1`)
	if err := row.Scan(&model, &prompt, &completion, &total, &cached); err != nil {
		t.Fatalf("no usage record written for streamed response without event-stream header: %v", err)
	}
	if model != "gpt-5.4" || prompt != 21 || completion != 9 || total != 30 || cached != 7 {
		t.Fatalf("streamed usage mis-recorded without event-stream header: model=%q prompt=%d completion=%d total=%d cached=%d", model, prompt, completion, total, cached)
	}
}

func TestGatewayChatStreamConvertsResponsesSSEWithoutEventStreamContentType(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody := readBody(t, r)
		if strings.Contains(upstreamBody, "stream_options") || strings.Contains(upstreamBody, "reasoning_effort") {
			t.Fatalf("Chat-only fields leaked to Responses upstream: %s", upstreamBody)
		}
		if !strings.Contains(upstreamBody, `"reasoning":{"effort":"high"`) {
			t.Fatalf("reasoning effort was not translated: %s", upstreamBody)
		}
		_, _ = w.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_chat_stream\",\"model\":\"gpt-5.4\"}}\n\n" +
			"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"chat stream ok\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_chat_stream\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":5,\"output_tokens\":3,\"total_tokens\":8,\"input_tokens_details\":{\"cached_tokens\":2},\"output_tokens_details\":{\"reasoning_tokens\":1}}}}\n\n" +
			"data: [DONE]\n\n"))
	})
	h.importAccount(t, "chat-stream-no-ct", "upstream-chat-stream-no-ct", "access-chat-stream-no-ct")

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-5.4","messages":[{"role":"user","content":"reply exactly: chat stream ok"}],"reasoning_effort":"high","stream":true,"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, `"content":"chat stream ok"`) {
		t.Fatalf("chat stream was not converted to chat chunks:\n%s", body)
	}
	if strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("raw Responses SSE leaked to chat downstream:\n%s", body)
	}
	for _, want := range []string{`"choices":[]`, `"prompt_tokens":5`, `"completion_tokens":3`, `"cached_tokens":2`, `"reasoning_tokens":1`} {
		if !strings.Contains(body, want) {
			t.Fatalf("requested Chat stream usage is incomplete (%s):\n%s", want, body)
		}
	}
}

func TestGatewayNonStreamingChatUsesStreamingCodexUpstream(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"Stream must be set to true"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_chat_streamed\",\"model\":\"gpt-5.4\"}}\n\n" +
			"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"chat nonstream ok\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_chat_streamed\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":11,\"output_tokens\":3,\"total_tokens\":14,\"input_tokens_details\":{\"cached_tokens\":2}}}}\n\n" +
			"data: [DONE]\n\n"))
	})
	h.importAccount(t, "chat-nonstream-wham", "upstream-chat-nonstream", "access-chat-nonstream")

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-5.4","messages":[{"role":"user","content":"reply exactly: chat nonstream ok"}],"stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("downstream response is not JSON: %v\n%s", err, raw)
	}
	if got["object"] != "chat.completion" {
		t.Fatalf("object = %v, body=%s", got["object"], raw)
	}
	choices, _ := got["choices"].([]interface{})
	if len(choices) != 1 {
		t.Fatalf("choices missing: %s", raw)
	}
	msg, _ := choices[0].(map[string]interface{})["message"].(map[string]interface{})
	if msg["content"] != "chat nonstream ok" {
		t.Fatalf("chat content = %v, body=%s", msg["content"], raw)
	}
	usage, _ := got["usage"].(map[string]interface{})
	if usage["total_tokens"] != float64(14) {
		t.Fatalf("usage not preserved from SSE completion: %#v", usage)
	}
}

func TestGatewayNonStreamingChatUpstreamBodyStableAcrossIdenticalRequests(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if body["stream"] != true {
			t.Fatalf("non-stream chat should use streaming Codex upstream body: %s", raw)
		}
		reasoning, _ := body["reasoning"].(map[string]interface{})
		if reasoning["effort"] != "high" {
			t.Fatalf("reasoning effort changed or dropped: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n" +
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stable\",\"model\":\"gpt-5.4\"}}\n\n" +
			"event: response.output_text.delta\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n" +
			"event: response.completed\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stable\",\"model\":\"gpt-5.4\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1,\"total_tokens\":8}}}\n\n" +
			"data: [DONE]\n\n"))
	})
	h.importAccount(t, "chat-body-stable", "upstream-chat-body-stable", "access-chat-body-stable")

	body := `{"model":"gpt-5.4","reasoning":{"effort":"high"},"messages":[{"role":"system","content":"stable prefix"},{"role":"user","content":"reply ok"}],"stream":false}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d", i+1, resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("captured requests = %d", len(reqs))
	}
	if !equalJSONIgnoringCodexTransportMetadata(t, reqs[0].Body, reqs[1].Body) {
		t.Fatalf("identical downstream chat requests produced different upstream bodies\nfirst:  %s\nsecond: %s", reqs[0].Body, reqs[1].Body)
	}
}

func TestGatewayPreservesResponsesToolItemsWhenVirtual2MEnabled(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc := h.importAccount(t, "tool-input", "upstream-tool", "access-tool")
	setTestCapability(t, h, acc, "gpt", 1024)

	large := strings.Repeat("A", 20000)
	// instructions AND store mirror the real Codex client (which always sends its base
	// prompt + store:false); the relay's Codex dispatch backfills BOTH when absent
	// (normalizeCodexResponsesBody), and backfilling re-marshals the body (reordering keys),
	// so a body missing EITHER fingerprint field would no longer be forwarded byte-for-byte.
	// This test's purpose is that tool/reasoning input items survive 2M virtualization
	// unchanged, not that a body missing those fields passes through. (The byte-fidelity
	// fast path in normalizeCodexResponsesBody returns the bytes verbatim only when both are
	// already present and correct — exactly the real-client shape.)
	body := `{"model":"gpt","instructions":"You are a coding agent.","store":false,"input":[` +
		`{"role":"user","content":"old ` + large + `"},` +
		`{"type":"reasoning","id":"rs_1","summary":[]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"guide.pdf\"}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"page text"},` +
		`{"role":"user","content":"continue"}` +
		`]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
	if !equalJSONIgnoringCodexTransportMetadata(t, body, upstreamBody) {
		t.Fatalf("tool/reasoning Responses input changed before upstream\nwant: %s\n got: %s", body, upstreamBody)
	}
}

func TestGatewayPreservesResponsesAttachmentsWhenVirtual2MEnabled(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc := h.importAccount(t, "attachment-input", "upstream-attachment", "access-attachment")
	setTestCapability(t, h, acc, "gpt", 1024)

	large := strings.Repeat("A", 20000)
	// instructions + store mirror the real Codex client and keep the body byte-stable
	// through normalizeCodexResponsesBody (which re-marshals — reordering keys — when
	// either is absent) — see the note in TestGatewayPreservesResponsesToolItemsWhenVirtual2MEnabled.
	body := `{"model":"gpt","instructions":"You are a coding agent.","store":false,"input":[` +
		`{"role":"user","content":"old ` + large + `"},` +
		`{"role":"user","content":[` +
		`{"type":"input_text","text":"summarize this page"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,AAAA"},` +
		`{"type":"input_file","filename":"guide.pdf","file_data":"data:application/pdf;base64,BBBB"}` +
		`]}` +
		`]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
	if promptCacheKeyFromBody(t, upstreamBody) == "" {
		t.Fatalf("attachment request with large stable prefix did not get prompt_cache_key:\n%s", upstreamBody)
	}
	if !equalJSONIgnoringCodexTransportMetadata(t, body, upstreamBody) {
		t.Fatalf("attachment Responses input changed before upstream\nwant: %s\n got: %s", body, upstreamBody)
	}
}

func TestGatewayCompactUnary(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses/compact" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"compact-1","output_text":"summary"}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	resp, err := http.Post(h.pool.URL+"/v1/responses/compact", "application/json", strings.NewReader(`{"model":"gpt","input":[{"role":"user","content":"old"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
}

func TestGatewayCompactSkipsLocalTokenBudget(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses/compact" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"compact-1","output_text":"summary"}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	cfg := h.app.scheduler.Config()
	cfg.AccountTokenBudget = 1
	h.app.scheduler.UpdateConfig(cfg)
	held, err := h.app.scheduler.Select(context.Background(), scheduler.Route{Group: "cyber", Provider: "codex", EstimatedTokens: 1})
	if err != nil {
		t.Fatalf("hold account: %v", err)
	}
	defer held.Release()

	body := `{"model":"gpt","input":"` + strings.Repeat("x", 2000) + `"}`
	resp, err := http.Post(h.pool.URL+"/v1/responses/compact", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("compact status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
}

func TestGatewayCompactionTriggerSkipsLocalTokenBudget(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-trigger","output_text":"summary"}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	cfg := h.app.scheduler.Config()
	cfg.AccountTokenBudget = 1
	h.app.scheduler.UpdateConfig(cfg)
	held, err := h.app.scheduler.Select(context.Background(), scheduler.Route{Group: "cyber", Provider: "codex", EstimatedTokens: 1})
	if err != nil {
		t.Fatalf("hold account: %v", err)
	}
	defer held.Release()

	body := `{"model":"gpt","compaction_trigger":true,"input":"` + strings.Repeat("x", 2000) + `"}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("compaction-trigger status = %d, want 200: %s", resp.StatusCode, raw)
	}
	if reqs := h.requests(); len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
}

func TestGatewayChatCompletionsConvertsToResponsesAndBack(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if !strings.Contains(readBody(t, r), `"input"`) {
			t.Fatalf("chat body was not converted")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","model":"gpt","output_text":"answer","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":5}}}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "chat.completion" {
		t.Fatalf("object = %#v", root["object"])
	}
}

func TestCyberPromptPatchInjectsWithoutOverwritingDownstreamInstructions(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if !strings.Contains(body, `"instructions":"cyber\n\ndownstream"`) {
			t.Fatalf("prompt not prepended correctly: %s", body)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	patch := `{"system_prompt":"cyber"}`
	req, _ := http.NewRequest(http.MethodPatch, h.pool.URL+"/admin/groups/cyber", strings.NewReader(patch))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d", resp.StatusCode)
	}
	resp, err = http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","instructions":"downstream","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
}

func TestRefreshTokenUpdatesAccessTokenUsedByGateway(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("oauth method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if scope := r.Form.Get("scope"); !strings.Contains(scope, "api.connectors.invoke") {
			t.Fatalf("refresh scope = %q, want Codex connector scope", scope)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer oauth.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer new-access" {
			t.Fatalf("auth = %q", got)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	// Override config-driven OAuth URL by rebuilding the app against the same store/upstream.
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
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

	acc := h.importAccount(t, "a", "upstream-a", "old-access")
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d", resp.StatusCode)
	}
	resp, err = http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
}

func TestGatewayRefreshesCodexTokenAfterAuthExpired(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if scope := r.Form.Get("scope"); !strings.Contains(scope, "api.connectors.invoke") {
			t.Fatalf("refresh scope = %q, want Codex connector scope", scope)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer oauth.Close()

	var mu sync.Mutex
	var seen []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer old-access" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token expired"}`))
			return
		}
		if auth != "Bearer new-access" {
			t.Fatalf("auth = %q", auth)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
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

	account := storage.Account{ID: "acc-expired", Label: "expired", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-expired", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "old-access", RefreshToken: "refresh-old"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "Bearer old-access" || seen[1] != "Bearer new-access" {
		t.Fatalf("expected old token then refreshed token, saw %v", seen)
	}
}

func TestGatewayQuarantinesCodexAccountOnInvalidatedRefresh(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Your refresh token has been invalidated. Please try signing in again.","code":"refresh_token_invalidated"}}`))
	}))
	defer oauth.Close()

	var mu sync.Mutex
	var seen []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer old-access" {
			w.Header().Set("cf-ray", "ray-json-auth")
			w.Header().Set("server", "cloudflare")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"token expired"}`))
			return
		}
		if auth != "Bearer good-access" {
			t.Fatalf("auth = %q", auth)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
	cfg.StickyWaitMillis = 1
	cfg.FailoverMaxAttempts = 2
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     h.store,
		Scheduler: scheduler.New(h.store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(h.store, cfg),
	})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	acc1 := storage.Account{ID: "acc-a-refresh-invalid", Label: "expired", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-old", Status: "active"}
	if err := h.store.UpsertAccount(ctx, acc1, storage.AccountToken{AccessToken: "old-access", RefreshToken: "refresh-old"}); err != nil {
		t.Fatal(err)
	}
	acc2 := storage.Account{ID: "acc-z-good", Label: "good", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-good", Status: "active"}
	if err := h.store.UpsertAccount(ctx, acc2, storage.AccountToken{AccessToken: "good-access", RefreshToken: "refresh-good"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	gotAcc, err := h.store.GetAccount(ctx, acc1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotAcc.QuarantineUntil <= storage.Now() || !strings.Contains(gotAcc.QuarantineReason, "refresh_token_invalidated") {
		t.Fatalf("account was not auth-quarantined: %+v", gotAcc)
	}
	events, err := h.store.ListCFEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("bare cf-ray JSON auth error should not create CF events: %+v", events)
	}
	audit, err := h.store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The auth_quarantine audit must be present, but it need not be audit[0]: a
	// successful failover to the good account can emit an unrelated diagnostic audit
	// (e.g. codex_no_ratelimit_headers) that lands newer. Search for the entry.
	foundQuarantine := false
	for _, a := range audit {
		if a.Action == "auth_quarantine" && a.Reason == "refresh_token_invalidated" {
			foundQuarantine = true
			break
		}
	}
	if !foundQuarantine {
		t.Fatalf("missing auth quarantine audit: %+v", audit)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "Bearer old-access" || seen[len(seen)-1] != "Bearer good-access" {
		t.Fatalf("expected invalid account then good account, saw %v", seen)
	}
}

func TestGatewayCFRayEdgeFailoversWithoutRefreshOrCFEvent(t *testing.T) {
	var oauthHits int
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer oauth.Close()

	var mu sync.Mutex
	var seen []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, auth)
		mu.Unlock()
		if auth == "Bearer cf-edge-access" {
			w.Header().Set("cf-ray", "ray-edge-only")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"challenge"}`))
			return
		}
		if auth != "Bearer good-access" {
			t.Fatalf("auth = %q", auth)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
	cfg.StickyWaitMillis = 1
	cfg.FailoverMaxAttempts = 2
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     h.store,
		Scheduler: scheduler.New(h.store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(h.store, cfg),
	})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	acc1 := storage.Account{ID: "acc-a-cf-edge", Label: "cf-edge", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-edge", Status: "active"}
	if err := h.store.UpsertAccount(ctx, acc1, storage.AccountToken{AccessToken: "cf-edge-access", RefreshToken: "refresh-edge"}); err != nil {
		t.Fatal(err)
	}
	acc2 := storage.Account{ID: "acc-z-good-edge", Label: "good", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-good", Status: "active"}
	if err := h.store.UpsertAccount(ctx, acc2, storage.AccountToken{AccessToken: "good-access", RefreshToken: "refresh-good"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	if oauthHits != 0 {
		t.Fatalf("cf-ray edge path should not refresh token, oauth hits=%d", oauthHits)
	}
	events, err := h.store.ListCFEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("cf-ray edge-only should not create CF events: %+v", events)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "Bearer cf-edge-access" || seen[len(seen)-1] != "Bearer good-access" {
		t.Fatalf("expected cf-edge account then good account, saw %v", seen)
	}
}

func TestGatewayHandlesMissingScopeWithoutQuarantine(t *testing.T) {
	var oauthHits int
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer oauth.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-ray", "ray-missing-scope")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: api.responses.write."}}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
	cfg.StickyWaitMillis = 1
	cfg.FailoverMaxAttempts = 2
	app := NewServer(Dependencies{
		Config:    cfg,
		Store:     h.store,
		Scheduler: scheduler.New(h.store, cfg),
		Upstream:  upstream.NewClient(cfg),
		Planner:   virtual.NewPlanner(h.store, cfg),
	})
	h.pool = httptest.NewServer(app)
	defer h.pool.Close()

	ctx := context.Background()
	acc := storage.Account{ID: "acc-missing-scope", Label: "missing-scope", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-scope", Status: "active"}
	if err := h.store.UpsertAccount(ctx, acc, storage.AccountToken{AccessToken: "scope-access", RefreshToken: "refresh-scope"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(body), "api.responses.write") {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	if oauthHits != 0 {
		t.Fatalf("missing scope should not refresh token, oauth hits=%d", oauthHits)
	}
	gotAcc, err := h.store.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	// PermissionDenied is a function-level failure. The account should remain active
	// and must not receive account-wide cooldown/recheck state.
	if gotAcc.QuarantineUntil > storage.Now() {
		t.Fatalf("account should NOT be quarantined for PermissionDenied (was Session <31 behavior): quarantine_until=%d reason=%q", gotAcc.QuarantineUntil, gotAcc.QuarantineReason)
	}
	binding, err := h.store.GetEgressBinding(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("PermissionDenied must not set binding cooldown/recheck: %+v", binding)
	}
	// Verify the error was audit-logged (not quarantined, but still tracked).
	logs, err := h.store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, log := range logs {
		if log.AccountID == acc.ID && log.Action == "permission_denied_no_quarantine" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected permission_denied_no_quarantine audit log, got %d logs", len(logs))
	}
	events, err := h.store.ListCFEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("missing scope cf-ray should not create CF events: %+v", events)
	}
	// Session 31: Verify the audit action changed from "auth_quarantine" to
	// "permission_denied_no_quarantine" (the new non-quarantining behavior).
	audit, err := h.store.ListAuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) == 0 || audit[0].Action != "permission_denied_no_quarantine" || audit[0].State != string(ban.PermissionDenied) {
		t.Fatalf("expected permission_denied_no_quarantine audit, got: %+v", audit)
	}
}

func TestGatewayUsesCurrentCodexVersionForGatedModel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("version"); got != config.DefaultClientVersion {
			t.Fatalf("version header = %q, want %q", got, config.DefaultClientVersion)
		}
		if got := r.Header.Get("x-codex-beta-features"); got != "remote_compaction_v2" {
			t.Fatalf("beta features = %q", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "/"+config.DefaultClientVersion+" ") {
			t.Fatalf("UA = %q, want current Codex version %s", ua, config.DefaultClientVersion)
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.importAccount(t, "gated", "acct-gated", "access-gated")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.5","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d", resp.StatusCode)
	}
}

func TestGatewayUsesResponsesWebSocketForGatedStreamingModel(t *testing.T) {
	var gotHeaders http.Header
	var gotPayload map[string]interface{}
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s", r.Method)
		}
		gotHeaders = r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read ws request: %v", err)
		}
		if err := json.Unmarshal(raw, &gotPayload); err != nil {
			t.Fatalf("payload json: %v\n%s", err, raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_ws","model":"gpt-5.5"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
	})
	h.importAccount(t, "gated-ws", "acct-gated-ws", "access-gated-ws")

	body := `{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), "event: response.completed") || !strings.Contains(string(got), "data: [DONE]") {
		t.Fatalf("gateway SSE body missing completed event:\n%s", got)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/backend-api/codex/responses" {
		t.Fatalf("upstream requests = %+v", reqs)
	}
	if gotHeaders.Get("OpenAI-Beta") != "responses_websockets=2026-02-06" {
		t.Fatalf("OpenAI-Beta = %q", gotHeaders.Get("OpenAI-Beta"))
	}
	if gotHeaders.Get("version") != config.DefaultClientVersion {
		t.Fatalf("WS version = %q", gotHeaders.Get("version"))
	}
	if gotHeaders.Get("x-codex-beta-features") != "remote_compaction_v2" {
		t.Fatalf("WS beta features = %q", gotHeaders.Get("x-codex-beta-features"))
	}
	if ua := gotHeaders.Get("User-Agent"); !strings.Contains(ua, "/"+config.DefaultClientVersion+" ") {
		t.Fatalf("WS UA = %q, want current Codex version %s", ua, config.DefaultClientVersion)
	}
	if gotPayload["type"] != "response.create" || gotPayload["model"] != "gpt-5.5" || gotPayload["stream"] != true {
		t.Fatalf("bad websocket payload: %+v", gotPayload)
	}
}

func TestGatewayFallsBackToHTTPSSEWhenGatedModelWebSocketMissingScope(t *testing.T) {
	var wsHits int32
	var httpHits int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/codex/responses":
			atomic.AddInt32(&wsHits, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: api.responses.write.","type":"invalid_request_error"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/codex/responses":
			atomic.AddInt32(&httpHits, 1)
			raw, _ := io.ReadAll(r.Body)
			if strings.Contains(string(raw), "prompt_cache_retention") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail":"Unsupported parameter: prompt_cache_retention"}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\n"))
			_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_fallback","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		}
	})
	accountID := h.importAccount(t, "gated-ws-fallback", "acct-gated-ws-fallback", "access-gated-ws-fallback")

	body := `{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), "resp_fallback") || !strings.Contains(string(got), "data: [DONE]") {
		t.Fatalf("gateway SSE body missing fallback response:\n%s", got)
	}
	if atomic.LoadInt32(&wsHits) != 1 || atomic.LoadInt32(&httpHits) != 1 {
		t.Fatalf("wsHits=%d httpHits=%d", wsHits, httpHits)
	}
	reqs := h.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[0].Path != "/backend-api/codex/responses" ||
		reqs[1].Method != http.MethodPost || reqs[1].Path != "/backend-api/codex/responses" {
		t.Fatalf("upstream requests = %+v", reqs)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("websocket permission fallback must not bench account: %+v", binding)
	}
}

func TestGatewayAcceptsDownstreamResponsesWebSocket(t *testing.T) {
	var gotPayload map[string]interface{}
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != http.MethodGet {
			t.Fatalf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream ws request: %v", err)
		}
		if err := json.Unmarshal(raw, &gotPayload); err != nil {
			t.Fatalf("payload json: %v\n%s", err, raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_downstream_ws","model":"gpt-5.5"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_downstream_ws","model":"gpt-5.5","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
	})
	h.importAccount(t, "downstream-ws", "acct-downstream-ws", "access-downstream-ws")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("downstream dial: %v status=%d body=%s", err, resp.StatusCode, raw)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.5","thread_id":"downstream-thread","session_id":"downstream-session","conversation_id":"downstream-conversation","input":[{"role":"user","content":"hi"}],"stream":true}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("write downstream ws request: %v", err)
	}
	_, created, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read created: %v", err)
	}
	_, completed, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read completed: %v", err)
	}
	if strings.Contains(string(created), "event:") || !strings.Contains(string(created), `"type":"response.created"`) {
		t.Fatalf("created frame is not raw websocket JSON: %s", created)
	}
	if strings.Contains(string(completed), "event:") || !strings.Contains(string(completed), `"type":"response.completed"`) {
		t.Fatalf("completed frame is not raw websocket JSON: %s", completed)
	}
	if gotPayload["type"] != "response.create" || gotPayload["model"] != "gpt-5.5" || gotPayload["stream"] != true {
		t.Fatalf("bad upstream websocket payload: %+v", gotPayload)
	}
	for _, unsupported := range []string{"thread_id", "session_id", "conversation_id"} {
		if _, present := gotPayload[unsupported]; present {
			t.Fatalf("unsupported top-level transport field %q leaked upstream: %+v", unsupported, gotPayload)
		}
	}
}

func TestDownstreamResponsesWebSocketNeverLeaksRepeatedOrphanedToolOutput(t *testing.T) {
	const callID = "call_GQqnpD0cS3uxvXlgWBSD974z"
	var attempts int32
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read upstream WS request: %v", err)
			return
		}
		atomic.AddInt32(&attempts, 1)
		inner := `{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id ` + callID + `.","param":"input"},"status":400}`
		payload, _ := json.Marshal(map[string]interface{}{
			"type":        "error",
			"status_code": 400,
			"error": map[string]interface{}{
				"type": "upstream_error", "message": inner,
			},
		})
		_ = conn.WriteMessage(websocket.TextMessage, payload)
	})
	accountID := h.importAccount(t, "downstream-ws-repeat", "acct-downstream-ws-repeat", "access-downstream-ws-repeat")
	if err := h.store.SetSetting(context.Background(), "leak_scrub", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.5","previous_response_id":"resp_missing","stream":true,"input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"keep"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("downstream dial: %v status=%d body=%s", err, resp.StatusCode, raw)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.5","previous_response_id":"resp_missing","input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"keep"}],"stream":true}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, terminal, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read safe downstream terminal error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("upstream attempts=%d, want exactly 2", got)
	}
	for _, leak := range []string{"No tool call found", callID, "invalid_request_error"} {
		if strings.Contains(string(terminal), leak) {
			t.Fatalf("downstream WebSocket leaked %q: %s", leak, terminal)
		}
	}
	if !strings.Contains(string(terminal), `"status":503`) || !strings.Contains(string(terminal), "server_error") {
		t.Fatalf("downstream WebSocket did not receive a stable safe error: %s", terminal)
	}
}

func TestGatewayDownstreamWebSocketKeepsWarmupStateOnOneUpstreamConnection(t *testing.T) {
	var connections atomic.Int32
	var sawProcessed atomic.Bool
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" || r.Method != http.MethodGet {
			t.Fatalf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upstream upgrade: %v", err)
		}
		defer conn.Close()

		_, warmRaw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read warmup: %v", err)
		}
		var warm map[string]interface{}
		if json.Unmarshal(warmRaw, &warm) != nil || warm["type"] != "response.create" || warm["generate"] != false {
			t.Fatalf("bad warmup payload: %s", warmRaw)
		}
		warmInput, _ := warm["input"].([]interface{})
		var warmFirst map[string]interface{}
		if len(warmInput) > 0 {
			warmFirst, _ = warmInput[0].(map[string]interface{})
		}
		warmMetadata, _ := warm["client_metadata"].(map[string]interface{})
		if warmFirst["type"] != "additional_tools" || warmMetadata["ws_request_header_x_openai_internal_codex_responses_lite"] != "true" || warm["tools"] != nil || warm["instructions"] != nil {
			t.Fatalf("warmup lost Responses Lite wire shape: %s", warmRaw)
		}
		// Pretty-printed frames exercise the WS->SSE->WS multiline path too.
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{\n  \"type\": \"response.completed\",\n  \"response\": {\"id\": \"resp_warm\", \"model\": \"gpt-5.6-sol\", \"output\": [], \"usage\": {\"input_tokens\": 5, \"output_tokens\": 0, \"total_tokens\": 5}}\n}"))

		_, processedRaw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read response.processed: %v", err)
		}
		var processed map[string]interface{}
		if json.Unmarshal(processedRaw, &processed) != nil || processed["type"] != "response.processed" {
			t.Fatalf("response.processed was not forwarded: %s", processedRaw)
		}
		sawProcessed.Store(true)

		_, actualRaw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read actual request: %v", err)
		}
		var actual map[string]interface{}
		if json.Unmarshal(actualRaw, &actual) != nil || actual["type"] != "response.create" || actual["previous_response_id"] != "resp_warm" {
			t.Fatalf("warmup state was not reused on the same connection: %s", actualRaw)
		}
		actualInput, _ := actual["input"].([]interface{})
		var actualFirst map[string]interface{}
		if len(actualInput) > 0 {
			actualFirst, _ = actualInput[0].(map[string]interface{})
		}
		actualMetadata, _ := actual["client_metadata"].(map[string]interface{})
		if actualFirst["type"] != "message" || actualFirst["role"] != "developer" ||
			actualMetadata["ws_request_header_x_openai_internal_codex_responses_lite"] != "true" || actual["tools"] != nil || actual["instructions"] != nil {
			t.Fatalf("inference frame fell back from Responses Lite to classic: %s", actualRaw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_actual","model":"gpt-5.6-sol"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"PERSISTENT_WS_OK"}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{\n  \"type\": \"response.completed\",\n  \"response\": {\"id\": \"resp_actual\", \"model\": \"gpt-5.6-sol\", \"usage\": {\"input_tokens\": 7, \"output_tokens\": 3, \"total_tokens\": 10}}\n}"))
	})
	h.importAccount(t, "downstream-ws-persistent", "acct-downstream-ws-persistent", "access-downstream-ws-persistent")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","generate":false,"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"stable context"}]}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"stream":true}`)); err != nil {
		t.Fatal(err)
	}
	readUntilType := func(want string) []byte {
		t.Helper()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read %s: %v", want, err)
			}
			var event map[string]interface{}
			if json.Unmarshal(raw, &event) == nil && event["type"] == want {
				return raw
			}
		}
	}
	readUntilType("response.completed")
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.processed","response_id":"resp_warm"}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","previous_response_id":"resp_warm","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"stable context"}]},{"role":"user","content":"reply"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"stream":true}`)); err != nil {
		t.Fatal(err)
	}
	readUntilType("response.completed")
	if connections.Load() != 1 || !sawProcessed.Load() {
		t.Fatalf("persistent WS connections=%d processed=%v, want 1/true", connections.Load(), sawProcessed.Load())
	}
}

func TestSSEDataPayloadReassemblesMultilineJSON(t *testing.T) {
	block := []byte("event: response.completed\n" +
		"data: {\n" +
		"data:   \"type\": \"response.completed\",\n" +
		"data:   \"response\": {\"id\": \"resp_multiline\"}\n" +
		"data: }")
	payload := sseDataPayload(block)
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatalf("reassembled payload is invalid JSON: %v\n%s", err, payload)
	}
	if event["type"] != "response.completed" {
		t.Fatalf("event = %#v", event)
	}
}

func TestSSEDataPayloadConvertsSafetyBufferingHeartbeatForWebSocket(t *testing.T) {
	payload := sseDataPayload([]byte(safetyBufferingHeartbeatFrame))
	if payload != `{"type":"response.in_progress"}` {
		t.Fatalf("heartbeat payload = %q", payload)
	}
}

// uaCodexVersion extracts the version from a Codex User-Agent (codex_cli_rs/<ver> (..)).
// The real client sends the same value in its User-Agent and `version` header; these
// retry fixtures parse the UA to exercise the older-version branch independently.
func uaCodexVersion(ua string) string {
	i := strings.Index(ua, "/")
	if i < 0 {
		return ""
	}
	rest := ua[i+1:]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return rest[:j]
	}
	return rest
}

func TestGatewayRetriesWithCurrentCodexVersionOnVersionGate(t *testing.T) {
	var mu sync.Mutex
	var versions []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		version := uaCodexVersion(r.Header.Get("User-Agent"))
		mu.Lock()
		versions = append(versions, version)
		mu.Unlock()
		if version != config.DefaultClientVersion {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"The 'gpt-future' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.CodexCLIVersionOverride = "0.130.0"
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

	h.importAccount(t, "future", "acct-future", "access-future")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-future","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 {
		t.Fatalf("expected initial attempt + current-version retry, saw %v", versions)
	}
	if versions[0] != "0.130.0" || versions[1] != config.DefaultClientVersion {
		t.Fatalf("unexpected version retry sequence: %v", versions)
	}
}

func TestGatewayRetriesVersionGateStreamingRequestOverWebSocket(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var gotPayload map[string]interface{}
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		version := uaCodexVersion(r.Header.Get("User-Agent"))
		mu.Lock()
		seen = append(seen, r.Method+":"+r.URL.Path+":"+version)
		mu.Unlock()
		if r.Method == http.MethodPost {
			if r.URL.Path != "/backend-api/codex/responses" {
				t.Fatalf("initial path = %s", r.URL.Path)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"The 'gpt-future' model requires a newer version of Codex. Please upgrade to the latest app or CLI and try again."}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("retry request = %s %s", r.Method, r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read ws request: %v", err)
		}
		if err := json.Unmarshal(raw, &gotPayload); err != nil {
			t.Fatalf("payload json: %v\n%s", err, raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_retry","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.CodexCLIVersionOverride = "0.130.0"
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

	h.importAccount(t, "future-ws", "acct-future-ws", "access-future-ws")
	body := `{"model":"gpt-future","input":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", resp.StatusCode, got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected HTTP attempt + WS retry, saw %v", seen)
	}
	if seen[0] != "POST:/backend-api/codex/responses:0.130.0" || seen[1] != "GET:/backend-api/codex/responses:"+config.DefaultClientVersion {
		t.Fatalf("unexpected retry sequence: %v", seen)
	}
	if gotPayload["type"] != "response.create" || gotPayload["model"] != "gpt-future" || gotPayload["stream"] != true {
		t.Fatalf("bad websocket retry payload: %+v", gotPayload)
	}
}

func TestAdminTenantUserProjectAndAccountLifecycle(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	postJSON := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(h.pool.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("%s status=%d body=%s", path, resp.StatusCode, raw)
		}
		return resp
	}
	postJSON("/admin/tenants", `{"id":"tenant-1","name":"Tenant"}`).Body.Close()
	postJSON("/admin/users", `{"id":"user-1","tenant_id":"tenant-1","email":"user@example.internal"}`).Body.Close()
	postJSON("/admin/projects", `{"id":"project-1","tenant_id":"tenant-1","name":"Project","group_name":"cyber"}`).Body.Close()
	for _, path := range []string{"/admin/tenants", "/admin/users", "/admin/projects"} {
		resp, err := http.Get(h.pool.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "-1") {
			t.Fatalf("%s status/body = %d %s", path, resp.StatusCode, raw)
		}
	}
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	postJSON("/admin/accounts/"+acc+"/disable", `{}`).Body.Close()
	account, _ := h.store.GetAccount(context.Background(), acc)
	if account.Status != "disabled" {
		t.Fatalf("status = %q", account.Status)
	}
	postJSON("/admin/accounts/"+acc+"/enable", `{}`).Body.Close()
	req, _ := http.NewRequest(http.MethodDelete, h.pool.URL+"/admin/accounts/"+acc+"/delete", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := h.store.GetAccount(context.Background(), acc); !storage.NotFound(err) {
		t.Fatalf("account should be deleted, err=%v", err)
	}
}

func TestModelsProbeAdvertisesNativeWindowsOnly(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("ETag", "models-etag")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-native","context_window":2000000},{"id":"gpt-virtual","context_window":128000}]}`))
	})
	accountID := h.importAccount(t, "a", "upstream-a", "access-a")
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+accountID+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d", resp.StatusCode)
	}
	resp, err = http.Get(h.pool.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&root)
	data := root["data"].([]interface{})
	modes := map[string]string{}
	for _, item := range data {
		m := item.(map[string]interface{})
		modes[m["id"].(string)] = m["window_mode"].(string)
	}
	if modes["gpt-native"] != "native" {
		t.Fatalf("native mode = %q", modes["gpt-native"])
	}
	if modes["gpt-virtual"] != "native" {
		t.Fatalf("non-2m model must advertise its real native window, mode = %q", modes["gpt-virtual"])
	}
	for id, mode := range modes {
		if mode == "virtual_2m" {
			t.Fatalf("%s still advertises removed virtual_2m mode", id)
		}
	}
	if resp.Header.Get("ETag") == "" {
		t.Fatalf("missing etag")
	}
}

func TestMultiAgentSameParentUsesSameAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	client := &http.Client{}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":"hi"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-codex-parent-thread-id", "parent-same")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].AccountID != reqs[1].AccountID {
		t.Fatalf("accounts differ: %+v", reqs)
	}
}

func TestStrictStickyDoesNotCrossAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc1 := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: "hash-strict", RouteKey: "key", Source: "test", AccountID: acc1}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountQuarantine(context.Background(), acc1, storage.Now()+3600, "test"); err != nil {
		t.Fatal(err)
	}
	reqBody := `{"model":"gpt","previous_response_id":"resp_1","prompt_cache_key":"strict","input":"hi"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: acc1}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
}

func TestStatefulStrictStickyWaitsForPinnedAccountCapacity(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc1 := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	cfg := h.app.scheduler.Config()
	cfg.AccountTokenBudget = 1
	cfg.StatefulStickyWaitSeconds = 1
	h.app.scheduler.UpdateConfig(cfg)

	reqBody := `{"model":"gpt","previous_response_id":"resp_1","prompt_cache_key":"stateful-wait","input":"` + strings.Repeat("x", 2000) + `"}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: acc1}); err != nil {
		t.Fatal(err)
	}
	held, err := h.app.scheduler.Select(context.Background(), scheduler.Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 1})
	if err != nil {
		t.Fatalf("hold pinned account: %v", err)
	}
	if held.Account.ID != acc1 {
		t.Fatalf("held account = %q, want %q", held.Account.ID, acc1)
	}

	type responseResult struct {
		status int
		body   string
		err    error
	}
	result := make(chan responseResult, 1)
	go func() {
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
		if err != nil {
			result <- responseResult{err: err}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		result <- responseResult{status: resp.StatusCode, body: string(raw)}
	}()

	select {
	case got := <-result:
		t.Fatalf("stateful request returned before capacity release: status=%d body=%s err=%v", got.status, got.body, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	held.Release()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.status != http.StatusOK {
			t.Fatalf("stateful status = %d, want 200: %s", got.status, got.body)
		}
	case <-time.After(time.Second):
		t.Fatal("stateful request did not resume after pinned account capacity release")
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(reqs))
	}
	if reqs[0].AccountID != "upstream-a" {
		t.Fatalf("stateful request went to %q, want upstream-a", reqs[0].AccountID)
	}
}

func TestCFRetryToStandbyAndRecordsEvent(t *testing.T) {
	var count int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 1 {
			w.Header().Set("cf-mitigated", "challenge")
			w.Header().Set("cf-ray", "ray-1")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html>Just a moment</html>"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{ID: "standby", Name: "standby", Type: "direct", StreamCapable: true, Health: "healthy", MaxConcurrency: 10}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{AccountID: acc, PrimaryEgressID: storage.DefaultDirectEgressID, StandbyEgressIDs: "standby"}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	events, err := h.store.ListCFEvents(context.Background(), 10)
	if err != nil || len(events) != 1 || events[0].CFRay != "ray-1" {
		t.Fatalf("cf events err=%v events=%+v", err, events)
	}
}

func TestSidecarCookieJarIsIsolated(t *testing.T) {
	var sidecarPayload map[string]interface{}
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy" {
			t.Fatalf("sidecar path = %s", r.URL.Path)
		}
		// New protocol: routing metadata (incl. cookie_jar_key) rides the X-Sidecar-Meta
		// header; the request body is the raw HTTP body. Fall back to the legacy JSON body.
		if meta := r.Header.Get("X-Sidecar-Meta"); meta != "" {
			metaRaw, derr := base64.StdEncoding.DecodeString(meta)
			if derr != nil {
				t.Fatal(derr)
			}
			if err := json.Unmarshal(metaRaw, &sidecarPayload); err != nil {
				t.Fatal(err)
			}
		} else if err := json.NewDecoder(r.Body).Decode(&sidecarPayload); err != nil {
			t.Fatal(err)
		}
		h := http.Header{"Content-Type": []string{"application/json"}}
		hraw, _ := json.Marshal(h)
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(hraw))
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	}))
	defer sidecar.Close()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("direct upstream should not be called")
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{ID: "sidecar", Name: "sidecar", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, StreamCapable: true, Health: "healthy", MaxConcurrency: 10}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{AccountID: acc, PrimaryEgressID: "sidecar"}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sidecarPayload == nil {
		t.Fatalf("sidecar was not called")
	}
	key, _ := sidecarPayload["cookie_jar_key"].(string)
	if !strings.Contains(key, acc+":sidecar") || !strings.Contains(key, "127.0.0.1") {
		t.Fatalf("cookie jar key not isolated: %q", key)
	}
}

func TestSeamlessFailoverOnLimitSwitchesAccountTransparently(t *testing.T) {
	var mu sync.Mutex
	seenAuth := map[string]bool{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		mu.Lock()
		seenAuth[auth] = true
		mu.Unlock()
		// Account A is at its usage limit; any other account answers normally.
		if auth == "Bearer access-a" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")

	// Self-contained request (no previous_response_id) → eligible for seamless,
	// lossless failover. Pin it to A first so the limited account is tried first.
	reqBody := `{"model":"gpt","input":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	// The downstream must see ONE success, never the 429.
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Fatalf("expected transparent success after failover, got %d %s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seenAuth["Bearer access-a"] || !seenAuth["Bearer access-b"] {
		t.Fatalf("expected failover A→B (both accounts contacted), saw: %v", seenAuth)
	}
}

func TestStatefulTurnRebuildsOnAccountFailure(t *testing.T) {
	var aCalls int
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			aCalls++
			if aCalls == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_a_1","object":"response","status":"completed","model":"gpt","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"assistant answer"}]}],"output_text":"assistant answer","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit."}}`))
		case "Bearer access-b":
			bCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_b_1","object":"response","status":"completed","model":"gpt","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"output_text":"ok"}`))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")

	firstBody := `{"model":"gpt","prompt_cache_key":"conv-state","prompt_cache_retention":"24h","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"role":"user","content":"hello"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(firstBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(firstBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("initial response status=%d body=%s", resp.StatusCode, body)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	secondBody := `{"model":"gpt","previous_response_id":"resp_a_1","prompt_cache_key":"conv-state","prompt_cache_retention":"24h","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"role":"user","content":"next"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(secondBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "account-a-state")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-MiCliProxy-Context-Status") != "rebuilt" {
		t.Fatalf("stateful turn should rebuild on account B, got %d header=%q %s", resp.StatusCode, resp.Header.Get("X-MiCliProxy-Context-Status"), body)
	}
	if !bCalled {
		t.Fatalf("stateful turn was not retried on account B")
	}
	reqs := h.requests()
	var secondA capturedRequest
	for _, req := range reqs {
		if req.Auth == "Bearer access-a" && strings.Contains(req.Body, "next") {
			secondA = req
			break
		}
	}
	if secondA.Body == "" {
		t.Fatalf("missing captured second account A request: %+v", reqs)
	}
	for _, want := range []string{"previous_response_id", "resp_a_1"} {
		if !strings.Contains(secondA.Body, want) {
			t.Fatalf("stateful request lost upstream state %q: body=%s", want, secondA.Body)
		}
	}
	if secondA.TurnState != "account-a-state" {
		t.Fatalf("turn state header should be preserved for the same upstream account, got %q", secondA.TurnState)
	}
}

func TestOrphanedToolOutputHTTP400DegradesAndRetries(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			inner := `{"error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id call_1."}}`
			wrapped, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"type": "upstream_error", "message": inner}})
			_, _ = w.Write(wrapped)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_recovered","object":"response","status":"completed","model":"gpt","output_text":"ok"}`))
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt","previous_response_id":"resp_missing","input":[{"type":"custom_tool_call_output","call_id":"call_1","output":"preserve this result"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	keyReq.Header.Set("X-Codex-Turn-State", "old-state")
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "old-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_recovered") {
		t.Fatalf("recovery status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("context status=%q, want degraded", got)
	}
	requests := h.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream attempts=%d, want 2: %+v", len(requests), requests)
	}
	retry := requests[1]
	if retry.Auth != "Bearer access-b" || retry.TurnState != "" {
		t.Fatalf("recovery leaked account state: %+v", retry)
	}
	if strings.Contains(retry.Body, "previous_response_id") || strings.Contains(retry.Body, "custom_tool_call_output") {
		t.Fatalf("recovery leaked orphaned state/output: %s", retry.Body)
	}
	if !strings.Contains(retry.Body, "preserve this result") {
		t.Fatalf("recovery lost tool result text: %s", retry.Body)
	}
	account, err := h.store.GetAccount(context.Background(), accA)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("context error changed account health: %+v", account)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accA)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("context error cooled/recheck-gated the account: %+v", binding)
	}
	if got := atomic.LoadUint64(&h.app.contextDegraded); got != 1 {
		t.Fatalf("context degraded counter = %d, want 1", got)
	}
}

func TestPreviousResponseNotFoundHTTP400DegradesAndRetries(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if r.Header.Get("Authorization") == "Bearer access-previous-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Previous response resp_missing was not found.","type":"previous_response_not_found","param":"previous_response_id"}}`)
			return
		}
		if r.Header.Get("X-Codex-Turn-State") != "" || strings.Contains(body, "previous_response_id") || strings.Contains(body, `"turn_state"`) || strings.Contains(body, `"type":"function_call_output"`) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"error":"retry retained stale context: %s"}`, body)
			return
		}
		for _, want := range []string{"preserve previous-response tool result", "900719925474099312345", "encrypted-result"} {
			if !strings.Contains(body, want) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = fmt.Fprintf(w, `{"error":"degraded replay lost %s"}`, want)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_previous_degraded","object":"response","status":"completed","model":"gpt","output_text":"ok"}`)
	})
	accountA := h.importAccount(t, "previous-a", "upstream-previous-a", "access-previous-a")
	h.importAccount(t, "previous-b", "upstream-previous-b", "access-previous-b")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt","previous_response_id":"resp_missing","turn_state":{"opaque":"old"},"input":[{"type":"function_call_output","call_id":"call_previous","status":"completed","output":{"text":"preserve previous-response tool result","n":900719925474099312345},"encrypted_content":"encrypted-result"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	keyReq.Header.Set("X-Codex-Turn-State", "previous-account-state")
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "previous-account-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_previous_degraded") {
		t.Fatalf("recovery status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("context status=%q, want degraded", got)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-previous-a" || requests[1].Auth != "Bearer access-previous-b" {
		t.Fatalf("context recovery did not switch accounts once: %+v", requests)
	}
	account, err := h.store.GetAccount(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("previous-response error changed account health: %+v", account)
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("previous-response error cooled/recheck-gated the account: %+v", binding)
	}
	if got := atomic.LoadUint64(&h.app.contextDegraded); got != 1 {
		t.Fatalf("context degraded counter = %d, want 1", got)
	}
}

func TestPreviousResponseNotFoundRebuildsFromJournal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if r.Header.Get("Authorization") == "Bearer access-rebuild-a" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"type":"previous_response_not_found","message":"Previous response resp_previous_journal was not found."}}`)
			return
		}
		callIndex := strings.Index(body, `"type":"custom_tool_call"`)
		outputIndex := strings.Index(body, `"type":"custom_tool_call_output"`)
		if r.Header.Get("X-Codex-Turn-State") != "" || strings.Contains(body, "previous_response_id") || strings.Contains(body, `"turn_state"`) || callIndex < 0 || outputIndex <= callIndex || !strings.Contains(body, "journal result") || !strings.Contains(body, "900719925474099312345") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = fmt.Fprintf(w, `{"error":"invalid rebuilt replay: %s"}`, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_previous_rebuilt","object":"response","status":"completed","model":"gpt","output_text":"ok"}`)
	})
	accountA := h.importAccount(t, "rebuild-a", "upstream-rebuild-a", "access-rebuild-a")
	h.importAccount(t, "rebuild-b", "upstream-rebuild-b", "access-rebuild-b")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	journalPayload := `{"model":"gpt","turn_state":{"opaque":"journal"},"tools":[{"type":"function","name":"big","parameters":{"type":"object","properties":{"n":{"const":900719925474099312345}}}}],"input":[{"type":"custom_tool_call","call_id":"call_previous_journal","name":"apply_patch","input":"{}"}]}`
	if err := h.store.PutContextJournal(context.Background(), storage.ContextJournal{ResponseID: "resp_previous_journal", AccountID: accountA, Payload: journalPayload, ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt","previous_response_id":"resp_previous_journal","input":[{"type":"custom_tool_call_output","call_id":"call_previous_journal","output":"journal result"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	keyReq.Header.Set("X-Codex-Turn-State", "rebuild-account-state")
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "rebuild-account-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_previous_rebuilt") {
		t.Fatalf("rebuild status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "rebuilt" {
		t.Fatalf("context status=%q, want rebuilt", got)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-rebuild-a" || requests[1].Auth != "Bearer access-rebuild-b" {
		t.Fatalf("journal recovery did not switch accounts once: %+v", requests)
	}
	if got := atomic.LoadUint64(&h.app.contextRebuilt); got != 1 {
		t.Fatalf("context rebuilt counter = %d, want 1", got)
	}
}

func TestPreviousResponseNotFoundSingleAccountRepairsOnlyOnce(t *testing.T) {
	var attempts int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		inner := `{"type":"error","error":{"type":"previous_response_not_found","message":"Previous response resp_single_missing was not found."},"status":400}`
		wrapped, _ := json.Marshal(map[string]interface{}{
			"error":  map[string]interface{}{"type": "upstream_error", "message": inner},
			"status": 400,
			"type":   "error",
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(wrapped)
	})
	accountID := h.importAccount(t, "previous-single", "upstream-previous-single", "access-previous-single")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt","previous_response_id":"resp_single_missing","turn_state":{"opaque":"old"},"input":[{"role":"user","content":"continue"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("single-account status=%d attempts=%d body=%s", resp.StatusCode, attempts, responseBody)
	}
	if strings.Contains(string(responseBody), "previous_response_not_found") || !strings.Contains(string(responseBody), "server_error") {
		t.Fatalf("terminal context error was not neutralized: %s", responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("context status=%q, want degraded", got)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-previous-single" || requests[1].Auth != "Bearer access-previous-single" {
		t.Fatalf("single account was not retried exactly once: %+v", requests)
	}
	if strings.Contains(requests[1].Body, "previous_response_id") || strings.Contains(requests[1].Body, `"turn_state"`) || !strings.Contains(requests[1].Body, "continue") {
		t.Fatalf("single-account retry was not stateless: %s", requests[1].Body)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("repeated context error changed account health: %+v", account)
	}
	if got := atomic.LoadUint64(&h.app.contextDegraded); got != 1 {
		t.Fatalf("context degraded counter = %d, want exactly one", got)
	}
}

func TestRuleFailoverThenOrphanedPairedOutputRecoversAgain(t *testing.T) {
	const callID = "call_diagnostic_chain"
	recoveredPayload := make(chan []byte, 1)
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer access-chain-a":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\n"+
				`data: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`+"\n\n"+
				"data: [DONE]\n\n")
		case "Bearer access-chain-b":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: error\n"+
				`data: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id `+callID+`."}}`+"\n\n"+
				"data: [DONE]\n\n")
		case "Bearer access-chain-c":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade recovered request: %v", err)
				return
			}
			defer conn.Close()
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read recovered request: %v", err)
				return
			}
			recoveredPayload <- append([]byte(nil), payload...)
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"ok-from-chain-c"}`))
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_chain_c","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accountA := h.importAccount(t, "chain-a", "upstream-chain-a", "access-chain-a")
	accountB := h.importAccount(t, "chain-b", "upstream-chain-b", "access-chain-b")
	accountC := h.importAccount(t, "chain-c", "upstream-chain-c", "access-chain-c")
	for _, accountID := range []string{accountA, accountB, accountC} {
		setTestCapability(t, h, accountID, "gpt-5.6-sol", 1024)
	}
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "true"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "5"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID: "chain-hide-safety", Name: "hide safety", Enabled: true, Priority: 1,
		Providers: []string{"chatgpt"}, Entrypoints: []string{"responses"},
		AccountAction: "none", DownstreamAction: "hide_safety_buffering",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{
		ID: "chain-failover-400", Name: "fail over unsupported model", Enabled: true, Priority: 2,
		Providers: []string{"chatgpt"}, Entrypoints: []string{"responses"}, StatusCodes: []int{400},
		BodyKeywords: []string{"using Codex with a ChatGPT account."}, MatchMode: "all",
		AccountAction: "cooldown_recheck", DownstreamAction: "failover", CooldownSeconds: 1800,
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"diagnostic-chain","input":[{"type":"custom_tool_call","call_id":"` + callID + `","name":"apply_patch","input":"{}"},{"type":"custom_tool_call_output","call_id":"` + callID + `","status":"completed","output":{"text":"diagnostic tool result","n":900719925474099312345}}]}`
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
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "ok-from-chain-c") {
		t.Fatalf("multi-stage recovery status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("context status=%q, want degraded", got)
	}
	requests := h.requests()
	wantAuth := []string{"Bearer access-chain-a", "Bearer access-chain-b", "Bearer access-chain-c"}
	if len(requests) != len(wantAuth) {
		t.Fatalf("multi-stage attempts=%d want=%d: %+v", len(requests), len(wantAuth), requests)
	}
	for index, want := range wantAuth {
		if requests[index].Auth != want {
			t.Fatalf("auth[%d]=%q want=%q: %+v", index, requests[index].Auth, want, requests)
		}
	}
	var recoveredBody []byte
	select {
	case recoveredBody = <-recoveredPayload:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recovered websocket payload")
	}
	recoveredRoot, err := decodeContextJSONMap(recoveredBody)
	if err != nil {
		t.Fatalf("decode recovered request method=%s path=%s body=%q: %v", requests[2].Method, requests[2].Path, recoveredBody, err)
	}
	preservedResult := false
	for _, value := range recoveredRoot["input"].([]interface{}) {
		item, _ := value.(map[string]interface{})
		if streamString(item["call_id"]) == callID && (isToolCallItemType(streamString(item["type"])) || isToolOutputItemType(streamString(item["type"]))) {
			t.Fatalf("recovered request could execute completed tool again: %s", recoveredBody)
		}
		encoded, _ := json.Marshal(item)
		if item["role"] == "user" && strings.Contains(string(encoded), "diagnostic tool result") && strings.Contains(string(encoded), "900719925474099312345") {
			preservedResult = true
		}
	}
	if !preservedResult {
		t.Fatalf("recovered request lost tool result: %s", recoveredBody)
	}
	account, err := h.store.GetAccount(context.Background(), accountB)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("context error changed account B health: %+v", account)
	}
	if got := atomic.LoadUint64(&h.app.contextDegraded); got != 1 {
		t.Fatalf("context degraded counter=%d, want 1", got)
	}
}

func TestUnrelatedHTTP400DoesNotRecoverResponsesContext(t *testing.T) {
	var attempts int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_previous_response","message":"The request is invalid."}}`)
	})
	accountID := h.importAccount(t, "ordinary-400", "upstream-ordinary-400", "access-ordinary-400")
	body := `{"model":"gpt","previous_response_id":"resp_invalid","input":[{"role":"user","content":"continue"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("ordinary 400 status=%d attempts=%d body=%s", resp.StatusCode, attempts, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "" {
		t.Fatalf("ordinary 400 unexpectedly recovered context: %q", got)
	}
	if got := atomic.LoadUint64(&h.app.contextRebuilt) + atomic.LoadUint64(&h.app.contextDegraded); got != 0 {
		t.Fatalf("ordinary 400 changed context recovery counters: %d", got)
	}
}

func TestOrphanedToolOutputAccountSwitchRepairsIncompleteDownstreamContext(t *testing.T) {
	const callID = "call_LhyrFDALDxT1GOWETYWfgfRz"
	const observedError = `{"error":{"message":"{\n\"type\": \"error\",\n\"error\": {\n\"type\": \"invalid_request_error\",\n\"message\": \"No tool call found for custom tool call output with call_id call_LhyrFDALDxT1GOWETYWfgfRz.\",\n\"param\": \"input\"\n},\n\"status\": 400\n}"},"status":400,"type":"error"}`

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if r.Header.Get("Authorization") == "Bearer access-incomplete-a" ||
			strings.Contains(body, `"previous_response_id"`) ||
			strings.Contains(body, `"type":"custom_tool_call_output"`) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, observedError)
			return
		}
		// The second account only accepts a stateless replay that retained the tool
		// result as user data. This makes the test fail if account switching merely
		// resends the incomplete downstream context unchanged.
		for _, want := range []string{callID, "preserve structured result", "900719925474099312345", "encrypted-result"} {
			if !strings.Contains(body, want) {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = fmt.Fprintf(w, `{"error":"degraded replay lost %s"}`, want)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_incomplete_context_recovered","object":"response","status":"completed","model":"gpt","output_text":"ok"}`)
	})
	accountA := h.importAccount(t, "incomplete-a", "upstream-incomplete-a", "access-incomplete-a")
	h.importAccount(t, "incomplete-b", "upstream-incomplete-b", "access-incomplete-b")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt","previous_response_id":"resp_incomplete","input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","status":"completed","output":{"text":"preserve structured result","n":900719925474099312345},"encrypted_content":"encrypted-result","future_field":{"kept":true}}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	keyReq.Header.Set("X-Codex-Turn-State", "incomplete-account-state")
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "incomplete-account-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_incomplete_context_recovered") {
		t.Fatalf("account-switch repair status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("context status=%q, want degraded", got)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-incomplete-a" || requests[1].Auth != "Bearer access-incomplete-b" {
		t.Fatalf("repair did not switch accounts exactly once: %+v", requests)
	}
	if requests[1].TurnState != "" || strings.Contains(requests[1].Body, "previous_response_id") || strings.Contains(requests[1].Body, `"type":"custom_tool_call_output"`) {
		t.Fatalf("account B still received incomplete account-local context: %+v", requests[1])
	}
	account, err := h.store.GetAccount(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("orphaned-output 400 changed account A health: %+v", account)
	}
}

func TestOrphanedToolOutputAccountSwitchReplaysAutomaticallyPersistedCall(t *testing.T) {
	const callID = "call_LhyrFDALDxT1GOWETYWfgfRz"
	const observedError = `{"error":{"message":"{\n\"type\": \"error\",\n\"error\": {\n\"type\": \"invalid_request_error\",\n\"message\": \"No tool call found for custom tool call output with call_id call_LhyrFDALDxT1GOWETYWfgfRz.\",\n\"param\": \"input\"\n},\n\"status\": 400\n}"},"status":400,"type":"error"}`

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		switch r.Header.Get("Authorization") {
		case "Bearer access-journal-a":
			if strings.Contains(body, `"type":"custom_tool_call_output"`) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, observedError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_saved_custom_call","object":"response","status":"completed","model":"gpt","output":[{"type":"custom_tool_call","id":"ctc_saved","call_id":"`+callID+`","name":"apply_patch","input":"{\"patch\":\"saved\"}"}]}`)
		case "Bearer access-journal-b":
			callIndex := strings.Index(body, `"type":"custom_tool_call"`)
			outputIndex := strings.Index(body, `"type":"custom_tool_call_output"`)
			valid := !strings.Contains(body, "previous_response_id") && r.Header.Get("X-Codex-Turn-State") == "" &&
				callIndex >= 0 && outputIndex > callIndex && strings.Count(body, callID) >= 2 && strings.Contains(body, "saved tool result")
			if !valid {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, observedError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_journal_switch_recovered","object":"response","status":"completed","model":"gpt","output_text":"ok"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	accountA := h.importAccount(t, "journal-a", "upstream-journal-a", "access-journal-a")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}

	firstBody := `{"model":"gpt","prompt_cache_key":"journal-switch","tools":[{"type":"custom","name":"apply_patch","description":"apply a patch","format":{"type":"text"}}],"input":[{"role":"user","content":"generate call"}]}`
	firstResp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(firstBody))
	if err != nil {
		t.Fatal(err)
	}
	firstResponseBody, _ := io.ReadAll(firstResp.Body)
	firstResp.Body.Close()
	if firstResp.StatusCode != http.StatusOK || !strings.Contains(string(firstResponseBody), "resp_saved_custom_call") {
		t.Fatalf("first turn status=%d body=%s", firstResp.StatusCode, firstResponseBody)
	}
	journal, err := h.store.GetContextJournal(context.Background(), "resp_saved_custom_call")
	if err != nil {
		t.Fatalf("first turn did not automatically persist context journal: %v", err)
	}
	if !strings.Contains(journal.Payload, `"type":"custom_tool_call"`) || !strings.Contains(journal.Payload, callID) {
		t.Fatalf("persisted journal lost custom call/call_id: %s", journal.Payload)
	}

	h.importAccount(t, "journal-b", "upstream-journal-b", "access-journal-b")
	secondBody := `{"model":"gpt","previous_response_id":"resp_saved_custom_call","input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"saved tool result"}]}`
	secondReq, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(secondBody))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("X-Codex-Turn-State", "journal-account-a-state")
	secondResp, err := http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	secondResponseBody, _ := io.ReadAll(secondResp.Body)
	secondResp.Body.Close()
	if secondResp.StatusCode != http.StatusOK || !strings.Contains(string(secondResponseBody), "resp_journal_switch_recovered") {
		t.Fatalf("journal-backed switch status=%d body=%s", secondResp.StatusCode, secondResponseBody)
	}
	if got := secondResp.Header.Get("X-MiCliProxy-Context-Status"); got != "rebuilt" {
		t.Fatalf("context status=%q, want rebuilt", got)
	}
	requests := h.requests()
	if len(requests) != 3 || requests[0].Auth != "Bearer access-journal-a" || requests[1].Auth != "Bearer access-journal-a" || requests[2].Auth != "Bearer access-journal-b" {
		t.Fatalf("expected first turn A, failed continuation A, rebuilt retry B: %+v", requests)
	}
	account, err := h.store.GetAccount(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "active" || account.QuarantineUntil != 0 || account.QuarantineReason != "" {
		t.Fatalf("journal-repaired 400 changed account A health: %+v", account)
	}
}

func TestOrphanedToolOutputContextJournalSurvivesRapidAccountChain(t *testing.T) {
	const (
		callA = "call_chain_a"
		callB = "call_chain_b"
		callC = "call_chain_c"
	)
	writeOrphan := func(w http.ResponseWriter, callID string) {
		inner, _ := json.MarshalIndent(map[string]interface{}{
			"type": "error",
			"error": map[string]interface{}{
				"type": "invalid_request_error", "message": "No tool call found for custom tool call output with call_id " + callID + ".", "param": "input",
			},
			"status": 400,
		}, "", "  ")
		outer, _ := json.Marshal(map[string]interface{}{
			"error": map[string]interface{}{"message": string(inner)}, "status": 400, "type": "error",
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(outer)
	}
	writeCall := func(w http.ResponseWriter, responseID, callID string) {
		payload, _ := json.Marshal(map[string]interface{}{
			"id": responseID, "object": "response", "status": "completed", "model": "gpt",
			"output": []interface{}{map[string]interface{}{
				"type": "custom_tool_call", "id": "ctc_" + callID, "call_id": callID, "name": "apply_patch", "input": "{}",
			}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}
	hasHistory := func(raw []byte, expected map[string]string) bool {
		root, err := decodeContextJSONMap(raw)
		if err != nil || streamString(root["previous_response_id"]) != "" {
			return false
		}
		input, _ := root["input"].([]interface{})
		seenCalls := map[string]bool{}
		seenOutputs := map[string]string{}
		for _, rawItem := range input {
			item, _ := rawItem.(map[string]interface{})
			switch streamString(item["type"]) {
			case "custom_tool_call":
				seenCalls[streamString(item["call_id"])] = true
			case "custom_tool_call_output":
				callID := streamString(item["call_id"])
				if !seenCalls[callID] {
					return false
				}
				seenOutputs[callID] = streamString(item["output"])
			}
		}
		for callID, output := range expected {
			if !seenCalls[callID] || seenOutputs[callID] != output {
				return false
			}
		}
		return true
	}

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		switch r.Header.Get("Authorization") {
		case "Bearer access-chain-a":
			if strings.Contains(body, callA) && strings.Contains(body, "custom_tool_call_output") {
				writeOrphan(w, callA)
				return
			}
			writeCall(w, "resp_chain_a", callA)
		case "Bearer access-chain-b":
			if strings.Contains(body, callB) {
				writeOrphan(w, callB)
				return
			}
			if r.Header.Get("X-Codex-Turn-State") != "" || !hasHistory(raw, map[string]string{callA: "result-a"}) {
				writeOrphan(w, callA)
				return
			}
			writeCall(w, "resp_chain_b", callB)
		case "Bearer access-chain-c":
			if strings.Contains(body, callC) {
				writeOrphan(w, callC)
				return
			}
			if r.Header.Get("X-Codex-Turn-State") != "" || !hasHistory(raw, map[string]string{callA: "result-a", callB: "result-b"}) {
				writeOrphan(w, callB)
				return
			}
			writeCall(w, "resp_chain_c", callC)
		case "Bearer access-chain-d":
			if r.Header.Get("X-Codex-Turn-State") != "" || !hasHistory(raw, map[string]string{callA: "result-a", callB: "result-b", callC: "result-c"}) {
				writeOrphan(w, callC)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_chain_d","object":"response","status":"completed","model":"gpt","output_text":"all hops recovered"}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	accountA := h.importAccount(t, "chain-a", "upstream-chain-a", "access-chain-a")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}

	firstBody := `{"model":"gpt","prompt_cache_key":"rapid-account-chain","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}],"input":[{"role":"user","content":"start chain"}]}`
	firstResp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(firstBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(firstResp.Body)
	firstResp.Body.Close()
	if firstResp.StatusCode != http.StatusOK {
		t.Fatalf("initial A turn status=%d", firstResp.StatusCode)
	}
	accountB := h.importAccount(t, "chain-b", "upstream-chain-b", "access-chain-b")

	type turnResult struct {
		status        int
		contextStatus string
		body          []byte
	}
	sendOutput := func(previousID, callID, output, state string) turnResult {
		t.Helper()
		body := `{"model":"gpt","previous_response_id":"` + previousID + `","input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"` + output + `"}]}`
		req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Codex-Turn-State", state)
		resp, requestErr := http.DefaultClient.Do(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return turnResult{status: resp.StatusCode, contextStatus: resp.Header.Get("X-MiCliProxy-Context-Status"), body: responseBody}
	}
	assertRebuilt := func(hop string, result turnResult, responseID string) {
		t.Helper()
		if result.status != http.StatusOK || result.contextStatus != "rebuilt" || !strings.Contains(string(result.body), responseID) {
			t.Fatalf("%s status=%d context=%q body=%s", hop, result.status, result.contextStatus, result.body)
		}
	}

	assertRebuilt("A->B", sendOutput("resp_chain_a", callA, "result-a", "state-a"), "resp_chain_b")
	if account, getErr := h.store.GetAccount(context.Background(), accountA); getErr != nil || account.Status != "active" || account.QuarantineUntil != 0 {
		t.Fatalf("A health changed before explicit retirement: account=%+v err=%v", account, getErr)
	}
	if err := h.store.SetAccountStatus(context.Background(), accountA, "disabled"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	accountC := h.importAccount(t, "chain-c", "upstream-chain-c", "access-chain-c")
	assertRebuilt("B->C after 2s", sendOutput("resp_chain_b", callB, "result-b", "state-b"), "resp_chain_c")
	if account, getErr := h.store.GetAccount(context.Background(), accountB); getErr != nil || account.Status != "active" || account.QuarantineUntil != 0 {
		t.Fatalf("B health changed before explicit retirement: account=%+v err=%v", account, getErr)
	}
	if err := h.store.SetAccountStatus(context.Background(), accountB, "disabled"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	h.importAccount(t, "chain-d", "upstream-chain-d", "access-chain-d")
	assertRebuilt("C->D after 1s", sendOutput("resp_chain_c", callC, "result-c", "state-c"), "resp_chain_d")
	if account, getErr := h.store.GetAccount(context.Background(), accountC); getErr != nil || account.Status != "active" || account.QuarantineUntil != 0 {
		t.Fatalf("C health changed after orphan repair: account=%+v err=%v", account, getErr)
	}

	requests := h.requests()
	wantAuth := []string{
		"Bearer access-chain-a", "Bearer access-chain-a", "Bearer access-chain-b",
		"Bearer access-chain-b", "Bearer access-chain-c", "Bearer access-chain-c", "Bearer access-chain-d",
	}
	if len(requests) != len(wantAuth) {
		t.Fatalf("rapid chain attempts=%d, want %d: %+v", len(requests), len(wantAuth), requests)
	}
	for index, want := range wantAuth {
		if requests[index].Auth != want {
			t.Fatalf("rapid chain auth[%d]=%q, want %q: %+v", index, requests[index].Auth, want, requests)
		}
	}
	if got := atomic.LoadUint64(&h.app.contextRebuilt); got != 3 {
		t.Fatalf("rebuilt counter=%d, want one per hop", got)
	}
}

func TestOrphanedToolOutputStreamStatus400RebuildsFromJournal(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: error\n" +
				`data: {"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id call_2."},"status":400}` + "\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_stream_recovered","object":"response","status":"completed","model":"gpt","output_text":"ok"}`))
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")

	body := `{"model":"gpt","previous_response_id":"resp_journal","input":[{"type":"custom_tool_call_output","call_id":"call_2","output":"tool result"}],"stream":true}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	keyReq.Header.Set("X-Codex-Turn-State", "old-stream-state")
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	journalPayload := `{"model":"gpt","turn_state":"journal-old-state","tools":[{"type":"function","name":"big","parameters":{"type":"object","properties":{"n":{"const":900719925474099312345}}}}],"input":[{"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"{}"}]}`
	if err := h.store.PutContextJournal(context.Background(), storage.ContextJournal{ResponseID: "resp_journal", AccountID: accA, Payload: journalPayload, ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "old-stream-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(responseBody), "No tool call found") {
		t.Fatalf("stream recovery status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "rebuilt" {
		t.Fatalf("context status=%q, want rebuilt", got)
	}
	requests := h.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream attempts=%d, want 2: %+v", len(requests), requests)
	}
	retry := requests[1]
	if retry.Auth != "Bearer access-b" || retry.TurnState != "" || strings.Contains(retry.Body, "previous_response_id") || strings.Contains(retry.Body, "turn_state") {
		t.Fatalf("rebuilt retry leaked old state: %+v", retry)
	}
	if !strings.Contains(retry.Body, "custom_tool_call") || !strings.Contains(retry.Body, "custom_tool_call_output") {
		t.Fatalf("journal replay did not preserve paired call/output: %s", retry.Body)
	}
	if !strings.Contains(retry.Body, "900719925474099312345") {
		t.Fatalf("journal replay rounded a large schema integer: %s", retry.Body)
	}
}

func TestBoundAccountUnavailableRebuildsFromJournalBeforeReturningConflict(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if r.Header.Get("Authorization") != "Bearer access-bound-b" ||
			r.Header.Get("X-Codex-Turn-State") != "" ||
			strings.Contains(body, "previous_response_id") ||
			!strings.Contains(body, `"type":"custom_tool_call"`) ||
			!strings.Contains(body, `"type":"custom_tool_call_output"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid rebuilt request"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_bound_rebuilt","object":"response","status":"completed","model":"gpt","output_text":"ok"}`)
	})
	accountA := h.importAccount(t, "bound-a", "upstream-bound-a", "access-bound-a")
	h.importAccount(t, "bound-b", "upstream-bound-b", "access-bound-b")
	body := `{"model":"gpt","previous_response_id":"resp_bound_old","input":[{"type":"custom_tool_call_output","call_id":"call_bound","output":"kept"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountA}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutContextJournal(context.Background(), storage.ContextJournal{
		ResponseID: "resp_bound_old", AccountID: accountA,
		Payload:   `{"model":"gpt","input":[{"type":"custom_tool_call","call_id":"call_bound","name":"apply_patch","input":"{}"}]}`,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetAccountStatus(context.Background(), accountA, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "bound-a-state")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_bound_rebuilt") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "rebuilt" {
		t.Fatalf("context status=%q", got)
	}
	if requests := h.requests(); len(requests) != 1 || requests[0].Auth != "Bearer access-bound-b" {
		t.Fatalf("unavailable bound account should not receive an upstream attempt: %+v", requests)
	}
}

func TestOrphanedToolOutputSingleAccountRetriesSameAccountOnce(t *testing.T) {
	var attempts int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"No tool call found for function call output with call_id call_single."}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_single_recovered","object":"response","status":"completed","model":"gpt","output_text":"ok"}`))
	})
	accountID := h.importAccount(t, "single", "upstream-single", "access-single")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_single","output":"keep"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("single-account recovery status=%d attempts=%d body=%s", resp.StatusCode, attempts, responseBody)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-single" || requests[1].Auth != "Bearer access-single" {
		t.Fatalf("single account was not safely retried in place: %+v", requests)
	}
	if strings.Contains(requests[1].Body, "previous_response_id") || strings.Contains(requests[1].Body, "function_call_output") || !strings.Contains(requests[1].Body, "keep") {
		t.Fatalf("same-account retry was not stateless/degraded: %s", requests[1].Body)
	}
}

func TestOrphanedToolOutputRepairIsAttemptedOnlyOnce(t *testing.T) {
	const callID = "call_GQqnpD0cS3uxvXlgWBSD974z"
	var attempts int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"{\n\"type\": \"error\",\n\"error\": {\n\"type\": \"invalid_request_error\",\n\"message\": \"No tool call found for custom tool call output with call_id ` + callID + `.\",\n\"param\": \"input\"\n},\n\"status\": 400\n}"},"status":400,"type":"error"}`))
	})
	accountID := h.importAccount(t, "repeat", "upstream-repeat", "access-repeat")
	if err := h.store.SetSetting(context.Background(), "leak_scrub", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt","previous_response_id":"resp_missing","input":[{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"keep"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("repeated orphan status=%d attempts=%d body=%s", resp.StatusCode, attempts, responseBody)
	}
	for _, leak := range []string{"No tool call found", callID, "invalid_request_error"} {
		if strings.Contains(string(responseBody), leak) {
			t.Fatalf("terminal context error leaked %q with leak_scrub disabled: %s", leak, responseBody)
		}
	}
	if !strings.Contains(string(responseBody), "server_error") || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("terminal context error was not replaced with a stable JSON error: headers=%v body=%s", resp.Header, responseBody)
	}
	if got := atomic.LoadUint64(&h.app.contextDegraded); got != 1 {
		t.Fatalf("context repair count = %d, want exactly one", got)
	}
}

func TestOrphanedToolOutputAfterCommittedStreamIsNotRetried(t *testing.T) {
	var attempts int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"visible"}`+"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = io.WriteString(w, "event: error\n"+
			`data: {"type":"error","status":400,"error":{"message":"No tool call found for function call output with call_id call_late."}}`+"\n\n")
	})
	accountID := h.importAccount(t, "late", "upstream-late", "access-late")
	if err := h.store.SetSetting(context.Background(), "leak_scrub", "false"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt","previous_response_id":"resp_missing","stream":true,"input":[{"type":"function_call_output","call_id":"call_late","output":"keep"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&attempts) != 1 || !strings.Contains(string(responseBody), "visible") {
		t.Fatalf("committed stream status=%d attempts=%d body=%s", resp.StatusCode, attempts, responseBody)
	}
	if strings.Contains(string(responseBody), "No tool call found") || strings.Contains(string(responseBody), "call_late") || !strings.Contains(string(responseBody), "server_error") {
		t.Fatalf("committed stream leaked its terminal context error: %s", responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "" {
		t.Fatalf("committed stream unexpectedly repaired context: %q", got)
	}
}

func TestOrphanedToolOutputWebSocketStatusCodeRepairsContext(t *testing.T) {
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/codex/responses" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read WS request: %v", err)
		}
		if r.Header.Get("Authorization") == "Bearer access-ws-a" {
			inner := `{"error":{"type":"invalid_request_error","message":"No tool call found for tool search output with call_id search_ws."}}`
			payload, _ := json.Marshal(map[string]interface{}{
				"type": "error", "status_code": 400,
				"error": map[string]interface{}{"type": "upstream_error", "message": inner},
			})
			_ = conn.WriteMessage(websocket.TextMessage, payload)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_ws_recovered","model":"gpt-5.5"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_recovered","model":"gpt-5.5","status":"completed","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
	})
	accA := h.importAccount(t, "ws-a", "upstream-ws-a", "access-ws-a")
	h.importAccount(t, "ws-b", "upstream-ws-b", "access-ws-b")
	if err := h.store.SetSetting(context.Background(), "seamless_failover", "false"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetSetting(context.Background(), "failover_max_attempts", "1"); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.5","previous_response_id":"resp_missing","stream":true,"input":[{"type":"tool_search_output","call_id":"search_ws","execution":"client","status":"completed","tools":[]}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	key := routing.ExtractAffinityKey(keyReq, []byte(body))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp_ws_recovered") {
		t.Fatalf("WS recovery status=%d body=%s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "degraded" {
		t.Fatalf("WS context status = %q", got)
	}
	requests := h.requests()
	if len(requests) != 2 || requests[0].Auth != "Bearer access-ws-a" || requests[1].Auth != "Bearer access-ws-b" {
		t.Fatalf("WS recovery did not prefer another account: %+v", requests)
	}
}

func TestNoAccountResponseIsGeneric(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	lower := strings.ToLower(string(body))
	for _, forbidden := range []string{"no available account", "group=", "candidate", "pool", "cyber"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("no-account response leaked internal detail %q: %s", forbidden, body)
		}
	}
}

// A strict-sticky but self-contained turn (function_call_output, no
// previous_response_id) MUST fail over on a 429 — the fix for "429 leaks downstream,
// no auto-switch". The downstream sees a single 200 served by account B.
func TestMovableStrictTurnFailsOverOn429(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer access-b":
			bCalled = true
			_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
		case "Bearer access-a":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit."}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
		}
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	// Strict (function_call_output) but movable (no previous_response_id). Pin to A.
	reqBody := `{"model":"gpt","prompt_cache_key":"conv-mov","input":[{"type":"function_call_output","call_id":"c1","output":"done"},{"role":"user","content":"more"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: acc}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !bCalled {
		t.Fatalf("movable strict turn must fail over to account B on 429")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200 (seamless failover)", resp.StatusCode)
	}
}

func TestCodexStreamingEarlyFailedFrameRetriesBeforeWrite(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = w.Write([]byte("event: response.created\n" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n" +
				"event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"You've hit your usage limit.\"}}}\n\n"))
		case "Bearer access-b":
			bCalled = true
			_, _ = w.Write([]byte("event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok-from-b\"}\n\n" +
				"event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
				"data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	reqBody := `{"model":"gpt","stream":true,"prompt_cache_key":"conv-stream","input":[{"role":"user","content":"hi"}]}`
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
	if !bCalled {
		t.Fatalf("early response.failed stream was not retried on account B; body=%s", body)
	}
	if !strings.Contains(string(body), "ok-from-b") || strings.Contains(string(body), "response.failed") {
		t.Fatalf("downstream saw wrong stream after retry: %s", body)
	}
}

func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return string(raw)
}

// The native Anthropic /v1/messages path must also fail over on a recoverable error
// (these requests are always self-contained) instead of leaking the 429.
func TestClaudeMessagesFailsOverOn429(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer sk-ant-oat-b":
			bCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg","content":[{"type":"text","text":"ok"}]}`))
		case "Bearer sk-ant-oat-a":
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit"}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg","content":[{"type":"text","text":"ok"}]}`))
		}
	})
	accA := h.importAccount(t, "claude-a", "", "sk-ant-oat-a")
	h.importAccount(t, "claude-b", "", "sk-ant-oat-b")
	reqBody := `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`
	// Pin to A so A is tried first and forced to fail over to B.
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !bCalled {
		t.Fatalf("claude /v1/messages must fail over to account B on 429")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200 (seamless failover)", resp.StatusCode)
	}
}

func TestClaudeMessagesStreamingEarlyErrorRetriesBeforeWrite(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer sk-ant-oat-a":
			_, _ = w.Write([]byte("event: error\n" +
				"data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This account has hit its usage limit.\"}}\n\n"))
		case "Bearer sk-ant-oat-b":
			bCalled = true
			_, _ = w.Write([]byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"event: content_block_start\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok-from-b\"}}\n\n" +
				"event: message_delta\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "claude-a", "", "sk-ant-oat-a")
	h.importAccount(t, "claude-b", "", "sk-ant-oat-b")
	reqBody := `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bCalled {
		t.Fatalf("early Claude stream error was not retried on account B; body=%s", body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok-from-b") || strings.Contains(string(body), "rate_limit_error") || strings.Contains(string(body), "msg_a") {
		t.Fatalf("downstream saw wrong stream after Claude retry: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestClaudeMessagesStreamingContentThenErrorDoesNotRetryAfterCommit(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer sk-ant-oat-a":
			_, _ = w.Write([]byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"role\":\"assistant\"}}\n\n" +
				"event: content_block_start\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial-from-a\"}}\n\n" +
				"event: error\n" +
				"data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This account has hit its usage limit.\"}}\n\n"))
		case "Bearer sk-ant-oat-b":
			bCalled = true
			_, _ = w.Write([]byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"event: content_block_start\n" +
				"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok-from-b\"}}\n\n" +
				"event: message_delta\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "claude-a", "", "sk-ant-oat-a")
	h.importAccount(t, "claude-b", "", "sk-ant-oat-b")
	reqBody := `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bCalled {
		t.Fatalf("Claude stream retried after content was already committed; body=%s", body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "partial-from-a") || !strings.Contains(string(body), "rate_limit_error") || strings.Contains(string(body), "ok-from-b") {
		t.Fatalf("downstream did not see the committed account A stream: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestClaudeMessagesStreamingIgnoresCaptureLimitAfterCommit(t *testing.T) {
	tempDir := t.TempDir()
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer sk-ant-oat-a":
			_, _ = w.Write([]byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"role\":\"assistant\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + strings.Repeat("partial-from-a", 64) + "\"}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n"))
		case "Bearer sk-ant-oat-b":
			bCalled = true
			_, _ = w.Write([]byte("event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok-from-b\"}}\n\n" +
				"event: message_delta\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	h.app.cfg.StreamFailoverHoldMemoryBytes = 700
	h.app.cfg.StreamFailoverHoldDiskBytes = 0
	h.app.cfg.StreamFailoverHoldTempDir = tempDir
	accA := h.importAccount(t, "claude-a", "", "sk-ant-oat-a")
	h.importAccount(t, "claude-b", "", "sk-ant-oat-b")
	reqBody := `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bCalled {
		t.Fatalf("Claude stream retried because of capture limit after content was committed; body=%s", body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "partial-from-a") || strings.Contains(string(body), "ok-from-b") {
		t.Fatalf("downstream did not stream oversized account A response: status=%d body=%s", resp.StatusCode, body)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("disk spill disabled but temp dir has %d files", len(entries))
	}
}

func TestCodexStreamingContentThenFailedRetriesWithoutLeakingPartial(t *testing.T) {
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			_, _ = w.Write([]byte("event: response.created\n" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\",\"model\":\"gpt\"}}\n\n" +
				"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial-from-a\"}\n\n" +
				"event: response.failed\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"You've hit your usage limit.\"}}}\n\n"))
		case "Bearer access-b":
			bCalled = true
			_, _ = w.Write([]byte("event: response.created\n" +
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_b\",\"model\":\"gpt\"}}\n\n" +
				"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok-from-b\"}\n\n" +
				"event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_b\",\"model\":\"gpt\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
				"data: [DONE]\n\n"))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	reqBody := `{"model":"gpt","stream":true,"prompt_cache_key":"conv-content-retry","input":[{"role":"user","content":"hi"}]}`
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
	if !bCalled {
		t.Fatalf("Codex stream content-then-failed was not retried on account B; body=%s", body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "ok-from-b") || strings.Contains(string(body), "partial-from-a") || strings.Contains(string(body), "response.failed") || strings.Contains(string(body), "resp_a") {
		t.Fatalf("downstream saw account A partial/error after Codex retry: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestCodexStreamingStatefulTurnRebuildsFromJournal(t *testing.T) {
	var aCalls int
	var bCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer access-a":
			aCalls++
			if aCalls == 1 {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("event: response.created\n" +
					"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a_stream\",\"model\":\"gpt\"}}\n\n" +
					"event: response.output_text.delta\n" +
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"stream answer\"}\n\n" +
					"event: response.completed\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_a_stream\",\"model\":\"gpt\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n" +
					"data: [DONE]\n\n"))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit."}}`))
		case "Bearer access-b":
			bCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_b_stream_rebuild","object":"response","status":"completed","model":"gpt","output_text":"ok"}`))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")
	firstBody := `{"model":"gpt","stream":true,"prompt_cache_key":"conv-stream-ledger","input":[{"role":"user","content":"hello"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(firstBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(firstBody))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initial streaming response status=%d", resp.StatusCode)
	}

	secondBody := `{"model":"gpt","previous_response_id":"resp_a_stream","prompt_cache_key":"conv-stream-ledger","input":[{"role":"user","content":"next"}]}`
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(secondBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "account-a-stream-state")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream-derived stateful turn should rebuild on account B, status=%d body=%s", resp.StatusCode, body)
	}
	if !bCalled || !strings.Contains(string(body), "resp_b_stream_rebuild") {
		t.Fatalf("stream-derived stateful turn was not rebuilt on account B: called=%v body=%s", bCalled, body)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); got != "rebuilt" {
		t.Fatalf("context status header=%q, want rebuilt", got)
	}
	var secondA capturedRequest
	for _, req := range h.requests() {
		if req.Auth == "Bearer access-a" && strings.Contains(req.Body, "next") {
			secondA = req
			break
		}
	}
	if secondA.Body == "" {
		t.Fatalf("missing second account A request")
	}
	for _, want := range []string{"previous_response_id", "resp_a_stream"} {
		if !strings.Contains(secondA.Body, want) {
			t.Fatalf("stateful streamed request lost upstream state %q: body=%s", want, secondA.Body)
		}
	}
	if secondA.TurnState != "account-a-stream-state" {
		t.Fatalf("turn state header should be preserved for the same upstream account, got %q", secondA.TurnState)
	}
}

func TestHealthTestDetectsBanDeletesAndAuditsWhenEnabled(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"account_deactivated","message":"Your account was deactivated for a policy violation"}}`))
	})
	h.app.cfg.BanAutoDelete = true
	acc := h.importAccount(t, "dead", "upstream-dead", "access-dead")
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res["alive"] != false || res["state"] != "banned" {
		t.Fatalf("expected banned/dead verdict, got %v", res)
	}
	if res["deleted"] != true {
		t.Fatalf("expected auto-delete on confirmed ban, got %v", res["deleted"])
	}
	if _, err := h.store.GetAccount(context.Background(), acc); !storage.NotFound(err) {
		t.Fatalf("banned account should be deleted, err=%v", err)
	}
	audit, _ := h.store.ListAuditLog(context.Background(), 10)
	if len(audit) == 0 || audit[0].Action != "ban_delete" || audit[0].State != "banned" {
		t.Fatalf("ban_delete audit record not written: %+v", audit)
	}
}

func TestHealthTestAliveDoesNotDelete(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"pong"}`))
	})
	acc := h.importAccount(t, "ok", "upstream-ok", "access-ok")
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res["alive"] != true || res["state"] != "alive" {
		t.Fatalf("expected alive, got %v", res)
	}
	if _, err := h.store.GetAccount(context.Background(), acc); err != nil {
		t.Fatalf("healthy account must not be deleted: %v", err)
	}
}

func TestClaudeCountTokensUsesFinalUpstreamRequestWithoutUsageRecord(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages/count_tokens" {
			t.Errorf("upstream path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":321}`))
	})
	account := storage.Account{ID: "claude-count", Label: "claude", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "sk-ant-api-test"}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-sonnet-4-6","system":"count final body","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Pool-Provider", "claude")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"input_tokens":321`)) {
		t.Fatalf("count_tokens status=%d body=%s", response.StatusCode, body)
	}
	if response.Header.Get("X-Pool-Resolved-Provider") != "claude" || response.Header.Get("X-Pool-Resolved-Model") != "claude-sonnet-4.6" {
		t.Fatalf("resolved headers=%v", response.Header)
	}
	requests := h.requests()
	if len(requests) != 1 || !strings.Contains(requests[0].Body, "count final body") || requests[0].Body == `{"model":"claude-sonnet-4-6","system":"count final body","messages":[{"role":"user","content":"hello"}]}` {
		t.Fatalf("count_tokens did not use final transformed upstream body: %+v", requests)
	}
	var usageRows int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM usage_records WHERE account_id=?`, account.ID).Scan(&usageRows); err != nil || usageRows != 0 {
		t.Fatalf("count_tokens usage rows=%d err=%v", usageRows, err)
	}
}

func TestHealthTestPermissionDeniedDoesNotQuarantine(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"You have insufficient permissions for this operation. Missing scopes: api.responses.write."}}`))
	})
	acc := h.importAccount(t, "health-scope", "upstream-health-scope", "access-health-scope")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if res["state"] != "permission_denied" {
		t.Fatalf("expected permission_denied health verdict, got %v", res)
	}
	gotAcc, err := h.store.GetAccount(context.Background(), acc)
	if err != nil {
		t.Fatal(err)
	}
	if gotAcc.QuarantineUntil != 0 || gotAcc.QuarantineReason != "" {
		t.Fatalf("health PermissionDenied must not quarantine account: %+v", gotAcc)
	}
	audit, err := h.store.ListAuditLog(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) == 0 {
		t.Fatal("expected a health PermissionDenied audit record")
	}
	for _, row := range audit {
		if row.Action == "auth_quarantine" {
			t.Fatalf("health PermissionDenied used deprecated auth_quarantine audit: %+v", audit)
		}
	}
	if audit[0].Action != "permission_denied_no_quarantine" || audit[0].State != string(ban.PermissionDenied) {
		t.Fatalf("expected non-quarantine PermissionDenied audit, got %+v", audit)
	}
}

// TestHealthTestCodexProbeSendsInstructions locks in the request-shape fixes for the
// synthetic Codex /responses liveness probe against the streaming-only WHAM backend:
//
//   - non-empty top-level "instructions" on the synthetic classic probe;
//   - "store":false — the chatgpt.com WHAM backend rejects store:true;
//   - "stream":true — THE REAL CODEX CLIENT ALWAYS STREAMS (buildCodexWebSocketCreatePayload
//     hard-sets it; the Session-9 capture shows every real /responses call is an SSE/WS
//     stream). A non-streaming (stream:false) probe makes the backend evaluate the request
//     in stored-response mode, which conflicts with the mandatory store:false and is
//     rejected with 400 {"detail":"Store must be set to false"} EVEN WHEN store:false is
//     present — i.e. stream:false, not the store value, was the real trigger of that 400.
//
// The live relay forwards the client's own (always-streaming) request, so this only bites
// the synthetic probe body, which must therefore set all three itself. Unlike a
// current Codex client turn, this classic-shaped probe does not opt into Lite.
func TestHealthTestCodexProbeSendsInstructions(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"pong"}`))
	})
	acc := h.importAccount(t, "ok", "upstream-ok", "access-ok")
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var probe *capturedRequest
	for i := range *h.captured {
		if c := &(*h.captured)[i]; strings.Contains(c.Path, "responses") && strings.Contains(c.Body, `"ping"`) {
			probe = c
		}
	}
	if probe == nil {
		t.Fatalf("no Codex /responses probe captured among %d requests", len(*h.captured))
	}
	var body map[string]interface{}
	if err := json.Unmarshal([]byte(probe.Body), &body); err != nil {
		t.Fatalf("probe body is not valid JSON: %v (%s)", err, probe.Body)
	}
	if instr, _ := body["instructions"].(string); strings.TrimSpace(instr) == "" {
		t.Fatalf("Codex probe missing non-empty instructions; body=%s", probe.Body)
	}
	if store, ok := body["store"].(bool); !ok || store {
		t.Fatalf("Codex probe must send store:false for the chatgpt.com WHAM backend; body=%s", probe.Body)
	}
	if stream, _ := body["stream"].(bool); !stream {
		t.Fatalf("Codex probe must stream (stream:true): the streaming-only WHAM backend rejects a non-streaming probe with 400 {\"detail\":\"Store must be set to false\"} even when store:false is present; body=%s", probe.Body)
	}
}

func TestRateLimitGivesNoBanDelete(t *testing.T) {
	// A 429 / quota response must cool the account, never delete it.
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You've hit your usage limit."}}`))
	})
	acc := h.importAccount(t, "limited", "upstream-l", "access-l")
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		binding, _ := h.store.GetEgressBinding(context.Background(), acc)
		if binding.CooldownUntil > storage.Now() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("rate-limited account should be on cooldown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("queued inference should end only after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release queued request")
	}
	if _, err := h.store.GetAccount(context.Background(), acc); err != nil {
		t.Fatalf("rate-limited account must not be deleted: %v", err)
	}
	binding, _ := h.store.GetEgressBinding(context.Background(), acc)
	if binding.CooldownUntil <= storage.Now() {
		t.Fatalf("rate-limited account should be on cooldown")
	}
	// Retry-After=120 should drive a ~120s cooldown (honored over the 1800s quota guess).
	if d := binding.CooldownUntil - storage.Now(); d > 200 {
		t.Fatalf("cooldown should honor Retry-After (~120s), got %ds", d)
	}
}

func TestIsolationToggleAndSessionMap(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	body := `{"model":"gpt","input":"hi","prompt_cache_key":"pck-iso"}`

	// Isolation is on by default → the forwarded prompt_cache_key is namespaced.
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	reqs := h.requests()
	if len(reqs) == 0 || strings.Contains(reqs[len(reqs)-1].Body, "pck-iso") {
		t.Fatalf("prompt_cache_key not namespaced under isolation: %+v", reqs)
	}

	// The session-map endpoint surfaces the pinned conversation with its per-account
	// original→namespaced mapping (the UI visualization data).
	smResp, err := http.Get(h.pool.URL + "/admin/accounts/" + acc + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	var sm map[string]interface{}
	_ = json.NewDecoder(smResp.Body).Decode(&sm)
	smResp.Body.Close()
	if sm["isolation_enabled"] != true {
		t.Fatalf("isolation should report on: %v", sm["isolation_enabled"])
	}
	sessions, _ := sm["sessions"].([]interface{})
	if len(sessions) == 0 {
		t.Fatalf("no pinned sessions recorded: %v", sm)
	}
	s0 := sessions[0].(map[string]interface{})
	if s0["original"] != "pck-iso" || !strings.HasPrefix(s0["namespaced"].(string), "cp_") {
		t.Fatalf("session mapping wrong: %v", s0)
	}

	// Toggle isolation OFF from the settings endpoint; it must persist and then the
	// forwarded key is verbatim (max cross-account cache sharing, no isolation).
	patch, _ := http.NewRequest(http.MethodPatch, h.pool.URL+"/admin/settings", strings.NewReader(`{"conversation_isolation":false}`))
	patch.Header.Set("Content-Type", "application/json")
	pr, err := http.DefaultClient.Do(patch)
	if err != nil {
		t.Fatal(err)
	}
	var eff map[string]interface{}
	_ = json.NewDecoder(pr.Body).Decode(&eff)
	pr.Body.Close()
	if eff["conversation_isolation"] != false {
		t.Fatalf("toggle did not persist: %v", eff)
	}
	resp2, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	last := h.requests()
	if !strings.Contains(last[len(last)-1].Body, "pck-iso") {
		t.Fatalf("isolation off should forward the cache key verbatim: %s", last[len(last)-1].Body)
	}
}

func TestClaudeMessagesRelayVirtualizesAndScrubs(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			t.Fatalf("claude path = %s", r.URL.Path)
		}
		// Anthropic sits behind Cloudflare: a normal 200 still carries these.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("server", "cloudflare")
		w.Header().Set("cf-ray", "anth-ray-1")
		// Echo the real telemetry user_id (must be scrubbed from the stream) AND a
		// real path (must be PRESERVED — rewriting paths breaks tool use, per policy).
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi real-user from /Users/realbob"}],"model":"claude-x","usage":{"input_tokens":5,"output_tokens":3,"cache_read_input_tokens":2}}`))
	})
	h.importAccount(t, "claude-a", "", "sk-ant-oat-test")

	reqBody := `{"model":"claude-x","metadata":{"user_id":"real-user"},"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"<env>\nWorking directory: /Users/realbob/proj\nPlatform: win32\n</env>"}],"tools":[{"name":"bash"}],"messages":[{"role":"user","content":"hello"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, got)
	}
	// Response scrubbing: the real telemetry user_id is replaced everywhere it
	// appears in the stream.
	if strings.Contains(string(got), "real-user") {
		t.Fatalf("user_id not scrubbed from response: %s", got)
	}
	// ...but real paths are deliberately preserved so the model's tool calls (run
	// on the downstream's real filesystem) keep working.
	if !strings.Contains(string(got), "/Users/realbob") {
		t.Fatalf("paths must NOT be rewritten in response (breaks tool use): %s", got)
	}

	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	up := reqs[0]
	if up.Auth != "Bearer sk-ant-oat-test" {
		t.Fatalf("claude oauth auth header = %q", up.Auth)
	}
	h.app.WaitForAsyncWrites()
	var usageProvider, claudeTTL string
	var cacheControlInjected, breakpointCount, latestUserCacheControl int
	if err := h.store.DB().QueryRow(`SELECT usage_provider, claude_cache_ttl, cache_control_injected, cache_breakpoint_count, latest_user_cache_control FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(
		&usageProvider, &claudeTTL, &cacheControlInjected, &breakpointCount, &latestUserCacheControl,
	); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if usageProvider != "claude" || claudeTTL != "1h" || cacheControlInjected != 1 || breakpointCount == 0 || latestUserCacheControl != 0 {
		t.Fatalf("claude cache diagnostics wrong: provider=%q ttl=%q injected=%d breakpoints=%d latest_user=%d", usageProvider, claudeTTL, cacheControlInjected, breakpointCount, latestUserCacheControl)
	}
	if !strings.Contains(up.Body, "Anthropic's Claude Agent SDK") {
		t.Fatalf("claude code system block not injected: %s", up.Body)
	}
	if strings.Contains(up.Body, "real-user") {
		t.Fatalf("user_id not virtualized: %s", up.Body)
	}
	var claudeBody map[string]interface{}
	if err := json.Unmarshal([]byte(up.Body), &claudeBody); err != nil {
		t.Fatalf("invalid Claude upstream body: %v", err)
	}
	metadata, _ := claudeBody["metadata"].(map[string]interface{})
	userID, _ := metadata["user_id"].(string)
	var userIdentity map[string]interface{}
	if len(metadata) != 1 || json.Unmarshal([]byte(userID), &userIdentity) != nil {
		t.Fatalf("Claude 2.1.206 metadata must contain only JSON-string user_id: %s", up.Body)
	}
	deviceID, _ := userIdentity["device_id"].(string)
	if len(deviceID) != 64 || userIdentity["account_uuid"] != "" || userIdentity["session_id"] != up.ClaudeSession || up.ClaudeSession == "" {
		t.Fatalf("Claude metadata/header identity drift: metadata=%+v session=%q", userIdentity, up.ClaudeSession)
	}
	// Paths are preserved in the request too (policy: never rewrite cwd/paths).
	if !strings.Contains(up.Body, "/Users/realbob") {
		t.Fatalf("paths must be preserved in request: %s", up.Body)
	}
	if !strings.Contains(up.Body, `"Bash"`) {
		t.Fatalf("tool name not normalized: %s", up.Body)
	}
}

// TestAnthropicPassthroughFilesAndSkills verifies the skills/code-execution
// passthrough: the extra Anthropic surfaces (Files API, skills, agents, ...) are
// transparently proxied -- route reachable (not 404), method + path + opaque body
// forwarded verbatim, the client's own Content-Type (multipart boundary) and
// Anthropic-Beta preserved, account auth attached, and the upstream body returned
// to the client UNscrubbed (it is file content / a resource definition, not a turn).
func TestAnthropicPassthroughFilesAndSkills(t *testing.T) {
	var mu sync.Mutex
	type seen struct {
		path, method, contentType, beta, auth string
	}
	var calls []seen
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, seen{
			path:        r.URL.Path,
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			beta:        r.Header.Get("Anthropic-Beta"),
			auth:        r.Header.Get("Authorization"),
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file_abc123","type":"file","filename":"data.csv"}`))
	})
	h.importAccount(t, "claude-skill", "", "sk-ant-oat-skill")

	// 1) A multipart Files upload (POST /v1/files). The opaque multipart body and its
	//    boundary Content-Type must survive verbatim; the client's beta must be kept.
	multipart := "------b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"data.csv\"\r\n\r\na,b,c\r\n------b--\r\n"
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/files", strings.NewReader(multipart))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=----b")
	req.Header.Set("Anthropic-Beta", "files-api-2025-04-14")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("files upload status=%d body=%s", resp.StatusCode, got)
	}
	if !strings.Contains(string(got), "file_abc123") {
		t.Fatalf("passthrough did not return the upstream file body verbatim: %s", got)
	}

	// 2) A GET on a skills sub-path (method + trailing subtree route both work).
	req2, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/skills/skill_42", nil)
	req2.Header.Set("Anthropic-Beta", "skills-2025-10-02")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("skills GET status=%d", resp2.StatusCode)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("upstream calls = %d, want 2: %+v", len(calls), calls)
	}
	up := calls[0]
	if up.path != "/v1/files" || up.method != http.MethodPost {
		t.Fatalf("upload reached upstream as %s %s, want POST /v1/files", up.method, up.path)
	}
	if up.contentType != "multipart/form-data; boundary=----b" {
		t.Fatalf("multipart Content-Type not preserved: %q", up.contentType)
	}
	if up.beta != "files-api-2025-04-14" {
		t.Fatalf("Anthropic-Beta not forwarded verbatim: %q", up.beta)
	}
	if up.auth != "Bearer sk-ant-oat-skill" {
		t.Fatalf("account auth not attached: %q", up.auth)
	}
	if calls[1].path != "/v1/skills/skill_42" || calls[1].method != http.MethodGet {
		t.Fatalf("skills GET reached upstream as %s %s", calls[1].method, calls[1].path)
	}
}

func TestChatCompletionsRoutesToClaudeByModel(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			t.Fatalf("claude-model chat should hit /v1/messages, got %s", r.URL.Path)
		}
		upstreamBody = readBody(t, r)
		if !strings.Contains(upstreamBody, `"max_tokens"`) {
			t.Fatalf("OpenAI->Anthropic conversion missing max_tokens: %s", upstreamBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	})
	h.importAccount(t, "claude-b", "", "sk-ant-oat-test2")

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "chat.completion" {
		t.Fatalf("object = %#v", root["object"])
	}
	choice := root["choices"].([]interface{})[0].(map[string]interface{})
	if choice["message"].(map[string]interface{})["content"] != "pong" {
		t.Fatalf("content = %#v", choice["message"])
	}
	if !strings.Contains(upstreamBody, `"ttl":"1h"`) {
		t.Fatalf("default claude cache ttl not applied upstream: %s", upstreamBody)
	}
	h.app.WaitForAsyncWrites()
	var usageProvider, claudeTTL string
	var breakpointCount, latestUserCacheControl int
	if err := h.store.DB().QueryRow(`SELECT usage_provider, claude_cache_ttl, cache_breakpoint_count, latest_user_cache_control FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(
		&usageProvider, &claudeTTL, &breakpointCount, &latestUserCacheControl,
	); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if usageProvider != "claude" || claudeTTL != "1h" || breakpointCount == 0 || latestUserCacheControl != 0 {
		t.Fatalf("compat claude diagnostics wrong: provider=%q ttl=%q breakpoints=%d latest_user=%d", usageProvider, claudeTTL, breakpointCount, latestUserCacheControl)
	}
}

func TestChatCompletionsClaudeMaxHitWritesLatestTail(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_max_hit","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1,"cache_creation_input_tokens":20}}`))
	})
	h.importAccount(t, "claude-max-hit", "", "sk-ant-oat-max-hit")
	if err := h.store.SetSetting(context.Background(), "claude_cache_mode", "max_hit"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"claude-x","messages":[{"role":"system","content":"stable instructions"},{"role":"user","content":"first question"},{"role":"assistant","content":"first answer"},{"role":"user","content":"latest tail should be written"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var sent map[string]interface{}
	if err := json.Unmarshal([]byte(upstreamBody), &sent); err != nil {
		t.Fatalf("decode upstream body: %v\n%s", err, upstreamBody)
	}
	msgs := sent["messages"].([]interface{})
	latest := msgs[len(msgs)-1].(map[string]interface{})
	latestBlocks, ok := latest["content"].([]interface{})
	if !ok {
		t.Fatalf("max_hit should convert latest tail into a marked text block, got %T in %s", latest["content"], upstreamBody)
	}
	if _, has := latestBlocks[len(latestBlocks)-1].(map[string]interface{})["cache_control"]; !has {
		t.Fatalf("max_hit should write latest tail cache_control: %s", upstreamBody)
	}
	if got := cacheControlCountPrompt(t, []byte(upstreamBody)); got > 4 {
		t.Fatalf("max_hit exceeded 4 breakpoints: %d in %s", got, upstreamBody)
	}
	h.app.WaitForAsyncWrites()
	var latestUserCacheControl, latestUserTailCacheControl int
	if err := h.store.DB().QueryRow(`SELECT latest_user_cache_control, latest_user_tail_cache_control FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(&latestUserCacheControl, &latestUserTailCacheControl); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if latestUserCacheControl != 1 || latestUserTailCacheControl != 1 {
		t.Fatalf("max_hit diagnostics latest_user=%d latest_tail=%d", latestUserCacheControl, latestUserTailCacheControl)
	}
}

func TestChatCompletionsClaudeSyncPrewarmBeforeRealRequest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw := readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(raw, `"max_tokens":0`) {
			_, _ = w.Write([]byte(`{"id":"msg_prewarm","model":"claude-x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":100}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_real","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":80}}`))
	})
	h.importAccount(t, "claude-prewarm", "", "sk-ant-oat-prewarm")
	if err := h.store.SetSetting(context.Background(), "claude_cache_prewarm_mode", "sync_extreme"); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"system","content":"stable instructions"},{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("prewarm should send warmup + real request, got %d requests: %+v", len(reqs), reqs)
	}
	if !strings.Contains(reqs[0].Body, `"max_tokens":0`) {
		t.Fatalf("first request should be max_tokens=0 prewarm: %s", reqs[0].Body)
	}
	if strings.Contains(reqs[1].Body, `"max_tokens":0`) {
		t.Fatalf("real request should keep generation max_tokens: %s", reqs[1].Body)
	}
	h.app.WaitForAsyncWrites()
	var cacheHitAfterPrewarm int
	if err := h.store.DB().QueryRow(`SELECT cache_hit_after_prewarm FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(&cacheHitAfterPrewarm); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if cacheHitAfterPrewarm != 1 {
		t.Fatalf("cache_hit_after_prewarm = %d, want 1", cacheHitAfterPrewarm)
	}
}

func TestChatCompletionsClaudeSingleflightSerializesSamePrefix(t *testing.T) {
	var mu sync.Mutex
	current := 0
	maxConcurrent := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		current++
		if current > maxConcurrent {
			maxConcurrent = current
		}
		mu.Unlock()
		time.Sleep(120 * time.Millisecond)
		mu.Lock()
		current--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_sf","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":20}}`))
	})
	h.importAccount(t, "claude-singleflight", "", "sk-ant-oat-singleflight")
	if err := h.store.SetSetting(context.Background(), "claude_cache_singleflight_enabled", "true"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"claude-x","messages":[{"role":"system","content":"stable instructions"},{"role":"user","content":"same request"}]}`
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status = %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	gotMax := maxConcurrent
	mu.Unlock()
	if gotMax > 1 {
		reqs := h.requests()
		prefixes := make([]string, 0, len(reqs))
		for _, req := range reqs {
			prefixes = append(prefixes, routing.AnthropicStablePromptPrefixHash([]byte(req.Body)))
		}
		t.Fatalf("singleflight allowed %d concurrent same-prefix upstream requests; upstream prefix hashes=%q", gotMax, prefixes)
	}
	h.app.WaitForAsyncWrites()
	var waited int
	if err := h.store.DB().QueryRow(`SELECT COALESCE(SUM(singleflight_waited_requests),0) FROM usage_records`).Scan(&waited); err != nil {
		t.Fatal(err)
	}
	if waited != 1 {
		t.Fatalf("singleflight_waited_requests sum = %d, want 1", waited)
	}
}

func TestChatCompletionsClaudeCacheDiagnosticsSendsBetaAndPreviousMessage(t *testing.T) {
	var n int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_diag_` + strconv.Itoa(n) + `","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	})
	h.importAccount(t, "claude-diagnostics", "", "sk-ant-oat-diagnostics")
	if err := h.store.SetSetting(context.Background(), "claude_cache_diagnostics_enabled", "true"); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"claude-x","messages":[{"role":"system","content":"stable instructions"},{"role":"user","content":"same route"}]}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}
	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	for _, req := range reqs {
		if !strings.Contains(req.Beta, "cache-diagnosis-2026-04-07") {
			t.Fatalf("diagnostics beta missing from Anthropic-Beta %q", req.Beta)
		}
	}
	if strings.Contains(reqs[0].Body, "previous_message_id") {
		t.Fatalf("first request should not have previous message diagnostics: %s", reqs[0].Body)
	}
	if !strings.Contains(reqs[1].Body, `"previous_message_id":"msg_diag_1"`) {
		t.Fatalf("second request missing previous_message_id: %s", reqs[1].Body)
	}
}

func TestChatCompletionsClaudeUsesConvertedAnthropicStablePrefixAffinity(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_stable","model":"claude-opus-4-8","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":1,"cache_read_input_tokens":20}}`))
	})
	h.importAccount(t, "claude-stable-a", "", "sk-ant-oat-stable-a")
	h.importAccount(t, "claude-stable-b", "", "sk-ant-oat-stable-b")
	systemPrefix := strings.Repeat("stable converted Anthropic system prefix. ", 120)

	for _, question := range []string{"summarize file A", "summarize file B"} {
		body := `{"model":"claude-opus-4-8","messages":[{"role":"system","content":` + strconv.Quote(systemPrefix) + `},{"role":"user","content":` + strconv.Quote(question) + `}]}`
		resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
	}

	reqs := h.requests()
	if len(reqs) != 2 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	if reqs[0].AccountID != reqs[1].AccountID {
		t.Fatalf("same converted stable prefix should stay on one account, got %s then %s", reqs[0].AccountID, reqs[1].AccountID)
	}
	h.app.WaitForAsyncWrites()
	var affinitySource string
	if err := h.store.DB().QueryRow(`SELECT affinity_source FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(&affinitySource); err != nil {
		t.Fatal(err)
	}
	if affinitySource != "cache_prefix_hash" {
		t.Fatalf("affinity_source = %q, want cache_prefix_hash", affinitySource)
	}
}

func TestClaudeCacheTTLEmptySettingDowngradesToStandard(t *testing.T) {
	var upstreamBody string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody = readBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_2","model":"claude-x","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	})
	h.importAccount(t, "claude-standard-cache", "", "sk-ant-oat-standard-cache")
	if err := h.store.SetSetting(context.Background(), "claude_cache_ttl", ""); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if strings.Contains(upstreamBody, `"ttl":"1h"`) {
		t.Fatalf("empty claude_cache_ttl setting should not send 1h marker: %s", upstreamBody)
	}
	h.app.WaitForAsyncWrites()
	var claudeTTL string
	if err := h.store.DB().QueryRow(`SELECT claude_cache_ttl FROM usage_records ORDER BY id DESC LIMIT 1`).Scan(&claudeTTL); err != nil {
		t.Fatalf("usage diagnostics missing: %v", err)
	}
	if claudeTTL != "" {
		t.Fatalf("empty claude_cache_ttl setting should record standard ttl, got %q", claudeTTL)
	}
}

// TestHealthTestClaudeUsesCloakedCountTokens locks in the 429 fix: a Claude OAuth
// liveness probe must be a CLOAKED count_tokens call (carrying the "You are Claude
// Code" identity block Anthropic silently requires of OAuth traffic), not a bare
// /v1/messages generation ping. The bare ping was rejected (400→429 under repeats)
// and consumed generation quota; count_tokens is free and high-limit, so a healthy
// account returns a clean 200/alive.
func TestHealthTestClaudeUsesCloakedCountTokens(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "count_tokens"):
			_, _ = w.Write([]byte(`{"input_tokens":7}`))
		case r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-7"}]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	acc := h.importAccount(t, "claude-live", "", "sk-ant-oat-live")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	if res["alive"] != true || res["state"] != "alive" {
		t.Fatalf("expected alive via count_tokens, got %v", res)
	}
	if res["provider"] != "claude" {
		t.Fatalf("provider = %v", res["provider"])
	}

	var probe *capturedRequest
	reqs := h.requests()
	for i := range reqs {
		if strings.Contains(reqs[i].Path, "count_tokens") {
			probe = &reqs[i]
			break
		}
	}
	if probe == nil {
		t.Fatalf("liveness probe never hit count_tokens; requests=%+v", reqs)
	}
	if probe.Method != http.MethodPost {
		t.Fatalf("count_tokens method = %s", probe.Method)
	}
	if probe.Auth != "Bearer sk-ant-oat-live" {
		t.Fatalf("count_tokens auth = %q", probe.Auth)
	}
	// OAuth liveness MUST carry the Claude Code identity block, else Anthropic 400/429.
	if !strings.Contains(probe.Body, "Anthropic's Claude Agent SDK") {
		t.Fatalf("count_tokens body missing cloak identity block: %s", probe.Body)
	}
	// count_tokens counts input only — it must not request generation tokens.
	if strings.Contains(probe.Body, "max_tokens") {
		t.Fatalf("count_tokens probe must not send max_tokens: %s", probe.Body)
	}
}

// TestProbeClaudeModelsViaV1Models confirms a Claude account's capabilities are
// populated from a live GET /v1/models probe, with the context window filled from
// the known-model table (Anthropic's /v1/models omits it).
func TestProbeClaudeModelsViaV1Models(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-4-7"},{"id":"claude-sonnet-4-6"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "claude-models", "", "sk-ant-oat-models")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	windows := map[string]int64{}
	for _, c := range res.Capabilities {
		windows[c.ModelSlug] = c.NativeMaxContextWindow
	}
	if windows["claude-opus-4-7"] != 1000000 {
		t.Fatalf("expected opus-4-7 window 1000000, got %d (caps=%+v)", windows["claude-opus-4-7"], res.Capabilities)
	}
	if windows["claude-sonnet-4-6"] != 200000 {
		t.Fatalf("expected sonnet-4-6 window 200000, got %d (caps=%+v)", windows["claude-sonnet-4-6"], res.Capabilities)
	}
	// The static floor unions the current flagship in even though this probe (like a
	// lagging Anthropic /v1/models) did not return it — with its 1M context window.
	if windows["claude-opus-4-8"] != 1000000 {
		t.Fatalf("expected opus-4-8 floored in at window 1000000, got %d (caps=%+v)", windows["claude-opus-4-8"], res.Capabilities)
	}
}

// TestProbeClaudeModelsFallsBackToStatic confirms that when /v1/models is rejected
// (an OAuth token without models-listing access — the likely 4xx cause), the probe
// falls back to the static current-generation set rather than failing, and never
// harms the account. This is what stops "模型能力获取失败" from surfacing.
func TestProbeClaudeModelsFallsBackToStatic(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"type":"forbidden","message":"not allowed"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "claude-fb", "", "sk-ant-oat-fb")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("probe-models status %d: %s", resp.StatusCode, body)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if len(res.Capabilities) == 0 {
		t.Fatalf("expected static fallback capabilities on /v1/models 403, got none")
	}
	// A capability probe failure must never delete or quarantine the account.
	if _, err := h.store.GetAccount(context.Background(), acc); err != nil {
		t.Fatalf("account must survive a model-probe failure: %v", err)
	}
}

// TestProbeCodexModelsSendsCurrentClientVersion locks in the Codex fix: the model
// probe must report a CURRENT client_version on the ChatGPT /models query, because
// that backend version-gates the catalog (gpt-5.5 needs 0.124.0). A stale 0.118.0 is
// exactly why the newest models never came back. Also asserts the returned model is
// stored.
func TestProbeCodexModelsSendsCurrentClientVersion(t *testing.T) {
	var mu sync.Mutex
	var gotClientVersion string
	var gotVersionHeader string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			mu.Lock()
			gotClientVersion = r.URL.Query().Get("client_version")
			gotVersionHeader = r.Header.Get("version")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","context_window":272000,"max_context_window":272000,"visibility":"list"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "codex-models", "acct-codex", "codex-access-token")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	mu.Lock()
	cv := gotClientVersion
	versionHeader := gotVersionHeader
	mu.Unlock()
	if cv != config.DefaultClientVersion {
		t.Fatalf("probe client_version = %q, want current %q", cv, config.DefaultClientVersion)
	}
	if cv == "0.118.0" {
		t.Fatalf("probe still reports the stale 0.118.0 that version-gates away new models")
	}
	if versionHeader != config.DefaultClientVersion {
		t.Fatalf("probe version header = %q, want current %q", versionHeader, config.DefaultClientVersion)
	}
	var hasFlagship bool
	for _, c := range res.Capabilities {
		if c.ModelSlug == "gpt-5.5" {
			hasFlagship = true
		}
	}
	if !hasFlagship {
		t.Fatalf("expected gpt-5.5 from probe, got %+v", res.Capabilities)
	}
}

func TestProbeCodexModelsFloorsNewerStaticFlagship(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.4","context_window":272000,"max_context_window":1000000,"visibility":"list"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "codex-floor", "acct-floor", "codex-floor-token")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()

	got := map[string]bool{}
	for _, c := range res.Capabilities {
		got[c.ModelSlug] = true
	}
	if !got["gpt-5.4"] || !got["gpt-5.5"] {
		t.Fatalf("expected live gpt-5.4 plus static newer gpt-5.5, got %+v", res.Capabilities)
	}
}

func TestProbeCodexModelsRefreshesExpiredToken(t *testing.T) {
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse refresh form: %v", err)
		}
		if scope := r.Form.Get("scope"); !strings.Contains(scope, "api.connectors.invoke") {
			t.Fatalf("refresh scope = %q, want Codex connector scope", scope)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer oauth.Close()

	var mu sync.Mutex
	var modelAuths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			auth := r.Header.Get("Authorization")
			mu.Lock()
			modelAuths = append(modelAuths, auth)
			mu.Unlock()
			if auth == "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"token expired"}`))
				return
			}
			if auth != "Bearer new-access" {
				t.Fatalf("model probe auth = %q", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","context_window":272000,"max_context_window":272000,"visibility":"list"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	h.pool.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = h.upstream.URL + "/backend-api/codex"
	cfg.OAuthTokenURL = oauth.URL
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

	account := storage.Account{ID: "acc-probe-expired", Label: "expired", GroupName: config.DefaultGroupName, UpstreamAccountID: "acct-probe", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "old-access", RefreshToken: "refresh-old"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+account.ID+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe status = %d", resp.StatusCode)
	}
	var hasFlagship bool
	for _, c := range res.Capabilities {
		if c.ModelSlug == "gpt-5.5" {
			hasFlagship = true
		}
	}
	if !hasFlagship {
		t.Fatalf("expected refreshed probe to store gpt-5.5, got %+v", res.Capabilities)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(modelAuths) < 2 || modelAuths[0] != "Bearer old-access" || modelAuths[1] != "Bearer new-access" {
		t.Fatalf("expected old token then refreshed token for model probe, saw %v", modelAuths)
	}
}

// TestProbeCodexModelsFallsBackToStatic confirms a failed ChatGPT /models probe
// falls back to the curated current-gen catalog instead of erroring out (which would
// leave the account with no models), and never harms the account.
func TestProbeCodexModelsFallsBackToStatic(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "codex-fb", "acct-fb", "codex-access-token-2")

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("probe-models status %d: %s", resp.StatusCode, body)
	}
	var res struct {
		Capabilities []storage.ModelCapability `json:"capabilities"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	var hasFlagship bool
	for _, c := range res.Capabilities {
		if c.ModelSlug == "gpt-5.5" {
			hasFlagship = true
		}
	}
	if !hasFlagship {
		t.Fatalf("expected static gpt-5.5 fallback on /models 401, got %+v", res.Capabilities)
	}
	if _, err := h.store.GetAccount(context.Background(), acc); err != nil {
		t.Fatalf("account must survive a model-probe failure: %v", err)
	}
}

// TestProbeReplacesStaleCapabilities locks in the eviction fix: a re-probe must drop
// models that no longer come back (a hidden preset now filtered, or access lost),
// instead of leaving stale rows that keep being advertised. First probe returns a
// hidden codex-auto-review (which is filtered at parse) plus gpt-5.4; the model set
// must never contain codex-auto-review, and a second probe returning only gpt-5.5
// must fully replace the earlier set.
func TestProbeReplacesStaleCapabilities(t *testing.T) {
	var mu sync.Mutex
	phase := 1
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			mu.Lock()
			p := phase
			mu.Unlock()
			if p == 1 {
				_, _ = w.Write([]byte(`{"models":[{"slug":"codex-auto-review","max_context_window":1000000,"visibility":"hide"},{"slug":"gpt-5.4","max_context_window":1000000,"visibility":"list"}]}`))
			} else {
				_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","max_context_window":272000,"visibility":"list"}]}`))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	acc := h.importAccount(t, "codex-stale", "acct-stale", "codex-access-stale")

	probe := func() map[string]bool {
		resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		var res struct {
			Capabilities []storage.ModelCapability `json:"capabilities"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		// Read back what is actually stored (what the UI/​/v1/models will show).
		caps, _ := h.store.ListCapabilities(context.Background(), acc)
		got := map[string]bool{}
		for _, c := range caps {
			got[c.ModelSlug] = true
		}
		return got
	}

	got := probe()
	if got["codex-auto-review"] {
		t.Fatalf("hidden codex-auto-review must never be stored: %v", got)
	}
	if !got["gpt-5.4"] {
		t.Fatalf("first probe should store gpt-5.4: %v", got)
	}

	mu.Lock()
	phase = 2
	mu.Unlock()
	got = probe()
	if got["gpt-5.4"] || got["codex-auto-review"] {
		t.Fatalf("re-probe must evict stale models, got %v", got)
	}
	if !got["gpt-5.5"] {
		t.Fatalf("re-probe should store the new gpt-5.5: %v", got)
	}
}

func TestResponsesPreviousResponseBindingPersistsWithoutPromptKey(t *testing.T) {
	var auths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		raw, _ := io.ReadAll(r.Body)
		id := "resp_state_root"
		if bytes.Contains(raw, []byte("previous_response_id")) {
			id = "resp_state_next"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"object":"response","model":"gpt","status":"completed","output_text":"ok","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, id)
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	h.importAccount(t, "b", "upstream-b", "access-b")

	first, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":[{"role":"user","content":"start"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || !bytes.Contains(firstBody, []byte("resp_state_root")) || len(auths) != 1 {
		t.Fatalf("first response status=%d auths=%v body=%s", first.StatusCode, auths, firstBody)
	}
	stateKey := routing.ResponseAffinityKey("resp_state_root")
	bound, err := h.store.GetAffinityBinding(context.Background(), stateKey.Hash)
	if err != nil || bound.Provider != "codex" || bound.Model != "gpt" || bound.EgressID == "" {
		t.Fatalf("response state binding=%+v err=%v", bound, err)
	}

	second, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","previous_response_id":"resp_state_root","input":[{"role":"user","content":"next"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || len(auths) != 2 || auths[1] != auths[0] || !bytes.Contains(secondBody, []byte("resp_state_next")) {
		t.Fatalf("stateful continuation switched account: status=%d auths=%v body=%s", second.StatusCode, auths, secondBody)
	}

	callsBeforeMissing := len(auths)
	missing, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","previous_response_id":"resp_unknown","input":"next"}`))
	if err != nil {
		t.Fatal(err)
	}
	missingBody, _ := io.ReadAll(missing.Body)
	missing.Body.Close()
	if missing.StatusCode != http.StatusOK || len(auths) != callsBeforeMissing+1 || missing.Header.Get("X-MiCliProxy-Context-Status") != "degraded" {
		t.Fatalf("missing state binding status=%d calls=%d body=%s", missing.StatusCode, len(auths), missingBody)
	}
}
