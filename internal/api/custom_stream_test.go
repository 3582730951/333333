package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
)

// custom_stream_test.go is the end-to-end guard for the custom OpenAI-compatible
// provider path (DeepSeek and friends), exercised through the REAL relay handlers
// (handleChatViaCustom / handleResponsesViaCustom / handleMessagesViaCustom →
// callCustom → upstream) against a mock Chat-Completions upstream. It covers all
// three downstream protocols — /v1/chat/completions (passthrough), /v1/responses
// (Codex ⇄ chat) and /v1/messages (Claude Code ⇄ chat) — in both non-streaming and
// streaming form, plus model auto-discovery and the local count_tokens estimate.

// A non-streaming Chat Completions response the mock upstream returns.
const dsChatResp = `{"id":"chatcmpl-ds","object":"chat.completion","model":"deepseek-chat",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7,"prompt_cache_hit_tokens":3,"prompt_cache_miss_tokens":2}}`

// A streamed Chat Completions response (the shape DeepSeek emits with
// stream_options.include_usage): content deltas then a terminal usage-bearing chunk.
const dsChatSSE = "data: {\"id\":\"c1\",\"model\":\"deepseek-chat\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hel\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_cache_hit_tokens\":3,\"prompt_cache_miss_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// deepseekMock is the upstream stand-in: it answers /models for discovery and
// /chat/completions for live traffic, choosing the streamed body when the forwarded
// request carries stream:true (as the relay sets via withStreamUsage).
func deepseekMock(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			body := readBody(t, r)
			if strings.Contains(body, `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, dsChatSSE)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(dsChatResp))
		default:
			t.Fatalf("unexpected custom-provider upstream path %s", r.URL.Path)
		}
	}
}

// setupDeepSeek points the seeded "deepseek" provider at the mock upstream and
// imports an API-key account for it, returning the account id. autoDiscover and the
// manual model list mirror what an operator would configure in the admin UI.
func setupDeepSeek(t *testing.T, h *testHarness, models []string, autoDiscover bool) string {
	t.Helper()
	prov := map[string]interface{}{
		"id": "deepseek", "name": "DeepSeek", "base_url": h.upstream.URL,
		"enabled": true, "auto_discover_models": autoDiscover, "models": models,
	}
	raw, _ := json.Marshal(prov)
	resp, err := http.Post(h.pool.URL+"/admin/providers", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert provider status = %d", resp.StatusCode)
	}
	imp := map[string]interface{}{"provider_id": "deepseek", "api_key": "sk-deepseek-test", "label": "ds"}
	iraw, _ := json.Marshal(imp)
	resp2, err := http.Post(h.pool.URL+"/admin/accounts/import-key", "application/json", bytes.NewReader(iraw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp2.Body)
		t.Fatalf("import-key status = %d: %s", resp2.StatusCode, b)
	}
	var acc storage.Account
	if err := json.NewDecoder(resp2.Body).Decode(&acc); err != nil {
		t.Fatal(err)
	}
	if acc.Provider != "deepseek" {
		t.Fatalf("imported account provider = %q, want deepseek", acc.Provider)
	}
	return acc.ID
}

func TestCustomProviderChatCompletionsPassthrough(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	acc := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "chat.completion" {
		t.Fatalf("chat passthrough object = %#v", root["object"])
	}
	choice := root["choices"].([]interface{})[0].(map[string]interface{})
	if choice["message"].(map[string]interface{})["content"] != "hello world" {
		t.Fatalf("content not relayed: %#v", choice["message"])
	}
	// The upstream is the custom provider's /chat/completions with Bearer key auth.
	var call *capturedRequest
	for i := range *h.captured {
		if c := &(*h.captured)[i]; strings.HasSuffix(c.Path, "/chat/completions") {
			call = c
		}
	}
	if call == nil {
		t.Fatalf("no /chat/completions upstream call captured: %+v", h.requests())
	}
	if call.Auth != "Bearer sk-deepseek-test" {
		t.Fatalf("custom provider auth = %q, want the imported API key", call.Auth)
	}
	h.app.WaitForAsyncWrites()
	var cached int64
	row := h.store.DB().QueryRow(`SELECT cached_tokens FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, acc)
	if err := row.Scan(&cached); err != nil {
		t.Fatalf("no usage recorded for custom passthrough response: %v", err)
	}
	if cached != 3 {
		t.Fatalf("custom passthrough cached_tokens = %d, want 3", cached)
	}
}

