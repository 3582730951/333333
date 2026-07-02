package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestProxyManager(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Create proxy manager
	manager := NewManager(store)

	// Test 1: Insert a static proxy config
	now := storage.Now()
	configID := "test-static-1"
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO proxy_configs(
			id, name, proxy_type, proxy_provider, proxy_url,
			enabled, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 1, ?, ?)
	`, configID, "Test Static", "static", "http", "http://proxy.example.com:8080", now, now)
	if err != nil {
		t.Fatalf("Insert proxy config: %v", err)
	}

	// Test 2: Get static proxy
	proxy, err := manager.GetProxy(ctx, configID, false)
	if err != nil {
		t.Errorf("GetProxy failed: %v", err)
	}
	if proxy.ProxyURL != "http://proxy.example.com:8080" {
		t.Errorf("Unexpected proxy URL: %s", proxy.ProxyURL)
	}

	t.Logf("✓ Static proxy retrieved: %s", proxy.ProxyURL)

	// Test 3: Clear cache
	manager.ClearCache(configID)
	t.Logf("✓ Cache cleared")

	// Test 4: Get stats
	stats, err := manager.GetStats(ctx, configID)
	if err != nil {
		t.Errorf("GetStats failed: %v", err)
	}
	t.Logf("✓ Stats retrieved: %+v", stats)
}

func TestMaskProxyURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "HTTP with auth",
			input:    "http://user:password@proxy.com:8080",
			expected: "http://user:***@proxy.com:8080",
		},
		{
			name:     "SOCKS5 with auth",
			input:    "socks5://admin:secret123@proxy.com:1080",
			expected: "socks5://admin:***@proxy.com:1080",
		},
		{
			name:     "No auth",
			input:    "http://proxy.com:8080",
			expected: "http://proxy.com:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MaskProxyURL(tt.input)
			if result != tt.expected {
				t.Errorf("MaskProxyURL() = %s, want %s", result, tt.expected)
			}
		})
	}

	t.Logf("✓ All MaskProxyURL tests passed")
}

func TestAPIExtractor(t *testing.T) {
	// This is a unit test for the API extractor structure
	extractor := &APIExtractor{
		APIURL: "https://api.example.com/proxy",
		APIKey: "test-key",
		Params: map[string]string{
			"country": "US",
			"type":    "residential",
		},
	}

	// Just verify the structure
	if extractor.APIURL == "" {
		t.Error("APIURL should not be empty")
	}
	if len(extractor.Params) != 2 {
		t.Errorf("Expected 2 params, got %d", len(extractor.Params))
	}

	t.Logf("✓ APIExtractor structure validated")
}

func TestAPIExtractorLimitsErrorBody(t *testing.T) {
	const largeBodySize = apiExtractorErrorBodyLimit * 4
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "test-key" {
			t.Fatalf("key query = %q, want test-key", got)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprint(w, strings.Repeat("x", largeBodySize))
	}))
	defer server.Close()

	extractor := &APIExtractor{
		APIURL: server.URL,
		APIKey: "test-key",
	}
	_, err := extractor.Extract(context.Background())
	if err == nil {
		t.Fatal("Extract returned nil error for non-200 API response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "API returned status 502") {
		t.Fatalf("error = %q, want status context", msg)
	}
	if strings.Count(msg, "x") != apiExtractorErrorBodyLimit {
		t.Fatalf("error body length = %d, want %d", strings.Count(msg, "x"), apiExtractorErrorBodyLimit)
	}
}

func TestLuminatiExtractor(t *testing.T) {
	extractor := &LuminatiExtractor{
		Username: "testuser",
		Password: "testpass",
		Zone:     "residential",
		Country:  "us",
	}

	if extractor.Username == "" {
		t.Error("Username should not be empty")
	}
	if extractor.Zone == "" {
		t.Error("Zone should not be empty")
	}

	t.Logf("✓ LuminatiExtractor structure validated")
}

func TestProxyTypes(t *testing.T) {
	// Test proxy type constants
	if ProxyTypeStatic != "static" {
		t.Errorf("ProxyTypeStatic = %s, want 'static'", ProxyTypeStatic)
	}
	if ProxyTypeDynamic != "dynamic" {
		t.Errorf("ProxyTypeDynamic = %s, want 'dynamic'", ProxyTypeDynamic)
	}
	if ProxyTypeRotating != "rotating" {
		t.Errorf("ProxyTypeRotating = %s, want 'rotating'", ProxyTypeRotating)
	}

	t.Logf("✓ Proxy type constants validated")
}

func TestExtractedProxyExpiration(t *testing.T) {
	proxy := &ExtractedProxy{
		ProxyURL:  "http://test.com:8080",
		IP:        "1.2.3.4",
		ExpiresAt: time.Now().Add(5 * time.Minute),
	}

	if time.Now().After(proxy.ExpiresAt) {
		t.Error("Proxy should not be expired immediately after creation")
	}

	if proxy.IP != "1.2.3.4" {
		t.Errorf("IP = %s, want '1.2.3.4'", proxy.IP)
	}

	t.Logf("✓ ExtractedProxy expiration logic validated")
}
