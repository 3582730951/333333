package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// anthropicHostMarkers are the substrings that identify an Anthropic-bound URL.
var anthropicHostMarkers = []string{
	"api.anthropic.com",
	"console.anthropic.com",
	"claude.ai",
	"anthropic.com",
}

// oauthClientCallees are the accessors that yield the plain, host-direct http.Client.
// Reaching Anthropic through one of these means the call leaves from the relay host's own
// IP with a Go TLS fingerprint, while that same account's inference traffic leaves from its
// bound egress with an impersonated fingerprint. Anthropic sees both on one account.
var oauthClientCallees = map[string]bool{
	"oauthHTTPClient":        true,
	"apiExternalHTTPClient":  true,
	"http.DefaultClient":     true,
	"defaultHTTPClientForAPI": true,
}

// functionsAllowedToUseThePlainClient are the reviewed exceptions. Each entry is a function
// that provably targets a NON-Anthropic host (OpenAI/Codex/ChatGPT/Antigravity endpoints),
// or is the documented no-upstream fallback for embedders and tests.
//
// This list is the point of the test: adding a new plain-client call site requires adding a
// name here, which forces the author to state which host it talks to.
var functionsAllowedToUseThePlainClient = map[string]string{
	// The accessor itself: it returns the client and issues no request.
	"oauthHTTPClient": "accessor for the plain client; issues no request of its own",
	"refreshCodexToken":         "auth.openai.com / chatgpt.com token endpoint",
	"fetchChatGPTSessionToken":  "chatgpt.com session endpoint",
	"runCodexReauthJob":         "operator-triggered Codex reauth worker (OpenAI hosts)",
	"exchangeCodexCode":         "OpenAI OAuth code exchange",
	"antigravityOAuthRequester": "Antigravity (Google) OAuth hosts",
	"doClaudeOAuthRequest":      "documented fallback used only when s.upstream is nil (embedders/tests); the live path uses upstream.DoAnthropicOAuth",
}

// TestNoAnthropicCallUsesThePlainHTTPClient walks every non-test file in the api package
// and reports any function that both references an Anthropic host and uses the plain
// host-direct client. It is the structural form of "all outbound Anthropic paths go through
// cloak/egress": it holds even for code paths no behavioral test exercises.
func TestNoAnthropicCallUsesThePlainHTTPClient(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	checked := 0
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
			usesPlainClient, clientPos := findPlainClientUse(fn)
			if !usesPlainClient {
				continue
			}
			checked++
			if _, allowed := functionsAllowedToUseThePlainClient[fn.Name.Name]; allowed {
				continue
			}
			t.Errorf("%s: %s uses the plain host-direct http.Client but is not in functionsAllowedToUseThePlainClient. "+
				"If it targets an Anthropic host, route it through s.upstream (DoAnthropicOAuth / upstream.Do) so the "+
				"account's egress and TLS fingerprint apply; if it targets another provider, add it to the allowlist with the host.",
				fset.Position(clientPos), fn.Name.Name)
		}
	}
	if checked == 0 {
		t.Fatal("found no plain-client call sites at all; the test is vacuous (did the accessor get renamed?)")
	}
}

// TestAnthropicHostsAreNotReachedByAllowlistedPlainClientFunctions closes the loophole in
// the allowlist above: an allowlisted function must not silently grow an Anthropic target.
func TestAnthropicHostsAreNotReachedByAllowlistedPlainClientFunctions(t *testing.T) {
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
			if _, allowed := functionsAllowedToUseThePlainClient[fn.Name.Name]; !allowed {
				continue
			}
			// doClaudeOAuthRequest is the documented nil-upstream fallback: its target IS an
			// Anthropic token URL, passed in by the caller, and the live path never reaches
			// the fallback. Verified separately by TestClaudeOAuthPrefersUpstreamClient.
			if fn.Name.Name == "doClaudeOAuthRequest" {
				continue
			}
			ast.Inspect(fn, func(node ast.Node) bool {
				lit, ok := node.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value := strings.ToLower(lit.Value)
				for _, marker := range anthropicHostMarkers {
					if strings.Contains(value, marker) {
						t.Errorf("%s: allowlisted plain-client function %s now references an Anthropic host (%s); "+
							"it must go through s.upstream instead", fset.Position(lit.Pos()), fn.Name.Name, marker)
					}
				}
				return true
			})
		}
	}
}

