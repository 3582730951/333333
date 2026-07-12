package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/capability"
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
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	if upstreamCalls != 0 {
		t.Fatalf("model-unsupported routing failure made %d upstream call(s)", upstreamCalls)
	}
	if bytes.Contains(body, []byte("model_unsupported")) || bytes.Contains(body, []byte("Kiro Free")) {
		t.Fatalf("public response leaked routing diagnostics: %s", body)
	}

	audit, err := h.store.ListAuditLog(ctx, 10)
	if err != nil || len(audit) == 0 {
		t.Fatalf("audit=%+v err=%v", audit, err)
	}
	row := audit[0]
	if row.Action != "routing_unavailable" || row.Reason != "model_unsupported" {
		t.Fatalf("routing audit=%+v", row)
	}
	for _, want := range []string{
		"allowed_providers=claude,kiro",
		"requested_model=claude-opus-4-8",
		"normalized_model=claude-opus-4.8",
		"kiro_accounts=1",
		`"model_unsupported":1`,
	} {
		if !strings.Contains(row.Detail, want) {
			t.Fatalf("routing audit missing %q: %s", want, row.Detail)
		}
	}
	if strings.Contains(row.Detail, "Point this API key's group") || strings.Contains(row.Detail, "None of the active accounts are Claude accounts") {
		t.Fatalf("routing audit gave a false group/provider diagnosis: %s", row.Detail)
	}
}
