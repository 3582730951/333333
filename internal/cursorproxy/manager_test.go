package cursorproxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateCredentialSeparatesAPIKeyAndBrowserState(t *testing.T) {
	if err := ValidateCredential(Credential{BridgeKey: "bridge", APIKey: "cursor-key"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cli-config.json"), []byte(`{"authInfo":{"email":"user@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCredential(Credential{BridgeKey: "bridge", ConfigDir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []Credential{
		{},
		{BridgeKey: "bridge"},
		{BridgeKey: "bridge", APIKey: "key", ConfigDir: dir},
		{BridgeKey: "bridge", ConfigDir: filepath.Join(dir, "missing")},
	} {
		if err := ValidateCredential(invalid); err == nil {
			t.Fatalf("credential %+v unexpectedly accepted", invalid)
		}
	}
}

func TestReferenceVersionIsPinned(t *testing.T) {
	if ReferenceVersion != "1.4.0" || len(ReferenceCommit) != 40 || len(ReviewedHeadCommit) != 40 {
		t.Fatalf("unreviewed Cursor proxy reference %s@%s (head %s)", ReferenceVersion, ReferenceCommit, ReviewedHeadCommit)
	}
}

func TestCursorChildEnvironmentDoesNotInheritCredentialsOrEgress(t *testing.T) {
	t.Setenv("CURSOR_API_KEY", "leaked-key")
	t.Setenv("CURSOR_CONFIG_DIRS", "/leaked/config")
	t.Setenv("CURSOR_BRIDGE_MODE", "agent")
	t.Setenv("HTTPS_PROXY", "http://leaked-proxy.invalid")
	t.Setenv("CODEX_POOL_ADMIN_TOKEN", "must-not-reach-child")
	t.Setenv("HOME", "/shared/pool-home")
	environment := cursorChildEnvironment(Credential{BridgeKey: "bridge", ConfigDir: "/account/config"}, 4321, "/bin/agent", "/isolated/runtime")
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	if _, found := values["CURSOR_API_KEY"]; found {
		t.Fatal("browser account inherited host CURSOR_API_KEY")
	}
	if _, found := values["HTTPS_PROXY"]; found {
		t.Fatal("direct account inherited host HTTPS_PROXY")
	}
	if values["CURSOR_CONFIG_DIRS"] != "/account/config" || values["CURSOR_BRIDGE_API_KEY"] != "bridge" || values["CURSOR_BRIDGE_PORT"] != "4321" {
		t.Fatalf("selected account environment = %#v", values)
	}
	if values["HOME"] != "/isolated/runtime" || values["USERPROFILE"] != "/isolated/runtime" {
		t.Fatalf("Cursor runtime home was not isolated: %#v", values)
	}
	if _, found := values["CURSOR_BRIDGE_MODE"]; found {
		t.Fatal("uncontrolled host CURSOR_BRIDGE_MODE was retained")
	}
	if _, found := values["CODEX_POOL_ADMIN_TOKEN"]; found {
		t.Fatal("pool secret reached Cursor child environment")
	}
}

func TestHealthClientNeverUsesEnvironmentProxy(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	transport, ok := manager.healthHTTP.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("Cursor health transport must be direct: %#v", manager.healthHTTP.Transport)
	}
}

func TestFetchUsageMakesOneBoundedRequest(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer cursor-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"startOfMonth":"2026-08-01T00:00:00Z","gpt-4":{"numRequests":12,"numRequestsTotal":12,"numTokens":99,"maxTokenUsage":null,"maxRequestUsage":500}}`))
	}))
	defer upstream.Close()
	manager := NewManager()
	defer manager.Close()
	manager.usageURL = upstream.URL
	usage, err := manager.FetchUsage(t.Context(), Credential{APIKey: "cursor-key"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || usage.Models["gpt-4"].NumRequests != 12 {
		t.Fatalf("calls=%d usage=%+v", calls, usage)
	}
}

func TestResolveNegativelyCachesProcessExitBeforeReady(t *testing.T) {
	temp := t.TempDir()
	starts := filepath.Join(temp, "starts")
	binary := filepath.Join(temp, "cursor-proxy-fail")
	script := "#!/bin/sh\nprintf 'start\\n' >> '" + starts + "'\nexit 23\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewManager()
	defer manager.Close()
	manager.binary = binary
	manager.agentBinary = "/bin/true"
	manager.available = true
	credential := Credential{BridgeKey: "bridge", APIKey: "cursor-key"}

	started := time.Now()
	if _, release, err := manager.Acquire(t.Context(), "account", credential); err == nil {
		release()
		t.Fatal("immediately exiting Cursor process unexpectedly resolved")
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("early process exit took %s to surface", elapsed)
	}
	if _, release, err := manager.Acquire(t.Context(), "account", credential); err == nil {
		release()
		t.Fatal("negative-cached Cursor process failure unexpectedly resolved")
	}
	raw, err := os.ReadFile(starts)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "start\n"); got != 1 {
		t.Fatalf("failed Cursor process starts = %d, want one within negative-cache TTL", got)
	}
}

func TestEvictOldestMarksInstanceStopped(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	oldestReady := make(chan struct{})
	newestReady := make(chan struct{})
	close(oldestReady)
	close(newestReady)
	oldest := &instance{lastUsed: time.Unix(1, 0), ready: oldestReady}
	newest := &instance{lastUsed: time.Unix(2, 0), ready: newestReady}
	manager.instances = map[string]*instance{"oldest": oldest, "newest": newest}
	manager.mu.Lock()
	if !manager.evictOldestLocked() {
		t.Fatal("idle instance was not evicted")
	}
	manager.mu.Unlock()
	if !oldest.stopped || manager.instances["oldest"] != nil || manager.instances["newest"] != newest {
		t.Fatalf("LRU eviction state oldest=%+v instances=%+v", oldest, manager.instances)
	}
}

func TestEvictOldestNeverStopsActiveOrStartingInstance(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	ready := make(chan struct{})
	close(ready)
	active := &instance{lastUsed: time.Unix(1, 0), ready: ready, leases: 1}
	starting := &instance{lastUsed: time.Unix(2, 0), ready: make(chan struct{})}
	manager.instances = map[string]*instance{"active": active, "starting": starting}
	manager.mu.Lock()
	evicted := manager.evictOldestLocked()
	manager.mu.Unlock()
	if evicted || active.stopped || starting.stopped || len(manager.instances) != 2 {
		t.Fatalf("busy Cursor instances were evicted: evicted=%v active=%+v starting=%+v", evicted, active, starting)
	}
}

func TestAcquireReadyLeaseIsIdempotentlyReleased(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	ready := make(chan struct{})
	close(ready)
	item := &instance{baseURL: "http://127.0.0.1:4321/v1", ready: ready}
	manager.instances = map[string]*instance{"account": item}
	baseURL, release, ok := manager.acquireReady("account", item)
	if !ok || baseURL != item.baseURL || item.leases != 1 {
		t.Fatalf("acquireReady base=%q ok=%v leases=%d", baseURL, ok, item.leases)
	}
	release()
	release()
	if item.leases != 0 {
		t.Fatalf("idempotent release left %d leases", item.leases)
	}
}
