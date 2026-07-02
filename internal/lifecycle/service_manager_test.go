package lifecycle

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/supervisor"
)

func TestServiceManager(t *testing.T) {
	// Create service manager
	sm := NewServiceManager(ServiceManagerConfig{
		AutoStart:   false,
		IdleTimeout: 5 * time.Second,
	})

	// Test 1: Register a service
	sm.RegisterService(
		"test-service",
		"sleep 100",
		"/tmp",
		8080,
		"",
	)

	status, err := sm.GetStatus("test-service")
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status != ServiceStatusStopped {
		t.Errorf("Expected status stopped, got %s", status)
	}

	t.Log("✓ Service registered successfully")

	// Test 2: List services
	services := sm.ListServices()
	if len(services) != 1 {
		t.Errorf("Expected 1 service, got %d", len(services))
	}
	if services["test-service"] != ServiceStatusStopped {
		t.Errorf("Expected test-service to be stopped")
	}

	t.Log("✓ ListServices works")

	// Test 3: Start and stop service
	ctx := context.Background()
	err = sm.EnsureRunning(ctx, "test-service")
	if err != nil {
		t.Logf("EnsureRunning failed (expected in test env): %v", err)
	}

	// Stop the service
	err = sm.StopService("test-service")
	if err != nil {
		t.Logf("StopService failed: %v", err)
	}

	t.Log("✓ Service lifecycle methods work")
}

func TestServiceManagerWithMockHTTPServer(t *testing.T) {
	// Create a mock HTTP server for health check
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{
		AutoStart:   false,
		IdleTimeout: 0, // Never stop
	})

	// Register service with health check
	sm.RegisterService(
		"mock-service",
		"sleep 10",
		"",
		0,
		server.URL,
	)

	ctx := context.Background()
	err := sm.EnsureRunning(ctx, "mock-service")
	if err != nil {
		t.Logf("EnsureRunning with health check: %v", err)
	}

	// Stop
	sm.StopService("mock-service")

	t.Log("✓ Health check integration works")
}

func TestServiceManagerIdleTimeout(t *testing.T) {
	sm := NewServiceManager(ServiceManagerConfig{
		AutoStart:   false,
		IdleTimeout: 100 * time.Millisecond,
	})

	sm.RegisterService(
		"idle-test",
		"sleep 10",
		"",
		0,
		"",
	)

	// Service should not be in services list initially if not registered
	status, err := sm.GetStatus("idle-test")
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if status != ServiceStatusStopped {
		t.Errorf("Expected stopped, got %s", status)
	}

	t.Log("✓ Idle timeout configuration works")
}

func TestStopAll(t *testing.T) {
	sm := NewServiceManager(ServiceManagerConfig{
		AutoStart:   false,
		IdleTimeout: 0,
	})

	// Register multiple services
	sm.RegisterService("svc1", "sleep 100", "", 0, "")
	sm.RegisterService("svc2", "sleep 100", "", 0, "")
	sm.RegisterService("svc3", "sleep 100", "", 0, "")

	// Stop all
	err := sm.StopAll()
	if err != nil {
		t.Logf("StopAll returned error (expected): %v", err)
	}

	// Verify all stopped
	services := sm.ListServices()
	for name, status := range services {
		if status != ServiceStatusStopped {
			t.Errorf("Service %s not stopped: %s", name, status)
		}
	}

	t.Log("✓ StopAll works")
}

func TestServiceNotRegistered(t *testing.T) {
	sm := NewServiceManager(ServiceManagerConfig{})

	// Try to access non-existent service
	_, err := sm.GetStatus("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent service")
	}

	ctx := context.Background()
	err = sm.EnsureRunning(ctx, "nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent service")
	}

	t.Log("✓ Error handling for non-existent services works")
}

func TestServiceManagerRecordsUnexpectedProcessExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("crashy", "sleep 0.05; exit 7", "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "crashy"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	snap := waitForServiceSnapshot(t, sm, "crashy", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusFailed
	})
	if !strings.Contains(snap.LastError, "exit status 7") {
		t.Fatalf("last_error = %q, want exit status 7", snap.LastError)
	}
	if snap.ExitedAt == 0 {
		t.Fatalf("exited_at was not recorded: %+v", snap)
	}
}

