package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestSpoolBodyReplaysIdenticallyAcrossAttempts(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.6-sol","input":"` + strings.Repeat("payload-", 256<<10) + `"}`)
	want := sha256.Sum256(payload)
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read attempt: %v", err)
		}
		if got := sha256.Sum256(body); got != want {
			t.Errorf("attempt %d hash=%x want=%x", attempts, got, want)
		}
		attempts++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	source, err := bodysource.Capture(context.Background(), strings.NewReader(string(payload)), bodysource.CaptureOptions{MaxBytes: int64(len(payload)) + 1, MemoryThreshold: 1, TempDir: t.TempDir(), Budget: bodysource.NewBudget(1, int64(len(payload))+1)})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL
	cfg.OpenAIAPIUpstreamBaseURL = server.URL
	client := NewClient(cfg)
	req := Request{Method: http.MethodPost, DownstreamPath: "/v1/probe", Body: source, Token: storage.AccountToken{OpenAIAPIKey: "test"}, MinimalProbe: true}
	for i := 0; i < 2; i++ {
		resp, err := client.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d", attempts)
	}
}

func TestDoRawSourceReplaysSpoolWithoutMaterializing(t *testing.T) {
	payload := []byte(strings.Repeat("kiro-native-body-", 256<<10))
	want := sha256.Sum256(payload)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		if sum := sha256.Sum256(got); sum != want {
			t.Errorf("body hash=%x want=%x", sum, want)
		}
		if r.ContentLength != int64(len(payload)) {
			t.Errorf("content length=%d want=%d", r.ContentLength, len(payload))
		}
		attempts.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	source, err := bodysource.Capture(context.Background(), bytes.NewReader(payload), bodysource.CaptureOptions{
		MaxBytes: int64(len(payload)) + 1, MemoryThreshold: 1, TempDir: t.TempDir(), Budget: bodysource.NewBudget(1, int64(len(payload))+1),
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingBodySource{BodySource: source}
	defer counted.Close()
	client := NewClient(config.Default())
	for i := 0; i < 2; i++ {
		resp, err := client.DoRawSource(context.Background(), storage.EgressProfile{Type: "direct"}, http.MethodPost, server.URL, http.Header{}, counted, "")
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts=%d want=2", got)
	}
	if got := counted.opens.Load(); got != 2 {
		t.Fatalf("body opens=%d want=2", got)
	}
}

func TestClaudePassthroughOpensBodyOnlyForNetworkSend(t *testing.T) {
	payload := []byte(strings.Repeat("opaque-upload-", 1<<16))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upload: %v", err)
		}
		if string(body) != string(payload) {
			t.Errorf("upload body mismatch: got=%d want=%d", len(body), len(payload))
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	source := &countingBodySource{BodySource: bodysource.Bytes(payload)}
	defer source.Close()
	cfg := config.Default()
	cfg.ClaudeUpstreamBaseURL = server.URL
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		PassThrough:    true,
		DownstreamPath: "/v1/files",
		Headers:        http.Header{"Content-Type": []string{"application/octet-stream"}},
		Body:           source,
		Token:          storage.AccountToken{OpenAIAPIKey: "sk-ant-api03-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := source.opens.Load(); got != 1 {
		t.Fatalf("body opens=%d want=1; passthrough was materialized before send", got)
	}
}

func TestCodexSourceNormalizationKeepsLargeInputOutOfHeapCopies(t *testing.T) {
	large := strings.Repeat("large-context-", 640<<10)
	payload := []byte(`{"model":"gpt-5","stream":true,"reasoning":{"effort":"ultra","summary":"auto"},"instructions":"","store":true,"prompt_cache_retention":"24h","thread_id":"downstream","max_output_tokens":123,"input":"` + large + `"}`)
	serverBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		serverBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	source, meta, err := bodysource.CaptureJSON(context.Background(), strings.NewReader(string(payload)), bodysource.CaptureOptions{MaxBytes: int64(len(payload)) + 1, MemoryThreshold: 1, TempDir: t.TempDir(), Budget: bodysource.NewBudget(1, int64(len(payload))+1)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	counted := &countingBodySource{BodySource: source}
	defer counted.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL
	cfg.OpenAIAPIUpstreamBaseURL = server.URL
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{Method: http.MethodPost, DownstreamPath: "/v1/responses", Body: counted, BodyMeta: &meta, Token: storage.AccountToken{OpenAIAPIKey: "sk-test"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	got := <-serverBody
	if !bytes.Contains(got, []byte(`"input":"`+large+`"`)) {
		t.Fatalf("large input bytes changed")
	}
	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := root["reasoning"].(map[string]any)
	if root["instructions"] != "You are a coding agent." || root["store"] != false || reasoning["effort"] != "max" || root["max_output_tokens"] != float64(123) {
		t.Fatalf("normalization mismatch: instructions=%v store=%v reasoning=%v max=%v", root["instructions"], root["store"], reasoning, root["max_output_tokens"])
	}
	if _, ok := root["prompt_cache_retention"]; ok {
		t.Fatalf("prompt_cache_retention was not stripped")
	}
	if _, ok := root["thread_id"]; ok {
		t.Fatalf("thread_id was not stripped")
	}
	if read := counted.bytes.Load(); read > int64(len(payload))+(64<<10) {
		t.Fatalf("source bytes read=%d body=%d; normalization replayed the large input", read, len(payload))
	}
}

type countingBodySource struct {
	bodysource.BodySource
	opens atomic.Int32
	bytes atomic.Int64
}

func (s *countingBodySource) Open() (io.ReadCloser, error) {
	s.opens.Add(1)
	r, err := s.BodySource.Open()
	if err != nil {
		return nil, err
	}
	if seeker, ok := r.(io.Seeker); ok {
		return &countingReadSeekCloser{ReadCloser: r, seeker: seeker, bytes: &s.bytes}, nil
	}
	return &countingReadCloser{ReadCloser: r, bytes: &s.bytes}, nil
}

type countingReadCloser struct {
	io.ReadCloser
	bytes *atomic.Int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

type countingReadSeekCloser struct {
	io.ReadCloser
	seeker io.Seeker
	bytes  *atomic.Int64
}

func (r *countingReadSeekCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

func (r *countingReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	return r.seeker.Seek(offset, whence)
}
