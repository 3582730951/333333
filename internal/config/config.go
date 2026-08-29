package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddr               = "127.0.0.1:8787"
	DefaultDatabasePath             = "codex-pool.sqlite3"
	DefaultStorageDriver            = "sqlite"
	DefaultUpstreamBaseURL          = "https://chatgpt.com/backend-api/codex"
	DefaultOpenAIAPIUpstreamBaseURL = "https://api.openai.com/v1"
	DefaultGroupName                = "cyber"
	// DefaultClientVersion is the current Codex client version sent on model
	// discovery and on version-gated live Codex requests. ChatGPT gates the returned
	// model catalog and some live models by this value, so old preserved config
	// values are floored to this default during normalization.
	// Refreshed 2026-08-27 to Codex CLI 0.150.1, whose wire traits were verified
	// against the codex-rs tree in third_party/reference/codex. The application-level protocol traits
	// for every accepted downstream version live in codexCLIFingerprints below.
	DefaultClientVersion                  = "0.150.1"
	DefaultStickyWaitMillis               = 100
	DefaultStrictStickyMaxCooldownSeconds = 60
	DefaultCooldownWaitMaxSeconds         = 30
	// AdmissionWaitMillis is retained for config compatibility. Admission now waits
	// until downstream cancellation or the request-wide 600-second deadline.
	DefaultAdmissionWaitMillis = 600000
	// Cooldown→health-recheck loop: how often to probe benched accounts, and how long
	// to re-bench one whose probe still fails before the next attempt.
	DefaultAccountRecheckIntervalSeconds = 20
	DefaultAccountRecheckBackoffSeconds  = 120
	// Consecutive-5xx breaker. Five failures is well past any single retry ladder, so
	// a healthy account with one bad minute is never benched by it, while a dead
	// upstream stops absorbing traffic within seconds instead of hours.
	DefaultAccountFailureStreakThreshold       = 5
	DefaultAccountFailureStreakCooldownSeconds = 300
	DefaultRequestTimeoutSec                   = 600
	// DefaultConnectTimeoutSeconds bounds only connection establishment (DNS,
	// TCP/proxy and TLS). Long inference and streaming reads continue to use the
	// independent request idle timeout below.
	DefaultConnectTimeoutSeconds    = 12
	DefaultShutdownDrainSec         = 30
	DefaultMaxBodyBytes             = 1 << 30
	DefaultBodyMemoryThresholdBytes = 8 << 20
	DefaultBodyMemoryBudgetMaxBytes = 256 << 20
	DefaultBodySpoolMaxBytes        = 32 << 30
	DefaultBodyDiskReserveBytes     = 0
	DefaultUsageJournalSegmentBytes = 8 << 20
	// Goal continuity keeps a seven-day sliding window. The original 256 MiB
	// bootstrap default was exhausted by sustained Codex tool traffic while the
	// server still had ample disk. One GiB provides fourfold admission headroom;
	// the legacy value is retained only for the one-time runtime-default migration.
	LegacyDefaultGoalStorageMaxMB         = 256
	DefaultGoalStorageMaxMB               = 1024
	DefaultStreamFailoverHoldMemoryBytes  = 8 << 20
	DefaultStreamFailoverHoldDiskBytes    = 0
	DefaultVirtualWindow                  = 2_000_000
	DefaultVirtualContextLedgerTTLSeconds = 3600 // 1 hour
	DefaultKiroCacheUnreportedThreshold   = 20
	// AccountTokenBudget caps CONCURRENT estimated tokens stacked on one account (a
	// smoothing gate against over-committing a single upstream, not a per-request limit).
	// Sized for several concurrent 1M-context Claude Code turns per account; exceeding it
	// now makes a request WAIT for headroom (see AdmissionWait), never fail downstream.
	DefaultAccountTokenBudget = 0
	// Standard Claude Code OAuth client (Claude Pro/Max). Operators may override
	// via config if Anthropic rotates these. These values are from the shipping
	// Claude Code 2.1.226 sa()/Ydc configuration.
	DefaultClaudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	DefaultClaudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// Web-login (paste-back) OAuth defaults. These drive the "generate a login URL →
	// log in in the browser → paste the redirected URL / code back" import flow for
	// both providers (internal/api/oauth.go). They mirror the official clients so the
	// authorize/token endpoints accept the request; every value is config-overridable
	// in case OpenAI/Anthropic rotate the client or move an endpoint.
	DefaultCodexOAuthAuthURL       = "https://auth.openai.com/oauth/authorize"
	DefaultCodexOAuthTokenURL      = "https://auth.openai.com/oauth/token"
	DefaultCodexOAuthClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexOAuthRedirectURI   = "http://localhost:1455/auth/callback"
	DefaultCodexOAuthScope         = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	DefaultClaudeOAuthAuthURL      = "https://claude.com/cai/oauth/authorize"
	DefaultClaudeOAuthRedirectURI  = "https://platform.claude.com/oauth/code/callback"
	DefaultClaudeOAuthScope        = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	DefaultClaudeOAuthRefreshScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// Antigravity (Google Cloud Code) OAuth defaults for web-login import flow.
	DefaultAntigravityOAuthAuthURL      = "https://accounts.google.com/o/oauth2/v2/auth"
	DefaultAntigravityOAuthTokenURL     = "https://oauth2.googleapis.com/token"
	DefaultAntigravityOAuthClientID     = "\x31\x30\x37\x31\x30\x30\x36\x30\x36\x30\x35\x39\x31\x2d\x74\x6d\x68\x73\x73\x69\x6e\x32\x68\x32\x31\x6c\x63\x72\x65\x32\x33\x35\x76\x74\x6f\x6c\x6f\x6a\x68\x34\x67\x34\x30\x33\x65\x70\x2e\x61\x70\x70\x73\x2e\x67\x6f\x6f\x67\x6c\x65\x75\x73\x65\x72\x63\x6f\x6e\x74\x65\x6e\x74\x2e\x63\x6f\x6d"
	DefaultAntigravityOAuthClientSecret = "\x47\x4f\x43\x53\x50\x58\x2d\x4b\x35\x38\x46\x57\x52\x34\x38\x36\x4c\x64\x4c\x4a\x31\x6d\x4c\x42\x38\x73\x58\x43\x34\x7a\x36\x71\x44\x41\x66"
	DefaultAntigravityOAuthRedirectURI  = "http://localhost:51121/oauth-callback"
	DefaultAntigravityOAuthScope        = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"
	// DefaultClaudeNodeVersion is the Node runtime version reported in
	// X-Stainless-Runtime-Version. Kept here (not in the identity package) so the
	// upstream Node fingerprint can be bumped from one place / overridden by config.
	// Reconfirmed 2026-08-24 from the Claude Code 2.1.236–2.1.241 shipping binaries.
	DefaultClaudeNodeVersion = "v26.3.0"
	// DefaultClaudeCLIVersion is the newest verified Claude Code release in the
	// fingerprint library (claudeCLIFingerprints). The pool stamps this claude-cli
	// version on requests unless the operator pins claude_cli_version or the
	// account identity selects a lag release.
	DefaultClaudeCLIVersion = "2.1.241"
	// DefaultModelProbeIntervalHours refreshes each account's last-good model
	// catalog every six hours. The worker adds per-account jitter.
	DefaultModelProbeIntervalHours           = 6
	DefaultCompatibilityManifestRefreshHours = 6
	DefaultCompatibilityManifestMaxStaleDays = 30
	// Model-quality monitoring is group×model (never per account). One compact
	// primary probe runs per interval; confirmations are anomaly-only.
	DefaultModelQualityIntervalMinutes   = 60
	DefaultModelQualityReasoningEffort   = "low"
	DefaultModelQualityDegradedThreshold = 2
	DefaultModelQualityHistoryDays       = 30
	// DefaultGeoProbeURL is the IP/geo echo used to auto-detect a proxy's exit
	// region when none is configured. It returns JSON with ip/country/region/city.
	DefaultGeoProbeURL          = "https://ipapi.co/json/"
	DefaultCodexReauthWorkerURL = "http://127.0.0.1:8802"

	legacyClaudeOAuthTokenURL       = "https://console.anthropic.com/v1/oauth/token"
	legacyClaudeOAuthTokenURLAPI    = "https://api.anthropic.com/v1/oauth/token"
	legacyClaudeOAuthAuthURL        = "https://claude.ai/oauth/authorize"
	legacyClaudeOAuthRedirectURI    = "http://localhost:54545/callback"
	legacyClaudeOAuthAuthorizeScope = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// CodexCLIFingerprint is the application-level wire contract emitted by one
// verified Codex CLI release. Keeping these traits beside the accepted version
// window prevents a detected downstream version from changing only User-Agent
// while retaining a different release's body/header shape.
type CodexCLIFingerprint struct {
	Version                 string
	RequiredBetaFeatures    string
	CodeModeToolNames       bool
	ParentTurnID            bool
	PromptCacheKeyBySession bool
}

// codexCLIFingerprints is the fingerprint library verified against the corresponding
// official Codex releases. Keep newest first.
//
// 0.150.1 was verified against the codex-rs tree in third_party/reference/codex: the default
// x-codex-beta-features value is still exactly "remote_compaction_v2" (the FEATURES
// table has only two Stage::Experimental entries — network_proxy and prevent_idle_sleep —
// both default_enabled:false, while RemoteCompactionV2 is Stage::Stable/default_enabled:true
// and is special-cased into the header), parent_turn_id and the session-scoped
// prompt_cache_key contract are unchanged, and code_mode_tool_names is now a LEGACY key
// superseded by tool_namespaces_info. The gateway passes tool_namespaces_info through
// untouched, so the Responses metadata contract for these four traits is identical to
// 0.149.1/0.148.0/0.147.0.
var codexCLIFingerprints = [...]CodexCLIFingerprint{
	{Version: "0.150.1", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, ParentTurnID: true, PromptCacheKeyBySession: true},
	{Version: "0.149.1", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, ParentTurnID: true, PromptCacheKeyBySession: true},
	{Version: "0.149.0", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, ParentTurnID: true, PromptCacheKeyBySession: true},
	{Version: "0.148.0", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, ParentTurnID: true, PromptCacheKeyBySession: true},
	{Version: "0.147.0", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, ParentTurnID: true, PromptCacheKeyBySession: true},
	{Version: "0.146.1", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, PromptCacheKeyBySession: true},
	{Version: "0.146.0", RequiredBetaFeatures: "remote_compaction_v2", CodeModeToolNames: true, PromptCacheKeyBySession: true},
	{Version: "0.145.0", RequiredBetaFeatures: "remote_compaction_v2", PromptCacheKeyBySession: true},
}

// SupportedCodexCLIVersions returns a copy so callers cannot mutate the process-
// wide compatibility contract.
func SupportedCodexCLIVersions() []string {
	versions := make([]string, len(codexCLIFingerprints))
	for i, fingerprint := range codexCLIFingerprints {
		versions[i] = fingerprint.Version
	}
	return versions
}

// IsSupportedCodexCLIVersion reports whether value is one of the official stable
// versions covered by this build.
func IsSupportedCodexCLIVersion(value string) bool {
	_, ok := CodexCLIFingerprintForVersion(value)
	return ok
}

// CodexCLIFingerprintForVersion returns an immutable copy of the exact profile
// carried by this build. Callers deliberately receive no nearest/future match:
// automatic downstream selection is allowed only when every version-bearing
// signal names a fingerprint we have actually verified.
func CodexCLIFingerprintForVersion(value string) (CodexCLIFingerprint, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	for _, fingerprint := range codexCLIFingerprints {
		if value == fingerprint.Version {
			return fingerprint, true
		}
	}
	return CodexCLIFingerprint{}, false
}

// ClaudeCLIFingerprint is the coherent upstream wire tuple of one verified Claude
// Code release. The claude-cli version, the @anthropic-ai/sdk "Stainless" package
// version, and the Node runtime ship as ONE combination in the official binary;
// rotating one axis alone produces a client-version combination no real release
// ever emitted — itself a relay-detection signal upstream can flag.
type ClaudeCLIFingerprint struct {
	// Version is the claude-cli version used in the User-Agent and as the plain
	// cc_version value in the x-anthropic-billing-header block (no build suffix).
	Version string
	// StainlessPackageVersion is the X-Stainless-Package-Version header value.
	StainlessPackageVersion string
	// NodeVersion is the X-Stainless-Runtime-Version header value.
	NodeVersion string
}

// claudeCLIFingerprints is the verified Claude Code fingerprint library, newest
// first. Verified on 2026-08-24 by running the official linux-x64 binaries for
// 2.1.226 and 2.1.236–2.1.241 against a local capture server:
//   - 2.1.236–2.1.241 all ship @anthropic-ai/sdk 0.112.1 on Node v26.3.0;
//   - 2.1.226 shipped @anthropic-ai/sdk 0.94.0 on Node v26.3.0;
//   - every release emits a billing block of the form
//     `cc_version=<v>.<3-hex>; cc_entrypoint=<cli|sdk-cli>;` — the message-derived
//     attribution suffix is LIVE (see below), and no cch field rides along
//     (NATIVE_CLIENT_ATTESTATION is off).
//
// Watch item: the real client appends a message-derived `.xxx` attribution
// suffix to cc_version (SHA256(salt+firstMsg[4,7,20]+version)[:3]). It is
// UNCONDITIONAL inside the attribution header, which is enabled by default
// (GrowthBook tengu_attribution_header) and was verified LIVE on official
// 2.1.236–2.1.241 binaries on 2026-08-24 (every captured cc_version carried the
// suffix; the hash matched the algorithm exactly). The pool mirrors the
// algorithm in cloak.computeClaudeAttributionFingerprint behind
// ClaudeAttributionFingerprint (env CODEX_POOL_CLAUDE_ATTRIBUTION_FINGERPRINT),
// which DEFAULTS ON to match the genuine wire. A signed_custom manifest
// assertion (attribution_suffix: "plain") flips it OFF if a future capture shows
// the server turned the feature down; a "live" assertion forces it back ON. The
// task#12 auto-fetch binary records cc_version VERBATIM and diffs for `.<3-hex>`.
//
// The anthropic-beta header is intentionally NOT stored here: captured 2.1.241
// wire proves it is model-dependent (fallback-credit rides opus-5 but not sonnet;
// haiku emits a shorter, differently-ordered list) and entitlement-gated
// (context-1m appears for native-1M models only on an entitled account, not a
// fake token). That variability is handled dynamically in claudeBetasForRequest,
// not as a static row in this library.
var claudeCLIFingerprints = [...]ClaudeCLIFingerprint{
	{Version: "2.1.241", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.240", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.239", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.238", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.237", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.236", StainlessPackageVersion: "0.112.1", NodeVersion: "v26.3.0"},
	{Version: "2.1.226", StainlessPackageVersion: "0.94.0", NodeVersion: "v26.3.0"},
}

// SupportedClaudeCLIVersions returns a copy of the verified Claude Code version
// window so callers cannot mutate the process-wide compatibility contract.
func SupportedClaudeCLIVersions() []string {
	versions := make([]string, len(claudeCLIFingerprints))
	for i, fingerprint := range claudeCLIFingerprints {
		versions[i] = fingerprint.Version
	}
	return versions
}

// ClaudeCLIFingerprintForVersion returns an immutable copy of the exact Claude
// Code profile carried by this build. Like the Codex library it deliberately
// offers no nearest/future match: a version outside the verified window is
// treated as unsupported rather than approximated.
func ClaudeCLIFingerprintForVersion(value string) (ClaudeCLIFingerprint, bool) {
	value = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "v"))
	for _, fingerprint := range claudeCLIFingerprints {
		if value == fingerprint.Version {
			return fingerprint, true
		}
	}
	return ClaudeCLIFingerprint{}, false
}

