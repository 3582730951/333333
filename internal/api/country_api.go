package api

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed phone_countries.json
var phoneCountriesFS embed.FS

// phoneCountry is one entry of the static ISO/dial/中英文 catalog served to the registration
// page's searchable country Select. Mirrors other_new_gpt_register/src/phoneCountryCatalog.js.
type phoneCountry struct {
	ISOCode  string `json:"isoCode"`
	DialCode string `json:"dialCode"`
	Name     string `json:"name"`
	NameZh   string `json:"nameZh"`
}

// cachedPhoneCountries is parsed once on first request.
var cachedPhoneCountries []phoneCountry
var phoneCountriesParsed bool
var phoneCountriesMu sync.RWMutex

func loadPhoneCountries() ([]phoneCountry, error) {
	phoneCountriesMu.RLock()
	if phoneCountriesParsed {
		out := append([]phoneCountry(nil), cachedPhoneCountries...)
		phoneCountriesMu.RUnlock()
		return out, nil
	}
	phoneCountriesMu.RUnlock()

	phoneCountriesMu.Lock()
	defer phoneCountriesMu.Unlock()
	if !phoneCountriesParsed {
		raw, err := phoneCountriesFS.ReadFile("phone_countries.json")
		if err != nil {
			return nil, err
		}
		var list []phoneCountry
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		cachedPhoneCountries = list
		phoneCountriesParsed = true
	}
	return append([]phoneCountry(nil), cachedPhoneCountries...), nil
}

func normalizePhoneCountryISO(value string, allowEmpty bool) (string, error) {
	iso := strings.ToUpper(strings.TrimSpace(value))
	if iso == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("expected ISO-2 country code")
	}
	countries, err := loadPhoneCountries()
	if err != nil {
		return "", err
	}
	for _, c := range countries {
		if strings.EqualFold(c.ISOCode, iso) {
			return strings.ToUpper(c.ISOCode), nil
		}
	}
	return "", fmt.Errorf("unknown ISO-2 country %q", iso)
}

func normalizePhoneCountryCSV(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		iso, err := normalizePhoneCountryISO(part, true)
		if err != nil {
			return "", err
		}
		if iso == "" || seen[iso] {
			continue
		}
		seen[iso] = true
		out = append(out, iso)
	}
	return strings.Join(out, ","), nil
}

// adminRegisterCountries returns the static phone-country catalog (ISO code + dial code +
// English + Chinese name) so the Registration page can render a searchable Select for the
// "manual country" mode. No per-request parsing — the JSON is embedded and parsed once.
//
//	GET /admin/register/countries  -> [{isoCode,dialCode,name,nameZh}, ...]
func (s *Server) adminRegisterCountries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	// Not admin-gated: the catalog is static public data (no secrets), and the registration
	// page needs it before the admin token may be set in some flows. Still fine to gate if
	// desired — left open for UX.
	list, err := loadPhoneCountries()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// adminRegisterStatsDaily aggregates registration_records by date + SMS provider + SMS
// country, returning per-group total/succeeded/failed/success_rate/avg_cost. This is the
// local success-rate signal that complements the platforms' own getTopCountriesByService
// ranking — surfaced so the operator (and a future selection-algorithm refinement) can see
// which (provider, country) actually converts over time.
//
//	GET /admin/register/stats/daily?days=14  -> [{date,provider,country,total,succeeded,failed,success_rate,avg_cost}, ...]
func (s *Server) adminRegisterStatsDaily(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	days := 14
	if d := r.URL.Query().Get("days"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	// sms_provider/sms_country were added by migration; guard against older schemas where the
	// columns are absent so the endpoint degrades gracefully rather than 500-ing.
	cutoff := time.Now().Unix() - int64(days*24*3600)
	rows, err := s.store.DB().QueryContext(r.Context(), `
		SELECT date(created_at,'unixepoch') AS d, sms_provider, sms_country,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status IN ('success','succeeded') THEN 1 ELSE 0 END) AS succ,
		       SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) AS fail,
		       AVG(cost_usd) AS avg_cost
		FROM registration_records
		WHERE created_at >= ? AND sms_provider IS NOT NULL
		GROUP BY d, sms_provider, sms_country
		ORDER BY d DESC`, cutoff)
	if err != nil {
		// Columns likely missing on an older DB — return an empty list, not an error.
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	defer rows.Close()
	type row struct {
		Date        string  `json:"date"`
		Provider    string  `json:"provider"`
		Country     string  `json:"country"`
		Total       int     `json:"total"`
		Succeeded   int     `json:"succeeded"`
		Failed      int     `json:"failed"`
		SuccessRate float64 `json:"success_rate"`
		AvgCost     float64 `json:"avg_cost"`
	}
	out := []row{}
	for rows.Next() {
		var r row
		var avg float64
		if err := rows.Scan(&r.Date, &r.Provider, &r.Country, &r.Total, &r.Succeeded, &r.Failed, &avg); err != nil {
			continue
		}
		r.AvgCost = avg
		if r.Total > 0 {
			r.SuccessRate = float64(r.Succeeded) / float64(r.Total)
		}
		out = append(out, r)
	}
	writeJSON(w, http.StatusOK, out)
}

// adminRegisterSMSMarket exposes the same evidence used by automatic country selection.
// GET is side-effect free; POST performs an immediate comparison scan before returning.
func (s *Server) adminRegisterSMSMarket(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(w, r) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.regHandler == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("registration pipeline is not initialized"))
		return
	}
	refreshed := 0
	warning := ""
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		var err error
		refreshed, err = s.regHandler.refreshSMSPrices(ctx)
		if err != nil {
			warning = err.Error()
		}
	}
	items, minPrice, maxPrice, preferred := s.regHandler.smsMarketSnapshot(r.Context())
	latest := int64(0)
	for _, item := range items {
		if item.FetchedAt > latest {
			latest = item.FetchedAt
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":                    items,
		"min_price":                minPrice,
		"max_price":                maxPrice,
		"preferred_countries":      preferred,
		"cold_start_policy":        "community_recommended_order",
		"history_window_days":      14,
		"minimum_history_samples":  3,
		"refresh_interval_seconds": 3600,
		"last_refreshed_at":        latest,
		"stale":                    latest == 0 || time.Now().Unix()-latest > 5400,
		"refreshed_rows":           refreshed,
		"warning":                  warning,
	})
}
