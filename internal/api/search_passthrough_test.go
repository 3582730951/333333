package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

type searchAccountFixture struct {
	id     string
	group  string
	key    string
	models []string
}

func seedSearchCustomProvider(
	t *testing.T,
	h *testHarness,
	provider storage.CustomProvider,
	accounts ...searchAccountFixture,
) {
	t.Helper()
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range accounts {
		if err := h.store.UpsertAccount(t.Context(), storage.Account{
			ID: fixture.id, Label: fixture.id, GroupName: fixture.group,
			Provider: provider.ID, Status: "active",
		}, storage.AccountToken{OpenAIAPIKey: fixture.key}); err != nil {
			t.Fatal(err)
		}
		caps := make([]storage.ModelCapability, 0, len(fixture.models))
		for _, model := range fixture.models {
			caps = append(caps, storage.ModelCapability{
				AccountID: fixture.id, ModelSlug: model,
				AvailabilityState: capability.AvailabilityVerified,
				Source:            "search_route_test",
			})
		}
		if len(caps) > 0 {
			if err := h.store.UpsertCapabilities(t.Context(), caps); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.app.scheduler.InvalidateAccountCache()
}

func TestStandaloneSearchAutoRoutesMappedModelToResponsesCustomProvider(t *testing.T) {
	const (
		providerID  = "mapped-search-responses"
		requested   = "client-visible-search-model"
		target      = "relay-search-model"
		upstreamKey = "search-relay-secret"
	)
	requestBody := `{"id":"search-session-1","model":"` + requested + `","input":"find this","commands":{"search_query":[{"q":"release notes","exact_id":900719925474099312345}]},"settings":{"search_context_size":"low"},"max_output_tokens":2500}`
	responseBody := `{"encrypted_output":"ciphertext","output":"search result","results":[{"type":"text_result","ref_id":"turn0search0","future_field":{"preserved":true}}]}`

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay/v1/alpha/search" || r.URL.RawQuery != "mode=live" {
			t.Fatalf("custom search URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+upstreamKey {
			t.Fatalf("custom search auth = %q", got)
		}
		if got := r.Header.Get("Originator"); got != "codex_exec" {
			t.Fatalf("custom search Originator = %q", got)
		}
		if got := r.Header.Get("X-Codex-Turn-Metadata"); got != `{"thread_id":"search-thread-1"}` {
			t.Fatalf("custom search turn metadata = %q", got)
		}
		if got := r.Header.Get("X-Pool-Provider"); got != "" {
			t.Fatalf("pool provider header leaked upstream: %q", got)
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(`"model":"`+target+`"`)) ||
			!bytes.Contains(raw, []byte(`900719925474099312345`)) ||
			!bytes.Contains(raw, []byte(`"search_context_size":"low"`)) {
			t.Fatalf("mapped search body drifted: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: providerID, Name: providerID, BaseURL: h.upstream.URL + "/relay/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		TransportProfile: storage.CustomProviderTransportGeneric,
		Enabled:          true, Models: []string{target},
		ModelMappings: map[string]string{requested: target},
	}, searchAccountFixture{id: providerID + "-account", group: "cyber", key: upstreamKey, models: []string{target}})

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+standaloneSearchPath+"?mode=live", strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Originator", "codex_exec")
	req.Header.Set("X-Codex-Turn-Metadata", `{"thread_id":"search-thread-1"}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(raw) != responseBody {
		t.Fatalf("search response status=%d body=%s", resp.StatusCode, raw)
	}
	if got := resp.Header.Get("X-Pool-Resolved-Provider"); got != "custom:"+providerID {
		t.Fatalf("resolved provider = %q", got)
	}
	if got := resp.Header.Get("X-Pool-Resolved-Model"); got != target {
		t.Fatalf("resolved model = %q", got)
	}
}

func TestStandaloneSearchSkipsModelMatchingChatProvider(t *testing.T) {
	const model = "shared-search-model"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses/v1/alpha/search" {
			t.Fatalf("search reached wrong provider path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer responses-search-key" {
			t.Fatalf("search used wrong provider account: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":null,"output":"ok","results":[]}`)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "a-chat-search", BaseURL: h.upstream.URL + "/chat/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		Enabled:          true, Models: []string{model},
	}, searchAccountFixture{id: "a-chat-search-account", group: "cyber", key: "chat-search-key", models: []string{model}})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "b-responses-search", BaseURL: h.upstream.URL + "/responses/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		Enabled:          true, Models: []string{model},
	}, searchAccountFixture{id: "b-responses-search-account", group: "cyber", key: "responses-search-key", models: []string{model}})

	resp, err := http.Post(h.pool.URL+standaloneSearchPath, "application/json", strings.NewReader(
		`{"id":"search-capability-filter","model":"`+model+`","commands":{"search_query":[{"q":"test"}]}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Pool-Resolved-Provider") != "custom:b-responses-search" {
		t.Fatalf("status=%d provider=%q body=%s", resp.StatusCode, resp.Header.Get("X-Pool-Resolved-Provider"), raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer responses-search-key" {
		t.Fatalf("upstream attempts = %+v", requests)
	}
}

func TestStandaloneSearchUsesExactResponsesRouteOnMultiRouteProvider(t *testing.T) {
	const model = "multi-route-search-model"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search-base/v1/alpha/search" {
			t.Fatalf("exact search route path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":null,"output":"routed","results":[]}`)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "multi-route-search", BaseURL: h.upstream.URL + "/default-chat/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		Enabled:          true, Models: []string{model},
		Routes: []storage.CustomProviderRoute{{
			ID: "standalone-search", DownstreamPath: standaloneSearchPath,
			BaseURL:          h.upstream.URL + "/search-base/v1",
			UpstreamProtocol: storage.CustomProviderProtocolResponses,
			TransportProfile: storage.CustomProviderTransportGeneric,
		}},
	}, searchAccountFixture{id: "multi-route-search-account", group: "cyber", key: "multi-route-search-key", models: []string{model}})

	resp, err := http.Post(h.pool.URL+standaloneSearchPath, "application/json", strings.NewReader(
		`{"id":"multi-route-search","model":"`+model+`","commands":{}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"output":"routed"`)) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}

