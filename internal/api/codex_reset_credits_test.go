package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestParseCodexResetCredits(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		source     string
		wantKnown  bool
		wantStatus string
		wantCount  int64
		wantSource string
	}{
		{
			name:       "snake count zero is known",
			body:       `{"available_count":0}`,
			source:     "rate-limit-reset-credits",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  0,
			wantSource: "rate-limit-reset-credits",
		},
		{
			name:       "camel count",
			body:       `{"availableCount":3}`,
			source:     "rate-limit-reset-credits",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  3,
			wantSource: "rate-limit-reset-credits",
		},
		{
			name: "credits fallback counts available entries",
			body: `{"credits":[
				{"id":"c1","reset_type":"codex_rate_limits","status":"available","expires_at":"2026-07-05T00:00:00Z"},
				{"id":"c2","resetType":"codex_rate_limits","status":"consumed","expiresAt":"2026-07-05T00:00:00Z"},
				{"id":"c3","resetType":"other","status":"available","expiresAt":"2026-07-05T00:00:00Z"},
				{"id":"c4","resetType":"codex_rate_limits","status":"available"}
			]}`,
			source:     "rate-limit-reset-credits",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  3,
			wantSource: "rate-limit-reset-credits",
		},
		{
			name:       "numeric string count",
			body:       `{"available_count":"3"}`,
			source:     "rate-limit-reset-credits",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  3,
			wantSource: "rate-limit-reset-credits",
		},
		{
			name:       "partial numeric string is unknown",
			body:       `{"available_count":"3abc"}`,
			source:     "rate-limit-reset-credits",
			wantStatus: "unknown",
			wantSource: "rate-limit-reset-credits",
		},
		{
			name:       "fractional count is unknown",
			body:       `{"available_count":1.5}`,
			source:     "rate-limit-reset-credits",
			wantStatus: "unknown",
			wantSource: "rate-limit-reset-credits",
		},
		{
			name:       "usage fallback snake",
			body:       `{"rate_limit_reset_credits":{"available_count":4}}`,
			source:     "usage_fallback",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  4,
			wantSource: "usage_fallback",
		},
		{
			name:       "usage fallback camel zero is known",
			body:       `{"rateLimitResetCredits":{"availableCount":0}}`,
			source:     "usage_fallback",
			wantKnown:  true,
			wantStatus: "ok",
			wantCount:  0,
			wantSource: "usage_fallback",
		},
		{
			name:       "invalid is unknown",
			body:       `{"available_count":"nope"}`,
			source:     "rate-limit-reset-credits",
			wantKnown:  false,
			wantStatus: "unknown",
			wantCount:  0,
			wantSource: "rate-limit-reset-credits",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseCodexResetCredits([]byte(tt.body), tt.source)
			if got.Known != tt.wantKnown || got.Status != tt.wantStatus || got.AvailableCount != tt.wantCount || got.Source != tt.wantSource {
				t.Fatalf("parse = %+v, want known=%v status=%q count=%d source=%q", got, tt.wantKnown, tt.wantStatus, tt.wantCount, tt.wantSource)
			}
		})
	}
}

func TestCodexResetConsumedRequiresExplicitResetCode(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{body: `{"code":"reset","windows_reset":2}`, want: true},
		{body: `{"code":"nothing_to_reset"}`},
		{body: `{"code":"no_credit"}`},
		{body: `{"code":"already_redeemed"}`},
		{body: `{}`},
		{body: `not-json`},
	}
	for _, tt := range tests {
		if got := codexResetConsumed([]byte(tt.body)); got != tt.want {
			t.Fatalf("codexResetConsumed(%s) = %v, want %v", tt.body, got, tt.want)
		}
	}
}

func TestCodexChatGPTAccountIDNeverUsesLocalPoolID(t *testing.T) {
	if got := codexChatGPTAccountID(storage.Account{ID: "local-pool-id"}); got != "" {
		t.Fatalf("account ID = %q, want empty when no upstream ID is known", got)
	}
	account := storage.Account{ID: "local", UpstreamAccountID: "workspace", ChatGPTUserID: "chatgpt"}
	if got := codexChatGPTAccountID(account); got != "chatgpt" {
		t.Fatalf("account ID = %q, want chatgpt", got)
	}
}

