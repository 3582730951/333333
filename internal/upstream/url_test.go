package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestComputeURLForChatGPTCodexBackend(t *testing.T) {
	got := ComputeURL("https://chatgpt.com/backend-api/codex", "/v1/responses/compact?trace=1")
	want := "https://chatgpt.com/backend-api/codex/responses/compact?trace=1"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}

func TestComputeCodexResponsesWebSocketURL(t *testing.T) {
	got := ComputeCodexResponsesWebSocketURL("https://chatgpt.com/backend-api/codex", "/v1/responses?trace=1")
	want := "wss://api.openai.com/v1/responses?trace=1"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}

	got = ComputeCodexResponsesWebSocketURL("http://127.0.0.1:8787/backend-api/codex", "/v1/responses")
	want = "ws://127.0.0.1:8787/v1/responses"
	if got != want {
		t.Fatalf("local url = %q, want %q", got, want)
	}
}

func TestDoAddsAuthAndFedrampHeaders(t *testing.T) {
	var gotAuth, gotAccount, gotFedramp, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-ID")
		gotFedramp = r.Header.Get("X-OpenAI-Fedramp")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(nilContext(t), Request{
		Method:         http.MethodPost,
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{"Authorization": []string{"Bearer downstream"}},
		Body:           []byte(`{"input":"hi"}`),
		Account:        storage.Account{ID: "acc", UpstreamAccountID: "workspace", IsFedramp: true},
		Token:          storage.AccountToken{AccessToken: "access"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotAuth != "Bearer access" || gotAccount != "workspace" || gotFedramp != "true" || !strings.Contains(gotBody, "hi") {
		t.Fatalf("headers/body = auth=%q account=%q fed=%q body=%q", gotAuth, gotAccount, gotFedramp, gotBody)
	}
}

func TestCookieJarIsScopedByAccountAndEgress(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "cf_clearance", Value: "cookie-" + r.Header.Get("ChatGPT-Account-ID")})
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	base := Request{
		Method:         http.MethodPost,
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{},
		Body:           []byte(`{}`),
		Token:          storage.AccountToken{AccessToken: "access"},
		Egress:         storage.EgressProfile{ID: "egress-a", Type: "direct", Health: "healthy"},
	}
	base.Account = storage.Account{ID: "acc-a", UpstreamAccountID: "workspace-a"}
	for i := 0; i < 2; i++ {
		resp, err := client.Do(context.Background(), base)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	other := base
	other.Account = storage.Account{ID: "acc-b", UpstreamAccountID: "workspace-b"}
	resp, err := client.Do(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(seen) != 3 {
		t.Fatalf("seen = %v", seen)
	}
	if seen[0] != "" || !strings.Contains(seen[1], "cookie-workspace-a") || seen[2] != "" {
		t.Fatalf("cookie scope violated: %#v", seen)
	}
}

func TestImportCookiesForBrowserRepair(t *testing.T) {
	var cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	host := strings.TrimPrefix(server.URL, "http://")
	if err := client.ImportCookies("acc-a", "egress-a", "http://"+host, "acc-a:egress-a", "cf_clearance=repair"); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(context.Background(), Request{
		Method:         http.MethodPost,
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{},
		Body:           []byte(`{}`),
		Account:        storage.Account{ID: "acc-a", UpstreamAccountID: "workspace-a"},
		Token:          storage.AccountToken{AccessToken: "access"},
		Egress:         storage.EgressProfile{ID: "egress-a", Type: "direct", Health: "healthy"},
		CookieJarKey:   "acc-a:egress-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(cookie, "cf_clearance=repair") {
		t.Fatalf("repair cookie missing: %q", cookie)
	}
}

func nilContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
