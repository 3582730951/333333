package api

import (
	"context"
	"math"
	"sort"
	"strings"
)

// USD cost basis for the window estimation scheme. The relay records token usage
// per request; pricing those tokens at the standard public API list prices is the
// pool's equivalent of sub2api's usage_logs.total_cost — the "local cost" side of
// the cost/percent ratio. Prices are a constant estimation basis, never a bill:
// the estimator measures the ratio of this cost to upstream used_percent changes,
// and any systematic price misscaling shows up as widened spread or a Huber
// disagreement and is surfaced through the confidence grade, exactly as the
// calculate_money scheme intends.

type quotaModelPrice struct {
	inputUSD            float64 // per 1M tokens
	outputUSD           float64 // per 1M tokens
	cacheReadFactor     float64 // multiplier of input price for cached reads
	cacheCreationFactor float64 // multiplier of input price for cache creation
}

type quotaModelPriceEntry struct {
	prefix string
	exact  bool
	price  quotaModelPrice
}

// Longest prefix wins, so -mini/-nano/-codex/-sol/-terra/-luna variants resolve to
// their family before the generic prefix is tried.
var quotaModelPriceTable = []quotaModelPriceEntry{
	// OpenAI / ChatGPT family (public API list prices, USD per 1M tokens).
	// Legacy rows do not retain a service-tier/catalog id, so only exact audited
	// model families are eligible here.  In particular, never let a broad
	// "gpt-5" prefix silently price gpt-5.6-sol/terra/luna at an unrelated rate.
	{prefix: "gpt-4o-mini", price: quotaModelPrice{inputUSD: 0.15, outputUSD: 0.60, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1-nano", price: quotaModelPrice{inputUSD: 0.10, outputUSD: 0.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1-mini", price: quotaModelPrice{inputUSD: 0.40, outputUSD: 1.60, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1", price: quotaModelPrice{inputUSD: 2.00, outputUSD: 8.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4o", price: quotaModelPrice{inputUSD: 2.50, outputUSD: 10.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4-turbo", price: quotaModelPrice{inputUSD: 10.00, outputUSD: 30.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4", price: quotaModelPrice{inputUSD: 30.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-3.5-turbo", price: quotaModelPrice{inputUSD: 0.50, outputUSD: 1.50, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-5.6-sol", price: quotaModelPrice{inputUSD: 4.00, outputUSD: 20.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	{prefix: "gpt-5.6-terra", price: quotaModelPrice{inputUSD: 2.00, outputUSD: 12.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	{prefix: "gpt-5.6-luna", price: quotaModelPrice{inputUSD: 0.20, outputUSD: 1.20, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	{prefix: "gpt-5.5", price: quotaModelPrice{inputUSD: 5.00, outputUSD: 30.00, cacheReadFactor: 0.1, cacheCreationFactor: -1}},
	{prefix: "gpt-5.4", price: quotaModelPrice{inputUSD: 2.50, outputUSD: 15.00, cacheReadFactor: 0.1, cacheCreationFactor: -1}},
	// Keep the historical exact gpt-5 row for pre-catalog records.  `exact`
	// prevents it from becoming a catch-all for newer gpt-5.x variants.
	{prefix: "gpt-5", exact: true, price: quotaModelPrice{inputUSD: 1.25, outputUSD: 10.00, cacheReadFactor: 0.1, cacheCreationFactor: -1}},
	{prefix: "o3-mini", price: quotaModelPrice{inputUSD: 1.10, outputUSD: 4.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o3", price: quotaModelPrice{inputUSD: 2.00, outputUSD: 8.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o4-mini", price: quotaModelPrice{inputUSD: 1.10, outputUSD: 4.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1-mini", price: quotaModelPrice{inputUSD: 3.00, outputUSD: 12.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1-preview", price: quotaModelPrice{inputUSD: 15.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1", price: quotaModelPrice{inputUSD: 15.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	// Claude family (public API list prices). Cache creation costs 1.25x input.
	{prefix: "claude-haiku", price: quotaModelPrice{inputUSD: 1.00, outputUSD: 5.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	{prefix: "claude-sonnet", price: quotaModelPrice{inputUSD: 3.00, outputUSD: 15.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	{prefix: "claude-opus", price: quotaModelPrice{inputUSD: 5.00, outputUSD: 25.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
}

func init() {
	sort.SliceStable(quotaModelPriceTable, func(i, j int) bool {
		return len(quotaModelPriceTable[i].prefix) > len(quotaModelPriceTable[j].prefix)
	})
}

// quotaModelPriceFor resolves a resolved model name to its pricing basis. Unknown
// models yield (false); their tokens contribute zero to the cost basis and the
// estimator's external-interval logic flags the gap.
func quotaModelPriceFor(model string) (quotaModelPrice, bool) {
	model = strings.TrimSpace(strings.ToLower(model))
	for _, entry := range quotaModelPriceTable {
		if entry.exact && model != entry.prefix {
			continue
		}
		if strings.HasPrefix(model, entry.prefix) {
			return entry.price, true
		}
	}
	return quotaModelPrice{}, false
}

// quotaUsageCostUSD prices one usage record. cache_read and cache_creation are
// subsumed into prompt_tokens by upstream usage reporting, so the priced input is
// the non-cached remainder at full input price, cached reads at the discounted
// factor, and cache creation at the creation factor (1.25x for Claude).
func quotaUsageCostUSD(model string, promptTokens, completionTokens, cacheReadTokens, cacheCreationTokens int64) float64 {
	price, ok := quotaModelPriceFor(model)
	if !ok {
		return 0
	}
	if cacheCreationTokens > 0 && price.cacheCreationFactor < 0 {
		// The historical catalog did not publish a cache-write rate for this
		// exact model.  Do not substitute the ordinary input rate.
		return 0
	}
	input := math.Max(0, float64(promptTokens)-float64(cacheReadTokens)-float64(cacheCreationTokens))
	cost := input*price.inputUSD + float64(completionTokens)*price.outputUSD
	if cacheReadTokens > 0 {
		cost += float64(cacheReadTokens) * price.inputUSD * price.cacheReadFactor
	}
	if cacheCreationTokens > 0 {
		cost += float64(cacheCreationTokens) * price.inputUSD * price.cacheCreationFactor
	}
	return cost / 1e6
}

// quotaWindowCostRow is one usage record priced into the window basis.
type quotaWindowCostRow struct {
	Model               string
	PromptTokens        int64
	CompletionTokens    int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Estimated           int64
}

// sumQuotaWindowCost totals the relay's recorded USD spend for an account inside
// [windowStart, windowEnd] and reports what fraction of that total came from requests
// whose real upstream usage had not settled yet.
//
// Every row is one request and is counted once. `estimated` is a column UPDATED IN
// PLACE on the same usage_records row when the upstream's real usage settles, so a row
// is either estimated or settled and there is never an estimate/settled PAIR to
// de-duplicate.
//
// This previously skipped every estimated row as soon as the window held any settled
// row, on the theory that settled records were authoritative and the estimates were
// stand-ins. Because the flag is per-row and not a duplicate marker, that dropped the
// spend of every request still awaiting settlement: a window with one settled request
// and ninety-nine in flight reported roughly one percent of its actual cost, and the
// window estimator divided that understated cost by the upstream's used_percent — which
// is why the USD estimate read low.
//
// Counting the unsettled rows is the correct total, but it is a softer number than a
// fully settled window, so the share is returned rather than hidden: the estimator
// downgrades confidence on a window built mostly from estimates.
func (s *Server) sumQuotaWindowCost(ctx context.Context, accountID string, windowStart, windowEnd int64) (float64, float64) {
	if s == nil || s.store == nil || windowEnd <= windowStart {
		return 0, 0
	}
	fixed, fixedErr := s.store.AccountUsageValuationWindowSummary(ctx, accountID, windowStart, windowEnd)
	if fixedErr == nil && fixed.TotalEvents > 0 {
		total := float64(fixed.TotalMicroUSD) / 1_000_000
		unsettledShare := 0.0
		if fixed.TotalMicroUSD > 0 {
			unsettledShare = float64(fixed.ProvisionalMicroUSD) / float64(fixed.TotalMicroUSD)
		}
		// Missing rates/components have no amount that can be included in the
		// numerator. Their event share is therefore a lower-bound quality penalty,
		// never a fabricated zero-dollar contribution.
		unavailableShare := float64(fixed.UnavailableEvents) / float64(fixed.TotalEvents)
		if unavailableShare > unsettledShare {
			unsettledShare = unavailableShare
		}
		return total, unsettledShare
	}
	// Historical rows written before the fixed-point cutover have no component
	// record. Preserve the old estimator solely as a visibly lower-confidence
	// compatibility input; every new event takes the audited path above.
	rows, err := s.store.AccountUsageCostRows(ctx, accountID, windowStart, windowEnd)
	if err != nil {
		return 0, 0
	}
	total, unsettled := 0.0, 0.0
	unknownRows := 0
	for _, row := range rows {
		if price, known := quotaModelPriceFor(row.Model); !known || (row.CacheCreationTokens > 0 && price.cacheCreationFactor < 0) {
			unknownRows++
		}
		cost := quotaUsageCostUSD(row.Model, row.PromptTokens, row.CompletionTokens, row.CacheReadTokens, row.CacheCreationTokens)
		total += cost
		if row.Estimated != 0 {
			unsettled += cost
		}
	}
	if total <= 0 {
		// A zero amount for an unknown model is not evidence of free usage.
		// Returning a non-zero uncertainty share keeps the capacity estimator from
		// presenting the lower bound as a settled dollar balance.
		if unknownRows > 0 && len(rows) > 0 {
			return total, 1
		}
		return total, 0
	}
	if unknownRows > 0 {
		unknownShare := float64(unknownRows) / float64(len(rows))
		if unknownShare > unsettled/total {
			unsettled = total * unknownShare
		}
	}
	return total, unsettled / total
}
