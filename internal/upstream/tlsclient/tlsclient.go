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
	"bufio"
	"container/list"
	"context"
	stdtls "crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	stdhttp "net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
	"golang.org/x/net/proxy"
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
// Chrome remains the validated profile for Codex and Kiro. Claude selects the separate
// byte-captured native Bun profile below rather than mixing this browser ClientHello with
// a claude-cli HTTP identity.
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
	// ProfileClaude reproduces the ClientHello of Claude Code 2.1.226's
	// native Bun 1.4.0 binary. It deliberately has no ALPN extension; the
	// shipping client speaks HTTP/1.1 on this path.
	ProfileClaude = "claude_bun"
	// ProfileNode is the Kiro KiroIDE fingerprint (aws-sdk-js / Node.js). Resolves to
	// Chrome_120 — the same profile the sidecar used for Kiro traffic.  See above for why
	// Chrome_120 is the correct value, not a placeholder.
	ProfileNode = "node"
	// ProfileRustls is the Kiro Amazon Q fingerprint (ksk_ keys / aws-sdk-rust). Resolves
	// to Chrome_120 — same reasoning as ProfileNode.
	ProfileRustls = "rustls"
)

// claudeBunProfile is built from the byte-level ClientHello captured through a
// local CONNECT tunnel to api.anthropic.com on 2026-08-09. HTTP/2 settings are
// intentionally empty because the native client advertises no ALPN and never
// reaches an h2 round trip.
var claudeBunProfile = profiles.NewClientProfile(utls.ClientHelloID{
	Client:  "ClaudeCodeBun",
	Version: "2.1.226-bun-1.4.0",
	SpecFactory: func() (utls.ClientHelloSpec, error) {
		return utls.ClientHelloSpec{
			CipherSuites: []uint16{
				utls.TLS_AES_128_GCM_SHA256,
				utls.TLS_AES_256_GCM_SHA384,
				utls.TLS_CHACHA20_POLY1305_SHA256,
				utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
				utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
				utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
				utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
				utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				utls.TLS_RSA_WITH_AES_128_CBC_SHA,
				utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			},
			CompressionMethods: []uint8{utls.CompressionNone},
			Extensions: []utls.TLSExtension{
				&utls.SNIExtension{},
				&utls.ExtendedMasterSecretExtension{},
				&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
				&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
				&utls.SupportedPointsExtension{SupportedPoints: []byte{utls.PointFormatUncompressed}},
				&utls.SessionTicketExtension{},
				&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
					utls.ECDSAWithP256AndSHA256,
					utls.PSSWithSHA256,
					utls.PKCS1WithSHA256,
					utls.ECDSAWithP384AndSHA384,
					utls.PSSWithSHA384,
					utls.PKCS1WithSHA384,
					utls.PSSWithSHA512,
					utls.PKCS1WithSHA512,
					utls.PKCS1WithSHA1,
				}},
				&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
				&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
				&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			},
		}, nil
	},
}, nil, nil, nil, 0, nil, nil, 0, false, nil, nil, 0, nil, false)

// Request is a provider-neutral egress request. HeaderOrder, when set, pins the exact
// wire order of the request headers (the curl_cffi default_headers=false / operator-JA3
// exact-replay behaviour); when empty the profile's own default header set/order is used.
type Request struct {
	Method       string
	URL          string
	Header       stdhttp.Header
	HeaderOrder  []string
	Body         bodysource.BodySource
	Profile      string        // ProfileClaude => captured Bun; ""/ProfileChrome => Chrome_120
	JA3Override  string        // explicit named profile key from profiles.MappedTLSClients ("chrome_120" etc.)
	ProxyURL     string        // optional chain proxy (http(s)/socks5(h))
	CookieJarKey string        // scopes a persistent cookie jar across a multi-call flow
	Timeout      time.Duration // 0 => factory default
	// ForceHTTP1 restricts protocol selection to HTTP/1.1. Profiles with an ALPN
	// extension are narrowed to ["http/1.1"]; profiles without one retain its absence.
	//
	// Claude Code 2.1.226's captured native Bun ClientHello has no ALPN extension and
	// its request is HTTP/1.1. A browser profile offers h2, so an Anthropic edge could
	// otherwise see an HTTP/2 connection carrying a claude-cli User-Agent — a
	// combination the captured client cannot produce. The custom Claude profile
	// already omits ALPN; this flag also protects explicit operator profile overrides.
	//
	// Left false for every non-Anthropic provider so their fingerprints are untouched.
	ForceHTTP1 bool
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
	wsSessionCache utls.ClientSessionCache
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
		wsSessionCache: utls.NewLRUClientSessionCache(128),
	}
}

