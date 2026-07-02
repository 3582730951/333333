package api

import (
	"codex-account-pool/internal/config"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPISourcesDoNotUseHTTPError(t *testing.T) {
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

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "http" {
				return true
			}
			t.Errorf("%s uses the plain-text HTTP error writer; use writeError or methodNotAllowed for JSON API failures", fset.Position(sel.Pos()))
			return true
		})
	}
}

func TestReadUpstreamErrorBodyIsLimited(t *testing.T) {
	body := readUpstreamErrorBody(strings.NewReader(strings.Repeat("x", upstreamErrorBodyLimit*2)))
	if len(body) != upstreamErrorBodyLimit {
		t.Fatalf("limited error body length = %d, want %d", len(body), upstreamErrorBodyLimit)
	}
}

func TestReadUpstreamResponseBodyUsesServerLimit(t *testing.T) {
	s := &Server{cfg: config.Config{MaxBodyBytes: 8}}

	body, err := s.readUpstreamResponseBody(strings.NewReader("12345678"))
	if err != nil {
		t.Fatalf("read within limit: %v", err)
	}
	if string(body) != "12345678" {
		t.Fatalf("body = %q", body)
	}

	_, err = s.readUpstreamResponseBody(strings.NewReader("123456789"))
	if err == nil {
		t.Fatal("expected oversized response error")
	}
	if !strings.Contains(err.Error(), "upstream response body too large") {
		t.Fatalf("error = %q", err)
	}
}
