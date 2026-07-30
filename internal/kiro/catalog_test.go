package kiro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
)

func kiroCatalogHarness(t *testing.T, handler http.HandlerFunc) (*Manager, *storage.Store, storage.Account, storage.KiroCredentials, storage.EgressProfile) {
	t.Helper()
	server := newTestServer(t, handler)
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "kiro-catalog", Label: "catalog", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "bearer"}); err != nil {
		t.Fatal(err)
	}
	credential := storage.KiroCredentials{
		AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "bearer",
		APIRegion: "us-east-1", Endpoint: server.URL, ProfileARN: "profile-a",
	}
	if err := store.UpsertKiroCredentials(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	egress, err := store.GetEgressProfile(context.Background(), storage.DefaultDirectEgressID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.KiroEndpointAllowlist = []string{server.URL}
	return NewManager(store, upstream.NewClient(cfg), cfg), store, account, credential, egress
}

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func TestRefreshModelCatalogRequiresCompletePagination(t *testing.T) {
	var calls atomic.Int32
	manager, store, account, credential, egress := kiroCatalogHarness(t, func(w http.ResponseWriter, r *http.Request) {
		page := calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method=%q", r.Method)
		}
		if r.URL.Path != "/listAvailableModels" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.URL.Query().Get("origin"); got != kiroCLIOrigin {
			t.Errorf("origin query=%q", got)
		}
		if got := r.URL.Query().Get("nextToken"); got != "" {
			t.Errorf("pagination leaked into query: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer bearer" {
			t.Errorf("authorization header=%q", got)
		}
		if got := r.Header.Get("X-Amz-Target"); got != "AmazonCodeWhispererService.ListAvailableModels" {
			t.Errorf("x-amz-target=%q", got)
		}
		wantAmzUA := "aws-sdk-rust/1.3.15 ua/2.1 api/codewhispererruntime/0.1.17975 os/linux lang/rust/1.92.0 m/F,C app/AmazonQ-For-CLI"
		if got := r.Header.Get("X-Amz-User-Agent"); got != wantAmzUA {
			t.Errorf("x-amz-user-agent=%q, want %q", got, wantAmzUA)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-amz-json-1.0" {
			t.Errorf("content-type=%q", got)
		}
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read body: %v", readErr)
		}
		var body map[string]any
		if decodeErr := json.Unmarshal(raw, &body); decodeErr != nil {
			t.Errorf("body=%q: %v", raw, decodeErr)
		}
		if got := body["origin"]; got != kiroCLIOrigin {
			t.Errorf("body origin=%v", got)
		}
		switch page {
		case 1:
			if string(raw) != `{"origin":"KIRO_CLI"}` {
				t.Errorf("first page wire body=%q", raw)
			}
			if _, present := body["nextToken"]; present {
				t.Errorf("first page body had nextToken: %s", raw)
			}
			_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-opus-5","aliases":["opus"],"maxInputTokens":1000000,"maxOutputTokens":128000,"thinking":{"type":"adaptive"},"supportedEfforts":["high","max"]}],"nextToken":"page-2"}`))
		case 2:
			if string(raw) != `{"origin":"KIRO_CLI","nextToken":"page-2"}` {
				t.Errorf("second page wire body=%q", raw)
			}
			if body["nextToken"] != "page-2" {
				t.Errorf("second page body token=%v", body["nextToken"])
			}
			_, _ = w.Write([]byte(`{"availableModels":[{"id":"auto","maxInputTokens":1000000}],"defaultModelId":"claude-opus-5"}`))
		default:
			t.Errorf("unexpected call")
		}
	})
	models, err := manager.RefreshModelCatalog(context.Background(), account, credential, "bearer", egress)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || calls.Load() != 2 {
		t.Fatalf("models=%+v calls=%d", models, calls.Load())
	}
	var opus storage.KiroModelDescriptor
	for _, model := range models {
		if model.PublicID == "claude-opus-5" {
			opus = model
		}
	}
	if !opus.Default || opus.MaxInputTokens != 1_000_000 || opus.MaxOutputTokens != 128_000 || !opus.Complete {
		t.Fatalf("Opus descriptor=%+v", opus)
	}
	if supported, known := CatalogAdaptiveThinking(opus); !known || !supported {
		t.Fatalf("Opus 5 adaptive-thinking descriptor was not honored: supported=%v known=%v raw=%s", supported, known, opus.ThinkingJSON)
	}
	if effort, known := CatalogMaximumEffort(opus); !known || effort != "max" {
		t.Fatalf("Opus 5 effort descriptor=(%q,%v), want max", effort, known)
	}
	endpointHash, _ := EndpointHash(credential.Endpoint, credential.APIRegion, manager.Config().KiroEndpointAllowlist)
	capabilityKey, _ := KiroCapabilityKey(endpointHash, credential.APIRegion, credential.ProfileARN)
	persisted, err := store.ListKiroModelCatalog(context.Background(), account.ID, capabilityKey)
	if err != nil || len(persisted) != 2 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestRefreshModelCatalogPartialFailurePreservesLastGood(t *testing.T) {
	var phase atomic.Int32
	manager, store, account, credential, egress := kiroCatalogHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if phase.Load() == 0 {
			_, _ = w.Write([]byte(`{"models":[{"id":"claude-opus-5","maxInputTokens":1000000}]}`))
			return
		}
		var request struct {
			NextToken string `json:"nextToken"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode catalog request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.NextToken == "" {
			_, _ = w.Write([]byte(`{"models":[{"id":"claude-sonnet-5","maxInputTokens":1000000}],"nextToken":"incomplete"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"internal":"must not escape"}`))
	})
	if _, err := manager.RefreshModelCatalog(context.Background(), account, credential, "bearer", egress); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	_, err := manager.RefreshModelCatalog(context.Background(), account, credential, "bearer", egress)
	var probeErr *CatalogProbeError
	if !errors.As(err, &probeErr) || probeErr.Class != "upstream" || strings.Contains(err.Error(), "must not escape") {
		t.Fatalf("error=%v", err)
	}
	endpointHash, _ := EndpointHash(credential.Endpoint, credential.APIRegion, manager.Config().KiroEndpointAllowlist)
	capabilityKey, _ := KiroCapabilityKey(endpointHash, credential.APIRegion, credential.ProfileARN)
	persisted, err := store.ListKiroModelCatalog(context.Background(), account.ID, capabilityKey)
	if err != nil || len(persisted) != 1 || persisted[0].PublicID != "claude-opus-5" {
		t.Fatalf("last-good was replaced: %+v err=%v", persisted, err)
	}
	state, err := store.GetKiroProbeState(context.Background(), account.ID, capabilityKey)
	if err != nil || state.LastErrorClass != "upstream" || state.LastSuccessAt == 0 {
		t.Fatalf("probe state=%+v err=%v", state, err)
	}
}

func TestRefreshModelCatalogIsSingleflight(t *testing.T) {
	var calls atomic.Int32
	manager, _, account, credential, egress := kiroCatalogHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte(`{"models":[{"id":"claude-opus-5","maxInputTokens":1000000}]}`))
	})
	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			models, err := manager.RefreshModelCatalog(context.Background(), account, credential, "bearer", egress)
			if err == nil && len(models) != 1 {
				err = fmt.Errorf("model count=%d", len(models))
			}
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("catalog calls=%d, want 1", calls.Load())
	}
}
