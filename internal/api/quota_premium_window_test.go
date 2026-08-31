package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/storage"
)

func TestCodexWhamPrimaryWindowKind(t *testing.T) {
	tests := []struct {
		name                             string
		primarySeconds, secondarySeconds int64
		want                             string
	}{
		{name: "plus_five_hour_and_weekly", primarySeconds: 5 * 60 * 60, secondarySeconds: 7 * 24 * 60 * 60, want: quotaWindowKind5h},
		{name: "premium_weekly_only", primarySeconds: 7 * 24 * 60 * 60, want: quotaWindowKind7d},
		{name: "ordinary_short_primary_only", primarySeconds: 5 * 60 * 60, want: quotaWindowKind5h},
		{name: "missing_primary", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := codexWhamPrimaryWindowKind(test.primarySeconds, test.secondarySeconds); got != test.want {
				t.Fatalf("kind=%q want=%q", got, test.want)
			}
		})
	}
}

func TestPremiumFixtureStatus(t *testing.T) {
	if got := premiumFixtureStatus(nil, false); got != "reviewed_mapping_active_waiting_for_match" {
		t.Fatalf("waiting status=%q", got)
	}
	multiplier, noFiveHour := int64(5000), true
	evidence := &storage.AccountEntitlementEvidence{
		RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, SourceKind: "quota_metadata",
		SeatType: entitlement.SeatBusinessPremium, Confidence: "high",
		UsageMultiplierMilli: &multiplier, NoFiveHourLimit: &noFiveHour,
	}
	if got := premiumFixtureStatus(evidence, false); got != "recognized_reviewed_5x_fixture_v1" {
		t.Fatalf("recognized status=%q", got)
	}
	if got := premiumFixtureStatus(evidence, true); got != "reviewed_mapping_active_evidence_conflict" {
		t.Fatalf("conflict status=%q", got)
	}
}

func TestAccountPlanPresentationRequiresCompleteReviewedPremiumEvidence(t *testing.T) {
	multiplier, noFiveHour := int64(5000), true
	complete := &storage.AccountEntitlementEvidence{
		RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, SourceKind: "quota_metadata",
		PlanFamily: entitlement.PlanBusiness, SeatType: entitlement.SeatBusinessPremium, Confidence: "high",
		UsageMultiplierMilli: &multiplier, NoFiveHourLimit: &noFiveHour,
	}
	if got := accountPlanPresentationFor(entitlement.BusinessPremiumRawPlanLabel, complete, false); got.Combined != "Team (5×)" || got.SeatDisplayName != "Premium (5×)" {
		t.Fatalf("complete fixture presentation=%+v", got)
	}
	for _, test := range []struct {
		name     string
		evidence *storage.AccountEntitlementEvidence
		conflict bool
	}{
		{name: "missing quota source", evidence: &storage.AccountEntitlementEvidence{
			RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, SourceKind: "imported_hint", PlanFamily: entitlement.PlanBusiness,
			SeatType: entitlement.SeatBusinessPremium, Confidence: "high", UsageMultiplierMilli: &multiplier, NoFiveHourLimit: &noFiveHour,
		}},
		{name: "conflicting evidence", evidence: complete, conflict: true},
		{name: "wrong multiplier", evidence: &storage.AccountEntitlementEvidence{
			RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, SourceKind: "quota_metadata", PlanFamily: entitlement.PlanBusiness,
			SeatType: entitlement.SeatBusinessPremium, Confidence: "high", UsageMultiplierMilli: int64Ptr(1000), NoFiveHourLimit: &noFiveHour,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := accountPlanPresentationFor(entitlement.BusinessPremiumRawPlanLabel, test.evidence, test.conflict); got.Combined == "Team (5×)" {
				t.Fatalf("incomplete evidence was presented as Premium: %+v", got)
			}
		})
	}
}

