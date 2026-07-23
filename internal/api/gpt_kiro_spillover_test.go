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
			body, _ := io.ReadAll(r.Body)
			mock.mu.Lock()
			mock.calls++
			mock.bodies = append(mock.bodies, append([]byte(nil), body...))
			mock.mu.Unlock()
			if !bytes.Contains(body, []byte(`"modelId":"`+autoKiroGPTModel+`"`)) {
				t.Errorf("Kiro GPT request lost model id: %s", body)
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

func (m *autoKiroGPTMock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
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

func TestAutoKiroGPTLeavesExplicitAndStatefulCodexRequestsNative(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
	}{
		{
			name: "explicit codex", headers: http.Header{"X-Pool-Provider": []string{"codex"}},
			body: `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "stateful responses",
			body: `{"model":"gpt-5.6-sol","previous_response_id":"resp_existing","input":[{"role":"user","content":"hello"}]}`,
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

			status, _, _, got := postAutoKiroGPT(t, h, "/v1/responses", tc.body, tc.headers)
			if status != http.StatusOK || codexCalls != 1 || kiroMock.callCount() != 0 {
				t.Fatalf("status=%d codex=%d kiro=%d body=%s", status, codexCalls, kiroMock.callCount(), got)
			}
		})
	}
}
