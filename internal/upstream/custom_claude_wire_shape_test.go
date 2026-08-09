package upstream

import (
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
)

// A custom provider whose upstream_protocol is anthropic_messages gets the SAME claude-cli
// identity as native Claude traffic (applyClaudeCodeCustomHeaders stamps x-app: cli, the
// whole x-stainless-* block, and a claude-cli User-Agent). Both wire-shape fixes that apply
// to doClaude therefore apply here, and an operator may legitimately point a custom
// provider at api.anthropic.com itself, so this path can reach the same edge.

// TestCustomClaudeSidecarCarriesCapturedHeaderOrder: the sidecar receives its header set as
// JSON, and Go's encoding/json sorts map keys. curl_cffi preserves the order it is given all
// the way to CurlOpt.HTTPHEADER, so with no explicit order this path put alphabetical
// headers on the wire beneath a claude-cli User-Agent.
func TestCustomClaudeSidecarCarriesCapturedHeaderOrder(t *testing.T) {
	capture := captureCustomClaudeSidecarMeta(t)

	if _, ok := capture.Fields["header_order"]; !ok {
		t.Fatalf("custom Claude sidecar meta carries no header_order; the sidecar will emit encoding/json's alphabetical map order under a claude-cli User-Agent")
	}
	names := capture.headerNamesInWireOrder(t)
	if len(names) < 4 {
		t.Fatalf("sidecar meta carried only %d headers (%v); the capture is not exercising the Claude header set", len(names), names)
	}
	wantPrefix := []string{"accept", "authorization", "content-type", "user-agent"}
	for i, want := range wantPrefix {
		if i >= len(names) || names[i] != want {
			t.Fatalf("custom Claude sidecar wire header order = %v\nwant it to start with captured native order %v", names, wantPrefix)
		}
	}
}

// TestCustomClaudeSidecarPinsHTTP11 pins the ALPN for the same path.
func TestCustomClaudeSidecarPinsHTTP11(t *testing.T) {
	capture := captureCustomClaudeSidecarMeta(t)

	raw, ok := capture.Fields["http_version"]
	if !ok {
		t.Fatalf("custom Claude sidecar meta carries no http_version; a claude-cli User-Agent would ride whatever curl_cffi negotiates, including h2")
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("http_version is not a string: %v (%s)", err, raw)
	}
	if got != sidecarHTTPVersion1 {
		t.Errorf("http_version = %q, want %q", got, sidecarHTTPVersion1)
	}
}

// TestCustomClaudeSidecarCarriesCapturedBunFingerprint proves the custom-provider
// dispatcher does not stop at Claude-looking headers while leaving Chrome beneath them.
// A raw native JA3 also disables curl's browser default-header injection and carries the
// signature-algorithm payload that JA3 itself cannot describe.
func TestCustomClaudeSidecarCarriesCapturedBunFingerprint(t *testing.T) {
	capture := captureCustomClaudeSidecarMeta(t)

	var ja3 string
	if err := json.Unmarshal(capture.Fields["ja3"], &ja3); err != nil {
		t.Fatalf("ja3 is absent or invalid: %v", err)
	}
	if ja3 != identity.ClaudeJA3 {
		t.Fatalf("custom Claude JA3 = %q, want captured Bun JA3 %q", ja3, identity.ClaudeJA3)
	}
	var defaultHeaders bool
	if err := json.Unmarshal(capture.Fields["default_headers"], &defaultHeaders); err != nil {
		t.Fatalf("default_headers is absent or invalid: %v", err)
	}
	if defaultHeaders {
		t.Fatal("custom Claude sidecar enables Chrome default headers beneath a claude-cli identity")
	}
	var acceptEncoding string
	if err := json.Unmarshal(capture.Fields["accept_encoding"], &acceptEncoding); err != nil {
		t.Fatalf("accept_encoding is absent or invalid: %v", err)
	}
	if acceptEncoding != claudeAcceptEncoding {
		t.Errorf("accept_encoding = %q, want %q", acceptEncoding, claudeAcceptEncoding)
	}
	var algorithms []string
	if err := json.Unmarshal(capture.Fields["tls_signature_algorithms"], &algorithms); err != nil {
		t.Fatalf("tls_signature_algorithms is absent or invalid: %v", err)
	}
	if !reflect.DeepEqual(algorithms, claudeTLSSignatureAlgorithms) {
		t.Errorf("signature algorithms = %v, want %v", algorithms, claudeTLSSignatureAlgorithms)
	}
}

// TestCustomClaudeDirectUsesFingerprintEngine covers direct/proxy egress for the same
// path. Merely pinning the stdlib transport to h1 still exposed Go's ClientHello; the
// request must avoid the stdlib transport cache entirely and use the native profile.
func TestCustomClaudeDirectUsesFingerprintEngine(t *testing.T) {
	client, seen := runCustomClaudeDirectRequest(t)

	if ua := seen.Get("User-Agent"); len(ua) < len("claude-cli/") || ua[:len("claude-cli/")] != "claude-cli/" {
		t.Fatalf("custom Claude direct User-Agent = %q, want a claude-cli/... identity; without it the HTTP/1.1 pin would not be required", ua)
	}

	client.tmu.Lock()
	keys := make([]string, 0, len(client.transports))
	for key := range client.transports {
		keys = append(keys, key)
	}
	client.tmu.Unlock()

	if len(keys) != 0 {
		t.Fatalf("custom Claude direct call built stdlib transports %v; want the in-process captured Bun profile", keys)
	}
	if client.tlsFactory == nil {
		t.Fatal("custom Claude direct call has no in-process fingerprint factory")
	}
}