func TestCustomProviderRuleFailoverRetriesDifferentAccount(t *testing.T) {
	var secondCalled bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		switch r.Header.Get("Authorization") {
		case "Bearer sk-deepseek-test":
			w.WriteHeader(http.StatusTeapot)
			_, _ = w.Write([]byte(`{"error":{"message":"vendor temporary block"}}`))
		case "Bearer sk-deepseek-b":
			secondCalled = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(dsChatResp))
		default:
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
	})
	accA := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)
	if err := h.store.UpsertAccount(context.Background(), storage.Account{ID: "ds-b", Label: "ds-b", GroupName: "cyber", Provider: "deepseek", Status: "active"}, storage.AccountToken{OpenAIAPIKey: "sk-deepseek-b"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(context.Background(), []storage.ModelCapability{{AccountID: "ds-b", ModelSlug: "deepseek-chat", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertUpstreamErrorRule(context.Background(), storage.UpstreamErrorRule{ID: "custom-failover", Name: "custom failover", Enabled: true, Priority: 1, Providers: []string{"openai_compatible"}, Entrypoints: []string{"chat_completions"}, ModelPatterns: []string{"deepseek-chat"}, StatusCodes: []int{http.StatusTeapot}, AccountAction: "none", DownstreamAction: "failover"}); err != nil {
		t.Fatal(err)
	}
	reqBody := `{"model":"deepseek-chat","messages":[{"role":"user","content":"hi"}]}`
	keyReq, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	key := routing.ExtractAffinityKey(keyReq, []byte(reqBody))
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accA}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !secondCalled {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("custom rule failover failed: status=%d secondCalled=%v body=%s", resp.StatusCode, secondCalled, body)
	}
}

func TestCustomProviderResponsesConversion(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	// A Codex /v1/responses request for a DeepSeek model: Responses → chat upstream,
	// chat response → Responses response.
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get(responsesCompatibilityLossesHeader); got != "none" {
		t.Fatalf("lossless Responses bridge header = %q", got)
	}
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "response" || root["status"] != "completed" {
		t.Fatalf("responses envelope wrong: %#v", root)
	}
	out := root["output"].([]interface{})
	msg := out[0].(map[string]interface{})
	if msg["type"] != "message" {
		t.Fatalf("first output item not a message: %#v", msg)
	}
	if txt := msg["content"].([]interface{})[0].(map[string]interface{})["text"]; txt != "hello world" {
		t.Fatalf("converted text wrong: %#v", txt)
	}
	// The upstream actually received the converted Chat Completions shape.
	var call *capturedRequest
	for i := range *h.captured {
		if c := &(*h.captured)[i]; strings.HasSuffix(c.Path, "/chat/completions") {
			call = c
		}
	}
	if call == nil || !strings.Contains(call.Body, `"messages"`) {
		t.Fatalf("Responses request not converted to chat messages: %+v", call)
	}
}

func TestCustomProviderNativeResponsesPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/responses"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-native","object":"response","model":"native-resp-model","status":"completed","output_text":"native ok","native_passthrough":true}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			t.Fatalf("native Responses provider must not be called through /chat/completions")
		default:
			t.Fatalf("unexpected native provider upstream path %s", r.URL.Path)
		}
	})
	prov := map[string]interface{}{
		"id": "native-resp", "name": "Native Responses", "base_url": h.upstream.URL,
		"upstream_protocol": "responses",
		"enabled":           true, "auto_discover_models": false, "models": []string{"native-resp-model"},
	}
	raw, _ := json.Marshal(prov)
	resp, err := http.Post(h.pool.URL+"/admin/providers", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upsert native provider status = %d", resp.StatusCode)
	}
	imp := map[string]interface{}{"provider_id": "native-resp", "api_key": "sk-native-resp", "label": "native"}
	iraw, _ := json.Marshal(imp)
	resp2, err := http.Post(h.pool.URL+"/admin/accounts/import-key", "application/json", bytes.NewReader(iraw))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("import-key status = %d", resp2.StatusCode)
	}

	resp3, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"native-resp-model","input":[{"role":"user","content":"search"}],"tools":[{"type":"function","name":"f","parameters":{"const":900719925474099312345}},{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"create"}]},{"type":"custom","name":"freeform","format":{"type":"text"}},{"type":"tool_search","execution":"client"},{"type":"web_search_preview"},{"type":"future_hosted_tool","future":{"x":1}}],"include":["web_search_call.results"],"unknown_future_field":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp3.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "response" || root["native_passthrough"] != true {
		t.Fatalf("native Responses body was not transparently returned: %#v", root)
	}
	if got := resp3.Header.Get(responsesCompatibilityLossesHeader); got != "" {
		t.Fatalf("native Responses path must not report Chat bridge losses: %q", got)
	}

	var responsesCall *capturedRequest
	for i := range *h.captured {
		c := &(*h.captured)[i]
		if strings.HasSuffix(c.Path, "/responses") {
			responsesCall = c
		}
		if strings.HasSuffix(c.Path, "/chat/completions") {
			t.Fatalf("unexpected chat conversion call for native Responses provider: %+v", c)
		}
	}
	if responsesCall == nil {
		t.Fatalf("no native /responses upstream call captured: %+v", h.requests())
	}
	for _, want := range []string{`"namespace"`, `"custom"`, `"tool_search"`, `"web_search_preview"`, `"future_hosted_tool"`, `"unknown_future_field"`, `"include"`, "900719925474099312345"} {
		if !strings.Contains(responsesCall.Body, want) {
			t.Fatalf("native Responses request lost %s: %s", want, responsesCall.Body)
		}
	}
	if responsesCall.Auth != "Bearer sk-native-resp" {
		t.Fatalf("native provider auth = %q, want imported API key", responsesCall.Auth)
	}
}

