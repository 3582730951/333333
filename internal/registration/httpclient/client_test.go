package httpclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSidecarTransportRejectsOversizedRequestBody(t *testing.T) {
	var called atomic.Bool
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer sidecar.Close()

	transport := &SidecarTransport{Endpoint: sidecar.URL, JarKey: "jar"}
	req, err := http.NewRequest(http.MethodPost, "https://example.test/signup", strings.NewReader(strings.Repeat("x", sidecarBodyLimit+1)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected oversized request body error")
	}
	if called.Load() {
		t.Fatal("sidecar was called despite oversized request body")
	}
	if !strings.Contains(err.Error(), "sidecar request body too large") {
		t.Fatalf("error = %q, want request size error", err)
	}
}

func TestSidecarTransportRejectsOversizedResponseBody(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-sidecar-upstream-status", "200")
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", sidecarBodyLimit+1)), sidecarBodyLimit+1))
	}))
	defer sidecar.Close()

	transport := &SidecarTransport{Endpoint: sidecar.URL, JarKey: "jar"}
	req, err := http.NewRequest(http.MethodGet, "https://example.test/signup", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		t.Fatal("expected oversized response body error")
	}
	if !strings.Contains(err.Error(), "sidecar response body too large") {
		t.Fatalf("error = %q, want response size error", err)
	}
}

func TestSidecarTransportReadAllStaysBounded(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "client.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var helperStart, helperEnd token.Pos
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "readSidecarBody" && fn.Body != nil {
			helperStart = fn.Body.Pos()
			helperEnd = fn.Body.End()
			break
		}
	}
	if helperStart == token.NoPos || helperEnd == token.NoPos {
		t.Fatal("readSidecarBody helper not found")
	}

	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "ReadAll" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "io" && (call.Pos() < helperStart || call.Pos() > helperEnd) {
			t.Errorf("%s uses io.ReadAll outside readSidecarBody", fset.Position(call.Pos()))
		}
		return true
	})
}