func TestPollOneCodexQuotaWritesFiveHourAndSevenDaySnapshots(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Fatalf("unexpected wham path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-quota" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("ChatGPT-Account-ID") != "upstream-quota" {
			t.Fatalf("account header = %q", r.Header.Get("ChatGPT-Account-ID"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{
				"limit_reached":true,
				"primary_window":{"used_percent":88,"limit_window_seconds":18000,"reset_after_seconds":600},
				"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_after_seconds":86400}
			},
			"rate_limit_reset_credits":{"available_count":2}
		}`))
	})
	oldUsageURL := whamUsageURL
	whamUsageURL = h.upstream.URL + "/backend-api/wham/usage"
	t.Cleanup(func() { whamUsageURL = oldUsageURL })

	ctx := context.Background()
	acc := storage.Account{ID: "acc-quota", Provider: "codex", Status: "active", UpstreamAccountID: "upstream-quota", GroupName: "cyber"}
	token := storage.AccountToken{AccountID: acc.ID, AccessToken: "access-quota"}
	if err := h.store.UpsertAccount(ctx, acc, token); err != nil {
		t.Fatal(err)
	}
	if err := h.app.pollOneCodexQuota(ctx, acc, token, storage.EgressProfile{Type: "direct"}); err != nil {
		t.Fatal(err)
	}

	five, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", "5h_polled")
	if err != nil || !ok {
		t.Fatalf("5h snapshot missing ok=%v err=%v", ok, err)
	}
	if five.UsedPercent != 88 || five.LimiterType != "5h_polled" {
		t.Fatalf("5h snapshot = %+v", five)
	}
	seven, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", "7d_polled")
	if err != nil || !ok {
		t.Fatalf("7d snapshot missing ok=%v err=%v", ok, err)
	}
	if seven.UsedPercent != 100 || seven.LimiterType != "7d_polled" || seven.ResetAt <= storage.Now() {
		t.Fatalf("7d snapshot = %+v", seven)
	}
	reset, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", "codex_reset_credits")
	if err != nil || !ok {
		t.Fatalf("reset credits snapshot missing ok=%v err=%v", ok, err)
	}
	if reset.RemainingRequests != 2 || reset.Status != "ok" || reset.Source != "usage_fallback" {
		t.Fatalf("reset credits snapshot = %+v", reset)
	}
}

func TestPollOneCodexQuotaDoesNotMarkSevenDayRejectedWhenOnlyPrimaryReached(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rate_limit":{
				"limit_reached":true,
				"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":600},
				"secondary_window":{"used_percent":42,"limit_window_seconds":604800,"reset_after_seconds":86400}
			}
		}`))
	})
	oldUsageURL := whamUsageURL
	whamUsageURL = h.upstream.URL + "/backend-api/wham/usage"
	t.Cleanup(func() { whamUsageURL = oldUsageURL })

	ctx := context.Background()
	acc := storage.Account{ID: "acc-primary-only", Provider: "codex", Status: "active", UpstreamAccountID: "upstream-primary-only", GroupName: "cyber"}
	token := storage.AccountToken{AccountID: acc.ID, AccessToken: "access-primary-only"}
	if err := h.store.UpsertAccount(ctx, acc, token); err != nil {
		t.Fatal(err)
	}
	if err := h.app.pollOneCodexQuota(ctx, acc, token, storage.EgressProfile{Type: "direct"}); err != nil {
		t.Fatal(err)
	}
	seven, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", "7d_polled")
	if err != nil || !ok {
		t.Fatalf("7d snapshot missing ok=%v err=%v", ok, err)
	}
	if seven.Status == "rejected" {
		t.Fatalf("7d snapshot must not inherit primary limit_reached: %+v", seven)
	}
	decision := codexResetGateFromSnapshots([]storage.AccountRateLimit{seven}, storage.Now())
	if decision.AllowConsume {
		t.Fatalf("reset gate allowed consume for 7d available snapshot: %+v", decision)
	}
}

