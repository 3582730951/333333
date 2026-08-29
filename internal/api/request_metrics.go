package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// HTTP request metrics deliberately use fixed arrays rather than maps. This keeps
// observation O(1), allocation-free, and immune to unbounded label cardinality from
// user-controlled paths, account IDs, models, or status codes.
const (
	httpRouteInference = iota
	httpRouteAdmin
	httpRouteAuth
	httpRouteUser
	httpRouteHealth
	httpRouteOther
	httpRouteCount
)

const (
	httpStatus1xx = iota
	httpStatus2xx
	httpStatus3xx
	httpStatus4xx
	httpStatus5xx
	httpStatusOther
	httpStatusClassCount
)

var (
	httpRouteNames = [...]string{
		"inference",
		"admin",
		"auth",
		"user",
		"health",
		"other",
	}
	httpStatusClassNames = [...]string{
		"1xx",
		"2xx",
		"3xx",
		"4xx",
		"5xx",
		"other",
	}
	httpLatencyUpperBounds = [...]time.Duration{
		time.Millisecond,
		5 * time.Millisecond,
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2500 * time.Millisecond,
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		0, // +Inf
	}
)

type httpRouteMetrics struct {
	inflight      atomic.Int64
	requests      atomic.Uint64
	requestBytes  atomic.Uint64
	responseBytes atomic.Uint64
	durationNanos atomic.Uint64
	statuses      [httpStatusClassCount]atomic.Uint64
	latencies     [len(httpLatencyUpperBounds)]atomic.Uint64
}

type httpRequestMetrics struct {
	routes [httpRouteCount]httpRouteMetrics
}

type httpLatencyBucketSnapshot struct {
	UpperBoundMillis int64  `json:"upper_bound_ms"`
	Count            uint64 `json:"count"`
}

type httpRouteMetricsSnapshot struct {
	Route         string                      `json:"route"`
	Inflight      int64                       `json:"inflight"`
	Requests      uint64                      `json:"requests"`
	RequestBytes  uint64                      `json:"request_bytes"`
	ResponseBytes uint64                      `json:"response_bytes"`
	DurationNanos uint64                      `json:"duration_nanos"`
	StatusClasses map[string]uint64           `json:"status_classes"`
	Latency       []httpLatencyBucketSnapshot `json:"latency_buckets"`
}

type httpRequestMetricsSnapshot struct {
	Inflight      int64                      `json:"inflight"`
	Requests      uint64                     `json:"requests"`
	RequestBytes  uint64                     `json:"request_bytes"`
	ResponseBytes uint64                     `json:"response_bytes"`
	Routes        []httpRouteMetricsSnapshot `json:"routes"`
}

func classifyHTTPRoute(path string) int {
	switch {
	case isInferenceRequestPath(path), path == "/v1/models", path == "/v1/gateway/identity":
		return httpRouteInference
	case strings.HasPrefix(path, "/admin/"):
		return httpRouteAdmin
	case strings.HasPrefix(path, "/auth/"):
		return httpRouteAuth
	case strings.HasPrefix(path, "/user/"):
		return httpRouteUser
	case path == "/healthz":
		return httpRouteHealth
	default:
		return httpRouteOther
	}
}

func classifyHTTPStatus(status int) int {
	switch status / 100 {
	case 1:
		return httpStatus1xx
	case 2:
		return httpStatus2xx
	case 3:
		return httpStatus3xx
	case 4:
		return httpStatus4xx
	case 5:
		return httpStatus5xx
	default:
		return httpStatusOther
	}
}

func (m *httpRequestMetrics) begin(path string) int {
	route := classifyHTTPRoute(path)
	m.routes[route].inflight.Add(1)
	return route
}

func (m *httpRequestMetrics) finish(route, status int, requestBytes, responseBytes int64, elapsed time.Duration) {
	if route < 0 || route >= len(m.routes) {
		route = httpRouteOther
	}
	current := &m.routes[route]
	current.inflight.Add(-1)
	current.requests.Add(1)
	if requestBytes > 0 {
		current.requestBytes.Add(uint64(requestBytes))
	}
	if responseBytes > 0 {
		current.responseBytes.Add(uint64(responseBytes))
	}
	if elapsed < 0 {
		elapsed = 0
	}
	current.durationNanos.Add(uint64(elapsed))
	current.statuses[classifyHTTPStatus(status)].Add(1)
	for i, upper := range httpLatencyUpperBounds {
		if upper == 0 || elapsed <= upper {
			current.latencies[i].Add(1)
			break
		}
	}
}

func (m *httpRequestMetrics) snapshot() httpRequestMetricsSnapshot {
	out := httpRequestMetricsSnapshot{Routes: make([]httpRouteMetricsSnapshot, 0, httpRouteCount)}
	for route, name := range httpRouteNames {
		current := &m.routes[route]
		row := httpRouteMetricsSnapshot{
			Route:         name,
			Inflight:      current.inflight.Load(),
			Requests:      current.requests.Load(),
			RequestBytes:  current.requestBytes.Load(),
			ResponseBytes: current.responseBytes.Load(),
			DurationNanos: current.durationNanos.Load(),
			StatusClasses: make(map[string]uint64, httpStatusClassCount),
			Latency:       make([]httpLatencyBucketSnapshot, len(httpLatencyUpperBounds)),
		}
		for i, class := range httpStatusClassNames {
			row.StatusClasses[class] = current.statuses[i].Load()
		}
		for i, upper := range httpLatencyUpperBounds {
			upperMillis := int64(-1)
			if upper > 0 {
				upperMillis = upper.Milliseconds()
			}
			row.Latency[i] = httpLatencyBucketSnapshot{
				UpperBoundMillis: upperMillis,
				Count:            current.latencies[i].Load(),
			}
		}
		out.Inflight += row.Inflight
		out.Requests += row.Requests
		out.RequestBytes += row.RequestBytes
		out.ResponseBytes += row.ResponseBytes
		out.Routes = append(out.Routes, row)
	}
	return out
}

