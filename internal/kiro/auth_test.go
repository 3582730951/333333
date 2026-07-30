package kiro

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

func kiroAuthHarness(t *testing.T, handler http.HandlerFunc, method string) (*Manager, *storage.Store, storage.Account, storage.KiroCredentials, storage.AccountToken, storage.EgressProfile) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "kiro-auth", Label: "kiro-auth", GroupName: "cyber", Provider: "kiro", Status: "active"}
	token := storage.AccountToken{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: storage.Now() - 1}
	if err := store.UpsertAccount(context.Background(), account, token); err != nil {
		t.Fatal(err)
	}
	cred := storage.KiroCredentials{AccountID: account.ID, AuthMethod: method, ClientID: "client", ClientSecret: "secret", Endpoint: srv.URL, CredentialHash: "hash"}
	if err := store.UpsertKiroCredentials(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	token, _ = store.GetToken(context.Background(), account.ID)
	egress, _ := store.GetEgressProfile(context.Background(), storage.DefaultDirectEgressID)
	cfg := config.Default()
	cfg.KiroEndpointAllowlist = []string{srv.URL}
	return NewManager(store, upstream.NewClient(cfg), cfg), store, account, cred, token, egress
}

func TestSocialRefreshRotatesTokens(t *testing.T) {
	m, store, account, cred, token, egress := kiroAuthHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refreshToken" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"accessToken":"access-new","refreshToken":"refresh-new","expiresIn":3600,"profileArn":"arn:new"}`))
	}, "social")
	bearer, updated, updatedCred, err := m.Prepare(context.Background(), account, cred, token, egress, true)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "access-new" || updated.RefreshToken != "refresh-new" || updatedCred.ProfileARN != "arn:new" {
		t.Fatalf("refresh result: bearer=%q token=%+v cred=%+v", bearer, updated, updatedCred)
	}
	persisted, _ := store.GetToken(context.Background(), account.ID)
	if persisted.AccessToken != "access-new" || persisted.RefreshToken != "refresh-new" {
		t.Fatalf("rotated tokens were not atomically persisted: %+v", persisted)
	}
}

func TestIDCRefreshPrefersIDToken(t *testing.T) {
	m, _, account, cred, token, egress := kiroAuthHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			t.Errorf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"accessToken":"access","idToken":"identity","expiresIn":3600}`))
	}, "idc")
	bearer, updated, _, err := m.Prepare(context.Background(), account, cred, token, egress, true)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "identity" || updated.IDTokenRaw != "identity" || updated.AccessToken != "identity" {
		t.Fatalf("IDC token preference failed: bearer=%q token=%+v", bearer, updated)
	}
}

func TestInvalidGrantPermanentlyInvalidatesAccount(t *testing.T) {
	m, store, account, cred, token, egress := kiroAuthHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}, "social")
	_, _, _, err := m.Prepare(context.Background(), account, cred, token, egress, true)
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err=%v, want ErrInvalidGrant", err)
	}
	got, _ := store.GetAccount(context.Background(), account.ID)
	if got.Status != "invalid" {
		t.Fatalf("account status=%q", got.Status)
	}
}

func TestRefreshIsSingleflightAndAPIKeyBypassesRefresh(t *testing.T) {
	var calls atomic.Int32
	m, _, account, cred, token, egress := kiroAuthHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"accessToken":"single","expiresIn":3600}`))
	}, "social")
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bearer, _, _, err := m.Prepare(context.Background(), account, cred, token, egress, false)
			if err == nil && bearer != "single" {
				err = errors.New("unexpected bearer")
			}
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d, want 1", calls.Load())
	}
	cred.AuthMethod = "api_key"
	cred.KiroAPIKey = "kiro-key"
	bearer, _, _, err := m.Prepare(context.Background(), account, cred, token, egress, true)
	if err != nil || bearer != "kiro-key" || calls.Load() != 1 {
		t.Fatalf("api key prepare: bearer=%q err=%v calls=%d", bearer, err, calls.Load())
	}
	headers := Headers(config.Default(), cred, bearer, true)
	if headers.Get("Authorization") != "Bearer kiro-key" || headers.Get("tokentype") != "API_KEY" {
		t.Fatalf("api key headers=%v", headers)
	}
}

