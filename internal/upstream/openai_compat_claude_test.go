package upstream

import (
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// TestClaudeShapedCustomCallDetection pins the detection that makes Claude-Code-mode
// relays work: the profile is inferred from the provider's NAME, so the protocol and the
// upstream path must be able to reach the Claude shape on their own.
func TestClaudeShapedCustomCallDetection(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Request
		want bool
	}{
		{"explicit claude_code profile", Request{TransportProfile: storage.CustomProviderTransportClaudeCode}, true},
		{"anthropic_messages protocol on generic profile", Request{
			TransportProfile: storage.CustomProviderTransportGeneric,
			UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		}, true},
		// The real-world case: a relay named "duckcoding"/"88code" lands on the generic
		// profile, and callCustomAttempt does not set UpstreamProtocol at all.
		{"messages path with no protocol hint", Request{
			TransportProfile: storage.CustomProviderTransportGeneric,
			DownstreamPath:   "/messages",
		}, true},
		{"messages path with query", Request{DownstreamPath: "/messages?beta=true"}, true},
		{"chat completions stays generic", Request{DownstreamPath: "/chat/completions"}, false},
		{"models probe stays generic", Request{DownstreamPath: "/models"}, false},
		{"codex_cli responses stays generic", Request{
			TransportProfile: storage.CustomProviderTransportCodexCLI,
			DownstreamPath:   "/responses",
		}, false},
		{"explicit chat_completions protocol overrides the path", Request{
			UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
			DownstreamPath:   "/messages",
		}, false},
	} {
		if got := claudeShapedCustomCall(tc.spec); got != tc.want {
			t.Errorf("%s: claudeShapedCustomCall = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func claudeCustomSpec(body string) Request {
	return Request{
		Provider:         "duckcoding",
		Account:          storage.Account{ID: "acc-custom", Provider: "duckcoding"},
		Token:            storage.AccountToken{OpenAIAPIKey: "relay-key-abc"},
		TransportProfile: storage.CustomProviderTransportGeneric,
		DownstreamPath:   "/messages",
		Headers:          http.Header{},
		Body:             testBody([]byte(body)),
	}
}

const claudeCustomBody = `{"model":"claude-opus-5","messages":[{"role":"user","content":"first"}]}`

// TestClaudeCodeCustomHeadersCarryClientShape asserts the markers a Claude-Code-mode
// relay gates on are all present. Before this, a generic-profile provider sent only
// Bearer + a generic SDK UA, which those relays reject.
func TestClaudeCodeCustomHeadersCarryClientShape(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-custom")

	built := http.Header{}
	c.applyClaudeCodeCustomHeaders(built, claudeCustomSpec(claudeCustomBody), id, true)

	for _, name := range []string{
		"X-App",
		"X-Stainless-Retry-Count",
		"X-Stainless-Lang",
		"X-Stainless-Runtime",
		"X-Stainless-Runtime-Version",
		"X-Stainless-Timeout",
		"X-Stainless-OS",
		"X-Stainless-Arch",
		"X-Stainless-Package-Version",
		"Anthropic-Version",
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"X-Claude-Code-Session-Id",
	} {
		if built.Get(name) == "" {
			t.Errorf("missing Claude Code marker %s: %#v", name, built)
		}
	}
	if got := built.Get("Accept-Language"); got != "" {
		t.Errorf("Accept-Language = %q, want absent as in Claude Code 2.1.226", got)
	}
	if got := built.Get("X-App"); got != "cli" {
		t.Errorf("X-App = %q, want cli", got)
	}
	if ua := built.Get("User-Agent"); !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("User-Agent = %q, want a claude-cli UA", ua)
	}
	if got := built.Get("Accept"); got != "application/json" {
		t.Errorf("streaming Accept = %q, want native Claude application/json", got)
	}
	// A relay-issued key must reach BOTH auth headers: relay auth conventions differ,
	// and the generic builder this replaces sent both. Dropping either regresses relays.
	if got := built.Get("Authorization"); got != "Bearer relay-key-abc" {
		t.Errorf("Authorization = %q, want Bearer relay-key-abc", got)
	}
	if got := built.Get("x-api-key"); got != "relay-key-abc" {
		t.Errorf("x-api-key = %q, want relay-key-abc", got)
	}
	assertNoRelayCharacteristics(t, "custom/claude-code", built)
}

// TestClaudeCodeCustomHeadersAuthPerCredentialShape: first-party Anthropic credentials
// must keep Anthropic's own single-header convention (it rejects both at once), while an
// arbitrary relay key gets both.
func TestClaudeCodeCustomHeadersAuthPerCredentialShape(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-custom")

	for _, tc := range []struct {
		name          string
		token         storage.AccountToken
		wantAuth      string
		wantAPIKey    string
		wantOAuthBeta bool
	}{
		{"relay key", storage.AccountToken{OpenAIAPIKey: "relay-key-abc"}, "Bearer relay-key-abc", "relay-key-abc", false},
		{"anthropic api key", storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-xyz"}, "", "sk-ant-api03-xyz", false},
		{"anthropic oauth", storage.AccountToken{AccessToken: "sk-ant-oat01-xyz"}, "Bearer sk-ant-oat01-xyz", "", true},
	} {
		spec := claudeCustomSpec(claudeCustomBody)
		spec.Token = tc.token
		built := http.Header{}
		c.applyClaudeCodeCustomHeaders(built, spec, id, false)

		if got := built.Get("Authorization"); got != tc.wantAuth {
			t.Errorf("%s: Authorization = %q, want %q", tc.name, got, tc.wantAuth)
		}
		if got := built.Get("x-api-key"); got != tc.wantAPIKey {
			t.Errorf("%s: x-api-key = %q, want %q", tc.name, got, tc.wantAPIKey)
		}
		// An API key can never present the OAuth permission marker.
		hasOAuth := strings.Contains(strings.ToLower(built.Get("Anthropic-Beta")), "oauth")
		if hasOAuth != tc.wantOAuthBeta {
			t.Errorf("%s: oauth beta present = %v, want %v (%q)", tc.name, hasOAuth, tc.wantOAuthBeta, built.Get("Anthropic-Beta"))
		}
	}
}

// TestClaudeCodeCustomHeadersKeepClientBetas: a capability beta changes the response
// wire shape, so the client's own set must survive rather than be replaced by ours.
func TestClaudeCodeCustomHeadersKeepClientBetas(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-custom")

	spec := claudeCustomSpec(claudeCustomBody)
	spec.Headers.Set("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")
	spec.Headers.Set("Anthropic-Version", "2023-06-01")
	built := http.Header{}
	c.applyClaudeCodeCustomHeaders(built, spec, id, true)

	if !strings.Contains(built.Get("Anthropic-Beta"), "fine-grained-tool-streaming-2025-05-14") {
		t.Errorf("client beta dropped: %q", built.Get("Anthropic-Beta"))
	}
	// With no client betas at all, the canonical set backfills so a minimal API client
	// still reaches a Claude-Code-gated relay.
	bare := http.Header{}
	c.applyClaudeCodeCustomHeaders(bare, claudeCustomSpec(claudeCustomBody), id, true)
	if !strings.Contains(bare.Get("Anthropic-Beta"), "claude-code-") {
		t.Errorf("canonical betas not backfilled: %q", bare.Get("Anthropic-Beta"))
	}
}

// TestCustomClaudeSessionIDSeparatesConversations is the session/context-bleeding
// assertion. When the pool holds ONE key and the downstream runs several concurrent
// conversations, every request used to reach the relay with no session id at all, so a
// relay keying conversation state by session saw a single session and interleaved them.
// Distinct conversations must get distinct ids; the same conversation must be stable
// across turns, or prompt caching and relay stickiness break.
func TestCustomClaudeSessionIDSeparatesConversations(t *testing.T) {
	_, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-custom")

	convA1 := customClaudeSessionID(claudeCustomSpec(`{"messages":[{"role":"user","content":"alpha"}]}`), id)
	convA2 := customClaudeSessionID(claudeCustomSpec(
		`{"messages":[{"role":"user","content":"alpha"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}]}`), id)
	convB := customClaudeSessionID(claudeCustomSpec(`{"messages":[{"role":"user","content":"beta"}]}`), id)

	if convA1 == "" || convB == "" {
		t.Fatal("session id must never be empty on a Claude-shaped custom call")
	}
	if convA1 != convA2 {
		t.Errorf("session id changed across turns of one conversation: %q vs %q", convA1, convA2)
	}
	if convA1 == convB {
		t.Errorf("two conversations collapsed onto one session id (%q) — this is the context-bleeding defect", convA1)
	}

	// An explicit downstream session header wins, and is replaced by a derived value so
	// the downstream's own identifier never reaches the upstream.
	withHeader := claudeCustomSpec(claudeCustomBody)
	withHeader.Headers.Set("X-Claude-Code-Session-Id", "downstream-session-7")
	derived := customClaudeSessionID(withHeader, id)
	if derived == "downstream-session-7" {
		t.Error("downstream session id was forwarded verbatim instead of derived")
	}
	if derived == convA1 {
		t.Error("explicit session header did not take precedence over the body anchor")
	}

	// Same account, different pool: derivation is account-bound, so one relay cannot
	// correlate sessions across accounts.
	otherAccount := customClaudeSessionID(claudeCustomSpec(claudeCustomBody), identity.For(secret, "acc-other"))
	if otherAccount == customClaudeSessionID(claudeCustomSpec(claudeCustomBody), id) {
		t.Error("session id is not account-bound")
	}
}

// TestCustomClaudeCookieJarKeySeparatesConversations covers the second path to the same
// bleeding: a relay session cookie stored in an account-wide jar pins every conversation
// on that account to one upstream session.
func TestCustomClaudeCookieJarKeySeparatesConversations(t *testing.T) {
	_, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-custom")

	specA := claudeCustomSpec(`{"messages":[{"role":"user","content":"alpha"}]}`)
	specA.CookieJarKey = "acc-custom:egress-1"
	specB := specA
	specB.Body = testBody([]byte(`{"messages":[{"role":"user","content":"beta"}]}`))

	keyA := customClaudeCookieJarKey(specA, id)
	keyB := customClaudeCookieJarKey(specB, id)
	if keyA == keyB {
		t.Errorf("two conversations share one cookie jar (%q)", keyA)
	}
	// Account/egress scoping is preserved so egress-bound state is unaffected.
	if !strings.HasPrefix(keyA, "acc-custom:egress-1") {
		t.Errorf("cookie jar key lost its account/egress scope: %q", keyA)
	}
	// An empty incoming key must still produce an account-scoped key, never a bare
	// conversation key shared across accounts.
	noBase := specA
	noBase.CookieJarKey = ""
	noBase.Egress = storage.EgressProfile{ID: "egress-9"}
	if got := customClaudeCookieJarKey(noBase, id); !strings.HasPrefix(got, "acc-custom:egress-9") {
		t.Errorf("fallback cookie jar key = %q, want an account/egress prefix", got)
	}
}
