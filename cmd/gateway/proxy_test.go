package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTLSResponseWriterWritesCompleteResponse(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSResponseWriter(&buf)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Gateway-Test", "ok")
	w.WriteHeader(http.StatusAccepted)
	if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}

	resp := readGatewayResponse(t, buf.Bytes())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := resp.Header.Get("X-Gateway-Test"); got != "ok" {
		t.Fatalf("x-gateway-test = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
}

func TestTLSResponseWriterImplicitOK(t *testing.T) {
	var buf bytes.Buffer
	w := newTLSResponseWriter(&buf)
	w.Header().Set("Content-Type", "text/plain")
	if _, err := w.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}

	resp := readGatewayResponse(t, buf.Bytes())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("content-type = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "ready" {
		t.Fatalf("body = %q", got)
	}
}

func TestWriteGatewayErrorUsesParseableResponse(t *testing.T) {
	var buf bytes.Buffer
	writeGatewayError(&buf, http.StatusBadGateway, "Pool unavailable")

	resp := readGatewayResponse(t, buf.Bytes())
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("x-content-type-options = %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "Pool unavailable\n" {
		t.Fatalf("body = %q", got)
	}
}

func TestNewCAManagerReturnsErrorInsteadOfPanicking(t *testing.T) {
	certPath, keyPath := blockedCAPaths(t)
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("NewCAManager panicked: %v", v)
		}
	}()

	mgr, err := NewCAManager(certPath, keyPath)
	if err == nil {
		t.Fatal("expected CA init error")
	}
	if mgr != nil {
		t.Fatal("manager must be nil on init error")
	}
}

func TestNewProxyReturnsInitErrorInsteadOfPanicking(t *testing.T) {
	certPath, keyPath := blockedCAPaths(t)
	cfg := DefaultConfig()
	cfg.MITM.CACert = certPath
	cfg.MITM.CAKey = keyPath
	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("NewProxy panicked: %v", v)
		}
	}()

	proxy, err := NewProxy(cfg)
	if err == nil {
		t.Fatal("expected proxy init error")
	}
	if proxy != nil {
		t.Fatal("proxy must be nil on init error")
	}
}

func TestNewProxyInitializesSharedPoolClient(t *testing.T) {
	tmp := t.TempDir()
	cfg := DefaultConfig()
	cfg.MITM.CACert = filepath.Join(tmp, "ca.pem")
	cfg.MITM.CAKey = filepath.Join(tmp, "ca-key.pem")

	proxy, err := NewProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if proxy.poolClient == nil {
		t.Fatal("pool client is nil")
	}
	if _, ok := proxy.poolClient.Transport.(*http.Transport); !ok {
		t.Fatalf("pool client transport = %T", proxy.poolClient.Transport)
	}
}

func TestSaveConfigUsesPrivatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.DownstreamKey = "cap_secret"

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}

	assertFileMode(t, dir, gatewayPrivateDirMode)
	assertFileMode(t, path, gatewayConfigFileMode)
}

func TestLoadConfigHardensLegacyWidePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	path := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"downstream_key":"cap_legacy"}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DownstreamKey != "cap_legacy" {
		t.Fatalf("downstream key = %q", cfg.DownstreamKey)
	}
	assertFileMode(t, dir, gatewayPrivateDirMode)
	assertFileMode(t, path, gatewayConfigFileMode)
}

func TestConfigIdentityTTLUsesSecondsOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.IdentityTTL = 5 * time.Minute

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got := raw["identity_ttl_seconds"]; got != float64(300) {
		t.Fatalf("identity_ttl_seconds = %#v, want 300", got)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IdentityTTL != 5*time.Minute {
		t.Fatalf("loaded ttl = %s, want 5m", loaded.IdentityTTL)
	}
}

func TestLoadConfigTreatsSmallTTLValuesAsSeconds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	path := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"identity_ttl_seconds":300}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdentityTTL != 5*time.Minute {
		t.Fatalf("ttl = %s, want 5m", cfg.IdentityTTL)
	}
}

func TestLoadConfigKeepsLegacyDurationTTLValues(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	path := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"identity_ttl_seconds":300000000000}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdentityTTL != 5*time.Minute {
		t.Fatalf("ttl = %s, want 5m", cfg.IdentityTTL)
	}
}

func TestNewCAManagerUsesPrivateKeyAndDirectoryPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gateway")
	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	if _, err := NewCAManager(certPath, keyPath); err != nil {
		t.Fatal(err)
	}

	assertFileMode(t, dir, gatewayPrivateDirMode)
	assertFileMode(t, certPath, gatewayPublicCertMode)
	assertFileMode(t, keyPath, gatewayPrivateFileMode)
}

func TestIdentityCacheFetchUsesAuthorizationHeader(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.URL.Query().Get("downstream_key"); got != "" {
			t.Fatalf("downstream_key leaked into URL query: %q", got)
		}
		if got := r.URL.Query().Get("provider"); got != "claude" {
			t.Fatalf("provider = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cap_secret" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"sid-1","user_id":"user-1"}`)
	}))
	defer srv.Close()

	cache := NewIdentityCache(srv.URL, "cap_secret", time.Minute, srv.Client())
	first, err := cache.Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	if first.Virtual.SessionID != "sid-1" {
		t.Fatalf("session = %q", first.Virtual.SessionID)
	}
	second, err := cache.Get("claude")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("expected cached identity on second get")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestIdentityCacheConcurrentGetIsSafe(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := r.URL.Query().Get("provider")
		mu.Lock()
		requests[provider]++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"session_id":"sid-concurrent","user_id":"user-concurrent"}`)
	}))
	defer srv.Close()

	cache := NewIdentityCache(srv.URL, "cap_secret", time.Minute, srv.Client())
	var wg sync.WaitGroup
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			provider := "claude"
			if i%3 == 0 {
				provider = "codex"
			}
			identity, err := cache.Get(provider)
			if err != nil {
				t.Errorf("get identity: %v", err)
				return
			}
			if identity.Virtual.SessionID == "" {
				t.Error("empty session id")
			}
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if got := requests["claude"]; got != 1 {
		t.Fatalf("claude upstream requests = %d, want 1", got)
	}
	if got := requests["codex"]; got != 1 {
		t.Fatalf("codex upstream requests = %d, want 1", got)
	}
}

func TestIdentityCacheLimitsErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", identityErrorBodyLimit*4))
	}))
	defer srv.Close()

	cache := NewIdentityCache(srv.URL, "cap_secret", time.Minute, srv.Client())
	_, err := cache.Get("claude")
	if err == nil {
		t.Fatal("Get returned nil error for bad upstream status")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pool returned 502") {
		t.Fatalf("error = %q, want status context", msg)
	}
	if strings.Count(msg, "x") != identityErrorBodyLimit {
		t.Fatalf("error body length = %d, want %d", strings.Count(msg, "x"), identityErrorBodyLimit)
	}
}

func TestReadRewriteBodyRejectsOversizedBody(t *testing.T) {
	_, err := readRewriteBody(io.LimitReader(repeatingByteReader('x'), gatewayRewriteBodyLimit+1))
	if err == nil {
		t.Fatal("readRewriteBody returned nil error for oversized body")
	}
	if !strings.Contains(err.Error(), "gateway request body too large") {
		t.Fatalf("error = %q, want body size context", err)
	}
}

func TestInspectGatewayStatusReportsConfiguredServices(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("pool health path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer pool.Close()

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.ListenAddr = ln.Addr().String()
	cfg.PoolServerURL = pool.URL
	cfg.DownstreamKey = "cap_secret"
	cfg.MITM.CACert = filepath.Join(dir, "ca-cert.pem")
	cfg.MITM.CAKey = filepath.Join(dir, "ca-key.pem")
	path := filepath.Join(dir, "config.json")

	if err := SaveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.MITM.CACert, []byte("cert"), gatewayPublicCertMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.MITM.CAKey, []byte("key"), gatewayPrivateFileMode); err != nil {
		t.Fatal(err)
	}

	report := inspectGatewayStatus(context.Background(), path)
	if !report.ConfigLoaded {
		t.Fatalf("config not loaded: %s", report.ConfigError)
	}
	if !report.DownstreamKeyConfigured {
		t.Fatal("downstream key should be reported as configured")
	}
	if !report.CACertPresent || !report.CAKeyPresent {
		t.Fatalf("ca presence = cert:%v key:%v, want both true", report.CACertPresent, report.CAKeyPresent)
	}
	if !report.GatewayReachable {
		t.Fatalf("gateway should be reachable: %s", report.GatewayError)
	}
	if !report.PoolReachable || report.PoolStatus != http.StatusOK {
		t.Fatalf("pool health = reachable:%v status:%d error:%s", report.PoolReachable, report.PoolStatus, report.PoolError)
	}
}

func TestInspectGatewayStatusReportsMissingConfig(t *testing.T) {
	report := inspectGatewayStatus(context.Background(), filepath.Join(t.TempDir(), "missing.json"))
	if report.ConfigLoaded {
		t.Fatal("missing config should not report loaded")
	}
	if report.ConfigError == "" {
		t.Fatal("missing config should include an error")
	}
}

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & os.ModePerm; got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func blockedCAPaths(t *testing.T) (string, string) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "ca.pem"), filepath.Join(blocker, "ca-key.pem")
}

func readGatewayResponse(t *testing.T, raw []byte) *http.Response {
	t.Helper()
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("parse response %q: %v", string(raw), err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

type repeatingByteReader byte

func (r repeatingByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestGatewayProductionCodeBoundsReadAllCalls(t *testing.T) {
	fileSet := token.NewFileSet()
	if err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !isSelectorCall(call, "io", "ReadAll") {
				return true
			}
			if len(call.Args) != 1 {
				t.Errorf("%s uses io.ReadAll without io.LimitReader", fileSet.Position(call.Pos()))
				return true
			}
			limitCall, ok := call.Args[0].(*ast.CallExpr)
			if !ok || !isSelectorCall(limitCall, "io", "LimitReader") {
				t.Errorf("%s uses io.ReadAll without io.LimitReader", fileSet.Position(call.Pos()))
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func isSelectorCall(call *ast.CallExpr, pkg, name string) bool {
	return isSelectorCallExpr(call.Fun, pkg, name)
}

func isSelectorCallExpr(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func TestGatewayProductionCodeAvoidsPanicAndHTTPError(t *testing.T) {
	fileSet := token.NewFileSet()
	if err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(source), "downstream_key=") {
			t.Errorf("%s contains downstream_key=; gateway production code must not put secrets in URLs", path)
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
			ident, ok := sel.X.(*ast.Ident)
			if ok && ident.Name == "http" {
				t.Errorf("%s uses http.Error; gateway responses must use writeGatewayError/newTLSResponseWriter", fileSet.Position(sel.Pos()))
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if ok && ident.Name == "panic" {
				t.Errorf("%s uses panic; gateway initialization and request handling must return errors instead", fileSet.Position(ident.Pos()))
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
