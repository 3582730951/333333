package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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

func TestSidecarV2StreamTrailersExposeStructuredFailure(t *testing.T) {
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc, _ := json.Marshal(map[string][]string{"content-type": {"text/event-stream"}})
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(enc))
		w.Header().Set("Trailer", "X-Sidecar-Stream-Error-Code, X-Sidecar-Stream-Error-Phase, X-Sidecar-Stream-Error-Retryable")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: partial\n\n"))
		w.Header().Set("X-Sidecar-Stream-Error-Code", "sidecar_stream_error")
		w.Header().Set("X-Sidecar-Stream-Error-Phase", "stream")
		w.Header().Set("X-Sidecar-Stream-Error-Retryable", "true")
	}))
	defer sidecar.Close()

	client := NewClient(config.Default())
	resp, err := client.DoViaSidecar(context.Background(), storage.EgressProfile{ID: "eg-v2", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"}, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", http.Header{}, []byte("{}"), "jar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	failure := resp.SidecarStreamFailure()
	if failure == nil || failure.Code != "sidecar_stream_error" || failure.Phase != "stream" || !failure.Retryable {
		t.Fatalf("sidecar v2 trailer failure=%+v trailers=%v", failure, resp.Trailer)
	}
}

func TestCodexSidecarResponsesStripsPromptCacheRetention(t *testing.T) {
	var gotBody string
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		enc, _ := json.Marshal(map[string][]string{"content-type": {"application/json"}})
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(enc))
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	}))
	defer sidecar.Close()

	cfg := sidecarEngineConfig()
	cfg.UpstreamBaseURL = "https://chatgpt.com/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{"Originator": []string{"codex_cli_rs"}},
		Body:           testBody([]byte(`{"model":"gpt-5.5","input":"hi","prompt_cache_retention":"24h"}`)),
		Account:        storage.Account{ID: "acc-sidecar", UpstreamAccountID: "acct-sidecar"},
		Token:          storage.AccountToken{AccessToken: "access-sidecar"},
		Egress:         storage.EgressProfile{ID: "sidecar", Type: "curl_cffi_sidecar", Endpoint: sidecar.URL, Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if strings.Contains(gotBody, "prompt_cache_retention") || strings.Contains(gotBody, "24h") {
		t.Fatalf("sidecar HTTP/SSE body must strip prompt_cache_retention:\n%s", gotBody)
	}
}

func TestCodexHTTPResponsesStripsPromptCacheRetention(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","output_text":"ok"}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = upstream.URL + "/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{"Originator": []string{"codex_cli_rs"}},
		Body:           testBody([]byte(`{"model":"gpt-5.4","input":"hi","prompt_cache_retention":"24h"}`)),
		Account:        storage.Account{ID: "acc-http", UpstreamAccountID: "acct-http"},
		Token:          storage.AccountToken{AccessToken: "access-http"},
		Egress:         storage.EgressProfile{ID: "direct", Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if gotPath != "/backend-api/codex/responses" {
		t.Fatalf("path = %q", gotPath)
	}
	if strings.Contains(gotBody, "prompt_cache_retention") || strings.Contains(gotBody, "24h") {
		t.Fatalf("HTTP/SSE body must strip prompt_cache_retention:\n%s", gotBody)
	}
}
