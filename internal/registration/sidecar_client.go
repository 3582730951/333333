package registration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultSidecarEndpoint = "http://127.0.0.1:8790"

// SidecarHTTPClient wraps HTTP calls through the curl_cffi sidecar for TLS fingerprint evasion.
type SidecarHTTPClient struct {
	Endpoint     string
	HTTPClient   *http.Client
	cookieJarKey string
	proxyURL     string
}

// SetProxy sets the proxy URL for all requests through this client.
func (c *SidecarHTTPClient) SetProxy(proxyURL string) *SidecarHTTPClient {
	c.proxyURL = proxyURL
	return c
}

// NewSidecarHTTPClient creates a client that routes through the curl_cffi sidecar.
func NewSidecarHTTPClient(endpoint string) *SidecarHTTPClient {
	if endpoint == "" {
		endpoint = defaultSidecarEndpoint
	}
	return &SidecarHTTPClient{
		Endpoint:   strings.TrimRight(endpoint, "/"),
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// SidecarMeta is the JSON structure sent in X-Sidecar-Meta header.
type SidecarMeta struct {
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	Proxy          string            `json:"proxy,omitempty"`
	Stream         bool              `json:"stream"`
	DefaultHeaders bool              `json:"default_headers"`
	CookieJarKey   string            `json:"cookie_jar_key,omitempty"`
}

// SetCookieJarKey sets a cookie jar key for maintaining cookies across requests.
func (c *SidecarHTTPClient) SetCookieJarKey(key string) *SidecarHTTPClient {
	c.cookieJarKey = key
	return c
}

// Do sends a request through the sidecar and returns the response.
func (c *SidecarHTTPClient) Do(ctx context.Context, method, urlStr string, headers map[string]string, body []byte, proxyURL string) (*http.Response, error) {
	// Use per-request proxy or fall back to client-level proxy
	pURL := proxyURL
	if pURL == "" {
		pURL = c.proxyURL
	}

	meta := SidecarMeta{
		Method:         method,
		URL:            urlStr,
		Headers:        headers,
		Stream:         false,
		DefaultHeaders: false,
		CookieJarKey:   c.cookieJarKey,
	}
	if pURL != "" {
		meta.Proxy = pURL
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal sidecar meta: %w", err)
	}
	encodedMeta := base64.StdEncoding.EncodeToString(metaJSON)

	if body == nil {
		body = []byte{}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/proxy", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Sidecar-Meta", encodedMeta)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sidecar request failed: %w", err)
	}

	// Map sidecar upstream status to response status code
	if statusStr := resp.Header.Get("x-sidecar-upstream-status"); statusStr != "" {
		var status int
		fmt.Sscanf(statusStr, "%d", &status)
		if status > 0 {
			resp.StatusCode = status
		}
	}

	return resp, nil
}

// Get is a convenience method for GET requests through the sidecar.
func (c *SidecarHTTPClient) Get(ctx context.Context, urlStr string, headers map[string]string, proxyURL string) (*http.Response, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	// Only set accept if not already present (case-insensitive check)
	if _, ok := headers["accept"]; !ok {
		if _, ok := headers["Accept"]; !ok {
			headers["accept"] = "application/json, text/plain, */*"
		}
	}
	return c.Do(ctx, http.MethodGet, urlStr, headers, nil, proxyURL)
}

// Post is a convenience method for POST requests through the sidecar.
func (c *SidecarHTTPClient) Post(ctx context.Context, urlStr string, headers map[string]string, body []byte, proxyURL string) (*http.Response, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	if _, ok := headers["accept"]; !ok {
		if _, ok := headers["Accept"]; !ok {
			headers["accept"] = "application/json"
		}
	}
	return c.Do(ctx, http.MethodPost, urlStr, headers, body, proxyURL)
}

// ReadBody reads and closes the response body, returning the bytes.
func ReadBody(resp *http.Response) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 256*1024))
}
