package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

func seedClaudeContextAccount(t *testing.T, h *testHarness, id, plan, contextState, authMethod string) {
	t.Helper()
	ctx := context.Background()
	account := storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "claude", PlanType: plan, Status: "active"}
	token := storage.AccountToken{AccountID: id, AuthMethod: authMethod, AccessToken: "credential-" + id}
	if authMethod == accountprovider.AuthMethodOAuth {
		token.RefreshToken = "refresh-" + id
	}
	if err := h.store.UpsertAccount(ctx, account, token); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: id, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: id, ModelSlug: "claude-opus-4-8", AvailabilityState: capability.AvailabilityVerified,
		Context1MState: contextState, Context1MSource: "test", NativeContextWindow: 200000,
		NativeMaxContextWindow: 1000000, EffectiveContextWindowPercent: 100, Source: "claude_probe",
	}}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
}

func sendClaudeContextRequest(t *testing.T, h *testHarness, beta string) (int, []byte) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	if beta != "" {
		request.Header.Set("Anthropic-Beta", beta)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response.StatusCode, body
}

func TestClaudeProOpus48UsesVirtualOneMillionAndCompactsBeforeNativeWindow(t *testing.T) {
	var upstreamCalls atomic.Int64
	var lastBeta atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			upstreamCalls.Add(1)
			lastBeta.Store(r.Header.Get("Anthropic-Beta"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
			return
		}
		http.NotFound(w, r)
	})
	seedClaudeContextAccount(t, h, "claude-pro", "Pro", capability.Context1MUnsupported, accountprovider.AuthMethodOAuth)

	status, body := sendClaudeContextRequest(t, h, "")
	if status != http.StatusOK {
		t.Fatalf("standard Opus 4.8 status=%d body=%s", status, body)
	}
	caps, err := h.store.ListCapabilities(context.Background(), "claude-pro")
	if err != nil || len(caps) != 1 || !strings.Contains(caps[0].Source, "claude_probe") || !strings.Contains(caps[0].Source, "claude_runtime_inference") {
		t.Fatalf("runtime verification lost Claude provider source: caps=%+v err=%v", caps, err)
	}
	status, body = sendClaudeContextRequest(t, h, anthropicContext1MBeta)
	if status != http.StatusOK {
		t.Fatalf("virtual 1M status=%d body=%s", status, body)
	}
	if got, _ := lastBeta.Load().(string); strings.Contains(strings.ToLower(got), anthropicContext1MBeta) {
		t.Fatalf("virtual 1M beta leaked to a 200K account: %q", got)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("standard + virtual 1M calls=%d, want 2", upstreamCalls.Load())
	}

	large := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"` + strings.Repeat("x", 570000) + `"}]}`
	caps, err = h.store.ListCapabilities(context.Background(), "claude-pro")
	if err != nil || len(caps) != 1 {
		t.Fatalf("load selected Pro capability before guard: caps=%+v err=%v", caps, err)
	}
	if directPlan, directGuard := buildClaudeAutoCompactPlan([]byte(large), "claude-opus-4-8[1m]", "claude-opus-4-8", "claude", caps[0], true, true); !directGuard || directPlan.NativeWindow != 200_000 || directPlan.EffectiveLimit != 167_000 {
		t.Fatalf("direct selected Pro guard = %+v active=%v caps=%+v", directPlan, directGuard, caps)
	}
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(large))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", anthropicContext1MBeta)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	guardBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest ||
		response.Header.Get("X-MiCliProxy-Auto-Compact") != "claude_code_reactive" ||
		response.Header.Get("X-MiCliProxy-Context-Mode") != "virtual_1m" ||
		response.Header.Get("X-MiCliProxy-Context-Policy") != "claude_code_standard_window" ||
		!bytes.Contains(guardBody, []byte("Prompt is too long:")) ||
		!bytes.Contains(guardBody, []byte("Claude Code should automatically compact")) {
		t.Fatalf("guard status=%d headers=%v body=%s", response.StatusCode, response.Header, guardBody)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("oversized virtual request reached the 200K upstream; calls=%d", upstreamCalls.Load())
	}

	// The client's dedicated summary request is allowed through the selected
	// provider/account trigger
	// while it remains below the real 200K boundary; otherwise the guard would
	// recursively prevent Claude Code from completing the requested compaction.
	compact := `{"model":"claude-opus-4-8","max_tokens":8,"system":"You are a helpful AI assistant tasked with summarizing conversations.","messages":[{"role":"user","content":"` + strings.Repeat("x", 570000) + `"}]}`
	compactRequest, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(compact))
	compactRequest.Header.Set("Content-Type", "application/json")
	compactRequest.Header.Set("Anthropic-Beta", anthropicContext1MBeta)
	compactResponse, err := http.DefaultClient.Do(compactRequest)
	if err != nil {
		t.Fatal(err)
	}
	compactBody, _ := io.ReadAll(compactResponse.Body)
	compactResponse.Body.Close()
	if compactResponse.StatusCode != http.StatusOK || upstreamCalls.Load() != 3 {
		t.Fatalf("native compaction was recursively blocked: status=%d calls=%d body=%s", compactResponse.StatusCode, upstreamCalls.Load(), compactBody)
	}
}

func TestClaudeOpus5UsesOneMillionByDefaultWithoutLegacyBeta(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte(`"model":"claude-opus-5"`)) {
			t.Errorf("Opus 5 request model changed: %s", raw)
		}
		if strings.Contains(strings.ToLower(r.Header.Get("Anthropic-Beta")), anthropicContext1MBeta) {
			t.Errorf("Opus 5 request synthesized obsolete context beta: %q", r.Header.Get("Anthropic-Beta"))
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-opus5","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	account := storage.Account{
		ID: "claude-opus5-pro", Label: "claude-opus5-pro", GroupName: "cyber",
		Provider: "claude", PlanType: "Pro", Status: "active",
	}
	token := storage.AccountToken{
		AccountID: account.ID, AuthMethod: accountprovider.AuthMethodOAuth,
		AccessToken: "credential-opus5", RefreshToken: "refresh-opus5",
	}
	if err := h.store.UpsertAccount(t.Context(), account, token); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(t.Context(), storage.AccountEgressBinding{
		AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: account.ID, ModelSlug: "claude-opus-5",
		AvailabilityState:             capability.AvailabilityVerified,
		Context1MState:                capability.Context1MSupported,
		Context1MSource:               "model_default",
		NativeContextWindow:           1_000_000,
		NativeMaxContextWindow:        1_000_000,
		EffectiveContextWindowPercent: 100,
		Source:                        "claude_probe",
	}}); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()

	for _, model := range []string{"claude-opus-5", "claude-opus-5[1m]"} {
		request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(
			`{"model":"`+model+`","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
		))
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", model, response.StatusCode, body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("Opus 5 upstream calls=%d, want 2", calls.Load())
	}
}

