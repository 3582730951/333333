package kiro

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
