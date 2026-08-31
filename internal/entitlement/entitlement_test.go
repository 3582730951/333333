package entitlement

import "testing"

func reviewedPremiumStreamFixture() map[string]interface{} {
	return map[string]interface{}{
		"plan_type": "self_serve_business_prolite",
		"rate_limits": map[string]interface{}{
			"primary": map[string]interface{}{
				"used_percent":        float64(29),
				"window_minutes":      float64(10080),
				"reset_after_seconds": float64(588235),
			},
		},
	}
}

func TestReviewedPremiumStreamFixture(t *testing.T) {
	normalized, observation := NormalizedFromWire(reviewedPremiumStreamFixture(), "quota_metadata", false, false)
	if normalized.SeatType != SeatBusinessPremium || normalized.PlanFamily != PlanBusiness || normalized.Confidence != "high" {
		t.Fatalf("unexpected normalized entitlement: %+v", normalized)
	}
	if normalized.UsageMultiplierMilli == nil || *normalized.UsageMultiplierMilli != 5000 {
		t.Fatalf("missing 5x multiplier: %+v", normalized)
	}
	if normalized.NoFiveHourLimit == nil || !*normalized.NoFiveHourLimit {
		t.Fatalf("missing no-five-hour evidence: %+v", normalized)
	}
	if observation.MappingVersion != BusinessPremiumMappingVersion || !observation.MappingReviewed {
		t.Fatalf("mapping provenance missing: %+v", observation)
	}
	if observation.Payload["mapping_version"] != BusinessPremiumMappingVersion {
		t.Fatalf("redacted payload lacks mapping version: %#v", observation.Payload)
	}
}

func TestReviewedPremiumWhamFixture(t *testing.T) {
	root := map[string]interface{}{
		"plan_type": "self_serve_business_prolite",
		"rate_limit": map[string]interface{}{
			"primary_window": map[string]interface{}{"limit_window_seconds": float64(604800)},
		},
	}
	normalized, _ := NormalizedFromWire(root, "quota_metadata", false, false)
	if normalized.SeatType != SeatBusinessPremium {
		t.Fatalf("wham fixture should be Premium: %+v", normalized)
	}
}

func TestReviewedPremiumMappingRequiresBothSignals(t *testing.T) {
	tests := []struct {
		name string
		root map[string]interface{}
	}{
		{"generic_business_weekly_only", map[string]interface{}{
			"plan_type":   "business",
			"rate_limits": map[string]interface{}{"primary": map[string]interface{}{"window_minutes": float64(10080)}},
		}},
		{"usage_based_weekly_only", map[string]interface{}{
			"plan_type":   "self_serve_business_usage_based",
			"rate_limits": map[string]interface{}{"primary": map[string]interface{}{"window_minutes": float64(10080)}},
		}},
		{"five_x_subtype_plan_only", map[string]interface{}{
			"plan_type": "self_serve_business_prolite",
		}},
		{"five_x_subtype_with_five_hour", map[string]interface{}{
			"plan_type": "self_serve_business_prolite",
			"rate_limits": map[string]interface{}{
				"primary":   map[string]interface{}{"window_minutes": float64(300)},
				"secondary": map[string]interface{}{"window_minutes": float64(10080)},
			},
		}},
		{"weekly_primary_with_secondary", map[string]interface{}{
			"plan_type": "self_serve_business_prolite",
			"rate_limits": map[string]interface{}{
				"primary":   map[string]interface{}{"window_minutes": float64(10080)},
				"secondary": map[string]interface{}{"window_minutes": float64(300)},
			},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			normalized, _ := NormalizedFromWire(test.root, "quota_metadata", false, false)
			if normalized.SeatType != SeatUnknown {
				t.Fatalf("unsafe Premium classification: %+v", normalized)
			}
		})
	}
}

func TestReviewedPremiumMappingRejectsContradictoryFlags(t *testing.T) {
	root := reviewedPremiumStreamFixture()
	root["seat_type"] = "premium"
	root["usage_multiplier"] = float64(1)
	root["no_five_hour_limit"] = true
	normalized, _ := NormalizedFromWire(root, "quota_metadata", true, false)
	if normalized.SeatType != SeatUnknown {
		t.Fatalf("contradictory 1x evidence must fail closed: %+v", normalized)
	}
}

func TestReviewedPremiumMappingFailsClosedForInvalidExplicitFlags(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value interface{}
	}{
		{name: "fractional multiplier", key: "usage_multiplier", value: float64(5.5)},
		{name: "negative multiplier", key: "usage_multiplier", value: float64(-5)},
		{name: "string multiplier", key: "usage_multiplier", value: "5"},
		{name: "zero multiplier", key: "usage_multiplier", value: float64(0)},
		{name: "string boolean", key: "no_five_hour_limit", value: "true"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := reviewedPremiumStreamFixture()
			root[test.key] = test.value
			normalized, observation := NormalizedFromWire(root, "quota_metadata", false, false)
			if normalized.SeatType != SeatUnknown || normalized.FlagsState != "contradictory" {
				t.Fatalf("invalid explicit field was not fail-closed: normalized=%+v observation=%+v", normalized, observation)
			}
		})
	}
}
