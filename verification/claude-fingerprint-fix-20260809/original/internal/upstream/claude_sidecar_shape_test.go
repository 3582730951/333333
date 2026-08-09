package upstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// sidecarMetaCapture runs one Claude request against a fake sidecar and returns the decoded
// X-Sidecar-Meta plus the RAW meta JSON (order-preserving, which is the whole point).
type sidecarMetaCapture struct {
	Raw    []byte
	Fields map[string]json.RawMessage
}

// headerNamesInWireOrder returns the header names in the order they appear in the meta JSON
// object. This is the order the sidecar puts on the socket: json.loads keeps the object's
// order, clean_headers is an order-preserving dict comprehension, and curl_cffi's Headers
// type is an ordered list written to CurlOpt.HTTPHEADER in that order.
func (c sidecarMetaCapture) headerNamesInWireOrder(t *testing.T) []string {
	t.Helper()
	rawHeaders, ok := c.Fields["headers"]
	if !ok {
		t.Fatalf("sidecar meta carries no headers field: %s", c.Raw)
	}
	dec := json.NewDecoder(strings.NewReader(string(rawHeaders)))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("headers is not a JSON object: %v (%s)", err, rawHeaders)
	}
	var names []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatalf("read header key: %v", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			t.Fatalf("header key is %T, want string", keyTok)
		}
		names = append(names, strings.ToLower(key))
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			t.Fatalf("read header value for %q: %v", key, err)
		}
	}
	return names
}