// adminMetrics exposes a bounded-cardinality Prometheus text surface for operators.
// It shares the existing admin authentication path and never includes model, account,
// key, tenant, request path, or other user-controlled labels.
func (s *Server) adminMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}

	snapshot := s.httpMetrics.snapshot()
	var out strings.Builder
	out.Grow(16 << 10)
	out.WriteString("# HELP codex_pool_http_requests_total Completed HTTP requests.\n")
	out.WriteString("# TYPE codex_pool_http_requests_total counter\n")
	out.WriteString("# HELP codex_pool_http_inflight_requests Current in-flight HTTP requests.\n")
	out.WriteString("# TYPE codex_pool_http_inflight_requests gauge\n")
	out.WriteString("# HELP codex_pool_http_request_size_bytes_total Accepted request body bytes when known.\n")
	out.WriteString("# TYPE codex_pool_http_request_size_bytes_total counter\n")
	out.WriteString("# HELP codex_pool_http_response_size_bytes_total Response body bytes written through HTTP.\n")
	out.WriteString("# TYPE codex_pool_http_response_size_bytes_total counter\n")
	out.WriteString("# HELP codex_pool_http_request_duration_seconds End-to-end HTTP request duration.\n")
	out.WriteString("# TYPE codex_pool_http_request_duration_seconds histogram\n")

	for _, route := range snapshot.Routes {
		for _, class := range httpStatusClassNames {
			fmt.Fprintf(&out, "codex_pool_http_requests_total{route=%q,status_class=%q} %d\n",
				route.Route, class, route.StatusClasses[class])
		}
		fmt.Fprintf(&out, "codex_pool_http_inflight_requests{route=%q} %d\n", route.Route, route.Inflight)
		fmt.Fprintf(&out, "codex_pool_http_request_size_bytes_total{route=%q} %d\n", route.Route, route.RequestBytes)
		fmt.Fprintf(&out, "codex_pool_http_response_size_bytes_total{route=%q} %d\n", route.Route, route.ResponseBytes)

		var cumulative uint64
		for _, bucket := range route.Latency {
			cumulative += bucket.Count
			le := "+Inf"
			if bucket.UpperBoundMillis >= 0 {
				le = strconv.FormatFloat(float64(bucket.UpperBoundMillis)/1000, 'f', -1, 64)
			}
			fmt.Fprintf(&out, "codex_pool_http_request_duration_seconds_bucket{route=%q,le=%q} %d\n",
				route.Route, le, cumulative)
		}
		fmt.Fprintf(&out, "codex_pool_http_request_duration_seconds_sum{route=%q} %s\n",
			route.Route, strconv.FormatFloat(float64(route.DurationNanos)/float64(time.Second), 'f', -1, 64))
		fmt.Fprintf(&out, "codex_pool_http_request_duration_seconds_count{route=%q} %d\n",
			route.Route, route.Requests)
	}

	deployment := s.deploymentStorageStatus()
	var draining int
	var reaperFailure int
	for _, release := range deployment.Draining {
		switch release.State {
		case "complete", "cancelled":
		default:
			draining++
		}
		if strings.TrimSpace(release.LastError) != "" {
			reaperFailure = 1
		}
	}
	out.WriteString("# HELP codex_pool_deployment_draining_releases Releases still protected by a drain/reaper lifecycle.\n")
	out.WriteString("# TYPE codex_pool_deployment_draining_releases gauge\n")
	fmt.Fprintf(&out, "codex_pool_deployment_draining_releases %d\n", draining)
	out.WriteString("# HELP codex_pool_deployment_release_bytes Immutable release storage bytes.\n")
	out.WriteString("# TYPE codex_pool_deployment_release_bytes gauge\n")
	fmt.Fprintf(&out, "codex_pool_deployment_release_bytes %d\n", deployment.TotalReleaseBytes)
	out.WriteString("# HELP codex_pool_deployment_reaper_failure Whether a retained reaper reports an error.\n")
	out.WriteString("# TYPE codex_pool_deployment_reaper_failure gauge\n")
	fmt.Fprintf(&out, "codex_pool_deployment_reaper_failure %d\n", reaperFailure)
	out.WriteString("# HELP codex_pool_deployment_admission_pause_duration_seconds Last deployment admission-pause duration.\n")
	out.WriteString("# TYPE codex_pool_deployment_admission_pause_duration_seconds gauge\n")
	fmt.Fprintf(&out, "codex_pool_deployment_admission_pause_duration_seconds %s\n",
		strconv.FormatFloat(float64(deployment.AdmissionPauseMillis)/1000, 'f', 3, 64))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out.String()))
}
