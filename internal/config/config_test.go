package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestBodyResourceDefaults(t *testing.T) {
	cfg := Default()
	if cfg.MaxBodyBytes != 1<<30 || cfg.BodyMemoryThresholdBytes != 8<<20 || cfg.BodySpoolMaxBytes != 32<<30 || cfg.BodyDiskReserveBytes != 0 {
		t.Fatalf("body defaults: max=%d threshold=%d spool=%d reserve=%d", cfg.MaxBodyBytes, cfg.BodyMemoryThresholdBytes, cfg.BodySpoolMaxBytes, cfg.BodyDiskReserveBytes)
	}
	if got := cfg.EffectiveBodyMemoryBudgetBytes(); got < 8<<20 || got > 256<<20 {
		t.Fatalf("automatic body memory budget=%d", got)
	}
	if !cfg.UsageJournalEnabled || cfg.UsageJournalSegmentBytes != 8<<20 {
		t.Fatalf("usage journal defaults: enabled=%v segment=%d", cfg.UsageJournalEnabled, cfg.UsageJournalSegmentBytes)
	}
	if !cfg.BodyV2Enabled || !cfg.SchedulerIndexEnabled {
		t.Fatalf("phase defaults: body_v2=%v scheduler_index=%v", cfg.BodyV2Enabled, cfg.SchedulerIndexEnabled)
	}
	cfg.BodyMemoryBudgetBytes = 17 << 20
	if got := cfg.EffectiveBodyMemoryBudgetBytes(); got != 17<<20 {
		t.Fatalf("explicit body memory budget=%d", got)
	}
}

func TestPhaseRollbackEnvironmentOverrides(t *testing.T) {
	t.Setenv("CODEX_POOL_BODY_V2_ENABLED", "false")
	t.Setenv("CODEX_POOL_SCHEDULER_INDEX_ENABLED", "false")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BodyV2Enabled || cfg.SchedulerIndexEnabled {
		t.Fatalf("rollback flags ignored: body_v2=%v scheduler_index=%v", cfg.BodyV2Enabled, cfg.SchedulerIndexEnabled)
	}
}

