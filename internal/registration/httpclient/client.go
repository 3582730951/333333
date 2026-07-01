// Package httpclient provides egress-aware HTTP clients for registration. Its
// SidecarTransport routes the OpenAI signup/OAuth calls through the local curl_cffi
// sidecar so chatgpt.com / auth.openai.com see a real browser TLS+HTTP2 (JA3) fingerprint
// instead of the stdlib's — without that impersonation Cloudflare serves a JS challenge
// to every request (empirically: plain Go/curl gets the "Just a moment" HTML, curl_cffi
// chrome120 gets a 200 + JSON). The sidecar also chains through an upstream residential
// proxy (cliproxy) and keeps a server-side cookie jar per key, which together carry the
// multi-step flow's redirects and session cookies.
package httpclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

const sidecarBodyLimit = 8 << 20

// SidecarTransport is an http.RoundTripper that forwards each request to a curl_cffi
// sidecar's POST /proxy endpoint. The sidecar replays a browser ClientHello, optionally
// routes through Proxy (a residential proxy URL), follows redirects, and persists cookies
// under JarKey — so a sequence of RoundTrips behaves like one browser session.
type SidecarTransport struct {
	Endpoint string       // sidecar base URL, e.g. http://127.0.0.1:3128
	Proxy    string       // upstream proxy URL the sidecar chains through (cliproxy); "" = none
	JarKey   string       // server-side cookie jar key shared across this flow's calls
	Inner    *http.Client // client used to reach the sidecar itself (direct, no proxy)
}

// sidecarMeta is the routing envelope carried in the X-Sidecar-Meta header (base64 JSON).
type sidecarMeta struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	CookieJarKey   string            `json:"cookie_jar_key"`
	Proxy          string            `json:"proxy,omitempty"`
	AllowRedirects bool              `json:"allow_redirects"`
}

func readSidecarBody(r io.Reader, label string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, sidecarBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > sidecarBodyLimit {
		return nil, fmt.Errorf("%s body too large", label)
	}
	return raw, nil
}

// RoundTrip implements http.RoundTripper.
func (t *SidecarTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := readSidecarBody(req.Body, "sidecar request")
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("sidecar: read body: %w", err)
		}
		body = b
	}

	headers := map[string]string{}
	for k := range req.Header {
		headers[k] = req.Header.Get(k)
	}

	meta := sidecarMeta{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: headers,
		// A unique per-request jar key keeps the sidecar's own (flat, domain-less) cookie
		// store out of the way — the Go client's domain-aware jar (which set the Cookie
		// header above) is the single source of truth for the multi-domain OAuth flow.
		CookieJarKey:   t.JarKey + "_" + randHex(),
		Proxy:          t.Proxy,
		AllowRedirects: false, // the Go client follows redirects with a domain-aware jar
	}
	metaJSON, _ := json.Marshal(meta)
	metaB64 := base64.StdEncoding.EncodeToString(metaJSON)

	endpoint := strings.TrimRight(t.Endpoint, "/") + "/proxy"
	sreq, err := http.NewRequestWithContext(req.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sreq.Header.Set("X-Sidecar-Meta", metaB64)
	sreq.Header.Set("Content-Type", "application/octet-stream")

	inner := t.Inner
	if inner == nil {
		inner = &http.Client{Timeout: 120 * time.Second}
	}
	sresp, err := inner.Do(sreq)
	if err != nil {
		return nil, fmt.Errorf("sidecar: %w", err)
	}
	defer sresp.Body.Close()
	respBody, err := readSidecarBody(sresp.Body, "sidecar response")
	if err != nil {
		return nil, fmt.Errorf("sidecar: read response: %w", err)
	}

	// A sidecar-level error (it could not even attempt the upstream call) comes back as a
	// non-2xx with a JSON {error} body and no upstream-status header.
	statusStr := sresp.Header.Get("x-sidecar-upstream-status")
	if statusStr == "" {
		return nil, fmt.Errorf("sidecar error (%d): %s", sresp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	status, _ := strconv.Atoi(statusStr)
	if status == 0 {
		status = http.StatusBadGateway
	}

	hdr := http.Header{}
	if b64 := sresp.Header.Get("x-sidecar-upstream-headers-b64"); b64 != "" {
		if raw, e := base64.StdEncoding.DecodeString(b64); e == nil {
			var m map[string][]string
			if json.Unmarshal(raw, &m) == nil {
				for k, vs := range m {
					for _, v := range vs {
						hdr.Add(k, v)
					}
				}
			}
		}
	}

	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(respBody)),
		ContentLength: int64(len(respBody)),
		Request:       req,
	}, nil
}

// NewSidecarClient returns an *http.Client whose transport routes every request through
// the curl_cffi sidecar (browser JA3) chained over proxyURL. Redirects are followed by
// the Go client itself (the sidecar does single hops) using a domain-scoped cookie jar,
// so the multi-domain OAuth signup flow's session cookies are preserved correctly — the
// sidecar's own flat cookie store would collide them across chatgpt.com/auth.openai.com.
// jarKey is still sent so the sidecar can seed any operator-injected cookies, but the Go
// jar is the source of truth.
func NewSidecarClient(endpoint, proxyURL, jarKey string) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: &SidecarTransport{
			Endpoint: endpoint,
			Proxy:    proxyURL,
			JarKey:   jarKey,
			Inner:    &http.Client{Timeout: 120 * time.Second},
		},
		Jar:     jar,
		Timeout: 150 * time.Second,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 15 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// randHex returns 8 random hex chars for per-request sidecar jar-key uniqueness.
func randHex() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SidecarHealthy reports whether a curl_cffi sidecar is responding at endpoint.
func SidecarHealthy(ctx context.Context, endpoint string) bool {
	if strings.TrimSpace(endpoint) == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