func TestStandaloneSearchPrefersExplicitPathRouteOverGenericResponsesProvider(t *testing.T) {
	const model = "explicit-search-route-model"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/explicit/v1/alpha/search" || r.Header.Get("Authorization") != "Bearer explicit-search-key" {
			t.Fatalf("search did not prefer explicit route: path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":null,"output":"explicit","results":[]}`)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "a-generic-responses", BaseURL: h.upstream.URL + "/generic/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		Enabled:          true, Models: []string{model},
	}, searchAccountFixture{id: "generic-search-account", group: "cyber", key: "generic-search-key", models: []string{model}})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "z-explicit-search", BaseURL: h.upstream.URL + "/default/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		Enabled:          true, Models: []string{model},
		Routes: []storage.CustomProviderRoute{{
			ID: "default", DownstreamPath: standaloneSearchPath,
			BaseURL: h.upstream.URL + "/explicit/v1", UpstreamProtocol: storage.CustomProviderProtocolResponses,
		}},
	}, searchAccountFixture{id: "explicit-search-account", group: "cyber", key: "explicit-search-key", models: []string{model}})

	resp, err := http.Post(h.pool.URL+standaloneSearchPath, "application/json", strings.NewReader(
		`{"id":"explicit-search","model":"`+model+`","commands":{}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"output":"explicit"`)) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer explicit-search-key" {
		t.Fatalf("upstream attempts = %+v", requests)
	}
}

func TestStandaloneSearchExplicitNonResponsesProviderReturnsCapabilityError(t *testing.T) {
	const providerID = "unsupported-search-chat"
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unsupported search provider reached upstream: %s", r.URL.Path)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: providerID, BaseURL: h.upstream.URL + "/chat/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		Enabled:          true, Models: []string{"unsupported-search-model"},
	}, searchAccountFixture{id: providerID + "-account", group: "cyber", key: "unsupported-search-key", models: []string{"unsupported-search-model"}})

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+standaloneSearchPath, strings.NewReader(
		`{"id":"unsupported-search","model":"unsupported-search-model","commands":{}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pool-Provider", "custom:"+providerID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !bytes.Contains(raw, []byte("capability_unavailable")) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("unsupported provider made upstream calls: %+v", requests)
	}
}

func TestStandaloneSearchActualLeaseHonorsGroupAndMappedModel(t *testing.T) {
	const (
		providerID = "bounded-search-provider"
		requested  = "bounded-client-model"
		target     = "bounded-upstream-model"
		group      = "bounded-search-group"
		poolKey    = "bounded-search-pool-key"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer bounded-target-key" {
			t.Fatalf("search crossed group/model account boundary: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":null,"output":"bounded","results":[]}`)
	})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: group}); err != nil {
		t.Fatal(err)
	}
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: providerID, BaseURL: h.upstream.URL + "/bounded/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		Enabled:          true, Models: []string{target},
		ModelMappings: map[string]string{requested: target},
	},
		searchAccountFixture{id: "bounded-wrong-group", group: "cyber", key: "bounded-wrong-group-key", models: []string{target}},
		searchAccountFixture{id: "bounded-wrong-model", group: group, key: "bounded-wrong-model-key", models: []string{"different-model"}},
		searchAccountFixture{id: "bounded-target", group: group, key: "bounded-target-key", models: []string{target}},
	)
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(poolKey), Label: "bounded search", GroupName: group,
		ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+standaloneSearchPath, strings.NewReader(
		`{"id":"bounded-search","model":"`+requested+`","commands":{}}`,
	))
	req.Header.Set("Authorization", "Bearer "+poolKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"output":"bounded"`)) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer bounded-target-key" {
		t.Fatalf("upstream attempts = %+v", requests)
	}
}

