package config

import (
	"encoding/json"
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

func TestClaudeCCHSigningDefaultAndEnvOverride(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if !cfg.ClaudeCCHSigning {
		t.Fatal("ClaudeCCHSigning default must be true")
	}

	t.Setenv("CODEX_POOL_CLAUDE_CCH_SIGNING", "false")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if cfg.ClaudeCCHSigning {
		t.Fatal("CODEX_POOL_CLAUDE_CCH_SIGNING=false was not applied")
	}
}

func TestExampleConfigKeepsOptimizedCacheDefaults(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example config: %v", err)
	}
	var example map[string]interface{}
	if err := json.Unmarshal(raw, &example); err != nil {
		t.Fatalf("decode example config: %v", err)
	}

	wants := map[string]interface{}{
		"conversation_isolation":                true,
		"codex_prefer_sidecar_ja3_over_ws":      true,
		"codex_prompt_cache_retention":          "24h",
		"claude_cache_control_inject":           true,
		"claude_cache_affinity_policy":          "balanced",
		"claude_cache_breakpoint_policy":        "balanced",
		"claude_cache_optimization_rollout":     "{}",
		"claude_native_cache_breakpoint_inject": true,
		"claude_cch_signing":                    true,
		"claude_cache_ttl":                      "1h",
		"rate_limit_guard_enabled":              true,
		"seamless_failover":                     true,
		"failover_max_attempts":                 float64(3),
		"force_failover_on_429":                 false,
		"leak_scrub_enabled":                    true,
		"token_save_enabled":                    false,
		"codex_install_model":                   "gpt-5.5",
		"codex_install_effort":                  "xhigh",
	}
	for key, want := range wants {
		if got, ok := example[key]; !ok || got != want {
			t.Fatalf("config.example.json[%q] = %#v (present=%v), want %#v", key, got, ok, want)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load example config: %v", err)
	}
	if cfg.ClaudeCacheTTL != "1h" {
		t.Fatalf("ClaudeCacheTTL = %q, want 1h", cfg.ClaudeCacheTTL)
	}
	if cfg.CodexPromptCacheRetention != "24h" {
		t.Fatalf("CodexPromptCacheRetention = %q, want 24h", cfg.CodexPromptCacheRetention)
	}
	if cfg.TokenSaveEnabled {
		t.Fatal("TokenSaveEnabled must stay false by default because it rewrites request content")
	}
	if cfg.CodexInstallModel != "gpt-5.5" {
		t.Fatalf("CodexInstallModel = %q, want gpt-5.5", cfg.CodexInstallModel)
	}
	if cfg.CodexInstallEffort != "xhigh" {
		t.Fatalf("CodexInstallEffort = %q, want xhigh", cfg.CodexInstallEffort)
	}
}

func TestCodexJA3OrDefaultRemoved(t *testing.T) {
	// CodexJA3OrDefault was removed; JA3 resolution for Codex now lives in
	// upstream.resolveCodexJA3 (default Chrome, real Codex JA3 opt-in), mirroring
	// resolveClaudeJA3. Coverage moved to the upstream package tests.
	t.Skip("moved to upstream.resolveCodexJA3")
}
