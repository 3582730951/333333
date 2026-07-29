package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (p *Pipeline) providerConfig(ctx context.Context, providerType, providerKey string) (map[string]interface{}, error) {
	if p == nil || p.store == nil {
		return nil, fmt.Errorf("provider storage unavailable")
	}
	var configJSON, authJSON string
	err := p.store.DB().QueryRowContext(ctx, `
SELECT config_json,auth_json FROM provider_settings
WHERE provider_type=? AND provider_key=? AND enabled=1 LIMIT 1`,
		providerType, providerKey).Scan(&configJSON, &authJSON)
	if err != nil {
		return nil, err
	}
	config := map[string]interface{}{}
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			return nil, fmt.Errorf("provider config is invalid")
		}
	}
	secrets, err := p.store.OpenProviderAuthJSON(providerType, providerKey, authJSON)
	if err != nil {
		return nil, fmt.Errorf("provider credentials are unavailable")
	}
	for field, value := range secrets {
		config[field] = value
	}
	return config, nil
}
