package upstream

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// relayCharacteristicHeaders are the header names whose presence on an upstream request
// identifies the sender as a proxy/relay rather than a first-party client.
//
//   - X-Forwarded-For / X-Real-IP / X-Forwarded-* / Forwarded / Via: standard proxy hop
//     metadata. Any of them tells Anthropic the request was relayed, and X-Forwarded-For
//     additionally discloses the real downstream client IP.
//   - X-Pool-* / X-MiCliProxy-* / X-Codex-*: this gateway's own control and diagnostic
//     header namespaces. They name the product, so a single one reaching upstream is a
//     conclusive fingerprint of this specific relay.
//   - Cookie / Referer / Origin: the real claude-cli sends none of these to the API; a
//     value here is downstream browser state being carried up verbatim.
//   - X-Api-Key on the OAuth path / Proxy-Authorization: credential material belonging to
//     a different hop.
var relayCharacteristicHeaders = []string{
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Forwarded-Port",
	"X-Real-IP",
	"X-Original-Forwarded-For",
	"Forwarded",
	"Via",
	"Proxy-Authorization",
	"Proxy-Connection",
	"X-Pool-Account",
	"X-Pool-Key",
	"X-Pool-Group",
	"X-MiCliProxy-Context-Engine",
	"X-MiCliProxy-Goal-Error",
	"X-Codex-Claude-Cache-Diagnostics",
	"X-Codex-Account",
	"Cookie",
	"Referer",
	"Origin",
	"CF-Connecting-IP",
	"True-Client-IP",
	"X-Envoy-External-Address",
	"X-Request-Id",
}

// downstreamHeadersWithRelayNoise is a downstream request header set that carries every
// relay characteristic we must not propagate, plus legitimate client headers that the
// builders are allowed to consume.
func downstreamHeadersWithRelayNoise() http.Header {
	h := http.Header{}
	for _, name := range relayCharacteristicHeaders {
		h.Set(name, "leaked-"+strings.ToLower(name))
	}
	// Realistic proxy-chain values rather than only sentinels.
	h.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.12")
	h.Set("Via", "1.1 codex-account-pool")
	h.Set("Forwarded", "for=203.0.113.7;proto=https;host=pool.internal")
	h.Set("Cookie", "session=downstream-browser-state")
	// Legitimate client headers.
	h.Set("Anthropic-Beta", "fast-mode-2099-01-01")
	h.Set("Content-Type", "application/json")
	return h
}

// assertNoRelayCharacteristics fails for any relay header present in the built set.
func assertNoRelayCharacteristics(t *testing.T, label string, built http.Header) {
	t.Helper()
	for _, name := range relayCharacteristicHeaders {
		if v := built.Get(name); v != "" {
			t.Errorf("%s: relay-characteristic header %s reached the upstream request (value %q)", label, name, v)
		}
	}
	// Nothing in the gateway's own namespaces, including names not yet enumerated above.
	for name := range built {
		lower := strings.ToLower(name)
		for _, prefix := range []string{"x-pool-", "x-micliproxy-", "x-codex-", "x-forwarded-"} {
			if strings.HasPrefix(lower, prefix) {
				t.Errorf("%s: header %s is in the relay namespace %q and must not go upstream", label, name, prefix)
			}
		}
	}
}

