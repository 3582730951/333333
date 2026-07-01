package proxy

import (
	"time"
)

// ProxyType defines the type of proxy
type ProxyType string

const (
	ProxyTypeStatic   ProxyType = "static"   // Fixed IP proxy
	ProxyTypeDynamic  ProxyType = "dynamic"  // Dynamic IP (API extraction)
	ProxyTypeRotating ProxyType = "rotating" // Rotating gateway
)

// ProxyProvider defines the proxy service provider
type ProxyProvider string

const (
	ProxyProviderHTTP            ProxyProvider = "http"
	ProxyProviderSocks5          ProxyProvider = "socks5"
	ProxyProviderAPIExtract      ProxyProvider = "api_extract"
	ProxyProviderLuminati        ProxyProvider = "luminati"
	ProxyProviderOxylabs         ProxyProvider = "oxylabs"
	ProxyProviderRotatingGateway ProxyProvider = "rotating_gateway"
)

// ProxyConfig represents a proxy configuration
type ProxyConfig struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	ProxyType     ProxyType     `json:"proxy_type"`
	ProxyProvider ProxyProvider `json:"proxy_provider"`

	// Static proxy
	ProxyURL string `json:"proxy_url"`

	// Dynamic proxy (API extraction)
	APIURL    string `json:"api_url"`
	APIKey    string `json:"api_key"`
	APIParams string `json:"api_params"` // JSON string

	// Rotating gateway
	GatewayURL string `json:"gateway_url"`

	// Luminati specific
	LuminatiUsername string `json:"luminati_username"`
	LuminatiPassword string `json:"luminati_password"`
	LuminatiZone     string `json:"luminati_zone"`

	// Oxylabs specific
	OxylabsUsername string `json:"oxylabs_username"`
	OxylabsPassword string `json:"oxylabs_password"`

	// General
	Country string `json:"country"`

	// Fingerprint
	FingerprintEnabled bool   `json:"fingerprint_enabled"`
	FingerprintMode    string `json:"fingerprint_mode"` // per_account, per_task

	// Statistics
	TotalUsed    int   `json:"total_used"`
	SuccessCount int   `json:"success_count"`
	FailCount    int   `json:"fail_count"`
	LastUsedAt   int64 `json:"last_used_at"`

	Enabled   bool  `json:"enabled"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// ExtractedProxy represents a proxy that has been extracted/resolved
type ExtractedProxy struct {
	ProxyURL  string    `json:"proxy_url"`  // Full proxy URL
	IP        string    `json:"ip"`         // Actual IP address
	ExpiresAt time.Time `json:"expires_at"` // When this proxy expires
}

// ProxyUsageRecord tracks proxy usage
type ProxyUsageRecord struct {
	ID            int64  `json:"id"`
	ProxyConfigID string `json:"proxy_config_id"`
	AccountID     string `json:"account_id"`
	TaskID        string `json:"task_id"`
	ExtractedIP   string `json:"extracted_ip"`
	ProxyURL      string `json:"proxy_url"` // Masked
	Success       bool   `json:"success"`
	ErrorMessage  string `json:"error_message"`
	DurationMs    int64  `json:"duration_ms"`
	CreatedAt     int64  `json:"created_at"`
}