// ClaudeStainlessVersionForCLI returns the coherent @anthropic-ai/sdk package
// version that shipped with claudeVer, or fallback when claudeVer is outside the
// verified window. It never invents a cli↔SDK combination.
func ClaudeStainlessVersionForCLI(claudeVer, fallback string) string {
	if fingerprint, ok := ClaudeCLIFingerprintForVersion(claudeVer); ok {
		return fingerprint.StainlessPackageVersion
	}
	return fallback
}

var (
	defaultClaudeGatewayInterceptHosts = []string{
		"api.anthropic.com",
	}
	defaultClaudeGatewayForwardHosts = []string{
		"api.openai.com",
		"chatgpt.com",
		"*.chatgpt.com",
		"chat.openai.com",
		"github.com",
		"*.github.com",
		"api.github.com",
		"pypi.org",
		"files.pythonhosted.org",
		"*.pythonhosted.org",
		"registry.npmjs.org",
		"*.npmjs.org",
		"osv.dev",
		"api.osv.dev",
	}
	defaultClaudeGatewayBlockedHostPatterns = []string{
		"statsig",
		"sentry",
		"telemetry",
		"segment",
		"posthog",
		"amplitude",
		"datadog",
		"update",
		"autoupdate",
	}
)

func DefaultClaudeGatewayInterceptHosts() []string {
	return append([]string(nil), defaultClaudeGatewayInterceptHosts...)
}

func DefaultClaudeGatewayForwardHosts() []string {
	return append([]string(nil), defaultClaudeGatewayForwardHosts...)
}

func DefaultClaudeGatewayBlockedHostPatterns() []string {
	return append([]string(nil), defaultClaudeGatewayBlockedHostPatterns...)
}

