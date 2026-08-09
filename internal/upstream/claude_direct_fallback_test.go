package upstream

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"codex-account-pool/internal/storage"
)

// findFuncDecl locates a top-level func or method by name in a source file of this
// package. It fails the test when the target is absent so a rename cannot turn a
// structural assertion into a silent no-op.
func findFuncDecl(t *testing.T, fileName, funcName string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", fileName), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", fileName, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == funcName {
			return fn
		}
	}
	t.Fatalf("%s: func %s not found (renamed or moved? this assertion must not silently pass)", fileName, funcName)
	return nil
}

// TestClaudeDirectFallbackNeverInventsAHostExit is the regression guard for the silent
// egress fallback class: any path that unwraps a sidecar must keep the account's real exit
// IP. A fallback that quietly connects from the relay host while the account's inference
// turns leave through a proxy puts one account on two networks — a stronger relay signal
// than an imperfect fingerprint, and invisible because the request still succeeds.
func TestClaudeDirectFallbackNeverInventsAHostExit(t *testing.T) {
	wrapped, err := storage.WrapEgressWithSidecar(
		storage.EgressProfile{ID: "eg-proxy", Type: "socks5h_proxy", Endpoint: "socks5h://user:pass@10.0.0.9:1080"},
		storage.EgressProfile{ID: "eg-sc", Type: storage.CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8081"},
	)
	if err != nil {
		t.Fatalf("WrapEgressWithSidecar: %v", err)
	}

	cases := []struct {
		name         string
		in           storage.EgressProfile
		wantType     string
		wantEndpoint string
	}{
		{
			name:         "two-layer binding restores the selected proxy",
			in:           wrapped,
			wantType:     "socks5h_proxy",
			wantEndpoint: "socks5h://user:pass@10.0.0.9:1080",
		},
		{
			// The leak this test exists for: WithoutSidecarTransport alone rewrites this
			// to direct with an empty endpoint, so the request would leave from the host.
			name: "legacy sidecar-primary keeps its explicit chain proxy",
			in: storage.EgressProfile{
				ID:         "eg-legacy",
				Type:       storage.CurlCFFISidecarEgressType,
				Endpoint:   "http://127.0.0.1:8081",
				ChainProxy: "http://user:pass@10.0.0.7:3128",
			},
			wantType:     "http_proxy",
			wantEndpoint: "http://user:pass@10.0.0.7:3128",
		},
		{
			name: "legacy sidecar-primary chain scheme is honored, not defaulted",
			in: storage.EgressProfile{
				ID:         "eg-legacy-socks",
				Type:       storage.CurlCFFISidecarEgressType,
				Endpoint:   "http://127.0.0.1:8081",
				ChainProxy: "socks5://10.0.0.8:1080",
			},
			wantType:     "socks5_proxy",
			wantEndpoint: "socks5://10.0.0.8:1080",
		},
		{
			// Genuinely no exit configured anywhere: direct is the only truthful answer.
			name:         "chainless legacy sidecar has no exit to preserve",
			in:           storage.EgressProfile{ID: "eg-bare", Type: storage.CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8081"},
			wantType:     "direct",
			wantEndpoint: "",
		},
		{
			name:         "a plain proxy egress is passed through untouched",
			in:           storage.EgressProfile{ID: "eg-plain", Type: "http_proxy", Endpoint: "http://10.0.0.5:3128"},
			wantType:     "http_proxy",
			wantEndpoint: "http://10.0.0.5:3128",
		},
		{
			name:         "a direct egress stays direct",
			in:           storage.EgressProfile{ID: "eg-direct", Type: "direct"},
			wantType:     "direct",
			wantEndpoint: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claudeDirectFallbackEgress(tc.in)
			if got.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tc.wantType)
			}
			if got.Endpoint != tc.wantEndpoint {
				t.Errorf("Endpoint = %q, want %q", got.Endpoint, tc.wantEndpoint)
			}
			if storage.IsSidecarEgress(got) {
				t.Errorf("Type = %q: the direct fallback must not still be a sidecar profile", got.Type)
			}
			// The exit must remain attributable to the same egress row, otherwise
			// scheduler load, cooldowns and quota accounting drift off the real exit.
			if got.ID != tc.in.ID {
				t.Errorf("ID = %q, want %q (exit must stay attributable to its egress row)", got.ID, tc.in.ID)
			}
			// A preserved chain must not be re-dialed as a chain on top of itself.
			if got.Endpoint != "" && got.ChainProxy == got.Endpoint {
				t.Errorf("ChainProxy = %q duplicates Endpoint; the proxy would be dialed twice", got.ChainProxy)
			}
		})
	}
}

