package api

import (
	"context"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestCacheRateBandSemantics(t *testing.T) {
	cases := []struct {
		rate, warning, critical float64
		want                    string
	}{
		{0.9, 0.7, 0.4, "healthy"},
		{0.7, 0.7, 0.4, "healthy"}, // at/above warning is healthy
		{0.69, 0.7, 0.4, "warning"},
		{0.4, 0.7, 0.4, "warning"}, // at/above critical is warning
		{0.39, 0.7, 0.4, "critical"},
		{0.0, 0.7, 0.4, "critical"},
		{0.5, 0, 0, "disabled"}, // zero/zero thresholds disable the score
		{0.0, 0, 0, "disabled"},
	}
	for _, tc := range cases {
		if got := cacheRateBand(tc.rate, tc.warning, tc.critical); got != tc.want {
			t.Fatalf("cacheRateBand(%.2f, %.2f, %.2f) = %q, want %q", tc.rate, tc.warning, tc.critical, got, tc.want)
		}
	}
}

func TestCacheRateScoreLinear(t *testing.T) {
	if got := cacheRateScore(0); got != 0 {
		t.Fatalf("score(0) = %v", got)
	}
	if got := cacheRateScore(1); got != 100 {
		t.Fatalf("score(1) = %v", got)
	}
	if got := cacheRateScore(0.5); got != 50 {
		t.Fatalf("score(0.5) = %v", got)
	}
}

func TestCacheAccountHealthRowsAggregatesAndFlags(t *testing.T) {
	codebook := buildDiagnosticCodebookWithKey([]byte("key"), nil, nil, nil, nil, nil, nil)
	codeA, codeB, codeC := codebook.code("acc-a"), codebook.code("acc-b"), codebook.code("acc-c")
	rows := []storage.CacheUsageMetricRow{
		{AccountID: "acc-a", Provider: "openai", Requests: 10, HitRequests: 9},    // 0.9 healthy
		{AccountID: "acc-a", Provider: "openai", Requests: 0, HitRequests: 0},     // zero row must not corrupt
		{AccountID: "acc-b", Provider: "anthropic", Requests: 10, HitRequests: 2}, // 0.2 critical
		{AccountID: "acc-c", Provider: "openai", Requests: 3, HitRequests: 1},     // insufficient samples
	}
	out := cacheAccountHealthRows(rows, codebook, 0.7, 0.4)
	byCode := map[string][]string{}
	for _, row := range out {
		byCode[row[0]] = row
	}
	if got := byCode[codeA]; got[2] != "10" || got[3] != "9" || got[5] != "90.0" || got[6] != "healthy" {
		t.Fatalf("acc-a row = %v", got)
	}
	if got := byCode[codeB]; got[6] != "critical" {
		t.Fatalf("acc-b band = %v", got)
	}
	if got := byCode[codeC]; got[6] != "" || got[5] != "" || got[7] != "insufficient_samples" {
		t.Fatalf("acc-c row = %v", got)
	}
	// Zero/zero thresholds → disabled, not a penalty.
	out = cacheAccountHealthRows(rows, codebook, 0, 0)
	byCode = map[string][]string{}
	for _, row := range out {
		byCode[row[0]] = row
	}
	if got := byCode[codeB]; got[6] != "disabled" || got[7] != "thresholds_disabled" {
		t.Fatalf("disabled row = %v", got)
	}
}

func TestCacheIntervalRowsBucketsAndDiagnoses(t *testing.T) {
	now := time.Now().Unix()
	// Session 1: turns 30s apart, all hits → healthy, no diagnosis.
	// Session 2: turns 30s apart, all misses → drift_miss.
	// Session 3: turns 45m apart, miss → cooldown_miss.
	base := int64(0)
	rows := []diagnosticUsageRecord{
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_a", CreatedAt: now + base + 0, CacheReadPresent: 0, CacheCreationPresent: 1},
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_a", CreatedAt: now + base + 30, CacheReadPresent: 1, CacheCreationPresent: 0},
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_b", CreatedAt: now + base + 1000, CacheReadPresent: 0, CacheCreationPresent: 1},
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_b", CreatedAt: now + base + 1030, CacheReadPresent: 0, CacheCreationPresent: 1},
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_c", CreatedAt: now + base + 2000, CacheReadPresent: 0, CacheCreationPresent: 1},
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_c", CreatedAt: now + base + 2000 + 45*60, CacheReadPresent: 0, CacheCreationPresent: 1},
		// Non-cache request must be excluded from the denominator.
		{UsageProvider: "openai", Model: "gpt-5.6", PromptCacheKeyHash: "key_a", CreatedAt: now + base + 60, CacheReadPresent: 0, CacheCreationPresent: 0},
	}
	out := cacheIntervalRows(rows)
	type row struct{ key, bucket, diag string }
	found := map[row]bool{}
	for _, r := range out {
		found[row{key: r[2], bucket: r[3], diag: r[9]}] = true
	}
	if !found[row{"key_a", "<1m", ""}] {
		t.Fatalf("missing key_a healthy bucket: %+v", out)
	}
	if !found[row{"key_b", "<1m", "drift_miss"}] {
		t.Fatalf("missing key_b drift_miss bucket: %+v", out)
	}
	if !found[row{"key_c", ">30m", "cooldown_miss"}] {
		t.Fatalf("missing key_c cooldown_miss bucket: %+v", out)
	}
}

func TestCacheRebindRowsJoinsFollowupUsage(t *testing.T) {
	store := driftTestStore(t)
	ctx := context.Background()
	rebindAt := time.Now().Add(-2 * time.Hour).Unix()
	if err := store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: "acc-b",
		Action:    "affinity_rebind", State: "recovered", Reason: "sticky_unavailable",
		Detail:    "from=acc-a to=acc-b affinity=aff_1 route_key=route_x group=codex model=gpt-5.6",
		CreatedAt: rebindAt,
	}); err != nil {
		t.Fatal(err)
	}
	usageRows := []diagnosticUsageRecord{
		// Two requests on the target account within the follow window: one hit.
		{AccountID: "acc-b", Model: "gpt-5.6", UsageSource: "upstream", CreatedAt: rebindAt + 10, CacheReadPresent: 1},
		{AccountID: "acc-b", Model: "gpt-5.6", UsageSource: "upstream", CreatedAt: rebindAt + 60, CacheReadPresent: 0},
		// Outside the window must not count.
		{AccountID: "acc-b", Model: "gpt-5.6", UsageSource: "upstream", CreatedAt: rebindAt + 2*cacheRebindFollowWindow, CacheReadPresent: 1},
		// Estimated (non-upstream) rows must not count.
		{AccountID: "acc-b", Model: "gpt-5.6", UsageSource: "estimated", CreatedAt: rebindAt + 20, CacheReadPresent: 1},
	}
	codebook := buildDiagnosticCodebookWithKey([]byte("key"), nil, nil, nil, nil, nil, nil)
	out := cacheRebindRows(ctx, store, codebook, usageRows, rebindAt-10)
	if len(out) != 1 {
		t.Fatalf("want 1 rebind row, got %+v", out)
	}
	row := out[0]
	if row[1] != codebook.code("acc-a") || row[2] != codebook.code("acc-b") {
		t.Fatalf("codes = %v", row[:3])
	}
	if row[5] != "2" || row[6] != "1" {
		t.Fatalf("post-rebind usage = %v", row[5:8])
	}
	if row[7] != "0.5000" {
		t.Fatalf("post-rebind hit rate = %q", row[7])
	}
}