// TestNonClaudeCustomProviderKeepsItsOwnTransport is the scoping guard: the two fixes above
// must not move any other custom provider's wire shape. A chat_completions provider sends no
// claude-cli identity, so it must keep the default (h2-capable) transport.
func TestNonClaudeCustomProviderKeepsItsOwnTransport(t *testing.T) {
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer upstreamSrv.Close()

	client := NewClient(config.Default())
	resp, err := client.Do(t.Context(), Request{
		Method:           http.MethodPost,
		Provider:         "deepseek",
		BaseURL:          upstreamSrv.URL,
		UpstreamProtocol: storage.CustomProviderProtocolChatCompletions,
		DownstreamPath:   "/chat/completions",
		Headers:          http.Header{},
		Account:          storage.Account{ID: "acct-2", Provider: "deepseek"},
		Token:            storage.AccountToken{OpenAIAPIKey: "sk-custom"},
		Egress:           storage.EgressProfile{ID: "eg", Type: "direct", Health: "healthy"},
		Body:             testBody([]byte(`{"model":"deepseek-chat","messages":[]}`)),
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	client.tmu.Lock()
	_, pinned := client.transports["http1|direct"]
	_, normal := client.transports["direct"]
	client.tmu.Unlock()

	if pinned {
		t.Errorf("a chat_completions custom provider was pinned to http/1.1; the Claude fix must be scoped to Claude-shaped calls")
	}
	if !normal {
		t.Errorf("chat_completions custom provider did not build the default transport")
	}
}

// TestCustomProviderSidecarCallsPassAHeaderOrder is the structural guard for this file, in
// the same spirit as TestAnthropicSidecarCallsAlwaysPassAHeaderOrder for anthropic.go: the
// order-less postViaSidecar must not reappear in openai_compat.go, because the
// Claude-Code-shaped branch of that dispatcher sends the claude-cli header set and would
// silently go back to encoding/json's alphabetical map order. The assertion is only that
// the order-less entry point is unused; which order expression is passed is covered
// behaviorally by TestCustomClaudeSidecarCarriesCapturedHeaderOrder above.
func TestCustomProviderSidecarCallsPassAHeaderOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "openai_compat.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse openai_compat.go: %v", err)
	}
	ordered := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "postViaSidecar":
			t.Errorf("%s: openai_compat.go calls postViaSidecar (no header order); the Claude-Code-shaped branch sends a claude-cli header set, so use postViaSidecarOrdered", fset.Position(call.Pos()))
		case "postViaSidecarOrdered":
			ordered++
		}
		return true
	})
	if ordered == 0 {
		t.Fatal("found no postViaSidecarOrdered call in openai_compat.go; the test is vacuous (was the custom sidecar path renamed?)")
	}
}

// customClaudeRequest is one Claude-Code-shaped custom-provider call: upstream_protocol is
// anthropic_messages, which is what makes claudeShapedCustomCall true and therefore stamps
// the full claude-cli header set.
func customClaudeRequest(baseURL string, egress storage.EgressProfile) Request {
	return Request{
		Method:           http.MethodPost,
		Provider:         "my-anthropic-gateway",
		BaseURL:          baseURL,
		UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages,
		DownstreamPath:   "/v1/messages",
		Headers:          http.Header{},
		Account:          storage.Account{ID: "acct-custom", Provider: "my-anthropic-gateway"},
		Token:            storage.AccountToken{OpenAIAPIKey: "sk-ant-oat-custom"},
		Egress:           egress,
		Body:             testBody([]byte(`{"model":"claude-opus-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
	}
}

func captureCustomClaudeSidecarMeta(t *testing.T) sidecarMetaCapture {
	t.Helper()
	var encodedMeta string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		encodedMeta = r.Header.Get("X-Sidecar-Meta")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":200,"headers":{"content-type":["application/json"]}}`)
	}))
	defer sidecar.Close()

	cfg := config.Default()
	cfg.EgressFingerprintEngine = "sidecar"
	client := NewClient(cfg)
	egress := storage.EgressProfile{
		ID:       "eg-sidecar",
		Type:     storage.CurlCFFISidecarEgressType,
		Endpoint: sidecar.URL,
		Health:   "healthy",
	}

	resp, err := client.Do(t.Context(), customClaudeRequest("https://api.anthropic.com", egress))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if encodedMeta == "" {
		t.Fatal("fake sidecar received no X-Sidecar-Meta header")
	}
	raw, err := base64.StdEncoding.DecodeString(encodedMeta)
	if err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal meta: %v (%s)", err, raw)
	}
	return sidecarMetaCapture{Raw: raw, Fields: fields}
}

func runCustomClaudeDirectRequest(t *testing.T) (*Client, http.Header) {
	t.Helper()
	var seen http.Header
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","content":[]}`)
	}))
	defer upstreamSrv.Close()

	client := NewClient(config.Default())
	egress := storage.EgressProfile{ID: "eg", Type: "direct", Health: "healthy"}
	resp, err := client.Do(t.Context(), customClaudeRequest(upstreamSrv.URL, egress))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return client, seen
}
