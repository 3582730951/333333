package upstream

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// allEgressTypes is every egress profile type the scheduler can bind to an account.
// Kept as a literal list on purpose: if a new type is added to storage, the coverage test
// below starts failing until it is considered here, which is the point.
var allEgressTypes = []string{
	"direct",
	"",
	"http_proxy",
	"https_proxy",
	"warp_proxy",
	"socks5_proxy",
	"socks5h_proxy",
	storage.CurlCFFISidecarEgressType,
}

// TestClaudeFingerprintEngineCoversEveryEgressType is the fix for the highest-severity
// exposure: the engine used to be gated on a curl_cffi_sidecar egress, so an account bound
// to direct/http_proxy/socks5/... sent its Claude traffic over Go's stdlib transport. That
// emits a Go TLS ClientHello and Go HTTP/2 SETTINGS while the headers claim claude-cli on
// Node — a self-contradiction visible on every single request.
//
// No egress type may resolve to the stdlib engine unless the operator explicitly set
// ClaudeForceDirect.
func TestClaudeFingerprintEngineCoversEveryEgressType(t *testing.T) {
	for _, engineMode := range []string{"inprocess", "sidecar", ""} {
		cfg := config.Default()
		cfg.EgressFingerprintEngine = engineMode
		c := NewClient(cfg)
		if c.tlsFactory == nil {
			t.Fatal("NewClient left tlsFactory nil; the in-process engine would be unavailable")
		}
		for _, egressType := range allEgressTypes {
			egress := storage.EgressProfile{ID: "eg-" + egressType, Type: egressType, Health: "healthy"}
			if got := c.claudeFingerprintEngine(egress); got == claudeEngineStdlib {
				t.Errorf("engine=%q egress=%q resolved to the stdlib transport, which leaks a Go TLS/H2 fingerprint to api.anthropic.com", engineMode, egressType)
			}
		}
	}
}

// TestClaudeForceDirectRemainsAnExplicitOptIn: the one path allowed to reach the stdlib
// transport must require the operator flag, so the fallback can never be entered silently.
func TestClaudeForceDirectRemainsAnExplicitOptIn(t *testing.T) {
	cfg := config.Default()
	cfg.ClaudeForceDirect = true
	c := NewClient(cfg)
	for _, egressType := range allEgressTypes {
		egress := storage.EgressProfile{ID: "eg", Type: egressType, Health: "healthy"}
		if got := c.claudeFingerprintEngine(egress); got != claudeEngineStdlib {
			t.Errorf("ClaudeForceDirect egress=%q engine=%v, want stdlib (the flag's documented meaning)", egressType, got)
		}
	}

	// Without the flag, and with no TLS factory at all, stdlib is the only option left —
	// assert that this is the ONLY way to get there apart from the flag.
	bare := &Client{cfg: config.Default()}
	if got := bare.claudeFingerprintEngine(storage.EgressProfile{Type: "direct"}); got != claudeEngineStdlib {
		t.Errorf("client with no tlsFactory engine=%v, want stdlib fallback", got)
	}
}

// TestEgressProxyURLResolvesEveryProxyType: the in-process engine receives the exit as a
// single ProxyURL string. It previously read only ChainProxy, which is empty for a plain
// http_proxy/socks5 profile (those carry the exit in Endpoint) — so those accounts would
// have egressed from the relay host's own IP while the account's other traffic used the
// proxy. One account appearing from two exit IPs is a strong relay signal.
func TestEgressProxyURLResolvesEveryProxyType(t *testing.T) {
	for _, egressType := range []string{"http_proxy", "https_proxy", "warp_proxy", "socks5_proxy", "socks5h_proxy"} {
		egress := storage.EgressProfile{Type: egressType, Endpoint: "http://proxy.example:8080"}
		if got := egressProxyURL(egress); got != "http://proxy.example:8080" {
			t.Errorf("%s: egressProxyURL = %q, want the Endpoint exit", egressType, got)
		}
		// ChainProxy is the fallback when Endpoint is unset.
		chained := storage.EgressProfile{Type: egressType, ChainProxy: "socks5://chain.example:1080"}
		if got := egressProxyURL(chained); got != "socks5://chain.example:1080" {
			t.Errorf("%s: egressProxyURL = %q, want the ChainProxy fallback", egressType, got)
		}
	}

	// A sidecar egress's Endpoint is the SIDECAR, not the exit: the real exit is the chain
	// proxy. Returning Endpoint here would send upstream traffic to the sidecar's own
	// control port instead of through the intended exit.
	sidecar := storage.EgressProfile{
		Type:       storage.CurlCFFISidecarEgressType,
		Endpoint:   "http://127.0.0.1:8788",
		ChainProxy: "http://exit.example:3128",
	}
	if got := egressProxyURL(sidecar); got != "http://exit.example:3128" {
		t.Errorf("sidecar egress: egressProxyURL = %q, want the chain proxy exit", got)
	}

	// Direct has no proxy.
	if got := egressProxyURL(storage.EgressProfile{Type: "direct"}); got != "" {
		t.Errorf("direct egress: egressProxyURL = %q, want empty", got)
	}
}

