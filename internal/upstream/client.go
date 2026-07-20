package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
	"golang.org/x/net/proxy"
)

const drainAndCloseBodyLimit = 8 << 20

type Client struct {
	cfg config.Config
	// liveCfg is an atomically-swappable config overlay. When set (via UpdateConfig),
	// the fingerprint / identity read sites consult it instead of the boot cfg, so an
	// admin settings change (JA3, CLI/node/stainless version, claude_force_direct)
	// takes effect on the next request without a process restart or a client rebuild.
	// nil overlay = use the boot cfg. Only ever stored whole (never mutated in place),
	// so concurrent request reads are race-free.
	liveCfg        atomic.Pointer[config.Config]
	identitySecret []byte
	jars           *jarLRU
	tmu            sync.Mutex
	transports     map[string]*http.Transport
}

// cfgSnapshot returns the live config overlay when an admin has pushed one via
// UpdateConfig, otherwise the boot config. Fingerprint/identity read sites use this
// so operator settings changes are hot; stable bootstrap fields (upstream base URL,
// timeouts) may keep reading c.cfg directly.
func (c *Client) cfgSnapshot() *config.Config {
	if p := c.liveCfg.Load(); p != nil {
		return p
	}
	return &c.cfg
}

// UpdateConfig atomically swaps the live config overlay. The caller (the admin
// settings handler) passes the boot config with the current DB setting overrides
// applied; the next upstream request reads the new fingerprint/identity values.
func (c *Client) UpdateConfig(cfg config.Config) {
	c.liveCfg.Store(&cfg)
}

// SidecarEndpoint returns the configured curl_cffi sidecar base URL ("" if none). The
// registration pipeline uses it to route the OpenAI signup flow through browser-JA3
// impersonation.
func (c *Client) SidecarEndpoint() string {
	return c.cfgSnapshot().DefaultSidecarEndpoint
}

func (c *Client) ImportCookies(accountID, egressID, upstreamHost, fallbackKey, cookieHeader string) error {
	u, err := c.resolveCookieTarget(upstreamHost)
	if err != nil {
		return err
	}
	key := fallbackKey
	if key == "" {
		key = accountID + ":" + egressID
	}
	key = key + ":" + u.Host
	jar := c.cookieJarForKey(key)
	jar.SetCookies(u, parseCookieHeader(cookieHeader))
	return nil
}

// resolveCookieTarget turns an operator-supplied upstream host (bare "chatgpt.com",
// a full URL, or "" = the configured Codex upstream) into the URL whose host scopes
// the cookie jar.
func (c *Client) resolveCookieTarget(upstreamHost string) (*url.URL, error) {
	if upstreamHost == "" {
		u, err := url.Parse(c.cfg.UpstreamBaseURL)
		if err != nil {
			return nil, err
		}
		upstreamHost = u.Host
	}
	target := "https://" + upstreamHost + "/"
	if strings.HasPrefix(upstreamHost, "http://") || strings.HasPrefix(upstreamHost, "https://") {
		target = upstreamHost
	}
	return url.Parse(target)
}

// CookieMapFromHeader parses a "name=value; name2=value2" Cookie header into a flat
// name→value map (the shape the curl_cffi sidecar's /cookies store expects).
func CookieMapFromHeader(header string) map[string]string {
	out := map[string]string{}
	for _, ck := range parseCookieHeader(header) {
		out[ck.Name] = ck.Value
	}
	return out
}

// SeedSidecarCookies pushes a cookie set into the curl_cffi sidecar's on-disk store
// under the SAME cookie_jar_key the live /proxy request path will read (so an injected
// cf_clearance actually rides sidecar-egress requests — the Go in-memory jar is only
// consulted on the direct/proxy transport). It is a no-op when there is no sidecar
// endpoint or nothing to seed. The key derivation mirrors postViaSidecar/cookieJarFor.
func (c *Client) SeedSidecarCookies(ctx context.Context, sidecarEndpoint, accountID, egressID, upstreamHost, fallbackKey string, cookies map[string]string) error {
	if strings.TrimSpace(sidecarEndpoint) == "" || len(cookies) == 0 {
		return nil
	}
	u, err := c.resolveCookieTarget(upstreamHost)
	if err != nil {
		return err
	}
	key := sidecarCookieKey(accountID, egressID, u.String(), fallbackKey)
	payload, err := json.Marshal(map[string]interface{}{"cookie_jar_key": key, "cookies": cookies})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(sidecarEndpoint, "/")+"/cookies", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client, err := c.sidecarHTTPClient()
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("sidecar /cookies status %d", resp.StatusCode)
	}
	return nil
}

type Request struct {
	Method   string
	Provider string // "" / "codex" (default), "claude", or a custom provider id
	// BaseURL is set for a custom OpenAI-compatible provider: the upstream API base
	// (including any "/v1" prefix). doOpenAICompatible appends DownstreamPath
	// (/chat/completions or /models). Ignored by the codex/claude paths, which derive
	// their base from config.
	BaseURL        string
	DownstreamPath string
	Headers        http.Header
	Body           []byte
	Account        storage.Account
	Token          storage.AccountToken
	Egress         storage.EgressProfile
	CookieJarKey   string
	// Model is the downstream-requested model name (may include thinking suffix like
	// "claude-opus-4-8(high)"). Used by thinking configuration resolution to apply
	// model-specific overrides and parse suffix-based thinking hints.
	Model string
	// OSHint, when set ("Mac OS"/"Linux"/"Windows" or aliases), makes the
	// account's virtual identity present that OS family instead of the VPS host
	// (the "downstream" identity source). Empty = use the detected host.
	OSHint string
	// CodexClientVersion, when set, overrides the synthesized Codex CLI version on
	// THIS request's User-Agent and `version` header (OAuth accounts only). Model
	// discovery and version-gated live models use the current client version; empty
	// keeps the normal per-account/default version. Has no effect on API-key traffic.
	CodexClientVersion string
	// CodexResponsesWebSocket sends a Codex Responses turn over the official
	// responses WebSocket beta transport, returning the upstream events as an SSE
	// body to the gateway. It is intended for streaming /v1/responses turns only.
	CodexResponsesWebSocket bool
	// CodexWebSocketSession keeps one upstream WebSocket alive across sequential
	// response.create frames from one downstream WebSocket. Codex 0.144.x performs
	// a generate=false warmup, then references that response id on the same
	// connection; opening a fresh upstream connection per frame loses that state.
	CodexWebSocketSession *CodexResponsesWebSocketSession
	// codexMetadata is an immutable, request-scoped snapshot generated at the Do
	// choke point. HTTP headers, HTTP client_metadata and the WS handshake/body all
	// consume the same snapshot so account-virtualized identifiers cannot drift.
	codexMetadata *codexRequestMetadata
	// PassThrough (Claude provider only) forwards the request to api.anthropic.com as a
	// TRANSPARENT proxy: the client's own Content-Type / Accept / Anthropic-Beta are
	// preserved verbatim and the body is NOT cloaked/virtualized. It is for the extra
	// Anthropic endpoints Claude Code skills / code-execution use — /v1/files (multipart
	// uploads), /v1/skills, /v1/agents|environments|sessions — which are not message
	// turns and must not be rewritten. Account auth + the Claude Code identity headers
	// are still attached, and the call still routes through the account's egress/sidecar.
	PassThrough bool
	// MinimalProbe sends the caller-provided provider-native body unchanged. It is
	// used only by the administrator-confirmed API-key inference probe, which must
	// not add thinking, tools, or cache points.
	MinimalProbe bool
}