type Config struct {
	ListenAddr                string `json:"listen_addr"`
	DataDir                   string `json:"data_dir"`
	MasterKeyFile             string `json:"master_key_file,omitempty"`
	IdentityKeyFile           string `json:"identity_key_file,omitempty"`
	DiagnosticAliasKeyFile    string `json:"diagnostic_alias_key_file,omitempty"`
	DiagnosticsDir            string `json:"diagnostics_dir,omitempty"`
	RuntimeIdentityKey        []byte `json:"-"`
	RuntimeDiagnosticAliasKey []byte `json:"-"`
	DatabasePath              string `json:"database_path"`
	StorageDriver             string `json:"storage_driver"`
	PostgresDSN               string `json:"postgres_dsn,omitempty"`
	RedisURL                  string `json:"redis_url,omitempty"`
	NodeID                    string `json:"node_id"`
	UpstreamBaseURL           string `json:"upstream_base_url"`
	OpenAIAPIUpstreamBaseURL  string `json:"openai_api_upstream_base_url"`
	OAuthTokenURL             string `json:"oauth_token_url"`
	ClientVersion             string `json:"client_version"`
	// CompatibilityManifest keeps the client identity tuple and optional fallback
	// model metadata current without making network availability a startup gate.
	// "official" reads allowlisted upstream release metadata and protects its A/B
	// snapshots with a digest; "signed_custom" additionally requires an Ed25519
	// signature from CompatibilityManifestPublicKey.
	CompatibilityManifestEnabled      bool   `json:"compatibility_manifest_enabled"`
	CompatibilityManifestSource       string `json:"compatibility_manifest_source"`
	CompatibilityManifestURL          string `json:"compatibility_manifest_url,omitempty"`
	CompatibilityManifestPublicKey    string `json:"compatibility_manifest_public_key,omitempty"`
	CompatibilityManifestRefreshHours int    `json:"compatibility_manifest_refresh_hours"`
	CompatibilityManifestMaxStaleDays int    `json:"compatibility_manifest_max_stale_days"`
	DefaultGroup                      string `json:"default_group"`
	Virtual2MEnabled                  bool   `json:"-"`
	VirtualContextWindow              int64  `json:"-"`
	VirtualContextLedgerTTLSeconds    int64  `json:"-"`
	StickyWaitMillis                  int    `json:"sticky_wait_millis"`
	// AdmissionWaitMillis bounds the per-account concurrency/token-budget backpressure
	// wait. It is retained for one-version config compatibility; the scheduler now
	// uses the cancellation-aware request deadline instead of a shorter queue timer.
	AdmissionWaitMillis   int   `json:"admission_wait_millis"`
	RequestTimeoutSeconds int   `json:"request_timeout_seconds"`
	ConnectTimeoutSeconds int   `json:"connect_timeout_seconds"`
	ShutdownDrainSeconds  int   `json:"shutdown_drain_seconds"`
	MaxBodyBytes          int64 `json:"max_body_bytes"`
	// BodyV2Enabled switches the bounded replayable body pipeline. Disable for one
	// release to roll back to the legacy in-memory reader without changing protocols.
	BodyV2Enabled            bool  `json:"body_v2_enabled"`
	BodyMemoryThresholdBytes int64 `json:"body_memory_threshold_bytes"`
	// BodyMemoryBudgetBytes is process-wide. Zero selects min(256 MiB, 12.5% of the cgroup/system memory limit).
	BodyMemoryBudgetBytes int64 `json:"body_memory_budget_bytes"`
	BodySpoolMaxBytes     int64 `json:"body_spool_max_bytes"`
	// BodyDiskReserveBytes optionally raises the automatic emergency reserve. Zero
	// keeps the automatic max(128 MiB, 2% of the spool filesystem) reserve.
	BodyDiskReserveBytes     int64  `json:"body_disk_reserve_bytes"`
	BodySpoolDir             string `json:"body_spool_dir"`
	UsageJournalEnabled      bool   `json:"usage_journal_enabled"`
	UsageJournalDir          string `json:"usage_journal_dir"`
	UsageJournalSegmentBytes int64  `json:"usage_journal_segment_bytes"`
	AccountTokenBudget       int64  `json:"account_token_budget"`
	// SchedulerIndexEnabled selects the immutable candidate index/power-of-two path.
	// Disabling it retains the full-scan compatibility path for one release.
	SchedulerIndexEnabled   bool `json:"scheduler_index_enabled"`
	ResourceHeadroomPercent int  `json:"resource_headroom_percent"`
	AdmissionCPUEnabled     bool `json:"admission_cpu_enabled"`
	SSEFlushBatchBytes      int  `json:"sse_flush_batch_bytes"`
	SSEFlushMaxDelayMillis  int  `json:"sse_flush_max_delay_millis"`
	SSETailFlushBytes       int  `json:"sse_tail_flush_bytes"`
	// SQLiteIncrementalVacuumEnabled opts maintenance into incremental page
	// reclamation only after the database has been explicitly migrated to
	// auto_vacuum=INCREMENTAL. It is off by default because enabling that SQLite
	// mode requires a one-time maintenance VACUUM.
	SQLiteIncrementalVacuumEnabled bool `json:"sqlite_incremental_vacuum_enabled"`
	// IntelligentRoutingEnabled is the master switch for the intelligent routing
	// mechanism: per-group fallback chains (GroupFallbacks) evaluated through
	// SelectAcross so an empty group is skipped instantly, instant selection when
	// the pool has a free account (no queue pre-gate), and cooldown trial
	// selection so accounts whose quota recovered but whose cooldown state is
	// stale still receive traffic.
	IntelligentRoutingEnabled bool `json:"intelligent_routing_enabled"`
	// GroupFallbacks maps an account-pool group to the ordered list of groups a
	// request should fail over to when the primary group cannot serve it (empty
	// pool, or every matching account blocked). The chain is evaluated atomically
	// with the primary via SelectAcross; groups without any compatible account
	// contribute zero candidates and are skipped instantly.
	GroupFallbacks map[string][]string `json:"group_fallbacks"`
	// SafetySessionRotationGroups opts specific user groups into upstream session
	// rotation. When a user group is present with value true, a Codex Responses
	// terminal that carries the safety_buffering control field ("security-not-
	// displayed" — the codex protocol's withheld-content signal) makes the gateway
	// rotate that binding's upstream RootSessionID to a fresh generated id. The
	// downstream session mapping is unchanged; only the upstream sees a new
	// session, and the single following turn detaches from the old response chain.
	// Keyed by user-group id; a group is opted in only when its value is true.
	SafetySessionRotationGroups map[string]bool `json:"safety_session_rotation_groups"`
	ContextJournalTTLSeconds    int             `json:"context_journal_ttl_seconds"`
	// ContextJournalMaxRows / ContextJournalMaxMB bound the encrypted replay journal on
	// low-config VPS hosts. When either is exceeded the disk guard evicts the rows with
	// the lowest expires_at first — which, thanks to sliding TTL, are the least-recently
	// resumed chains — so active long tasks are preserved while disk stays bounded. 0
	// disables that dimension.
	ContextJournalMaxRows int `json:"context_journal_max_rows"`
	ContextJournalMaxMB   int `json:"context_journal_max_mb"`
	// GoalContinuity persists the durable, cross-account context chain used to
	// recover long-running Codex / Claude Code goals after a restart.  It is kept
	// separate from context_journal: the latter remains a short-lived v1 replay
	// fallback during the migration, while this chain is bounded and incremental.
	GoalContinuityEnabled      bool    `json:"goal_continuity_enabled"`
	GoalLegacyJournalDualWrite bool    `json:"goal_legacy_journal_dual_write"`
	GoalRetentionDays          int     `json:"goal_retention_days"`
	GoalStorageMaxMB           int     `json:"goal_storage_max_mb"`
	GoalCompressionChunkRatio  float64 `json:"goal_compression_chunk_ratio"`
	GoalCompressionMaxStages   int     `json:"goal_compression_max_stages"`
	GoalLeaseSeconds           int     `json:"goal_lease_seconds"`
	GoalHeartbeatSeconds       int     `json:"goal_heartbeat_seconds"`
	GoalCompressionConcurrency int     `json:"goal_compression_concurrency"`
	GoalChunkFormatV2          bool    `json:"goal_chunk_format_v2"`
	// CodexSessionMappingEnabled enables the strict native Responses context
	// engine. Normal turns use encrypted identity metadata plus HMAC aliases; when
	// the bound account or previous_response_id is lost, the encrypted goal chain
	// can rebuild one fresh root so active work survives a pool refill.
	CodexSessionMappingEnabled       bool `json:"codex_session_mapping_enabled"`
	CodexSessionMappingRetentionDays int  `json:"codex_session_mapping_retention_days"`
	CodexCPAStrict                   bool `json:"codex_cpa_strict"`
	// CodexStatelessPassthrough makes native Codex /v1/responses fully self-contained
	// (CPA-style). When explicitly enabled, the durable session-mapping /
	// goal-continuity / context-journal engine is bypassed and every turn strips
	// previous_response_id + x-codex-turn-state, so any account can serve any turn.
	// It is off by default because it trades away native continuation and its
	// server-side cache semantics; strict mapping plus durable Goal replay is the
	// lossless default recovery path.
	CodexStatelessPassthrough bool   `json:"codex_stateless_passthrough"`
	AdminToken                string `json:"admin_token"`
	// TrustedProxyCIDRs controls when forwarding headers may affect client IP,
	// cookie security, or generated public URLs. Direct internet clients cannot
	// spoof X-Forwarded-* unless their immediate peer is in this list.
	TrustedProxyCIDRs     []string `json:"trusted_proxy_cidrs"`
	SidecarTimeoutSeconds int      `json:"sidecar_timeout_seconds"`
	// DefaultSidecarEndpoint, when set by install/runtime configuration, seeds a
	// local curl_cffi_sidecar egress profile. Admins still decide whether accounts
	// bind to it.
	DefaultSidecarEndpoint string `json:"default_sidecar_endpoint"`
	// DefaultSidecarChainProxy, when set, is the upstream proxy the seeded
	// curl_cffi_sidecar egress chains through (the sidecar makes the JA3-replayed
	// request, then routes it via this proxy). It is environment-specific: locally it
	// may point at a developer's Clash/loopback proxy (e.g. http://127.0.0.1:7897),
	// but on a VPS it MUST be left empty (no such proxy exists there) so the sidecar
	// egresses directly. Kept in config — never hardcoded — so the local/VPS
	// difference is a single line and the value never leaks into code or the shipped DB.
	DefaultSidecarChainProxy string `json:"default_sidecar_chain_proxy"`
	// SidecarImpersonate names the curl_cffi impersonation target the sidecar
	// presents (its TLS/JA3 + HTTP2 fingerprint), e.g. "chrome120". Informational
	// here: the live value is read by the sidecar process from
	// CODEX_POOL_SIDECAR_IMPERSONATE. The pool surfaces it (admin UI / identity
	// view) so operators can see which fingerprint a sidecar-bound account presents
	// and keep it consistent with the impersonated client. Empty = sidecar default.
	SidecarImpersonate string `json:"sidecar_impersonate"`
	// EgressFingerprintEngine selects how upstream JA3/TLS+HTTP2 fingerprinting is done:
	// "inprocess" (default) uses the in-process Go tls-client (uTLS fork + fhttp) which
	// reproduces each provider's validated wire profile (Claude's captured Bun ClientHello;
	// Chrome_120 for browser-shaped traffic) without a separate process, localhost hop, or
	// doubled sockets; "sidecar" routes fingerprinted egress through the external Python
	// curl_cffi sidecar and remains a fully-supported switchable fallback. Both engines carry
	// the same provider profile; validate with /admin/egress-fingerprint-check after deploy.
	EgressFingerprintEngine string `json:"egress_fingerprint_engine"`
	// IdentitySecret seeds the deterministic per-account virtual identity
	// (User-Agent, session ids, device profile, env values). Set a unique value
	// per deployment so profiles are not predictable across installs. When empty
	// a built-in default is used (still deterministic and per-account unique).
	IdentitySecret string `json:"identity_secret"`
	// IdentityConvergenceMode controls only the virtual device fingerprint.
	// "account" (default) derives ONE device per account, stable across every egress,
	// so rotating exit IP no longer mints a new installation id; "off" preserves the
	// legacy per-account/per-egress derivation; "full" converges new virtual devices
	// deployment-wide. Durable native session/thread mappings remain separate, and
	// existing mappings retain their stored device.
	IdentityConvergenceMode string `json:"identity_convergence_mode"`
	// WebSearchEnabled makes the gateway ensure a web_search tool is present on
	// /v1/responses requests (the "联网搜索" AT path). WebSearchToolType overrides
	// the injected tool type (default "web_search").
	WebSearchEnabled  bool   `json:"web_search_enabled"`
	WebSearchToolType string `json:"web_search_tool_type"`
	// Claude/Anthropic relay. ClaudeUpstreamBaseURL defaults to
	// https://api.anthropic.com. ClaudeSensitiveWords are operator-supplied
	// strings that must be scrubbed (100%) from the Claude data stream.
	ClaudeUpstreamBaseURL string   `json:"claude_upstream_base_url"`
	ClaudeSensitiveWords  []string `json:"claude_sensitive_words"`
	// ClaudeForceDirect, when true, makes the Claude/Anthropic upstream always use
	// the direct/proxy (Go stdlib) transport even when the serving account is bound
	// to a curl_cffi_sidecar egress or has an account-level sidecar transport wrapper.
	// A wrapper is removed while its underlying HTTP/SOCKS/WARP exit is retained; a
	// legacy primary sidecar falls back to true direct. This is an ESCAPE HATCH only:
	// the default in-process engine and the optional sidecar both present the captured
	// Claude/Bun fingerprint, whereas forced direct exposes the Go standard library's
	// ClientHello (itself a relay-detection signal). Leave false unless a deployment
	// explicitly accepts that weaker transport shape.
	ClaudeForceDirect bool `json:"claude_force_direct"`
	// ClaudeOAuthTokenURL / ClaudeOAuthClientID drive Claude OAuth (sk-ant-oat)
	// token refresh. Defaults target the standard Claude Code OAuth client.
	ClaudeOAuthTokenURL string `json:"claude_oauth_token_url"`
	ClaudeOAuthClientID string `json:"claude_oauth_client_id"`
	// Web-login (paste-back) OAuth knobs for the "generate URL → browser login →
	// paste redirected URL/code" account import flow (internal/api/oauth.go). All
	// have built-in defaults (the official client values); set them only to track an
	// upstream rotation without recompiling. CodexOAuthTokenURL also seeds the Codex
	// refresh endpoint when OAuthTokenURL is unset.
	CodexOAuthAuthURL            string `json:"codex_oauth_auth_url"`
	CodexOAuthTokenURL           string `json:"codex_oauth_token_url"`
	CodexOAuthClientID           string `json:"codex_oauth_client_id"`
	CodexOAuthRedirectURI        string `json:"codex_oauth_redirect_uri"`
	CodexOAuthScope              string `json:"codex_oauth_scope"`
	ClaudeOAuthAuthURL           string `json:"claude_oauth_auth_url"`
	ClaudeOAuthRedirectURI       string `json:"claude_oauth_redirect_uri"`
	ClaudeOAuthScope             string `json:"claude_oauth_scope"`
	AntigravityOAuthAuthURL      string `json:"antigravity_oauth_auth_url"`
	AntigravityOAuthTokenURL     string `json:"antigravity_oauth_token_url"`
	AntigravityOAuthClientID     string `json:"antigravity_oauth_client_id"`
	AntigravityOAuthClientSecret string `json:"antigravity_oauth_client_secret"`
	AntigravityOAuthRedirectURI  string `json:"antigravity_oauth_redirect_uri"`
	AntigravityOAuthScope        string `json:"antigravity_oauth_scope"`
	// Client-version fingerprints. These are sent upstream verbatim and therefore
	// become a "time fingerprint" if they drift behind the real shipping clients
	// (an old version pinned forever is as detectable as a fake one). They are
	// exposed as config so a deployment can track the real Codex/Claude Code
	// releases WITHOUT a recompile. Empty = the built-in default (see identity
	// package constants), which should track the current shipping client.
	CodexCLIVersionOverride  string `json:"codex_cli_version"`
	ClaudeCLIVersionOverride string `json:"claude_cli_version"`
	// ClaudeAttributionFingerprint, when true, makes the pool append the real Claude
	// Code attribution fingerprint to the billing block's cc_version (the client's
	// message-derived 3-hex SHA256 suffix). Defaults ON: official 2.1.236–2.1.241
	// binaries verified LIVE on 2026-08-24 (every cc_version carries the suffix;
	// hash matches the algorithm). The env flag CODEX_POOL_CLAUDE_ATTRIBUTION_FINGERPRINT
	// and a signed_custom manifest attribution_suffix assertion ("live"/"plain")
	// override it, so the pool follows the server's remote-config state.
	ClaudeAttributionFingerprint bool `json:"claude_attribution_fingerprint"`
	// CodexJA3 selects the TLS ClientHello fingerprint the curl_cffi sidecar replays
	// for OAuth Codex traffic. Empty (DEFAULT) = the sidecar's native Chrome
	// impersonation — see upstream.resolveCodexJA3. The "real Codex" aliases
	// ("codex-cli"/"real"/"native"/"rust"/"codex") ALSO resolve to Chrome: verified
	// against the Codex source (other_codex), the real client does no JA3 spoofing
	// (vanilla reqwest 0.12 + rustls 0.23), its JA3 carries the 0xFF SCSV value
	// curl_cffi/BoringSSL cannot list as a cipher, and chatgpt.com's CF edge whitelists
	// the Chrome JA3 curl_cffi reproduces natively — so matching it buys nothing and
	// used to 502 every request. Operators who insist can still set an explicit JA3
	// string (sanitized of unlistable SCSV values, then best-effort replayed; the
	// sidecar degrades to Chrome rather than 502 if it still can't). "off"/"none"/
	// "disabled"/"-"/"chrome" also keep Chrome. Only applies to the curl_cffi_sidecar
	// egress (the direct/proxy stdlib transport cannot set a JA3).
	CodexJA3Override string `json:"codex_ja3"`
	// ClaudeJA3 selects the TLS ClientHello fingerprint for Claude/Anthropic traffic.
	// Empty (DEFAULT) and "claude-cli"/"real"/"native" use the ClientHello captured
	// from Claude Code 2.1.226's native Bun binary (identity.ClaudeJA3 plus the complete
	// in-process extension spec). An explicit JA3/named profile overrides it. "off"/
	// "none"/"disabled"/"-"/"chrome" select the browser compatibility profile.
	ClaudeJA3Override string `json:"claude_ja3"`
	// ClaudeNodeVersion is the Node runtime version reported in
	// X-Stainless-Runtime-Version (the current native build reports runtime=node). Empty = default.
	ClaudeNodeVersion string `json:"claude_node_version"`
	// Kiro CLI wire version and regional service planes. These are hot-reloadable
	// through the settings registry; endpoint overrides on an individual credential
	// win, while legacy official q.<region>.amazonaws.com values are translated to
	// the operation-specific runtime/management host.
	KiroVersion           string `json:"kiro_version"`
	KiroNodeVersion       string `json:"kiro_node_version"`
	KiroDefaultAuthRegion string `json:"kiro_default_auth_region"`
	KiroDefaultAPIRegion  string `json:"kiro_default_api_region"`
	// KiroDefaultThinking is retained for config-file compatibility. Kiro inference
	// now enforces native adaptive thinking at maximum effort; false is ignored so
	// neither a legacy setting nor a downstream request can silently lower quality.
	KiroDefaultThinking          bool     `json:"kiro_default_thinking"`
	KiroCacheMode                string   `json:"kiro_cache_mode"`
	KiroEndpointAllowlist        []string `json:"kiro_endpoint_allowlist"`
	KiroCacheUnreportedThreshold int      `json:"kiro_cache_unreported_threshold"`
	SchedulerHeartbeatSeconds    int      `json:"scheduler_heartbeat_seconds"`
	// StreamKeepAliveSeconds is the general downstream SSE keepalive interval for the
	// no-rule relay path. When the upstream is silent this long, a provider-appropriate
	// protocol keepalive frame (Codex response.in_progress / Claude ping) is emitted so
	// an intermediary or client does not close a long streaming task before
	// response.completed. 0 disables it (leaving the byte-for-byte fast relay paths
	// untouched); the value is capped well below common intermediary idle timeouts at
	// read time. Default 15.
	StreamKeepAliveSeconds int `json:"stream_keepalive_seconds"`
	// StreamStallRecoverySeconds bounds how long an open upstream stream may emit no
	// bytes before the relay treats it as stalled and enters the same lossless
	// continuation path used for an EOF without a terminal event. 0 disables stall
	// recovery. Default 360 seconds, just beyond the upstream's normal idle window.
	StreamStallRecoverySeconds int `json:"stream_stall_recovery_seconds"`
	// StreamAutoContinueEnabled is the master switch for the auto-continue subsystem:
	// when a streaming response ends WITHOUT its terminal event (Codex
	// response.completed / Anthropic message_stop), the relay re-issues the request
	// once with an appended "continue" turn (re-injecting the partial output so the
	// prompt-cache prefix stays intact) and stitches the continuation into a single
	// coherent downstream response. Default OFF — it re-issues upstream traffic, so an
	// operator opts in explicitly. Never fabricates content; the only synthetic text is
	// the operator-configured continue instruction sent upstream, never to the client.
	StreamAutoContinueEnabled bool `json:"stream_auto_continue_enabled"`
	// StreamContinueText is the user-turn instruction sent upstream when auto-continue
	// (or the #3 safety-buffering continue) re-issues a truncated stream. Default English;
	// an operator may set any language. Sent ONLY upstream, never surfaced downstream.
	StreamContinueText string `json:"stream_continue_text"`
	// StreamAutoContinueMaxAttempts bounds how many times a single request may be
	// auto-continued before the relay gives up and ends the (partial) stream cleanly.
	// Default 1 ("send one continue"); capped low to bound re-issue amplification.
	StreamAutoContinueMaxAttempts int `json:"stream_auto_continue_max_attempts"`
	// ClaudeStainlessVersion is the @anthropic-ai/sdk (Stainless) version reported
	// in X-Stainless-Package-Version. It is a SEPARATE axis from the claude-cli
	// version. Empty = the built-in/ per-account default.
	ClaudeStainlessVersion string `json:"claude_stainless_version"`
	// SensitiveWords are scrubbed from BOTH providers' streams (in addition to
	// ClaudeSensitiveWords for Claude). CodexIdentityScrub opts the Codex path
	// into request/response home-directory & cwd scrubbing; it is off by default
	// so the Codex fast path stays byte-for-byte raw passthrough.
	SensitiveWords     []string `json:"sensitive_words"`
	CodexIdentityScrub bool     `json:"codex_identity_scrub"`
	// IdentityOSSource selects where the virtual identity's OS comes from:
	// "vps" (default/empty) auto-detects the deployed host; "downstream" infers
	// the OS family from each request so the reported OS matches the downstream's
	// real (preserved) working directory; "diverse" draws each account from the
	// full cross-OS pool (use with per-account egress so OS variety matches IP
	// variety). Note: an account bound to its own egress is auto-diversified even
	// under "vps".
	IdentityOSSource string `json:"identity_os_source"`
	// ConversationIsolation ("串号隔离") namespaces the forwarded Codex
	// conversation correlators (conversation_id / x-codex-* / prompt_cache_key) to
	// deterministic per-account values so a rate-limited or risk-flagged session on
	// one account can never contaminate another account that later serves the same
	// conversation. Default on. It is cache-neutral (the namespaced value is stable
	// per account+conversation) and only a control surface — the live value can be
	// toggled at runtime from the admin UI (persisted in the settings table); this
	// is the boot default. Turning it off forwards the downstream identifiers
	// verbatim (max cross-account cache sharing, no isolation guarantee).
	ConversationIsolation bool `json:"conversation_isolation"`
	// ClaudeCacheControlInject auto-injects Anthropic cache_control breakpoints on
	// the OpenAI-compatible → Claude path (which otherwise emits none, getting zero
	// prompt caching). Default on; quality/behavior unchanged, only cached-token
	// billing drops. Runtime-toggleable from the admin UI.
	ClaudeCacheControlInject bool `json:"claude_cache_control_inject"`
	// ClaudeCacheMode selects the prompt-cache planner. "stable_safe" keeps the
	// conservative stable-prefix behavior; "max_hit" spends the four Anthropic
	// breakpoints to maximize real cache reads, including a latest-tail write point
	// for the next turn when enabled.
	ClaudeCacheMode string `json:"claude_cache_mode"`
	// ClaudeCacheAffinityPolicy controls Claude account affinity. "legacy" preserves
	// the pre-optimization route key order; "balanced" prefers true Claude sessions
	// and stable Anthropic cache prefixes before falling back to coarse routing.
	ClaudeCacheAffinityPolicy string `json:"claude_cache_affinity_policy"`
	// ClaudeCacheBreakpointPolicy controls injected Claude cache_control placement.
	// "legacy" preserves the old full injection behavior; "balanced" keeps full
	// breakpoints for true/stable routes and uses coarse_safe for coarse routes;
	// "stable_prefix_safe" prefers stable prefixes without filling volatile tails;
	// "coarse_safe" only marks stable tools and non-billing system prefixes.
	ClaudeCacheBreakpointPolicy string `json:"claude_cache_breakpoint_policy"`
	// ClaudeCacheOptimizationRollout is a JSON scope for Claude cache optimization
	// rollout. Supported keys: groups, api_key_hash_prefixes, account_ids
	// (breakpoints only), and percent. "{}" means all Claude traffic; model-level
	// scoping is intentionally unsupported.
	ClaudeCacheOptimizationRollout string `json:"claude_cache_optimization_rollout"`
	// ClaudeNativeCacheBreakpointInject conservatively adds one cache_control marker
	// to a recognized Claude Code auto-context prefix when the native request has spare
	// marker budget. It never changes prompt text, tools, thinking, auth, or billing
	// blocks. Default on. Runtime key: claude_native_cache_breakpoint_inject.
	ClaudeNativeCacheBreakpointInject bool `json:"claude_native_cache_breakpoint_inject"`
	// ClaudeCacheLatestTailWrite is used by claude_cache_mode=max_hit. When true,
	// the newest cacheable message tail receives a cache_control marker so the next
	// turn can read the full grown context. The current turn does not benefit from
	// that newest marker.
	ClaudeCacheLatestTailWrite bool `json:"claude_cache_latest_tail_write"`
	// ClaudeCachePrewarmMode controls optional cache-write warmups before the real
	// Claude request: "off", "async", or "sync_extreme".
	ClaudeCachePrewarmMode string `json:"claude_cache_prewarm_mode"`
	// ClaudeCacheDiagnosticsEnabled opts into Anthropic cache diagnostics headers and
	// request diagnostics wiring when route-local previous message ids are available.
	ClaudeCacheDiagnosticsEnabled bool `json:"claude_cache_diagnostics_enabled"`
	// ClaudeCacheSingleflightEnabled makes concurrent requests with the same cache
	// prefix wait behind the first writer to reduce parallel cold misses.
	ClaudeCacheSingleflightEnabled bool `json:"claude_cache_singleflight_enabled"`
	// CodexCacheSingleflightEnabled coordinates concurrent cold writes for the same
	// account, model and prompt_cache_key until the leader returns response headers.
	CodexCacheSingleflightEnabled bool `json:"codex_cache_singleflight_enabled"`
	// CodexPromptCacheKeyShards deterministically fans generated Codex CLI UUID
	// cache keys across stable session shards. One preserves the legacy single key.
	CodexPromptCacheKeyShards int `json:"codex_prompt_cache_key_shards"`
	// CodexGPT56ExplicitCacheMode controls GPT-5.6 explicit cache breakpoints on
	// native OpenAI API-key transports: observe, auto, or off.
	CodexGPT56ExplicitCacheMode string `json:"codex_gpt56_explicit_cache_mode"`
	// ClaudeCacheLosslessBlockSplit enables byte-preserving text-block splitting in
	// max-hit mode when the split can be round-tripped exactly.
	ClaudeCacheLosslessBlockSplit bool `json:"claude_cache_lossless_block_split"`
	// ClaudeCacheTTL selects the injected cache_control TTL: "" / "5m" = standard
	// ephemeral (5 min), "1h" = extended (1 hour; higher hit rate across gaps, the
	// cache write costs ~2x). Applies only to auto-injected breakpoints.
	ClaudeCacheTTL string `json:"claude_cache_ttl"`
	// ClaudeCacheTTLRouteAware, when enabled, reserves the configured 1h extended cache
	// for true-conversation routes (a stable session/thread id) and lets collapsible
	// routes (stable-prefix / coarse / anonymous) fall back to the 5m ephemeral cache —
	// bounding staleness on prefixes several distinct requests can share and trimming the
	// 1h write cost. Default OFF preserves the historical "configured TTL applies to every
	// route" behavior. TTL is retention only; it never changes model, context, or reasoning.
	ClaudeCacheTTLRouteAware bool `json:"claude_cache_ttl_route_aware"`
	// ClaudeCCHSigning is retained only so older config files continue to parse.
	// Claude Code 2.1.226 emits no cch, so the request path ignores this
	// deprecated setting and the default is false.
	ClaudeCCHSigning bool `json:"claude_cch_signing"`
	// BanDetectionEnabled turns on automatic classification of upstream responses
	// into ban / rate-limit / auth / region verdicts (heuristics ported from the
	// reference Codex Manager). Default on.
	BanDetectionEnabled bool `json:"ban_detection_enabled"`
	// BanAutoDelete deletes an account on a HIGH-confidence ban (deactivated /
	// suspended) after writing an audit record. Default on (per ops requirement);
	// set false to quarantine-long instead of deleting. Only ever triggers on the
	// terminal "banned" verdict, never on a recoverable rate-limit/auth/region one.
	BanAutoDelete bool `json:"ban_auto_delete"`
	// QuarantineDurationHours controls how long an account stays quarantined after
	// a ban or permission-denied verdict (re-login required). Default 72h (3 days),
	// down from the prior hard-coded 30 days. Set 0 to disable auto-quarantine
	// (manual review only). Admins can always clear quarantine manually via the UI.
	QuarantineDurationHours int `json:"quarantine_duration_hours"`
	// HealthTestClearsQuarantine: when true (DEFAULT), a successful health-test
	// (alive=true, non-ban) auto-clears any existing quarantine on that account so
	// operators don't have to manually unquarantine after fixing the underlying
	// issue (e.g. re-login, scope update). Set false to require manual review.
	HealthTestClearsQuarantine bool `json:"health_test_clears_quarantine"`
	// MaxConcurrentUpstream is retained solely so older configuration files continue
	// to parse. Process-wide admission limiting was removed; account/egress capacity
	// is handled by the scheduler's unbounded, cancellation-aware wait queue.
	MaxConcurrentUpstream int `json:"max_concurrent_upstream"`
	// RateLimitGuardEnabled enables proactive rate-limit avoidance: honor
	// Retry-After on 429 and pre-emptively cool an account whose x-ratelimit-
	// remaining headers approach zero, so the pool rotates BEFORE hitting the wall.
	// Default on. Reduces limit-hits without touching quality/experience.
	RateLimitGuardEnabled bool `json:"rate_limit_guard_enabled"`
	// Codex reset credits are ChatGPT-side active reset credits for the Codex 7d
	// rate-limit window. Auto consume is default-on; allow/deny lists are emergency
	// rollout controls.
	CodexResetCreditsAutoEnabled           bool     `json:"codex_reset_credits_auto_enabled"`
	CodexResetCreditsUnknownConsumeEnabled bool     `json:"codex_reset_credits_unknown_consume_enabled"`
	CodexResetCreditsAccountDenylist       []string `json:"codex_reset_credits_account_denylist"`
	CodexResetCreditsAccountAllowlist      []string `json:"codex_reset_credits_account_allowlist"`
	CodexResetCreditsGroupAllowlist        []string `json:"codex_reset_credits_group_allowlist"`
	// SeamlessFailover transparently retries a request on a fresh account when the
	// chosen account returns a recoverable error (rate limit / region / auth / ban)
	// — but ONLY for self-contained requests (no previous_response_id), which carry
	// their full input and so can move accounts losslessly. Stateful Codex turns
	// (previous_response_id, a server-side-state delta) are never silently switched
	// (that would lose context); they stay strict-sticky. Default on.
	SeamlessFailover bool `json:"seamless_failover"`
	// StrictStickyMaxCooldownSeconds is the maximum cooldown duration (in seconds)
	// a strict-sticky request will wait before allowing failover to another account.
	// When a bound account's cooldown exceeds this threshold, the scheduler treats
	// the request as non-strict and selects a fresh account instead of returning
	// a 409 Conflict error. This prevents long cooldowns (e.g., 30+ minutes) from
	// blocking stateful conversations when other accounts are available.
	// Default: 60 seconds. Set 0 to never allow strict-sticky failover (original behavior).
	StrictStickyMaxCooldownSeconds int `json:"strict_sticky_max_cooldown_seconds"`
	// StatefulStickyWaitSeconds bounds how long a request carrying server-side state
	// waits for its already-bound account to free local capacity. 0 follows
	// RequestTimeoutSeconds; the effective wait is always capped by request timeout.
	StatefulStickyWaitSeconds int `json:"stateful_sticky_wait_seconds"`
	// CooldownWaitMaxSeconds is the maximum time (in seconds) to wait for an account
	// to come off cooldown when ALL accounts in the group are currently cooling down.
	// When set to a positive value, if no accounts are immediately available but at
	// least one will become available within this window, the scheduler waits for it
	// instead of returning an error. This reduces 429 cascades during burst rate-limiting.
	// Default: 30 seconds. Set 0 to disable waiting (original behavior).
	CooldownWaitMaxSeconds int `json:"cooldown_wait_max_seconds"`
	// FailoverMaxAttempts bounds how many accounts a single request may try before
	// surfacing the error (incl. the first). Default 3.
	FailoverMaxAttempts int `json:"failover_max_attempts"`
	// StreamFailoverHoldMemoryBytes bounds how much successful-looking SSE data is
	// buffered in memory while the gateway waits to prove a stream did not end in a
	// retryable account error. Default 8MiB.
	StreamFailoverHoldMemoryBytes int64 `json:"-"`
	// StreamFailoverHoldDiskBytes is an OPTIONAL spill budget for the same hold-back
	// buffer. Default 0 disables disk spill entirely; set a positive value only when
	// the deployment has a known-safe temp volume. When memory+disk budgets are
	// exceeded before a clean stream completes, the request tries another account
	// instead of writing partial output downstream.
	StreamFailoverHoldDiskBytes int64 `json:"-"`
	// StreamFailoverHoldTempDir selects the directory for optional spill files. Empty
	// uses os.TempDir. Ignored when StreamFailoverHoldDiskBytes is 0.
	StreamFailoverHoldTempDir string `json:"-"`
	// AccountRecheckEnabled turns on the cooldown→health-recheck loop: an account
	// benched after an upstream error stays out of the candidate pool until a
	// background liveness probe ("测活") confirms it recovered. Default on. With it
	// off, a benched account silently re-enters rotation the instant its cooldown
	// elapses (the original behavior).
	AccountRecheckEnabled bool `json:"account_recheck_enabled"`
	// AccountRecheckIntervalSeconds is how often the recheck loop scans for benched
	// accounts whose cooldown has elapsed and probes them. Default 20s.
	AccountRecheckIntervalSeconds int `json:"account_recheck_interval_seconds"`
	// AccountRecheckBackoffSeconds is how long to re-bench an account whose recheck
	// probe still fails, before the next probe attempt. Default 120s.
	AccountRecheckBackoffSeconds int `json:"account_recheck_backoff_seconds"`
	// AccountFailureStreakThreshold is how many consecutive upstream 5xx responses
	// from one account bench it for recheck. Only rate/quota signals used to bench an
	// account, so an upstream that failed *every* request — a dead relay behind a
	// custom provider — stayed in rotation indefinitely: production diagnostics show
	// one provider account serving 1512 consecutive 503s across 14 unbroken hours,
	// each also consuming a fallback attempt against a second group. A success, or a
	// non-5xx response, resets the streak. 0 disables the breaker.
	// Runtime key: account_failure_streak_threshold. Default 5.
	AccountFailureStreakThreshold int `json:"account_failure_streak_threshold"`
	// AccountFailureStreakCooldownSeconds is how long a streak-benched account is
	// cooled. It is benched for recheck, so the liveness probe (not the clock alone)
	// readmits it. Runtime key: account_failure_streak_cooldown_seconds. Default 300s.
	AccountFailureStreakCooldownSeconds int `json:"account_failure_streak_cooldown_seconds"`
	// CodexPromptCacheRetention is retained only for config-file compatibility.
	// Current codex-rs 0.144.x sends no prompt_cache_retention field on HTTP or WS,
	// so the relay does not inject it and strips legacy downstream values. Cache
	// affinity uses prompt_cache_key instead. Runtime key: codex_prompt_cache_retention.
	CodexPromptCacheRetention string `json:"codex_prompt_cache_retention"`
	// Codex install settings used by the generated ~/.codex/config.toml setup script
	// (GET /file/<key>). Model/provider/auth are always managed. Empty effort,
	// approval, and sandbox values preserve the client's existing settings; non-empty
	// values are explicit operator overrides validated against codex-rs enums. A zero
	// context window likewise preserves the client/model default.
	CodexInstallModel          string `json:"codex_install_model"`           // default "gpt-5.6-sol"
	CodexInstallEffort         string `json:"codex_install_effort"`          // default empty: preserve Codex config
	CodexInstallApprovalPolicy string `json:"codex_install_approval_policy"` // default empty: preserve Codex config
	CodexInstallSandboxMode    string `json:"codex_install_sandbox_mode"`    // default empty: preserve Codex config
	CodexInstallContextWindow  int    `json:"codex_install_context_window"`  // default 0: preserve Codex/model context window
	// SuperInstructLocalEnabled is retained for configuration compatibility only.
	// Runtime Super-Instruct capability is the intersection of the resolved user
	// group policy and the request's explicit client opt-in header.
	SuperInstructLocalEnabled bool `json:"super_instruct_local_enabled"`
	// Claude gateway local runtime / egress policy. These are surfaced through the
	// admin System config page and returned by /v1/gateway/identity so installed
	// gateway binaries can follow operator-edited policy without recompilation.
	ClaudeGatewayInterceptHosts         []string `json:"claude_gateway_intercept_hosts"`
	ClaudeGatewayForwardHosts           []string `json:"claude_gateway_forward_hosts"`
	ClaudeGatewayBlockedHostPatterns    []string `json:"claude_gateway_blocked_host_patterns"`
	ClaudeGatewayUnknownTargetPolicy    string   `json:"claude_gateway_unknown_target_policy"` // block|forward
	ClaudeGatewayDisableNonessentialEnv bool     `json:"claude_gateway_disable_nonessential_env"`
	ClaudeGatewayStrictLinuxDefault     bool     `json:"claude_gateway_strict_linux_default"`
	ClaudeGatewayVirtualDNSServers      []string `json:"claude_gateway_virtual_dns_servers"`
	// DefaultRegisterMethod is the registration engine used when a trigger (admin batch
	// or auto-refill) does not name a method. Runtime-overridable via the
	// "default_register_method" setting. Default "protocol_v2"; every method remains
	// gated by artifact/provider/egress readiness and a matching one-account canary.
	DefaultRegisterMethod string `json:"default_register_method"`
	// RegistrationEgressPoolID is the runtime-editable default pool used only for
	// launching registration tasks when a request does not name an egress_id or
	// registration_egress_pool_id. Registered accounts still keep their own direct
	// account_egress_bindings default until an admin changes that account.
	RegistrationEgressPoolID string `json:"registration_egress_pool_id"`
	// LeakScrubEnabled hides pool-internal upstream signals from the downstream
	// client: the x-codex-*/openai-model/x-ratelimit-* response headers, the
	// informational rate-limit SSE frames, and limit/quota/overload/billing error
	// bodies (which are neutralized into a generic, account-agnostic error). A
	// relay must not reveal it is fronting a rotating pool of accounts. Default on;
	// runtime-toggleable from the admin UI (settings key "leak_scrub").
	LeakScrubEnabled bool `json:"leak_scrub_enabled"`
	// TokenSaveEnabled turns on server-side token compression: large tool-result blocks
	// in relayed requests are conservatively compressed (rtk-style — ANSI strip, blank/
	// dupe collapse, head+tail truncation) before forwarding upstream, cutting billed
	// input tokens. Default OFF (mutating request content can affect model output);
	// runtime-toggleable from the admin UI (settings key "token_save_enabled").
	TokenSaveEnabled bool `json:"token_save_enabled"`
	// RequireDownstreamKey rejects gateway requests whose bearer key is not a known,
	// enabled api_keys row. Default off (open relay) to preserve existing behavior;
	// a matched key's group + forced model/effort policy applies regardless of this
	// flag. Turn on to make the relay private.
	RequireDownstreamKey bool `json:"require_downstream_key"`
	// ModelProbeIntervalHours controls the background re-probe of each active
	// account's upstream-supported models (so /v1/models stays current without a
	// manual probe). 0 disables the background refresh. Imports always probe once.
	ModelProbeIntervalHours int `json:"model_probe_interval_hours"`
	// ModelQualityMonitorEnabled is opt-in because every scheduled probe consumes a
	// small amount of upstream quota. Models empty means all advertised models in
	// every active group. Verdicts are group×model, not account-level.
	ModelQualityMonitorEnabled    bool     `json:"model_quality_monitor_enabled"`
	ModelQualityIntervalMinutes   int      `json:"model_quality_interval_minutes"`
	ModelQualityReasoningEffort   string   `json:"model_quality_reasoning_effort"`
	ModelQualityModels            []string `json:"model_quality_models"`
	ModelQualityDegradedThreshold int      `json:"model_quality_degraded_threshold"`
	ModelQualityHistoryDays       int      `json:"model_quality_history_days"`
	// GeoProbeURL is the IP/geo echo endpoint used to auto-detect a proxy egress's
	// exit IP + region (the request is made THROUGH the proxy, so it reflects the
	// real exit location and doubles as a health check). Empty uses a sensible
	// default. The response is parsed for common fields (ip, country/countryCode,
	// region, city).
	GeoProbeURL string `json:"geo_probe_url"`
	// ── Codex OAuth 自动重登 ──
	// CodexReauthWorkerURL is the local HTTP worker used by manual/auto Codex reauth.
	// The pool server stores encrypted credentials and calls POST /v1/codex/reauth
	// only for Codex/ChatGPT accounts whose reauth config enables it.
	CodexReauthWorkerURL string `json:"codex_reauth_worker_url"`
	// CodexReauthWorkerConcurrency is surfaced for the standalone worker; the pool
	// server itself uses per-account/job de-dupe. Default 1.
	CodexReauthWorkerConcurrency int `json:"codex_reauth_worker_concurrency"`

	// ── WARP CF fallback (multi-exit) ──
	// WarpEnabled turns on the WARP fallback pool. When on, the server ensures a set
	// of warp-* egress profiles exist (one per local wireproxy SOCKS5 exit) and the
	// CF ladder moves a CF-hit account onto a WARP exit (≤WarpAccountsPerExit
	// accounts per exit) instead of just benching it. The exits themselves are
	// provisioned by scripts/install.sh --with-warp (wgcf + wireproxy); this flag and
	// the count/port describe that provisioned topology to the server.
	WarpEnabled bool `json:"warp_enabled"`
	// WarpExitCount is how many wireproxy exits install.sh provisioned. Each exit i
	// (1..N) listens on WarpExitBasePort+i-1 and is a distinct WARP IP.
	WarpExitCount int `json:"warp_exit_count"`
	// WarpExitBasePort is the first local SOCKS5 port (default 40000); exit i uses
	// WarpExitBasePort+i-1.
	WarpExitBasePort int `json:"warp_exit_base_port"`
	// WarpAccountsPerExit caps how many accounts may share one WARP exit IP (the
	// "一个组3个号" requirement; default 3) so a flagged exit's blast radius is small.
	WarpAccountsPerExit int `json:"warp_accounts_per_exit"`
	// WarpExitScript is the path to scripts/warp-exit.sh, which the server execs to
	// re-register an exit's wgcf profile (new WARP IP) when that exit is CF-flagged
	// and the solver could not clear it. Empty disables re-registration.
	WarpExitScript string `json:"warp_exit_script"`
	// ── cf_clearance solver (last-resort CF rung) ──
	// CFSolverEnabled turns on the FlareSolverr-compatible solver rung: when a WARP
	// exit itself is CF-blocked, the server asks the solver to solve the upstream
	// host THROUGH that exit's proxy and injects the returned cf_clearance (+UA+exit
	// IP) via the existing injected-cookie plumbing before re-registering the exit.
	CFSolverEnabled bool `json:"cf_solver_enabled"`
	// CFSolverURL is the solver's FlareSolverr-compatible base URL (e.g.
	// http://127.0.0.1:8191). FlareSolverr v3 uses sessions.create/request.get/
	// sessions.destroy; implementations exposing only the historical stateless
	// /v1 {cmd:request.get} contract remain supported as a compatibility fallback.
	CFSolverURL string `json:"cf_solver_url"`
	// ── Registration (auto phone registration) ──
	RegistrationEnabled     bool `json:"registration_enabled"`
	RegistrationConcurrency int  `json:"registration_concurrency"`
	RegistrationTimeout     int  `json:"registration_timeout"`
	// RegistrationDefaultGroup is the account group used only by registration
	// triggers that do not name a group. It is the canonical replacement for the
	// historical email_registration_group key.
	RegistrationDefaultGroup string `json:"registration_default_group"`
	DefaultSMSProvider       string `json:"default_sms_provider"`
	DefaultMailboxProvider   string `json:"default_mailbox_provider"`
	DefaultCaptchaProvider   string `json:"default_captcha_provider"`

	// ── SMS multi-platform smart country selection ──
	// SMSPlatformStrategy is "auto" (default) to let the Manager pick the best platform +
	// country from live platform statistics (success ranking + price + inventory + the
	// preferred-country list), or "manual" to use SMSManualCountry verbatim. "auto" honors
	// the operator's preferred-country priority (BR > CO > PL by default) while still
	// weighing the platforms' own same-day success ranking.
	SMSPlatformStrategy string `json:"sms_platform_strategy"`
	// SMSPreferredCountries is the comma-separated ISO-2 priority list the auto strategy
	// weights as a tiebreaker bonus (default "BR,CO,PL" — BR highest because live tests +
	// platform stats show it has the best OpenAI SMS success rate).
	SMSPreferredCountries string `json:"sms_preferred_countries"`
	// SMSManualCountry is the ISO-2 code used verbatim when SMSPlatformStrategy == "manual".
	SMSManualCountry string `json:"sms_manual_country"`
	// SMSStatsTopN is how many top candidates the auto strategy tries in order before giving
	// up (default 3). Higher = more fallback resilience at the cost of more number purchases
	// on a bad day.
	SMSStatsTopN int `json:"sms_stats_top_n"`
	// SMSMinPrice/SMSMaxPrice bound automatic number purchases in USD. Zero leaves
	// the corresponding side open; the same bounds are shown on the registration page.
	SMSMinPrice float64 `json:"sms_min_price"`
	SMSMaxPrice float64 `json:"sms_max_price"`

	// ── CLIPProxy API whitelist mode + exit-region validation ──
	// CliproxyAPIBase is the cliproxy white-api base URL (default https://api.cliproxy.io),
	// used when an egress profile has proxy_auth_mode="api_whitelist" to extract ip:port.
	CliproxyAPIBase string `json:"cliproxy_api_base"`
	// CliproxyAPIKey is a fallback account API token used by api_whitelist egress profiles
	// that don't carry their own per-egress key. Per-egress keys (egress.proxy_api_key) win.
	CliproxyAPIKey string `json:"cliproxy_api_key"`
	// CliproxyValidateRegion (default true) makes the node registrar confirm the exit IP's
	// country matches the SMS number's country before launching the browser, re-rotating
	// the sid/IP when it doesn't (geo mismatch → OpenAI withholds the SMS).
	CliproxyValidateRegion bool `json:"cliproxy_validate_region"`

	// CodexPreferSidecarJA3OverWS, when true (default), makes a Codex request from a
	// sidecar-bound account take the HTTP/SSE Responses path (which carries the real
	// Codex JA3 through the curl_cffi sidecar) INSTEAD of the WebSocket Responses
	// transport for version-gated models — because the WS dialer cannot replay the
	// Codex JA3 (it would dial with Go-stdlib TLS, losing the fingerprint the sidecar
	// binding exists to present). Set false to always use WS for those models even on a
	// sidecar egress (e.g. if a model strictly requires the WS transport).
	CodexPreferSidecarJA3OverWS bool `json:"codex_prefer_sidecar_ja3_over_ws"`

	// ── Thinking (Deep Reasoning) ──
	// ThinkingEnabled is the master switch for thinking configuration injection.
	// When false, all thinking configuration is stripped. Default false (opt-in).
	ThinkingEnabled bool `json:"thinking_enabled"`
	// ThinkingDefaultMode selects the default thinking configuration mode:
	// "level" (discrete levels: minimal/low/medium/high/xhigh/max/ultra),
	// "budget" (numeric token budget), "auto" (dynamic), "none" (disabled).
	// Only effective when ThinkingEnabled=true. Default "level".
	ThinkingDefaultMode string `json:"thinking_default_mode"`
	// ThinkingDefaultLevel is the default discrete thinking level when
	// ThinkingDefaultMode="level". Examples: "minimal", "low", "medium", "high",
	// "xhigh", "max", "ultra". Default "medium".
	ThinkingDefaultLevel string `json:"thinking_default_level"`
	// ThinkingDefaultBudget is the default numeric thinking budget (token count)
	// when ThinkingDefaultMode="budget". Default 8192.
	ThinkingDefaultBudget int `json:"thinking_default_budget"`
	// ThinkingProviders contains per-provider thinking configuration overrides.
	// Keys are provider names (e.g., "claude", "codex"); values specify the mode
	// and level/budget to use for that provider, overriding the global default.
	ThinkingProviders map[string]ThinkingOverride `json:"thinking_providers"`
	// ThinkingModels contains per-model thinking configuration overrides.
	// Keys are model names (e.g., "claude-opus-4-8", "gpt-5.2"); values specify
	// the mode and level/budget to use for that model, overriding both provider
	// and global defaults. This is the highest priority configuration source
	// (after model name suffix).
	ThinkingModels map[string]ThinkingOverride `json:"thinking_models"`

	// ── Gateway Reliability Layer (Model Reliability Layer / anti-degradation) ──
	// The reliability layer makes a thin, zero-config downstream client robust by
	// injecting high-priority developer rules, classifying each request's task type +
	// risk, flooring the reasoning effort by risk, maintaining per-conversation
	// working_state, and guarding output against fabricated tool/test/command results.
	// Every knob is runtime-toggleable from the settings table (same overlay as the
	// other flags); these are the boot defaults.
	//
	// GatewayReliabilityEnabled is the MASTER switch. Default OFF so the relay is
	// byte-for-byte unchanged until an operator opts in (key "gateway_reliability").
	GatewayReliabilityEnabled bool `json:"gateway_reliability_enabled"`
	// GatewayReliabilityEffortFloor lets the risk classifier RAISE reasoning effort to
	// a per-risk floor (medium/high/xhigh) — it never lowers an operator-forced effort,
	// so a high-risk task can't be downgraded by the downstream. Default true (only
	// effective when the master switch is on). Key "gateway_reliability_effort_floor".
	GatewayReliabilityEffortFloor bool `json:"gateway_reliability_effort_floor"`
	// GatewayReliabilityGuardMode selects the output guard: "off", "lenient" (default;
	// high-precision fabrication checks only) or "strict" (adds completeness checks).
	// Key "gateway_reliability_guard_mode".
	GatewayReliabilityGuardMode string `json:"gateway_reliability_guard_mode"`
	// GatewayReliabilityModel, when non-empty, is the model the layer forces upstream
	// when enabled AND neither the key nor the group forced one — the spec's model
	// routing axis. Default empty = NEVER override the client's model, because a pooled
	// account may not have a given model and forcing a missing one would 502. Set it to
	// a model you are sure every account in the default group supports. Key
	// "gateway_reliability_model".
	GatewayReliabilityModel string `json:"gateway_reliability_model"`
	// GatewayReliabilityRepair allows ONE upstream repair re-ask on a NON-streaming
	// response that fails the guard, before falling back to a deterministic downgrade.
	// Default false: deterministic downgrade only (cheaper, and cannot itself
	// hallucinate). Key "gateway_reliability_repair".
	GatewayReliabilityRepair bool `json:"gateway_reliability_repair"`
}

