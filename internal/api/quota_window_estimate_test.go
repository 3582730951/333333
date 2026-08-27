package api

import (
	"math"
	"testing"
)

// Port of the calculate_money reference test suite (test/estimate.test.js), with
// timestamps converted from milliseconds to unix seconds. Every numeric
// expectation is ported verbatim so a behavioral drift of the Go estimator shows
// up immediately.

func sample(index int, usedPercent, cost float64) quotaWindowSample {
	return quotaWindowSample{SampleAt: int64(index) * 60, UsedPercent: usedPercent, CostUSD: cost}
}

func assertClose(t *testing.T, actual, expected, tolerance float64) {
	t.Helper()
	if math.Abs(actual-expected) > tolerance {
		t.Fatalf("%v is not within %v of %v", actual, tolerance, expected)
	}
}

func TestQuotaWindowRoughEstimateFromFirstNonZeroSample(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{sample(1, 20, 4)}, 60, quotaWindowStaleAfterSeconds5h)
	if estimate.State != "estimated" {
		t.Fatalf("state=%q", estimate.State)
	}
	if estimate.Method != "first_sample_rough" {
		t.Fatalf("method=%q", estimate.Method)
	}
	if estimate.Cost.Center != 20 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	if estimate.UsedCost.Center != 4 {
		t.Fatalf("usedCost.center=%v", estimate.UsedCost.Center)
	}
	if estimate.RemainingCost.Center != 16 {
		t.Fatalf("remainingCost.center=%v", estimate.RemainingCost.Center)
	}
	if estimate.USDPerPercent.Center != 0.2 {
		t.Fatalf("usdPerPercent.center=%v", estimate.USDPerPercent.Center)
	}
	if estimate.Confidence != "low" {
		t.Fatalf("confidence=%q", estimate.Confidence)
	}
	if estimate.AlgorithmVersion != "total_cost_robust_v2" {
		t.Fatalf("algorithmVersion=%q", estimate.AlgorithmVersion)
	}
}

func TestQuotaWindowColdEstimateAnchoredToFirstNonZeroSample(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 20, 4),
		sample(2, 20, 5),
	}, 120, quotaWindowStaleAfterSeconds5h)
	if estimate.Method != "first_sample_rough" {
		t.Fatalf("method=%q", estimate.Method)
	}
	if estimate.Cost.Center != 20 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	if estimate.ObservedCost != 5 {
		t.Fatalf("observedCost=%v", estimate.ObservedCost)
	}
	if estimate.Evidence.ColdSampleAt != 60 {
		t.Fatalf("coldSampleAt=%v", estimate.Evidence.ColdSampleAt)
	}
}

func TestQuotaWindowAccumulatesQuantizedPercentageChangesFromPlateauStart(t *testing.T) {
	samples := []quotaWindowSample{
		sample(1, 10, 5),
		sample(2, 10, 5.4),
		sample(3, 10.4, 5.8),
		sample(4, 11, 6),
	}
	estimate := estimateQuotaWindowSamples(samples, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Method != "delta_squared_weighted_median_mad" {
		t.Fatalf("method=%q", estimate.Method)
	}
	if estimate.Evidence.DeltaCandidateCount != 1 {
		t.Fatalf("deltaCandidateCount=%v", estimate.Evidence.DeltaCandidateCount)
	}
	if estimate.Cost.Center != 100 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
}

func TestQuotaWindowUsesSquaredPercentageSpansAsCandidateWeights(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 14, 14),
		sample(3, 17, 20),
		sample(4, 19, 26),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Cost.Center != 100 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	if estimate.Evidence.PercentageCoverage != 9 {
		t.Fatalf("percentageCoverage=%v", estimate.Evidence.PercentageCoverage)
	}
}

func TestQuotaWindowRobustDeltaMedians(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 12, 14),
		sample(3, 15, 20),
		sample(4, 18, 26),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Method != "delta_squared_weighted_median_mad" {
		t.Fatalf("method=%q", estimate.Method)
	}
	if estimate.Cost.Center != 200 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	if estimate.Evidence.PercentageCoverage != 8 {
		t.Fatalf("percentageCoverage=%v", estimate.Evidence.PercentageCoverage)
	}
}

