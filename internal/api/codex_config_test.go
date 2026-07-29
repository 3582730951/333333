package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// TestCodexConfigScript verifies GET /file/{key} returns a bash script that
// configures the codex CLI against THIS pool: a custom model_provider whose
// base_url is the pool origin + /v1, wire_api=responses, and the caller's key as
// the bearer token. The model defaults to the key's force_model.
func TestCodexConfigScript(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	const plain = "cap_codextest"
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash:    hashAPIKey(plain),
		Label:      "codex",
		GroupName:  "cyber",
		ForceModel: "gpt-5.6-sol",
		Enabled:    true,
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(h.pool.URL + "/file/" + plain)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	for _, want := range []string{
		"#!/usr/bin/env bash",
		`model = "$MODEL"`,
		`model_provider = "$PROVIDER_ID"`,
		`wire_api = "responses"`,
		`supports_websockets = $supports_websockets`,
		`probe_codex_websocket`,
		`POOL_CODEX_WEBSOCKETS`,
		"experimental_bearer_token",
		`API_KEY='` + plain + `'`,
		`MODEL='gpt-5.6-sol'`,
		`model_context_window = 272000`,
		`model_auto_compact_token_limit = 272000`,
		"/v1\"", // base_url ends with /v1
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("codex config script missing %q\n---\n%s", want, s)
		}
	}
	// chat wire_api was removed upstream — never emit it.
	if strings.Contains(s, `wire_api = "chat"`) {
		t.Fatalf("script must not use the removed chat wire_api")
	}
}

// TestCodexConfigScriptUnknownKeyRequired confirms that, when downstream keys are
// required, an unknown key is refused with a clear message rather than a script
// that would 401 on first use.
func TestCodexConfigScriptUnknownKeyRequired(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	h.app.cfg.RequireDownstreamKey = true
	resp, err := http.Get(h.pool.URL + "/file/cap_does_not_exist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for unknown key when keys required", resp.StatusCode)
	}
}

// TestDescribeNoAccountGroupMismatch is the core of requirement #4: when the
// routed group holds no usable account but accounts DO exist in another group, the
// 503 must say so (the group-mismatch smoking gun) instead of the opaque
// "no active account available".
func TestDescribeNoAccountGroupMismatch(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	// A GPT/Codex account exists, but in group "other" — not the routed "cyber".
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "gpt-1", Label: "gpt-1", GroupName: "other", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	got := h.app.describeNoAccount(ctx, "cyber", "", "gpt-5.4", scheduler.ErrNoAccount)
	msg := got.Error()
	for _, want := range []string{"cyber", "other", "no available account"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("diagnostic missing %q: %s", want, msg)
		}
	}
}

// TestDescribeNoAccountPassThrough confirms a non-ErrNoAccount error is returned
// unchanged so HTTP status mapping (e.g. strict-sticky 409) is preserved.
func TestDescribeNoAccountPassThrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	other := errors.New("boom")
	if got := h.app.describeNoAccount(context.Background(), "cyber", "", "", other); got != other {
		t.Fatalf("non-ErrNoAccount error must pass through unchanged, got %v", got)
	}
	if got := h.app.describeNoAccount(context.Background(), "cyber", "", "", scheduler.ErrStrictUnavailable); !errors.Is(got, scheduler.ErrStrictUnavailable) {
		t.Fatalf("strict-unavailable must pass through, got %v", got)
	}
}

func TestDescribeNoAccountReportsAntigravityCapabilityCandidates(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: "antigravity-diagnostic", GroupName: "cyber", Provider: "antigravity", Status: "active",
	}, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	err := &scheduler.NoAccountError{
		Group: "cyber", AllowedProviders: []string{"claude", "kiro", "antigravity"}, Model: "gemini-unverified",
		Counters: scheduler.NoAccountCounters{ModelUnsupported: 1},
	}
	message := h.app.describeNoAccount(ctx, "cyber", "claude,kiro,antigravity", "gemini-unverified", err).Error()
	for _, want := range []string{"antigravity=1", "model_unsupported=1", "gemini-unverified"} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic missing %q: %s", want, message)
		}
	}
}
