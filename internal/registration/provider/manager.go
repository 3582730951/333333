package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// smsCandidate is one (platform, country) option the selection algorithm considers.
type smsCandidate struct {
	Provider    SMSProvider
	CountryID   string  // numeric country id (as the platform expects)
	CountryISO  string  // ISO-2 if known (for preferred-country weighting)
	Price       float64 // per-number cost on this platform for this country
	Count       int     // available inventory
	Rank        int     // platform's internal success-priority rank (0 = best)
	Score       float64 // computed composite score (higher = better)
	SuccessRate float64
	Attempts    int
	HasHistory  bool
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
	purchase, err := m.GetBestSMSPurchase(ctx, preferredCountries, topN, 0, 0)
	if err != nil {
		return nil, "", "", err
	}
	return purchase.Provider, purchase.Phone, purchase.OrderID, nil
}

// GetBestSMSPurchase applies the administrator's price bounds and returns the exact market
// row selected. A fresh hourly catalog is preferred; live top-country data remains a fallback
// for first boot and providers whose catalog refresh failed.
func (m *Manager) GetBestSMSPurchase(ctx context.Context, preferredCountries []string, topN int, minPrice, maxPrice float64) (SMSPurchase, error) {
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
	providerByName := make(map[string]SMSProvider, len(m.SMS))
	for _, item := range m.SMS {
		providerByName[strings.ToLower(strings.TrimSpace(item.Name()))] = item
	}
	if m.Stats != nil {
		fresh := m.Stats.PriceSnapshots(ctx, 90*time.Minute)
		if len(fresh) == 0 {
			_, _ = m.RefreshSMSPrices(ctx)
			fresh = m.Stats.PriceSnapshots(ctx, 90*time.Minute)
		}
		for _, item := range fresh {
			p := providerByName[strings.ToLower(strings.TrimSpace(item.Provider))]
			if p == nil || item.Inventory <= 0 || item.Price <= 0 || item.Balance == 0 || !priceWithinBounds(item.Price, minPrice, maxPrice) {
				continue
			}
			candidates = append(candidates, smsCandidate{
				Provider: p, CountryID: item.CountryID, CountryISO: normalizedCountryISO(item.CountryISO, item.CountryID),
				Price: item.Price, Count: item.Inventory, Rank: item.Rank,
			})
		}
	}
	// First pass: collect all (provider, country) candidates and remember the global price
	// range so the price component of the score is comparable ACROSS platforms (not just
	// within one platform's list).
	var globalMinPrice, globalMaxPrice float64
	usingCachedCatalog := len(candidates) > 0
	for _, p := range m.SMS {
		if usingCachedCatalog {
			break
		}
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
			if t.Price <= 0 || !priceWithinBounds(t.Price, minPrice, maxPrice) {
				continue // can't price-compare
			}
			iso := normalizedCountryISO(isoByID[t.Country], t.Country)
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
		return SMSPurchase{}, ErrNoProviderAvailable
	}
	for _, c := range candidates {
		if globalMinPrice == 0 || c.Price < globalMinPrice {
			globalMinPrice = c.Price
		}
		if c.Price > globalMaxPrice {
			globalMaxPrice = c.Price
		}
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
	statSnap := map[string]SMSSuccessStat{}
	if m.Stats != nil {
		rateSnap = m.Stats.SuccessRateSnapshot(ctx)
		statSnap = m.Stats.SuccessStatsSnapshot(ctx)
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
		c.SuccessRate = rate
		if stat, ok := statSnap[rateKey]; ok {
			c.Attempts = stat.Attempts
			c.HasHistory = stat.Attempts >= 3
		}
		score += 0.50 * rate
		// Price weight (cheaper better): 25% — cheaper than the global max scores higher.
		if priceSpan > 0 {
			score += 0.25 * (globalMaxPrice - c.Price) / priceSpan
		}
		// Rank weight (platform internal success-priority rank): 15%. Full-market
		// catalogs use 9999 for countries that are absent from the provider's short
		// "top" list. Treat that sentinel as no rank evidence rather than allowing
		// an unbounded negative term to erase real historical success evidence.
		rankScore := 0.0
		if c.Rank >= 0 && c.Rank < 1000 {
			rankScore = 1.0 - float64(c.Rank)/float64(maxRank+1)
			if rankScore < 0 {
				rankScore = 0
			}
		}
		score += 0.15 * rankScore
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
		var phone, orderID string
		var err error
		if bounded, ok := c.Provider.(BoundedSMSProvider); ok {
			phone, orderID, err = bounded.GetNumberWithPriceBounds(ctx, c.CountryID, minPrice, maxPrice)
		} else {
			phone, orderID, err = c.Provider.GetNumber(ctx, c.CountryID)
		}
		if err == nil && phone != "" {
			return SMSPurchase{Provider: c.Provider, Phone: phone, OrderID: orderID,
				CountryID: c.CountryID, CountryISO: c.CountryISO, Price: c.Price}, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return SMSPurchase{}, fmt.Errorf("no SMS provider produced a number (last: %w)", lastErr)
	}
	return SMSPurchase{}, ErrNoProviderAvailable
}

// SMSMarketCandidate is the administrator-facing explanation for one hourly offer.
type SMSMarketCandidate struct {
	SMSPriceSnapshot
	Attempts       int     `json:"attempts"`
	Succeeded      int     `json:"succeeded"`
	SuccessRate    float64 `json:"success_rate"`
	Score          float64 `json:"score"`
	Eligible       bool    `json:"eligible"`
	SelectionBasis string  `json:"selection_basis"`
}

// RefreshSMSPrices traverses every country exposed by each capable provider and replaces
// that provider's persisted market snapshot. It is serialized because runtime and manual
// refreshes may coincide.
func (m *Manager) RefreshSMSPrices(ctx context.Context) (int, error) {
	if m == nil || m.Stats == nil {
		return 0, errors.New("SMS price storage is unavailable")
	}
	m.priceRefreshMu.Lock()
	defer m.priceRefreshMu.Unlock()

	const service = "dr"
	total := 0
	var refreshErrors []error
	for _, smsProvider := range m.SMS {
		priceProvider, ok := smsProvider.(PriceProvider)
		if !ok {
			continue
		}
		providerName := strings.ToLower(strings.TrimSpace(smsProvider.Name()))
		balance := -1.0
		if capable, ok := smsProvider.(BalanceProvider); ok {
			value, err := capable.GetBalance(ctx)
			if err != nil {
				refreshErrors = append(refreshErrors, fmt.Errorf("%s balance: %w", providerName, err))
			} else {
				balance = value
			}
		}

		countries, countryErr := priceProvider.GetCountries(ctx)
		if countryErr != nil {
			refreshErrors = append(refreshErrors, fmt.Errorf("%s countries: %w", providerName, countryErr))
		}
		countryByID := make(map[string]CountryInfo, len(countries))
		for _, country := range countries {
			countryByID[strconv.Itoa(country.ID)] = country
		}

		top, topErr := priceProvider.GetTopCountries(ctx, service)
		catalogSucceeded := topErr == nil
		if topErr != nil {
			refreshErrors = append(refreshErrors, fmt.Errorf("%s top countries: %w", providerName, topErr))
		}
		rankByID := make(map[string]int, len(top))
		for _, item := range top {
			rankByID[item.Country] = item.Rank
		}

		offers := top
		if complete, ok := smsProvider.(FullPriceProvider); ok {
			all, err := complete.GetAllCountryPrices(ctx, service)
			if err != nil {
				refreshErrors = append(refreshErrors, fmt.Errorf("%s all prices: %w", providerName, err))
			} else if len(all) > 0 {
				catalogSucceeded = true
				offers = all
			} else {
				catalogSucceeded = true
			}
		}
		if !catalogSucceeded {
			continue
		}
		fetchedAt := time.Now().Unix()
		items := make([]SMSPriceSnapshot, 0, len(offers))
		seen := map[string]bool{}
		for _, offer := range offers {
			countryID := strings.TrimSpace(offer.Country)
			if countryID == "" || seen[countryID] || offer.Price <= 0 {
				continue
			}
			seen[countryID] = true
			country := countryByID[countryID]
			rank := 9999
			if value, ok := rankByID[countryID]; ok {
				rank = value
			}
			name := strings.TrimSpace(country.Eng)
			if name == "" {
				name = strings.TrimSpace(country.Chn)
			}
			if name == "" {
				name = strings.TrimSpace(offer.Name)
			}
			items = append(items, SMSPriceSnapshot{
				Provider: providerName, Service: service, CountryID: countryID,
				CountryISO: normalizedCountryISO(country.ISO, countryID), CountryName: name,
				Price: offer.Price, Inventory: offer.Count, Rank: rank, Balance: balance,
				FetchedAt: fetchedAt,
			})
		}
		if err := m.Stats.ReplacePriceSnapshots(ctx, providerName, service, items, fetchedAt); err != nil {
			refreshErrors = append(refreshErrors, fmt.Errorf("%s persist prices: %w", providerName, err))
			continue
		}
		total += len(items)
	}
	return total, errors.Join(refreshErrors...)
}

// SMSMarketSnapshot combines the last hourly catalog with the 14-day success evidence used
// by automatic selection. Rows are ordered as the selector would consider them.
func (m *Manager) SMSMarketSnapshot(ctx context.Context, preferredCountries []string, minPrice, maxPrice float64) []SMSMarketCandidate {
	if m == nil || m.Stats == nil {
		return []SMSMarketCandidate{}
	}
	prices := m.Stats.PriceSnapshots(ctx, 0)
	stats := m.Stats.SuccessStatsSnapshot(ctx)
	pref := make(map[string]int, len(preferredCountries))
	for index, country := range preferredCountries {
		pref[strings.ToUpper(strings.TrimSpace(country))] = len(preferredCountries) - index
	}
	maxPriceSeen := 0.0
	for _, item := range prices {
		if item.Price > maxPriceSeen {
			maxPriceSeen = item.Price
		}
	}
	if maxPriceSeen <= 0 {
		maxPriceSeen = 1
	}
	out := make([]SMSMarketCandidate, 0, len(prices))
	for _, item := range prices {
		item.CountryISO = normalizedCountryISO(item.CountryISO, item.CountryID)
		stat := stats[fmt.Sprintf("%s|%s", item.Provider, item.CountryISO)]
		rate := 0.5
		basis := "community_cold_start"
		if stat.Attempts >= 3 {
			rate = stat.SuccessRate
			basis = "historical_success_rate"
		}
		preference := 0.0
		if weight := pref[item.CountryISO]; weight > 0 && len(preferredCountries) > 0 {
			preference = float64(weight) / float64(len(preferredCountries))
		}
		rankScore := 0.0
		if item.Rank >= 0 && item.Rank < 1000 {
			rankScore = 1 / float64(item.Rank+1)
		}
		row := SMSMarketCandidate{
			SMSPriceSnapshot: item, Attempts: stat.Attempts, Succeeded: stat.Succeeded,
			SuccessRate: rate, SelectionBasis: basis,
			Eligible: item.Inventory > 0 && item.Price > 0 && item.Balance != 0 && priceWithinBounds(item.Price, minPrice, maxPrice),
		}
		row.Score = 0.50*rate + 0.25*(maxPriceSeen-item.Price)/maxPriceSeen + 0.15*rankScore + 0.10*preference
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Eligible != out[j].Eligible {
			return out[i].Eligible
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Price < out[j].Price
	})
	return out
}

// BestSMSCountry chooses a country without reserving a number. Browser engines call this
// before launch so a conditional add_phone step starts with the same historical-success
// policy while avoiding an unnecessary purchase when no phone challenge appears.
func (m *Manager) BestSMSCountry(ctx context.Context, providerName string, preferredCountries []string, minPrice, maxPrice float64) (SMSMarketCandidate, bool) {
	if m == nil || m.Stats == nil {
		return SMSMarketCandidate{}, false
	}
	if len(m.Stats.PriceSnapshots(ctx, 90*time.Minute)) == 0 {
		_, _ = m.RefreshSMSPrices(ctx)
	}
	providerName = strings.TrimSpace(providerName)
	for _, item := range m.SMSMarketSnapshot(ctx, preferredCountries, minPrice, maxPrice) {
		if !item.Eligible || time.Now().Unix()-item.FetchedAt > 5400 {
			continue
		}
		if providerName == "" || strings.EqualFold(providerName, item.Provider) {
			return item, true
		}
	}
	return SMSMarketCandidate{}, false
}

func priceWithinBounds(price, minPrice, maxPrice float64) bool {
	return price > 0 && (minPrice <= 0 || price >= minPrice) && (maxPrice <= 0 || price <= maxPrice)
}

func normalizedCountryISO(value, countryID string) string {
	if iso := strings.ToUpper(strings.TrimSpace(value)); iso != "" {
		return iso
	}
	known := map[string]string{
		"4": "PH", "6": "ID", "15": "PL", "16": "GB", "22": "IN",
		"27": "ZA", "33": "CO", "52": "TH", "56": "CL", "73": "BR",
	}
	return known[strings.TrimSpace(countryID)]
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
	out := append([]smsCountryPrice(nil), raw...)
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
		for _, ci := range raw {
			if ci.ISO != "" {
				m[fmt.Sprintf("%d", ci.ID)] = strings.ToUpper(ci.ISO)
			}
		}
	}
	c.iso[key] = m
	c.isoOK[key] = true
	return m
}
