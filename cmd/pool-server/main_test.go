package main

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

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
