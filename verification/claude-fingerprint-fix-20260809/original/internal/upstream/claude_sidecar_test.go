package upstream

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// fakeSidecar stands in for the curl_cffi sidecar /proxy protocol: it records the
// forwarded envelope (target URL + headers + body) and replays a canned upstream
// response using the x-sidecar-upstream-status / -headers-b64 contract.
type sidecarCapture struct {
	url     string
	method  string
	headers http.Header
	body    string
	ja3     string
	proxy   string
	// defaultHeaders mirrors the meta's "default_headers" field: nil = absent (sidecar
	// keeps curl's browser defaults), non-nil = the caller pinned it. The Claude path
	// pins it false to stop curl-impersonate injecting sec-ch-ua/sec-fetch browser headers
	// on top of the claude-cli/Node identity.
	defaultHeaders *bool
	hit            bool
}

func newFakeSidecar(t *testing.T, cap *sidecarCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy" {
			http.NotFound(w, r)
			return
		}
		cap.hit = true
		var payload struct {
			Method         string              `json:"method"`
			URL            string              `json:"url"`
			Headers        map[string][]string `json:"headers"`
			BodyB64        string              `json:"body_b64"`
			JA3            string              `json:"ja3"`
			Proxy          string              `json:"proxy"`
			DefaultHeaders *bool               `json:"default_headers"`
		}
		raw, _ := io.ReadAll(r.Body)
		if meta := r.Header.Get("X-Sidecar-Meta"); meta != "" {
			// New protocol: metadata in the X-Sidecar-Meta header, raw body is the HTTP body.
			metaRaw, _ := base64.StdEncoding.DecodeString(meta)
			if err := json.Unmarshal(metaRaw, &payload); err != nil {
				t.Errorf("sidecar meta unmarshal: %v", err)
			}
			cap.body = string(raw)
		} else {
			// Legacy protocol: a single JSON object carrying a base64'd body.
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Errorf("sidecar payload unmarshal: %v", err)
			}
			if b, err := base64.StdEncoding.DecodeString(payload.BodyB64); err == nil {
				cap.body = string(b)
			}
		}
		cap.url = payload.URL
		cap.method = payload.Method
		cap.headers = http.Header(payload.Headers)
		cap.ja3 = payload.JA3
		cap.proxy = payload.Proxy
		cap.defaultHeaders = payload.DefaultHeaders
		upstreamHdr := http.Header{"Content-Type": []string{"text/event-stream"}}
		enc, _ := json.Marshal(upstreamHdr)
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(enc))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
}

// sidecarEngineConfig returns a Default() config pinned to the curl_cffi sidecar
// egress engine. The production default is now the in-process TLS fingerprinter
// (EgressFingerprintEngine="inprocess"); on that default a client.Do call bound to
// a curl_cffi_sidecar egress bypasses the fake sidecar and reaches the real
// upstream instead. Every test that asserts on the forwarded sidecar envelope
// (target URL / headers / body / ja3 / default_headers captured via newFakeSidecar)
// must opt back into the sidecar engine so the fake sidecar is actually contacted
// and the assertions are genuine rather than vacuous. The in-process path has its
// own fidelity validation via the reflector (/admin/egress-fingerprint-check), not
// these sidecar wire-protocol tests.
func sidecarEngineConfig() config.Config {
	cfg := config.Default()
	cfg.EgressFingerprintEngine = "sidecar"
	return cfg
}

