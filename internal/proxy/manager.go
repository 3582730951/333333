package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

// Manager manages proxy configurations and extraction
type Manager struct {
	store *storage.Store
	cache sync.Map // map[string]*ExtractedProxy
	mu    sync.RWMutex
}

// NewManager creates a new proxy manager
func NewManager(store *storage.Store) *Manager {
	return &Manager{
		store: store,
	}
}

// GetProxy retrieves or extracts a proxy based on configuration
func (m *Manager) GetProxy(ctx context.Context, configID string, forceNew bool) (*ExtractedProxy, error) {
	config, err := m.loadConfig(ctx, configID)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	if !config.Enabled {
		return nil, fmt.Errorf("proxy config %s is disabled", configID)
	}

	switch config.ProxyType {
	case ProxyTypeStatic:
		return m.getStaticProxy(config)
	case ProxyTypeDynamic:
		return m.getDynamicProxy(ctx, config, forceNew)
	case ProxyTypeRotating:
		return m.getRotatingProxy(config)
	default:
		return nil, fmt.Errorf("unknown proxy type: %s", config.ProxyType)
	}
}

func (m *Manager) getStaticProxy(config *ProxyConfig) (*ExtractedProxy, error) {
	if config.ProxyURL == "" {
		return nil, fmt.Errorf("static proxy URL is empty")
	}
	return &ExtractedProxy{
		ProxyURL:  config.ProxyURL,
		IP:        "",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, nil
}

func (m *Manager) getDynamicProxy(ctx context.Context, config *ProxyConfig, forceNew bool) (*ExtractedProxy, error) {
	if !forceNew {
		if cached, ok := m.cache.Load(config.ID); ok {
			proxy := cached.(*ExtractedProxy)
			if time.Now().Before(proxy.ExpiresAt) {
				return proxy, nil
			}
		}
	}

	extractor, err := m.createExtractor(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create extractor: %w", err)
	}

	proxy, err := extractor.Extract(ctx)
	if err != nil {
		m.recordUsage(ctx, config.ID, "", "", false, err.Error(), 0)
		return nil, fmt.Errorf("extraction failed: %w", err)
	}

	if proxy.ProxyURL == "" {
		return nil, fmt.Errorf("extractor returned empty proxy URL")
	}

	m.cache.Store(config.ID, proxy)
	m.recordUsage(ctx, config.ID, "", proxy.IP, true, "", 0)
	return proxy, nil
}

func (m *Manager) getRotatingProxy(config *ProxyConfig) (*ExtractedProxy, error) {
	if config.GatewayURL == "" {
		return nil, fmt.Errorf("rotating gateway URL is empty")
	}
	return &ExtractedProxy{
		ProxyURL:  config.GatewayURL,
		IP:        "",
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}, nil
}

func (m *Manager) createExtractor(config *ProxyConfig) (ProxyExtractor, error) {
	switch config.ProxyProvider {
	case ProxyProviderLuminati:
		return &LuminatiExtractor{
			Username: config.LuminatiUsername,
			Password: config.LuminatiPassword,
			Zone:     config.LuminatiZone,
			Country:  config.Country,
		}, nil
	case ProxyProviderOxylabs:
		return &OxylabsExtractor{
			Username: config.OxylabsUsername,
			Password: config.OxylabsPassword,
			Country:  config.Country,
		}, nil
	case ProxyProviderAPIExtract:
		params := make(map[string]string)
		if config.APIParams != "" {
			if err := json.Unmarshal([]byte(config.APIParams), &params); err != nil {
				return nil, fmt.Errorf("invalid API params JSON: %w", err)
			}
		}
		return &APIExtractor{
			APIURL: config.APIURL,
			APIKey: config.APIKey,
			Params: params,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy provider: %s", config.ProxyProvider)
	}
}

func (m *Manager) loadConfig(ctx context.Context, configID string) (*ProxyConfig, error) {
	row := m.store.DB().QueryRowContext(ctx, `
		SELECT id, name, proxy_type, proxy_provider, proxy_url,
		       api_url, api_key, api_params, gateway_url,
		       luminati_username, luminati_password, luminati_zone,
		       oxylabs_username, oxylabs_password, country,
		       fingerprint_enabled, fingerprint_mode,
		       total_used, success_count, fail_count, last_used_at,
		       enabled, created_at, updated_at
		FROM proxy_configs WHERE id = ?`, configID)

	var config ProxyConfig
	var fingerprintEnabled, enabled int

	err := row.Scan(
		&config.ID, &config.Name, &config.ProxyType, &config.ProxyProvider,
		&config.ProxyURL, &config.APIURL, &config.APIKey, &config.APIParams,
		&config.GatewayURL, &config.LuminatiUsername, &config.LuminatiPassword,
		&config.LuminatiZone, &config.OxylabsUsername, &config.OxylabsPassword,
		&config.Country, &fingerprintEnabled, &config.FingerprintMode,
		&config.TotalUsed, &config.SuccessCount, &config.FailCount,
		&config.LastUsedAt, &enabled, &config.CreatedAt, &config.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	config.FingerprintEnabled = fingerprintEnabled != 0
	config.Enabled = enabled != 0
	return &config, nil
}

func (m *Manager) recordUsage(ctx context.Context, configID, accountID, ip string, success bool, errorMsg string, durationMs int64) {
	now := storage.Now()
	successInt := 0
	if success {
		successInt = 1
	}

	_, _ = m.store.DB().ExecContext(ctx, `
		INSERT INTO proxy_usage_records(
			proxy_config_id, account_id, extracted_ip,
			success, error_message, duration_ms, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, configID, accountID, ip, successInt, errorMsg, durationMs, now)

	if success {
		_, _ = m.store.DB().ExecContext(ctx, `
			UPDATE proxy_configs
			SET total_used = total_used + 1,
			    success_count = success_count + 1,
			    last_used_at = ?
			WHERE id = ?`, now, configID)
	} else {
		_, _ = m.store.DB().ExecContext(ctx, `
			UPDATE proxy_configs
			SET total_used = total_used + 1,
			    fail_count = fail_count + 1,
			    last_used_at = ?
			WHERE id = ?`, now, configID)
	}
}

func (m *Manager) ValidateProxy(ctx context.Context, configID string) error {
	proxy, err := m.GetProxy(ctx, configID, true)
	if err != nil {
		return err
	}
	ip, err := checkProxyIP(ctx, proxy.ProxyURL)
	if err != nil {
		return fmt.Errorf("proxy validation failed: %w", err)
	}
	if ip == "" {
		return fmt.Errorf("proxy returned empty IP")
	}
	return nil
}

func (m *Manager) ClearCache(configID string) {
	if configID == "" {
		m.cache = sync.Map{}
	} else {
		m.cache.Delete(configID)
	}
}

func (m *Manager) GetStats(ctx context.Context, configID string) (map[string]interface{}, error) {
	config, err := m.loadConfig(ctx, configID)
	if err != nil {
		return nil, err
	}

	successRate := 0.0
	if config.TotalUsed > 0 {
		successRate = float64(config.SuccessCount) / float64(config.TotalUsed) * 100
	}

	var recentUsage int
	_ = m.store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM proxy_usage_records
		WHERE proxy_config_id = ? AND created_at > ?
	`, configID, storage.Now()-3600).Scan(&recentUsage)

	return map[string]interface{}{
		"total_used":    config.TotalUsed,
		"success_count": config.SuccessCount,
		"fail_count":    config.FailCount,
		"success_rate":  successRate,
		"recent_usage":  recentUsage,
		"last_used_at":  config.LastUsedAt,
	}, nil
}