// ThinkingOverride specifies a thinking configuration override for a provider or model.
type ThinkingOverride struct {
	// Mode is the thinking mode: "level", "budget", "auto", or "none"
	Mode string `json:"mode"`
	// Level is the discrete thinking level (only used when Mode="level")
	// Valid values: "minimal", "low", "medium", "high", "xhigh", "max", "ultra"
	Level string `json:"level,omitempty"`
	// Budget is the numeric thinking budget in tokens (only used when Mode="budget")
	// Valid range: typically 512-128000, model-dependent
	Budget int `json:"budget,omitempty"`
}

func Default() Config {
	return Config{
		ListenAddr:                        DefaultListenAddr,
		DataDir:                           "data",
		DatabasePath:                      DefaultDatabasePath,
		StorageDriver:                     DefaultStorageDriver,
		UpstreamBaseURL:                   DefaultUpstreamBaseURL,
		OpenAIAPIUpstreamBaseURL:          DefaultOpenAIAPIUpstreamBaseURL,
		ClientVersion:                     DefaultClientVersion,
		ClaudeAttributionFingerprint:      true, // verified live on official 2.1.236–2.1.241 (2026-08-24)
		CompatibilityManifestEnabled:      true,
		CompatibilityManifestSource:       "official",
		CompatibilityManifestRefreshHours: DefaultCompatibilityManifestRefreshHours,
		CompatibilityManifestMaxStaleDays: DefaultCompatibilityManifestMaxStaleDays,
		CodexOAuthAuthURL:                 DefaultCodexOAuthAuthURL,
		CodexOAuthTokenURL:                DefaultCodexOAuthTokenURL,
		CodexOAuthClientID:                DefaultCodexOAuthClientID,
		CodexOAuthRedirectURI:             DefaultCodexOAuthRedirectURI,
		CodexOAuthScope:                   DefaultCodexOAuthScope,
		ClaudeOAuthAuthURL:                DefaultClaudeOAuthAuthURL,
		ClaudeOAuthTokenURL:               DefaultClaudeOAuthTokenURL,
		ClaudeOAuthClientID:               DefaultClaudeOAuthClientID,
		ClaudeOAuthRedirectURI:            DefaultClaudeOAuthRedirectURI,
		ClaudeOAuthScope:                  DefaultClaudeOAuthScope,
		AntigravityOAuthAuthURL:           DefaultAntigravityOAuthAuthURL,
		AntigravityOAuthTokenURL:          DefaultAntigravityOAuthTokenURL,
		AntigravityOAuthClientID:          DefaultAntigravityOAuthClientID,
		AntigravityOAuthClientSecret:      DefaultAntigravityOAuthClientSecret,
		AntigravityOAuthRedirectURI:       DefaultAntigravityOAuthRedirectURI,
		AntigravityOAuthScope:             DefaultAntigravityOAuthScope,
		DefaultGroup:                      DefaultGroupName,
		Virtual2MEnabled:                  false,
		VirtualContextWindow:              DefaultVirtualWindow,
		VirtualContextLedgerTTLSeconds:    DefaultVirtualContextLedgerTTLSeconds,
		StickyWaitMillis:                  DefaultStickyWaitMillis,
		AdmissionWaitMillis:               DefaultAdmissionWaitMillis,
		RequestTimeoutSeconds:             DefaultRequestTimeoutSec,
		ConnectTimeoutSeconds:             DefaultConnectTimeoutSeconds,
		ShutdownDrainSeconds:              DefaultShutdownDrainSec,
		MaxBodyBytes:                      DefaultMaxBodyBytes,
		BodyV2Enabled:                     true,
		BodyMemoryThresholdBytes:          DefaultBodyMemoryThresholdBytes,
		BodySpoolMaxBytes:                 DefaultBodySpoolMaxBytes,
		BodyDiskReserveBytes:              DefaultBodyDiskReserveBytes,
		UsageJournalEnabled:               true,
		UsageJournalSegmentBytes:          DefaultUsageJournalSegmentBytes,
		AccountTokenBudget:                DefaultAccountTokenBudget,
		SchedulerIndexEnabled:             true,
		IntelligentRoutingEnabled:         true,
		ResourceHeadroomPercent:           10,
		AdmissionCPUEnabled:               true,
		SSEFlushBatchBytes:                4 * 1024,
		SSEFlushMaxDelayMillis:            3,
		SSETailFlushBytes:                 8 * 1024,
		SQLiteIncrementalVacuumEnabled:    false,
		ContextJournalTTLSeconds:          3600,
		ContextJournalMaxRows:             50000,
		ContextJournalMaxMB:               200,
		GoalContinuityEnabled:             true,
		GoalLegacyJournalDualWrite:        false,
		GoalRetentionDays:                 7,
		GoalStorageMaxMB:                  DefaultGoalStorageMaxMB,
		GoalCompressionChunkRatio:         0.70,
		GoalCompressionMaxStages:          16,
		GoalLeaseSeconds:                  90,
		GoalHeartbeatSeconds:              15,
		GoalCompressionConcurrency:        1,
		GoalChunkFormatV2:                 false,
		CodexSessionMappingEnabled:        true,
		CodexSessionMappingRetentionDays:  7,
		CodexCPAStrict:                    true,
		CodexStatelessPassthrough:         false,
		TrustedProxyCIDRs:                 []string{"127.0.0.0/8", "::1/128"},
		SidecarTimeoutSeconds:             120,
		EgressFingerprintEngine:           "inprocess",
		IdentityConvergenceMode:           "account",
		KiroVersion:                       "0.11.107",
		KiroNodeVersion:                   "22.22.0",
		KiroDefaultAuthRegion:             "us-east-1",
		KiroDefaultAPIRegion:              "us-east-1",
		KiroDefaultThinking:               true,
		KiroCacheMode:                     "auto",
		KiroCacheUnreportedThreshold:      DefaultKiroCacheUnreportedThreshold,
		SchedulerHeartbeatSeconds:         15,
		StreamKeepAliveSeconds:            15,
		StreamStallRecoverySeconds:        360,
		StreamContinueText:                "Please continue from exactly where you left off, without repeating anything.",
		StreamAutoContinueMaxAttempts:     1,
		ConversationIsolation:             true,
		// Auto-inject Claude cache_control on the OpenAI-compat path by default so
		// that path benefits from prompt caching like native Claude Code does.
		ClaudeCacheControlInject:          true,
		ClaudeCacheMode:                   "stable_safe",
		ClaudeCacheAffinityPolicy:         "balanced",
		ClaudeCacheBreakpointPolicy:       "stable_prefix_safe",
		ClaudeCacheOptimizationRollout:    "{}",
		ClaudeNativeCacheBreakpointInject: true,
		ClaudeCacheLatestTailWrite:        true,
		ClaudeCachePrewarmMode:            "off",
		ClaudeCacheDiagnosticsEnabled:     false,
		ClaudeCacheSingleflightEnabled:    true,
		CodexCacheSingleflightEnabled:     true,
		CodexPromptCacheKeyShards:         4,
		CodexGPT56ExplicitCacheMode:       "observe",
		ClaudeCacheLosslessBlockSplit:     false,
		ClaudeCacheTTL:                    "1h",
		ClaudeCCHSigning:                  false,
		BanDetectionEnabled:               true,
		BanAutoDelete:                     false,
		QuarantineDurationHours:           72,
		HealthTestClearsQuarantine:        true,
		MaxConcurrentUpstream:             64,
		// Session 32: Changed RateLimitGuardEnabled default to TRUE. Proactive
		// cooldown is essential for auto-switching when accounts exhaust their quota.
		// Without it, the pool will keep sending to an exhausted account until it
		// gets a 429, causing poor UX. The guard honors rate-limit headers from
		// successful responses and preemptively rotates to fresh accounts.
		RateLimitGuardEnabled:                  true,
		CodexResetCreditsAutoEnabled:           true,
		CodexResetCreditsUnknownConsumeEnabled: true,
		SeamlessFailover:                       true,
		FailoverMaxAttempts:                    3,
		StreamFailoverHoldMemoryBytes:          DefaultStreamFailoverHoldMemoryBytes,
		StreamFailoverHoldDiskBytes:            DefaultStreamFailoverHoldDiskBytes,
		AccountRecheckEnabled:                  true,
		AccountRecheckIntervalSeconds:          DefaultAccountRecheckIntervalSeconds,
		AccountRecheckBackoffSeconds:           DefaultAccountRecheckBackoffSeconds,
		AccountFailureStreakThreshold:          DefaultAccountFailureStreakThreshold,
		AccountFailureStreakCooldownSeconds:    DefaultAccountFailureStreakCooldownSeconds,
		CodexPromptCacheRetention:              "",
		CodexInstallModel:                      "gpt-5.6-sol",
		CodexInstallEffort:                     "",
		CodexInstallApprovalPolicy:             "",
		CodexInstallSandboxMode:                "",
		CodexInstallContextWindow:              0,
		SuperInstructLocalEnabled:              false,
		ClaudeGatewayInterceptHosts:            DefaultClaudeGatewayInterceptHosts(),
		ClaudeGatewayForwardHosts:              DefaultClaudeGatewayForwardHosts(),
		ClaudeGatewayBlockedHostPatterns:       DefaultClaudeGatewayBlockedHostPatterns(),
		ClaudeGatewayUnknownTargetPolicy:       "forward",
		// Parity mode is the default: keep Claude Code's model discovery, feature
		// flags, updates, and tool streaming behavior unless the operator opts into
		// the stricter privacy switch.
		ClaudeGatewayDisableNonessentialEnv: false,
		ClaudeGatewayStrictLinuxDefault:     false,
		DefaultRegisterMethod:               "protocol_v2",
		StrictStickyMaxCooldownSeconds:      DefaultStrictStickyMaxCooldownSeconds,
		StatefulStickyWaitSeconds:           0,
		CooldownWaitMaxSeconds:              DefaultCooldownWaitMaxSeconds,
		LeakScrubEnabled:                    true,
		ModelProbeIntervalHours:             DefaultModelProbeIntervalHours,
		ModelQualityMonitorEnabled:          false,
		ModelQualityIntervalMinutes:         DefaultModelQualityIntervalMinutes,
		ModelQualityReasoningEffort:         DefaultModelQualityReasoningEffort,
		ModelQualityDegradedThreshold:       DefaultModelQualityDegradedThreshold,
		ModelQualityHistoryDays:             DefaultModelQualityHistoryDays,
		GeoProbeURL:                         DefaultGeoProbeURL,
		CodexReauthWorkerURL:                DefaultCodexReauthWorkerURL,
		CodexReauthWorkerConcurrency:        1,
		WarpExitBasePort:                    40000,
		WarpAccountsPerExit:                 3,
		RegistrationConcurrency:             1,
		RegistrationTimeout:                 300,
		RegistrationDefaultGroup:            DefaultGroupName,
		CodexPreferSidecarJA3OverWS:         true,
		SMSPlatformStrategy:                 "auto",
		SMSPreferredCountries:               "BR,CO,PL",
		SMSStatsTopN:                        3,

		// Thinking defaults: disabled by default (opt-in feature)
		ThinkingEnabled:       false,
		ThinkingDefaultMode:   "level",
		ThinkingDefaultLevel:  "medium",
		ThinkingDefaultBudget: 8192,
		ThinkingProviders:     make(map[string]ThinkingOverride),
		ThinkingModels:        make(map[string]ThinkingOverride),

		// Gateway reliability layer: master OFF (opt-in). The sub-knobs carry sensible
		// values so flipping the master on yields good behavior without further config.
		GatewayReliabilityEnabled:     false,
		GatewayReliabilityEffortFloor: true,
		GatewayReliabilityGuardMode:   "lenient",
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return Config{}, err
		}
		if err := applyLegacyRegistrationConfig(raw, &cfg); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return Config{}, err
	}
	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch strings.ToLower(strings.TrimSpace(c.StorageDriver)) {
	case "", "sqlite":
		return nil
	case "postgres":
		if strings.TrimSpace(c.PostgresDSN) == "" {
			return errors.New("storage_driver=postgres requires postgres_dsn or CODEX_POOL_POSTGRES_DSN")
		}
		if strings.TrimSpace(c.RedisURL) == "" {
			return errors.New("storage_driver=postgres requires redis_url or CODEX_POOL_REDIS_URL")
		}
		return nil
	default:
		return errors.New("storage_driver must be sqlite or postgres")
	}
}