// TestClaudeRoutesThroughSidecarWithClaudeHeaders is the core anti-fingerprint
// fix: a Claude account bound to a curl_cffi_sidecar egress must EGRESS THROUGH
// the sidecar (for a real client TLS/JA3 fingerprint), and the forwarded request
// must carry the Claude target URL + Claude Code headers, not Codex ones.
func TestClaudeRoutesThroughSidecarWithClaudeHeaders(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.ClaudeUpstreamBaseURL = "https://api.anthropic.com"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           testBody([]byte(`{"model":"claude-3-5-sonnet","stream":true}`)),
		Account:        storage.Account{ID: "acc-claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !cap.hit {
		t.Fatal("sidecar was not contacted: Claude bypassed the impersonating egress")
	}
	if !strings.HasPrefix(cap.url, "https://api.anthropic.com/v1/messages") {
		t.Fatalf("sidecar target = %q, want anthropic /v1/messages", cap.url)
	}
	if !strings.Contains(cap.url, "beta=true") {
		t.Fatalf("sidecar target missing beta=true: %q", cap.url)
	}
	// Claude Code fingerprint headers, not Codex ones.
	if got := cap.headers.Get("Authorization"); got != "Bearer sk-ant-oat-xyz" {
		t.Fatalf("Authorization = %q", got)
	}
	if cap.headers.Get("Anthropic-Version") == "" || cap.headers.Get("X-Stainless-Runtime") != "node" {
		t.Fatalf("missing Claude headers: %v", cap.headers)
	}
	if !strings.HasPrefix(cap.headers.Get("User-Agent"), "claude-cli/") {
		t.Fatalf("User-Agent = %q, want claude-cli/*", cap.headers.Get("User-Agent"))
	}
	if cap.headers.Get("Originator") != "" {
		t.Fatalf("Codex Originator header leaked onto Claude request: %q", cap.headers.Get("Originator"))
	}
	// A real client never asks for identity encoding; the sidecar negotiates it.
	if cap.headers.Get("Accept-Encoding") != "" {
		t.Fatalf("Accept-Encoding should be unset on the sidecar path, got %q", cap.headers.Get("Accept-Encoding"))
	}
}

// TestClaudeForceDirectBypassesSidecar verifies the escape hatch: with
// claude_force_direct the sidecar is NOT used even when the account is bound to
// one — the request goes direct to the (overridable) Claude base URL instead.
func TestClaudeForceDirectBypassesSidecar(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	var directHit bool
	var directUA string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHit = true
		directUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.ClaudeForceDirect = true
	cfg.ClaudeUpstreamBaseURL = upstream.URL
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           testBody([]byte(`{"model":"claude-3-5-sonnet"}`)),
		Account:        storage.Account{ID: "acc-claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if cap.hit {
		t.Fatal("claude_force_direct must not route through the sidecar")
	}
	if !directHit {
		t.Fatal("force-direct request never reached the direct upstream")
	}
	if !strings.HasPrefix(directUA, "claude-cli/") {
		t.Fatalf("direct path lost Claude UA: %q", directUA)
	}
}

