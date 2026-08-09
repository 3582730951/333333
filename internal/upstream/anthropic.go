package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream/tlsclient"
)

// Official Claude Code anti-fingerprint constants (see cliproxyapi-reference).
const (
	claudeAnthropicVersion  = "2023-06-01"
	claudeAgentIDHeader     = "X-Claude-Code-Agent-Id"
	claudeParentAgentHeader = "X-Claude-Code-Parent-Agent-Id"
	// Lowercase forms used by the wire header-order table (HTTP/2 field names).
	claudeAgentIDHeaderLower     = "x-claude-code-agent-id"
	claudeParentAgentHeaderLower = "x-claude-code-parent-agent-id"
	// OAuth (Claude Pro/Max) carries the oauth beta; API keys must not.
	claudeOAuthBetas             = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24,extended-cache-ttl-2025-04-11"
	claudeAPIKeyBetas            = "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24"
	claudeCacheDiagnosticsBeta   = "cache-diagnosis-2026-04-07"
	claudeCacheDiagnosticsHeader = "X-Codex-Claude-Cache-Diagnostics"
)

func claudeBaseURL(cfg config.Config) string {
	if b := strings.TrimSpace(cfg.ClaudeUpstreamBaseURL); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.anthropic.com"
}

// claudeCredentialProvider keeps built-in/legacy Claude rows on Claude's native
// credential inference while letting custom Claude-profile providers use their
// own account provider id. The latter matters for legacy custom keys with an
// arbitrary prefix: custom-provider OpenAIAPIKey rows are API keys even when they
// do not begin with sk-ant-api.
func claudeCredentialProvider(spec Request) string {
	if provider := strings.TrimSpace(spec.Account.Provider); provider != "" {
		return provider
	}
	return "claude"
}

func claudeUsesAPIKey(spec Request) bool {
	return accountprovider.UsesAPIKey(claudeCredentialProvider(spec), spec.Token)
}

func (c *Client) doClaude(ctx context.Context, spec Request) (*Response, error) {
	base := claudeBaseURL(c.cfg)
	path := spec.DownstreamPath
	if path == "" {
		path = "/v1/messages"
	}
	target := base + path
	// Anthropic messages endpoints are addressed with ?beta=true. Passthrough endpoints
	// (Files API, skills, agents/environments/sessions) are NOT messages endpoints and
	// must keep the client's own query verbatim.
	if !spec.PassThrough && strings.Contains(path, "/v1/messages") && !strings.Contains(target, "beta=") {
		if strings.Contains(target, "?") {
			target += "&beta=true"
		} else {
			target += "?beta=true"
		}
	}
	// In passthrough mode the body is opaque (a multipart upload, a JSON skill/agent
	// definition, …) — never parse it for a `stream` flag. Stream framing follows the
	// client's Accept header instead.
	stream := false
	if spec.PassThrough {
		stream = strings.Contains(strings.ToLower(spec.Headers.Get("Accept")), "text/event-stream")
	} else {
		stream = bodyStreamTrue(requestBody(spec))
	}

	// === THINKING INJECTION: Apply thinking configuration before forwarding ===
	// Only inject when:
	// 1. Not in passthrough mode (passthrough = opaque body, no thinking injection)
	// 2. Thinking is enabled in config
	// 3. Path is a messages endpoint (not files/skills/agents)
	// 4. The request is not a max_tokens:0 cache pre-warm. Anthropic documents
	//    max_tokens:0 as "reads your prompt into the model and writes the cache at any
	//    cache_control breakpoint, then returns immediately without generating any
	//    output", and documents that it is rejected with invalid_request_error when
	//    extended thinking is enabled on the request. A pre-warm's whole purpose is to
	//    carry the client's own parameters unchanged so that the cache entry it writes
	//    matches the real turn that follows it; a thinking block this relay added, which
	//    the downstream client never sent, makes the pre-warm and the real turn disagree
	//    and risks the documented rejection on every warmed turn — a repeating,
	//    self-similar 400 pattern against the account. Note the observable injection here
	//    is thinking.type=adaptive (every Claude model in the local registry is
	//    level-only, so budget mode is converted to level mode before the applier runs);
	//    max_tokens itself is left at 0 in that branch. The narrower claim is the one
	//    that holds: the relay must not add a thinking block to a pre-warm at all.
	if !spec.PassThrough && !spec.MinimalProbe && !claudeZeroMaxTokensPrewarm(requestBody(spec)) && c.cfgSnapshot().ThinkingEnabled && strings.Contains(path, "/v1/messages") {
		setRequestBody(&spec, c.applyThinkingConfig(requestBody(spec), "claude", spec.Model, spec.Account))
	}
	if !spec.PassThrough && !spec.MinimalProbe && strings.Contains(path, "/v1/messages") {
		spec = c.normalizeClaudeMessagesSpec(spec)
	}

	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)

	applyHeaders := c.applyClaudeHeaders
	if spec.PassThrough {
		applyHeaders = c.applyClaudePassthroughHeaders
	}

	// Route Anthropic traffic through a real client TLS/JA3 + HTTP2 fingerprint instead
	// of the Go standard library's. Although Anthropic (unlike chatgpt.com) has no
	// Cloudflare challenge wall, the stdlib transport's fingerprint is itself a
	// relay-detection signal: a request whose headers claim native claude-cli/Bun but whose
	// ClientHello and HTTP/2 SETTINGS are Go's is self-contradicting, and every pooled
	// account would share that same contradiction.
	//
	// The in-process engine (tls-client/uTLS) needs no external process and can dial ANY
	// egress type — direct, http(s)/warp/socks5(h) proxy, or a sidecar's chain — so it is
	// applied to every egress, not only "curl_cffi_sidecar". Gating it on the sidecar
	// egress type (the historical behavior) meant an account bound to a plain proxy or to
	// direct silently fell back to the stdlib fingerprint; that is exactly the "escaped
	// request" case that puts one account's traffic behind two different fingerprints.
	//
	// The external curl_cffi sidecar keeps its previous scope: it is a separate process
	// reachable only through a sidecar-typed egress, so it stays limited to that type.
	// The ClaudeForceDirect escape hatch still forces the plain stdlib transport for
	// deployments that must not use either engine.
	if c.claudeFingerprintEngine(spec.Egress) != claudeEngineStdlib {
		built := http.Header{}
		applyHeaders(built, spec, id, stream)
		// The fingerprint transport negotiates and auto-decompresses encoding itself
		// (and the captured native client never asks for "identity"), so drop our
		// identity hint and let the impersonated transport present its native
		// Accept-Encoding.
		built.Del("Accept-Encoding")
		claudeSpec := spec
		claudeSpec.Method = firstNonEmpty(spec.Method, http.MethodPost)
		// JA3 the sidecar replays for Claude. The default is the ClientHello captured
		// from the shipping native Bun build, so TLS and HTTP fingerprints describe the
		// same client. An explicit browser alias remains available for compatibility.
		claudeJA3Override := c.cfgSnapshot().ClaudeJA3Override
		ja3 := resolveClaudeJA3(claudeJA3Override)
		// In-process fingerprint engine: route through tls-client's captured Bun profile instead of
		// the external curl_cffi sidecar. The sidecar stays the fallback on engine "sidecar".
		//
		// The idle budget is the plain request timeout here. The sidecar branch below adds
		// its own headroom because that path crosses an extra process hop; the in-process
		// engine has no such hop, so borrowing the sidecar's (much larger) budget would
		// silently weaken the idle watchdog and let a hung upstream hold a slot for the
		// sidecar window instead of the configured request timeout.
		if c.claudeFingerprintEngine(spec.Egress) == claudeEngineInProcess {
			return c.postInProcessOrdered(ctx, claudeSpec, target, built, c.cfg.RequestTimeout(), ja3, resolveClaudeTLSProfile(claudeJA3Override), claudeHeaderOrder(built))
		}
		// Sidecar path: allow the larger of the request timeout and the sidecar budget,
		// since this call crosses an extra process hop before it reaches the upstream.
		sidecarTimeout := c.cfg.RequestTimeout()
		if st := c.cfg.SidecarTimeout(); st > sidecarTimeout {
			sidecarTimeout = st
		}
		return c.postViaSidecarOrdered(ctx, claudeSpec, target, built, sidecarTimeout, ja3, false, claudeHeaderOrder(built))
	}

	req, err := newReplayableHTTPRequest(ctx, firstNonEmpty(spec.Method, http.MethodPost), target, spec)
	if err != nil {
		return nil, err
	}
	applyHeaders(req.Header, spec, id, stream)
	if forceHTTP1ForClaudeImpersonation(target, req.Header) {
		req.Header = claudeWireHeaders(req.Header)
	}

	// Bound the call by an IDLE-timeout guard (reset on every read), released on the
	// response body's Close — NOT a `defer cancel()` on this function's return, and NOT
	// an absolute deadline. A deferred cancel fires the instant doClaude returns, before
	// the caller streams resp.Body, truncating the SSE; an absolute deadline cuts a
	// legitimately long stream (big context, extended thinking, a long agentic turn)
	// even while it is actively flowing. The idle guard lets a stream of any length
	// through as long as it keeps making progress, while still aborting a dead upstream.
	// Both failure modes surface downstream as "Failed to parse JSON" / "empty or
	// malformed response (HTTP 200)". See requestGuard in client.go.
	directCtx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	req = req.WithContext(directCtx)
	// Direct/proxy fallback. A sidecar egress only reaches here when ClaudeForceDirect
	// is set. For the account-level two-layer binding, restore the selected HTTP/SOCKS
	// exit and bypass only its sidecar wrapper.
	directSpec := spec
	directSpec.Egress = claudeDirectFallbackEgress(directSpec.Egress)
	// Pin HTTP/1.1 here too. This is the stdlib escape hatch (ClaudeForceDirect, or an
	// engine that resolved to stdlib because no TLS factory exists), so the ClientHello is
	// already Go's — but the request still carries the full claude-cli identity built by
	// applyHeaders above, and net/http's default ForceAttemptHTTP2 negotiates h2 against
	// Anthropic's edge. "claude-cli User-Agent arriving over HTTP/2" is by construction
	// never the captured native client (its ClientHello has no ALPN), so it is a
	// free-standing discriminator that survives the operator's decision to skip TLS
	// impersonation. The same predicate the fingerprint engines use decides it, so all
	// three engines agree on the wire protocol for one account.
	client, err := c.httpClientForEgressMode(directSpec, forceHTTP1ForClaudeImpersonation(target, req.Header))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: guard.Wrap(resp.Body)}, nil
}