func (c *Config) StickyWait() time.Duration {
	return time.Duration(c.StickyWaitMillis) * time.Millisecond
}

func (c Config) SchedulerHeartbeat() time.Duration {
	seconds := c.SchedulerHeartbeatSeconds
	if seconds <= 0 {
		seconds = 15
	}
	return time.Duration(seconds) * time.Second
}

// AdmissionWait is retained for callers reading the compatibility setting. The
// scheduler itself waits on the request context so capacity never causes fail-fast.
func (c *Config) AdmissionWait() time.Duration {
	if c.AdmissionWaitMillis <= 0 {
		return 0
	}
	return time.Duration(c.AdmissionWaitMillis) * time.Millisecond
}

func (c *Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
}

// ConnectTimeout bounds connection establishment independently from the much
// longer inference/stream idle timeout.
func (c *Config) ConnectTimeout() time.Duration {
	seconds := c.ConnectTimeoutSeconds
	if seconds <= 0 {
		seconds = DefaultConnectTimeoutSeconds
	}
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func (c Config) EffectiveBodyMemoryBudgetBytes() int64 {
	if c.BodyMemoryBudgetBytes > 0 {
		return c.BodyMemoryBudgetBytes
	}
	limit := detectedMemoryLimitBytes()
	if limit <= 0 {
		return DefaultBodyMemoryBudgetMaxBytes
	}
	budget := limit / 8
	if budget > DefaultBodyMemoryBudgetMaxBytes {
		return DefaultBodyMemoryBudgetMaxBytes
	}
	if budget < DefaultBodyMemoryThresholdBytes {
		return DefaultBodyMemoryThresholdBytes
	}
	return budget
}

func detectedMemoryLimitBytes() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "" || value == "max" {
			continue
		}
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 && parsed < 1<<62 {
			return parsed
		}
	}
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func (c *Config) StatefulStickyWait() time.Duration {
	waitSeconds := c.StatefulStickyWaitSeconds
	if waitSeconds <= 0 {
		waitSeconds = c.RequestTimeoutSeconds
	}
	if waitSeconds <= 0 {
		waitSeconds = DefaultRequestTimeoutSec
	}
	wait := time.Duration(waitSeconds) * time.Second
	requestTimeout := c.RequestTimeout()
	if requestTimeout <= 0 {
		requestTimeout = time.Duration(DefaultRequestTimeoutSec) * time.Second
	}
	if wait > requestTimeout {
		return requestTimeout
	}
	return wait
}

