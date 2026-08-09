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

// An empty group must be queryable by reason. Every selection failure used to land under
// `no_public_account_detail` unless a model filter fired, so the one cause an operator can
// fix in a second — the routed group has no accounts — was buried in free-text detail that
// the diagnostics aliaser truncates.
func TestRoutingUnavailableAuditNamesEmptyGroup(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()

	empty := &scheduler.NoAccountError{
		Group: "gpt-pro", AllowedProviders: []string{"codex"}, Model: "gpt-test",
		EmptyPool: true,
	}
	rec := httptest.NewRecorder()
	h.app.writePublicNoAccountError(ctx, rec, http.StatusServiceUnavailable, "gpt-pro", "codex", "gpt-test", empty)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	saturated := &scheduler.NoAccountError{
		Group: "cyber", AllowedProviders: []string{"codex"}, Model: "gpt-test",
		Counters: scheduler.NoAccountCounters{Concurrency: 1},
	}
	rec = httptest.NewRecorder()
	h.app.writePublicNoAccountError(ctx, rec, http.StatusTooManyRequests, "cyber", "codex", "gpt-test", saturated)

	var emptyRows, saturatedRows int
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND reason='group_has_no_accounts'`).Scan(&emptyRows); err != nil {
		t.Fatal(err)
	}
	if emptyRows != 1 {
		t.Fatalf("group_has_no_accounts audit rows=%d, want 1", emptyRows)
	}
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action='routing_unavailable' AND reason='no_public_account_detail'`).Scan(&saturatedRows); err != nil {
		t.Fatal(err)
	}
	if saturatedRows != 1 {
		t.Fatalf("saturation audit rows=%d, want 1 (the two causes must not share a reason)", saturatedRows)
	}
}
