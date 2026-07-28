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

func TestClaudeNativeMessagesAutoUsesGroupCapabilityProviderUnion(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	request := httptest.NewRequest("POST", "/v1/messages", nil)

	providers, mode, err := h.app.resolveClaudeMessageProviders(ctx, request, downstreamPolicy{Group: "cyber", ProviderHint: "auto"})
	if err != nil || mode != "auto" {
		t.Fatalf("native auto route providers=%v mode=%q err=%v", providers, mode, err)
	}
	want := []string{"claude", "kiro", "antigravity"}
	if len(providers) != len(want) {
		t.Fatalf("native auto providers=%v, want %v", providers, want)
	}
	for index := range want {
		if providers[index] != want[index] {
			t.Fatalf("native auto providers=%v, want %v", providers, want)
		}
	}

	request.Header.Set("X-Pool-Provider", "antigravity")
	providers, mode, err = h.app.resolveClaudeMessageProviders(ctx, request, downstreamPolicy{Group: "cyber", ProviderHint: "auto"})
	if err != nil || mode != "antigravity" || len(providers) != 1 || providers[0] != "antigravity" {
		t.Fatalf("explicit native Antigravity providers=%v mode=%q err=%v", providers, mode, err)
	}
	if _, _, err = h.app.resolveClaudeProviders(ctx, request, downstreamPolicy{Group: "cyber", ProviderHint: "auto"}); err == nil {
		t.Fatal("OpenAI Chat Claude bridge accepted Antigravity without a response bridge")
	}
}

func TestClaudeRouteModelsEquivalentForAffinity(t *testing.T) {
	if !claudeRouteModelsEquivalent("claude-opus-4.6", "claude-opus-4-6") {
		t.Fatal("equivalent Kiro Claude spellings split affinity")
	}
	if !claudeRouteModelsEquivalent("gemini-3-flash", "GEMINI-3-FLASH") {
		t.Fatal("exact non-Kiro model spelling split affinity")
	}
	if claudeRouteModelsEquivalent("claude-opus-4-6", "gemini-3-flash") {
		t.Fatal("different requested models reused immutable Kiro affinity")
	}
}
