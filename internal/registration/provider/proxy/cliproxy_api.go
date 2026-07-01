// cliproxy_api.go adds CLIPProxy API-based dynamic-IP extraction and exit-region
// validation on top of the existing sid-rotation scheme. Two purposes:
//
//  1. API whitelist mode (proxy_auth_mode="api_whitelist"): call api.cliproxy.io/white/api
//     to get ip:port list, use them directly as unauthenticated proxies (the API whitelists
//     the VPS public IP). The IP is pinned to a region (region=BR) so the exit country
//     matches the SMS number's country.
//  2. Exit-region validation (both modes): after building a proxy URL (sid-rotated or
//     API-extracted), probe the actual exit IP's country via a geo endpoint to CONFIRM it
//     matches the SMS country. cliproxy may route a region-BR sid to a neighbouring country
//     when BR inventory is low — that geo-mismatch is what gets the SMS withheld by OpenAI.
//     Validating catches it and lets the caller rotate to a fresh sid/IP.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CliproxyAPIExtractor calls the cliproxy.io white-api to extract dynamic residential IPs.
type CliproxyAPIExtractor struct {
	BaseURL string      // default https://api.cliproxy.io
	APIKey  string      // cliproxy account API token (may be empty — some endpoints are keyless)
	HC      *http.Client
}

// ipCacheEntry caches one extracted ip:port per region with its expiry.
type ipCacheEntry struct {
	ip       string
	expiresAt time.Time
}

var (
	ipCacheMu sync.Mutex
	ipCache   = map[string]ipCacheEntry{} // key = region
)

// ExtractIPs calls GET {BaseURL}/white/api?region=&num=&time=&format=n&type=txt and returns
// the ip:port list. region is the country (BR/CO/... or Rand). num is how many IPs. time is
// the rotation duration in minutes (the IP is sticky for that long). Returns at most num IPs.
func (e *CliproxyAPIExtractor) ExtractIPs(ctx context.Context, region string, num int, timeMin int) ([]string, error) {
	if e == nil || e.BaseURL == "" {
		return nil, fmt.Errorf("cliproxy api: base url not configured")
	}
	if num < 1 {
		num = 1
	}
	q := url.Values{}
	q.Set("region", region)
	q.Set("num", fmt.Sprintf("%d", num))
	if timeMin > 0 {
		q.Set("time", fmt.Sprintf("%d", timeMin))
	}
	q.Set("format", "n")
	q.Set("type", "txt")
	endpoint := strings.TrimRight(e.BaseURL, "/") + "/white/api?" + q.Encode()
	hc := e.HC
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cliproxy api: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cliproxy api: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil, fmt.Errorf("cliproxy api: empty response")
	}
	// txt format with format=n: one ip:port per line.
	var ips []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			ips = append(ips, line)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("cliproxy api: no ips in response: %s", text)
	}
	return ips, nil
}

// CachedIP returns a cached ip:port for region if still fresh, else extracts a new one.
// The cache TTL is timeMin minutes minus a 1-minute safety margin. This lets concurrent
// registrations in the same region share an IP within its sticky window instead of each
// calling the API.
func (e *CliproxyAPIExtractor) CachedIP(ctx context.Context, region string, num, timeMin int) (string, error) {
	if timeMin <= 0 {
		timeMin = 10
	}
	ipCacheMu.Lock()
	if ent, ok := ipCache[region]; ok && time.Now().Before(ent.expiresAt) {
		ipCacheMu.Unlock()
		return ent.ip, nil
	}
	ipCacheMu.Unlock()

	ips, err := e.ExtractIPs(ctx, region, num, timeMin)
	if err != nil {
		return "", err
	}
	ip := ips[0]
	ttl := time.Duration(timeMin-1) * time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	ipCacheMu.Lock()
	ipCache[region] = ipCacheEntry{ip: ip, expiresAt: time.Now().Add(ttl)}
	ipCacheMu.Unlock()
	return ip, nil
}

// geoResult is the parsed response from a geo-lookup endpoint (ip-api.com/json style).
type geoResult struct {
	CountryCode string `json:"countryCode"`
	Country     string `json:"country"`
}

// ValidateExitRegion confirms that traffic through proxyURL actually exits from
// expectedRegionISO (e.g. "BR"). It routes a request to a geo-lookup endpoint THROUGH the
// proxy and checks the returned country code. Returns true on match. A 1-2s timeout keeps
// this from blocking registration; on any error it returns true (optimistic) so a transient
// geo-endpoint failure doesn't abort a registration that may be fine.
func ValidateExitRegion(parentCtx context.Context, proxyURL, expectedRegionISO string) bool {
	expected := strings.ToUpper(strings.TrimSpace(expectedRegionISO))
	if expected == "" || expected == "RAND" {
		return true // no constraint to validate
	}
	ctx, cancel := context.WithTimeout(parentCtx, 8*time.Second)
	defer cancel()
	// Use the geo endpoint that accepts a proxy and returns JSON.
	geoURL := "http://ip-api.com/json/?fields=countryCode"
	hc := &http.Client{Timeout: 7 * time.Second}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil && u.Host != "" {
			hc.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, geoURL, nil)
	if err != nil {
		return true
	}
	resp, err := hc.Do(req)
	if err != nil {
		return true // optimistic — geo probe failed, don't block registration
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode != 200 {
		return true
	}
	var g geoResult
	if err := json.Unmarshal(body, &g); err != nil {
		return true
	}
	return strings.ToUpper(g.CountryCode) == expected
}

// InvalidateRegionCache drops the cached ip:port for a region so the next CachedIP call
// extracts fresh. Called when ValidateExitRegion fails for an API-mode IP.
func InvalidateRegionCache(region string) {
	ipCacheMu.Lock()
	delete(ipCache, strings.ToUpper(region))
	ipCacheMu.Unlock()
}
