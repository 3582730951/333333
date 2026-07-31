// stats.go provides per-platform, per-country, per-service success-rate queries
// from the registration_records table, so GetBestSMS can use historical conversion
// data instead of a hardcoded preferred-country list.
package provider

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// SMSStats queries registration_records for per-(provider,country) success rates.
// It is a lightweight wrapper around the pool_server's sqlite DB — the table already
// has sms_provider/sms_country/sms_cost columns (added by migration), and
// adminRegisterStatsDaily already aggregates them. This makes the same data available
// to GetBestSMS for weighting.
type SMSStats struct {
	db *sql.DB
	mu sync.RWMutex
	// In-memory cache of recent success rates (TTL 5 min) so GetBestSMS doesn't
	// hammer the DB for every candidate.
	cache   map[string]float64 // key = "provider|countryISO"
	cacheTS time.Time
	ttl     time.Duration
}

// SMSPriceSnapshot is the latest hourly market observation for one provider and country.
type SMSPriceSnapshot struct {
	Provider    string  `json:"provider"`
	Service     string  `json:"service"`
	CountryID   string  `json:"country_id"`
	CountryISO  string  `json:"country_iso"`
	CountryName string  `json:"country_name,omitempty"`
	Price       float64 `json:"price"`
	Inventory   int     `json:"inventory"`
	Rank        int     `json:"provider_rank"`
	Balance     float64 `json:"balance"`
	FetchedAt   int64   `json:"fetched_at"`
}

// SMSSuccessStat exposes the evidence used by automatic country selection.
type SMSSuccessStat struct {
	Provider    string  `json:"provider"`
	CountryISO  string  `json:"country_iso"`
	Attempts    int     `json:"attempts"`
	Succeeded   int     `json:"succeeded"`
	SuccessRate float64 `json:"success_rate"`
}

// NewSMSStats creates a stats reader backed by the pool_server's sqlite DB.
// db is the write-capable handle (reads are safe on WAL sqlite).
func NewSMSStats(db *sql.DB) *SMSStats {
	return &SMSStats{db: db, ttl: 5 * time.Minute, cache: map[string]float64{}}
}

// cacheKey builds the lookup key: "herosms|BR"
func (s *SMSStats) cacheKey(provider, countryISO string) string {
	return fmt.Sprintf("%s|%s", provider, countryISO)
}

// SuccessRate returns the rolling success rate [0,1] for (provider, countryISO) from
// recent registration_records (last 14 days, min 3 attempts). Returns (rate, valid)
// where valid=false means "not enough data to judge" (caller should fall back to
// preferred-country cold-start).
func (s *SMSStats) SuccessRate(ctx context.Context, provider, countryISO string) (float64, bool) {
	if s == nil || s.db == nil {
		return 0, false
	}
	key := s.cacheKey(provider, countryISO)
	s.mu.RLock()
	if time.Since(s.cacheTS) < s.ttl {
		v, ok := s.cache[key]
		s.mu.RUnlock()
		return v, ok
	}
	s.mu.RUnlock()

	// Refresh cache: query all (provider,country) pairs at once so a single
	// GetBestSMS call only hits the DB once.
	cutoff := time.Now().Unix() - int64(14*24*3600)
	rows, err := s.db.QueryContext(ctx,
		`SELECT sms_provider, sms_country, COUNT(*), SUM(CASE WHEN status IN ('success','succeeded') THEN 1 ELSE 0 END)
		 FROM registration_records
		 WHERE created_at >= ? AND sms_provider != '' AND sms_country != ''
		 GROUP BY sms_provider, sms_country`, cutoff)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	m := map[string]float64{}
	var minSamples int = 3
	for rows.Next() {
		var prov, country string
		var total, succ int
		if err := rows.Scan(&prov, &country, &total, &succ); err != nil {
			continue
		}
		if total < minSamples {
			continue
		}
		m[fmt.Sprintf("%s|%s", prov, country)] = float64(succ) / float64(total)
	}
	s.mu.Lock()
	s.cache = m
	s.cacheTS = time.Now()
	s.mu.Unlock()

	if v, ok := m[key]; ok {
		return v, true
	}
	return 0, false
}

// SuccessRateSnapshot returns all cached (provider,country) -> rate mappings.
// Use this when GetBestSMS needs many rates at once — one call covers all candidates.
func (s *SMSStats) SuccessRateSnapshot(ctx context.Context) map[string]float64 {
	s.SuccessRate(ctx, "", "") // trigger cache refresh (empty key won't match but refresh runs)
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]float64, len(s.cache))
	for k, v := range s.cache {
		out[k] = v
	}
	return out
}

// SuccessStatsSnapshot returns 14-day sample counts as well as rates. Both historical
// "succeeded" and current "success" spellings are accepted during rolling upgrades.
func (s *SMSStats) SuccessStatsSnapshot(ctx context.Context) map[string]SMSSuccessStat {
	out := map[string]SMSSuccessStat{}
	if s == nil || s.db == nil {
		return out
	}
	cutoff := time.Now().Unix() - int64(14*24*3600)
	rows, err := s.db.QueryContext(ctx, `
SELECT sms_provider, sms_country, COUNT(*),
       SUM(CASE WHEN status IN ('success','succeeded') THEN 1 ELSE 0 END)
FROM registration_records
WHERE created_at >= ? AND sms_provider <> '' AND sms_country <> ''
GROUP BY sms_provider, sms_country`, cutoff)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var item SMSSuccessStat
		if rows.Scan(&item.Provider, &item.CountryISO, &item.Attempts, &item.Succeeded) != nil {
			continue
		}
		if item.Attempts > 0 {
			item.SuccessRate = float64(item.Succeeded) / float64(item.Attempts)
		}
		out[fmt.Sprintf("%s|%s", item.Provider, item.CountryISO)] = item
	}
	return out
}

// ReplacePriceSnapshots atomically replaces one provider's latest catalog. Network work
// is completed before this method is called, keeping the write transaction short.
func (s *SMSStats) ReplacePriceSnapshots(ctx context.Context, provider, service string, items []SMSPriceSnapshot, fetchedAt int64) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sms_country_price_snapshots WHERE provider=? AND service=?`, provider, service); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO sms_country_price_snapshots(
 provider,service,country_id,country_iso,country_name,price,inventory,provider_rank,balance,fetched_at
) VALUES(?,?,?,?,?,?,?,?,?,?)`, provider, service, item.CountryID, item.CountryISO, item.CountryName,
			item.Price, item.Inventory, item.Rank, item.Balance, fetchedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PriceSnapshots returns observations newer than maxAge. maxAge<=0 returns all rows.
func (s *SMSStats) PriceSnapshots(ctx context.Context, maxAge time.Duration) []SMSPriceSnapshot {
	out := []SMSPriceSnapshot{}
	if s == nil || s.db == nil {
		return out
	}
	cutoff := int64(0)
	if maxAge > 0 {
		cutoff = time.Now().Add(-maxAge).Unix()
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT provider,service,country_id,country_iso,country_name,price,inventory,provider_rank,balance,fetched_at
FROM sms_country_price_snapshots WHERE fetched_at >= ?
ORDER BY price ASC, provider_rank ASC, provider ASC, country_iso ASC`, cutoff)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var item SMSPriceSnapshot
		if rows.Scan(&item.Provider, &item.Service, &item.CountryID, &item.CountryISO, &item.CountryName,
			&item.Price, &item.Inventory, &item.Rank, &item.Balance, &item.FetchedAt) == nil {
			out = append(out, item)
		}
	}
	return out
}
