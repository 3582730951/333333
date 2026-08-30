package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type ProductSurface string
type UnitKind string

const (
	SurfaceOpenAIAPI           ProductSurface = "openai_api"
	SurfaceChatGPTSubscription ProductSurface = "chatgpt_subscription"
	SurfaceEnterpriseContract  ProductSurface = "enterprise_contract"
	SurfaceThirdParty          ProductSurface = "third_party"

	UnitMicroUSD    UnitKind = "micro_usd"
	UnitMilliCredit UnitKind = "milli_credit"
)

const (
	ContextShort = "short"
	ContextLong  = "long"
	ContextAll   = "all"
	LongContext  = int64(272_000)
)

var ErrRateUnavailable = errors.New("an exact audited pricing rate is unavailable")

type Rate struct {
	ProductSurface  ProductSurface `json:"product_surface"`
	Provider        string         `json:"provider"`
	Model           string         `json:"model"`
	ServiceTier     string         `json:"service_tier"`
	ContextBand     string         `json:"context_band"`
	UnitKind        UnitKind       `json:"unit_kind"`
	InputRateUnits  int64          `json:"input_rate_units"`
	CachedRateUnits int64          `json:"cached_read_rate_units"`
	CacheWriteRate  *int64         `json:"cache_write_rate_units"`
	OutputRateUnits int64          `json:"output_rate_units"`
	PerRequestUnits int64          `json:"per_request_rate_units"`
	MultiplierMilli int64          `json:"multiplier_milli"`
	SourceLineRef   string         `json:"source_line_ref"`
}

