package upstream

import (
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// fixedSecretClient builds a Client with a deterministic identity secret so tests
// can recompute the expected per-account identity values.
func fixedSecretClient(t *testing.T, cfg config.Config) (*Client, []byte) {
	t.Helper()
	cfg.IdentitySecret = "fleet-test-secret"
	c := NewClient(cfg)
	return c, identity.ResolveSecret([]byte(cfg.IdentitySecret))
}

// --- Claude: session-id fallback rotates by conversation when no header is sent ---

func TestClaudeSessionIDFallsBackToConversationAnchor(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-1")

	emit := func(body string) string {
		spec := Request{
			Provider: "claude",
			Account:  storage.Account{ID: "acc-1"},
			Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
			Headers:  http.Header{}, // no X-Claude-Code-Session-Id
			Body:     []byte(body),
		}
		h := http.Header{}
		c.applyClaudeHeaders(h, spec, id, true)
		return h.Get("X-Claude-Code-Session-Id")
	}

	convA1 := `{"model":"claude","messages":[{"role":"user","content":"build a parser"}]}`
	convA2 := `{"model":"claude","messages":[{"role":"user","content":"build a parser"},{"role":"assistant","content":"ok"},{"role":"user","content":"add tests"}]}`
	convB := `{"model":"claude","messages":[{"role":"user","content":"totally different task"}]}`

	// Stable across a conversation's turns (anchor = first user message).
	if emit(convA1) != emit(convA2) {
		t.Fatal("session id changed across turns of the same conversation")
	}
	// Distinct across different conversations (no more one immortal session).
	if emit(convA1) == emit(convB) {
		t.Fatal("different conversations collapsed onto the same session id")
	}
	// Never the static per-account fallback when an anchor exists.
	if emit(convA1) == id.ClaudeSessionID {
		t.Fatal("fallback used the static session id instead of the conversation anchor")
	}
}

// --- Claude: X-Claude-Code-Session-Id rotates from the incoming downstream id ---

func TestClaudeSessionIDRotatesFromIncoming(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-1")

	build := func(incoming string) string {
		spec := Request{
			Provider: "claude",
			Account:  storage.Account{ID: "acc-1"},
			Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
			Headers:  http.Header{},
		}
		if incoming != "" {
			spec.Headers.Set("X-Claude-Code-Session-Id", incoming)
		}
		h := http.Header{}
		c.applyClaudeHeaders(h, spec, id, true)
		return h.Get("X-Claude-Code-Session-Id")
	}

	runA := "11111111-1111-4111-8111-111111111111"
	runB := "22222222-2222-4222-8222-222222222222"

	// Multi-agent coherence: same incoming id ⇒ same emitted id.
	if build(runA) != build(runA) {
		t.Fatal("same incoming session id produced different emitted ids")
	}
	// Rotation: a different run (incoming id) ⇒ a different emitted id.
	if build(runA) == build(runB) {
		t.Fatal("different incoming session ids collapsed to the same emitted id")
	}
	// The emitted value must NEVER be the raw downstream value (no leak).
	if build(runA) == runA {
		t.Fatal("downstream session id leaked upstream unchanged")
	}
	// It must equal the account-bound derivation.
	if got, want := build(runA), identity.DerivedUUID(id.MachineID, runA); got != want {
		t.Fatalf("derived session id = %q, want %q", got, want)
	}
	// No incoming id ⇒ fall back to the account's stable id.
	if got := build(""); got != id.ClaudeSessionID {
		t.Fatalf("fallback session id = %q, want stable %q", got, id.ClaudeSessionID)
	}
}

// --- Claude: X-Stainless-Package-Version is the SDK axis, distinct from the CLI ---

func TestClaudeStainlessVersionIsSDKAxis(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-sdk")

	h := http.Header{}
	c.applyClaudeHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-sdk"},
		Token:    storage.AccountToken{AccessToken: "sk-ant-oat-xyz"},
		Headers:  http.Header{},
	}, id, true)

	if got := h.Get("X-Stainless-Package-Version"); got != id.StainlessPackageVersion {
		t.Fatalf("package version = %q, want SDK axis %q", got, id.StainlessPackageVersion)
	}
	// The UA carries the claude-cli version, which is a separate axis — they must
	// not be the same field value unless the pools coincidentally agree.
	if got := h.Get("User-Agent"); got != id.ClaudeUserAgentVersion(id.ClaudeCLIVersion) {
		t.Fatalf("UA = %q, want claude-cli/%s shape", got, id.ClaudeCLIVersion)
	}
	if got := h.Get("X-Stainless-Runtime-Version"); got != id.NodeVersion {
		t.Fatalf("runtime version = %q, want per-account node %q", got, id.NodeVersion)
	}
}

