package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// TestSidecarLowercaseHeadersAreCanonicalized is the regression guard for the SSE
// "one huge block instead of token-by-token" bug. A curl_cffi sidecar fronting an
// HTTP/2 upstream (chatgpt.com / api.anthropic.com) forwards LOWERCASE header names
// ("content-type"). If the relay keeps that casing, http.Header.Get("Content-Type")
// misses, isEventStream() returns false, and the whole SSE response is buffered via
// io.ReadAll and delivered as one block. The relay must canonicalize the keys so Get
// works and streaming is detected.
func TestSidecarLowercaseHeadersAreCanonicalized(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Lowercase keys, exactly as an HTTP/2 upstream + the sidecar forward them.
		enc, _ := json.Marshal(map[string][]string{
			"content-type":                   {"text/event-stream; charset=utf-8"},
			"x-ratelimit-remaining-requests": {"42"},
		})
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(enc))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer sidecar.Close()

	client := NewClient(config.Default())
	resp, err := client.DoViaSidecar(
		context.Background(),
		storage.EgressProfile{ID: "eg1", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
		http.MethodPost,
		"https://chatgpt.com/backend-api/codex/responses",
		http.Header{},
		[]byte("{}"),
		"jar",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(got), "text/event-stream") {
		t.Fatalf("Content-Type via canonical Get = %q; want text/event-stream (streaming-detection bug)", got)
	}
	if got := resp.Header.Get("X-Ratelimit-Remaining-Requests"); got != "42" {
		t.Fatalf("rate-limit header not canonicalized: %q", got)
	}
}

func TestCanonicalizeHeaders(t *testing.T) {
	canon := canonicalizeHeaders(map[string][]string{
		"content-type":      {"text/event-stream"},
		"anthropic-version": {"2023-06-01"},
	})
	if canon.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q", canon.Get("Content-Type"))
	}
	if canon.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("Anthropic-Version = %q", canon.Get("Anthropic-Version"))
	}
}
