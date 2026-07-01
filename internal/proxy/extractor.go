package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const apiExtractorErrorBodyLimit = 64 * 1024

// ProxyExtractor is the interface for extracting dynamic proxies
type ProxyExtractor interface {
	Extract(ctx context.Context) (*ExtractedProxy, error)
}

// LuminatiExtractor extracts proxies from Luminati/BrightData
type LuminatiExtractor struct {
	Username string
	Password string
	Zone     string
	Country  string
}

// Extract implements ProxyExtractor for Luminati
func (e *LuminatiExtractor) Extract(ctx context.Context) (*ExtractedProxy, error) {
	// Luminati format: http://user-zone-{zone}-country-{country}:pass@zproxy.lum-superproxy.io:22225
	username := fmt.Sprintf("%s-zone-%s", e.Username, e.Zone)
	if e.Country != "" {
		username = fmt.Sprintf("%s-country-%s", username, e.Country)
	}

	proxyURL := fmt.Sprintf("http://%s:%s@zproxy.lum-superproxy.io:22225",
		username, e.Password)

	// Get actual exit IP
	ip, err := checkProxyIP(ctx, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("luminati IP check failed: %w", err)
	}

	return &ExtractedProxy{
		ProxyURL:  proxyURL,
		IP:        ip,
		ExpiresAt: time.Now().Add(10 * time.Minute), // Luminati session typically lasts 10min
	}, nil
}

// OxylabsExtractor extracts proxies from Oxylabs
type OxylabsExtractor struct {
	Username string
	Password string
	Country  string
}

// Extract implements ProxyExtractor for Oxylabs
func (e *OxylabsExtractor) Extract(ctx context.Context) (*ExtractedProxy, error) {
	// Oxylabs format: http://customer-{username}-cc-{country}:pass@pr.oxylabs.io:7777
	username := fmt.Sprintf("customer-%s", e.Username)
	if e.Country != "" {
		username = fmt.Sprintf("%s-cc-%s", username, e.Country)
	}

	proxyURL := fmt.Sprintf("http://%s:%s@pr.oxylabs.io:7777",
		username, e.Password)

	// Get actual exit IP
	ip, err := checkProxyIP(ctx, proxyURL)
	if err != nil {
		return nil, fmt.Errorf("oxylabs IP check failed: %w", err)
	}

	return &ExtractedProxy{
		ProxyURL:  proxyURL,
		IP:        ip,
		ExpiresAt: time.Now().Add(10 * time.Minute),
	}, nil
}

// APIExtractor extracts proxies from a custom API
type APIExtractor struct {
	APIURL string
	APIKey string
	Params map[string]string
}

// Extract implements ProxyExtractor for custom APIs
func (e *APIExtractor) Extract(ctx context.Context) (*ExtractedProxy, error) {
	// Build request URL with parameters
	reqURL, err := url.Parse(e.APIURL)
	if err != nil {
		return nil, fmt.Errorf("invalid API URL: %w", err)
	}

	q := reqURL.Query()
	if e.APIKey != "" {
		q.Set("key", e.APIKey)
	}
	for k, v := range e.Params {
		q.Set(k, v)
	}
	reqURL.RawQuery = q.Encode()

	// Make request
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, apiExtractorErrorBodyLimit))
		return nil, fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var result struct {
		Proxy  string `json:"proxy"`
		IP     string `json:"ip"`
		Expire int64  `json:"expire"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse API response: %w", err)
	}

	if result.Proxy == "" {
		return nil, fmt.Errorf("API returned empty proxy")
	}

	// Determine expiration
	expiresAt := time.Now().Add(5 * time.Minute) // Default 5 minutes
	if result.Expire > 0 {
		expiresAt = time.Unix(result.Expire, 0)
	}

	return &ExtractedProxy{
		ProxyURL:  result.Proxy,
		IP:        result.IP,
		ExpiresAt: expiresAt,
	}, nil
}

// checkProxyIP checks the actual exit IP of a proxy
func checkProxyIP(ctx context.Context, proxyURL string) (string, error) {
	proxyURLParsed, err := url.Parse(proxyURL)
	if err != nil {
		return "", fmt.Errorf("invalid proxy URL: %w", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURLParsed),
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.ipify.org?format=json", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("IP check request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse IP response: %w", err)
	}

	return result.IP, nil
}

// MaskProxyURL masks sensitive information in a proxy URL
func MaskProxyURL(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return proxyURL
	}

	if u.User != nil {
		// Mask password - return unescaped string
		username := u.User.Username()
		masked := fmt.Sprintf("%s://%s:***@%s", u.Scheme, username, u.Host)
		if u.Path != "" {
			masked += u.Path
		}
		return masked
	}

	return u.String()
}