func TestAccountAndQuotaViewsExposePresentationNotRawPlan(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	account := storage.Account{ID: "acct-premium-view", Provider: "codex", PlanType: entitlement.BusinessPremiumRawPlanLabel, Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AuthMethod: "oauth"}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()
	multiplier, noFiveHour := int64(5000), true
	if _, err := store.InsertAccountEntitlementEvidence(ctx, storage.AccountEntitlementEvidence{
		ID: "ent-premium-view", AccountID: account.ID, SourceKind: "quota_metadata", SourceFingerprint: strings.Repeat("c", 32),
		PlanFamily: entitlement.PlanBusiness, SeatType: entitlement.SeatBusinessPremium, UsageMultiplierMilli: &multiplier,
		NoFiveHourLimit: &noFiveHour, RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, Confidence: "high",
		ObservedAt: now - 1, ExpiresAt: now + 3600, PayloadRedacted: []byte(`{"mapping_version":"fixture"}`),
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store}
	accounts, err := server.accountViews(ctx, []storage.Account{account})
	if err != nil {
		t.Fatal(err)
	}
	quota, err := server.quotaViewsForAccounts(ctx, []storage.Account{account})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []interface{}{accounts[0], quota[0]} {
		body, marshalErr := json.Marshal(view)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(body), entitlement.BusinessPremiumRawPlanLabel) || !strings.Contains(string(body), "Team (5×)") {
			t.Fatalf("presentation response leaked raw or missed Team (5×): %s", body)
		}
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestBusinessStandardFiveHourPriorUsesQualifiedPlusBaseline(t *testing.T) {
	used, remaining, lower, upper := int64(500_000), int64(50_000_000), int64(40_000_000), int64(60_000_000)
	qualified := storage.AccountCapacityEstimatePlan{
		AccountCapacityEstimate: storage.AccountCapacityEstimate{
			AccountID: "plus-source", LimiterKind: "5h", ModelFamily: "gpt-5.6-sol", ServiceTier: "default",
			UsedRatioPPM: &used, USDEquivalentMicro: &remaining, LowerBoundUnits: &lower, UpperBoundUnits: &upper,
			UnitKind: "api_list_price_equivalent_micro_usd", Method: "empirical_quota_window_versioned_valuation",
			SampleCount: 8, Confidence: "high", UpdatedAt: 123,
		},
		PlanType: "plus",
	}
	ignoredPro := qualified
	ignoredPro.AccountID, ignoredPro.PlanType = "pro-source", "pro_plus"
	prior := deriveBusinessStandardFiveHourPrior([]storage.AccountCapacityEstimatePlan{ignoredPro, qualified})
	if prior.Status != "derived_from_plus_empirical_5h" || prior.FactorMilli != 1250 || prior.Role != "capacity_prior_not_entitlement" {
		t.Fatalf("unexpected prior metadata: %+v", prior)
	}
	if len(prior.Estimates) != 1 {
		t.Fatalf("qualified estimates=%d want=1: %+v", len(prior.Estimates), prior.Estimates)
	}
	estimate := prior.Estimates[0]
	if estimate.LimitMicroUSD != 125_000_000 || estimate.LowerMicroUSD == nil || *estimate.LowerMicroUSD != 100_000_000 ||
		estimate.UpperMicroUSD == nil || *estimate.UpperMicroUSD != 150_000_000 {
		t.Fatalf("Plus×1.25 fixed-point derivation is wrong: %+v", estimate)
	}
}

func TestBusinessStandardFiveHourPriorWaitsWithoutReliablePlusSample(t *testing.T) {
	used, remaining := int64(1_000_000), int64(50_000_000)
	prior := deriveBusinessStandardFiveHourPrior([]storage.AccountCapacityEstimatePlan{{
		AccountCapacityEstimate: storage.AccountCapacityEstimate{
			ModelFamily: "gpt-5.6-sol", ServiceTier: "default", UsedRatioPPM: &used,
			USDEquivalentMicro: &remaining, UnitKind: "api_list_price_equivalent_micro_usd",
			Method: "empirical_quota_window_versioned_valuation", SampleCount: 8, Confidence: "high",
		},
		PlanType: "plus",
	}})
	if prior.Status != "waiting_for_plus_empirical_5h_baseline" || len(prior.Estimates) != 0 {
		t.Fatalf("exhausted/invalid Plus sample must not produce a prior: %+v", prior)
	}
}
