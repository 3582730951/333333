package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestBuildQuotaSummarySelectsProviderPrimarySecondaryAndReasons(t *testing.T) {
	now := int64(1_700_000_000)
	token := storage.AccountToken{AccountID: "acc", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now + 3600}
	codex := storage.Account{ID: "acc", Provider: "codex", Status: "active"}
	summary := BuildQuotaSummary(codex, &token, []storage.AccountRateLimit{
		{AccountID: "acc", Provider: "codex", LimiterType: "unified", Source: "unified", UsedPercent: 33, UpdatedAt: now - 30},
		{AccountID: "acc", Provider: "codex", LimiterType: "tokens", Source: "tokens", UsedPercent: 44, UpdatedAt: now - 20},
		{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 55, ResetAt: now + 300, Raw: `{"secondary":{"used_percent":22,"remaining_tokens":700,"limit_tokens":1000,"limit_window_seconds":604800,"reset_after_seconds":86400}}`, UpdatedAt: now - 10},
	}, now)
	if summary.SyncReason != "ok" {
		t.Fatalf("codex sync_reason = %q, want ok: %#v", summary.SyncReason, summary)
	}
	if summary.Primary == nil || summary.Primary.Source != "5h_polled" || summary.Primary.UsedPercent != 55 {
		t.Fatalf("codex primary = %#v, want 5h_polled", summary.Primary)
	}
	if summary.Secondary == nil || summary.Secondary.Source != "raw.secondary" || summary.Secondary.UsedPercent != 22 {
		t.Fatalf("codex secondary = %#v, want raw.secondary", summary.Secondary)
	}

	claudeToken := storage.AccountToken{AccountID: "claude", AccessToken: "sk-ant-oat-access", RefreshToken: "sk-ant-ort-refresh", ExpiresAt: now + 3600}
	claude := storage.Account{ID: "claude", Provider: "claude", Status: "active"}
	claudeSummary := BuildQuotaSummary(claude, &claudeToken, []storage.AccountRateLimit{
		{AccountID: "claude", Provider: "claude", LimiterType: "opus", Source: "opus", UsedPercent: 99, UpdatedAt: now},
		{AccountID: "claude", Provider: "claude", LimiterType: "7d_oauth_usage", Source: "7d_oauth_usage", UsedPercent: 12, UpdatedAt: now - 2},
		{AccountID: "claude", Provider: "claude", LimiterType: "5h_oauth_usage", Source: "5h_oauth_usage", UsedPercent: 42, UpdatedAt: now - 5},
	}, now)
	if claudeSummary.Primary == nil || claudeSummary.Primary.Source != "5h_oauth_usage" || claudeSummary.Primary.UsedPercent != 42 {
		t.Fatalf("claude primary = %#v, want provider-level 5h_oauth_usage", claudeSummary.Primary)
	}
	if claudeSummary.Secondary == nil || claudeSummary.Secondary.Source != "7d_oauth_usage" {
		t.Fatalf("claude secondary = %#v, want 7d_oauth_usage", claudeSummary.Secondary)
	}

	cases := []struct {
		name    string
		account storage.Account
		token   *storage.AccountToken
		snaps   []storage.AccountRateLimit
		want    string
	}{
		{
			name:    "inactive wins over newer error",
			account: storage.Account{ID: "a", Provider: "codex", Status: "disabled"},
			token:   &token,
			snaps: []storage.AccountRateLimit{
				{AccountID: "a", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 20, UpdatedAt: now - 5},
				{AccountID: "a", Provider: "codex", LimiterType: "quota_poll_error", Source: "quota_poll_error", Status: "error/http_error", UpdatedAt: now},
			},
			want: "inactive",
		},
		{
			name:    "newer error marker",
			account: codex,
			token:   &token,
			snaps: []storage.AccountRateLimit{
				{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 20, UpdatedAt: now - 5},
				{AccountID: "acc", Provider: "codex", LimiterType: "quota_poll_error", Source: "quota_poll_error", Status: "error/http_error", UpdatedAt: now},
			},
			want: "error/http_error",
		},
		{name: "missing token", account: codex, token: nil, snaps: nil, want: "token_missing"},
		{name: "expired token", account: codex, token: &storage.AccountToken{AccountID: "acc", AccessToken: "access", ExpiresAt: now - 1}, snaps: nil, want: "token_expired"},
		{name: "unsupported provider", account: storage.Account{ID: "deep", Provider: "deepseek", Status: "active"}, token: &token, snaps: nil, want: "unsupported_provider"},
		{name: "claude non oauth", account: claude, token: &storage.AccountToken{AccountID: "claude", AccessToken: "sk-ant-api"}, snaps: nil, want: "unsupported_claude_non_oauth"},
		{name: "never polled", account: codex, token: &token, snaps: nil, want: "never_polled"},
		{name: "stale", account: codex, token: &token, snaps: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 10, UpdatedAt: now - 901}}, want: "stale"},
		{name: "partial", account: codex, token: &token, snaps: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: -1, UpdatedAt: now - 10}}, want: "partial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildQuotaSummary(tc.account, tc.token, tc.snaps, now)
			if got.SyncReason != tc.want {
				t.Fatalf("sync_reason = %q, want %q (summary=%#v)", got.SyncReason, tc.want, got)
			}
		})
	}
}