// --- Claude: the API-key path keeps the SDK header shape, drops oauth betas ---

func TestClaudeAPIKeyPathShape(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-apikey")

	h := http.Header{}
	c.applyClaudeHeaders(h, Request{
		Provider: "claude",
		Account:  storage.Account{ID: "acc-apikey"},
		Token:    storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-xyz"},
		Headers:  http.Header{},
	}, id, false)

	if h.Get("x-api-key") == "" {
		t.Fatal("api-key path did not set x-api-key")
	}
	if h.Get("Authorization") != "" {
		t.Fatal("api-key path must not send a Bearer Authorization")
	}
	if beta := h.Get("Anthropic-Beta"); containsToken(beta, "oauth-2025-04-20") {
		t.Fatalf("api-key path must not carry an oauth beta: %q", beta)
	}
}

// --- Codex: Session_id is seeded from the run correlator and rotates per run ---

func TestCodexSessionIDSeededFromCorrelator(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-cdx")

	build := func(conv string) http.Header {
		spec := Request{
			Account: storage.Account{ID: "acc-cdx"},
			Token:   storage.AccountToken{AccessToken: "oauth-access-token"},
			Headers: http.Header{},
		}
		if conv != "" {
			spec.Headers.Set("conversation_id", conv)
		}
		h := http.Header{}
		c.applyCodexHeaders(h, spec)
		return h
	}
	threadOf := func(conv string) string { return getHeaderFold(build(conv), "thread-id") }

	convA := "conv-aaaa"
	convB := "conv-bbbb"

	// The real Codex client uses LOWERCASE session-id/thread-id and advertises its
	// version plus the default-enabled remote compaction feature on Responses calls.
	hA := build(convA)
	if getHeaderFold(hA, "Session_id") != "" {
		t.Fatal("must not emit capitalized Session_id")
	}
	if getHeaderFold(hA, "version") != config.DefaultClientVersion || getHeaderFold(hA, "x-codex-beta-features") != codexBetaFeaturesHeader {
		t.Fatalf("responses version fingerprint missing: %+v", hA)
	}
	if getHeaderFold(hA, "session-id") == "" || getHeaderFold(hA, "thread-id") == "" {
		t.Fatal("session-id/thread-id missing")
	}
	if getHeaderFold(hA, "session-id") != getHeaderFold(hA, "thread-id") {
		t.Fatal("official Codex session-id and thread-id must be the same UUIDv7")
	}
	if threadOf(convA) != threadOf(convA) {
		t.Fatal("same correlator produced different thread-id")
	}
	if threadOf(convA) == threadOf(convB) {
		t.Fatal("different correlators collapsed to the same thread-id")
	}
	if got, want := threadOf(convA), identity.DerivedUUIDv7(id.MachineID+"\x00thread", convA); got != want {
		t.Fatalf("thread-id = %q, want %q", got, want)
	}
	if got, want := threadOf(""), identity.DerivedUUIDv7(id.MachineID+"\x00thread", id.SessionID); got != want {
		t.Fatalf("fallback thread-id = %q, want seeded-by-stable-id %q", got, want)
	}
	// x-client-request-id mirrors thread-id (the real client sets them equal).
	if getHeaderFold(hA, "x-client-request-id") != getHeaderFold(hA, "thread-id") {
		t.Fatal("x-client-request-id must equal thread-id")
	}
}

