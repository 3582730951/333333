package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminProvidersRejectsInvalidBaseURL(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	cases := []string{
		`{"id":"bad","name":"Bad"}`,
		`{"id":"bad","name":"Bad","base_url":"not-a-url"}`,
		`{"id":"bad","name":"Bad","base_url":"ftp://example.com/v1"}`,
		`{"id":"bad","name":"Bad","base_url":"https://user:pass@example.com/v1"}`,
		`{"id":"bad","name":"Bad","base_url":"https://example.com/v1?tenant=unexpected"}`,
		`{"id":"bad","name":"Bad","base_url":"https://example.com/v1#fragment"}`,
	}
	for _, body := range cases {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", body)
		if code != http.StatusBadRequest {
			t.Fatalf("POST /admin/providers %s = %d, want 400: %s", body, code, raw)
		}
	}

	for _, id := range []string{"bad"} {
		if _, ok, err := h.store.GetCustomProvider(context.Background(), id); err != nil {
			t.Fatal(err)
		} else if ok {
			t.Fatalf("invalid provider %q was persisted", id)
		}
	}
}

func TestAdminProvidersRejectsCaseFoldedMappingConflicts(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"mapping-conflict",
		"name":"Mapping Conflict",
		"base_url":"https://relay.example/v1",
		"model_mappings":{
			"Claude-Sonnet-5":"relay-a",
			"claude-sonnet-5":"relay-b"
		}
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("case-folded conflicting mapping status=%d, want 400: %s", code, raw)
	}
	if _, ok, err := h.store.GetCustomProvider(t.Context(), "mapping-conflict"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("provider with ambiguous model mapping was persisted")
	}
}

func TestAdminProvidersRejectsAllBuiltInProviderIDs(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, id := range []string{"codex", "claude", "kiro", "antigravity"} {
		code, raw := grpReq(t, h, http.MethodPost, "/admin/providers",
			`{"id":"`+id+`","base_url":"https://relay.example/v1"}`)
		if code != http.StatusBadRequest {
			t.Fatalf("built-in provider id %q status=%d body=%s", id, code, raw)
		}
	}
}

func TestAdminProvidersUpsertReturnsStoredProvider(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"Deep Seek",
		"name":"DeepSeek",
		"base_url":" https://api.deepseek.com/v1 ",
		"upstream_protocol":"responses",
		"enabled":false,
		"auto_discover_models":false,
		"models":["deepseek-chat","deepseek-chat","deepseek-reasoner",""]
	}`)
	if code != http.StatusOK {
		t.Fatalf("POST /admin/providers = %d: %s", code, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode provider: %v (%s)", err, raw)
	}
	if got["id"] != "deep-seek" || got["name"] != "DeepSeek" || got["base_url"] != "https://api.deepseek.com/v1" {
		t.Fatalf("provider identity not normalized: %#v", got)
	}
	if got["upstream_protocol"] != "responses" {
		t.Fatalf("upstream_protocol not persisted: %#v", got)
	}
	if got["enabled"] != false || got["auto_discover_models"] != false {
		t.Fatalf("provider switches not persisted: %#v", got)
	}
	models, ok := got["models"].([]interface{})
	if !ok || len(models) != 2 || models[0] != "deepseek-chat" || models[1] != "deepseek-reasoner" {
		t.Fatalf("models not normalized: %#v", got["models"])
	}
}

func TestAdminProvidersDefaultsToChatCompletionsProtocol(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"kimi",
		"name":"Kimi",
		"base_url":"https://api.moonshot.cn/v1",
		"models":["moonshot-v1-8k"]
	}`)
	if code != http.StatusOK {
		t.Fatalf("POST /admin/providers = %d: %s", code, raw)
	}
	var got storage.CustomProvider
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode provider: %v (%s)", err, raw)
	}
	if got.UpstreamProtocol != "chat_completions" {
		t.Fatalf("default upstream protocol = %q, want chat_completions", got.UpstreamProtocol)
	}
}

