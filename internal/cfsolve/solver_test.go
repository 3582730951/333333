package cfsolve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/config"
)

func TestSolveUsesV3SessionAndParsesClearance(t *testing.T) {
	var mu sync.Mutex
	var commands []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		commands = append(commands, req)
		mu.Unlock()
		switch req["cmd"] {
		case "sessions.create":
			proxy := req["proxy"].(map[string]interface{})
			if proxy["url"] != "socks5://127.0.0.1:40000" || proxy["username"] != "alice" || proxy["password"] != "p@ss" {
				t.Errorf("session proxy was not normalized/separated: %#v", proxy)
			}
			writeJSON(t, w, map[string]interface{}{"status": "ok", "session": req["session"], "version": "3.5.0"})
		case "request.get":
			if _, ok := req["proxy"]; ok {
				t.Error("request.get must inherit the session proxy instead of sending another proxy")
			}
			if req["session"] == "" || req["session_ttl_minutes"] != float64(2) || req["waitInSeconds"] != float64(2) || req["disableMedia"] != false {
				t.Errorf("unexpected v3 request fields: %#v", req)
			}
			writeSolved(t, w, "https://chatgpt.com/", []map[string]interface{}{
				{"name": "__cf_bm", "value": "xyz", "domain": ".chatgpt.com", "path": "/"},
				{"name": "cf_clearance", "value": "abc123", "domain": ".chatgpt.com", "path": "/", "secure": true, "expires": time.Now().Add(time.Hour).Unix()},
				{"name": "wrong_domain", "value": "drop", "domain": ".example.com", "path": "/"},
			})
		case "sessions.destroy":
			writeJSON(t, w, map[string]interface{}{"status": "ok"})
		default:
			t.Errorf("unexpected cmd: %v", req["cmd"])
		}
	}))
	defer srv.Close()

	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	sol, err := c.Solve(context.Background(), "https://chatgpt.com/", "socks5h://alice:p%40ss@127.0.0.1:40000")
	if err != nil {
		t.Fatal(err)
	}
	if sol.Mode != "session" || sol.Attempt != 1 || sol.StatusCode != 200 || sol.SolverVersion != "3.5.0" {
		t.Fatalf("unexpected solution evidence: %+v", sol)
	}
	if sol.CookieHeader != "cf_clearance=abc123; __cf_bm=xyz" {
		t.Fatalf("unexpected deterministic cookie header: %q", sol.CookieHeader)
	}
	if sol.UserAgent == "" || sol.Cookies["wrong_domain"] != "" || sol.ExpiresAt == 0 {
		t.Fatalf("solution validation/filtering failed: %+v", sol)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 3 || commands[0]["cmd"] != "sessions.create" || commands[1]["cmd"] != "request.get" || commands[2]["cmd"] != "sessions.destroy" {
		t.Fatalf("unexpected session lifecycle: %#v", commands)
	}
}

func TestSolveFallsBackToStatelessWhenSessionsUnsupported(t *testing.T) {
	var commands []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cmd, _ := req["cmd"].(string)
		commands = append(commands, cmd)
		if cmd == "sessions.create" {
			writeJSON(t, w, map[string]interface{}{"status": "error", "message": "unknown command"})
			return
		}
		if cmd != "request.get" {
			t.Fatalf("unexpected command: %q", cmd)
		}
		if req["proxy"].(map[string]interface{})["url"] != "http://127.0.0.1:8080" {
			t.Fatalf("stateless proxy missing: %#v", req)
		}
		writeSolved(t, w, "https://chatgpt.com/", []map[string]interface{}{{"name": "cf_clearance", "value": "legacy", "domain": "chatgpt.com", "path": "/"}})
	}))
	defer srv.Close()

	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	sol, err := c.Solve(context.Background(), "https://chatgpt.com/", "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if sol.Mode != "stateless" || sol.Attempt != 1 {
		t.Fatalf("unexpected fallback solution: %+v", sol)
	}
	if strings.Join(commands, ",") != "sessions.create,request.get" {
		t.Fatalf("unexpected compatibility commands: %v", commands)
	}
}

func TestSolveRetriesFreshSessionWhenFinalPageIsStillChallenge(t *testing.T) {
	getAttempts := 0
	destroys := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["cmd"] {
		case "sessions.create":
			writeJSON(t, w, map[string]interface{}{"status": "ok", "session": req["session"]})
		case "sessions.destroy":
			destroys++
			writeJSON(t, w, map[string]interface{}{"status": "ok"})
		case "request.get":
			getAttempts++
			if getAttempts == 1 {
				writeJSON(t, w, map[string]interface{}{
					"status": "ok",
					"solution": map[string]interface{}{
						"url":       "https://chatgpt.com/",
						"status":    200,
						"userAgent": testBrowserUA,
						"response":  "<title>Just a moment...</title><script src='/cdn-cgi/challenge-platform/x'></script>",
						"cookies":   []map[string]interface{}{{"name": "cf_clearance", "value": "premature"}},
					},
				})
				return
			}
			writeSolved(t, w, "https://chatgpt.com/", []map[string]interface{}{{"name": "cf_clearance", "value": "passed"}})
		}
	}))
	defer srv.Close()

	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	sol, err := c.Solve(context.Background(), "https://chatgpt.com/", "")
	if err != nil {
		t.Fatal(err)
	}
	if sol.Attempt != 2 || sol.Cookies["cf_clearance"] != "passed" || getAttempts != 2 || destroys != 2 {
		t.Fatalf("fresh-session retry did not complete: sol=%+v gets=%d destroys=%d", sol, getAttempts, destroys)
	}
}

func TestSolveNoClearanceIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["cmd"] == "sessions.create" {
			writeJSON(t, w, map[string]interface{}{"status": "ok", "session": req["session"]})
			return
		}
		if req["cmd"] == "sessions.destroy" {
			writeJSON(t, w, map[string]interface{}{"status": "ok"})
			return
		}
		writeSolved(t, w, "https://chatgpt.com/", []map[string]interface{}{{"name": "other", "value": "1"}})
	}))
	defer srv.Close()
	c := NewClient(config.Config{CFSolverEnabled: true, CFSolverURL: srv.URL})
	if _, err := c.Solve(context.Background(), "https://chatgpt.com/", ""); err == nil || !strings.Contains(err.Error(), "cf_clearance") {
		t.Fatalf("expected clearance error, got %v", err)
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

const testBrowserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/136.0.0.0 Safari/537.36"

func writeSolved(t *testing.T, w http.ResponseWriter, finalURL string, cookies []map[string]interface{}) {
	t.Helper()
	writeJSON(t, w, map[string]interface{}{
		"status":  "ok",
		"version": "3.5.0",
		"solution": map[string]interface{}{
			"url":       finalURL,
			"status":    200,
			"headers":   map[string]interface{}{"content-type": "text/html"},
			"response":  "<!doctype html><title>ready</title>",
			"userAgent": testBrowserUA,
			"cookies":   cookies,
		},
	})
}

func writeJSON(t *testing.T, w http.ResponseWriter, value interface{}) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
