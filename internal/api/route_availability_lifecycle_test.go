package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/supervisor"
)

func TestRouteAvailabilityRestartsAcrossActiveRoleGenerations(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	index := newRouteAvailabilityIndex(h.app.scheduler, h.store)
	route := scheduler.Route{Group: "missing", Provider: "codex", Model: "gpt-lifecycle-test"}
	index.track(routeAvailabilityKey(route), route)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	index.Start(firstCtx)
	waitForRouteAvailabilityScans(t, index, 1)

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	index.Start(secondCtx)
	waitForRouteAvailabilityScans(t, index, 2)
	cancelFirst()
	if status := routeAvailabilityModuleStatus(); status != supervisor.StatusRunning {
		t.Fatalf("replacement route availability status = %q, want running", status)
	}

	cancelSecond()
	deadline := time.Now().Add(time.Second)
	for routeAvailabilityModuleStatus() != supervisor.StatusStopped && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if status := routeAvailabilityModuleStatus(); status != supervisor.StatusStopped {
		t.Fatalf("cancelled route availability status = %q, want stopped", status)
	}
}

func waitForRouteAvailabilityScans(t *testing.T, index *routeAvailabilityIndex, want uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for index.scans.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := index.scans.Load(); got < want {
		t.Fatalf("route availability scans = %d, want at least %d", got, want)
	}
}

func routeAvailabilityModuleStatus() string {
	for _, state := range supervisor.ModuleStates() {
		if state.Name == "user-group-route-availability" {
			return state.Status
		}
	}
	return ""
}