type Catalog struct {
	ID          string `json:"id"`
	CatalogKind string `json:"catalog_kind"`
	SourceURL   string `json:"source_url"`
	EffectiveAt int64  `json:"effective_at"`
	// ExpiresAt is an optional validity boundary for the immutable snapshot.
	// Zero means no explicit expiry (legacy catalogs).
	ExpiresAt   int64  `json:"expires_at,omitempty"`
	FetchedAt   int64  `json:"fetched_at"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	SnapshotRef string `json:"raw_snapshot_ref"`
	SHA256      string `json:"sha256"`
	Rates       []Rate `json:"rates"`
}

func int64Pointer(value int64) *int64 { return &value }

func apiRate(model, tier, band string, input, cached, cacheWrite, output int64, source string) Rate {
	return Rate{ProductSurface: SurfaceOpenAIAPI, Provider: "openai", Model: model, ServiceTier: tier,
		ContextBand: band, UnitKind: UnitMicroUSD, InputRateUnits: input, CachedRateUnits: cached,
		CacheWriteRate: int64Pointer(cacheWrite), OutputRateUnits: output, MultiplierMilli: DefaultMultiplier, SourceLineRef: source}
}

func apiRateWithoutCacheWrite(model, tier string, input, cached, output int64, source string) Rate {
	return Rate{ProductSurface: SurfaceOpenAIAPI, Provider: "openai", Model: model, ServiceTier: tier,
		ContextBand: ContextShort, UnitKind: UnitMicroUSD, InputRateUnits: input, CachedRateUnits: cached,
		OutputRateUnits: output, MultiplierMilli: DefaultMultiplier, SourceLineRef: source}
}

func creditRate(model, tier string, input, cached, output, multiplier int64, source string) Rate {
	return Rate{ProductSurface: SurfaceChatGPTSubscription, Provider: "openai", Model: model, ServiceTier: tier,
		ContextBand: ContextAll, UnitKind: UnitMilliCredit, InputRateUnits: input, CachedRateUnits: cached,
		CacheWriteRate: int64Pointer(0), OutputRateUnits: output, MultiplierMilli: multiplier, SourceLineRef: source}
}

// OfficialOpenAI20260829 is an immutable, reviewed snapshot of the official
// pages listed in SourceURL/SourceLineRef. Amounts are base units per 1M tokens:
// micro-USD for API rows and milli-credit for ChatGPT rows.
func OfficialOpenAI20260829() Catalog {
	const (
		modelSol   = "gpt-5.6-sol"
		modelTerra = "gpt-5.6-terra"
		modelLuna  = "gpt-5.6-luna"
		apiModel   = "developers.openai.com/api/docs/models (2026-08-29)"
		apiFast    = "openai.com/api-fast-mode lines 20-32 (2026-08-29)"
		credits    = "help.openai.com/11481834 lines 56-95 (2026-08-29)"
		speed      = "learn.chatgpt.com/docs/agent-configuration/speed lines 801-808 (2026-08-29)"
	)
	rates := []Rate{
		apiRate(modelSol, TierDefault, ContextShort, 4_000_000, 400_000, 5_000_000, 20_000_000, apiModel),
		apiRate(modelSol, TierDefault, ContextLong, 8_000_000, 800_000, 10_000_000, 30_000_000, apiModel),
		apiRate(modelTerra, TierDefault, ContextShort, 2_000_000, 200_000, 2_500_000, 12_000_000, apiModel),
		apiRate(modelTerra, TierDefault, ContextLong, 4_000_000, 400_000, 5_000_000, 18_000_000, apiModel),
		apiRate(modelLuna, TierDefault, ContextShort, 200_000, 20_000, 250_000, 1_200_000, apiModel),
		apiRate(modelLuna, TierDefault, ContextLong, 400_000, 40_000, 500_000, 1_800_000, apiModel),
		apiRate(modelSol, TierFast, ContextShort, 8_000_000, 800_000, 10_000_000, 40_000_000, apiFast),
		apiRate(modelSol, TierFast, ContextLong, 16_000_000, 1_600_000, 20_000_000, 60_000_000, apiFast),
		apiRate(modelTerra, TierFast, ContextShort, 4_000_000, 400_000, 5_000_000, 24_000_000, apiFast),
		apiRate(modelTerra, TierFast, ContextLong, 8_000_000, 800_000, 10_000_000, 36_000_000, apiFast),
		apiRate(modelLuna, TierFast, ContextShort, 400_000, 40_000, 500_000, 2_400_000, apiFast),
		apiRate(modelLuna, TierFast, ContextLong, 800_000, 80_000, 1_000_000, 3_600_000, apiFast),
		apiRateWithoutCacheWrite("gpt-5.5", TierDefault, 5_000_000, 500_000, 30_000_000, apiModel),
		apiRateWithoutCacheWrite("gpt-5.5", TierFast, 12_500_000, 1_250_000, 75_000_000, apiFast),
		apiRateWithoutCacheWrite("gpt-5.4", TierDefault, 2_500_000, 250_000, 15_000_000, apiModel),
		apiRateWithoutCacheWrite("gpt-5.4", TierFast, 5_000_000, 500_000, 30_000_000, apiFast),

		creditRate(modelSol, TierDefault, 100_000, 10_000, 500_000, DefaultMultiplier, credits),
		creditRate(modelSol, TierFast, 100_000, 10_000, 500_000, 2_500, credits+"; "+speed),
		creditRate(modelTerra, TierDefault, 50_000, 5_000, 300_000, DefaultMultiplier, credits),
		creditRate(modelTerra, TierFast, 50_000, 5_000, 300_000, 2_500, credits+"; "+speed),
		creditRate(modelLuna, TierDefault, 5_000, 500, 30_000, DefaultMultiplier, credits),
		creditRate(modelLuna, TierFast, 5_000, 500, 30_000, 2_500, credits+"; "+speed),
		creditRate("gpt-5.5", TierDefault, 125_000, 12_500, 750_000, DefaultMultiplier, credits),
		creditRate("gpt-5.5", TierFast, 125_000, 12_500, 750_000, 2_500, credits+"; "+speed),
		creditRate("gpt-5.4", TierDefault, 62_500, 6_250, 375_000, DefaultMultiplier, credits),
		creditRate("gpt-5.4", TierFast, 62_500, 6_250, 375_000, 2_000, credits+"; "+speed),
	}
	sort.Slice(rates, func(i, j int) bool {
		left := string(rates[i].ProductSurface) + "\x00" + rates[i].Model + "\x00" + rates[i].ServiceTier + "\x00" + rates[i].ContextBand
		right := string(rates[j].ProductSurface) + "\x00" + rates[j].Model + "\x00" + rates[j].ServiceTier + "\x00" + rates[j].ContextBand
		return left < right
	})
	catalog := Catalog{
		ID: "openai-official-2026-08-29-v2", CatalogKind: "official_list_price_and_credits",
		SourceURL:   "https://developers.openai.com/api/docs/models;https://openai.com/api-fast-mode/;https://help.openai.com/en/articles/11481834;https://learn.chatgpt.com/docs/agent-configuration/speed",
		EffectiveAt: 1787284800, ExpiresAt: 1795305600, FetchedAt: 1787976000, Currency: "USD+CREDITS", Status: "active",
		SnapshotRef: "embedded:internal/pricing/catalog.go#OfficialOpenAI20260829", Rates: rates,
	}
	catalog.SHA256 = CanonicalSHA256(catalog)
	return catalog
}

// CanonicalSHA256 fingerprints immutable pricing content. Lifecycle status and
// the fingerprint itself are intentionally excluded: moving an audited snapshot
// from draft to active must not change the content identity.
func CanonicalSHA256(catalog Catalog) string {
	catalog.SHA256 = ""
	catalog.Status = ""
	canonical, _ := json.Marshal(catalog)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func officialCatalogSourceAllowed(raw string) bool {
	parts := strings.Split(raw, ";")
	if len(parts) == 0 {
		return false
	}
	allowed := map[string]bool{
		"openai.com": true, "developers.openai.com": true,
		"help.openai.com": true, "learn.chatgpt.com": true,
	}
	for _, part := range parts {
		parsed, err := url.Parse(strings.TrimSpace(part))
		if err != nil || parsed.Scheme != "https" || !allowed[strings.ToLower(parsed.Hostname())] || parsed.User != nil {
			return false
		}
	}
	return true
}

// ValidateCatalog accepts only complete, internally consistent OpenAI snapshots
// sourced from the audited official-domain allowlist. Unknown models and rate
// dimensions fail closed instead of silently inheriting a family price.
func ValidateCatalog(catalog Catalog) error {
	if strings.TrimSpace(catalog.ID) == "" || len(catalog.ID) > 160 {
		return fmt.Errorf("invalid catalog id")
	}
	if catalog.EffectiveAt <= 0 || catalog.FetchedAt <= 0 || catalog.FetchedAt < catalog.EffectiveAt {
		return fmt.Errorf("invalid catalog timestamps")
	}
	if !officialCatalogSourceAllowed(catalog.SourceURL) {
		return fmt.Errorf("catalog source is not on the official allowlist")
	}
	if strings.TrimSpace(catalog.Currency) == "" || strings.TrimSpace(catalog.CatalogKind) == "" || len(catalog.Rates) == 0 {
		return fmt.Errorf("catalog metadata is incomplete")
	}
	if got := CanonicalSHA256(catalog); !strings.EqualFold(strings.TrimSpace(catalog.SHA256), got) {
		return fmt.Errorf("catalog checksum mismatch")
	}
	seen := make(map[string]struct{}, len(catalog.Rates))
	for index, rate := range catalog.Rates {
		if rate.Provider != "openai" {
			return fmt.Errorf("rate %d has unsupported provider", index)
		}
		canonical, ok := CanonicalModel(rate.Model)
		if !ok || canonical != rate.Model {
			return fmt.Errorf("rate %d has unknown or non-canonical model", index)
		}
		if rate.ServiceTier != TierDefault && rate.ServiceTier != TierFast {
			return fmt.Errorf("rate %d has unknown service tier", index)
		}
		switch rate.ProductSurface {
		case SurfaceOpenAIAPI:
			if rate.UnitKind != UnitMicroUSD || (rate.ContextBand != ContextShort && rate.ContextBand != ContextLong) {
				return fmt.Errorf("rate %d has inconsistent API dimensions", index)
			}
		case SurfaceChatGPTSubscription:
			if rate.UnitKind != UnitMilliCredit || rate.ContextBand != ContextAll {
				return fmt.Errorf("rate %d has inconsistent ChatGPT dimensions", index)
			}
		default:
			return fmt.Errorf("rate %d has unsupported product surface", index)
		}
		if rate.InputRateUnits < 0 || rate.CachedRateUnits < 0 || rate.OutputRateUnits < 0 || rate.PerRequestUnits < 0 || rate.MultiplierMilli <= 0 {
			return fmt.Errorf("rate %d has invalid amount", index)
		}
		if rate.CacheWriteRate != nil && *rate.CacheWriteRate < 0 {
			return fmt.Errorf("rate %d has invalid cache-write amount", index)
		}
		if strings.TrimSpace(rate.SourceLineRef) == "" {
			return fmt.Errorf("rate %d has no source reference", index)
		}
		key := strings.Join([]string{string(rate.ProductSurface), rate.Provider, rate.Model, rate.ServiceTier, rate.ContextBand, string(rate.UnitKind)}, "\x00")
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("rate %d duplicates a pricing dimension", index)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func CanonicalModel(model string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.6", "gpt-5.6-sol":
		return "gpt-5.6-sol", true
	case "gpt-5.6-terra":
		return "gpt-5.6-terra", true
	case "gpt-5.6-luna":
		return "gpt-5.6-luna", true
	case "gpt-5.5":
		return "gpt-5.5", true
	case "gpt-5.4":
		return "gpt-5.4", true
	default:
		return "", false
	}
}

func (catalog Catalog) Resolve(surface ProductSurface, model, tier string, inputTotal int64) (Rate, error) {
	canonical, ok := CanonicalModel(model)
	if !ok || inputTotal < 0 {
		return Rate{}, ErrRateUnavailable
	}
	tier = NormalizeServiceTier(tier)
	if tier == TierUnknown {
		return Rate{}, ErrRateUnavailable
	}
	band := ContextShort
	if inputTotal > LongContext {
		band = ContextLong
	}
	for _, rate := range catalog.Rates {
		if rate.ProductSurface != surface || rate.Model != canonical || rate.ServiceTier != tier {
			continue
		}
		if rate.ContextBand == ContextAll || rate.ContextBand == band {
			return rate, nil
		}
	}
	return Rate{}, ErrRateUnavailable
}
