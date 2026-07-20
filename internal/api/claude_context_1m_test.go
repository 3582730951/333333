package api

import (
	"bytes"
	"context"
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

func TestClaudeProOpus48StandardSucceedsButOneMillionIsVisibleRejection(t *testing.T) {
	var upstreamCalls atomic.Int64
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
			upstreamCalls.Add(1)
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
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"claude_context_1m_unavailable"`)) || !bytes.Contains(body, []byte(`"fallback_model":"claude-opus-4-8"`)) || !bytes.Contains(body, []byte(`"fallback_command":"/model opus"`)) || !bytes.Contains(body, []byte(`"manual_switch_required":true`)) {
		t.Fatalf("1M rejection status=%d body=%s", status, body)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("rejected Pro 1M request reached upstream; calls=%d", upstreamCalls.Load())
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
