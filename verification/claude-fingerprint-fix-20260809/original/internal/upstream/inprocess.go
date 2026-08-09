package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream/tlsclient"
)

// postInProcess is the in-process analogue of postViaSidecar. It performs the actual
// network call through the bogdanfinn/tls-client engine so the upstream sees a real
// browser TLS/JA3 + HTTP2 fingerprint (Chrome_120, matching the sidecar's chrome120
// default) instead of the Go standard library's — with no external Python process.
//
// It is provider-neutral: the caller supplies the target URL, the fully-built upstream
// header set, the overall timeout, and an optional explicit JA3/profile override
// (jaProfileOverride, mapped to a named tls-client profile). It shares the same idle-timeout
// guard semantics as every other upstream transport (bound to the body's Close, reset on
// each read) so a long streaming completion is never truncated. A sidecar egress's chain
// proxy is honored so traffic still leaves from the intended exit IP.
//
// jaProfileOverride "" keeps the default Chrome_120. The in-process engine works with
// hardened named profiles from profiles.MappedTLSClients rather than raw-string JA3 replay
// (the sidecar's curl_cffi feature); an unrecognized override degrades to Chrome_120 rather
// than failing the request.
func (c *Client) postInProcess(ctx context.Context, spec Request, target string, built http.Header, timeout time.Duration, jaProfileOverride, profileName string) (*Response, error) {
	return c.postInProcessOrdered(ctx, spec, target, built, timeout, jaProfileOverride, profileName, nil)
}

// postInProcessOrdered is postInProcess with an explicit wire header order. Passing a
// non-nil order is what keeps the emitted header sequence identical to the impersonated
// client: with no order set, fhttp sorts header names lexicographically (see
// fhttp/header.go headerSorter.Less), an ordering no real Node/undici or Chrome client
// produces and therefore a fingerprint of its own.
func (c *Client) postInProcessOrdered(ctx context.Context, spec Request, target string, built http.Header, timeout time.Duration, jaProfileOverride, profileName string, headerOrder []string) (*Response, error) {
	headers := built.Clone()
	applyInProcessAcceptEncoding(headers, target)

	ctx, guard := newRequestGuard(ctx, timeout)
	resp, err := c.tlsFactory.Do(ctx, tlsclient.Request{
		Method:       firstNonEmpty(spec.Method, http.MethodPost),
		URL:          target,
		Header:       headers,
		HeaderOrder:  headerOrder,
		Body:         spec.Body,
		Profile:      profileName,
		JA3Override:  inProcessNamedProfile(jaProfileOverride),
		ForceHTTP1:   forceHTTP1ForClaudeImpersonation(target, headers),
		ProxyURL:     egressProxyURL(spec.Egress),
		CookieJarKey: sidecarCookieKey(spec.Account.ID, spec.Egress.ID, target, spec.CookieJarKey),
		// Timeout intentionally left 0: the requestGuard context bounds the call with the
		// pool's idle-timeout semantics, not an absolute deadline.
	})
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       guard.Wrap(resp.Body),
	}, nil
}

// undiciAcceptEncoding is the Accept-Encoding value Node's fetch appends to every https
// request. Claude Code never sets one itself, so this is what the real client's wire
// carries.
const undiciAcceptEncoding = "br, gzip, deflate, zstd"

// applyInProcessAcceptEncoding decides the Accept-Encoding this request puts on the wire.
//
// The header set we are handed never carries one (applyClaudeFetchHeaders deletes it,
// deliberately, because the stdlib fallback can only transparently decompress gzip that IT
// added). If we simply forwarded that emptiness, fhttp fills the gap itself with
// "gzip, deflate, br" (transport.go:2567) — three codecs in an order no real client sends,
// and missing zstd, on a request whose User-Agent claims to be Node.
//
// For Claude impersonation we can do better, because fhttp CAN decode all four codecs:
// DecompressBodyByType handles gzip/br/deflate/zstd (transport.go:2883), and it is armed
// whenever the caller's own value merely contains "gzip" (transport.go:2571 sets
// requestedGzip, which becomes rc.addedGzip at :2609). That is an fhttp patch — the Go
// stdlib only decompresses what it added itself — so sending undici's exact value here is
// both faithful and still transparently decompressed for the SSE scanner.
//
// Any non-Claude target keeps the previous behavior (deleted, transport decides) so no
// other provider's header set moves.
func applyInProcessAcceptEncoding(headers http.Header, target string) {
	headers.Del("Accept-Encoding")
	if forceHTTP1ForClaudeImpersonation(target, headers) {
		headers.Set("Accept-Encoding", undiciAcceptEncoding)
	}
}

