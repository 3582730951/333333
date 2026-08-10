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
	"testing/fstest"

	"github.com/andybalholm/brotli"
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

func TestHandlerServesPrecompressedAssets(t *testing.T) {
	handler := Handler()
	asset := firstEmbeddedAsset(t)

	plain := request(t, handler, "/console/"+asset)
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain Content-Encoding = %q", got)
	}

	compressed := requestWithHeader(t, handler, "/console/"+asset, "Accept-Encoding", "br, gzip")
	encoding := compressed.Header().Get("Content-Encoding")
	if encoding != "br" && encoding != "gzip" {
		t.Fatalf("compressed Content-Encoding = %q", encoding)
	}
	if got := compressed.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("compressed Cache-Control = %q", got)
	}
	var reader io.Reader
	var closeReader io.Closer
	if encoding == "br" {
		reader = brotli.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	} else {
		zr, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		reader, closeReader = zr, zr
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if closeReader != nil {
		_ = closeReader.Close()
	}
	if len(raw) == 0 {
		t.Fatal("decompressed asset is empty")
	}

	rangeReq := requestWithHeader(t, handler, "/console/"+asset, "Range", "bytes=0-20")
	if got := rangeReq.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("range Content-Encoding = %q", got)
	}

	gzipOnly := requestWithHeader(t, handler, "/console/"+asset, "Accept-Encoding", "br;q=0, gzip")
	if got := gzipOnly.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("gzip fallback Content-Encoding = %q", got)
	}
}

func TestBuildCompressedAssetsPrefersBuildTimeBrotli(t *testing.T) {
	assets := fstest.MapFS{
		"assets/app.js":    {Data: bytes.Repeat([]byte("const value = 1;\n"), 100)},
		"assets/app.js.br": {Data: []byte("precompressed-brotli")},
	}
	compressed := buildCompressedAssets(assets)
	variants, ok := compressed["assets/app.js"]
	if !ok || variants.brotli == nil || variants.brotli.encoding != "br" || string(variants.brotli.body) != "precompressed-brotli" || variants.gzip == nil {
		t.Fatalf("compressed variants=%+v ok=%v", variants, ok)
	}
}

func TestHandlerReturnsNotFoundForMissingImmutableAsset(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/console/assets/index-missing.js", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, want %d; body=%q", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	if strings.Contains(strings.ToLower(rec.Header().Get("Content-Type")), "text/html") {
		t.Fatalf("missing asset was disguised as HTML: %#v", rec.Header())
	}
}

func TestValidateConsoleIndexAssets(t *testing.T) {
	assets := fstest.MapFS{
		"assets/index-ok.js":  {Data: []byte(`console.log("ok")`)},
		"assets/index-ok.css": {Data: []byte(`body { color: black; }`)},
	}
	good := []byte(`<script type="module" src="/console/assets/index-ok.js"></script><link rel="stylesheet" href="/console/assets/index-ok.css">`)
	if err := validateConsoleIndexAssets(assets, good); err != nil {
		t.Fatalf("complete index rejected: %v", err)
	}
	missing := []byte(`<script type="module" src="/console/assets/index-missing.js"></script>`)
	if err := validateConsoleIndexAssets(assets, missing); err == nil {
		t.Fatal("index with missing asset was accepted")
	}
	unsafe := []byte(`<script type="module" src="/console/assets/../index-ok.js"></script>`)
	if err := validateConsoleIndexAssets(assets, unsafe); err == nil {
		t.Fatal("index with unsafe asset path was accepted")
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
