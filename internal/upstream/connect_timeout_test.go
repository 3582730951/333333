package upstream

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestHTTPProxyConnectResponseIsBoundedIndependentlyFromRequestTTFT(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		// Deliberately never send the CONNECT response. The connection deadline
		// must release the request without applying a response-header timeout to
		// ordinary inference calls.
		<-time.After(3 * time.Second)
	}()

	cfg := config.Default()
	cfg.ConnectTimeoutSeconds = 1
	cfg.RequestTimeoutSeconds = 10
	client := NewClient(cfg)
	started := time.Now()
	_, err = client.DoRaw(context.Background(), storage.EgressProfile{ID: "proxy", Type: "http_proxy", Endpoint: "http://" + listener.Addr().String()},
		http.MethodGet, "https://upstream.invalid/v1/models", http.Header{}, nil, "")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("stalled proxy CONNECT unexpectedly succeeded")
	}
	if elapsed < 800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("CONNECT timeout elapsed=%s, want connection-stage bound", elapsed)
	}
	listener.Close()
	<-done
}

func TestConnectionTimeoutDoesNotCapCleartextProxyResponseLatency(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.String(), "http://slow-upstream.invalid/") {
			t.Errorf("proxy target=%q", r.URL.String())
		}
		time.Sleep(1200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer proxy.Close()
	cfg := config.Default()
	cfg.ConnectTimeoutSeconds = 1
	cfg.RequestTimeoutSeconds = 5
	client := NewClient(cfg)
	started := time.Now()
	resp, err := client.DoRaw(context.Background(), storage.EgressProfile{ID: "proxy", Type: "http_proxy", Endpoint: proxy.URL},
		http.MethodGet, "http://slow-upstream.invalid/result", http.Header{}, nil, "")
	if err != nil {
		t.Fatalf("slow response was mistaken for connection timeout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || time.Since(started) < time.Second {
		t.Fatalf("unexpected slow response status=%d elapsed=%s", resp.StatusCode, time.Since(started))
	}
}

func TestFirstByteObserverTreatsEmptySuccessfulBodyAsSuccess(t *testing.T) {
	observed := false
	called := 0
	body := &firstByteObservedBody{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		started:    time.Now(),
		status:     http.StatusNoContent,
		observe: func(_ time.Duration, success bool) {
			called++
			observed = success
		},
	}
	if _, err := body.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("empty response read err=%v, want EOF", err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if called != 1 || !observed {
		t.Fatalf("empty successful response observation called=%d success=%v", called, observed)
	}
}

func TestInProcessHTTPProxyConnectUsesConnectionStageTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil || line == "\r\n" {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}()

	cfg := config.Default()
	cfg.EgressFingerprintEngine = "inprocess"
	cfg.ConnectTimeoutSeconds = 1
	cfg.RequestTimeoutSeconds = 10
	client := NewClient(cfg)
	started := time.Now()
	_, err = client.DoRaw(context.Background(), storage.EgressProfile{
		ID: "inprocess-proxy", Type: storage.CurlCFFISidecarEgressType,
		ChainProxy: "http://" + listener.Addr().String(),
	}, http.MethodGet, "https://upstream.invalid/v1/models", http.Header{}, nil, "")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("stalled in-process proxy CONNECT unexpectedly succeeded")
	}
	if elapsed < 800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("in-process CONNECT timeout elapsed=%s", elapsed)
	}
	listener.Close()
	<-done
}

func TestInProcessTLSHandshakeUsesConnectionStageTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		time.Sleep(3 * time.Second)
	}()

	cfg := config.Default()
	cfg.EgressFingerprintEngine = "inprocess"
	cfg.ConnectTimeoutSeconds = 1
	cfg.RequestTimeoutSeconds = 10
	client := NewClient(cfg)
	started := time.Now()
	_, err = client.DoRaw(context.Background(), storage.EgressProfile{
		ID: "inprocess-direct", Type: storage.CurlCFFISidecarEgressType,
	}, http.MethodGet, "https://"+listener.Addr().String()+"/v1/models", http.Header{}, nil, "")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("stalled in-process TLS handshake unexpectedly succeeded")
	}
	if elapsed < 800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("in-process TLS timeout elapsed=%s", elapsed)
	}
	listener.Close()
	<-done
}

func TestInProcessConnectionTimeoutDoesNotCapResponseStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: done\n\n")
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.EgressFingerprintEngine = "inprocess"
	cfg.ConnectTimeoutSeconds = 1
	cfg.RequestTimeoutSeconds = 5
	client := NewClient(cfg)
	started := time.Now()
	resp, err := client.DoRaw(context.Background(), storage.EgressProfile{
		ID: "inprocess-direct", Type: storage.CurlCFFISidecarEgressType,
	}, http.MethodGet, server.URL+"/stream", http.Header{}, nil, "")
	if err != nil {
		t.Fatalf("in-process response headers: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream was truncated by connection timeout: %v", err)
	}
	if string(body) != "data: done\n\n" || time.Since(started) < time.Second {
		t.Fatalf("unexpected stream body=%q elapsed=%s", body, time.Since(started))
	}
}
