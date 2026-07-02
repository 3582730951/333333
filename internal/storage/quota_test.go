package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestUsageTimeseriesBuckets(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	// Three records: two in the same hourly bucket, one in the next.
	base := int64(1_700_000_000)
	base = (base / 3600) * 3600 // align to a bucket boundary
	recs := []struct {
		at                              int64
		prompt, completion, total, cach int64
	}{
		{base + 10, 100, 50, 150, 20},
		{base + 200, 200, 100, 300, 40},
		{base + 3600 + 5, 10, 5, 15, 1},
	}
	for _, rec := range recs {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO usage_records(account_id, route_key_hash, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, raw_usage_json, created_at) VALUES('acc','','m',?,?,?,?, '{}', ?)`,
			rec.prompt, rec.completion, rec.total, rec.cach, rec.at); err != nil {
			t.Fatalf("insert usage: %v", err)
		}
	}
	buckets, err := store.UsageTimeseries(ctx, base, 3600)
	if err != nil {
		t.Fatalf("timeseries: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("want 2 buckets, got %d: %+v", len(buckets), buckets)
	}
	if buckets[0].Bucket != base {
		t.Fatalf("bucket[0] start = %d, want %d", buckets[0].Bucket, base)
	}
	if buckets[0].Requests != 2 || buckets[0].PromptTokens != 300 || buckets[0].TotalTokens != 450 || buckets[0].CachedTokens != 60 {
		t.Fatalf("bucket[0] rollup wrong: %+v", buckets[0])
	}
	if buckets[1].Requests != 1 || buckets[1].TotalTokens != 15 {
		t.Fatalf("bucket[1] rollup wrong: %+v", buckets[1])
	}
	// since filter should exclude the earlier bucket.
	only, err := store.UsageTimeseries(ctx, base+3600, 3600)
	if err != nil {
		t.Fatalf("timeseries since: %v", err)
	}
	if len(only) != 1 || only[0].Requests != 1 {
		t.Fatalf("since filter wrong: %+v", only)
	}
}

func TestAccountRateLimitUpsertAndList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{ID: "acc-1", Label: "a", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "x"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	if _, ok, err := store.GetAccountRateLimit(ctx, "acc-1"); err != nil || ok {
		t.Fatalf("expected no snapshot yet, ok=%v err=%v", ok, err)
	}

	snap := AccountRateLimit{
		AccountID: "acc-1", Provider: "claude", Source: "unified",
		UsedPercent: 42.5, LimitTokens: 1000, RemainingTokens: 575,
		LimitRequests: -1, RemainingRequests: -1, ResetAt: 1_700_000_000,
		Status: "allowed", Raw: `{"anthropic-ratelimit-unified-remaining":"575"}`,
	}
	if err := store.UpsertAccountRateLimit(ctx, snap); err != nil {
		t.Fatalf("upsert ratelimit: %v", err)
	}
	got, ok, err := store.GetAccountRateLimit(ctx, "acc-1")
	if err != nil || !ok {
		t.Fatalf("get ratelimit: ok=%v err=%v", ok, err)
	}
	if got.UsedPercent != 42.5 || got.RemainingTokens != 575 || got.Source != "unified" || got.Status != "allowed" {
		t.Fatalf("snapshot mismatch: %+v", got)
	}
	if got.UpdatedAt == 0 {
		t.Fatalf("updated_at should be auto-stamped")
	}

	// Upsert again replaces, not duplicates.
	snap.UsedPercent = 90
	if err := store.UpsertAccountRateLimit(ctx, snap); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	list, err := store.ListAccountRateLimits(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 snapshot, got %d", len(list))
	}
	if list[0].UsedPercent != 90 {
		t.Fatalf("replace failed, used=%v", list[0].UsedPercent)
	}

	// Raw must round-trip as valid JSON.
	var raw map[string]string
	if err := json.Unmarshal([]byte(list[0].Raw), &raw); err != nil {
		t.Fatalf("raw not valid json: %v", err)
	}
}

func TestAccountRateLimitCompositeDimension(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{ID: "acc-1", Label: "a", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "x"}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	gpt5 := AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		UsedPercent: 100, LimitTokens: 1000, RemainingTokens: 0,
		LimitRequests: -1, RemainingRequests: -1, ResetAt: 1_700_000_100,
		Status: "rejected",
	}
	gpt4 := AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-4", LimiterType: "tokens", Source: "tokens",
		UsedPercent: 10, LimitTokens: 1000, RemainingTokens: 900,
		LimitRequests: -1, RemainingRequests: -1, ResetAt: 1_700_000_200,
		Status: "allowed",
	}
	if err := store.UpsertAccountRateLimit(ctx, gpt5); err != nil {
		t.Fatalf("upsert gpt5: %v", err)
	}
	if err := store.UpsertAccountRateLimit(ctx, gpt4); err != nil {
		t.Fatalf("upsert gpt4: %v", err)
	}
	got5, ok, err := store.GetAccountRateLimitFor(ctx, "acc-1", "codex", "gpt-5", "tokens")
	if err != nil || !ok {
		t.Fatalf("get gpt5: ok=%v err=%v", ok, err)
	}
	got4, ok, err := store.GetAccountRateLimitFor(ctx, "acc-1", "codex", "gpt-4", "tokens")
	if err != nil || !ok {
		t.Fatalf("get gpt4: ok=%v err=%v", ok, err)
	}
	if got5.Status != "rejected" || got4.Status != "allowed" {
		t.Fatalf("snapshots overwrote each other: gpt5=%+v gpt4=%+v", got5, got4)
	}
	list, err := store.ListAccountRateLimits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 dimensioned snapshots, got %d: %+v", len(list), list)
	}
}

func TestAccountRateLimitMigrationAddsCompositeDimension(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`
CREATE TABLE accounts(
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT 'cyber',
  upstream_account_id TEXT,
  chatgpt_user_id TEXT,
  email TEXT,
  plan_type TEXT,
  provider TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',
  is_fedramp INTEGER NOT NULL DEFAULT 0,
  quarantine_until INTEGER NOT NULL DEFAULT 0,
  quarantine_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
INSERT INTO accounts(id, label, group_name, provider, status, created_at, updated_at)
VALUES('acc-old', 'old', 'cyber', 'codex', 'active', 1700000000, 1700000000);
CREATE TABLE account_rate_limits(
  account_id TEXT PRIMARY KEY,
  provider TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  used_percent REAL NOT NULL DEFAULT -1,
  limit_tokens INTEGER NOT NULL DEFAULT -1,
  remaining_tokens INTEGER NOT NULL DEFAULT -1,
  limit_requests INTEGER NOT NULL DEFAULT -1,
  remaining_requests INTEGER NOT NULL DEFAULT -1,
  reset_at INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  raw_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
INSERT INTO account_rate_limits(account_id, provider, source, used_percent, limit_tokens, remaining_tokens, limit_requests, remaining_requests, reset_at, status, raw_json, updated_at)
VALUES('acc-old', 'codex', 'tokens', 100, 1000, 0, -1, -1, 1700000100, 'rejected', '{}', 1700000000);
`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init/migrate: %v", err)
	}
	got, ok, err := store.GetAccountRateLimitFor(context.Background(), "acc-old", "codex", "", "tokens")
	if err != nil || !ok {
		t.Fatalf("migrated snapshot missing: ok=%v err=%v", ok, err)
	}
	if got.Model != "" || got.LimiterType != "tokens" || got.RemainingTokens != 0 {
		t.Fatalf("migrated snapshot mismatch: %+v", got)
	}
}

func TestAccountLabelsByID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, account := range []Account{
		{ID: "acc-label", Label: "Visible Label", Email: "label@example.com", GroupName: "cyber", Status: "active"},
		{ID: "acc-email", Email: "email@example.com", GroupName: "cyber", Status: "active"},
		{ID: "acc-unrequested", Label: "Do Not Load", GroupName: "cyber", Status: "active"},
	} {
		if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "x-" + account.ID}); err != nil {
			t.Fatalf("upsert %s: %v", account.ID, err)
		}
	}

	labels, err := store.AccountLabelsByID(ctx, []string{"acc-label", "acc-email", "missing", "acc-label"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"acc-label": "Visible Label",
		"acc-email": "email@example.com",
		"missing":   "missing",
	}
	if len(labels) != len(want) {
		t.Fatalf("labels = %#v, want %d entries", labels, len(want))
	}
	for id, label := range want {
		if labels[id] != label {
			t.Fatalf("labels[%s] = %q, want %q (all=%#v)", id, labels[id], label, labels)
		}
	}
	if _, ok := labels["acc-unrequested"]; ok {
		t.Fatalf("loaded unrequested account label: %#v", labels)
	}
}
