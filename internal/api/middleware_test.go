package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"codex-account-pool/internal/supervisor"
)

func TestServeHTTPAddsAndPreservesRequestID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	resp, err := http.Get(h.pool.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get(requestIDHeader); got == "" {
		t.Fatalf("missing generated %s", requestIDHeader)
	}

	req, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/healthz", nil)
	req.Header.Set(requestIDHeader, "req-test.123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get(requestIDHeader); got != "req-test.123" {
		t.Fatalf("request id not preserved: %q", got)
	}
}

func TestServeHTTPAddsSecurityHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	resp, err := http.Get(h.pool.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("API CSP = %q, want empty", got)
	}

	resp, err = http.Get(h.pool.URL + "/console/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") || !strings.Contains(got, "frame-ancestors 'self'") {
		t.Fatalf("console CSP = %q", got)
	}

	resp, err = http.Get(h.pool.URL + "/legacy/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Fatalf("legacy CSP = %q, want empty", got)
	}
}

func TestServeHTTPRecoversPanic(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h.app.mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	req, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/panic", nil)
	req.Header.Set(requestIDHeader, "panic-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := resp.Header.Get(requestIDHeader); got != "panic-test" {
		t.Fatalf("request id header = %q", got)
	}
	var body map[string]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	errBody := body["error"]
	if errBody["type"] != "codex_pool_panic" || errBody["request_id"] != "panic-test" {
		t.Fatalf("unexpected panic body: %#v", body)
	}
	if errBody["message"] == "boom" {
		t.Fatalf("panic detail leaked to client: %#v", body)
	}

	events := supervisor.RecentEvents()
	if len(events) == 0 {
		t.Fatal("supervisor events are empty, want latest http request panic")
	}
	if events[0].Module != "http-request" || events[0].Type != "panic" ||
		!strings.Contains(events[0].Panic, "request_id=panic-test") ||
		!strings.Contains(events[0].Panic, "panic=boom") {
		t.Fatalf("latest supervisor event = %#v, want http request panic with request context", events[0])
	}
}

func TestWriteErrorIncludesRequestID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/client/errors", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(requestIDHeader, "bad-json-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body map[string]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if got := body["error"]["request_id"]; got != "bad-json-test" {
		t.Fatalf("request_id = %v, want bad-json-test in error body %#v", got, body)
	}
}

func TestServeHTTPDoesNotWriteAfterHijackPanic(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("/panic-after-hijack", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijack")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
		panic("boom")
	})

	w := newHijackResponseWriter()
	defer w.Close()
	req := httptest.NewRequest(http.MethodGet, "/panic-after-hijack", nil)
	req.Header.Set(requestIDHeader, "hijack-panic-test")

	s.ServeHTTP(w, req)

	if !w.hijacked {
		t.Fatal("handler did not hijack the connection")
	}
	if w.status != 0 {
		t.Fatalf("status written after hijack panic = %d, want none", w.status)
	}
	if len(w.body) != 0 {
		t.Fatalf("body written after hijack panic = %q, want none", string(w.body))
	}
}

func TestClientErrorsEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogWriter)

	resp, err := http.Post(h.pool.URL+"/client/errors", "application/json", strings.NewReader(`{"source":"react","message":"chunk failed\nretry","stack":"at route\nline 2","component_stack":"Keys\nApiKeysTable","path":"/console/keys","asset_signature":"/console/assets/index-old.js\n/console/assets/index-old.css","resource_url":"/console/assets/page-old.js\nnext","user_agent":"test browser"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	gotLog := logs.String()
	for _, want := range []string{
		`fingerprint=`,
		`source="react"`,
		`path="/console/keys"`,
		`asset_signature="/console/assets/index-old.js\\n/console/assets/index-old.css"`,
		`resource_url="/console/assets/page-old.js\\nnext"`,
		`message="chunk failed\\nretry"`,
		`component_stack="Keys\\nApiKeysTable"`,
		`detail="at route\\nline 2"`,
		`ua="test browser"`,
	} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("client error log missing %q: %s", want, gotLog)
		}
	}
	if strings.Contains(gotLog, "chunk failed\nretry") || strings.Contains(gotLog, "at route\nline 2") || strings.Contains(gotLog, "index-old.js\n") || strings.Contains(gotLog, "page-old.js\n") {
		t.Fatalf("client error log contains raw multiline payload: %q", gotLog)
	}
}

func TestClientErrorLogFieldIsSingleLineAndRuneSafe(t *testing.T) {
	got := clientErrorLogField("中中\n\t\x00\x7fok", 4)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated log field is not valid UTF-8: %q", got)
	}
	if strings.ContainsAny(got, "\n\t\x00\x7f") {
		t.Fatalf("log field contains raw control characters: %q", got)
	}
	if got != "中..." {
		t.Fatalf("log field = %q, want rune-safe truncation", got)
	}
}

func TestClientErrorsEndpointRateLimited(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h.app.clientErrors = newClientErrorLimiter(2, time.Minute, 8)

	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogWriter)

	for i := 0; i < 2; i++ {
		resp, err := http.Post(h.pool.URL+"/client/errors", "application/json", strings.NewReader(`{"message":"looping"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status[%d] = %d, want 204", i, resp.StatusCode)
		}
	}

	resp, err := http.Post(h.pool.URL+"/client/errors", "application/json", strings.NewReader(`{"message":"looping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q, want 60", got)
	}
	if got := strings.Count(logs.String(), "[CLIENT-ERROR]"); got != 2 {
		t.Fatalf("logged client errors = %d, want 2; logs=%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "[CLIENT-ERROR-LIMITED]"); got != 1 {
		t.Fatalf("limited client error logs = %d, want 1; logs=%s", got, logs.String())
	}
}

type hijackResponseWriter struct {
	header   http.Header
	status   int
	body     []byte
	hijacked bool
	conn     net.Conn
	peer     net.Conn
}

func newHijackResponseWriter() *hijackResponseWriter {
	return &hijackResponseWriter{header: http.Header{}}
}

func (w *hijackResponseWriter) Header() http.Header {
	return w.header
}

func (w *hijackResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *hijackResponseWriter) Write(p []byte) (int, error) {
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	w.conn, w.peer = net.Pipe()
	rw := bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn))
	return w.conn, rw, nil
}

func (w *hijackResponseWriter) Close() {
	if w.conn != nil {
		_ = w.conn.Close()
	}
	if w.peer != nil {
		_ = w.peer.Close()
	}
}