// ShutdownDrainTimeout bounds the graceful drain on SIGTERM/SIGINT: the server
// stops accepting new connections and waits up to this long for in-flight requests
// (notably long-lived SSE streams) to finish before exiting. Make the systemd unit's
// TimeoutStopSec >= this so systemd does not SIGKILL mid-drain.
func (c *Config) ShutdownDrainTimeout() time.Duration {
	return time.Duration(c.ShutdownDrainSeconds) * time.Second
}

func (c *Config) SidecarTimeout() time.Duration {
	return time.Duration(c.SidecarTimeoutSeconds) * time.Second
}

// CodexCLIVersionOrDefault / ClaudeCLIVersionOrDefault / ClaudeNodeVersionOrDefault
// resolve the effective client-version fingerprints: the operator override when
// set, otherwise the supplied built-in default (the identity package's current
// constant). Centralizing this lets a deployment track the real shipping clients
// from config so the upstream fingerprint never silently rots into a stale,
// detectable "time fingerprint".
func (c *Config) CodexCLIVersionOrDefault(def string) string {
	if v := strings.TrimSpace(c.CodexCLIVersionOverride); v != "" {
		return v
	}
	return def
}

func (c *Config) ClaudeCLIVersionOrDefault(def string) string {
	if v := strings.TrimSpace(c.ClaudeCLIVersionOverride); v != "" {
		return v
	}
	return def
}

// ClaudeAttributionFingerprintEnabled reports whether the billing block should carry
// the real Claude Code message-derived attribution suffix (see cloak's
// computeClaudeAttributionFingerprint). Defaults on (the verified live client
// state); the operator env flag and a signed_custom manifest assertion override it.
func (c *Config) ClaudeAttributionFingerprintEnabled() bool {
	return c.ClaudeAttributionFingerprint
}

func (c *Config) ClaudeNodeVersionOrDefault(def string) string {
	if v := strings.TrimSpace(c.ClaudeNodeVersion); v != "" {
		return v
	}
	if strings.TrimSpace(def) != "" {
		return def
	}
	return DefaultClaudeNodeVersion
}

// ClaudeStainlessVersionOrDefault resolves the X-Stainless-Package-Version
// fingerprint: the operator override when set (forces one value across the whole
// deployment), otherwise the supplied per-account default so accounts vary.
func (c *Config) ClaudeStainlessVersionOrDefault(def string) string {
	if v := strings.TrimSpace(c.ClaudeStainlessVersion); v != "" {
		return v
	}
	return def
}

// FingerprintWarnings returns advisory warnings about identity/fingerprint version
// overrides that, while accepted, risk forming a client-version combination no real
// client ever shipped — itself a relay-detection signal. It NEVER mutates config or
// blocks startup; main logs each line at boot so an operator who hand-pins one axis is
// nudged to set the others coherently.
//
// The checks are deliberately conservative: they validate the SHAPE and COHERENCE of
// operator INPUT, not a guessed claude-cli ↔ @anthropic-ai/sdk ↔ Node compatibility
// matrix (which we do not have and which would produce false positives). Empty
// overrides (the default — self-consistent per-account values) produce no warnings.
func (c *Config) FingerprintWarnings() []string {
	var w []string
	cli := strings.TrimSpace(c.ClaudeCLIVersionOverride)
	sdk := strings.TrimSpace(c.ClaudeStainlessVersion)
	node := strings.TrimSpace(c.ClaudeNodeVersion)

	// Coherence: pinning some-but-not-all Claude version axes mixes a fixed value with
	// per-account/default ones. A real claude-cli ships these as one coherent set.
	set := 0
	for _, v := range []string{cli, sdk, node} {
		if v != "" {
			set++
		}
	}
	if set > 0 && set < 3 {
		w = append(w, "claude fingerprint: only some of claude_cli_version / claude_stainless_version / "+
			"claude_node_version are set — a pinned axis combined with rotating defaults can form a "+
			"client-version combo no real client shipped; set all three coherently or leave all unset")
	}
	// Shape: a set override must look like the real header value the client sends.
	if cli != "" && !looksLikeDotVersion(cli) {
		w = append(w, "claude_cli_version "+strconv.Quote(cli)+" does not look like a semver (e.g. 2.1.226)")
	}
	if sdk != "" && !looksLikeDotVersion(sdk) {
		w = append(w, "claude_stainless_version "+strconv.Quote(sdk)+" does not look like a semver (e.g. 0.94.0)")
	}
	if node != "" && !strings.HasPrefix(node, "v") {
		w = append(w, "claude_node_version "+strconv.Quote(node)+" should start with 'v' (Node convention, e.g. v26.3.0)")
	}
	return w
}

