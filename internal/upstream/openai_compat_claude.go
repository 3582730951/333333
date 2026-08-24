package upstream

import (
	"net/http"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// claudeShapedCustomCall reports whether a custom-provider call must present the
// Claude Code client shape upstream.
//
// Why this is not simply `TransportProfile == claude_code`: the profile is inferred
// from the provider's id/name (inferredProviderTransportProfile), so a relay named
// "duckcoding", "88code" or any bare hostname lands on the generic profile even when
// the operator explicitly selected upstream_protocol=anthropic_messages. The generic
// profile then emits only Bearer+x-api-key+UA, with none of the markers a Claude-Code-
// mode relay gates on (X-App, X-Stainless-*, a session id, the client betas), so those
// relays reject or degrade the request. Anthropic Messages is only ever spoken by
// Claude Code and the Anthropic SDKs, so the protocol alone is sufficient evidence.
//
// UpstreamProtocol is authoritative when set. It is NOT set on the main inference path
// (callCustomAttempt builds its upstream.Request without it), so fall back to the
// literal upstream path, which that path always derives per protocol: only the three
// anthropic_messages entry points ever call with "/messages".
func claudeShapedCustomCall(spec Request) bool {
	if strings.TrimSpace(spec.TransportProfile) == storage.CustomProviderTransportClaudeCode {
		return true
	}
	switch strings.TrimSpace(spec.UpstreamProtocol) {
	case storage.CustomProviderProtocolAnthropicMessages:
		return true
	case storage.CustomProviderProtocolChatCompletions, storage.CustomProviderProtocolResponses:
		return false
	}
	if strings.TrimSpace(spec.TransportProfile) == storage.CustomProviderTransportCodexCLI {
		return false
	}
	path := strings.ToLower(strings.SplitN(spec.DownstreamPath, "?", 2)[0])
	return strings.Contains(path, "/messages")
}

// applyClaudeCodeCustomHeaders builds the Claude Code header set for a custom
// provider from scratch. It deliberately does NOT reuse applyClaudeHeaders, for one
// reason that would otherwise break working deployments: applyClaudeHeaders picks
// exactly one auth header per credential shape (x-api-key for API keys, Bearer for
// OAuth), and a custom provider's key is classified as an API key by
// accountprovider.EffectiveAuthMethod (provider != codex/claude ⇒ AuthMethodAPIKey).
// Routing custom traffic through it would therefore DROP Authorization: Bearer, which
// the generic path sends today and which most relays authenticate on. Relay auth
// conventions are not uniform — some read x-api-key, some Authorization — so both are
// sent, exactly as applyOpenAICompatHeaders already does.
//
// The identity axes (UA, X-App, X-Stainless-*, session id) come from the same
// account-bound virtual identity as the official Claude path, so a custom provider's
// traffic is self-consistent per account rather than leaking the host machine.
func (c *Client) applyClaudeCodeCustomHeaders(dst http.Header, spec Request, id identity.Identity, stream bool) {
	credential := accountprovider.Credential(claudeCredentialProvider(spec), spec.Token)
	// The credential's own shape decides both the auth header and the beta set, so the two
	// can never contradict each other. accountprovider.EffectiveAuthMethod is not usable
	// here: it classifies ANY custom-provider credential as an API key (provider is neither
	// codex nor claude), which would strip the OAuth marker from a genuine sk-ant-oat token.
	oauthCredential := strings.HasPrefix(credential, "sk-ant-oat")
	switch {
	case credential == "":
	case oauthCredential:
		// Anthropic OAuth token: Bearer only, exactly as the real client sends it.
		dst.Set("Authorization", "Bearer "+credential)
	case strings.HasPrefix(credential, "sk-ant-"):
		// First-party Anthropic API key: x-api-key only. Anthropic rejects a request
		// carrying both auth headers, so this must not be widened.
		dst.Set("x-api-key", credential)
	default:
		// Relay-issued key of arbitrary shape. Relay auth conventions are not uniform —
		// some read x-api-key, some Authorization — and the generic path already sends
		// both today, so keep both or every relay that authenticates on Authorization
		// (i.e. most of them) regresses to 401.
		dst.Set("Authorization", "Bearer "+credential)
		dst.Set("x-api-key", credential)
	}

	if requestBodySize(spec) > 0 {
		dst.Set("Content-Type", "application/json")
	}
	// Claude Code keeps Accept: application/json even when the response body is an
	// SSE stream; stream=true is carried in JSON. Using text/event-stream here made
	// custom Anthropic relays distinguishable from the built-in/native route.
	dst.Set("Accept", "application/json")

	if v := strings.TrimSpace(spec.Headers.Get("Anthropic-Version")); v != "" {
		dst.Set("Anthropic-Version", v)
	} else {
		dst.Set("Anthropic-Version", claudeAnthropicVersion)
	}
	dst.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")

	// Betas: keep the client's own set as the base (a capability beta changes the wire
	// shape of the response, and the downstream parser is built for exactly what it asked
	// for) and fall back to the canonical set only when the client sent none, so a minimal
	// API client still reaches a Claude-Code-gated relay. dropOAuth follows the credential
	// shape for the same reason applyClaudeHeaders does it: an API key cannot present the
	// OAuth client's permission marker, and pairing them is itself a rejectable mismatch.
	if oauthCredential {
		dst.Set("Anthropic-Beta", claudeBetasForRequest(claudeOAuthBetas, spec, false))
	} else {
		dst.Set("Anthropic-Beta", claudeBetasForRequest(claudeAPIKeyBetas, spec, true))
	}

	if strings.TrimSpace(spec.Headers.Get(claudeCacheDiagnosticsHeader)) != "" {
		appendClaudeBeta(dst, claudeCacheDiagnosticsBeta)
	}

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
	dst.Set("X-Claude-Code-Session-Id", customClaudeSessionID(spec, id))
	// Subagent ancestry is forwarded only when the downstream client supplied it.
	// Synthesizing it would claim every conversation is a subagent of some parent that
	// never itself makes a request — a shape real Claude Code never produces.
	forwardClaudeAgentContextHeaders(dst, spec.Headers)
	dst.Set("User-Agent", id.ClaudeUserAgentVersionForEntrypoint(claudeVer, claudeEntrypoint(spec.Headers, requestBody(spec))))
	applyClaudeFetchHeaders(dst)
}

// customClaudeSessionID derives the upstream session identity for a custom-provider
// Claude call. It is the fix for downstream sessions bleeding into each other when the
// pool holds fewer API keys than there are concurrent downstream conversations: with N
// conversations sharing one key, an upstream that keys conversation state by session id
// sees a single session and interleaves them. Every branch below is stable across a
// conversation's turns (so prompt caching and relay-side stickiness still work) while
// differing between conversations.
//
// Ordering is by strength of evidence. An explicit session header beats a body-derived
// anchor; a mere client id is weaker than the anchor because one client runs many
// conversations. Real values are never forwarded — each is replaced by a deterministic
// per-account UUID, so the downstream's own identifiers do not reach the upstream.
func customClaudeSessionID(spec Request, id identity.Identity) string {
	seed := identity.SessionSeed(id)
	if spec.Headers != nil {
		for _, header := range []string{"X-Claude-Code-Session-Id", "X-Session-ID"} {
			if value := strings.TrimSpace(spec.Headers.Get(header)); value != "" {
				return identity.DerivedUUID(seed, value)
			}
		}
	}
	if anchor := routing.ConversationAnchor(requestBody(spec)); anchor != "" {
		return identity.DerivedUUID(seed, "claude-session-anchor\x00"+anchor)
	}
	if spec.Headers != nil {
		for _, header := range []string{"X-Client-ID", "X-Pool-Client-ID"} {
			if value := strings.TrimSpace(spec.Headers.Get(header)); value != "" {
				return identity.DerivedUUID(seed, "claude-client\x00"+value)
			}
		}
	}
	return id.ClaudeSessionID
}

// customClaudeCookieJarKey narrows the cookie jar from per-account to per-conversation
// on Claude-shaped custom calls. Most relays are stateless JSON endpoints that set no
// cookies at all, in which case this is a no-op; when a relay DOES issue a session
// cookie, a jar shared by every conversation on one account pins them all to one
// upstream session, which is a second path to the same context bleeding that
// customClaudeSessionID addresses. The account/egress scoping of the incoming key is
// preserved so egress-bound state is unaffected.
func customClaudeCookieJarKey(spec Request, id identity.Identity) string {
	base := strings.TrimSpace(spec.CookieJarKey)
	if base == "" {
		base = spec.Account.ID + ":" + strings.TrimSpace(spec.Egress.ID)
	}
	return base + ":cc-session:" + customClaudeSessionID(spec, id)
}
