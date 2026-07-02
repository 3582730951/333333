package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// usage_sidecar_test.go is the diagnostic + regression guard for the operator report
// that accounts egressing through the curl_cffi sidecar record NO usage / token /
// cache-hit data (while direct-egress accounts do). It drives a request through a mock
// sidecar — the real /proxy protocol (X-Sidecar-Meta + x-sidecar-upstream-* headers,
// auto-decompressed plaintext body) — for BOTH the non-streaming and streaming Codex
// response shapes, then asserts a usage_records row landed and /admin/usage reflects it
// (including cached_tokens). If usage recording were egress-dependent, these fail.

// mockSidecar stands up an httptest server speaking the sidecar /proxy reply protocol:
// it reports a 200 upstream status, carries the upstream headers (incl. Content-Type,
// which drives the relay's streaming-vs-buffered decision) in x-sidecar-upstream-headers-b64,
// and writes body as already-plaintext bytes (the real sidecar auto-decompresses).
func mockSidecar(t *testing.T, upstreamContentType, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy" {
			t.Fatalf("sidecar path = %s", r.URL.Path)
		}
		hdr := http.Header{"Content-Type": []string{upstreamContentType}}
		hraw, _ := json.Marshal(hdr)
		w.Header().Set("x-sidecar-upstream-status", "200")
		w.Header().Set("x-sidecar-upstream-headers-b64", base64.StdEncoding.EncodeToString(hraw))
		w.Header().Set("Content-Type", upstreamContentType)
		_, _ = w.Write([]byte(body))
	}))
}

// bindSidecarEgress points the given account's primary egress at a curl_cffi sidecar
// endpoint, so its upstream calls are replayed via the sidecar /proxy path.
func bindSidecarEgress(t *testing.T, h *testHarness, accountID, endpoint string) {
	t.Helper()
	if err := h.store.UpsertEgressProfile(context.Background(), storage.EgressProfile{
		ID: "sidecar", Name: "sidecar", Type: "curl_cffi_sidecar", Endpoint: endpoint,
		StreamCapable: true, Health: "healthy", MaxConcurrency: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: "sidecar",
	}); err != nil {
		t.Fatal(err)
	}
}

// adminUsageRows GETs /admin/usage (open in the test harness — empty AdminToken) and
// returns the per-account rollup rows.
func adminUsageRows(t *testing.T, h *testHarness) []map[string]interface{} {
	t.Helper()
	resp, err := http.Get(h.pool.URL + "/admin/usage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/admin/usage status %d: %s", resp.StatusCode, raw)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode /admin/usage: %v (%s)", err, raw)
	}
	return rows
}

func sumUsage(rows []map[string]interface{}, field string) float64 {
	var n float64
	for _, r := range rows {
		if v, ok := r[field].(float64); ok {
			n += v
		}
	}
	return n
}

// Non-streaming: a /v1/responses body returned through the sidecar carries top-level
// `usage`; recordUsage(body)→usage.ParseResponse must record it (incl. cached_tokens)
// exactly as on the direct path.
func TestSidecarEgressRecordsUsageNonStreaming(t *testing.T) {
	sc := mockSidecar(t, "application/json",
		`{"id":"resp","model":"gpt-5","usage":{"input_tokens":11,"output_tokens":4,"total_tokens":15,"input_tokens_details":{"cached_tokens":7}}}`)
	defer sc.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("direct upstream must not be called when bound to sidecar")
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	bindSidecarEgress(t, h, acc, sc.URL)

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.app.WaitForAsyncWrites()
	rows := adminUsageRows(t, h)
	if len(rows) == 0 {
		t.Fatalf("sidecar-egress usage NOT recorded: /admin/usage empty")
	}
	if total := sumUsage(rows, "total_tokens"); total != 15 {
		t.Fatalf("total_tokens via sidecar = %v, want 15 (rows=%v)", total, rows)
	}
	if cached := sumUsage(rows, "cached_tokens"); cached != 7 {
		t.Fatalf("cached_tokens (cache-hit) via sidecar = %v, want 7 (rows=%v)", cached, rows)
	}
}

// Streaming: a /v1/responses SSE stream returned through the sidecar carries usage in
// the terminal response.completed frame; streamSSE tees through usage.StreamScanner and
// records it. The relay decides to stream from the (sidecar-reconstructed) upstream
// Content-Type, so this guards that the sidecar path preserves it.
func TestSidecarEgressRecordsUsageStreaming(t *testing.T) {
	sse := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp","model":"gpt-5"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"model":"gpt-5","usage":{"input_tokens":21,"output_tokens":9,"total_tokens":30,"input_tokens_details":{"cached_tokens":5}}}}` + "\n\n" +
		"data: [DONE]\n\n"
	sc := mockSidecar(t, "text/event-stream", sse)
	defer sc.Close()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("direct upstream must not be called when bound to sidecar")
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")
	bindSidecarEgress(t, h, acc, sc.URL)

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	h.app.WaitForAsyncWrites()
	rows := adminUsageRows(t, h)
	if len(rows) == 0 {
		t.Fatalf("sidecar-egress streaming usage NOT recorded: /admin/usage empty")
	}
	if total := sumUsage(rows, "total_tokens"); total != 30 {
		t.Fatalf("streaming total_tokens via sidecar = %v, want 30 (rows=%v)", total, rows)
	}
	if cached := sumUsage(rows, "cached_tokens"); cached != 5 {
		t.Fatalf("streaming cached_tokens via sidecar = %v, want 5 (rows=%v)", cached, rows)
	}
}