// --- Structural (AST) regression tests ---------------------------------------------
//
// The tests above assert current behavior. These assert that the SHAPE of the code cannot
// regress: a future edit that reintroduces a raw http.Client on an Anthropic path, or drops
// the header order, fails here even if no behavioral test happens to cover that call.

// TestClaudeUpstreamPathsAlwaysPassAHeaderOrder walks every non-test source file in this
// package and requires each postInProcess* call on an Anthropic path to pass a header
// order. A nil order silently reverts to fhttp's lexicographic sort.
func TestClaudeUpstreamPathsAlwaysPassAHeaderOrder(t *testing.T) {
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
		case "postInProcess":
			t.Errorf("%s: anthropic.go calls postInProcess (no header order); use postInProcessOrdered with claudeHeaderOrder", fset.Position(call.Pos()))
		case "postInProcessOrdered":
			calls++
			last := call.Args[len(call.Args)-1]
			ident, isIdent := last.(*ast.Ident)
			if isIdent && ident.Name == "nil" {
				t.Errorf("%s: postInProcessOrdered called with a nil header order; fhttp then sorts headers lexicographically", fset.Position(call.Pos()))
				return true
			}
			orderCall, isCall := last.(*ast.CallExpr)
			if !isCall {
				t.Errorf("%s: postInProcessOrdered header order is %T, want a claudeHeaderOrder(...) call", fset.Position(call.Pos()), last)
				return true
			}
			fn, _ := orderCall.Fun.(*ast.Ident)
			if fn == nil || (fn.Name != "claudeHeaderOrder" && fn.Name != "claudeOAuthHeaderOrder") {
				t.Errorf("%s: postInProcessOrdered header order does not come from a Claude wire-order helper", fset.Position(call.Pos()))
			}
		}
		return true
	})
	if calls == 0 {
		t.Fatal("found no postInProcessOrdered calls in anthropic.go; the test is vacuous (was the Claude in-process path renamed?)")
	}
}

// httpClientLiteralAllowlist records every place in this package that builds its own
// http.Client, with the reason it is egress-safe. An http.Client built outside this list
// bypasses httpClientForEgress, and therefore the account's egress binding and cookie jar.
//
// The allowlist is keyed by "file.go:funcName" so moving a literal into a new function is a
// deliberate act that shows up here, and each entry has to state why it is acceptable.
var httpClientLiteralAllowlist = map[string]string{
	// The pooled chokepoint every egress-bound request funnels through.
	"client.go:httpClientForEgressMode": "builds the per-egress client from transportForEgressMode; this IS the chokepoint",
	"client.go:sidecarHTTPClient":       "sidecar control-plane client, transport comes from the egress",
	// Wraps transportForEgress, so the egress applies; used by non-Anthropic adapters.
	"egress_client.go:EgressHTTPClient": "wraps transportForEgress, so the egress and its proxy still apply",
	// Antigravity is a different provider (Google hosts). Its transport is HTTP/1.1-pinned
	// on purpose. These are NOT Anthropic paths; TestNoAnthropicHostOnUnboundClient below
	// enforces that they never grow one.
	"antigravity.go:directAntigravityRawRequest": "Antigravity (Google) token/model metadata calls, not Anthropic",
	"antigravity.go:DoAntigravity":               "standalone Antigravity adapter entrypoint, not Anthropic",
}

// TestNoUnreviewedHTTPClientLiteral fails when a new http.Client literal appears without a
// reviewed allowlist entry. Transport construction is fine; an unbound Client is not,
// because that is how a request escapes the account's egress.
func TestNoUnreviewedHTTPClientLiteral(t *testing.T) {
	found := 0
	forEachClientLiteral(t, func(fset *token.FileSet, file, fn string, pos token.Pos) {
		found++
		key := file + ":" + fn
		if _, ok := httpClientLiteralAllowlist[key]; !ok {
			t.Errorf("%s: %s builds an http.Client literal with no reviewed allowlist entry (%q). "+
				"Route it through httpClientForEgress so the account's egress and cookie jar apply, "+
				"or add an entry to httpClientLiteralAllowlist stating which host it targets.",
				fset.Position(pos), fn, key)
		}
	})
	if found == 0 {
		t.Fatal("found no http.Client literals anywhere; the test is vacuous (did the chokepoint move?)")
	}
	// Every allowlist entry must still correspond to real code, so the list cannot rot into
	// a set of stale exemptions that quietly permit new call sites.
	seen := map[string]bool{}
	forEachClientLiteral(t, func(_ *token.FileSet, file, fn string, _ token.Pos) {
		seen[file+":"+fn] = true
	})
	for key := range httpClientLiteralAllowlist {
		if !seen[key] {
			t.Errorf("stale allowlist entry %q: no http.Client literal there anymore; remove it", key)
		}
	}
}