// captureClaudeSidecarMeta drives doClaude with the sidecar engine and captures the meta the
// Go side hands the sidecar process.
func captureClaudeSidecarMeta(t *testing.T, target string) sidecarMetaCapture {
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
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":200,"headers":{"content-type":["application/json"]}}`)
	}))
	defer sidecar.Close()

	cfg := config.Default()
	// Pin the sidecar engine: this is the operator configuration this test is about.
	cfg.EgressFingerprintEngine = "sidecar"
	cfg.ClaudeUpstreamBaseURL = target
	client := NewClient(cfg)

	egress := storage.EgressProfile{
		ID:       "eg-sidecar",
		Type:     storage.CurlCFFISidecarEgressType,
		Endpoint: sidecar.URL,
		Health:   "healthy",
	}
	if got := client.claudeFingerprintEngine(egress); got != claudeEngineSidecar {
		t.Fatalf("engine = %v, want claudeEngineSidecar; the test would not exercise the sidecar path", got)
	}

	resp, err := client.Do(t.Context(), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Account:        storage.Account{ID: "acct-1", Provider: "claude"},
		Token:          storage.AccountToken{AccessToken: "sk-ant-oat-test"},
		Egress:         egress,
		Body:           testBody([]byte(`{"model":"claude-opus-4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)),
	})
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

// TestClaudeSidecarMetaCarriesUndiciHeaderOrder is the regression test for the sidecar
// engine emitting headers ALPHABETICALLY.
//
// The header map used to be a plain map[string][]string, and encoding/json sorts map keys.
// Every stage after that preserves order (Python dict -> clean_headers comprehension ->
// curl_cffi Headers -> CurlOpt.HTTPHEADER), so Go's map-key sort reached the socket: the
// sidecar engine put "accept, anthropic-beta, anthropic-version, authorization,
// content-type, user-agent, x-app, x-stainless-*" on the wire under a claude-cli
// User-Agent. Real Claude Code (Anthropic TS SDK on Node's undici) emits insertion order,
// which starts accept, user-agent, x-stainless-retry-count.
func TestClaudeSidecarMetaCarriesUndiciHeaderOrder(t *testing.T) {
	capture := captureClaudeSidecarMeta(t, "https://api.anthropic.com")
	names := capture.headerNamesInWireOrder(t)
	if len(names) < 4 {
		t.Fatalf("sidecar meta carried only %d headers (%v); the capture is not exercising the Claude header set", len(names), names)
	}

	// The real client's first three headers, per anthropic-sdk-typescript buildHeaders().
	wantPrefix := []string{"accept", "user-agent", "x-stainless-retry-count"}
	for i, want := range wantPrefix {
		if i >= len(names) || names[i] != want {
			t.Fatalf("sidecar wire header order = %v\nwant it to start with %v (the Anthropic TS SDK insertion order); alphabetical order is a relay tell under a claude-cli User-Agent", names, wantPrefix)
		}
	}

	// Guard the specific regression: alphabetical order must not be what we emit.
	sorted := append([]string(nil), names...)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] > sorted[i] {
			return // not sorted: good
		}
	}
	t.Fatalf("sidecar wire header order %v is exactly alphabetical, i.e. encoding/json's map-key sort still reaches the socket", names)
}

// TestClaudeSidecarMetaAlsoCarriesAnExplicitHeaderOrder: the ordered JSON object is the
// mechanism that works with today's sidecar, but a sidecar that rebuilds the mapping
// internally would lose it. The explicit list is the belt-and-braces channel, and it must
// agree with the object's own order.
func TestClaudeSidecarMetaAlsoCarriesAnExplicitHeaderOrder(t *testing.T) {
	capture := captureClaudeSidecarMeta(t, "https://api.anthropic.com")
	rawOrder, ok := capture.Fields["header_order"]
	if !ok {
		t.Fatalf("sidecar meta has no header_order field: %s", capture.Raw)
	}
	var order []string
	if err := json.Unmarshal(rawOrder, &order); err != nil {
		t.Fatalf("unmarshal header_order: %v", err)
	}
	names := capture.headerNamesInWireOrder(t)
	present := map[string]bool{}
	for _, n := range names {
		present[n] = true
	}
	// header_order may name transport-injected headers (host, content-length) that are
	// absent from the header set; the ones that ARE present must appear in the same
	// relative order as the JSON object.
	var projected []string
	for _, n := range order {
		if present[strings.ToLower(n)] {
			projected = append(projected, strings.ToLower(n))
		}
	}
	if fmt.Sprint(projected) != fmt.Sprint(names) {
		t.Errorf("header_order projects to %v but the meta object order is %v; the two channels disagree", projected, names)
	}
}

// TestClaudeSidecarMetaPinsHTTP11 is the regression test for the sidecar engine negotiating
// HTTP/2 at Anthropic.
//
// Round one pinned HTTP/1.1 for the in-process engine only (tls_client.WithForceHttp1). The
// sidecar's curl_cffi impersonation advertises ALPN ["h2","http/1.1"] and Anthropic's edge
// prefers h2, so a sidecar-engine account emitted a claude-cli User-Agent over HTTP/2 —
// something Node's bundled undici (allowH2:false) cannot produce.
func TestClaudeSidecarMetaPinsHTTP11(t *testing.T) {
	capture := captureClaudeSidecarMeta(t, "https://api.anthropic.com")
	rawVersion, ok := capture.Fields["http_version"]
	if !ok {
		t.Fatalf("sidecar meta has no http_version field, so the sidecar negotiates h2 at Anthropic under a claude-cli UA: %s", capture.Raw)
	}
	var version string
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		t.Fatalf("unmarshal http_version: %v", err)
	}
	if version != sidecarHTTPVersion1 {
		t.Errorf("http_version = %q, want %q (curl_cffi normalize_http_version's HTTP/1.1 literal)", version, sidecarHTTPVersion1)
	}
}

// TestAnthropicSidecarCallsAlwaysPassAHeaderOrder is the structural guard: anthropic.go must
// never call the order-less postViaSidecar, mirroring the existing postInProcess guard. A
// future edit that reverts to postViaSidecar would silently restore alphabetical wire order
// even if no behavioral test happened to cover that call.
func TestAnthropicSidecarCallsAlwaysPassAHeaderOrder(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "anthropic.go"), nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse anthropic.go: %v", err)
	}
	calls := 0
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
			t.Errorf("%s: anthropic.go calls postViaSidecar (no header order); use postViaSidecarOrdered with claudeHeaderOrder, or the sidecar emits headers alphabetically", fset.Position(call.Pos()))
		case "postViaSidecarOrdered":
			calls++
			last := call.Args[len(call.Args)-1]
			if ident, isIdent := last.(*ast.Ident); isIdent && ident.Name == "nil" {
				t.Errorf("%s: postViaSidecarOrdered called with a nil header order; encoding/json then sorts the header map and the sidecar puts that alphabetical order on the wire", fset.Position(call.Pos()))
				return true
			}
			orderCall, isCall := last.(*ast.CallExpr)
			if !isCall {
				t.Errorf("%s: postViaSidecarOrdered header order is %T, want a claudeHeaderOrder(...) call", fset.Position(call.Pos()), last)
				return true
			}
			fn, _ := orderCall.Fun.(*ast.Ident)
			if fn == nil || fn.Name != "claudeHeaderOrder" {
				t.Errorf("%s: postViaSidecarOrdered header order does not come from claudeHeaderOrder", fset.Position(call.Pos()))
			}
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no postViaSidecarOrdered calls in anthropic.go; the test is vacuous (was the Claude sidecar path renamed?)")
	}
}

// TestOrderedHeadersWithoutOrderMatchesPlainMapEncoding: the non-Claude sidecar callers
// (Codex, registration, the raw egress path) pass no order and MUST keep byte-identical
// wire meta, or this change would move their fingerprint too.
func TestOrderedHeadersWithoutOrderMatchesPlainMapEncoding(t *testing.T) {
	built := http.Header{}
	built.Set("Accept", "application/json")
	built.Set("User-Agent", "codex/1.0")
	built.Set("Content-Type", "application/json")
	built.Add("Anthropic-Beta", "oauth-2025-04-20")

	plain := map[string][]string{}
	for k, values := range built {
		plain[k] = append([]string(nil), values...)
	}
	want, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain map: %v", err)
	}
	got, err := json.Marshal(orderedHeaders{Header: built})
	if err != nil {
		t.Fatalf("marshal orderedHeaders: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("orderedHeaders with no order encoded as\n  %s\nwant the previous plain-map encoding\n  %s", got, want)
	}
}

// TestOrderedHeadersPutsUnnamedHeadersInADeterministicTail: an order that does not mention
// every header must still produce a stable sequence. Order drift between two requests from
// the same account is itself a risk-control signal.
func TestOrderedHeadersPutsUnnamedHeadersInADeterministicTail(t *testing.T) {
	built := http.Header{}
	built.Set("Accept", "application/json")
	built.Set("User-Agent", "claude-cli/2.0.1 (external, cli)")
	built.Set("X-Custom-B", "b")
	built.Set("X-Custom-A", "a")

	first, err := json.Marshal(orderedHeaders{Header: built, Order: []string{"host", "accept", "user-agent", "content-length"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 32; i++ {
		again, err := json.Marshal(orderedHeaders{Header: built, Order: []string{"host", "accept", "user-agent", "content-length"}})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("orderedHeaders is not deterministic:\n  %s\n  %s", first, again)
		}
	}
	want := `{"Accept":["application/json"],"User-Agent":["claude-cli/2.0.1 (external, cli)"],"X-Custom-A":["a"],"X-Custom-B":["b"]}`
	if string(first) != want {
		t.Errorf("orderedHeaders = %s\nwant %s (ordered names first, absent transport-injected names skipped, remainder sorted)", first, want)
	}
}
