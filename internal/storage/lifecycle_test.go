package storage

import (
	"context"
	"testing"
)

func TestLifecycleSchema(t *testing.T) {
	store, err := OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Test that all lifecycle tables were created
	tables := []string{
		"proxy_configs",
		"proxy_usage_records",
		"mailbox_providers",
		"sms_providers",
		"lifecycle_tasks",
		"lifecycle_task_logs",
		"gopay_accounts",
		"paypal_accounts",
		"lifecycle_events",
	}

	for _, table := range tables {
		var name string
		err := store.db.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("Table %s not found: %v", table, err)
		}
	}

	// Test that accounts table has new columns
	columns := []string{
		"registration_method",
		"phone",
		"subscription_status",
		"subscription_expires_at",
		"last_validity_check_at",
		"registration_task_id",
	}

	for _, col := range columns {
		var count int
		err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('accounts') WHERE name=?", col).Scan(&count)
		if err != nil {
			t.Errorf("Error checking column %s: %v", col, err)
		}
		if count == 0 {
			t.Errorf("Column %s not found in accounts table", col)
		}
	}

	t.Log("✓ All lifecycle tables and columns created successfully")
}
