package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDeploymentHandlerReadinessAndInflight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := newDeploymentHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		_, _ = io.WriteString(w, "done")
	}), "release-test", "/tmp/worker-test.sock")
	h.markActive(1)

	normalDone := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(normalDone)
	}()
	<-entered
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("ready status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var status map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["release_id"] != "release-test" || status["deployment_state"] != "active" || status["inflight"] != float64(1) {
		t.Fatalf("unexpected readiness: %#v", status)
	}
	capabilities, _ := status["capabilities"].(map[string]interface{})
	if capabilities["idle_websocket_drain_signal"] != "SIGUSR1" {
		t.Fatalf("readiness omitted turn-safe WebSocket drain capability: %#v", status)
	}
	close(release)
	<-normalDone
	h.ready.Store(false)
	h.draining.Store(true)
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"deployment_state":"draining"`) ||
		!strings.Contains(recorder.Body.String(), `"ready":false`) {
		t.Fatalf("draining readiness = %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/standbyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"deployment_state":"draining"`) ||
		!strings.Contains(recorder.Body.String(), `"standby_ready":false`) {
		t.Fatalf("draining standby readiness = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeploymentHandlerStandbyIsObservableButRejectsTraffic(t *testing.T) {
	h := newDeploymentHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("standby request reached application handler")
	}), "release-standby", "/tmp/worker-standby.sock")
	h.standbyReady.Store(true)

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/standbyz", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"standby_ready":true`) ||
		!strings.Contains(recorder.Body.String(), `"deployment_state":"standby_ready"`) {
		t.Fatalf("standby readiness = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), `"code":"service_unavailable"`) ||
		strings.Contains(recorder.Body.String(), "worker-standby.sock") {
		t.Fatalf("standby traffic response = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Retry-After") != "3" || !strings.HasPrefix(recorder.Header().Get("X-Request-ID"), "REQ-") {
		t.Fatalf("standby traffic headers = %#v", recorder.Header())
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"deployment_state":"standby_ready"`) {
		t.Fatalf("active readiness while standby = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestPreinitStandbyExposesOnlyDeploymentContract(t *testing.T) {
	h := &preinitStandbyHandler{
		releaseID:  "release-preinit",
		workerAddr: "/run/codex-pool/worker-release-preinit.sock",
		startedAt:  time.Unix(123, 0).UTC(),
	}

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/standbyz", nil))
	if recorder.Code != http.StatusOK ||
		!strings.Contains(recorder.Body.String(), `"standby_ready":true`) ||
		!strings.Contains(recorder.Body.String(), `"deployment_state":"preinit_standby"`) ||
		!strings.Contains(recorder.Body.String(), `"storage":false`) ||
		!strings.Contains(recorder.Body.String(), `"preinit_promotion":"exec_on_active_link"`) {
		t.Fatalf("preinit standby = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), `"ready":false`) {
		t.Fatalf("preinit ready = %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != http.StatusServiceUnavailable ||
		!strings.Contains(recorder.Body.String(), `"code":"service_unavailable"`) {
		t.Fatalf("preinit traffic = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeploymentHandlerAllowsOnlyEmergencyDiagnosticOnStandby(t *testing.T) {
	var reached int
	h := newDeploymentHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		_, _ = io.WriteString(w, r.URL.Query().Get("mode"))
	}), "release-standby", "/tmp/worker-standby.sock")
	h.standbyReady.Store(true)

	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/export/logs?mode=rescue", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "rescue" || reached != 1 {
		t.Fatalf("standby rescue = status %d body %q reached %d", recorder.Code, recorder.Body.String(), reached)
	}

	for _, target := range []string{
		"/admin/export/logs",
		"/admin/export/logs?mode=other",
		"/admin/diagnostics/jobs",
	} {
		recorder = httptest.NewRecorder()
		h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("standby non-emergency target %q status=%d", target, recorder.Code)
		}
	}
	if reached != 1 {
		t.Fatalf("non-emergency standby requests reached application: %d", reached)
	}
}

func TestIsLoopbackBindHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1": true,
		"127.0.0.5": true,
		"localhost": true,
		"::1":       true,
		"[::1]":     true,
		"LOCALHOST": true,
		"0.0.0.0":   false, // all interfaces → publicly reachable
		"::":        false,
		"":          false, // empty host = all interfaces
		"10.0.0.4":  false,
		"1.2.3.4":   false,
	}
	for host, want := range cases {
		if got := isLoopbackBindHost(host); got != want {
			t.Errorf("isLoopbackBindHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLooksLikeWeakToken(t *testing.T) {
	weak := []string{
		"",                         // empty
		"short",                    // < 24
		"testadmin_token_local",    // contains test/local
		"changeme-please-now-ok-1", // contains changeme
		"my-dev-secret-token-here", // contains dev/secret
	}
	for _, tok := range weak {
		if !looksLikeWeakToken(tok) {
			t.Errorf("looksLikeWeakToken(%q) = false, want true (should be flagged weak)", tok)
		}
	}
	strong := []string{
		"Z9f3Q7pX1kR4nL8wB2vH6cJ0mD5sT", // 29 random-looking chars, no giveaway substring
		"a1B2c3D4e5F6g7H8i9J0k1L2m3N4",
	}
	for _, tok := range strong {
		if looksLikeWeakToken(tok) {
			t.Errorf("looksLikeWeakToken(%q) = true, want false (should pass)", tok)
		}
	}
}

func TestServeHTTPServerReturnsListenError(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen test socket: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{Addr: ln.Addr().String()}
	if err := serveHTTPServer(srv); err == nil {
		t.Fatal("serveHTTPServer returned nil for an occupied listen address")
	}
}

func TestListenerFromSystemdFileClosesOriginalAndKeepsListenerUsable(t *testing.T) {
	source, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen source socket: %v", err)
	}
	tcpSource, ok := source.(*net.TCPListener)
	if !ok {
		source.Close()
		t.Fatalf("source listener type = %T, want *net.TCPListener", source)
	}
	activationFile, err := tcpSource.File()
	if err != nil {
		source.Close()
		t.Fatalf("duplicate activation socket: %v", err)
	}
	addr := source.Addr().String()
	if err := source.Close(); err != nil {
		activationFile.Close()
		t.Fatalf("close source listener: %v", err)
	}

	activated, ok := listenerFromSystemdFile(activationFile)
	if !ok {
		t.Fatal("listenerFromSystemdFile rejected a valid TCP socket")
	}
	defer activated.Close()
	if _, err := activationFile.Stat(); err == nil {
		t.Fatal("inherited activation descriptor remains open")
	}

	accepted := make(chan error, 1)
	go func() {
		conn, err := activated.Accept()
		if err == nil {
			err = conn.Close()
		}
		accepted <- err
	}()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial duplicated activation listener: %v", err)
	}
	_ = conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("accept on duplicated activation listener: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicated activation listener did not accept a connection")
	}
}

func TestCleanServeErrorTreatsServerClosedAsClean(t *testing.T) {
	if err := cleanServeError(http.ErrServerClosed); err != nil {
		t.Fatalf("cleanServeError(http.ErrServerClosed) = %v, want nil", err)
	}
}

func TestServeHTTPServerAsyncReportsPanic(t *testing.T) {
	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogWriter)

	serveErr := make(chan error, 1)
	serveHTTPServerAsync(serveErr, func() error {
		panic("listen boom")
	})

	select {
	case err := <-serveErr:
		if err == nil || !strings.Contains(err.Error(), "listen boom") {
			t.Fatalf("serve error = %v, want panic detail", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve panic was not reported")
	}

	gotLog := logs.String()
	if !strings.Contains(gotLog, "module=http-server") || !strings.Contains(gotLog, "panic=listen boom") {
		t.Fatalf("panic log missing module or panic value: %s", gotLog)
	}
}
