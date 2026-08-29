package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func newTestStore(t testing.TB) *Store {
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

func TestSQLiteSpacePragmasAreApplied(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	var journalLimit, autoCheckpoint int64
	if err := store.DB().QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&journalLimit); err != nil {
		t.Fatalf("read journal_size_limit: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `PRAGMA wal_autocheckpoint`).Scan(&autoCheckpoint); err != nil {
		t.Fatalf("read wal_autocheckpoint: %v", err)
	}
	if journalLimit != 64<<20 {
		t.Fatalf("journal_size_limit=%d, want %d", journalLimit, int64(64<<20))
	}
	if autoCheckpoint != 2000 {
		t.Fatalf("wal_autocheckpoint=%d, want 2000", autoCheckpoint)
	}
}

func TestCodexHTTPStatelessMigrationUpdatesOnlyLegacyStableProfileOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const marker = "codex_http_stateless_v1_migrated"
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key=?`, marker); err != nil {
		t.Fatal(err)
	}
	store.InvalidateSettingsCache()
	if err := store.SetSettings(ctx, map[string]string{
		"codex_session_mapping_enabled": "true",
		"codex_stateless_passthrough":   "false",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateCodexHTTPStateless(ctx, Now()); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.GetSetting(ctx, "codex_stateless_passthrough"); err != nil || !ok || got != "true" {
		t.Fatalf("migrated stateless setting=%q present=%v err=%v", got, ok, err)
	}
	if got, ok, err := store.GetSetting(ctx, "codex_session_mapping_enabled"); err != nil || !ok || got != "true" {
		t.Fatalf("mapping setting=%q present=%v err=%v", got, ok, err)
	}

	// A later explicit operator choice must not be overwritten on every restart.
	if err := store.SetSetting(ctx, "codex_stateless_passthrough", "false"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateCodexHTTPStateless(ctx, Now()+1); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.GetSetting(ctx, "codex_stateless_passthrough"); err != nil || got != "false" {
		t.Fatalf("post-migration operator setting=%q err=%v", got, err)
	}
}

func TestCodexNativeCacheMigrationRevertsOnlyAutoForcedStateless(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, marker := range []string{"codex_http_stateless_v1_migrated", "codex_native_cache_default_v2_migrated"} {
		if _, err := store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key=?`, marker); err != nil {
			t.Fatal(err)
		}
	}
	store.InvalidateSettingsCache()
	if err := store.SetSettings(ctx, map[string]string{"codex_session_mapping_enabled": "true", "codex_stateless_passthrough": "false"}); err != nil {
		t.Fatal(err)
	}
	now := Now() + 10
	if err := store.migrateCodexHTTPStateless(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateCodexNativeCacheDefault(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.GetSetting(ctx, "codex_stateless_passthrough"); err != nil || got != "false" {
		t.Fatalf("auto-forced stateless value was not repaired: value=%q err=%v", got, err)
	}
	if err := store.SetSetting(ctx, "codex_stateless_passthrough", "true"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateCodexNativeCacheDefault(ctx, now+1); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.GetSetting(ctx, "codex_stateless_passthrough"); err != nil || got != "true" {
		t.Fatalf("explicit post-migration operator choice changed: value=%q err=%v", got, err)
	}
}

func TestCodexNativeCacheMigrationPreservesExplicitStateless(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, marker := range []string{"codex_http_stateless_v1_migrated", "codex_native_cache_default_v2_migrated"} {
		if _, err := store.DB().ExecContext(ctx, `DELETE FROM settings WHERE key=?`, marker); err != nil {
			t.Fatal(err)
		}
	}
	store.InvalidateSettingsCache()
	if err := store.SetSettings(ctx, map[string]string{"codex_session_mapping_enabled": "true", "codex_stateless_passthrough": "true"}); err != nil {
		t.Fatal(err)
	}
	now := Now() + 20
	if err := store.migrateCodexHTTPStateless(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateCodexNativeCacheDefault(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, err := store.GetSetting(ctx, "codex_stateless_passthrough"); err != nil || got != "true" {
		t.Fatalf("explicit stateless choice changed: value=%q err=%v", got, err)
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

func TestAccountIgnoreRateLimitControlsPersistsIndividually(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, account := range []Account{
		{ID: "ignore-controls-on", Label: "on", GroupName: "cyber", Status: "active", IgnoreRateLimitControls: true},
		{ID: "ignore-controls-off", Label: "off", GroupName: "cyber", Status: "active"},
	} {
		if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "access-" + account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	on, err := store.GetAccount(ctx, "ignore-controls-on")
	if err != nil || !on.IgnoreRateLimitControls {
		t.Fatalf("enabled account = %+v err=%v", on, err)
	}
	off, err := store.GetAccount(ctx, "ignore-controls-off")
	if err != nil || off.IgnoreRateLimitControls {
		t.Fatalf("disabled account = %+v err=%v", off, err)
	}
	if err := store.SetAccountIgnoreRateLimitControls(ctx, off.ID, true); err != nil {
		t.Fatal(err)
	}
	off, err = store.GetAccount(ctx, off.ID)
	if err != nil || !off.IgnoreRateLimitControls {
		t.Fatalf("updated account = %+v err=%v", off, err)
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

func TestUpdateTokenAfterCredentialRefreshReactivatesOnlyAuthExpiredAccount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, status := range []string{"auth_expired", "disabled"} {
		id := "claude-refresh-" + status
		account := Account{
			ID: id, Label: id, GroupName: "cyber", Provider: "claude",
			Status: status, QuarantineUntil: Now() + 3600, QuarantineReason: "old auth failure",
		}
		token := AccountToken{
			AccessToken: "sk-ant-oat-old-" + status, RefreshToken: "refresh-old-" + status,
			ExpiresAt: Now() - 1,
		}
		if err := store.UpsertAccount(ctx, account, token); err != nil {
			t.Fatal(err)
		}
		if err := store.BenchBindingForRecheck(ctx, id, Now()+300); err != nil {
			t.Fatal(err)
		}
		token.AccountID = id
		token.AccessToken = "sk-ant-oat-new-" + status
		token.RefreshToken = "refresh-new-" + status
		token.ExpiresAt = Now() + 3600
		token.LastRefresh = Now()

		reactivated, err := store.UpdateTokenAfterCredentialRefresh(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		wantReactivated := status == "auth_expired"
		if reactivated != wantReactivated {
			t.Fatalf("status %q reactivated=%v, want %v", status, reactivated, wantReactivated)
		}
		gotAccount, err := store.GetAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		gotToken, err := store.GetToken(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := store.GetEgressBinding(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if gotToken.AccessToken != token.AccessToken || gotToken.RefreshToken != token.RefreshToken || gotToken.ExpiresAt != token.ExpiresAt {
			t.Fatalf("status %q refreshed token mismatch: %+v", status, gotToken)
		}
		if wantReactivated {
			if gotAccount.Status != "active" || gotAccount.QuarantineUntil != 0 || gotAccount.QuarantineReason != "" {
				t.Fatalf("auth-expired account not fully restored: %+v", gotAccount)
			}
			if binding.RecheckPending || binding.CooldownUntil != 0 {
				t.Fatalf("auth-expired binding still benched: %+v", binding)
			}
		} else {
			if gotAccount.Status != "disabled" || gotAccount.QuarantineUntil == 0 {
				t.Fatalf("explicit disabled state changed: %+v", gotAccount)
			}
			if !binding.RecheckPending || binding.CooldownUntil == 0 {
				t.Fatalf("disabled account binding unexpectedly cleared: %+v", binding)
			}
		}
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

func TestDeferredUsageDiagnosticsMigrationRunsOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO usage_records(account_id, route_key_hash, model, prompt_tokens, completion_tokens, total_tokens, raw_usage_json, created_at)
VALUES('deferred-before', 'before', 'gpt-test', 25, 1, 26, '{"input_tokens":25,"output_tokens":1}', ?)`, Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RunDeferredMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var total, markerCount int64
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_total_input_tokens FROM usage_records WHERE account_id='deferred-before'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, usageCacheDiagnosticsMigrationMarker).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if total != 25 || markerCount != 1 {
		t.Fatalf("deferred migration total=%d markers=%d, want 25/1", total, markerCount)
	}

	// Once marked, subsequent starts do constant work instead of rescanning history.
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO usage_records(account_id, route_key_hash, model, prompt_tokens, completion_tokens, total_tokens, raw_usage_json, created_at)
VALUES('deferred-after', 'after', 'gpt-test', 30, 1, 31, '{"input_tokens":30,"output_tokens":1}', ?)`, Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.RunDeferredMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_total_input_tokens FROM usage_records WHERE account_id='deferred-after'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("completed one-time migration rescanned new row: total=%d", total)
	}
}

func TestCodexNestedCachedTokensPopulateCacheDiagnostics(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "codex-cache-hit", "route-hit", "", "", "gpt-5.6-sol",
		165770, 215, 165985, 165632, 165632, 0,
		json.RawMessage(`{"usage":{"input_tokens":165770,"output_tokens":215,"total_tokens":165985,"input_tokens_details":{"cached_tokens":165632}}}`),
		UsageDiagnostics{UsageProvider: "codex", UsageSource: "upstream"}); err != nil {
		t.Fatal(err)
	}
	var present int
	var capability, missReason string
	var possible, miss, total int64
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_read_present,cache_capability,max_possible_cache_read_tokens,
diagnostics_miss_reason,cache_miss_tokens,cache_total_input_tokens FROM usage_records WHERE account_id='codex-cache-hit'`).Scan(
		&present, &capability, &possible, &missReason, &miss, &total); err != nil {
		t.Fatal(err)
	}
	if present != 1 || capability != "hit_observed" || possible != 165770 || missReason != "" || miss != 138 || total != 165770 {
		t.Fatalf("Codex hit diagnostics present=%d capability=%q possible=%d reason=%q miss/total=%d/%d",
			present, capability, possible, missReason, miss, total)
	}

	if err := store.InsertUsageRecordWithDiagnostics(ctx, "codex-cache-miss", "route-miss", "", "", "gpt-5.6-sol",
		4096, 12, 4108, 0, 0, 0,
		json.RawMessage(`{"usage":{"input_tokens":4096,"output_tokens":12,"total_tokens":4108,"input_tokens_details":{"cached_tokens":0}}}`),
		UsageDiagnostics{UsageProvider: "codex", UsageSource: "upstream"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_read_present,cache_capability,max_possible_cache_read_tokens,
diagnostics_miss_reason FROM usage_records WHERE account_id='codex-cache-miss'`).Scan(
		&present, &capability, &possible, &missReason); err != nil {
		t.Fatal(err)
	}
	if present != 1 || capability != "reported" || possible != 4096 || missReason != "upstream_reported_zero_cache_read" {
		t.Fatalf("Codex miss diagnostics present=%d capability=%q possible=%d reason=%q",
			present, capability, possible, missReason)
	}
}

// The prompt-total upper bound was gated to the provider names "codex" and "openai", so
// antigravity and every custom relay stored 0 while reporting a real cache read. Zero is
// indistinguishable from "nothing was reusable", and a read/max ratio over a mixed pool
// then divided by zero on those rows: one export summed to 1.06 against a bound that
// cannot be exceeded.
func TestNonAnthropicProvidersGetAPromptTotalCacheCeiling(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	openAIShaped := json.RawMessage(`{"usage":{"input_tokens":32248,"output_tokens":40,"total_tokens":32288,"input_tokens_details":{"cached_tokens":28327}}}`)
	for _, provider := range []string{"antigravity", "relay-deepseek", "codex", "openai"} {
		if err := store.InsertUsageRecordWithDiagnostics(ctx, "ceiling-"+provider, "route-"+provider, "", "", "some-model",
			32248, 40, 32288, 28327, 28327, 0, openAIShaped,
			UsageDiagnostics{UsageProvider: provider, UsageSource: "upstream"}); err != nil {
			t.Fatal(err)
		}
		var possible, cacheRead int64
		if err := store.DB().QueryRowContext(ctx, `SELECT max_possible_cache_read_tokens,cache_read_tokens
FROM usage_records WHERE account_id=?`, "ceiling-"+provider).Scan(&possible, &cacheRead); err != nil {
			t.Fatal(err)
		}
		if possible != 32248 {
			t.Fatalf("provider %q ceiling = %d, want the prompt total 32248", provider, possible)
		}
		// The whole point of the column: it must actually bound the observed read.
		if cacheRead > possible {
			t.Fatalf("provider %q reported read %d above its own ceiling %d", provider, cacheRead, possible)
		}
	}

	// Anthropic bodies keep deriving the ceiling from breakpoint metadata. Falling back to
	// the prompt total there would overstate it, since a breakpoint can sit mid-prompt.
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "ceiling-anthropic", "route-anthropic", "", "", "claude-sonnet-4-5",
		900, 20, 920, 800, 800, 100,
		json.RawMessage(`{"usage":{"input_tokens":900,"output_tokens":20,"cache_read_input_tokens":800,"cache_creation_input_tokens":100}}`),
		UsageDiagnostics{UsageProvider: "claude", UsageSource: "upstream"}); err != nil {
		t.Fatal(err)
	}
	var anthropicPossible int64
	if err := store.DB().QueryRowContext(ctx, `SELECT max_possible_cache_read_tokens FROM usage_records
WHERE account_id='ceiling-anthropic'`).Scan(&anthropicPossible); err != nil {
		t.Fatal(err)
	}
	if anthropicPossible == 900 {
		t.Fatal("Anthropic ceiling was overwritten with the prompt total instead of breakpoint metadata")
	}
}

func TestDeferredOpenAICacheDiagnosticsBackfillsV3MarkedRowsOnce(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		usageCacheDiagnosticsMigrationMarker, "1", Now()); err != nil {
		t.Fatal(err)
	}
	insert := func(account, route string, prompt, cached int64) {
		t.Helper()
		raw := fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":1,"total_tokens":%d,"input_tokens_details":{"cached_tokens":%d}}}`,
			prompt, prompt+1, cached)
		if _, err := store.DB().ExecContext(ctx, `INSERT INTO usage_records(account_id,route_key_hash,model,prompt_tokens,completion_tokens,total_tokens,
cached_tokens,cache_read_tokens,usage_provider,usage_source,raw_usage_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			account, route, "gpt-5.6-sol", prompt, 1, prompt+1, cached, cached, "codex", "upstream", raw, Now()); err != nil {
			t.Fatal(err)
		}
	}
	insert("legacy-codex-cache", "legacy", 32768, 28672)
	if err := store.RunDeferredMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	var present, marker int
	var capability string
	var possible int64
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_read_present,cache_capability,max_possible_cache_read_tokens
FROM usage_records WHERE account_id='legacy-codex-cache'`).Scan(&present, &capability, &possible); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, openAICacheDiagnosticsMigrationMarker).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if present != 1 || capability != "hit_observed" || possible != 32768 || marker != 1 {
		t.Fatalf("v4 backfill present=%d capability=%q possible=%d marker=%d", present, capability, possible, marker)
	}

	// The durable marker makes later startups constant-time and leaves new rows to
	// the corrected normal insert path rather than repeatedly rescanning history.
	insert("post-v4-manual-row", "post", 8192, 4096)
	if err := store.RunDeferredMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT cache_read_present FROM usage_records WHERE account_id='post-v4-manual-row'`).Scan(&present); err != nil {
		t.Fatal(err)
	}
	if present != 0 {
		t.Fatalf("completed v4 migration rescanned a later manual row: present=%d", present)
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
		{Account{ID: "kiro-active", GroupName: "kiro", Provider: "KIRO", Status: "active"}, AccountToken{AccessToken: "kiro-token"}},
		{Account{ID: "cursor-active", GroupName: "cursor", Provider: "Cursor", Status: "active"}, AccountToken{AccessToken: "cursor-token"}},
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
		Total:       6,
		Active:      4,
		Quarantined: 1,
		Cooling:     1,
		Recheck:     1,
		Codex:       2,
		Claude:      1,
		Kiro:        1,
		Cursor:      1,
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

func TestListAccountsPageByAuthTypeSeparatesAPIKeysAndLoginAccounts(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	for _, item := range []struct {
		account Account
		token   AccountToken
	}{
		{Account{ID: "platform-key", Label: "Platform key", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AuthMethod: "api_key", AccessToken: "sk-test"}},
		{Account{ID: "oauth-login", Label: "OAuth login", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AuthMethod: "oauth", AccessToken: "access-test", RefreshToken: "refresh-test"}},
		{Account{ID: "kiro-key", Label: "Kiro key", GroupName: "kiro", Provider: "kiro", Status: "active"}, AccountToken{}},
	} {
		if err := store.UpsertAccount(ctx, item.account, item.token); err != nil {
			t.Fatalf("upsert %s: %v", item.account.ID, err)
		}
	}
	if err := store.UpsertKiroCredentials(ctx, KiroCredentials{
		AccountID: "kiro-key", AuthMethod: "api_key", AuthRegion: "us-east-1", APIRegion: "us-east-1", KiroAPIKey: "ksk_test",
	}); err != nil {
		t.Fatal(err)
	}

	apiKeys, total, err := store.ListAccountsPageByAuthType(ctx, 20, 0, "", "", "api_key")
	if err != nil || total != 2 || len(apiKeys) != 2 {
		t.Fatalf("api-key page total=%d rows=%+v err=%v", total, apiKeys, err)
	}
	apiKeyIDs := map[string]bool{}
	for _, account := range apiKeys {
		apiKeyIDs[account.ID] = true
	}
	if !apiKeyIDs["platform-key"] || !apiKeyIDs["kiro-key"] || apiKeyIDs["oauth-login"] {
		t.Fatalf("api-key ids=%v", apiKeyIDs)
	}

	logins, total, err := store.ListAccountsPageByAuthType(ctx, 20, 0, "", "", "account")
	if err != nil || total != 1 || len(logins) != 1 || logins[0].ID != "oauth-login" {
		t.Fatalf("login page total=%d rows=%+v err=%v", total, logins, err)
	}
	searched, total, err := store.ListAccountsPageByAuthType(ctx, 20, 0, "Kiro", "", "api_key")
	if err != nil || total != 1 || len(searched) != 1 || searched[0].ID != "kiro-key" {
		t.Fatalf("searched api-key page total=%d rows=%+v err=%v", total, searched, err)
	}
	grouped, total, err := store.ListAccountsPageFiltered(ctx, 20, 0, "", "", "api_key", "kiro")
	if err != nil || total != 1 || len(grouped) != 1 || grouped[0].ID != "kiro-key" {
		t.Fatalf("group-filtered api-key page total=%d rows=%+v err=%v", total, grouped, err)
	}
}

func TestAccountReadersNormalizeLegacyNullableIdentityFields(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	now := Now()
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO accounts(
 id,label,group_name,upstream_account_id,chatgpt_user_id,email,plan_type,
 provider,status,created_at,updated_at
) VALUES('legacy-null-account','Legacy nullable identity','cyber',NULL,NULL,NULL,NULL,'codex','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}

	listed, err := store.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list accounts with legacy NULL identity fields: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed accounts = %d, want 1", len(listed))
	}
	got := listed[0]
	if got.UpstreamAccountID != "" || got.ChatGPTUserID != "" || got.Email != "" || got.PlanType != "" {
		t.Fatalf("nullable identity fields were not normalized: %+v", got)
	}

	paged, total, err := store.ListAccountsPage(ctx, 20, 0, "", "")
	if err != nil {
		t.Fatalf("page accounts with legacy NULL identity fields: %v", err)
	}
	if total != 1 || len(paged) != 1 || paged[0].ID != got.ID {
		t.Fatalf("paged accounts total=%d rows=%+v", total, paged)
	}

	loaded, err := store.GetAccount(ctx, got.ID)
	if err != nil {
		t.Fatalf("get account with legacy NULL identity fields: %v", err)
	}
	if loaded.Email != "" || loaded.ChatGPTUserID != "" {
		t.Fatalf("loaded nullable fields were not normalized: %+v", loaded)
	}
}

func TestReplaceCapabilitiesPersistsAuthoritativeEmptyCatalog(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertAccount(ctx, Account{ID: "account", GroupName: "cyber", Provider: "codex", Status: "active"}, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []ModelCapability{{AccountID: "account", ModelSlug: "stale", AvailabilityState: "unverified", Source: "static"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceCapabilities(ctx, "account", nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListCapabilities(ctx, "account")
	if err != nil || len(rows) != 0 {
		t.Fatalf("successful empty catalog retained stale rows=%+v err=%v", rows, err)
	}
	authoritative, err := store.ModelCatalogAuthoritative(ctx, "account")
	if err != nil || !authoritative {
		t.Fatalf("successful empty catalog authority=%v err=%v", authoritative, err)
	}
}

func TestModelCapabilityRuntimeEvidencePreservesProviderAndWindowAxes(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertAccount(ctx, Account{ID: "claude", GroupName: "cyber", Provider: "claude", Status: "active"}, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []ModelCapability{{
		AccountID: "claude", ModelSlug: "claude-opus-4-8", AvailabilityState: "unverified",
		NativeContextWindow: 200000, NativeMaxContextWindow: 1000000, Source: "claude_static_unverified",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelCapabilityState(ctx, "claude", "claude-opus-4-8", "verified", "supported", "runtime_inference", "claude_runtime_inference"); err != nil {
		t.Fatal(err)
	}
	standard, err := store.BestNativeWindow(ctx, "claude", "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	maximum, err := store.BestNativeMaxWindow(ctx, "claude", "claude-opus-4-8")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListCapabilities(ctx, "claude")
	if err != nil || len(rows) != 1 || rows[0].Source != "claude_runtime_inference" || standard != 200000 || maximum != 1000000 {
		t.Fatalf("rows=%+v standard=%d maximum=%d err=%v", rows, standard, maximum, err)
	}
}

func TestRuntimeEvidenceDoesNotEraseLiveCatalogAuthority(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.UpsertAccount(ctx, Account{ID: "claude-live", GroupName: "cyber", Provider: "claude", Status: "active"}, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceCapabilities(ctx, "claude-live", []ModelCapability{{
		AccountID: "claude-live", ModelSlug: "claude-opus-4-8", AvailabilityState: "verified", Source: "claude_probe",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetModelCapabilityState(ctx, "claude-live", "claude-opus-4-8", "verified", "unknown", "", "claude_runtime_inference"); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListCapabilities(ctx, "claude-live")
	if err != nil || len(rows) != 1 || rows[0].Source != "claude_probe+claude_runtime_inference" {
		t.Fatalf("combined capability source=%+v err=%v", rows, err)
	}
	// Rewriting the retained rows after a failed background probe must preserve
	// the successful live catalog's authority, including absence of other models.
	if err := store.UpsertCapabilities(ctx, rows); err != nil {
		t.Fatal(err)
	}
	authoritative, err := store.ModelCatalogAuthoritative(ctx, "claude-live")
	if err != nil || !authoritative {
		t.Fatalf("live catalog authority=%v err=%v", authoritative, err)
	}
}

func TestListRoutableCapabilitiesIgnoresHealthyStandbyWhenPrimaryUnavailable(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "standby-capability", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, EgressProfile{ID: "blocked-primary", Name: "blocked", Type: "direct", Health: "disabled"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, EgressProfile{ID: "healthy-standby", Name: "standby", Type: "direct", Health: "healthy"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(ctx, AccountEgressBinding{
		AccountID: account.ID, PrimaryEgressID: "blocked-primary", StandbyEgressIDs: "healthy-standby", BindingScope: EgressBindingScopeAccount,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []ModelCapability{{
		AccountID: account.ID, ModelSlug: "gpt-standby", AvailabilityState: "verified", Source: "probe",
	}}); err != nil {
		t.Fatal(err)
	}

	caps, err := store.ListRoutableCapabilities(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 0 {
		t.Fatalf("healthy standby incorrectly made the account routable: %+v", caps)
	}
	if err := store.SetEgressHealth(ctx, "blocked-primary", "healthy"); err != nil {
		t.Fatal(err)
	}
	caps, err = store.ListRoutableCapabilities(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 || caps[0].ModelSlug != "gpt-standby" {
		t.Fatalf("healthy primary capability missing: %+v", caps)
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

func TestUsageEventIdempotencyAndActualReplacement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	d := UsageDiagnostics{UsageEventID: "evt-1", Estimated: true}
	if err := s.InsertUsageRecordWithDiagnostics(ctx, "a", "r", "", "", "gpt", 5, 0, 5, 0, 0, 0, json.RawMessage(`{"estimated":true}`), d); err != nil {
		t.Fatal(err)
	}
	d.Estimated = false
	if err := s.InsertUsageRecordWithDiagnostics(ctx, "a", "r", "", "", "gpt", 7, 3, 10, 0, 0, 0, json.RawMessage(`{"total_tokens":10}`), d); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertUsageRecordWithDiagnostics(ctx, "a", "r", "", "", "gpt", 99, 1, 100, 0, 0, 0, json.RawMessage(`{"total_tokens":100}`), d); err != nil {
		t.Fatal(err)
	}
	var count, total, estimated int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),total_tokens,estimated FROM usage_records WHERE usage_event_id='evt-1'`).Scan(&count, &total, &estimated); err != nil {
		t.Fatal(err)
	}
	if count != 1 || total != 10 || estimated != 0 {
		t.Fatalf("count=%d total=%d estimated=%d", count, total, estimated)
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

func TestCodexCacheCapabilityPersistsProbeEvidence(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "codex-capability", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{OpenAIAPIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	want := CodexCacheCapability{AccountID: account.ID, Model: "gpt-5.6-sol", ExplicitBreakpointState: "supported", FirstWriteTokens: 1000, SecondReadTokens: 2000, ProbedAt: 1234}
	if err := store.SetCodexCacheCapability(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetCodexCacheCapability(ctx, account.ID, "GPT-5.6-SOL")
	if err != nil || got.ExplicitBreakpointState != "supported" || got.FirstWriteTokens != 1000 || got.SecondReadTokens != 2000 || got.ProbedAt != 1234 {
		t.Fatalf("capability=%+v err=%v", got, err)
	}
}

func TestOpenAICacheWritePresenceAndRoutingDiagnosticsPersist(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	diag := UsageDiagnostics{
		UsageProvider: "codex", UsageSource: "upstream", PromptCacheKeyPresent: true,
		PromptCacheKeyHash: "0123456789abcdef", PromptCacheKeyShard: 3, PromptCacheKeyMinuteRPM: 14, PromptCacheKeyConcurrencyPeak: 5,
		CoordinationPrefixSource: "explicit_breakpoint", SingleflightWaitReason: "matching_cold_prefix", SingleflightReleaseReason: "response_headers",
	}
	raw := json.RawMessage(`{"input_tokens":100,"output_tokens":1,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`)
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "acc", "route", "", "", "gpt-5.6-sol", 100, 1, 101, 0, 0, 0, raw, diag); err != nil {
		t.Fatal(err)
	}
	var creationPresent, shard, rpm, peak int64
	var hash, prefix, waitReason, releaseReason string
	err := store.DB().QueryRowContext(ctx, `SELECT cache_creation_present, prompt_cache_key_hash, prompt_cache_key_shard, prompt_cache_key_minute_rpm, prompt_cache_key_concurrency_peak, coordination_prefix_source, singleflight_wait_reason, singleflight_release_reason FROM usage_records WHERE route_key_hash='route'`).Scan(
		&creationPresent, &hash, &shard, &rpm, &peak, &prefix, &waitReason, &releaseReason)
	if err != nil || creationPresent != 1 || hash != diag.PromptCacheKeyHash || shard != 3 || rpm != 14 || peak != 5 || prefix != diag.CoordinationPrefixSource || waitReason != diag.SingleflightWaitReason || releaseReason != diag.SingleflightReleaseReason {
		t.Fatalf("persisted diagnostics present=%d hash=%q shard=%d rpm=%d peak=%d prefix=%q wait=%q release=%q err=%v", creationPresent, hash, shard, rpm, peak, prefix, waitReason, releaseReason, err)
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