func TestQuotaWindowRemovesHighWeightOutlierWithUnweightedMAD(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 11, 10.9),
		sample(3, 12, 11.9),
		sample(4, 13, 13),
		sample(5, 18, 63),
	}, 300, quotaWindowStaleAfterSeconds5h)
	assertClose(t, estimate.Cost.Center, 100, 1e-9)
	if estimate.Evidence.RawDeltaCandidateCount != 4 {
		t.Fatalf("rawDeltaCandidateCount=%v", estimate.Evidence.RawDeltaCandidateCount)
	}
	if estimate.Evidence.DeltaCandidateCount != 3 {
		t.Fatalf("deltaCandidateCount=%v", estimate.Evidence.DeltaCandidateCount)
	}
	if estimate.Evidence.OutlierCandidateCount != 1 {
		t.Fatalf("outlierCandidateCount=%v", estimate.Evidence.OutlierCandidateCount)
	}
	if estimate.Evidence.RawPercentageCoverage != 8 {
		t.Fatalf("rawPercentageCoverage=%v", estimate.Evidence.RawPercentageCoverage)
	}
	if estimate.Evidence.PercentageCoverage != 3 {
		t.Fatalf("percentageCoverage=%v", estimate.Evidence.PercentageCoverage)
	}
}

func TestQuotaWindowFivePercentToleranceWhenCandidateMADIsZero(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 11, 11),
		sample(3, 12, 12),
		sample(4, 13, 13.04),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if !estimate.Evidence.CandidateMadSet || estimate.Evidence.CandidateMadUSD != 0 {
		t.Fatalf("candidateMadUsd=%v set=%v", estimate.Evidence.CandidateMadUSD, estimate.Evidence.CandidateMadSet)
	}
	if !estimate.Evidence.MadThresholdSet || estimate.Evidence.MadThresholdUSD != 5 {
		t.Fatalf("madThresholdUsd=%v set=%v", estimate.Evidence.MadThresholdUSD, estimate.Evidence.MadThresholdSet)
	}
	if estimate.Evidence.OutlierCandidateCount != 0 {
		t.Fatalf("outlierCandidateCount=%v", estimate.Evidence.OutlierCandidateCount)
	}
}

func TestQuotaWindowNoMADFilteringBeforeThreeCandidates(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 11, 11),
		sample(3, 16, 61),
	}, 180, quotaWindowStaleAfterSeconds5h)
	if estimate.Evidence.CandidateMadSet {
		t.Fatalf("candidateMadUsd must be null before three candidates")
	}
	if estimate.Evidence.OutlierCandidateCount != 0 {
		t.Fatalf("outlierCandidateCount=%v", estimate.Evidence.OutlierCandidateCount)
	}
	if estimate.Cost.Center != 1000 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
}

func TestQuotaWindowAgreeingHuberCrossCheckKeepsMainEstimate(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 5),
		sample(2, 12, 9),
		sample(3, 14, 13),
		sample(4, 16, 17),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Cost.Center != 200 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	assertClose(t, estimate.Evidence.HuberEstimateUSD, 200, 1e-9)
	if estimate.Evidence.HuberDisagrees {
		t.Fatalf("huberDisagrees must be false")
	}
	if estimate.Confidence != "medium" {
		t.Fatalf("confidence=%q", estimate.Confidence)
	}
}

func TestQuotaWindowLowersConfidenceWhenHuberDiffersOverFifteenPercent(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 12, 12),
		sample(3, 14, 14),
		sample(4, 16, 16),
		sample(5, 17, 17.6),
		sample(6, 18, 19.2),
		sample(7, 19, 20.8),
	}, 420, quotaWindowStaleAfterSeconds5h)
	if estimate.Cost.Center != 100 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
	if !(estimate.Evidence.HuberRelativeDiff > 0.15) {
		t.Fatalf("huberRelativeDifference=%v must exceed 0.15", estimate.Evidence.HuberRelativeDiff)
	}
	if !estimate.Evidence.HuberDisagrees {
		t.Fatalf("huberDisagrees must be true")
	}
	if estimate.Confidence != "low" {
		t.Fatalf("confidence=%q", estimate.Confidence)
	}
}

