package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

// TestGatewayIdentity exercises GET /v1/gateway/identity. The endpoint derives a *virtual*
// identity deterministically from the downstream_key seed and is intentionally NOT bound to
// a specific pooled account (AccountID stays empty — the gateway picks the account later).
func TestGatewayIdentity(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)

	call := func(key string) GatewayIdentityResponse {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		srv.handleGatewayIdentity(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp GatewayIdentityResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp
	}

	resp := call("cap_test123")

	// Current contract: identity is keyed off the downstream_key, not account-bound.
	if resp.AccountID != "" {
		t.Errorf("account_id should be empty (identity is not account-bound), got %q", resp.AccountID)
	}
	if resp.SessionID == "" {
		t.Error("session_id should not be empty")
	}
	if resp.UserID == "" {
		t.Error("user_id should not be empty")
	}
	if resp.Hostname == "" {
		t.Error("hostname should not be empty")
	}
	if len(resp.DNSServers) == 0 {
		t.Error("dns_servers should not be empty")
	}
	if resp.GatewayIP == "" {
		t.Error("gateway_ip should not be empty")
	}
	if resp.ProcessInfo.PID == 0 {
		t.Error("process_info.pid should not be 0")
	}

	// Determinism: the same downstream_key yields a stable virtual identity; a different
	// key must not collide.
	if again := call("cap_test123"); again.SessionID != resp.SessionID {
		t.Errorf("identity should be stable for the same downstream_key: %s vs %s", resp.SessionID, again.SessionID)
	}
	if other := call("cap_other999"); other.SessionID == resp.SessionID {
		t.Error("a different downstream_key should yield a different identity")
	}
}

func TestGatewayIdentityAcceptsHeaderAndQueryKeys(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)

	call := func(req *http.Request) GatewayIdentityResponse {
		t.Helper()
		w := httptest.NewRecorder()
		srv.handleGatewayIdentity(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var resp GatewayIdentityResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return resp
	}

	authReq := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
	authReq.Header.Set("Authorization", "Bearer cap_header")
	authResp := call(authReq)

	xKeyReq := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
	xKeyReq.Header.Set("X-Downstream-Key", "cap_header")
	if xKeyResp := call(xKeyReq); xKeyResp.SessionID != authResp.SessionID {
		t.Fatalf("X-Downstream-Key identity differs from bearer identity")
	}

	queryReq := httptest.NewRequest("GET", "/v1/gateway/identity?downstream_key=cap_header&provider=claude", nil)
	if queryResp := call(queryReq); queryResp.SessionID != authResp.SessionID {
		t.Fatalf("query fallback identity differs from bearer identity")
	}
}

func TestGatewayIdentityRequiresKnownEnabledKeyWhenConfigured(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)
	srv.cfg.RequireDownstreamKey = true
	ctx := context.Background()
	if err := srv.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash: hashAPIKey("cap_identity_ok"),
		Label:   "identity-ok",
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash: hashAPIKey("cap_identity_disabled"),
		Label:   "identity-disabled",
		Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	assertStatus := func(key string, want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		srv.handleGatewayIdentity(w, req)
		if w.Code != want {
			t.Fatalf("%s status = %d, want %d; body=%s", key, w.Code, want, w.Body.String())
		}
	}

	assertStatus("cap_identity_ok", http.StatusOK)
	assertStatus("cap_identity_unknown", http.StatusUnauthorized)
	assertStatus("cap_identity_disabled", http.StatusUnauthorized)
}

func TestGatewayIdentityMissingDownstreamKeyUsesJSONError(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)

	req := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
	w := httptest.NewRecorder()
	srv.handleGatewayIdentity(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}

	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertErrorEnvelope(t, body, "missing downstream_key")
}

func newGatewayIdentityTestServer(t *testing.T) *Server {
	t.Helper()

	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.IdentitySecret = "test-secret"
	srv := NewServer(Dependencies{
		Config:    cfg,
		Store:     store,
		Scheduler: scheduler.New(store, cfg),
	})
	return srv
}
