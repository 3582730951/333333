package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// smsCandidate is one (platform, country) option the selection algorithm considers.
type smsCandidate struct {
	Provider   SMSProvider
	CountryID  string  // numeric country id (as the platform expects)
	CountryISO string  // ISO-2 if known (for preferred-country weighting)
	Price      float64 // per-number cost on this platform for this country
	Count      int     // available inventory
	Rank       int     // platform's internal success-priority rank (0 = best)
	Score      float64 // computed composite score (higher = better)
}

// smsStatsCache memoizes per-platform balance + top-countries + iso map within a single
// GetBestSMS call so we don't re-query the platform for each candidate. Per-call (not shared
// across registrations) — GetBestSMS creates one and threads it through.
type smsStatsCache struct {
	mu      sync.Mutex
	balance map[string]float64           // provider name -> balance
	balOK   map[string]bool              // provider name -> balance fetch succeeded & >0
	top     map[string][]smsCountryPrice // provider name -> success-ranked priced countries
	topOK   map[string]bool              // provider name -> top fetch succeeded
	iso     map[string]map[string]string // provider name -> numeric-id -> ISO-2
	isoOK   map[string]bool              // provider name -> iso fetch attempted
}

func newSMSStatsCache() *smsStatsCache {
	return &smsStatsCache{
		balance: map[string]float64{}, balOK: map[string]bool{},
		top: map[string][]smsCountryPrice{}, topOK: map[string]bool{},
		iso: map[string]map[string]string{}, isoOK: map[string]bool{},
	}
}

// GetBestSMS picks the best (provider, country) for an OpenAI registration by combining
// each platform's same-day success ranking, price, inventory, and the operator's preferred
// country order (default BR > CO > PL). It then acquires a number from the top N candidates
// in order, falling back to the next on NO_NUMBERS / timeout.
//
// Algorithm:
//  1. For each SMS provider that implements BalanceProvider, fetch the balance (cached) and
//     skip platforms with zero funds.
//  2. For each funded provider that implements PriceProvider, fetch its success-ranked
//     top countries (cached) and compute a composite score per candidate country:
//     score = rankWeight*(maxRank-rank) + priceWeight*(maxPrice-price)/maxPrice + prefBonus
//     where prefBonus rewards countries in the preferred list (earlier = bigger bonus),
//     and inventory=0 candidates are dropped.
//  3. Sort all candidates cross-platform by score desc, then price asc, then prefer hero-sms
//     (Name=="herosms") on ties (live-verified stable).
//  4. Take the top `topN` candidates; try GetNumber on each until one succeeds.
//
// preferredCountries is ISO-2 codes in priority order (e.g. ["BR","CO","PL"]).
// Returns the chosen provider, phone, and order id.
func (m *Manager) GetBestSMS(ctx context.Context, preferredCountries []string, topN int) (SMSProvider, string, string, error) {
	if topN < 1 {
		topN = 3
	}
	cache := newSMSStatsCache()

	// Normalize the preferred list once. Earlier entry → higher weight.
	pref := make(map[string]int, len(preferredCountries))
	for i, c := range preferredCountries {
		iso := strings.ToUpper(strings.TrimSpace(c))
		if iso != "" {
			pref[iso] = len(preferredCountries) - i
		}
	}
	prefLen := len(preferredCountries)
	if prefLen < 1 {
		prefLen = 1
	}

	var candidates []smsCandidate
	// First pass: collect all (provider, country) candidates and remember the global price
	// range so the price component of the score is comparable ACROSS platforms (not just
	// within one platform's list).
	var globalMinPrice, globalMaxPrice float64
	for _, p := range m.SMS {
		name := p.Name()

		// Balance gate — skip platforms with no funds.
		if bp, ok := p.(BalanceProvider); ok {
			bal, ok := cache.balanceOf(ctx, bp, name)
			if !ok || bal <= 0 {
				continue
			}
		}

		// Success-ranked price list (can't compare without it).
		pp, ok := p.(PriceProvider)
		if !ok {
			continue
		}
		tops, ok := cache.topOf(ctx, pp, name, "dr")
		if !ok || len(tops) == 0 {
			continue
		}

		// Resolve numeric-country-id → ISO-2 (best-effort) to match the preferred list.
		isoByID := cache.isoMapOf(ctx, pp, name)

		maxRank := len(tops)
		if maxRank < 1 {
			maxRank = 1
		}

		for _, t := range tops {
			if t.Count <= 0 {
				continue // no inventory
			}
			if t.Price <= 0 {
				continue // can't price-compare
			}
			iso := isoByID[t.Country]
			candidates = append(candidates, smsCandidate{
				Provider: p, CountryID: t.Country, CountryISO: iso,
				Price: t.Price, Count: t.Count, Rank: t.Rank,
			})
			if globalMinPrice == 0 || t.Price < globalMinPrice {
				globalMinPrice = t.Price
			}
			if t.Price > globalMaxPrice {
				globalMaxPrice = t.Price
			}
		}
	}

	if len(candidates) == 0 {
		return nil, "", "", ErrNoProviderAvailable
	}

	priceSpan := globalMaxPrice - globalMinPrice
	if priceSpan <= 0 {
		priceSpan = globalMinPrice // single price point — price component is a no-op
	}

	// Second pass: compute the composite score per candidate, now with a global price
	// normalization that makes cross-platform comparison fair (a cheaper platform actually
	// scores higher, instead of being penalized because its only country is its own max).
	maxRank := 0
	for range candidates {
		maxRank++
	}
	if maxRank < 1 {
		maxRank = 1
	}
	// Historical success-rate snapshot: per (provider,countryISO) from registration_records.
	// Drives the 50% success-rate weight; when no data, rate defaults to 0.5 (neutral) and the
	// preferred-country bonus provides the initial direction (BR first).
	rateSnap := map[string]float64{}
	if m.Stats != nil {
		rateSnap = m.Stats.SuccessRateSnapshot(ctx)
	}
	for i := range candidates {
		c := &candidates[i]
		score := 0.0
		// Success-rate weight (best): 50%. Falls back to 0.5 when no data.
		rateKey := fmt.Sprintf("%s|%s", c.Provider.Name(), c.CountryISO)
		rate, hasRate := rateSnap[rateKey]
		if !hasRate {
			rate = 0.5
		}
		score += 0.50 * rate
		// Price weight (cheaper better): 25% — cheaper than the global max scores higher.
		if priceSpan > 0 {
			score += 0.25 * (globalMaxPrice - c.Price) / priceSpan
		}
		// Rank weight (platform internal success-priority rank): 15%.
		score += 0.15 * (1.0 - float64(c.Rank)/float64(maxRank+1))
		// Preferred-country bonus: 10% scaled by position in the preferred list.
		if w, ok := pref[c.CountryISO]; ok && w > 0 {
			score += 0.10 * float64(w) / float64(prefLen)
		}
		c.Score = score
	}

	// Step 3: cross-platform sort.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Price != candidates[j].Price {
			return candidates[i].Price < candidates[j].Price
		}
		// Tiebreak: prefer hero-sms (live-verified).
		ih := candidates[i].Provider.Name() == "herosms"
		jh := candidates[j].Provider.Name() == "herosms"
		if ih != jh {
			return ih
		}
		return candidates[i].Count > candidates[j].Count
	})

	// Step 4: try the top N candidates in order.
	if topN > len(candidates) {
		topN = len(candidates)
	}
	var lastErr error
	for i := 0; i < topN; i++ {
		c := candidates[i]
		if ctx.Err() != nil {
			break
		}
		phone, orderID, err := c.Provider.GetNumber(ctx, c.CountryID)
		if err == nil && phone != "" {
			return c.Provider, phone, orderID, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, "", "", fmt.Errorf("no SMS provider produced a number (last: %w)", lastErr)
	}
	return nil, "", "", ErrNoProviderAvailable
}