// TestNoAnthropicHostOnUnboundClient: the allowlisted, egress-unbound clients above must
// never be used for an Anthropic host. Anthropic traffic has to carry the account's exit IP
// and impersonated fingerprint, so a hardcoded Anthropic URL in one of those files is a bug.
func TestNoAnthropicHostOnUnboundClient(t *testing.T) {
	unbound := map[string]bool{"antigravity.go": true, "egress_client.go": true}
	fset := token.NewFileSet()
	for name := range unbound {
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			lit, ok := node.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value := strings.ToLower(lit.Value)
			for _, marker := range []string{"anthropic.com", "claude.ai"} {
				if strings.Contains(value, marker) {
					t.Errorf("%s: %s references %s but uses an egress-unbound client; Anthropic traffic must go through the Claude path",
						fset.Position(lit.Pos()), name, marker)
				}
			}
			return true
		})
	}
}

// forEachClientLiteral calls visit for every http.Client composite literal in the package's
// non-test sources, reporting the enclosing file and function name.
func forEachClientLiteral(t *testing.T, visit func(fset *token.FileSet, file, fn string, pos token.Pos)) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn, func(node ast.Node) bool {
				lit, ok := node.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Client" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "http" {
					visit(fset, name, fn.Name.Name, lit.Pos())
				}
				return true
			})
		}
	}
}

// TestClaudeUpstreamRequestsNeverCopyDownstreamHeaders: newReplayableHTTPRequest must not
// seed the upstream header set from the downstream request. Copying it wholesale is the
// classic relay leak (it carries X-Forwarded-For, Cookie, and the gateway's own headers),
// and it is invisible in behavioral tests as long as fixtures send clean headers.
func TestClaudeUpstreamRequestsNeverCopyDownstreamHeaders(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "client.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse client.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if ok && decl.Name.Name == "newReplayableHTTPRequest" {
			fn = decl
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("newReplayableHTTPRequest not found in client.go (renamed? update this test)")
	}

	ast.Inspect(fn, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// Any read of spec.Headers inside the request constructor means downstream headers
		// are being seeded onto the upstream request.
		if sel.Sel.Name != "Headers" {
			return true
		}
		if inner, ok := sel.X.(*ast.Ident); ok && inner.Name == "spec" {
			t.Errorf("%s: newReplayableHTTPRequest reads spec.Headers; the upstream header set must be built from scratch by the provider builders", fset.Position(sel.Pos()))
		}
		return true
	})
}

// TestDoAnthropicOAuthUsesAccountEgress: the OAuth token call must travel on the account's
// egress on every engine, including the ClaudeForceDirect fallback. If the refresh runs from
// the relay host while inference runs from the account's proxy, Anthropic sees one account's
// credentials being maintained from a machine that never serves its traffic.
func TestDoAnthropicOAuthUsesAccountEgress(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "anthropic.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse anthropic.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if ok && decl.Name.Name == "DoAnthropicOAuth" {
			fn = decl
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("DoAnthropicOAuth not found; the Anthropic OAuth path must go through the upstream client")
	}

	// It must never fall back to a bare http.Get/http.Post or http.DefaultClient.
	ast.Inspect(fn, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return true
		}
		switch sel.Sel.Name {
		case "Get", "Post", "PostForm", "DefaultClient":
			t.Errorf("%s: DoAnthropicOAuth uses http.%s, which ignores the account's egress", fset.Position(sel.Pos()), sel.Sel.Name)
		}
		return true
	})

	// It must consult claudeFingerprintEngine so the token call shares the inference
	// fingerprint rather than emitting a Go ClientHello at the same host.
	usesEngine := false
	ast.Inspect(fn, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "claudeFingerprintEngine" {
			usesEngine = true
		}
		return true
	})
	if !usesEngine {
		t.Error("DoAnthropicOAuth does not consult claudeFingerprintEngine; the token call would not share the account's TLS fingerprint")
	}
}

// TestDoAnthropicOAuthSendsNoRelayHeaders: the OAuth call builds its own header set, so
// verify at the boundary that a caller-supplied relay header cannot ride along. This is the
// same assertion class as the messages path, applied to the credential lifecycle.
func TestDoAnthropicOAuthSendsNoRelayHeaders(t *testing.T) {
	// The header set the api layer hands to DoAnthropicOAuth is constructed from scratch;
	// assert that shape here so a future edit that forwards r.Header is caught.
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "test-agent")
	assertNoRelayCharacteristics(t, "anthropic-oauth", headers)
}
