package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"codex-account-pool/internal/anthropicwire"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/routing"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Official Claude Code anti-fingerprint constants (see cliproxyapi-reference).
const (
	claudeAnthropicVersion = "2023-06-01"
	// OAuth (Claude Pro/Max) carries the oauth beta; API keys must not.
	claudeOAuthBetas  = "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28"
	claudeAPIKeyBetas = "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,structured-outputs-2025-12-15,fast-mode-2026-02-01,redact-thinking-2026-02-12,token-efficient-tools-2026-03-28"
)

func claudeBaseURL(cfg config.Config) string {
	if b := strings.TrimSpace(cfg.ClaudeUpstreamBaseURL); b != "" {
		return strings.TrimRight(b, "/")
	}
	return "https://api.anthropic.com"
}

// claudeUsesAPIKey reports whether the credential is an Anthropic API key
// (sk-ant-api...) rather than an OAuth access token (sk-ant-oat...).
func claudeUsesAPIKey(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), "sk-ant-api")
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
		stream = bodyStreamTrue(spec.Body)
	}

	// === THINKING INJECTION: Apply thinking configuration before forwarding ===
	// Only inject when:
	// 1. Not in passthrough mode (passthrough = opaque body, no thinking injection)
	// 2. Thinking is enabled in config
	// 3. Path is a messages endpoint (not files/skills/agents)
	if !spec.PassThrough && c.cfg.ThinkingEnabled && strings.Contains(path, "/v1/messages") {
		spec.Body = c.applyThinkingConfig(spec.Body, "claude", spec.Model, spec.Account)
	}
	if !spec.PassThrough && strings.Contains(path, "/v1/messages") {
		spec = c.normalizeClaudeMessagesSpec(spec)
	}

	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)

	applyHeaders := c.applyClaudeHeaders
	if spec.PassThrough {
		applyHeaders = c.applyClaudePassthroughHeaders
	}

	// Route through the curl_cffi sidecar when the account is bound to one, so the
	// Anthropic upstream sees a real client TLS/JA3 + HTTP2 fingerprint instead of
	// the Go standard library's. Although Anthropic (unlike chatgpt.com) has no
	// Cloudflare challenge wall, the stdlib transport's fingerprint is itself a
	// relay-detection signal — which is exactly what binding "curl_cffi_sidecar"
	// out on an account is meant to defeat (and what the admin UI advertises). The
	// ClaudeForceDirect escape hatch forces the direct/proxy transport for
	// deployments that cannot run the sidecar against api.anthropic.com.
	if spec.Egress.Type == "curl_cffi_sidecar" && !c.cfgSnapshot().ClaudeForceDirect {
		built := http.Header{}
		applyHeaders(built, spec, id, stream)
		// The sidecar negotiates and auto-decompresses encoding itself (and a real
		// Node/Chrome client never asks for "identity"), so drop our identity hint
		// and let the impersonated transport present its native Accept-Encoding.
		built.Del("Accept-Encoding")
		claudeSpec := spec
		claudeSpec.Method = firstNonEmpty(spec.Method, http.MethodPost)
		// Bound the call by the full request timeout (like the direct path), not the
		// shorter sidecar connect budget, so a long streaming completion is not cut
		// off mid-stream.
		timeout := c.cfg.RequestTimeout()
		if st := c.cfg.SidecarTimeout(); st > timeout {
			timeout = st
		}
		// JA3 the sidecar replays for Claude. DEFAULT IS CHROME (""): see resolveClaudeJA3
		// — the real claude-cli/Node JA3 is an explicit opt-in, not the default. When a
		// JA3 is set, the sidecar disables its impersonation default headers and uses
		// exactly the header set built above.
		ja3 := resolveClaudeJA3(c.cfgSnapshot().ClaudeJA3Override)
		return c.postViaSidecar(ctx, claudeSpec, target, built, timeout, ja3)
	}

	req, err := http.NewRequestWithContext(ctx, firstNonEmpty(spec.Method, http.MethodPost), target, bytes.NewReader(spec.Body))
	if err != nil {
		return nil, err
	}
	applyHeaders(req.Header, spec, id, stream)

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
	// is set; httpClientForEgress does not understand the sidecar type, so present it
	// as a plain direct transport (the sidecar's own proxy chaining does not apply).
	directSpec := spec
	if directSpec.Egress.Type == "curl_cffi_sidecar" {
		directSpec.Egress.Type = "direct"
	}
	client, err := c.httpClientForEgress(directSpec)
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