func TestHeadersMatchOfficialKiroCLI2152WireProfile(t *testing.T) {
	cfg := config.Default() // legacy 0.11.x config is normalized to the CLI release train.
	cred := storage.KiroCredentials{
		AccountID:  "account-that-must-not-leak",
		AuthMethod: "api_key",
		KiroAPIKey: "key-that-must-not-leak",
		MachineID:  strings.Repeat("a", 64),
	}
	got := Headers(cfg, cred, "test-bearer", true)
	wantUA := "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.17975 os/linux lang/rust/1.92.0 md/appVersion-2.15.2 app/AmazonQ-For-CLI"
	wantAmzUA := "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererstreaming/0.1.17975 os/linux lang/rust/1.92.0 m/F app/AmazonQ-For-CLI"
	if got.Get("User-Agent") != wantUA {
		t.Fatalf("User-Agent=%q, want %q", got.Get("User-Agent"), wantUA)
	}
	if got.Get("X-Amz-User-Agent") != wantAmzUA {
		t.Fatalf("X-Amz-User-Agent=%q, want %q", got.Get("X-Amz-User-Agent"), wantAmzUA)
	}
	for name, want := range map[string]string{
		"Content-Type":                "application/x-amz-json-1.0",
		"Accept":                      "*/*",
		"Accept-Encoding":             "gzip",
		"X-Amzn-Codewhisperer-Optout": "false",
		"TokenType":                   "API_KEY",
		"Authorization":               "Bearer test-bearer",
		"Amz-Sdk-Request":             "attempt=1; max=3",
		"X-Amzn-Kiro-Agent-Mode":      "",
		"X-Amzn-Kiro-Profile-Arn":     "",
	} {
		if value := got.Get(name); value != want {
			t.Errorf("%s=%q, want %q", name, value, want)
		}
	}
	if got.Get("Amz-Sdk-Invocation-Id") == "" {
		t.Fatal("Amz-Sdk-Invocation-Id is empty")
	}
	combined := got.Get("User-Agent") + got.Get("X-Amz-User-Agent")
	for _, forbidden := range []string{"KiroIDE", cred.MachineID, cred.AccountID, cred.KiroAPIKey} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("wire user agent leaked obsolete/device identity %q: %q", forbidden, combined)
		}
	}
}

func TestHeadersUseRuntimeServiceAndAuthSpecificTokenType(t *testing.T) {
	cfg := config.Default()
	cfg.KiroVersion = "2.16.0"
	social := Headers(cfg, storage.KiroCredentials{
		AuthMethod: "social",
		ProfileARN: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test",
	}, "social-bearer", false)
	if !strings.Contains(social.Get("User-Agent"), "api/codewhispererruntime/0.1.17975") ||
		!strings.Contains(social.Get("User-Agent"), "md/appVersion-2.16.0") {
		t.Fatalf("runtime User-Agent=%q", social.Get("User-Agent"))
	}
	if social.Get("TokenType") != "EXTERNAL_IDP" {
		t.Fatalf("social TokenType=%q", social.Get("TokenType"))
	}
	if social.Get("X-Amzn-Kiro-Profile-Arn") == "" {
		t.Fatal("social profile ARN header is empty")
	}
	idc := Headers(cfg, storage.KiroCredentials{AuthMethod: "idc"}, "idc-bearer", false)
	if idc.Get("TokenType") != "" {
		t.Fatalf("IDC TokenType=%q, want omitted", idc.Get("TokenType"))
	}
	if idc.Get("Amz-Sdk-Invocation-Id") == social.Get("Amz-Sdk-Invocation-Id") {
		t.Fatal("independent requests reused an invocation id")
	}
}

func TestUsageLimitsMatchesOfficialKiroCLI2152WireRequest(t *testing.T) {
	m, _, account, cred, _, egress := kiroAuthHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%q", r.Method)
		}
		if r.URL.Path != "/getUsageLimits" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.URL.RawQuery != "origin=KIRO_CLI&isEmailRequired=false" {
			t.Errorf("query=%q", r.URL.RawQuery)
		}
		if got := r.Header.Get("X-Amz-Target"); got != "AmazonCodeWhispererService.GetUsageLimits" {
			t.Errorf("x-amz-target=%q", got)
		}
		wantAmzUA := "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererruntime/0.1.17975 os/linux lang/rust/1.92.0 m/F app/AmazonQ-For-CLI"
		if got := r.Header.Get("X-Amz-User-Agent"); got != wantAmzUA {
			t.Errorf("x-amz-user-agent=%q, want %q", got, wantAmzUA)
		}
		if got := r.Header.Get("TokenType"); got != "API_KEY" {
			t.Errorf("tokentype=%q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if string(raw) != `{"origin":"KIRO_CLI","isEmailRequired":false}` {
			t.Errorf("wire body=%q", raw)
		}
		_, _ = w.Write([]byte(`{"usageLimitList":[]}`))
	}, "api_key")
	cred.KiroAPIKey = "bearer"

	result, err := m.UsageLimitsProbe(context.Background(), account, cred, "bearer", egress)
	if err != nil || result.StatusCode != http.StatusOK || result.Limits == nil {
		t.Fatalf("usage result=%+v err=%v", result, err)
	}
}