func TestClaudeAPIKeyOneMillionCapabilityRoutesWithAPIKeyAuth(t *testing.T) {
	var gotBeta, gotAPIKey, gotAuthorization string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		gotBeta = r.Header.Get("Anthropic-Beta")
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	seedClaudeContextAccount(t, h, "claude-api", "api", capability.Context1MSupported, accountprovider.AuthMethodAPIKey)
	status, body := sendClaudeContextRequest(t, h, anthropicContext1MBeta)
	if status != http.StatusOK {
		t.Fatalf("API-key 1M status=%d body=%s", status, body)
	}
	if gotAPIKey != "credential-claude-api" || gotAuthorization != "" || !strings.Contains(strings.ToLower(gotBeta), anthropicContext1MBeta) || strings.Contains(strings.ToLower(gotBeta), "oauth") {
		t.Fatalf("API-key 1M headers x-api-key=%q authorization=%q beta=%q", gotAPIKey, gotAuthorization, gotBeta)
	}
}

func TestClaudeMaxOneMillionCapabilityRoutesAndPreservesBeta(t *testing.T) {
	var gotBeta string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			gotBeta = r.Header.Get("Anthropic-Beta")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
			return
		}
		http.NotFound(w, r)
	})
	seedClaudeContextAccount(t, h, "claude-max", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	status, body := sendClaudeContextRequest(t, h, anthropicContext1MBeta)
	if status != http.StatusOK {
		t.Fatalf("Max 1M status=%d body=%s", status, body)
	}
	if !strings.Contains(strings.ToLower(gotBeta), anthropicContext1MBeta) {
		t.Fatalf("1M beta was removed before upstream: %q", gotBeta)
	}
}

