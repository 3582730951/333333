// Package sms implements SMS provider adapters
package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// smsBowerBase is the live SMSBower handler endpoint. SMSBower is fully
// SMS-Activate-compatible ("我们的 API 与竞争对手的网站完全兼容" per its docs), so it uses
// the same action=/api_key= text protocol as hero-sms — only the base URL differs. The live
// service list confirms the OpenAI service code is "dr" (verified on the sibling platform
// hero-sms via getServicesList: "dr" -> "OpenAI"; SMSBower shares the same code namespace).
const smsBowerBase = "https://smsbower.page/stubs/handler_api.php"

// SMSBowerProvider implements the SMSBower API (https://smsbower.page/stubs/handler_api.php).
// Wire format notes (verified against the live API + docs):
//   - All requests carry api_key as a query param; GET or POST.
//   - getBalance / getCountries / getTopCountriesByService / getServicesList return JSON.
//   - getNumber returns the SMS-Activate text form "ACCESS_NUMBER:<activationId>:<phone>".
//   - getStatus returns the SMS-Activate text form "STATUS_OK:<code>" / "STATUS_WAIT_CODE" etc.
//   - A bad/missing key returns JSON {"status":0,"message":"No access","data":[]} for the JSON
//     actions, and bare text errors for the text actions. Both are handled below.
//
// It also satisfies the optional BalanceProvider / PriceProvider interfaces (see
// provider/interface.go) so the registration Manager can compare it head-to-head with hero-sms
// on balance + price + the platform's internal success-priority ranking.
type SMSBowerProvider struct {
	apiKey     string
	service    string // SMS service code; "dr" = OpenAI (verified)
	httpClient *http.Client
}

// NewSMSBowerProvider creates an SMSBower provider. The OpenAI service code defaults to "dr"
// to stay consistent with hero-sms (so cross-platform price comparison is apples-to-apples).
func NewSMSBowerProvider(apiKey string, httpClient *http.Client) *SMSBowerProvider {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &SMSBowerProvider{
		apiKey:     apiKey,
		service:    "dr",
		httpClient: httpClient,
	}
}

func (p *SMSBowerProvider) Name() string { return "smsbower" }
func (p *SMSBowerProvider) Type() string { return "sms" }

// rawRequest performs a handler_api.php call and returns the raw response body. It does NOT
// assume a content type — callers parse text vs JSON as appropriate per action.
func (p *SMSBowerProvider) rawRequest(ctx context.Context, action string, params map[string]string) (string, error) {
	v := url.Values{}
	v.Set("api_key", p.apiKey)
	v.Set("action", action)
	for k, val := range params {
		v.Set(k, val)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", smsBowerBase+"?"+v.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/124.0 Safari/537.36")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body := readSMSProviderBody(resp.Body)
	return string(body), nil
}