type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// requestGuard bounds an upstream call by an IDLE timeout and releases the request
// context only when the response body is Closed. It exists to defeat two distinct
// truncation bugs that both surface downstream as "API Error: Failed to parse JSON"
// or "API returned an empty or malformed response (HTTP 200)" on otherwise-healthy
// accounts:
//
//  1. Premature cancel. Returning a streaming resp.Body from under a `defer cancel()`
//     cancels the request context the instant the upstream function returns — BEFORE
//     the caller reads the body. Go's HTTP/2 transport then resets the stream and
//     closes the body pipe, so only locally-buffered bytes survive and every
//     not-yet-arrived SSE frame is lost. Intermittent by nature: short completions
//     are already buffered when cancel wins, long ones are truncated. Fixed by tying
//     cancel to the body's Close (which every caller defers), never to function return.
//
//  2. Absolute deadline on a long-but-progressing stream. A single context.WithTimeout
//     covering the WHOLE call cuts a legitimately long response (large context / slow
//     TTFT, extended thinking, a long agentic turn) the moment the deadline passes,
//     even though the stream is actively flowing — the exact "long context / continuous
//     waiting re-triggers the error" case. requestGuard instead arms an IDLE watchdog:
//     the timer is reset on every Read, so a stream of ANY total length survives as long
//     as it keeps making progress (Claude emits periodic `ping` events during long
//     thinking, so a healthy stream never goes idle), while a genuinely hung/dead
//     upstream — no bytes for the whole idle window — is still aborted.
//
// Usage: build with newRequestGuard, run the request with the returned ctx, then call
// Fail() on error or Wrap(body) on success. The same watchdog timer bounds the connect
// phase (no response within idle) and, after Wrap, each read of the body.
type requestGuard struct {
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
}

// newRequestGuard returns a derived context for the request plus a guard whose idle
// watchdog is already armed (so a connect that produces no response within idle is
// aborted). idle <= 0 disables the watchdog (the context is still released on Close).
func newRequestGuard(parent context.Context, idle time.Duration) (context.Context, *requestGuard) {
	ctx, cancel := context.WithCancel(parent)
	g := &requestGuard{cancel: cancel, idle: idle}
	if idle > 0 {
		g.timer = time.AfterFunc(idle, cancel)
	}
	return ctx, g
}

// Fail tears the guard down on an error path (no body to hand back): stop the
// watchdog and cancel the context.
func (g *requestGuard) Fail() {
	if g.timer != nil {
		g.timer.Stop()
	}
	g.cancel()
}

// Wrap hands ownership of the guard to the response body: the idle watchdog now
// bounds reads, and Close releases the context. Re-arm the watchdog for the first
// read (the connect phase is over).
func (g *requestGuard) Wrap(body io.ReadCloser) io.ReadCloser {
	if g.timer != nil {
		g.timer.Reset(g.idle)
	}
	return &idleCancelBody{ReadCloser: body, guard: g}
}

// idleCancelBody is the body returned by requestGuard.Wrap. Every Read re-arms the
// idle watchdog (so a flowing stream is never cut), and Close stops the watchdog and
// cancels the request context (so a finished/abandoned stream releases its resources
// and its keep-alive connection returns cleanly to the pool). cancel is idempotent, so
// a double Close is harmless; if a caller forgets to Close, the watchdog still reclaims
// the context after one idle window.
type idleCancelBody struct {
	io.ReadCloser
	guard *requestGuard
}

func (b *idleCancelBody) Read(p []byte) (int, error) {
	if b.guard.timer != nil {
		b.guard.timer.Reset(b.guard.idle)
	}
	return b.ReadCloser.Read(p)
}

func (b *idleCancelBody) Close() error {
	if b.guard.timer != nil {
		b.guard.timer.Stop()
	}
	err := b.ReadCloser.Close()
	b.guard.cancel()
	return err
}

func NewClient(cfg config.Config) *Client {
	cfg.UpstreamBaseURL = NormalizeBaseURL(cfg.UpstreamBaseURL)
	cfg.OpenAIAPIUpstreamBaseURL = NormalizeBaseURL(cfg.OpenAIAPIUpstreamBaseURL)
	secret := identity.ResolveSecret([]byte(cfg.IdentitySecret))
	return &Client{cfg: cfg, identitySecret: secret, jars: newJarLRU(0), transports: map[string]*http.Transport{}}
}

