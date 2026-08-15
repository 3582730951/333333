package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/capability"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

const autoKiroGPTModel = "gpt-5.6-sol"

type autoKiroGPTMock struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  int
	bodies [][]byte
}

func newAutoKiroGPTMock(t *testing.T) *autoKiroGPTMock {
	t.Helper()
	mock := &autoKiroGPTMock{}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generateAssistantResponse":
			if r.Method != http.MethodPost {
				t.Errorf("Kiro generation method=%q", r.Method)
			}
			if got := r.Header.Get("X-Amz-Target"); got != "AmazonCodeWhispererStreamingService.GenerateAssistantResponse" {
				t.Errorf("Kiro generation x-amz-target=%q", got)
			}
			body, _ := io.ReadAll(r.Body)
			mock.mu.Lock()
			mock.calls++
			mock.bodies = append(mock.bodies, append([]byte(nil), body...))
			mock.mu.Unlock()
			if !bytes.Contains(body, []byte(`"modelId":"`+autoKiroGPTModel+`"`)) {
				t.Errorf("Kiro GPT request lost model id: %s", body)
			}
			if !bytes.Contains(body, []byte(`"origin":"KIRO_CLI"`)) || bytes.Contains(body, []byte(`"origin":"AI_EDITOR"`)) {
				t.Errorf("Kiro GPT request origin does not match CLI: %s", body)
			}
			for _, forbidden := range []string{`"thinking"`, `"output_config"`, `"max_tokens"`} {
				if bytes.Contains(body, []byte(forbidden)) {
					t.Errorf("Kiro GPT request sent Claude-only %s: %s", forbidden, body)
				}
			}
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			_, _ = w.Write(kiroEventFrame(map[string]string{
				":message-type": "event", ":event-type": "assistantResponseEvent",
			}, []byte(`{"modelId":"gpt-5.6-sol","content":"kiro GPT answer"}`)))
			_, _ = w.Write(kiroEventFrame(map[string]string{
				":message-type": "event", ":event-type": "meteringEvent",
			}, []byte(`{"inputTokens":7,"outputTokens":3}`)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(mock.server.Close)
	return mock
}

func newAutoKiroGPTRejectThenSucceedMock(t *testing.T) *autoKiroGPTMock {
	t.Helper()
	mock := &autoKiroGPTMock{}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generateAssistantResponse" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mock.mu.Lock()
		mock.calls++
		call := mock.calls
		mock.bodies = append(mock.bodies, append([]byte(nil), body...))
		mock.mu.Unlock()
		if !bytes.Contains(body, []byte(`"modelId":"`+autoKiroGPTModel+`"`)) {
			t.Errorf("Kiro GPT request lost model id: %s", body)
		}
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(kiroEventFrame(map[string]string{
			":message-type": "event", ":event-type": "assistantResponseEvent",
		}, []byte(`{"modelId":"gpt-5.6-sol","content":"kiro answer after codex compaction"}`)))
		_, _ = w.Write(kiroEventFrame(map[string]string{
			":message-type": "event", ":event-type": "meteringEvent",
		}, []byte(`{"inputTokens":11,"outputTokens":5}`)))
	}))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *autoKiroGPTMock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *autoKiroGPTMock) bodiesSnapshot() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.bodies))
	for i := range m.bodies {
		out[i] = append([]byte(nil), m.bodies[i]...)
	}
	return out
}

func seedAutoKiroGPTAccount(t *testing.T, h *testHarness, id, endpoint string) {
	t.Helper()
	ctx := context.Background()
	account := storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "kiro", PlanType: "KIRO PRO", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "kiro-token-" + id}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, capability.StaticKiroModels(id)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertKiroCredentials(ctx, storage.KiroCredentials{
		AccountID: id, AuthMethod: "api_key", KiroAPIKey: "kiro-key-" + id,
		APIRegion: "us-east-1", Endpoint: endpoint,
	}); err != nil {
		t.Fatal(err)
	}
	endpointHash, err := kirowire.EndpointHash(endpoint, "us-east-1", []string{endpoint})
	if err != nil {
		t.Fatal(err)
	}
	models := capability.StaticKiroModels(id)
	modelIDs := make([]string, 0, len(models))
	for _, model := range models {
		modelIDs = append(modelIDs, model.ModelSlug)
	}
	if err := h.store.EnsureKiroRuntimeModels(ctx, id, endpointHash, modelIDs); err != nil {
		t.Fatal(err)
	}
	allowKiroTestEndpoint(t, h, endpoint)
	h.app.scheduler.InvalidateAccountCache()
}

