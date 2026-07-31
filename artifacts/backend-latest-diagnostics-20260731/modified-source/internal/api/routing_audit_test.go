package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/scheduler"
)

func TestRoutingUnavailableAuditCoalescesOnlyNon409Repetitions(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	err := &scheduler.NoAccountError{
		Group: "fixture", AllowedProviders: []string{"codex"}, Model: "gpt-test",
		Counters: scheduler.NoAccountCounters{Concurrency: 1},
	}
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.app.writePublicNoAccountError(ctx, rec, http.StatusTooManyRequests, "fixture", "codex", "gpt-test", err)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("response %d status=%d, want %d", i, rec.Code, http.StatusTooManyRequests)
		}
	}
	var nonConflict int
	if queryErr := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND detail LIKE '%status=429%'`).Scan(&nonConflict); queryErr != nil {
		t.Fatal(queryErr)
	}
	if nonConflict != 1 {
		t.Fatalf("non-409 routing audits=%d, want 1", nonConflict)
	}
	if got := h.app.routingAuditDiagnostics()["suppressed_repetitions"]; got != uint64(2) {
		t.Fatalf("suppressed repetitions=%v, want 2", got)
	}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.app.writePublicNoAccountError(ctx, rec, http.StatusConflict, "fixture", "codex", "gpt-test", scheduler.ErrStrictUnavailable)
		if rec.Code != http.StatusConflict {
			t.Fatalf("strict response %d status=%d, want %d", i, rec.Code, http.StatusConflict)
		}
	}
	var conflicts int
	if queryErr := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND detail LIKE '%status=409%'`).Scan(&conflicts); queryErr != nil {
		t.Fatal(queryErr)
	}
	if conflicts != 2 {
		t.Fatalf("strict 409 routing audits=%d, want 2", conflicts)
	}
}