func (c *Client) Do(ctx context.Context, req Request) (*Response, error) {
	if req.Provider == "claude" {
		return c.doClaude(ctx, req)
	}
	// Custom OpenAI-Chat-Completions-compatible provider (DeepSeek, Kimi, …): a clean
	// Bearer-auth OpenAI client, no Codex/Claude fingerprint and no body normalization.
	if IsCustomProvider(req.Provider) {
		return c.doOpenAICompatible(ctx, req)
	}
	// Codex /responses: normalize the two fields the WHAM backend hard-validates so
	// every transport (WS/sidecar/HTTP) sends the real-client shape. (1) "instructions"
	// must be a non-empty string — else 400 {"detail":"Instructions are required"}; the
	// relay forwards the downstream's own (or InjectResponsesSystemPrompt backfills one),
	// but an empty/absent one is backfilled here. (2) "store" must equal what the real
	// Codex client sends for this upstream — false for the chatgpt.com WHAM backend, true
	// only for an Azure responses endpoint (other_codex client.rs:781) — else
	// 400 {"detail":"Store must be set to false"}; a downstream client wrongly sending
	// store:true is corrected. This is the production-relay complement to the health-test
	// probe at server.go:2243, and continues the Session-21 masked-bug chain.
	if strings.Contains(req.DownstreamPath, "/responses") && !req.MinimalProbe {
		isCompact := strings.Contains(strings.ToLower(req.DownstreamPath), "/responses/compact")
		usesAPIKey := AccountUsesAPIKey(req.Token)
		codexBaseURL := c.codexBaseURL(req)
		responsesLite := !usesAPIKey && CodexRequestUsesResponsesLite(req.Body)
		if isCompact && responsesLite {
			req.Body = normalizeCodexResponsesLiteCompactBody(req.Body)
		} else if !isCompact {
			req.Body = normalizeCodexResponsesBody(req.Body, codexBaseURL, responsesLite)
		}
		// Parse the top-level object ONCE here. The remaining normalizations each probe
		// only one or two top-level fields, so they share this single scan instead of
		// re-unmarshalling the whole body 4-6 times. None of the fields these steps
		// inspect (reasoning.effort, prompt_cache_retention, max_output_tokens,
		// thread_id/session_id/conversation_id) are added or removed by the step above,
		// so the map stays an accurate probe. It is nil for a non-object body, and every
		// *WithFields core then returns its input unchanged — the same byte-fidelity
		// passthrough each function had on malformed JSON. Mutations are still applied
		// with targeted sjson edits on the live req.Body, never a map re-marshal.
		var codexFields map[string]json.RawMessage
		_ = json.Unmarshal(req.Body, &codexFields)
		// `ultra` is a client-side Codex capability: the official client enables
		// automatic delegation locally, but serializes `max` on every Responses wire
		// path (including /responses/compact). Keep the downstream/config value intact
		// until this final upstream boundary, then mirror that wire contract.
		req.Body = normalizeCodexReasoningEffortForWireWithFields(req.Body, codexFields)
		// Current codex-rs has no prompt_cache_retention request field on either
		// HTTP or Responses-over-WebSocket. Keep prompt_cache_key (the supported
		// cache-affinity control), but strip this obsolete extension consistently.
		req.Body = stripCodexResponsesPromptCacheRetentionWithFields(req.Body, codexFields)
		if !usesAPIKey {
			// The ChatGPT Codex/WHAM contract does not accept the public Responses API's
			// max_output_tokens field. Claude Code always sends Anthropic max_tokens;
			// the Messages -> Chat -> Responses bridge preserves it until this transport
			// boundary, where an OAuth account must mirror the official Codex client and
			// omit the unsupported field. API-key Responses endpoints keep the limit.
			req.Body = stripCodexResponsesMaxOutputTokensWithFields(req.Body, codexFields)
			metadata := c.newCodexRequestMetadataWithResponsesLite(req, responsesLite)
			req.codexMetadata = &metadata
			// ApiCompactionInput projects the same identity through headers; only
			// normal Responses turns serialize client_metadata in the request body.
			if !isCompact {
				req.Body = applyCodexClientMetadataWithFields(req.Body, codexFields, metadata, req.CodexResponsesWebSocket)
			}
		}
		// Downstream Codex WebSocket clients may include transport correlators at
		// the request root. They are useful for routing/identity derivation above,
		// but are not valid upstream Responses parameters.
		req.Body = stripCodexTopLevelTransportCorrelatorsWithFields(req.Body, codexFields)
	}
	if req.CodexResponsesWebSocket {
		return c.doCodexResponsesWebSocket(ctx, req)
	}
	switch req.Egress.Type {
	case "curl_cffi_sidecar":
		return c.doSidecar(ctx, req)
	default:
		return c.doHTTP(ctx, req)
	}
}

func (c *Client) doHTTP(ctx context.Context, spec Request) (*Response, error) {
	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}
	target := ComputeURL(c.codexBaseURL(spec), spec.DownstreamPath)
	// An IDLE-timeout guard (reset on every read) bounds the call without cutting a
	// long-but-progressing stream, and releases the context on the body's Close — never
	// on this function's return (see requestGuard).
	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(spec.Body))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	// Build exactly the header set the official client would send (allowlist +
	// account-bound identity). We deliberately do NOT forward arbitrary
	// downstream headers, which would leak the relay's presence to the upstream.
	c.applyCodexHeaders(req.Header, spec)
	if req.Header.Get("Content-Type") == "" && len(spec.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	client, err := c.httpClientForEgress(spec)
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

func (c *Client) httpClientForEgress(spec Request) (*http.Client, error) {
	transport, err := c.transportForEgress(spec.Egress)
	if err != nil {
		return nil, err
	}
	// The Transport (which OWNS the connection pool) is cached and shared; only the
	// lightweight Client wrapper + per-account cookie jar are built per request.
	return &http.Client{Transport: transport, Jar: c.cookieJarFor(spec)}, nil
}

func (c *Client) sidecarHTTPClient() (*http.Client, error) {
	transport, err := c.transportForEgress(storage.EgressProfile{Type: "direct"})
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport}, nil
}

// transportKey is the cache key for a reusable Transport. Egresses that dial the
// same way (same proxy/socks endpoint, or all "direct") share one Transport — and
// therefore one keep-alive connection pool — so repeated requests reuse warm
// TCP/TLS/HTTP2 connections instead of handshaking every time.
func transportKey(egress storage.EgressProfile) string {
	switch egress.Type {
	case "", "direct":
		return "direct"
	default:
		return egress.Type + "|" + egress.Endpoint
	}
}