func TestStandaloneSearchUserGroupFallsThroughUnsupportedProviderTier(t *testing.T) {
	const (
		model   = "group-search-model"
		groupID = "standalone-search-user-group"
		poolKey = "standalone-search-user-key"
	)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/secondary/v1/alpha/search" || r.Header.Get("Authorization") != "Bearer secondary-search-key" {
			t.Fatalf("user-group search reached wrong target: path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"encrypted_output":null,"output":"secondary","results":[]}`)
	})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "group-search-primary-chat", BaseURL: h.upstream.URL + "/primary/v1",
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		Enabled:          true, Models: []string{model},
	}, searchAccountFixture{id: "group-search-primary-account", group: "cyber", key: "primary-search-key", models: []string{model}})
	seedSearchCustomProvider(t, h, storage.CustomProvider{
		ID: "group-search-secondary", BaseURL: h.upstream.URL + "/secondary/v1",
		UpstreamProtocol: storage.CustomProviderProtocolResponses,
		Enabled:          true, Models: []string{model},
	}, searchAccountFixture{id: "group-search-secondary-account", group: "cyber", key: "secondary-search-key", models: []string{model}})
	primary := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "group-search-primary-chat"}
	secondary := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "group-search-secondary"}
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID: groupID, Name: groupID, Targets: []storage.TargetRef{primary, secondary},
		ModelRouting: []storage.ModelRoutingRule{{Model: model, Tiers: [][]storage.TargetRef{{primary}, {secondary}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(poolKey), Label: "search user group", GroupName: "cyber",
		UserGroupID: groupID, ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"id":"user-group-search","model":"` + model + `","commands":{"search_query":[{"q":"fallback"}]}}`)
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+standaloneSearchPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+poolKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Thread-Id", "user-group-search-thread")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Pool-Resolved-Provider") != "custom:"+secondary.ID ||
		!bytes.Contains(raw, []byte(`"output":"secondary"`)) {
		t.Fatalf("status=%d provider=%q body=%s", resp.StatusCode, resp.Header.Get("X-Pool-Resolved-Provider"), raw)
	}
	if requests := h.requests(); len(requests) != 1 || requests[0].Auth != "Bearer secondary-search-key" {
		t.Fatalf("upstream attempts = %+v", requests)
	}
	affinityRequest, _ := http.NewRequest(http.MethodPost, standaloneSearchPath, bytes.NewReader(body))
	affinityRequest.Header.Set("Thread-Id", "user-group-search-thread")
	affinity := routing.ExtractAffinityKey(affinityRequest, body)
	binding, found, err := h.store.GetUserGroupTargetBinding(context.Background(), groupID, affinity.Hash, "")
	if err != nil || !found || binding.Target != secondary {
		t.Fatalf("search target binding=%+v found=%v err=%v", binding, found, err)
	}
}