func seedAutoCodexGPTAccount(t *testing.T, h *testHarness, label string) string {
	t.Helper()
	id := h.importAccount(t, label, "upstream-"+label, "access-"+label)
	setTestCapability(t, h, id, autoKiroGPTModel, 272000)
	h.app.scheduler.InvalidateAccountCache()
	return id
}

func postAutoKiroGPT(t *testing.T, h *testHarness, path, body string, headers http.Header) (int, http.Header, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), resp.Trailer.Clone(), string(raw)
}

func TestAutoKiroGPTLowCapacityBridgesChatAndResponses(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("Codex must not be called when Kiro is the only fair-pool candidate")
	})
	kiroMock := newAutoKiroGPTMock(t)
	seedAutoKiroGPTAccount(t, h, "kiro-gpt-only", kiroMock.server.URL)

	cases := []struct {
		name string
		path string
		body string
		want []string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			body: `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}]}`,
			want: []string{`"object":"chat.completion"`, `"content":"kiro GPT answer"`},
		},
		{
			name: "responses", path: "/v1/responses",
			body: `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`,
			want: []string{`"object":"response"`, `"output_text":"kiro GPT answer"`},
		},
		{
			name: "responses stream", path: "/v1/responses",
			body: `{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":"hello"}]}`,
			want: []string{"response.created", "response.output_text.delta", "response.completed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, header, _, got := postAutoKiroGPT(t, h, tc.path, tc.body, nil)
			if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" {
				t.Fatalf("status=%d provider=%q body=%s", status, header.Get("X-Pool-Resolved-Provider"), got)
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("response missing %q: %s", want, got)
				}
			}
		})
	}
	if kiroMock.callCount() != len(cases) {
		t.Fatalf("Kiro calls=%d, want %d", kiroMock.callCount(), len(cases))
	}
}

func TestAutoKiroGPTFallsBackUnsupportedCodexModelToSol(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("Codex must not be called when the fallback Kiro model is the only fair-pool candidate")
	})
	kiroMock := newAutoKiroGPTMock(t)
	seedAutoKiroGPTAccount(t, h, "kiro-gpt-fallback", kiroMock.server.URL)

	status, header, _, body := postAutoKiroGPT(t, h, "/v1/responses", `{"model":"gpt-5.4","input":[{"role":"user","content":"fallback please"}]}`, nil)
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" ||
		header.Get("X-Pool-Resolved-Model") != autoKiroGPTFallbackModel ||
		header.Get("X-Pool-Kiro-Fallback-From") != "gpt-5.4" ||
		!strings.Contains(body, "kiro GPT answer") {
		t.Fatalf("fallback status=%d provider=%q model=%q from=%q body=%s", status, header.Get("X-Pool-Resolved-Provider"), header.Get("X-Pool-Resolved-Model"), header.Get("X-Pool-Kiro-Fallback-From"), body)
	}
	if kiroMock.callCount() != 1 {
		t.Fatalf("Kiro calls=%d, want 1", kiroMock.callCount())
	}
}

func TestAutoKiroGPTLowCapacityUsesFairPoolRatherThanKiroPriority(t *testing.T) {
	var codexCalls int
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		codexCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_codex","object":"response","model":"gpt-5.6-sol","status":"completed","output_text":"codex answer"}`))
	})
	kiroMock := newAutoKiroGPTMock(t)
	seedAutoCodexGPTAccount(t, h, "fair-codex")
	seedAutoKiroGPTAccount(t, h, "fair-kiro", kiroMock.server.URL)

	// Available Codex capacity is one and pressure is zero: under the policy Kiro
	// joins the candidate set, but neither provider is a priority override. Repeated
	// independent conversations must therefore exercise both fair-pool members.
	for i := 0; i < 4; i++ {
		body := `{"model":"gpt-5.6-sol","conversation_id":"fair-` + string(rune('a'+i)) + `","input":[{"role":"user","content":"hello"}]}`
		status, _, _, got := postAutoKiroGPT(t, h, "/v1/responses", body, nil)
		if status != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", i, status, got)
		}
	}
	if codexCalls == 0 || kiroMock.callCount() == 0 {
		t.Fatalf("fair pool did not use both providers: codex=%d kiro=%d", codexCalls, kiroMock.callCount())
	}
}