func TestQuotaWindowDoesNotDuplicateFlatPercentagePollsInHuber(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 10, 10.5),
		sample(3, 12, 12),
		sample(4, 14, 14),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Evidence.HuberPointCount != 3 {
		t.Fatalf("huberPointCount=%v", estimate.Evidence.HuberPointCount)
	}
	assertClose(t, estimate.Evidence.HuberEstimateUSD, 100, 1e-9)
}

func TestQuotaWindowIgnoresSmallPercentageRegressionWithoutResettingAnchor(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 12, 12),
		sample(3, 11, 12.2),
		sample(4, 13, 13),
	}, 240, quotaWindowStaleAfterSeconds5h)
	if estimate.Evidence.PercentRegressionCount != 1 {
		t.Fatalf("percentRegressionCount=%v", estimate.Evidence.PercentRegressionCount)
	}
	if estimate.Evidence.DeltaCandidateCount != 2 {
		t.Fatalf("deltaCandidateCount=%v", estimate.Evidence.DeltaCandidateCount)
	}
	if estimate.Cost.Center != 100 {
		t.Fatalf("center=%v", estimate.Cost.Center)
	}
}

func TestQuotaWindowHuberRejectsNonPositiveSlope(t *testing.T) {
	_, ok := quotaWindowHuberRegression([]quotaWindowPoint{
		{x: 1, y: 3},
		{x: 2, y: 2},
		{x: 3, y: 1},
	}, quotaWindowHuberOptions{})
	if ok {
		t.Fatalf("huberRegression must return null for a non-positive slope")
	}
}

func TestQuotaWindowPenalizesPercentageRiseWithoutLocalUsage(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 12, 10),
		sample(3, 14, 14),
	}, 180, quotaWindowStaleAfterSeconds5h)
	if estimate.Evidence.ExternalIntervals != 1 {
		t.Fatalf("externalIntervals=%v", estimate.Evidence.ExternalIntervals)
	}
	if estimate.Confidence != "low" {
		t.Fatalf("confidence=%q", estimate.Confidence)
	}
}

func TestQuotaWindowWeightedMedianFavorsGreaterPercentageCoverage(t *testing.T) {
	median, ok := quotaWindowWeightedMedian([]quotaWeightedEntry{
		{value: 10, weight: 1},
		{value: 20, weight: 3},
	})
	if !ok || median != 20 {
		t.Fatalf("median=%v ok=%v", median, ok)
	}
}

func TestQuotaWindowNoDollarEstimateWithoutLocalCost(t *testing.T) {
	estimate := estimateQuotaWindowSamples([]quotaWindowSample{
		sample(1, 10, 0),
		sample(2, 12, 0),
	}, 120, quotaWindowStaleAfterSeconds5h)
	if estimate.State != "waiting" {
		t.Fatalf("state=%q", estimate.State)
	}
	if estimate.Reason != "no_local_cost" {
		t.Fatalf("reason=%q", estimate.Reason)
	}
}

func TestQuotaWindowSelectsHighestQualityEstimateFromCompletedCycle(t *testing.T) {
	samples := []quotaWindowSample{
		sample(1, 10, 10),
		sample(2, 12, 14),
		sample(3, 15, 20),
		sample(4, 18, 26),
		sample(5, 20, 26),
	}
	best := quotaWindowSelectBest(samples, 300, quotaWindowStaleAfterSeconds5h)
	if best.State != "estimated" {
		t.Fatalf("state=%q", best.State)
	}
	if best.Cost.Center != 200 {
		t.Fatalf("center=%v", best.Cost.Center)
	}
	if best.QualityScore <= 0 {
		t.Fatalf("qualityScore=%v", best.QualityScore)
	}
}
