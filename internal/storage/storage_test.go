package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestInitCreatesCyberGroupAndDirectEgress(t *testing.T) {
	store := newTestStore(t)
	group, err := store.GetGroup(context.Background(), "cyber")
	if err != nil {
		t.Fatalf("get cyber group: %v", err)
	}
	if group.SystemPrompt != "" {
		t.Fatalf("cyber prompt = %q, want empty", group.SystemPrompt)
	}
	if group.PromptMode != "prepend" {
		t.Fatalf("prompt mode = %q", group.PromptMode)
	}
	if !group.SystemPromptApplyToCompaction {
		t.Fatalf("system prompt should apply to compaction by default")
	}
	egress, err := store.GetEgressProfile(context.Background(), DefaultDirectEgressID)
	if err != nil {
		t.Fatalf("get direct egress: %v", err)
	}
	if egress.Type != "direct" || !egress.StreamCapable {
		t.Fatalf("unexpected direct egress: %+v", egress)
	}
}

func TestAccountImportCreatesEgressBinding(t *testing.T) {
	store := newTestStore(t)
	err := store.UpsertAccount(context.Background(), Account{
		ID:                "acc-1",
		Label:             "test",
		GroupName:         "cyber",
		UpstreamAccountID: "chatgpt-account",
		Status:            "active",
	}, AccountToken{AccessToken: "access"})
	if err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	binding, err := store.GetEgressBinding(context.Background(), "acc-1")
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}
	if binding.PrimaryEgressID != DefaultDirectEgressID {
		t.Fatalf("primary egress = %q", binding.PrimaryEgressID)
	}
}