// transportForEgress returns a process-lifetime Transport for the egress, building
// it once and caching it. Reusing the Transport is what makes Go's connection pool
// (MaxIdleConnsPerHost / IdleConnTimeout below) actually reuse connections; a fresh
// Transport per request — the previous behavior — gave every request an empty pool
// and thus a full DNS+TLS handshake, the dominant per-request latency.
func (c *Client) transportForEgress(egress storage.EgressProfile) (*http.Transport, error) {
	key := transportKey(egress)
	c.tmu.Lock()
	defer c.tmu.Unlock()
	if t := c.transports[key]; t != nil {
		return t, nil
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		// Larger socket buffers than the 4KiB stdlib default: this proxy moves very large
		// request bodies (multi-MB 1M-context turns) and long SSE streams, so 64KiB read/
		// write buffers cut the syscall count per request substantially with negligible
		// memory cost (bounded by the idle-conn caps above). Pure throughput — no behavior
		// change to what is sent or received.
		WriteBufferSize: 64 << 10,
		ReadBufferSize:  64 << 10,
		// Negotiate HTTP/2 over TLS like the official clients do. Full JA3/JA4
		// mimicry requires a TLS-impersonating egress (curl_cffi sidecar); the
		// stdlib transport at least presents a modern HTTP/2 + TLS 1.2+ profile
		// rather than an obviously non-browser one.
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			ClientSessionCache: tls.NewLRUClientSessionCache(128),
		},
	}
	switch egress.Type {
	case "", "direct":
	case "http_proxy", "https_proxy", "warp_proxy":
		if egress.Endpoint == "" {
			return nil, errors.New("proxy egress endpoint required")
		}
		proxyURL, err := url.Parse(egress.Endpoint)
		if err != nil {
			return nil, err
		}
		// url.Parse carries any user:pass userinfo, which Go's transport turns into
		// a Proxy-Authorization header on the CONNECT, so authenticated HTTP(S)
		// proxies work without extra wiring.
		transport.Proxy = http.ProxyURL(proxyURL)
	case "socks5h_proxy", "socks5_proxy":
		if egress.Endpoint == "" {
			return nil, errors.New("socks5 egress endpoint required")
		}
		addr, auth := socksAuthAndAddr(egress.Endpoint)
		dialer, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			type contextDialer interface {
				DialContext(context.Context, string, string) (net.Conn, error)
			}
			if d, ok := dialer.(contextDialer); ok {
				return d.DialContext(ctx, network, addr)
			}
			return dialer.Dial(network, addr)
		}
	default:
		return nil, fmt.Errorf("unsupported egress type %q", egress.Type)
	}
	c.transports[key] = transport
	return transport, nil
}

func (c *Client) doSidecar(ctx context.Context, spec Request) (*Response, error) {
	if spec.Egress.Endpoint == "" {
		return nil, errors.New("curl_cffi_sidecar endpoint required")
	}
	target := ComputeURL(c.codexBaseURL(spec), spec.DownstreamPath)
	built := http.Header{}
	c.applyCodexHeaders(built, spec)
	// TLS/JA3 the sidecar replays for Codex. DEFAULT IS CHROME (""), i.e. the
	// sidecar's native impersonation — and so is the "real Codex" opt-in. The real
	// Codex client does NO JA3 spoofing (vanilla reqwest 0.12 + rustls 0.23, verified
	// against the Codex source in other_codex), its vanilla-rustls ClientHello carries
	// the 0xFF SCSV signalling value curl_cffi/BoringSSL cannot list as a cipher, and
	// chatgpt.com's CF edge whitelists the Chrome JA3 curl_cffi reproduces natively —
	// so matching Codex's TLS buys nothing and used to 502 every request. See
	// resolveCodexJA3 for the full rationale; the sidecar now degrades gracefully if an
	// explicit operator JA3 still can't be replayed. Only OAuth/ChatGPT-session accounts
	// would carry a Codex fingerprint at all (API-key accounts are plain SDK clients),
	// so an API-key request always keeps the generic impersonation.
	ja3 := ""
	if !AccountUsesAPIKey(spec.Token) {
		ja3 = resolveCodexJA3(c.cfgSnapshot().CodexJA3Override)
	}
	// Preserve the historical browser-shaped defaults for ChatGPT OAuth. Platform
	// API keys are ordinary SDK traffic, so their complete header set must not be
	// mixed with injected sec-ch-ua/sec-fetch browser headers.
	defaultHeaders := !AccountUsesAPIKey(spec.Token)
	return c.postViaSidecar(ctx, spec, target, built, c.cfg.SidecarTimeout(), ja3, defaultHeaders)
}

// resolveCodexJA3 resolves the TLS/JA3 fingerprint the curl_cffi sidecar replays for
// Codex/OpenAI traffic from the operator's codex_ja3 setting.
//
// THE DEFAULT IS CHROME (""), i.e. the sidecar's native impersonation — and so is every
// "real Codex" alias. This is grounded in the Codex source (other_codex) plus a hard
// technical limit:
//   - VERIFIED AGAINST THE CODEX SOURCE: the real Codex client does NO TLS/JA3 spoofing.
//     backend-client and login/auth build a stock reqwest 0.12 client over rustls 0.23
//     (features "rustls-tls"/"rustls-tls-native-roots") with zero ClientHello, cipher,
//     or ALPN customization — just default_headers (originator=codex_cli_rs, UA) and an
//     HTTP/2-by-ALPN transport. So there is no special Codex fingerprint to "match"; its
//     JA3 is simply whatever vanilla rustls emits.
//   - That vanilla-rustls ClientHello (identity.CodexJA3) lists cipher 255 (0xFF), the
//     TLS_EMPTY_RENEGOTIATION_INFO_SCSV *signalling* value — not a real, listable cipher.
//     curl_cffi/BoringSSL emit it automatically via the renegotiation_info extension and
//     cannot list it as a cipher, so forcing this JA3 raised ImpersonateError("Cipher
//     0xff is not found"), which the sidecar turned into a 502 — breaking 100% of Codex
//     sidecar traffic. (The sidecar now degrades gracefully instead of 502'ing; see
//     curl_cffi_sidecar.py. We still keep this fingerprint off the default path.)
//   - chatgpt.com sits behind Cloudflare, which whitelists the Chrome JA3 curl_cffi
//     reproduces natively; the OAuth token is the real auth. Even a successful partial
//     replay (rustls ciphers on curl_cffi's Chrome extension base) would be an INCOHERENT
//     third fingerprint — strictly worse than coherent Chrome — for no detection benefit.
//     Same conclusion as Claude (see resolveClaudeJA3).
//
// Operators who insist can still force a SPECIFIC JA3 by setting codex_ja3 to an explicit
// JA3 string (sanitized of unlistable SCSV signalling values, then best-effort replayed —
// the sidecar falls back to Chrome if its local curl_cffi build still can't reproduce it,
// never a 502). The "codex-cli"/"real"/"native"/"rust"/"codex" aliases, and
// ""/"off"/"none"/"disabled"/"-"/"chrome"/"browser", all keep Chrome.
func resolveCodexJA3(override string) string {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "", "off", "none", "disabled", "-", "chrome", "browser",
		// Real-Codex aliases are intentionally Chrome too: Codex does no JA3 spoofing
		// (vanilla rustls — verified in other_codex), its JA3 can't be cleanly replayed
		// (0xFF SCSV), and a partial replay buys nothing. See the doc comment above.
		"codex-cli", "codex_cli", "real", "native", "rust", "codex":
		return "" // sidecar's native Chrome impersonation (default)
	default:
		// Explicit raw JA3 string escape hatch (sanitized at the postViaSidecar choke
		// point; the sidecar degrades to Chrome if it still can't be replayed).
		return strings.TrimSpace(override)
	}
}