// resolveClaudeJA3 resolves the TLS/JA3 fingerprint the curl_cffi sidecar replays for
// Claude/Anthropic traffic from the operator's claude_ja3 setting.
//
// THE DEFAULT IS CHROME (""), i.e. the sidecar's native impersonation — NOT the real
// claude-cli JA3. This is a deliberate, evidence-based choice:
//   - The mature reference relays (e.g. justlovemaki/AIClient2API, a Go uTLS sidecar)
//     impersonate Chrome for upstreams that validate JA3/JA4, rather than replaying each
//     client's real fingerprint — Chrome is what curl_cffi/uTLS reproduce natively and
//     what Cloudflare edges whitelist.
//   - Empirical research into Anthropic's third-party-client detection (mrcattusdev,
//     tested by routing OpenCode's TLS through Node vs Bun) found the block is keyed on
//     SYSTEM-PROMPT CONTENT, not TLS — changing the TLS stack did not change the outcome.
//     We already neutralize that surface in the cloak layer (the Claude Code identity
//     system block), so matching the real claude-cli JA3 buys no measurable
//     anti-detection benefit, while a best-effort replay of a non-native (Node/undici)
//     profile risks presenting an imperfect THIRD fingerprint that is neither Chrome nor
//     real claude-cli.
//   - api.anthropic.com plainly accepts the Chrome impersonation today (it has no
//     aggressive CF challenge wall for the API), and accepts the real Node fingerprint
//     too (the real CLI uses it), so Chrome is the lower-risk default.
//
// Operators who specifically want TLS↔UA coherence can OPT IN: the sentinels
// "claude-cli"/"claude_cli"/"real"/"native"/"node" select identity.ClaudeJA3 (the
// captured real claude-cli fingerprint, ja3 hash d871d02c…), or set claude_ja3 to an
// explicit JA3 string. "" / "off"/"none"/"disabled"/"-"/"chrome"/"browser" all keep
// Chrome.
func resolveClaudeJA3(override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "", "off", "none", "disabled", "-", "chrome", "browser":
		return "" // sidecar's native Chrome impersonation (default)
	case "claude-cli", "claude_cli", "real", "native", "node":
		return identity.ClaudeJA3
	default:
		return strings.TrimSpace(override)
	}
}