// DoAnthropicOAuth carries a Claude OAuth token-endpoint call (authorization-code
// exchange and refresh_token grant) on the same account egress and TLS fingerprint
// engine as that account's inference traffic.
//
// Why this exists instead of a plain http.Client:
//
// platform.claude.com and api.anthropic.com share the same Anthropic account perimeter.
// Mixing the relay host IP/Go ClientHello on token calls with a bound proxy/native
// ClientHello on inference creates a stable per-account relay signal.
//
// The caller supplies the fully-built header set; nothing is inherited from a downstream
// request. Only the identity-neutral OAuth grant travels in the body.
func (c *Client) DoAnthropicOAuth(ctx context.Context, egress storage.EgressProfile, account storage.Account, target string, headers http.Header, body []byte, cookieJarKey string) (*Response, error) {
	built := headers.Clone()
	if built == nil {
		built = http.Header{}
	}
	spec := Request{
		Method:       http.MethodPost,
		Body:         bodysource.Bytes(body),
		Account:      account,
		Egress:       egress,
		CookieJarKey: cookieJarKey,
	}
	timeout := c.cfg.RequestTimeout()
	claudeJA3Override := c.cfgSnapshot().ClaudeJA3Override
	switch c.claudeFingerprintEngine(egress) {
	case claudeEngineInProcess:
		return c.postInProcessOrdered(ctx, spec, target, built, timeout, resolveClaudeJA3(claudeJA3Override), resolveClaudeTLSProfile(claudeJA3Override), claudeOAuthHeaderOrder(built))
	case claudeEngineSidecar:
		return c.postViaSidecarOrdered(ctx, spec, target, built, timeout, resolveClaudeJA3(claudeJA3Override), false, claudeOAuthHeaderOrder(built))
	}
	// Explicit ClaudeForceDirect (or no fingerprint engine available): still keep the
	// account's egress so the token call and the inference calls share an exit IP.
	directSpec := spec
	directSpec.Egress = claudeDirectFallbackEgress(egress)
	req, err := newReplayableHTTPRequest(ctx, http.MethodPost, target, directSpec)
	if err != nil {
		return nil, err
	}
	req.Header = built
	if forceHTTP1ForClaudeImpersonation(target, req.Header) {
		req.Header = claudeWireHeaders(req.Header)
	}
	directCtx, guard := newRequestGuard(ctx, timeout)
	req = req.WithContext(directCtx)
	client, err := c.httpClientForEgressMode(directSpec, forceHTTP1ForClaudeImpersonation(target, req.Header))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: guard.Wrap(resp.Body)}, nil
}