func TestCustomProviderMessagesConversion(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	// A Claude Code /v1/messages request for a DeepSeek model: Anthropic → chat
	// upstream, chat response → Anthropic response.
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if root["type"] != "message" || root["role"] != "assistant" || root["stop_reason"] != "end_turn" {
		t.Fatalf("anthropic envelope wrong: %#v", root)
	}
	if txt := root["content"].([]interface{})[0].(map[string]interface{})["text"]; txt != "hello world" {
		t.Fatalf("converted text wrong: %#v", txt)
	}
}

func TestCustomProviderResponsesStreaming(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	acc := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","stream":true,"include":["reasoning.encrypted_content"],"tools":[{"type":"web_search_preview"}],"input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(got)
	// The chat-completions SSE was rewritten into the Responses API event sequence.
	for _, want := range []string{"response.created", "response.output_text.delta", `"delta":"hel"`, "response.completed"} {
		if !strings.Contains(s, want) {
			t.Fatalf("responses SSE missing %q:\n%s", want, s)
		}
	}
	wantLosses := `["responses_hosted_tool_omitted","responses_include_omitted"]`
	if got := resp.Trailer.Get(responsesCompatibilityLossesHeader); got != wantLosses {
		t.Fatalf("compatibility trailer = %q, want %q; headers=%v trailers=%v", got, wantLosses, resp.Header, resp.Trailer)
	}
	// Streamed usage is recorded (the relay set stream_options.include_usage and the
	// terminal chunk carried usage) so the admin overview reflects custom traffic.
	h.app.WaitForAsyncWrites() // usage rows are written asynchronously; drain before asserting
	var prompt, completion, total, cached int64
	var compatibility string
	row := h.store.DB().QueryRow(`SELECT prompt_tokens, completion_tokens, total_tokens, cached_tokens, compatibility_losses_json FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, acc)
	if err := row.Scan(&prompt, &completion, &total, &cached, &compatibility); err != nil {
		t.Fatalf("no usage recorded for streamed custom response: %v", err)
	}
	if prompt != 5 || completion != 2 || total != 7 || cached != 3 {
		t.Fatalf("streamed custom usage mis-recorded: prompt=%d completion=%d total=%d cached=%d", prompt, completion, total, cached)
	}
	if compatibility != wantLosses {
		t.Fatalf("recorded compatibility losses = %q, want %q", compatibility, wantLosses)
	}
}

func TestChatStreamToResponsesSSEUsesBridgePlanForStableTools(t *testing.T) {
	plan := prompt.NewResponsesToolBridgePlan()
	customAlias := plan.EnsureChatName(prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolCustom, Name: "apply_patch"})
	searchAlias := plan.EnsureChatName(prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolSearch, Name: "tool_search", Execution: "client"})
	namespaceAlias := plan.EnsureChatName(prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolFunction, Namespace: "filesystem", Name: "read"})
	stream := "data: " + `{"id":"stable-tools","choices":[{"delta":{"tool_calls":[` +
		`{"index":0,"id":"call_custom","function":{"name":"` + customAlias + `","arguments":"{\"input\":\"hel"}},` +
		`{"index":1,"id":"call_search","function":{"name":"` + searchAlias + `","arguments":"{\"limit\":900719925474099312345}"}},` +
		`{"index":2,"id":"call_read","function":{"name":"` + namespaceAlias + `","arguments":"{\"path\":\"a\"}"}}` +
		`]}}]}` + "\n\n" +
		"data: " + `{"id":"stable-tools","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lo\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"

	recorder := httptest.NewRecorder()
	chatStreamToResponsesSSE(recorder, strings.NewReader(stream), "chat-model", streamrewrite.New(nil), plan)
	got := recorder.Body.String()
	for _, want := range []string{
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		`"type":"custom_tool_call"`,
		`"name":"apply_patch"`,
		`"input":"hello"`,
		`"type":"tool_search_call"`,
		`"execution":"client"`,
		"900719925474099312345",
		`"namespace":"filesystem"`,
		`"name":"read"`,
		"response.completed",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("stable tool SSE missing %q:\n%s", want, got)
		}
	}
}

func TestChatStreamToResponsesSSEPreservesLegacyFunctionCall(t *testing.T) {
	plan := prompt.NewResponsesToolBridgePlan()
	alias := plan.EnsureChatName(prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolCustom, Name: "apply_patch"})
	stream := "data: " + `{"id":"legacy-stream","choices":[{"delta":{"function_call":{"name":"` + alias + `","arguments":""}}}]}` + "\n\n" +
		"data: " + `{"id":"legacy-stream","choices":[{"delta":{"function_call":{"arguments":"{\"input\":\"*** Begin Patch\"}"}},"finish_reason":"function_call"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	chatStreamToResponsesSSE(recorder, strings.NewReader(stream), "chat-model", streamrewrite.New(nil), plan)
	got := recorder.Body.String()
	for _, want := range []string{`"type":"custom_tool_call"`, `"name":"apply_patch"`, `"input":"*** Begin Patch"`, "response.completed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("legacy streaming function call missing %q:\n%s", want, got)
		}
	}
}

func TestChatStreamToAnthropicSSESerializesInterleavedTools(t *testing.T) {
	stream := "data: " + `{"id":"parallel-tools","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"Bash","arguments":"{\"cmd\":\"ec"}}]}}]}` + "\n\n" +
		"data: " + `{"id":"parallel-tools","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"Write","arguments":"{\"path\":\"a"}}]}}]}` + "\n\n" +
		"data: " + `{"id":"parallel-tools","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ho\"}"}},{"index":1,"function":{"arguments":".txt\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	chatStreamToAnthropicSSE(recorder, strings.NewReader(stream), "relay-model", streamrewrite.New(nil))
	got := recorder.Body.String()
	for _, want := range []string{`"name":"Bash"`, `"partial_json":"{\"cmd\":\"echo\"}"`, `"name":"Write"`, `"partial_json":"{\"path\":\"a.txt\"}"`, `"stop_reason":"tool_use"`, "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("serialized tool stream missing %q:\n%s", want, got)
		}
	}
	if strings.Count(got, "event: content_block_start") != 2 || strings.Count(got, "event: content_block_stop") != 2 {
		t.Fatalf("interleaved tools produced invalid block lifecycle:\n%s", got)
	}
	firstStart := strings.Index(got, `"name":"Bash"`)
	firstStop := strings.Index(got, `event: content_block_stop`)
	secondStart := strings.Index(got, `"name":"Write"`)
	if firstStart < 0 || firstStop < firstStart || secondStart < firstStop {
		t.Fatalf("tool blocks were not serialized contiguously:\n%s", got)
	}
}