// forceHTTP1ForClaudeImpersonation reports whether this request must be pinned to
// HTTP/1.1 at the ALPN layer.
//
// GROUND TRUTH: Claude Code reaches Anthropic through the Anthropic TypeScript SDK, which
// issues its calls on Node's global fetch. Global fetch is backed by Node's BUNDLED undici,
// and undici forces allowH2:false for those legacy v1 dispatcher consumers ("Legacy (v1)
// consumers (like Node.js's bundled undici on Node 22) do not support HTTP/2", undici
// commit for issue #4989). A packet capture of the real client agrees — see
// tools/capture/parse.py: "Claude Code -> api.anthropic.com offers ALPN http/1.1". So every
// genuine Claude Code request is HTTP/1.1 and its ClientHello advertises only http/1.1.
//
// Our default Chrome_120 profile advertises ["h2","http/1.1"], and an edge that prefers h2
// (Anthropic's does) then negotiates HTTP/2. That produces two signals real Claude Code
// cannot produce: a JA4 whose ALPN field reads h2 instead of h1, and an HTTP/2
// SETTINGS/Akamai fingerprint on a connection carrying a claude-cli User-Agent. The second
// is a cheap, essentially false-positive-free discriminator — "claude-cli UA arriving over
// h2" is by construction never the real client.
//
// The trigger is deliberately derived from the request itself rather than a new parameter,
// so every caller (including the OpenAI-compat -> Claude bridge) is covered without any
// signature change. Two independent conditions, either of which is sufficient:
//   - an Anthropic host, which covers requests whose headers are built elsewhere; and
//   - a claude-cli User-Agent, which covers a custom ClaudeUpstreamBaseURL (operators may
//     point the base URL at their own front door, and a request that CLAIMS to be Claude
//     Code must still look like it on the wire).
//
// Everything else is left on the profile's native ALPN so no other provider's fingerprint
// moves.
func forceHTTP1ForClaudeImpersonation(target string, headers http.Header) bool {
	if isAnthropicTarget(target) {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(headers.Get("User-Agent"))), "claude-cli/")
}