// looksLikeDotVersion reports whether s is a dotted, digits-only version like "2.1.226"
// or "0.94.0" (≥2 segments, every segment a non-empty run of digits).
func looksLikeDotVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func (c *Config) applyEnv() error {
	credentialDirectory := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY"))
	credentialPath := func(name string) string {
		if credentialDirectory == "" {
			return ""
		}
		return filepath.Join(credentialDirectory, name)
	}
	if v := os.Getenv("CODEX_POOL_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("CODEX_POOL_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("CODEX_POOL_MASTER_KEY_FILE"); v != "" {
		c.MasterKeyFile = v
	} else if strings.TrimSpace(c.MasterKeyFile) == "" && credentialDirectory != "" {
		c.MasterKeyFile = credentialPath("master.key")
	}
	if v := os.Getenv("CODEX_POOL_IDENTITY_KEY_FILE"); v != "" {
		c.IdentityKeyFile = v
	} else if strings.TrimSpace(c.IdentityKeyFile) == "" && credentialDirectory != "" {
		c.IdentityKeyFile = credentialPath("identity.key")
	}
	if v := os.Getenv("CODEX_POOL_DIAGNOSTIC_ALIAS_KEY_FILE"); v != "" {
		c.DiagnosticAliasKeyFile = v
	} else if strings.TrimSpace(c.DiagnosticAliasKeyFile) == "" && credentialDirectory != "" {
		c.DiagnosticAliasKeyFile = credentialPath("diagnostic-alias.key")
	}
	if v := os.Getenv("CODEX_POOL_DATABASE"); v != "" {
		c.DatabasePath = v
	}
	if v := os.Getenv("CODEX_POOL_STORAGE_DRIVER"); v != "" {
		c.StorageDriver = v
	}
	if v := os.Getenv("CODEX_POOL_POSTGRES_DSN"); v != "" {
		c.PostgresDSN = v
	}
	if v := os.Getenv("CODEX_POOL_REDIS_URL"); v != "" {
		c.RedisURL = v
	}
	if v := os.Getenv("CODEX_POOL_NODE_ID"); v != "" {
		c.NodeID = v
	}
	if v := os.Getenv("CODEX_POOL_BODY_SPOOL_DIR"); v != "" {
		c.BodySpoolDir = v
	}
	if v := os.Getenv("CODEX_POOL_BODY_V2_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.BodyV2Enabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_USAGE_JOURNAL_DIR"); v != "" {
		c.UsageJournalDir = v
	}
	if v := os.Getenv("CODEX_POOL_USAGE_JOURNAL_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.UsageJournalEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_SCHEDULER_INDEX_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.SchedulerIndexEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_BODY_MEMORY_BUDGET_BYTES"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.BodyMemoryBudgetBytes = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_UPSTREAM_BASE_URL"); v != "" {
		c.UpstreamBaseURL = v
	}
	if v := os.Getenv("CODEX_POOL_OPENAI_API_UPSTREAM_BASE_URL"); v != "" {
		c.OpenAIAPIUpstreamBaseURL = v
	}
	if v := os.Getenv("CODEX_POOL_OAUTH_TOKEN_URL"); v != "" {
		c.OAuthTokenURL = v
	}
	if v := os.Getenv("CODEX_POOL_CLIENT_VERSION"); v != "" {
		c.ClientVersion = v
	}
	if v := os.Getenv("CODEX_POOL_CONNECT_TIMEOUT_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.ConnectTimeoutSeconds = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_IDENTITY_CONVERGENCE_MODE"); v != "" {
		c.IdentityConvergenceMode = v
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.CompatibilityManifestEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_SOURCE"); v != "" {
		c.CompatibilityManifestSource = v
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_URL"); v != "" {
		c.CompatibilityManifestURL = v
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_PUBLIC_KEY"); v != "" {
		c.CompatibilityManifestPublicKey = v
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_REFRESH_HOURS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.CompatibilityManifestRefreshHours = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_COMPATIBILITY_MANIFEST_MAX_STALE_DAYS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.CompatibilityManifestMaxStaleDays = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CODEX_JA3"); v != "" {
		c.CodexJA3Override = v
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_JA3"); v != "" {
		c.ClaudeJA3Override = v
	}
	if v := os.Getenv("CODEX_POOL_CODEX_CLI_VERSION"); v != "" {
		c.CodexCLIVersionOverride = v
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_ATTRIBUTION_FINGERPRINT"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.ClaudeAttributionFingerprint = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_ADMIN_TOKEN"); v != "" {
		c.AdminToken = v
	}
	if path := strings.TrimSpace(os.Getenv("CODEX_POOL_ADMIN_TOKEN_FILE")); path != "" {
		value, err := readSecretFile(path, 4096)
		if err != nil {
			return fmt.Errorf("load admin token credential: %w", err)
		}
		c.AdminToken = value
	} else if path := credentialPath("admin.token"); path != "" {
		if _, err := os.Stat(path); err == nil {
			value, readErr := readSecretFile(path, 4096)
			if readErr != nil {
				return fmt.Errorf("load admin token credential: %w", readErr)
			}
			c.AdminToken = value
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat admin token credential: %w", err)
		}
	}
	if v := os.Getenv("CODEX_POOL_TRUSTED_PROXY_CIDRS"); v != "" {
		c.TrustedProxyCIDRs = strings.Split(v, ",")
	}
	if v := os.Getenv("CODEX_POOL_IDENTITY_SECRET"); v != "" {
		c.IdentitySecret = v
	}
	if v := os.Getenv("CODEX_POOL_WEB_SEARCH"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.WebSearchEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CONVERSATION_ISOLATION"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.ConversationIsolation = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_CACHE_INJECT"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.ClaudeCacheControlInject = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_CACHE_MODE"); v != "" {
		c.ClaudeCacheMode = v
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_CCH_SIGNING"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.ClaudeCCHSigning = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_LEAK_SCRUB"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.LeakScrubEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_REQUIRE_KEY"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.RequireDownstreamKey = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_STICKY_WAIT_MS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.StickyWaitMillis = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_STATEFUL_STICKY_WAIT_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.StatefulStickyWaitSeconds = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_SHUTDOWN_DRAIN_SECONDS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.ShutdownDrainSeconds = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_DEFAULT_SIDECAR_ENDPOINT"); v != "" {
		c.DefaultSidecarEndpoint = v
	}
	if v := os.Getenv("CODEX_POOL_SIDECAR_IMPERSONATE"); v != "" {
		c.SidecarImpersonate = v
	}
	if v := os.Getenv("CODEX_POOL_EGRESS_FINGERPRINT_ENGINE"); v != "" {
		c.EgressFingerprintEngine = v
	}
	if v := os.Getenv("CODEX_POOL_CLAUDE_FORCE_DIRECT"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.ClaudeForceDirect = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CODEX_REAUTH_WORKER_URL"); v != "" {
		c.CodexReauthWorkerURL = v
	}
	if v := os.Getenv("CODEX_POOL_CODEX_REAUTH_WORKER_CONCURRENCY"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.CodexReauthWorkerConcurrency = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_WARP_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.WarpEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_WARP_EXIT_COUNT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.WarpExitCount = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_WARP_EXIT_BASE_PORT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.WarpExitBasePort = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_WARP_ACCOUNTS_PER_EXIT"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			c.WarpAccountsPerExit = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_WARP_EXIT_SCRIPT"); v != "" {
		c.WarpExitScript = v
	}
	if v := os.Getenv("CODEX_POOL_CF_SOLVER_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.CFSolverEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_CF_SOLVER_URL"); v != "" {
		c.CFSolverURL = v
	}
	if v := os.Getenv("CODEX_POOL_CODEX_PREFER_SIDECAR_JA3_OVER_WS"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.CodexPreferSidecarJA3OverWS = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_GATEWAY_RELIABILITY"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.GatewayReliabilityEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_GATEWAY_RELIABILITY_EFFORT_FLOOR"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.GatewayReliabilityEffortFloor = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_GATEWAY_RELIABILITY_GUARD"); v != "" {
		c.GatewayReliabilityGuardMode = v
	}
	if v := os.Getenv("CODEX_POOL_GATEWAY_RELIABILITY_MODEL"); v != "" {
		c.GatewayReliabilityModel = v
	}
	if v := os.Getenv("CODEX_POOL_GATEWAY_RELIABILITY_REPAIR"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.GatewayReliabilityRepair = parsed
		}
	}
	return nil
}

func readSecretFile(path string, maxBytes int64) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("secret file must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file has unsafe permissions %04o", info.Mode().Perm())
	}
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	if info.Size() <= 0 || info.Size() > maxBytes {
		return "", errors.New("secret file has invalid size")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", errors.New("secret file is empty")
	}
	return value, nil
}

