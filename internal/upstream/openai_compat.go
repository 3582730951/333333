package upstream

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/accountprovider"
)

// openAICompatUserAgent is sent on custom-provider traffic. It mimics the official
// OpenAI Python SDK, which every OpenAI-compatible provider (DeepSeek, Kimi,
// OpenRouter, vLLM, …) expects — these are normal API endpoints, not clients we
// must fingerprint, so a stable generic SDK UA is both correct and low-risk.
const openAICompatUserAgent = "OpenAI/Python 1.61.0"

// IsCustomProvider reports whether a provider id names a custom OpenAI-compatible
// provider (anything other than the built-in codex/claude/kiro upstreams or the empty
// legacy value). Exported so the API layer routes consistently with Client.Do.
func IsCustomProvider(provider string) bool {
	switch strings.TrimSpace(provider) {
	case "", "codex", "claude", "kiro":
		return false
	default:
		return true
	}
}

// doOpenAICompatible performs a request against a custom OpenAI-compatible provider
// (Chat Completions or native Responses). The target is the provider's BaseURL
// (which already carries any "/v1" prefix) + DownstreamPath ("/chat/completions",
// "/responses", or "/models"). Headers are a clean Bearer-auth OpenAI client set — no
// downstream header passthrough (which would leak the relay), no Codex/Claude
// fingerprint (irrelevant to these providers). A sidecar-bound account still routes
// through the impersonating sidecar (Chrome JA3) for proxy-chaining / IP control;
// otherwise the cached direct/proxy transport is used. The same idle-timeout guard
// as every other path bounds the call without truncating a long-but-progressing
// stream (see requestGuard).
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
	method := firstNonEmpty(spec.Method, http.MethodPost)
	stream := bodyStreamTrue(spec.Body)

	built := http.Header{}
	applyOpenAICompatHeaders(built, spec, stream)

	// Sidecar egress: present a real client TLS/JA3 + HTTP2 fingerprint and (optionally)
	// chain through a proxy/WARP exit, exactly like the Codex/Claude sidecar paths. ja3=""
	// keeps the sidecar's native Chrome impersonation (these providers need no specific JA3).
	if spec.Egress.Type == "curl_cffi_sidecar" {
		built.Del("Accept-Encoding")
		timeout := c.cfg.RequestTimeout()
		if st := c.cfg.SidecarTimeout(); st > timeout {
			timeout = st
		}
		sidecarSpec := spec
		sidecarSpec.Method = method
		return c.postViaSidecar(ctx, sidecarSpec, target, built, timeout, "", true)
	}

	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(spec.Body))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	for k, values := range built {
		req.Header[k] = append([]string(nil), values...)
	}
	// httpClientForEgress does not understand the sidecar type; that case is handled
	// above, so any non-direct/proxy egress here is treated as direct.
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

// applyOpenAICompatHeaders builds the upstream header set for a custom provider from
// scratch: Bearer auth (the account's API key), JSON content type when there is a
// body, the streaming-appropriate Accept, and a generic OpenAI-SDK User-Agent. It
// deliberately forwards NONE of the downstream client's headers.
func applyOpenAICompatHeaders(dst http.Header, spec Request, stream bool) {
	if bearer := accountprovider.Credential(spec.Account.Provider, spec.Token); bearer != "" {
		dst.Set("Authorization", "Bearer "+bearer)
	}
	if len(spec.Body) > 0 {
		dst.Set("Content-Type", "application/json")
	}
	if stream {
		dst.Set("Accept", "text/event-stream")
	} else {
		dst.Set("Accept", "application/json")
	}
	dst.Set("User-Agent", openAICompatUserAgent)
}
