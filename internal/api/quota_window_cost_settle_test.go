package api

import "testing"

// A window mixes settled rows with requests still awaiting settlement. `estimated` is
// updated in place on the same usage_records row, so each row is one request and must be
// priced once. The previous gate dropped every estimated row as soon as one settled row
// existed, reporting a fraction of the window's real spend and dragging the USD estimate
// down with it.
func TestQuotaWindowCostCountsUnsettledRequests(t *testing.T) {
	rows := []quotaWindowCostRow{
		{Model: "gpt-5.6-sol", PromptTokens: 10_000, CompletionTokens: 1_000, Estimated: 0},
	}
	for i := 0; i < 99; i++ {
		rows = append(rows, quotaWindowCostRow{Model: "gpt-5.6-sol", PromptTokens: 10_000, CompletionTokens: 1_000, Estimated: 1})
	}

	settledOnly := quotaUsageCostUSD(rows[0].Model, rows[0].PromptTokens, rows[0].CompletionTokens, 0, 0)
	total := 0.0
	for _, row := range rows {
		total += quotaUsageCostUSD(row.Model, row.PromptTokens, row.CompletionTokens, row.CacheReadTokens, row.CacheCreationTokens)
	}
	if settledOnly <= 0 {
		t.Fatal("fixture priced to zero; pricing table lookup failed")
	}
	if ratio := total / settledOnly; ratio < 99 {
		t.Fatalf("window total %.6f is only %.1fx the single settled row; unsettled spend is being dropped", total, ratio)
	}
}

func TestQuotaUsageCostSubtractsCachedInputOnce(t *testing.T) {
	// prompt_tokens subsumes cache reads, so the full-price input is the remainder.
	full := quotaUsageCostUSD("gpt-5.6-sol", 10_000, 0, 0, 0)
	cached := quotaUsageCostUSD("gpt-5.6-sol", 10_000, 0, 10_000, 0)
	if cached >= full {
		t.Fatalf("fully cached input (%.6f) not cheaper than uncached (%.6f)", cached, full)
	}
	if cached <= 0 {
		t.Fatal("cached reads must still cost the discounted rate, not zero")
	}
}

// A window whose cost is mostly pre-settlement still reports the full total — dropping
// those rows is what made the USD estimate read low — but must not present that total
// with the same confidence as a settled window.
func TestQuotaWindowUnsettledShareDowngradesConfidence(t *testing.T) {
	base := []quotaWindowSample{
		{SampleAt: 1_000, UsedPercent: 4, CostUSD: 4},
		{SampleAt: 2_000, UsedPercent: 12, CostUSD: 12},
		{SampleAt: 3_000, UsedPercent: 25, CostUSD: 25},
		{SampleAt: 4_000, UsedPercent: 41, CostUSD: 41},
		{SampleAt: 5_000, UsedPercent: 58, CostUSD: 58},
	}
	settled := append([]quotaWindowSample(nil), base...)
	unsettled := append([]quotaWindowSample(nil), base...)
	unsettled[len(unsettled)-1].UnsettledShare = 0.9

	settledEstimate := estimateQuotaWindowSamples(settled, 5_100, 1<<40)
	unsettledEstimate := estimateQuotaWindowSamples(unsettled, 5_100, 1<<40)

	if settledEstimate.State != "estimated" || unsettledEstimate.State != "estimated" {
		t.Fatalf("states: settled=%q unsettled=%q", settledEstimate.State, unsettledEstimate.State)
	}
	rank := map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3}
	if rank[unsettledEstimate.Confidence] >= rank[settledEstimate.Confidence] {
		t.Fatalf("mostly-unsettled window kept confidence %q against settled %q",
			unsettledEstimate.Confidence, settledEstimate.Confidence)
	}
	// The dollar figure itself must be unchanged — only the confidence moves.
	if unsettledEstimate.Cost.Center != settledEstimate.Cost.Center {
		t.Fatalf("unsettled share changed the estimate: %v vs %v",
			unsettledEstimate.Cost.Center, settledEstimate.Cost.Center)
	}
}

// A share at or under the limit must not downgrade: normal traffic always has some
// requests in flight, and penalizing that would make every live window read as soft.
func TestQuotaWindowSmallUnsettledShareKeepsConfidence(t *testing.T) {
	base := []quotaWindowSample{
		{SampleAt: 1_000, UsedPercent: 4, CostUSD: 4},
		{SampleAt: 2_000, UsedPercent: 12, CostUSD: 12},
		{SampleAt: 3_000, UsedPercent: 25, CostUSD: 25},
		{SampleAt: 4_000, UsedPercent: 41, CostUSD: 41},
		{SampleAt: 5_000, UsedPercent: 58, CostUSD: 58},
	}
	settled := estimateQuotaWindowSamples(base, 5_100, 1<<40)
	light := append([]quotaWindowSample(nil), base...)
	light[len(light)-1].UnsettledShare = quotaWindowUnsettledShareLimit
	if got := estimateQuotaWindowSamples(light, 5_100, 1<<40); got.Confidence != settled.Confidence {
		t.Fatalf("share at the limit downgraded confidence: %q vs %q", got.Confidence, settled.Confidence)
	}
}
