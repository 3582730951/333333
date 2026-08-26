package api

import (
	"math"
	"sort"
)

// Port of the calculate_money quota estimator (algorithm "total_cost_robust_v2",
// github.com/Liao-zipeng/calculate_money). The pool's relay records real token
// usage per account, and the quota poller snapshots upstream used_percent — so the
// dollar value of a window cycle is inferred empirically: how much recorded cost a
// measured percentage-point drop corresponds to. No plan list price is ever
// consulted; the estimate is a ratio of observed cost to observed percentage
// change, cross-checked by a robust Huber regression, with a confidence grade that
// shrinks when the two disagree, when usage happened outside local accounting
// (external intervals), or when the sample stream went stale.
//
// The one deliberate divergence from the JS original: sample timestamps are unix
// seconds, not milliseconds, and staleAfter is expressed in seconds. All numeric
// constants (weights, MAD thresholds, Huber tuning, confidence thresholds, quality
// scoring) are ported verbatim.

const (
	quotaWindowAlgorithmVersion    = "total_cost_robust_v2"
	quotaWindowMinDeltaPercent     = 1.0
	quotaWindowMADMinCandidates    = 3
	quotaWindowMADMultiplier       = 3.0
	quotaWindowMADZeroTolerance    = 0.05
	quotaWindowHuberTuning         = 1.345
	quotaWindowHuberMaxIterations  = 50
	quotaWindowHuberDisagreement   = 0.15
	quotaWindowStaleAfterSeconds5h = 30 * 60
	quotaWindowStaleAfterSeconds7d = 6 * 60 * 60
)

type quotaWindowSample struct {
	AccountID   string
	WindowKind  string
	CycleStart  int64
	SampleAt    int64
	UsedPercent float64
	CostUSD     float64
}

