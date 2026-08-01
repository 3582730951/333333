package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestCustomProviderSelectsEndpointByDownstreamPath(t *testing.T) {
	var chatCalls, responsesCalls atomic.Int64
	chatUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		if r.URL.Path != "/chat-base/v1/chat/completions" {
			t.Errorf("chat route path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-route","object":"chat.completion","model":"multi-model","choices":[{"index":0,"message":{"role":"assistant","content":"chat route"},"finish_reason":"stop"}]}`)
	}))
	defer chatUpstream.Close()
	responsesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responsesCalls.Add(1)
		if r.URL.Path != "/responses-base/v1/responses" {
			t.Errorf("responses route path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-route","object":"response","model":"multi-model","status":"completed","output":[]}`)
	}))
	defer responsesUpstream.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("default upstream should not be used: %s", r.URL.Path)
		http.Error(w, "wrong route", http.StatusBadGateway)
	})
	provider := storage.CustomProvider{
		ID: "multi-route-runtime", Name: "Multi Route Runtime",
		BaseURL: h.upstream.URL + "/default/v1", Enabled: true,
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		TransportProfile: storage.CustomProviderTransportGeneric,
		Models:           []string{"multi-model"},
		Routes: []storage.CustomProviderRoute{
			{DownstreamPath: storage.CustomProviderDownstreamChat, BaseURL: chatUpstream.URL + "/chat-base/v1", UpstreamProtocol: storage.CustomProviderProtocolChatCompletions, TransportProfile: storage.CustomProviderTransportGeneric},
			{DownstreamPath: storage.CustomProviderDownstreamResponses, BaseURL: responsesUpstream.URL + "/responses-base/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses, TransportProfile: storage.CustomProviderTransportCodexCLI},
		},
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: "multi-route-account", GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "multi-route-key"}); err != nil {
		t.Fatal(err)
	}

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"multi-model","messages":[{"role":"user","content":"chat"}]}`},
		{path: "/v1/responses", body: `{"model":"multi-model","input":"responses"}`},
	}
	for _, request := range requests {
		resp, err := http.Post(h.pool.URL+request.path, "application/json", strings.NewReader(request.body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", request.path, resp.StatusCode, raw)
		}
	}
	if chatCalls.Load() != 1 || responsesCalls.Load() != 1 {
		t.Fatalf("route calls chat=%d responses=%d", chatCalls.Load(), responsesCalls.Load())
	}
}

func TestCustomProviderDownstreamNamespacesAffinityAndCookies(t *testing.T) {
	provider := storage.CustomProvider{
		ID: "isolated-provider", BaseURL: "https://relay.example/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		TransportProfile: storage.CustomProviderTransportCodexCLI,
	}
	provider, _ = storage.ResolveCustomProviderRoute(provider, storage.CustomProviderDownstreamResponses)
	body := []byte(`{"model":"example","conversation_id":"shared-conversation"}`)
	requestFor := func(keyHash string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		return r.WithContext(withDownstreamKey(r.Context(), downstreamPolicy{KeyHash: keyHash}))
	}
	requestA := requestFor(strings.Repeat("a", 64))
	requestB := requestFor(strings.Repeat("b", 64))
	baseA := routing.ExtractAffinityKey(requestA, body)
	baseB := routing.ExtractAffinityKey(requestB, body)
	if baseA.Hash != baseB.Hash || baseA.Source != "conversation_id" {
		t.Fatalf("fixture affinity mismatch a=%+v b=%+v", baseA, baseB)
	}
	scopedA := customProviderScopedAffinity(requestA, provider, baseA)
	scopedAAgain := customProviderScopedAffinity(requestA, provider, baseA)
	scopedB := customProviderScopedAffinity(requestB, provider, baseB)
	if scopedA.Hash == scopedB.Hash || scopedA.Hash != scopedAAgain.Hash {
		t.Fatalf("downstream affinity isolation failed a=%+v again=%+v b=%+v", scopedA, scopedAAgain, scopedB)
	}
	if !routing.IsTrueConversationAffinity(scopedA) || !routing.IsTrueConversationAffinity(scopedB) {
		t.Fatalf("scoped true conversation lost persistence semantics: a=%+v b=%+v", scopedA, scopedB)
	}
	lease := scheduler.Lease{
		Account: storage.Account{ID: "account-1"},
		Egress:  storage.EgressProfile{ID: "egress-1"},
		Binding: storage.AccountEgressBinding{CookieJarKey: "account-1:egress-1"},
	}
	jarA := customProviderCookieJarKey(requestA, lease, provider)
	jarB := customProviderCookieJarKey(requestB, lease, provider)
	if jarA == jarB || !strings.HasPrefix(jarA, "account-1:egress-1:custom:") {
		t.Fatalf("cookie namespaces a=%q b=%q", jarA, jarB)
	}
	for _, secret := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if strings.Contains(jarA, secret) || strings.Contains(jarB, secret) {
			t.Fatal("cookie namespace leaked a downstream key hash")
		}
	}

	encoded, err := json.Marshal(map[string]string{"a": scopedA.Hash, "b": scopedB.Hash})
	if err != nil || len(encoded) == 0 {
		t.Fatalf("namespace evidence encoding: %v", err)
	}
}
