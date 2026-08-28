package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

func seedClaudeResponsesAccount(t *testing.T, h *testHarness, id string) {
	t.Helper()
	ctx := context.Background()
	account := storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "claude", PlanType: "Pro", Status: "active"}
	token := storage.AccountToken{AccountID: id, AuthMethod: accountprovider.AuthMethodOAuth, AccessToken: "credential-" + id, RefreshToken: "refresh-" + id}
	if err := h.store.UpsertAccount(ctx, account, token); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: id, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: id, ModelSlug: "claude-opus-4-8", AvailabilityState: capability.AvailabilityVerified,
		Context1MState: capability.Context1MSupported, Context1MSource: "test", NativeContextWindow: 200000,
		NativeMaxContextWindow: 1000000, EffectiveContextWindowPercent: 100, Source: "claude_probe",
	}}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
}

// Codex CLI speaks the Responses protocol. A Claude model requested through
// /v1/responses must therefore be bridged to Anthropic Messages, the same way
// /v1/chat/completions already is by handleChatViaClaude. This records the current
// behavior of that entrypoint so the gap is explicit rather than a silent misroute.
func TestResponsesEntrypointWithClaudeModel(t *testing.T) {
	var upstreamPaths []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		// An Anthropic Messages upstream reply, which is what a Claude account serves.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_bridge","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"claude via responses"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`)
	})
	seedClaudeResponsesAccount(t, h, "responses-claude-bridge")

	body := `{"model":"claude-opus-4-8","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"reply"}]}]}`
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	t.Logf("status=%d", resp.StatusCode)
	t.Logf("body=%s", raw)
	t.Logf("upstream paths=%v", upstreamPaths)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Claude model via /v1/responses failed: status=%d body=%s", resp.StatusCode, raw)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, raw)
	}
	// Codex CLI requires the Responses envelope.
	if got["object"] != "response" && got["type"] != "response" {
		t.Fatalf("downstream did not receive a Responses envelope: %s", raw)
	}
}