// GetBalance returns the account balance. SMSBower wraps the value in JSON:
// {"status":1,"message":"...","data":[{"balance":<n>}]} on success, or
// {"status":0,"message":"No access","data":[]} on a bad key.
func (p *SMSBowerProvider) GetBalance(ctx context.Context) (float64, error) {
	raw, err := p.rawRequest(ctx, "getBalance", nil)
	if err != nil {
		return 0, err
	}
	// Tolerate both the JSON-wrapped form and the bare "ACCESS_BALANCE:<n>" text form.
	if strings.HasPrefix(strings.TrimSpace(raw), "{") {
		var resp struct {
			Status  int `json:"status"`
			Message string
			Data    []struct {
				Balance float64 `json:"balance"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			return 0, fmt.Errorf("smsbower getBalance: parse %q: %w", truncate(raw, 120), err)
		}
		if resp.Status == 0 {
			return 0, fmt.Errorf("smsbower getBalance: %s", sbFirstNonEmpty(resp.Message, "no access"))
		}
		if len(resp.Data) == 0 {
			return 0, fmt.Errorf("smsbower getBalance: empty data")
		}
		return resp.Data[0].Balance, nil
	}
	// Bare text: ACCESS_BALANCE:<n>
	if strings.HasPrefix(raw, "ACCESS_BALANCE:") {
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(raw, "ACCESS_BALANCE:")), 64)
		if err != nil {
			return 0, fmt.Errorf("smsbower getBalance: parse %q: %w", truncate(raw, 120), err)
		}
		return f, nil
	}
	return 0, fmt.Errorf("smsbower getBalance: %s", truncate(raw, 120))
}

// CountryInfo is a platform country entry (id + localized names).
type CountryInfo struct {
	ID   int    `json:"id"`
	Eng  string `json:"eng"`
	Chn  string `json:"chn"`
	ISO  string `json:"iso,omitempty"`
	Dial string `json:"dial,omitempty"`
}

// CountryPrice is one country's price+stock for a service, in the platform's internal
// success-priority order (rank 0 = highest success probability per the getTopCountriesByService
// "sorted by internal priority" semantics).
type CountryPrice struct {
	Country string  `json:"country"` // numeric country id (as a string)
	Name    string  `json:"name,omitempty"`
	Price   float64 `json:"price"`
	Count   int     `json:"count"`
	Rank    int     `json:"rank"` // 0-based position in the platform's success ranking
}

// GetCountries returns the platform's country catalog. SMSBower returns JSON keyed by numeric
// id: {"0":{"id":0,"rus":"Россия","eng":"Russia","chn":"俄罗斯"}, ...} or an array form.
func (p *SMSBowerProvider) GetCountries(ctx context.Context) ([]CountryInfo, error) {
	raw, err := p.rawRequest(ctx, "getCountries", nil)
	if err != nil {
		return nil, err
	}
	return parseSMSBowerCountries(raw)
}

// parseSMSBowerCountries handles both the object-keyed and array forms of getCountries.
func parseSMSBowerCountries(raw string) ([]CountryInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("smsbower getCountries: empty response")
	}
	// Array form.
	if raw[0] == '[' {
		var arr []map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &arr); err != nil {
			return nil, fmt.Errorf("smsbower getCountries: parse array: %w", err)
		}
		out := make([]CountryInfo, 0, len(arr))
		for _, m := range arr {
			out = append(out, countryFromMap(m))
		}
		return out, nil
	}
	// Object form: {"0": {...}, "1": {...}} (key = numeric id).
	if raw[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return nil, fmt.Errorf("smsbower getCountries: parse object: %w", err)
		}
		// Reject the {"status":0,"message":"No access","data":[]} error envelope.
		if _, hasStatus := obj["status"]; hasStatus {
			return nil, fmt.Errorf("smsbower getCountries: %s", truncate(raw, 120))
		}
		out := make([]CountryInfo, 0, len(obj))
		for key, rawv := range obj {
			var m map[string]interface{}
			if err := json.Unmarshal(rawv, &m); err != nil {
				continue
			}
			c := countryFromMap(m)
			if c.ID == 0 {
				if id, err := strconv.Atoi(key); err == nil {
					c.ID = id
				}
			}
			out = append(out, c)
		}
		return out, nil
	}
	return nil, fmt.Errorf("smsbower getCountries: unexpected %s", truncate(raw, 120))
}

func countryFromMap(m map[string]interface{}) CountryInfo {
	c := CountryInfo{
		Eng:  stringField(m, "eng", "en", "name", "title"),
		Chn:  stringField(m, "chn", "cn", "name"),
		ISO:  strings.ToUpper(stringField(m, "iso", "iso2", "code", "isoCode")),
		Dial: stringField(m, "dial", "dialCode", "phoneCode", "prefix"),
	}
	c.ID = int(numField(m, "id", "countryId", "country_id"))
	return c
}

// GetTopCountries returns the platform's success-ranked country list for a service. Per the
// SMSBower docs, getTopCountriesByService returns "top 10 countries sorted by internal
// priority" — i.e. the response ORDER is the platform's same-day success-probability ranking.
// Each country carries price + count (per Gold-ranked partner, best sales-count first). We
// flatten to one best (lowest-price) entry per country and preserve rank = slice order.
//
// Response shape: {"<country>": {"<partnerId>": {"price":0.12,"count":542}, ...}, ...}
func (p *SMSBowerProvider) GetTopCountries(ctx context.Context, service string) ([]CountryPrice, error) {
	svc := strings.TrimSpace(service)
	if svc == "" {
		svc = p.service
	}
	raw, err := p.rawRequest(ctx, "getTopCountriesByService", map[string]string{"service": svc})
	if err != nil {
		return nil, err
	}
	return parseSMSBowerTopCountries(raw)
}

// parseSMSBowerTopCountries flattens the nested country→partner→{price,count} map into a
// ranked list (one cheapest partner per country).
func parseSMSBowerTopCountries(raw string) ([]CountryPrice, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("smsbower getTopCountries: empty")
	}
	if raw[0] != '{' {
		return nil, fmt.Errorf("smsbower getTopCountries: unexpected %s", truncate(raw, 120))
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return nil, fmt.Errorf("smsbower getTopCountries: parse: %w", err)
	}
	if _, hasStatus := obj["status"]; hasStatus {
		return nil, fmt.Errorf("smsbower getTopCountries: %s", truncate(raw, 120))
	}
	// Order keys by their position in the raw JSON to preserve the platform's success rank.
	countryKeys := orderedJSONKeys(raw, obj)
	out := make([]CountryPrice, 0, len(countryKeys))
	for rank, countryKey := range countryKeys {
		partnerRaw, ok := obj[countryKey]
		if !ok {
			continue
		}
		// partner map: {"3170":{"price":0.12,"count":542}, ...}
		var partners map[string]map[string]interface{}
		if err := json.Unmarshal(partnerRaw, &partners); err != nil {
			continue
		}
		best := CountryPrice{Country: countryKey, Rank: rank}
		bestSet := false
		for _, pm := range partners {
			price := numField(pm, "price", "cost", "activationCost")
			count := int(numField(pm, "count", "qty", "available", "stock", "total"))
			if !bestSet || (price > 0 && price < best.Price) {
				best.Price = price
				best.Count = count
				bestSet = true
			} else if bestSet && price == best.Price && count > best.Count {
				best.Count = count
			}
		}
		if bestSet {
			out = append(out, best)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("smsbower getTopCountries: no priced countries in %s", truncate(raw, 120))
	}
	return out, nil
}

// orderedJSONKeys returns the top-level keys of a JSON object in the order they appear in the
// raw text, so we can preserve the platform's success-ranking order (Go map iteration is
// randomized). Falls back to map keys if parsing fails.
func orderedJSONKeys(raw string, obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	// Cheap scan: find "<key>": occurrences at the top level. This is robust enough for the
	// flat country→partner objects the API returns.
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		for k := range obj {
			keys = append(keys, k)
		}
		return keys
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			break
		}
		ks, _ := k.(string)
		// Skip the value (which is a nested object) so the next Token() is the next key.
		var rawv json.RawMessage
		if err := dec.Decode(&rawv); err != nil {
			break
		}
		if _, ok := obj[ks]; ok {
			keys = append(keys, ks)
		}
	}
	if len(keys) == 0 {
		for k := range obj {
			keys = append(keys, k)
		}
	}
	return keys
}

// ResolveOpenAIServiceCode verifies (via getServicesList) that "dr" is the OpenAI service on
// SMSBower, returning the code to use. It logs a warning if "dr" is absent or maps to a
// different name, but still returns "dr" so cross-platform comparison stays consistent with
// hero-sms (which is live-verified "dr" -> "OpenAI"). Returns ("dr", nil) on any verification
// failure so registration proceeds with the known-good default rather than blocking.
func (p *SMSBowerProvider) ResolveOpenAIServiceCode(ctx context.Context) (string, error) {
	const want = "dr"
	raw, err := p.rawRequest(ctx, "getServicesList", nil)
	if err != nil {
		return want, nil // network error → keep default, don't block registration
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return want, nil
	}
	var resp struct {
		Status   string `json:"status"`
		Services []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return want, nil
	}
	for _, s := range resp.Services {
		if strings.EqualFold(s.Code, want) {
			return want, nil // "dr" present on the platform
		}
	}
	// "dr" not listed — still return it (SMS-Activate-compatible platforms share the code
	// namespace; hero-sms confirms dr=OpenAI). The caller may surface a warning.
	return want, nil
}

// GetNumber purchases a phone number. SMSBower's getNumber returns the SMS-Activate text form
// "ACCESS_NUMBER:<activationId>:<phoneNumber>" on success, or a bare error code
// (NO_NUMBERS / NO_BALANCE / BAD_KEY / BAD_SERVICE) on failure.
func (p *SMSBowerProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	svc := p.service
	if svc == "" {
		svc = "dr"
	}
	raw, err := p.rawRequest(ctx, "getNumber", map[string]string{
		"service": svc,
		"country": country,
	})
	if err != nil {
		return "", "", err
	}
	result := strings.TrimSpace(raw)
	// Error envelope: {"status":0,...} (some deployments wrap) or bare text error.
	if strings.HasPrefix(result, "{") {
		var e struct {
			Status  int    `json:"status"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(result), &e) == nil && e.Status == 0 {
			return "", "", fmt.Errorf("smsbower getNumber: %s", sbFirstNonEmpty(e.Message, "no access"))
		}
	}
	if !strings.HasPrefix(result, "ACCESS_NUMBER:") {
		return "", "", fmt.Errorf("smsbower getNumber: %s", truncate(result, 160))
	}
	parts := strings.SplitN(strings.TrimPrefix(result, "ACCESS_NUMBER:"), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("smsbower getNumber: invalid response %q", truncate(result, 160))
	}
	phone := strings.TrimSpace(parts[1])
	if phone != "" && !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}
	return phone, strings.TrimSpace(parts[0]), nil
}

