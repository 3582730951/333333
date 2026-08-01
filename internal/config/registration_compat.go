package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// applyLegacyRegistrationConfig is a one-way compatibility adapter. The normal
// Config struct remains canonical; historical keys are read only when their new
// counterpart is absent, so mixed-version deployments have deterministic
// precedence and every subsequent write can use the current schema.
func applyLegacyRegistrationConfig(raw []byte, cfg *Config) error {
	if cfg == nil || len(raw) == 0 {
		return nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	type alias struct {
		legacy    string
		canonical string
		apply     func(json.RawMessage) error
	}
	aliases := []alias{
		{"email_registration_enabled", "registration_enabled", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.RegistrationEnabled)
		}},
		{"email_registration_concurrency", "registration_concurrency", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.RegistrationConcurrency)
		}},
		{"email_registration_timeout_seconds", "registration_timeout", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.RegistrationTimeout)
		}},
		{"email_registration_group", "registration_default_group", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.RegistrationDefaultGroup)
		}},
		{"email_registration_egress_pool_id", "registration_egress_pool_id", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.RegistrationEgressPoolID)
		}},
		{"register_method", "default_register_method", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.DefaultRegisterMethod)
		}},
		{"registration_method", "default_register_method", func(value json.RawMessage) error {
			return decodeLegacyRegistrationValue(value, &cfg.DefaultRegisterMethod)
		}},
	}
	for _, entry := range aliases {
		if _, canonicalPresent := fields[entry.canonical]; canonicalPresent {
			continue
		}
		value, legacyPresent := fields[entry.legacy]
		if !legacyPresent {
			continue
		}
		if err := entry.apply(value); err != nil {
			return fmt.Errorf("legacy registration config %s: %w", entry.legacy, err)
		}
	}
	return nil
}

func decodeLegacyRegistrationValue(raw json.RawMessage, destination interface{}) error {
	if err := json.Unmarshal(raw, destination); err == nil {
		return nil
	}
	// Several pre-release installers rendered numeric and boolean form values as
	// strings. Accept those without weakening canonical config decoding.
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}
	switch target := destination.(type) {
	case *bool:
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1", "on", "yes":
			*target = true
			return nil
		case "false", "0", "off", "no":
			*target = false
			return nil
		}
	case *int:
		var value int
		if _, err := fmt.Sscanf(strings.TrimSpace(text), "%d", &value); err == nil {
			*target = value
			return nil
		}
	}
	return fmt.Errorf("value %q has an incompatible type", text)
}

func normalizeRegistrationMethodAlias(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "email", "email_otp", "email_register", "protocol2", "protocol_v_2":
		return "protocol_v2"
	case "turbo", "turbo_gpt", "turbo_gpt_register", "playwright", "browser3", "browser_v_3":
		return "browser_v3"
	default:
		return normalized
	}
}

func firstNonEmptyConfig(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
