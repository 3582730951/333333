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

func TestAPISourcesDoNotUseDefaultHTTPClient(t *testing.T) {
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
			sel, ok := node.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "DefaultClient" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "http" {
				t.Errorf("%s uses http.DefaultClient; use the API outbound client", fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

func TestSensitiveMutationSourcesUseBoundedJSONDecoder(t *testing.T) {
	files := []string{
		"admin_accounts.go",
		"admin_config.go",
		"admin_lifecycle.go",
		"admin_resources.go",
		"automation.go",
		"gopay.go",
		"keys.go",
		"lifecycle.go",
		"moderation.go",
		"oauth.go",
		"provider_testing.go",
		"providers.go",
		"registrar_config.go",
		"registration.go",
		"settings_center.go",
		"thinking.go",
		"userauth.go",
		"userportal.go",
	}

	fset := token.NewFileSet()
	for _, name := range files {
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
			if !ok || sel.Sel.Name != "NewDecoder" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "json" {
				t.Errorf("%s uses json.NewDecoder directly; use decodeJSONRequestBody/readJSONRequestBody so sensitive mutations are bounded and single-value", fset.Position(call.Pos()))
			}
			return true
		})
	}
}
