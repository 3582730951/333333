package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

// The extra paid balance reported by /wham/usage travels in the `credits` and
// `spend_control` blocks, which are siblings of `rate_limit` in the codex-backend
// RateLimitStatusPayload. It must land in its own limiter row so it never gets
// mistaken for a rate-limit window, and must survive the round trip back out
// through BuildQuotaSummary.
func TestPollOneCodexQuotaPersistsExtraCredits(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"pro",
			"rate_limit":{
				"allowed":true,
				"limit_reached":false,
				"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_after_seconds":600},
				"secondary_window":{"used_percent":55,"limit_window_seconds":604800,"reset_after_seconds":86400}
			},
			"credits":{"has_credits":true,"unlimited":false,"balance":"$12.50"},
			"spend_control":{
				"reached":false,
				"individual_limit":{
					"source":"workspace",
					"limit":"$100.00",
					"used":"$37.00",
					"remaining":"$63.00",
					"used_percent":37,
					"remaining_percent":63,
					"reset_after_seconds":172800,
					"reset_at":1893456000
				}
			}
		}`))
	})
	oldUsageURL := whamUsageURL
	whamUsageURL = h.upstream.URL + "/backend-api/wham/usage"
	t.Cleanup(func() { whamUsageURL = oldUsageURL })

	ctx := context.Background()
	acc := storage.Account{ID: "acc-credits", Provider: "codex", Status: "active", UpstreamAccountID: "upstream-credits", GroupName: "cyber"}
	token := storage.AccountToken{AccountID: acc.ID, AccessToken: "access-credits"}
	if err := h.store.UpsertAccount(ctx, acc, token); err != nil {
		t.Fatal(err)
	}
	if err := h.app.pollOneCodexQuota(ctx, acc, token, storage.EgressProfile{Type: "direct"}); err != nil {
		t.Fatal(err)
	}

	snap, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", codexCreditsLimiterType)
	if err != nil || !ok {
		t.Fatalf("credits snapshot missing ok=%v err=%v", ok, err)
	}
	if snap.UsedPercent != 37 || snap.Status != "ok" || snap.ResetAt != 1893456000 {
		t.Fatalf("credits snapshot = %+v", snap)
	}

	summary := BuildQuotaSummary(acc, &token, []storage.AccountRateLimit{snap}, storage.Now())
	if summary.Credits == nil {
		t.Fatal("quota summary has no credits block")
	}
	if !summary.Credits.HasCredits || summary.Credits.Unlimited {
		t.Fatalf("credits flags = %+v", summary.Credits)
	}
	if summary.Credits.Balance != "$12.50" || summary.Credits.Limit != "$100.00" || summary.Credits.Used != "$37.00" || summary.Credits.Remaining != "$63.00" {
		t.Fatalf("credits display strings = %+v", summary.Credits)
	}
	if summary.Credits.UsedPercent != 37 || summary.Credits.RemainingPercent != 63 {
		t.Fatalf("credits percentages = %+v", summary.Credits)
	}
	if !summary.Credits.SpendControl {
		t.Fatalf("spend_control flag not set: %+v", summary.Credits)
	}
	// The balance row must never be promoted into the 5h/7d window slots.
	if summary.Primary != nil && summary.Primary.LimiterType == codexCreditsLimiterType {
		t.Fatalf("credits row leaked into primary window: %+v", summary.Primary)
	}
	// The live wham plan_type must be persisted so the USD estimate keys off the
	// current plan, not the plan detected at registration/import time.
	got, err := h.store.GetAccount(ctx, acc.ID)
	if err != nil || got.PlanType != "pro" {
		t.Fatalf("account plan_type = %q (err=%v), want %q", got.PlanType, err, "pro")
	}
}

// A plan without extra credits must not produce a row at all, so the console can
// hide the panel rather than render an empty one.
func TestPollOneCodexQuotaOmitsCreditsWhenUpstreamSilent(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"plan_type":"plus",
			"rate_limit":{
				"limit_reached":false,
				"primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_after_seconds":600}
			}
		}`))
	})
	oldUsageURL := whamUsageURL
	whamUsageURL = h.upstream.URL + "/backend-api/wham/usage"
	t.Cleanup(func() { whamUsageURL = oldUsageURL })

	ctx := context.Background()
	acc := storage.Account{ID: "acc-no-credits", Provider: "codex", Status: "active", UpstreamAccountID: "upstream-no-credits"}
	token := storage.AccountToken{AccountID: acc.ID, AccessToken: "access-no-credits"}
	if err := h.store.UpsertAccount(ctx, acc, token); err != nil {
		t.Fatal(err)
	}
	if err := h.app.pollOneCodexQuota(ctx, acc, token, storage.EgressProfile{Type: "direct"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", codexCreditsLimiterType); err != nil || ok {
		t.Fatalf("unexpected credits snapshot ok=%v err=%v", ok, err)
	}
}