// claudeDirectFallbackEgress maps an egress onto the transport the stdlib fallback
// must dial, WITHOUT ever inventing a host-direct exit for an account that has one
// configured.
//
// storage.WithoutSidecarTransport alone is not sufficient here. For the two-layer
// binding (proxy wrapped by a sidecar) it correctly restores TransportBase*, but for a
// LEGACY account whose primary egress is itself a curl_cffi_sidecar profile there are no
// TransportBase* fields, so it rewrites the profile to Type="direct" with Endpoint and
// ChainProxy cleared. Such a profile can still carry an explicit ChainProxy — that chain
// IS the account's intended exit IP. Degrading it to a host-direct connection means the
// inference turns for that account arrive from the proxy IP while the ClaudeForceDirect
// turns (and the OAuth refresh) arrive from the relay host's own address: one account
// observed on two networks, which is the IP/account binding inconsistency upstream risk
// control looks for, and it happens silently.
//
// client.go's sidecarBypassEgress already applies this rescue for the sidecar
// pre-header retry path; this keeps the Claude direct/OAuth fallbacks consistent with it
// instead of leaving one of the three sidecar-unwrapping sites leaking.
func claudeDirectFallbackEgress(egress storage.EgressProfile) storage.EgressProfile {
	base := storage.WithoutSidecarTransport(egress)
	if strings.TrimSpace(base.Endpoint) != "" {
		return base
	}
	// No restored endpoint: only a legacy sidecar-primary profile can land here with an
	// exit that WithoutSidecarTransport just discarded.
	if strings.TrimSpace(egress.TransportSidecarID) == "" && storage.IsSidecarEgress(egress) {
		if chain := strings.TrimSpace(egress.ChainProxy); chain != "" {
			base.Type = proxyTypeForURL(chain)
			base.Endpoint = chain
			base.ChainProxy = ""
		}
	}
	return base
}

// claudeEngine names the transport that carries an Anthropic request.
type claudeEngine int

const (
	// claudeEngineStdlib is the Go net/http transport. It emits Go's TLS ClientHello
	// and HTTP/2 SETTINGS, which contradict the claude-cli headers we send, so it is
	// only ever selected by the explicit ClaudeForceDirect escape hatch (or when the
	// in-process engine is unavailable and the egress cannot reach the sidecar).
	claudeEngineStdlib claudeEngine = iota
	// claudeEngineInProcess is the tls-client/uTLS engine inside this process.
	claudeEngineInProcess
	// claudeEngineSidecar is the external curl_cffi sidecar process.
	claudeEngineSidecar
)

// claudeFingerprintEngine picks the transport for an Anthropic request.
//
// Ordering matters for risk control: the in-process engine is preferred for EVERY egress
// type because it can present the captured native fingerprint regardless of how the account
// exits. Falling back to the stdlib transport is reported as
// such by the caller rather than silently accepted, because that fallback is the one path
// that leaks a Go fingerprint to api.anthropic.com.
func (c *Client) claudeFingerprintEngine(egress storage.EgressProfile) claudeEngine {
	if c.cfgSnapshot().ClaudeForceDirect {
		return claudeEngineStdlib
	}
	if c.inProcessFingerprint() && c.tlsFactory != nil {
		return claudeEngineInProcess
	}
	// Engine "sidecar": only a sidecar-typed egress can reach the external process.
	if storage.IsSidecarEgress(egress) {
		return claudeEngineSidecar
	}
	// Engine is pinned to "sidecar" but this egress has no sidecar to talk to. Prefer a
	// real fingerprint from the in-process engine over the stdlib's Go fingerprint when
	// the factory exists; the operator's engine choice is about which impersonation
	// backend to use, not a license to emit a Go ClientHello at Anthropic.
	if c.tlsFactory != nil {
		return claudeEngineInProcess
	}
	return claudeEngineStdlib
}

// claudeUpstreamHeaderOrder is the observed HTTP/1.1 wire order of Claude Code
// 2.1.226's native Bun 1.4.0 build. It comes from direct
// ANTHROPIC_BASE_URL captures of API-key, OAuth and full-tools requests rather
// than from the TypeScript SDK's pre-bundle insertion order. The native build
// emits canonical-cased application headers, lower-case Anthropic/default
// headers, and finally Bun's transport headers.
//
// Names in this order table are lower-case because fhttp's sorter lowercases
// before lookup. claudeWireHeaders separately reproduces the observed h1 field
// casing; Content-Length remains canonical exactly as Bun emits it.
var claudeUpstreamHeaderOrder = []string{
	"accept",
	"authorization",
	"content-type",
	"user-agent",
	claudeAgentIDHeaderLower,
	claudeParentAgentHeaderLower,
	"x-claude-code-session-id",
	"x-stainless-arch",
	"x-stainless-lang",
	"x-stainless-os",
	"x-stainless-package-version",
	"x-stainless-retry-count",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
	"x-stainless-timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-api-key",
	"x-app",
	"connection",
	"host",
	"accept-encoding",
	"content-length",
}

// transportInjectedHeaderOrder marks entries of claudeUpstreamHeaderOrder that the
// transport adds on our behalf, so claudeHeaderOrder keeps their slot even though they are
// absent from the header set we build. See claudeHeaderOrder for the per-header detail.
var transportInjectedHeaderOrder = map[string]bool{
	"connection":      true,
	"host":            true,
	"accept-encoding": true,
	"content-length":  true,
}