// findPlainClientUse reports whether fn calls the plain host-direct client.
func findPlainClientUse(fn *ast.FuncDecl) (bool, token.Pos) {
	var found bool
	var pos token.Pos
	ast.Inspect(fn, func(node ast.Node) bool {
		if found {
			return false
		}
		switch expr := node.(type) {
		case *ast.CallExpr:
			if ident, ok := expr.Fun.(*ast.Ident); ok && oauthClientCallees[ident.Name] {
				found, pos = true, expr.Pos()
				return false
			}
		case *ast.SelectorExpr:
			// http.DefaultClient
			if pkg, ok := expr.X.(*ast.Ident); ok && pkg.Name == "http" && expr.Sel.Name == "DefaultClient" {
				found, pos = true, expr.Pos()
				return false
			}
		case *ast.Ident:
			// Direct use of the package-level client variable, e.g. apiExternalHTTPClient.Do.
			if expr.Name == "apiExternalHTTPClient" {
				found, pos = true, expr.Pos()
				return false
			}
		}
		return true
	})
	return found, pos
}

// TestClaudeRefreshResolvesAccountEgress asserts structurally that the Claude token refresh
// resolves the account's egress before issuing the call. A refresh loop that runs on the
// host exit re-signals the pool for every account on a fixed schedule.
func TestClaudeRefreshResolvesAccountEgress(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "claude_refresh.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse claude_refresh.go: %v", err)
	}

	var refresh *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if ok && decl.Name.Name == "refreshClaudeToken" {
			refresh = decl
			return false
		}
		return true
	})
	if refresh == nil {
		t.Fatal("refreshClaudeToken not found in claude_refresh.go")
	}

	var resolvesEgress, usesHelper bool
	ast.Inspect(refresh, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "claudeOAuthEgress":
			resolvesEgress = true
		case "doClaudeOAuthRequest":
			usesHelper = true
		}
		return true
	})
	if !resolvesEgress {
		t.Error("refreshClaudeToken does not call claudeOAuthEgress; the token call would not share the account's exit IP")
	}
	if !usesHelper {
		t.Error("refreshClaudeToken does not use doClaudeOAuthRequest; it must not build its own client")
	}
}

// TestClaudeOAuthPrefersUpstreamClient verifies doClaudeOAuthRequest tries the upstream
// client first and only falls back when s.upstream is nil.
func TestClaudeOAuthPrefersUpstreamClient(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "claude_refresh.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse claude_refresh.go: %v", err)
	}

	var fn *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if ok && decl.Name.Name == "doClaudeOAuthRequest" {
			fn = decl
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatal("doClaudeOAuthRequest not found")
	}

	usesUpstream := false
	ast.Inspect(fn, func(node ast.Node) bool {
		if sel, ok := node.(*ast.SelectorExpr); ok && sel.Sel.Name == "DoAnthropicOAuth" {
			usesUpstream = true
		}
		return true
	})
	if !usesUpstream {
		t.Error("doClaudeOAuthRequest never calls upstream.DoAnthropicOAuth; the Anthropic token call would bypass the account egress")
	}

	// The plain-client fallback must be guarded by a nil check on s.upstream.
	guarded := false
	ast.Inspect(fn, func(node ast.Node) bool {
		ifStmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifStmt.Cond.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		sel, ok := bin.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "upstream" {
			return true
		}
		if ident, ok := bin.Y.(*ast.Ident); ok && ident.Name == "nil" {
			guarded = true
		}
		return true
	})
	if !guarded {
		t.Error("the plain-client fallback in doClaudeOAuthRequest is not guarded by an s.upstream != nil check")
	}
}
