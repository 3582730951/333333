package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// A user group pinned to the built-in claude provider makes claudeRelayTarget true from
// userGroupProvider alone, regardless of the requested model's spelling. A GPT model on
// /v1/responses therefore enters the Claude relay and is rejected by the model
// capability check.
//
// The status code is the point. Before the relay branch existed this returned 503
// server_error "Please retry.", which a client reasonably retries forever even though a
// claude-pinned group can never serve gpt-5.6-sol. It must stay a non-retryable 4xx that
// names the real problem.
func TestClaudePinnedGroupRejectsGPTModelAsNonRetryable(t *testing.T) {
	var upstreamPaths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_guard","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
	})
	seedClaudeResponsesAccount(t, h, "route-guard-claude")

	const poolKey = "route-guard-claude-key"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID:      "route-guard-claude-group",
		Name:    "route guard claude group",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindModelProvider, ID: "claude"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(poolKey), Label: "route guard claude group key",
		GroupName: "cyber", UserGroupID: "route-guard-claude-group",
		ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"probe"}]}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+poolKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("regressed to a retryable 503 for a permanently unserviceable pairing: %s", raw)
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("status = %d, want a non-retryable 4xx: %s", resp.StatusCode, raw)
	}
	if len(upstreamPaths) != 0 {
		t.Fatalf("an unserviceable pairing reached the upstream: %v", upstreamPaths)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("error body is not JSON: %v\n%s", err, raw)
	}
	errObj, ok := got["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("no error object: %s", raw)
	}
	if code, _ := errObj["code"].(string); code != "model_fallback_required" {
		t.Fatalf("error.code = %q, want model_fallback_required: %s", code, raw)
	}
	if requested, _ := errObj["requested_model"].(string); requested != "gpt-5.6-sol" {
		t.Fatalf("error did not name the requested model: %s", raw)
	}
}

// The same group serving its own provider's model must still work, proving the guard
// above rejects only the unserviceable pairing rather than disabling the group.
func TestClaudePinnedGroupServesClaudeModelOnResponses(t *testing.T) {
	var upstreamPaths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"pinned ok"}],"stop_reason":"end_turn","usage":{"input_tokens":6,"output_tokens":2}}`)
	})
	seedClaudeResponsesAccount(t, h, "route-guard-ok")

	const poolKey = "route-guard-ok-key"
	if err := h.store.CreateUserGroupDefinition(t.Context(), storage.UserGroup{
		ID:      "route-guard-ok-group",
		Name:    "route guard ok group",
		Targets: []storage.TargetRef{{Kind: storage.TargetKindModelProvider, ID: "claude"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(t.Context(), storage.APIKey{
		KeyHash: hashAPIKey(poolKey), Label: "route guard ok group key",
		GroupName: "cyber", UserGroupID: "route-guard-ok-group",
		ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	body := `{"model":"claude-opus-4-8","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"probe"}]}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+poolKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claude-pinned group could not serve its own model: status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	if got["object"] != "response" {
		t.Fatalf("missing Responses envelope: %s", raw)
	}
	if len(upstreamPaths) == 0 || upstreamPaths[0] != "/v1/messages" {
		t.Fatalf("expected an Anthropic Messages upstream call, got %v", upstreamPaths)
	}
}