// claudeHeaderOrder projects claudeUpstreamHeaderOrder onto the headers actually present
// in built, then appends any remaining header in a deterministic (sorted) position. The
// projection keeps the emitted order stable when an optional header is absent (e.g. an
// OAuth account has authorization but no x-api-key) instead of leaving a gap, and the
// deterministic tail guarantees two requests from the same account never reorder — header
// order drift on one account is itself a risk-control signal.
//
// transportInjectedHeaderOrder names headers the TRANSPORT adds after we compute this
// order, so they are never present in built and a presence-gated projection would drop
// them — leaving them to be sorted into the alphabetical tail instead of held at their
// real position. Listing a name that ends up absent is harmless: fhttp's headerSorter
// only consults the order map for headers it is actually writing.
//
// Host and Content-Length are folded into fhttp's header map before sorting.
// Connection and Accept-Encoding are applied by the provider-neutral transport
// helpers after this order is computed. All four therefore need reserved slots.
func claudeHeaderOrder(built http.Header) []string {
	order := make([]string, 0, len(built)+len(claudeUpstreamHeaderOrder)+1)
	placed := make(map[string]bool, len(built))
	for _, name := range claudeUpstreamHeaderOrder {
		if placed[name] {
			continue
		}
		_, present := built[http.CanonicalHeaderKey(name)]
		if !present && !transportInjectedHeaderOrder[name] {
			continue
		}
		placed[name] = true
		order = append(order, name)
	}
	var extra []string
	for name := range built {
		lower := strings.ToLower(name)
		if placed[lower] {
			continue
		}
		extra = append(extra, lower)
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// claudeOAuthHeaderOrder is the HTTP/1.1 order captured from Claude Code 2.1.226's
// Axios 1.15.2 authorization-code and refresh-token calls. OAuth does not use the
// messages SDK's x-stainless/anthropic header sequence.
func claudeOAuthHeaderOrder(built http.Header) []string {
	native := []string{
		"accept",
		"content-type",
		"user-agent",
		"content-length",
		"accept-encoding",
		"host",
		"connection",
	}
	injected := map[string]bool{"content-length": true, "host": true}
	order := make([]string, 0, len(native)+len(built))
	placed := make(map[string]bool, len(native)+len(built))
	for _, name := range native {
		_, present := built[http.CanonicalHeaderKey(name)]
		if !present && !injected[name] {
			continue
		}
		order = append(order, name)
		placed[name] = true
	}
	var extra []string
	for name := range built {
		lower := strings.ToLower(name)
		if !placed[lower] {
			extra = append(extra, lower)
		}
	}
	sort.Strings(extra)
	return append(order, extra...)
}

// claudeWireHeaders projects net/http's canonical map keys onto the exact case
// observed from the native client. The returned clone is transport-only: keeping
// the builders canonical lets Header.Get/Values and policy code stay
// case-insensitive without creating duplicate logical keys.
func claudeWireHeaders(built http.Header) http.Header {
	wire := built.Clone()
	for _, name := range []string{
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
		"Anthropic-Version",
		"X-Api-Key",
		"X-App",
	} {
		values := append([]string(nil), wire.Values(name)...)
		wire.Del(name)
		if len(values) > 0 {
			wire[strings.ToLower(name)] = values
		}
	}
	// net/textproto canonicalizes the acronym to X-Stainless-Os, while Bun keeps
	// the SDK's literal X-Stainless-OS spelling on HTTP/1.1.
	osValues := append([]string(nil), wire.Values("X-Stainless-OS")...)
	wire.Del("X-Stainless-OS")
	if len(osValues) > 0 {
		wire["X-Stainless-OS"] = osValues
	}
	if wire.Get("Connection") == "" {
		wire.Set("Connection", "keep-alive")
	}
	return wire
}

// resolveClaudeJA3 resolves the TLS/JA3 fingerprint the curl_cffi sidecar replays for
// Claude/Anthropic traffic from the operator's claude_ja3 setting.
//
// The default is the captured native Bun fingerprint. Chrome remains an
// explicit escape hatch, but using it by default puts a Chrome ClientHello
// beneath a claude-cli/Bun HTTP shape and exposes the relay through that
// contradiction.
func resolveClaudeJA3(override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "", "claude-cli", "claude_cli", "real", "native", "node", "bun":
		return identity.ClaudeJA3
	case "off", "none", "disabled", "-", "chrome", "browser":
		return ""
	default:
		return strings.TrimSpace(override)
	}
}

// resolveClaudeTLSProfile selects the complete in-process ClientHello profile. The raw
// JA3 string alone cannot describe extension payloads (signature schemes, key shares,
// ALPN absence), so the default/native aliases use the captured Bun ClientHelloSpec.
// Browser aliases are the only values that deliberately select Chrome.
func resolveClaudeTLSProfile(override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "off", "none", "disabled", "-", "chrome", "browser":
		return tlsclient.ProfileChrome
	default:
		return tlsclient.ProfileClaude
	}
}

// applyClaudeHeaders builds the official Claude Code request headers from
// scratch using the account-bound virtual identity, so the upstream sees a
// consistent, first-party-looking client instead of the relay/host machine.
func (c *Client) applyClaudeHeaders(dst http.Header, spec Request, id identity.Identity, stream bool) {
	token := accountprovider.Credential(claudeCredentialProvider(spec), spec.Token)
	apiKey := claudeUsesAPIKey(spec)

	dst.Set("Content-Type", "application/json")
	if apiKey {
		dst.Set("x-api-key", token)
		// Match the official client shape per credential type: real Claude Code in
		// OAuth (Pro/Max) mode does NOT send this header, while the API-key/SDK path
		// does. An API key is fundamentally not the Claude Code OAuth client, so it
		// can never present the full OAuth fingerprint (no oauth-* beta, no Bearer);
		// sending the SDK-correct header here is the closest faithful shape and is
		// what the reference relay does. Prefer OAuth (sk-ant-oat) accounts when full
		// Claude Code mimicry matters.
		dst.Set("Anthropic-Beta", mergeBetas(claudeAPIKeyBetas, spec.Headers, true))
	} else {
		if token != "" {
			dst.Set("Authorization", "Bearer "+token)
		}
		dst.Set("Anthropic-Beta", mergeBetas(claudeOAuthBetas, spec.Headers, false))
	}
	// Claude Code 2.1.226 sends this in both API-key and third-party Bearer
	// configurations (verified through ANTHROPIC_BASE_URL captures).
	dst.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	if strings.TrimSpace(spec.Headers.Get(claudeCacheDiagnosticsHeader)) != "" {
		appendClaudeBeta(dst, claudeCacheDiagnosticsBeta)
	}
	dst.Set("Anthropic-Version", claudeAnthropicVersion)
	dst.Set("X-App", "cli")
	dst.Set("X-Stainless-Retry-Count", "0")
	dst.Set("X-Stainless-Runtime", "node")
	dst.Set("X-Stainless-Lang", "js")
	dst.Set("X-Stainless-Timeout", "600")
	dst.Set("X-Stainless-OS", id.StainlessOS)
	dst.Set("X-Stainless-Arch", id.StainlessArch)
	// Version axes use the captured self-consistent shipping tuple; account-bound
	// OS/device/session axes still provide fleet diversity. An explicit operator
	// override wins globally. The claude-cli version (UA) and the @anthropic-ai/sdk
	// "Stainless" package version are separate axes — the real client sends
	// claude-cli/2.1.226 with X-Stainless-Package-Version: 0.94.0.
	claudeVer := c.cfgSnapshot().ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
	dst.Set("X-Stainless-Package-Version", c.cfgSnapshot().ClaudeStainlessVersionOrDefault(id.StainlessPackageVersion))
	dst.Set("X-Stainless-Runtime-Version", c.cfgSnapshot().ClaudeNodeVersionOrDefault(id.NodeVersion))
	dst.Set("X-Claude-Code-Session-Id", claudeSessionID(spec.Headers, requestBody(spec), id))
	forwardClaudeAgentContextHeaders(dst, spec.Headers)
	dst.Set("User-Agent", id.ClaudeUserAgentVersionForEntrypoint(claudeVer, claudeEntrypoint(spec.Headers, requestBody(spec))))
	dst.Set("Accept", "application/json")
	applyClaudeFetchHeaders(dst)
}

