package upstream

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream/tlsclient"
)

// DoRaw performs a provider-neutral request while retaining the pool's egress and
// idle-timeout behavior. It is used by native adapters such as Kiro whose endpoint
// and wire protocol are neither Codex nor Anthropic HTTP.
func (c *Client) DoRaw(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey string) (*Response, error) {
	return c.doRaw(ctx, egress, method, rawURL, headers, body, cookieJarKey, false)
}

// DoRawHTTP1 is the HTTP/1.1-only counterpart to DoRaw. It is used by native
// adapters whose first-party client does not negotiate HTTP/2. A sidecar wrapper
// cannot guarantee the requested application protocol, so this path retains its
// underlying chain proxy (and therefore exit IP) while using the Go HTTP/1.1
// transport directly.
func (c *Client) DoRawHTTP1(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey string) (*Response, error) {
	return c.doRaw(ctx, http1TransparentEgress(egress), method, rawURL, headers, body, cookieJarKey, true)
}

func (c *Client) doRaw(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey string, forceHTTP1 bool) (*Response, error) {
	if strings.EqualFold(strings.TrimSpace(egress.Type), "curl_cffi_sidecar") {
		// In-process fingerprint engine: route native-adapter egress (Kiro's aws-sdk-js
		// calls, Codex/WHAM reset-credit/quota/registration calls) through tls-client with
		// the per-client profile instead of the external curl_cffi sidecar. The profile is
		// derived from the User-Agent so aws-sdk-js (Kiro/Node) and Codex (Chrome) each get
		// the right fingerprint without changing this provider-neutral signature. The
		// sidecar stays the fallback whenever the engine is left on "sidecar".
		if c.inProcessFingerprint() {
			return c.doRawInProcess(ctx, egress, method, rawURL, headers, body, cookieJarKey, rawProfileForHeaders(headers), nil)
		}
		return c.DoViaSidecar(ctx, egress, method, rawURL, headers, body, cookieJarKey)
	}
	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	req.Header = headers.Clone()
	client, err := c.httpClientForEgressMode(Request{Egress: egress, CookieJarKey: cookieJarKey}, forceHTTP1)
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

func http1TransparentEgress(egress storage.EgressProfile) storage.EgressProfile {
	if !strings.EqualFold(strings.TrimSpace(egress.Type), "curl_cffi_sidecar") {
		return egress
	}
	if chainProxy := strings.TrimSpace(egress.ChainProxy); chainProxy != "" {
		return storage.EgressProfile{
			ID:       egress.ID,
			Type:     proxyTypeForURL(chainProxy),
			Endpoint: chainProxy,
		}
	}
	return storage.EgressProfile{ID: egress.ID, Type: "direct"}
}

// EgressHTTPClient returns a transparent *http.Client whose transport routes
// through the given egress (direct / http(s)_proxy / warp_proxy / socks5(h)_proxy),
// with a fresh per-call cookie jar. It is used by the registration pipeline so the
// multi-step OpenAI signup/OAuth flow (which relies on automatic cookie + redirect
// handling) egresses off the shared VPS IP through the operator's chosen proxy/WARP
// exit instead of leaking the host address straight to chatgpt.com's Cloudflare wall.
//
// For a curl_cffi_sidecar egress the sidecar's POST /proxy JA3-replay protocol is not
// a Go RoundTripper, so it cannot transparently back an *http.Client; this maps such
// an egress onto its chain proxy when set (so traffic still leaves from the intended
// exit IP) and onto a direct transport otherwise. Per-request real-JA3 replay for the
// CF-walled calls is available separately via DoViaSidecar.
func (c *Client) EgressHTTPClient(egress storage.EgressProfile) (*http.Client, error) {
	eg := http1TransparentEgress(egress)
	transport, err := c.transportForEgress(eg)
	if err != nil {
		return nil, err
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   90 * time.Second,
	}, nil
}

// rawProfileForHeaders picks the in-process tls-client profile for a provider-neutral
// DoRaw call from its User-Agent. Kiro's traffic (KiroIDE Node and ksk_ Amazon Q alike)
// is emitted by aws-sdk-js over Node.js, so it maps to ProfileNode; everything else
// (Codex/WHAM reset-credit, quota, agent registration) keeps the Chrome default. Node
// currently resolves to Chrome_120 in tlsclient.ResolveProfile — a safe placeholder until
// a validated aws-sdk-js capture replaces it (the non-negotiable fingerprint gate).
func rawProfileForHeaders(headers http.Header) string {
	ua := strings.ToLower(headers.Get("User-Agent") + " " + headers.Get("x-amz-user-agent"))
	if strings.Contains(ua, "aws-sdk-js") || strings.Contains(ua, "kiroide") {
		return tlsclient.ProfileNode
	}
	return tlsclient.ProfileChrome
}

// proxyTypeForURL maps a proxy URL scheme onto the egress Type that
// transportForEgress understands. Defaults to http_proxy for unknown/empty schemes.
func proxyTypeForURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "http_proxy"
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5h":
		return "socks5h_proxy"
	case "socks5":
		return "socks5_proxy"
	case "https":
		return "https_proxy"
	default:
		return "http_proxy"
	}
}

// DoViaSidecar performs a single arbitrary HTTP request through the curl_cffi sidecar
// so the upstream sees a real browser TLS/JA3 + HTTP2 fingerprint (Chrome impersonation
// by default). It is provider-neutral — the caller supplies the full target URL, method,
// headers and body — and is intended for the individual Cloudflare-walled calls of the
// registration flow (chatgpt.com / auth.openai.com / sentinel.openai.com) where the
// stdlib transport's fingerprint would be flagged. cookieJarKey scopes the sidecar's
// server-side cookie store so a multi-call flow shares cookies under one key.
func (c *Client) DoViaSidecar(ctx context.Context, egress storage.EgressProfile, method, rawURL string, headers http.Header, body []byte, cookieJarKey string) (*Response, error) {
	built := http.Header{}
	for k, vs := range headers {
		for _, v := range vs {
			built.Add(k, v)
		}
	}
	spec := Request{
		Method:       firstNonEmpty(method, http.MethodGet),
		Body:         body,
		Egress:       egress,
		CookieJarKey: cookieJarKey,
	}
	return c.postViaSidecar(ctx, spec, rawURL, built, c.cfg.SidecarTimeout(), "", true)
}