// TestClaudeUpstreamHeadersCarryNoRelayCharacteristics is the core exposure-1 assertion:
// whatever a downstream client (or an intermediate proxy) puts on the inbound request,
// the header set we hand to api.anthropic.com contains no proxy-hop metadata, no
// gateway-internal control headers, and no downstream browser state.
func TestClaudeUpstreamHeadersCarryNoRelayCharacteristics(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-relay")

	oauthSpec := Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-relay"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Headers:  downstreamHeadersWithRelayNoise(),
		Body:     testBody([]byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`)),
	}
	apiKeySpec := oauthSpec
	apiKeySpec.Token = storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-test"}

	for _, tc := range []struct {
		name   string
		spec   Request
		stream bool
	}{
		{"messages/oauth/stream", oauthSpec, true},
		{"messages/oauth/nonstream", oauthSpec, false},
		{"messages/apikey/stream", apiKeySpec, true},
	} {
		built := http.Header{}
		c.applyClaudeHeaders(built, tc.spec, id, tc.stream)
		assertNoRelayCharacteristics(t, tc.name, built)
	}

	// The passthrough builder (Files API / skills / agents) forwards more of the client's
	// content negotiation, so it is the likelier place for a blanket copy to creep in.
	for _, tc := range []struct {
		name   string
		spec   Request
		stream bool
	}{
		{"passthrough/oauth/stream", oauthSpec, true},
		{"passthrough/apikey/nonstream", apiKeySpec, false},
	} {
		built := http.Header{}
		c.applyClaudePassthroughHeaders(built, tc.spec, id, tc.stream)
		assertNoRelayCharacteristics(t, tc.name, built)
	}
}

// TestClaudeUpstreamHeadersDoNotLeakDownstreamClientIdentity guards the narrower case
// where a downstream client sends its OWN real claude-cli identity headers. Those describe
// the caller's machine (its OS, arch, node build, session), not the account's virtual
// identity, so forwarding them would both contradict the account-bound identity and expose
// the real client host.
func TestClaudeUpstreamHeadersDoNotLeakDownstreamClientIdentity(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-identity")

	downstream := http.Header{}
	downstream.Set("User-Agent", "claude-cli/9.9.9 (external, cli)")
	downstream.Set("X-Stainless-OS", "FreeBSD")
	downstream.Set("X-Stainless-Arch", "arm")
	downstream.Set("X-Stainless-Runtime-Version", "v0.0.1-downstream")
	downstream.Set("X-Stainless-Package-Version", "0.0.1-downstream")
	downstream.Set("X-Stainless-Lang", "python")
	downstream.Set("X-Stainless-Runtime", "CPython")

	built := http.Header{}
	c.applyClaudeHeaders(built, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-identity"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Headers:  downstream,
	}, id, true)

	for _, check := range []struct{ header, forbidden, want string }{
		{"X-Stainless-OS", "FreeBSD", id.StainlessOS},
		{"X-Stainless-Arch", "arm", id.StainlessArch},
		{"X-Stainless-Runtime-Version", "v0.0.1-downstream", id.NodeVersion},
		{"X-Stainless-Package-Version", "0.0.1-downstream", id.StainlessPackageVersion},
		{"X-Stainless-Lang", "python", "js"},
		{"X-Stainless-Runtime", "CPython", "node"},
	} {
		got := built.Get(check.header)
		if got == check.forbidden {
			t.Errorf("%s forwarded the downstream client's own value %q", check.header, check.forbidden)
		}
		if got != check.want {
			t.Errorf("%s = %q, want account-bound %q", check.header, got, check.want)
		}
	}
	if got := built.Get("User-Agent"); strings.Contains(got, "9.9.9") {
		t.Errorf("User-Agent forwarded the downstream client version: %q", got)
	}
}

// TestClaudeHeaderOrderMatchesCapturedNativeOrder locks the wire header order to a raw
// ANTHROPIC_BASE_URL capture from the shipping native binary. Without an explicit order
// fhttp sorts header names lexicographically, which is an ordering the captured client
// never emits and is therefore a relay fingerprint of its own.
func TestClaudeHeaderOrderMatchesCapturedNativeOrder(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-order")

	built := http.Header{}
	c.applyClaudeHeaders(built, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-order"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Headers:  http.Header{},
	}, id, true)

	order := claudeHeaderOrder(built)

	// The native Bun build starts with Accept. Host is transport-injected near the tail,
	// after Connection and before Accept-Encoding.
	if len(order) == 0 || order[0] != "accept" {
		t.Fatalf("accept must lead the captured wire order, got %v", order)
	}

	// Every built header appears exactly once, plus exactly the transport-injected names.
	if len(order) != len(built)+len(transportInjectedHeaderOrder) {
		t.Fatalf("order has %d entries for %d built headers + %d transport-injected: %v",
			len(order), len(built), len(transportInjectedHeaderOrder), order)
	}
	seen := map[string]bool{}
	for _, name := range order {
		if name != strings.ToLower(name) {
			t.Errorf("order entry %q is not lowercase", name)
		}
		if seen[name] {
			t.Errorf("order lists %q twice", name)
		}
		seen[name] = true
		if _, ok := built[http.CanonicalHeaderKey(name)]; !ok && !transportInjectedHeaderOrder[name] {
			t.Errorf("order lists %q which is neither in the built header set nor transport-injected", name)
		}
	}
	for name := range built {
		if !seen[strings.ToLower(name)] {
			t.Errorf("built header %q is missing from the wire order", name)
		}
	}
	// Each transport-injected name holds its slot even though it is absent from built.
	for name := range transportInjectedHeaderOrder {
		if !seen[name] {
			t.Errorf("transport-injected header %q lost its ordered slot and would be sorted into the alphabetical tail", name)
		}
	}

	// The capture's relative order is preserved for the headers present.
	pos := func(name string) int {
		for i, n := range order {
			if n == name {
				return i
			}
		}
		return -1
	}
	for _, pair := range [][2]string{
		{"accept", "authorization"},
		{"authorization", "content-type"},
		{"content-type", "user-agent"},
		{"user-agent", "x-claude-code-session-id"},
		{"x-claude-code-session-id", "x-stainless-arch"},
		{"x-stainless-arch", "x-stainless-lang"},
		{"x-stainless-lang", "x-stainless-os"},
		{"x-stainless-os", "x-stainless-package-version"},
		{"x-stainless-package-version", "x-stainless-retry-count"},
		{"x-stainless-retry-count", "x-stainless-runtime"},
		{"x-stainless-runtime", "x-stainless-runtime-version"},
		{"x-stainless-runtime-version", "x-stainless-timeout"},
		{"x-stainless-timeout", "anthropic-beta"},
		{"anthropic-beta", "anthropic-dangerous-direct-browser-access"},
		{"anthropic-dangerous-direct-browser-access", "anthropic-version"},
		{"anthropic-version", "x-app"},
		{"x-app", "connection"},
		{"connection", "host"},
		{"host", "accept-encoding"},
		{"accept-encoding", "content-length"},
	} {
		a, b := pos(pair[0]), pos(pair[1])
		if a < 0 || b < 0 {
			t.Fatalf("expected both %q (%d) and %q (%d) in the order: %v", pair[0], a, pair[1], b, order)
		}
		if a > b {
			t.Errorf("%q must precede %q, got %v", pair[0], pair[1], order)
		}
	}

	// It must not be plain alphabetical order — that is the fhttp default we are avoiding.
	alphabetical := append([]string(nil), order...)
	sort.Strings(alphabetical)
	same := true
	for i := range order {
		if order[i] != alphabetical[i] {
			same = false
			break
		}
	}
	if same {
		t.Error("wire order is lexicographic, i.e. the fhttp default fingerprint rather than the SDK's order")
	}
}

// TestClaudeHeaderOrderCoversUnknownHeaders: a header not in the SDK table must still be
// ordered deterministically, otherwise Go's random map iteration would make the same
// account emit a different header order on every request — fingerprint drift on one
// account is itself a risk-control signal.
func TestClaudeHeaderOrderCoversUnknownHeaders(t *testing.T) {
	built := http.Header{}
	built.Set("Accept", "application/json")
	built.Set("User-Agent", "claude-cli/2.1.226")
	built.Set("Z-Unknown", "1")
	built.Set("A-Unknown", "2")
	built.Set("M-Unknown", "3")

	first := claudeHeaderOrder(built)
	for i := 0; i < 50; i++ {
		got := claudeHeaderOrder(built)
		if len(got) != len(first) {
			t.Fatalf("order length changed between calls: %v vs %v", first, got)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("order is not deterministic: %v vs %v", first, got)
			}
		}
	}
	// Known captured headers lead, followed by the transport-injected tail, then unknown
	// names in a stable sorted tail.
	if want := []string{"accept", "user-agent", "connection", "host", "accept-encoding", "content-length"}; len(first) < len(want) {
		t.Fatalf("order too short: %v", first)
	} else {
		for i, name := range want {
			if first[i] != name {
				t.Fatalf("known/injected headers did not lead the order in table position: want %v, got %v", want, first)
			}
		}
	}
	if got := first[6:]; len(got) != 3 || got[0] != "a-unknown" || got[1] != "m-unknown" || got[2] != "z-unknown" {
		t.Fatalf("unknown headers not in stable sorted tail: %v", got)
	}
}

// TestForceHTTP1ForClaudeImpersonation pins the protocol decision. The captured native
// Bun ClientHello has no ALPN extension and its request is HTTP/1.1. An accidental Chrome
// profile would otherwise negotiate h2 beneath a claude-cli User-Agent.
func TestForceHTTP1ForClaudeImpersonation(t *testing.T) {
	claudeUA := http.Header{}
	claudeUA.Set("User-Agent", "claude-cli/2.1.226 (external, sdk-cli)")

	cases := []struct {
		name    string
		target  string
		headers http.Header
		want    bool
	}{
		{"anthropic messages", "https://api.anthropic.com/v1/messages", http.Header{}, true},
		{"anthropic count_tokens", "https://api.anthropic.com/v1/messages/count_tokens", http.Header{}, true},
		{"anthropic oauth token", "https://api.anthropic.com/v1/oauth/token", http.Header{}, true},
		{"console subdomain", "https://console.anthropic.com/v1/x", http.Header{}, true},
		{"claude.ai", "https://claude.ai/api/x", http.Header{}, true},
		{"platform claude oauth", "https://platform.claude.com/v1/oauth/token", http.Header{}, true},
		// A custom base URL still claims to be Claude Code, so it must look like it.
		{"custom base url with claude-cli UA", "https://gateway.internal.test/v1/messages", claudeUA, true},
		// Lookalike hosts must not match on a substring.
		{"lookalike suffix", "https://api.anthropic.com.evil.test/v1/messages", http.Header{}, false},
		{"lookalike prefix", "https://notanthropic.com/v1/messages", http.Header{}, false},
		// Other providers keep their own profile's native ALPN.
		{"openai", "https://chatgpt.com/backend-api/codex/responses", http.Header{}, false},
		{"kiro", "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", http.Header{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forceHTTP1ForClaudeImpersonation(tc.target, tc.headers); got != tc.want {
				t.Fatalf("forceHTTP1ForClaudeImpersonation(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

// TestInProcessAcceptEncodingMatchesCapturedBun pins the Accept-Encoding on the wire. Left
// empty, fhttp fills it with "gzip, deflate, br" (transport.go:2567) — an order no real
// captured request sends, missing zstd, on a request whose UA claims to be Claude. fhttp
// can decode all four codecs and arms decompression whenever our value contains "gzip", so Bun's
// exact value is both faithful and still transparently decompressed.
func TestInProcessAcceptEncodingMatchesCapturedBun(t *testing.T) {
	t.Run("claude target gets Bun's value", func(t *testing.T) {
		h := http.Header{}
		applyInProcessAcceptEncoding(h, "https://api.anthropic.com/v1/messages")
		if got := h.Get("Accept-Encoding"); got != claudeAcceptEncoding {
			t.Fatalf("Accept-Encoding = %q, want %q", got, claudeAcceptEncoding)
		}
		// Must contain gzip or fhttp will not arm transparent decompression, and the SSE
		// scanner would then read compressed bytes.
		if !strings.Contains(h.Get("Accept-Encoding"), "gzip") {
			t.Fatal("value must contain gzip so fhttp arms DecompressBody")
		}
	})
	t.Run("claude oauth keeps captured Axios value", func(t *testing.T) {
		h := http.Header{}
		h.Set("Accept-Encoding", "gzip, compress, deflate, br")
		applyInProcessAcceptEncoding(h, "https://platform.claude.com/v1/oauth/token")
		if got := h.Get("Accept-Encoding"); got != "gzip, compress, deflate, br" {
			t.Fatalf("Accept-Encoding = %q, want captured Axios value", got)
		}
	})
	t.Run("non-claude target is left to the transport", func(t *testing.T) {
		h := http.Header{}
		h.Set("Accept-Encoding", "identity")
		applyInProcessAcceptEncoding(h, "https://chatgpt.com/backend-api/codex/responses")
		if got := h.Get("Accept-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding = %q, want it deleted for non-Claude targets", got)
		}
	})
}

// TestApplyClaudeFetchHeaders pins the native Bun header absence. The shipping capture has
// no Accept-Language, and caller-provided locale/encoding fields must not leak through.
func TestApplyClaudeFetchHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Encoding", "identity")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	applyClaudeFetchHeaders(h)
	if got := h.Get("Accept-Language"); got != "" {
		t.Errorf("Accept-Language = %q, want absent as in the native capture", got)
	}
	if got := h.Get("Accept-Encoding"); got != "" {
		t.Errorf("Accept-Encoding = %q, want unset so the transport negotiates it", got)
	}
}

// TestClaudeStreamingDoesNotRequestIdentityEncoding is the end-to-end form of the same
// assertion: neither Claude builder may emit Accept-Encoding: identity on a streaming turn.
func TestClaudeStreamingDoesNotRequestIdentityEncoding(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-encoding")
	spec := Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-encoding"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Headers:  http.Header{},
	}

	for _, tc := range []struct {
		name  string
		build func(http.Header)
	}{
		{"messages", func(h http.Header) { c.applyClaudeHeaders(h, spec, id, true) }},
		{"passthrough", func(h http.Header) { c.applyClaudePassthroughHeaders(h, spec, id, true) }},
	} {
		h := http.Header{}
		tc.build(h)
		if got := h.Get("Accept-Encoding"); strings.Contains(strings.ToLower(got), "identity") {
			t.Errorf("%s: Accept-Encoding = %q; no real Node/browser client asks HTTPS for identity", tc.name, got)
		}
		if got := h.Get("Accept-Language"); got != "" {
			t.Errorf("%s: Accept-Language = %q; native Claude 2.1.226 sends none", tc.name, got)
		}
	}
}

func TestClaudeWireHeadersMatchCapturedCasing(t *testing.T) {
	built := http.Header{}
	built.Set("Accept", "application/json")
	built.Set("Anthropic-Beta", "beta")
	built.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	built.Set("Anthropic-Version", "2023-06-01")
	built.Set("X-Api-Key", "secret")
	built.Set("X-App", "cli")
	built.Set("X-Stainless-Arch", "x64")
	built.Set("X-Stainless-OS", "Linux")

	wire := claudeWireHeaders(built)
	for _, name := range []string{"anthropic-beta", "anthropic-dangerous-direct-browser-access", "anthropic-version", "x-api-key", "x-app"} {
		if _, ok := wire[name]; !ok {
			t.Errorf("wire header %q missing: %#v", name, wire)
		}
		if _, duplicate := wire[http.CanonicalHeaderKey(name)]; duplicate {
			t.Errorf("wire contains canonical duplicate %q", http.CanonicalHeaderKey(name))
		}
	}
	if _, ok := wire["X-Stainless-Arch"]; !ok {
		t.Errorf("captured canonical X-Stainless-Arch casing changed: %#v", wire)
	}
	if _, ok := wire["X-Stainless-OS"]; !ok {
		t.Errorf("captured literal X-Stainless-OS casing missing: %#v", wire)
	}
	if _, wrong := wire["X-Stainless-Os"]; wrong {
		t.Errorf("Go-canonical X-Stainless-Os leaked onto the wire: %#v", wire)
	}
	if got := wire["Connection"]; len(got) != 1 || got[0] != "keep-alive" {
		t.Errorf("Connection = %v, want [keep-alive]", got)
	}
}

func TestResolveClaudeTLSDefaultsToCapturedBun(t *testing.T) {
	for _, value := range []string{"", "claude-cli", "real", "native", "bun"} {
		if got := resolveClaudeJA3(value); got != identity.ClaudeJA3 {
			t.Errorf("resolveClaudeJA3(%q) = %q, want captured %q", value, got, identity.ClaudeJA3)
		}
		if got := resolveClaudeTLSProfile(value); got != "claude_bun" {
			t.Errorf("resolveClaudeTLSProfile(%q) = %q, want claude_bun", value, got)
		}
	}
	for _, value := range []string{"off", "chrome", "browser"} {
		if got := resolveClaudeJA3(value); got != "" {
			t.Errorf("resolveClaudeJA3(%q) = %q, want empty Chrome sentinel", value, got)
		}
		if got := resolveClaudeTLSProfile(value); got != "chrome" {
			t.Errorf("resolveClaudeTLSProfile(%q) = %q, want chrome", value, got)
		}
	}
}
