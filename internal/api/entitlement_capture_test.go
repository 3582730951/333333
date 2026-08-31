package api

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/storage"
)

func TestCaptureReviewedPremiumFixtureEndToEnd(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acct-fixture", Provider: "codex"}, storage.AccountToken{AuthMethod: "oauth"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: store, identitySecretCached: bytes.Repeat([]byte{0x42}, 32)}
	server.captureEntitlementEvidence(ctx, "acct-fixture", "quota_metadata", map[string]interface{}{
		"plan_type":    "self_serve_business_prolite",
		"access_token": "must-not-be-persisted",
		"rate_limits": map[string]interface{}{
			"primary": map[string]interface{}{"window_minutes": float64(10080), "used_percent": float64(29)},
		},
	})
	current, conflict, err := store.CurrentAccountEntitlementEvidence(ctx, "acct-fixture", storage.Now())
	if err != nil {
		t.Fatal(err)
	}
	if conflict || current == nil || current.SeatType != entitlement.SeatBusinessPremium {
		t.Fatalf("fixture was not recognized: current=%+v conflict=%v", current, conflict)
	}
	if current.UsageMultiplierMilli == nil || *current.UsageMultiplierMilli != 5000 || current.NoFiveHourLimit == nil || !*current.NoFiveHourLimit {
		t.Fatalf("fixture flags are incomplete: %+v", current)
	}
	payload := strings.ToLower(string(current.PayloadRedacted))
	if strings.Contains(payload, "access_token") || strings.Contains(payload, "must-not-be-persisted") {
		t.Fatalf("credential leaked into evidence payload: %s", payload)
	}
	if !strings.Contains(payload, entitlement.BusinessPremiumMappingVersion) {
		t.Fatalf("mapping provenance missing: %s", payload)
	}
}

func TestEntitlementEvidenceFingerprintRefreshesAcrossObservationBuckets(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 32)
	payload := []byte(`{"mapping_version":"fixture"}`)
	first := entitlementEvidenceFingerprint(secret, "account", "quota_metadata", payload, entitlementEvidenceRefreshBucketSeconds)
	sameBucket := entitlementEvidenceFingerprint(secret, "account", "quota_metadata", payload, entitlementEvidenceRefreshBucketSeconds+59)
	later := entitlementEvidenceFingerprint(secret, "account", "quota_metadata", payload, 2*entitlementEvidenceRefreshBucketSeconds)
	if first == "" || first != sameBucket || later == first {
		t.Fatalf("evidence fingerprint must dedupe within, and refresh across, observation buckets: first=%q same=%q later=%q", first, sameBucket, later)
	}
}

func TestCurrentEntitlementEvidenceByAccountIDsUsesSameReviewedWinner(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"acct-premium", "acct-plus"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: accountID, Provider: "codex"}, storage.AccountToken{AuthMethod: "oauth"}); err != nil {
			t.Fatal(err)
		}
	}
	now := storage.Now()
	multiplier, noFiveHour := int64(5000), true
	for _, evidence := range []storage.AccountEntitlementEvidence{
		{
			ID: "ent-premium", AccountID: "acct-premium", SourceKind: "quota_metadata", SourceFingerprint: strings.Repeat("a", 32),
			PlanFamily: entitlement.PlanBusiness, SeatType: entitlement.SeatBusinessPremium, UsageMultiplierMilli: &multiplier,
			NoFiveHourLimit: &noFiveHour, RawPlanLabel: entitlement.BusinessPremiumRawPlanLabel, Confidence: "high",
			ObservedAt: now - 1, ExpiresAt: now + 3600, PayloadRedacted: []byte(`{"mapping_version":"fixture"}`),
		},
		{
			ID: "ent-plus", AccountID: "acct-plus", SourceKind: "quota_metadata", SourceFingerprint: strings.Repeat("b", 32),
			PlanFamily: entitlement.PlanPlus, SeatType: entitlement.SeatUnknown, RawPlanLabel: "plus", Confidence: "low",
			ObservedAt: now - 1, ExpiresAt: now + 3600, PayloadRedacted: []byte(`{"plan_type":"plus"}`),
		},
	} {
		if _, err := store.InsertAccountEntitlementEvidence(ctx, evidence); err != nil {
			t.Fatal(err)
		}
	}
	current, conflicts, err := store.CurrentAccountEntitlementEvidenceByAccountIDs(ctx, []string{"acct-premium", "acct-plus", "missing"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if current["acct-premium"] == nil || !entitlement.IsReviewedBusinessPremiumEvidence(
		current["acct-premium"].RawPlanLabel, current["acct-premium"].SourceKind, current["acct-premium"].SeatType,
		current["acct-premium"].Confidence, current["acct-premium"].UsageMultiplierMilli, current["acct-premium"].NoFiveHourLimit,
	) {
		t.Fatalf("batched current evidence did not retain reviewed Premium: %#v", current["acct-premium"])
	}
	if conflicts["acct-premium"] || current["acct-plus"] == nil || current["missing"] != nil {
		t.Fatalf("unexpected batched evidence result: current=%#v conflicts=%#v", current, conflicts)
	}
}