// sanitizeJA3 strips TLS signalling pseudo-ciphers from a JA3 string's cipher list before
// it is handed to the curl_cffi sidecar. The SCSV values 255 (0x00FF,
// TLS_EMPTY_RENEGOTIATION_INFO_SCSV — emitted by rustls, hence present in the real Codex
// client's captured JA3) and 22016 (0x5600, TLS_FALLBACK_SCSV) are *signals*, not real
// ciphers: BoringSSL/curl_cffi manage them via extensions/options and reject them when
// listed as ciphers (ImpersonateError "Cipher 0xff is not found"). Removing them lets an
// explicit operator-supplied JA3 (e.g. one pasted from a rustls capture) actually replay
// instead of forcing a sidecar fallback/502. JA3 format is
// "version,ciphers,extensions,curves,points" with '-'-separated lists; only the cipher
// field (index 1) is touched. Malformed/empty input is returned unchanged.
func sanitizeJA3(ja3 string) string {
	parts := strings.Split(ja3, ",")
	if len(parts) < 2 || parts[1] == "" {
		return ja3
	}
	ciphers := strings.Split(parts[1], "-")
	kept := make([]string, 0, len(ciphers))
	for _, c := range ciphers {
		if t := strings.TrimSpace(c); t == "255" || t == "22016" {
			continue // 0x00FF / 0x5600 SCSV signalling values, not listable ciphers
		}
		kept = append(kept, c)
	}
	parts[1] = strings.Join(kept, "-")
	return strings.Join(parts, ",")
}

// postViaSidecar forwards an already-built request (target URL + final upstream
// header set) through the curl_cffi sidecar, which performs the actual network
// call with a real client TLS/JA3 + HTTP2 fingerprint (impersonation) instead of
// the Go standard library's. It is provider-neutral: the caller decides the URL,
// headers (Codex vs Claude), and overall timeout, so both upstreams can share the
// impersonating transport. The sidecar streams the upstream response back
// verbatim. timeout bounds the whole call (connect + stream); pass the streaming
// request timeout for long SSE responses, not just a connect budget. ja3, when
// non-empty, is a JA3 ClientHello string the sidecar replays so the upstream sees
// that exact TLS fingerprint (Codex's own) instead of the impersonation profile's;
// pass "" to keep the sidecar's default impersonation (e.g. for Claude).
// canonicalizeHeaders rebuilds an upstream header map (as forwarded by the sidecar,
// with verbatim — often lowercase, HTTP/2-style — key casing) into an http.Header with
// canonical keys, so http.Header.Get works. Without this, Get("Content-Type") misses a
// lowercase "content-type" key and isEventStream() wrongly reports a non-stream, sending
// the relay down the buffered (whole-body) path instead of streaming token-by-token.
func canonicalizeHeaders(raw map[string][]string) http.Header {
	canon := make(http.Header, len(raw))
	for k, vs := range raw {
		ck := http.CanonicalHeaderKey(k)
		canon[ck] = append(canon[ck], vs...)
	}
	return canon
}

// defaultHeaders controls whether the sidecar lets curl-impersonate INJECT the
// impersonated browser's own header set (sec-ch-ua*, sec-fetch-*, accept-language,
// upgrade-insecure-requests, …) on top of `built`. Pass false whenever `built` is a
// complete, authentic non-browser client fingerprint (e.g. the claude-cli/Node header
// set): the browser extras would otherwise ride alongside a claude-cli User-Agent +
// x-stainless-* headers, an incoherent combination no real client emits and thus a
// clear impersonation-relay tell. The TLS/HTTP2 impersonation is unaffected either way.
// Pass true to keep curl's browser defaults (a browser-shaped call, e.g. the CF-walled
// OpenAI signup flow, or a path validated to need them). An explicit ja3 already forces
// them off in the sidecar regardless.
func (c *Client) postViaSidecar(ctx context.Context, spec Request, target string, built http.Header, timeout time.Duration, ja3 string, defaultHeaders bool) (*Response, error) {
	if spec.Egress.Endpoint == "" {
		return nil, errors.New("curl_cffi_sidecar endpoint required")
	}
	headers := map[string][]string{}
	for k, values := range built {
		headers[k] = append([]string(nil), values...)
	}
	// The request body travels as the raw HTTP body; only the small routing metadata is
	// base64'd, into the X-Sidecar-Meta header. This avoids base64-inflating a large
	// (1M-context) body by ~33% plus a full encode here and decode in the sidecar on
	// every call — the body bytes are forwarded verbatim. The sidecar still accepts the
	// legacy body_b64-in-JSON shape (no X-Sidecar-Meta header) for rolling deploys.
	meta := map[string]interface{}{
		"method":         firstNonEmpty(spec.Method, http.MethodPost),
		"url":            target,
		"headers":        headers,
		"cookie_jar_key": sidecarCookieKey(spec.Account.ID, spec.Egress.ID, target, spec.CookieJarKey),
		"stream":         true,
	}
	if !defaultHeaders {
		// Tell the sidecar NOT to let curl-impersonate inject the browser's own header set
		// on top of `built` (see the doc comment). Emitted only when suppressing, so the
		// wire meta for every existing (browser-shaped) caller is byte-identical — an older
		// sidecar simply ignores the unknown key during a rolling deploy.
		meta["default_headers"] = false
	}
	if ja3 != "" {
		// Strip unlistable SCSV signalling values (e.g. rustls' 0xFF) so an explicit
		// operator JA3 can actually replay; the sidecar still degrades to Chrome if its
		// local curl_cffi build can't reproduce the rest. Codex/Claude default paths pass
		// ja3="" (Chrome) and never reach here.
		meta["ja3"] = sanitizeJA3(ja3)
	}
	// A sidecar egress may chain through an upstream proxy (e.g. a WARP exit's local
	// SOCKS5): the sidecar then presents the real JA3 AND leaves from that proxy's IP.
	if cp := strings.TrimSpace(spec.Egress.ChainProxy); cp != "" {
		meta["proxy"] = cp
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return nil, err
	}
	// Idle-timeout guard, released on the body's Close (see requestGuard): bounds the
	// sidecar call (connect + each streamed chunk) without truncating a long-but-
	// progressing upstream response that the sidecar relays back chunk by chunk.
	ctx, guard := newRequestGuard(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(spec.Egress.Endpoint, "/")+"/proxy", bytes.NewReader(spec.Body))
	if err != nil {
		guard.Fail()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Sidecar-Meta", base64.StdEncoding.EncodeToString(metaRaw))
	client, err := c.sidecarHTTPClient()
	if err != nil {
		guard.Fail()
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		guard.Fail()
		return nil, err
	}
	header := resp.Header.Clone()
	if statusHeader := header.Get("x-sidecar-upstream-status"); statusHeader != "" {
		var status int
		_, _ = fmt.Sscanf(statusHeader, "%d", &status)
		if status > 0 {
			resp.StatusCode = status
		}
	}
	if encoded := header.Get("x-sidecar-upstream-headers-b64"); encoded != "" {
		if decoded, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			var rawHeaders map[string][]string
			if json.Unmarshal(decoded, &rawHeaders) == nil {
				header = canonicalizeHeaders(rawHeaders)
			}
		}
	}
	return &Response{StatusCode: resp.StatusCode, Header: header, Body: guard.Wrap(resp.Body)}, nil
}

