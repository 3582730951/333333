package api

import (
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
)

const (
	clientPerformanceLimit      = 12
	clientPerformanceWindow     = 5 * time.Minute
	clientPerformanceMaxClients = 4096
)

// clientPerformanceReport is deliberately aggregate-only. It has no route,
// account, label, input, URL, token or browser-storage field, so performance
// diagnostics cannot become a shadow analytics stream.
type clientPerformanceReport struct {
	LCPMillis             float64 `json:"lcp_ms"`
	CLS                   float64 `json:"cls"`
	INPMillis             float64 `json:"inp_ms"`
	TTFBMillis            float64 `json:"ttfb_ms"`
	LongTaskCount         int64   `json:"long_task_count"`
	LongTaskMillis        float64 `json:"long_task_ms"`
	RouteIntentCommit     float64 `json:"route_intent_commit_ms"`
	RouteCommitDataReady  float64 `json:"route_commit_data_ready_ms"`
	MutationAcceptMillis  float64 `json:"mutation_accept_ms"`
	MutationSettledMillis float64 `json:"mutation_settled_ms"`
	SampledAt             int64   `json:"sampled_at"`
}

func finiteMetric(value, ceiling float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0
	}
	if value > ceiling {
		return ceiling
	}
	return math.Round(value*100) / 100
}

func (s *Server) handleClientPerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	remote := s.clientIP(r)
	if s.clientPerformance != nil {
		allowed, _ := s.clientPerformance.allowWithLimitLog(remote, time.Now())
		if !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(s.clientPerformance.retryAfterSeconds()))
			writeError(w, http.StatusTooManyRequests, errors.New("too many client performance reports; try again later"))
			return
		}
	}
	var report clientPerformanceReport
	if err := decodeJSONRequestBody(r.Body, &report, 4*1024); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	report.LCPMillis = finiteMetric(report.LCPMillis, 120_000)
	report.CLS = finiteMetric(report.CLS, 100)
	report.INPMillis = finiteMetric(report.INPMillis, 120_000)
	report.TTFBMillis = finiteMetric(report.TTFBMillis, 120_000)
	report.LongTaskMillis = finiteMetric(report.LongTaskMillis, 600_000)
	report.RouteIntentCommit = finiteMetric(report.RouteIntentCommit, 120_000)
	report.RouteCommitDataReady = finiteMetric(report.RouteCommitDataReady, 120_000)
	report.MutationAcceptMillis = finiteMetric(report.MutationAcceptMillis, 120_000)
	report.MutationSettledMillis = finiteMetric(report.MutationSettledMillis, 600_000)
	if report.LongTaskCount < 0 {
		report.LongTaskCount = 0
	} else if report.LongTaskCount > 100_000 {
		report.LongTaskCount = 100_000
	}
	if report.SampledAt <= 0 {
		report.SampledAt = time.Now().Unix()
	}
	log.Printf("[CLIENT-PERFORMANCE] request_id=%q lcp_ms=%.2f cls=%.3f inp_ms=%.2f ttfb_ms=%.2f long_tasks=%d long_task_ms=%.2f route_intent_commit_ms=%.2f route_commit_data_ready_ms=%.2f mutation_accept_ms=%.2f mutation_settled_ms=%.2f sampled_at=%d",
		requestIDFromContext(r.Context()), report.LCPMillis, report.CLS,
		report.INPMillis, report.TTFBMillis, report.LongTaskCount, report.LongTaskMillis,
		report.RouteIntentCommit, report.RouteCommitDataReady, report.MutationAcceptMillis,
		report.MutationSettledMillis, report.SampledAt)
	w.WriteHeader(http.StatusNoContent)
}