func TestAdminProvidersAcceptsAnthropicMessagesProtocol(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"claude-relay",
		"name":"Claude Relay",
		"base_url":"https://relay.example/v1",
		"upstream_protocol":"anthropic_messages",
		"auto_discover_models":true
	}`)
	if code != http.StatusOK {
		t.Fatalf("POST anthropic provider = %d: %s", code, raw)
	}
	var got storage.CustomProvider
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UpstreamProtocol != storage.CustomProviderProtocolAnthropicMessages {
		t.Fatalf("protocol = %q", got.UpstreamProtocol)
	}
}

func TestAdminProvidersPersistsMultipleInvocationRoutes(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"multi-path-relay",
		"name":"Multi Path Relay",
		"base_url":"https://default.example/v1",
		"routes":[
			{"downstream_path":"/v1/chat/completions","base_url":"https://chat.example/v1","upstream_protocol":"chat_completions","transport_profile":"generic"},
			{"id":"codex-edge","downstream_path":"responses","base_url":"https://responses.example/openai/v1","upstream_protocol":"responses","transport_profile":"codex_cli"}
		]
	}`)
	if code != http.StatusOK {
		t.Fatalf("create multi-route provider = %d: %s", code, raw)
	}
	var got storage.CustomProvider
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 2 || got.Routes[0].ID != "chat" ||
		got.Routes[1].ID != "codex-edge" ||
		got.Routes[1].DownstreamPath != storage.CustomProviderDownstreamResponses {
		t.Fatalf("stored routes = %+v", got.Routes)
	}

	for _, body := range []string{
		`{"id":"bad-route-url","base_url":"https://default.example/v1","routes":[{"downstream_path":"/v1/responses","base_url":"file:///tmp/socket"}]}`,
		`{"id":"duplicate-route","base_url":"https://default.example/v1","routes":[{"downstream_path":"responses"},{"downstream_path":"/v1/responses"}]}`,
	} {
		code, raw = grpReq(t, h, http.MethodPost, "/admin/providers", body)
		if code != http.StatusBadRequest {
			t.Fatalf("invalid route status=%d body=%s request=%s", code, raw, body)
		}
	}
}

func TestAdminProvidersPartialPatchPreservesProtocolAndRoutingFields(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	upsertTestEgressProfile(t, h, "provider-patch-egress")
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"provider-patch",
		"name":"Provider Patch",
		"base_url":"https://relay.example/v1",
		"upstream_protocol":"anthropic_messages",
		"transport_profile":"claude_code",
		"egress_ids":["provider-patch-egress"],
		"models":["claude-test"]
	}`)
	if code != http.StatusOK {
		t.Fatalf("create provider = %d: %s", code, raw)
	}
	code, raw = grpReq(t, h, http.MethodPatch, "/admin/providers", `{"id":"provider-patch","enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("patch provider = %d: %s", code, raw)
	}
	var got storage.CustomProvider
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.UpstreamProtocol != storage.CustomProviderProtocolAnthropicMessages || got.TransportProfile != storage.CustomProviderTransportClaudeCode {
		t.Fatalf("partial patch changed provider protocol/profile: %+v", got)
	}
	if len(got.EgressIDs) != 1 || got.EgressIDs[0] != "provider-patch-egress" || len(got.Models) != 1 || got.Models[0] != "claude-test" {
		t.Fatalf("partial patch changed provider routing fields: %+v", got)
	}
}

// TestAdminProvidersInfersClaudeCodeFromProtocol covers the real-world Claude-Code-relay
// case: the operator selects upstream_protocol=anthropic_messages but the provider's
// id/name carries no "claude-code" hint, so name inference alone left it on the generic
// transport and it never presented the Claude Code client shape upstream.
func TestAdminProvidersInfersClaudeCodeFromProtocol(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"duckcoding",
		"name":"DuckCoding",
		"base_url":"https://relay.example/v1",
		"upstream_protocol":"anthropic_messages"
	}`)
	if code != http.StatusOK {
		t.Fatalf("create provider = %d: %s", code, raw)
	}
	var got storage.CustomProvider
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.TransportProfile != storage.CustomProviderTransportClaudeCode {
		t.Fatalf("anthropic_messages provider did not get the claude_code transport: %+v", got)
	}

	// An explicit transport_profile always wins over the inference.
	code, raw = grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"explicit-generic",
		"base_url":"https://relay.example/v1",
		"upstream_protocol":"anthropic_messages",
		"transport_profile":"generic"
	}`)
	if code != http.StatusOK {
		t.Fatalf("create provider = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.TransportProfile != storage.CustomProviderTransportGeneric {
		t.Fatalf("explicit transport_profile was overridden: %+v", got)
	}

	// Non-Anthropic protocols keep their existing inference.
	code, raw = grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"deepseek",
		"base_url":"https://api.deepseek.example/v1"
	}`)
	if code != http.StatusOK {
		t.Fatalf("create provider = %d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.TransportProfile != storage.CustomProviderTransportGeneric {
		t.Fatalf("chat_completions provider changed transport: %+v", got)
	}
}

func TestAdminProvidersRejectsInvalidProtocol(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})

	code, raw := grpReq(t, h, http.MethodPost, "/admin/providers", `{
		"id":"bad-proto",
		"name":"Bad Protocol",
		"base_url":"https://example.com/v1",
		"upstream_protocol":"legacy_responses"
	}`)
	if code != http.StatusBadRequest {
		t.Fatalf("POST invalid protocol = %d, want 400: %s", code, raw)
	}
}