// applyClaudeFetchHeaders removes caller-controlled content coding. Claude Code
// 2.1.226 is a native Bun binary: direct captures show no Accept-Language header,
// so injecting undici's historical `Accept-Language: *` is itself a relay tell.
//
// Accept-Encoding is deliberately NOT set here:
//   - the fingerprint engines strip it and emit the impersonated transport's own value
//     (see postInProcess / the sidecar path), which is what keeps TLS↔header coherence;
//   - on the plain stdlib fallback, leaving it unset lets net/http add its own
//     `Accept-Encoding: gzip` AND transparently decompress the response, so the SSE
//     scanner still reads plaintext lines.
//
// The transport installs Bun's exact `gzip, deflate, br, zstd` value after the
// header order is computed.
func applyClaudeFetchHeaders(dst http.Header) {
	dst.Del("Accept-Language")
	dst.Del("Accept-Encoding")
}

// applyClaudePassthroughHeaders builds headers for a TRANSPARENT proxy of the extra
// Anthropic endpoints that Claude Code skills / code-execution depend on — the Files
// API (/v1/files, multipart uploads), /v1/skills, and /v1/agents|environments|sessions.
//
// Unlike applyClaudeHeaders it does NOT impose the messages-turn shape: it forwards the
// client's own Content-Type (so a multipart upload's boundary survives intact),
// Accept, Anthropic-Version, and CRITICALLY its Anthropic-Beta verbatim — the client
// alone knows which capability beta a given endpoint requires (files-api-2025-04-14,
// skills-2025-10-02, code-execution-2025-08-25, managed-agents-2026-04-01, …) and
// forcing our canonical messages-betas onto a Files upload would make the upstream
// reject it. The request body is never cloaked/rewritten. We still attach the account
// auth and the Claude Code identity fingerprint (X-App, X-Stainless-*, UA) so the call
// looks like it came from the same first-party client as the account's message turns.
func (c *Client) applyClaudePassthroughHeaders(dst http.Header, spec Request, id identity.Identity, stream bool) {
	token := accountprovider.Credential(claudeCredentialProvider(spec), spec.Token)
	apiKey := claudeUsesAPIKey(spec)

	// Auth, per credential type (mirrors applyClaudeHeaders).
	if apiKey {
		dst.Set("x-api-key", token)
	} else if token != "" {
		dst.Set("Authorization", "Bearer "+token)
	}
	dst.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")

	// Forward the client's content negotiation verbatim. Content-Type MUST be preserved
	// byte-for-byte — a multipart/form-data upload carries its boundary= parameter there,
	// and rewriting it to application/json corrupts the upload.
	if ct := strings.TrimSpace(spec.Headers.Get("Content-Type")); ct != "" {
		dst.Set("Content-Type", ct)
	} else if requestBodySize(spec) > 0 {
		dst.Set("Content-Type", "application/json")
	}
	if acc := strings.TrimSpace(spec.Headers.Get("Accept")); acc != "" {
		dst.Set("Accept", acc)
	} else if stream {
		dst.Set("Accept", "text/event-stream")
	} else {
		dst.Set("Accept", "application/json")
	}

	// Anthropic-Beta: prefer the client's own set verbatim (it knows the endpoint's
	// required capability beta). On the API-key path strip any oauth-* beta, which never
	// belongs on x-api-key traffic. Only fall back to the canonical set if the client
	// sent none at all (defensive — skills clients always send the relevant beta).
	clientBetas := spec.Headers.Values("Anthropic-Beta")
	if len(clientBetas) > 0 {
		var betas []string
		seen := make(map[string]bool)
		for _, hv := range clientBetas {
			for _, b := range splitCSV(hv) {
				lb := strings.ToLower(b)
				if apiKey && strings.HasPrefix(lb, "oauth") {
					continue
				}
				if seen[lb] {
					continue
				}
				seen[lb] = true
				betas = append(betas, b)
			}
		}
		if len(betas) > 0 {
			dst.Set("Anthropic-Beta", strings.Join(betas, ","))
		}
	} else if apiKey {
		dst.Set("Anthropic-Beta", claudeAPIKeyBetas)
	} else {
		dst.Set("Anthropic-Beta", claudeOAuthBetas)
	}

	// Anthropic-Version: forward the client's, else the canonical default.
	if v := strings.TrimSpace(spec.Headers.Get("Anthropic-Version")); v != "" {
		dst.Set("Anthropic-Version", v)
	} else {
		dst.Set("Anthropic-Version", claudeAnthropicVersion)
	}

	// Claude Code identity fingerprint (same axes as the message-turn path) so the
	// passthrough call is coherent with the account's other traffic.
	dst.Set("X-App", "cli")
	dst.Set("X-Stainless-Retry-Count", "0")
	dst.Set("X-Stainless-Runtime", "node")
	dst.Set("X-Stainless-Lang", "js")
	dst.Set("X-Stainless-Timeout", "600")
	dst.Set("X-Stainless-OS", id.StainlessOS)
	dst.Set("X-Stainless-Arch", id.StainlessArch)
	claudeVer := c.cfgSnapshot().ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
	dst.Set("X-Stainless-Package-Version", c.cfgSnapshot().ClaudeStainlessVersionOrDefault(id.StainlessPackageVersion))
	dst.Set("X-Stainless-Runtime-Version", c.cfgSnapshot().ClaudeNodeVersionOrDefault(id.NodeVersion))
	dst.Set("X-Claude-Code-Session-Id", claudeSessionID(spec.Headers, nil, id))
	forwardClaudeAgentContextHeaders(dst, spec.Headers)
	dst.Set("User-Agent", id.ClaudeUserAgentVersionForEntrypoint(claudeVer, claudeEntrypoint(spec.Headers, nil)))
	applyClaudeFetchHeaders(dst)
}