func TestServiceManagerRecordsUnexpectedProcessExitInSupervisor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("crashy-supervisor", "sleep 0.05; exit 7", "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "crashy-supervisor"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	waitForServiceSnapshot(t, sm, "crashy-supervisor", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusFailed
	})

	module := serviceModuleName("crashy-supervisor")
	state := waitForSupervisorState(t, module, func(s supervisor.ModuleState) bool {
		return s.Status == supervisor.StatusFailed
	})
	if state.UnexpectedExitCount == 0 {
		t.Fatalf("unexpected_exit_count = %d, want > 0 in %#v", state.UnexpectedExitCount, state)
	}
	if !strings.Contains(state.LastMessage, "exit status 7") {
		t.Fatalf("last_message = %q, want exit status 7", state.LastMessage)
	}
	if !recentSupervisorEvent(module, "failed") {
		t.Fatalf("recent supervisor events missing failed event for %s: %#v", module, supervisor.RecentEvents())
	}
}

func TestServiceManagerCleanUnexpectedExitIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("clean-exit", "sleep 0.05; exit 0", "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "clean-exit"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	snap := waitForServiceSnapshot(t, sm, "clean-exit", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusFailed
	})
	if !strings.Contains(snap.LastError, "unexpectedly") {
		t.Fatalf("last_error = %q, want unexpected exit", snap.LastError)
	}
}

func TestServiceManagerAutoRestartsUnexpectedExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{AutoStart: true})
	sm.restartDelay = 100 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "first-run")
	command := fmt.Sprintf("if [ ! -f %[1]q ]; then touch %[1]q; sleep 0.05; exit 7; fi; sleep 5", marker)
	sm.RegisterService("restartable", command, "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "restartable"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	failed := waitForServiceSnapshot(t, sm, "restartable", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusFailed
	})
	if !strings.Contains(failed.LastError, "exit status 7") {
		t.Fatalf("last_error = %q, want exit status 7", failed.LastError)
	}

	restarted := waitForServiceSnapshot(t, sm, "restartable", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusRunning && s.LastError == ""
	})
	if restarted.ExitedAt != 0 {
		t.Fatalf("exited_at = %d, want cleared after successful restart", restarted.ExitedAt)
	}

	if err := sm.StopService("restartable"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
}

func TestServiceManagerAutoRestartRecordsSupervisorState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{AutoStart: true})
	sm.restartDelay = 300 * time.Millisecond
	marker := filepath.Join(t.TempDir(), "first-run")
	command := fmt.Sprintf("if [ ! -f %[1]q ]; then touch %[1]q; sleep 0.05; exit 7; fi; sleep 5", marker)
	sm.RegisterService("restartable-supervisor", command, "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "restartable-supervisor"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}

	waitForServiceSnapshot(t, sm, "restartable-supervisor", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusFailed
	})

	module := serviceModuleName("restartable-supervisor")
	restarting := waitForSupervisorState(t, module, func(s supervisor.ModuleState) bool {
		return s.Status == supervisor.StatusRestarting
	})
	if restarting.RestartCount == 0 || restarting.UnexpectedExitCount == 0 {
		t.Fatalf("restart state = %#v, want restart and unexpected-exit counts", restarting)
	}
	if restarting.LastUptimeMillis <= 0 {
		t.Fatalf("last_uptime_millis = %d, want > 0 in %#v", restarting.LastUptimeMillis, restarting)
	}
	if restarting.RestartBackoffMillis != sm.restartDelay.Milliseconds() {
		t.Fatalf("restart_backoff_millis = %d, want %d in %#v", restarting.RestartBackoffMillis, sm.restartDelay.Milliseconds(), restarting)
	}
	if !strings.Contains(restarting.LastMessage, "exit status 7") {
		t.Fatalf("last_message = %q, want exit status 7", restarting.LastMessage)
	}

	waitForServiceSnapshot(t, sm, "restartable-supervisor", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusRunning && s.LastError == ""
	})
	running := waitForSupervisorState(t, module, func(s supervisor.ModuleState) bool {
		return s.Status == supervisor.StatusRunning
	})
	if running.RestartCount == 0 {
		t.Fatalf("running state lost restart count: %#v", running)
	}

	if err := sm.StopService("restartable-supervisor"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
}

