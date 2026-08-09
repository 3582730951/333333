package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

const customClaudeRelayResponse = `{
  "id":"msg_relay","type":"message","role":"assistant","model":"relay-sonnet",
  "content":[{"type":"text","text":"relay reached"}],
  "stop_reason":"end_turn",
  "usage":{"input_tokens":2,"output_tokens":2}
}`

func postJSONForTest(t *testing.T, endpoint string, body interface{}) (*http.Response, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, responseBody
}

func TestCustomClaudeProviderMappingRoutesToRelayAndAdminTest(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("relay path = %q, want /v1/messages", r.URL.Path)
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	})

	resp, raw := postJSONForTest(t, h.pool.URL+"/admin/providers", map[string]interface{}{
		"id":                   "claude-relay",
		"name":                 "Claude Relay",
		"base_url":             h.upstream.URL + "/v1",
		"upstream_protocol":    storage.CustomProviderProtocolAnthropicMessages,
		"transport_profile":    storage.CustomProviderTransportGeneric,
		"enabled":              true,
		"auto_discover_models": false,
		"model_mappings": map[string]string{
			"CLAUDE-SONNET-5": "relay-sonnet",
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create provider status=%d body=%s", resp.StatusCode, raw)
	}
	var saved storage.CustomProvider
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ModelMappings["claude-sonnet-5"] != "relay-sonnet" {
		t.Fatalf("normalized model mappings = %#v", saved.ModelMappings)
	}

	resp, raw = postJSONForTest(t, h.pool.URL+"/admin/accounts/import-key", map[string]interface{}{
		"provider_id": "claude-relay",
		"api_key":     "relay-api-key",
		"label":       "relay account",
		// group_name is intentionally omitted: URL + API key is the complete
		// operator setup for a custom relay account.
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import key status=%d body=%s", resp.StatusCode, raw)
	}
	var account storage.Account
	if err := json.Unmarshal(raw, &account); err != nil {
		t.Fatal(err)
	}
	if account.GroupName != h.app.cfg.DefaultGroup {
		t.Fatalf("import group = %q, want default %q", account.GroupName, h.app.cfg.DefaultGroup)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: account.ID, ModelSlug: "unrelated-existing-model",
		AvailabilityState: capability.AvailabilityVerified,
		Context1MState:    capability.Context1MUnknown,
		Source:            "unrelated-before-admin-test",
	}}); err != nil {
		t.Fatal(err)
	}

	resp, raw = postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      "claude-sonnet-5",
		"max_tokens": 16,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("messages status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 {
		t.Fatalf("production request count=%d, want 1: %+v", len(requests), requests)
	}
	if requests[0].Path != "/v1/messages" ||
		requests[0].Auth != "Bearer relay-api-key" ||
		!strings.Contains(requests[0].Body, `"model":"relay-sonnet"`) {
		t.Fatalf("production relay request = %+v", requests[0])
	}
	assertClaudeCodeIdentityWireShape(t, requests[0].Body, "relay-sonnet")

	resp, raw = postJSONForTest(t, h.pool.URL+"/admin/providers/claude-relay/test", map[string]interface{}{
		"model": "claude-sonnet-5",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin provider test status=%d body=%s", resp.StatusCode, raw)
	}
	var result customProviderModelTestResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK ||
		result.RequestedModel != "claude-sonnet-5" ||
		result.TargetModel != "relay-sonnet" ||
		result.UpstreamPath != "/messages" ||
		result.HTTPStatus != http.StatusOK {
		t.Fatalf("provider test result = %+v", result)
	}
	requests = h.requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Body, `"model":"relay-sonnet"`) {
		t.Fatalf("admin test did not reach mapped relay: %+v", requests)
	}
	assertClaudeCodeProbeWireShape(t, requests[1].Body, "relay-sonnet", "Reply OK", 1)
	caps, err := h.store.ListCapabilities(t.Context(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, cap := range caps {
		states[cap.ModelSlug] = cap.AvailabilityState
	}
	if states["claude-sonnet-5"] != capability.AvailabilityVerified ||
		states["relay-sonnet"] != capability.AvailabilityVerified {
		t.Fatalf("mapping source/target capability states = %#v", states)
	}
	if states["unrelated-existing-model"] != capability.AvailabilityVerified {
		t.Fatalf("admin provider test erased unrelated capability: %#v", states)
	}
}

func TestCustomNativeAnthropicToolsUseExplicitAutoChoice(t *testing.T) {
	const model = "native-anthropic-tool-model"
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var request map[string]interface{}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		choice, _ := request["tool_choice"].(map[string]interface{})
		if choice["type"] != "auto" {
			t.Fatalf("native relay request omitted explicit auto tool choice: %s", raw)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_tool","type":"message","role":"assistant","model":"`+model+`","content":[{"type":"tool_use","id":"toolu_write","name":"write_file","input":{"path":"a.txt"}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`)
	})
	setupProtocolMatrixProvider(t, h, model, storage.CustomProviderProtocolAnthropicMessages, model)
	body := `{"model":"` + model + `","max_tokens":64,"messages":[{"role":"user","content":"write it"}],"tools":[{"name":"write_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`
	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", json.RawMessage(body))
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"type":"tool_use"`)) || !bytes.Contains(raw, []byte(`"name":"write_file"`)) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
}

func TestCustomClaudeAutoDiscoveryProbesMaintainedModelTable(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.Error(w, "models endpoint unavailable", http.StatusNotFound)
		case "/v1/messages":
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode candidate request: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			assertClaudeCodeProbeWireShape(t, string(raw), body.Model, "Reply OK", 1)
			mu.Lock()
			probed = append(probed, body.Model)
			mu.Unlock()
			if body.Model != "claude-sonnet-5" {
				http.Error(w, "model not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, customClaudeRelayResponse)
		default:
			http.Error(w, "wrong path", http.StatusNotFound)
		}
	})
	provider := storage.CustomProvider{
		ID:                 "claude-discovery",
		Name:               "Claude Discovery",
		BaseURL:            h.upstream.URL + "/v1",
		UpstreamProtocol:   storage.CustomProviderProtocolAnthropicMessages,
		TransportProfile:   storage.CustomProviderTransportGeneric,
		Enabled:            true,
		AutoDiscoverModels: true,
		Models:             nil,
		ModelMappings:      nil,
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: "claude-discovery-account", Label: "discovery",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}
	if err := h.store.UpsertAccount(t.Context(), account, storage.AccountToken{OpenAIAPIKey: "discovery-key"}); err != nil {
		t.Fatal(err)
	}

	caps, err := h.app.probeAccountModels(context.Background(), account)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotProbed := append([]string(nil), probed...)
	mu.Unlock()
	wantProbed := capability.ClaudeProbeModelTable()
	if len(gotProbed) != len(wantProbed) {
		t.Fatalf("probed %d models, want maintained table of %d: %v", len(gotProbed), len(wantProbed), gotProbed)
	}
	for i := range wantProbed {
		if gotProbed[i] != wantProbed[i] {
			t.Fatalf("probe[%d] = %q, want %q; got=%v", i, gotProbed[i], wantProbed[i], gotProbed)
		}
	}
	if len(caps) != 1 ||
		caps[0].ModelSlug != "claude-sonnet-5" ||
		caps[0].AvailabilityState != capability.AvailabilityVerified {
		t.Fatalf("discovered capabilities = %+v", caps)
	}
	saved, ok, err := h.store.GetCustomProvider(t.Context(), provider.ID)
	if err != nil || !ok {
		t.Fatalf("reload provider: found=%v err=%v", ok, err)
	}
	if len(saved.Models) != 1 || saved.Models[0] != "claude-sonnet-5" {
		t.Fatalf("persisted discovered models = %#v", saved.Models)
	}
}

func TestCustomClaudeAutoTableRoutesBeforeFirstDiscovery(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	})
	provider := storage.CustomProvider{
		ID: "claude-empty-auto", Name: "Claude Empty Auto", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          true, AutoDiscoverModels: true,
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: "claude-empty-auto-account", Label: "auto",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "auto-key"}); err != nil {
		t.Fatal(err)
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      "claude-opus-5",
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-discovery route status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Path != "/v1/messages" {
		t.Fatalf("pre-discovery request did not reach relay: %+v", requests)
	}
}

func TestDisabledCustomProviderSkipsAutomaticModelProbeAndPreservesCapabilities(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("disabled custom provider received unexpected probe: %s %s", r.Method, r.URL.String())
		http.Error(w, "disabled provider must not be contacted", http.StatusInternalServerError)
	})
	provider := storage.CustomProvider{
		ID:                 "disabled-relay",
		Name:               "Disabled Relay",
		BaseURL:            h.upstream.URL + "/v1",
		UpstreamProtocol:   storage.CustomProviderProtocolAnthropicMessages,
		Enabled:            false,
		AutoDiscoverModels: true,
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: "disabled-relay-account", Label: "disabled",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}
	if err := h.store.UpsertAccount(t.Context(), account, storage.AccountToken{OpenAIAPIKey: "disabled-key"}); err != nil {
		t.Fatal(err)
	}
	want := storage.ModelCapability{
		AccountID: account.ID, ModelSlug: "claude-sonnet-5",
		AvailabilityState: capability.AvailabilityVerified,
		Context1MState:    capability.Context1MUnknown,
		Source:            "existing-before-disable",
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{want}); err != nil {
		t.Fatal(err)
	}

	got, err := h.app.probeAccountModels(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ModelSlug != want.ModelSlug || got[0].Source != want.Source {
		t.Fatalf("disabled provider capabilities = %+v, want preserved %+v", got, want)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("disabled provider request count=%d, want 0: %+v", len(requests), requests)
	}
}

func TestCustomClaudeAutoDiscoveryRejectsInvalidTwoHundredEnvelope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.Error(w, "not implemented", http.StatusNotFound)
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		default:
			http.NotFound(w, r)
		}
	})
	provider := storage.CustomProvider{
		ID: "invalid-envelope-relay", Name: "Invalid Envelope Relay",
		BaseURL: h.upstream.URL + "/v1", Enabled: true, AutoDiscoverModels: true,
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: "invalid-envelope-account", Label: "invalid envelope",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}
	if err := h.store.UpsertAccount(t.Context(), account, storage.AccountToken{OpenAIAPIKey: "invalid-envelope-key"}); err != nil {
		t.Fatal(err)
	}

	caps, err := h.app.probeAccountModels(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	for _, cap := range caps {
		if cap.AvailabilityState == capability.AvailabilityVerified {
			t.Fatalf("invalid 2xx envelope produced verified capability: %+v", cap)
		}
	}
	requests := h.requests()
	if len(requests) != 2 {
		t.Fatalf("invalid 2xx should stop after one candidate; requests=%d %+v", len(requests), requests)
	}
}

func TestCustomProviderAutoRouteSkipsUnschedulableMatchingProvider(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	})
	const model = "overlap-custom-model"
	for _, provider := range []storage.CustomProvider{
		{
			ID: "aaa-empty-provider", Name: "AAA Empty", BaseURL: h.upstream.URL + "/v1",
			UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
			Enabled:          true, Models: []string{model},
		},
		{
			ID: "bbb-healthy-provider", Name: "BBB Healthy", BaseURL: h.upstream.URL + "/v1",
			UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
			Enabled:          true, Models: []string{model},
		},
	} {
		if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: "bbb-healthy-account", Label: "healthy custom",
		GroupName: h.app.cfg.DefaultGroup, Provider: "bbb-healthy-provider", Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "bbb-healthy-key"}); err != nil {
		t.Fatal(err)
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      model,
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("auto custom fallback status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer bbb-healthy-key" {
		t.Fatalf("unschedulable first provider shadowed healthy provider: %+v", requests)
	}
}

func TestNonClaudeCustomBracketModelRemainsLiteral(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	})
	const model = "vendor-model[preview]"
	provider := storage.CustomProvider{
		ID: "bracket-model-relay", Name: "Bracket Model Relay", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          true, Models: []string{model},
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{
		ID: "bracket-model-account", Label: "bracket model",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}
	if err := h.store.UpsertAccount(t.Context(), account, storage.AccountToken{OpenAIAPIKey: "bracket-model-key"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: account.ID, ModelSlug: model,
		AvailabilityState: capability.AvailabilityVerified,
		Source:            "bracket-model-test",
	}}); err != nil {
		t.Fatal(err)
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      model,
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bracket custom model status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 ||
		requests[0].Path != "/v1/messages" ||
		!strings.Contains(requests[0].Body, `"model":"`+model+`"`) {
		t.Fatalf("bracket custom model was rejected or rewritten: %+v", requests)
	}
}

func TestCustomProviderCountTokensDoesNotRequireSchedulableAccount(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("local count_tokens reached upstream: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	})
	const model = "local-count-empty-model"
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "local-count-empty", Name: "Local Count Empty", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          true, Models: []string{model},
	}); err != nil {
		t.Fatal(err)
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages/count_tokens", map[string]interface{}{
		"model":    model,
		"messages": []map[string]interface{}{{"role": "user", "content": "count without an account"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accountless count_tokens status=%d body=%s", resp.StatusCode, raw)
	}
	var result struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.InputTokens <= 0 {
		t.Fatalf("accountless count_tokens result=%+v body=%s", result, raw)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("accountless count_tokens upstream requests=%d: %+v", len(requests), requests)
	}
}

func TestCustomProviderCountTokensIgnoresFullConcurrencyButInferenceDoesNot(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("saturated custom provider reached upstream: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	})
	const (
		model      = "local-count-saturated-model"
		providerID = "local-count-saturated"
		accountID  = "local-count-saturated-account"
	)
	if err := h.store.UpsertEgressProfile(t.Context(), storage.EgressProfile{
		ID: storage.DefaultDirectEgressID, Name: "direct", Type: "direct",
		StreamCapable: true, Health: "healthy", MaxConcurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: providerID, Name: "Local Count Saturated", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          true, Models: []string{model},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: accountID, Label: "saturated count account",
		GroupName: h.app.cfg.DefaultGroup, Provider: providerID, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "saturated-count-key"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: accountID, ModelSlug: model,
		AvailabilityState: capability.AvailabilityVerified,
		Source:            "saturated-count-test",
	}}); err != nil {
		t.Fatal(err)
	}
	held, err := h.app.scheduler.Select(t.Context(), scheduler.Route{
		Group: h.app.cfg.DefaultGroup, Provider: providerID, Model: model,
	})
	if err != nil {
		t.Fatalf("hold sole custom-provider slot: %v", err)
	}
	defer held.Release()

	countBody := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"count while saturated"}]}`)
	for _, tc := range []struct {
		name         string
		providerHint string
	}{
		{name: "automatic"},
		{name: "explicit", providerHint: "custom:" + providerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages/count_tokens", bytes.NewReader(countBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.providerHint != "" {
				req.Header.Set("X-Pool-Provider", tc.providerHint)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"input_tokens"`)) {
				t.Fatalf("saturated %s count_tokens status=%d body=%s", tc.name, resp.StatusCode, raw)
			}
		})
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      model,
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "inference must still preflight"}},
	})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("saturated inference bypassed scheduler preflight: body=%s", raw)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("saturated local counts/inference reached upstream: %+v", requests)
	}
}

func TestCustomProviderAutoRouteFallsBackToBuiltInWhenNoCustomCandidateIsSchedulable(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_builtin","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"built-in reached"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "aaa-empty-claude", Name: "Empty Claude Relay", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          true, Models: []string{"claude-opus-4-8"},
	}); err != nil {
		t.Fatal(err)
	}
	seedClaudeContextAccount(t, h, "native-claude-fallback", "Pro", capability.Context1MUnsupported, accountprovider.AuthMethodOAuth)

	resp, raw := postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      "claude-opus-4-8",
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte("built-in reached")) {
		t.Fatalf("built-in fallback status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 || requests[0].Auth != "Bearer credential-native-claude-fallback" {
		t.Fatalf("request did not fall through to built-in Claude: %+v", requests)
	}
}

func TestCustomAnthropicMappingParsesOneMillionSuffixBeforeRouting(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	})
	resp, raw := postJSONForTest(t, h.pool.URL+"/admin/providers", map[string]interface{}{
		"id":                   "mapped-1m-relay",
		"name":                 "Mapped 1M Relay",
		"base_url":             h.upstream.URL + "/v1",
		"upstream_protocol":    storage.CustomProviderProtocolAnthropicMessages,
		"enabled":              true,
		"auto_discover_models": false,
		"model_mappings": map[string]string{
			"claude-opus-4-8": "relay-opus-extended",
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create mapped provider status=%d body=%s", resp.StatusCode, raw)
	}
	resp, raw = postJSONForTest(t, h.pool.URL+"/admin/accounts/import-key", map[string]interface{}{
		"provider_id": "mapped-1m-relay",
		"api_key":     "mapped-1m-key",
		"label":       "mapped 1m",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import mapped provider key status=%d body=%s", resp.StatusCode, raw)
	}

	resp, raw = postJSONForTest(t, h.pool.URL+"/v1/messages", map[string]interface{}{
		"model":      "claude-opus-4-8[1m]",
		"max_tokens": 8,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mapped 1m status=%d body=%s", resp.StatusCode, raw)
	}
	requests := h.requests()
	if len(requests) != 1 {
		t.Fatalf("mapped 1m upstream requests=%d, want 1: %+v", len(requests), requests)
	}
	if strings.Contains(requests[0].Body, "[1m]") ||
		!strings.Contains(requests[0].Body, `"model":"relay-opus-extended"`) ||
		!strings.Contains(strings.ToLower(requests[0].Beta), anthropicContext1MBeta) {
		t.Fatalf("mapped 1m wire request missing base-model rewrite/beta: %+v", requests[0])
	}
}

func TestCustomProviderModelMappingClaudeAliasIsScopedAndExactFirst(t *testing.T) {
	provider := storage.CustomProvider{ModelMappings: map[string]string{
		"claude-opus-4-8": "relay-opus",
		"acme-model-4-8":  "must-not-fuzzy-match",
		"*":               "fallback",
	}}
	if target, mapped := customProviderMappedModel(provider, "claude-opus-4.8"); !mapped || target != "relay-opus" {
		t.Fatalf("Claude concrete alias target=%q mapped=%v", target, mapped)
	}
	if target, mapped := customProviderMappedModel(provider, "acme-model-4.8"); !mapped || target != "fallback" {
		t.Fatalf("arbitrary custom slug was fuzzily matched target=%q mapped=%v", target, mapped)
	}
	provider.ModelMappings["claude-opus-4.8"] = "exact-opus"
	if target, mapped := customProviderMappedModel(provider, "claude-opus-4.8"); !mapped || target != "exact-opus" {
		t.Fatalf("exact mapping did not win target=%q mapped=%v", target, mapped)
	}
}

func TestExplicitDisabledCustomProviderReturnsVisibleError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("disabled explicit custom provider reached upstream: %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	})
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: "explicit-disabled", Name: "Explicit Disabled", BaseURL: h.upstream.URL + "/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		Enabled:          false, Models: []string{"explicit-disabled-model"},
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"explicit-disabled-model","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pool-Provider", "custom:explicit-disabled")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!bytes.Contains(raw, []byte("selected custom model provider is disabled or unavailable")) {
		t.Fatalf("disabled explicit provider status=%d body=%s", resp.StatusCode, raw)
	}
	if requests := h.requests(); len(requests) != 0 {
		t.Fatalf("disabled explicit provider upstream requests=%d: %+v", len(requests), requests)
	}
}

func TestAdminCustomProviderTestUsesConfiguredProductionEgress(t *testing.T) {
	var mu sync.Mutex
	proxyCalls := 0
	var proxyMethods []string
	var proxyURI, proxyAuth, proxyBody string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		proxyMethods = append(proxyMethods, r.Method+" "+r.RequestURI)
		// The in-process fingerprint transport establishes a CONNECT tunnel even for
		// an http:// target. That is proxy control traffic, not a duplicated upstream
		// request; count only the application request in the delivery assertion.
		if r.Method != http.MethodConnect {
			proxyCalls++
			proxyURI = r.RequestURI
			proxyAuth = r.Header.Get("Authorization")
			proxyBody = string(raw)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, customClaudeRelayResponse)
	}))
	defer proxy.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("provider test bypassed configured egress and reached default upstream: %s %s", r.Method, r.URL)
		http.Error(w, "wrong egress", http.StatusInternalServerError)
	})
	if err := h.store.UpsertEgressProfile(t.Context(), storage.EgressProfile{
		ID: "admin-test-provider-egress", Name: "Admin Test Provider Egress",
		Type: "http_proxy", Endpoint: proxy.URL, Health: "healthy",
		StreamCapable: true, MaxConcurrency: 10,
	}); err != nil {
		t.Fatal(err)
	}
	provider := storage.CustomProvider{
		ID: "admin-test-egress-relay", Name: "Admin Test Egress Relay",
		BaseURL:          "http://relay.invalid/v1",
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		EgressIDs:        []string{"admin-test-provider-egress"},
		Enabled:          true, Models: []string{"claude-sonnet-5"},
	}
	if err := h.store.UpsertCustomProvider(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: "admin-test-egress-account", Label: "egress account",
		GroupName: h.app.cfg.DefaultGroup, Provider: provider.ID, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "admin-test-egress-key"}); err != nil {
		t.Fatal(err)
	}

	resp, raw := postJSONForTest(t, h.pool.URL+"/admin/providers/"+provider.ID+"/test", map[string]interface{}{
		"model": "claude-sonnet-5",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("provider egress test status=%d body=%s", resp.StatusCode, raw)
	}
	var result customProviderModelTestResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("provider egress test result=%+v", result)
	}
	mu.Lock()
	gotCalls, gotMethods, gotURI, gotAuth, gotBody := proxyCalls, append([]string(nil), proxyMethods...), proxyURI, proxyAuth, proxyBody
	mu.Unlock()
	connectedToRelay := false
	for _, method := range gotMethods {
		if method == "CONNECT relay.invalid:80" {
			connectedToRelay = true
			break
		}
	}
	if gotCalls != 1 || !connectedToRelay || !strings.Contains(gotURI, "/v1/messages?beta=true") ||
		gotAuth != "Bearer admin-test-egress-key" ||
		!strings.Contains(gotBody, `"model":"claude-sonnet-5"`) {
		t.Fatalf("configured egress evidence calls=%d methods=%v uri=%q auth=%q body=%s", gotCalls, gotMethods, gotURI, gotAuth, gotBody)
	}
}