// ResolveProfile maps a profile name onto a tls-client browser profile.
//
// Priority order:
//  1. JA3Override on the Request — looked up in profiles.MappedTLSClients (e.g. "firefox_133",
//     "chrome_124") to support explicit operator JA3 overrides without raw-string replay.
//  2. The Profile constant (ProfileClaude / ProfileChrome / ProfileNode / ProfileRustls).
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
	case ProfileClaude:
		return claudeBunProfile
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
//
// ForceHTTP1 MUST be part of the key: it is baked into the client's ALPN at construction
// time, so an h1-pinned and a non-pinned request that share a profile+proxy would
// otherwise share one cached client and the first caller to create it would silently
// decide the other's protocol — either leaking h2 onto Claude traffic or dragging another
// provider down to HTTP/1.1.
func cacheKey(r Request) string {
	h1 := "0"
	if r.ForceHTTP1 {
		h1 = "1"
	}
	return r.Profile + "\x00" + r.JA3Override + "\x00" + r.ProxyURL + "\x00" + h1
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
	if r.ForceHTTP1 {
		// utls rewrites an existing ALPNExtension to exactly ["http/1.1"]
		// (u_parrots.go, WithForceHttp1 branch). The Claude profile has no ALPN
		// extension, so it remains byte-identical while the transport stays h1-only.
		opts = append(opts, tls_client.WithForceHttp1())
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
	copyHeadersPreservingCase(req.Header, r.Header)
	if order := r.HeaderOrder; len(order) > 0 {
		req.Header[fhttp.HeaderOrderKey] = lowered(order)
	}
	f.applyCookies(r.CookieJarKey, r.URL, req.Header)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, errors.New("tls transport returned a nil response")
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("tls transport returned a nil response body (status %d)", resp.StatusCode)
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

// TLSDialerFunc opens a fingerprinted TLS connection suitable for a WebSocket
// HTTP/1.1 upgrade. It deliberately lives in this package rather than exposing a
// version-specific tls-client type.
type TLSDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// TLSDialerFor returns a TLS dialer matching the HTTP client's browser profile
// and proxy route. tls-client does not expose its internal TLS dialer, so
// the small WebSocket-specific path is built directly with the same uTLS
// profile. HTTP traffic still uses tls-client and retains its HTTP/2 fingerprint.
func (f *Factory) TLSDialerFor(r Request) (TLSDialerFunc, error) {
	profile := ResolveProfile(r.Profile, r.JA3Override)
	proxyURL, err := parseDialProxy(r.ProxyURL)
	if err != nil {
		return nil, err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = f.defaultTimeout
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.ContainsAny(addr, "\r\n") {
			return nil, errors.New("invalid TLS target")
		}
		rawConn, err := dialRoute(ctx, network, addr, proxyURL, timeout)
		if err != nil {
			return nil, err
		}
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		tlsConfig := &utls.Config{
			ClientSessionCache: f.wsSessionCache,
			ServerName:         host,
			OmitEmptyPsk:       true,
			MinVersion:         utls.VersionTLS12,
		}
		// WebSocket upgrades are HTTP/1.1 only. Disable HTTP/3 explicitly as well
		// so the uTLS profile cannot advertise a transport this dialer cannot use.
		conn := utls.UClient(rawConn, tlsConfig, profile.GetClientHelloId(), false, true, true)
		if err = conn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		return conn, nil
	}, nil
}

func parseDialProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid proxy URL")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, errors.New("unsupported proxy scheme")
	}
}

func dialRoute(ctx context.Context, network, addr string, proxyURL *url.URL, timeout time.Duration) (net.Conn, error) {
	direct := &net.Dialer{Timeout: timeout}
	if proxyURL == nil {
		return direct.DialContext(ctx, network, addr)
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			auth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, direct)
		if err != nil {
			return nil, err
		}
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			return contextDialer.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	case "http", "https":
		return dialHTTPConnect(ctx, network, addr, proxyURL, direct)
	default:
		return nil, errors.New("unsupported proxy scheme")
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func dialHTTPConnect(ctx context.Context, network, addr string, proxyURL *url.URL, direct *net.Dialer) (net.Conn, error) {
	conn, err := direct.DialContext(ctx, network, proxyURL.Host)
	if err != nil {
		return nil, err
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = conn.Close()
		}
	}()

	if strings.EqualFold(proxyURL.Scheme, "https") {
		proxyHost := proxyURL.Hostname()
		tlsConn := stdtls.Client(conn, &stdtls.Config{
			MinVersion: stdtls.VersionTLS12,
			ServerName: proxyHost,
		})
		if err = tlsConn.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = tlsConn
	}

	authHeader := ""
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		encoded := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		authHeader = "Proxy-Authorization: Basic " + encoded + "\r\n"
	}
	if _, err = fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n%s\r\n",
		addr, addr, authHeader,
	); err != nil {
		return nil, err
	}

	reader := bufio.NewReaderSize(conn, 4096)
	statusLine, total, err := readProxyLine(reader, 0)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return nil, errors.New("invalid proxy CONNECT response")
	}
	statusCode, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, errors.New("invalid proxy CONNECT status")
	}
	for {
		var line string
		line, total, err = readProxyLine(reader, total)
		if err != nil {
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if statusCode != stdhttp.StatusOK {
		return nil, fmt.Errorf("proxy CONNECT failed with status %d", statusCode)
	}
	closeOnError = false
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func readProxyLine(reader *bufio.Reader, total int) (string, int, error) {
	const maxProxyHeaderBytes = 32 << 10
	line, err := reader.ReadString('\n')
	total += len(line)
	if total > maxProxyHeaderBytes {
		return "", total, errors.New("proxy CONNECT response headers too large")
	}
	if err != nil {
		return "", total, err
	}
	return line, total, nil
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

// copyHeadersPreservingCase intentionally bypasses Header.Add/Set. Both helpers
// canonicalize field names through textproto, which changed the lower-case
// anthropic-* and x-app/x-api-key names emitted by the native Claude client into
// Go-style title case. fhttp accepts direct map entries and its explicit order map
// remains case-insensitive, so a deep direct copy preserves both wire casing and
// value ownership.
func copyHeadersPreservingCase(dst fhttp.Header, src stdhttp.Header) {
	for k, values := range src {
		dst[k] = append([]string(nil), values...)
	}
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
