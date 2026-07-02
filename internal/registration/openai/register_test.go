package openai

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRegisterClientDefaultsHTTPClient(t *testing.T) {
	c := NewRegisterClient(nil, "device")
	if c.httpClient == nil {
		t.Fatal("http client is nil")
	}
	if c.httpClient.Timeout != registerHTTPTimeout {
		t.Fatalf("timeout = %s, want %s", c.httpClient.Timeout, registerHTTPTimeout)
	}
}

func TestReadRegisterResponseBodyIsLimited(t *testing.T) {
	body := readRegisterResponseBody(strings.NewReader(strings.Repeat("x", registerResponseBodyLimit*2)))
	if len(body) != registerResponseBodyLimit {
		t.Fatalf("body length = %d, want %d", len(body), registerResponseBodyLimit)
	}
}

func TestRegisterProductionDiscardCopiesAreBounded(t *testing.T) {
	fileSet := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSelectorCall(call, "io", "Copy") {
				return true
			}
			if len(call.Args) < 2 || !isSelectorExpr(call.Args[0], "io", "Discard") {
				return true
			}
			limitCall, ok := call.Args[1].(*ast.CallExpr)
			if !ok || !isSelectorCall(limitCall, "io", "LimitReader") {
				t.Errorf("%s discards a response body without io.LimitReader", fileSet.Position(call.Pos()))
			}
			return true
		})
	}
}

func isSelectorCall(call *ast.CallExpr, pkg, name string) bool {
	return isSelectorExpr(call.Fun, pkg, name)
}

func isSelectorExpr(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
