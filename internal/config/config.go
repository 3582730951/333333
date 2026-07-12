package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddr      = "127.0.0.1:8787"
	DefaultDatabasePath    = "codex-pool.sqlite3"
	DefaultUpstreamBaseURL = "https://chatgpt.com/backend-api/codex"
	DefaultGroupName       = "cyber"
	// DefaultClientVersion is the current Codex client version sent on model
	// discovery and on version-gated live Codex requests. ChatGPT gates the returned
	// model catalog and some live models by this value, so old preserved config
	// values are floored to this default during normalization.
	// Refreshed 2026-07-10 from the installed shipping CLI (codex-cli 0.144.1)
	// and other_codex's 0.144.0-gated GPT-5.6 catalog.
	DefaultClientVersion                  = "0.144.1"
	DefaultStickyWaitMillis               = 100
	DefaultStrictStickyMaxCooldownSeconds = 60
	DefaultCooldownWaitMaxSeconds         = 30
	// AdmissionWait: when every provider/model-matching account is momentarily at its
	// concurrency cap or over its per-account token budget, a request waits this long
	// for an in-flight slot to free (retrying selection each time a lease releases)
	// before the pool reports saturation. This is backpressure/queueing, never a model
	// or context downgrade — the request still lands on a full-quality account. 0 disables.
	DefaultAdmissionWaitMillis = 2500
	// Cooldown→health-recheck loop: how often to probe benched accounts, and how long
	// to re-bench one whose probe still fails before the next attempt.
	DefaultAccountRecheckIntervalSeconds  = 20
	DefaultAccountRecheckBackoffSeconds   = 120
	DefaultRequestTimeoutSec              = 600
	DefaultShutdownDrainSec               = 30
	DefaultMaxBodyBytes                   = 256 << 20
	DefaultStreamFailoverHoldMemoryBytes  = 8 << 20
	DefaultStreamFailoverHoldDiskBytes    = 0
	DefaultVirtualWindow                  = 2_000_000
	DefaultVirtualContextLedgerTTLSeconds = 3600 // 1 hour
	DefaultKiroCacheUnreportedThreshold   = 20
	// AccountTokenBudget caps CONCURRENT estimated tokens stacked on one account (a
	// smoothing gate against over-committing a single upstream, not a per-request limit).
	// Sized for several concurrent 1M-context Claude Code turns per account; exceeding it
	// now makes a request WAIT for headroom (see AdmissionWait), never fail downstream.
	DefaultAccountTokenBudget = 8_000_000
	// Standard Claude Code OAuth client (Claude Pro/Max). Operators may override
	// via config if Anthropic rotates these.
	DefaultClaudeOAuthTokenURL = "https://api.anthropic.com/v1/oauth/token"
	DefaultClaudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// Web-login (paste-back) OAuth defaults. These drive the "generate a login URL →
	// log in in the browser → paste the redirected URL / code back" import flow for
	// both providers (internal/api/oauth.go). They mirror the official clients so the
	// authorize/token endpoints accept the request; every value is config-overridable
	// in case OpenAI/Anthropic rotate the client or move an endpoint.
	DefaultCodexOAuthAuthURL      = "https://auth.openai.com/oauth/authorize"
	DefaultCodexOAuthTokenURL     = "https://auth.openai.com/oauth/token"
	DefaultCodexOAuthClientID     = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	DefaultCodexOAuthScope        = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	DefaultClaudeOAuthAuthURL     = "https://claude.ai/oauth/authorize"
	DefaultClaudeOAuthRedirectURI = "http://localhost:54545/callback"
	DefaultClaudeOAuthScope       = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	// DefaultClaudeNodeVersion is the Node runtime version reported in
	// X-Stainless-Runtime-Version. Kept here (not in the identity package) so the
	// upstream Node fingerprint can be bumped from one place / overridden by config.
	// Reconfirmed 2026-07-10 from Claude Code 2.1.206 Docker capture.
	DefaultClaudeNodeVersion = "v26.3.0"
	// DefaultModelProbeIntervalHours re-probes each account's upstream model list
	// twice a day so the advertised /v1/models union stays fresh.
	DefaultModelProbeIntervalHours = 12
	// Model-quality monitoring is group×model (never per account). One compact
	// primary probe runs per interval; confirmations are anomaly-only.
	DefaultModelQualityIntervalMinutes   = 60
	DefaultModelQualityReasoningEffort   = "low"
	DefaultModelQualityDegradedThreshold = 2
	DefaultModelQualityHistoryDays       = 30
	// DefaultGeoProbeURL is the IP/geo echo used to auto-detect a proxy's exit
	// region when none is configured. It returns JSON with ip/country/region/city.
	DefaultGeoProbeURL = "https://ipapi.co/json/"
	// GoPay auto-subscribe integration defaults (Part 4). The feature is OFF by
	// default; these only take effect once an admin enables it.
	DefaultGopayDir             = "gopay/plus"
	DefaultGopayPython          = "python3"
	DefaultGopayOrchestratorURL = "http://127.0.0.1:8800"
	DefaultCodexReauthWorkerURL = "http://127.0.0.1:8802"

	legacyClaudeOAuthTokenURL = "https://console.anthropic.com/v1/oauth/token"
)

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
	ListenAddr                     string `json:"listen_addr"`
	DatabasePath                   string `json:"database_path"`
	UpstreamBaseURL                string `json:"upstream_base_url"`
	OAuthTokenURL                  string `json:"oauth_token_url"`
	ClientVersion                  string `json:"client_version"`
	DefaultGroup                   string `json:"default_group"`
	Virtual2MEnabled               bool   `json:"-"`
	VirtualContextWindow           int64  `json:"-"`
	VirtualContextLedgerTTLSeconds int64  `json:"-"`
	StickyWaitMillis               int    `json:"sticky_wait_millis"`
	// AdmissionWaitMillis bounds the per-account concurrency/token-budget backpressure
	// wait (see DefaultAdmissionWaitMillis). Unset (0) adopts the default; a negative
	// value disables the wait (legacy fail-fast).
	AdmissionWaitMillis   int    `json:"admission_wait_millis"`
	RequestTimeoutSeconds int    `json:"request_timeout_seconds"`
	ShutdownDrainSeconds  int    `json:"shutdown_drain_seconds"`
	MaxBodyBytes          int64  `json:"max_body_bytes"`
	AccountTokenBudget    int64  `json:"account_token_budget"`
	AdminToken            string `json:"admin_token"`
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
	// IdentitySecret seeds the deterministic per-account virtual identity
	// (User-Agent, session ids, device profile, env values). Set a unique value
	// per deployment so profiles are not predictable across installs. When empty
	// a built-in default is used (still deterministic and per-account unique).
	IdentitySecret string `json:"identity_secret"`
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
	// to a curl_cffi_sidecar egress. This is an ESCAPE HATCH only: by default a
	// sidecar-bound Claude account IS routed through the sidecar, because the sidecar
	// is the sole way to present a real client TLS/JA3/HTTP2 fingerprint instead of
	// the Go standard library's (which is itself a relay-detection signal even though
	// Anthropic, unlike chatgpt.com, has no Cloudflare challenge wall). Leave false
	// unless a deployment cannot run the sidecar against api.anthropic.com.
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
	CodexOAuthAuthURL      string `json:"codex_oauth_auth_url"`
	CodexOAuthTokenURL     string `json:"codex_oauth_token_url"`
	CodexOAuthClientID     string `json:"codex_oauth_client_id"`
	CodexOAuthRedirectURI  string `json:"codex_oauth_redirect_uri"`
	CodexOAuthScope        string `json:"codex_oauth_scope"`
	ClaudeOAuthAuthURL     string `json:"claude_oauth_auth_url"`
	ClaudeOAuthRedirectURI string `json:"claude_oauth_redirect_uri"`
	ClaudeOAuthScope       string `json:"claude_oauth_scope"`
	// Client-version fingerprints. These are sent upstream verbatim and therefore
	// become a "time fingerprint" if they drift behind the real shipping clients
	// (an old version pinned forever is as detectable as a fake one). They are
	// exposed as config so a deployment can track the real Codex/Claude Code
	// releases WITHOUT a recompile. Empty = the built-in default (see identity
	// package constants), which should track the current shipping client.
	CodexCLIVersionOverride  string `json:"codex_cli_version"`
	ClaudeCLIVersionOverride string `json:"claude_cli_version"`
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
	// ClaudeJA3 selects the TLS ClientHello fingerprint the curl_cffi sidecar replays
	// for Claude/Anthropic traffic. Empty (DEFAULT) = the sidecar's native Chrome
	// impersonation — the proven, Cloudflare-friendly choice used by reference relays;
	// the best available research shows Anthropic detects third-party clients by
	// system-prompt content (handled in the cloak layer), not TLS, so the real
	// claude-cli fingerprint is an opt-in rather than the default. Set to
	// "claude-cli"/"real"/"native" to opt into the captured real claude-cli/Node JA3
	// (identity.ClaudeJA3), or to an explicit JA3 string. "off"/"none"/"disabled"/"-"/
	// "chrome" also keep Chrome. Only applies to the curl_cffi_sidecar egress.
	ClaudeJA3Override string `json:"claude_ja3"`
	// ClaudeNodeVersion is the Node runtime version reported in
	// X-Stainless-Runtime-Version (real Claude Code runs on Node). Empty = default.
	ClaudeNodeVersion string `json:"claude_node_version"`
	// Kiro IDE fingerprint and regional endpoints. These are hot-reloadable through
	// the settings registry; endpoint overrides on an individual credential win.
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
	// Claude Code 2.1.206 no longer emits cch, so the request path ignores this
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
	// CodexPromptCacheRetention is retained only for config-file compatibility.
	// Current codex-rs 0.144.x sends no prompt_cache_retention field on HTTP or WS,
	// so the relay does not inject it and strips legacy downstream values. Cache
	// affinity uses prompt_cache_key instead. Runtime key: codex_prompt_cache_retention.
	CodexPromptCacheRetention string `json:"codex_prompt_cache_retention"`
	// Codex install defaults written into the generated ~/.codex/config.toml by the
	// one-shot setup script (GET /file/<key>). Together approval_policy="never" +
	// sandbox_mode="danger-full-access" are Codex's fully-automated "goal mode": the
	// CLI runs every command without asking. These are the operator-requested defaults;
	// set a more conservative value (e.g. sandbox "workspace-write", approval
	// "on-request") to dial back. All are validated against codex-rs enums.
	CodexInstallModel          string `json:"codex_install_model"`           // default "gpt-5.6-sol"
	CodexInstallEffort         string `json:"codex_install_effort"`          // default "xhigh"
	CodexInstallApprovalPolicy string `json:"codex_install_approval_policy"` // default "never"
	CodexInstallSandboxMode    string `json:"codex_install_sandbox_mode"`    // default "danger-full-access"
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
	// or auto-refill) does not name a method. "node" = the transplanted
	// puppeteer-real-browser registrar (other_new_gpt_register) orchestrated per-job.
	// Runtime-overridable via the "default_register_method" setting. Default "node".
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

	// ── GoPay 自动订阅 (Part 4) ──
	// GopayEnabled is the BOOT default for the bundled GoPay Plus auto-subscribe
	// integration; default false (off). The live value is runtime-toggleable from
	// the admin UI (settings key "gopay_enabled"). When enabled the manager can
	// launch the bundled Python services (gopay/plus) as managed subprocesses and
	// expose /admin/gopay/subscribe, which feeds each pooled account's stored
	// session token into the proven Stripe→Midtrans→GoPay payment flow.
	GopayEnabled bool `json:"gopay_enabled"`
	// GopayDir is the path to the bundled gopay-plus project (holds orchestrator.py
	// and plus_gopay_links/). Relative to the server's working directory.
	GopayDir string `json:"gopay_dir"`
	// GopayPython is the interpreter used to launch the bundled services.
	GopayPython string `json:"gopay_python"`
	// GopayAutoStart, when true, makes the server launch (and stop) the bundled
	// gopay services as child processes on enable. When false the operator runs the
	// orchestrator themselves and the server only talks to GopayOrchestratorURL.
	GopayAutoStart bool `json:"gopay_auto_start"`
	// GopayOrchestratorURL is the base URL of the gopay orchestrator HTTP API.
	GopayOrchestratorURL string `json:"gopay_orchestrator_url"`
	// GopayAuthToken is the bearer token the orchestrator requires on /subscribe;
	// the manager also writes it into the generated gopay config.json.
	GopayAuthToken string `json:"gopay_auth_token"`
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
	// http://127.0.0.1:8191). The /v1 {cmd:request.get} contract is shared by
	// FlareSolverr / Byparr / Solvearr.
	CFSolverURL string `json:"cf_solver_url"`
	// ── Registration (auto phone registration) ──
	RegistrationEnabled     bool   `json:"registration_enabled"`
	RegistrationConcurrency int    `json:"registration_concurrency"`
	RegistrationTimeout     int    `json:"registration_timeout"`
	DefaultSMSProvider      string `json:"default_sms_provider"`
	DefaultMailboxProvider  string `json:"default_mailbox_provider"`
	DefaultCaptchaProvider  string `json:"default_captcha_provider"`

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
		ListenAddr:                     DefaultListenAddr,
		DatabasePath:                   DefaultDatabasePath,
		UpstreamBaseURL:                DefaultUpstreamBaseURL,
		ClientVersion:                  DefaultClientVersion,
		DefaultGroup:                   DefaultGroupName,
		Virtual2MEnabled:               false,
		VirtualContextWindow:           DefaultVirtualWindow,
		VirtualContextLedgerTTLSeconds: DefaultVirtualContextLedgerTTLSeconds,
		StickyWaitMillis:               DefaultStickyWaitMillis,
		AdmissionWaitMillis:            DefaultAdmissionWaitMillis,
		RequestTimeoutSeconds:          DefaultRequestTimeoutSec,
		ShutdownDrainSeconds:           DefaultShutdownDrainSec,
		MaxBodyBytes:                   DefaultMaxBodyBytes,
		AccountTokenBudget:             DefaultAccountTokenBudget,
		TrustedProxyCIDRs:              []string{"127.0.0.0/8", "::1/128"},
		SidecarTimeoutSeconds:          120,
		KiroVersion:                    "0.11.107",
		KiroNodeVersion:                "22.22.0",
		KiroDefaultAuthRegion:          "us-east-1",
		KiroDefaultAPIRegion:           "us-east-1",
		KiroDefaultThinking:            true,
		KiroCacheMode:                  "auto",
		KiroCacheUnreportedThreshold:   DefaultKiroCacheUnreportedThreshold,
		SchedulerHeartbeatSeconds:      15,
		ConversationIsolation:          true,
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
		ClaudeCacheSingleflightEnabled:    false,
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
		CodexPromptCacheRetention:              "",
		CodexInstallModel:                      "gpt-5.6-sol",
		CodexInstallEffort:                     "xhigh",
		CodexInstallApprovalPolicy:             "never",
		CodexInstallSandboxMode:                "danger-full-access",
		ClaudeGatewayInterceptHosts:            DefaultClaudeGatewayInterceptHosts(),
		ClaudeGatewayForwardHosts:              DefaultClaudeGatewayForwardHosts(),
		ClaudeGatewayBlockedHostPatterns:       DefaultClaudeGatewayBlockedHostPatterns(),
		ClaudeGatewayUnknownTargetPolicy:       "forward",
		ClaudeGatewayDisableNonessentialEnv:    true,
		ClaudeGatewayStrictLinuxDefault:        false,
		DefaultRegisterMethod:                  "node",
		StrictStickyMaxCooldownSeconds:         DefaultStrictStickyMaxCooldownSeconds,
		StatefulStickyWaitSeconds:              0,
		CooldownWaitMaxSeconds:                 DefaultCooldownWaitMaxSeconds,
		LeakScrubEnabled:                       true,
		ModelProbeIntervalHours:                DefaultModelProbeIntervalHours,
		ModelQualityMonitorEnabled:             false,
		ModelQualityIntervalMinutes:            DefaultModelQualityIntervalMinutes,
		ModelQualityReasoningEffort:            DefaultModelQualityReasoningEffort,
		ModelQualityDegradedThreshold:          DefaultModelQualityDegradedThreshold,
		ModelQualityHistoryDays:                DefaultModelQualityHistoryDays,
		GeoProbeURL:                            DefaultGeoProbeURL,
		CodexReauthWorkerURL:                   DefaultCodexReauthWorkerURL,
		CodexReauthWorkerConcurrency:           1,
		GopayDir:                               DefaultGopayDir,
		GopayPython:                            DefaultGopayPython,
		GopayOrchestratorURL:                   DefaultGopayOrchestratorURL,
		WarpExitBasePort:                       40000,
		WarpAccountsPerExit:                    3,
		RegistrationConcurrency:                1,
		RegistrationTimeout:                    300,
		GopayAutoStart:                         true,
		CodexPreferSidecarJA3OverWS:            true,
		SMSPlatformStrategy:                    "auto",
		SMSPreferredCountries:                  "BR,CO,PL",
		SMSStatsTopN:                           3,

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
	}
	cfg.applyEnv()
	cfg.normalize()
	return cfg, nil
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