// applyClaudeHeaders builds the official Claude Code request headers from
// scratch using the account-bound virtual identity, so the upstream sees a
// consistent, first-party-looking client instead of the relay/host machine.
func (c *Client) applyClaudeHeaders(dst http.Header, spec Request, id identity.Identity, stream bool) {
	token := firstNonEmpty(spec.Token.AccessToken, spec.Token.OpenAIAPIKey)
	apiKey := claudeUsesAPIKey(token)

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
		dst.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
		dst.Set("Anthropic-Beta", mergeBetas(claudeAPIKeyBetas, spec.Headers, true))
	} else {
		if token != "" {
			dst.Set("Authorization", "Bearer "+token)
		}
		dst.Set("Anthropic-Beta", mergeBetas(claudeOAuthBetas, spec.Headers, false))
	}
	dst.Set("Anthropic-Version", claudeAnthropicVersion)
	dst.Set("X-App", "cli")
	dst.Set("X-Stainless-Retry-Count", "0")
	dst.Set("X-Stainless-Runtime", "node")
	dst.Set("X-Stainless-Lang", "js")
	dst.Set("X-Stainless-Timeout", "600")
	dst.Set("X-Stainless-OS", id.StainlessOS)
	dst.Set("X-Stainless-Arch", id.StainlessArch)
	// Per-account version axes: an explicit operator override (config) still wins
	// globally, but absent one each account presents its own diverse, self-consistent
	// values so N accounts are not byte-identical (a fleet signal). The claude-cli
	// version (UA) and the @anthropic-ai/sdk "Stainless" package version are SEPARATE
	// axes — the real client sends e.g. claude-cli/2.1.159 with X-Stainless-Package-
	// Version: 0.94.0 — so they must not be conflated.
	claudeVer := c.cfgSnapshot().ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
	dst.Set("X-Stainless-Package-Version", c.cfgSnapshot().ClaudeStainlessVersionOrDefault(id.StainlessPackageVersion))
	dst.Set("X-Stainless-Runtime-Version", c.cfgSnapshot().ClaudeNodeVersionOrDefault(id.NodeVersion))
	dst.Set("X-Claude-Code-Session-Id", claudeSessionID(spec.Headers, spec.Body, id))
	dst.Set("x-client-request-id", randomUUID())
	dst.Set("User-Agent", id.ClaudeUserAgentVersion(claudeVer))
	if stream {
		dst.Set("Accept", "text/event-stream")
		// Uncompressed so the downstream SSE scanner can read it line-by-line.
		dst.Set("Accept-Encoding", "identity")
	} else {
		dst.Set("Accept", "application/json")
	}
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
	token := firstNonEmpty(spec.Token.AccessToken, spec.Token.OpenAIAPIKey)
	apiKey := claudeUsesAPIKey(token)

	// Auth, per credential type (mirrors applyClaudeHeaders).
	if apiKey {
		dst.Set("x-api-key", token)
		dst.Set("Anthropic-Dangerous-Direct-Browser-Access", "true")
	} else if token != "" {
		dst.Set("Authorization", "Bearer "+token)
	}

	// Forward the client's content negotiation verbatim. Content-Type MUST be preserved
	// byte-for-byte — a multipart/form-data upload carries its boundary= parameter there,
	// and rewriting it to application/json corrupts the upload.
	if ct := strings.TrimSpace(spec.Headers.Get("Content-Type")); ct != "" {
		dst.Set("Content-Type", ct)
	} else if len(spec.Body) > 0 {
		dst.Set("Content-Type", "application/json")
	}
	if acc := strings.TrimSpace(spec.Headers.Get("Accept")); acc != "" {
		dst.Set("Accept", acc)
	} else if stream {
		dst.Set("Accept", "text/event-stream")
	} else {
		dst.Set("Accept", "application/json")
	}
	if stream {
		// Uncompressed so a streamed body can be scanned line-by-line downstream.
		dst.Set("Accept-Encoding", "identity")
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
	dst.Set("x-client-request-id", randomUUID())
	dst.Set("User-Agent", id.ClaudeUserAgentVersion(claudeVer))
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

const claudeCCHSeed uint64 = 0x6E52736AC806831E
const claudeBillingHeaderPrefix = "x-anthropic-billing-header:"

var claudeBillingHeaderCCHPattern = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

func (c *Client) normalizeClaudeMessagesSpec(spec Request) Request {
	var root map[string]interface{}
	if json.Unmarshal(spec.Body, &root) != nil {
		return spec
	}

	changed := false
	if _, ok := root["metadata"]; ok {
		delete(root, "metadata")
		changed = true
	}
	if betas := extractClaudeBodyBetas(root["betas"]); len(betas) > 0 {
		delete(root, "betas")
		spec.Headers = cloneHeaders(spec.Headers)
		spec.Headers.Add("Anthropic-Beta", strings.Join(betas, ","))
		changed = true
	} else if _, ok := root["betas"]; ok {
		delete(root, "betas")
		changed = true
	}

	if sanitizeClaudeWebSearchTools(root) {
		changed = true
	}
	if sanitizeClaudeHistory(root) {
		changed = true
	}
	if normalizeClaudeThinkingCompatibility(root) {
		changed = true
	}
	if anthropicwire.NormalizeCacheControlTTL(root) {
		changed = true
	}

	body := spec.Body
	if changed {
		if marshaled, err := json.Marshal(root); err == nil {
			body = marshaled
		}
	}
	if c.claudeCCHSigningEnabled(spec) {
		body = signClaudeBillingCCH(body)
	}
	spec.Body = body
	return spec
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

func normalizeClaudeThinkingCompatibility(root map[string]interface{}) bool {
	changed := false
	if toolChoice, ok := root["tool_choice"].(map[string]interface{}); ok {
		switch typ, _ := toolChoice["type"].(string); typ {
		case "any", "tool":
			if _, ok := root["thinking"]; ok {
				delete(root, "thinking")
				changed = true
			}
			if removeOutputConfigEffort(root) {
				changed = true
			}
			return changed
		}
	}

	thinking, ok := root["thinking"].(map[string]interface{})
	if !ok {
		return changed
	}
	switch typ, _ := thinking["type"].(string); typ {
	case "enabled", "adaptive", "auto":
		if temp, ok := root["temperature"].(float64); !ok || temp != 1 {
			root["temperature"] = 1.0
			changed = true
		}
		for _, key := range []string{"top_p", "top_k"} {
			if _, ok := root[key]; ok {
				delete(root, key)
				changed = true
			}
		}
	}
	return changed
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

func (c *Client) claudeCCHSigningEnabled(spec Request) bool {
	if !c.cfgSnapshot().ClaudeCCHSigning {
		return false
	}
	token := firstNonEmpty(spec.Token.AccessToken, spec.Token.OpenAIAPIKey)
	return token != "" && !claudeUsesAPIKey(token)
}

func signClaudeBillingCCH(body []byte) []byte {
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(strings.TrimSpace(billingHeader), claudeBillingHeaderPrefix) {
		return body
	}
	if !claudeBillingHeaderCCHPattern.MatchString(billingHeader) {
		return body
	}
	unsignedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(billingHeader, "cch=00000;")
	unsignedBody, err := sjson.SetBytes(body, "system.0.text", unsignedBillingHeader)
	if err != nil {
		return body
	}
	cch := fmt.Sprintf("%05x", xxHash64.Checksum(claudeCCHSigningBody(unsignedBody), claudeCCHSeed)&0xFFFFF)
	signedBillingHeader := claudeBillingHeaderCCHPattern.ReplaceAllString(unsignedBillingHeader, "cch="+cch+";")
	signedBody, err := sjson.SetBytes(unsignedBody, "system.0.text", signedBillingHeader)
	if err != nil {
		return unsignedBody
	}
	return signedBody
}

func claudeCCHSigningBody(unsignedBody []byte) []byte {
	var root map[string]interface{}
	if json.Unmarshal(unsignedBody, &root) != nil {
		return unsignedBody
	}
	hasToolOrSystemCache := claudeBlockListHasCacheControl(root["tools"]) || claudeBlockListHasCacheControl(root["system"])
	hasMessageCache := claudeMessagesHaveCacheControl(root["messages"])
	if !hasToolOrSystemCache && !hasMessageCache {
		return unsignedBody
	}

	if hasToolOrSystemCache {
		delete(root, "messages")
	} else if prefix, ok := claudeMessagesPrefixThroughCacheControl(root["messages"]); ok {
		root["messages"] = prefix
	} else {
		return unsignedBody
	}
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return unsignedBody
}

func claudeBlockListHasCacheControl(v interface{}) bool {
	blocks, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, block := range blocks {
		if bm, ok := block.(map[string]interface{}); ok {
			if _, has := bm["cache_control"]; has {
				return true
			}
		}
	}
	return false
}

func claudeMessagesHaveCacheControl(v interface{}) bool {
	msgs, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := m["cache_control"]; has {
			return true
		}
		if blocks, ok := m["content"].([]interface{}); ok {
			for _, block := range blocks {
				if bm, ok := block.(map[string]interface{}); ok {
					if _, has := bm["cache_control"]; has {
						return true
					}
				}
			}
		}
	}
	return false
}

func claudeMessagesPrefixThroughCacheControl(v interface{}) ([]interface{}, bool) {
	msgs, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]interface{}, 0, len(msgs))
	for _, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if !ok {
			out = append(out, msg)
			continue
		}
		if _, has := m["cache_control"]; has {
			out = append(out, m)
			return out, true
		}
		blocks, ok := m["content"].([]interface{})
		if !ok {
			out = append(out, m)
			continue
		}
		for i, block := range blocks {
			if bm, ok := block.(map[string]interface{}); ok {
				if _, has := bm["cache_control"]; has {
					cp := make(map[string]interface{}, len(m))
					for k, v := range m {
						cp[k] = v
					}
					cp["content"] = blocks[:i+1]
					out = append(out, cp)
					return out, true
				}
			}
		}
		out = append(out, m)
	}
	return nil, false
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

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