// Depleted and unlimited are distinct states: neither may collapse into the other,
// and an unlimited balance has no percentage to report.
func TestCodexCreditsStatusReflectsUpstreamState(t *testing.T) {
	for _, tc := range []struct {
		name             string
		body             string
		wantStatus       string
		wantPct          float64
		wantSpendControl bool
	}{
		{
			name:       "depleted",
			body:       `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}},"credits":{"has_credits":false,"unlimited":false,"balance":"$0.00"}}`,
			wantStatus: "depleted",
			wantPct:    -1,
		},
		{
			name:       "unlimited",
			body:       `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}},"credits":{"has_credits":true,"unlimited":true}}`,
			wantStatus: "unlimited",
			wantPct:    -1,
		},
		{
			name:       "spend limit reached",
			body:       `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}},"credits":{"has_credits":true,"unlimited":false},"spend_control":{"reached":true}}`,
			wantStatus: "spend_limit_reached",
			wantPct:    -1,
		},
		{
			// Spend-control-only rows (no credits block at all) must be
			// distinguishable from a genuinely depleted balance: has_credits=false
			// here means "no extra balance", not "balance used up".
			name:             "spend control only",
			body:             `{"rate_limit":{"primary_window":{"used_percent":5,"limit_window_seconds":18000,"reset_after_seconds":60}},"spend_control":{"reached":false,"individual_limit":{"source":"workspace","limit":"$50.00","used":"$10.00","remaining":"$40.00","used_percent":20}}}`,
			wantStatus:       "ok",
			wantPct:          20,
			wantSpendControl: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			oldUsageURL := whamUsageURL
			whamUsageURL = h.upstream.URL + "/backend-api/wham/usage"
			t.Cleanup(func() { whamUsageURL = oldUsageURL })

			ctx := context.Background()
			acc := storage.Account{ID: "acc-" + tc.name, Provider: "codex", Status: "active", UpstreamAccountID: "upstream-" + tc.name}
			token := storage.AccountToken{AccountID: acc.ID, AccessToken: "access-" + tc.name}
			if err := h.store.UpsertAccount(ctx, acc, token); err != nil {
				t.Fatal(err)
			}
			if err := h.app.pollOneCodexQuota(ctx, acc, token, storage.EgressProfile{Type: "direct"}); err != nil {
				t.Fatal(err)
			}
			snap, ok, err := h.store.GetAccountRateLimitFor(ctx, acc.ID, "codex", "", codexCreditsLimiterType)
			if err != nil || !ok {
				t.Fatalf("credits snapshot missing ok=%v err=%v", ok, err)
			}
			if snap.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", snap.Status, tc.wantStatus)
			}
			if snap.UsedPercent != tc.wantPct {
				t.Fatalf("used percent = %v, want %v", snap.UsedPercent, tc.wantPct)
			}
			summary := BuildQuotaSummary(acc, &token, []storage.AccountRateLimit{snap}, storage.Now())
			if tc.wantSpendControl && (summary.Credits == nil || !summary.Credits.SpendControl) {
				t.Fatalf("spend_control flag missing in summary: %+v", summary.Credits)
			}
		})
	}
}