func TestClaudeNativeOneMillionRelaysUpstreamContextErrorForClientCompaction(t *testing.T) {
	const upstreamMessage = "Prompt is too long: 1000001 tokens > 1000000"
	var upstreamCalls atomic.Int64
	var gotBeta atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		upstreamCalls.Add(1)
		gotBeta.Store(r.Header.Get("Anthropic-Beta"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"`+upstreamMessage+`"}}`)
	})
	seedClaudeContextAccount(t, h, "claude-native-1m", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)

	// This body is deliberately above the relay's conservative one-token-per-three-
	// ASCII estimate for a 1M window. A verified native-1M selection must still reach
	// upstream: the upstream error is the signal Claude Code consumes to compact and
	// retry, so intercepting it locally would create a second compaction authority.
	raw := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"` + strings.Repeat("x", 3_100_000) + `"}]}`
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", anthropicContext1MBeta)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()

	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode relayed upstream error: %v body=%s", err, body)
	}
	if response.StatusCode != http.StatusBadRequest || envelope.Type != "error" ||
		envelope.Error.Type != "invalid_request_error" ||
		envelope.Error.Code != "context_length_exceeded" ||
		envelope.Error.Message != upstreamMessage {
		t.Fatalf("upstream context error was not relayed intact: status=%d envelope=%+v body=%s", response.StatusCode, envelope, body)
	}
	if response.Header.Get("X-MiCliProxy-Auto-Compact") != "" {
		t.Fatalf("native 1M response was replaced by relay compaction guard: headers=%v", response.Header)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("native 1M upstream calls=%d, want exactly 1", upstreamCalls.Load())
	}
	if beta, _ := gotBeta.Load().(string); !strings.Contains(strings.ToLower(beta), anthropicContext1MBeta) {
		t.Fatalf("native 1M beta was removed before upstream: %q", beta)
	}
}

func TestExactClaudeModelRetriesAnotherAccountWithoutQuarantiningFirst(t *testing.T) {
	var firstAuth string
	seen := map[string]bool{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		seen[auth] = true
		w.Header().Set("Content-Type", "application/json")
		if firstAuth == "" {
			firstAuth = auth
		}
		if auth == firstAuth {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","code":"model_not_found","message":"model not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	seedClaudeContextAccount(t, h, "claude-a", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	seedClaudeContextAccount(t, h, "claude-b", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)

	status, body := sendClaudeContextRequest(t, h, "")
	if status != http.StatusOK || len(seen) != 2 {
		t.Fatalf("exact model did not retry another account: status=%d auth=%v body=%s", status, seen, body)
	}
	firstID := strings.TrimPrefix(strings.TrimPrefix(firstAuth, "Bearer "), "credential-")
	account, err := h.store.GetAccount(context.Background(), firstID)
	if err != nil || account.QuarantineUntil != 0 || account.Status != "active" {
		t.Fatalf("model-specific failure quarantined account=%+v err=%v", account, err)
	}
	caps, err := h.store.ListCapabilities(context.Background(), firstID)
	if err != nil || len(caps) != 1 || caps[0].AvailabilityState != capability.AvailabilityUnsupported {
		t.Fatalf("model-specific unsupported evidence=%+v err=%v", caps, err)
	}
}

func TestExactClaudeModelExhaustionReturnsManualFallbackError(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"not_found_error","code":"model_not_found","message":"model not found"}}`)
	})
	seedClaudeContextAccount(t, h, "claude-missing-a", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	seedClaudeContextAccount(t, h, "claude-missing-b", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	status, body := sendClaudeContextRequest(t, h, "")
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"model_fallback_required"`)) || !bytes.Contains(body, []byte(`"manual_switch_required":true`)) {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), body)
	}
	if calls.Load() != 2 {
		t.Fatalf("model exhaustion calls=%d, want both accounts", calls.Load())
	}
}

func TestChatClaudeExactModelRetriesAnotherAccount(t *testing.T) {
	var firstAuth string
	seen := map[string]bool{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		seen[auth] = true
		w.Header().Set("Content-Type", "application/json")
		if firstAuth == "" {
			firstAuth = auth
		}
		if auth == firstAuth {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found","message":"model not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":1}}`)
	})
	seedClaudeContextAccount(t, h, "chat-a", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	seedClaudeContextAccount(t, h, "chat-b", "Max", capability.Context1MSupported, accountprovider.AuthMethodOAuth)
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(seen) != 2 || !bytes.Contains(body, []byte(`"choices"`)) {
		t.Fatalf("chat bridge did not retry another account: status=%d auth=%v body=%s", response.StatusCode, seen, body)
	}
}

func TestSuggestedCodexFallbackUsesNaturalVersionWithinQualityTier(t *testing.T) {
	caps := []storage.ModelCapability{
		{ModelSlug: "gpt-5.8-sol"},
		{ModelSlug: "gpt-5.9-sol"},
		{ModelSlug: "gpt-5.11-sol"},
		{ModelSlug: "gpt-5.10-terra"},
	}
	if got := suggestedCodexFallback("gpt-5.10-sol", caps); got != "gpt-5.9-sol" {
		t.Fatalf("fallback=%q, want highest lower natural version in sol tier", got)
	}
}