// forwardClaudeAgentContextHeaders preserves Claude Code 2.1.226's subagent
// ancestry only when it was supplied by the downstream client. Values are
// protocol metadata rather than identity fields: the relay does not invent them.
// Keep the validation stricter than net/http's generic header validation so an
// opaque value cannot smuggle control bytes or an unbounded payload upstream.
func forwardClaudeAgentContextHeaders(dst, src http.Header) {
	for _, name := range []string{claudeAgentIDHeader, claudeParentAgentHeader} {
		if value := validClaudeAgentContextValue(src.Get(name)); value != "" {
			dst.Set(name, value)
		}
	}
}

func validClaudeAgentContextValue(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return ""
		}
	}
	return value
}

// claudeEntrypoint projects the real downstream launch mode while discarding
// the downstream version. Claude 2.1.226 uses sdk-cli for `claude -p`/Agent SDK
// and cli for the interactive terminal.
func claudeEntrypoint(headers http.Header, body []byte) string {
	if strings.Contains(strings.ToLower(headers.Get("User-Agent")), "(external, sdk-cli)") ||
		bytes.Contains(body, []byte("cc_entrypoint=sdk-cli")) {
		return "sdk-cli"
	}
	return "cli"
}

// mergeBetas decides the Anthropic-Beta header to send upstream. It PREFERS the
// downstream client's own beta set, falling back to our canonical official-Claude-
// Code list only when the client sent none.
//
// This ordering is critical for correctness. A capability beta such as
// token-efficient-tools, structured-outputs or redact-thinking changes the wire
// SHAPE of the response (tool-call encoding, output framing, thinking blocks). The
// downstream client's parser is built for exactly the betas IT requested; forcing an
// extra one it never opted into makes Anthropic emit a stream/JSON the client cannot
// read — surfacing downstream as "API Error: Failed to parse JSON" or "API returned
// an empty or malformed response (HTTP 200)". The previous behavior (always send the
// full canonical superset, then union the client's) did exactly that whenever the
// client's Claude Code version sent a smaller/different beta set than our hardcoded
// list — frequent under auto mode, which leans on tools and thinking. The reference
// relay (other_cpa) takes the client's header verbatim for the same reason.
//
// We still guarantee the markers the Claude Code transport itself relies on:
// oauth-2025-04-20 on OAuth traffic (an auth/permission marker, not a format
// changer) and interleaved-thinking (which Claude Code always sends, so this is a
// no-op for it). On the API-key path dropOAuth strips any oauth-* beta, which never
// belongs on x-api-key traffic.
func mergeBetas(canonical string, clientHeaders http.Header, dropOAuth bool) string {
	var base []string
	if clientHeaders != nil {
		for _, hv := range clientHeaders.Values("Anthropic-Beta") {
			base = append(base, splitCSV(hv)...)
		}
	}
	if len(base) == 0 {
		// Client sent no Anthropic-Beta (e.g. our own count_tokens / models probe):
		// fall back to the canonical official-Claude-Code fingerprint.
		base = splitCSV(canonical)
	}

	seen := make(map[string]bool, len(base)+2)
	out := make([]string, 0, len(base)+2)
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b == "" {
			return
		}
		lb := strings.ToLower(b)
		if dropOAuth && strings.HasPrefix(lb, "oauth") {
			return
		}
		if seen[lb] {
			return
		}
		seen[lb] = true
		out = append(out, b)
	}
	for _, b := range base {
		add(b)
	}
	// Ensure the OAuth marker on OAuth traffic (required for the sk-ant-oat path).
	if !dropOAuth {
		add("oauth-2025-04-20")
	}
	// Ensure interleaved-thinking is present, mirroring the reference relay. Claude
	// Code already sends it, so this never actually adds a beta the real client
	// lacks; it only backstops minimal/non-CLI callers.
	hasInterleaved := false
	for lb := range seen {
		if strings.HasPrefix(lb, "interleaved-thinking") {
			hasInterleaved = true
			break
		}
	}
	if !hasInterleaved {
		add("interleaved-thinking-2025-05-14")
	}
	return strings.Join(out, ",")
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func appendClaudeBeta(h http.Header, beta string) {
	beta = strings.TrimSpace(beta)
	if beta == "" {
		return
	}
	current := splitCSV(h.Get("Anthropic-Beta"))
	for _, existing := range current {
		if strings.EqualFold(existing, beta) {
			return
		}
	}
	current = append(current, beta)
	h.Set("Anthropic-Beta", strings.Join(current, ","))
}

// claudeZeroMaxTokensPrewarm reports whether body is an Anthropic cache pre-warm, i.e.
// carries an explicit "max_tokens": 0.
//
// Anthropic documents this exact shape for pre-warming the prompt cache: the API reads the
// prompt, writes the cache at each cache_control breakpoint, and returns an empty content
// array with stop_reason "max_tokens" and zero output tokens billed. It is deliberately NOT
// inferred from anything else (a caller flag, the path, a header) because the zero is the
// whole contract — any code that then treats max_tokens:0 as "unset" and substitutes a
// default silently turns a free zero-output call into a full billable generation.
func claudeZeroMaxTokensPrewarm(body []byte) bool {
	var root map[string]interface{}
	if decodeClaudeJSONObject(body, &root) != nil {
		return false
	}
	raw, ok := root["max_tokens"]
	if !ok {
		return false
	}
	// decodeClaudeJSONObject uses json.Number, so match that first; float64 covers a caller
	// that decoded the body itself. A string "0" is deliberately NOT a match: Anthropic
	// requires a number here, so a string is a malformed body, not a pre-warm.
	switch value := raw.(type) {
	case json.Number:
		parsed, err := value.Float64()
		return err == nil && parsed == 0
	case float64:
		return value == 0
	default:
		return false
	}
}

const claudeBillingHeaderPrefix = "x-anthropic-billing-header:"