// quotaWindowCostRange is an estimate interval; the JS original calls it a cost.
type quotaWindowCostRange struct {
	Center float64 `json:"center"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
}

type quotaWindowEstimateEvidence struct {
	SampleCount            int     `json:"sample_count"`
	CandidateCount         int     `json:"candidate_count"`
	RawCandidateCount      int     `json:"raw_candidate_count"`
	DeltaCandidateCount    int     `json:"delta_candidate_count"`
	RawDeltaCandidateCount int     `json:"raw_delta_candidate_count"`
	OutlierCandidateCount  int     `json:"outlier_candidate_count"`
	PercentageCoverage     float64 `json:"percentage_coverage"`
	RawPercentageCoverage  float64 `json:"raw_percentage_coverage"`
	ExternalIntervals      int     `json:"external_intervals"`
	ExternalCoverage       float64 `json:"external_coverage"`
	RelativeSpread         float64 `json:"relative_spread"`
	CandidateMedianUSD     float64 `json:"candidate_median_usd,omitempty"`
	CandidateMedianSet     bool    `json:"-"`
	CandidateMadUSD        float64 `json:"candidate_mad_usd,omitempty"`
	CandidateMadSet        bool    `json:"-"`
	RelativeMad            float64 `json:"relative_mad,omitempty"`
	RelativeMadSet         bool    `json:"-"`
	MadThresholdUSD        float64 `json:"mad_threshold_usd,omitempty"`
	MadThresholdSet        bool    `json:"-"`
	HuberEstimateUSD       float64 `json:"huber_estimate_usd,omitempty"`
	HuberEstimateSet       bool    `json:"-"`
	HuberRelativeDiff      float64 `json:"huber_relative_difference,omitempty"`
	HuberRelativeSet       bool    `json:"-"`
	HuberDisagrees         bool    `json:"huber_disagrees"`
	HuberPointCount        int     `json:"huber_point_count"`
	HuberConverged         bool    `json:"huber_converged"`
	PercentRegressionCount int     `json:"percent_regression_count"`
	ColdSampleAt           int64   `json:"cold_sample_at,omitempty"`
}

type quotaWindowEstimate struct {
	State           string                      `json:"state"`
	Confidence      string                      `json:"confidence"`
	Reason          string                      `json:"reason,omitempty"`
	Method          string                      `json:"method,omitempty"`
	AlgorithmVersion string                     `json:"algorithm_version"`
	CostBasis       string                      `json:"cost_basis"`
	QualityScore    float64                     `json:"quality_score"`
	Stale           bool                        `json:"stale"`
	SampleAt        int64                       `json:"sample_at,omitempty"`
	UsedPercent     float64                     `json:"used_percent,omitempty"`
	ObservedCost    float64                     `json:"observed_cost,omitempty"`
	Cost            quotaWindowCostRange        `json:"cost"`
	USDPerPercent   quotaWindowCostRange        `json:"usd_per_percent"`
	UsedCost        quotaWindowCostRange        `json:"used_cost"`
	RemainingCost   quotaWindowCostRange        `json:"remaining_cost"`
	Evidence        quotaWindowEstimateEvidence `json:"evidence"`
}

func quotaWindowIsFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func quotaWindowFinite(value float64) float64 {
	if quotaWindowIsFinite(value) {
		return value
	}
	return 0
}

func quotaWindowMedian(values []float64) (float64, bool) {
	filtered := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			filtered = append(filtered, v)
		}
	}
	sort.Float64s(filtered)
	if len(filtered) == 0 {
		return 0, false
	}
	middle := len(filtered) / 2
	if len(filtered)%2 == 1 {
		return filtered[middle], true
	}
	return (filtered[middle-1] + filtered[middle]) / 2, true
}

type quotaWeightedEntry struct {
	value  float64
	weight float64
}

func quotaWeightedQuantile(entries []quotaWeightedEntry, quantile float64) (float64, bool) {
	sorted := make([]quotaWeightedEntry, 0, len(entries))
	totalWeight := 0.0
	for _, entry := range entries {
		if quotaWindowIsFinite(entry.value) && quotaWindowIsFinite(entry.weight) && entry.weight > 0 {
			sorted = append(sorted, entry)
			totalWeight += entry.weight
		}
	}
	if len(sorted) == 0 || totalWeight <= 0 {
		return 0, false
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].value < sorted[j].value })
	target := totalWeight * quantile
	cumulative := 0.0
	for _, item := range sorted {
		cumulative += item.weight
		if cumulative >= target {
			return item.value, true
		}
	}
	return sorted[len(sorted)-1].value, true
}

func quotaWindowWeightedMedian(entries []quotaWeightedEntry) (float64, bool) {
	return quotaWeightedQuantile(entries, 0.5)
}

func quotaWindowCostRangeFor(entries []quotaWeightedEntry, center float64) quotaWindowCostRange {
	if len(entries) == 0 || center <= 0 {
		return quotaWindowCostRange{}
	}
	if len(entries) == 1 {
		return quotaWindowCostRange{
			Center: center,
			Lower:  math.Max(0, center*0.7),
			Upper:  center * 1.3,
		}
	}
	if len(entries) == 2 {
		values := []float64{entries[0].value, entries[1].value}
		sort.Float64s(values)
		return quotaWindowCostRange{
			Center: center,
			Lower:  math.Max(0, values[0]*0.9),
			Upper:  values[1] * 1.1,
		}
	}
	lower, lowerOK := quotaWeightedQuantile(entries, 0.1)
	upper, upperOK := quotaWeightedQuantile(entries, 0.9)
	minimumHalfWidth := math.Abs(center) * 0.05
	if !lowerOK || !upperOK {
		return quotaWindowCostRange{Center: center}
	}
	return quotaWindowCostRange{
		Center: center,
		Lower:  math.Max(0, math.Min(lower, center-minimumHalfWidth)),
		Upper:  math.Max(upper, center+minimumHalfWidth),
	}
}

func quotaWindowMakeCost(candidates []quotaWindowCandidate) quotaWindowCostRange {
	entries := make([]quotaWeightedEntry, 0, len(candidates))
	for _, candidate := range candidates {
		if quotaWindowIsFinite(candidate.cost) && candidate.cost > 0 && quotaWindowIsFinite(candidate.weight) && candidate.weight > 0 {
			entries = append(entries, quotaWeightedEntry{value: candidate.cost, weight: candidate.weight})
		}
	}
	center, ok := quotaWindowWeightedMedian(entries)
	if !ok {
		return quotaWindowCostRange{}
	}
	return quotaWindowCostRangeFor(entries, center)
}

type quotaWindowCandidate struct {
	cost     float64
	weight   float64
	coverage float64
}

type quotaWindowCandidateFilter struct {
	inliers  []quotaWindowCandidate
	outliers []quotaWindowCandidate
	median   float64
	medianOK bool
	mad      float64
	madOK    bool
	threshold float64
	thresholdOK bool
}

func quotaWindowFilterCandidateOutliers(candidates []quotaWindowCandidate) quotaWindowCandidateFilter {
	valid := make([]quotaWindowCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if quotaWindowIsFinite(candidate.cost) && candidate.cost > 0 && quotaWindowIsFinite(candidate.weight) && candidate.weight > 0 {
			valid = append(valid, candidate)
		}
	}
	if len(valid) < quotaWindowMADMinCandidates {
		median, medianOK := quotaWindowMedian(quotaWindowCandidateCosts(valid))
		return quotaWindowCandidateFilter{
			inliers:  valid,
			median:   median,
			medianOK: medianOK,
		}
	}
	rawMedian, medianOK := quotaWindowMedian(quotaWindowCandidateCosts(valid))
	if !medianOK {
		return quotaWindowCandidateFilter{inliers: valid}
	}
	absDeviations := make([]float64, 0, len(valid))
	for _, candidate := range valid {
		absDeviations = append(absDeviations, math.Abs(candidate.cost-rawMedian))
	}
	mad, madOK := quotaWindowMedian(absDeviations)
	threshold := 0.0
	if madOK && mad > 0 {
		threshold = quotaWindowMADMultiplier * mad
	} else {
		threshold = math.Max(math.Abs(rawMedian)*quotaWindowMADZeroTolerance, 1e-9)
	}
	var inliers, outliers []quotaWindowCandidate
	for _, candidate := range valid {
		if math.Abs(candidate.cost-rawMedian) > threshold {
			outliers = append(outliers, candidate)
		} else {
			inliers = append(inliers, candidate)
		}
	}
	return quotaWindowCandidateFilter{
		inliers:     inliers,
		outliers:    outliers,
		median:      rawMedian,
		medianOK:    true,
		mad:         mad,
		madOK:       madOK,
		threshold:   threshold,
		thresholdOK: true,
	}
}

func quotaWindowCandidateCosts(candidates []quotaWindowCandidate) []float64 {
	out := make([]float64, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.cost)
	}
	return out
}

func quotaWindowScaleCost(cost quotaWindowCostRange, factor float64) quotaWindowCostRange {
	safeFactor := math.Max(0, quotaWindowFinite(factor))
	return quotaWindowCostRange{
		Center: cost.Center * safeFactor,
		Lower:  cost.Lower * safeFactor,
		Upper:  cost.Upper * safeFactor,
	}
}

func quotaWindowRoughCandidate(sample quotaWindowSample) (quotaWindowCandidate, bool) {
	cost := quotaWindowFinite(sample.CostUSD)
	if sample.UsedPercent <= 0 || cost <= 0 {
		return quotaWindowCandidate{}, false
	}
	multiplier := 100 / sample.UsedPercent
	return quotaWindowCandidate{
		cost:     cost * multiplier,
		weight:   math.Max(sample.UsedPercent, 0.1),
		coverage: sample.UsedPercent,
	}, true
}

type quotaWindowDeltaResult struct {
	valid      bool
	external   bool
	coverage   float64
	candidate  quotaWindowCandidate
}

func quotaWindowDeltaCandidate(previous, current quotaWindowSample) quotaWindowDeltaResult {
	deltaPercent := current.UsedPercent - previous.UsedPercent
	deltaCost := quotaWindowFinite(current.CostUSD) - quotaWindowFinite(previous.CostUSD)

	if deltaPercent <= 0 {
		return quotaWindowDeltaResult{}
	}
	if deltaCost == 0 {
		return quotaWindowDeltaResult{external: true, coverage: deltaPercent}
	}
	if deltaPercent < quotaWindowMinDeltaPercent || deltaCost < 0 {
		return quotaWindowDeltaResult{}
	}
	return quotaWindowDeltaResult{
		valid: true,
		candidate: quotaWindowCandidate{
			cost:     deltaCost * (100 / deltaPercent),
			weight:   deltaPercent * deltaPercent,
			coverage: deltaPercent,
		},
	}
}

type quotaWindowFit struct {
	intercept float64
	beta      float64
}

func quotaWindowWeightedLinearFit(points []quotaWindowPoint, weights []float64) (quotaWindowFit, bool) {
	sumWeight, sumX, sumY, sumXX, sumXY := 0.0, 0.0, 0.0, 0.0, 0.0
	for index, point := range points {
		weight := weights[index]
		sumWeight += weight
		sumX += weight * point.x
		sumY += weight * point.y
		sumXX += weight * point.x * point.x
		sumXY += weight * point.x * point.y
	}
	denominator := sumWeight*sumXX - sumX*sumX
	if !quotaWindowIsFinite(denominator) || math.Abs(denominator) <= 1e-12 {
		return quotaWindowFit{}, false
	}
	beta := (sumWeight*sumXY - sumX*sumY) / denominator
	intercept := (sumY - beta*sumX) / sumWeight
	if !quotaWindowIsFinite(beta) || !quotaWindowIsFinite(intercept) {
		return quotaWindowFit{}, false
	}
	return quotaWindowFit{intercept: intercept, beta: beta}, true
}

func quotaWindowTheilSenSeed(points []quotaWindowPoint) (quotaWindowFit, bool) {
	var slopes []float64
	for left := 0; left < len(points)-1; left++ {
		for right := left + 1; right < len(points); right++ {
			deltaX := points[right].x - points[left].x
			if deltaX > 0 {
				slopes = append(slopes, (points[right].y-points[left].y)/deltaX)
			}
		}
	}
	beta, betaOK := quotaWindowMedian(slopes)
	if !betaOK || !quotaWindowIsFinite(beta) {
		return quotaWindowFit{}, false
	}
	intercepts := make([]float64, 0, len(points))
	for _, point := range points {
		intercepts = append(intercepts, point.y-beta*point.x)
	}
	intercept, ok := quotaWindowMedian(intercepts)
	if !ok || !quotaWindowIsFinite(intercept) {
		return quotaWindowFit{}, false
	}
	return quotaWindowFit{intercept: intercept, beta: beta}, true
}

type quotaWindowPoint struct {
	x float64
	y float64
}

type quotaWindowHuberResult struct {
	fit        quotaWindowFit
	pointCount int
	iterations int
	converged  bool
}

func quotaWindowHuberRegression(rawPoints []quotaWindowPoint, options quotaWindowHuberOptions) (quotaWindowHuberResult, bool) {
	tuning := options.tuning
	if tuning <= 0 {
		tuning = quotaWindowHuberTuning
	}
	maxIterations := options.maxIterations
	if maxIterations <= 0 {
		maxIterations = quotaWindowHuberMaxIterations
	}
	points := make([]quotaWindowPoint, 0, len(rawPoints))
	lastX := math.Inf(-1)
	for _, point := range rawPoints {
		if !quotaWindowIsFinite(point.x) || !quotaWindowIsFinite(point.y) || point.x <= lastX {
			continue
		}
		points = append(points, point)
		lastX = point.x
	}
	if len(points) < 3 || points[len(points)-1].x-points[0].x < 2 {
		return quotaWindowHuberResult{}, false
	}

	fit, ok := quotaWindowTheilSenSeed(points)
	if !ok {
		return quotaWindowHuberResult{}, false
	}
	converged := false
	iterations := 0

	for iteration := 0; iteration < maxIterations; iteration++ {
		iterations = iteration + 1
		residuals := make([]float64, 0, len(points))
		for _, point := range points {
			residuals = append(residuals, point.y-(fit.intercept+fit.beta*point.x))
		}
		residualCenter, _ := quotaWindowMedian(residuals)
		absResiduals := make([]float64, 0, len(residuals))
		for _, residual := range residuals {
			absResiduals = append(absResiduals, math.Abs(residual-residualCenter))
		}
		residualMad, _ := quotaWindowMedian(absResiduals)
		yValues := make([]float64, 0, len(points))
		for _, point := range points {
			yValues = append(yValues, point.y)
		}
		yMin, yMax := yValues[0], yValues[0]
		for _, value := range yValues {
			if value < yMin {
				yMin = value
			}
			if value > yMax {
				yMax = value
			}
		}
		yRange := yMax - yMin
		scale := math.Max(1.4826*residualMad, math.Max(1, yRange)*1e-9)
		limit := tuning * scale
		weights := make([]float64, 0, len(residuals))
		for _, residual := range residuals {
			magnitude := math.Abs(residual)
			if magnitude <= limit {
				weights = append(weights, 1)
			} else {
				weights = append(weights, limit/magnitude)
			}
		}
		next, nextOK := quotaWindowWeightedLinearFit(points, weights)
		if !nextOK {
			return quotaWindowHuberResult{}, false
		}
		betaTolerance := 1e-9 * math.Max(1, math.Abs(fit.beta))
		interceptTolerance := 1e-9 * math.Max(1, math.Abs(fit.intercept))
		betaStable := math.Abs(next.beta-fit.beta) <= betaTolerance
		interceptStable := math.Abs(next.intercept-fit.intercept) <= interceptTolerance
		fit = next
		if betaStable && interceptStable {
			converged = true
			break
		}
	}

	if !quotaWindowIsFinite(fit.beta) || fit.beta <= 0 {
		return quotaWindowHuberResult{}, false
	}
	return quotaWindowHuberResult{
		fit:        fit,
		pointCount: len(points),
		iterations: iterations,
		converged:  converged,
	}, true
}

type quotaWindowHuberOptions struct {
	tuning       float64
	maxIterations int
}

func quotaWindowLowerConfidence(confidence string) string {
	switch confidence {
	case "high":
		return "medium"
	case "medium":
		return "low"
	}
	return confidence
}

func quotaWindowRelativeSpread(cost quotaWindowCostRange) float64 {
	if cost.Center <= 0 {
		return 1
	}
	return (cost.Upper - cost.Lower) / (2 * cost.Center)
}

// quotaWindowEstimate evaluates one window cycle's samples. It mirrors the JS
// estimateWindow state machine: "waiting" with a reason when the evidence is too
// thin, "estimated" otherwise.
func estimateQuotaWindowSamples(samples []quotaWindowSample, now, staleAfter int64) quotaWindowEstimate {
	ordered := make([]quotaWindowSample, 0, len(samples))
	for _, sample := range samples {
		if !quotaWindowIsFinite(sample.UsedPercent) {
			continue
		}
		ordered = append(ordered, sample)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SampleAt < ordered[j].SampleAt })

	if len(ordered) == 0 {
		return quotaWindowEstimate{State: "waiting", Confidence: "none", Reason: "no_samples", AlgorithmVersion: quotaWindowAlgorithmVersion, CostBasis: "total_cost"}
	}

	latest := ordered[len(ordered)-1]
	var coldSample quotaWindowSample
	coldFound := false
	for _, sample := range ordered {
		if sample.UsedPercent > 0 && quotaWindowFinite(sample.CostUSD) > 0 {
			coldSample = sample
			coldFound = true
			break
		}
	}
	var rough quotaWindowCandidate
	var roughOK bool
	if coldFound {
		rough, roughOK = quotaWindowRoughCandidate(coldSample)
	}
	var deltaCandidates []quotaWindowCandidate
	regressionPoints := []quotaWindowPoint{{x: ordered[0].UsedPercent, y: quotaWindowFinite(ordered[0].CostUSD)}}
	externalIntervals := 0
	externalCoverage := 0.0
	percentRegressionCount := 0

	anchor := ordered[0]
	for index := 1; index < len(ordered); index++ {
		current := ordered[index]
		if current.UsedPercent < anchor.UsedPercent {
			percentRegressionCount++
			continue
		}
		if current.UsedPercent-anchor.UsedPercent < quotaWindowMinDeltaPercent {
			continue
		}
		result := quotaWindowDeltaCandidate(anchor, current)
		if result.external {
			externalIntervals++
			externalCoverage += result.coverage
		}
		if result.valid {
			deltaCandidates = append(deltaCandidates, result.candidate)
		}
		regressionPoints = append(regressionPoints, quotaWindowPoint{x: current.UsedPercent, y: quotaWindowFinite(current.CostUSD)})
		anchor = current
	}

	filtered := quotaWindowFilterCandidateOutliers(deltaCandidates)
	formalCandidates := filtered.inliers
	var candidates []quotaWindowCandidate
	if len(deltaCandidates) > 0 {
		candidates = append(candidates, formalCandidates...)
	} else if roughOK {
		candidates = append(candidates, rough)
	}
	if len(candidates) == 0 {
		reason := "insufficient_change"
		if latest.UsedPercent <= 0 {
			reason = "zero_percent"
		} else if quotaWindowFinite(latest.CostUSD) <= 0 {
			reason = "no_local_cost"
		}
		return quotaWindowEstimate{
			State:           "waiting",
			Confidence:      "none",
			Reason:          reason,
			AlgorithmVersion: quotaWindowAlgorithmVersion,
			CostBasis:       "total_cost",
			UsedPercent:     latest.UsedPercent,
			SampleAt:        latest.SampleAt,
			ObservedCost:    quotaWindowFinite(latest.CostUSD),
			Evidence: quotaWindowEstimateEvidence{
				SampleCount:   len(ordered),
				ExternalIntervals: externalIntervals,
				ExternalCoverage:  externalCoverage,
			},
		}
	}

	cost := quotaWindowMakeCost(candidates)
	if cost.Center <= 0 {
		return quotaWindowEstimate{
			State:           "waiting",
			Confidence:      "none",
			Reason:          "no_local_cost",
			AlgorithmVersion: quotaWindowAlgorithmVersion,
			CostBasis:       "total_cost",
			UsedPercent:     latest.UsedPercent,
			SampleAt:        latest.SampleAt,
			ObservedCost:    quotaWindowFinite(latest.CostUSD),
			Evidence: quotaWindowEstimateEvidence{
				SampleCount:   len(ordered),
				ExternalIntervals: externalIntervals,
				ExternalCoverage:  externalCoverage,
			},
		}
	}

	coverage := 0.0
	if len(deltaCandidates) > 0 {
		for _, candidate := range formalCandidates {
			coverage += candidate.coverage
		}
	} else {
		coverage = rough.coverage
	}
	rawCoverage := 0.0
	for _, candidate := range deltaCandidates {
		rawCoverage += candidate.coverage
	}
	spread := quotaWindowRelativeSpread(cost)
	relativeMad := 0.0
	relativeMadOK := false
	if filtered.madOK && filtered.medianOK && math.Abs(filtered.median) > 0 {
		relativeMad = filtered.mad / math.Abs(filtered.median)
		relativeMadOK = true
	}
	dispersion := spread
	if relativeMadOK {
		dispersion = relativeMad
	}

	var regression quotaWindowHuberResult
	regressionOK := false
	if len(deltaCandidates) > 0 {
		regression, regressionOK = quotaWindowHuberRegression(regressionPoints, quotaWindowHuberOptions{})
	}
	huberEstimateUSD := 0.0
	huberEstimateSet := false
	if regressionOK {
		huberEstimateUSD = regression.fit.beta * 100
		huberEstimateSet = true
	}
	huberRelativeDifference := 0.0
	huberRelativeSet := false
	if regressionOK && cost.Center > 0 {
		huberRelativeDifference = math.Abs(huberEstimateUSD-cost.Center) / cost.Center
		huberRelativeSet = true
	}
	huberDisagrees := huberRelativeSet && huberRelativeDifference > quotaWindowHuberDisagreement

	confidence := "low"
	if len(formalCandidates) >= 4 && coverage >= 8 && dispersion <= 0.15 {
		confidence = "high"
	} else if len(formalCandidates) >= 2 && coverage >= 3 && dispersion <= 0.35 {
		confidence = "medium"
	}
	if externalIntervals > 0 {
		confidence = quotaWindowLowerConfidence(confidence)
	}
	if huberDisagrees {
		confidence = quotaWindowLowerConfidence(confidence)
	}
	stale := now-latest.SampleAt > staleAfter
	if stale {
		confidence = quotaWindowLowerConfidence(confidence)
	}

	confidenceRank := map[string]float64{"none": 0, "low": 1, "medium": 2, "high": 3}[confidence]
	formalBonus := 0.0
	if len(deltaCandidates) > 0 {
		formalBonus = 500
	}
	qualityCoverage := 0.0
	if len(deltaCandidates) > 0 {
		qualityCoverage = coverage
	}
	qualityScore := math.Max(0,
		confidenceRank*1000+
			formalBonus+
			math.Min(qualityCoverage, 100)*10+
			float64(len(candidates))*5-
			math.Min(spread, 2)*100-
			float64(externalIntervals)*100-
			float64(len(filtered.outliers))*25)
	if huberDisagrees {
		qualityScore -= 150
	}
	qualityScore = math.Max(0, qualityScore)

	usedFactor := math.Max(0, math.Min(1, latest.UsedPercent/100))

	method := "first_sample_rough"
	if len(deltaCandidates) > 0 {
		method = "delta_squared_weighted_median_mad"
	}

	return quotaWindowEstimate{
		State:            "estimated",
		Confidence:       confidence,
		Method:           method,
		AlgorithmVersion: quotaWindowAlgorithmVersion,
		CostBasis:        "total_cost",
		QualityScore:     qualityScore,
		Stale:            stale,
		SampleAt:         latest.SampleAt,
		UsedPercent:      latest.UsedPercent,
		ObservedCost:     quotaWindowFinite(latest.CostUSD),
		Cost:             cost,
		USDPerPercent:    quotaWindowScaleCost(cost, 0.01),
		UsedCost:         quotaWindowScaleCost(cost, usedFactor),
		RemainingCost:    quotaWindowScaleCost(cost, 1-usedFactor),
		Evidence: quotaWindowEstimateEvidence{
			SampleCount:            len(ordered),
			CandidateCount:         len(candidates),
			RawCandidateCount:      len(deltaCandidates),
			DeltaCandidateCount:    len(formalCandidates),
			RawDeltaCandidateCount: len(deltaCandidates),
			OutlierCandidateCount:  len(filtered.outliers),
			PercentageCoverage:     coverage,
			RawPercentageCoverage:  rawCoverage,
			ExternalIntervals:      externalIntervals,
			ExternalCoverage:       externalCoverage,
			RelativeSpread:         spread,
			CandidateMedianUSD:     filtered.median,
			CandidateMedianSet:     filtered.medianOK,
			CandidateMadUSD:        filtered.mad,
			CandidateMadSet:        filtered.madOK,
			RelativeMad:            relativeMad,
			RelativeMadSet:         relativeMadOK,
			MadThresholdUSD:        filtered.threshold,
			MadThresholdSet:        filtered.thresholdOK,
			HuberEstimateUSD:       huberEstimateUSD,
			HuberEstimateSet:       huberEstimateSet,
			HuberRelativeDiff:      huberRelativeDifference,
			HuberRelativeSet:       huberRelativeSet,
			HuberDisagrees:         huberDisagrees,
			HuberPointCount:        regression.pointCount,
			HuberConverged:         regression.converged,
			PercentRegressionCount: percentRegressionCount,
			ColdSampleAt:           coldSample.SampleAt,
		},
	}
}

func quotaWindowShouldReplaceBest(existing, candidate quotaWindowEstimate) bool {
	if candidate.State != "estimated" {
		return false
	}
	if existing.State != "estimated" {
		return true
	}
	return candidate.QualityScore > existing.QualityScore
}

// quotaWindowSelectBest evaluates every prefix of the sample stream and keeps the
// best-quality estimate. Reported percentages are quantized, so a prefix ending
// after the last clean percentage step is often more trustworthy than the full
// stream (which may have crossed into an anomalous or external segment).
func quotaWindowSelectBest(samples []quotaWindowSample, now, staleAfter int64) quotaWindowEstimate {
	var best quotaWindowEstimate
	bestSet := false
	for length := 1; length <= len(samples); length++ {
		candidate := estimateQuotaWindowSamples(samples[:length], now, staleAfter)
		if quotaWindowShouldReplaceBest(best, candidate) {
			best = candidate
			bestSet = true
		}
	}
	if bestSet {
		return best
	}
	return estimateQuotaWindowSamples(samples, now, staleAfter)
}
