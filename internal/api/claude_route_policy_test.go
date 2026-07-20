package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestClaudeAutoProviderUsesActiveAccountCountNotTemporaryAvailability(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	claude := storage.Account{
		ID: "claude-primary", Provider: "claude", GroupName: "cyber", Status: "active",
		QuarantineUntil: storage.Now() + 3600,
	}
	if err := h.store.UpsertAccount(ctx, claude, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	providers, mode, err := h.app.resolveClaudeProviders(ctx, request, downstreamPolicy{Group: "cyber", ProviderHint: "auto"})
	if err != nil || mode != "auto" || len(providers) != 1 || providers[0] != "claude" {
		t.Fatalf("active quarantined Claude route = %v mode=%q err=%v", providers, mode, err)
	}
	claude.Status = "disabled"
	if err := h.store.UpsertAccount(ctx, claude, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	providers, _, err = h.app.resolveClaudeProviders(ctx, request, downstreamPolicy{Group: "cyber", ProviderHint: "auto"})
	if err != nil || len(providers) != 1 || providers[0] != "kiro" {
		t.Fatalf("zero active Claude route = %v err=%v", providers, err)
	}
}

func TestExplicitKiroAndGroupsAreIndependent(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "claude-a", Provider: "claude", GroupName: "a", Status: "active"}, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	providers, _, err := h.app.resolveClaudeProviders(ctx, request, downstreamPolicy{Group: "b", ProviderHint: "auto"})
	if err != nil || providers[0] != "kiro" {
		t.Fatalf("group b route = %v err=%v", providers, err)
	}
	request.Header.Set("X-Pool-Provider", "kiro")
	providers, mode, err := h.app.resolveClaudeProviders(ctx, request, downstreamPolicy{Group: "a", ProviderHint: "auto"})
	if err != nil || mode != "kiro" || providers[0] != "kiro" {
		t.Fatalf("explicit Kiro route = %v mode=%q err=%v", providers, mode, err)
	}
}