func TestClaudeForceDirectKeepsWrappedBaseProxy(t *testing.T) {
	proxyTargets := make(chan string, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyTargets <- r.URL.String()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer proxyServer.Close()

	var cap sidecarCapture
	sidecarServer := newFakeSidecar(t, &cap)
	defer sidecarServer.Close()
	wrapped, err := storage.WrapEgressWithSidecar(
		storage.EgressProfile{ID: "residential-exit", Type: "http_proxy", Endpoint: proxyServer.URL, Health: "healthy"},
		storage.EgressProfile{ID: "local-sidecar", Type: storage.CurlCFFISidecarEgressType, Endpoint: sidecarServer.URL, Health: "healthy"},
	)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ClaudeForceDirect = true
	cfg.ClaudeUpstreamBaseURL = "http://anthropic.invalid"
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		Method: http.MethodPost, Provider: "claude", DownstreamPath: "/v1/messages",
		Body: testBody([]byte(`{"model":"claude-sonnet-4-6"}`)), Account: storage.Account{ID: "acc-claude-proxy"},
		Token: storage.AccountToken{AccessToken: "sk-ant-oat-test"}, Egress: wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cap.hit {
		t.Fatal("claude_force_direct did not bypass the account sidecar wrapper")
	}
	if target := <-proxyTargets; !strings.HasPrefix(target, "http://anthropic.invalid/v1/messages") {
		t.Fatalf("force-direct target through base proxy = %q", target)
	}
}

func TestClaudeAccountSidecarWrapperChainsThroughSelectedProxy(t *testing.T) {
	var cap sidecarCapture
	sidecarServer := newFakeSidecar(t, &cap)
	defer sidecarServer.Close()

	base := storage.EgressProfile{ID: "residential-exit", Type: "http_proxy", Endpoint: "http://user:pass@proxy.example:8080", Health: "healthy"}
	wrapped, err := storage.WrapEgressWithSidecar(base, storage.EgressProfile{
		ID: "local-sidecar", Type: storage.CurlCFFISidecarEgressType, Endpoint: sidecarServer.URL, Health: "healthy",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(sidecarEngineConfig())
	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           testBody([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)),
		Account:        storage.Account{ID: "acc-claude-wrapped"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Egress:         wrapped,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !cap.hit {
		t.Fatal("account-bound sidecar was not used")
	}
	if cap.proxy != base.Endpoint {
		t.Fatalf("sidecar proxy = %q, want selected exit %q", cap.proxy, base.Endpoint)
	}
	if wrapped.ID != base.ID {
		t.Fatalf("wrapped egress id = %q, want real exit %q", wrapped.ID, base.ID)
	}
}

// TestClaudeCLIVersionOverrideAppliesToHeaders ensures the config version
// override flows into the upstream fingerprint headers (no stale pinned version).
func TestClaudeCLIVersionOverrideAppliesToHeaders(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.ClaudeCLIVersionOverride = "9.9.9"
	cfg.ClaudeNodeVersion = "v30.0.0"
	cfg.ClaudeStainlessVersion = "0.99.0"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           testBody([]byte(`{"stream":true}`)),
		Account:        storage.Account{ID: "acc-claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// X-Stainless-Package-Version is the @anthropic-ai/sdk version — a SEPARATE
	// axis from the claude-cli version, driven by its own override.
	if got := cap.headers.Get("X-Stainless-Package-Version"); got != "0.99.0" {
		t.Fatalf("package version = %q, want override 0.99.0", got)
	}
	if got := cap.headers.Get("X-Stainless-Runtime-Version"); got != "v30.0.0" {
		t.Fatalf("node version = %q, want override v30.0.0", got)
	}
	// The claude-cli version override flows into the User-Agent.
	if got := cap.headers.Get("User-Agent"); got != "claude-cli/9.9.9 (external, cli)" {
		t.Fatalf("UA = %q, want overridden version", got)
	}
}

// TestClaudeSidecarDefaultsToChromeJA3 verifies the evidence-based default: with no
// claude_ja3 configured, the Claude sidecar path forwards NO ja3, so the sidecar uses
// its native Chrome impersonation (matching reference relays; Anthropic's third-party
// detection is system-prompt-content based, not TLS, so this is the lower-risk default).
func TestClaudeSidecarDefaultsToChromeJA3(t *testing.T) {
	for _, tok := range []storage.AccountToken{
		{AccessToken: "sk-ant-oat-xyz"},      // OAuth
		{OpenAIAPIKey: "sk-ant-api03-abcde"}, // API key
	} {
		var cap sidecarCapture
		sidecar := newFakeSidecar(t, &cap)

		cfg := sidecarEngineConfig()
		client := NewClient(cfg)
		resp, err := client.Do(nilContext(t), Request{
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Body:           testBody([]byte(`{"stream":true}`)),
			Account:        storage.Account{ID: "acc-claude"},
			Token:          tok,
			Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
		})
		if err != nil {
			sidecar.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		sidecar.Close()

		if cap.ja3 != "" {
			t.Fatalf("default must forward no ja3 (Chrome impersonation), got %q", cap.ja3)
		}
	}
}

// TestClaudeJA3OptInReplaysRealClaudeJA3 verifies the opt-in path: setting
// claude_ja3=claude-cli makes the sidecar replay the captured real claude-cli (Node)
// JA3 for operators who want explicit TLS↔UA coherence.
func TestClaudeJA3OptInReplaysRealClaudeJA3(t *testing.T) {
	for _, override := range []string{"claude-cli", "real", "native", identity.ClaudeJA3} {
		var cap sidecarCapture
		sidecar := newFakeSidecar(t, &cap)

		cfg := sidecarEngineConfig()
		cfg.ClaudeJA3Override = override
		client := NewClient(cfg)
		resp, err := client.Do(nilContext(t), Request{
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Body:           testBody([]byte(`{"stream":true}`)),
			Account:        storage.Account{ID: "acc-claude"},
			Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
			Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
		})
		if err != nil {
			sidecar.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		sidecar.Close()

		if cap.ja3 != identity.ClaudeJA3 {
			t.Fatalf("claude_ja3=%q must replay real claude-cli JA3 %q, got %q", override, identity.ClaudeJA3, cap.ja3)
		}
	}
}

// TestClaudeJA3DisableKeepsChrome verifies the explicit off sentinel forwards no ja3.
func TestClaudeJA3DisableKeepsChrome(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.ClaudeJA3Override = "off"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           testBody([]byte(`{"stream":true}`)),
		Account:        storage.Account{ID: "acc-claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if cap.ja3 != "" {
		t.Fatalf("claude_ja3=off must forward no ja3 (Chrome), got %q", cap.ja3)
	}
}

// TestClaudeSidecarSuppressesBrowserDefaultHeaders is the relay-fingerprint fix: on the
// DEFAULT (Chrome-impersonation, no explicit JA3) sidecar path, the Claude request must
// pin the meta "default_headers": false so the sidecar tells curl-impersonate NOT to
// inject the browser's own header set (sec-ch-ua*, sec-fetch-*, accept-language,
// upgrade-insecure-requests). Those browser-only headers riding next to a claude-cli
// User-Agent + x-stainless-* headers are a combination no genuine Claude Code (Node)
// client emits — i.e. a clear "impersonation relay" tell to the upstream. The Chrome
// TLS/HTTP2 impersonation is unaffected (still no ja3 forwarded).
func TestClaudeSidecarSuppressesBrowserDefaultHeaders(t *testing.T) {
	for _, tok := range []storage.AccountToken{
		{AccessToken: "sk-ant-oat-xyz"},    // OAuth (Pro/Max)
		{OpenAIAPIKey: "sk-ant-api03-abc"}, // API key (SDK)
	} {
		var cap sidecarCapture
		sidecar := newFakeSidecar(t, &cap)

		client := NewClient(sidecarEngineConfig())
		resp, err := client.Do(nilContext(t), Request{
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Body:           testBody([]byte(`{"stream":true}`)),
			Account:        storage.Account{ID: "acc-claude"},
			Token:          tok,
			Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
		})
		if err != nil {
			sidecar.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		sidecar.Close()

		if cap.defaultHeaders == nil {
			t.Fatalf("Claude sidecar meta omitted default_headers: browser headers (sec-ch-ua/sec-fetch/*) would be injected onto the claude-cli identity")
		}
		if *cap.defaultHeaders {
			t.Fatalf("Claude sidecar must pin default_headers=false, got true")
		}
		// The Chrome TLS/HTTP2 impersonation stays: suppression is headers-only.
		if cap.ja3 != "" {
			t.Fatalf("suppression must not change the TLS default; want no ja3, got %q", cap.ja3)
		}
	}
}

// TestCodexOAuthSidecarKeepsBrowserDefaultHeaders guards the deliberate asymmetry: a
// ChatGPT-OAuth Codex account behind Cloudflare KEEPS curl's browser defaults (no
// default_headers pin in the meta), because chatgpt.com's CF edge is validated against
// the Chrome-shaped request. Only Claude (no CF wall) and API-key SDK traffic suppress.
func TestCodexOAuthSidecarKeepsBrowserDefaultHeaders(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(sidecarEngineConfig())
	resp, err := client.Do(nilContext(t), Request{
		Provider:       "codex",
		DownstreamPath: "/responses",
		Body:           testBody([]byte(`{"stream":true}`)),
		Account:        storage.Account{ID: "acc-codex"},
		Token:          storage.AccountToken{AccessToken: "eyJhb.codex.oauth"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if cap.defaultHeaders != nil {
		t.Fatalf("Codex OAuth (CF-walled) must keep curl browser defaults; meta pinned default_headers=%v", *cap.defaultHeaders)
	}
}