// --- smsStatsCache helpers ---

func (c *smsStatsCache) balanceOf(ctx context.Context, bp BalanceProvider, key string) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok, has := c.balOK[key]; has {
		return c.balance[key], ok
	}
	bal, err := bp.GetBalance(ctx)
	ok := err == nil && bal > 0
	c.balance[key] = bal
	c.balOK[key] = ok
	return bal, ok
}

func (c *smsStatsCache) topOf(ctx context.Context, pp PriceProvider, key, service string) ([]smsCountryPrice, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok, has := c.topOK[key]; has {
		return c.top[key], ok
	}
	raw, err := pp.GetTopCountries(ctx, service)
	out := make([]smsCountryPrice, 0, len(raw))
	for _, r := range raw {
		// The sms package returns its own CountryPrice (aliased to smsCountryPrice); via the
		// interface it arrives as interface{} — assert to the concrete alias type.
		if cp, ok := r.(smsCountryPrice); ok {
			out = append(out, cp)
		} else if cp, ok := r.(*smsCountryPrice); ok && cp != nil {
			out = append(out, *cp)
		}
	}
	ok := err == nil && len(out) > 0
	c.top[key] = out
	c.topOK[key] = ok
	return out, ok
}

// isoMapOf returns a numeric-country-id → ISO-2 mapping for a platform (best-effort, via
// GetCountries). Cached per call.
func (c *smsStatsCache) isoMapOf(ctx context.Context, pp PriceProvider, key string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isoOK[key] {
		return c.iso[key]
	}
	m := map[string]string{}
	raw, err := pp.GetCountries(ctx)
	if err == nil {
		for _, r := range raw {
			ci, ok := r.(smsCountryInfo)
			if !ok {
				if cp, ok := r.(*smsCountryInfo); ok && cp != nil {
					ci = *cp
				} else {
					continue
				}
			}
			if ci.ISO != "" {
				m[fmt.Sprintf("%d", ci.ID)] = strings.ToUpper(ci.ISO)
			}
		}
	}
	c.iso[key] = m
	c.isoOK[key] = true
	return m
}