func TestBuildQuotaSummaryIncludesResetCreditsWithoutChangingPrimaryOrSyncReason(t *testing.T) {
	now := int64(1_700_000_000)
	account := storage.Account{ID: "acc", Provider: "codex", Status: "active"}
	token := storage.AccountToken{AccountID: "acc", AccessToken: "access", ExpiresAt: now + 3600}
	summary := BuildQuotaSummary(account, &token, []storage.AccountRateLimit{
		{AccountID: "acc", Provider: "codex", LimiterType: "5h_polled", Source: "5h_polled", UsedPercent: 12, UpdatedAt: now - 10},
		{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 99, ResetAt: now + 600, UpdatedAt: now - 10},
		{AccountID: "acc", Provider: "codex", LimiterType: "codex_reset_credits", Source: "rate-limit-reset-credits", RemainingRequests: 0, Status: "ok", UpdatedAt: now - 5},
	}, now)
	if summary.SyncReason != "ok" {
		t.Fatalf("sync_reason = %q, want ok", summary.SyncReason)
	}
	if summary.Primary == nil || summary.Primary.Source != "5h_polled" {
		t.Fatalf("primary = %#v, want 5h_polled", summary.Primary)
	}
	if summary.Secondary == nil || summary.Secondary.Source != "7d_polled" {
		t.Fatalf("secondary = %#v, want 7d_polled", summary.Secondary)
	}
	if summary.ResetCredits == nil || summary.ResetCredits.Status != "ok" || summary.ResetCredits.AvailableCount != 0 || summary.ResetCredits.Source != "rate-limit-reset-credits" {
		t.Fatalf("reset credits = %#v, want known zero", summary.ResetCredits)
	}

	raw, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"available_count":0`) {
		t.Fatalf("summary JSON must keep known zero available_count: %s", raw)
	}
}

func TestCodexResetGateRequiresFreshExhaustedSevenDaySnapshot(t *testing.T) {
	now := int64(1_700_000_000)
	exhausted := storage.AccountRateLimit{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 100, ResetAt: now + 3600, UpdatedAt: now - 30}
	tests := []struct {
		name      string
		snapshots []storage.AccountRateLimit
		wantAllow bool
	}{
		{name: "fresh used percent exhausted proceeds", snapshots: []storage.AccountRateLimit{exhausted}, wantAllow: true},
		{name: "fresh rejected proceeds", snapshots: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 12, Status: "rejected", ResetAt: now + 3600, UpdatedAt: now - 30}}, wantAllow: true},
		{name: "missing skips", snapshots: nil},
		{name: "available skips", snapshots: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 99, ResetAt: now + 3600, UpdatedAt: now - 30}}},
		{name: "stale skips", snapshots: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 100, ResetAt: now + 3600, UpdatedAt: now - quotaFreshSeconds - 1}}},
		{name: "partial skips", snapshots: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: -1, ResetAt: now + 3600, UpdatedAt: now - 30}}},
		{name: "expired window skips", snapshots: []storage.AccountRateLimit{{AccountID: "acc", Provider: "codex", LimiterType: "7d_polled", Source: "7d_polled", UsedPercent: 100, ResetAt: now - 1, UpdatedAt: now - 30}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexResetGateFromSnapshots(tt.snapshots, now)
			if got.AllowConsume != tt.wantAllow {
				t.Fatalf("AllowConsume = %v, want %v (decision=%+v)", got.AllowConsume, tt.wantAllow, got)
			}
		})
	}
}

func TestCodexResetLimitClassifier(t *testing.T) {
	tests := []struct {
		name string
		code int
		body string
		want bool
	}{
		{name: "insufficient quota", code: 429, body: `{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`, want: true},
		{name: "usage limit", code: 200, body: `{"detail":"usage_limit_exceeded"}`, want: true},
		{name: "weekly limit", code: 429, body: `{"message":"weekly limit reached"}`, want: true},
		{name: "insufficient scope does not consume", code: 403, body: `{"error":"insufficient_scope"}`},
		{name: "permission does not consume", code: 403, body: `{"error":"permission denied"}`},
		{name: "region does not consume", code: 403, body: `{"error":"unsupported_country"}`},
		{name: "ban does not consume", code: 403, body: `{"error":"account banned"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexResetTriggerAllowed(tt.code, []byte(tt.body)); got != tt.want {
				t.Fatalf("codexResetTriggerAllowed = %v, want %v", got, tt.want)
			}
		})
	}
}
