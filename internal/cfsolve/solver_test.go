package cfsolve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
)

func TestSolveParsesClearance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["cmd"] != "request.get" {
			t.Errorf("unexpected cmd: %v", req["cmd"])
		}
		if _, ok := req["proxy"]; !ok {
			t.Errorf("proxy should be forwarded to the solver")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "ok",
			"solution": map[string]interface{}{
				"userAgent": "Mozilla/5.0 (X11; Linux x86_64) Chrome/...",
				"cookies": []map[string]interface{}{
					{"name": "cf_clearance", "value": "abc123"},
					{"name": "__cf_bm", "value": "xyz"},
				},
			},
		})
	}))
	defer srv.Close()

	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	sol, err := c.Solve(context.Background(), "https://chatgpt.com/", "socks5h://127.0.0.1:40000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sol.CookieHeader, "cf_clearance=abc123") {
		t.Fatalf("cookie header missing clearance: %q", sol.CookieHeader)
	}
	if sol.UserAgent == "" {
		t.Fatal("solver UA not captured (needed for replay)")
	}
	if sol.Cookies["__cf_bm"] != "xyz" {
		t.Fatalf("auxiliary cookies dropped: %+v", sol.Cookies)
	}
}

func TestSolveNoClearanceIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"solution": map[string]interface{}{"userAgent": "x", "cookies": []map[string]interface{}{{"name": "other", "value": "1"}}},
		})
	}))
	defer srv.Close()
	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	if _, err := c.Solve(context.Background(), "https://chatgpt.com/", ""); err == nil {
		t.Fatal("expected error when no cf_clearance is returned")
	}
}

func TestSolveDisabled(t *testing.T) {
	c := NewClient(config.Config{CFSolverEnabled: false, CFSolverURL: "http://x"})
	if c.Enabled() {
		t.Fatal("should be disabled")
	}
	if _, err := c.Solve(context.Background(), "https://x/", ""); err == nil {
		t.Fatal("expected error when disabled")
	}
}
