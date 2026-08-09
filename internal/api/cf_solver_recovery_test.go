package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/cfsolve"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

func TestSolveAndInjectPromotesOnlyAfterApplicationReplay(t *testing.T) {
	var replayCookie, replayUA string
	var replayVersions []string
	application := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		replayCookie = r.Header.Get("Cookie")
		replayUA = r.Header.Get("User-Agent")
		replayVersions = append(replayVersions, r.Header.Get("version"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer application.Close()

	app, store, account, egress := newCFSolverRecoveryFixture(t, application.URL)
	if err := app.solveAndInject(context.Background(), account, egress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replayCookie, "cf_clearance=verified") {
		t.Fatalf("application replay did not receive temporary clearance: %q", replayCookie)
	}
	if !strings.HasPrefix(replayUA, "codex_cli_rs/") {
		t.Fatalf("application replay did not retain Codex UA: %q", replayUA)
	}
	if got, want := strings.Join(replayVersions, ","), strings.Join(config.SupportedCodexCLIVersions(), ","); got != want {
		t.Fatalf("clearance was not verified across the five-version jar: got=%q want=%q", got, want)
	}
	injected, err := store.ListInjectedCookies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(injected) != 1 || !strings.Contains(injected[0].CookieHeader, "cf_clearance=verified") || injected[0].UserAgent == "" {
		t.Fatalf("verified clearance was not promoted: %+v", injected)
	}
	binding, err := store.GetEgressBinding(context.Background(), account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 {
		t.Fatalf("verified replay did not clear cooldown: %+v", binding)
	}
}

func TestSolveAndInjectDoesNotPromoteBrowserOnlySuccess(t *testing.T) {
	application := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<title>Just a moment...</title>"))
	}))
	defer application.Close()

	app, store, account, egress := newCFSolverRecoveryFixture(t, application.URL)
	err := app.solveAndInject(context.Background(), account, egress)
	if err == nil || !strings.Contains(err.Error(), "remained challenged") {
		t.Fatalf("expected application replay challenge, got %v", err)
	}
	injected, listErr := store.ListInjectedCookies(context.Background())
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(injected) != 0 {
		t.Fatalf("browser-only clearance was persisted: %+v", injected)
	}
	binding, getErr := store.GetEgressBinding(context.Background(), account.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if binding.CooldownUntil == 0 {
		t.Fatalf("failed replay incorrectly cleared cooldown: %+v", binding)
	}
}

func newCFSolverRecoveryFixture(t *testing.T, applicationURL string) (*Server, *storage.Store, storage.Account, storage.EgressProfile) {
	t.Helper()
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		switch req["cmd"] {
		case "sessions.create":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "session": req["session"]})
		case "sessions.destroy":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case "request.get":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "ok",
				"version": "3.5.0",
				"solution": map[string]interface{}{
					"url":       req["url"],
					"status":    200,
					"headers":   map[string]string{"content-type": "text/html"},
					"response":  "<!doctype html><title>ready</title>",
					"userAgent": "Mozilla/5.0 Chrome/136.0.0.0 Safari/537.36",
					"cookies": []map[string]interface{}{
						{"name": "cf_clearance", "value": "verified", "path": "/", "expires": time.Now().Add(time.Hour).Unix()},
					},
				},
			})
		default:
			t.Fatalf("unexpected solver command: %v", req["cmd"])
		}
	}))
	t.Cleanup(solver.Close)

	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := config.Default()
	cfg.UpstreamBaseURL = applicationURL + "/backend-api/codex"
	cfg.CFSolverEnabled = true
	cfg.CFSolverURL = solver.URL
	account := storage.Account{ID: "acc-cf-replay", Label: "cf replay", GroupName: "cyber", Provider: "codex", Status: "active", UpstreamAccountID: "chatgpt-account"}
	token := storage.AccountToken{AccountID: account.ID, AccessToken: "oauth-access", RefreshToken: "oauth-refresh"}
	if err := store.UpsertAccount(context.Background(), account, token); err != nil {
		t.Fatal(err)
	}
	egress := storage.EgressProfile{ID: "egress-cf-replay", Name: "CF replay", Type: "direct", ExitIP: "203.0.113.10", Health: "healthy"}
	if err := store.UpsertEgressProfile(context.Background(), egress); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{
		AccountID:       account.ID,
		PrimaryEgressID: egress.ID,
		CookieJarKey:    account.ID + ":" + egress.ID,
		CooldownUntil:   storage.Now() + 600,
	}); err != nil {
		t.Fatal(err)
	}
	up := upstream.NewClient(cfg)
	app := &Server{
		cfg:       cfg,
		store:     store,
		upstream:  up,
		solver:    cfsolve.NewClient(cfg),
		scheduler: scheduler.New(store, cfg),
	}
	return app, store, account, egress
}
