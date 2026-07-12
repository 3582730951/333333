package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminLogClearDeletesHistoryAndPreservesActiveHolds(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	account := storage.Account{ID: "log-clear-account", GroupName: "cyber", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()
	for _, statement := range []string{
		`INSERT INTO audit_log(action, created_at) VALUES('clear-me', ` + itoa64(now) + `)`,
		`INSERT INTO usage_records(account_id, route_key_hash, model, created_at) VALUES('log-clear-account','clear-me','gpt', ` + itoa64(now) + `)`,
		`INSERT INTO billing_holds(id, account_id, status, created_at, updated_at) VALUES('clear-terminal','log-clear-account','settled', ` + itoa64(now) + `, ` + itoa64(now) + `)`,
		`INSERT INTO billing_holds(id, account_id, status, created_at, updated_at) VALUES('keep-active','log-clear-account','held', ` + itoa64(now) + `, ` + itoa64(now) + `)`,
	} {
		if _, err := h.store.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed log clear: %v\n%s", err, statement)
		}
	}

	code, raw := grpReq(t, h, http.MethodDelete, "/admin/logs", "")
	if code != http.StatusOK {
		t.Fatalf("clear logs status=%d body=%s", code, raw)
	}
	var response struct {
		DeletedTotal                int64                   `json:"deleted_total"`
		Deleted                     storage.LogRecordCounts `json:"deleted"`
		PreservedActiveBillingHolds int64                   `json:"preserved_active_billing_holds"`
		SpaceReclaimed              bool                    `json:"space_reclaimed"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response.DeletedTotal < 3 || response.Deleted.AuditLog < 1 || response.Deleted.UsageRecords < 1 || response.Deleted.TerminalBillingHolds != 1 || response.PreservedActiveBillingHolds != 1 || !response.SpaceReclaimed {
		t.Fatalf("clear response=%+v body=%s", response, raw)
	}
	var active, terminal int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_holds WHERE id='keep-active' AND status='held'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM billing_holds WHERE id='clear-terminal'`).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if active != 1 || terminal != 0 {
		t.Fatalf("billing holds after clear: active=%d terminal=%d", active, terminal)
	}
}

func TestAdminLogClearRejectsNonDelete(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if code, _ := grpReq(t, h, http.MethodGet, "/admin/logs", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /admin/logs status=%d, want 405", code)
	}
}

func TestLogRetentionDefaultsToSevenDaysAcrossUnifiedLogs(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	now := int64(2_000_000)
	oldAt := now - 8*24*60*60
	recentAt := now - 6*24*60*60
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO audit_log(action, created_at) VALUES('expired', ?), ('recent', ?)`, oldAt, recentAt); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.DB().ExecContext(ctx, `INSERT INTO registration_task_events(task_id, message, created_at) VALUES('expired','old',?), ('recent','new',?)`, oldAt, recentAt); err != nil {
		t.Fatal(err)
	}
	counts, err := h.app.runLogRetentionCleanup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if counts.AuditLog != 1 || counts.RegistrationTaskEvents != 1 || h.app.logRetentionDays(ctx) != 7 {
		t.Fatalf("seven-day cleanup counts=%+v retention=%d", counts, h.app.logRetentionDays(ctx))
	}
	for _, table := range []string{"audit_log", "registration_task_events"} {
		var count int
		if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("remaining %s=%d err=%v", table, count, err)
		}
	}
}