func TestCodexMappedDeviceUsesSnapshotOSProfile(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	account := storage.Account{ID: "acc-mapped-device"}
	egress := storage.EgressProfile{ID: "egress-mapped-device", Type: "direct"}
	mappedDevice := identity.CodexDevice(secret, account.ID, egress.ID, "Mac OS")
	snapshot := &CodexIdentitySnapshot{
		InstallationID: mappedDevice.MachineID,
		DeviceOSHint:   "Mac OS",
		SessionID:      "019f0000-0000-7000-8000-000000000051",
		ThreadID:       "019f0000-0000-7000-8000-000000000051",
		TurnID:         "019f0000-0000-7000-8000-000000000052",
	}
	spec := Request{
		DownstreamPath: "/v1/responses",
		Body:           []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"tool output says Platform: linux"}`),
		Account:        account,
		Token:          storage.AccountToken{AccessToken: "oauth-access-token"},
		Egress:         egress,
		// A later tool-result body can infer Linux, but the strict CPA snapshot
		// must keep the root-elected macOS profile coherent with its device.
		OSHint:        "Linux",
		CodexIdentity: snapshot,
	}
	wantUA := mappedDevice.CodexUserAgentVersion(config.DefaultClientVersion)
	httpHeaders := http.Header{}
	if err := c.applyCodexHeaders(httpHeaders, spec); err != nil {
		t.Fatal(err)
	}
	if got := httpHeaders.Get("User-Agent"); got != wantUA {
		t.Fatalf("HTTP mapped device UA = %q, want root snapshot profile %q", got, wantUA)
	}

	spec.CodexResponsesWebSocket = true
	_, wsHeaders, _, err := c.prepareCodexResponsesWebSocket(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := wsHeaders.Get("User-Agent"); got != wantUA {
		t.Fatalf("WebSocket mapped device UA = %q, want root snapshot profile %q", got, wantUA)
	}
}

// --- Codex: the API-key path carries no CLI session fingerprint ---

func TestCodexAPIKeyPathOmitsFingerprint(t *testing.T) {
	c, _ := fixedSecretClient(t, config.Default())
	h := http.Header{}
	c.applyCodexHeaders(h, Request{
		Account: storage.Account{ID: "acc-cdx-api"},
		Token:   storage.AccountToken{OpenAIAPIKey: "sk-openai-xyz"},
		Headers: http.Header{},
	})
	for _, k := range []string{"Originator", "session-id", "thread-id", "ChatGPT-Account-ID"} {
		if getHeaderFold(h, k) != "" {
			t.Fatalf("api-key Codex path leaked CLI fingerprint header %q", k)
		}
	}
}

// --- Codex: Originator + UA mirror the downstream launch entrypoint ---

func TestCodexEntrypointAdaptive(t *testing.T) {
	c, secret := fixedSecretClient(t, config.Default())
	id := identity.For(secret, "acc-cdx")

	build := func(downstream http.Header) (originator, ua string) {
		h := http.Header{}
		c.applyCodexHeaders(h, Request{
			Account: storage.Account{ID: "acc-cdx"},
			Token:   storage.AccountToken{AccessToken: "oauth-access-token"},
			Headers: downstream,
		})
		return getHeaderFold(h, "Originator"), h.Get("User-Agent")
	}

	// Default (downstream sent nothing) → interactive CLI.
	if o, ua := build(http.Header{}); o != identity.CodexOriginator || !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("default entrypoint: originator=%q ua=%q, want codex_cli_rs", o, ua)
	}
	// Downstream Originator: codex_exec → exec UA shape.
	execHdr := http.Header{}
	execHdr.Set("Originator", "codex_exec")
	o, ua := build(execHdr)
	if o != identity.CodexOriginatorExec {
		t.Fatalf("exec originator = %q, want codex_exec", o)
	}
	if !strings.HasPrefix(ua, "codex_exec/") || !strings.Contains(ua, id.Terminal) {
		t.Fatalf("exec UA = %q, want codex_exec source shape with terminal %q", ua, id.Terminal)
	}
	if ua != id.CodexUserAgentExecVersion(identity.CodexCLIVersion) {
		t.Fatalf("exec UA = %q, want %q (account-bound OS/arch)", ua, id.CodexUserAgentExecVersion(identity.CodexCLIVersion))
	}
	// Downstream User-Agent prefix alone (no Originator header) is enough to detect exec.
	uaOnly := http.Header{}
	uaOnly.Set("User-Agent", "codex_exec/0.144.5 (Debian 12.0.0; x86_64) unknown")
	if o, _ := build(uaOnly); o != identity.CodexOriginatorExec {
		t.Fatalf("UA-only exec detection: originator = %q, want codex_exec", o)
	}

	// The process User-Agent and the thread Originator are distinct in app-server
	// clients: a VS Code process can operate a Codex Work thread.
	vscodeWork := http.Header{}
	vscodeWork.Set("User-Agent", "codex_vscode/0.144.5 (Darwin 25; arm64) vscode")
	vscodeWork.Set("Originator", "codex_work_desktop")
	o, ua = build(vscodeWork)
	if o != "codex_work_desktop" || !strings.HasPrefix(ua, "codex_vscode/") {
		t.Fatalf("split process/thread identity: originator=%q ua=%q", o, ua)
	}
	if strings.Contains(ua, "Darwin 25") || !strings.Contains(ua, id.OSName) || !strings.Contains(ua, id.Arch) {
		t.Fatalf("VS Code UA did not use account virtual OS/arch: %q", ua)
	}
}

func TestCodexBetaAndSemanticHeadersAreValidatedAndMerged(t *testing.T) {
	c, _ := fixedSecretClient(t, config.Default())
	downstream := http.Header{}
	downstream.Set("x-codex-beta-features", "memories,prevent_idle_sleep,network_proxy,remote_compaction_v2,unknown_future,network_proxy")
	downstream.Set("x-openai-memgen-request", "true")
	downstream.Set("x-openai-internal-codex-residency", "us")
	downstream.Set("x-responsesapi-include-timing-metrics", "true")
	downstream.Set("x-oai-attestation", "untrusted")
	build := func(websocket bool) http.Header {
		headers := http.Header{}
		c.applyCodexHeaders(headers, Request{
			Account: storage.Account{ID: "acc-semantics"},
			Token:   storage.AccountToken{AccessToken: "oauth-access-token"},
			Headers: downstream, CodexResponsesWebSocket: websocket,
		})
		return headers
	}
	httpHeaders := build(false)
	if got := getHeaderFold(httpHeaders, "x-codex-beta-features"); got != "memories,prevent_idle_sleep,network_proxy,remote_compaction_v2" {
		t.Fatalf("merged beta features = %q", got)
	}
	if getHeaderFold(httpHeaders, "x-openai-memgen-request") != "true" || getHeaderFold(httpHeaders, "x-openai-internal-codex-residency") != "us" {
		t.Fatalf("valid semantic headers missing: %v", httpHeaders)
	}
	if getHeaderFold(httpHeaders, "x-responsesapi-include-timing-metrics") != "" {
		t.Fatalf("WS timing metric header leaked onto HTTP: %v", httpHeaders)
	}
	if getHeaderFold(httpHeaders, "x-oai-attestation") != "" {
		t.Fatalf("untrusted attestation was forwarded: %v", httpHeaders)
	}
	wsHeaders := build(true)
	if getHeaderFold(wsHeaders, "x-responsesapi-include-timing-metrics") != "true" {
		t.Fatalf("valid WS timing metric header missing: %v", wsHeaders)
	}

	invalid := downstream.Clone()
	invalid.Set("x-openai-memgen-request", "false")
	invalid.Set("x-openai-internal-codex-residency", "eu")
	invalid.Set("x-responsesapi-include-timing-metrics", "1")
	headers := http.Header{}
	c.applyCodexHeaders(headers, Request{
		Account: storage.Account{ID: "acc-invalid-semantics"},
		Token:   storage.AccountToken{AccessToken: "oauth-access-token"},
		Headers: invalid, CodexResponsesWebSocket: true,
	})
	for _, name := range []string{"x-openai-memgen-request", "x-openai-internal-codex-residency", "x-responsesapi-include-timing-metrics"} {
		if getHeaderFold(headers, name) != "" {
			t.Fatalf("invalid semantic header %s survived: %v", name, headers)
		}
	}
}

// --- Codex: the Responses version header and UA must use the same version ---

func TestCodexVersionHeaderMatchesUA(t *testing.T) {
	cfg := config.Default()
	cfg.CodexCLIVersionOverride = "0.130.0"
	c, _ := fixedSecretClient(t, cfg)

	// An untrusted downstream version must not override the account-bound version.
	downstream := http.Header{}
	downstream.Set("version", "0.135.0")
	h := http.Header{}
	c.applyCodexHeaders(h, Request{
		Account: storage.Account{ID: "acc-cdx"},
		Token:   storage.AccountToken{AccessToken: "oauth-access-token"},
		Headers: downstream,
	})
	if got := getHeaderFold(h, "version"); got != "0.130.0" {
		t.Fatalf("version header = %q, want account-bound 0.130.0", got)
	}
	if ua := h.Get("User-Agent"); !strings.Contains(ua, "/0.130.0 ") {
		t.Fatalf("UA = %q, want account-bound version 0.130.0", ua)
	}
}

// TestCodexClientVersionOverrideCoherent locks in the model-probe fix: a per-request
// CodexClientVersion override drives the UA (and the `?client_version=` query the probe
// sends), so the newest models are not version-gated away. The override must win over
// the config default and drive both version-bearing fingerprint fields.
func TestCodexClientVersionOverrideCoherent(t *testing.T) {
	cfg := config.Default()
	cfg.CodexCLIVersionOverride = "0.118.0" // deliberately stale to prove the per-request override wins
	c, _ := fixedSecretClient(t, cfg)

	downstream := http.Header{}
	downstream.Set("version", "0.118.0")
	h := http.Header{}
	c.applyCodexHeaders(h, Request{
		Account:            storage.Account{ID: "acc-probe"},
		Token:              storage.AccountToken{AccessToken: "oauth-access-token"},
		Headers:            downstream,
		CodexClientVersion: "0.135.0",
	})
	if got := getHeaderFold(h, "version"); got != "0.135.0" {
		t.Fatalf("version header = %q, want probe override 0.135.0", got)
	}
	if ua := h.Get("User-Agent"); !strings.Contains(ua, "/0.135.0 ") {
		t.Fatalf("UA = %q, want probe override version 0.135.0", ua)
	}
}

// TestCodexClientVersionOverrideIgnoredForAPIKey confirms the override never makes an
// API-key (non-OAuth) account present the Codex CLI fingerprint — API keys take the
// early return before any UA/version synthesis.
func TestCodexClientVersionOverrideIgnoredForAPIKey(t *testing.T) {
	c, _ := fixedSecretClient(t, config.Default())
	h := http.Header{}
	c.applyCodexHeaders(h, Request{
		Account:            storage.Account{ID: "acc-apikey"},
		Token:              storage.AccountToken{OpenAIAPIKey: "sk-openai-xyz"},
		CodexClientVersion: "0.135.0",
	})
	if ua := h.Get("User-Agent"); ua != "" {
		t.Fatalf("API-key account must not carry a synthesized Codex UA, got %q", ua)
	}
	if v := getHeaderFold(h, "version"); v != "" {
		t.Fatalf("API-key account must not carry a synthesized version header, got %q", v)
	}
}

// --- Codex: the sidecar egress defaults to Chrome impersonation, and so do the
// "real Codex" aliases — the real client does no JA3 spoofing (vanilla rustls, verified
// in other_codex) and its JA3 carries the unlistable 0xFF SCSV. See resolveCodexJA3. ---

func TestCodexSidecarDefaultsToChromeJA3(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	client := NewClient(sidecarEngineConfig())
	resp, err := client.Do(nilContext(t), Request{
		DownstreamPath: "/v1/responses",
		Body:           []byte(`{"stream":true}`),
		Account:        storage.Account{ID: "acc-cdx"},
		Token:          storage.AccountToken{AccessToken: "oauth-access-token"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !cap.hit {
		t.Fatal("sidecar was not contacted")
	}
	// DEFAULT must be Chrome (no ja3): the real Codex JA3 carries cipher 0xFF/SCSV +
	// extensions curl_cffi can't replay, so forcing it 502'd every Codex request.
	if cap.ja3 != "" {
		t.Fatalf("sidecar ja3 = %q, want empty (Chrome impersonation default)", cap.ja3)
	}
	if !strings.HasPrefix(cap.headers.Get("User-Agent"), "codex_cli_rs/") {
		t.Fatalf("UA = %q, want codex_cli_rs/*", cap.headers.Get("User-Agent"))
	}
	// The official Codex client POSTs JSON with an explicit application/json content type
	// (other_codex codex-client/src/request.rs); without it the upstream rejects the
	// request with 400 {"detail":"Unsupported content type"} — and the sidecar path,
	// unlike doHTTP, has no post-hoc fallback, so this MUST come from applyCodexHeaders.
	if ct := cap.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

func TestCodexSidecarAliasResolvesToChrome(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.CodexJA3Override = "codex-cli" // "real Codex" alias — now resolves to Chrome
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		DownstreamPath: "/v1/responses",
		Body:           []byte(`{"stream":true}`),
		Account:        storage.Account{ID: "acc-cdx"},
		Token:          storage.AccountToken{AccessToken: "oauth-access-token"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The alias must NOT force a JA3 (Chrome impersonation, no doomed sidecar attempt,
	// no 502): Codex does no spoofing and its JA3 can't be cleanly replayed.
	if cap.ja3 != "" {
		t.Fatalf("codex_ja3=codex-cli must resolve to Chrome (empty ja3), got %q", cap.ja3)
	}
}

func TestCodexSidecarRawJA3IsSanitized(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	// An operator who pastes the real Codex JA3 verbatim gets a best-effort replay with
	// the unlistable 0xFF SCSV signalling cipher stripped, so curl_cffi doesn't 502 on it.
	cfg.CodexJA3Override = identity.CodexJA3
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		DownstreamPath: "/v1/responses",
		Body:           []byte(`{"stream":true}`),
		Account:        storage.Account{ID: "acc-cdx"},
		Token:          storage.AccountToken{AccessToken: "oauth-access-token"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	want := sanitizeJA3(identity.CodexJA3)
	if cap.ja3 != want {
		t.Fatalf("raw codex_ja3 reached sidecar = %q, want SCSV-sanitized %q", cap.ja3, want)
	}
	if strings.Contains(cap.ja3, "-255,") || strings.HasSuffix(cap.ja3, "-255") {
		t.Fatalf("sanitized ja3 still contains the 0xFF SCSV cipher 255: %q", cap.ja3)
	}
}

func TestCodexSidecarAPIKeyOmitsJA3(t *testing.T) {
	var cap sidecarCapture
	sidecar := newFakeSidecar(t, &cap)
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.OpenAIAPIUpstreamBaseURL = "https://api.openai.example/v1"
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		DownstreamPath: "/v1/responses",
		Body:           []byte(`{"stream":true}`),
		Account:        storage.Account{ID: "acc-cdx-api"},
		Token:          storage.AccountToken{AuthMethod: "api_key", OpenAIAPIKey: "sk-openai-xyz"},
		Egress:         storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// An API-key account is a plain SDK client, not the Codex binary — it must not
	// masquerade with Codex's TLS fingerprint.
	if cap.ja3 != "" {
		t.Fatalf("api-key path replayed Codex JA3 %q, want none", cap.ja3)
	}
	if cap.url != "https://api.openai.example/v1/responses" {
		t.Fatalf("api-key sidecar target = %q, want OpenAI Platform URL", cap.url)
	}
	if cap.headers.Get("Authorization") != "Bearer sk-openai-xyz" {
		t.Fatalf("api-key Authorization = %q", cap.headers.Get("Authorization"))
	}
	if cap.defaultHeaders == nil || *cap.defaultHeaders {
		t.Fatalf("api-key sidecar must suppress browser default headers, got %v", cap.defaultHeaders)
	}
	// ...but it still POSTs JSON, so it needs the content type too (the 400 fix applies
	// to API-key and OAuth accounts alike).
	if ct := cap.headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("api-key Content-Type = %q, want application/json", ct)
	}
}

func containsToken(csv, tok string) bool {
	for _, p := range splitCSV(csv) {
		if p == tok {
			return true
		}
	}
	return false
}
