package upstream

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

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
	eg := egress
	if strings.EqualFold(strings.TrimSpace(eg.Type), "curl_cffi_sidecar") {
		if cp := strings.TrimSpace(eg.ChainProxy); cp != "" {
			eg = storage.EgressProfile{Type: proxyTypeForURL(cp), Endpoint: cp}
		} else {
			eg = storage.EgressProfile{Type: "direct"}
		}
	}
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
	return c.postViaSidecar(ctx, spec, rawURL, built, c.cfg.SidecarTimeout(), "")
}
