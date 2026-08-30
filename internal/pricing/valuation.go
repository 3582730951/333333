package pricing

import "fmt"

type TokenUsage struct {
	InputTotal      int64 `json:"input_total"`
	InputUncached   int64 `json:"input_uncached"`
	CachedRead      int64 `json:"cached_read"`
	CacheWrite      int64 `json:"cache_write"`
	OutputTotal     int64 `json:"output_total"`
	OutputReasoning int64 `json:"output_reasoning"`
}

type Breakdown struct {
	InputUnits      int64 `json:"input_units"`
	CachedUnits     int64 `json:"cached_units"`
	CacheWriteUnits int64 `json:"cache_write_units"`
	OutputUnits     int64 `json:"output_units"`
	PerRequestUnits int64 `json:"per_request_units"`
	BaseAmountUnits int64 `json:"base_amount_units"`
	MultiplierMilli int64 `json:"multiplier_milli"`
}

type Valuation struct {
	Kind        string     `json:"valuation_kind"`
	CatalogID   string     `json:"catalog_id"`
	AmountUnits *int64     `json:"amount_units"`
	UnitScale   int64      `json:"unit_scale"`
	Confidence  string     `json:"confidence"`
	Reason      string     `json:"reason,omitempty"`
	Rate        *Rate      `json:"rate,omitempty"`
	Breakdown   *Breakdown `json:"breakdown,omitempty"`
}

func UnavailableValuation(kind, catalogID string, unitScale int64, reason string) Valuation {
	return Valuation{Kind: kind, CatalogID: catalogID, UnitScale: unitScale, Confidence: "unavailable", Reason: reason}
}

func validateTokenUsage(usage TokenUsage) error {
	if usage.InputTotal < 0 || usage.InputUncached < 0 || usage.CachedRead < 0 || usage.CacheWrite < 0 || usage.OutputTotal < 0 || usage.OutputReasoning < 0 {
		return ErrNegativeAmount
	}
	if usage.OutputReasoning > usage.OutputTotal {
		return fmt.Errorf("reasoning output is not a subset of output: %w", ErrNegativeAmount)
	}
	if usage.InputTotal > 0 && usage.CachedRead > usage.InputTotal {
		return fmt.Errorf("cached input exceeds input total: %w", ErrNegativeAmount)
	}
	return nil
}

func (catalog Catalog) Value(surface ProductSurface, model, tier string, usage TokenUsage) Valuation {
	kind, scale := "api_usd_equivalent", MicroUSDScale
	if surface == SurfaceChatGPTSubscription {
		kind, scale = "chatgpt_credits", MilliCreditScale
	}
	unavailable := func(reason string) Valuation {
		return UnavailableValuation(kind, catalog.ID, scale, reason)
	}
	if err := validateTokenUsage(usage); err != nil {
		return unavailable("invalid_usage_components")
	}
	rate, err := catalog.Resolve(surface, model, tier, usage.InputTotal)
	if err != nil {
		return unavailable("exact_rate_unavailable")
	}
	if usage.CacheWrite > 0 && rate.CacheWriteRate == nil {
		return unavailable("cache_write_rate_unavailable")
	}
	cacheWriteRate := int64(0)
	if rate.CacheWriteRate != nil {
		cacheWriteRate = *rate.CacheWriteRate
	}
	inputAmount, err := RoundWeightedSumEven([]WeightedComponent{{usage.InputUncached, rate.InputRateUnits}}, PerMillionDenom)
	if err != nil {
		return unavailable("input_amount_overflow")
	}
	cachedAmount, err := RoundWeightedSumEven([]WeightedComponent{{usage.CachedRead, rate.CachedRateUnits}}, PerMillionDenom)
	if err != nil {
		return unavailable("cached_amount_overflow")
	}
	cacheWriteAmount, err := RoundWeightedSumEven([]WeightedComponent{{usage.CacheWrite, cacheWriteRate}}, PerMillionDenom)
	if err != nil {
		return unavailable("cache_write_amount_overflow")
	}
	outputAmount, err := RoundWeightedSumEven([]WeightedComponent{{usage.OutputTotal, rate.OutputRateUnits}}, PerMillionDenom)
	if err != nil {
		return unavailable("output_amount_overflow")
	}
	baseAmount, err := RoundWeightedSumEven([]WeightedComponent{
		{usage.InputUncached, rate.InputRateUnits},
		{usage.CachedRead, rate.CachedRateUnits},
		{usage.CacheWrite, cacheWriteRate},
		{usage.OutputTotal, rate.OutputRateUnits},
	}, PerMillionDenom)
	if err != nil {
		return unavailable("total_amount_overflow")
	}
	if rate.PerRequestUnits > 0 {
		const maxInt64 = int64(^uint64(0) >> 1)
		if rate.PerRequestUnits > maxInt64-baseAmount {
			return unavailable("per_request_amount_overflow")
		}
		baseAmount += rate.PerRequestUnits
	}
	amount, err := MultiplyRatioEven(baseAmount, rate.MultiplierMilli, MultiplierDenom)
	if err != nil {
		return unavailable("multiplied_amount_overflow")
	}
	breakdown := &Breakdown{
		InputUnits: inputAmount, CachedUnits: cachedAmount, CacheWriteUnits: cacheWriteAmount,
		OutputUnits: outputAmount, PerRequestUnits: rate.PerRequestUnits, BaseAmountUnits: baseAmount,
		MultiplierMilli: rate.MultiplierMilli,
	}
	return Valuation{Kind: kind, CatalogID: catalog.ID, AmountUnits: &amount, UnitScale: scale,
		Confidence: "settled", Rate: &rate, Breakdown: breakdown}
}
