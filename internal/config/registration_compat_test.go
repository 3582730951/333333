package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeRegistrationCompatConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadMapsLegacyEmailRegistrationConfig(t *testing.T) {
	path := writeRegistrationCompatConfig(t, `{
		"email_registration_enabled": true,
		"email_registration_concurrency": 7,
		"email_registration_timeout_seconds": 481,
		"email_registration_group": "legacy-registration",
		"email_registration_egress_pool_id": "legacy-egress-pool",
		"register_method": "email"
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load legacy registration config: %v", err)
	}
	if !cfg.RegistrationEnabled || cfg.RegistrationConcurrency != 7 || cfg.RegistrationTimeout != 481 {
		t.Fatalf("legacy runtime fields not migrated: enabled=%v concurrency=%d timeout=%d",
			cfg.RegistrationEnabled, cfg.RegistrationConcurrency, cfg.RegistrationTimeout)
	}
	if cfg.RegistrationDefaultGroup != "legacy-registration" {
		t.Fatalf("RegistrationDefaultGroup=%q, want legacy-registration", cfg.RegistrationDefaultGroup)
	}
	if cfg.RegistrationEgressPoolID != "legacy-egress-pool" {
		t.Fatalf("RegistrationEgressPoolID=%q", cfg.RegistrationEgressPoolID)
	}
	if cfg.DefaultRegisterMethod != "protocol_v2" {
		t.Fatalf("DefaultRegisterMethod=%q, want protocol_v2 alias", cfg.DefaultRegisterMethod)
	}
}

func TestLoadCanonicalRegistrationConfigWinsOverLegacyAliases(t *testing.T) {
	path := writeRegistrationCompatConfig(t, `{
		"registration_enabled": false,
		"email_registration_enabled": true,
		"registration_concurrency": 4,
		"email_registration_concurrency": 9,
		"registration_timeout": 222,
		"email_registration_timeout_seconds": 999,
		"registration_default_group": "canonical-group",
		"email_registration_group": "legacy-group",
		"registration_egress_pool_id": "canonical-pool",
		"email_registration_egress_pool_id": "legacy-pool",
		"default_register_method": "browser_v3",
		"register_method": "email"
	}`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load mixed registration config: %v", err)
	}
	if cfg.RegistrationEnabled || cfg.RegistrationConcurrency != 4 || cfg.RegistrationTimeout != 222 {
		t.Fatalf("canonical fields lost precedence: enabled=%v concurrency=%d timeout=%d",
			cfg.RegistrationEnabled, cfg.RegistrationConcurrency, cfg.RegistrationTimeout)
	}
	if cfg.RegistrationDefaultGroup != "canonical-group" || cfg.RegistrationEgressPoolID != "canonical-pool" {
		t.Fatalf("canonical routing fields lost precedence: group=%q pool=%q",
			cfg.RegistrationDefaultGroup, cfg.RegistrationEgressPoolID)
	}
	if cfg.DefaultRegisterMethod != "browser_v3" {
		t.Fatalf("DefaultRegisterMethod=%q", cfg.DefaultRegisterMethod)
	}
}
