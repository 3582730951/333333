package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMigratesLegacyClaudeOAuthTokenURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"claude_oauth_token_url":"https://console.anthropic.com/v1/oauth/token"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClaudeOAuthTokenURL != DefaultClaudeOAuthTokenURL {
		t.Fatalf("ClaudeOAuthTokenURL = %q, want %q", cfg.ClaudeOAuthTokenURL, DefaultClaudeOAuthTokenURL)
	}
}

func TestLoadFloorsLegacyCodexClientVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"client_version":"0.118.0","codex_cli_version":"0.122.0"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientVersion != DefaultClientVersion {
		t.Fatalf("ClientVersion = %q, want floored %q", cfg.ClientVersion, DefaultClientVersion)
	}
	if cfg.CodexCLIVersionOverride != DefaultClientVersion {
		t.Fatalf("CodexCLIVersionOverride = %q, want floored %q", cfg.CodexCLIVersionOverride, DefaultClientVersion)
	}
}

func TestLoadKeepsNewerCodexClientVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Versions NEWER than the built-in default — these must be preserved verbatim
	// (the floor only bumps versions that LAG behind the shipping default).
	raw := []byte(`{"client_version":"0.200.0","codex_cli_version":"0.199.0"}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ClientVersion != "0.200.0" {
		t.Fatalf("ClientVersion = %q, want preserved newer override", cfg.ClientVersion)
	}
	if cfg.CodexCLIVersionOverride != "0.199.0" {
		t.Fatalf("CodexCLIVersionOverride = %q, want preserved newer override", cfg.CodexCLIVersionOverride)
	}
}

func TestCodexCLIVersionEnvOverride(t *testing.T) {
	// A version NEWER than the built-in default must be honored from the env
	// (the floor only bumps lagging versions).
	t.Setenv("CODEX_POOL_CODEX_CLI_VERSION", "0.200.0")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CodexCLIVersionOverride != "0.200.0" {
		t.Fatalf("CodexCLIVersionOverride = %q, want env override preserved", cfg.CodexCLIVersionOverride)
	}
}

func TestCodexJA3OrDefaultRemoved(t *testing.T) {
	// CodexJA3OrDefault was removed; JA3 resolution for Codex now lives in
	// upstream.resolveCodexJA3 (default Chrome, real Codex JA3 opt-in), mirroring
	// resolveClaudeJA3. Coverage moved to the upstream package tests.
	t.Skip("moved to upstream.resolveCodexJA3")
}
