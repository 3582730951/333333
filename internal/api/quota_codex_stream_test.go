package api

import (
	"context"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestParseCodexRateLimitsEvent(t *testing.T) {
	ev := map[string]interface{}{
		"type":      "codex.rate_limits",
		"plan_type": "pro",
		"rate_limits": map[string]interface{}{
			"primary":   map[string]interface{}{"used_percent": float64(42), "reset_after_seconds": float64(600)},
			"secondary": map[string]interface{}{"used_percent": float64(10), "resets_in_seconds": float64(86400)},
		},
	}
	rl, ok := parseCodexRateLimitsEvent(ev)
	if !ok || !rl.primary.present || !rl.secondary.present {
		t.Fatalf("parse failed: %+v ok=%v", rl, ok)
	}
	if rl.primary.usedPct != 42 || rl.primary.resetAfterSeconds != 600 {
		t.Fatalf("primary wrong: %+v", rl)
	}
	if rl.secondary.usedPct != 10 || rl.secondary.resetAfterSeconds != 86400 {
		t.Fatalf("secondary wrong: %+v", rl)
	}

	// A window with used_percent but no reset is SKIPPED, so a partial frame never
	// clobbers the complete reset the /wham/usage poll already stored.
	ev2 := map[string]interface{}{"type": "codex.rate_limits", "rate_limits": map[string]interface{}{"primary": map[string]interface{}{"used_percent": float64(99)}}}
	if rl2, ok2 := parseCodexRateLimitsEvent(ev2); ok2 || rl2.primary.present {
		t.Fatalf("partial window (no reset) must be skipped: %+v ok=%v", rl2, ok2)
	}

	// Numeric strings are tolerated (Codex sometimes stringifies).
	ev3 := map[string]interface{}{"type": "codex.rate_limits", "rate_limits": map[string]interface{}{"primary": map[string]interface{}{"used_percent": "55", "reset_after_seconds": "120"}}}
	rl3, ok3 := parseCodexRateLimitsEvent(ev3)
	if !ok3 || rl3.primary.usedPct != 55 || rl3.primary.resetAfterSeconds != 120 {
		t.Fatalf("string numerics: %+v ok=%v", rl3, ok3)
	}

	// No rate_limits object → nothing.
	if _, ok4 := parseCodexRateLimitsEvent(map[string]interface{}{"type": "codex.rate_limits"}); ok4 {
		t.Fatal("frame without rate_limits must not parse")
	}
}

func TestCodexRecorderCapturesRateLimitsFrame(t *testing.T) {
	rec := newCodexStreamLedgerRecorder()
	_, _ = rec.Write([]byte("event: codex.rate_limits\n" +
		`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":42,"reset_after_seconds":600},"secondary":{"used_percent":7,"reset_after_seconds":86400}}}` + "\n\n"))
	_, _, rateLimits := rec.metadata()
	if !rateLimits.any() {
		t.Fatal("recorder did not capture codex.rate_limits frame")
	}
	if rateLimits.primary.usedPct != 42 || rateLimits.secondary.usedPct != 7 {
		t.Fatalf("recorder captured wrong windows: %+v", rateLimits)
	}
	// The frame must not disturb terminal detection (it is not a terminal event).
	if rec.reachedTerminal() {
		t.Fatal("codex.rate_limits must not count as a terminal event")
	}
}

func TestParseCodexRateLimitsEventSupportsObservedCompatibilityFields(t *testing.T) {
	resetAt := storage.Now() + 6*24*60*60
	ev := map[string]interface{}{
		"type":             "codex.rate_limits",
		"planType":         "self_serve_business",
		"meteredLimitName": "codex_bengalfox",
		"rateLimit": map[string]interface{}{
			// Production websocket captures can expose the weekly window as primary,
			// so window_minutes (not primary/secondary position) must drive mapping.
			"primary": map[string]interface{}{
				"usedPercent": float64(71), "windowMinutes": float64(10080), "resetAt": float64(resetAt),
			},
		},
		"additionalRateLimits": []interface{}{
			map[string]interface{}{
				"limitName": "GPT-5.3-Codex-Spark",
				"rateLimit": map[string]interface{}{
					"primary": map[string]interface{}{
						"usedPercent": float64(13), "windowMinutes": float64(300), "resetAfterSeconds": float64(60),
					},
					"secondary": map[string]interface{}{
						"usedPercent": float64(51), "windowMinutes": float64(10080), "resetAfterSeconds": float64(120),
					},
				},
			},
		},
		"codeReviewRateLimits": map[string]interface{}{
			"primary": map[string]interface{}{
				"usedPercent": float64(4), "windowMinutes": float64(300), "resetAfterSeconds": float64(900),
			},
		},
	}

	rl, ok := parseCodexRateLimitsEvent(ev)
	if !ok {
		t.Fatal("observed compatibility event was not parsed")
	}
	if rl.planType != "self_serve_business" || rl.activeLimit != "codex_bengalfox" {
		t.Fatalf("identity fields = plan %q active %q", rl.planType, rl.activeLimit)
	}
	if !rl.primary.present || rl.primary.usedPct != 71 || rl.primary.windowMinutes != 10080 || rl.primary.resetAt != resetAt {
		t.Fatalf("absolute-reset primary = %+v", rl.primary)
	}
	if len(rl.additional) != 1 || rl.additional[0].name != "GPT-5.3-Codex-Spark" || !rl.additional[0].secondary.present {
		t.Fatalf("additional limits = %+v", rl.additional)
	}
	if rl.codeReview == nil || !rl.codeReview.primary.present || rl.codeReview.primary.usedPct != 4 {
		t.Fatalf("code-review limit = %+v", rl.codeReview)
	}
}

func TestParseCodexRateLimitsEventAcceptsPlanOnlyAndRejectsUnsafePlan(t *testing.T) {
	rl, ok := parseCodexRateLimitsEvent(map[string]interface{}{
		"type": "rate_limits.updated", "planType": "team",
	})
	if !ok || rl.planType != "team" || !rl.any() {
		t.Fatalf("plan-only event = %+v ok=%v", rl, ok)
	}
	if bad, ok := parseCodexRateLimitsEvent(map[string]interface{}{
		"type": "rate_limits.updated", "plan_type": "pro\r\nforged",
	}); ok || bad.planType != "" {
		t.Fatalf("unsafe plan was accepted: %+v ok=%v", bad, ok)
	}
}

func TestCodexRecorderCapturesRateLimitsUpdatedPlanOnlyFrame(t *testing.T) {
	rec := newCodexStreamLedgerRecorder()
	defer rec.Close()
	_, _ = rec.Write([]byte("event: rate_limits.updated\n" +
		`data: {"type":"rate_limits.updated","planType":"team"}` + "\n\n"))
	_, _, rateLimits := rec.metadata()
	if !rateLimits.any() || rateLimits.planType != "team" {
		t.Fatalf("rate_limits.updated metadata = %+v", rateLimits)
	}
}

func TestCaptureCodexStreamRateLimitsPersistsPlanExactResetAndFeatureObservation(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	account := storage.Account{
		ID: "stream-plan-observation", Provider: "codex", PlanType: "plus", Status: "active", GroupName: "cyber",
	}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccountID: account.ID, AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	resetAt := storage.Now() + 6*24*60*60
	rl, ok := parseCodexRateLimitsEvent(map[string]interface{}{
		"type": "codex.rate_limits", "planType": "pro", "meteredLimitName": "codex_bengalfox",
		"rateLimit": map[string]interface{}{
			"primary": map[string]interface{}{
				"usedPercent": float64(81), "windowMinutes": float64(10080), "resetAt": float64(resetAt),
			},
		},
		"additionalRateLimits": map[string]interface{}{
			"GPT-5.3-Codex-Spark": map[string]interface{}{
				"primary": map[string]interface{}{
					"usedPercent": float64(3), "windowMinutes": float64(300), "resetAfterSeconds": float64(60),
				},
			},
		},
		"codeReviewRateLimits": map[string]interface{}{
			"primary": map[string]interface{}{
				"usedPercent": float64(4), "windowMinutes": float64(300), "resetAfterSeconds": float64(900),
			},
		},
	})
	if !ok {
		t.Fatal("fixture did not parse")
	}
	h.app.captureCodexStreamRateLimits(account, rl)
	h.app.WaitForAsyncWrites()

	gotAccount, err := h.store.GetAccount(ctx, account.ID)
	if err != nil || gotAccount.PlanType != "pro" {
		t.Fatalf("persisted plan = %q err=%v, want pro", gotAccount.PlanType, err)
	}
	weekly, found, err := h.store.GetAccountRateLimitFor(ctx, account.ID, "codex", "", "7d_polled")
	if err != nil || !found {
		t.Fatalf("weekly snapshot missing: found=%v err=%v", found, err)
	}
	if weekly.UsedPercent != 81 || weekly.ResetAt != resetAt || weekly.Source != "codex_stream" {
		t.Fatalf("weekly snapshot = %+v", weekly)
	}
	if _, found, err := h.store.GetAccountRateLimitFor(ctx, account.ID, "codex", "", "5h_polled"); err != nil || found {
		t.Fatalf("weekly primary was misclassified as 5h: found=%v err=%v", found, err)
	}
	features, found, err := h.store.GetAccountRateLimitFor(ctx, account.ID, "codex", "", codexStreamFeaturesLimiterType)
	if err != nil || !found {
		t.Fatalf("feature observation missing: found=%v err=%v", found, err)
	}
	if !strings.Contains(features.Raw, "GPT-5.3-Codex-Spark") || !strings.Contains(features.Raw, "code_review_rate_limits") {
		t.Fatalf("feature observation raw = %s", features.Raw)
	}
	if primary := selectQuotaPrimary("codex", []storage.AccountRateLimit{features}); primary != nil {
		t.Fatalf("observation-only feature row became routable quota primary: %+v", primary)
	}
}
