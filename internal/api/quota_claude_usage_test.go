package api

import (
	"testing"

	"codex-account-pool/internal/storage"
)

// TestParseClaudeOAuthUsageUtilizationShape covers the live Claude usage shape the
// reference implementations use — windows keyed five_hour/seven_day with "utilization"
// (used percent) and an RFC3339 "resets_at". Before the fix this parsed to 0%/dropped
// (the "Claude quota wrong" symptom) because the code only read used_percent and treated
// resets_at as an integer, and seven_day did not match the 7d window pattern.
func TestParseClaudeOAuthUsageUtilizationShape(t *testing.T) {
	now := storage.Now()
	body := []byte(`{
		"subscription_type":"max",
		"five_hour":{"utilization":12,"resets_at":"2030-01-01T00:00:00Z"},
		"seven_day":{"utilization":45,"resets_at":"2030-01-02T00:00:00Z"},
		"seven_day_opus":{"utilization":80,"resets_at":"2030-01-02T00:00:00Z"}
	}`)
	parsed, err := parseClaudeOAuthUsage(body, now)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.PlanType != "max" {
		t.Fatalf("plan_type=%q want max", parsed.PlanType)
	}
	byLimiter := map[string]claudeOAuthUsageWindow{}
	for _, w := range parsed.Windows {
		byLimiter[w.LimiterType] = w
	}
	p, ok := byLimiter["5h_oauth_usage"]
	if !ok {
		t.Fatalf("5h window missing from %+v", parsed.Windows)
	}
	if p.UsedPercent != 12 {
		t.Fatalf("5h used=%v want 12 (utilization not read)", p.UsedPercent)
	}
	if p.ResetAt <= now {
		t.Fatalf("5h reset not parsed to a future epoch: %d (now %d)", p.ResetAt, now)
	}
	s, ok := byLimiter["7d_oauth_usage"]
	if !ok {
		t.Fatalf("7d window missing — seven_day not matched: %+v", parsed.Windows)
	}
	if s.UsedPercent != 45 {
		t.Fatalf("7d used=%v want 45", s.UsedPercent)
	}
	// The per-model opus window must not be promoted to a primary/secondary gauge.
	if _, leaked := byLimiter[""]; leaked {
		t.Fatal("a model-specific window leaked as an empty limiter")
	}
}
