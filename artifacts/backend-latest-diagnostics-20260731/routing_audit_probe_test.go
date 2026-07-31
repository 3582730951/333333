package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/scheduler"
)

func TestRoutingAuditBehaviorProbe(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	noAccount := &scheduler.NoAccountError{
		Group: "probe", AllowedProviders: []string{"codex"}, Model: "gpt-test",
		Counters: scheduler.NoAccountCounters{Concurrency: 1},
	}
	for i := 0; i < 3; i++ {
		h.app.writePublicNoAccountError(ctx, httptest.NewRecorder(), http.StatusTooManyRequests, "probe", "codex", "gpt-test", noAccount)
	}
	for i := 0; i < 2; i++ {
		h.app.writePublicNoAccountError(ctx, httptest.NewRecorder(), http.StatusConflict, "probe", "codex", "gpt-test", scheduler.ErrStrictUnavailable)
	}
	var non409, strict409 int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND detail LIKE '%status=429%'`).Scan(&non409); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND detail LIKE '%status=409%'`).Scan(&strict409); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("ROUTING_AUDIT_PROBE non409_requests=3 non409_audits=%d strict409_requests=2 strict409_audits=%d\n", non409, strict409)
}
