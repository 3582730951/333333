package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPRequestMetricsFixedCardinalitySnapshot(t *testing.T) {
	var metrics httpRequestMetrics
	route := metrics.begin("/v1/responses")
	metrics.finish(route, http.StatusTooManyRequests, 128, 64, 125*time.Millisecond)
	route = metrics.begin("/admin/system")
	metrics.finish(route, http.StatusOK, -1, 512, 2*time.Millisecond)

	snapshot := metrics.snapshot()
	if snapshot.Inflight != 0 || snapshot.Requests != 2 {
		t.Fatalf("snapshot totals = %+v", snapshot)
	}
	if len(snapshot.Routes) != httpRouteCount {
		t.Fatalf("routes = %d, want fixed %d", len(snapshot.Routes), httpRouteCount)
	}
	inference := snapshot.Routes[httpRouteInference]
	if inference.Route != "inference" || inference.Requests != 1 ||
		inference.RequestBytes != 128 || inference.ResponseBytes != 64 ||
		inference.StatusClasses["4xx"] != 1 {
		t.Fatalf("inference snapshot = %+v", inference)
	}
	var bucketTotal uint64
	for _, bucket := range inference.Latency {
		bucketTotal += bucket.Count
	}
	if bucketTotal != inference.Requests {
		t.Fatalf("exclusive latency buckets total = %d, requests = %d", bucketTotal, inference.Requests)
	}
}

func TestAdminMetricsPrometheusOutputIsBoundedAndAuthenticated(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	code, _ := grpReq(t, h, http.MethodGet, "/healthz", "")
	if code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", code)
	}
	code, raw := grpReq(t, h, http.MethodGet, "/admin/metrics", "")
	if code != http.StatusOK {
		t.Fatalf("admin metrics = %d, want 200: %s", code, raw)
	}
	body := string(raw)
	for _, want := range []string{
		"# TYPE codex_pool_http_requests_total counter",
		`codex_pool_http_requests_total{route="health",status_class="2xx"} 1`,
		`codex_pool_http_request_duration_seconds_bucket{route="health",le="+Inf"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/healthz") {
		t.Fatalf("metrics leaked raw request path:\n%s", body)
	}

	h.app.cfg.AdminToken = "metrics-secret"
	code, _ = grpReq(t, h, http.MethodGet, "/admin/metrics", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin metrics = %d, want 401", code)
	}
}

func BenchmarkHTTPRequestMetricsObserve(b *testing.B) {
	var metrics httpRequestMetrics
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		route := metrics.begin("/v1/responses")
		metrics.finish(route, http.StatusOK, 1024, 4096, 10*time.Millisecond)
	}
}
