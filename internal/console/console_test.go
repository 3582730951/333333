package console

import (
	"bytes"
	"compress/gzip"
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
)

func TestHandlerCachePolicy(t *testing.T) {
	handler := Handler()

	indexResp := request(t, handler, "/console/")
	if got := indexResp.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Fatalf("index Cache-Control = %q", got)
	}
	if indexResp.Header().Get("Pragma") != "no-cache" || indexResp.Header().Get("Expires") != "0" {
		t.Fatalf("index missing legacy no-cache headers: %#v", indexResp.Header())
	}

	deepLinkResp := request(t, handler, "/console/accounts")
	if got := deepLinkResp.Header().Get("Cache-Control"); got != "no-cache, no-store, must-revalidate" {
		t.Fatalf("deep link Cache-Control = %q", got)
	}

	asset := firstEmbeddedAsset(t)
	assetResp := request(t, handler, "/console/"+asset)
	if got := assetResp.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset Cache-Control = %q", got)
	}
	if got := assetResp.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("asset Vary = %q", got)
	}
}

func TestHandlerServesGzipAssets(t *testing.T) {
	handler := Handler()
	asset := firstEmbeddedAsset(t)

	plain := request(t, handler, "/console/"+asset)
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain Content-Encoding = %q", got)
	}

	gzipped := requestWithHeader(t, handler, "/console/"+asset, "Accept-Encoding", "br, gzip")
	if got := gzipped.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip Content-Encoding = %q", got)
	}
	if got := gzipped.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("gzip Cache-Control = %q", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzipped.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	_ = zr.Close()
	if len(raw) == 0 {
		t.Fatal("decompressed asset is empty")
	}

	rangeReq := requestWithHeader(t, handler, "/console/"+asset, "Range", "bytes=0-20")
	if got := rangeReq.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("range Content-Encoding = %q", got)
	}
}

func TestConsoleSourcesDoNotUseHTTPError(t *testing.T) {
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
			t.Errorf("%s uses plain-text http.Error; use the console HTML fallback instead", fset.Position(sel.Pos()))
			return true
		})
	}
}

func request(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	return requestWithHeader(t, handler, path, "", "")
}

func requestWithHeader(t *testing.T, handler http.Handler, path, key, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		t.Fatalf("GET %s status = %s", path, rec.Result().Status)
	}
	return rec
}

func firstEmbeddedAsset(t *testing.T) string {
	t.Helper()
	matches, err := fs.Glob(distFS, "dist/assets/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if strings.HasSuffix(match, ".js") || strings.HasSuffix(match, ".css") {
			return strings.TrimPrefix(match, "dist/")
		}
	}
	t.Fatal("no embedded js/css asset found")
	return ""
}