// TestIsHTTPSTarget pins the gate that decides whether a bypass has a TLS fingerprint
// worth preserving. A lookalike scheme must not read as https.
func TestIsHTTPSTarget(t *testing.T) {
	cases := map[string]bool{
		"https://api.anthropic.com/v1/messages": true,
		"HTTPS://api.anthropic.com/v1/messages": true,
		"http://api.anthropic.com/v1/messages":  false,
		"http://upstream.invalid/v1/responses":  false,
		"ws://example.test/socket":              false,
		"httpsx://example.test/":                false,
		"":                                      false,
		"://malformed":                          false,
	}
	for target, want := range cases {
		if got := isHTTPSTarget(target); got != want {
			t.Errorf("isHTTPSTarget(%q) = %v, want %v", target, got, want)
		}
	}
}

// TestSidecarBypassPrefersFingerprintEngine: when the sidecar is unhealthy the bypass keeps
// the exit IP but must not swap the impersonated fingerprint for Go's. This is pinned
// structurally because tlsFactory is a concrete type with no seam to observe, and the
// failure is invisible at runtime — the bypassed request succeeds either way.
func TestSidecarBypassPrefersFingerprintEngine(t *testing.T) {
	fn := findFuncDecl(t, "client.go", "postDirectThroughSidecarChain")
	var usesInProcess, gatesOnTLS, gatesOnFactory bool
	ast.Inspect(fn, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			if n.Sel.Name == "postInProcessOrdered" {
				usesInProcess = true
			}
			if n.Sel.Name == "tlsFactory" {
				gatesOnFactory = true
			}
		case *ast.Ident:
			if n.Name == "isHTTPSTarget" {
				gatesOnTLS = true
			}
		}
		return true
	})
	if !usesInProcess {
		t.Error("postDirectThroughSidecarChain does not call postInProcessOrdered; a sidecar outage would emit a Go ClientHello under impersonated headers")
	}
	if !gatesOnFactory {
		t.Error("postDirectThroughSidecarChain does not check tlsFactory; it would nil-panic where no engine is configured")
	}
	if !gatesOnTLS {
		t.Error("postDirectThroughSidecarChain does not gate on isHTTPSTarget; a cleartext bypass has no fingerprint to preserve and must keep the stdlib wire shape")
	}
}

// TestClaudeDirectFallbackSitesUseTheHelper pins the call sites structurally. Calling
// storage.WithoutSidecarTransport directly in either Claude fallback re-introduces the
// host-exit leak, and no runtime assertion would notice because the request still succeeds.
func TestClaudeDirectFallbackSitesUseTheHelper(t *testing.T) {
	for _, fnName := range []string{"doClaude", "DoAnthropicOAuth"} {
		t.Run(fnName, func(t *testing.T) {
			fn := findFuncDecl(t, "anthropic.go", fnName)
			ast.Inspect(fn, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if pkg.Name == "storage" && sel.Sel.Name == "WithoutSidecarTransport" {
					t.Errorf("%s calls storage.WithoutSidecarTransport directly; use claudeDirectFallbackEgress so a legacy sidecar's chain proxy is not replaced by a host-direct exit", fnName)
				}
				return true
			})
		})
	}
}