func TestAutoKiroGPTCodexOnlyRouteDoesNotSelectTwice(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_codex_only","object":"response","model":"gpt-5.6-sol","status":"completed","output_text":"codex only"}`))
	})
	seedAutoCodexGPTAccount(t, h, "codex-only")
	before := h.app.scheduler.Metrics().RouteSelects
	status, header, _, body := postAutoKiroGPT(t, h, "/v1/responses", `{"model":"gpt-5.6-sol","conversation_id":"codex-only-route","input":[{"role":"user","content":"hello"}]}`, nil)
	after := h.app.scheduler.Metrics().RouteSelects
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") == "kiro" || !strings.Contains(body, "codex only") {
		t.Fatalf("Codex-only route status=%d provider=%q body=%s", status, header.Get("X-Pool-Resolved-Provider"), body)
	}
	if delta := after - before; delta != 1 {
		t.Fatalf("Codex-only request selected %d times, want exactly one", delta)
	}
}

func TestAutoKiroGPTHighPressureAdmitsKiroToFairPool(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_codex","object":"response","model":"gpt-5.6-sol","status":"completed"}`))
	})
	kiroMock := newAutoKiroGPTMock(t)
	seedAutoCodexGPTAccount(t, h, "pressure-codex-a")
	seedAutoCodexGPTAccount(t, h, "pressure-codex-b")
	seedAutoKiroGPTAccount(t, h, "pressure-kiro", kiroMock.server.URL)

	ctx := context.Background()
	first, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex", Model: autoKiroGPTModel})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex", Model: autoKiroGPTModel})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Release()
	snapshot, err := h.app.scheduler.ProviderPressureSnapshot(ctx, "cyber", "codex", autoKiroGPTModel)
	if err != nil || snapshot.PressurePercent <= 50 || !snapshot.ShouldAdmitKiroFairly() {
		t.Fatalf("pressure snapshot=%+v err=%v", snapshot, err)
	}

	status, header, _, got := postAutoKiroGPT(t, h, "/v1/responses", `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"help"}]}`, nil)
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" || kiroMock.callCount() != 1 {
		t.Fatalf("high-pressure fair admission status=%d provider=%q kiro=%d body=%s", status, header.Get("X-Pool-Resolved-Provider"), kiroMock.callCount(), got)
	}
}

