package api

import (
	"context"
	"testing"

	"codex-account-pool/internal/storage"
)

func streamPlanFixture(t *testing.T, planType string) codexStreamRateLimits {
	t.Helper()
	rl, ok := parseCodexRateLimitsEvent(map[string]interface{}{
		"type": "codex.rate_limits", "planType": planType,
		"rateLimit": map[string]interface{}{
			"primary": map[string]interface{}{
				"usedPercent": float64(42), "windowMinutes": float64(300),
				"resetAfterSeconds": float64(120),
			},
		},
	})
	if !ok {
		t.Fatalf("fixture with planType %q did not parse", planType)
	}
	return rl
}

// The stored plan and the stream header come from different producers with
// different vocabularies for the same entitlement. Comparing them as raw text
// reports a change on every completed request, so each side rewrites the other
// forever: a plan_type UPDATE plus a scheduler cache refresh per stream, on exactly
// the path whose latency is the user-visible complaint.
func TestCaptureCodexStreamRateLimitsDoesNotRewritePlanForAnEquivalentTier(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	account := storage.Account{
		ID: "stream-plan-tier-stable", Provider: "codex", PlanType: "Pro Plus",
		Status: "active", GroupName: "cyber",
	}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccountID: account.ID, AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}

	h.app.captureCodexStreamRateLimits(account, streamPlanFixture(t, "pro"))
	h.app.WaitForAsyncWrites()

	got, err := h.store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanType != "Pro Plus" {
		t.Errorf("stored plan = %q, want it left as %q: the stream reported the same tier "+
			"spelled differently, which must not trigger a write", got.PlanType, "Pro Plus")
	}

	// The function must still do its actual job; "no plan write" must not mean
	// "returned early and dropped the quota snapshot".
	if _, found, err := h.store.GetAccountRateLimitFor(ctx, account.ID, "codex", "", "5h_polled"); err != nil || !found {
		t.Fatalf("rate-limit snapshot was lost: found=%v err=%v", found, err)
	}
}

func TestCaptureCodexStreamRateLimitsStillPersistsARealTierChange(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		stored  string
		stream  string
		wantEnd string
	}{
		{name: "upgrade", stored: "plus", stream: "pro", wantEnd: "pro"},
		{name: "first-plan-ever-seen", stored: "", stream: "pro", wantEnd: "pro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := storage.Account{
				ID: "stream-plan-tier-" + tc.name, Provider: "codex", PlanType: tc.stored,
				Status: "active", GroupName: "cyber",
			}
			if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccountID: account.ID, AccessToken: "token"}); err != nil {
				t.Fatal(err)
			}

			h.app.captureCodexStreamRateLimits(account, streamPlanFixture(t, tc.stream))
			h.app.WaitForAsyncWrites()

			got, err := h.store.GetAccount(ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.PlanType != tc.wantEnd {
				t.Errorf("stored plan = %q, want %q: a genuine tier change must still land",
					got.PlanType, tc.wantEnd)
			}
		})
	}
}
