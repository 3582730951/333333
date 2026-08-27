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
	price  quotaModelPrice
}

// Longest prefix wins, so -mini/-nano/-codex/-sol/-terra/-luna variants resolve to
// their family before the generic prefix is tried.
var quotaModelPriceTable = []quotaModelPriceEntry{
	// OpenAI / ChatGPT family (public API list prices, USD per 1M tokens).
	// The gpt-5 entry covers the Codex plan variants (gpt-5-codex, gpt-5.6-sol,
	// gpt-5.6-terra, gpt-5.6-luna, gpt-5.5, gpt-5.4, gpt-5.2) at the gpt-5 tier.
	{prefix: "gpt-4o-mini", price: quotaModelPrice{inputUSD: 0.15, outputUSD: 0.60, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1-nano", price: quotaModelPrice{inputUSD: 0.10, outputUSD: 0.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1-mini", price: quotaModelPrice{inputUSD: 0.40, outputUSD: 1.60, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4.1", price: quotaModelPrice{inputUSD: 2.00, outputUSD: 8.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4o", price: quotaModelPrice{inputUSD: 2.50, outputUSD: 10.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4-turbo", price: quotaModelPrice{inputUSD: 10.00, outputUSD: 30.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-4", price: quotaModelPrice{inputUSD: 30.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-3.5-turbo", price: quotaModelPrice{inputUSD: 0.50, outputUSD: 1.50, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "gpt-5", price: quotaModelPrice{inputUSD: 1.25, outputUSD: 10.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o3-mini", price: quotaModelPrice{inputUSD: 1.10, outputUSD: 4.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o3", price: quotaModelPrice{inputUSD: 2.00, outputUSD: 8.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o4-mini", price: quotaModelPrice{inputUSD: 1.10, outputUSD: 4.40, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1-mini", price: quotaModelPrice{inputUSD: 3.00, outputUSD: 12.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1-preview", price: quotaModelPrice{inputUSD: 15.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	{prefix: "o1", price: quotaModelPrice{inputUSD: 15.00, outputUSD: 60.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.0}},
	// Claude family (public API list prices). Cache creation costs 1.25x input.
	{prefix: "claude-haiku", price: quotaModelPrice{inputUSD: 1.00, outputUSD: 5.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
	// claude-fable-5 has no published list price; sonnet-tier placeholder keeps a
	// served flagship from reading as zero-cost. Confidence grading tolerates the
	// approximation (constant per-model bias widens spread, never fabricates).
	{prefix: "claude-fable-5", price: quotaModelPrice{inputUSD: 3.00, outputUSD: 15.00, cacheReadFactor: 0.1, cacheCreationFactor: 1.25}},
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
	rows, err := s.store.AccountUsageCostRows(ctx, accountID, windowStart, windowEnd)
	if err != nil {
		return 0, 0
	}
	total, unsettled := 0.0, 0.0
	for _, row := range rows {
		cost := quotaUsageCostUSD(row.Model, row.PromptTokens, row.CompletionTokens, row.CacheReadTokens, row.CacheCreationTokens)
		total += cost
		if row.Estimated != 0 {
			unsettled += cost
		}
	}
	if total <= 0 {
		return total, 0
	}
	return total, unsettled / total
}
