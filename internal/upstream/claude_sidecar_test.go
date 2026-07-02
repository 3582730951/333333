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
	hit     bool
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
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			BodyB64 string              `json:"body_b64"`
			JA3     string              `json:"ja3"`
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
		upstreamHdr := http.Header{"Content-Type": []string{"text/event-stream"}}
		enc, _ := json.Marshal(upstreamHdr)
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(enc))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
}

// TestClaudeRoutesThroughSidecarWithClaudeHeaders is the core anti-fingerprint
// fix: a Claude account bound to a curl_cffi_sidecar egress must EGRESS THROUGH
// the sidecar (for a real client TLS/JA3 fingerprint), and the forwarded request
// must carry the Claude target URL + Claude Code headers, not Codex ones.
func TestClaudeRoutesThroughSidecarWithClaudeHeaders(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := config.Default()
	cfg.ClaudeUpstreamBaseURL = "https://api.anthropic.com"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           []byte(`{"model":"claude-3-5-sonnet","stream":true}`),
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
		Body:           []byte(`{"model":"claude-3-5-sonnet"}`),
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

// TestClaudeCLIVersionOverrideAppliesToHeaders ensures the config version
// override flows into the upstream fingerprint headers (no stale pinned version).
func TestClaudeCLIVersionOverrideAppliesToHeaders(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := config.Default()
	cfg.ClaudeCLIVersionOverride = "9.9.9"
	cfg.ClaudeNodeVersion = "v30.0.0"
	cfg.ClaudeStainlessVersion = "0.99.0"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           []byte(`{"stream":true}`),
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

		cfg := config.Default()
		client := NewClient(cfg)
		resp, err := client.Do(nilContext(t), Request{
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Body:           []byte(`{"stream":true}`),
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

		cfg := config.Default()
		cfg.ClaudeJA3Override = override
		client := NewClient(cfg)
		resp, err := client.Do(nilContext(t), Request{
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Body:           []byte(`{"stream":true}`),
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

	cfg := config.Default()
	cfg.ClaudeJA3Override = "off"
	client := NewClient(cfg)

	resp, err := client.Do(nilContext(t), Request{
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Body:           []byte(`{"stream":true}`),
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