func TestChatStreamToResponsesSSEPreservesTerminalChatError(t *testing.T) {
	stream := "data: " + `{"id":"chat-err","choices":[{"delta":{"role":"assistant","content":"partial"}}]}` + "\n\n" +
		"data: " + `{"error":{"type":"api_error","message":"Kiro stream failed"}}` + "\n\n"
	recorder := httptest.NewRecorder()
	chatStreamToResponsesSSE(recorder, strings.NewReader(stream), "gpt-5.6-sol", streamrewrite.New(nil))
	got := recorder.Body.String()
	if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
		t.Fatalf("Responses stream lost its public terminal:\n%s", got)
	}
	if strings.Contains(got, "Kiro stream failed") {
		t.Fatalf("Responses stream leaked the upstream error:\n%s", got)
	}
	if strings.Contains(got, "response.completed") {
		t.Fatalf("Responses stream completed after a terminal chat error:\n%s", got)
	}
}

func TestCustomProviderMessagesStreaming(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	acc := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	s := string(got)
	// The chat-completions SSE was rewritten into the Anthropic Messages event sequence.
	for _, want := range []string{"message_start", "content_block_delta", `"text":"hel"`, `"cache_read_input_tokens":3`, "message_stop"} {
		if !strings.Contains(s, want) {
			t.Fatalf("anthropic SSE missing %q:\n%s", want, s)
		}
	}
	h.app.WaitForAsyncWrites()
	var cached int64
	row := h.store.DB().QueryRow(`SELECT cached_tokens FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, acc)
	if err := row.Scan(&cached); err != nil {
		t.Fatalf("no usage recorded for streamed custom messages response: %v", err)
	}
	if cached != 3 {
		t.Fatalf("streamed custom messages cached_tokens = %d, want 3", cached)
	}
}

func TestCustomProviderModelAutoDiscovery(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	acc := setupDeepSeek(t, h, nil, true) // empty manual list, auto-discover on

	// Probe discovers the model list from {base}/models and unions it into the provider.
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+acc+"/probe-models", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("probe-models status = %d", resp.StatusCode)
	}

	// Discovered ids are persisted back into the provider record (so routing + the
	// admin model-list inputs reflect them).
	provs, err := http.Get(h.pool.URL + "/admin/providers")
	if err != nil {
		t.Fatal(err)
	}
	var list []storage.CustomProvider
	_ = json.NewDecoder(provs.Body).Decode(&list)
	provs.Body.Close()
	var ds *storage.CustomProvider
	for i := range list {
		if list[i].ID == "deepseek" {
			ds = &list[i]
		}
	}
	if ds == nil {
		t.Fatalf("deepseek provider missing from registry: %+v", list)
	}
	gotModels := map[string]bool{}
	for _, m := range ds.Models {
		gotModels[m] = true
	}
	if !gotModels["deepseek-chat"] || !gotModels["deepseek-reasoner"] {
		t.Fatalf("auto-discovery did not persist the discovered models: %+v", ds.Models)
	}

	// ...and they are advertised on /v1/models so a client can pick them.
	mResp, err := http.Get(h.pool.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	mRaw, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	for _, want := range []string{"deepseek-chat", "deepseek-reasoner"} {
		if !strings.Contains(string(mRaw), want) {
			t.Fatalf("/v1/models missing discovered model %q:\n%s", want, mRaw)
		}
	}
}

func TestCustomProviderCountTokensLocalEstimate(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	// count_tokens has no Chat Completions equivalent, so a custom-model count is
	// answered locally with an estimate and never proxied upstream.
	resp, err := http.Post(h.pool.URL+"/v1/messages/count_tokens", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"count these tokens please"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if n, _ := root["input_tokens"].(float64); n <= 0 {
		t.Fatalf("count_tokens estimate should be positive: %#v", root["input_tokens"])
	}
	for _, c := range h.requests() {
		if strings.HasSuffix(c.Path, "/chat/completions") {
			t.Fatalf("count_tokens must not proxy upstream, saw %s", c.Path)
		}
	}
}

func TestWithStreamUsagePreservesLargeSchemaIntegers(t *testing.T) {
	body := []byte(`{"stream":true,"tools":[{"type":"function","function":{"name":"huge","parameters":{"type":"integer","const":900719925474099312345}}}]}`)
	converted := withStreamUsage(body)
	if !strings.Contains(string(converted), "900719925474099312345") {
		t.Fatalf("large integer was rounded while adding stream usage: %s", converted)
	}
	if !strings.Contains(string(converted), `"include_usage":true`) {
		t.Fatalf("stream usage was not enabled: %s", converted)
	}
}
