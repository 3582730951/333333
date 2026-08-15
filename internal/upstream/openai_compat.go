package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// openAICompatUserAgent is sent on custom-provider traffic. It mimics the official
// OpenAI Python SDK, which every OpenAI-compatible provider (DeepSeek, Kimi,
// OpenRouter, vLLM, …) expects — these are normal API endpoints, not clients we
// must fingerprint, so a stable generic SDK UA is both correct and low-risk.
const openAICompatUserAgent = "OpenAI/Python 1.61.0"

// IsCustomProvider reports whether a provider id names a custom OpenAI-compatible
// provider (anything other than the built-in codex/claude/kiro/antigravity upstreams or the empty
// legacy value). Exported so the API layer routes consistently with Client.Do.
func IsCustomProvider(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "", "codex", "claude", "kiro", "antigravity":
		return false
	default:
		return true
	}
}

func customCodexResponsesProfile(spec Request) bool {
	if spec.PassThrough || spec.MinimalProbe {
		return false
	}
	if strings.TrimSpace(spec.TransportProfile) != storage.CustomProviderTransportCodexCLI ||
		strings.TrimSpace(spec.UpstreamProtocol) != storage.CustomProviderProtocolResponses {
		return false
	}
	path := strings.ToLower(strings.SplitN(strings.TrimSpace(spec.DownstreamPath), "?", 2)[0])
	return strings.Contains(path, "/responses")
}

// prepareOpenAICompatCodexRequest makes the custom-provider Codex profile match
// the contract advertised by the console: headers, client_metadata, and the small
// set of Responses transport fields all come from one canonical identity. Generic
// native-Responses providers remain byte-for-byte pass-through.
func (c *Client) prepareOpenAICompatCodexRequest(spec *Request) error {
	if spec == nil || !customCodexResponsesProfile(*spec) {
		return nil
	}
	spec.codexResolvedClientVersion = c.resolveCodexClientVersion(*spec)
	if err := spec.loadBody(); err != nil {
		return err
	}
	isCompact := strings.Contains(strings.ToLower(spec.DownstreamPath), "/responses/compact")
	responsesLite := CodexRequestUsesResponsesLite(spec.bodyBytes)
	if isCompact && responsesLite {
		setRequestBody(spec, normalizeCodexResponsesLiteCompactBody(spec.bodyBytes))
	} else if !isCompact {
		setRequestBody(spec, normalizeCodexResponsesBody(spec.bodyBytes, spec.BaseURL, responsesLite))
	}

	var fields map[string]json.RawMessage
	_ = json.Unmarshal(spec.bodyBytes, &fields)
	setRequestBody(spec, normalizeCodexReasoningEffortForWireWithFields(spec.bodyBytes, fields))
	setRequestBody(spec, stripCodexResponsesPromptCacheRetentionWithFields(spec.bodyBytes, fields))
	setRequestBody(spec, stripCodexUnsupportedPromptCacheControls(spec.bodyBytes))
	metadata := c.newCodexRequestMetadataWithResponsesLite(*spec, responsesLite)
	spec.codexMetadata = &metadata
	setRequestBody(spec, normalizeCodexPromptCacheKeyForProfileWithFields(spec.bodyBytes, fields, metadata))
	if !isCompact {
		setRequestBody(spec, applyCodexClientMetadataWithFields(spec.bodyBytes, fields, metadata, false))
	}
	setRequestBody(spec, stripCodexResponsesHTTPGenerateWithFields(spec.bodyBytes, fields))
	setRequestBody(spec, stripCodexTopLevelTransportCorrelatorsWithFields(spec.bodyBytes, fields))
	return nil
}

