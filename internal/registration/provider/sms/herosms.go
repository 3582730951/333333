package sms

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// heroSMSBase is the live hero-sms.com legacy handler endpoint. The API authenticates
// with an "api_key" query param (NOT "token") and uses the SMS-Activate-style
// ACCESS_*/STATUS_* text protocol. service "dr" == OpenAI (NOT "ot" which is "Any other").
const heroSMSBase = "https://hero-sms.com/stubs/handler_api.php"

// heroCountryID maps common ISO-ish codes to hero-sms numeric country IDs. hero-sms
// requires a numeric country, so an unmapped value is passed through as-is (callers may
// already supply a number) and a blank one defaults to Philippines (4, cheapest at $0.025).
// IDs verified live via hero-sms getCountries (2026-06-27): BR=73, CO=33, PL=15, etc.
// SMSBower shares the same IDs (SMS-Activate-compatible). Pricing (service "dr"=OpenAI):
// BR=$0.045, CO=?, PL=?, PH=$0.025, ID=$0.045, CL=$0.10, ZA=$0.10, TH=$0.30.
func heroCountryID(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "", "PH":
		return "4" // Philippines $0.025 ⭐ cheapest
	case "ID":
		return "6" // Indonesia $0.045
	case "PL":
		return "15" // Poland
	case "UK", "GB":
		return "16" // UK $0.03 (often no numbers)
	case "IN":
		return "22" // India $0.35
	case "ZA":
		return "27" // South Africa $0.10
	case "CO":
		return "33" // Colombia
	case "TH":
		return "52" // Thailand $0.30
	case "CL":
		return "56" // Chile $0.10
	case "BR":
		return "73" // Brazil $0.045
	default:
		return code // assume already numeric
	}
}

// HeroSMSProvider implements the hero-sms.com API
type HeroSMSProvider struct {
	apiKey     string
	service    string // SMS service code; defaults to "ot" (OpenAI)
	httpClient *http.Client
}

// NewHeroSMSProvider creates a HeroSMS provider
func NewHeroSMSProvider(apiKey string, httpClient *http.Client) *HeroSMSProvider {
	return &HeroSMSProvider{
		apiKey:     apiKey,
		service:    "dr",
		httpClient: httpClient,
	}
}

func (p *HeroSMSProvider) Name() string { return "herosms" }
func (p *HeroSMSProvider) Type() string { return "sms" }

// GetNumber purchases a phone number
func (p *HeroSMSProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	svc := p.service
	if svc == "" {
		svc = "ot"
	}
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "getNumber")
	params.Set("service", svc)
	params.Set("country", heroCountryID(country))

	req, _ := http.NewRequestWithContext(ctx, "GET",
		heroSMSBase+"?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body := readSMSProviderBody(resp.Body)
	result := string(body)

	// HeroSMS response format: ACCESS_NUMBER:orderId:phone
	if len(result) < 6 || result[:6] != "ACCESS" {
		return "", "", fmt.Errorf("herosms: %s", result)
	}

	// Parse: ACCESS_NUMBER:123456789:+6281234567890
	parts := splitN(result, ":", 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("herosms: invalid response format")
	}

	return parts[2], parts[1], nil
}

// WaitCode polls for SMS code
func (p *HeroSMSProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for SMS")
			}

			params := url.Values{}
			params.Set("api_key", p.apiKey)
			params.Set("action", "getStatus")
			params.Set("id", orderID)

			req, _ := http.NewRequestWithContext(ctx, "GET",
				heroSMSBase+"?"+params.Encode(), nil)
			resp, err := p.httpClient.Do(req)
			if err != nil {
				continue
			}

			body := readSMSProviderBody(resp.Body)
			resp.Body.Close()
			result := string(body)

			// Response format: STATUS_OK:code or STATUS_WAIT_CODE
			if len(result) > 10 && result[:9] == "STATUS_OK" {
				parts := splitN(result, ":", 2)
				if len(parts) == 2 {
					return parts[1], nil
				}
			}
		}
	}
}

// CancelNumber cancels the order
func (p *HeroSMSProvider) CancelNumber(ctx context.Context, orderID string) error {
	return p.setStatus(ctx, orderID, "8")
}

// CompleteNumber marks a consumed activation complete (SMS-Activate status 6).
func (p *HeroSMSProvider) CompleteNumber(ctx context.Context, orderID string) error {
	return p.setStatus(ctx, orderID, "6")
}

func (p *HeroSMSProvider) setStatus(ctx context.Context, orderID, status string) error {
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "setStatus")
	params.Set("id", orderID)
	params.Set("status", status)

	req, _ := http.NewRequestWithContext(ctx, "GET",
		heroSMSBase+"?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := strings.TrimSpace(string(readSMSProviderBody(resp.Body)))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("herosms setStatus: HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(body, "ACCESS_") {
		return nil
	}
	return fmt.Errorf("herosms setStatus rejected")
}

// GetBalance returns the hero-sms account balance. hero-sms may return the bare
// "ACCESS_BALANCE:<n>" text form or a JSON error on a bad key; both are handled.
func (p *HeroSMSProvider) GetBalance(ctx context.Context) (float64, error) {
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "getBalance")
	req, _ := http.NewRequestWithContext(ctx, "GET", heroSMSBase+"?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body := readSMSProviderBody(resp.Body)
	result := strings.TrimSpace(string(body))
	if strings.HasPrefix(result, "ACCESS_BALANCE:") {
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(result, "ACCESS_BALANCE:")), 64)
		if err != nil {
			return 0, fmt.Errorf("herosms getBalance: parse %q: %w", truncate(result, 120), err)
		}
		return f, nil
	}
	return 0, fmt.Errorf("herosms getBalance: %s", truncate(result, 120))
}

