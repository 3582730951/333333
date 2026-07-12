package storage

import (
	"context"
	"testing"
)

func TestLogRetentionAndManualClearPreserveActiveBillingHolds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	cutoff := int64(1_000_000)
	oldAt, newAt := cutoff-1, cutoff+1
	if err := store.UpsertAccount(ctx, Account{ID: "log-account", GroupName: "cyber", Status: "active"}, AccountToken{}); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []interface{}
	}{
		{`INSERT INTO audit_log(action, created_at) VALUES('old', ?), ('new', ?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO cf_events(account_id, egress_id, status, category, created_at) VALUES('log-account','egress_direct',403,'old',?), ('log-account','egress_direct',200,'new',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO usage_records(account_id, route_key_hash, model, created_at) VALUES('log-account','old','gpt',?), ('log-account','new','gpt',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO registration_task_events(task_id, message, created_at) VALUES('old','old',?), ('new','new',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO lifecycle_tasks(id, task_type, created_at) VALUES('log-task','test',?)`, []interface{}{oldAt}},
		{`INSERT INTO lifecycle_task_logs(task_id, message, timestamp) VALUES('log-task','old',?), ('log-task','new',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO lifecycle_events(account_id, event_type, timestamp) VALUES('log-account','old',?), ('log-account','new',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO proxy_configs(id, name, proxy_type, proxy_provider, created_at, updated_at) VALUES('log-proxy','log','fixed','test',?,?)`, []interface{}{oldAt, oldAt}},
		{`INSERT INTO proxy_usage_records(proxy_config_id, account_id, task_id, created_at) VALUES('log-proxy','log-account','old',?), ('log-proxy','log-account','new',?)`, []interface{}{oldAt, newAt}},
		{`INSERT INTO billing_holds(id, account_id, status, created_at, updated_at) VALUES('terminal-old','log-account','settled',?,?), ('terminal-new','log-account','settled',?,?), ('active-old','log-account','held',?,?)`, []interface{}{oldAt, oldAt, newAt, newAt, oldAt, oldAt}},
	}
	for _, statement := range statements {
		if _, err := store.DB().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed logs: %v\n%s", err, statement.query)
		}
	}

	counts, err := store.PurgeLogRecordsBefore(ctx, cutoff, 1)
	if err != nil {
		t.Fatal(err)
	}
	if counts.AuditLog != 1 || counts.CFEvents != 1 || counts.UsageRecords != 1 ||
		counts.RegistrationTaskEvents != 1 || counts.LifecycleTaskLogs != 1 ||
		counts.LifecycleEvents != 1 || counts.ProxyUsageRecords != 1 || counts.TerminalBillingHolds != 1 || counts.Total() != 8 {
		t.Fatalf("retention counts = %+v total=%d", counts, counts.Total())
	}
	assertTableCount(t, store, "audit_log", 1)
	assertTableCount(t, store, "cf_events", 1)
	assertTableCount(t, store, "usage_records", 1)
	assertTableCount(t, store, "registration_task_events", 1)
	assertTableCount(t, store, "lifecycle_task_logs", 1)
	assertTableCount(t, store, "lifecycle_events", 1)
	assertTableCount(t, store, "proxy_usage_records", 1)
	assertTableCount(t, store, "billing_holds", 2)
	var activeStatus string
	if err := store.DB().QueryRowContext(ctx, `SELECT status FROM billing_holds WHERE id='active-old'`).Scan(&activeStatus); err != nil || activeStatus != "held" {
		t.Fatalf("active hold was not preserved: status=%q err=%v", activeStatus, err)
	}

	cleared, err := store.ClearLogRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.PreservedActiveBillingHolds != 1 || cleared.Deleted.Total() != 8 {
		t.Fatalf("manual clear result = %+v", cleared)
	}
	for _, table := range []string{"audit_log", "cf_events", "usage_records", "registration_task_events", "lifecycle_task_logs", "lifecycle_events", "proxy_usage_records"} {
		assertTableCount(t, store, table, 0)
	}
	assertTableCount(t, store, "billing_holds", 1)
	if err := store.ReclaimLogStorage(ctx); err != nil {
		t.Fatalf("reclaim log storage: %v", err)
	}
}

func assertTableCount(t *testing.T, store *Store, table string, want int64) {
	t.Helper()
	var got int64
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("count %s = %d, want %d", table, got, want)
	}
}
