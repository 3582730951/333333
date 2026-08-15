package config

import "testing"

func TestCompatibilityAndIdentityDefaultsAreSafe(t *testing.T) {
	cfg := Default()
	if cfg.IdentityConvergenceMode != "off" {
		t.Fatalf("identity convergence default=%q", cfg.IdentityConvergenceMode)
	}
	if !cfg.CompatibilityManifestEnabled || cfg.CompatibilityManifestSource != "official" {
		t.Fatalf("compatibility defaults=%+v", cfg)
	}
	if cfg.ConnectTimeoutSeconds != DefaultConnectTimeoutSeconds {
		t.Fatalf("connect timeout=%d", cfg.ConnectTimeoutSeconds)
	}
	cfg.IdentityConvergenceMode = "unexpected"
	cfg.CompatibilityManifestRefreshHours = 0
	cfg.CompatibilityManifestMaxStaleDays = 9999
	cfg.ConnectTimeoutSeconds = 0
	cfg.normalize()
	if cfg.IdentityConvergenceMode != "off" || cfg.CompatibilityManifestRefreshHours != DefaultCompatibilityManifestRefreshHours ||
		cfg.CompatibilityManifestMaxStaleDays != 365 || cfg.ConnectTimeoutSeconds != DefaultConnectTimeoutSeconds {
		t.Fatalf("unsafe values were not normalized: %+v", cfg)
	}
}