func TestAccountTokenOAuthMetadataPersists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "claude-meta", Label: "Claude", GroupName: "cyber", Provider: "claude", Status: "active", PlanType: "max"}
	token := AccountToken{
		AccessToken:        "sk-ant-oat-old",
		RefreshToken:       "refresh-claude",
		ExpiresAt:          1760000123,
		Scopes:             "user:profile user:inference user:sessions:claude_code",
		OAuthRateLimitTier: "tier_4",
	}
	if err := store.UpsertAccount(ctx, account, token); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetToken(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt != token.ExpiresAt || got.Scopes != token.Scopes || got.OAuthRateLimitTier != token.OAuthRateLimitTier {
		t.Fatalf("metadata not persisted: %+v", got)
	}
	got.AccessToken = "sk-ant-oat-new"
	got.ExpiresAt = 1760000999
	if err := store.UpdateToken(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetToken(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "sk-ant-oat-new" || got.ExpiresAt != 1760000999 || got.OAuthRateLimitTier != "tier_4" {
		t.Fatalf("metadata lost on update: %+v", got)
	}
}

func TestBackfillUsageCacheDiagnosticsIncludesZeroPromptAnthropicRows(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO usage_records(account_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, raw_usage_json, created_at)
VALUES
('acc-read', 'claude-x', 0, 1, 1, 700, 700, 0, '{"input_tokens":0,"output_tokens":1,"cache_read_input_tokens":700}', ?),
('acc-write', 'claude-x', 0, 1, 1, 0, 0, 75, '{"input_tokens":0,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":75}', ?)`, Now(), Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.backfillUsageCacheDiagnostics(ctx); err != nil {
		t.Fatal(err)
	}
	var readTotal, writeTotal, readMiss, writeMiss int64
	var readProvider, writeProvider string
	if err := store.db.QueryRowContext(ctx, `SELECT usage_provider, cache_total_input_tokens, cache_miss_tokens FROM usage_records WHERE account_id='acc-read'`).Scan(&readProvider, &readTotal, &readMiss); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT usage_provider, cache_total_input_tokens, cache_miss_tokens FROM usage_records WHERE account_id='acc-write'`).Scan(&writeProvider, &writeTotal, &writeMiss); err != nil {
		t.Fatal(err)
	}
	if readProvider != "anthropic" || readTotal != 700 || readMiss != 0 {
		t.Fatalf("read row backfill = provider %q total %d miss %d", readProvider, readTotal, readMiss)
	}
	if writeProvider != "anthropic" || writeTotal != 75 || writeMiss != 0 {
		t.Fatalf("write row backfill = provider %q total %d miss %d", writeProvider, writeTotal, writeMiss)
	}
}

func TestAccountPoolSummaryCountsDashboardFields(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := Now()
	accounts := []struct {
		account Account
		token   AccountToken
	}{
		{Account{ID: "codex-active", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "codex-token"}},
		{Account{ID: "claude-legacy", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "sk-ant-oat-test"}},
		{Account{ID: "custom-disabled", GroupName: "cyber", Provider: "deepseek", Status: "disabled"}, AccountToken{OpenAIAPIKey: "sk-custom"}},
		{Account{ID: "codex-quarantined", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "codex-token-2"}},
	}
	for _, item := range accounts {
		if err := store.UpsertAccount(ctx, item.account, item.token); err != nil {
			t.Fatalf("upsert %s: %v", item.account.ID, err)
		}
	}
	if err := store.SetBindingCooldown(ctx, "codex-active", now+300); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	if err := store.SetBindingRecheckPending(ctx, "claude-legacy", true); err != nil {
		t.Fatalf("set recheck pending: %v", err)
	}
	if err := store.SetAccountQuarantine(ctx, "codex-quarantined", now+600, "test"); err != nil {
		t.Fatalf("set quarantine: %v", err)
	}

	got, err := store.AccountPoolSummary(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := AccountPoolSummary{
		Total:       4,
		Active:      2,
		Quarantined: 1,
		Cooling:     1,
		Recheck:     1,
		Codex:       2,
		Claude:      1,
		Other:       1,
	}
	if got != want {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
}

func TestAccountBatchExpansionHelpers(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	accounts := []struct {
		account Account
		token   AccountToken
	}{
		{Account{ID: "codex-explicit", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "codex-token"}},
		{Account{ID: "claude-legacy", GroupName: "cyber", Status: "active"}, AccountToken{AccessToken: "sk-ant-oat-test"}},
	}
	for _, item := range accounts {
		if err := store.UpsertAccount(ctx, item.account, item.token); err != nil {
			t.Fatalf("upsert %s: %v", item.account.ID, err)
		}
	}
	if err := store.SetBindingRecheckPending(ctx, "claude-legacy", true); err != nil {
		t.Fatalf("set recheck pending: %v", err)
	}
	if err := store.UpsertCapabilities(ctx, []ModelCapability{
		{AccountID: "codex-explicit", ModelSlug: "gpt-5"},
		{AccountID: "claude-legacy", ModelSlug: "claude-sonnet"},
	}); err != nil {
		t.Fatalf("upsert capabilities: %v", err)
	}
	accountRows, _, err := store.ListAccountsPage(ctx, 20, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}

	providers, err := store.ResolveAccountProviders(ctx, accountRows)
	if err != nil {
		t.Fatal(err)
	}
	if providers["codex-explicit"] != "codex" || providers["claude-legacy"] != "claude" {
		t.Fatalf("providers = %#v, want codex explicit and claude legacy fallback", providers)
	}

	bindings, err := store.ListEgressBindingsByAccountIDs(ctx, []string{"codex-explicit", "claude-legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 || !bindings["claude-legacy"].RecheckPending {
		t.Fatalf("bindings = %#v, want two bindings with claude recheck pending", bindings)
	}

	caps, err := store.ListCapabilitiesByAccountIDs(ctx, []string{"codex-explicit", "claude-legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if got := caps["codex-explicit"]; len(got) != 1 || got[0].ModelSlug != "gpt-5" {
		t.Fatalf("codex capabilities = %#v, want gpt-5", got)
	}
	if got := caps["claude-legacy"]; len(got) != 1 || got[0].ModelSlug != "claude-sonnet" {
		t.Fatalf("claude capabilities = %#v, want claude-sonnet", got)
	}
}

func TestListAccountsByIDs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for _, account := range []Account{
		{ID: "acc-a", Label: "A", GroupName: "cyber", Status: "active"},
		{ID: "acc-b", Label: "B", GroupName: "cyber", Status: "disabled"},
		{ID: "acc-c", Label: "C", GroupName: "cyber", Status: "active"},
	} {
		if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token-" + account.ID}); err != nil {
			t.Fatalf("upsert %s: %v", account.ID, err)
		}
	}

	got, err := store.ListAccountsByIDs(ctx, []string{"acc-a", "missing", "acc-b", "acc-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("accounts = %#v, want two requested existing accounts", got)
	}
	if got["acc-a"].Label != "A" || got["acc-b"].Status != "disabled" {
		t.Fatalf("accounts = %#v, want acc-a label and acc-b status", got)
	}
	if _, ok := got["acc-c"]; ok {
		t.Fatalf("loaded unrequested account: %#v", got["acc-c"])
	}
}

func TestUsageSummaryByAccountIDs(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.InsertUsageRecord(ctx, "acc-a", "", "", "", "gpt-5", 10, 20, 30, 3, nil); err != nil {
		t.Fatalf("insert acc-a usage: %v", err)
	}
	if err := store.InsertUsageRecord(ctx, "acc-a", "", "", "", "gpt-5", 1, 2, 3, 1, nil); err != nil {
		t.Fatalf("insert second acc-a usage: %v", err)
	}
	if err := store.InsertUsageRecord(ctx, "acc-b", "", "", "", "claude-sonnet", 5, 6, 11, 0, nil); err != nil {
		t.Fatalf("insert acc-b usage: %v", err)
	}
	if err := store.InsertUsageRecord(ctx, "acc-c", "", "", "", "other", 100, 100, 200, 0, nil); err != nil {
		t.Fatalf("insert acc-c usage: %v", err)
	}

	got, err := store.UsageSummaryByAccountIDs(ctx, []string{"acc-a", "acc-b", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("summary row count = %d, want 2: %#v", len(got), got)
	}
	if row := got["acc-a"]; row.Requests != 2 || row.PromptTokens != 11 || row.CompletionTokens != 22 || row.TotalTokens != 33 || row.CachedTokens != 4 {
		t.Fatalf("acc-a summary = %#v, want aggregated usage", row)
	}
	if row := got["acc-b"]; row.Requests != 1 || row.TotalTokens != 11 {
		t.Fatalf("acc-b summary = %#v, want one request total 11", row)
	}
	if _, ok := got["acc-c"]; ok {
		t.Fatalf("summary included non-requested account: %#v", got["acc-c"])
	}
	if empty, err := store.UsageSummaryByAccountIDs(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty accountIDs = %#v err=%v, want empty map", empty, err)
	}
}

func TestListAuditLogForAccount(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.InsertAuditLog(ctx, AuditLogRow{AccountID: "acc-a", AccountLabel: "alpha", Action: "first", State: "alive"}); err != nil {
		t.Fatalf("insert acc-a first audit: %v", err)
	}
	if err := store.InsertAuditLog(ctx, AuditLogRow{AccountID: "acc-b", AccountLabel: "beta", Action: "other", State: "banned"}); err != nil {
		t.Fatalf("insert acc-b audit: %v", err)
	}
	if err := store.InsertAuditLog(ctx, AuditLogRow{AccountID: "acc-a", AccountLabel: "alpha", Action: "latest", State: "alive"}); err != nil {
		t.Fatalf("insert acc-a latest audit: %v", err)
	}

	got, err := store.ListAuditLogForAccount(ctx, "acc-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("account audit rows = %d, want 2: %#v", len(got), got)
	}
	if got[0].Action != "latest" || got[1].Action != "first" {
		t.Fatalf("account audit order = %#v, want newest first for acc-a", got)
	}
	for _, row := range got {
		if row.AccountID != "acc-a" {
			t.Fatalf("account audit included unrelated row: %#v", row)
		}
	}
}

func TestSetSettingsIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.SetSetting(ctx, "aaa_runtime_key", "old"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `
CREATE TRIGGER fail_zzz_runtime_key
BEFORE INSERT ON settings
WHEN NEW.key = 'zzz_runtime_key'
BEGIN
	SELECT RAISE(ABORT, 'synthetic settings failure');
END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	err := store.SetSettings(ctx, map[string]string{
		"aaa_runtime_key": "new",
		"zzz_runtime_key": "blocked",
	})
	if err == nil {
		t.Fatal("SetSettings succeeded, want synthetic failure")
	}

	got, ok, err := store.GetSetting(ctx, "aaa_runtime_key")
	if err != nil {
		t.Fatalf("read rolled-back key: %v", err)
	}
	if !ok || got != "old" {
		t.Fatalf("aaa_runtime_key = %q ok=%v, want old value after rollback", got, ok)
	}
	if got, ok, err := store.GetSetting(ctx, "zzz_runtime_key"); err != nil || ok {
		t.Fatalf("zzz_runtime_key = %q ok=%v err=%v, want absent after rollback", got, ok, err)
	}
}

func TestKiroRuntimeCapabilityPresenceAndUnreportedThreshold(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "kiro-cap", Label: "kiro", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureKiroRuntimeModels(ctx, account.ID, "endpoint", []string{"claude-sonnet-4.6"}); err != nil {
		t.Fatal(err)
	}
	var state KiroRuntimeCapability
	var err error
	for i := 0; i < 19; i++ {
		state, err = store.ObserveKiroCapability(ctx, account.ID, "endpoint", "claude-sonnet-4.6", KiroCapabilityObservation{ModelSucceeded: true, MeteringEvents: 1, UnreportedThreshold: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.ModelState != "verified" || state.CacheCapability != "unknown" || state.ConsecutiveUnreported != 19 {
		t.Fatalf("after 19 = %+v", state)
	}
	state, err = store.ObserveKiroCapability(ctx, account.ID, "endpoint", "claude-sonnet-4.6", KiroCapabilityObservation{ModelSucceeded: true, MeteringEvents: 1, UnreportedThreshold: 20})
	if err != nil || state.CacheCapability != "unreported" || state.ConsecutiveUnreported != 20 {
		t.Fatalf("after 20 = %+v err=%v", state, err)
	}
	state, err = store.ObserveKiroCapability(ctx, account.ID, "endpoint", "claude-sonnet-4.6", KiroCapabilityObservation{MeteringEvents: 1, CacheReadPresent: true, CacheReadTokens: 0})
	if err != nil || state.CacheCapability != "reported" || state.ConsecutiveUnreported != 0 {
		t.Fatalf("reported zero = %+v err=%v", state, err)
	}
	state, err = store.ObserveKiroCapability(ctx, account.ID, "endpoint", "claude-sonnet-4.6", KiroCapabilityObservation{MeteringEvents: 1, CacheReadPresent: true, CacheReadTokens: 5})
	if err != nil || state.CacheCapability != "hit_observed" || state.CacheHitObservations != 1 {
		t.Fatalf("hit = %+v err=%v", state, err)
	}
}

func TestKiroCacheReuseProbeIsIndependentAndVerifiedIsMonotonic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "kiro-cache-reuse", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKiroCachePointState(ctx, account.ID, "endpoint", "claude-opus-4.8", "verified"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKiroCacheReuseProbe(ctx, account.ID, "endpoint", "claude-opus-4.8", "verified", "credits_reduction", 47.5, 1234); err != nil {
		t.Fatal(err)
	}
	state, err := store.GetKiroRuntimeCapability(ctx, account.ID, "endpoint", "claude-opus-4.8")
	if err != nil {
		t.Fatal(err)
	}
	if state.CachePointState != "verified" || state.CacheCapability != "unknown" || state.CacheReuseState != "verified" || state.CacheReuseEvidence != "credits_reduction" || state.CacheReuseReductionPct != 47.5 || state.CacheReuseProbedAt != 1234 {
		t.Fatalf("persisted cache reuse state=%+v", state)
	}
	if _, err := store.ObserveKiroCapability(ctx, account.ID, "endpoint", "claude-opus-4.8", KiroCapabilityObservation{ModelSucceeded: true, MeteringEvents: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetKiroCacheReuseProbe(ctx, account.ID, "endpoint", "claude-opus-4.8", "not_observed", "none", 0, 2345); err != nil {
		t.Fatal(err)
	}
	state, err = store.GetKiroRuntimeCapability(ctx, account.ID, "endpoint", "claude-opus-4.8")
	if err != nil {
		t.Fatal(err)
	}
	if state.CacheReuseState != "verified" || state.CacheReuseEvidence != "credits_reduction" || state.CacheReuseProbedAt != 1234 {
		t.Fatalf("inconclusive probe downgraded verified evidence: %+v", state)
	}
	if err := store.SetKiroCacheReuseProbe(ctx, account.ID, "endpoint", "claude-sonnet-4.6", "not_observed", "none", 0, 3456); err != nil {
		t.Fatal(err)
	}
	notObserved, err := store.GetKiroRuntimeCapability(ctx, account.ID, "endpoint", "claude-sonnet-4.6")
	if err != nil || notObserved.CacheReuseState != "not_observed" || notObserved.CacheReuseProbedAt != 3456 {
		t.Fatalf("not-observed cache probe=%+v err=%v", notObserved, err)
	}
}

func TestKiroUnreportedUsageIsNotCacheMiss(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "kiro-usage", Label: "kiro", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "route", "", "", "claude-sonnet-4.6", 100, 10, 110, 0, 0, 0, json.RawMessage(`{"input_tokens":100,"output_tokens":10}`), UsageDiagnostics{UsageProvider: "kiro", UsageSource: "upstream"}); err != nil {
		t.Fatal(err)
	}
	report, err := store.CacheUsageMetricsWindowFullRoutes(ctx, 0, Now()+10)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.Requests != 1 || report.Summary.RealRequests != 0 || report.Summary.CacheMissTokens != 0 || report.Summary.CacheInputTokens != 0 {
		t.Fatalf("unreported Kiro usage counted as cache miss: %+v", report.Summary)
	}
}

func TestKiroHistoricalZeroUsageBackfillAndModelAggregation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "kiro-history", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "zero", "", "", "claude-opus-4-8", 0, 0, 0, 0, 0, 0,
		json.RawMessage(`{"metering_event_count":1,"usage_source":"upstream"}`),
		UsageDiagnostics{UsageProvider: "kiro", UsageSource: "upstream"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "a", "", "", "claude-opus-4-8", 10, 1, 11, 0, 0, 0,
		json.RawMessage(`{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`),
		UsageDiagnostics{UsageProvider: "kiro", UsageSource: "upstream", CacheReadPresent: true, CacheCreationPresent: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "b", "", "", "claude-opus-4.8", 12, 1, 13, 0, 0, 0,
		json.RawMessage(`{"input_tokens":12,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}`),
		UsageDiagnostics{UsageProvider: "kiro", UsageSource: "upstream", CacheReadPresent: true, CacheCreationPresent: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.backfillUsageCacheDiagnostics(ctx); err != nil {
		t.Fatal(err)
	}
	var source, model string
	if err := store.DB().QueryRowContext(ctx, `SELECT usage_source, model FROM usage_records WHERE route_key_hash='zero'`).Scan(&source, &model); err != nil {
		t.Fatal(err)
	}
	if source != "unreported" || model != "claude-opus-4.8" {
		t.Fatalf("historical zero/model backfill = source %q model %q", source, model)
	}
	report, err := store.CacheUsageMetricsWindowFullRoutes(ctx, 0, Now()+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ByModel) != 1 || report.ByModel[0].Model != "claude-opus-4.8" || report.ByModel[0].Requests != 3 {
		t.Fatalf("normalized model aggregation = %+v", report.ByModel)
	}
}

func TestCacheUsageAggregateKeepsBreakpointsJSONFromMaximumCount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "cache-breakpoints", GroupName: "cyber", Provider: "kiro", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		count int
		json  string
	}{
		{count: 2, json: `[{"section":"system"},{"section":"messages"}]`},
		{count: 3, json: `[{"section":"system"},{"section":"messages"},{"section":"messages","message_index":2}]`},
	} {
		if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "same-route", "", "", "claude-opus-4.8", 100, 10, 110, 0, 0, 0, nil, UsageDiagnostics{
			UsageProvider:        "kiro",
			UsageSource:          "estimated",
			Estimated:            true,
			CacheBreakpointCount: tc.count,
			CacheBreakpointsJSON: tc.json,
		}); err != nil {
			t.Fatal(err)
		}
	}
	report, err := store.CacheUsageMetricsWindowFullRoutes(ctx, 0, Now()+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ByRoute) != 1 {
		t.Fatalf("by-route rows = %+v", report.ByRoute)
	}
	row := report.ByRoute[0]
	if row.CacheBreakpointCount != 3 || row.CacheBreakpointsJSON != `[{"section":"system"},{"section":"messages"},{"section":"messages","message_index":2}]` {
		t.Fatalf("breakpoint aggregate count/json mismatch: count=%d json=%s", row.CacheBreakpointCount, row.CacheBreakpointsJSON)
	}
}