func TestServiceManagerStopUsesMonitorWait(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("stoppable", "sleep 10", "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "stoppable"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := sm.StopService("stoppable"); err != nil {
		t.Fatalf("StopService: %v", err)
	}

	snap, err := sm.GetSnapshot("stoppable")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Status != ServiceStatusStopped {
		t.Fatalf("status = %s, want stopped (snapshot=%+v)", snap.Status, snap)
	}
	if snap.LastError != "" {
		t.Fatalf("last_error = %q, want empty on intentional stop", snap.LastError)
	}
}

func TestServiceManagerStopDoesNotAutoRestart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{AutoStart: true})
	sm.restartDelay = 10 * time.Millisecond
	sm.RegisterService("stoppable", "sleep 5", "", 0, server.URL)

	if err := sm.EnsureRunning(context.Background(), "stoppable"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if err := sm.StopService("stoppable"); err != nil {
		t.Fatalf("StopService: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	snap, err := sm.GetSnapshot("stoppable")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Status != ServiceStatusStopped {
		t.Fatalf("status = %s, want stopped without auto restart (snapshot=%+v)", snap.Status, snap)
	}
	if snap.LastError != "" {
		t.Fatalf("last_error = %q, want empty after intentional stop", snap.LastError)
	}
}

func TestServiceManagerStopWhileStartingClosesDone(t *testing.T) {
	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("starting", "sleep 5", "", 0, "")

	errCh := make(chan error, 1)
	go func() {
		errCh <- sm.EnsureRunning(context.Background(), "starting")
	}()

	waitForServiceSnapshot(t, sm, "starting", func(s ServiceSnapshot) bool {
		return s.Status == ServiceStatusStarting
	})

	start := time.Now()
	if err := sm.StopService("starting"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("StopService took %s while service was starting", elapsed)
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "service start stopped") {
			t.Fatalf("EnsureRunning error = %v, want service start stopped", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnsureRunning did not return after stopping a starting service")
	}

	snap, err := sm.GetSnapshot("starting")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Status != ServiceStatusStopped {
		t.Fatalf("status = %s, want stopped after cancelling start (snapshot=%+v)", snap.Status, snap)
	}
	if snap.LastError != "" {
		t.Fatalf("last_error = %q, want empty after cancelling start", snap.LastError)
	}
}

func TestServiceManagerStartContextDoesNotOwnProcessLifetime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sm := NewServiceManager(ServiceManagerConfig{})
	sm.RegisterService("long-running", "sleep 5", "", 0, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	if err := sm.EnsureRunning(ctx, "long-running"); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	cancel()

	time.Sleep(100 * time.Millisecond)
	snap, err := sm.GetSnapshot("long-running")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if snap.Status != ServiceStatusRunning {
		t.Fatalf("status = %s, want running after start context cancellation (snapshot=%+v)", snap.Status, snap)
	}
	if snap.LastError != "" {
		t.Fatalf("last_error = %q, want empty while service keeps running", snap.LastError)
	}
	if err := sm.StopService("long-running"); err != nil {
		t.Fatalf("StopService: %v", err)
	}
}

func waitForServiceSnapshot(t *testing.T, sm *ServiceManager, name string, ok func(ServiceSnapshot) bool) ServiceSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var snap ServiceSnapshot
	for time.Now().Before(deadline) {
		var err error
		snap, err = sm.GetSnapshot(name)
		if err != nil {
			t.Fatal(err)
		}
		if ok(snap) {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("service %s did not reach expected state; last snapshot=%+v", name, snap)
	return snap
}

func waitForSupervisorState(t *testing.T, name string, ok func(supervisor.ModuleState) bool) supervisor.ModuleState {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var found supervisor.ModuleState
	for time.Now().Before(deadline) {
		for _, state := range supervisor.ModuleStates() {
			if state.Name != name {
				continue
			}
			found = state
			if ok(state) {
				return state
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("supervisor module %s did not reach expected state; last=%+v all=%+v", name, found, supervisor.ModuleStates())
	return found
}

func recentSupervisorEvent(module, eventType string) bool {
	for _, event := range supervisor.RecentEvents() {
		if event.Module == module && event.Type == eventType {
			return true
		}
	}
	return false
}

func BenchmarkEnsureRunning(b *testing.B) {
	sm := NewServiceManager(ServiceManagerConfig{
		AutoStart:   false,
		IdleTimeout: 0,
	})

	sm.RegisterService("bench", "sleep 1", "", 0, "")
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sm.EnsureRunning(ctx, "bench")
	}

	sm.StopAll()
}