func TestAutoKiroGPTMidConversationHandoffPreservesHistoryAndSticks(t *testing.T) {
	const conversationID = "codex-to-kiro-midstream"
	var codexCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		codexCalls++
		if !bytes.Contains(body, []byte("turn-one-user-marker")) {
			t.Errorf("initial Codex turn lost user content: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_codex_midstream","object":"response","model":"gpt-5.6-sol","status":"completed","output_text":"turn-one-codex-answer-marker"}`))
	})
	codexID := seedAutoCodexGPTAccount(t, h, "midstream-codex")

	firstBody := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"turn-one-user-marker"}]}`
	status, header, _, body := postAutoKiroGPT(t, h, "/v1/responses", firstBody, nil)
	if status != http.StatusOK || codexCalls != 1 || !strings.Contains(body, "turn-one-codex-answer-marker") {
		t.Fatalf("initial turn status=%d provider=%q codex=%d body=%s", status, header.Get("X-Pool-Resolved-Provider"), codexCalls, body)
	}
	t.Logf("MIDSTREAM_TURN_1 status=%d provider=codex codex_calls=%d kiro_calls=0", status, codexCalls)

	// Add Kiro only after the conversation has already started, then make the
	// original Codex account unavailable. The next self-contained turn must hand
	// off through the real Responses -> Anthropic -> Kiro bridge without dropping
	// the earlier user/assistant history carried by the client.
	kiroMock := newAutoKiroGPTMock(t)
	seedAutoKiroGPTAccount(t, h, "midstream-kiro", kiroMock.server.URL)
	if err := h.store.SetAccountStatus(context.Background(), codexID, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	secondBody := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","stream":true,"input":[{"role":"user","content":"turn-one-user-marker"},{"role":"assistant","content":"turn-one-codex-answer-marker"},{"role":"user","content":"turn-two-kiro-handoff-marker"}]}`
	status, header, trailer, body := postAutoKiroGPT(t, h, "/v1/responses", secondBody, nil)
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" ||
		!strings.Contains(body, "response.output_text.delta") || !strings.Contains(body, "response.completed") ||
		!strings.Contains(body, "kiro GPT answer") {
		t.Fatalf("handoff status=%d provider=%q headers=%v trailers=%v body=%s", status, header.Get("X-Pool-Resolved-Provider"), header, trailer, body)
	}
	if codexCalls != 1 || kiroMock.callCount() != 1 {
		t.Fatalf("handoff calls codex=%d kiro=%d", codexCalls, kiroMock.callCount())
	}
	t.Logf("MIDSTREAM_TURN_2 status=%d provider=%s codex_calls=%d kiro_calls=%d stream_completed=true", status, header.Get("X-Pool-Resolved-Provider"), codexCalls, kiroMock.callCount())
	bodies := kiroMock.bodiesSnapshot()
	for _, marker := range []string{"turn-one-user-marker", "turn-one-codex-answer-marker", "turn-two-kiro-handoff-marker"} {
		if len(bodies) != 1 || !bytes.Contains(bodies[0], []byte(marker)) {
			t.Fatalf("Kiro handoff lost %q: requests=%q", marker, bodies)
		}
	}

	affinity := routing.AffinityFromKey("conversation_id:"+conversationID, "conversation_id")
	bound, err := h.store.GetAffinityBinding(context.Background(), affinity.Hash)
	if err != nil || bound.Provider != "kiro" || bound.AccountID != "midstream-kiro" {
		t.Fatalf("handoff binding=%+v err=%v", bound, err)
	}
	t.Logf("MIDSTREAM_BINDING provider=%s account=%s epoch=%d", bound.Provider, bound.AccountID, bound.Epoch)

	// Once Kiro accepted the handoff, restoring Codex capacity must not bounce the
	// same conversation back across providers on the next turn.
	if err := h.store.SetAccountStatus(context.Background(), codexID, "active"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	thirdBody := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"turn-one-user-marker"},{"role":"assistant","content":"turn-one-codex-answer-marker"},{"role":"user","content":"turn-two-kiro-handoff-marker"},{"role":"assistant","content":"kiro GPT answer"},{"role":"user","content":"turn-three-sticky-kiro-marker"}]}`
	status, header, _, body = postAutoKiroGPT(t, h, "/v1/responses", thirdBody, nil)
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" ||
		codexCalls != 1 || kiroMock.callCount() != 2 || !strings.Contains(body, "kiro GPT answer") {
		t.Fatalf("sticky turn status=%d provider=%q codex=%d kiro=%d body=%s", status, header.Get("X-Pool-Resolved-Provider"), codexCalls, kiroMock.callCount(), body)
	}
	t.Logf("MIDSTREAM_TURN_3 status=%d provider=%s codex_calls=%d kiro_calls=%d sticky=true", status, header.Get("X-Pool-Resolved-Provider"), codexCalls, kiroMock.callCount())
	bodies = kiroMock.bodiesSnapshot()
	for _, marker := range []string{"turn-one-user-marker", "turn-one-codex-answer-marker", "turn-two-kiro-handoff-marker", "turn-three-sticky-kiro-marker"} {
		if len(bodies) != 2 || !bytes.Contains(bodies[1], []byte(marker)) {
			t.Fatalf("sticky Kiro turn lost %q: requests=%q", marker, bodies)
		}
	}
}

func TestAutoKiroGPTMidConversationContextErrorTriggersNativeCodexThenReturnsToKiro(t *testing.T) {
	const conversationID = "codex-kiro-context-recovery"
	var codexCalls int
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		codexCalls++
		answer := "initial codex answer"
		if bytes.Contains(body, []byte(`"type":"compaction_trigger"`)) {
			answer = "codex native compact summary"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_codex_context_recovery","object":"response","model":"gpt-5.6-sol","status":"completed","output_text":"` + answer + `"}`))
	})
	codexID := seedAutoCodexGPTAccount(t, h, "context-recovery-codex")

	first := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"initial context marker"}]}`
	status, _, _, body := postAutoKiroGPT(t, h, "/v1/responses", first, nil)
	if status != http.StatusOK || codexCalls != 1 || !strings.Contains(body, "initial codex answer") {
		t.Fatalf("initial Codex turn status=%d codex=%d body=%s", status, codexCalls, body)
	}

	kiroMock := newAutoKiroGPTRejectThenSucceedMock(t)
	seedAutoKiroGPTAccount(t, h, "context-recovery-kiro", kiroMock.server.URL)
	if err := h.store.SetAccountStatus(context.Background(), codexID, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	overflow := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"initial context marker"},{"role":"assistant","content":"initial codex answer"},{"role":"user","content":"overflow on midstream Kiro marker"}]}`
	status, header, _, body := postAutoKiroGPT(t, h, "/v1/responses", overflow, nil)
	if status != http.StatusBadRequest || kiroMock.callCount() != 1 ||
		header.Get("X-MiCliProxy-Context-Status") != "compact_required" ||
		header.Get("X-MiCliProxy-Auto-Compact") != "client_retry" ||
		header.Get("X-MiCliProxy-Compaction-Stage") != "codex_native" ||
		header.Get("X-MiCliProxy-Compaction-Order") != "codex_native,kiro_retry" ||
		!strings.Contains(body, `"code":"context_length_exceeded"`) ||
		!strings.Contains(body, "Codex should automatically compact the conversation and retry") ||
		strings.Contains(body, "Claude Code should automatically compact") {
		t.Fatalf("midstream context signal status=%d headers=%v body=%s", status, header, body)
	}
	t.Logf("MIDSTREAM_CONTEXT status=%d provider=kiro stage=%s order=%s", status, header.Get("X-MiCliProxy-Compaction-Stage"), header.Get("X-MiCliProxy-Compaction-Order"))

	if err := h.store.SetAccountStatus(context.Background(), codexID, "active"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	compact := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"preserve compact source"},{"type":"compaction_trigger"}]}`
	status, header, _, body = postAutoKiroGPT(t, h, "/v1/responses", compact, nil)
	if status != http.StatusOK || codexCalls != 2 || kiroMock.callCount() != 1 ||
		header.Get("X-MiCliProxy-Auto-Compact") != "codex_native" ||
		header.Get("X-MiCliProxy-Compaction-Stage") != "codex_native" ||
		!strings.Contains(body, "codex native compact summary") {
		t.Fatalf("native compact status=%d headers=%v codex=%d kiro=%d body=%s", status, header, codexCalls, kiroMock.callCount(), body)
	}
	t.Logf("MIDSTREAM_COMPACT status=%d provider=codex codex_calls=%d kiro_calls=%d", status, codexCalls, kiroMock.callCount())

	if err := h.store.SetAccountStatus(context.Background(), codexID, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	retry := `{"model":"gpt-5.6-sol","conversation_id":"` + conversationID + `","input":[{"role":"user","content":"codex native compact summary"},{"role":"user","content":"retry compacted history on Kiro"}]}`
	status, header, _, body = postAutoKiroGPT(t, h, "/v1/responses", retry, nil)
	if status != http.StatusOK || header.Get("X-Pool-Resolved-Provider") != "kiro" ||
		codexCalls != 2 || kiroMock.callCount() != 2 || !strings.Contains(body, "kiro answer after codex compaction") {
		t.Fatalf("post-compact Kiro retry status=%d provider=%q codex=%d kiro=%d body=%s", status, header.Get("X-Pool-Resolved-Provider"), codexCalls, kiroMock.callCount(), body)
	}
	bodies := kiroMock.bodiesSnapshot()
	if len(bodies) != 2 || !bytes.Contains(bodies[1], []byte("codex native compact summary")) ||
		!bytes.Contains(bodies[1], []byte("retry compacted history on Kiro")) {
		t.Fatalf("post-compact Kiro request lost reduced history: %q", bodies)
	}
	t.Logf("MIDSTREAM_RETRY status=%d provider=kiro codex_calls=%d kiro_calls=%d", status, codexCalls, kiroMock.callCount())
}

func TestAutoKiroGPTLeavesExplicitAndStatefulCodexRequestsNative(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		headers http.Header
		body    string
		compact bool
	}{
		{
			name: "explicit codex", headers: http.Header{"X-Pool-Provider": []string{"codex"}},
			body: `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "stateful responses",
			body: `{"model":"gpt-5.6-sol","previous_response_id":"resp_existing","input":[{"role":"user","content":"hello"}]}`,
		},
		{
			name:    "Codex native compaction trigger",
			body:    `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"preserve native history"},{"type":"compaction_trigger"}]}`,
			compact: true,
		},
		{
			name: "Codex native compact endpoint", path: "/v1/responses/compact",
			body:    `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"compact through native endpoint"}]}`,
			compact: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var codexCalls int
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				codexCalls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_codex","object":"response","status":"completed"}`))
			})
			kiroMock := newAutoKiroGPTMock(t)
			seedAutoCodexGPTAccount(t, h, "native-codex")
			seedAutoKiroGPTAccount(t, h, "native-kiro", kiroMock.server.URL)

			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			status, header, _, got := postAutoKiroGPT(t, h, path, tc.body, tc.headers)
			if status != http.StatusOK || codexCalls != 1 || kiroMock.callCount() != 0 {
				t.Fatalf("status=%d codex=%d kiro=%d body=%s", status, codexCalls, kiroMock.callCount(), got)
			}
			if tc.compact && (header.Get("X-MiCliProxy-Auto-Compact") != "codex_native" ||
				header.Get("X-MiCliProxy-Compaction-Stage") != "codex_native" ||
				header.Get("X-MiCliProxy-Compaction-Order") != "codex_native") {
				t.Fatalf("native Codex compaction metadata=%v", header)
			}
		})
	}
}
