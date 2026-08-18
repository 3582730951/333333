package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUsageDashboardResponseCacheFreshAndStaleRefresh(t *testing.T) {
	cache := newUsageDashboardResponseCache()
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	var renders atomic.Int64
	refreshed := make(chan struct{}, 1)
	render := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := renders.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"render":%d}`, count)
		if count == 2 {
			refreshed <- struct{}{}
		}
	})
	request := httptest.NewRequest(http.MethodGet, "/admin/usage/dashboard?period=24h", nil)

	first := httptest.NewRecorder()
	cache.Serve(first, request, render)
	if got := first.Header().Get("X-Pool-Dashboard-Cache"); got != "miss" {
		t.Fatalf("first cache state = %q", got)
	}
	second := httptest.NewRecorder()
	cache.Serve(second, request, render)
	if got := second.Header().Get("X-Pool-Dashboard-Cache"); got != "fresh" || renders.Load() != 1 {
		t.Fatalf("fresh state=%q renders=%d", got, renders.Load())
	}

	now = now.Add(cache.fresh + time.Second)
	stale := httptest.NewRecorder()
	cache.Serve(stale, request, render)
	if got := stale.Header().Get("X-Pool-Dashboard-Cache"); got != "stale" {
		t.Fatalf("stale cache state = %q", got)
	}
	select {
	case <-refreshed:
	case <-time.After(2 * time.Second):
		t.Fatal("background refresh did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for renders.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	latest := httptest.NewRecorder()
	cache.Serve(latest, request, render)
	if body := latest.Body.String(); body != `{"render":2}` {
		t.Fatalf("refreshed body = %s", body)
	}
}

func TestUsageDashboardResponseCachePropagatesSafeFailureClass(t *testing.T) {
	cache := newUsageDashboardResponseCache()
	request := httptest.NewRequest(http.MethodGet, "/admin/usage/dashboard", nil)
	underlying := httptest.NewRecorder()
	recorder := &responseRecorder{ResponseWriter: underlying, status: http.StatusOK}
	cache.Serve(recorder, request, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusInternalServerError, errors.New("database is locked (5)"))
	})
	if underlying.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", underlying.Code, underlying.Body.String())
	}
	if recorder.diagnosticErrClass != "database_busy" {
		t.Fatalf("diagnostic error class=%q", recorder.diagnosticErrClass)
	}
}