// doOpenAICompatible performs a request against a custom OpenAI-compatible provider
// (Chat Completions or native Responses). The target is the provider's BaseURL
// (which already carries any "/v1" prefix) + DownstreamPath ("/chat/completions",
// "/responses", or "/models"). Normal model calls use a clean provider-authenticated
// header set with no downstream passthrough. Opaque Files/Skills/Agents calls opt into
// PassThrough, which retains only endpoint-semantic headers while still replacing all
// credentials. A sidecar-bound account routes through the impersonating sidecar for
// proxy-chaining / IP control; otherwise the cached direct/proxy transport is used.
// The same idle-timeout guard as every other path bounds the call without truncating a
// long-but-progressing stream (see requestGuard).
func (c *Client) doOpenAICompatible(ctx context.Context, spec Request) (*Response, error) {
	base := strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/")
	if base == "" {
		return nil, errors.New("custom provider base_url required")
	}
	path := spec.DownstreamPath
	if path == "" {
		path = "/chat/completions"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	target := base + path
	// Real Claude Code addresses every messages endpoint as ?beta=true (see doClaude), so
	// any relay that genuinely serves Claude Code clients already accepts it — which makes
	// sending it the faithful shape rather than a risk, and lets a provider stuck on the
	// generic profile still look like the real client. PassThrough keeps its own query.
	if !spec.PassThrough && claudeShapedCustomCall(spec) && strings.Contains(path, "/messages") && !strings.Contains(target, "beta=") {
		if strings.Contains(target, "?") {
			target += "&beta=true"
		} else {
			target += "?beta=true"
		}
	}
	method := firstNonEmpty(spec.Method, http.MethodPost)
	stream := false
	if spec.PassThrough {
		// Shared Files/Skills/Agents bodies may be multipart or arbitrary binary
		// data. Never inspect them as JSON merely to determine response framing.
		stream = strings.Contains(strings.ToLower(spec.Headers.Get("Accept")), "text/event-stream")
	} else {
		stream = requestStreamTrue(spec)
	}

	built := http.Header{}
	// Claude-Code-mode relays gate on the client shape (X-App, X-Stainless-*, a session
	// id, the betas), which the generic builder does not emit — and the profile that used
	// to be the only way to reach that builder is inferred from the provider's name, so an
	// operator who correctly selected upstream_protocol=anthropic_messages could still land
	// on the generic path. claudeShapedCustomCall keys off the protocol/path instead of the
	// name. PassThrough is excluded: those endpoints (multipart Files uploads, Skills) need
	// their own Content-Type and beta forwarded verbatim, which is a different header set.
	claudeShaped := !spec.PassThrough && claudeShapedCustomCall(spec)
	if claudeShaped {
		// Narrow the cookie jar to the conversation before either transport reads it.
		spec.CookieJarKey = customClaudeCookieJarKey(spec, c.identityForOS(spec.Account.ID, spec.OSHint))
	}
	switch {
	case claudeShaped:
		id := c.identityForOS(spec.Account.ID, spec.OSHint)
		c.applyClaudeCodeCustomHeaders(built, spec, id, stream)
	case spec.PassThrough && spec.TransportProfile == storage.CustomProviderTransportClaudeCode:
		// Opaque Files/Skills/Agents proxy: endpoint semantics, not the messages shape.
		id := c.identityForOS(spec.Account.ID, spec.OSHint)
		c.applyClaudePassthroughHeaders(built, spec, id, stream)
		copyOpenAICompatPassthroughHeaders(built, spec, []string{
			"Content-Range",
			"Idempotency-Key",
			"If-Match",
			"If-Modified-Since",
			"If-None-Match",
			"If-Unmodified-Since",
			"Last-Event-ID",
			"OpenAI-Beta",
			"Prefer",
			"Range",
		})
	case spec.TransportProfile == storage.CustomProviderTransportCodexCLI:
		if spec.PassThrough {
			applyOpenAICompatPassthroughHeaders(built, spec, stream)
		} else {
			applyOpenAICompatHeaders(built, spec, stream)
		}
		c.applyOpenAICompatCodexIdentity(built, spec)
	default:
		if spec.PassThrough {
			applyOpenAICompatPassthroughHeaders(built, spec, stream)
		} else {
			applyOpenAICompatHeaders(built, spec, stream)
		}
	}

	// A custom Anthropic Messages/Claude-Code transport carries the same claude-cli
	// identity as the built-in Claude route, so it must use the same captured Bun TLS,
	// HTTP/1.1 and header profile as well. Previously this branch only changed the
	// application headers: sidecar traffic still inherited Chrome's ClientHello and
	// browser defaults, while direct/proxy traffic exposed Go's ClientHello. Both are
	// internally contradictory fingerprints visible to an upstream relay.
	claudeTransport := claudeShaped ||
		(spec.PassThrough && spec.TransportProfile == storage.CustomProviderTransportClaudeCode)
	if claudeTransport && c.claudeFingerprintEngine(spec.Egress) != claudeEngineStdlib {
		built.Del("Accept-Encoding")
		transportSpec := spec
		transportSpec.Method = method
		override := c.cfgSnapshot().ClaudeJA3Override
		headerOrder := claudeHeaderOrder(built)
		switch c.claudeFingerprintEngine(spec.Egress) {
		case claudeEngineInProcess:
			return c.postInProcessOrdered(ctx, transportSpec, target, built, c.cfg.RequestTimeout(), resolveClaudeJA3(override), resolveClaudeTLSProfile(override), headerOrder)
		case claudeEngineSidecar:
			timeout := c.cfg.RequestTimeout()
			if st := c.cfg.SidecarTimeout(); st > timeout {
				timeout = st
			}
			return c.postViaSidecarOrdered(ctx, transportSpec, target, built, timeout, resolveClaudeJA3(override), false, headerOrder)
		}
	}

	// Non-Claude sidecar egress keeps the transport profile used by generic custom
	// providers and may chain through a proxy/WARP exit. A custom provider explicitly
	// using the Codex CLI profile already has a complete CLI application header set,
	// so it must suppress curl's browser headers just like the built-in Codex path.
	// Claude-shaped calls have already returned through the provider-coherent branch.
	if spec.Egress.Type == "curl_cffi_sidecar" && !claudeTransport {
		built.Del("Accept-Encoding")
		timeout := c.cfg.RequestTimeout()
		if st := c.cfg.SidecarTimeout(); st > timeout {
			timeout = st
		}
		sidecarSpec := spec
		sidecarSpec.Method = method
		defaultHeaders := spec.TransportProfile != storage.CustomProviderTransportCodexCLI
		return c.postViaSidecarOrdered(ctx, sidecarSpec, target, built, timeout, "", defaultHeaders, nil)
	}

	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	req, err := newReplayableHTTPRequest(ctx, method, target, spec)
	if err != nil {
		guard.Fail()
		return nil, err
	}
	wireBuilt := built
	if claudeTransport && forceHTTP1ForClaudeImpersonation(target, built) {
		wireBuilt = claudeWireHeaders(built)
	}
	for k, values := range wireBuilt {
		req.Header[k] = append([]string(nil), values...)
	}
	// httpClientForEgress does not understand the sidecar type; that case is handled
	// above, so any non-direct/proxy egress here is treated as direct.
	directSpec := spec
	if claudeTransport {
		directSpec.Egress = claudeDirectFallbackEgress(spec.Egress)
	} else if directSpec.Egress.Type == "curl_cffi_sidecar" {
		directSpec.Egress.Type = "direct"
	}
	// Same ALPN reasoning as doClaude's stdlib fallback: when this request claims to be
	// claude-cli (or targets an Anthropic host), it must not ride HTTP/2, because the real
	// captured native client does not negotiate it. Non-Claude custom providers are untouched —
	// the predicate keys off the target host and the claude-cli User-Agent only.
	client, err := c.httpClientForEgressMode(directSpec, forceHTTP1ForClaudeImpersonation(target, built))
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

func (c *Client) applyOpenAICompatCodexIdentity(dst http.Header, spec Request) {
	id := c.identityForOS(spec.Account.ID, spec.OSHint)
	threadOriginator := codexThreadOriginator(spec.Headers)
	processOriginator := codexProcessOriginator(spec.Headers, threadOriginator)
	version := c.codexClientVersionForRequest(spec)
	dst.Set("User-Agent", id.CodexUserAgentForOriginator(processOriginator, version))
	setHeaderPreserveCase(dst, "Originator", threadOriginator)
	setHeaderPreserveCase(dst, "version", version)
	if beta := mergeCodexBetaFeatures(getHeaderFold(spec.Headers, "x-codex-beta-features")); beta != "" {
		setHeaderPreserveCase(dst, "x-codex-beta-features", beta)
	}
	if strings.Contains(strings.ToLower(spec.DownstreamPath), "responses") {
		metadata := spec.codexMetadata
		if metadata == nil {
			generated := c.newCodexRequestMetadata(spec)
			metadata = &generated
		}
		setHeaderPreserveCase(dst, "session-id", metadata.sessionID)
		setHeaderPreserveCase(dst, "thread-id", metadata.threadID)
		setHeaderPreserveCase(dst, "x-client-request-id", metadata.threadID)
		setHeaderPreserveCase(dst, "x-codex-window-id", metadata.windowID)
		if metadata.turnMetadataHeader != "" {
			setHeaderPreserveCase(dst, "x-codex-turn-metadata", metadata.turnMetadataHeader)
		}
		if metadata.parentThreadID != "" {
			setHeaderPreserveCase(dst, "x-codex-parent-thread-id", metadata.parentThreadID)
		}
		if metadata.subagent != "" {
			setHeaderPreserveCase(dst, codexSubagentHeader, metadata.subagent)
		}
		if metadata.turnState != "" {
			setHeaderPreserveCase(dst, "x-codex-turn-state", metadata.turnState)
		}
		if metadata.responsesLite {
			setHeaderPreserveCase(dst, codexResponsesLiteHeader, "true")
		}
	}
}

// applyOpenAICompatHeaders builds the upstream header set for a custom provider from
// scratch: Bearer auth (the account's API key), JSON content type when there is a
// body, the streaming-appropriate Accept, and a generic OpenAI-SDK User-Agent. It
// deliberately forwards NONE of the downstream client's headers.
func applyOpenAICompatHeaders(dst http.Header, spec Request, stream bool) {
	credential := accountprovider.Credential(spec.Account.Provider, spec.Token)
	if credential != "" {
		dst.Set("Authorization", "Bearer "+credential)
	}
	if anthropicVersion := strings.TrimSpace(spec.Headers.Get("Anthropic-Version")); anthropicVersion != "" {
		dst.Set("Anthropic-Version", anthropicVersion)
		if credential != "" {
			dst.Set("X-Api-Key", credential)
		}
		if beta := strings.TrimSpace(spec.Headers.Get("Anthropic-Beta")); beta != "" {
			dst.Set("Anthropic-Beta", beta)
		}
	}
	if requestBodySize(spec) > 0 {
		dst.Set("Content-Type", "application/json")
	}
	if stream {
		dst.Set("Accept", "text/event-stream")
	} else {
		dst.Set("Accept", "application/json")
	}
	if dst.Get("Anthropic-Version") != "" {
		dst.Set("User-Agent", "claude-cli/"+identity.ClaudeCLIVersion+" (external, cli)")
	} else {
		dst.Set("User-Agent", openAICompatUserAgent)
	}
}

// applyOpenAICompatPassthroughHeaders builds a minimal safe header set for
// opaque custom-provider endpoints. It intentionally does not forward the
// downstream Authorization, x-api-key, Cookie, forwarding, or pool headers:
// those can contain the pool credential or internal routing metadata. Endpoint
// semantics (multipart boundary, beta/version selection, range and conditional
// resource headers) survive verbatim, and account auth is installed afterward.
func applyOpenAICompatPassthroughHeaders(dst http.Header, spec Request, stream bool) {
	semanticHeaders := []string{
		"Accept",
		"Anthropic-Beta",
		"Anthropic-Version",
		"Content-Range",
		"Content-Type",
		"Idempotency-Key",
		"If-Match",
		"If-Modified-Since",
		"If-None-Match",
		"If-Unmodified-Since",
		"Last-Event-ID",
		"OpenAI-Beta",
		"Prefer",
		"Range",
	}
	semanticPath := strings.TrimSuffix(strings.ToLower(strings.SplitN(spec.DownstreamPath, "?", 2)[0]), "/")
	if strings.HasSuffix(semanticPath, "/alpha/search") {
		// The standalone-search client attaches these two request-scoped fields.
		// Preserve them only for that endpoint; credentials and pool headers remain
		// excluded by the fixed passthrough allowlist.
		semanticHeaders = append(semanticHeaders, "Originator", "X-Codex-Turn-Metadata")
	}
	copyOpenAICompatPassthroughHeaders(dst, spec, semanticHeaders)

	credential := accountprovider.Credential(spec.Account.Provider, spec.Token)
	if credential != "" {
		dst.Set("Authorization", "Bearer "+credential)
	}
	if spec.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages ||
		dst.Get("Anthropic-Version") != "" {
		if credential != "" {
			dst.Set("X-Api-Key", credential)
		}
		if dst.Get("Anthropic-Version") == "" {
			dst.Set("Anthropic-Version", claudeAnthropicVersion)
		}
		dst.Set("User-Agent", "claude-cli/"+identity.ClaudeCLIVersion+" (external, cli)")
	} else {
		dst.Set("User-Agent", openAICompatUserAgent)
	}
	if dst.Get("Accept") == "" {
		if stream {
			dst.Set("Accept", "text/event-stream")
		} else {
			dst.Set("Accept", "application/json")
		}
	}
	if requestBodySize(spec) > 0 && dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
}

func copyOpenAICompatPassthroughHeaders(dst http.Header, spec Request, names []string) {
	for _, name := range names {
		values := spec.Headers.Values(name)
		if len(values) == 0 {
			continue
		}
		dst.Del(name)
		for _, value := range values {
			dst.Add(name, value)
		}
	}
}