// AdmissionWait is the bounded backpressure window a request spends waiting for a
// per-account concurrency / token-budget slot to free before the pool reports
// saturation. 0 disables the wait (legacy fail-fast selection).
func (c *Config) AdmissionWait() time.Duration {
	if c.AdmissionWaitMillis <= 0 {
		return 0
	}
	return time.Duration(c.AdmissionWaitMillis) * time.Millisecond
}

func (c *Config) RequestTimeout() time.Duration {
	return time.Duration(c.RequestTimeoutSeconds) * time.Second
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
		w = append(w, "claude_cli_version "+strconv.Quote(cli)+" does not look like a semver (e.g. 2.1.206)")
	}
	if sdk != "" && !looksLikeDotVersion(sdk) {
		w = append(w, "claude_stainless_version "+strconv.Quote(sdk)+" does not look like a semver (e.g. 0.94.0)")
	}
	if node != "" && !strings.HasPrefix(node, "v") {
		w = append(w, "claude_node_version "+strconv.Quote(node)+" should start with 'v' (Node convention, e.g. v26.3.0)")
	}
	return w
}

// looksLikeDotVersion reports whether s is a dotted, digits-only version like "2.1.206"
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

func (c *Config) applyEnv() {
	if v := os.Getenv("CODEX_POOL_LISTEN_ADDR"); v != "" {
		c.ListenAddr = v
	}
	if v := os.Getenv("CODEX_POOL_DATABASE"); v != "" {
		c.DatabasePath = v
	}
	if v := os.Getenv("CODEX_POOL_UPSTREAM_BASE_URL"); v != "" {
		c.UpstreamBaseURL = v
	}
	if v := os.Getenv("CODEX_POOL_OAUTH_TOKEN_URL"); v != "" {
		c.OAuthTokenURL = v
	}
	if v := os.Getenv("CODEX_POOL_CLIENT_VERSION"); v != "" {
		c.ClientVersion = v
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
	if v := os.Getenv("CODEX_POOL_ADMIN_TOKEN"); v != "" {
		c.AdminToken = v
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
	if v := os.Getenv("CODEX_POOL_GOPAY_ENABLED"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.GopayEnabled = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_GOPAY_DIR"); v != "" {
		c.GopayDir = v
	}
	if v := os.Getenv("CODEX_POOL_GOPAY_PYTHON"); v != "" {
		c.GopayPython = v
	}
	if v := os.Getenv("CODEX_POOL_GOPAY_AUTO_START"); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			c.GopayAutoStart = parsed
		}
	}
	if v := os.Getenv("CODEX_POOL_GOPAY_ORCHESTRATOR_URL"); v != "" {
		c.GopayOrchestratorURL = v
	}
	if v := os.Getenv("CODEX_POOL_GOPAY_AUTH_TOKEN"); v != "" {
		c.GopayAuthToken = v
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
}

func (c *Config) normalize() {
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.DatabasePath == "" {
		c.DatabasePath = DefaultDatabasePath
	}
	if c.UpstreamBaseURL == "" {
		c.UpstreamBaseURL = DefaultUpstreamBaseURL
	}
	if c.ClientVersion == "" || dottedVersionLess(c.ClientVersion, DefaultClientVersion) {
		c.ClientVersion = DefaultClientVersion
	}
	if c.CodexCLIVersionOverride != "" && dottedVersionLess(c.CodexCLIVersionOverride, DefaultClientVersion) {
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
	if c.StatefulStickyWaitSeconds < 0 {
		c.StatefulStickyWaitSeconds = 0
	}
	if c.ShutdownDrainSeconds <= 0 {
		c.ShutdownDrainSeconds = DefaultShutdownDrainSec
	}
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.AccountTokenBudget <= 0 {
		c.AccountTokenBudget = DefaultAccountTokenBudget
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
	if c.WebSearchEnabled && c.WebSearchToolType == "" {
		c.WebSearchToolType = "web_search"
	}
	if c.ClaudeOAuthTokenURL == "" || sameURL(c.ClaudeOAuthTokenURL, legacyClaudeOAuthTokenURL) {
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
	if c.ClaudeOAuthAuthURL == "" {
		c.ClaudeOAuthAuthURL = DefaultClaudeOAuthAuthURL
	}
	if c.ClaudeOAuthRedirectURI == "" {
		c.ClaudeOAuthRedirectURI = DefaultClaudeOAuthRedirectURI
	}
	if c.ClaudeOAuthScope == "" {
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
	if c.GopayDir == "" {
		c.GopayDir = DefaultGopayDir
	}
	if c.GopayPython == "" {
		c.GopayPython = DefaultGopayPython
	}
	if c.GopayOrchestratorURL == "" {
		c.GopayOrchestratorURL = DefaultGopayOrchestratorURL
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
	c.RegistrationEgressPoolID = strings.TrimSpace(c.RegistrationEgressPoolID)
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