// isAnthropicTarget reports whether rawURL points at an Anthropic-operated host. It matches
// on host boundaries (exact host or a dot-prefixed subdomain) so a lookalike registration
// such as "api.anthropic.com.evil.test" or "notanthropic.com" cannot match.
func isAnthropicTarget(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range []string{"anthropic.com", "claude.ai"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// inProcessNamedProfile maps a resolved sidecar JA3 string onto a named tls-client
// profile key. The sidecar consumes a raw JA3 string; the in-process engine consumes a
// named profile from profiles.MappedTLSClients. Empty / Chrome sentinels keep the default
// (Chrome_120). A raw JA3 string that isn't a known named key returns "" so ResolveProfile
// degrades to Chrome_120 rather than presenting an incoherent partial fingerprint.
func inProcessNamedProfile(ja3 string) string {
	switch v := strings.ToLower(strings.TrimSpace(ja3)); v {
	case "", "off", "none", "disabled", "-", "chrome", "browser":
		return ""
	default:
		// Named profile passthrough (e.g. "chrome_133", "firefox_120"): ResolveProfile
		// looks these up in profiles.MappedTLSClients. A raw JA3 fingerprint string will
		// not match and safely degrades to Chrome_120.
		return v
	}
}

// doRawInProcess is the in-process analogue of DoViaSidecar for provider-neutral native
// adapters (Kiro). It routes an arbitrary request through the tls-client engine with the
// per-provider profile so the upstream sees the intended fingerprint. profileName selects
// ProfileNode / ProfileRustls / ProfileChrome; headerOrder, when non-nil, pins the wire
// order of the request headers to match the native client (e.g. aws-sdk-js).
func (c *Client) doRawInProcess(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey, profileName string, headerOrder []string) (*Response, error) {
	return c.doRawInProcessSource(ctx, egress, method, rawURL, headers, bodysource.Bytes(body), cookieJarKey, profileName, headerOrder)
}

func (c *Client) doRawInProcessSource(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body bodysource.BodySource, cookieJarKey, profileName string, headerOrder []string) (*Response, error) {
	built := headers.Clone()
	applyInProcessAcceptEncoding(built, rawURL)

	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	resp, err := c.tlsFactory.Do(ctx, tlsclient.Request{
		Method:       firstNonEmpty(method, http.MethodGet),
		URL:          rawURL,
		Header:       built,
		HeaderOrder:  headerOrder,
		Body:         body,
		Profile:      profileName,
		ForceHTTP1:   forceHTTP1ForClaudeImpersonation(rawURL, built),
		ProxyURL:     egressProxyURL(egress),
		CookieJarKey: cookieJarKey,
	})
	if err != nil {
		guard.Fail()
		return nil, err
	}
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       guard.Wrap(resp.Body),
	}, nil
}

// CloseIdleFingerprintConnections releases idle keep-alive connections held by the
// in-process TLS engine. Called alongside the sidecar/ transport idle sweeps.
func (c *Client) CloseIdleFingerprintConnections() {
	if c.tlsFactory != nil {
		c.tlsFactory.CloseIdle()
	}
}

// FingerprintReflection is the parsed subset of a JA3/JA4/Akamai reflector response
// (tls.peet.ws/api/all shape) plus the raw body, for the admin fidelity-diff diagnostic.
type FingerprintReflection struct {
	Engine        string          `json:"engine"` // "inprocess" or "sidecar"
	StatusCode    int             `json:"status_code"`
	JA3           string          `json:"ja3,omitempty"`
	JA3Hash       string          `json:"ja3_hash,omitempty"`
	JA4           string          `json:"ja4,omitempty"`
	AkamaiHash    string          `json:"akamai_hash,omitempty"`
	Akamai        string          `json:"akamai,omitempty"`
	UserAgentSeen string          `json:"user_agent_seen,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// ReflectFingerprint performs a GET against a public TLS/HTTP2 reflector (default
// https://tls.peet.ws/api/all) through the requested fingerprint engine and returns the
// observed JA3/JA4/Akamai fingerprint. It is the diagnostic that lets an operator verify,
// post-deploy on the live VPS, that the in-process engine presents the SAME fingerprint the
// sidecar did before flipping (the non-negotiable validation gate).
//
// SECURITY NOTE: this makes an outbound request that reveals the chosen egress IP to the
// third-party reflector. It is an explicit, admin-triggered diagnostic only; never called
// on the request hot path. Operators can point `reflectorURL` at a self-hosted reflector.
//
// engine: "inprocess" routes through the tls-client engine; "sidecar" routes through the
// curl_cffi sidecar (only valid for a curl_cffi_sidecar egress). profileName selects the
// in-process profile (ProfileChrome for Codex/Claude, ProfileNode/ProfileRustls for Kiro).
func (c *Client) ReflectFingerprint(ctx context.Context, egress storage.EgressProfile, engine, profileName, reflectorURL string) FingerprintReflection {
	if strings.TrimSpace(reflectorURL) == "" {
		reflectorURL = "https://tls.peet.ws/api/all"
	}
	out := FingerprintReflection{Engine: engine}
	headers := http.Header{}
	var resp *Response
	var err error
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "sidecar":
		resp, err = c.DoViaSidecar(ctx, egress, http.MethodGet, reflectorURL, headers, nil, "fingerprint-check")
	default: // inprocess
		resp, err = c.doRawInProcess(ctx, egress, http.MethodGet, reflectorURL, headers, nil, "fingerprint-check", profileName, nil)
	}
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer resp.Body.Close()
	out.StatusCode = resp.StatusCode
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if readErr != nil {
		out.Error = readErr.Error()
		return out
	}
	out.Raw = json.RawMessage(body)
	parseReflectorBody(body, &out)
	return out
}

// parseReflectorBody extracts the tls.peet.ws /api/all fingerprint fields into out. It is
// tolerant of a body that does not match the expected shape (leaves fields empty).
func parseReflectorBody(body []byte, out *FingerprintReflection) {
	var parsed struct {
		TLS struct {
			JA3     string `json:"ja3"`
			JA3Hash string `json:"ja3_hash"`
			JA4     string `json:"ja4"`
		} `json:"tls"`
		HTTP2 struct {
			AkamaiFingerprint     string `json:"akamai_fingerprint"`
			AkamaiFingerprintHash string `json:"akamai_fingerprint_hash"`
		} `json:"http2"`
		UserAgent string `json:"user_agent"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return
	}
	out.JA3 = parsed.TLS.JA3
	out.JA3Hash = parsed.TLS.JA3Hash
	out.JA4 = parsed.TLS.JA4
	out.Akamai = parsed.HTTP2.AkamaiFingerprint
	out.AkamaiHash = parsed.HTTP2.AkamaiFingerprintHash
	out.UserAgentSeen = parsed.UserAgent
}