func (c *Client) normalizeClaudeMessagesSpec(spec Request) Request {
	var root map[string]interface{}
	if decodeClaudeJSONObject(requestBody(spec), &root) != nil {
		return spec
	}

	c.normalizeClaudeMessagesMetadata(root, spec)
	if betas := extractClaudeBodyBetas(root["betas"]); len(betas) > 0 {
		delete(root, "betas")
		spec.Headers = cloneHeaders(spec.Headers)
		spec.Headers.Add("Anthropic-Beta", strings.Join(betas, ","))
	} else if _, ok := root["betas"]; ok {
		delete(root, "betas")
	}

	sanitizeClaudeWebSearchTools(root)
	sanitizeClaudeHistory(root)
	normalizeClaudeThinkingCompatibility(root, claudeMessagesModel(root, spec.Model))
	anthropicwire.NormalizeCacheControlTTL(root)

	body := requestBody(spec)
	// Always perform the final Claude serializer pass, even when no semantic
	// normalizer changed the request. Earlier sjson/continuation/moderation steps
	// may have appended fields or used Go's map order; allowing that body through
	// unchanged exposes a stable non-Bun wire signature on otherwise native calls.
	if marshaled, err := anthropicwire.MarshalPreservingOrder(body, root); err == nil {
		body = marshaled
	}
	setRequestBody(&spec, body)
	return spec
}

// decodeClaudeJSONObject keeps arbitrary-precision JSON number text intact for
// the messages normalizers below. They mutate maps and may re-marshal the entire
// request, so decoding through float64 would corrupt large integer tool inputs.
// The trailing decode matches json.Unmarshal's requirement of one JSON value.
func decodeClaudeJSONObject(body []byte, root *map[string]interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(root); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

// normalizeClaudeMessagesMetadata preserves the exact current Claude Code
// metadata surface while replacing downstream identity. Captured 2.1.226 sends
// exactly metadata.user_id, whose value is a JSON string; its session_id is the
// same UUID as X-Claude-Code-Session-Id. Generic API-key SDK traffic keeps a plain
// opaque user id, while OAuth and native Claude Code requests use the JSON shape.
func (c *Client) normalizeClaudeMessagesMetadata(root map[string]interface{}, spec Request) bool {
	metadata, hasMetadata := root["metadata"].(map[string]interface{})
	token := accountprovider.Credential(claudeCredentialProvider(spec), spec.Token)
	claudeCode := (token != "" && !claudeUsesAPIKey(spec)) || claudeRootHasCodeIdentity(root)
	if !hasMetadata && !claudeCode {
		return false
	}
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)
	userID := id.UserID
	if claudeCode {
		sessionID := claudeSessionID(spec.Headers, requestBody(spec), id)
		userID = fmt.Sprintf(`{"device_id":%q,"account_uuid":%q,"session_id":%q}`, id.UserID, "", sessionID)
	}
	changed := !hasMetadata || metadata["user_id"] != userID || len(metadata) != 1
	for key := range metadata {
		if key != "user_id" {
			delete(metadata, key)
		}
	}
	metadata["user_id"] = userID
	root["metadata"] = metadata
	return changed
}

func claudeRootHasCodeIdentity(root map[string]interface{}) bool {
	system, ok := root["system"].([]interface{})
	if !ok {
		return false
	}
	for i, block := range system {
		if i >= 2 {
			break
		}
		item, ok := block.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := item["text"].(string)
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, claudeBillingHeaderPrefix) ||
			strings.HasPrefix(text, "You are a Claude agent, built on Anthropic's Claude Agent SDK.") ||
			strings.HasPrefix(text, "You are Claude Code, Anthropic's official CLI for Claude.") {
			return true
		}
	}
	return false
}

func extractClaudeBodyBetas(v interface{}) []string {
	switch t := v.(type) {
	case string:
		return splitCSV(t)
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, splitCSV(s)...)
			}
		}
		return out
	default:
		return nil
	}
}

func cloneHeaders(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	return h.Clone()
}

func sanitizeClaudeWebSearchTools(root map[string]interface{}) bool {
	tools, ok := root["tools"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for _, item := range tools {
		tool, ok := item.(map[string]interface{})
		if !ok || !isClaudeWebSearchTool(tool) {
			continue
		}
		for _, key := range []string{"allowed_domains", "blocked_domains"} {
			if arr, ok := tool[key].([]interface{}); ok && len(arr) == 0 {
				delete(tool, key)
				changed = true
			}
		}
	}
	return changed
}

func isClaudeWebSearchTool(tool map[string]interface{}) bool {
	typ, _ := tool["type"].(string)
	name, _ := tool["name"].(string)
	return strings.HasPrefix(typ, "web_search") || strings.HasPrefix(name, "web_search")
}

func sanitizeClaudeHistory(root map[string]interface{}) bool {
	msgs, ok := root["messages"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for _, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		out := make([]interface{}, 0, len(blocks))
		for _, block := range blocks {
			bm, ok := block.(map[string]interface{})
			if !ok {
				out = append(out, block)
				continue
			}
			if isEmptyClaudeThinkingBlock(bm) {
				changed = true
				continue
			}
			if typ, _ := bm["type"].(string); typ == "tool_use" {
				if stripClaudeToolUseProvenance(bm) {
					changed = true
				}
			}
			out = append(out, bm)
		}
		if len(out) != len(blocks) {
			m["content"] = out
		}
	}
	return changed
}

func isEmptyClaudeThinkingBlock(block map[string]interface{}) bool {
	typ, _ := block["type"].(string)
	if typ != "thinking" {
		return false
	}
	text, _ := block["text"].(string)
	signature, _ := block["signature"].(string)
	return strings.TrimSpace(text) == "" && strings.TrimSpace(signature) == ""
}

func stripClaudeToolUseProvenance(block map[string]interface{}) bool {
	changed := false
	for _, key := range []string{"signature", "thoughtSignature", "thought_signature", "model"} {
		if _, ok := block[key]; ok {
			delete(block, key)
			changed = true
		}
	}
	extra, ok := block["extra_content"].(map[string]interface{})
	if !ok {
		return changed
	}
	google, ok := extra["google"].(map[string]interface{})
	if !ok {
		return changed
	}
	if _, ok := google["thought_signature"]; ok {
		delete(google, "thought_signature")
		changed = true
	}
	if len(google) == 0 {
		delete(extra, "google")
		changed = true
	}
	if len(extra) == 0 {
		delete(block, "extra_content")
		changed = true
	}
	return changed
}

func claudeMessagesModel(root map[string]interface{}, fallback string) string {
	if model, ok := root["model"].(string); ok && strings.TrimSpace(model) != "" {
		return model
	}
	return fallback
}

func normalizeClaudeThinkingCompatibility(root map[string]interface{}, model string) bool {
	changed := false
	model = canonicalClaudeCompatibilityModel(model)
	thinking, hasThinking := root["thinking"].(map[string]interface{})
	thinkingType, _ := thinking["type"].(string)
	thinkingType = strings.ToLower(strings.TrimSpace(thinkingType))

	// Current adaptive-only models reject the legacy manual budget form. Preserve
	// an optional display preference while converting the control plane to the
	// current adaptive contract. Fable/Mythos cannot disable thinking at all, and
	// Opus 5 cannot disable it at xhigh/max effort.
	switch {
	case claudeAdaptiveOnlyModel(model) && (thinkingType == "enabled" || thinkingType == "auto"):
		thinking["type"] = "adaptive"
		if _, ok := thinking["budget_tokens"]; ok {
			delete(thinking, "budget_tokens")
		}
		changed = true
		thinkingType = "adaptive"
	case claudeAlwaysThinkingModel(model) && thinkingType == "disabled":
		thinking["type"] = "adaptive"
		delete(thinking, "budget_tokens")
		changed = true
		thinkingType = "adaptive"
	case claudeOpus5Model(model) && thinkingType == "disabled" && claudeHighOnlyThinkingConflict(root):
		thinking["type"] = "adaptive"
		delete(thinking, "budget_tokens")
		changed = true
		thinkingType = "adaptive"
	case claudeAdaptiveModel(model) && thinkingType == "auto":
		thinking["type"] = "adaptive"
		delete(thinking, "budget_tokens")
		changed = true
		thinkingType = "adaptive"
	}

	if toolChoice, ok := root["tool_choice"].(map[string]interface{}); ok {
		switch typ, _ := toolChoice["type"].(string); typ {
		case "any", "tool":
			// Forced tool choice is incompatible only with legacy manual
			// extended thinking. Adaptive thinking supports it and must remain
			// attached on current Claude models.
			if hasThinking && thinkingType == "enabled" {
				delete(root, "thinking")
				changed = true
				if !claudeAdaptiveModel(model) && removeOutputConfigEffort(root) {
					changed = true
				}
			}
			return changed
		}
	}

	// Fable/Opus/Sonnet 5 and the current Opus 4.7+ line reject non-default
	// sampling values on every request. Omitting all three parameters is the
	// stable wire representation of their defaults.
	if claudeRejectsSamplingParameters(model) {
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			if _, ok := root[key]; ok {
				delete(root, key)
				changed = true
			}
		}
		return changed
	}

	if !hasThinking {
		return changed
	}
	switch thinkingType {
	case "enabled", "adaptive", "auto":
		// Older models also reject explicit sampling controls while thinking is
		// active. Remove them instead of forcing temperature=1, which is still an
		// explicit parameter and can be rejected by newer API revisions.
		for _, key := range []string{"temperature", "top_p", "top_k"} {
			if _, ok := root[key]; ok {
				delete(root, key)
				changed = true
			}
		}
	}
	return changed
}

func canonicalClaudeCompatibilityModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if index := strings.IndexByte(model, '['); index >= 0 {
		model = model[:index]
	}
	model = strings.TrimSuffix(model, "-thinking")
	return strings.ReplaceAll(model, ".", "-")
}

func claudeCompatibilityModelIs(model, exact string) bool {
	return model == exact || strings.HasPrefix(model, exact+"-20")
}

func claudeAdaptiveOnlyModel(model string) bool {
	for _, exact := range []string{
		"claude-fable-5",
		"claude-mythos-5",
		"claude-mythos-preview",
		"claude-opus-5",
		"claude-sonnet-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
	} {
		if claudeCompatibilityModelIs(model, exact) {
			return true
		}
	}
	return false
}

func claudeAdaptiveModel(model string) bool {
	if claudeAdaptiveOnlyModel(model) {
		return true
	}
	return claudeCompatibilityModelIs(model, "claude-opus-4-6") ||
		claudeCompatibilityModelIs(model, "claude-sonnet-4-6")
}

func claudeAlwaysThinkingModel(model string) bool {
	return claudeCompatibilityModelIs(model, "claude-fable-5") ||
		claudeCompatibilityModelIs(model, "claude-mythos-5") ||
		claudeCompatibilityModelIs(model, "claude-mythos-preview")
}

func claudeOpus5Model(model string) bool {
	return claudeCompatibilityModelIs(model, "claude-opus-5")
}

func claudeHighOnlyThinkingConflict(root map[string]interface{}) bool {
	output, _ := root["output_config"].(map[string]interface{})
	effort, _ := output["effort"].(string)
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "xhigh", "max":
		return true
	default:
		return false
	}
}

func claudeRejectsSamplingParameters(model string) bool {
	for _, exact := range []string{
		"claude-fable-5",
		"claude-mythos-5",
		"claude-mythos-preview",
		"claude-opus-5",
		"claude-opus-4-8",
		"claude-opus-4-7",
		"claude-sonnet-5",
	} {
		if claudeCompatibilityModelIs(model, exact) {
			return true
		}
	}
	return false
}

func removeOutputConfigEffort(root map[string]interface{}) bool {
	output, ok := root["output_config"].(map[string]interface{})
	if !ok {
		return false
	}
	changed := false
	if _, ok := output["effort"]; ok {
		delete(output, "effort")
		changed = true
	}
	if len(output) == 0 {
		delete(root, "output_config")
		changed = true
	}
	return changed
}

// claudeSessionID derives the virtual X-Claude-Code-Session-Id, bound to the account.
// It ROTATES the way a real user's does — a new CLI run sends a new session id, so we
// emit a new (account-bound) one — while staying STABLE within a single run/conversation:
//   - downstream sent one (real Claude Code): derive from it, so every sub-agent request
//     of one multi-agent run (same incoming id) maps to the same virtual id.
//   - downstream sent none (OpenAI-compat→Claude, non-CC clients, internal probes): derive
//     from the conversation anchor (the first-user-message hash, stable across a
//     conversation's turns and distinct across conversations). This avoids the previous
//     behavior where ALL such traffic collapsed onto one static-forever per-account
//     session id — to the upstream, one immortal session doing an implausible amount of
//     work, an obvious anomaly.
//   - neither available: fall back to the account's stable id.
//
// Seeding with id.MachineID keeps it per-account isolated and never leaks the real value.
func claudeSessionID(downstream http.Header, body []byte, id identity.Identity) string {
	if downstream != nil {
		if v := strings.TrimSpace(downstream.Get("X-Claude-Code-Session-Id")); v != "" {
			return identity.DerivedUUID(id.MachineID, v)
		}
	}
	if anchor := routing.ConversationAnchor(body); anchor != "" {
		return identity.DerivedUUID(id.MachineID, "claude-session-anchor\x00"+anchor)
	}
	return id.ClaudeSessionID
}

func bodyStreamTrue(body []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return false
	}
	if v, ok := root["stream"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			return b
		}
	}
	return false
}
