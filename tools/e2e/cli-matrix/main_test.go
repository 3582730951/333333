package main

import (
	"strings"
	"testing"
)

func TestScenarioPhaseDistinguishesColdSteadyAndParallel(t *testing.T) {
	tests := map[string]string{
		coldWarmupScenario:       "cold",
		steadyCacheBaseline:      "steady",
		"same-key-parallel":      "parallel",
		"different-key-parallel": "parallel",
		"manual":                 "other",
	}
	for scenario, want := range tests {
		if got := scenarioPhase(scenario); got != want {
			t.Fatalf("scenarioPhase(%q) = %q, want %q", scenario, got, want)
		}
	}
}

func TestRedactSecretsRemovesPlaintextKeyFromErrors(t *testing.T) {
	const secret = "cap_disposable_secret_value"
	got := redactSecrets("failed override bearer="+secret+" and again "+secret, secret)
	if strings.Contains(got, secret) {
		t.Fatalf("redacted error still contains disposable key: %q", got)
	}
	if strings.Count(got, "[REDACTED]") != 2 {
		t.Fatalf("redacted error = %q, want both key occurrences replaced", got)
	}
}

func TestScenarioCacheRatesKeepsColdAndSteadySeparate(t *testing.T) {
	rates := scenarioCacheRates([]cliResult{
		{Scenario: coldWarmupScenario, Usage: cliUsage{Input: 100, Cached: 0}},
		{Scenario: steadyCacheBaseline, Usage: cliUsage{Input: 100, Cached: 80}},
		{Scenario: "same-key-parallel", Usage: cliUsage{Input: 100, Cached: 70}},
		{Scenario: "same-key-parallel", Usage: cliUsage{Input: 100, Cached: 90}},
	})
	if rates[coldWarmupScenario] != 0 {
		t.Fatalf("cold rate = %v, want 0", rates[coldWarmupScenario])
	}
	if rates[steadyCacheBaseline] != 0.8 {
		t.Fatalf("steady rate = %v, want 0.8", rates[steadyCacheBaseline])
	}
	if rates["same-key-parallel"] != 0.8 {
		t.Fatalf("parallel rate = %v, want 0.8", rates["same-key-parallel"])
	}
}

func TestScenarioGeneratedInferenceStatsCountsOnlyCompletedGenerationTurns(t *testing.T) {
	stats := scenarioGeneratedInferenceStats([]cliResult{
		{Scenario: steadyCacheBaseline, InferenceCompleted: true, Usage: cliUsage{Input: 100, Cached: 80}},
		{Scenario: steadyCacheBaseline, InferenceCompleted: true, Usage: cliUsage{Input: 50}},
		// A failed/non-completed request is not a generated inference result, even if
		// a partial event happened to carry token-looking values.
		{Scenario: steadyCacheBaseline, Usage: cliUsage{Input: 999, Cached: 999}},
		{Scenario: "same-key-parallel", InferenceCompleted: true, Usage: cliUsage{Input: 200, Cached: 100}},
	})

	steady := stats[steadyCacheBaseline]
	if steady.Requests != 2 || steady.HitRequests != 1 {
		t.Fatalf("steady request counts = %+v, want requests=2 hit_requests=1", steady)
	}
	if steady.PromptTokens != 150 || steady.CachedTokens != 80 {
		t.Fatalf("steady token counts = %+v, want prompt=150 cached=80", steady)
	}
	wantRate := 80.0 / 150.0
	if steady.TokenHitRate != wantRate {
		t.Fatalf("steady token_hit_rate = %v, want %v", steady.TokenHitRate, wantRate)
	}
	parallel := stats["same-key-parallel"]
	if parallel.Requests != 1 || parallel.HitRequests != 1 || parallel.TokenHitRate != 0.5 {
		t.Fatalf("parallel stats = %+v", parallel)
	}
}

func TestReportScopesDistinguishGenerationFromAdminPrewarmRollup(t *testing.T) {
	if !strings.Contains(generatedInferenceScope, "excludes") || !strings.Contains(generatedInferenceScope, "generate=false") {
		t.Fatalf("generated inference scope is ambiguous: %q", generatedInferenceScope)
	}
	if !strings.Contains(adminAPIKeyCacheScope, "includes") || !strings.Contains(adminAPIKeyCacheScope, "generate=false") {
		t.Fatalf("admin cache scope is ambiguous: %q", adminAPIKeyCacheScope)
	}
}
