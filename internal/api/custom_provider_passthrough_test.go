package api

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func seedCustomSharedProvider(
	t *testing.T,
	h *testHarness,
	id, baseURL, protocol, profile, upstreamKey string,
	enabled bool,
) string {
	t.Helper()
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID:                 id,
		Name:               id,
		BaseURL:            baseURL,
		UpstreamProtocol:   protocol,
		TransportProfile:   profile,
		Enabled:            enabled,
		AutoDiscoverModels: false,
	}); err != nil {
		t.Fatal(err)
	}
	accountID := id + "-account"
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID:        accountID,
		Label:     accountID,
		GroupName: "cyber",
		Provider:  id,
		Status:    "active",
	}, storage.AccountToken{
		// Deliberately omit auth_method and use a non-sk key. Legacy custom
		// provider rows must still classify OpenAIAPIKey as API-key auth.
		OpenAIAPIKey: upstreamKey,
	}); err != nil {
		t.Fatal(err)
	}
	return accountID
}

func TestCustomProviderSharedFilesPreservesMultipartQueryAuthBodyAndResponse(t *testing.T) {
	const (
		providerID  = "claude-relay-shared"
		poolKey     = "pool-custom-files"
		upstreamKey = "relay-files-secret"
		boundary    = "codex-pool-boundary-7MA4YWxkTrZu0gW"
	)
	var multipartBody bytes.Buffer
	writer := multipart.NewWriter(&multipartBody)
	if err := writer.SetBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("purpose", "assistants"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "fixture.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("opaque\x00multipart\npayload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	expectedBody := append([]byte(nil), multipartBody.Bytes()...)
	expectedContentType := "multipart/form-data; boundary=" + boundary

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/relay/v1/files" {
			t.Fatalf("path = %s, want /relay/v1/files", r.URL.Path)
		}
		if r.URL.RawQuery != "purpose=assistants&beta=true" {
			t.Fatalf("query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Content-Type"); got != expectedContentType {
			t.Fatalf("content-type = %q, want %q", got, expectedContentType)
		}
		if got := r.Header.Get("X-Api-Key"); got != upstreamKey {
			t.Fatalf("x-api-key = %q, want selected account key", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("unexpected authorization header = %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2025-01-01" {
			t.Fatalf("anthropic-version = %q", got)
		}
		if got := r.Header.Get("Anthropic-Beta"); got != "files-api-2025-04-14" {
			t.Fatalf("anthropic-beta = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "idem-files-1" {
			t.Fatalf("idempotency-key = %q", got)
		}
		if got := r.Header.Get("If-Match"); got != `"upload-etag"` {
			t.Fatalf("if-match = %q", got)
		}
		if got := r.Header.Get("If-None-Match"); got != `"no-duplicate"` {
			t.Fatalf("if-none-match = %q", got)
		}
		if got := r.Header.Get("Range"); got != "bytes=0-31" {
			t.Fatalf("range = %q", got)
		}
		if got := r.Header.Get("OpenAI-Beta"); got != "files=v1" {
			t.Fatalf("openai-beta = %q", got)
		}
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(body, expectedBody) {
			t.Fatalf("multipart body changed:\n got=%q\nwant=%q", body, expectedBody)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"relay-file-etag"`)
		w.Header().Set("X-Relay-Trace", "trace-files-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"file_custom_1","status":"ready"}`)
	})
	accountID := seedCustomSharedProvider(
		t,
		h,
		providerID,
		h.upstream.URL+"/relay/v1/",
		storage.CustomProviderProtocolAnthropicMessages,
		storage.CustomProviderTransportClaudeCode,
		upstreamKey,
		true,
	)
	seedDownstreamKey(t, h, poolKey, "custom:"+providerID)

	req, err := http.NewRequest(
		http.MethodPost,
		h.pool.URL+"/v1/files?purpose=assistants&beta=true",
		bytes.NewReader(expectedBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", poolKey)
	req.Header.Set("Content-Type", expectedContentType)
	req.Header.Set("Anthropic-Version", "2025-01-01")
	req.Header.Set("Anthropic-Beta", "files-api-2025-04-14")
	req.Header.Set("Idempotency-Key", "idem-files-1")
	req.Header.Set("If-Match", `"upload-etag"`)
	req.Header.Set("If-None-Match", `"no-duplicate"`)
	req.Header.Set("Range", "bytes=0-31")
	req.Header.Set("OpenAI-Beta", "files=v1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d body=%s", resp.StatusCode, responseBody)
	}
	if string(responseBody) != `{"id":"file_custom_1","status":"ready"}` {
		t.Fatalf("response body changed: %q", responseBody)
	}
	if got := resp.Header.Get("ETag"); got != `"relay-file-etag"` {
		t.Fatalf("response etag = %q", got)
	}
	if got := resp.Header.Get("X-Relay-Trace"); got != "trace-files-1" {
		t.Fatalf("response trace = %q", got)
	}
	if got := resp.Header.Get("X-Pool-Resolved-Provider"); got != "custom:"+providerID {
		t.Fatalf("resolved provider = %q", got)
	}

	storedProvider, found, err := h.store.GetCustomProvider(t.Context(), providerID)
	if err != nil || !found {
		t.Fatalf("load provider for resource affinity: found=%v err=%v", found, err)
	}
	storedProvider, _ = storage.ResolveCustomProviderRoute(storedProvider, "/v1/files/file_custom_1")
	affinityRequest := httptest.NewRequest(http.MethodGet, "/v1/files/file_custom_1", nil)
	affinityRequest = affinityRequest.WithContext(withDownstreamKey(
		affinityRequest.Context(), downstreamPolicy{KeyHash: hashAPIKey(poolKey)},
	))
	affinity, _, _ := customProviderResourceAffinity(affinityRequest, storedProvider, "/v1/files/file_custom_1")
	binding, err := h.store.GetAffinityBinding(context.Background(), affinity.Hash)
	if err != nil {
		t.Fatalf("created resource binding: %v", err)
	}
	if binding.Provider != providerID || binding.AccountID != accountID ||
		binding.Model != "resource:files" || binding.EgressID == "" {
		t.Fatalf("created resource binding = %+v", binding)
	}
}

func TestCustomProviderSharedSkillsGetAndAgentsRoute(t *testing.T) {
	const (
		providerID  = "openai-relay-shared"
		poolKey     = "pool-custom-skills"
		upstreamKey = "relay-openai-secret"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Relay-Resource", "preserved")
		switch r.URL.Path {
		case "/openai/v1/skills/skill_fixture":
			if r.Method != http.MethodGet || r.URL.RawQuery != "include=versions" {
				t.Fatalf("skills request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			if got := r.Header.Get("OpenAI-Beta"); got != "skills=v1" {
				t.Fatalf("OpenAI-Beta = %q", got)
			}
			if got := r.Header.Get("If-None-Match"); got != `"skill-etag"` {
				t.Fatalf("If-None-Match = %q", got)
			}
			_, _ = io.WriteString(w, `{"id":"skill_fixture","version":2}`)
		case "/openai/v1/agents/agent_fixture":
			if r.Method != http.MethodGet || r.URL.RawQuery != "expand=tools" {
				t.Fatalf("agents request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"id":"agent_fixture","tools":["search"]}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamKey {
			t.Fatalf("upstream authorization = %q", got)
		}
		if got := r.Header.Get("X-Api-Key"); got != "" {
			t.Fatalf("unexpected x-api-key = %q", got)
		}
	})
	seedCustomSharedProvider(
		t,
		h,
		providerID,
		h.upstream.URL+"/openai/v1",
		storage.CustomProviderProtocolChatCompletions,
		storage.CustomProviderTransportGeneric,
		upstreamKey,
		true,
	)
	// A conflicting key policy proves that the explicit custom hint is the
	// intentional selector; shared endpoints never auto-discover a relay.
	seedDownstreamKey(t, h, poolKey, "claude")

	requests := []struct {
		path string
		body string
	}{
		{path: "/v1/skills/skill_fixture?include=versions", body: `{"id":"skill_fixture","version":2}`},
		{path: "/v1/agents/agent_fixture?expand=tools", body: `{"id":"agent_fixture","tools":["search"]}`},
	}
	for _, test := range requests {
		req, err := http.NewRequest(http.MethodGet, h.pool.URL+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+poolKey)
		req.Header.Set("X-Pool-Provider", "custom:"+providerID)
		req.Header.Set("OpenAI-Beta", "skills=v1")
		req.Header.Set("If-None-Match", `"skill-etag"`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		responseBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || string(responseBody) != test.body {
			t.Fatalf("%s: status=%d body=%q", test.path, resp.StatusCode, responseBody)
		}
		if got := resp.Header.Get("X-Relay-Resource"); got != "preserved" {
			t.Fatalf("%s: response header = %q", test.path, got)
		}
		if got := resp.Header.Get("X-Pool-Resolved-Provider"); got != "custom:"+providerID {
			t.Fatalf("%s: resolved provider = %q", test.path, got)
		}
	}
}

func TestCustomProviderSharedEndpointUsesExplicitUserGroupRelayTarget(t *testing.T) {
	const (
		providerID = "group-relay-shared"
		poolKey    = "pool-user-group-shared"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/group/v1/skills" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("X-Api-Key"); got != "group-upstream-secret" {
			t.Fatalf("x-api-key = %q", got)
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"group-skill"}]}`)
	})
	seedCustomSharedProvider(
		t,
		h,
		providerID,
		h.upstream.URL+"/group/v1",
		storage.CustomProviderProtocolAnthropicMessages,
		storage.CustomProviderTransportGeneric,
		"group-upstream-secret",
		true,
	)
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID:   "shared-custom-group",
		Name: "shared custom group",
		Targets: []storage.TargetRef{{
			Kind: storage.TargetKindModelProvider,
			ID:   providerID,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash:      hashAPIKey(poolKey),
		Label:        "shared custom group key",
		GroupName:    "cyber",
		UserGroupID:  "shared-custom-group",
		ProviderHint: "auto",
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/skills", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", poolKey)
	// This protocol signal must not divert the explicit group relay target to
	// the built-in Claude account pool.
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "group-skill") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Pool-Resolved-Provider"); got != "custom:"+providerID {
		t.Fatalf("resolved provider = %q", got)
	}
}

func TestCustomProviderSharedEndpointRejectsDisabledProviderWithoutFallback(t *testing.T) {
	const providerID = "disabled-shared-relay"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("disabled custom provider reached upstream: %s %s", r.Method, r.URL.Path)
	})
	seedCustomSharedProvider(
		t,
		h,
		providerID,
		h.upstream.URL+"/v1",
		storage.CustomProviderProtocolAnthropicMessages,
		storage.CustomProviderTransportClaudeCode,
		"disabled-upstream-secret",
		false,
	)
	seedDownstreamKey(t, h, "pool-disabled-shared", "custom:"+providerID)

	req, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/skills", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "pool-disabled-shared")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(body), "capability_unavailable") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("unexpected upstream requests: %+v", requests)
	}
}