// codexProtocolHeaders is the allowlist of request headers the official Codex
// CLI legitimately sends and that carry protocol/caching meaning. Anything not
// on this list (random User-Agent, x-stainless-*, x-forwarded-*, cookies, etc.)
// is dropped so the upstream sees a clean, official-looking request.
var codexProtocolHeaders = map[string]bool{
	"accept":                                true,
	"openai-beta":                           true,
	"conversation_id":                       true,
	"x-codex-turn-state":                    true,
	"x-codex-turn-metadata":                 true,
	"x-codex-window-id":                     true,
	"x-codex-parent-thread-id":              true,
	"x-codex-beta-features":                 true,
	"x-openai-subagent":                     true,
	"x-client-request-id":                   true,
	"x-openai-memgen-request":               true,
	"x-openai-internal-codex-residency":     true,
	"x-responsesapi-include-timing-metrics": true,
}

// applyCodexHeaders writes the upstream Codex request headers into dst from
// scratch: it forwards only allowlisted downstream protocol headers, then sets
// the auth + account-bound identity headers that the official client carries.
func (c *Client) applyCodexHeaders(dst http.Header, spec Request) {
	usesAPIKey := AccountUsesAPIKey(spec.Token)

	for k, values := range spec.Headers {
		lowerName := strings.ToLower(strings.TrimSpace(k))
		if !codexProtocolHeaders[lowerName] || lowerName == "x-codex-beta-features" {
			continue
		}
		for _, v := range values {
			if !validCodexSemanticHeader(lowerName, v, spec.CodexResponsesWebSocket) {
				continue
			}
			dst.Add(k, v)
		}
	}

	addAuthHeaders(dst, spec.Token)
	// The official Codex client always POSTs a JSON body to /responses with an explicit
	// Content-Type: application/json (verified in other_codex: codex-client/src/request.rs
	// inserts CONTENT_TYPE = "application/json"; the body is RequestBody::Json). The relay
	// builds the Codex header set from scratch and does NOT forward the downstream's
	// Content-Type (it isn't in codexProtocolHeaders), so it must be set here. The
	// direct/proxy doHTTP path sets it as a post-hoc fallback, but doSidecar does not —
	// which made every Codex SIDECAR request reach chatgpt.com with no content type and
	// get rejected with 400 {"detail":"Unsupported content type"}. Setting it in this
	// shared builder fixes all transports (the WS path overwrites it afterward).
	if len(spec.Body) > 0 && dst.Get("Content-Type") == "" {
		dst.Set("Content-Type", "application/json")
	}
	if dst.Get("Accept") == "" {
		if strings.EqualFold(firstNonEmpty(spec.Method, http.MethodPost), http.MethodGet) || (usesAPIKey && !bodyStreamTrue(spec.Body)) {
			dst.Set("Accept", "application/json")
		} else {
			dst.Set("Accept", "text/event-stream")
		}
	}
	// NOTE: do not set a "Connection" header — Go's HTTP/2 transport rejects
	// requests carrying hop-by-hop headers (checkConnHeaders), which would break
	// real chatgpt.com (HTTP/2) traffic. The transport manages keep-alive itself.

	// API-key accounts are not ChatGPT-session clients, so they must NOT carry
	// the Codex CLI session fingerprint (UA/Originator/Account-ID/Session_id).
	// Only OAuth (AT / Sign-in-with-ChatGPT) accounts present it.
	if usesAPIKey {
		return
	}

	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)
	// Mirror the downstream client's launch entrypoint (interactive `codex` vs
	// `codex exec`) so Originator + User-Agent agree with each other and with what
	// the user is actually running, while the OS/arch/version stay account-bound
	// (the virtual identity is never the downstream's real one). Default to the
	// interactive CLI when the downstream sent nothing recognizable.
	threadOriginator := codexThreadOriginator(spec.Headers)
	processOriginator := codexProcessOriginator(spec.Headers, threadOriginator)
	version := c.cfgSnapshot().CodexCLIVersionOrDefault(id.CodexCLIVersion)
	// A per-request override (model-discovery probe or version-gated live model) wins,
	// so the UA/`version` header agree with the client version required upstream.
	if v := strings.TrimSpace(spec.CodexClientVersion); v != "" {
		version = v
	}
	if dst.Get("User-Agent") == "" {
		dst.Set("User-Agent", id.CodexUserAgentForOriginator(processOriginator, version))
	}
	setIfEmptyPreserveCase(dst, "Originator", threadOriginator)
	// The built-in OpenAI provider installs `version` as a provider-wide default,
	// including on /models discovery. RemoteCompactionV2, by contrast, is a
	// Responses-only session feature.
	if spec.DownstreamPath == "" || strings.Contains(spec.DownstreamPath, "/responses") || strings.Contains(spec.DownstreamPath, "/models") {
		setHeaderPreserveCase(dst, "version", version)
	}
	if spec.DownstreamPath == "" || strings.Contains(spec.DownstreamPath, "/responses") {
		setHeaderPreserveCase(dst, "x-codex-beta-features", mergeCodexBetaFeatures(getHeaderFold(spec.Headers, "x-codex-beta-features")))
	}
	// Session identity and compatibility projections come from one canonical
	// request snapshot, matching CodexResponsesMetadata in codex-rs. /models is not
	// a turn and therefore carries only the default auth/UA/originator headers.
	metadata := spec.codexMetadata
	if metadata == nil && (spec.DownstreamPath == "" || strings.Contains(spec.DownstreamPath, "/responses")) {
		generated := c.newCodexRequestMetadata(spec)
		metadata = &generated
	}
	if metadata != nil {
		setHeaderPreserveCase(dst, "session-id", metadata.sessionID)
		setHeaderPreserveCase(dst, "thread-id", metadata.threadID)
		setHeaderPreserveCase(dst, "x-client-request-id", metadata.threadID)
		setHeaderPreserveCase(dst, "x-codex-window-id", metadata.windowID)
		if metadata.turnMetadata != "" {
			setHeaderPreserveCase(dst, "x-codex-turn-metadata", metadata.turnMetadata)
		}
		if metadata.parentThreadID != "" {
			setHeaderPreserveCase(dst, "x-codex-parent-thread-id", metadata.parentThreadID)
		} else {
			deleteHeaderFold(dst, "x-codex-parent-thread-id")
		}
		if metadata.subagent != "" {
			setHeaderPreserveCase(dst, codexSubagentHeader, metadata.subagent)
		} else {
			deleteHeaderFold(dst, codexSubagentHeader)
		}
		if strings.Contains(strings.ToLower(spec.DownstreamPath), "compact") {
			setHeaderPreserveCase(dst, codexInstallationIDMetadataKey, metadata.installationID)
		}
		if metadata.responsesLite && !spec.CodexResponsesWebSocket {
			setHeaderPreserveCase(dst, codexResponsesLiteHeader, "true")
		}
	}
	if spec.Account.UpstreamAccountID != "" {
		setIfEmptyPreserveCase(dst, "ChatGPT-Account-ID", spec.Account.UpstreamAccountID)
	}
	if spec.Account.IsFedramp {
		setIfEmptyPreserveCase(dst, "X-OpenAI-Fedramp", "true")
	}
}

