// Package tlsclient provides in-process, high-fidelity TLS + HTTP/2 fingerprinting for
// upstream egress. It is the replacement for the external Python curl_cffi sidecar.
//
// It wraps github.com/bogdanfinn/tls-client (a uTLS fork + the fhttp fork of net/http),
// which reproduces BOTH the TLS ClientHello (JA3) AND the HTTP/2 fingerprint (Akamai:
// SETTINGS, WINDOW_UPDATE, pseudo-header order, priority) — the exact fidelity curl_cffi
// provides. Plain refraction-networking/utls (what CPA uses) controls only the TLS
// ClientHello and would silently downgrade the HTTP/2 fingerprint, so it is deliberately
// NOT used here.
//
// This package must not import the parent upstream package (which imports this one); it
// therefore speaks only the standard library net/http types plus a small neutral
// Request/Response pair. The upstream package adapts these into its own Response.
package tlsclient

import (
	"container/list"
	"context"
	"io"
	stdhttp "net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Profile names understood by ResolveProfile.
//
// # Chrome_120 baseline (validated 2026-07-25 via tls.peet.ws, go run ./cmd/fpcheck chrome)
//
//	JA3 hash:    1d9a054bac1eef41f30d370f9bbb2ad2
//	JA4:         t13d1516h2_8daaf6152771_02713d6af862
//	Akamai hash: 52d84b11737d980aef856699f885ca86
//	Akamai fp:   1:65536;2:0;4:6291456;6:262144|15663105|0|m,a,s,p
//
// Chrome is the default profile and matches the curl_cffi sidecar's impersonate=chrome120
// output exactly (same BoringSSL/uTLS parameters). This is the validated in-process
// fingerprint for Codex, Claude, and Kiro (see ProfileNode below).
//
// # ProfileNode rationale — Chrome_120 is correct, not a placeholder
//
// The curl_cffi sidecar used impersonate=chrome120 for ALL providers including Kiro; it
// never had a separate Node.js/OpenSSL profile. Because the safety invariant requires
// in-process == sidecar, ProfileNode must produce the same Chrome_120 hashes.
//
// For reference: real Node.js 22 (http2 module, OpenSSL 3.x) produces a substantially
// different fingerprint (captured 2026-07-25):
//
//	JA3 hash:    9bf9a924d7e59ffbef05e1f9c85a7bd6
//	JA4:         t13d5213h2_b262b3658495_ecd0401ec68b
//	Akamai hash: 38e36840395e07ae27bae1632ddc8b92
//	Akamai fp:   |00|0|p,m,a,s
//	JA3 string:  771,4866-4867-4865-49199-49195-49200-49196-158-...,65281-0-11-10-35-16-22-23-13-43-45-51-27,...
//
// Building a real OpenSSL/Node.js profile in bogdanfinn/tls-client requires a custom
// tls.ClientHelloID (not available in MappedTLSClients). Switching ProfileNode to the
// real Node fingerprint would diverge from the sidecar (Chrome_120) and fail the
// fingerprint gate. Chrome_120 is therefore the correct production value for ProfileNode.
//
// # ProfileRustls (Amazon Q / ksk_ accounts)
//
// Similarly, the sidecar used Chrome_120 for ksk_ key traffic; Chrome_120 is correct.
// Real aws-sdk-rust/rustls 0.23 JA3 would contain cipher 0xFF (TLS_EMPTY_RENEGOTIATION_INFO_SCSV)
// which is unlistable in bogdanfinn's BoringSSL layer and would crash with ImpersonateError.
const (
	ProfileChrome = "chrome"
	// ProfileNode is the Kiro KiroIDE fingerprint (aws-sdk-js / Node.js). Resolves to
	// Chrome_120 — the same profile the sidecar used for Kiro traffic.  See above for why
	// Chrome_120 is the correct value, not a placeholder.
	ProfileNode = "node"
	// ProfileRustls is the Kiro Amazon Q fingerprint (ksk_ keys / aws-sdk-rust). Resolves
	// to Chrome_120 — same reasoning as ProfileNode.
	ProfileRustls = "rustls"
)

// Request is a provider-neutral egress request. HeaderOrder, when set, pins the exact
// wire order of the request headers (the curl_cffi default_headers=false / operator-JA3
// exact-replay behaviour); when empty the profile's own default header set/order is used.
type Request struct {
	Method       string
	URL          string
	Header       stdhttp.Header
	HeaderOrder  []string
	Body         bodysource.BodySource
	Profile      string        // "" or ProfileChrome => Chrome_120; ProfileNode/ProfileRustls => stub
	JA3Override  string        // explicit named profile key from profiles.MappedTLSClients ("chrome_120" etc.)
	ProxyURL     string        // optional chain proxy (http(s)/socks5(h))
	CookieJarKey string        // scopes a persistent cookie jar across a multi-call flow
	Timeout      time.Duration // 0 => factory default
}

// Response mirrors the subset of a response the pool needs, with a streaming Body.
type Response struct {
	StatusCode int
	Header     stdhttp.Header
	Body       io.ReadCloser
}

// Factory pools transport clients independently from request-scoped cookie state.
type Factory struct {
	mu             sync.Mutex
	clients        map[string]tls_client.HttpClient
	jars           map[string]*list.Element
	jarLRU         *list.List
	cookieJarMax   int
	cookieJarTTL   time.Duration
	defaultTimeout time.Duration
}

const (
	defaultCookieJarMax = 131072
	defaultCookieJarTTL = 24 * time.Hour
)

type cookieJarEntry struct {
	key      string
	jar      *cookiejar.Jar
	lastUsed time.Time
}

// New returns a Factory with a sane default request timeout.
func New() *Factory {
	return &Factory{
		clients:        map[string]tls_client.HttpClient{},
		jars:           map[string]*list.Element{},
		jarLRU:         list.New(),
		cookieJarMax:   defaultCookieJarMax,
		cookieJarTTL:   defaultCookieJarTTL,
		defaultTimeout: 120 * time.Second,
	}
}

// ResolveProfile maps a profile name onto a tls-client browser profile.
//
// Priority order:
//  1. JA3Override on the Request — looked up in profiles.MappedTLSClients (e.g. "firefox_133",
//     "chrome_124") to support explicit operator JA3 overrides without raw-string replay.
//  2. The Profile constant (ProfileChrome / ProfileNode / ProfileRustls).
//  3. Fallback: Chrome_120 (matches sidecar's impersonate=chrome120 default — the bug-fix
//     from the earlier Chrome_131 hardcoding is the primary reason for this function).
//
// ProfileNode and ProfileRustls both resolve to Chrome_120 — the same profile the sidecar
// used for Kiro traffic. This is deliberate and correct; see the ProfileNode doc block at
// the top of the file for the full rationale and captured fingerprint values.
func ResolveProfile(profileName, ja3Override string) profiles.ClientProfile {
	// Operator explicit named-profile override (e.g. "chrome_133", "firefox_120").
	if o := ja3Override; o != "" {
		if p, ok := profiles.MappedTLSClients[o]; ok {
			return p
		}
		// Unknown name: fall through to profile-based resolution rather than 502.
	}
	switch profileName {
	case ProfileChrome, "":
		return profiles.Chrome_120
	case ProfileNode, "node_undici", "undici":
		// Chrome_120 — correct value, not a placeholder.  The sidecar used impersonate=chrome120
		// for Kiro traffic; real Node.js 22 JA3 (9bf9a924…) would diverge from the sidecar and
		// fail the fingerprint gate. See package-level ProfileNode comment for full rationale.
		return profiles.Chrome_120
	case ProfileRustls, "aws_sdk_rust", "amazon_q":
		// Chrome_120 — same reasoning as ProfileNode; real rustls 0.23 JA3 contains 0xFF SCSV
		// which bogdanfinn's BoringSSL layer cannot list as a cipher (ImpersonateError).
		return profiles.Chrome_120
	default:
		// Attempt MappedTLSClients lookup for arbitrary string profile names.
		if p, ok := profiles.MappedTLSClients[profileName]; ok {
			return p
		}
		return profiles.Chrome_120
	}
}

// cacheKey intentionally excludes CookieJarKey so account/session state cannot fragment connections.
func cacheKey(r Request) string {
	return r.Profile + "\x00" + r.JA3Override + "\x00" + r.ProxyURL
}

func (f *Factory) clientFor(r Request) (tls_client.HttpClient, error) {
	key := cacheKey(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.clients[key]; c != nil {
		return c, nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = f.defaultTimeout
	}
	secs := int(timeout / time.Second)
	if secs <= 0 {
		secs = 1
	}
	opts := []tls_client.HttpClientOption{
		tls_client.WithClientProfile(ResolveProfile(r.Profile, r.JA3Override)),
		tls_client.WithTimeoutSeconds(secs),
		// The pool owns failover/retry semantics; the transport must not silently follow
		// redirects (an upstream 3xx is a signal we surface, not chase).
		tls_client.WithNotFollowRedirects(),
	}
	if r.ProxyURL != "" {
		opts = append(opts, tls_client.WithProxyUrl(r.ProxyURL))
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	f.clients[key] = c
	return c, nil
}

// Do issues r and returns a streaming Response. The response Body is the live upstream
// stream (SSE-safe); the caller closes it.
func (f *Factory) Do(ctx context.Context, r Request) (*Response, error) {
	client, err := f.clientFor(r)
	if err != nil {
		return nil, err
	}
	method := r.Method
	if method == "" {
		method = fhttp.MethodPost
	}
	var body io.ReadCloser
	if r.Body != nil && r.Body.Size() > 0 {
		body, err = r.Body.Open()
		if err != nil {
			return nil, err
		}
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, r.URL, body)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, err
	}
	if body != nil {
		defer body.Close()
	}
	if r.Body != nil {
		req.ContentLength = r.Body.Size()
		req.GetBody = r.Body.Open
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if order := r.HeaderOrder; len(order) > 0 {
		req.Header[fhttp.HeaderOrderKey] = lowered(order)
	}
	f.applyCookies(r.CookieJarKey, r.URL, req.Header)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	f.storeCookies(r.CookieJarKey, r.URL, toStdHeader(resp.Header))
	return &Response{
		StatusCode: resp.StatusCode,
		Header:     toStdHeader(resp.Header),
		Body:       resp.Body,
	}, nil
}

func (f *Factory) cookieJar(key string) *cookiejar.Jar {
	if key == "" {
		return nil
	}
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	for back := f.jarLRU.Back(); back != nil; back = f.jarLRU.Back() {
		entry := back.Value.(*cookieJarEntry)
		if f.cookieJarTTL <= 0 || now.Sub(entry.lastUsed) < f.cookieJarTTL {
			break
		}
		f.jarLRU.Remove(back)
		delete(f.jars, entry.key)
	}
	if element := f.jars[key]; element != nil {
		entry := element.Value.(*cookieJarEntry)
		entry.lastUsed = now
		f.jarLRU.MoveToFront(element)
		return entry.jar
	}
	jar, _ := cookiejar.New(nil)
	element := f.jarLRU.PushFront(&cookieJarEntry{key: key, jar: jar, lastUsed: now})
	f.jars[key] = element
	if f.cookieJarMax > 0 && f.jarLRU.Len() > f.cookieJarMax {
		back := f.jarLRU.Back()
		f.jarLRU.Remove(back)
		delete(f.jars, back.Value.(*cookieJarEntry).key)
	}
	return jar
}

func (f *Factory) applyCookies(key, rawURL string, header fhttp.Header) {
	jar := f.cookieJar(key)
	u, err := url.Parse(rawURL)
	if jar == nil || err != nil || header.Get("Cookie") != "" {
		return
	}
	values := make([]string, 0, 4)
	for _, cookie := range jar.Cookies(u) {
		values = append(values, cookie.Name+"="+cookie.Value)
	}
	if len(values) > 0 {
		header.Set("Cookie", strings.Join(values, "; "))
	}
}

func (f *Factory) storeCookies(key, rawURL string, header stdhttp.Header) {
	jar := f.cookieJar(key)
	u, err := url.Parse(rawURL)
	if jar == nil || err != nil {
		return
	}
	jar.SetCookies(u, (&stdhttp.Response{Header: header}).Cookies())
}

// TLSDialerFor returns the TLS dialer function from a pooled client matching the
// profile in r.  The dialer is used by the WebSocket transport (A4) to open a
// fingerprinted TCP+TLS connection that gorilla/websocket then upgrades.
func (f *Factory) TLSDialerFor(r Request) (tls_client.TLSDialerFunc, error) {
	client, err := f.clientFor(r)
	if err != nil {
		return nil, err
	}
	return client.GetTLSDialer(), nil
}

// CloseIdle releases idle keep-alive connections across all pooled clients.
func (f *Factory) CloseIdle() {
	f.mu.Lock()
	clients := make([]tls_client.HttpClient, 0, len(f.clients))
	for _, c := range f.clients {
		clients = append(clients, c)
	}
	f.mu.Unlock()
	for _, c := range clients {
		c.CloseIdleConnections()
	}
}

func lowered(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = toLowerASCII(s)
	}
	return out
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// toStdHeader converts an fhttp.Header into a standard net/http.Header. fhttp.Header is a
// map[string][]string like net/http.Header, but they are distinct types, so the pool sees
// only the standard library type.
func toStdHeader(h fhttp.Header) stdhttp.Header {
	out := make(stdhttp.Header, len(h))
	for k, vs := range h {
		if k == fhttp.HeaderOrderKey || k == fhttp.PHeaderOrderKey {
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}