func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if strings.TrimSpace(c.DataDir) == "" {
		c.DataDir = "data"
	}
	if c.DatabasePath == "" {
		c.DatabasePath = DefaultDatabasePath
	}
	c.StorageDriver = strings.ToLower(strings.TrimSpace(c.StorageDriver))
	if c.StorageDriver == "" {
		c.StorageDriver = DefaultStorageDriver
	}
	c.PostgresDSN = strings.TrimSpace(c.PostgresDSN)
	c.RedisURL = strings.TrimSpace(c.RedisURL)
	c.NodeID = normalizeNodeID(c.NodeID)
	if c.UpstreamBaseURL == "" {
		c.UpstreamBaseURL = DefaultUpstreamBaseURL
	}
	if c.OpenAIAPIUpstreamBaseURL == "" {
		c.OpenAIAPIUpstreamBaseURL = DefaultOpenAIAPIUpstreamBaseURL
	}
	if c.ClientVersion == "" || dottedVersionLess(c.ClientVersion, DefaultClientVersion) {
		c.ClientVersion = DefaultClientVersion
	}
	switch strings.ToLower(strings.TrimSpace(c.CompatibilityManifestSource)) {
	case "official", "signed_custom":
		c.CompatibilityManifestSource = strings.ToLower(strings.TrimSpace(c.CompatibilityManifestSource))
	default:
		c.CompatibilityManifestSource = "official"
	}
	c.CompatibilityManifestURL = strings.TrimSpace(c.CompatibilityManifestURL)
	c.CompatibilityManifestPublicKey = strings.TrimSpace(c.CompatibilityManifestPublicKey)
	if c.CompatibilityManifestRefreshHours <= 0 {
		c.CompatibilityManifestRefreshHours = DefaultCompatibilityManifestRefreshHours
	}
	if c.CompatibilityManifestRefreshHours > 168 {
		c.CompatibilityManifestRefreshHours = 168
	}
	if c.CompatibilityManifestMaxStaleDays <= 0 {
		c.CompatibilityManifestMaxStaleDays = DefaultCompatibilityManifestMaxStaleDays
	}
	if c.CompatibilityManifestMaxStaleDays > 365 {
		c.CompatibilityManifestMaxStaleDays = 365
	}
	if c.CodexCLIVersionOverride != "" &&
		dottedVersionLess(c.CodexCLIVersionOverride, DefaultClientVersion) &&
		!IsSupportedCodexCLIVersion(c.CodexCLIVersionOverride) {
		c.CodexCLIVersionOverride = DefaultClientVersion
	}
	if c.DefaultGroup == "" {
		c.DefaultGroup = DefaultGroupName
	}
	if c.VirtualContextWindow <= 0 {
		c.VirtualContextWindow = DefaultVirtualWindow
	}
	if c.StickyWaitMillis <= 0 {
		c.StickyWaitMillis = DefaultStickyWaitMillis
	}
	// 0 (unset — e.g. a config predating this field) adopts the default so existing
	// deployments get admission backpressure on update; a negative value is the explicit
	// "disable" escape hatch (AdmissionWait() returns 0 for anything <= 0).
	if c.AdmissionWaitMillis == 0 {
		c.AdmissionWaitMillis = DefaultAdmissionWaitMillis
	}
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = DefaultRequestTimeoutSec
	}
	if c.ConnectTimeoutSeconds <= 0 {
		c.ConnectTimeoutSeconds = DefaultConnectTimeoutSeconds
	}
	if c.ConnectTimeoutSeconds > 60 {
		c.ConnectTimeoutSeconds = 60
	}
	switch strings.ToLower(strings.TrimSpace(c.IdentityConvergenceMode)) {
	case "full":
		c.IdentityConvergenceMode = "full"
	case "off":
		c.IdentityConvergenceMode = "off"
	default:
		c.IdentityConvergenceMode = "account"
	}
	if c.StatefulStickyWaitSeconds < 0 {
		c.StatefulStickyWaitSeconds = 0
	}
	if c.ShutdownDrainSeconds <= 0 {
		c.ShutdownDrainSeconds = DefaultShutdownDrainSec
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.BodyMemoryThresholdBytes <= 0 {
		c.BodyMemoryThresholdBytes = DefaultBodyMemoryThresholdBytes
	}
	if c.BodyMemoryBudgetBytes < 0 {
		c.BodyMemoryBudgetBytes = 0
	}
	if c.BodySpoolMaxBytes <= 0 {
		c.BodySpoolMaxBytes = DefaultBodySpoolMaxBytes
	}
	if c.BodyDiskReserveBytes < 0 {
		c.BodyDiskReserveBytes = DefaultBodyDiskReserveBytes
	}
	if c.UsageJournalSegmentBytes <= 0 {
		c.UsageJournalSegmentBytes = DefaultUsageJournalSegmentBytes
	}
	if c.AccountTokenBudget < 0 {
		c.AccountTokenBudget = 0
	}
	if c.ResourceHeadroomPercent < 10 {
		c.ResourceHeadroomPercent = 10
	}
	// Streaming flush knobs are runtime-tunable, but normalize boot-loaded
	// configs as well so callers that bypass the admin settings registry retain
	// the documented defaults and cannot accidentally disable progress.
	if c.SSEFlushBatchBytes <= 0 {
		c.SSEFlushBatchBytes = 4 * 1024
	}
	if c.SSEFlushMaxDelayMillis < 0 {
		c.SSEFlushMaxDelayMillis = 3
	}
	if c.SSEFlushMaxDelayMillis > 1000 {
		c.SSEFlushMaxDelayMillis = 1000
	}
	if c.SSETailFlushBytes <= 0 {
		c.SSETailFlushBytes = 8 * 1024
	}
	if c.ContextJournalTTLSeconds <= 0 {
		c.ContextJournalTTLSeconds = 3600
	}
	// 0 = that budget dimension is disabled (unbounded); a negative value is meaningless.
	if c.ContextJournalMaxRows < 0 {
		c.ContextJournalMaxRows = 0
	}
	if c.ContextJournalMaxMB < 0 {
		c.ContextJournalMaxMB = 0
	}
	if c.GoalRetentionDays <= 0 {
		c.GoalRetentionDays = 7
	}
	if c.GoalStorageMaxMB <= 0 {
		c.GoalStorageMaxMB = DefaultGoalStorageMaxMB
	}
	if c.GoalCompressionChunkRatio <= 0 || c.GoalCompressionChunkRatio >= 1 {
		c.GoalCompressionChunkRatio = 0.70
	}
	if c.GoalCompressionMaxStages <= 0 {
		c.GoalCompressionMaxStages = 16
	}
	if c.GoalLeaseSeconds <= 0 {
		c.GoalLeaseSeconds = 90
	}
	if c.GoalHeartbeatSeconds <= 0 || c.GoalHeartbeatSeconds >= c.GoalLeaseSeconds {
		c.GoalHeartbeatSeconds = 15
	}
	if c.GoalCompressionConcurrency <= 0 {
		c.GoalCompressionConcurrency = 1
	}
	if c.CodexSessionMappingRetentionDays <= 0 {
		c.CodexSessionMappingRetentionDays = 7
	}
	if c.SidecarTimeoutSeconds <= 0 {
		c.SidecarTimeoutSeconds = 120
	}
	if strings.TrimSpace(c.KiroVersion) == "" {
		c.KiroVersion = "0.11.107"
	}
	if strings.TrimSpace(c.KiroNodeVersion) == "" {
		c.KiroNodeVersion = "22.22.0"
	}
	if strings.TrimSpace(c.KiroDefaultAuthRegion) == "" {
		c.KiroDefaultAuthRegion = "us-east-1"
	}
	if strings.TrimSpace(c.KiroDefaultAPIRegion) == "" {
		c.KiroDefaultAPIRegion = "us-east-1"
	}
	switch strings.ToLower(strings.TrimSpace(c.KiroCacheMode)) {
	case "auto", "observe", "off":
		c.KiroCacheMode = strings.ToLower(strings.TrimSpace(c.KiroCacheMode))
	default:
		c.KiroCacheMode = "auto"
	}
	if c.KiroCacheUnreportedThreshold <= 0 {
		c.KiroCacheUnreportedThreshold = DefaultKiroCacheUnreportedThreshold
	}
	if c.SchedulerHeartbeatSeconds <= 0 {
		c.SchedulerHeartbeatSeconds = 15
	}
	// A negative keepalive is meaningless; clamp to 0 (disabled). 0 is a valid
	// operator choice (off), so it is never forced back to the default here.
	if c.StreamKeepAliveSeconds < 0 {
		c.StreamKeepAliveSeconds = 0
	}
	if c.StreamStallRecoverySeconds < 0 {
		c.StreamStallRecoverySeconds = 0
	}
	if strings.TrimSpace(c.StreamContinueText) == "" {
		c.StreamContinueText = "Please continue from exactly where you left off, without repeating anything."
	}
	if c.StreamAutoContinueMaxAttempts <= 0 {
		c.StreamAutoContinueMaxAttempts = 1
	}
	// Bound re-issue amplification: a truncation loop must never fan out upstream.
	if c.StreamAutoContinueMaxAttempts > 3 {
		c.StreamAutoContinueMaxAttempts = 3
	}
	if c.FailoverMaxAttempts <= 0 {
		c.FailoverMaxAttempts = 3
	}
	if c.StreamFailoverHoldMemoryBytes <= 0 {
		c.StreamFailoverHoldMemoryBytes = DefaultStreamFailoverHoldMemoryBytes
	}
	if c.StreamFailoverHoldDiskBytes < 0 {
		c.StreamFailoverHoldDiskBytes = DefaultStreamFailoverHoldDiskBytes
	}
	if c.AccountRecheckIntervalSeconds <= 0 {
		c.AccountRecheckIntervalSeconds = DefaultAccountRecheckIntervalSeconds
	}
	if c.AccountRecheckBackoffSeconds <= 0 {
		c.AccountRecheckBackoffSeconds = DefaultAccountRecheckBackoffSeconds
	}
	// A negative threshold is a typo, not "disabled"; 0 is the explicit off switch.
	if c.AccountFailureStreakThreshold < 0 {
		c.AccountFailureStreakThreshold = DefaultAccountFailureStreakThreshold
	}
	if c.AccountFailureStreakCooldownSeconds <= 0 {
		c.AccountFailureStreakCooldownSeconds = DefaultAccountFailureStreakCooldownSeconds
	}
	if c.WebSearchEnabled && c.WebSearchToolType == "" {
		c.WebSearchToolType = "web_search"
	}
	if c.ClaudeOAuthTokenURL == "" || sameURL(c.ClaudeOAuthTokenURL, legacyClaudeOAuthTokenURL) || sameURL(c.ClaudeOAuthTokenURL, legacyClaudeOAuthTokenURLAPI) {
		c.ClaudeOAuthTokenURL = DefaultClaudeOAuthTokenURL
	}
	if c.ClaudeOAuthClientID == "" {
		c.ClaudeOAuthClientID = DefaultClaudeOAuthClientID
	}
	if c.CodexOAuthAuthURL == "" {
		c.CodexOAuthAuthURL = DefaultCodexOAuthAuthURL
	}
	if c.CodexOAuthTokenURL == "" {
		c.CodexOAuthTokenURL = DefaultCodexOAuthTokenURL
	}
	if c.CodexOAuthClientID == "" {
		c.CodexOAuthClientID = DefaultCodexOAuthClientID
	}
	if c.CodexOAuthRedirectURI == "" {
		c.CodexOAuthRedirectURI = DefaultCodexOAuthRedirectURI
	}
	if c.CodexOAuthScope == "" {
		c.CodexOAuthScope = DefaultCodexOAuthScope
	}
	if c.AntigravityOAuthAuthURL == "" {
		c.AntigravityOAuthAuthURL = DefaultAntigravityOAuthAuthURL
	}
	if c.AntigravityOAuthTokenURL == "" {
		c.AntigravityOAuthTokenURL = DefaultAntigravityOAuthTokenURL
	}
	if c.AntigravityOAuthClientID == "" {
		c.AntigravityOAuthClientID = DefaultAntigravityOAuthClientID
	}
	if c.AntigravityOAuthClientSecret == "" {
		c.AntigravityOAuthClientSecret = DefaultAntigravityOAuthClientSecret
	}
	if c.AntigravityOAuthRedirectURI == "" {
		c.AntigravityOAuthRedirectURI = DefaultAntigravityOAuthRedirectURI
	}
	if c.AntigravityOAuthScope == "" {
		c.AntigravityOAuthScope = DefaultAntigravityOAuthScope
	}
	if len(c.ClaudeGatewayInterceptHosts) == 0 {
		c.ClaudeGatewayInterceptHosts = DefaultClaudeGatewayInterceptHosts()
	}
	if len(c.ClaudeGatewayForwardHosts) == 0 {
		c.ClaudeGatewayForwardHosts = DefaultClaudeGatewayForwardHosts()
	}
	if len(c.ClaudeGatewayBlockedHostPatterns) == 0 {
		c.ClaudeGatewayBlockedHostPatterns = DefaultClaudeGatewayBlockedHostPatterns()
	}
	switch strings.ToLower(strings.TrimSpace(c.ClaudeGatewayUnknownTargetPolicy)) {
	case "block", "forward":
		c.ClaudeGatewayUnknownTargetPolicy = strings.ToLower(strings.TrimSpace(c.ClaudeGatewayUnknownTargetPolicy))
	default:
		c.ClaudeGatewayUnknownTargetPolicy = "forward"
	}
	if c.ClaudeOAuthAuthURL == "" || sameURL(c.ClaudeOAuthAuthURL, legacyClaudeOAuthAuthURL) {
		c.ClaudeOAuthAuthURL = DefaultClaudeOAuthAuthURL
	}
	if c.ClaudeOAuthRedirectURI == "" || sameURL(c.ClaudeOAuthRedirectURI, legacyClaudeOAuthRedirectURI) {
		c.ClaudeOAuthRedirectURI = DefaultClaudeOAuthRedirectURI
	}
	if c.ClaudeOAuthScope == "" || strings.Join(strings.Fields(c.ClaudeOAuthScope), " ") == legacyClaudeOAuthAuthorizeScope {
		c.ClaudeOAuthScope = DefaultClaudeOAuthScope
	}
	if c.GeoProbeURL == "" {
		c.GeoProbeURL = DefaultGeoProbeURL
	}
	if c.ModelQualityIntervalMinutes < 60 {
		c.ModelQualityIntervalMinutes = DefaultModelQualityIntervalMinutes
	}
	switch strings.ToLower(strings.TrimSpace(c.ModelQualityReasoningEffort)) {
	case "low", "medium", "high":
		c.ModelQualityReasoningEffort = strings.ToLower(strings.TrimSpace(c.ModelQualityReasoningEffort))
	default:
		c.ModelQualityReasoningEffort = DefaultModelQualityReasoningEffort
	}
	if c.ModelQualityDegradedThreshold < 2 {
		c.ModelQualityDegradedThreshold = DefaultModelQualityDegradedThreshold
	}
	if c.ModelQualityHistoryDays <= 0 {
		c.ModelQualityHistoryDays = DefaultModelQualityHistoryDays
	}
	if c.CodexReauthWorkerURL == "" {
		c.CodexReauthWorkerURL = DefaultCodexReauthWorkerURL
	}
	if c.CodexReauthWorkerConcurrency <= 0 {
		c.CodexReauthWorkerConcurrency = 1
	}
	if c.WarpExitBasePort <= 0 {
		c.WarpExitBasePort = 40000
	}
	if c.WarpAccountsPerExit <= 0 {
		c.WarpAccountsPerExit = 3
	}
	if c.WarpExitCount < 0 {
		c.WarpExitCount = 0
	}
	if c.RegistrationConcurrency <= 0 {
		c.RegistrationConcurrency = 3
	}
	if c.RegistrationTimeout <= 0 {
		c.RegistrationTimeout = 300
	}
	c.RegistrationDefaultGroup = strings.TrimSpace(c.RegistrationDefaultGroup)
	if c.RegistrationDefaultGroup == "" {
		c.RegistrationDefaultGroup = firstNonEmptyConfig(c.DefaultGroup, DefaultGroupName)
	}
	c.RegistrationEgressPoolID = strings.TrimSpace(c.RegistrationEgressPoolID)
	c.DefaultRegisterMethod = normalizeRegistrationMethodAlias(c.DefaultRegisterMethod)
	// Email registration normalization
	if c.DefaultSMSProvider == "" {
		c.DefaultSMSProvider = "smsbower"
	}
	if c.DefaultMailboxProvider == "" {
		c.DefaultMailboxProvider = "tempmail_lol"
	}
	if c.DefaultCaptchaProvider == "" {
		c.DefaultCaptchaProvider = "yescaptcha"
	}
	if strings.TrimSpace(c.SMSPlatformStrategy) == "" {
		c.SMSPlatformStrategy = "auto"
	} else {
		c.SMSPlatformStrategy = strings.ToLower(strings.TrimSpace(c.SMSPlatformStrategy))
	}
	if strings.TrimSpace(c.SMSPreferredCountries) == "" {
		c.SMSPreferredCountries = "BR,CO,PL"
	}
	if c.SMSStatsTopN < 1 {
		c.SMSStatsTopN = 3
	}
	if strings.TrimSpace(c.CliproxyAPIBase) == "" {
		c.CliproxyAPIBase = "https://api.cliproxy.io"
	}
	// CliproxyValidateRegion defaults true: geo-mismatch is the #1 cause of withheld SMS.
	switch strings.ToLower(strings.TrimSpace(c.GatewayReliabilityGuardMode)) {
	case "off", "lenient", "strict":
		c.GatewayReliabilityGuardMode = strings.ToLower(strings.TrimSpace(c.GatewayReliabilityGuardMode))
	default:
		c.GatewayReliabilityGuardMode = "lenient"
	}
	// Intelligent routing fallback chains: trim every entry, drop blanks and
	// self-references, and deduplicate while preserving the configured order.
	if len(c.GroupFallbacks) > 0 {
		normalized := make(map[string][]string, len(c.GroupFallbacks))
		for group, chain := range c.GroupFallbacks {
			group = strings.TrimSpace(group)
			if group == "" {
				continue
			}
			out := make([]string, 0, len(chain))
			seen := make(map[string]struct{}, len(chain))
			for _, fallback := range chain {
				fallback = strings.TrimSpace(fallback)
				if fallback == "" || strings.EqualFold(fallback, group) {
					continue
				}
				if _, duplicate := seen[strings.ToLower(fallback)]; duplicate {
					continue
				}
				seen[strings.ToLower(fallback)] = struct{}{}
				out = append(out, fallback)
			}
			if len(out) > 0 {
				normalized[group] = out
			}
		}
		c.GroupFallbacks = normalized
	}
	// Safety-session-rotation opt-ins: trim keys; a blank key or a false value is
	// dropped (only an explicit true opt-in matters).
	if len(c.SafetySessionRotationGroups) > 0 {
		normalized := make(map[string]bool, len(c.SafetySessionRotationGroups))
		for group, on := range c.SafetySessionRotationGroups {
			group = strings.TrimSpace(group)
			if group != "" && on {
				normalized[group] = true
			}
		}
		c.SafetySessionRotationGroups = normalized
	}
}

func normalizeNodeID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value, _ = os.Hostname()
	}
	if value == "" {
		value = "node-local"
	}
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			out.WriteRune(r)
		default:
			out.WriteByte('_')
		}
		if out.Len() >= 128 {
			break
		}
	}
	if out.Len() == 0 {
		return "node-local"
	}
	return out.String()
}

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
}

func dottedVersionLess(a, b string) bool {
	av, aok := parseDottedVersion(a)
	bv, bok := parseDottedVersion(b)
	if !aok || !bok {
		return false
	}
	for i := 0; i < len(av); i++ {
		if av[i] != bv[i] {
			return av[i] < bv[i]
		}
	}
	return false
}

func parseDottedVersion(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 2 {
		return out, false
	}
	for i := 0; i < len(out); i++ {
		if i >= len(parts) {
			break
		}
		part := parts[i]
		for j, r := range part {
			if r < '0' || r > '9' {
				part = part[:j]
				break
			}
		}
		if part == "" {
			return out, false
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// SensitiveWordsFor returns the words to scrub for a provider: the global
// SensitiveWords, plus the Claude-specific list for the "claude" provider.
func (c Config) SensitiveWordsFor(provider string) []string {
	if provider == "claude" && len(c.ClaudeSensitiveWords) > 0 {
		out := make([]string, 0, len(c.SensitiveWords)+len(c.ClaudeSensitiveWords))
		out = append(out, c.SensitiveWords...)
		out = append(out, c.ClaudeSensitiveWords...)
		return out
	}
	return c.SensitiveWords
}