// codexEntrypoint returns the Codex launch entrypoint the downstream client
// presented — identity.CodexOriginatorExec ("codex_exec") when the incoming
// Originator header or User-Agent indicates the `codex exec` subcommand, otherwise
// identity.CodexOriginator ("codex_cli_rs", the interactive CLI and the default).
// Detection reads the raw downstream headers even though Originator/User-Agent are
// not on the forward allowlist; the relay synthesizes its own coherent values.
func codexEntrypoint(h http.Header) string {
	return codexThreadOriginator(h)
}

func codexThreadOriginator(h http.Header) string {
	if candidate := strings.TrimSpace(getHeaderFold(h, "Originator")); isRecognizedCodexOriginator(candidate) {
		return candidate
	}
	if candidate := codexUserAgentOriginator(getHeaderFold(h, "User-Agent")); isRecognizedCodexOriginator(candidate) {
		return candidate
	}
	return identity.CodexOriginator
}

func codexProcessOriginator(h http.Header, threadOriginator string) string {
	if candidate := codexUserAgentOriginator(getHeaderFold(h, "User-Agent")); isRecognizedCodexOriginator(candidate) {
		return candidate
	}
	if isRecognizedCodexOriginator(threadOriginator) {
		return threadOriginator
	}
	return identity.CodexOriginator
}

func codexUserAgentOriginator(value string) string {
	value = strings.TrimSpace(value)
	slash := strings.IndexByte(value, '/')
	if slash <= 0 {
		return ""
	}
	return strings.TrimSpace(value[:slash])
}

func isRecognizedCodexOriginator(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	switch strings.ToLower(value) {
	case "codex_cli_rs", "codex_exec", "codex-tui", "codex_tui", "codex_vscode",
		"codex_desktop", "codex-app-server", "codex_mcp_server", "codex-cli",
		"codex_sdk_ts", "codex-app-server-sdk", "codex_atlas", "codex_chatgpt_desktop",
		"codex_work_desktop", "codex_work_web", "codex_work_mobile", "codex_work_cca", "chatgpt_cca":
		return true
	}
	return strings.HasPrefix(value, "Codex ")
}

func validCodexSemanticHeader(name, value string, websocket bool) bool {
	value = strings.TrimSpace(value)
	switch name {
	case "x-openai-memgen-request":
		return value == "true"
	case "x-openai-internal-codex-residency":
		return value == "us"
	case "x-responsesapi-include-timing-metrics":
		return websocket && value == "true"
	default:
		return true
	}
}

func mergeCodexBetaFeatures(downstream string) string {
	allowed := map[string]bool{
		"memories":             true,
		"network_proxy":        true,
		"prevent_idle_sleep":   true,
		"remote_compaction_v2": true,
	}
	seen := map[string]bool{}
	features := make([]string, 0, 3)
	for _, raw := range strings.Split(downstream, ",") {
		feature := strings.ToLower(strings.TrimSpace(raw))
		if !allowed[feature] || seen[feature] {
			continue
		}
		seen[feature] = true
		features = append(features, feature)
	}
	for _, required := range strings.Split(codexBetaFeaturesHeader, ",") {
		required = strings.TrimSpace(required)
		if required != "" && !seen[required] {
			seen[required] = true
			features = append(features, required)
		}
	}
	return strings.Join(features, ",")
}

// codexRunCorrelator returns the first present per-run correlator the downstream
// Codex client sent (conversation_id, then the x-codex window/parent-thread ids),
// or "" if none. These are stable within one run/conversation and rotate across
// runs, so they make a good rotation seed for the derived Session_id.
func codexRunCorrelator(h http.Header) string {
	for _, key := range []string{"conversation_id", "x-codex-window-id", "x-codex-parent-thread-id"} {
		if v := getHeaderFold(h, key); v != "" {
			return v
		}
	}
	return ""
}

