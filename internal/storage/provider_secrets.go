package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codex-account-pool/internal/secretbox"
)

// IsProviderSecretField centralizes which dynamic provider values must never be
// persisted in config_json or returned by the management API.
func IsProviderSecretField(field string) bool {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "api_key", "admin_token", "access_token", "refresh_token", "id_token",
		"password", "client_id", "client_secret", "username", "email", "base_email",
		"otp_url", "phone", "proxy_username", "proxy_password":
		return true
	}
	return strings.HasSuffix(field, "_api_key") ||
		strings.HasSuffix(field, "_token") ||
		strings.HasSuffix(field, "_password") ||
		strings.HasSuffix(field, "_secret") ||
		strings.HasSuffix(field, "_email")
}

func providerSecretDomain(providerType, providerKey, field string) string {
	return "provider-settings/" +
		strings.ToLower(strings.TrimSpace(providerType)) + "/" +
		strings.ToLower(strings.TrimSpace(providerKey)) + "/" +
		strings.ToLower(strings.TrimSpace(field))
}

// SplitProviderConfig removes sensitive string fields from a provider config object.
func SplitProviderConfig(config map[string]interface{}) (map[string]interface{}, map[string]string, error) {
	public := make(map[string]interface{}, len(config))
	secrets := make(map[string]string)
	for key, value := range config {
		if !IsProviderSecretField(key) {
			public[key] = value
			continue
		}
		if value == nil {
			secrets[key] = ""
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, nil, fmt.Errorf("provider secret field %q must be a string", key)
		}
		secrets[key] = strings.TrimSpace(text)
	}
	return public, secrets, nil
}

// SealProviderAuthJSON encrypts each field under a provider-and-field-specific HKDF
// domain, preventing ciphertext from being swapped between providers or purposes.
func (s *Store) SealProviderAuthJSON(providerType, providerKey string, values map[string]string) (string, error) {
	stored := make(map[string]string, len(values))
	for field, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if len(s.tokenKey) == 0 {
			if s.cryptoStrict {
				return "", errors.New("provider secret encryption key is unavailable")
			}
			// Compatibility for isolated storage tests before a key is installed.
			stored[field] = value
			continue
		}
		sealed, err := secretbox.SealDomain(
			s.tokenKey,
			providerSecretDomain(providerType, providerKey, field),
			value,
		)
		if err != nil {
			s.recordCryptoError(err)
			return "", err
		}
		stored[field] = sealed
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// OpenProviderAuthJSON fails closed for malformed, undecryptable, or post-migration
// plaintext credentials.
func (s *Store) OpenProviderAuthJSON(providerType, providerKey, raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	stored := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("provider auth_json is invalid: %w", err)
	}
	plain := make(map[string]string, len(stored))
	for field, value := range stored {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if !secretbox.IsSealed(value) {
			if s.cryptoStrict {
				err := errors.New("plaintext provider secret encountered after encryption migration")
				s.recordCryptoError(err)
				return nil, err
			}
			plain[field] = value
			continue
		}
		opened, err := secretbox.OpenDomainWithKeys(
			s.tokenKeys,
			providerSecretDomain(providerType, providerKey, field),
			value,
		)
		if err != nil {
			s.recordCryptoError(err)
			return nil, err
		}
		plain[field] = opened
	}
	return plain, nil
}

// ProviderAuthMetadata exposes non-reversible presence/version metadata without
// invoking the decryption path.
func ProviderAuthMetadata(raw string) map[string]map[string]interface{} {
	stored := map[string]string{}
	if json.Unmarshal([]byte(firstNonEmptyString(strings.TrimSpace(raw), "{}")), &stored) != nil {
		return map[string]map[string]interface{}{}
	}
	out := make(map[string]map[string]interface{}, len(stored))
	for field, value := range stored {
		if strings.TrimSpace(value) == "" {
			continue
		}
		version := "legacy"
		keyID := ""
		switch {
		case strings.HasPrefix(value, secretbox.Prefix):
			version = "v2"
			rest := strings.TrimPrefix(value, secretbox.Prefix)
			keyID, _, _ = strings.Cut(rest, ":")
		case strings.HasPrefix(value, secretbox.LegacyPrefix):
			version = "v1"
		}
		out[field] = map[string]interface{}{
			"configured":  true,
			"masked":      "••••",
			"key_version": version,
			"key_id":      keyID,
		}
	}
	return out
}

func (s *Store) migrateProviderSecrets(ctx context.Context) (int, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT id,provider_type,provider_key,config_json,auth_json
FROM provider_settings`)
	if err != nil {
		return 0, err
	}
	type providerRow struct {
		id, providerType, providerKey, configJSON, authJSON string
	}
	pending := make([]providerRow, 0)
	for rows.Next() {
		var row providerRow
		if err := rows.Scan(&row.id, &row.providerType, &row.providerKey, &row.configJSON, &row.authJSON); err != nil {
			rows.Close()
			return 0, err
		}
		config := map[string]interface{}{}
		if strings.TrimSpace(row.configJSON) != "" {
			if err := json.Unmarshal([]byte(row.configJSON), &config); err != nil {
				rows.Close()
				return 0, fmt.Errorf("provider %s config_json is invalid: %w", row.id, err)
			}
		}
		_, configSecrets, err := SplitProviderConfig(config)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("provider %s: %w", row.id, err)
		}
		stored := map[string]string{}
		if strings.TrimSpace(row.authJSON) != "" {
			if err := json.Unmarshal([]byte(row.authJSON), &stored); err != nil {
				rows.Close()
				return 0, fmt.Errorf("provider %s auth_json is invalid: %w", row.id, err)
			}
		}
		needsRewrite := len(configSecrets) > 0
		for _, value := range stored {
			if strings.TrimSpace(value) != "" && !secretbox.IsCurrent(s.tokenKey, value) {
				needsRewrite = true
				break
			}
		}
		if needsRewrite {
			pending = append(pending, row)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	if len(pending) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, row := range pending {
		config := map[string]interface{}{}
		if strings.TrimSpace(row.configJSON) != "" {
			if err := json.Unmarshal([]byte(row.configJSON), &config); err != nil {
				return 0, err
			}
		}
		public, fromConfig, err := SplitProviderConfig(config)
		if err != nil {
			return 0, err
		}
		secrets, err := s.OpenProviderAuthJSON(row.providerType, row.providerKey, row.authJSON)
		if err != nil {
			return 0, fmt.Errorf("decrypt provider %s credentials: %w", row.id, err)
		}
		for field, value := range fromConfig {
			if strings.TrimSpace(value) != "" {
				secrets[field] = value
			}
		}
		configRaw, err := json.Marshal(public)
		if err != nil {
			return 0, err
		}
		authRaw, err := s.SealProviderAuthJSON(row.providerType, row.providerKey, secrets)
		if err != nil {
			return 0, fmt.Errorf("encrypt provider %s credentials: %w", row.id, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE provider_settings SET config_json=?,auth_json=?,updated_at=? WHERE id=?`,
			string(configRaw), authRaw, Now(), row.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(pending), nil
}