func TestSystemdCredentialDirectoryFallback(t *testing.T) {
	credentialDirectory := t.TempDir()
	adminToken := "credential-admin-token-0123456789"
	if err := os.WriteFile(filepath.Join(credentialDirectory, "admin.token"), []byte(adminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credentialDirectory)
	t.Setenv("CODEX_POOL_MASTER_KEY_FILE", "")
	t.Setenv("CODEX_POOL_IDENTITY_KEY_FILE", "")
	t.Setenv("CODEX_POOL_DIAGNOSTIC_ALIAS_KEY_FILE", "")
	t.Setenv("CODEX_POOL_ADMIN_TOKEN", "")
	t.Setenv("CODEX_POOL_ADMIN_TOKEN_FILE", "")

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterKeyFile != filepath.Join(credentialDirectory, "master.key") ||
		cfg.IdentityKeyFile != filepath.Join(credentialDirectory, "identity.key") ||
		cfg.DiagnosticAliasKeyFile != filepath.Join(credentialDirectory, "diagnostic-alias.key") {
		t.Fatalf("credential paths were not derived from CREDENTIALS_DIRECTORY: %+v", cfg)
	}
	if cfg.AdminToken != adminToken {
		t.Fatalf("AdminToken = %q, want credential file value", cfg.AdminToken)
	}
}

func TestSystemdCredentialDirectoryAllowsMissingOptionalAdminToken(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	t.Setenv("CODEX_POOL_ADMIN_TOKEN", "")
	t.Setenv("CODEX_POOL_ADMIN_TOKEN_FILE", "")
	if _, err := Load(""); err != nil {
		t.Fatalf("missing optional admin.token must not fail config load: %v", err)
	}
}

func TestPostgresRequiresRedisAndSensitiveEnvironmentOverrides(t *testing.T) {
	t.Setenv("CODEX_POOL_STORAGE_DRIVER", "postgres")
	t.Setenv("CODEX_POOL_POSTGRES_DSN", "postgres://db.example/pool")
	t.Setenv("CODEX_POOL_REDIS_URL", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "requires redis_url") {
		t.Fatalf("missing Redis validation err=%v", err)
	}
	t.Setenv("CODEX_POOL_REDIS_URL", "redis://cache.example/0")
	t.Setenv("CODEX_POOL_NODE_ID", "node east/1")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StorageDriver != "postgres" || cfg.PostgresDSN != "postgres://db.example/pool" || cfg.RedisURL != "redis://cache.example/0" || cfg.NodeID != "node_east_1" {
		t.Fatalf("cluster config=%+v", cfg)
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

func TestDeprecatedClaudeCCHSigningDefaultsOffAndStillParses(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.ClaudeCCHSigning {
		t.Fatal("ClaudeCCHSigning default must be false for the cch-free current wire")
	}

	t.Setenv("CODEX_POOL_CLAUDE_CCH_SIGNING", "true")
	cfg, err = Load("")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if !cfg.ClaudeCCHSigning {
		t.Fatal("deprecated CODEX_POOL_CLAUDE_CCH_SIGNING setting must still parse")
	}
}

func TestStatefulStickyWaitDefaultsToRequestTimeout(t *testing.T) {
	cfg := Default()
	if cfg.StatefulStickyWaitSeconds != 0 {
		t.Fatalf("StatefulStickyWaitSeconds default = %d, want 0", cfg.StatefulStickyWaitSeconds)
	}
	if got, want := cfg.StatefulStickyWait(), cfg.RequestTimeout(); got != want {
		t.Fatalf("StatefulStickyWait() = %v, want request timeout %v", got, want)
	}
}

func TestStatefulStickyWaitCapsAtRequestTimeoutAndNormalizesNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"request_timeout_seconds":10,"stateful_sticky_wait_seconds":30}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.StatefulStickyWait(), 10; int(got.Seconds()) != want {
		t.Fatalf("StatefulStickyWait() = %v, want %ds", got, want)
	}

	raw = []byte(`{"request_timeout_seconds":10,"stateful_sticky_wait_seconds":-5}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load negative: %v", err)
	}
	if cfg.StatefulStickyWaitSeconds != 0 {
		t.Fatalf("StatefulStickyWaitSeconds = %d, want normalized 0", cfg.StatefulStickyWaitSeconds)
	}
	if got, want := cfg.StatefulStickyWait(), 10; int(got.Seconds()) != want {
		t.Fatalf("StatefulStickyWait() after negative = %v, want %ds", got, want)
	}
}

func TestStatefulStickyWaitEnvOverride(t *testing.T) {
	t.Setenv("CODEX_POOL_STATEFUL_STICKY_WAIT_SECONDS", "7")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load env: %v", err)
	}
	if cfg.StatefulStickyWaitSeconds != 7 {
		t.Fatalf("StatefulStickyWaitSeconds = %d, want env override 7", cfg.StatefulStickyWaitSeconds)
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
		"codex_session_mapping_enabled":         true,
		"codex_cpa_strict":                      true,
		"codex_stateless_passthrough":           false,
		"codex_prefer_sidecar_ja3_over_ws":      true,
		"codex_prompt_cache_retention":          "",
		"claude_cache_control_inject":           true,
		"claude_cache_mode":                     "max_hit",
		"claude_cache_affinity_policy":          "balanced",
		"claude_cache_breakpoint_policy":        "max_hit",
		"claude_cache_optimization_rollout":     "{}",
		"claude_native_cache_breakpoint_inject": true,
		"claude_cache_latest_tail_write":        true,
		"claude_cache_prewarm_mode":             "sync_extreme",
		"claude_cache_diagnostics_enabled":      true,
		"claude_cache_singleflight_enabled":     true,
		"claude_cache_lossless_block_split":     true,
		"claude_cch_signing":                    false,
		"claude_cache_ttl":                      "1h",
		"rate_limit_guard_enabled":              true,
		"seamless_failover":                     true,
		"failover_max_attempts":                 float64(3),
		"stateful_sticky_wait_seconds":          float64(0),
		"leak_scrub_enabled":                    true,
		"token_save_enabled":                    false,
		"codex_install_model":                   "gpt-5.6-sol",
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
	if cfg.CodexPromptCacheRetention != "" {
		t.Fatalf("CodexPromptCacheRetention = %q, want empty for Codex 0.144.x", cfg.CodexPromptCacheRetention)
	}
	if cfg.TokenSaveEnabled {
		t.Fatal("TokenSaveEnabled must stay false by default because it rewrites request content")
	}
	if cfg.CodexInstallModel != "gpt-5.6-sol" {
		t.Fatalf("CodexInstallModel = %q, want gpt-5.6-sol", cfg.CodexInstallModel)
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
