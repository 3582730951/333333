package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/cursorproxy"
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
		{name: "cursor user api key", account: storage.Account{ID: "cursor", Provider: "cursor", Status: "active"}, token: &storage.AccountToken{AccountID: "cursor", AuthMethod: "api_key", OpenAIAPIKey: "key"}, snaps: nil, want: "unsupported_cursor_api_key_billing"},
		{name: "cursor browser never polled", account: storage.Account{ID: "cursor-browser", Provider: "cursor", Status: "active"}, token: &storage.AccountToken{AccountID: "cursor-browser", CredentialMode: cursorproxy.CredentialBrowser, AccessToken: "bridge"}, snaps: nil, want: "never_polled"},
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

func TestEstimateQuota(t *testing.T) {
	now := int64(1_700_000_000)
	cases := []struct {
		name          string
		account       storage.Account
		primary       *storage.AccountRateLimit
		credits       *QuotaCredits
		wantEstimated bool
		wantMethod    string
		wantLimit     float64
		wantUsed      float64
		wantRemaining float64
		wantExtra     float64
		wantPlan      string
	}{
		{
			// sub2api model: a subscription window is a rate limit, not dollars —
			// no plan price is ever converted, no matter how full the window is.
			name: "codex plus 55% used", account: storage.Account{Provider: "codex", PlanType: "plus"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_polled", UsedPercent: 55, UpdatedAt: now},
			wantEstimated: false, wantMethod: "window_based", wantPlan: "plus",
		},
		{
			name: "claude pro 42% used", account: storage.Account{Provider: "claude", PlanType: "pro"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_oauth_usage", UsedPercent: 42, UpdatedAt: now},
			wantEstimated: false, wantMethod: "window_based", wantPlan: "pro",
		},
		{
			name: "claude max 20x", account: storage.Account{Provider: "claude", PlanType: "max_20x"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_oauth_usage", UsedPercent: 0, UpdatedAt: now},
			wantEstimated: false, wantMethod: "window_based", wantPlan: "max_20x",
		},
		{
			// Without a price table every plan on a subscription provider is
			// window_based; the plan name no longer selects a dollar baseline.
			name: "unlisted plan", account: storage.Account{Provider: "codex", PlanType: "cosmic"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_polled", UsedPercent: 10, UpdatedAt: now},
			wantEstimated: false, wantMethod: "window_based", wantPlan: "cosmic",
		},
		{
			name: "api key payg", account: storage.Account{Provider: "codex", PlanType: "api"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_polled", UsedPercent: 10, UpdatedAt: now},
			wantEstimated: false, wantMethod: "pay_as_you_go",
		},
		{
			name: "deepseek not supported", account: storage.Account{Provider: "deepseek", PlanType: "payg"},
			primary:       &storage.AccountRateLimit{LimiterType: "deepseek_balance", UsedPercent: -1, UpdatedAt: now},
			wantEstimated: false, wantMethod: "not_supported",
		},
		{
			// A subscription plan plus a real upstream balance: the balance is the
			// only dollar figure and is surfaced verbatim (no plan price added).
			name: "plus credits added", account: storage.Account{Provider: "codex", PlanType: "plus"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_polled", UsedPercent: 50, UpdatedAt: now},
			credits:       &QuotaCredits{HasCredits: true, Balance: "$5.00"},
			wantEstimated: true, wantMethod: "payg_credits_balance", wantRemaining: 5, wantExtra: 5, wantPlan: "plus",
		},
		{
			// A window row without a percentage changes nothing: windows never
			// produce dollars, so the basis is window_based with no numbers.
			name: "no window data", account: storage.Account{Provider: "claude", PlanType: "pro"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_oauth_usage", UsedPercent: -1, UpdatedAt: now},
			wantEstimated: false, wantMethod: "window_based", wantPlan: "pro",
		},
		{
			// Pay-as-you-go has no plan window, but an upstream credit balance is
			// the one real dollar figure available and should surface as remaining.
			name: "payg credits balance", account: storage.Account{Provider: "codex", PlanType: "payg"},
			primary:       &storage.AccountRateLimit{LimiterType: "5h_polled", UsedPercent: 10, UpdatedAt: now},
			credits:       &QuotaCredits{HasCredits: true, Balance: "$25.00"},
			wantEstimated: true, wantMethod: "payg_credits_balance", wantRemaining: 25, wantExtra: 25,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			est := estimateQuota(tc.account, tc.primary, tc.credits, now)
			if est == nil {
				t.Fatal("estimate nil")
			}
			if est.Estimated != tc.wantEstimated || est.Method != tc.wantMethod {
				t.Fatalf("estimated=%v method=%q, want estimated=%v method=%q", est.Estimated, est.Method, tc.wantEstimated, tc.wantMethod)
			}
			if est.Plan != tc.wantPlan {
				t.Fatalf("plan=%q, want %q", est.Plan, tc.wantPlan)
			}
			if tc.wantEstimated {
				if est.LimitUSD != tc.wantLimit || est.UsedUSD != tc.wantUsed || est.RemainingUSD != tc.wantRemaining || est.ExtraUSD != tc.wantExtra {
					t.Fatalf("limit=%v used=%v remaining=%v extra=%v, want limit=%v used=%v remaining=%v extra=%v", est.LimitUSD, est.UsedUSD, est.RemainingUSD, est.ExtraUSD, tc.wantLimit, tc.wantUsed, tc.wantRemaining, tc.wantExtra)
				}
			}
		})
	}
}

func TestBuildQuotaSummaryIncludesEstimate(t *testing.T) {
	now := int64(1_700_000_000)
	token := storage.AccountToken{AccountID: "acc", AccessToken: "access", RefreshToken: "refresh", ExpiresAt: now + 3600}
	account := storage.Account{ID: "acc", Provider: "codex", PlanType: "plus", Status: "active"}
	summary := BuildQuotaSummary(account, &token, []storage.AccountRateLimit{
		{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 25, UpdatedAt: now - 10},
	}, now)
	if summary.SyncReason != "ok" {
		t.Fatalf("sync_reason=%q, want ok: %#v", summary.SyncReason, summary)
	}
	if summary.Estimate == nil || summary.Estimate.Method != "window_based" {
		t.Fatalf("estimate missing window_based basis: %#v", summary)
	}
	// A subscription window must never fabricate dollars (sub2api model).
	if summary.Estimate.Estimated || summary.Estimate.RemainingUSD != 0 || summary.Estimate.LimitUSD != 0 {
		t.Fatalf("window_based estimate must carry no fabricated USD: %#v", summary.Estimate)
	}
	if summary.Estimate.UsedPercent != 25 {
		t.Fatalf("used_percent=%v, want 25", summary.Estimate.UsedPercent)
	}
	// unsupported-billing gates must not produce an estimate
	apiKey := storage.Account{ID: "acc", Provider: "codex", PlanType: "plus", Status: "active"}
	apiToken := storage.AccountToken{AccountID: "acc", OpenAIAPIKey: "sk-test"}
	apiSummary := BuildQuotaSummary(apiKey, &apiToken, []storage.AccountRateLimit{
		{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", UsedPercent: 25, UpdatedAt: now - 10},
	}, now)
	if apiSummary.Supported || apiSummary.Estimate != nil {
		t.Fatalf("api-key billing should not produce an estimate: %#v", apiSummary)
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
