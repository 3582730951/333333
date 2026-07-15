package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestQuotaCapturedFromUpstreamHeaders drives a real non-streaming gateway
// request whose upstream response carries OpenAI rate-limit headers, then
// asserts the per-account quota snapshot was captured and is served by
// /admin/quota — exercising the captureQuota hook end to end.
func TestQuotaCapturedFromUpstreamHeaders(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-ratelimit-limit-tokens", "10000")
		w.Header().Set("x-ratelimit-remaining-tokens", "2500")
		w.Header().Set("x-ratelimit-reset-tokens", "5m0s")
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "97")
		_, _ = w.Write([]byte(`{"id":"resp-1","model":"gpt","output_text":"hi","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"input_tokens_details":{"cached_tokens":5}}}`))
	})
	acc := h.importAccount(t, "a", "upstream-a", "access-a")

	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.app.WaitForAsyncWrites()

	// /admin/quota must now report a snapshot for the account.
	qr, err := http.Get(h.pool.URL + "/admin/quota")
	if err != nil {
		t.Fatal(err)
	}
	defer qr.Body.Close()
	var quota []map[string]interface{}
	if err := json.NewDecoder(qr.Body).Decode(&quota); err != nil {
		t.Fatal(err)
	}
	var snap map[string]interface{}
	for _, q := range quota {
		if q["account_id"] == acc {
			snap = q
		}
	}
	if snap == nil {
		t.Fatalf("no quota snapshot for account %s in %v", acc, quota)
	}
	if snap["source"] != "tokens" {
		t.Fatalf("source = %v, want tokens", snap["source"])
	}
	if snap["model"] != "gpt" {
		t.Fatalf("model = %v, want gpt", snap["model"])
	}
	if snap["limiter_type"] != "tokens" {
		t.Fatalf("limiter_type = %v, want tokens", snap["limiter_type"])
	}
	if up := snap["used_percent"].(float64); up != 75 {
		t.Fatalf("used_percent = %v, want 75", up)
	}
	if rt := snap["remaining_tokens"].(float64); rt != 2500 {
		t.Fatalf("remaining_tokens = %v, want 2500", rt)
	}
	if rr := snap["remaining_requests"].(float64); rr != 97 {
		t.Fatalf("remaining_requests = %v, want 97", rr)
	}
	if snap["label"] != "a" {
		t.Fatalf("label = %v, want a", snap["label"])
	}
}

func TestAdminQuotaLabelsSnapshotAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "with-snapshot", Label: "Visible", Email: "visible@example.com", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "visible-token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccount(ctx, storage.Account{ID: "without-snapshot", Label: "Hidden", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "hidden-token"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:       "with-snapshot",
		Provider:        "codex",
		Source:          "tokens",
		UsedPercent:     12.5,
		LimitTokens:     1000,
		RemainingTokens: 875,
		Status:          "allowed",
	}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/quota", "")
	if code != http.StatusOK {
		t.Fatalf("admin quota = %d: %s", code, raw)
	}
	var quota []map[string]interface{}
	if err := json.Unmarshal(raw, &quota); err != nil {
		t.Fatalf("decode quota: %v\n%s", err, raw)
	}
	if len(quota) != 1 {
		t.Fatalf("quota rows = %d, want only one snapshot row: %#v", len(quota), quota)
	}
	if quota[0]["account_id"] != "with-snapshot" || quota[0]["label"] != "Visible" {
		t.Fatalf("quota row = %#v, want visible label for snapshot account", quota[0])
	}
}

// TestUsageTimeseriesEndpoint verifies the timeseries endpoint returns buckets
// after a gateway request records usage.
func TestUsageTimeseriesEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r","model":"gpt","output_text":"hi","usage":{"input_tokens":40,"output_tokens":8,"total_tokens":48,"input_tokens_details":{"cached_tokens":10}}}`))
	})
	h.importAccount(t, "a", "upstream-a", "access-a")
	resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	h.app.WaitForAsyncWrites() // usage rows are written asynchronously; drain before querying
	tr, err := http.Get(h.pool.URL + "/admin/usage/timeseries?bucket=3600")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Body.Close()
	var out struct {
		Bucket  int64 `json:"bucket"`
		Buckets []struct {
			Bucket      int64 `json:"bucket"`
			Requests    int64 `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"buckets"`
	}
	if err := json.NewDecoder(tr.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Bucket != 3600 {
		t.Fatalf("bucket = %d, want 3600", out.Bucket)
	}
	var total int64
	for _, b := range out.Buckets {
		total += b.TotalTokens
	}
	if total != 48 {
		t.Fatalf("total tokens across buckets = %d, want 48", total)
	}
}

// TestEmbeddedUIServes confirms the new SPA console is embedded and primary (root
// redirects to it), and the legacy vanilla-JS UI remains reachable at /legacy/.
func TestEmbeddedUIServes(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	get := func(path string) string {
		resp, err := http.Get(h.pool.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	// Root now redirects to the new SPA console (the client follows the 302).
	index := get("/")
	for _, want := range []string{"Pool 控制台", "/console/assets/"} {
		if !strings.Contains(index, want) {
			t.Fatalf("root → SPA index missing %q", want)
		}
	}
	// The SPA is served at /console/ and deep links fall back to its shell.
	if c := get("/console/accounts"); !strings.Contains(c, "/console/assets/") {
		t.Fatalf("/console/ deep link did not serve the SPA shell")
	}
	// The legacy vanilla-JS UI remains reachable at /legacy/.
	legacy := get("/legacy/")
	for _, want := range []string{"appShell", "core.js"} {
		if !strings.Contains(legacy, want) {
			t.Fatalf("/legacy/ index missing %q", want)
		}
	}
}
