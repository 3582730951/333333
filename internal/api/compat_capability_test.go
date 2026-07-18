package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func decodeErrorBody(t *testing.T, r io.Reader) map[string]interface{} {
	t.Helper()
	var root map[string]interface{}
	if err := json.NewDecoder(r).Decode(&root); err != nil {
		t.Fatal(err)
	}
	errObj, ok := root["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing error object: %#v", root)
	}
	return errObj
}

func seedDownstreamKey(t *testing.T, h *testHarness, plain, hint string) {
	t.Helper()
	if err := h.store.UpsertAPIKey(context.Background(), storage.APIKey{
		KeyHash:      hashAPIKey(plain),
		Label:        hint,
		GroupName:    "cyber",
		ProviderHint: hint,
		Enabled:      true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCustomResponsesHostedToolIsOmittedWithDiagnostic(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	accountID := setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{
	  "model":"deepseek-chat",
	  "input":"search",
	  "tools":[{"type":"web_search_preview"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || root["object"] != "response" {
		t.Fatalf("status = %d, response=%#v", resp.StatusCode, root)
	}
	if got := resp.Header.Get(responsesCompatibilityLossesHeader); got != `["responses_hosted_tool_omitted"]` {
		t.Fatalf("compatibility header = %q", got)
	}
	forwarded := false
	for _, req := range h.requests() {
		if strings.HasSuffix(req.Path, "/chat/completions") {
			forwarded = true
			if strings.Contains(req.Body, "web_search") {
				t.Fatalf("hosted Responses tool leaked to Chat upstream: %+v", req)
			}
		}
	}
	if !forwarded {
		t.Fatal("request was not continued through the Chat bridge")
	}
	h.app.WaitForAsyncWrites()
	var recorded string
	if err := h.store.DB().QueryRow(`SELECT compatibility_losses_json FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, accountID).Scan(&recorded); err != nil {
		t.Fatalf("read compatibility losses: %v", err)
	}
	if recorded != `["responses_hosted_tool_omitted"]` {
		t.Fatalf("recorded compatibility losses = %q", recorded)
	}
}

func TestCustomMessagesTypedServerToolReturnsCapabilityUnavailable(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(`{
	  "model":"deepseek-chat",
	  "max_tokens":128,
	  "messages":[{"role":"user","content":"search"}],
	  "tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errObj := decodeErrorBody(t, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, error=%#v", resp.StatusCode, errObj)
	}
	if errObj["type"] != "capability_unavailable" {
		t.Fatalf("error type = %#v, want capability_unavailable", errObj["type"])
	}
	if errObj["required_tier"] != "official_claude" {
		t.Fatalf("required_tier = %#v", errObj["required_tier"])
	}
	for _, req := range h.requests() {
		if strings.HasSuffix(req.Path, "/chat/completions") {
			t.Fatalf("typed Claude server tool must not be sent to chat bridge: %+v", req)
		}
	}
}

func TestCustomMessagesClaudeBetaHeaderReturnsCapabilityUnavailable(t *testing.T) {
	h := newHarness(t, deepseekMock(t))
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{
	  "model":"deepseek-chat",
	  "max_tokens":128,
	  "messages":[{"role":"user","content":"hi"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Anthropic-Beta", "skills-2025-10-02")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errObj := decodeErrorBody(t, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, error=%#v", resp.StatusCode, errObj)
	}
	if errObj["type"] != "capability_unavailable" || errObj["required_tier"] != "official_claude" {
		t.Fatalf("wrong capability error: %#v", errObj)
	}
	if len(h.requests()) != 0 {
		t.Fatalf("Claude beta request must not be sent to chat bridge: %+v", h.requests())
	}

	doctor, err := http.Get(h.pool.URL + "/admin/compat/skills")
	if err != nil {
		t.Fatal(err)
	}
	defer doctor.Body.Close()
	var root map[string]interface{}
	if err := json.NewDecoder(doctor.Body).Decode(&root); err != nil {
		t.Fatal(err)
	}
	recent, ok := root["recent_incompatibilities"].([]interface{})
	if !ok || len(recent) == 0 {
		t.Fatalf("doctor did not expose recent incompatibility: %#v", root["recent_incompatibilities"])
	}
	last := recent[len(recent)-1].(map[string]interface{})
	if last["requested_capability"] != "anthropic_beta:skills-2025-10-02" {
		t.Fatalf("wrong recorded capability: %#v", last)
	}
	if last["chosen_route"] != "custom_chat_completions_bridge:deepseek" {
		t.Fatalf("wrong recorded route: %#v", last)
	}
	if !strings.Contains(last["fix_hint"].(string), "official Claude") {
		t.Fatalf("missing recorded fix hint: %#v", last)
	}
}
