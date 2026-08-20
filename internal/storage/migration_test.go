package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestInitMigratesLegacyAffinityBindingExpiryBeforeCreatingIndex(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// This is the affinity_bindings schema shipped immediately before expires_at
	// was added. Init must add the column before creating an index that uses it.
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE affinity_bindings(
  route_key_hash TEXT PRIMARY KEY,
  route_key TEXT NOT NULL,
  source TEXT NOT NULL,
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  egress_id TEXT NOT NULL DEFAULT '',
  epoch INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy affinity table: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("migrate legacy store: %v", err)
	}

	var columnCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('affinity_bindings') WHERE name='expires_at'`).Scan(&columnCount); err != nil {
		t.Fatalf("inspect migrated column: %v", err)
	}
	if columnCount != 1 {
		t.Fatalf("expires_at column count = %d, want 1", columnCount)
	}

	var indexCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_affinity_bindings_expiry'`).Scan(&indexCount); err != nil {
		t.Fatalf("inspect migrated index: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("expiry index count = %d, want 1", indexCount)
	}
	for _, name := range []string{
		"idx_affinity_bindings_source_expiry",
		"idx_affinity_bindings_updated",
		"idx_affinity_aliases_updated",
		"idx_billing_holds_created",
		"idx_audit_log_action_time",
		"idx_audit_log_state_time",
		"idx_goal_session_updated",
	} {
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&indexCount); err != nil {
			t.Fatalf("inspect diagnostic/retention index %s: %v", name, err)
		}
		if indexCount != 1 {
			t.Fatalf("diagnostic/retention index %s count=%d, want 1", name, indexCount)
		}
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("second init should remain idempotent: %v", err)
	}
}

func TestInitAddsBillingHoldColumnBeforeRecoveryIndex(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "legacy-usage.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Base usage schema from before billing holds and usage diagnostics existed.
	// schemaSQL runs before migrate(), so the recovery index must stay in the
	// ordered migration list after ALTER TABLE adds billing_hold_id.
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE usage_records(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  route_key_hash TEXT NOT NULL,
  model TEXT NOT NULL,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  raw_usage_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy usage table: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("migrate legacy usage store: %v", err)
	}
	var columnCount, indexCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('usage_records') WHERE name='billing_hold_id'`).Scan(&columnCount); err != nil {
		t.Fatalf("inspect billing_hold_id: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_usage_records_billing_hold'`).Scan(&indexCount); err != nil {
		t.Fatalf("inspect billing recovery index: %v", err)
	}
	if columnCount != 1 || indexCount != 1 {
		t.Fatalf("billing_hold_id columns=%d indexes=%d, want 1/1", columnCount, indexCount)
	}
}

func TestInitAddsTrafficFallbackColumnsToLegacyUserGroups(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "legacy-user-groups.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE user_groups(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  system_prompt TEXT NOT NULL DEFAULT '',
  prompt_mode TEXT NOT NULL DEFAULT 'prepend',
  system_prompt_apply_to_compaction INTEGER NOT NULL DEFAULT 1,
  model_instructions_enabled INTEGER NOT NULL DEFAULT 0,
  model_instructions_files TEXT NOT NULL DEFAULT '[]',
  model_instruction_profiles TEXT NOT NULL DEFAULT '{}',
  force_model TEXT NOT NULL DEFAULT '',
  force_effort TEXT NOT NULL DEFAULT '',
  block_claude_target_groups TEXT NOT NULL DEFAULT '[]',
  block_gpt_target_groups TEXT NOT NULL DEFAULT '[]',
  model_routing_json TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`); err != nil {
		t.Fatalf("create legacy user_groups: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO user_groups(id,name,created_at,updated_at) VALUES('ug_legacy_fallback','legacy fallback',1,1)`); err != nil {
		t.Fatalf("seed legacy user group: %v", err)
	}

	if err := store.Init(ctx); err != nil {
		t.Fatalf("migrate legacy user groups: %v", err)
	}
	for _, column := range []string{"traffic_fallback_groups_json", "traffic_fallback_model_mappings_json"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('user_groups') WHERE name=?`, column).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", column, err)
		}
		if count != 1 {
			t.Fatalf("%s column count=%d, want 1", column, count)
		}
	}
	group, found, err := store.GetUserGroup(ctx, "ug_legacy_fallback")
	if err != nil || !found {
		t.Fatalf("read migrated user group found=%v err=%v", found, err)
	}
	if len(group.TrafficFallbackGroups.GPT) != 0 ||
		len(group.TrafficFallbackGroups.Claude) != 0 ||
		len(group.TrafficFallbackGroups.Gemini) != 0 ||
		len(group.TrafficFallbackModelMappings) != 0 {
		t.Fatalf("legacy fallback defaults changed: groups=%+v mappings=%+v", group.TrafficFallbackGroups, group.TrafficFallbackModelMappings)
	}
}