// GetCountries returns the hero-sms country catalog. Response is JSON keyed by numeric id.
func (p *HeroSMSProvider) GetCountries(ctx context.Context) ([]CountryInfo, error) {
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "getCountries")
	req, _ := http.NewRequestWithContext(ctx, "GET", heroSMSBase+"?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := readSMSProviderBody(resp.Body)
	return parseSMSBowerCountries(string(body))
}

// GetTopCountries returns the success-ranked, priced country list for the given service.
// Uses hero-sms's getTopCountriesByServiceRank (the platform's own success-priority order),
// falling back to getTopCountriesByService.
func (p *HeroSMSProvider) GetTopCountries(ctx context.Context, service string) ([]CountryPrice, error) {
	svc := strings.TrimSpace(service)
	if svc == "" {
		svc = p.service
	}
	// Try the hero-sms-specific rank endpoint first, then the generic one.
	var lastErr error
	for _, action := range []string{"getTopCountriesByServiceRank", "getTopCountriesByService"} {
		params := url.Values{}
		params.Set("api_key", p.apiKey)
		params.Set("action", action)
		params.Set("service", svc)
		req, _ := http.NewRequestWithContext(ctx, "GET", heroSMSBase+"?"+params.Encode(), nil)
		resp, err := p.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body := readSMSProviderBody(resp.Body)
		resp.Body.Close()
		raw := string(body)
		ranks, err := parseSMSBowerTopCountries(raw)
		if err != nil {
			lastErr = err
			continue
		}
		if len(ranks) > 0 {
			return ranks, nil
		}
		lastErr = fmt.Errorf("herosms getTopCountries: empty result")
	}
	return nil, lastErr
}

// strconv 和 truncate 在 smsbower.go 中定义 (同包共享)

// SMSActivateProvider implements SMS-Activate API
type SMSActivateProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewSMSActivateProvider creates an SMS-Activate provider
func NewSMSActivateProvider(apiKey string, httpClient *http.Client) *SMSActivateProvider {
	return &SMSActivateProvider{
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

func (p *SMSActivateProvider) Name() string { return "sms_activate" }
func (p *SMSActivateProvider) Type() string { return "sms" }

// GetNumber purchases a phone number
func (p *SMSActivateProvider) GetNumber(ctx context.Context, country string) (string, string, error) {
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "getNumber")
	params.Set("service", "openai")
	params.Set("country", countryCodeToID(country))

	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://api.sms-activate.org/stubs/handler_api.php?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	body := readSMSProviderBody(resp.Body)
	result := string(body)

	// Response format: ACCESS_NUMBER:orderId:phone
	if len(result) < 6 || result[:6] != "ACCESS" {
		return "", "", fmt.Errorf("sms-activate: %s", result)
	}

	parts := splitN(result, ":", 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("sms-activate: invalid response")
	}

	return parts[2], parts[1], nil
}

// WaitCode polls for SMS code
func (p *SMSActivateProvider) WaitCode(ctx context.Context, orderID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("timeout waiting for SMS")
			}

			params := url.Values{}
			params.Set("api_key", p.apiKey)
			params.Set("action", "getStatus")
			params.Set("id", orderID)

			req, _ := http.NewRequestWithContext(ctx, "GET",
				"https://api.sms-activate.org/stubs/handler_api.php?"+params.Encode(), nil)
			resp, err := p.httpClient.Do(req)
			if err != nil {
				continue
			}

			body := readSMSProviderBody(resp.Body)
			resp.Body.Close()
			result := string(body)

			if len(result) > 10 && result[:9] == "STATUS_OK" {
				parts := splitN(result, ":", 2)
				if len(parts) == 2 {
					return parts[1], nil
				}
			}
		}
	}
}

// CancelNumber cancels the order
func (p *SMSActivateProvider) CancelNumber(ctx context.Context, orderID string) error {
	return p.setStatus(ctx, orderID, "8")
}

// CompleteNumber marks a consumed activation complete (SMS-Activate status 6).
func (p *SMSActivateProvider) CompleteNumber(ctx context.Context, orderID string) error {
	return p.setStatus(ctx, orderID, "6")
}

func (p *SMSActivateProvider) setStatus(ctx context.Context, orderID, status string) error {
	params := url.Values{}
	params.Set("api_key", p.apiKey)
	params.Set("action", "setStatus")
	params.Set("id", orderID)
	params.Set("status", status)

	req, _ := http.NewRequestWithContext(ctx, "GET",
		"https://api.sms-activate.org/stubs/handler_api.php?"+params.Encode(), nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := strings.TrimSpace(string(readSMSProviderBody(resp.Body)))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("sms-activate setStatus: HTTP %d", resp.StatusCode)
	}
	if strings.HasPrefix(body, "ACCESS_") {
		return nil
	}
	return fmt.Errorf("sms-activate setStatus rejected")
}

func splitN(s, sep string, n int) []string {
	result := make([]string, 0, n)
	start := 0
	for i := 0; i < n-1; i++ {
		idx := indexFrom(s, sep, start)
		if idx == -1 {
			break
		}
		result = append(result, s[start:idx])
		start = idx + len(sep)
	}
	result = append(result, s[start:])
	return result
}

func indexFrom(s, substr string, start int) int {
	if start >= len(s) {
		return -1
	}
	idx := 0
	for i := start; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
		idx++
	}
	return -1
}

func countryCodeToID(code string) string {
	// SMS-Activate uses numeric country IDs
	mapping := map[string]string{
		"ID": "6",  // Indonesia
		"PH": "4",  // Philippines
		"VN": "10", // Vietnam
		"MY": "7",  // Malaysia
		"IN": "22", // India
	}
	if id, ok := mapping[code]; ok {
		return id
	}
	return "6" // default to Indonesia
}
