package upstream

import (
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// TestMergeBetasPrefersClientBetas locks in the fix for the downstream
// "Failed to parse JSON" / "empty or malformed response (HTTP 200)" errors: when
// the client sends its own Anthropic-Beta set we forward THAT, and must NOT force
// our canonical capability betas (token-efficient-tools, structured-outputs, ...)
// on top — those change the response wire shape and break the client's parser.
func TestMergeBetasPrefersClientBetas(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "fast-mode-2099-01-01,some-new-capability-2099")
	got := mergeBetas(claudeOAuthBetas, h, false)

	// The client's own betas are forwarded verbatim.
	for _, want := range []string{"fast-mode-2099-01-01", "some-new-capability-2099"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dropped client beta %q: %s", want, got)
		}
	}
	// Required transport markers are guaranteed.
	if !strings.Contains(got, "oauth-2025-04-20") {
		t.Fatalf("oauth marker missing on OAuth path: %s", got)
	}
	if !strings.Contains(got, "interleaved-thinking-2025-05-14") {
		t.Fatalf("interleaved-thinking marker missing: %s", got)
	}
	// Canonical capability betas the client did NOT request must NOT be injected —
	// this is what previously corrupted the response format.
	for _, forbidden := range []string{
		"token-efficient-tools-2026-03-28",
		"structured-outputs-2025-12-15",
		"redact-thinking-2026-02-12",
		"context-management-2025-06-27",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("forced canonical beta %q the client never requested: %s", forbidden, got)
		}
	}
	// No duplicates.
	if strings.Count(got, "interleaved-thinking-2025-05-14") != 1 {
		t.Fatalf("duplicate beta: %s", got)
	}
}

// TestMergeBetasFallsBackToCanonical: when the client sends no Anthropic-Beta
// (e.g. our own count_tokens / models probe), we present the canonical official
// Claude Code fingerprint so the request still looks first-party.
func TestMergeBetasFallsBackToCanonical(t *testing.T) {
	got := mergeBetas(claudeOAuthBetas, http.Header{}, false)
	for _, want := range []string{
		"claude-code-20250219",
		"oauth-2025-04-20",
		"token-efficient-tools-2026-03-28",
		"interleaved-thinking-2025-05-14",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("canonical fallback missing %q: %s", want, got)
		}
	}
}

func TestMergeBetasAPIKeyDropsOAuth(t *testing.T) {
	h := http.Header{}
	h.Add("Anthropic-Beta", "oauth-2025-04-20,x-extra-feature")
	got := mergeBetas(claudeAPIKeyBetas, h, true)
	if strings.Contains(got, "oauth") {
		t.Fatalf("api-key path must not carry an oauth beta: %s", got)
	}
	if !strings.Contains(got, "x-extra-feature") {
		t.Fatalf("dropped a non-oauth client beta on the api-key path: %s", got)
	}
}

// TestClaudePassthroughPreservesContentTypeAndBeta locks in the skills/Files-API
// passthrough contract: the client's own Content-Type (with the multipart boundary),
// Anthropic-Beta and Anthropic-Version ride upstream VERBATIM — we must not force the
// canonical messages-beta superset (which would make the Files upload be rejected) nor
// rewrite the multipart Content-Type to application/json (which would corrupt it).
// Account auth + the Claude Code identity fingerprint are still attached.
func TestClaudePassthroughPreservesContentTypeAndBeta(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill")

	in := http.Header{}
	in.Set("Content-Type", "multipart/form-data; boundary=----abc123XYZ")
	in.Set("Accept", "application/json")
	in.Set("Anthropic-Beta", "files-api-2025-04-14,skills-2025-10-02")
	in.Set("Anthropic-Version", "2099-09-09")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Headers:  in,
		Body:     []byte("------abc123XYZ\r\n"),
	}, id, false)

	if got := h.Get("Content-Type"); got != "multipart/form-data; boundary=----abc123XYZ" {
		t.Fatalf("Content-Type rewritten, got %q (multipart boundary must survive)", got)
	}
	// The client's exact beta set is forwarded; the canonical messages betas are NOT
	// injected on top (they would make the Files endpoint reject the request).
	if got := h.Get("Anthropic-Beta"); got != "files-api-2025-04-14,skills-2025-10-02" {
		t.Fatalf("Anthropic-Beta not forwarded verbatim, got %q", got)
	}
	for _, forbidden := range []string{"token-efficient-tools", "interleaved-thinking", "oauth-2025-04-20"} {
		if strings.Contains(h.Get("Anthropic-Beta"), forbidden) {
			t.Fatalf("passthrough injected canonical beta %q the client never sent: %s", forbidden, h.Get("Anthropic-Beta"))
		}
	}
	if got := h.Get("Anthropic-Version"); got != "2099-09-09" {
		t.Fatalf("Anthropic-Version not forwarded, got %q", got)
	}
	// Auth + identity attached (OAuth path).
	if got := h.Get("Authorization"); got != "Bearer sk-ant-oat-xyz" {
		t.Fatalf("Authorization = %q, want Bearer token", got)
	}
	if h.Get("X-App") != "cli" || h.Get("X-Stainless-OS") != id.StainlessOS {
		t.Fatalf("identity fingerprint missing: %+v", h)
	}
	if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "claude-cli/") {
		t.Fatalf("UA = %q, want claude-cli shape", ua)
	}
}

// TestClaudePassthroughAPIKeyPath: the api-key credential uses x-api-key (no Bearer),
// sets the browser-access header, and any oauth-* beta the client sent is stripped.
func TestClaudePassthroughAPIKeyPath(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill-api")

	in := http.Header{}
	in.Set("Anthropic-Beta", "files-api-2025-04-14,oauth-2025-04-20")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill-api"},
		Token:    storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-xyz"},
		Headers:  in,
	}, id, false)

	if h.Get("x-api-key") != "sk-ant-api03-xyz" {
		t.Fatalf("x-api-key not set on api-key path: %+v", h)
	}
	if h.Get("Authorization") != "" {
		t.Fatalf("api-key path must not send a Bearer Authorization")
	}
	if h.Get("Anthropic-Dangerous-Direct-Browser-Access") != "true" {
		t.Fatal("api-key path must set the browser-access header")
	}
	if strings.Contains(strings.ToLower(h.Get("Anthropic-Beta")), "oauth") {
		t.Fatalf("api-key path leaked an oauth beta: %q", h.Get("Anthropic-Beta"))
	}
	if !strings.Contains(h.Get("Anthropic-Beta"), "files-api-2025-04-14") {
		t.Fatalf("dropped the client's non-oauth beta: %q", h.Get("Anthropic-Beta"))
	}
}

// TestClaudePassthroughBetaFallback: a client that sent no Anthropic-Beta still gets a
// coherent first-party fingerprint (the canonical set), so an internal call isn't naked.
func TestClaudePassthroughBetaFallback(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-skill-bare")

	h := http.Header{}
	c.applyClaudePassthroughHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-skill-bare"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Headers:  http.Header{},
	}, id, false)

	if !strings.Contains(h.Get("Anthropic-Beta"), "claude-code-20250219") {
		t.Fatalf("bare passthrough missing canonical fallback beta: %q", h.Get("Anthropic-Beta"))
	}
}