func TestAdminQuotaIncludeMissingAndAccountsUseQuotaSummary(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	now := storage.Now()
	accounts := []storage.Account{
		{ID: "old-with-snapshot", Label: "Old", Provider: "codex", Status: "active", GroupName: "cyber"},
		{ID: "new-missing-b", Label: "New B", Provider: "codex", Status: "active", GroupName: "cyber"},
		{ID: "new-missing-c", Label: "New C", Provider: "codex", Status: "active", GroupName: "cyber"},
	}
	for _, account := range accounts {
		if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccountID: account.ID, AccessToken: "access-" + account.ID, ExpiresAt: now + 3600}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.store.DB().ExecContext(ctx, `UPDATE accounts SET created_at = CASE id WHEN 'old-with-snapshot' THEN ? WHEN 'new-missing-b' THEN ? ELSE ? END`, now-30, now-10, now-10); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{AccountID: "old-with-snapshot", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 64, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/quota", "")
	if code != http.StatusOK {
		t.Fatalf("default quota = %d: %s", code, raw)
	}
	var legacyRows []map[string]interface{}
	if err := json.Unmarshal(raw, &legacyRows); err != nil {
		t.Fatalf("default quota must remain array-compatible: %v (%s)", err, raw)
	}
	if len(legacyRows) != 1 || legacyRows[0]["account_id"] != "old-with-snapshot" {
		t.Fatalf("default quota rows = %#v, want only account with snapshot", legacyRows)
	}
	if qs, ok := legacyRows[0]["quota_summary"].(map[string]interface{}); !ok || qs["sync_reason"] != "ok" {
		t.Fatalf("default quota missing same-source quota_summary: %#v", legacyRows[0])
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/quota?include_missing=1&page=1&pageSize=2", "")
	if code != http.StatusOK {
		t.Fatalf("include_missing quota = %d: %s", code, raw)
	}
	var page struct {
		Rows     []map[string]interface{} `json:"rows"`
		Total    int                      `json:"total"`
		Page     int                      `json:"page"`
		PageSize int                      `json:"pageSize"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("decode include_missing quota: %v (%s)", err, raw)
	}
	if page.Total != 3 || page.Page != 1 || page.PageSize != 2 || len(page.Rows) != 2 {
		t.Fatalf("quota page metadata = %+v", page)
	}
	if page.Rows[0]["account_id"] != "new-missing-c" || page.Rows[1]["account_id"] != "new-missing-b" {
		t.Fatalf("quota page order = %#v, want created_at DESC, id DESC", page.Rows)
	}
	for _, row := range page.Rows {
		qs, _ := row["quota_summary"].(map[string]interface{})
		if qs["sync_reason"] != "never_polled" {
			t.Fatalf("missing account quota_summary = %#v, want never_polled", row)
		}
	}
	for _, path := range []string{"/admin/quota?include_missing=1&page=0&pageSize=2", "/admin/quota?include_missing=1&page=abc&pageSize=2", "/admin/quota?include_missing=1&page=1&pageSize=0"} {
		if code, raw := grpReq(t, h, http.MethodGet, path, ""); code != http.StatusBadRequest {
			t.Fatalf("%s = %d, want 400: %s", path, code, raw)
		}
	}

	code, raw = grpReq(t, h, http.MethodGet, "/admin/accounts?page=1&pageSize=10", "")
	if code != http.StatusOK {
		t.Fatalf("accounts page = %d: %s", code, raw)
	}
	var accountsPage struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &accountsPage); err != nil {
		t.Fatalf("decode accounts page: %v (%s)", err, raw)
	}
	if len(accountsPage.Accounts) == 0 {
		t.Fatal("accounts page empty")
	}
	for _, row := range accountsPage.Accounts {
		if _, ok := row["quota_summary"].(map[string]interface{}); !ok {
			t.Fatalf("account row missing quota_summary: %#v", row)
		}
	}
}
