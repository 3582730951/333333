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
	if resp.ProcessInfo.CWD != "" {
		t.Errorf("process_info.cwd should be empty; gateway must preserve local cwd, got %q", resp.ProcessInfo.CWD)
	}
	if got := strings.Join(resp.GatewayPolicy.InterceptHosts, ","); strings.Contains(got, "api.openai.com") || strings.Contains(got, "chatgpt.com") {
		t.Fatalf("default gateway policy must not MITM Codex/OpenAI hosts: %q", got)
	}
	if got := strings.Join(resp.GatewayPolicy.ForwardHosts, ","); !strings.Contains(got, "api.openai.com") || !strings.Contains(got, "api.github.com") || !strings.Contains(got, "api.osv.dev") {
		t.Fatalf("default gateway policy must forward child-tool hosts, got %q", got)
	}
	if resp.GatewayPolicy.UnknownTargetPolicy != "forward" {
		t.Fatalf("default gateway policy must forward unknown child-tool hosts, got %q", resp.GatewayPolicy.UnknownTargetPolicy)
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

func TestGatewayFullDeviceConvergencePreservesDownstreamSessions(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)
	if err := srv.store.SetSetting(context.Background(), "identity_convergence_mode", "full"); err != nil {
		t.Fatal(err)
	}
	call := func(key string) GatewayIdentityResponse {
		req := httptest.NewRequest(http.MethodGet, "/v1/gateway/identity", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		srv.handleGatewayIdentity(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var response GatewayIdentityResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	left, right := call("converged-left"), call("converged-right")
	if left.MachineID != right.MachineID || left.UserID != right.UserID || left.Hostname != right.Hostname ||
		left.GatewayIP != right.GatewayIP || left.LocalIP != right.LocalIP || left.ProcessInfo.PID != right.ProcessInfo.PID {
		t.Fatalf("gateway device fields did not converge: left=%+v right=%+v", left, right)
	}
	if left.SessionID == right.SessionID {
		t.Fatal("full device convergence merged downstream logical sessions")
	}
}

func TestGatewayIdentityIncludesEditablePolicyAndDNSOverride(t *testing.T) {
	srv := newGatewayIdentityTestServer(t)
	ctx := context.Background()
	if err := srv.store.SetSettings(ctx, map[string]string{
		"claude_gateway_intercept_hosts":          "api.anthropic.com,custom-api.example.com",
		"claude_gateway_forward_hosts":            "assets.example.com",
		"claude_gateway_blocked_host_patterns":    "statsig,customtelemetry",
		"claude_gateway_unknown_target_policy":    "forward",
		"claude_gateway_disable_nonessential_env": "false",
		"claude_gateway_strict_linux_default":     "false",
		"claude_gateway_virtual_dns_servers":      "9.9.9.9,149.112.112.112",
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/v1/gateway/identity?provider=claude", nil)
	req.Header.Set("Authorization", "Bearer cap_policy")
	w := httptest.NewRecorder()
	srv.handleGatewayIdentity(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp GatewayIdentityResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if got := strings.Join(resp.DNSServers, ","); got != "9.9.9.9,149.112.112.112" {
		t.Fatalf("dns override = %q", got)
	}
	if got := strings.Join(resp.GatewayPolicy.InterceptHosts, ","); got != "api.anthropic.com,custom-api.example.com" {
		t.Fatalf("intercept hosts = %q", got)
	}
	if got := strings.Join(resp.GatewayPolicy.ForwardHosts, ","); got != "assets.example.com" {
		t.Fatalf("forward hosts = %q", got)
	}
	if got := strings.Join(resp.GatewayPolicy.BlockedHostPatterns, ","); got != "statsig,customtelemetry" {
		t.Fatalf("blocked patterns = %q", got)
	}
	if resp.GatewayPolicy.UnknownTargetPolicy != "forward" {
		t.Fatalf("unknown target policy = %q", resp.GatewayPolicy.UnknownTargetPolicy)
	}
	if resp.GatewayPolicy.DisableNonessentialEnv {
		t.Fatal("disable nonessential env should follow admin override false")
	}
	if resp.GatewayPolicy.StrictLinuxDefault {
		t.Fatal("strict linux default should follow admin override false")
	}
}

func TestClaudeGatewayPolicyConfigFieldsAreAdminEditable(t *testing.T) {
	for _, key := range []string{
		"claude_gateway_intercept_hosts",
		"claude_gateway_forward_hosts",
		"claude_gateway_blocked_host_patterns",
		"claude_gateway_unknown_target_policy",
		"claude_gateway_disable_nonessential_env",
		"claude_gateway_strict_linux_default",
		"claude_gateway_virtual_dns_servers",
	} {
		field, ok := configFieldByKey(key)
		if !ok {
			t.Fatalf("admin config field %q is missing", key)
		}
		if field.Effect != effectHot {
			t.Fatalf("%s effect = %s, want hot", key, field.Effect)
		}
		if field.Category == "" {
			t.Fatalf("%s should have a UI category", key)
		}
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
	assertErrorEnvelope(t, body, http.StatusBadRequest)
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
	// Registered after the store cleanup so LIFO cleanup drains the journal and
	// stops its replayer before closing SQLite and deleting the temp directory.
	t.Cleanup(srv.FlushWrites)
	return srv
}
