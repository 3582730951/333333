package config

import "testing"

func TestCompatibilityAndIdentityDefaultsAreSafe(t *testing.T) {
	cfg := Default()
	// One virtual device per account is the safe default: the legacy per-egress
	// derivation minted a fresh installation id for every exit an account touched, so
	// the upstream accumulated one account with many devices.
	if cfg.IdentityConvergenceMode != "account" {
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
	// An unrecognized mode must converge, never fall back to the exit-scoped device.
	if cfg.IdentityConvergenceMode != "account" || cfg.CompatibilityManifestRefreshHours != DefaultCompatibilityManifestRefreshHours ||
		cfg.CompatibilityManifestMaxStaleDays != 365 || cfg.ConnectTimeoutSeconds != DefaultConnectTimeoutSeconds {
		t.Fatalf("unsafe values were not normalized: %+v", cfg)
	}
}