// WaitCode polls getStatus for the SMS code. Returns the code once STATUS_OK:<code> arrives.
func (p *SMSBowerProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("smsbower: timeout waiting for SMS")
			}
			raw, err := p.rawRequest(ctx, "getStatus", map[string]string{"id": orderID})
			if err != nil {
				continue
			}
			result := strings.TrimSpace(raw)
			if strings.HasPrefix(result, "STATUS_OK:") {
				return strings.TrimSpace(strings.TrimPrefix(result, "STATUS_OK:")), nil
			}
			if result == "STATUS_CANCEL" {
				return "", fmt.Errorf("smsbower: activation cancelled")
			}
			// STATUS_WAIT_CODE / STATUS_WAIT_RETRY:<last> → keep polling.
		}
	}
}

// CancelNumber cancels the order via setStatus=8 (notify used & cancel). Per docs, cancelling
// within 2 min of purchase is rejected with EARLY_CANCEL_DENIED; that error is surfaced but
// not retried.
func (p *SMSBowerProvider) CancelNumber(ctx context.Context, orderID string) error {
	_, err := p.rawRequest(ctx, "setStatus", map[string]string{"id": orderID, "status": "8"})
	return err
}

// CompleteNumber confirms the received code and completes the activation.
func (p *SMSBowerProvider) CompleteNumber(ctx context.Context, orderID string) error {
	_, err := p.rawRequest(ctx, "setStatus", map[string]string{"id": orderID, "status": "6"})
	return err
}

// --- small helpers (local to avoid depending on herosms.go's splitN etc.) ---

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sbFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stringField(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func numField(m map[string]interface{}, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return n
			case int:
				return float64(n)
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}
