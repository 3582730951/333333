package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestKiroModelUnsupportedAuditUsesActualAutoRouteDiagnostics(t *testing.T) {
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx := context.Background()
	kiroAccount := storage.Account{ID: "kiro-free", Label: "Kiro Free", GroupName: "cyber", Provider: "kiro", PlanType: "KIRO FREE", Status: "active"}
	if err := h.store.UpsertAccount(ctx, kiroAccount, storage.AccountToken{AccessToken: "kiro-token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, capability.StaticKiroModels(kiroAccount.ID)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: kiroAccount.ID, AuthMethod: "api_key", KiroAPIKey: "kiro-key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	// Keep an active account in another group to ensure model filtering is not
	// misdiagnosed as a group mismatch with an instruction to move the API key.
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "other-claude", Label: "other", GroupName: "other", Provider: "claude", Status: "active"}, storage.AccountToken{AccessToken: "claude-token"}); err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if upstreamCalls != 0 {
		t.Fatalf("model-unsupported routing failure made %d upstream call(s)", upstreamCalls)
	}
	if !bytes.Contains(body, []byte(`"code":"model_fallback_required"`)) || !bytes.Contains(body, []byte(`"fallback_command":"/model opus"`)) || !bytes.Contains(body, []byte(`"manual_switch_required":true`)) {
		t.Fatalf("public response omitted structured manual fallback: %s", body)
	}
	if bytes.Contains(body, []byte("model_unsupported")) || bytes.Contains(body, []byte("Kiro Free")) {
		t.Fatalf("public response leaked routing diagnostics: %s", body)
	}

	audit, err := h.store.ListAuditLog(ctx, 10)
	if err != nil || len(audit) == 0 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	foundFallback, foundCapability := false, false
	for _, row := range audit {
		if row.Action == "model_fallback_required" && row.Reason == "model_fallback_required" && strings.Contains(row.Detail, "requested_model=claude-opus-4-8") {
			foundFallback = true
		}
		if row.Action == "model_capability_rejected" && row.Reason == "model_fallback_required" {
			foundCapability = true
		}
	}
	if !foundFallback || foundCapability {
		t.Fatalf("fallback must have one terminal audit and no false upstream rejection: %+v", audit)
	}
}

func TestCapabilityFallbackProbeDoesNotPolluteTerminalAudit(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := withUserGroupFallbackProbe(context.Background())
	rec := httptest.NewRecorder()
	err := &scheduler.NoAccountError{
		Model: "claude-opus-4-8",
		Counters: scheduler.NoAccountCounters{
			ModelUnsupported: 1,
		},
	}
	if !h.app.handleCapabilitySelectionError(ctx, rec, err, true, "cyber", "claude,kiro", "claude-opus-4-8", "") {
		t.Fatal("fallback probe was not classified")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"model_fallback_required"`) {
		t.Fatalf("private probe response changed: %d %s", rec.Code, rec.Body.String())
	}
	rows, queryErr := h.store.ListAuditLog(context.Background(), 20)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	for _, row := range rows {
		if row.Action == "model_fallback_required" || row.Action == "model_capability_rejected" {
			t.Fatalf("speculative probe leaked into terminal audit: %+v", rows)
		}
	}
}

func TestExplicitKiroAliasWithoutRuntimeCapabilityReturnsStructuredFallback(t *testing.T) {
	var upstreamCalls int
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ctx := context.Background()
	account := storage.Account{ID: "kiro-alias", Label: "Kiro", GroupName: "cyber", Provider: "kiro", PlanType: "KIRO PRO", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "kiro-token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, capability.StaticKiroModels(account.ID)); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "kiro-key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		beta string
		code string
	}{
		{name: "standard", code: "model_fallback_required"},
		{name: "one-million", beta: anthropicContext1MBeta, code: "claude_context_1m_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"opus","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Pool-Provider", "kiro")
			if tc.beta != "" {
				request.Header.Set("Anthropic-Beta", tc.beta)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest || !bytes.Contains(body, []byte(`"code":"`+tc.code+`"`)) || !bytes.Contains(body, []byte(`"fallback_command":"/model opus"`)) || !bytes.Contains(body, []byte(`"manual_switch_required":true`)) {
				t.Fatalf("status=%d body=%s", response.StatusCode, body)
			}
		})
	}
	if upstreamCalls != 0 {
		t.Fatalf("unverified Kiro alias made %d upstream call(s)", upstreamCalls)
	}
}