// getHeaderFold returns the value of the first header whose name case-insensitively
// matches key (Go's canonical Get would miss the fingerprint-preserved "Session_id"
// casing), or "" if none is present.
func getHeaderFold(h http.Header, key string) string {
	for k, vs := range h {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// setHeaderPreserveCase replaces any case-insensitive variant of key with a single
// value under the exact provided casing (matters for "Session_id").
func setHeaderPreserveCase(h http.Header, key, value string) {
	for k := range h {
		if strings.EqualFold(k, key) {
			delete(h, k)
		}
	}
	h[key] = []string{value}
}

// setIfEmptyPreserveCase sets header key (preserving the exact provided casing,
// which matters for fingerprint-sensitive headers like "Session_id") only if no
// case-insensitive variant is already present.
func setIfEmptyPreserveCase(h http.Header, key, value string) {
	if value == "" {
		return
	}
	for existing := range h {
		if strings.EqualFold(existing, key) {
			return
		}
	}
	h[key] = []string{value}
}

// AccountUsesAPIKey recognizes both the direct stored form (OpenAIAPIKey only)
// and auth.json imports, whose parser mirrors that key into AccessToken. A distinct
// access token still means OAuth even if an auxiliary API-key field is present.
func AccountUsesAPIKey(token storage.AccountToken) bool {
	return accountprovider.UsesAPIKey(accountprovider.InferProviderFromToken(token), token)
}

func addAuthHeaders(h http.Header, token storage.AccountToken) {
	bearer := accountprovider.Credential("codex", token)
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
}

func DrainAndClose(body io.ReadCloser) ([]byte, error) {
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, drainAndCloseBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > drainAndCloseBodyLimit {
		return nil, fmt.Errorf("upstream response body exceeds %d bytes", drainAndCloseBodyLimit)
	}
	return raw, nil
}

func trimSocksScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "socks5h://")
	endpoint = strings.TrimPrefix(endpoint, "socks5://")
	return endpoint
}

// socksAuthAndAddr splits a SOCKS5 endpoint into the dial address and optional
// username/password auth. It accepts "socks5h://user:pass@host:port",
// "user:pass@host:port", and bare "host:port".
func socksAuthAndAddr(endpoint string) (string, *proxy.Auth) {
	e := strings.TrimSpace(endpoint)
	if strings.Contains(e, "://") {
		if u, err := url.Parse(e); err == nil && u.Host != "" {
			var auth *proxy.Auth
			if u.User != nil {
				pw, _ := u.User.Password()
				if u.User.Username() != "" {
					auth = &proxy.Auth{User: u.User.Username(), Password: pw}
				}
			}
			return u.Host, auth
		}
		e = trimSocksScheme(e)
	}
	if at := strings.LastIndex(e, "@"); at >= 0 {
		cred, host := e[:at], e[at+1:]
		user, pass, _ := strings.Cut(cred, ":")
		if user != "" {
			return host, &proxy.Auth{User: user, Password: pass}
		}
		return host, nil
	}
	return e, nil
}

// ProbeResult is the parsed exit-location of an egress proxy.
type ProbeResult struct {
	IP        string `json:"ip"`
	Country   string `json:"country"`
	Region    string `json:"region"`
	City      string `json:"city"`
	LatencyMS int64  `json:"latency_ms"`
}

// ProbeEgress sends a GET to the configured geo-probe URL THROUGH the given
// egress, so the response reflects the proxy's real exit IP + region; it doubles
// as a connectivity/health check. Common ipapi.co / ip-api.com / ipinfo.io field
// names are parsed. A direct (no-proxy) egress reports the host's own exit.
func (c *Client) ProbeEgress(ctx context.Context, egress storage.EgressProfile) (ProbeResult, error) {
	geoURL := strings.TrimSpace(c.cfg.GeoProbeURL)
	if geoURL == "" {
		geoURL = config.DefaultGeoProbeURL
	}
	client, err := c.httpClientForEgress(Request{Egress: egress})
	if err != nil {
		return ProbeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoURL, nil)
	if err != nil {
		return ProbeResult{}, err
	}
	req.Header.Set("User-Agent", "curl/8.4.0")
	req.Header.Set("Accept", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return ProbeResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	latency := time.Since(start).Milliseconds()
	if resp.StatusCode >= 400 {
		return ProbeResult{}, fmt.Errorf("geo probe status %d", resp.StatusCode)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ProbeResult{}, fmt.Errorf("geo probe parse: %w", err)
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
		return ""
	}
	return ProbeResult{
		IP:        pick("ip", "query", "ip_addr", "YourFuckingIPAddress"),
		Country:   pick("country_code", "countryCode", "country"),
		Region:    pick("region", "region_name", "regionName", "region_code"),
		City:      pick("city"),
		LatencyMS: latency,
	}, nil
}

func sidecarCookieKey(accountID, egressID, target, fallback string) string {
	if fallback == "" {
		fallback = accountID + ":" + egressID
	}
	u, err := url.Parse(target)
	if err != nil {
		return fallback
	}
	return fallback + ":" + u.Host
}

func (c *Client) cookieJarFor(spec Request) *cookiejar.Jar {
	target := ComputeURL(c.codexBaseURL(spec), spec.DownstreamPath)
	key := sidecarCookieKey(spec.Account.ID, spec.Egress.ID, target, spec.CookieJarKey)
	return c.cookieJarForKey(key)
}

// codexBaseURL keeps ChatGPT OAuth/access-token traffic on the WHAM backend while
// routing OpenAI Platform API keys through the public /v1 API on every transport.
func (c *Client) codexBaseURL(spec Request) string {
	cfg := c.cfgSnapshot()
	if accountprovider.UsesAPIKey("codex", spec.Token) {
		return NormalizeBaseURL(firstNonEmpty(cfg.OpenAIAPIUpstreamBaseURL, config.DefaultOpenAIAPIUpstreamBaseURL))
	}
	return NormalizeBaseURL(cfg.UpstreamBaseURL)
}

func (c *Client) cookieJarForKey(key string) *cookiejar.Jar {
	return c.jars.getOrCreate(key)
}

func parseCookieHeader(header string) []*http.Cookie {
	var out []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, &http.Cookie{Name: name, Value: strings.TrimSpace(value)})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
