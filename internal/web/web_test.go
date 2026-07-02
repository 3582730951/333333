package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandlerServesEmbeddedIndex(t *testing.T) {
	rec := request(t, Handler(), "/")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status = %s", rec.Result().Status)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Fatalf("Content-Type = %q, want html", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<title>Pool") {
		t.Fatalf("embedded index body did not look like the legacy UI")
	}
}

func TestAssetsHandlerFallsBackWhenEmbeddedDirMissing(t *testing.T) {
	handler := assetsHandler(fstest.MapFS{}, "assets")
	rec := request(t, handler, "/")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fallback status = %s", rec.Result().Status)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("fallback Content-Type = %q, want text/plain", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "embedded web assets unavailable") {
		t.Fatalf("fallback body = %q", body)
	}
}

func TestWebSourcesDoNotPanicOrUseHTTPError(t *testing.T) {
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
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "panic" {
					t.Errorf("%s calls panic; return a fallback handler instead", fset.Position(fun.Pos()))
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name != "Error" {
					return true
				}
				pkg, ok := fun.X.(*ast.Ident)
				if ok && pkg.Name == "http" {
					t.Errorf("%s uses http.Error; use the web fallback response instead", fset.Position(fun.Pos()))
				}
			}
			return true
		})
	}
}

func TestEmbeddedAssetFilesRemainReadable(t *testing.T) {
	matches, err := fs.Glob(assets, "assets/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no embedded html assets found")
	}
	for _, match := range matches {
		f, err := assets.Open(match)
		if err != nil {
			t.Fatalf("open %s: %v", match, err)
		}
		buf := make([]byte, 1)
		_, readErr := f.Read(buf)
		closeErr := f.Close()
		if readErr != nil && readErr != io.EOF {
			t.Fatalf("read %s: %v", match, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close %s: %v", match, closeErr)
		}
	}
}

func request(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
