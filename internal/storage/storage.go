package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/secretbox"

	_ "github.com/mattn/go-sqlite3"
)

const DefaultDirectEgressID = "egress_direct"

var (
	ErrUserEmailExists    = errors.New("user email already exists")
	ErrRegistrationClosed = errors.New("user registration is disabled")
)

type Store struct {
	// db is the single-connection WRITE pool. SQLite permits only one writer at a
	// time, so funneling every write through one connection means our own
	// concurrency never collides into SQLITE_BUSY. Init() sets WAL/synchronous on it.
	db *sql.DB
	// rdb is the multi-connection READ pool against the same WAL database. Under WAL
	// readers run concurrently with each other and with the single writer, each on
	// the latest committed snapshot, so the per-request account-selection / token /
	// group SELECTs no longer serialize behind the writer. For an in-memory test DB
	// it is the same handle as db (see Open).
	rdb *sql.DB
	// tokenKey, when set (32 bytes), enables transparent AES-256-GCM encryption of the
	// secret columns in account_auth_tokens (access/refresh/id token, upstream api key),
	// api_keys.secret (copyable downstream key), and session cookies. nil = encryption
	// disabled (plaintext, legacy behavior) — kept nil by tests/in-memory stores so
	// they are unaffected. Set via SetTokenEncryptionKey from main using the resolved
	// deployment identity secret.
	tokenKey []byte

	// settings snapshot cache: a hot request reads ~15-20 distinct settings keys, each
	// previously its own SELECT against the small WAL read pool. This caches the whole
	// (tiny) settings table as one map behind an RWMutex, refreshed on a short TTL and
	// invalidated on every write (SetSettings is the sole write path). Reads are
	// byte-identical; a write is visible immediately, and the TTL bounds any missed
	// invalidation. The map is only ever replaced wholesale (never mutated in place),
	// so a reference handed to a caller stays safe for concurrent reads.
	settingsMu       sync.RWMutex
	settingsSnapshot map[string]string
	settingsLoadedAt time.Time
	tokenCache       sync.Map // account id -> decrypted AccountToken
	kiroCache        sync.Map // account id -> decrypted KiroCredentials
	apiKeyUsed       sync.Map // key hash -> last persisted minute
	rateLimitGen     atomic.Uint64
	affinityGen      atomic.Uint64
}

type Group struct {
	Name                          string   `json:"name"`
	SystemPrompt                  string   `json:"system_prompt"`
	PromptMode                    string   `json:"prompt_mode"`
	SystemPromptApplyToCompaction bool     `json:"system_prompt_apply_to_compaction"`
	Virtual2MEnabled              bool     `json:"-"`
	ModelInstructionsEnabled      bool     `json:"model_instructions_enabled"`
	ModelInstructionsFiles        []string `json:"model_instructions_files,omitempty"`
	// ForceModel / ForceEffort, when set, are the group-level default override that
	// rewrites the downstream-requested model / reasoning effort for any key in this
	// group that does not set its own. Empty means "respect the client's request".
	ForceModel  string `json:"force_model"`
	ForceEffort string `json:"force_effort"`
	// DefaultEgressID is a legacy operator note kept for backward-compatible group
	// payloads. Runtime routing no longer reads it; account_egress_bindings owns the
	// account's default egress.
	DefaultEgressID string `json:"default_egress_id"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type GroupAccountCounts struct {
	AccountCount       int `json:"account_count"`
	ActiveAccountCount int `json:"active_account_count"`
}

// APIKey is a downstream client credential. The sha256 hash is used for auth lookup;
// Secret stores a recoverable plaintext copy for admin/owner "copy key" and one-click
// install UX, encrypted at rest when Store.SetTokenEncryptionKey is configured.
// A key scopes requests to a group and may force the model / reasoning effort the
// request uses upstream regardless of what the client asked for.
type APIKey struct {
	KeyHash     string `json:"key_hash"`
	Label       string `json:"label"`
	KeyType     string `json:"key_type"`
	GroupName   string `json:"group_name"`
	ForceModel  string `json:"force_model"`
	ForceEffort string `json:"force_effort"`
	// ProviderHint disambiguates shared non-message endpoints such as /v1/files and
	// /v1/skills. "auto" preserves legacy header-based detection; "codex" and
	// "claude" pin official client families; "custom:<provider_id>" is reserved for
	// custom provider-specific surfaces.
	ProviderHint string `json:"provider_hint"`
	Enabled      bool   `json:"enabled"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	LastUsedAt   int64  `json:"last_used_at,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	// UserID is the owning portal user (empty = admin-owned). Set when a user creates
	// the key via the self-service /user/api-keys endpoint so it scopes to them.
	UserID string `json:"user_id,omitempty"`
	// Secret is plaintext in memory/API responses, but encrypted before persistence
	// when tokenKey is configured. It lets the admin/owner re-copy the key and its
	// one-click install command. Empty for legacy keys created before this column
	// existed — those remain unrecoverable and must be rotated to get a copyable secret.
	Secret    string `json:"secret,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Tenant struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type User struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	// Role is "admin" or "user" (default). Status is "active" or "disabled".
	Role   string `json:"role"`
	Status string `json:"status"`
	// PasswordHash is the PBKDF2 verifier (see internal/api/password.go). Never
	// serialized to clients (json:"-").
	PasswordHash string `json:"-"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// UserSession is a logged-in end-user/admin browser session. TokenHash is the
// sha256 of the random cookie value (the plaintext token is never stored).
type UserSession struct {
	TokenHash string
	UserID    string
	CreatedAt int64
	ExpiresAt int64
	UserAgent string
}

type Project struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	Name      string `json:"name"`
	GroupName string `json:"group_name"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Account struct {
	ID                string `json:"id"`
	Label             string `json:"label"`
	GroupName         string `json:"group_name"`
	UpstreamAccountID string `json:"upstream_account_id,omitempty"`
	ChatGPTUserID     string `json:"chatgpt_user_id,omitempty"`
	Email             string `json:"email,omitempty"`
	PlanType          string `json:"plan_type,omitempty"`
	// Provider is the explicit upstream class: "" (legacy — infer codex/claude from
	// the credential shape), "codex", "claude", or a custom OpenAI-compatible
	// provider id (e.g. "deepseek"). It is the authoritative routing key; the
	// token-shape heuristic (scheduler.ProviderFromToken) is only the fallback for
	// pre-migration rows whose provider is still empty.
	Provider         string `json:"provider,omitempty"`
	Status           string `json:"status"`
	IsFedramp        bool   `json:"is_fedramp"`
	QuarantineUntil  int64  `json:"quarantine_until,omitempty"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
}

type AccountPoolSummary struct {
	Total       int `json:"total"`
	Active      int `json:"active"`
	Quarantined int `json:"quarantined"`
	Cooling     int `json:"cooling"`
	Recheck     int `json:"recheck"`
	Codex       int `json:"codex"`
	Claude      int `json:"claude"`
	Kiro        int `json:"kiro"`
	Other       int `json:"other"`
}

type AccountToken struct {
	AccountID          string `json:"account_id"`
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	OpenAIAPIKey       string `json:"openai_api_key,omitempty"`
	IDTokenRaw         string `json:"id_token_raw,omitempty"`
	LastRefresh        int64  `json:"last_refresh,omitempty"`
	ExpiresAt          int64  `json:"expires_at,omitempty"`
	Scopes             string `json:"scopes,omitempty"`
	OAuthRateLimitTier string `json:"oauth_rate_limit_tier,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

// KiroCredentials contains the non-token portion of a Kiro IDE credential. Access
// and refresh tokens remain in AccountToken so rotation uses the existing atomic
// token update path. ClientSecret and KiroAPIKey are transparently encrypted at rest.
type KiroCredentials struct {
	AccountID      string `json:"account_id"`
	AuthMethod     string `json:"auth_method"`
	ClientID       string `json:"client_id,omitempty"`
	ClientSecret   string `json:"-"`
	ProfileARN     string `json:"profile_arn,omitempty"`
	AuthRegion     string `json:"auth_region"`
	APIRegion      string `json:"api_region"`
	MachineID      string `json:"machine_id,omitempty"`
	KiroAPIKey     string `json:"-"`
	Endpoint       string `json:"endpoint,omitempty"`
	CredentialHash string `json:"-"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

type KiroAuthSummary struct {
	AuthMethod      string `json:"auth_method"`
	AuthRegion      string `json:"auth_region"`
	APIRegion       string `json:"api_region"`
	Endpoint        string `json:"endpoint,omitempty"`
	HasClientID     bool   `json:"has_client_id"`
	HasClientSecret bool   `json:"has_client_secret"`
	HasProfileARN   bool   `json:"has_profile_arn"`
	HasMachineID    bool   `json:"has_machine_id"`
	HasAPIKey       bool   `json:"has_api_key"`
}

type ModelCapability struct {
	AccountID                     string `json:"account_id"`
	ModelSlug                     string `json:"model_slug"`
	NativeContextWindow           int64  `json:"native_context_window"`
	NativeMaxContextWindow        int64  `json:"native_max_context_window"`
	EffectiveContextWindowPercent int64  `json:"effective_context_window_percent"`
	AutoCompactTokenLimit         int64  `json:"auto_compact_token_limit"`
	Visibility                    string `json:"visibility"`
	ETag                          string `json:"etag"`
	RawModelJSONHash              string `json:"raw_model_json_hash"`
	RawModelJSON                  string `json:"raw_model_json,omitempty"`
	Source                        string `json:"source"`
	LastProbeAt                   int64  `json:"last_probe_at"`
}

type KiroRuntimeCapability struct {
	AccountID                 string  `json:"account_id"`
	EndpointHash              string  `json:"endpoint_hash"`
	Model                     string  `json:"model"`
	ModelState                string  `json:"model_state"`
	ThinkingState             string  `json:"thinking_state"`
	CacheCapability           string  `json:"cache_capability"`
	CachePointState           string  `json:"cache_point_state"`
	CacheReuseState           string  `json:"cache_reuse_state"`
	CacheReuseEvidence        string  `json:"cache_reuse_evidence,omitempty"`
	CacheReuseReductionPct    float64 `json:"cache_reuse_credit_reduction_percent,omitempty"`
	CacheReuseProbedAt        int64   `json:"cache_reuse_probed_at,omitempty"`
	Observations              int64   `json:"observations"`
	MeteringEvents            int64   `json:"metering_events"`
	CacheReportedObservations int64   `json:"cache_reported_observations"`
	CacheHitObservations      int64   `json:"cache_hit_observations"`
	ConsecutiveUnreported     int64   `json:"consecutive_unreported"`
	UnknownCacheSchemaJSON    string  `json:"unknown_cache_schema_json,omitempty"`
	UpdatedAt                 int64   `json:"updated_at"`
}

type KiroCapabilityObservation struct {
	ModelSucceeded         bool
	ThinkingRequested      bool
	MeteringEvents         int
	CacheReadPresent       bool
	CacheReadTokens        int64
	CacheCreationPresent   bool
	CacheCreationTokens    int64
	ExplicitlyUnsupported  bool
	UnknownCacheSchemaJSON string
	UnreportedThreshold    int
}

type EgressProfile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	// ChainProxy, when set, is an upstream proxy URL (e.g. a WARP exit's local
	// SOCKS5 "socks5h://127.0.0.1:40000") that a curl_cffi_sidecar egress routes
	// its impersonated request THROUGH. It is what lets a JA3-bound account also
	// change its exit IP — present the real Codex/Claude TLS fingerprint AND leave
	// from a clean (WARP) IP at once, the combination that actually clears a CF
	// block. Ignored by non-sidecar egress types (those carry the proxy in Endpoint).
	ChainProxy     string `json:"chain_proxy,omitempty"`
	Region         string `json:"region"`
	ExitIP         string `json:"exit_ip"`
	StreamCapable  bool   `json:"stream_capable"`
	Health         string `json:"health"`
	LatencyMillis  int64  `json:"latency_millis"`
	CFScore        int64  `json:"cf_score"`
	LastCFRay      string `json:"last_cf_ray,omitempty"`
	CooldownUntil  int64  `json:"cooldown_until,omitempty"`
	MaxConcurrency int    `json:"max_concurrency"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	// ProxyAuthMode selects how cliproxy proxy IPs are obtained:
	// "credential" = username/password mode (sid rotation + region in username, current default)
	// "api_whitelist" = API whitelist mode (call api.cliproxy.io/white/api to extract ip:port)
	// Empty string = credential mode (backward compatible).
	ProxyAuthMode string `json:"proxy_auth_mode,omitempty"`
	// ProxyAPIKey is the cliproxy account API token used only in api_whitelist mode.
	// It is stored encrypted at rest like other secrets (enc:v1: prefix).
	ProxyAPIKey string `json:"proxy_api_key,omitempty"`
	// IPMode describes the operator-facing address behavior, e.g.
	// "static_residential", "dynamic_residential", or "datacenter". It is metadata used
	// by the admin UI and registration policy; request routing still follows Type/Endpoint.
	IPMode string `json:"ip_mode,omitempty"`
	// ProviderKey records the proxy/egress vendor or local mechanism ("cliproxy",
	// "cuff", "warp", ...). It is deliberately free-form so operators can add providers
	// without schema churn.
	ProviderKey string `json:"provider_key,omitempty"`
	// DynamicConfigJSON stores provider-specific dynamic proxy settings as a JSON object
	// string. Keeping it opaque at the storage layer lets the UI support SID rotation,
	// API whitelist extraction, and future providers without hardcoding one schema here.
	DynamicConfigJSON string `json:"dynamic_config_json,omitempty"`
}

type EgressPool struct {
	ID                 string             `json:"id"`
	Name               string             `json:"name"`
	Purpose            string             `json:"purpose"` // "registration" | "runtime" | custom
	AssignmentStrategy string             `json:"assignment_strategy"`
	CreatedAt          int64              `json:"created_at"`
	UpdatedAt          int64              `json:"updated_at"`
	Members            []EgressPoolMember `json:"members,omitempty"`
}

type EgressPoolMember struct {
	PoolID    string        `json:"pool_id"`
	EgressID  string        `json:"egress_id"`
	Enabled   bool          `json:"enabled"`
	Capacity  int           `json:"capacity"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`
	Egress    EgressProfile `json:"egress,omitempty"`
}

type GroupEgressPolicy struct {
	GroupName          string `json:"group_name"`
	RegistrationPoolID string `json:"registration_pool_id"`
	RuntimePoolID      string `json:"runtime_pool_id"`
	AssignmentStrategy string `json:"assignment_strategy"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type AccountEgressBinding struct {
	AccountID        string `json:"account_id"`
	PrimaryEgressID  string `json:"primary_egress_id"`
	StandbyEgressIDs string `json:"standby_egress_ids"`
	CookieJarKey     string `json:"cookie_jar_key"`
	CooldownUntil    int64  `json:"cooldown_until,omitempty"`
	// RecheckPending marks an account that was benched after an upstream error and
	// must pass a liveness re-check ("测活") before it is allowed back into the
	// candidate pool. While set, the scheduler treats the account as ineligible even
	// after CooldownUntil elapses; the background recheck loop probes it and clears
	// this flag only on a healthy result (re-cooling it otherwise). This is what
	// guarantees a failed account never silently re-enters rotation unverified.
	RecheckPending bool  `json:"recheck_pending,omitempty"`
	CreatedAt      int64 `json:"created_at"`
	UpdatedAt      int64 `json:"updated_at"`
}

func (b AccountEgressBinding) StandbyIDs() []string {
	if strings.TrimSpace(b.StandbyEgressIDs) == "" {
		return nil
	}
	parts := strings.Split(b.StandbyEgressIDs, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

type CFEvent struct {
	ID        int64  `json:"id"`
	AccountID string `json:"account_id"`
	EgressID  string `json:"egress_id"`
	Status    int    `json:"status"`
	CFRay     string `json:"cf_ray,omitempty"`
	Category  string `json:"category"`
	Message   string `json:"message"`
	CreatedAt int64  `json:"created_at"`
}

// InjectedCookie is an operator/auto-harvested cookie set (e.g. a cf_clearance
// solved in a browser or by FlareSolverr) bound to a specific account+egress+host.
// It is persisted so the cookie survives a restart and can be re-seeded into BOTH
// the Go cookie jar (direct/proxy egress) and the curl_cffi sidecar's store (sidecar
// egress). UserAgent/ExitIP record what the clearance is bound to — a cf_clearance is
// only valid when replayed with the same UA + exit IP that solved it.
type InjectedCookie struct {
	AccountID    string `json:"account_id"`
	EgressID     string `json:"egress_id"`
	UpstreamHost string `json:"upstream_host"`
	CookieHeader string `json:"cookie_header"`
	UserAgent    string `json:"user_agent"`
	ExitIP       string `json:"exit_ip"`
	UpdatedAt    int64  `json:"updated_at"`
}

type AffinityBinding struct {
	RouteKeyHash string `json:"route_key_hash"`
	RouteKey     string `json:"route_key"`
	Source       string `json:"source"`
	AccountID    string `json:"account_id"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	EgressID     string `json:"egress_id,omitempty"`
	Epoch        int64  `json:"epoch"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

type VirtualLedgerItem struct {
	ID             int64  `json:"id"`
	RouteKeyHash   string `json:"route_key_hash"`
	AccountID      string `json:"account_id"`
	Model          string `json:"model"`
	PromptCacheKey string `json:"prompt_cache_key"`
	Role           string `json:"role"`
	Content        string `json:"content"`
	TokenEstimate  int64  `json:"token_estimate"`
	RawJSON        string `json:"raw_json"`
	CreatedAt      int64  `json:"created_at"`
}

type BillingHold struct {
	ID              string `json:"id"`
	RouteKeyHash    string `json:"route_key_hash"`
	AccountID       string `json:"account_id"`
	EstimatedTokens int64  `json:"estimated_tokens"`
	Status          string `json:"status"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// AccountRateLimit is the latest rate-limit / remaining-quota snapshot captured
// from an upstream response's rate-limit headers (Anthropic unified/tokens/requests
// or OpenAI x-ratelimit-*). It is the data behind the per-account quota gauges in
// the admin UI. A field set to -1 means "unknown / not signalled by this provider".
type AccountRateLimit struct {
	AccountID         string  `json:"account_id"`
	Provider          string  `json:"provider"`
	Model             string  `json:"model"`
	LimiterType       string  `json:"limiter_type"`
	Source            string  `json:"source"`             // primary window: unified | tokens | requests
	UsedPercent       float64 `json:"used_percent"`       // 0..100 for the primary window, -1 unknown
	LimitTokens       int64   `json:"limit_tokens"`       // -1 unknown
	RemainingTokens   int64   `json:"remaining_tokens"`   // -1 unknown
	LimitRequests     int64   `json:"limit_requests"`     // -1 unknown
	RemainingRequests int64   `json:"remaining_requests"` // -1 unknown
	ResetAt           int64   `json:"reset_at"`           // epoch seconds the primary window resets, 0 unknown
	Status            string  `json:"status"`             // provider status hint, e.g. allowed_warning / rejected
	Raw               string  `json:"raw,omitempty"`      // JSON of the raw rate-limit headers, for the drawer
	UpdatedAt         int64   `json:"updated_at"`
}

type CodexResetCreditConsumption struct {
	AccountID       string `json:"account_id"`
	SevenDayResetAt int64  `json:"seven_day_reset_at"`
	RedeemRequestID string `json:"redeem_request_id"`
	Status          string `json:"status"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type CodexResetCreditClaim struct {
	Claimed bool                        `json:"claimed"`
	Row     CodexResetCreditConsumption `json:"row"`
}

// UpstreamErrorRule lets operators override how a particular upstream HTTP error is
// interpreted and surfaced. The lists are stored as JSON arrays so future providers,
// entrypoints, and model families can be added without schema churn.
type UpstreamErrorRule struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Enabled              bool     `json:"enabled"`
	Priority             int      `json:"priority"`
	Providers            []string `json:"providers"`
	Entrypoints          []string `json:"entrypoints"`
	ModelPatterns        []string `json:"model_patterns"`
	StatusCodes          []int    `json:"status_codes"`
	BodyKeywords         []string `json:"body_keywords"`
	MatchMode            string   `json:"match_mode"`
	AccountAction        string   `json:"account_action"`
	DownstreamAction     string   `json:"downstream_action"`
	ResponseStatus       int      `json:"response_status,omitempty"`
	CustomMessage        string   `json:"custom_message,omitempty"`
	CooldownSeconds      int64    `json:"cooldown_seconds,omitempty"`
	PreferRetryAfter     bool     `json:"prefer_retry_after"`
	IdleSeconds          int64    `json:"idle_seconds,omitempty"`
	IdlePingSeconds      int64    `json:"idle_ping_seconds,omitempty"`
	SkipLog              bool     `json:"skip_log"`
	FilterAccountAction  bool     `json:"filter_account_action"`
	KeywordCaseSensitive bool     `json:"keyword_case_sensitive"`
	Description          string   `json:"description,omitempty"`
	CreatedAt            int64    `json:"created_at"`
	UpdatedAt            int64    `json:"updated_at"`
}

// CustomProvider is an OpenAI-compatible upstream provider (Chat Completions by
// default, or native Responses when UpstreamProtocol="responses") the relay can pool
// accounts against. Unlike the built-in "codex"/"claude" upstreams it carries no
// fingerprint mimicry: requests go out as a clean Bearer-auth OpenAI client. An
// account's Provider column names which CustomProvider serves it. The model set is
// auto-discovered from {BaseURL}/models and/or set manually by an operator (Models,
// edited via input boxes in the admin UI — never raw JSON). BaseURL includes the
// OpenAI-compat path prefix (e.g. ".../v1"); the adapter appends /chat/completions
// or /responses for live requests, and /models for discovery.
type CustomProvider struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	BaseURL            string   `json:"base_url"`
	UpstreamProtocol   string   `json:"upstream_protocol"`
	Enabled            bool     `json:"enabled"`
	AutoDiscoverModels bool     `json:"auto_discover_models"`
	Models             []string `json:"models"`
	CreatedAt          int64    `json:"created_at"`
	UpdatedAt          int64    `json:"updated_at"`
}

const (
	CustomProviderProtocolChatCompletions = "chat_completions"
	CustomProviderProtocolResponses       = "responses"
)

func NormalizeCustomProviderProtocol(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return CustomProviderProtocolChatCompletions, true
	case CustomProviderProtocolChatCompletions:
		return CustomProviderProtocolChatCompletions, true
	case CustomProviderProtocolResponses:
		return CustomProviderProtocolResponses, true
	default:
		return "", false
	}
}

// ModerationConfig is the operator-configured response/history moderation policy.
// When Enabled, each incoming request's conversation history is keyword-scanned
// (Words, case-insensitive); on a hit the configured pool Model is asked to rewrite
// the offending prior-turn text (preserving code verbatim) before forwarding upstream.
// AutoTranslate appends an English translation when a Chinese word is added (UI side).
type ModerationConfig struct {
	Enabled       bool     `json:"enabled"`
	Model         string   `json:"model"`
	AutoTranslate bool     `json:"auto_translate"`
	Words         []string `json:"words"`
}

func Open(path string) (*Store, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	db, err := sql.Open("sqlite3", path+sep+"_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	// Single-connection WRITE pool: SQLite has one writer, so serializing writes here
	// means our concurrency never triggers SQLITE_BUSY. Init() sets WAL on this pool.
	db.SetMaxOpenConns(1)

	// Separate multi-connection READ pool against the same WAL file, so per-request
	// SELECTs run in parallel instead of queuing behind the single writer. An
	// in-memory shared-cache test DB reuses the one handle — a second pool there adds
	// connection-lifetime/visibility quirks for no real gain (the split only helps
	// file-backed databases under concurrent load).
	rdb := db
	if !strings.Contains(path, "mode=memory") {
		r, rerr := sql.Open("sqlite3", path+sep+"_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL")
		if rerr != nil {
			_ = db.Close()
			return nil, rerr
		}
		n := readPoolSize()
		r.SetMaxOpenConns(n)
		r.SetMaxIdleConns(n)
		rdb = r
	}
	return &Store{db: db, rdb: rdb}, nil
}

// readPoolSize uses a 2/4/8 memory tier and remains overridable via
// CODEX_POOL_DB_MAX_READ_CONNS. Each SQLite connection owns a page cache, so CPU
// count alone is a poor default on a many-core, low-memory VPS.
func readPoolSize() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_POOL_DB_MAX_READ_CONNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return readPoolSizeForMemory(hostOrCgroupMemoryLimit())
}

func readPoolSizeForMemory(bytes uint64) int {
	const gib = uint64(1024 * 1024 * 1024)
	switch {
	case bytes > 0 && bytes < 2*gib:
		return 2
	case bytes > 0 && bytes < 8*gib:
		return 4
	default:
		return 8
	}
}

func hostOrCgroupMemoryLimit() uint64 {
	var limit uint64
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "" || value == "max" {
			continue
		}
		if n, err := strconv.ParseUint(value, 10, 64); err == nil && n > 0 && n < 1<<60 {
			limit = n
			break
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "MemTotal:" {
				if kib, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
					host := kib * 1024
					if limit == 0 || host < limit {
						limit = host
					}
				}
				break
			}
		}
	}
	return limit
}

func OpenInMemory() (*Store, error) {
	return Open("file:codex_pool_test?mode=memory&cache=shared")
}

func (s *Store) Close() error {
	if s.rdb != nil && s.rdb != s.db {
		_ = s.rdb.Close()
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func Now() int64 {
	return time.Now().Unix()
}

func (s *Store) Init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA cache_size=-16384; PRAGMA mmap_size=67108864; PRAGMA temp_store=MEMORY;`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	// Create lifecycle management tables
	if _, err := s.db.ExecContext(ctx, lifecycleSchemaSQL); err != nil {
		return err
	}
	if err := s.migrate(ctx); err != nil {
		return err
	}
	if err := s.migrateAccountRateLimits(ctx); err != nil {
		return err
	}
	if err := s.migrateLifecycle(ctx); err != nil {
		return err
	}
	now := Now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('usage_accuracy_cutover_at',?,?) ON CONFLICT(key) DO NOTHING`, strconv.FormatInt(now, 10), now); err != nil {
		return err
	}
	if err := s.migrateContextJournalTTL(ctx, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO groups(name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, created_at, updated_at)
VALUES('cyber', '', 'prepend', 1, 0, ?, ?)
ON CONFLICT(name) DO NOTHING`, now, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO egress_profiles(id, name, type, endpoint, region, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at)
VALUES(?, 'direct', 'direct', '', '', 1, 'healthy', 0, 0, '', 0, 128, ?, ?)
ON CONFLICT(id) DO NOTHING`, DefaultDirectEgressID, now, now)
	if err != nil {
		return err
	}
	// Migrate only the two historical built-in defaults. Other positive limits are
	// administrator choices and must remain untouched.
	if _, err := s.db.ExecContext(ctx, `UPDATE egress_profiles SET max_concurrency=0, updated_at=? WHERE id=? AND type='direct' AND max_concurrency=128`, now, DefaultDirectEgressID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE egress_profiles SET max_concurrency=0, updated_at=? WHERE id='egress_sidecar' AND type='curl_cffi_sidecar' AND max_concurrency=16`, now); err != nil {
		return err
	}
	// Seed default OpenAI-compatible custom providers so the adapter works out of the
	// box (the "适配 deepseek / 硅基流动" baseline). Operators can edit base_url / model
	// list from the admin UI, disable them, or add more (Kimi, OpenRouter, vLLM, …).
	// base_url carries the OpenAI-compat "/v1" prefix; the adapter appends
	// /chat/completions and /models. ON CONFLICT keeps any operator edits on restart.
	seededProviders := []struct{ id, name, baseURL, models string }{
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", `["deepseek-chat","deepseek-reasoner"]`},
		// SiliconFlow (硅基流动) aggregates many open models behind one OpenAI-compatible
		// API; its catalog is large and changes often, so seed an empty model list and
		// rely on auto-discovery ({base}/models) once an API key is imported.
		{"siliconflow", "SiliconFlow 硅基流动", "https://api.siliconflow.cn/v1", `[]`},
	}
	for _, p := range seededProviders {
		if _, err = s.db.ExecContext(ctx, `
INSERT INTO custom_providers(id, name, base_url, upstream_protocol, enabled, auto_discover_models, models_json, created_at, updated_at)
VALUES(?, ?, ?, ?, 1, 1, ?, ?, ?)
ON CONFLICT(id) DO NOTHING`, p.id, p.name, p.baseURL, CustomProviderProtocolChatCompletions, p.models, now, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateContextJournalTTL(ctx context.Context, now int64) error {
	const marker = "context_journal_ttl_1h_migrated"
	var migrated int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, marker).Scan(&migrated); err != nil {
		return err
	}
	if migrated > 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 86400 was the shipped default. Migrate only that known value once; any other
	// administrator-defined retention remains untouched.
	if _, err := tx.ExecContext(ctx, `UPDATE settings SET value='3600', updated_at=? WHERE key='context_journal_ttl_seconds' AND value='86400'`, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)`, marker, "1", now); err != nil {
		return err
	}
	return tx.Commit()
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS tenants(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  email TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT 'user',
  status TEXT NOT NULL DEFAULT 'active',
  password_hash TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE TABLE IF NOT EXISTS user_sessions(
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON user_sessions(user_id);
CREATE TABLE IF NOT EXISTS projects(
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  name TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT 'cyber',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys(
  key_hash TEXT PRIMARY KEY,
  tenant_id TEXT,
  project_id TEXT,
  key_type TEXT NOT NULL DEFAULT 'downstream',
  label TEXT,
  group_name TEXT NOT NULL DEFAULT '',
  force_model TEXT NOT NULL DEFAULT '',
  force_effort TEXT NOT NULL DEFAULT '',
  provider_hint TEXT NOT NULL DEFAULT 'auto',
  enabled INTEGER NOT NULL DEFAULT 1,
  expires_at INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  secret TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS groups(
  name TEXT PRIMARY KEY,
  system_prompt TEXT NOT NULL DEFAULT '',
  prompt_mode TEXT NOT NULL DEFAULT 'prepend',
  system_prompt_apply_to_compaction INTEGER NOT NULL DEFAULT 1,
  virtual_2m_enabled INTEGER NOT NULL DEFAULT 0,
  model_instructions_enabled INTEGER NOT NULL DEFAULT 0,
  model_instructions_files TEXT NOT NULL DEFAULT '[]',
  force_model TEXT NOT NULL DEFAULT '',
  force_effort TEXT NOT NULL DEFAULT '',
  default_egress_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS accounts(
  id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  group_name TEXT NOT NULL DEFAULT 'cyber',
  upstream_account_id TEXT,
  chatgpt_user_id TEXT,
  email TEXT,
  plan_type TEXT,
  provider TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active',
  is_fedramp INTEGER NOT NULL DEFAULT 0,
  quarantine_until INTEGER NOT NULL DEFAULT 0,
  quarantine_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_group_status ON accounts(group_name, status);
CREATE TABLE IF NOT EXISTS account_auth_tokens(
  account_id TEXT PRIMARY KEY,
  access_token TEXT,
  refresh_token TEXT,
  openai_api_key TEXT,
  id_token_raw TEXT,
  last_refresh INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  scopes TEXT NOT NULL DEFAULT '',
  oauth_rate_limit_tier TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS account_kiro_credentials(
  account_id TEXT PRIMARY KEY,
  auth_method TEXT NOT NULL,
  client_id TEXT NOT NULL DEFAULT '',
  client_secret TEXT NOT NULL DEFAULT '',
  profile_arn TEXT NOT NULL DEFAULT '',
  auth_region TEXT NOT NULL DEFAULT 'us-east-1',
  api_region TEXT NOT NULL DEFAULT 'us-east-1',
  machine_id TEXT NOT NULL DEFAULT '',
  kiro_api_key TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL DEFAULT '',
  credential_hash TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_kiro_credential_hash ON account_kiro_credentials(credential_hash) WHERE credential_hash <> '';
CREATE TABLE IF NOT EXISTS account_model_capabilities(
  account_id TEXT NOT NULL,
  model_slug TEXT NOT NULL,
  native_context_window INTEGER NOT NULL DEFAULT 0,
  native_max_context_window INTEGER NOT NULL DEFAULT 0,
  effective_context_window_percent INTEGER NOT NULL DEFAULT 100,
  auto_compact_token_limit INTEGER NOT NULL DEFAULT 0,
  visibility TEXT NOT NULL DEFAULT '',
  etag TEXT NOT NULL DEFAULT '',
  raw_model_json_hash TEXT NOT NULL DEFAULT '',
  raw_model_json TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'probe',
  last_probe_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, model_slug),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
-- Composite index covering the model capability JOIN query used by capability-aware routing:
-- SELECT DISTINCT c.account_id FROM account_model_capabilities c JOIN accounts a ON a.id = c.account_id
-- WHERE a.group_name = ? AND a.status = 'active' AND c.model_slug = ?
CREATE INDEX IF NOT EXISTS idx_capabilities_model ON account_model_capabilities(model_slug, account_id);
CREATE TABLE IF NOT EXISTS kiro_runtime_capabilities(
  account_id TEXT NOT NULL,
  endpoint_hash TEXT NOT NULL,
  model TEXT NOT NULL,
  model_state TEXT NOT NULL DEFAULT 'unknown',
  thinking_state TEXT NOT NULL DEFAULT 'unknown',
  cache_capability TEXT NOT NULL DEFAULT 'unknown',
	cache_point_state TEXT NOT NULL DEFAULT 'unknown',
  cache_reuse_state TEXT NOT NULL DEFAULT 'unknown',
  cache_reuse_evidence TEXT NOT NULL DEFAULT '',
  cache_reuse_credit_reduction_percent REAL NOT NULL DEFAULT 0,
  cache_reuse_probed_at INTEGER NOT NULL DEFAULT 0,
  observations INTEGER NOT NULL DEFAULT 0,
  metering_events INTEGER NOT NULL DEFAULT 0,
  cache_reported_observations INTEGER NOT NULL DEFAULT 0,
  cache_hit_observations INTEGER NOT NULL DEFAULT 0,
  consecutive_unreported INTEGER NOT NULL DEFAULT 0,
  unknown_cache_schema_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, endpoint_hash, model),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_kiro_runtime_verified ON kiro_runtime_capabilities(account_id, endpoint_hash, model_state, model);
CREATE TABLE IF NOT EXISTS affinity_bindings(
  route_key_hash TEXT PRIMARY KEY,
  route_key TEXT NOT NULL,
  source TEXT NOT NULL,
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  egress_id TEXT NOT NULL DEFAULT '',
  epoch INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS egress_profiles(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  endpoint TEXT NOT NULL DEFAULT '',
  chain_proxy TEXT NOT NULL DEFAULT '',
  region TEXT NOT NULL DEFAULT '',
  exit_ip TEXT NOT NULL DEFAULT '',
  stream_capable INTEGER NOT NULL DEFAULT 1,
  health TEXT NOT NULL DEFAULT 'healthy',
  latency_millis INTEGER NOT NULL DEFAULT 0,
  cf_score INTEGER NOT NULL DEFAULT 0,
  last_cf_ray TEXT NOT NULL DEFAULT '',
  cooldown_until INTEGER NOT NULL DEFAULT 0,
  max_concurrency INTEGER NOT NULL DEFAULT 16,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  proxy_auth_mode TEXT NOT NULL DEFAULT '',
  proxy_api_key TEXT NOT NULL DEFAULT '',
  ip_mode TEXT NOT NULL DEFAULT '',
  provider_key TEXT NOT NULL DEFAULT '',
  dynamic_config_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS egress_pools(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  purpose TEXT NOT NULL DEFAULT '',
  assignment_strategy TEXT NOT NULL DEFAULT 'sticky_least_used',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS egress_pool_members(
  pool_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  capacity INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(pool_id, egress_id),
  FOREIGN KEY(pool_id) REFERENCES egress_pools(id) ON DELETE CASCADE,
  FOREIGN KEY(egress_id) REFERENCES egress_profiles(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_egress_pool_members_egress ON egress_pool_members(egress_id);
CREATE TABLE IF NOT EXISTS group_egress_policies(
  group_name TEXT PRIMARY KEY,
  registration_pool_id TEXT NOT NULL DEFAULT '',
  runtime_pool_id TEXT NOT NULL DEFAULT '',
  assignment_strategy TEXT NOT NULL DEFAULT 'sticky_least_used',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(group_name) REFERENCES groups(name) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS account_egress_bindings(
  account_id TEXT PRIMARY KEY,
  primary_egress_id TEXT NOT NULL,
  standby_egress_ids TEXT NOT NULL DEFAULT '',
  cookie_jar_key TEXT NOT NULL DEFAULT '',
  cooldown_until INTEGER NOT NULL DEFAULT 0,
  recheck_pending INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
-- Index for egress binding cooldown queries used by shortestCooldown:
-- SELECT account_id FROM account_egress_bindings WHERE cooldown_until <= ? AND recheck_pending = 0
CREATE INDEX IF NOT EXISTS idx_egress_binding_cooldown ON account_egress_bindings(cooldown_until, recheck_pending);
CREATE TABLE IF NOT EXISTS cf_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  status INTEGER NOT NULL,
  cf_ray TEXT NOT NULL DEFAULT '',
  category TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cf_events_account_egress_time ON cf_events(account_id, egress_id, created_at);
CREATE INDEX IF NOT EXISTS idx_cf_events_egress_time ON cf_events(egress_id, created_at);
CREATE INDEX IF NOT EXISTS idx_cf_events_created_at ON cf_events(created_at);
CREATE TABLE IF NOT EXISTS virtual_context_ledger(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  route_key_hash TEXT NOT NULL,
  account_id TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  prompt_cache_key TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL DEFAULT '',
  content TEXT NOT NULL DEFAULT '',
  token_estimate INTEGER NOT NULL DEFAULT 0,
  raw_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_virtual_ledger_route_time ON virtual_context_ledger(route_key_hash, created_at);
-- TTL cleanup optimization: index on created_at for efficient range deletes
CREATE INDEX IF NOT EXISTS idx_virtual_ledger_created_at ON virtual_context_ledger(created_at);
CREATE TABLE IF NOT EXISTS billing_holds(
  id TEXT PRIMARY KEY,
  route_key_hash TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL,
  estimated_tokens INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'held',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_billing_holds_status_updated ON billing_holds(status, updated_at);
CREATE TABLE IF NOT EXISTS usage_records(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  usage_event_id TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL,
  route_key_hash TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  usage_provider TEXT NOT NULL DEFAULT '',
  usage_source TEXT NOT NULL DEFAULT '',
  cache_read_present INTEGER NOT NULL DEFAULT 0,
  cache_creation_present INTEGER NOT NULL DEFAULT 0,
  compatibility_losses_json TEXT NOT NULL DEFAULT '',
  cache_capability TEXT NOT NULL DEFAULT '',
  estimated INTEGER NOT NULL DEFAULT 0,
  cache_miss_tokens INTEGER NOT NULL DEFAULT 0,
  cache_total_input_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_5m_tokens INTEGER NOT NULL DEFAULT 0,
  cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0,
  affinity_source TEXT NOT NULL DEFAULT '',
  prompt_cache_key_present INTEGER NOT NULL DEFAULT 0,
  prompt_cache_key_source TEXT NOT NULL DEFAULT '',
  stable_prefix_source TEXT NOT NULL DEFAULT '',
  stable_prefix_reason TEXT NOT NULL DEFAULT '',
  stable_prefix_bytes INTEGER NOT NULL DEFAULT 0,
  retention_effective TEXT NOT NULL DEFAULT '',
  retention_source TEXT NOT NULL DEFAULT '',
  claude_cache_ttl TEXT NOT NULL DEFAULT '',
  cache_control_injected INTEGER NOT NULL DEFAULT 0,
  cache_breakpoint_count INTEGER NOT NULL DEFAULT 0,
  cache_breakpoints_json TEXT NOT NULL DEFAULT '',
  unwritten_tail_tokens INTEGER NOT NULL DEFAULT 0,
  max_possible_cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_hit_after_prewarm INTEGER NOT NULL DEFAULT 0,
  singleflight_waited_requests INTEGER NOT NULL DEFAULT 0,
  diagnostics_miss_reason TEXT NOT NULL DEFAULT '',
  latest_user_cache_control INTEGER NOT NULL DEFAULT 0,
  latest_user_auto_context_cache_control INTEGER NOT NULL DEFAULT 0,
  latest_user_tail_cache_control INTEGER NOT NULL DEFAULT 0,
  latest_user_tool_result_cache_control INTEGER NOT NULL DEFAULT 0,
  route_epoch INTEGER NOT NULL DEFAULT 0,
  raw_usage_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS context_journal(
 response_id TEXT PRIMARY KEY,
 affinity_hash TEXT NOT NULL DEFAULT '', account_id TEXT NOT NULL DEFAULT '',
 encrypted_payload TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_context_journal_expires ON context_journal(expires_at);
CREATE INDEX IF NOT EXISTS idx_usage_records_created_model ON usage_records(created_at, model);
CREATE INDEX IF NOT EXISTS idx_usage_records_account_created ON usage_records(account_id, created_at);
CREATE TABLE IF NOT EXISTS model_quality_status(
  group_name TEXT NOT NULL,
  model_slug TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'unknown',
  last_outcome TEXT NOT NULL DEFAULT '',
  last_probe_at INTEGER NOT NULL DEFAULT 0,
  last_pass_at INTEGER NOT NULL DEFAULT 0,
  consecutive_anomalies INTEGER NOT NULL DEFAULT 0,
  consecutive_errors INTEGER NOT NULL DEFAULT 0,
  total_checks INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  last_probe_id TEXT NOT NULL DEFAULT '',
  last_expected TEXT NOT NULL DEFAULT '',
  last_actual TEXT NOT NULL DEFAULT '',
  last_returned_model TEXT NOT NULL DEFAULT '',
  last_latency_ms INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(group_name, model_slug, provider)
);
CREATE INDEX IF NOT EXISTS idx_model_quality_status_state ON model_quality_status(state, last_probe_at);
CREATE TABLE IF NOT EXISTS model_quality_runs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  group_name TEXT NOT NULL,
  model_slug TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  probe_id TEXT NOT NULL,
  phase TEXT NOT NULL,
  outcome TEXT NOT NULL,
  expected TEXT NOT NULL DEFAULT '',
  actual TEXT NOT NULL DEFAULT '',
  returned_model TEXT NOT NULL DEFAULT '',
  http_status INTEGER NOT NULL DEFAULT 0,
  error_kind TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_model_quality_runs_combo_time ON model_quality_runs(group_name, model_slug, provider, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_model_quality_runs_created_at ON model_quality_runs(created_at);
CREATE TABLE IF NOT EXISTS account_session_cookies(
  account_id TEXT PRIMARY KEY,
  cookie TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS account_codex_reauth_config(
  account_id TEXT PRIMARY KEY,
  login_email TEXT NOT NULL DEFAULT '',
  encrypted_password TEXT NOT NULL DEFAULT '',
  encrypted_otp_url TEXT NOT NULL DEFAULT '',
  target_workspace_id TEXT NOT NULL DEFAULT '',
  auto_enabled INTEGER NOT NULL DEFAULT 0,
  last_status TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS account_codex_reauth_jobs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  reason TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_codex_reauth_jobs_account_status ON account_codex_reauth_jobs(account_id, status, created_at);
CREATE TABLE IF NOT EXISTS account_injected_cookies(
  account_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  upstream_host TEXT NOT NULL DEFAULT '',
  cookie_header TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  exit_ip TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, egress_id, upstream_host),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS account_rate_limits(
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  limiter_type TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  used_percent REAL NOT NULL DEFAULT -1,
  limit_tokens INTEGER NOT NULL DEFAULT -1,
  remaining_tokens INTEGER NOT NULL DEFAULT -1,
  limit_requests INTEGER NOT NULL DEFAULT -1,
  remaining_requests INTEGER NOT NULL DEFAULT -1,
  reset_at INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  raw_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, provider, model, limiter_type),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS codex_reset_credit_consumptions(
  account_id TEXT NOT NULL,
  seven_day_reset_at INTEGER NOT NULL,
  redeem_request_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'in_progress',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, seven_day_reset_at),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS settings(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS custom_providers(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  base_url TEXT NOT NULL DEFAULT '',
  upstream_protocol TEXT NOT NULL DEFAULT 'chat_completions',
  enabled INTEGER NOT NULL DEFAULT 1,
  auto_discover_models INTEGER NOT NULL DEFAULT 1,
  models_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS upstream_error_rules(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 100,
  providers_json TEXT NOT NULL DEFAULT '[]',
  entrypoints_json TEXT NOT NULL DEFAULT '[]',
  model_patterns_json TEXT NOT NULL DEFAULT '[]',
  status_codes_json TEXT NOT NULL DEFAULT '[]',
  body_keywords_json TEXT NOT NULL DEFAULT '[]',
  match_mode TEXT NOT NULL DEFAULT 'any',
  account_action TEXT NOT NULL DEFAULT 'builtin',
  downstream_action TEXT NOT NULL DEFAULT 'builtin',
  response_status INTEGER NOT NULL DEFAULT 0,
  custom_message TEXT NOT NULL DEFAULT '',
  cooldown_seconds INTEGER NOT NULL DEFAULT 0,
  prefer_retry_after INTEGER NOT NULL DEFAULT 0,
  idle_seconds INTEGER NOT NULL DEFAULT 0,
  idle_ping_seconds INTEGER NOT NULL DEFAULT 15,
  skip_log INTEGER NOT NULL DEFAULT 0,
  filter_account_action INTEGER NOT NULL DEFAULT 0,
  keyword_case_sensitive INTEGER NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_upstream_error_rules_enabled_priority ON upstream_error_rules(enabled, priority, created_at);
CREATE TABLE IF NOT EXISTS audit_log(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL DEFAULT '',
  account_label TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_log_time ON audit_log(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_log_account ON audit_log(account_id, id DESC);

CREATE TABLE IF NOT EXISTS registration_jobs(
  id TEXT PRIMARY KEY,
  platform TEXT NOT NULL DEFAULT 'chatgpt',
  method TEXT NOT NULL,
  total INTEGER NOT NULL DEFAULT 0,
  succeeded INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  config_json TEXT NOT NULL DEFAULT '{}',
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reg_jobs_status ON registration_jobs(status);
CREATE INDEX IF NOT EXISTS idx_reg_jobs_platform ON registration_jobs(platform);

CREATE TABLE IF NOT EXISTS registration_records(
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  account_id TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL DEFAULT '',
  phone TEXT NOT NULL DEFAULT '',
  tier TEXT NOT NULL DEFAULT 'free',
  cost_usd REAL NOT NULL DEFAULT 0,
  duration_seconds INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending',
  error TEXT NOT NULL DEFAULT '',
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reg_records_job ON registration_records(job_id);
CREATE INDEX IF NOT EXISTS idx_reg_records_status ON registration_records(status);

CREATE TABLE IF NOT EXISTS registration_task_events(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  level TEXT NOT NULL DEFAULT 'info',
  message TEXT NOT NULL,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reg_events_task ON registration_task_events(task_id);
CREATE INDEX IF NOT EXISTS idx_reg_events_created_at ON registration_task_events(created_at);

CREATE TABLE IF NOT EXISTS provider_settings(
  id TEXT PRIMARY KEY,
  provider_type TEXT NOT NULL,
  provider_key TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  priority INTEGER NOT NULL DEFAULT 0,
  config_json TEXT NOT NULL DEFAULT '{}',
  auth_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  UNIQUE(provider_type, provider_key)
);
CREATE INDEX IF NOT EXISTS idx_provider_settings_type ON provider_settings(provider_type);

CREATE TABLE IF NOT EXISTS sms_blacklist(
  phone TEXT PRIMARY KEY,
  reason TEXT NOT NULL DEFAULT '',
  fail_count INTEGER NOT NULL DEFAULT 1,
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS account_lifecycle_status(
  account_id TEXT PRIMARY KEY,
  validity_status TEXT NOT NULL DEFAULT 'unknown',
  subscription_tier TEXT NOT NULL DEFAULT 'free',
  subscription_expires_at INTEGER NOT NULL DEFAULT 0,
  last_health_check_at INTEGER NOT NULL DEFAULT 0,
  last_token_refresh_at INTEGER NOT NULL DEFAULT 0,
  health_check_fail_count INTEGER NOT NULL DEFAULT 0,
  summary_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS registration_stats_daily(
  date TEXT NOT NULL,
  platform TEXT NOT NULL,
  method TEXT NOT NULL,
  provider_key TEXT NOT NULL DEFAULT 'unknown',
  total INTEGER NOT NULL DEFAULT 0,
  succeeded INTEGER NOT NULL DEFAULT 0,
  failed INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (date, platform, method, provider_key)
);
`

// migrate applies additive schema changes to databases created by an older
// build. Each statement adds a column newer code relies on; SQLite errors with
// "duplicate column name" when it already exists, which is ignored so the
// migration is idempotent. New tables are handled by CREATE TABLE IF NOT EXISTS.
// CREATE INDEX IF NOT EXISTS is also idempotent and adds new indexes to old DBs.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`ALTER TABLE api_keys ADD COLUMN group_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN force_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN force_effort TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN provider_hint TEXT NOT NULL DEFAULT 'auto'`,
		`ALTER TABLE api_keys ADD COLUMN key_type TEXT NOT NULL DEFAULT 'downstream'`,
		`ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE api_keys ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE groups ADD COLUMN force_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN force_effort TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN default_egress_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN model_instructions_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE groups ADD COLUMN model_instructions_files TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE egress_profiles ADD COLUMN exit_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN chain_proxy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE account_auth_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN oauth_rate_limit_tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_model_capabilities ADD COLUMN raw_model_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN egress_id TEXT NOT NULL DEFAULT ''`,
		// Multi-user portal: end-user credentials/roles, key ownership, and per-user
		// usage attribution (older DBs created before the portal existed).
		`ALTER TABLE users ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'`,
		`ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`,
		`ALTER TABLE users ADD COLUMN password_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN secret TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN api_key_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN usage_provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN usage_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN cache_read_present INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_present INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN compatibility_losses_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN cache_capability TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN estimated INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_miss_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_total_input_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_5m_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_creation_1h_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN affinity_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_present INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN stable_prefix_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN stable_prefix_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN stable_prefix_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN retention_effective TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN retention_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN claude_cache_ttl TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN cache_control_injected INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_breakpoint_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_breakpoints_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN unwritten_tail_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN max_possible_cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN cache_hit_after_prewarm INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN singleflight_waited_requests INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_point_state TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_state TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_evidence TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_credit_reduction_percent REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_probed_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN diagnostics_miss_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_auto_context_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_tail_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_tool_result_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN route_epoch INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN usage_event_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_records_event_id ON usage_records(usage_event_id) WHERE usage_event_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_created_model ON usage_records(created_at, model)`,
		// Per-account usage rollups (the admin account list's UsageSummaryByAccountIDs)
		// filter `account_id IN (...) AND created_at >= ?`. Without an account_id-leading
		// index SQLite scans the whole usage_records history per page load; this composite
		// turns it into per-account range seeks. Biggest single win for admin list latency.
		`CREATE INDEX IF NOT EXISTS idx_usage_records_account_created ON usage_records(account_id, created_at)`,
		// Cooldown→health-recheck gate: a benched account stays out of the candidate
		// pool until a liveness probe confirms it recovered (older DBs created before
		// the recheck loop existed).
		`ALTER TABLE account_egress_bindings ADD COLUMN recheck_pending INTEGER NOT NULL DEFAULT 0`,
		// SMS multi-platform tracking: record which provider + country a registration
		// used, and what it cost, so the local stats API can aggregate per-platform
		// per-country success rates (Phase 7 — additive migration).
		`ALTER TABLE registration_records ADD COLUMN sms_provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE registration_records ADD COLUMN sms_country TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE registration_records ADD COLUMN sms_cost REAL NOT NULL DEFAULT 0`,
		// CLIPProxy API whitelist mode + exit-region validation (Phase 8 — additive).
		// ProxyAuthMode selects credential vs api_whitelist IP acquisition; ProxyAPIKey is
		// the cliproxy account token used in api_whitelist mode.
		`ALTER TABLE egress_profiles ADD COLUMN proxy_auth_mode TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN proxy_api_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN ip_mode TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN provider_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN dynamic_config_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE custom_providers ADD COLUMN upstream_protocol TEXT NOT NULL DEFAULT 'chat_completions'`,
		`CREATE TABLE IF NOT EXISTS egress_pools(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  purpose TEXT NOT NULL DEFAULT '',
  assignment_strategy TEXT NOT NULL DEFAULT 'sticky_least_used',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS egress_pool_members(
  pool_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  capacity INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(pool_id, egress_id),
  FOREIGN KEY(pool_id) REFERENCES egress_pools(id) ON DELETE CASCADE,
  FOREIGN KEY(egress_id) REFERENCES egress_profiles(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_egress_pool_members_egress ON egress_pool_members(egress_id)`,
		`CREATE TABLE IF NOT EXISTS group_egress_policies(
  group_name TEXT PRIMARY KEY,
  registration_pool_id TEXT NOT NULL DEFAULT '',
  runtime_pool_id TEXT NOT NULL DEFAULT '',
  assignment_strategy TEXT NOT NULL DEFAULT 'sticky_least_used',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(group_name) REFERENCES groups(name) ON DELETE CASCADE
)`,
		// New indexes for query optimization (idempotent CREATE INDEX IF NOT EXISTS)
		// Covers the model capability JOIN used by capability-aware routing
		`CREATE INDEX IF NOT EXISTS idx_capabilities_model ON account_model_capabilities(model_slug, account_id)`,
		`CREATE TABLE IF NOT EXISTS kiro_runtime_capabilities(
  account_id TEXT NOT NULL,
  endpoint_hash TEXT NOT NULL,
  model TEXT NOT NULL,
  model_state TEXT NOT NULL DEFAULT 'unknown',
  thinking_state TEXT NOT NULL DEFAULT 'unknown',
  cache_capability TEXT NOT NULL DEFAULT 'unknown',
	cache_point_state TEXT NOT NULL DEFAULT 'unknown',
  cache_reuse_state TEXT NOT NULL DEFAULT 'unknown',
  cache_reuse_evidence TEXT NOT NULL DEFAULT '',
  cache_reuse_credit_reduction_percent REAL NOT NULL DEFAULT 0,
  cache_reuse_probed_at INTEGER NOT NULL DEFAULT 0,
  observations INTEGER NOT NULL DEFAULT 0,
  metering_events INTEGER NOT NULL DEFAULT 0,
  cache_reported_observations INTEGER NOT NULL DEFAULT 0,
  cache_hit_observations INTEGER NOT NULL DEFAULT 0,
  consecutive_unreported INTEGER NOT NULL DEFAULT 0,
  unknown_cache_schema_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, endpoint_hash, model),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_kiro_runtime_verified ON kiro_runtime_capabilities(account_id, endpoint_hash, model_state, model)`,
		`CREATE TABLE IF NOT EXISTS account_kiro_credentials(
  account_id TEXT PRIMARY KEY,
  auth_method TEXT NOT NULL,
  client_id TEXT NOT NULL DEFAULT '',
  client_secret TEXT NOT NULL DEFAULT '',
  profile_arn TEXT NOT NULL DEFAULT '',
  auth_region TEXT NOT NULL DEFAULT 'us-east-1',
  api_region TEXT NOT NULL DEFAULT 'us-east-1',
  machine_id TEXT NOT NULL DEFAULT '',
  kiro_api_key TEXT NOT NULL DEFAULT '',
  endpoint TEXT NOT NULL DEFAULT '',
  credential_hash TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_kiro_credential_hash ON account_kiro_credentials(credential_hash) WHERE credential_hash <> ''`,
		// Covers egress binding cooldown queries used by shortestCooldown
		`CREATE INDEX IF NOT EXISTS idx_egress_binding_cooldown ON account_egress_bindings(cooldown_until, recheck_pending)`,
		// Covers account detail drawer audit lookups without scanning the global audit log.
		`CREATE INDEX IF NOT EXISTS idx_audit_log_account ON audit_log(account_id, id DESC)`,
		`CREATE TABLE IF NOT EXISTS account_codex_reauth_config(
  account_id TEXT PRIMARY KEY,
  login_email TEXT NOT NULL DEFAULT '',
  encrypted_password TEXT NOT NULL DEFAULT '',
  encrypted_otp_url TEXT NOT NULL DEFAULT '',
  target_workspace_id TEXT NOT NULL DEFAULT '',
  auto_enabled INTEGER NOT NULL DEFAULT 0,
  last_status TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE TABLE IF NOT EXISTS account_codex_reauth_jobs(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'queued',
  reason TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_reauth_jobs_account_status ON account_codex_reauth_jobs(account_id, status, created_at)`,
		`CREATE TABLE IF NOT EXISTS codex_reset_credit_consumptions(
  account_id TEXT NOT NULL,
  seven_day_reset_at INTEGER NOT NULL,
  redeem_request_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'in_progress',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, seven_day_reset_at),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE TABLE IF NOT EXISTS upstream_error_rules(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 100,
  providers_json TEXT NOT NULL DEFAULT '[]',
  entrypoints_json TEXT NOT NULL DEFAULT '[]',
  model_patterns_json TEXT NOT NULL DEFAULT '[]',
  status_codes_json TEXT NOT NULL DEFAULT '[]',
  body_keywords_json TEXT NOT NULL DEFAULT '[]',
  match_mode TEXT NOT NULL DEFAULT 'any',
  account_action TEXT NOT NULL DEFAULT 'builtin',
  downstream_action TEXT NOT NULL DEFAULT 'builtin',
  response_status INTEGER NOT NULL DEFAULT 0,
  custom_message TEXT NOT NULL DEFAULT '',
  cooldown_seconds INTEGER NOT NULL DEFAULT 0,
  prefer_retry_after INTEGER NOT NULL DEFAULT 0,
  idle_seconds INTEGER NOT NULL DEFAULT 0,
  idle_ping_seconds INTEGER NOT NULL DEFAULT 15,
  skip_log INTEGER NOT NULL DEFAULT 0,
  description TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`ALTER TABLE upstream_error_rules ADD COLUMN filter_account_action INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE upstream_error_rules ADD COLUMN keyword_case_sensitive INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_upstream_error_rules_enabled_priority ON upstream_error_rules(enabled, priority, created_at)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.backfillUsageCacheDiagnostics(ctx)
}

func (s *Store) migrateAccountRateLimits(ctx context.Context) error {
	rows, err := s.rdb.QueryContext(ctx, `PRAGMA table_info(account_rate_limits)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasModel := false
	hasLimiterType := false
	accountIDPK := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		switch name {
		case "model":
			hasModel = true
		case "limiter_type":
			hasLimiterType = true
		case "account_id":
			accountIDPK = pk == 1
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if hasModel && hasLimiterType && !accountIDPK {
		_, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_rate_limits_route ON account_rate_limits(account_id, provider, model, reset_at)`)
		return err
	}
	_, err = s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS account_rate_limits_new(
  account_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  limiter_type TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  used_percent REAL NOT NULL DEFAULT -1,
  limit_tokens INTEGER NOT NULL DEFAULT -1,
  remaining_tokens INTEGER NOT NULL DEFAULT -1,
  limit_requests INTEGER NOT NULL DEFAULT -1,
  remaining_requests INTEGER NOT NULL DEFAULT -1,
  reset_at INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '',
  raw_json TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, provider, model, limiter_type),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
INSERT OR REPLACE INTO account_rate_limits_new(
  account_id, provider, model, limiter_type, source, used_percent, limit_tokens, remaining_tokens,
  limit_requests, remaining_requests, reset_at, status, raw_json, updated_at
)
SELECT account_id, provider, '', COALESCE(NULLIF(source, ''), 'default'), source, used_percent,
  limit_tokens, remaining_tokens, limit_requests, remaining_requests, reset_at, status, raw_json, updated_at
FROM account_rate_limits;
DROP TABLE account_rate_limits;
ALTER TABLE account_rate_limits_new RENAME TO account_rate_limits;
CREATE INDEX IF NOT EXISTS idx_rate_limits_route ON account_rate_limits(account_id, provider, model, reset_at);
`)
	return err
}

// migrateLifecycle adds lifecycle management columns to existing accounts table
func (s *Store) migrateLifecycle(ctx context.Context) error {
	stmts := []string{
		`ALTER TABLE accounts ADD COLUMN registration_method TEXT NOT NULL DEFAULT 'manual'`,
		`ALTER TABLE accounts ADD COLUMN phone TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN subscription_status TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE accounts ADD COLUMN subscription_expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN last_validity_check_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN registration_task_id TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return err
		}
	}
	return nil
}

func (s *Store) GetGroup(ctx context.Context, name string) (Group, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, created_at, updated_at FROM groups WHERE name = ?`, name)
	var g Group
	return scanGroup(row.Scan, &g)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, created_at, updated_at FROM groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if _, err := scanGroup(rows.Scan, &g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func scanGroup(scan func(...interface{}) error, g *Group) (Group, error) {
	var apply, virtual, modelInstructionsEnabled int
	var filesJSON string
	err := scan(&g.Name, &g.SystemPrompt, &g.PromptMode, &apply, &virtual, &modelInstructionsEnabled, &filesJSON, &g.ForceModel, &g.ForceEffort, &g.DefaultEgressID, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return *g, err
	}
	g.SystemPromptApplyToCompaction = apply != 0
	g.Virtual2MEnabled = virtual != 0
	g.ModelInstructionsEnabled = modelInstructionsEnabled != 0
	g.ModelInstructionsFiles = decodeStringList(filesJSON)
	return *g, nil
}

func encodeStringList(values []string) string {
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			cleaned = append(cleaned, value)
		}
	}
	raw, err := json.Marshal(cleaned)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func decodeStringList(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func (s *Store) UpdateGroup(ctx context.Context, g Group) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET system_prompt = ?, prompt_mode = ?, system_prompt_apply_to_compaction = ?, virtual_2m_enabled = ?, model_instructions_enabled = ?, model_instructions_files = ?, force_model = ?, force_effort = ?, default_egress_id = ?, updated_at = ? WHERE name = ?`,
		g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.Virtual2MEnabled), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), g.ForceModel, g.ForceEffort, g.DefaultEgressID, Now(), g.Name)
	return err
}

// CreateGroup inserts a new group (multi-group support — the pool is no longer limited
// to the single seeded "cyber" group). Name must be non-empty and unique (PRIMARY KEY);
// prompt_mode defaults to "prepend" when blank. created_at/updated_at are stamped now.
func (s *Store) CreateGroup(ctx context.Context, g Group) error {
	if strings.TrimSpace(g.Name) == "" {
		return errors.New("group name required")
	}
	if strings.TrimSpace(g.PromptMode) == "" {
		g.PromptMode = "prepend"
	}
	now := Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO groups(name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(g.Name), g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.Virtual2MEnabled), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), g.ForceModel, g.ForceEffort, g.DefaultEgressID, now, now)
	return err
}

// DeleteGroup removes a group by name. The caller must guard against deleting the
// configured default group and against orphaning members (see CountAccountsByGroup).
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM groups WHERE name = ?`, name)
	return err
}

// SetAccountGroup reassigns an account to a different group. Membership is the
// accounts.group_name column the scheduler routes by, so this is the per-account
// "改派分组" control.
func (s *Store) SetAccountGroup(ctx context.Context, accountID, group string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET group_name = ?, updated_at = ? WHERE id = ?`, strings.TrimSpace(group), Now(), accountID)
	return err
}

// CountAccountsByGroup returns how many accounts (any status) belong to a group — used
// to guard group deletion and to show membership counts in the admin UI.
func (s *Store) CountAccountsByGroup(ctx context.Context, group string) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE group_name = ?`, group).Scan(&n)
	return n, err
}

func (s *Store) CountAccountsByGroups(ctx context.Context, groups []string) (map[string]GroupAccountCounts, error) {
	out := make(map[string]GroupAccountCounts, len(groups))
	names := make([]string, 0, len(groups))
	for _, name := range groups {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := out[name]; ok {
			continue
		}
		out[name] = GroupAccountCounts{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT group_name, COUNT(*), COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END),0)
FROM accounts
WHERE group_name IN (`+sqlPlaceholders(len(names))+`)
GROUP BY group_name`, stringArgs(names)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var counts GroupAccountCounts
		if err := rows.Scan(&name, &counts.AccountCount, &counts.ActiveAccountCount); err != nil {
			return nil, err
		}
		out[name] = counts
	}
	return out, rows.Err()
}

const apiKeyCols = `key_hash, COALESCE(label,''), COALESCE(key_type,'downstream'), COALESCE(group_name,''), COALESCE(force_model,''), COALESCE(force_effort,''), COALESCE(provider_hint,'auto'), enabled, expires_at, last_used_at, COALESCE(tenant_id,''), COALESCE(project_id,''), COALESCE(user_id,''), created_at, updated_at, COALESCE(secret,'')`

func scanAPIKey(scan func(...interface{}) error) (APIKey, error) {
	var k APIKey
	var enabled int
	err := scan(&k.KeyHash, &k.Label, &k.KeyType, &k.GroupName, &k.ForceModel, &k.ForceEffort, &k.ProviderHint, &enabled, &k.ExpiresAt, &k.LastUsedAt, &k.TenantID, &k.ProjectID, &k.UserID, &k.CreatedAt, &k.UpdatedAt, &k.Secret)
	k.Enabled = enabled != 0
	if strings.TrimSpace(k.KeyType) == "" {
		k.KeyType = "downstream"
	}
	k.ProviderHint = normalizeStoredProviderHint(k.ProviderHint)
	return k, err
}

func (s *Store) scanAPIKey(scan func(...interface{}) error) (APIKey, error) {
	k, err := scanAPIKey(scan)
	if err != nil {
		return k, err
	}
	k.Secret = s.openToken(k.Secret)
	return k, nil
}

// LookupAPIKey returns the api key with the given sha256 hash, if present.
func (s *Store) LookupAPIKey(ctx context.Context, keyHash string) (APIKey, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash = ?`, keyHash)
	k, err := s.scanAPIKey(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, false, nil
	}
	if err != nil {
		return APIKey{}, false, err
	}
	return k, true, nil
}

// MarkAPIKeyUsed records successful downstream authentication without rewriting the
// key's routing policy or recoverable secret. The monotonic predicate avoids moving
// last_used_at backwards when concurrent requests finish out of order.
func (s *Store) MarkAPIKeyUsed(ctx context.Context, keyHash string, usedAt int64) error {
	keyHash = strings.TrimSpace(keyHash)
	if keyHash == "" {
		return nil
	}
	if usedAt <= 0 {
		usedAt = Now()
	}
	minute := usedAt / 60
	if old, ok := s.apiKeyUsed.Load(keyHash); ok && old.(int64) >= minute {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE key_hash = ? AND last_used_at < ?`, usedAt, keyHash, usedAt)
	if err == nil {
		s.apiKeyUsed.Store(keyHash, minute)
	}
	return err
}

// ListAPIKeys returns all api keys (hash only, never plaintext), newest first.
func (s *Store) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys ORDER BY created_at DESC, key_hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := s.scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ListAPIKeysByUser returns the keys owned by one portal user (self-service view).
func (s *Store) ListAPIKeysByUser(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE user_id = ? ORDER BY created_at DESC, key_hash`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := s.scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpsertAPIKey inserts or updates an api key by hash.
func (s *Store) UpsertAPIKey(ctx context.Context, k APIKey) error {
	now := Now()
	if k.CreatedAt == 0 {
		k.CreatedAt = now
	}
	if strings.TrimSpace(k.KeyType) == "" {
		k.KeyType = "downstream"
	}
	k.ProviderHint = normalizeStoredProviderHint(k.ProviderHint)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(key_hash, tenant_id, project_id, user_id, key_type, label, group_name, force_model, force_effort, provider_hint, enabled, expires_at, last_used_at, secret, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(key_hash) DO UPDATE SET tenant_id=excluded.tenant_id, project_id=excluded.project_id, user_id=excluded.user_id, key_type=excluded.key_type, label=excluded.label, group_name=excluded.group_name, force_model=excluded.force_model, force_effort=excluded.force_effort, provider_hint=excluded.provider_hint, enabled=excluded.enabled, expires_at=excluded.expires_at, last_used_at=excluded.last_used_at, secret=excluded.secret, updated_at=excluded.updated_at`,
		k.KeyHash, k.TenantID, k.ProjectID, k.UserID, k.KeyType, k.Label, k.GroupName, k.ForceModel, k.ForceEffort, k.ProviderHint, boolInt(k.Enabled), k.ExpiresAt, k.LastUsedAt, s.sealToken(k.Secret), k.CreatedAt, now)
	return err
}

func normalizeStoredProviderHint(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "auto"
	}
	return v
}

// DeleteAPIKey removes an api key by hash.
func (s *Store) DeleteAPIKey(ctx context.Context, keyHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE key_hash = ?`, keyHash)
	return err
}

func (s *Store) ListTenants(ctx context.Context) ([]Tenant, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, name, created_at, updated_at FROM tenants ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var item Tenant
		if err := rows.Scan(&item.ID, &item.Name, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpsertTenant(ctx context.Context, item Tenant) error {
	now := Now()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO tenants(id, name, created_at, updated_at) VALUES(?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
		item.ID, item.Name, item.CreatedAt, item.UpdatedAt)
	return err
}

const userColumns = `id, tenant_id, email, COALESCE(name,''), COALESCE(role,'user'), COALESCE(status,'active'), COALESCE(password_hash,''), created_at, updated_at`

func scanUser(scan func(...interface{}) error) (User, error) {
	var u User
	err := scan(&u.ID, &u.TenantID, &u.Email, &u.Name, &u.Role, &u.Status, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		item, err := scanUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetUserByEmail looks up a user by case-insensitive email; ok=false when absent.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ? COLLATE NOCASE`, strings.TrimSpace(email))
	u, err := scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	return u, err == nil, err
}

// GetUser looks up a user by id; ok=false when absent.
func (s *Store) GetUser(ctx context.Context, id string) (User, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	u, err := scanUser(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	return u, err == nil, err
}

// CountUsers returns the number of registered users (used to bootstrap the first
// user as an admin).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUserWithBootstrap atomically assigns the first registered user the admin
// role. The role decision, registration gate, duplicate-email check, and insert are
// one SQLite write statement, so concurrent first registrations cannot both observe
// an empty users table and become administrators.
func (s *Store) CreateUserWithBootstrap(ctx context.Context, item User, allowRegistration bool) (User, error) {
	now := Now()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if item.Status == "" {
		item.Status = "active"
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO users(id, tenant_id, email, name, role, status, password_hash, created_at, updated_at)
SELECT ?, ?, ?, ?,
       CASE WHEN NOT EXISTS(SELECT 1 FROM users) THEN 'admin' ELSE 'user' END,
       ?, ?, ?, ?
WHERE (? OR NOT EXISTS(SELECT 1 FROM users))
  AND NOT EXISTS(SELECT 1 FROM users WHERE email = ? COLLATE NOCASE)`,
		item.ID, item.TenantID, strings.TrimSpace(item.Email), item.Name, item.Status, item.PasswordHash, item.CreatedAt, item.UpdatedAt,
		boolInt(allowRegistration), strings.TrimSpace(item.Email))
	if err != nil {
		return User{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return User{}, err
	}
	if affected == 0 {
		if _, exists, lookupErr := s.GetUserByEmail(ctx, item.Email); lookupErr != nil {
			return User{}, lookupErr
		} else if exists {
			return User{}, ErrUserEmailExists
		}
		return User{}, ErrRegistrationClosed
	}
	created, ok, err := s.GetUser(ctx, item.ID)
	if err != nil {
		return User{}, err
	}
	if !ok {
		return User{}, errors.New("created user could not be read back")
	}
	return created, nil
}

// HasAdminUser reports whether at least one active admin user exists. Once true, an
// open deployment (no admin_token) stops allowing anonymous /admin access — the
// portal has been bootstrapped and an admin must log in.
func (s *Store) HasAdminUser(ctx context.Context) (bool, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&n)
	return n > 0, err
}

func (s *Store) UpsertUser(ctx context.Context, item User) error {
	now := Now()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	if item.Role == "" {
		item.Role = "user"
	}
	if item.Status == "" {
		item.Status = "active"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, tenant_id, email, name, role, status, password_hash, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET tenant_id = excluded.tenant_id, email = excluded.email, name = excluded.name, role = excluded.role, status = excluded.status, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		item.ID, item.TenantID, item.Email, item.Name, item.Role, item.Status, item.PasswordHash, item.CreatedAt, item.UpdatedAt)
	return err
}

// ── End-user sessions ──

// CreateUserSession persists a session row (token already hashed by the caller).
func (s *Store) CreateUserSession(ctx context.Context, sess UserSession) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_sessions(token_hash, user_id, user_agent, created_at, expires_at) VALUES(?, ?, ?, ?, ?) ON CONFLICT(token_hash) DO UPDATE SET expires_at = excluded.expires_at`,
		sess.TokenHash, sess.UserID, sess.UserAgent, sess.CreatedAt, sess.ExpiresAt)
	return err
}

// GetUserSession returns a non-expired session by token hash; ok=false otherwise.
func (s *Store) GetUserSession(ctx context.Context, tokenHash string) (UserSession, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT token_hash, user_id, COALESCE(user_agent,''), created_at, expires_at FROM user_sessions WHERE token_hash = ?`, tokenHash)
	var sess UserSession
	err := row.Scan(&sess.TokenHash, &sess.UserID, &sess.UserAgent, &sess.CreatedAt, &sess.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UserSession{}, false, nil
	}
	if err != nil {
		return UserSession{}, false, err
	}
	if sess.ExpiresAt > 0 && sess.ExpiresAt < Now() {
		_ = s.DeleteUserSession(ctx, tokenHash)
		return UserSession{}, false, nil
	}
	return sess, true, nil
}

// DeleteUserSession removes one session (logout).
func (s *Store) DeleteUserSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// DeleteUserSessionsForUser removes all sessions for a user (e.g. on password
// change or admin disable).
func (s *Store) DeleteUserSessionsForUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE user_id = ?`, userID)
	return err
}

// DeleteUser removes a user along with their sessions and the downstream api keys
// they own (admin user management). Other users' keys are untouched.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM account_kiro_credentials WHERE account_id = ?`,
		`DELETE FROM user_sessions WHERE user_id = ?`,
		`DELETE FROM api_keys WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, tenant_id, name, group_name, created_at, updated_at FROM projects ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var item Project
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Name, &item.GroupName, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpsertProject(ctx context.Context, item Project) error {
	now := Now()
	if item.GroupName == "" {
		item.GroupName = "cyber"
	}
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO projects(id, tenant_id, name, group_name, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET tenant_id = excluded.tenant_id, name = excluded.name, group_name = excluded.group_name, updated_at = excluded.updated_at`,
		item.ID, item.TenantID, item.Name, item.GroupName, item.CreatedAt, item.UpdatedAt)
	return err
}

func (s *Store) UpsertAccount(ctx context.Context, account Account, token AccountToken) error {
	now := Now()
	if account.CreatedAt == 0 {
		account.CreatedAt = now
	}
	account.UpdatedAt = now
	if account.Status == "" {
		account.Status = "active"
	}
	if account.GroupName == "" {
		account.GroupName = "cyber"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO accounts(id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 label = excluded.label,
 group_name = excluded.group_name,
 upstream_account_id = excluded.upstream_account_id,
 chatgpt_user_id = excluded.chatgpt_user_id,
 email = excluded.email,
 plan_type = excluded.plan_type,
 provider = excluded.provider,
 status = excluded.status,
 is_fedramp = excluded.is_fedramp,
 updated_at = excluded.updated_at`,
		account.ID, account.Label, account.GroupName, account.UpstreamAccountID, account.ChatGPTUserID, account.Email, account.PlanType, account.Provider, account.Status, boolInt(account.IsFedramp), account.QuarantineUntil, account.QuarantineReason, account.CreatedAt, account.UpdatedAt)
	if err != nil {
		return err
	}
	token.AccountID = account.ID
	if token.CreatedAt == 0 {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `
INSERT INTO account_auth_tokens(account_id, access_token, refresh_token, openai_api_key, id_token_raw, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
 access_token = excluded.access_token,
 refresh_token = excluded.refresh_token,
 openai_api_key = excluded.openai_api_key,
 id_token_raw = excluded.id_token_raw,
 last_refresh = excluded.last_refresh,
 expires_at = excluded.expires_at,
 scopes = excluded.scopes,
 oauth_rate_limit_tier = excluded.oauth_rate_limit_tier,
 updated_at = excluded.updated_at`,
		token.AccountID, s.sealToken(token.AccessToken), s.sealToken(token.RefreshToken), s.sealToken(token.OpenAIAPIKey), s.sealToken(token.IDTokenRaw), token.LastRefresh, token.ExpiresAt, token.Scopes, token.OAuthRateLimitTier, token.CreatedAt, token.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, created_at, updated_at)
VALUES(?, ?, '', ?, 0, ?, ?)
ON CONFLICT(account_id) DO NOTHING`, account.ID, DefaultDirectEgressID, account.ID+":"+DefaultDirectEgressID, now, now)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tokenCache.Delete(account.ID)
	return nil
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

// AccountPoolSummary returns the lightweight counts used by the dashboard. It avoids
// the full /admin/accounts list path, which expands every account with capabilities,
// egress bindings, and provider fallback lookups on each refresh.
func (s *Store) AccountPoolSummary(ctx context.Context, now int64) (AccountPoolSummary, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT
	a.provider,
	a.status,
	a.quarantine_until,
	COALESCE(b.cooldown_until, 0),
	COALESCE(b.recheck_pending, 0),
	COALESCE(t.access_token, ''),
	COALESCE(t.openai_api_key, '')
FROM accounts a
LEFT JOIN account_egress_bindings b ON b.account_id = a.id
LEFT JOIN account_auth_tokens t ON t.account_id = a.id
`)
	if err != nil {
		return AccountPoolSummary{}, err
	}
	defer rows.Close()

	var summary AccountPoolSummary
	for rows.Next() {
		var provider, status, accessToken, apiKey string
		var quarantineUntil, cooldownUntil int64
		var recheck int
		if err := rows.Scan(&provider, &status, &quarantineUntil, &cooldownUntil, &recheck, &accessToken, &apiKey); err != nil {
			return AccountPoolSummary{}, err
		}

		summary.Total++
		if status == "active" && quarantineUntil <= now {
			summary.Active++
		}
		if quarantineUntil > now {
			summary.Quarantined++
		}
		if cooldownUntil > now {
			summary.Cooling++
		}
		if recheck != 0 {
			summary.Recheck++
		}
		switch accountProviderSummary(provider, s.openToken(accessToken), s.openToken(apiKey)) {
		case "codex":
			summary.Codex++
		case "claude":
			summary.Claude++
		case "kiro":
			summary.Kiro++
		default:
			summary.Other++
		}
	}
	return summary, rows.Err()
}

func accountProviderSummary(provider, accessToken, apiKey string) string {
	if provider = strings.TrimSpace(provider); provider != "" {
		return provider
	}
	if strings.HasPrefix(accessToken, "sk-ant") || strings.HasPrefix(apiKey, "sk-ant") {
		return "claude"
	}
	return "codex"
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ListAccountsPage returns a paginated, searchable account list with total count.
// When search is empty, no LIKE filter is applied (the fast path for the default
// "all accounts" view). Status filters are applied when non-empty.
func (s *Store) ListAccountsPage(ctx context.Context, limit, offset int, search, status string) ([]Account, int, error) {
	return s.listAccountsPage(ctx, limit, offset, search, status, "created_at, id DESC")
}

func (s *Store) ListAccountsPageDesc(ctx context.Context, limit, offset int, search, status string) ([]Account, int, error) {
	return s.listAccountsPage(ctx, limit, offset, search, status, "created_at DESC, id DESC")
}

func (s *Store) listAccountsPage(ctx context.Context, limit, offset int, search, status, orderBy string) ([]Account, int, error) {
	where := ""
	args := []interface{}{}
	if search != "" {
		where = " WHERE (label LIKE ? OR email LIKE ? OR group_name LIKE ? OR id LIKE ?)"
		s := "%" + search + "%"
		args = append(args, s, s, s, s)
	}
	if status != "" {
		if where == "" {
			where = " WHERE status = ?"
		} else {
			where += " AND status = ?"
		}
		args = append(args, status)
	}
	var total int
	countQuery := "SELECT COUNT(*) FROM accounts" + where
	if err := s.rdb.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := "SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts" + where + " ORDER BY " + orderBy + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, total, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, total, err
		}
		out = append(out, acc)
	}
	return out, total, rows.Err()
}

func (s *Store) AccountLabelsByID(ctx context.Context, accountIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		out[id] = id
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, email FROM accounts WHERE id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, label, email string
		if err := rows.Scan(&id, &label, &email); err != nil {
			return nil, err
		}
		out[id] = firstNonEmptyString(label, email, id)
	}
	return out, rows.Err()
}

func (s *Store) ResolveAccountProviders(ctx context.Context, accounts []Account) (map[string]string, error) {
	out := make(map[string]string, len(accounts))
	legacyIDs := make([]string, 0)
	for _, account := range accounts {
		if p := strings.TrimSpace(account.Provider); p != "" {
			out[account.ID] = p
			continue
		}
		legacyIDs = append(legacyIDs, account.ID)
	}
	if len(legacyIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, access_token, openai_api_key FROM account_auth_tokens WHERE account_id IN (`+sqlPlaceholders(len(legacyIDs))+`)`, stringArgs(legacyIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, accessToken, apiKey string
		if err := rows.Scan(&accountID, &accessToken, &apiKey); err != nil {
			return nil, err
		}
		out[accountID] = accountProviderSummary("", s.openToken(accessToken), s.openToken(apiKey))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, accountID := range legacyIDs {
		if _, ok := out[accountID]; !ok {
			out[accountID] = "codex"
		}
	}
	return out, nil
}

func (s *Store) ListActiveAccountsByGroup(ctx context.Context, group string) ([]Account, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE group_name = ? AND status = 'active' ORDER BY created_at, id`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		acc, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, acc)
	}
	return out, rows.Err()
}

// AccountWithEgress bundles the account, its egress binding, and primary egress profile
// in a single row so the scheduler's selectFresh candidate loop can iterate in memory
// without N+1 queries per selection (was: GetEgressBinding + selectEgress per account).
type AccountWithEgress struct {
	Account Account
	Binding AccountEgressBinding
	Egress  EgressProfile // zero-value when primary egress deleted
}

// ListActiveAccountsWithEgress returns all active accounts in a group together with
// their bindings and primary egress profiles in a single query. Used by the scheduler's
// optimized selectFresh path to collapse N+1 DB round-trips into one.
func (s *Store) ListActiveAccountsWithEgress(ctx context.Context, group string) ([]AccountWithEgress, error) {
	rows, err := s.rdb.QueryContext(ctx, `
		SELECT a.id, a.label, a.group_name, a.upstream_account_id, a.chatgpt_user_id,
		       a.email, a.plan_type, a.provider, a.status, a.is_fedramp, a.quarantine_until,
		       a.quarantine_reason, a.created_at, a.updated_at,
		       b.primary_egress_id, b.standby_egress_ids, b.cookie_jar_key, b.cooldown_until,
		       b.recheck_pending, b.created_at, b.updated_at,
		       COALESCE(e.id,''), COALESCE(e.name,''), COALESCE(e.type,''), COALESCE(e.endpoint,''),
		       COALESCE(e.chain_proxy,''), COALESCE(e.region,''), COALESCE(e.exit_ip,''),
		       e.stream_capable, COALESCE(e.health,'healthy'), e.latency_millis, e.cf_score,
		       COALESCE(e.last_cf_ray,''), e.cooldown_until, e.max_concurrency,
		       e.created_at, e.updated_at,
		       COALESCE(e.proxy_auth_mode,''), COALESCE(e.proxy_api_key,''),
		       COALESCE(e.ip_mode,''), COALESCE(e.provider_key,''), COALESCE(e.dynamic_config_json,'{}')
		FROM accounts a
		JOIN account_egress_bindings b ON a.id = b.account_id
		LEFT JOIN egress_profiles e ON b.primary_egress_id = e.id
		WHERE a.group_name = ? AND a.status = 'active'
		ORDER BY a.created_at, a.id
	`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountWithEgress
	for rows.Next() {
		var a AccountWithEgress
		var isFedramp int
		var recheck int
		var streamCapable int
		err := rows.Scan(
			&a.Account.ID, &a.Account.Label, &a.Account.GroupName, &a.Account.UpstreamAccountID,
			&a.Account.ChatGPTUserID, &a.Account.Email, &a.Account.PlanType, &a.Account.Provider,
			&a.Account.Status, &isFedramp, &a.Account.QuarantineUntil, &a.Account.QuarantineReason,
			&a.Account.CreatedAt, &a.Account.UpdatedAt,
			&a.Binding.PrimaryEgressID, &a.Binding.StandbyEgressIDs, &a.Binding.CookieJarKey,
			&a.Binding.CooldownUntil, &recheck, &a.Binding.CreatedAt, &a.Binding.UpdatedAt,
			&a.Egress.ID, &a.Egress.Name, &a.Egress.Type, &a.Egress.Endpoint,
			&a.Egress.ChainProxy, &a.Egress.Region, &a.Egress.ExitIP,
			&streamCapable, &a.Egress.Health, &a.Egress.LatencyMillis, &a.Egress.CFScore,
			&a.Egress.LastCFRay, &a.Egress.CooldownUntil, &a.Egress.MaxConcurrency,
			&a.Egress.CreatedAt, &a.Egress.UpdatedAt,
			&a.Egress.ProxyAuthMode, &a.Egress.ProxyAPIKey,
			&a.Egress.IPMode, &a.Egress.ProviderKey, &a.Egress.DynamicConfigJSON,
		)
		if err != nil {
			return nil, err
		}
		a.Account.IsFedramp = isFedramp != 0
		a.Binding.RecheckPending = recheck != 0
		a.Egress.StreamCapable = streamCapable != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

func (s *Store) ListAccountsByIDs(ctx context.Context, accountIDs []string) (map[string]Account, error) {
	out := make(map[string]Account, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out[account.ID] = account
	}
	return out, rows.Err()
}

func (s *Store) SetAccountStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`, status, Now(), id)
	return err
}

func (s *Store) SetAccountPlanType(ctx context.Context, id, planType string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET plan_type = ?, updated_at = ? WHERE id = ?`, strings.TrimSpace(planType), Now(), id)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DELETE FROM account_lifecycle_status WHERE account_id = ?`,
		`DELETE FROM account_codex_reauth_jobs WHERE account_id = ?`,
		`DELETE FROM account_codex_reauth_config WHERE account_id = ?`,
		`DELETE FROM account_rate_limits WHERE account_id = ?`,
		`DELETE FROM account_model_capabilities WHERE account_id = ?`,
		`DELETE FROM affinity_bindings WHERE account_id = ?`,
		`DELETE FROM account_egress_bindings WHERE account_id = ?`,
		`DELETE FROM account_session_cookies WHERE account_id = ?`,
		`DELETE FROM account_injected_cookies WHERE account_id = ?`,
		`DELETE FROM codex_reset_credit_consumptions WHERE account_id = ?`,
		`DELETE FROM accounts WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	err = tx.Commit()
	if err == nil {
		s.tokenCache.Delete(id)
		s.kiroCache.Delete(id)
		s.rateLimitGen.Add(1)
		s.affinityGen.Add(1)
	}
	return err
}

func (s *Store) SetAccountQuarantine(ctx context.Context, id string, until int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET quarantine_until = ?, quarantine_reason = ?, updated_at = ? WHERE id = ?`, until, reason, Now(), id)
	return err
}

func (s *Store) GetToken(ctx context.Context, accountID string) (AccountToken, error) {
	if cached, ok := s.tokenCache.Load(accountID); ok {
		return cached.(AccountToken), nil
	}
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, access_token, refresh_token, openai_api_key, id_token_raw, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at FROM account_auth_tokens WHERE account_id = ?`, accountID)
	var t AccountToken
	err := row.Scan(&t.AccountID, &t.AccessToken, &t.RefreshToken, &t.OpenAIAPIKey, &t.IDTokenRaw, &t.LastRefresh, &t.ExpiresAt, &t.Scopes, &t.OAuthRateLimitTier, &t.CreatedAt, &t.UpdatedAt)
	t.AccessToken = s.openToken(t.AccessToken)
	t.RefreshToken = s.openToken(t.RefreshToken)
	t.OpenAIAPIKey = s.openToken(t.OpenAIAPIKey)
	t.IDTokenRaw = s.openToken(t.IDTokenRaw)
	if err == nil {
		s.tokenCache.Store(accountID, t)
	}
	return t, err
}

func (s *Store) ListTokensByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountToken, error) {
	out := make(map[string]AccountToken, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, access_token, refresh_token, openai_api_key, id_token_raw, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at FROM account_auth_tokens WHERE account_id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t AccountToken
		if err := rows.Scan(&t.AccountID, &t.AccessToken, &t.RefreshToken, &t.OpenAIAPIKey, &t.IDTokenRaw, &t.LastRefresh, &t.ExpiresAt, &t.Scopes, &t.OAuthRateLimitTier, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.AccessToken = s.openToken(t.AccessToken)
		t.RefreshToken = s.openToken(t.RefreshToken)
		t.OpenAIAPIKey = s.openToken(t.OpenAIAPIKey)
		t.IDTokenRaw = s.openToken(t.IDTokenRaw)
		out[t.AccountID] = t
	}
	return out, rows.Err()
}

func (s *Store) UpdateToken(ctx context.Context, t AccountToken) error {
	s.tokenCache.Delete(t.AccountID)
	_, err := s.db.ExecContext(ctx, `UPDATE account_auth_tokens SET access_token = ?, refresh_token = ?, openai_api_key = ?, id_token_raw = ?, last_refresh = ?, expires_at = ?, scopes = ?, oauth_rate_limit_tier = ?, updated_at = ? WHERE account_id = ?`,
		s.sealToken(t.AccessToken), s.sealToken(t.RefreshToken), s.sealToken(t.OpenAIAPIKey), s.sealToken(t.IDTokenRaw), t.LastRefresh, t.ExpiresAt, t.Scopes, t.OAuthRateLimitTier, Now(), t.AccountID)
	return err
}

func (s *Store) UpsertKiroCredentials(ctx context.Context, c KiroCredentials) error {
	if strings.TrimSpace(c.AccountID) == "" {
		return errors.New("kiro account id required")
	}
	now := Now()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	s.kiroCache.Delete(c.AccountID)
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_kiro_credentials(account_id, auth_method, client_id, client_secret, profile_arn, auth_region, api_region, machine_id, kiro_api_key, endpoint, credential_hash, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET auth_method=excluded.auth_method, client_id=excluded.client_id, client_secret=excluded.client_secret, profile_arn=excluded.profile_arn, auth_region=excluded.auth_region, api_region=excluded.api_region, machine_id=excluded.machine_id, kiro_api_key=excluded.kiro_api_key, endpoint=excluded.endpoint, credential_hash=excluded.credential_hash, updated_at=excluded.updated_at`,
		c.AccountID, c.AuthMethod, c.ClientID, s.sealToken(c.ClientSecret), c.ProfileARN, c.AuthRegion, c.APIRegion, c.MachineID, s.sealToken(c.KiroAPIKey), c.Endpoint, c.CredentialHash, c.CreatedAt, c.UpdatedAt)
	return err
}

func (s *Store) GetKiroCredentials(ctx context.Context, accountID string) (KiroCredentials, error) {
	if cached, ok := s.kiroCache.Load(accountID); ok {
		return cached.(KiroCredentials), nil
	}
	var c KiroCredentials
	err := s.rdb.QueryRowContext(ctx, `SELECT account_id, auth_method, client_id, client_secret, profile_arn, auth_region, api_region, machine_id, kiro_api_key, endpoint, credential_hash, created_at, updated_at FROM account_kiro_credentials WHERE account_id=?`, accountID).
		Scan(&c.AccountID, &c.AuthMethod, &c.ClientID, &c.ClientSecret, &c.ProfileARN, &c.AuthRegion, &c.APIRegion, &c.MachineID, &c.KiroAPIKey, &c.Endpoint, &c.CredentialHash, &c.CreatedAt, &c.UpdatedAt)
	c.ClientSecret = s.openToken(c.ClientSecret)
	c.KiroAPIKey = s.openToken(c.KiroAPIKey)
	if err == nil {
		s.kiroCache.Store(accountID, c)
	}
	return c, err
}

func (s *Store) KiroCredentialHashExists(ctx context.Context, hash string) (bool, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_kiro_credentials WHERE credential_hash=?`, hash).Scan(&n)
	return n > 0, err
}

// KiroAuthSummariesByAccountIDs batch-loads Kiro auth summaries in a single query,
// replacing the per-account KiroAuthSummary N+1 in the account list. The summary
// only needs presence booleans, so the encrypted secret/api-key columns are tested
// for non-emptiness on the SEALED value (sealed is non-empty iff plaintext is) —
// no secretbox.Open per row.
func (s *Store) KiroAuthSummariesByAccountIDs(ctx context.Context, accountIDs []string) (map[string]KiroAuthSummary, error) {
	out := make(map[string]KiroAuthSummary, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, auth_method, auth_region, api_region, endpoint, client_id, client_secret, profile_arn, machine_id, kiro_api_key FROM account_kiro_credentials WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`)`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, authMethod, authRegion, apiRegion, endpoint, clientID, clientSecret, profileARN, machineID, apiKey string
		if err := rows.Scan(&accountID, &authMethod, &authRegion, &apiRegion, &endpoint, &clientID, &clientSecret, &profileARN, &machineID, &apiKey); err != nil {
			return nil, err
		}
		out[accountID] = KiroAuthSummary{AuthMethod: authMethod, AuthRegion: authRegion, APIRegion: apiRegion, Endpoint: publicKiroEndpoint(endpoint),
			HasClientID: clientID != "", HasClientSecret: clientSecret != "", HasProfileARN: profileARN != "", HasMachineID: machineID != "", HasAPIKey: apiKey != ""}
	}
	return out, rows.Err()
}

func (s *Store) KiroAuthSummary(ctx context.Context, accountID string) (KiroAuthSummary, error) {
	c, err := s.GetKiroCredentials(ctx, accountID)
	if err != nil {
		return KiroAuthSummary{}, err
	}
	return KiroAuthSummary{AuthMethod: c.AuthMethod, AuthRegion: c.AuthRegion, APIRegion: c.APIRegion, Endpoint: publicKiroEndpoint(c.Endpoint),
		HasClientID: c.ClientID != "", HasClientSecret: c.ClientSecret != "", HasProfileARN: c.ProfileARN != "", HasMachineID: c.MachineID != "", HasAPIKey: c.KiroAPIKey != ""}, nil
}

func publicKiroEndpoint(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// SetTokenEncryptionKey enables transparent at-rest encryption of secret columns using
// a 32-byte key derived from the deployment secret (identity.ResolveSecret in main).
// Call once after Open/Init and before serving. An empty secret leaves encryption off.
func (s *Store) SetTokenEncryptionKey(secret []byte) {
	if len(secret) == 0 {
		return
	}
	s.tokenKey = secretbox.DeriveKey(secret)
}

// sealToken encrypts a secret for storage (no-op when encryption is disabled, and
// best-effort: a Seal failure falls back to storing the value rather than losing it).
func (s *Store) sealToken(v string) string {
	out, err := secretbox.Seal(s.tokenKey, v)
	if err != nil {
		return v
	}
	return out
}

// openToken decrypts a stored secret. Legacy plaintext passes through unchanged. A
// decrypt failure (e.g. the operator rotated identity_secret) returns the raw stored
// value rather than panicking — the account simply fails auth and can be re-imported.
func (s *Store) openToken(v string) string {
	out, err := secretbox.Open(s.tokenKey, v)
	if err != nil {
		return v
	}
	return out
}

// EncryptExistingTokens re-encrypts any plaintext rows in secret-bearing tables so an
// existing pool is protected at rest, not just newly-written rows. Idempotent (skips
// already-sealed values) and a no-op when encryption is disabled. Returns the number of
// rows upgraded. Run once at startup after SetTokenEncryptionKey.
func (s *Store) EncryptExistingTokens(ctx context.Context) (int, error) {
	if len(s.tokenKey) == 0 {
		return 0, nil
	}
	n := 0
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, access_token, refresh_token, openai_api_key, id_token_raw FROM account_auth_tokens`)
	if err != nil {
		return 0, err
	}
	type rec struct{ id, at, rt, ak, it string }
	var pending []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.at, &r.rt, &r.ak, &r.it); err != nil {
			rows.Close()
			return 0, err
		}
		// Only rows with at least one non-empty, not-yet-sealed secret need an upgrade.
		if anyPlaintextSecret(r.at, r.rt, r.ak, r.it) {
			pending = append(pending, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range pending {
		if _, err := s.db.ExecContext(ctx, `UPDATE account_auth_tokens SET access_token = ?, refresh_token = ?, openai_api_key = ?, id_token_raw = ? WHERE account_id = ?`,
			s.sealToken(r.at), s.sealToken(r.rt), s.sealToken(r.ak), s.sealToken(r.it), r.id); err != nil {
			return n, err
		}
		n++
	}
	keyRows, err := s.rdb.QueryContext(ctx, `SELECT key_hash, secret FROM api_keys WHERE secret <> ''`)
	if err != nil {
		return n, err
	}
	type keyRec struct{ hash, secret string }
	var keyPending []keyRec
	for keyRows.Next() {
		var r keyRec
		if err := keyRows.Scan(&r.hash, &r.secret); err != nil {
			keyRows.Close()
			return n, err
		}
		if anyPlaintextSecret(r.secret) {
			keyPending = append(keyPending, r)
		}
	}
	keyRows.Close()
	if err := keyRows.Err(); err != nil {
		return n, err
	}
	for _, r := range keyPending {
		if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET secret = ?, updated_at = ? WHERE key_hash = ?`, s.sealToken(r.secret), Now(), r.hash); err != nil {
			return n, err
		}
		n++
	}
	kiroRows, err := s.rdb.QueryContext(ctx, `SELECT account_id, client_secret, kiro_api_key FROM account_kiro_credentials`)
	if err != nil {
		return n, err
	}
	type kiroRec struct{ id, secret, apiKey string }
	var kiroPending []kiroRec
	for kiroRows.Next() {
		var r kiroRec
		if err := kiroRows.Scan(&r.id, &r.secret, &r.apiKey); err != nil {
			kiroRows.Close()
			return n, err
		}
		if anyPlaintextSecret(r.secret, r.apiKey) {
			kiroPending = append(kiroPending, r)
		}
	}
	kiroRows.Close()
	if err := kiroRows.Err(); err != nil {
		return n, err
	}
	for _, r := range kiroPending {
		if _, err := s.db.ExecContext(ctx, `UPDATE account_kiro_credentials SET client_secret=?, kiro_api_key=? WHERE account_id=?`, s.sealToken(r.secret), s.sealToken(r.apiKey), r.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// anyPlaintextSecret reports whether any field is a non-empty value that is not already
// encrypted (so EncryptExistingTokens only touches rows that actually need upgrading).
func anyPlaintextSecret(vals ...string) bool {
	for _, v := range vals {
		if v != "" && !secretbox.IsSealed(v) {
			return true
		}
	}
	return false
}

// SetSessionCookie stores the chatgpt.com session cookie used to (re)mint a
// ChatGPT access token for cookie-imported "AT" accounts.
func (s *Store) SetSessionCookie(ctx context.Context, accountID, cookie string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_session_cookies(account_id, cookie, updated_at) VALUES(?, ?, ?) ON CONFLICT(account_id) DO UPDATE SET cookie = excluded.cookie, updated_at = excluded.updated_at`,
		accountID, s.sealToken(cookie), Now())
	return err
}

// GetSessionCookie returns the stored session cookie for an account, or "" if none.
func (s *Store) GetSessionCookie(ctx context.Context, accountID string) (string, error) {
	var cookie string
	err := s.rdb.QueryRowContext(ctx, `SELECT cookie FROM account_session_cookies WHERE account_id = ?`, accountID).Scan(&cookie)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return s.openToken(cookie), err
}

// UpsertInjectedCookie persists (or replaces) a browser-repair/FlareSolverr cookie
// set for an account+egress+host so it can be re-seeded after a restart and reused by
// the escalation ladder.
func (s *Store) UpsertInjectedCookie(ctx context.Context, c InjectedCookie) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_injected_cookies(account_id, egress_id, upstream_host, cookie_header, user_agent, exit_ip, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, egress_id, upstream_host) DO UPDATE SET
 cookie_header = excluded.cookie_header,
 user_agent = excluded.user_agent,
 exit_ip = excluded.exit_ip,
 updated_at = excluded.updated_at`,
		c.AccountID, c.EgressID, c.UpstreamHost, c.CookieHeader, c.UserAgent, c.ExitIP, Now())
	return err
}

// ListInjectedCookies returns all persisted injected cookies (for startup re-seeding).
func (s *Store) ListInjectedCookies(ctx context.Context) ([]InjectedCookie, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, egress_id, upstream_host, cookie_header, user_agent, exit_ip, updated_at FROM account_injected_cookies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InjectedCookie
	for rows.Next() {
		var c InjectedCookie
		if err := rows.Scan(&c.AccountID, &c.EgressID, &c.UpstreamHost, &c.CookieHeader, &c.UserAgent, &c.ExitIP, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) UpsertCapabilities(ctx context.Context, capabilities []ModelCapability) error {
	if len(capabilities) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A probe yields the full current capability set for an account, so replace that
	// account's rows wholesale: delete first, then insert. Plain upsert-per-slug would
	// leave behind models that disappeared between probes — e.g. a now-filtered hidden
	// preset (codex-auto-review) or a model the account lost access to — which would
	// keep being advertised forever. Scoped to the account IDs actually present in the
	// input (an empty input early-returns above, so this never wipes on a transient).
	deleted := map[string]bool{}
	for _, c := range capabilities {
		if deleted[c.AccountID] {
			continue
		}
		deleted[c.AccountID] = true
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_model_capabilities WHERE account_id = ?`, c.AccountID); err != nil {
			return err
		}
	}
	for _, c := range capabilities {
		if c.LastProbeAt == 0 {
			c.LastProbeAt = Now()
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO account_model_capabilities(account_id, model_slug, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, model_slug) DO UPDATE SET
 native_context_window = excluded.native_context_window,
 native_max_context_window = excluded.native_max_context_window,
 effective_context_window_percent = excluded.effective_context_window_percent,
 auto_compact_token_limit = excluded.auto_compact_token_limit,
 visibility = excluded.visibility,
 etag = excluded.etag,
 raw_model_json_hash = excluded.raw_model_json_hash,
 raw_model_json = excluded.raw_model_json,
 source = excluded.source,
 last_probe_at = excluded.last_probe_at`,
			c.AccountID, c.ModelSlug, c.NativeContextWindow, c.NativeMaxContextWindow, c.EffectiveContextWindowPercent, c.AutoCompactTokenLimit, c.Visibility, c.ETag, c.RawModelJSONHash, c.RawModelJSON, c.Source, c.LastProbeAt)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListCapabilities(ctx context.Context, accountID string) ([]ModelCapability, error) {
	query := `SELECT account_id, model_slug, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at FROM account_model_capabilities`
	args := []interface{}{}
	if accountID != "" {
		query += ` WHERE account_id = ?`
		args = append(args, accountID)
	}
	query += ` ORDER BY account_id, model_slug`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCapability
	for rows.Next() {
		var c ModelCapability
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.RawModelJSON, &c.Source, &c.LastProbeAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ListCapabilitiesByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]ModelCapability, error) {
	out := make(map[string][]ModelCapability, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, model_slug, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at FROM account_model_capabilities WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`) ORDER BY account_id, model_slug`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ModelCapability
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.RawModelJSON, &c.Source, &c.LastProbeAt); err != nil {
			return nil, err
		}
		out[c.AccountID] = append(out[c.AccountID], c)
	}
	return out, rows.Err()
}

// ListCapabilitiesSummaryByAccountIDs is ListCapabilitiesByAccountIDs WITHOUT the
// heavy raw_model_json blob (RawModelJSON left empty). The account LIST view never
// renders the raw model-catalog JSON, so omitting it at the SQL level avoids
// reading/scanning/marshaling potentially several MB per page load (50 accounts ×
// N probed models). Use the full variant only where the raw JSON is actually shown.
func (s *Store) ListCapabilitiesSummaryByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]ModelCapability, error) {
	out := make(map[string][]ModelCapability, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, model_slug, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, source, last_probe_at FROM account_model_capabilities WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`) ORDER BY account_id, model_slug`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ModelCapability
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.Source, &c.LastProbeAt); err != nil {
			return nil, err
		}
		out[c.AccountID] = append(out[c.AccountID], c)
	}
	return out, rows.Err()
}

func (s *Store) BestNativeWindow(ctx context.Context, accountID, model string) (int64, error) {
	var value int64
	err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(native_max_context_window), 0) FROM account_model_capabilities WHERE account_id = ? AND (? = '' OR model_slug = ?)`, accountID, model, model).Scan(&value)
	return value, err
}

// AccountsWithModel returns the set of active accounts in a group whose probed
// capabilities include the given model. Used for capability-aware routing: when
// the set is non-empty the scheduler restricts selection to it (so a request for
// a model is sent to an account that actually has it); when empty (nothing probed
// for that model) the caller falls back to all accounts rather than over-restrict.
func (s *Store) AccountsWithModel(ctx context.Context, group, model string) (map[string]bool, error) {
	if model == "" {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT c.account_id FROM account_model_capabilities c JOIN accounts a ON a.id = c.account_id WHERE a.group_name = ? AND a.status = 'active' AND c.model_slug = ?`, group, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (s *Store) EnsureKiroRuntimeModels(ctx context.Context, accountID, endpointHash string, models []string) error {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(endpointHash) == "" {
		return errors.New("kiro runtime capability account and endpoint are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO kiro_runtime_capabilities(
account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state, updated_at)
VALUES(?, ?, ?, 'unknown', 'unknown', 'unknown', 'unknown', ?)
ON CONFLICT(account_id, endpoint_hash, model) DO UPDATE SET updated_at=excluded.updated_at`, accountID, endpointHash, model, Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetKiroRuntimeCapability(ctx context.Context, accountID, endpointHash, model string) (KiroRuntimeCapability, error) {
	var capability KiroRuntimeCapability
	err := s.rdb.QueryRowContext(ctx, `SELECT account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state,
cache_reuse_state, cache_reuse_evidence, cache_reuse_credit_reduction_percent, cache_reuse_probed_at,
observations, metering_events, cache_reported_observations, cache_hit_observations, consecutive_unreported,
unknown_cache_schema_json, updated_at
FROM kiro_runtime_capabilities WHERE account_id=? AND endpoint_hash=? AND model=?`, accountID, endpointHash, model).Scan(
		&capability.AccountID, &capability.EndpointHash, &capability.Model, &capability.ModelState, &capability.ThinkingState, &capability.CacheCapability, &capability.CachePointState,
		&capability.CacheReuseState, &capability.CacheReuseEvidence, &capability.CacheReuseReductionPct, &capability.CacheReuseProbedAt,
		&capability.Observations, &capability.MeteringEvents, &capability.CacheReportedObservations, &capability.CacheHitObservations,
		&capability.ConsecutiveUnreported, &capability.UnknownCacheSchemaJSON, &capability.UpdatedAt)
	return capability, err
}

func (s *Store) ListKiroRuntimeCapabilities(ctx context.Context, accountID string) ([]KiroRuntimeCapability, error) {
	query := `SELECT account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state,
cache_reuse_state, cache_reuse_evidence, cache_reuse_credit_reduction_percent, cache_reuse_probed_at,
observations, metering_events, cache_reported_observations, cache_hit_observations, consecutive_unreported,
unknown_cache_schema_json, updated_at FROM kiro_runtime_capabilities`
	args := []any{}
	if strings.TrimSpace(accountID) != "" {
		query += ` WHERE account_id=?`
		args = append(args, accountID)
	}
	query += ` ORDER BY account_id, endpoint_hash, model`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KiroRuntimeCapability
	for rows.Next() {
		var capability KiroRuntimeCapability
		if err := rows.Scan(&capability.AccountID, &capability.EndpointHash, &capability.Model, &capability.ModelState, &capability.ThinkingState, &capability.CacheCapability, &capability.CachePointState,
			&capability.CacheReuseState, &capability.CacheReuseEvidence, &capability.CacheReuseReductionPct, &capability.CacheReuseProbedAt,
			&capability.Observations, &capability.MeteringEvents, &capability.CacheReportedObservations, &capability.CacheHitObservations,
			&capability.ConsecutiveUnreported, &capability.UnknownCacheSchemaJSON, &capability.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, capability)
	}
	return out, rows.Err()
}

func (s *Store) VerifiedKiroModels(ctx context.Context, accountID, endpointHash string, thinkingRequired bool) ([]string, error) {
	query := `SELECT model FROM kiro_runtime_capabilities WHERE account_id=? AND endpoint_hash=? AND model_state='verified'`
	args := []any{accountID, endpointHash}
	if thinkingRequired {
		query += ` AND thinking_state='verified'`
	}
	query += ` ORDER BY model`
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		out = append(out, model)
	}
	return out, rows.Err()
}

func (s *Store) ObserveKiroCapability(ctx context.Context, accountID, endpointHash, model string, observation KiroCapabilityObservation) (KiroRuntimeCapability, error) {
	if observation.UnreportedThreshold <= 0 {
		observation.UnreportedThreshold = 20
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return KiroRuntimeCapability{}, err
	}
	defer tx.Rollback()
	capability := KiroRuntimeCapability{
		AccountID: accountID, EndpointHash: endpointHash, Model: model,
		ModelState: "unknown", ThinkingState: "unknown", CacheCapability: "unknown", CachePointState: "unknown", CacheReuseState: "unknown",
	}
	err = tx.QueryRowContext(ctx, `SELECT model_state, thinking_state, cache_capability, cache_point_state,
cache_reuse_state, cache_reuse_evidence, cache_reuse_credit_reduction_percent, cache_reuse_probed_at, observations, metering_events,
cache_reported_observations, cache_hit_observations, consecutive_unreported, unknown_cache_schema_json, updated_at
FROM kiro_runtime_capabilities WHERE account_id=? AND endpoint_hash=? AND model=?`, accountID, endpointHash, model).Scan(
		&capability.ModelState, &capability.ThinkingState, &capability.CacheCapability, &capability.CachePointState,
		&capability.CacheReuseState, &capability.CacheReuseEvidence, &capability.CacheReuseReductionPct, &capability.CacheReuseProbedAt,
		&capability.Observations, &capability.MeteringEvents,
		&capability.CacheReportedObservations, &capability.CacheHitObservations, &capability.ConsecutiveUnreported,
		&capability.UnknownCacheSchemaJSON, &capability.UpdatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return KiroRuntimeCapability{}, err
	}
	if observation.ModelSucceeded {
		capability.ModelState = "verified"
		capability.Observations++
		if observation.ThinkingRequested {
			capability.ThinkingState = "verified"
		}
	}
	if observation.MeteringEvents > 0 {
		capability.MeteringEvents += int64(observation.MeteringEvents)
	}
	cacheReported := observation.CacheReadPresent || observation.CacheCreationPresent
	switch {
	case observation.CacheReadPresent && observation.CacheReadTokens > 0:
		capability.CacheCapability = "hit_observed"
		capability.CacheHitObservations++
		capability.CacheReportedObservations++
		capability.ConsecutiveUnreported = 0
	case cacheReported:
		if capability.CacheCapability != "hit_observed" {
			capability.CacheCapability = "reported"
		}
		capability.CacheReportedObservations++
		capability.ConsecutiveUnreported = 0
	case observation.ExplicitlyUnsupported:
		if capability.CacheCapability != "hit_observed" {
			capability.CacheCapability = "explicitly_unsupported"
		}
		capability.ConsecutiveUnreported = 0
	case observation.ModelSucceeded:
		// Count successful responses, not the number of event envelopes in a
		// response. A provider may emit multiple metering events for one request.
		capability.ConsecutiveUnreported++
		if capability.ConsecutiveUnreported >= int64(observation.UnreportedThreshold) && capability.CacheCapability == "unknown" {
			capability.CacheCapability = "unreported"
		}
	}
	_ = observation.CacheCreationTokens // presence is capability-significant; the usage row stores the value.
	if strings.TrimSpace(observation.UnknownCacheSchemaJSON) != "" {
		capability.UnknownCacheSchemaJSON = observation.UnknownCacheSchemaJSON
	}
	capability.UpdatedAt = Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO kiro_runtime_capabilities(
account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state,
cache_reuse_state, cache_reuse_evidence, cache_reuse_credit_reduction_percent, cache_reuse_probed_at, observations, metering_events,
cache_reported_observations, cache_hit_observations, consecutive_unreported, unknown_cache_schema_json, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, endpoint_hash, model) DO UPDATE SET
model_state=excluded.model_state, thinking_state=excluded.thinking_state, cache_capability=excluded.cache_capability, cache_point_state=excluded.cache_point_state,
cache_reuse_state=excluded.cache_reuse_state, cache_reuse_evidence=excluded.cache_reuse_evidence,
cache_reuse_credit_reduction_percent=excluded.cache_reuse_credit_reduction_percent, cache_reuse_probed_at=excluded.cache_reuse_probed_at,
observations=excluded.observations, metering_events=excluded.metering_events,
cache_reported_observations=excluded.cache_reported_observations, cache_hit_observations=excluded.cache_hit_observations,
consecutive_unreported=excluded.consecutive_unreported, unknown_cache_schema_json=excluded.unknown_cache_schema_json,
updated_at=excluded.updated_at`, capability.AccountID, capability.EndpointHash, capability.Model, capability.ModelState,
		capability.ThinkingState, capability.CacheCapability, capability.CachePointState,
		capability.CacheReuseState, capability.CacheReuseEvidence, capability.CacheReuseReductionPct, capability.CacheReuseProbedAt,
		capability.Observations, capability.MeteringEvents,
		capability.CacheReportedObservations, capability.CacheHitObservations, capability.ConsecutiveUnreported,
		capability.UnknownCacheSchemaJSON, capability.UpdatedAt)
	if err != nil {
		return KiroRuntimeCapability{}, err
	}
	if err := tx.Commit(); err != nil {
		return KiroRuntimeCapability{}, err
	}
	return capability, nil
}

// SetKiroCachePointState records request-side cachePoint protocol support. This is
// deliberately independent from CacheCapability, which describes whether token
// cache buckets were reported in successful responses.
func (s *Store) SetKiroCachePointState(ctx context.Context, accountID, endpointHash, model, state string) error {
	state = strings.ToLower(strings.TrimSpace(state))
	if state != "unknown" && state != "verified" && state != "unsupported" {
		return fmt.Errorf("invalid Kiro cache point state %q", state)
	}
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(endpointHash) == "" || strings.TrimSpace(model) == "" {
		return errors.New("kiro cache point state account, endpoint, and model are required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO kiro_runtime_capabilities(
account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state, updated_at)
VALUES(?, ?, ?, 'unknown', 'unknown', 'unknown', ?, ?)
ON CONFLICT(account_id, endpoint_hash, model) DO UPDATE SET cache_point_state=excluded.cache_point_state, updated_at=excluded.updated_at`,
		accountID, endpointHash, model, state, Now())
	return err
}

// SetKiroCacheReuseProbe records the outcome of the explicit two-request paid
// cache probe. It is deliberately separate from CachePointState (the endpoint
// accepted the request syntax) and CacheCapability (the endpoint reported token
// cache buckets). A verified result is monotonic: one later noisy or inconclusive
// probe must not erase earlier positive write/read or credits-reduction evidence.
func (s *Store) SetKiroCacheReuseProbe(ctx context.Context, accountID, endpointHash, model, state, evidence string, reductionPercent float64, probedAt int64) error {
	state = strings.ToLower(strings.TrimSpace(state))
	evidence = strings.ToLower(strings.TrimSpace(evidence))
	if state != "unknown" && state != "verified" && state != "not_observed" {
		return fmt.Errorf("invalid Kiro cache reuse state %q", state)
	}
	if evidence == "" {
		evidence = "none"
	}
	if evidence != "none" && evidence != "token_metadata" && evidence != "credits_reduction" {
		return fmt.Errorf("invalid Kiro cache reuse evidence %q", evidence)
	}
	if state == "verified" && evidence == "none" {
		return errors.New("verified Kiro cache reuse requires evidence")
	}
	if state != "verified" && evidence != "none" {
		return errors.New("non-verified Kiro cache reuse cannot carry positive evidence")
	}
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(endpointHash) == "" || strings.TrimSpace(model) == "" {
		return errors.New("kiro cache reuse probe account, endpoint, and model are required")
	}
	if probedAt <= 0 {
		probedAt = Now()
	}
	updatedAt := Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO kiro_runtime_capabilities(
account_id, endpoint_hash, model, model_state, thinking_state, cache_capability, cache_point_state,
cache_reuse_state, cache_reuse_evidence, cache_reuse_credit_reduction_percent, cache_reuse_probed_at, updated_at)
VALUES(?, ?, ?, 'unknown', 'unknown', 'unknown', 'unknown', ?, ?, ?, ?, ?)
ON CONFLICT(account_id, endpoint_hash, model) DO UPDATE SET
cache_reuse_state=CASE
  WHEN kiro_runtime_capabilities.cache_reuse_state='verified' AND excluded.cache_reuse_state<>'verified'
  THEN kiro_runtime_capabilities.cache_reuse_state ELSE excluded.cache_reuse_state END,
cache_reuse_evidence=CASE
  WHEN kiro_runtime_capabilities.cache_reuse_state='verified' AND excluded.cache_reuse_state<>'verified'
  THEN kiro_runtime_capabilities.cache_reuse_evidence ELSE excluded.cache_reuse_evidence END,
cache_reuse_credit_reduction_percent=CASE
  WHEN kiro_runtime_capabilities.cache_reuse_state='verified' AND excluded.cache_reuse_state<>'verified'
  THEN kiro_runtime_capabilities.cache_reuse_credit_reduction_percent ELSE excluded.cache_reuse_credit_reduction_percent END,
cache_reuse_probed_at=CASE
  WHEN kiro_runtime_capabilities.cache_reuse_state='verified' AND excluded.cache_reuse_state<>'verified'
  THEN kiro_runtime_capabilities.cache_reuse_probed_at ELSE excluded.cache_reuse_probed_at END,
updated_at=excluded.updated_at`, accountID, endpointHash, model, state, evidence, reductionPercent, probedAt, updatedAt)
	return err
}

func (s *Store) UpsertEgressProfile(ctx context.Context, p EgressProfile) error {
	now := Now()
	if p.ID == "" {
		return errors.New("egress id required")
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.Type == "" {
		p.Type = "direct"
	}
	if p.Health == "" {
		p.Health = "healthy"
	}
	if p.MaxConcurrency <= 0 {
		p.MaxConcurrency = 16
	}
	if strings.TrimSpace(p.DynamicConfigJSON) == "" {
		p.DynamicConfigJSON = "{}"
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO egress_profiles(id, name, type, endpoint, chain_proxy, region, exit_ip, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at, proxy_auth_mode, proxy_api_key, ip_mode, provider_key, dynamic_config_json)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name = excluded.name,
 type = excluded.type,
 endpoint = excluded.endpoint,
 chain_proxy = excluded.chain_proxy,
 region = excluded.region,
 exit_ip = excluded.exit_ip,
 stream_capable = excluded.stream_capable,
 health = excluded.health,
 latency_millis = excluded.latency_millis,
 cf_score = excluded.cf_score,
 last_cf_ray = excluded.last_cf_ray,
 cooldown_until = excluded.cooldown_until,
 max_concurrency = excluded.max_concurrency,
 updated_at = excluded.updated_at,
 proxy_auth_mode = excluded.proxy_auth_mode,
 proxy_api_key = excluded.proxy_api_key,
 ip_mode = excluded.ip_mode,
 provider_key = excluded.provider_key,
 dynamic_config_json = excluded.dynamic_config_json`,
		p.ID, p.Name, p.Type, p.Endpoint, p.ChainProxy, p.Region, p.ExitIP, boolInt(p.StreamCapable), p.Health, p.LatencyMillis, p.CFScore, p.LastCFRay, p.CooldownUntil, p.MaxConcurrency, p.CreatedAt, p.UpdatedAt, p.ProxyAuthMode, p.ProxyAPIKey, p.IPMode, p.ProviderKey, p.DynamicConfigJSON)
	return err
}

func (s *Store) GetEgressProfile(ctx context.Context, id string) (EgressProfile, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, name, type, endpoint, chain_proxy, region, exit_ip, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at, proxy_auth_mode, proxy_api_key, ip_mode, provider_key, dynamic_config_json FROM egress_profiles WHERE id = ?`, id)
	return scanEgress(row)
}

func (s *Store) ListEgressProfiles(ctx context.Context) ([]EgressProfile, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, name, type, endpoint, chain_proxy, region, exit_ip, stream_capable, health, latency_millis, cf_score, last_cf_ray, cooldown_until, max_concurrency, created_at, updated_at, proxy_auth_mode, proxy_api_key, ip_mode, provider_key, dynamic_config_json FROM egress_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EgressProfile
	for rows.Next() {
		p, err := scanEgress(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func normalizeEgressPoolStrategy(strategy string) string {
	if strings.TrimSpace(strategy) == "" {
		return "sticky_least_used"
	}
	return strings.TrimSpace(strategy)
}

func (s *Store) UpsertEgressPool(ctx context.Context, p EgressPool) error {
	now := Now()
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return errors.New("egress pool id required")
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.ID
	}
	p.Purpose = strings.TrimSpace(p.Purpose)
	p.AssignmentStrategy = normalizeEgressPoolStrategy(p.AssignmentStrategy)
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO egress_pools(id, name, purpose, assignment_strategy, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name = excluded.name,
 purpose = excluded.purpose,
 assignment_strategy = excluded.assignment_strategy,
 updated_at = excluded.updated_at`,
		p.ID, p.Name, p.Purpose, p.AssignmentStrategy, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) GetEgressPool(ctx context.Context, id string) (EgressPool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, name, purpose, assignment_strategy, created_at, updated_at FROM egress_pools WHERE id = ?`, strings.TrimSpace(id))
	var p EgressPool
	if err := row.Scan(&p.ID, &p.Name, &p.Purpose, &p.AssignmentStrategy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	members, err := s.ListEgressPoolMembers(ctx, p.ID)
	if err != nil {
		return p, err
	}
	p.Members = members
	return p, nil
}

func (s *Store) ListEgressPools(ctx context.Context) ([]EgressPool, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, name, purpose, assignment_strategy, created_at, updated_at FROM egress_pools ORDER BY purpose, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EgressPool
	for rows.Next() {
		var p EgressPool
		if err := rows.Scan(&p.ID, &p.Name, &p.Purpose, &p.AssignmentStrategy, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		members, err := s.ListEgressPoolMembers(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Members = members
	}
	return out, nil
}

func (s *Store) UpsertEgressPoolMember(ctx context.Context, m EgressPoolMember) error {
	now := Now()
	m.PoolID = strings.TrimSpace(m.PoolID)
	m.EgressID = strings.TrimSpace(m.EgressID)
	if m.PoolID == "" || m.EgressID == "" {
		return errors.New("pool_id and egress_id are required")
	}
	if m.Capacity < 0 {
		m.Capacity = 0
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO egress_pool_members(pool_id, egress_id, enabled, capacity, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(pool_id, egress_id) DO UPDATE SET
 enabled = excluded.enabled,
 capacity = excluded.capacity,
 updated_at = excluded.updated_at`,
		m.PoolID, m.EgressID, boolInt(m.Enabled), m.Capacity, m.CreatedAt, m.UpdatedAt)
	return err
}

func (s *Store) ListEgressPoolMembers(ctx context.Context, poolID string) ([]EgressPoolMember, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT m.pool_id, m.egress_id, m.enabled, m.capacity, m.created_at, m.updated_at,
       e.id, e.name, e.type, e.endpoint, e.chain_proxy, e.region, e.exit_ip,
       e.stream_capable, e.health, e.latency_millis, e.cf_score, e.last_cf_ray,
       e.cooldown_until, e.max_concurrency, e.created_at, e.updated_at,
       e.proxy_auth_mode, e.proxy_api_key, e.ip_mode, e.provider_key, e.dynamic_config_json
FROM egress_pool_members m
JOIN egress_profiles e ON e.id = m.egress_id
WHERE m.pool_id = ?
ORDER BY m.egress_id`, strings.TrimSpace(poolID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EgressPoolMember
	for rows.Next() {
		var m EgressPoolMember
		var enabled int
		var stream int
		if err := rows.Scan(
			&m.PoolID, &m.EgressID, &enabled, &m.Capacity, &m.CreatedAt, &m.UpdatedAt,
			&m.Egress.ID, &m.Egress.Name, &m.Egress.Type, &m.Egress.Endpoint,
			&m.Egress.ChainProxy, &m.Egress.Region, &m.Egress.ExitIP, &stream,
			&m.Egress.Health, &m.Egress.LatencyMillis, &m.Egress.CFScore, &m.Egress.LastCFRay,
			&m.Egress.CooldownUntil, &m.Egress.MaxConcurrency, &m.Egress.CreatedAt, &m.Egress.UpdatedAt,
			&m.Egress.ProxyAuthMode, &m.Egress.ProxyAPIKey, &m.Egress.IPMode, &m.Egress.ProviderKey, &m.Egress.DynamicConfigJSON,
		); err != nil {
			return nil, err
		}
		m.Enabled = enabled != 0
		m.Egress.StreamCapable = stream != 0
		if strings.TrimSpace(m.Egress.DynamicConfigJSON) == "" {
			m.Egress.DynamicConfigJSON = "{}"
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpsertGroupEgressPolicy(ctx context.Context, p GroupEgressPolicy) error {
	now := Now()
	p.GroupName = strings.TrimSpace(p.GroupName)
	if p.GroupName == "" {
		return errors.New("group_name required")
	}
	p.RegistrationPoolID = strings.TrimSpace(p.RegistrationPoolID)
	p.RuntimePoolID = strings.TrimSpace(p.RuntimePoolID)
	p.AssignmentStrategy = normalizeEgressPoolStrategy(p.AssignmentStrategy)
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO group_egress_policies(group_name, registration_pool_id, runtime_pool_id, assignment_strategy, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(group_name) DO UPDATE SET
 registration_pool_id = excluded.registration_pool_id,
 runtime_pool_id = excluded.runtime_pool_id,
 assignment_strategy = excluded.assignment_strategy,
 updated_at = excluded.updated_at`,
		p.GroupName, p.RegistrationPoolID, p.RuntimePoolID, p.AssignmentStrategy, p.CreatedAt, p.UpdatedAt)
	return err
}

func (s *Store) GetGroupEgressPolicy(ctx context.Context, groupName string) (GroupEgressPolicy, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT group_name, registration_pool_id, runtime_pool_id, assignment_strategy, created_at, updated_at FROM group_egress_policies WHERE group_name = ?`, strings.TrimSpace(groupName))
	var p GroupEgressPolicy
	if err := row.Scan(&p.GroupName, &p.RegistrationPoolID, &p.RuntimePoolID, &p.AssignmentStrategy, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	return p, nil
}

func egressPoolMemberHealthy(m EgressPoolMember, now int64) bool {
	if !m.Enabled {
		return false
	}
	health := strings.ToLower(strings.TrimSpace(m.Egress.Health))
	if health != "" && health != "healthy" {
		return false
	}
	if m.Egress.CooldownUntil > now {
		return false
	}
	return true
}

func (s *Store) primaryBindingCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT primary_egress_id, COUNT(*) FROM account_egress_bindings GROUP BY primary_egress_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (s *Store) selectEgressPoolMember(ctx context.Context, poolID string) (EgressPoolMember, error) {
	members, err := s.ListEgressPoolMembers(ctx, poolID)
	if err != nil {
		return EgressPoolMember{}, err
	}
	if len(members) == 0 {
		return EgressPoolMember{}, fmt.Errorf("egress pool %q has no members", poolID)
	}
	counts, err := s.primaryBindingCounts(ctx)
	if err != nil {
		return EgressPoolMember{}, err
	}
	now := Now()
	var candidates []EgressPoolMember
	for _, m := range members {
		if !egressPoolMemberHealthy(m, now) {
			continue
		}
		count := counts[m.EgressID]
		capacity := m.Capacity
		if capacity <= 0 {
			capacity = m.Egress.MaxConcurrency
		}
		if capacity > 0 && count >= capacity {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return EgressPoolMember{}, fmt.Errorf("egress pool %q has no healthy members with available capacity", poolID)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := counts[candidates[i].EgressID], counts[candidates[j].EgressID]
		if ci != cj {
			return ci < cj
		}
		capi, capj := candidates[i].Capacity, candidates[j].Capacity
		if capi <= 0 {
			capi = candidates[i].Egress.MaxConcurrency
		}
		if capj <= 0 {
			capj = candidates[j].Egress.MaxConcurrency
		}
		if capi != capj {
			return capi > capj
		}
		return candidates[i].EgressID < candidates[j].EgressID
	})
	return candidates[0], nil
}

func (s *Store) SelectEgressFromPool(ctx context.Context, poolID string) (EgressProfile, error) {
	member, err := s.selectEgressPoolMember(ctx, strings.TrimSpace(poolID))
	if err != nil {
		return EgressProfile{}, err
	}
	return member.Egress, nil
}

func (s *Store) AssignAccountToEgressPool(ctx context.Context, accountID, poolID string) (AccountEgressBinding, error) {
	accountID = strings.TrimSpace(accountID)
	poolID = strings.TrimSpace(poolID)
	if accountID == "" {
		return AccountEgressBinding{}, errors.New("account_id required")
	}
	if poolID == "" {
		return AccountEgressBinding{}, errors.New("egress pool id required")
	}
	members, err := s.ListEgressPoolMembers(ctx, poolID)
	if err != nil {
		return AccountEgressBinding{}, err
	}
	memberIDs := map[string]bool{}
	for _, m := range members {
		if egressPoolMemberHealthy(m, Now()) {
			memberIDs[m.EgressID] = true
		}
	}
	if binding, err := s.GetEgressBinding(ctx, accountID); err == nil && memberIDs[binding.PrimaryEgressID] {
		return binding, nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return AccountEgressBinding{}, err
	}
	chosen, err := s.selectEgressPoolMember(ctx, poolID)
	if err != nil {
		return AccountEgressBinding{}, err
	}
	binding := AccountEgressBinding{
		AccountID:       accountID,
		PrimaryEgressID: chosen.EgressID,
		CookieJarKey:    accountID + ":" + chosen.EgressID,
	}
	if err := s.UpsertEgressBinding(ctx, binding); err != nil {
		return AccountEgressBinding{}, err
	}
	return s.GetEgressBinding(ctx, accountID)
}

func (s *Store) SetEgressCooldown(ctx context.Context, id string, until int64, ray string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE egress_profiles SET cooldown_until = ?, health = CASE WHEN ? > strftime('%s','now') THEN 'cooldown' ELSE health END, last_cf_ray = ?, cf_score = cf_score + 1, updated_at = ? WHERE id = ?`,
		until, until, ray, Now(), id)
	return err
}

func (s *Store) SetEgressHealth(ctx context.Context, id, health string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE egress_profiles SET health = ?, updated_at = ? WHERE id = ?`, health, Now(), id)
	return err
}

func (s *Store) GetEgressBinding(ctx context.Context, accountID string) (AccountEgressBinding, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE account_id = ?`, accountID)
	var b AccountEgressBinding
	var recheck int
	err := row.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt)
	b.RecheckPending = recheck != 0
	return b, err
}

func (s *Store) ListEgressBindingsByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountEgressBinding, error) {
	out := make(map[string]AccountEgressBinding, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`)`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.RecheckPending = recheck != 0
		out[b.AccountID] = b
	}
	return out, rows.Err()
}

func (s *Store) UpsertEgressBinding(ctx context.Context, b AccountEgressBinding) error {
	now := Now()
	if b.CookieJarKey == "" {
		b.CookieJarKey = b.AccountID + ":" + b.PrimaryEgressID
	}
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
 primary_egress_id = excluded.primary_egress_id,
 standby_egress_ids = excluded.standby_egress_ids,
 cookie_jar_key = excluded.cookie_jar_key,
 cooldown_until = excluded.cooldown_until,
 recheck_pending = excluded.recheck_pending,
 updated_at = excluded.updated_at`,
		b.AccountID, b.PrimaryEgressID, b.StandbyEgressIDs, b.CookieJarKey, b.CooldownUntil, boolInt(b.RecheckPending), b.CreatedAt, b.UpdatedAt)
	return err
}

func (s *Store) SetBindingCooldown(ctx context.Context, accountID string, until int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_egress_bindings SET cooldown_until = ?, updated_at = ? WHERE account_id = ?`, until, Now(), accountID)
	return err
}

// BenchBindingForRecheck cools an account's binding AND marks it recheck-pending in
// one write, so a single upstream error both rotates the pool off the account and
// keeps it out of the candidate set until the background recheck loop verifies it is
// healthy again. This is the seamless-failover counterpart to a plain cooldown:
// SetBindingCooldown alone would let the account silently re-enter rotation the
// instant its cooldown elapsed, with no liveness proof.
func (s *Store) BenchBindingForRecheck(ctx context.Context, accountID string, until int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_egress_bindings SET cooldown_until = ?, recheck_pending = 1, updated_at = ? WHERE account_id = ?`, until, Now(), accountID)
	return err
}

// SetBindingRecheckPending toggles the recheck-pending flag without touching the
// cooldown. Used to drop the flag for an account that can no longer be probed
// (deleted/quarantined) so the recheck loop stops revisiting it.
func (s *Store) SetBindingRecheckPending(ctx context.Context, accountID string, pending bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_egress_bindings SET recheck_pending = ?, updated_at = ? WHERE account_id = ?`, boolInt(pending), Now(), accountID)
	return err
}

// ClearBindingRecheck marks an account healthy again: clears both the cooldown and
// the recheck-pending flag so it immediately rejoins the candidate pool. Called by
// the recheck loop after a successful liveness probe.
func (s *Store) ClearBindingRecheck(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_egress_bindings SET cooldown_until = 0, recheck_pending = 0, updated_at = ? WHERE account_id = ?`, Now(), accountID)
	return err
}

// ListBindingsNeedingRecheck returns the bindings whose cooldown has elapsed but
// which are still recheck-pending — i.e. the accounts the recheck loop should now
// probe. Gating on cooldown_until <= now means the initial cooldown window (often a
// server-signaled Retry-After) is always honored before the first probe.
func (s *Store) ListBindingsNeedingRecheck(ctx context.Context, now int64) ([]AccountEgressBinding, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE recheck_pending = 1 AND cooldown_until <= ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountEgressBinding
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.RecheckPending = recheck != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddStandbyEgress appends egressID to an account's standby_egress_ids list if it
// is not already present (primary or standby), so a CF-hit account gains a fallback
// egress (e.g. a WARP exit) the scheduler's selectEgress will reroute to when the
// primary binding is cooled. It is idempotent. Returns (added, err): added is false
// when the egress was already the primary or already in the standby list.
func (s *Store) AddStandbyEgress(ctx context.Context, accountID, egressID string) (bool, error) {
	egressID = strings.TrimSpace(egressID)
	if egressID == "" {
		return false, errors.New("egress id required")
	}
	binding, err := s.GetEgressBinding(ctx, accountID)
	if err != nil {
		return false, err
	}
	if binding.PrimaryEgressID == egressID {
		return false, nil
	}
	for _, id := range binding.StandbyIDs() {
		if id == egressID {
			return false, nil
		}
	}
	ids := append(binding.StandbyIDs(), egressID)
	binding.StandbyEgressIDs = strings.Join(ids, ",")
	if err := s.UpsertEgressBinding(ctx, binding); err != nil {
		return false, err
	}
	return true, nil
}

// ListEgressBindings returns every account→egress binding. Used by the WARP manager
// to compute per-exit membership (how many accounts a given WARP exit serves) so it
// can pack accounts ≤N per exit. Bounded by the account count and only read on CF
// events / assignment, never the hot path.
func (s *Store) ListEgressBindings(ctx context.Context) ([]AccountEgressBinding, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountEgressBinding
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.RecheckPending = recheck != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetAffinityBinding(ctx context.Context, routeKeyHash string) (AffinityBinding, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at FROM affinity_bindings WHERE route_key_hash = ?`, routeKeyHash)
	var b AffinityBinding
	err := row.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Provider, &b.Model, &b.EgressID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

func (s *Store) UpsertAffinityBinding(ctx context.Context, b AffinityBinding) error {
	now := Now()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO affinity_bindings(route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(route_key_hash) DO UPDATE SET
 account_id = excluded.account_id,
 source = excluded.source,
 route_key = excluded.route_key,
 provider = excluded.provider,
 model = excluded.model,
 egress_id = excluded.egress_id,
 epoch = affinity_bindings.epoch + 1,
 updated_at = excluded.updated_at`,
		b.RouteKeyHash, b.RouteKey, b.Source, b.AccountID, b.Provider, b.Model, b.EgressID, b.Epoch, b.CreatedAt, b.UpdatedAt)
	if err == nil {
		s.affinityGen.Add(1)
	}
	return err
}

func (s *Store) AffinityGeneration() uint64 { return s.affinityGen.Load() }

// ListAffinityBindingsByAccount returns the conversations currently pinned to an
// account (the per-account session map), most-recently-bound first. The epoch is
// the rebind/handoff counter for that conversation — a high epoch indicates churn
// (repeated failover), useful for the isolation view.
func (s *Store) ListAffinityBindingsByAccount(ctx context.Context, accountID string, limit int) ([]AffinityBinding, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at FROM affinity_bindings WHERE account_id = ? ORDER BY updated_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AffinityBinding
	for rows.Next() {
		var b AffinityBinding
		if err := rows.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Provider, &b.Model, &b.EgressID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CountAffinityBindingsByAccount returns how many conversations are pinned to an
// account (for the dashboard / account view summary).
func (s *Store) CountAffinityBindingsByAccount(ctx context.Context, accountID string) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM affinity_bindings WHERE account_id = ?`, accountID).Scan(&n)
	return n, err
}

// GetSetting returns a runtime setting override and whether it was present. These
// store admin-UI-toggleable flags (e.g. conversation_isolation) so they can be
// changed without editing the config file or restarting; absence means "use the
// config default".
// settingsCacheTTL bounds how long the process-level settings snapshot is served
// before a reload — short enough that a missed invalidation self-heals quickly,
// long enough to amortize the ~15-20 settings reads a single request makes.
const settingsCacheTTL = time.Second

// settingsMap returns the whole settings table as a shared, immutable map, served
// from the process-level snapshot when fresh and reloaded in ONE query otherwise.
// The returned map must not be mutated by callers.
func (s *Store) settingsMap(ctx context.Context) (map[string]string, error) {
	s.settingsMu.RLock()
	if s.settingsSnapshot != nil && time.Since(s.settingsLoadedAt) < settingsCacheTTL {
		m := s.settingsSnapshot
		s.settingsMu.RUnlock()
		return m, nil
	}
	s.settingsMu.RUnlock()

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	// Double-check: another goroutine may have reloaded while we waited for the lock.
	if s.settingsSnapshot != nil && time.Since(s.settingsLoadedAt) < settingsCacheTTL {
		return s.settingsSnapshot, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.settingsSnapshot = m
	s.settingsLoadedAt = time.Now()
	return m, nil
}

// InvalidateSettingsCache forces the next GetSetting to reload from the DB. Called
// after any settings write (SetSettings and the few other paths that touch the
// settings table) so an operator/runtime change is visible immediately; the short
// TTL in settingsMap is only a backstop for any write path that forgets to call it.
func (s *Store) InvalidateSettingsCache() {
	s.settingsMu.Lock()
	s.settingsSnapshot = nil
	s.settingsMu.Unlock()
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool, error) {
	m, err := s.settingsMap(ctx)
	if err != nil {
		return "", false, err
	}
	v, ok := m[key]
	return v, ok, nil
}

// SetSetting stores a runtime setting override.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.SetSettings(ctx, map[string]string{key: value})
}

// SetSettings stores a batch of runtime setting overrides atomically. Settings-center
// saves often touch related knobs together; one transaction prevents half-applied UI
// changes if SQLite rejects any row in the batch.
func (s *Store) SetSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, values[key], now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Make the write visible immediately (the TTL is only a backstop).
	s.InvalidateSettingsCache()
	return nil
}

const moderationSettingKey = "moderation_config"

// GetModerationConfig returns the response/history moderation config (stored as a
// JSON blob in the settings table). A missing/blank value yields a disabled default.
func (s *Store) GetModerationConfig(ctx context.Context) (ModerationConfig, error) {
	cfg := ModerationConfig{AutoTranslate: true}
	v, ok, err := s.GetSetting(ctx, moderationSettingKey)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return cfg, err
	}
	_ = json.Unmarshal([]byte(v), &cfg)
	cfg.Words = decodeProviderModelsFromSlice(cfg.Words) // trim + de-dup, reuse helper
	return cfg, nil
}

// SetModerationConfig persists the moderation config as JSON.
func (s *Store) SetModerationConfig(ctx context.Context, cfg ModerationConfig) error {
	cfg.Words = decodeProviderModelsFromSlice(cfg.Words)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, moderationSettingKey, string(raw))
}

// ── Custom OpenAI-compatible providers ──

func scanCustomProvider(scan func(...interface{}) error) (CustomProvider, error) {
	var p CustomProvider
	var enabled, auto int
	var modelsJSON string
	if err := scan(&p.ID, &p.Name, &p.BaseURL, &p.UpstreamProtocol, &enabled, &auto, &modelsJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return CustomProvider{}, err
	}
	if proto, ok := NormalizeCustomProviderProtocol(p.UpstreamProtocol); ok {
		p.UpstreamProtocol = proto
	} else {
		p.UpstreamProtocol = CustomProviderProtocolChatCompletions
	}
	p.Enabled = enabled != 0
	p.AutoDiscoverModels = auto != 0
	p.Models = decodeProviderModels(modelsJSON)
	return p, nil
}

// decodeProviderModels parses the stored models_json (a JSON array of slugs) into a
// clean, de-duplicated slice. Tolerates empty/legacy values, returning nil.
func decodeProviderModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []string
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(arr))
	for _, m := range arr {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

const customProviderCols = `id, name, base_url, upstream_protocol, enabled, auto_discover_models, models_json, created_at, updated_at`

func (s *Store) ListCustomProviders(ctx context.Context) ([]CustomProvider, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+customProviderCols+` FROM custom_providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomProvider
	for rows.Next() {
		p, err := scanCustomProvider(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetCustomProvider returns a provider by id; the bool is false when not present.
func (s *Store) GetCustomProvider(ctx context.Context, id string) (CustomProvider, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+customProviderCols+` FROM custom_providers WHERE id = ?`, id)
	p, err := scanCustomProvider(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return CustomProvider{}, false, nil
	}
	if err != nil {
		return CustomProvider{}, false, err
	}
	return p, true, nil
}

// UpsertCustomProvider inserts or updates a provider by id. The Models slice is
// stored as a JSON array; the admin UI edits it via input boxes, never raw JSON.
func (s *Store) UpsertCustomProvider(ctx context.Context, p CustomProvider) error {
	if strings.TrimSpace(p.ID) == "" {
		return errors.New("provider id required")
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if proto, ok := NormalizeCustomProviderProtocol(p.UpstreamProtocol); ok {
		p.UpstreamProtocol = proto
	} else {
		p.UpstreamProtocol = CustomProviderProtocolChatCompletions
	}
	now := Now()
	if p.CreatedAt == 0 {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	modelsJSON := "[]"
	if cleaned := decodeProviderModelsFromSlice(p.Models); len(cleaned) > 0 {
		if raw, err := json.Marshal(cleaned); err == nil {
			modelsJSON = string(raw)
		}
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO custom_providers(id, name, base_url, upstream_protocol, enabled, auto_discover_models, models_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name = excluded.name,
 base_url = excluded.base_url,
 upstream_protocol = excluded.upstream_protocol,
 enabled = excluded.enabled,
 auto_discover_models = excluded.auto_discover_models,
 models_json = excluded.models_json,
 updated_at = excluded.updated_at`,
		p.ID, p.Name, p.BaseURL, p.UpstreamProtocol, boolInt(p.Enabled), boolInt(p.AutoDiscoverModels), modelsJSON, p.CreatedAt, p.UpdatedAt)
	return err
}

// decodeProviderModelsFromSlice trims, de-duplicates, and drops empties from an
// incoming model slice (the same normalization decodeProviderModels applies on read).
func decodeProviderModelsFromSlice(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func (s *Store) DeleteCustomProvider(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM custom_providers WHERE id = ?`, id)
	return err
}

// ── Upstream error rules ──

const upstreamErrorRuleCols = `id, name, enabled, priority, providers_json, entrypoints_json, model_patterns_json, status_codes_json, body_keywords_json, match_mode, account_action, downstream_action, response_status, custom_message, cooldown_seconds, prefer_retry_after, idle_seconds, idle_ping_seconds, skip_log, filter_account_action, keyword_case_sensitive, description, created_at, updated_at`

func encodeStringListJSON(values []string) string {
	clean := decodeProviderModelsFromSlice(values)
	if clean == nil {
		clean = []string{}
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func encodeIntListJSON(values []int) string {
	seen := map[int]bool{}
	clean := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 || seen[value] {
			continue
		}
		seen[value] = true
		clean = append(clean, value)
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func decodeStringListJSON(raw string) []string {
	return decodeProviderModels(raw)
}

func decodeIntListJSON(raw string) []int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var arr []int
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return nil
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(arr))
	for _, n := range arr {
		if n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func scanUpstreamErrorRule(scan func(...interface{}) error) (UpstreamErrorRule, error) {
	var r UpstreamErrorRule
	var enabled, preferRetryAfter, skipLog, filterAccountAction, keywordCaseSensitive int
	var providersJSON, entrypointsJSON, modelPatternsJSON, statusCodesJSON, bodyKeywordsJSON string
	if err := scan(
		&r.ID, &r.Name, &enabled, &r.Priority,
		&providersJSON, &entrypointsJSON, &modelPatternsJSON, &statusCodesJSON, &bodyKeywordsJSON,
		&r.MatchMode, &r.AccountAction, &r.DownstreamAction, &r.ResponseStatus, &r.CustomMessage,
		&r.CooldownSeconds, &preferRetryAfter, &r.IdleSeconds, &r.IdlePingSeconds, &skipLog, &filterAccountAction, &keywordCaseSensitive,
		&r.Description, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return UpstreamErrorRule{}, err
	}
	r.Enabled = enabled != 0
	r.PreferRetryAfter = preferRetryAfter != 0
	r.SkipLog = skipLog != 0
	r.FilterAccountAction = filterAccountAction != 0
	r.KeywordCaseSensitive = keywordCaseSensitive != 0
	r.Providers = decodeStringListJSON(providersJSON)
	r.Entrypoints = decodeStringListJSON(entrypointsJSON)
	r.ModelPatterns = decodeStringListJSON(modelPatternsJSON)
	r.StatusCodes = decodeIntListJSON(statusCodesJSON)
	r.BodyKeywords = decodeStringListJSON(bodyKeywordsJSON)
	if strings.TrimSpace(r.MatchMode) == "" {
		r.MatchMode = "any"
	}
	if strings.TrimSpace(r.AccountAction) == "" {
		r.AccountAction = "builtin"
	}
	if strings.TrimSpace(r.DownstreamAction) == "" {
		r.DownstreamAction = "builtin"
	}
	if r.IdlePingSeconds == 0 {
		r.IdlePingSeconds = 15
	}
	return r, nil
}

func (s *Store) ListUpstreamErrorRules(ctx context.Context) ([]UpstreamErrorRule, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+upstreamErrorRuleCols+` FROM upstream_error_rules ORDER BY priority ASC, created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UpstreamErrorRule
	for rows.Next() {
		r, err := scanUpstreamErrorRule(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetUpstreamErrorRule(ctx context.Context, id string) (UpstreamErrorRule, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+upstreamErrorRuleCols+` FROM upstream_error_rules WHERE id = ?`, id)
	r, err := scanUpstreamErrorRule(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UpstreamErrorRule{}, false, nil
	}
	if err != nil {
		return UpstreamErrorRule{}, false, err
	}
	return r, true, nil
}

func (s *Store) UpsertUpstreamErrorRule(ctx context.Context, r UpstreamErrorRule) error {
	r.ID = strings.TrimSpace(r.ID)
	if r.ID == "" {
		return errors.New("rule id required")
	}
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		r.Name = r.ID
	}
	r.MatchMode = strings.TrimSpace(r.MatchMode)
	if r.MatchMode == "" {
		r.MatchMode = "any"
	}
	r.AccountAction = strings.TrimSpace(r.AccountAction)
	if r.AccountAction == "" {
		r.AccountAction = "builtin"
	}
	r.DownstreamAction = strings.TrimSpace(r.DownstreamAction)
	if r.DownstreamAction == "" {
		r.DownstreamAction = "builtin"
	}
	if r.IdlePingSeconds == 0 {
		r.IdlePingSeconds = 15
	}
	now := Now()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO upstream_error_rules(id, name, enabled, priority, providers_json, entrypoints_json, model_patterns_json, status_codes_json, body_keywords_json, match_mode, account_action, downstream_action, response_status, custom_message, cooldown_seconds, prefer_retry_after, idle_seconds, idle_ping_seconds, skip_log, filter_account_action, keyword_case_sensitive, description, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name = excluded.name,
 enabled = excluded.enabled,
 priority = excluded.priority,
 providers_json = excluded.providers_json,
 entrypoints_json = excluded.entrypoints_json,
 model_patterns_json = excluded.model_patterns_json,
 status_codes_json = excluded.status_codes_json,
 body_keywords_json = excluded.body_keywords_json,
 match_mode = excluded.match_mode,
 account_action = excluded.account_action,
 downstream_action = excluded.downstream_action,
 response_status = excluded.response_status,
 custom_message = excluded.custom_message,
 cooldown_seconds = excluded.cooldown_seconds,
 prefer_retry_after = excluded.prefer_retry_after,
 idle_seconds = excluded.idle_seconds,
 idle_ping_seconds = excluded.idle_ping_seconds,
 skip_log = excluded.skip_log,
	filter_account_action = excluded.filter_account_action,
	keyword_case_sensitive = excluded.keyword_case_sensitive,
 description = excluded.description,
 updated_at = excluded.updated_at`,
		r.ID, r.Name, boolInt(r.Enabled), r.Priority,
		encodeStringListJSON(r.Providers), encodeStringListJSON(r.Entrypoints), encodeStringListJSON(r.ModelPatterns), encodeIntListJSON(r.StatusCodes), encodeStringListJSON(r.BodyKeywords),
		r.MatchMode, r.AccountAction, r.DownstreamAction, r.ResponseStatus, r.CustomMessage, r.CooldownSeconds, boolInt(r.PreferRetryAfter), r.IdleSeconds, r.IdlePingSeconds, boolInt(r.SkipLog), boolInt(r.FilterAccountAction), boolInt(r.KeywordCaseSensitive), r.Description, r.CreatedAt, r.UpdatedAt)
	return err
}

func (s *Store) DeleteUpstreamErrorRule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM upstream_error_rules WHERE id = ?`, id)
	return err
}

// AuditLogRow is one operator-visible audit record (account lifecycle / automated
// actions such as ban-detection deletes).
type AuditLogRow struct {
	ID           int64  `json:"id"`
	AccountID    string `json:"account_id"`
	AccountLabel string `json:"account_label"`
	Action       string `json:"action"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
	CreatedAt    int64  `json:"created_at"`
}

// InsertAuditLog appends an audit record. It is written BEFORE any destructive
// action (e.g. ban delete) so the trail survives even after the account row is
// gone.
func (s *Store) InsertAuditLog(ctx context.Context, row AuditLogRow) error {
	if row.CreatedAt == 0 {
		row.CreatedAt = Now()
	}
	if len(row.Detail) > 4000 {
		row.Detail = row.Detail[:4000]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_log(account_id, account_label, action, state, reason, detail, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		row.AccountID, row.AccountLabel, row.Action, row.State, row.Reason, row.Detail, row.CreatedAt)
	return err
}

const usageDailyResetDaySettingKey = "usage_daily_reset_day"

func (s *Store) EnsureUsageDailyResetAudit(ctx context.Context, now time.Time) error {
	localDay := now.In(time.Local).Format("2006-01-02")
	createdAt := now.Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
INSERT INTO settings(key, value, updated_at) VALUES(?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
WHERE settings.value <> excluded.value`, usageDailyResetDaySettingKey, localDay, createdAt)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id, account_label, action, state, reason, detail, created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
			"usage_daily_window_reset", "ok", "daily_window", "admin usage daily window reset for VPS local day "+localDay, createdAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed > 0 {
		// This path also writes the settings table; keep the snapshot coherent.
		s.InvalidateSettingsCache()
	}
	return nil
}

// ListAuditLog returns the most recent audit records, newest first.
func (s *Store) ListAuditLog(ctx context.Context, limit int) ([]AuditLogRow, error) {
	limit = normalizeAuditLimit(limit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, account_id, account_label, action, state, reason, detail, created_at FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditLogRows(rows)
}

// ListAuditLogForAccount returns recent audit records for one account without
// forcing callers to load and filter the global audit stream.
func (s *Store) ListAuditLogForAccount(ctx context.Context, accountID string, limit int) ([]AuditLogRow, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return s.ListAuditLog(ctx, limit)
	}
	limit = normalizeAuditLimit(limit)
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, account_id, account_label, action, state, reason, detail, created_at FROM audit_log WHERE account_id = ? ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAuditLogRows(rows)
}

func normalizeAuditLimit(limit int) int {
	if limit <= 0 || limit > 1000 {
		return 200
	}
	return limit
}

func scanAuditLogRows(rows *sql.Rows) ([]AuditLogRow, error) {
	var out []AuditLogRow
	for rows.Next() {
		var r AuditLogRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.AccountLabel, &r.Action, &r.State, &r.Reason, &r.Detail, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InsertCFEvent(ctx context.Context, e CFEvent) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO cf_events(account_id, egress_id, status, cf_ray, category, message, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		e.AccountID, e.EgressID, e.Status, e.CFRay, e.Category, e.Message, e.CreatedAt)
	return err
}

func (s *Store) ListCFEvents(ctx context.Context, limit int) ([]CFEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, account_id, egress_id, status, cf_ray, category, message, created_at FROM cf_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CFEvent
	for rows.Next() {
		var e CFEvent
		if err := rows.Scan(&e.ID, &e.AccountID, &e.EgressID, &e.Status, &e.CFRay, &e.Category, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CountCFEvents(ctx context.Context, where string, args ...interface{}) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM cf_events`
	if strings.TrimSpace(where) != "" {
		query += ` WHERE ` + where
	}
	err := s.rdb.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *Store) DistinctCFAccountsForEgress(ctx context.Context, egressID string, since int64) (int, error) {
	var count int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(DISTINCT account_id) FROM cf_events WHERE egress_id = ? AND created_at >= ?`, egressID, since).Scan(&count)
	return count, err
}

func (s *Store) DistinctCFEgressForAccount(ctx context.Context, accountID string, since int64) (int, error) {
	var count int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(DISTINCT egress_id) FROM cf_events WHERE account_id = ? AND created_at >= ?`, accountID, since).Scan(&count)
	return count, err
}

func (s *Store) InsertVirtualLedger(ctx context.Context, item VirtualLedgerItem) error {
	if item.CreatedAt == 0 {
		item.CreatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO virtual_context_ledger(route_key_hash, account_id, model, prompt_cache_key, role, content, token_estimate, raw_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.RouteKeyHash, item.AccountID, item.Model, item.PromptCacheKey, item.Role, item.Content, item.TokenEstimate, item.RawJSON, item.CreatedAt)
	return err
}

// PurgeVirtualLedger deletes ledger rows older than maxAge seconds.
// The idx_virtual_ledger_route_time index on (route_key_hash, created_at)
// makes the age filter efficient: SQLite uses it to seek to the old rows
// per route rather than scanning the whole table. A background goroutine
// or cron should call this periodically (e.g. every 5 minutes) to prevent
// unbounded ledger growth. Rows are deleted in batches of 500 to avoid
// long write transactions.
func (s *Store) PurgeVirtualLedger(ctx context.Context, maxAgeSeconds int64) (int64, error) {
	if maxAgeSeconds <= 0 {
		maxAgeSeconds = 3600 // default: 1 hour
	}
	cutoff := Now() - maxAgeSeconds
	var totalDeleted int64
	for {
		res, err := s.db.ExecContext(ctx,
			`DELETE FROM virtual_context_ledger WHERE id IN (SELECT id FROM virtual_context_ledger WHERE created_at < ? LIMIT 500)`,
			cutoff)
		if err != nil {
			return totalDeleted, err
		}
		n, _ := res.RowsAffected()
		totalDeleted += n
		if n < 500 {
			break
		}
		// Yield to other operations briefly
		time.Sleep(time.Millisecond)
	}
	return totalDeleted, nil
}

func (s *Store) ListVirtualLedger(ctx context.Context, routeKeyHash string, limit int) ([]VirtualLedgerItem, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, route_key_hash, account_id, model, prompt_cache_key, role, content, token_estimate, raw_json, created_at FROM virtual_context_ledger WHERE route_key_hash = ? ORDER BY id DESC LIMIT ?`, routeKeyHash, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reversed []VirtualLedgerItem
	for rows.Next() {
		var item VirtualLedgerItem
		if err := rows.Scan(&item.ID, &item.RouteKeyHash, &item.AccountID, &item.Model, &item.PromptCacheKey, &item.Role, &item.Content, &item.TokenEstimate, &item.RawJSON, &item.CreatedAt); err != nil {
			return nil, err
		}
		reversed = append(reversed, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]VirtualLedgerItem, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, reversed[i])
	}
	return out, nil
}

func (s *Store) InsertUsageRecord(ctx context.Context, accountID, routeKeyHash, apiKeyHash, userID, model string, prompt, completion, total, cached int64, raw json.RawMessage) error {
	return s.InsertUsageRecordWithCacheDetails(ctx, accountID, routeKeyHash, apiKeyHash, userID, model, prompt, completion, total, cached, cached, 0, raw)
}

func (s *Store) InsertUsageRecordWithCacheDetails(ctx context.Context, accountID, routeKeyHash, apiKeyHash, userID, model string, prompt, completion, total, cached, cacheRead, cacheCreation int64, raw json.RawMessage) error {
	return s.InsertUsageRecordWithDiagnostics(ctx, accountID, routeKeyHash, apiKeyHash, userID, model, prompt, completion, total, cached, cacheRead, cacheCreation, raw, UsageDiagnostics{})
}

type UsageDiagnostics struct {
	UsageEventID                      string
	UsageProvider                     string
	UsageSource                       string
	CacheReadPresent                  bool
	CacheCreationPresent              bool
	CompatibilityLossesJSON           string
	CacheCapability                   string
	Estimated                         bool
	CacheMissTokens                   int64
	CacheTotalInputTokens             int64
	CacheCreation5mTokens             int64
	CacheCreation1hTokens             int64
	AffinitySource                    string
	PromptCacheKeyPresent             bool
	PromptCacheKeySource              string
	StablePrefixSource                string
	StablePrefixReason                string
	StablePrefixBytes                 int
	RetentionEffective                string
	RetentionSource                   string
	ClaudeCacheTTL                    string
	CacheControlInjected              bool
	CacheBreakpointCount              int
	CacheBreakpointsJSON              string
	UnwrittenTailTokens               int64
	MaxPossibleCacheReadTokens        int64
	CachePrewarmAttempted             bool
	CacheHitAfterPrewarm              bool
	SingleflightWaitedRequests        int64
	DiagnosticsMissReason             string
	LatestUserCacheControl            bool
	LatestUserAutoContextCacheControl bool
	LatestUserTailCacheControl        bool
	LatestUserToolResultCacheControl  bool
	RouteEpoch                        int64
}

type UsageRecordWrite struct {
	AccountID, RouteKeyHash, APIKeyHash, UserID, Model string
	Prompt, Completion, Total, Cached                  int64
	CacheRead, CacheCreation                           int64
	Raw                                                json.RawMessage
	Diagnostics                                        UsageDiagnostics
}

type BillingHoldWrite struct {
	ID, RouteKeyHash, AccountID, Status string
	EstimatedTokens, CreatedAt          int64
	Create, IfHeld                      bool
}

type sqlExecContext interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func (s *Store) InsertUsageRecordWithDiagnostics(ctx context.Context, accountID, routeKeyHash, apiKeyHash, userID, model string, prompt, completion, total, cached, cacheRead, cacheCreation int64, raw json.RawMessage, diag UsageDiagnostics) error {
	return s.insertUsageRecord(ctx, s.db, UsageRecordWrite{AccountID: accountID, RouteKeyHash: routeKeyHash, APIKeyHash: apiKeyHash, UserID: userID, Model: model, Prompt: prompt, Completion: completion, Total: total, Cached: cached, CacheRead: cacheRead, CacheCreation: cacheCreation, Raw: raw, Diagnostics: diag})
}

func (s *Store) BatchInsertUsageRecords(ctx context.Context, writes []UsageRecordWrite) error {
	return s.BatchWriteTelemetry(ctx, writes, nil, nil, nil)
}

func (s *Store) BatchWriteTelemetry(ctx context.Context, writes []UsageRecordWrite, apiKeyUsed map[string]int64, holds []BillingHoldWrite, audits []AuditLogRow) error {
	if len(writes) == 0 && len(apiKeyUsed) == 0 && len(holds) == 0 && len(audits) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	persistedKeyMinutes := make(map[string]int64, len(apiKeyUsed))
	for _, write := range writes {
		if err := s.insertUsageRecord(ctx, tx, write); err != nil {
			return err
		}
	}
	for keyHash, usedAt := range apiKeyUsed {
		minute := usedAt / 60
		if old, ok := s.apiKeyUsed.Load(keyHash); ok && old.(int64) >= minute {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE key_hash = ? AND last_used_at < ?`, usedAt, keyHash, usedAt); err != nil {
			return err
		}
		persistedKeyMinutes[keyHash] = minute
	}
	for _, hold := range holds {
		now := hold.CreatedAt
		if now == 0 {
			now = Now()
		}
		if hold.Create {
			if _, err := tx.ExecContext(ctx, `INSERT INTO billing_holds(id, route_key_hash, account_id, estimated_tokens, status, created_at, updated_at) VALUES(?, ?, ?, ?, 'held', ?, ?) ON CONFLICT(id) DO NOTHING`, hold.ID, hold.RouteKeyHash, hold.AccountID, hold.EstimatedTokens, now, now); err != nil {
				return err
			}
			continue
		}
		status := hold.Status
		if status == "" {
			status = "settled"
		}
		query := `UPDATE billing_holds SET status = ?, updated_at = ? WHERE id = ?`
		if hold.IfHeld {
			query += ` AND status = 'held'`
		}
		if _, err := tx.ExecContext(ctx, query, status, now, hold.ID); err != nil {
			return err
		}
	}
	for _, row := range audits {
		if row.CreatedAt == 0 {
			row.CreatedAt = Now()
		}
		if len(row.Detail) > 4000 {
			row.Detail = row.Detail[:4000]
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id, account_label, action, state, reason, detail, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
			row.AccountID, row.AccountLabel, row.Action, row.State, row.Reason, row.Detail, row.CreatedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for keyHash, minute := range persistedKeyMinutes {
		s.apiKeyUsed.Store(keyHash, minute)
	}
	return nil
}

func (s *Store) insertUsageRecord(ctx context.Context, exec sqlExecContext, write UsageRecordWrite) error {
	accountID, routeKeyHash, apiKeyHash, userID, model := write.AccountID, write.RouteKeyHash, write.APIKeyHash, write.UserID, write.Model
	prompt, completion, total, cached := write.Prompt, write.Completion, write.Total, write.Cached
	cacheRead, cacheCreation, raw, diag := write.CacheRead, write.CacheCreation, write.Raw, write.Diagnostics
	diag = finalizeUsageDiagnostics(model, prompt, cached, cacheRead, cacheCreation, raw, diag)
	_, err := exec.ExecContext(ctx, `INSERT INTO usage_records(
usage_event_id, account_id, route_key_hash, api_key_hash, user_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
usage_provider, usage_source, cache_read_present, cache_creation_present, compatibility_losses_json, cache_capability,
estimated, cache_miss_tokens, cache_total_input_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens,
affinity_source, prompt_cache_key_present, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, stable_prefix_bytes,
retention_effective, retention_source, claude_cache_ttl, cache_control_injected, cache_breakpoint_count,
cache_breakpoints_json, unwritten_tail_tokens, max_possible_cache_read_tokens, cache_hit_after_prewarm, singleflight_waited_requests, diagnostics_miss_reason,
latest_user_cache_control, latest_user_auto_context_cache_control, latest_user_tail_cache_control, latest_user_tool_result_cache_control, route_epoch,
raw_usage_json, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(usage_event_id) WHERE usage_event_id <> '' DO UPDATE SET
 account_id=excluded.account_id, route_key_hash=excluded.route_key_hash, api_key_hash=excluded.api_key_hash, user_id=excluded.user_id,
 model=excluded.model, prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens, total_tokens=excluded.total_tokens,
 cached_tokens=excluded.cached_tokens, cache_read_tokens=excluded.cache_read_tokens, cache_creation_tokens=excluded.cache_creation_tokens,
 usage_provider=excluded.usage_provider, usage_source=excluded.usage_source, estimated=excluded.estimated, raw_usage_json=excluded.raw_usage_json
WHERE usage_records.estimated > 0 AND excluded.estimated = 0`,
		diag.UsageEventID, accountID, routeKeyHash, apiKeyHash, userID, model, prompt, completion, total, cached, cacheRead, cacheCreation,
		diag.UsageProvider, diag.UsageSource, boolInt(diag.CacheReadPresent), boolInt(diag.CacheCreationPresent), diag.CompatibilityLossesJSON, diag.CacheCapability,
		boolInt(diag.Estimated), diag.CacheMissTokens, diag.CacheTotalInputTokens, diag.CacheCreation5mTokens, diag.CacheCreation1hTokens,
		diag.AffinitySource, boolInt(diag.PromptCacheKeyPresent), diag.PromptCacheKeySource, diag.StablePrefixSource, diag.StablePrefixReason, diag.StablePrefixBytes,
		diag.RetentionEffective, diag.RetentionSource, diag.ClaudeCacheTTL, boolInt(diag.CacheControlInjected), diag.CacheBreakpointCount,
		diag.CacheBreakpointsJSON, diag.UnwrittenTailTokens, diag.MaxPossibleCacheReadTokens, boolInt(diag.CacheHitAfterPrewarm), diag.SingleflightWaitedRequests, diag.DiagnosticsMissReason,
		boolInt(diag.LatestUserCacheControl),
		boolInt(diag.LatestUserAutoContextCacheControl), boolInt(diag.LatestUserTailCacheControl), boolInt(diag.LatestUserToolResultCacheControl), diag.RouteEpoch,
		string(raw), Now())
	return err
}

func finalizeUsageDiagnostics(model string, prompt, cached, cacheRead, cacheCreation int64, raw json.RawMessage, diag UsageDiagnostics) UsageDiagnostics {
	usageMap := rawUsageMap(raw)
	if diag.LatestUserTailCacheControl || diag.LatestUserToolResultCacheControl {
		diag.LatestUserCacheControl = true
	}
	if diag.Estimated || rawEstimated(raw, usageMap) {
		diag.Estimated = true
	}
	if diag.UsageProvider == "" {
		diag.UsageProvider = usageProvider(model, usageMap, cacheCreation)
	}
	if _, ok := usageMap["cache_read_input_tokens"]; ok {
		diag.CacheReadPresent = true
	}
	if _, ok := usageMap["cache_creation_input_tokens"]; ok {
		diag.CacheCreationPresent = true
	}
	if cacheCreation > 0 {
		diag.CacheCreationPresent = true
	}
	if diag.UsageSource == "" {
		if diag.Estimated {
			diag.UsageSource = "estimated"
		} else {
			diag.UsageSource = "upstream"
		}
	}
	if diag.CacheCreation5mTokens == 0 {
		diag.CacheCreation5mTokens = nestedUsageInt(usageMap, "cache_creation", "ephemeral_5m_input_tokens")
	}
	if diag.CacheCreation1hTokens == 0 {
		diag.CacheCreation1hTokens = nestedUsageInt(usageMap, "cache_creation", "ephemeral_1h_input_tokens")
	}
	if strings.EqualFold(diag.UsageProvider, "kiro") && !diag.CacheReadPresent && !diag.CacheCreationPresent {
		// No Kiro cache field means the upstream did not report a cache metric. It
		// is not a zero-token miss and must not enter the calculable denominator.
		diag.CacheMissTokens = 0
		diag.CacheTotalInputTokens = 0
	} else if diag.CacheTotalInputTokens <= 0 || diag.CacheMissTokens < 0 {
		if isAnthropicUsageMap(usageMap, cacheCreation) {
			diag.CacheMissTokens = prompt
			diag.CacheTotalInputTokens = prompt + cacheRead + cacheCreation
		} else {
			diag.CacheMissTokens = prompt - cacheRead
			if diag.CacheMissTokens < 0 {
				diag.CacheMissTokens = 0
			}
			diag.CacheTotalInputTokens = prompt
		}
	}
	if diag.CacheTotalInputTokens < 0 {
		diag.CacheTotalInputTokens = 0
	}
	if diag.CacheMissTokens < 0 {
		diag.CacheMissTokens = 0
	}
	_ = cached // kept in the signature so callers can pass the parsed usage shape without lossy recomputation.
	return diag
}

func rawUsageMap(raw json.RawMessage) map[string]interface{} {
	var root map[string]interface{}
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	if usage, ok := root["usage"].(map[string]interface{}); ok {
		return usage
	}
	if response, ok := root["response"].(map[string]interface{}); ok {
		if usage, ok := response["usage"].(map[string]interface{}); ok {
			return usage
		}
	}
	return root
}

func rawEstimated(raw json.RawMessage, usageMap map[string]interface{}) bool {
	if b, ok := usageMap["estimated"].(bool); ok && b {
		return true
	}
	var root map[string]interface{}
	if len(raw) > 0 && json.Unmarshal(raw, &root) == nil {
		if b, ok := root["estimated"].(bool); ok && b {
			return true
		}
	}
	return false
}

func usageProvider(model string, usageMap map[string]interface{}, cacheCreation int64) string {
	if isAnthropicUsageMap(usageMap, cacheCreation) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude") {
		return "anthropic"
	}
	return "openai"
}

func isAnthropicUsageMap(usageMap map[string]interface{}, cacheCreation int64) bool {
	if cacheCreation > 0 {
		return true
	}
	if _, ok := usageMap["cache_read_input_tokens"]; ok {
		return true
	}
	if _, ok := usageMap["cache_creation_input_tokens"]; ok {
		return true
	}
	return usageMap["cache_creation"] != nil
}

func usageInt(m map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		case json.Number:
			n, _ := v.Int64()
			return n
		}
	}
	return 0
}

func nestedUsageInt(m map[string]interface{}, parent, child string) int64 {
	if detail, ok := m[parent].(map[string]interface{}); ok {
		return usageInt(detail, child)
	}
	return 0
}

func (s *Store) backfillUsageCacheDiagnostics(ctx context.Context) error {
	// Older Kiro converters used the wire spelling as the persisted model while
	// newer responses use the canonical dotted version. Keep one reporting key.
	if _, err := s.db.ExecContext(ctx, `UPDATE usage_records SET model='claude-opus-4.8' WHERE usage_provider='kiro' AND lower(trim(model))='claude-opus-4-8'`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE affinity_bindings SET model='claude-opus-4.8' WHERE provider='kiro' AND lower(trim(model))='claude-opus-4-8'`); err != nil {
		return err
	}
	// A historical meteringEvent containing only credits was previously treated as
	// authoritative zero-token usage. Preserve the original raw payload for audit,
	// but mark the derived row unreported rather than fabricating token values.
	if _, err := s.db.ExecContext(ctx, `
UPDATE usage_records
SET usage_source='unreported', cache_capability=CASE WHEN cache_capability='' OR cache_capability='unknown' THEN 'unreported' ELSE cache_capability END,
    cache_miss_tokens=0, cache_total_input_tokens=0
WHERE usage_provider='kiro' AND usage_source='upstream'
  AND prompt_tokens=0 AND completion_tokens=0 AND total_tokens=0
  AND cached_tokens=0 AND cache_read_tokens=0 AND cache_creation_tokens=0
  AND cache_read_present=0 AND cache_creation_present=0`); err != nil {
		return err
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT id, model, usage_provider, usage_source, cache_read_present, cache_creation_present, prompt_tokens, cached_tokens,
       CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END,
       cache_creation_tokens, raw_usage_json
FROM usage_records
WHERE cache_total_input_tokens = 0
  AND (prompt_tokens > 0 OR cached_tokens > 0 OR cache_read_tokens > 0 OR cache_creation_tokens > 0
       OR raw_usage_json LIKE '%cache_read_input_tokens%' OR raw_usage_json LIKE '%cache_creation_input_tokens%' OR raw_usage_json LIKE '%estimated%')`)
	if err != nil {
		return err
	}
	type row struct {
		id                                int64
		model, provider, source, raw      string
		cacheReadPresent, createPresent   int
		prompt, cached, cacheRead, create int64
	}
	var items []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model, &r.provider, &r.source, &r.cacheReadPresent, &r.createPresent, &r.prompt, &r.cached, &r.cacheRead, &r.create, &r.raw); err != nil {
			rows.Close()
			return err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range items {
		diag := finalizeUsageDiagnostics(r.model, r.prompt, r.cached, r.cacheRead, r.create, json.RawMessage(r.raw), UsageDiagnostics{
			UsageProvider: r.provider, UsageSource: r.source,
			CacheReadPresent: r.cacheReadPresent != 0, CacheCreationPresent: r.createPresent != 0,
		})
		if _, err := s.db.ExecContext(ctx, `
UPDATE usage_records
SET usage_provider = ?, usage_source = ?, cache_read_present = ?, cache_creation_present = ?, estimated = ?, cache_miss_tokens = ?, cache_total_input_tokens = ?,
    cache_creation_5m_tokens = ?, cache_creation_1h_tokens = ?
WHERE id = ?`,
			diag.UsageProvider, diag.UsageSource, boolInt(diag.CacheReadPresent), boolInt(diag.CacheCreationPresent), boolInt(diag.Estimated), diag.CacheMissTokens, diag.CacheTotalInputTokens,
			diag.CacheCreation5mTokens, diag.CacheCreation1hTokens, r.id); err != nil {
			return err
		}
	}
	return nil
}

// UserUsageRow is a per-model rollup of one portal user's usage (their own console).
type UserUsageRow struct {
	Model               string `json:"model"`
	ModelKey            string `json:"model_key"`
	ModelLabel          string `json:"model_label"`
	Requests            int64  `json:"requests"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheInputTokens    int64  `json:"cache_input_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
}

type CacheUsageReport struct {
	Summary             CacheUsageMetricRow   `json:"summary"`
	ByAccount           []CacheUsageMetricRow `json:"by_account"`
	ByModel             []CacheUsageMetricRow `json:"by_model"`
	ByAPIKey            []CacheUsageMetricRow `json:"by_api_key"`
	ByAccountModel      []CacheUsageMetricRow `json:"by_account_model"`
	ByRoute             []CacheUsageMetricRow `json:"by_route"`
	ByRouteAccountModel []CacheUsageMetricRow `json:"by_route_account_model"`
	ByTimeBucket        []CacheUsageBucket    `json:"by_time_bucket"`
}

type CacheUsageMetricRow struct {
	AccountID                         string   `json:"account_id,omitempty"`
	Model                             string   `json:"model,omitempty"`
	ModelKey                          string   `json:"model_key,omitempty"`
	ModelLabel                        string   `json:"model_label,omitempty"`
	APIKeyHashPrefix                  string   `json:"api_key_hash_prefix,omitempty"`
	RouteKeyHashPrefix                string   `json:"route_key_hash_prefix,omitempty"`
	AffinitySource                    string   `json:"affinity_source,omitempty"`
	RouteClass                        string   `json:"route_class,omitempty"`
	PromptCacheKeySource              string   `json:"prompt_cache_key_source,omitempty"`
	StablePrefixSource                string   `json:"stable_prefix_source,omitempty"`
	StablePrefixReason                string   `json:"stable_prefix_reason,omitempty"`
	RetentionEffective                string   `json:"retention_effective,omitempty"`
	RetentionSource                   string   `json:"retention_source,omitempty"`
	ClaudeCacheTTL                    string   `json:"claude_cache_ttl,omitempty"`
	Requests                          int64    `json:"requests"`
	RealRequests                      int64    `json:"real_requests"`
	HitRequests                       int64    `json:"hit_requests"`
	RequestHitRate                    float64  `json:"request_hit_rate"`
	PromptTokens                      int64    `json:"prompt_tokens"`
	CachedTokens                      int64    `json:"cached_tokens"`
	CacheInputTokens                  int64    `json:"cache_input_tokens"`
	CacheMissTokens                   int64    `json:"cache_miss_tokens"`
	CacheReadTokens                   int64    `json:"cache_read_tokens"`
	CacheCreationTokens               int64    `json:"cache_creation_tokens"`
	CacheCreationReportedRequests     int64    `json:"cache_creation_reported_requests"`
	CacheCreation5mTokens             int64    `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens             int64    `json:"cache_creation_1h_tokens"`
	CacheCreation5mShare              float64  `json:"cache_creation_5m_share"`
	TokenHitRate                      float64  `json:"token_hit_rate"`
	CacheReadShare                    float64  `json:"cache_read_share"`
	CacheWriteShare                   float64  `json:"cache_write_share"`
	EligibleHitRate                   float64  `json:"eligible_cache_hit_rate"`
	RealTokenHitRate                  float64  `json:"real_token_hit_rate"`
	SingleUseRoute                    bool     `json:"single_use_route"`
	RiskFlags                         []string `json:"risk_flags,omitempty"`
	EstimatedRequests                 int64    `json:"estimated_requests"`
	EstimatedRate                     float64  `json:"estimated_rate"`
	StablePrefixBytes                 int64    `json:"stable_prefix_bytes"`
	PromptCacheKeyPresent             int64    `json:"prompt_cache_key_present"`
	CacheControlInjected              int64    `json:"cache_control_injected"`
	CacheBreakpointCount              int64    `json:"cache_breakpoint_count"`
	CacheBreakpointsJSON              string   `json:"cache_breakpoints_json,omitempty"`
	UnwrittenTailTokens               int64    `json:"unwritten_tail_tokens"`
	MaxPossibleCacheReadTokens        int64    `json:"max_possible_cache_read_tokens"`
	CacheHitAfterPrewarm              int64    `json:"cache_hit_after_prewarm"`
	SingleflightWaitedRequests        int64    `json:"singleflight_waited_requests"`
	DiagnosticsMissReason             string   `json:"diagnostics_miss_reason,omitempty"`
	LatestUserCacheControl            int64    `json:"latest_user_cache_control"`
	LatestUserAutoContextCacheControl int64    `json:"latest_user_auto_context_cache_control"`
	LatestUserTailCacheControl        int64    `json:"latest_user_tail_cache_control"`
	LatestUserToolResultCacheControl  int64    `json:"latest_user_tool_result_cache_control"`
	RouteEpoch                        int64    `json:"route_epoch"`
	realCacheInputTokens              int64
	realCacheReadTokens               int64
}

func normalizeUsageModel(model string) (key, label string) {
	label = strings.TrimSpace(model)
	if label == "" {
		return "__unknown__", "(未知)"
	}
	if strings.EqualFold(label, "claude-opus-4-8") {
		label = "claude-opus-4.8"
	}
	return label, label
}

const normalizedUsageModelSQL = `(CASE WHEN lower(TRIM(COALESCE(model,'')))='claude-opus-4-8' THEN 'claude-opus-4.8' ELSE TRIM(COALESCE(model,'')) END)`

func applyUsageModelFields(model string, keyOut, labelOut *string) string {
	key, label := normalizeUsageModel(model)
	if keyOut != nil {
		*keyOut = key
	}
	if labelOut != nil {
		*labelOut = label
	}
	if key == "__unknown__" {
		return ""
	}
	return label
}

type CacheUsageBucket struct {
	Bucket                        int64   `json:"bucket"`
	Requests                      int64   `json:"requests"`
	RealRequests                  int64   `json:"real_requests"`
	HitRequests                   int64   `json:"hit_requests"`
	PromptTokens                  int64   `json:"prompt_tokens"`
	CacheInputTokens              int64   `json:"cache_input_tokens"`
	CacheMissTokens               int64   `json:"cache_miss_tokens"`
	CacheReadTokens               int64   `json:"cache_read_tokens"`
	CacheCreationTokens           int64   `json:"cache_creation_tokens"`
	CacheCreationReportedRequests int64   `json:"cache_creation_reported_requests"`
	CacheCreation5mTokens         int64   `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens         int64   `json:"cache_creation_1h_tokens"`
	CacheReadShare                float64 `json:"cache_read_share"`
	CacheWriteShare               float64 `json:"cache_write_share"`
	EligibleHitRate               float64 `json:"eligible_cache_hit_rate"`
	EstimatedRequests             int64   `json:"estimated_requests"`
	EstimatedRate                 float64 `json:"estimated_rate"`
}

const (
	cacheReadTokensSQL       = "(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END)"
	kiroCacheUnreportedSQL   = "(usage_provider = 'kiro' AND cache_read_present = 0 AND cache_creation_present = 0)"
	cacheTotalInputTokensSQL = "(CASE WHEN " + kiroCacheUnreportedSQL + " THEN 0 WHEN cache_total_input_tokens > 0 THEN cache_total_input_tokens ELSE prompt_tokens END)"
	cacheMissTokensSQL       = "(CASE WHEN " + kiroCacheUnreportedSQL + " THEN 0 WHEN cache_miss_tokens > 0 THEN cache_miss_tokens ELSE MAX(prompt_tokens - " + cacheReadTokensSQL + ", 0) END)"
	estimatedUsageRecordSQL  = "(CASE WHEN estimated > 0 THEN 1 ELSE 0 END)"
	realUsageRecordSQL       = "(CASE WHEN estimated > 0 OR " + kiroCacheUnreportedSQL + " THEN 0 ELSE 1 END)"
	realCacheInputTokensSQL  = "(CASE WHEN estimated > 0 OR " + kiroCacheUnreportedSQL + " THEN 0 ELSE " + cacheTotalInputTokensSQL + " END)"
	realCacheReadTokensSQL   = "(CASE WHEN estimated > 0 OR " + kiroCacheUnreportedSQL + " THEN 0 ELSE " + cacheReadTokensSQL + " END)"
)

// UsageByUser aggregates a single user's usage per model, most-used first.
func (s *Store) UsageByUser(ctx context.Context, userID string) ([]UserUsageRow, error) {
	cutover := s.UsageAccuracyCutover(ctx)
	rows, err := s.rdb.QueryContext(ctx, `
SELECT `+normalizedUsageModelSQL+`, COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0)
FROM usage_records WHERE user_id = ? AND estimated=0 AND created_at>=? GROUP BY `+normalizedUsageModelSQL+` ORDER BY SUM(total_tokens) DESC`, userID, cutover)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserUsageRow
	for rows.Next() {
		var r UserUsageRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens, &r.CacheInputTokens, &r.CacheReadTokens, &r.CacheCreationTokens); err != nil {
			return nil, err
		}
		r.Model = applyUsageModelFields(r.Model, &r.ModelKey, &r.ModelLabel)
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByModel aggregates usage across ALL accounts grouped by model since the given
// epoch second (0 = all time). Powers the admin per-model cache-hit-rate view; same row
// shape as UsageByUser (UserUsageRow) — the UI computes hit rate = cached/prompt.
func (s *Store) UsageByModel(ctx context.Context, since int64) ([]UserUsageRow, error) {
	return s.UsageByModelWindow(ctx, since, usageWindowOpenEndedUntil)
}

func (s *Store) UsageByModelWindow(ctx context.Context, since, until int64) ([]UserUsageRow, error) {
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT `+normalizedUsageModelSQL+`, COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(total_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0)
FROM usage_records WHERE created_at >= ? AND created_at < ? AND estimated=0 GROUP BY `+normalizedUsageModelSQL+` ORDER BY SUM(total_tokens) DESC, COUNT(*) DESC, `+normalizedUsageModelSQL+` ASC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserUsageRow
	for rows.Next() {
		var r UserUsageRow
		if err := rows.Scan(&r.Model, &r.Requests, &r.PromptTokens, &r.CompletionTokens, &r.TotalTokens, &r.CachedTokens, &r.CacheInputTokens, &r.CacheReadTokens, &r.CacheCreationTokens); err != nil {
			return nil, err
		}
		r.Model = applyUsageModelFields(r.Model, &r.ModelKey, &r.ModelLabel)
		out = append(out, r)
	}
	return out, rows.Err()
}

const usageWindowOpenEndedUntil int64 = 1 << 62

func (s *Store) CacheUsageMetrics(ctx context.Context, since int64) (CacheUsageReport, error) {
	return s.CacheUsageMetricsWindow(ctx, since, usageWindowOpenEndedUntil)
}

func (s *Store) CacheUsageMetricsWindow(ctx context.Context, since, until int64) (CacheUsageReport, error) {
	return s.cacheUsageMetricsWindow(ctx, since, until, 200, nil)
}

func (s *Store) CacheUsageMetricsWindowFullRoutes(ctx context.Context, since, until int64) (CacheUsageReport, error) {
	return s.cacheUsageMetricsWindow(ctx, since, until, 0, nil)
}

func (s *Store) CacheUsageMetricsWindowFields(ctx context.Context, since, until int64, routeLimit int, fields map[string]bool) (CacheUsageReport, error) {
	return s.cacheUsageMetricsWindow(ctx, since, until, routeLimit, fields)
}

func (s *Store) cacheUsageMetricsWindow(ctx context.Context, since, until int64, routeLimit int, fields map[string]bool) (CacheUsageReport, error) {
	all := len(fields) == 0
	want := func(name string) bool { return all || fields[name] }
	var report CacheUsageReport
	var err error
	if want("summary") {
		report.Summary, err = s.cacheUsageSummary(ctx, since, until)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_account") {
		report.ByAccount, err = s.cacheUsageRows(ctx, since, until, "account", 200)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_model") {
		report.ByModel, err = s.cacheUsageRows(ctx, since, until, "model", 200)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_api_key") {
		report.ByAPIKey, err = s.cacheUsageRows(ctx, since, until, "api_key", 200)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_account_model") {
		report.ByAccountModel, err = s.cacheUsageRows(ctx, since, until, "account_model", 200)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_route") {
		report.ByRoute, err = s.cacheUsageRows(ctx, since, until, "route", routeLimit)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_route_account_model") {
		report.ByRouteAccountModel, err = s.cacheUsageRows(ctx, since, until, "route_account_model", routeLimit)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	if want("by_time_bucket") {
		report.ByTimeBucket, err = s.CacheUsageBucketsWindow(ctx, since, until, 3600)
		if err != nil {
			return CacheUsageReport{}, err
		}
	}
	return report, nil
}

func (s *Store) cacheUsageSummary(ctx context.Context, since, until int64) (CacheUsageMetricRow, error) {
	var row CacheUsageMetricRow
	err := s.rdb.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(`+realUsageRecordSQL+`),0),
       COALESCE(SUM(CASE WHEN `+cacheReadTokensSQL+` > 0 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(prompt_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
       COALESCE(SUM(`+cacheMissTokensSQL+`),0),
       COALESCE(SUM(`+cacheReadTokensSQL+`),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(cache_creation_5m_tokens),0),
       COALESCE(SUM(cache_creation_1h_tokens),0),
       COALESCE(SUM(`+estimatedUsageRecordSQL+`),0),
       COALESCE(SUM(`+realCacheInputTokensSQL+`),0),
       COALESCE(SUM(`+realCacheReadTokensSQL+`),0),
       COALESCE(SUM(cache_creation_present),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ?`, since, until).Scan(&row.Requests, &row.RealRequests, &row.HitRequests, &row.PromptTokens, &row.CachedTokens, &row.CacheInputTokens, &row.CacheMissTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.CacheCreation5mTokens, &row.CacheCreation1hTokens, &row.EstimatedRequests, &row.realCacheInputTokens, &row.realCacheReadTokens, &row.CacheCreationReportedRequests)
	if err != nil {
		return CacheUsageMetricRow{}, err
	}
	finalizeCacheUsageMetric(&row)
	return row, nil
}

func (s *Store) cacheUsageRows(ctx context.Context, since, until int64, dimension string, limit int) ([]CacheUsageMetricRow, error) {
	selectCols := "COALESCE(account_id,''), '', '', '', '', '', '', '', '', '', ''"
	groupBy := "account_id"
	switch dimension {
	case "account":
	case "model":
		selectCols = "'', " + normalizedUsageModelSQL + ", '', '', '', '', '', '', '', '', ''"
		groupBy = normalizedUsageModelSQL
	case "api_key":
		selectCols = "'', '', CASE WHEN COALESCE(api_key_hash,'') = '' THEN '' ELSE substr(api_key_hash,1,12) END, '', '', '', '', '', '', '', ''"
		groupBy = "CASE WHEN COALESCE(api_key_hash,'') = '' THEN '' ELSE substr(api_key_hash,1,12) END"
	case "account_model":
		selectCols = "COALESCE(account_id,''), " + normalizedUsageModelSQL + ", '', '', '', '', '', '', '', '', ''"
		groupBy = "account_id, " + normalizedUsageModelSQL
	case "route":
		selectCols = "'', '', '', CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, COALESCE(affinity_source,''), COALESCE(prompt_cache_key_source,''), COALESCE(stable_prefix_source,''), COALESCE(stable_prefix_reason,''), COALESCE(retention_effective,''), COALESCE(retention_source,''), COALESCE(claude_cache_ttl,'')"
		groupBy = "CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, affinity_source, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, retention_effective, retention_source, claude_cache_ttl"
	case "route_account_model":
		selectCols = "COALESCE(account_id,''), " + normalizedUsageModelSQL + ", '', CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, COALESCE(affinity_source,''), COALESCE(prompt_cache_key_source,''), COALESCE(stable_prefix_source,''), COALESCE(stable_prefix_reason,''), COALESCE(retention_effective,''), COALESCE(retention_source,''), COALESCE(claude_cache_ttl,'')"
		groupBy = "account_id, " + normalizedUsageModelSQL + ", CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, affinity_source, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, retention_effective, retention_source, claude_cache_ttl"
	default:
		return nil, fmt.Errorf("unknown cache usage dimension %q", dimension)
	}
	cacheReadSQL := cacheReadTokensSQL
	limitSQL := ""
	if limit > 0 {
		limitSQL = fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`
SELECT %s,
       COUNT(*),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(CASE WHEN `+cacheReadSQL+` > 0 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(prompt_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(`+cacheReadSQL+`),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(cache_creation_5m_tokens),0),
       COALESCE(SUM(cache_creation_1h_tokens),0),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(%s),0),
       COALESCE(SUM(cache_creation_present),0),
       COALESCE(MAX(stable_prefix_bytes),0),
       COALESCE(SUM(prompt_cache_key_present),0),
       COALESCE(SUM(cache_control_injected),0),
       COALESCE(MAX(cache_breakpoint_count),0),
       COALESCE(substr(MAX(printf('%%020d', cache_breakpoint_count) || COALESCE(cache_breakpoints_json,'')),21),''),
       COALESCE(MAX(unwritten_tail_tokens),0),
       COALESCE(MAX(max_possible_cache_read_tokens),0),
       COALESCE(SUM(cache_hit_after_prewarm),0),
       COALESCE(SUM(singleflight_waited_requests),0),
       COALESCE(MAX(diagnostics_miss_reason),''),
       COALESCE(SUM(latest_user_cache_control),0),
       COALESCE(SUM(latest_user_auto_context_cache_control),0),
       COALESCE(SUM(latest_user_tail_cache_control),0),
       COALESCE(SUM(latest_user_tool_result_cache_control),0),
       COALESCE(MAX(route_epoch),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ?
GROUP BY %s
ORDER BY SUM(cache_creation_tokens) DESC, SUM(`+cacheReadTokensSQL+`) DESC, COUNT(*) DESC
%s`, selectCols, realUsageRecordSQL, cacheTotalInputTokensSQL, cacheMissTokensSQL, estimatedUsageRecordSQL, realCacheInputTokensSQL, realCacheReadTokensSQL, groupBy, limitSQL), since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheUsageMetricRow
	for rows.Next() {
		var row CacheUsageMetricRow
		if err := rows.Scan(
			&row.AccountID, &row.Model, &row.APIKeyHashPrefix, &row.RouteKeyHashPrefix,
			&row.AffinitySource, &row.PromptCacheKeySource, &row.StablePrefixSource, &row.StablePrefixReason,
			&row.RetentionEffective, &row.RetentionSource, &row.ClaudeCacheTTL,
			&row.Requests, &row.RealRequests, &row.HitRequests, &row.PromptTokens, &row.CachedTokens,
			&row.CacheInputTokens, &row.CacheMissTokens, &row.CacheReadTokens, &row.CacheCreationTokens,
			&row.CacheCreation5mTokens, &row.CacheCreation1hTokens, &row.EstimatedRequests,
			&row.realCacheInputTokens, &row.realCacheReadTokens, &row.CacheCreationReportedRequests, &row.StablePrefixBytes,
			&row.PromptCacheKeyPresent, &row.CacheControlInjected, &row.CacheBreakpointCount,
			&row.CacheBreakpointsJSON, &row.UnwrittenTailTokens, &row.MaxPossibleCacheReadTokens,
			&row.CacheHitAfterPrewarm, &row.SingleflightWaitedRequests, &row.DiagnosticsMissReason,
			&row.LatestUserCacheControl, &row.LatestUserAutoContextCacheControl,
			&row.LatestUserTailCacheControl, &row.LatestUserToolResultCacheControl, &row.RouteEpoch,
		); err != nil {
			return nil, err
		}
		if dimension == "model" || dimension == "account_model" || dimension == "route_account_model" {
			row.Model = applyUsageModelFields(row.Model, &row.ModelKey, &row.ModelLabel)
		}
		finalizeCacheUsageMetric(&row)
		out = append(out, row)
	}
	return out, rows.Err()
}

func finalizeCacheUsageMetric(row *CacheUsageMetricRow) {
	if row.RealRequests > 0 {
		// Estimated usage and Kiro rows whose upstream omitted cache fields are not
		// misses. Only requests with calculable upstream cache metering belong in
		// the hit-rate denominator.
		row.RequestHitRate = float64(row.HitRequests) / float64(row.RealRequests)
	}
	if row.Requests > 0 {
		row.EstimatedRate = float64(row.EstimatedRequests) / float64(row.Requests)
	}
	denominator := row.CacheInputTokens
	if denominator <= 0 && row.RealRequests > 0 {
		denominator = row.PromptTokens
		row.CacheInputTokens = row.PromptTokens
	}
	if denominator > 0 {
		row.TokenHitRate = float64(row.CacheReadTokens) / float64(denominator)
		row.CacheReadShare = row.TokenHitRate
		if row.CacheCreationReportedRequests > 0 {
			row.CacheWriteShare = float64(row.CacheCreationTokens) / float64(denominator)
		}
		if row.TokenHitRate > 1 {
			row.TokenHitRate = 1
		}
		if row.TokenHitRate < 0 {
			row.TokenHitRate = 0
		}
		if row.CacheReadShare > 1 {
			row.CacheReadShare = 1
		}
		if row.CacheReadShare < 0 {
			row.CacheReadShare = 0
		}
		if row.CacheWriteShare > 1 {
			row.CacheWriteShare = 1
		}
		if row.CacheWriteShare < 0 {
			row.CacheWriteShare = 0
		}
	}
	eligible := row.CacheReadTokens + row.CacheCreationTokens
	if row.CacheCreationReportedRequests > 0 && eligible > 0 {
		row.EligibleHitRate = float64(row.CacheReadTokens) / float64(eligible)
	}
	if row.CacheCreationTokens > 0 {
		row.CacheCreation5mShare = float64(row.CacheCreation5mTokens) / float64(row.CacheCreationTokens)
	}
	if row.realCacheInputTokens > 0 {
		row.RealTokenHitRate = float64(row.realCacheReadTokens) / float64(row.realCacheInputTokens)
		if row.RealTokenHitRate > 1 {
			row.RealTokenHitRate = 1
		}
		if row.RealTokenHitRate < 0 {
			row.RealTokenHitRate = 0
		}
	}
	row.RouteClass = RouteClassForAffinitySource(row.AffinitySource)
	row.SingleUseRoute = row.Requests == 1
	row.RiskFlags = CacheRiskFlags(*row)
}

func RouteClassForAffinitySource(source string) string {
	switch source {
	case "cache_prefix_hash", "stable_messages_hash":
		return "stable_prefix"
	case "x-claude-code-session-id", "codex-root-thread-id", "x-codex-parent-thread-id", "thread_id", "conversation_id",
		"x-codex-window-id", "prompt_cache_key", "previous_response_id", "claude_resource", "x-codex-turn-metadata":
		return "true_conversation"
	case "downstream_api_project_model":
		return "coarse"
	default:
		return "other"
	}
}

func CacheRiskFlags(row CacheUsageMetricRow) []string {
	flags := []string{}
	if row.CacheWriteShare >= 0.20 {
		flags = append(flags, "high_write_share")
	}
	if row.SingleUseRoute {
		flags = append(flags, "single_use")
	}
	if row.RouteClass == "coarse" {
		flags = append(flags, "coarse_route")
	}
	if row.LatestUserTailCacheControl > 0 || row.LatestUserToolResultCacheControl > 0 {
		flags = append(flags, "volatile_latest_user_cache_control")
	}
	return flags
}

func (s *Store) CacheUsageBuckets(ctx context.Context, since, bucketSeconds int64) ([]CacheUsageBucket, error) {
	return s.CacheUsageBucketsWindow(ctx, since, usageWindowOpenEndedUntil, bucketSeconds)
}

func (s *Store) CacheUsageBucketsWindow(ctx context.Context, since, until, bucketSeconds int64) ([]CacheUsageBucket, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (created_at / ?) * ? AS bucket,
       COUNT(*),
       COALESCE(SUM(`+realUsageRecordSQL+`),0),
       COALESCE(SUM(CASE WHEN `+cacheReadTokensSQL+` > 0 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(prompt_tokens),0),
       COALESCE(SUM(`+cacheTotalInputTokensSQL+`),0),
       COALESCE(SUM(`+cacheMissTokensSQL+`),0),
       COALESCE(SUM(`+cacheReadTokensSQL+`),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(cache_creation_5m_tokens),0),
       COALESCE(SUM(cache_creation_1h_tokens),0),
       COALESCE(SUM(`+estimatedUsageRecordSQL+`),0),
       COALESCE(SUM(cache_creation_present),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ?
GROUP BY bucket ORDER BY bucket`, bucketSeconds, bucketSeconds, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CacheUsageBucket
	for rows.Next() {
		var b CacheUsageBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.RealRequests, &b.HitRequests, &b.PromptTokens, &b.CacheInputTokens, &b.CacheMissTokens, &b.CacheReadTokens, &b.CacheCreationTokens, &b.CacheCreation5mTokens, &b.CacheCreation1hTokens, &b.EstimatedRequests, &b.CacheCreationReportedRequests); err != nil {
			return nil, err
		}
		finalizeCacheUsageBucket(&b)
		out = append(out, b)
	}
	return out, rows.Err()
}

func finalizeCacheUsageBucket(b *CacheUsageBucket) {
	if b.Requests > 0 {
		b.EstimatedRate = float64(b.EstimatedRequests) / float64(b.Requests)
	}
	if b.CacheInputTokens > 0 {
		b.CacheReadShare = float64(b.CacheReadTokens) / float64(b.CacheInputTokens)
		if b.CacheCreationReportedRequests > 0 {
			b.CacheWriteShare = float64(b.CacheCreationTokens) / float64(b.CacheInputTokens)
		}
		if b.CacheReadShare > 1 {
			b.CacheReadShare = 1
		}
		if b.CacheReadShare < 0 {
			b.CacheReadShare = 0
		}
		if b.CacheWriteShare > 1 {
			b.CacheWriteShare = 1
		}
		if b.CacheWriteShare < 0 {
			b.CacheWriteShare = 0
		}
	}
	eligible := b.CacheReadTokens + b.CacheCreationTokens
	if b.CacheCreationReportedRequests > 0 && eligible > 0 {
		b.EligibleHitRate = float64(b.CacheReadTokens) / float64(eligible)
	}
}

// UsageTimeseriesByUser is UsageTimeseries scoped to one user's attributed rows.
func (s *Store) UsageTimeseriesByUser(ctx context.Context, userID string, since, bucketSeconds int64) ([]UsageBucket, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (created_at / ?) * ? AS bucket, COUNT(*),
       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(total_tokens),0)
FROM usage_records WHERE user_id = ? AND created_at >= ? AND estimated=0
GROUP BY bucket ORDER BY bucket`, bucketSeconds, bucketSeconds, userID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageBucket
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.PromptTokens, &b.CompletionTokens, &b.CachedTokens, &b.CacheReadTokens, &b.CacheCreationTokens, &b.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UsageSummaryRow is a per-account rollup of recorded usage.
type UsageSummaryRow struct {
	AccountID           string `json:"account_id"`
	Requests            int64  `json:"requests"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	ActualRequests      int64  `json:"actual_requests"`
	ActualTokens        int64  `json:"actual_tokens"`
	EstimatedRequests   int64  `json:"estimated_requests"`
	EstimatedTokens     int64  `json:"estimated_tokens"`
}

// UsageSummary aggregates usage_records per account, most-used first.
func (s *Store) UsageSummary(ctx context.Context) ([]UsageSummaryRow, error) {
	return s.UsageSummaryWindow(ctx, 0, usageWindowOpenEndedUntil)
}

func (s *Store) UsageSummaryWindow(ctx context.Context, since, until int64) ([]UsageSummaryRow, error) {
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT account_id, SUM(CASE WHEN estimated=0 THEN 1 ELSE 0 END), COALESCE(SUM(CASE WHEN estimated=0 THEN prompt_tokens ELSE 0 END),0), COALESCE(SUM(CASE WHEN estimated=0 THEN completion_tokens ELSE 0 END),0), COALESCE(SUM(CASE WHEN estimated=0 THEN total_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN cached_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN cache_creation_tokens ELSE 0 END),0),
       SUM(CASE WHEN estimated=0 THEN 1 ELSE 0 END), COALESCE(SUM(CASE WHEN estimated=0 THEN total_tokens ELSE 0 END),0),
       SUM(CASE WHEN estimated>0 THEN 1 ELSE 0 END), COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0)
FROM usage_records WHERE created_at >= ? AND created_at < ? GROUP BY account_id ORDER BY SUM(total_tokens) DESC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSummaryRow
	for rows.Next() {
		var row UsageSummaryRow
		if err := rows.Scan(&row.AccountID, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.ActualRequests, &row.ActualTokens, &row.EstimatedRequests, &row.EstimatedTokens); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) UsageAccuracyCutover(ctx context.Context) int64 {
	v, ok, err := s.GetSetting(ctx, "usage_accuracy_cutover_at")
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// UsageSummaryByAccountIDs aggregates usage only for the requested accounts.
func (s *Store) UsageSummaryByAccountIDs(ctx context.Context, accountIDs []string) (map[string]UsageSummaryRow, error) {
	out := make(map[string]UsageSummaryRow, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	args := stringArgs(accountIDs)
	args = append(args, s.UsageAccuracyCutover(ctx))
	// Primary columns stay real-only (estimated=0) so the numbers the pool page
	// already shows are unchanged; the Actual/Estimated split columns additionally
	// surface estimated traffic (D2) instead of the old `WHERE estimated=0` that
	// dropped it from the account list entirely (Kiro-unmetered traffic especially).
	rows, err := s.rdb.QueryContext(ctx, `
SELECT account_id,
       SUM(CASE WHEN estimated=0 THEN 1 ELSE 0 END),
       COALESCE(SUM(CASE WHEN estimated=0 THEN prompt_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN completion_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN total_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN cached_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 THEN cache_creation_tokens ELSE 0 END),0),
       SUM(CASE WHEN estimated=0 THEN 1 ELSE 0 END),
       COALESCE(SUM(CASE WHEN estimated=0 THEN total_tokens ELSE 0 END),0),
       SUM(CASE WHEN estimated>0 THEN 1 ELSE 0 END),
       COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0)
FROM usage_records
WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`)
  AND created_at>=?
GROUP BY account_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var row UsageSummaryRow
		if err := rows.Scan(&row.AccountID, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.ActualRequests, &row.ActualTokens, &row.EstimatedRequests, &row.EstimatedTokens); err != nil {
			return nil, err
		}
		out[row.AccountID] = row
	}
	return out, rows.Err()
}

// UsageBucket is a time-bucketed rollup of usage_records, used to render the
// usage-over-time area chart on the dashboard.
type UsageBucket struct {
	Bucket              int64 `json:"bucket"` // epoch seconds at the start of the bucket
	Requests            int64 `json:"requests"`
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type UsageSeriesDescriptor struct {
	SeriesDimension string `json:"series_dimension"`
	SeriesKey       string `json:"series_key"`
	SeriesLabel     string `json:"series_label"`
}

type UsageModelSeriesRow struct {
	Bucket              int64  `json:"bucket"`
	SeriesDimension     string `json:"series_dimension"`
	SeriesKey           string `json:"series_key"`
	SeriesLabel         string `json:"series_label"`
	Requests            int64  `json:"requests"`
	PromptTokens        int64  `json:"prompt_tokens"`
	CompletionTokens    int64  `json:"completion_tokens"`
	CachedTokens        int64  `json:"cached_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
	TotalTokens         int64  `json:"total_tokens"`
}

// UsageTimeseries returns usage_records aggregated into fixed-width time buckets
// from since (epoch seconds) to now, oldest first. bucketSeconds must be > 0.
func (s *Store) UsageTimeseries(ctx context.Context, since, bucketSeconds int64) ([]UsageBucket, error) {
	return s.UsageTimeseriesWindow(ctx, since, usageWindowOpenEndedUntil, bucketSeconds)
}

func (s *Store) UsageTimeseriesWindow(ctx context.Context, since, until, bucketSeconds int64) ([]UsageBucket, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (created_at / ?) * ? AS bucket,
       COUNT(*),
       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(total_tokens),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ? AND estimated=0
GROUP BY bucket ORDER BY bucket`, bucketSeconds, bucketSeconds, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageBucket
	for rows.Next() {
		var b UsageBucket
		if err := rows.Scan(&b.Bucket, &b.Requests, &b.PromptTokens, &b.CompletionTokens, &b.CachedTokens, &b.CacheReadTokens, &b.CacheCreationTokens, &b.TotalTokens); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) UsageModelSeriesWindow(ctx context.Context, since, until, bucketSeconds int64, seriesLimit int) ([]UsageSeriesDescriptor, []UsageModelSeriesRow, error) {
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	if seriesLimit <= 0 {
		seriesLimit = 6
	}
	cutover := s.UsageAccuracyCutover(ctx)
	if since < cutover {
		since = cutover
	}
	const keyExpr = "CASE WHEN TRIM(COALESCE(model,'')) = '' THEN '__unknown__' ELSE TRIM(COALESCE(model,'')) END"
	const labelExpr = "CASE WHEN TRIM(COALESCE(model,'')) = '' THEN '(未知)' ELSE TRIM(COALESCE(model,'')) END"
	topRows, err := s.rdb.QueryContext(ctx, `
SELECT `+keyExpr+`, `+labelExpr+`, COALESCE(SUM(total_tokens),0), COUNT(*)
FROM usage_records
WHERE created_at >= ? AND created_at < ? AND estimated=0
GROUP BY `+keyExpr+`
ORDER BY COALESCE(SUM(total_tokens),0) DESC, COUNT(*) DESC, `+keyExpr+` ASC
LIMIT ?`, since, until, seriesLimit)
	if err != nil {
		return nil, nil, err
	}
	var series []UsageSeriesDescriptor
	keys := []string{}
	for topRows.Next() {
		var key, label string
		var total, count int64
		if err := topRows.Scan(&key, &label, &total, &count); err != nil {
			topRows.Close()
			return nil, nil, err
		}
		_ = total
		_ = count
		series = append(series, UsageSeriesDescriptor{SeriesDimension: "model", SeriesKey: key, SeriesLabel: label})
		keys = append(keys, key)
	}
	if err := topRows.Err(); err != nil {
		topRows.Close()
		return nil, nil, err
	}
	topRows.Close()
	if len(keys) == 0 {
		return series, []UsageModelSeriesRow{}, nil
	}
	args := []interface{}{bucketSeconds, bucketSeconds, since, until}
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT (created_at / ?) * ? AS bucket,
       `+keyExpr+` AS model_key,
       `+labelExpr+` AS model_label,
       COUNT(*),
       COALESCE(SUM(prompt_tokens),0),
       COALESCE(SUM(completion_tokens),0),
       COALESCE(SUM(cached_tokens),0),
       COALESCE(SUM(CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END),0),
       COALESCE(SUM(cache_creation_tokens),0),
       COALESCE(SUM(total_tokens),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ? AND estimated=0
  AND `+keyExpr+` IN (`+sqlPlaceholders(len(keys))+`)
GROUP BY bucket, model_key
ORDER BY bucket, model_key`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []UsageModelSeriesRow{}
	for rows.Next() {
		var row UsageModelSeriesRow
		row.SeriesDimension = "model"
		if err := rows.Scan(&row.Bucket, &row.SeriesKey, &row.SeriesLabel, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.TotalTokens); err != nil {
			return nil, nil, err
		}
		out = append(out, row)
	}
	return series, out, rows.Err()
}

// UpsertAccountRateLimit stores the latest rate-limit / quota snapshot for an
// account/provider/model/limiter dimension, replacing only that dimension.
func (s *Store) UpsertAccountRateLimit(ctx context.Context, r AccountRateLimit) error {
	if r.UpdatedAt == 0 {
		r.UpdatedAt = Now()
	}
	r.Provider = strings.TrimSpace(r.Provider)
	r.Model = strings.TrimSpace(r.Model)
	r.LimiterType = strings.TrimSpace(r.LimiterType)
	if r.LimiterType == "" {
		r.LimiterType = strings.TrimSpace(r.Source)
	}
	if r.LimiterType == "" {
		r.LimiterType = "default"
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_rate_limits(account_id, provider, model, limiter_type, source, used_percent, limit_tokens, remaining_tokens, limit_requests, remaining_requests, reset_at, status, raw_json, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, provider, model, limiter_type) DO UPDATE SET
 source = excluded.source, used_percent = excluded.used_percent,
 limit_tokens = excluded.limit_tokens, remaining_tokens = excluded.remaining_tokens,
 limit_requests = excluded.limit_requests, remaining_requests = excluded.remaining_requests,
 reset_at = excluded.reset_at, status = excluded.status, raw_json = excluded.raw_json, updated_at = excluded.updated_at`,
		r.AccountID, r.Provider, r.Model, r.LimiterType, r.Source, r.UsedPercent, r.LimitTokens, r.RemainingTokens,
		r.LimitRequests, r.RemainingRequests, r.ResetAt, r.Status, r.Raw, r.UpdatedAt)
	if err == nil {
		s.rateLimitGen.Add(1)
	}
	return err
}

func (s *Store) RateLimitGeneration() uint64 { return s.rateLimitGen.Load() }

func scanAccountRateLimit(scan func(...interface{}) error) (AccountRateLimit, error) {
	var r AccountRateLimit
	err := scan(&r.AccountID, &r.Provider, &r.Model, &r.LimiterType, &r.Source, &r.UsedPercent, &r.LimitTokens, &r.RemainingTokens,
		&r.LimitRequests, &r.RemainingRequests, &r.ResetAt, &r.Status, &r.Raw, &r.UpdatedAt)
	return r, err
}

const rateLimitCols = `account_id, provider, model, limiter_type, source, used_percent, limit_tokens, remaining_tokens, limit_requests, remaining_requests, reset_at, status, raw_json, updated_at`

// GetAccountRateLimit returns the most recently updated quota snapshot for an
// account. The returned bool is false when no snapshot has been captured yet.
func (s *Store) GetAccountRateLimit(ctx context.Context, accountID string) (AccountRateLimit, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits WHERE account_id = ? ORDER BY updated_at DESC LIMIT 1`, accountID)
	r, err := scanAccountRateLimit(row.Scan)
	if err == sql.ErrNoRows {
		return AccountRateLimit{}, false, nil
	}
	if err != nil {
		return AccountRateLimit{}, false, err
	}
	return r, true, nil
}

// GetAccountRateLimitFor returns a quota snapshot for one concrete
// account/provider/model/limiter dimension.
func (s *Store) GetAccountRateLimitFor(ctx context.Context, accountID, provider, model, limiterType string) (AccountRateLimit, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits WHERE account_id = ? AND provider = ? AND model = ? AND limiter_type = ?`,
		accountID, strings.TrimSpace(provider), strings.TrimSpace(model), strings.TrimSpace(limiterType))
	r, err := scanAccountRateLimit(row.Scan)
	if err == sql.ErrNoRows {
		return AccountRateLimit{}, false, nil
	}
	if err != nil {
		return AccountRateLimit{}, false, err
	}
	return r, true, nil
}

// ListAccountRateLimits returns every stored quota snapshot, most-recently-updated
// first, for the admin quota overview.
func (s *Store) ListAccountRateLimits(ctx context.Context) ([]AccountRateLimit, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountRateLimit
	for rows.Next() {
		r, err := scanAccountRateLimit(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListAccountRateLimitsByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]AccountRateLimit, error) {
	out := make(map[string][]AccountRateLimit, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = nil
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	// Keep each IN clause comfortably below SQLite's host-parameter limit. Large
	// pools can contain thousands of accounts, and this method is used on the hot
	// scheduler path where a parameter-limit failure must not disable quota routing.
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		rows, err := s.rdb.QueryContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits WHERE account_id IN (`+sqlPlaceholders(len(batch))+`) ORDER BY updated_at DESC`, stringArgs(batch)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			r, err := scanAccountRateLimit(rows.Scan)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[r.AccountID] = append(out[r.AccountID], r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AccountRateLimitCooldownUntilFromSnapshots applies the same provider/model and
// exhaustion semantics as AccountRateLimitCooldownUntil to an already-loaded set of
// snapshots. The scheduler uses this after one batch query, avoiding one SQLite query
// per candidate while preserving account-wide (blank model/provider) limits.
func AccountRateLimitCooldownUntilFromSnapshots(rows []AccountRateLimit, provider, model string, now int64) (int64, bool) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	var earliest int64
	for _, r := range rows {
		if r.ResetAt <= now {
			continue
		}
		rowProvider := strings.TrimSpace(r.Provider)
		if provider != "" && rowProvider != "" && rowProvider != provider {
			continue
		}
		rowModel := strings.TrimSpace(r.Model)
		if rowModel != "" && rowModel != model {
			continue
		}
		if !accountRateLimitExhausted(r) {
			continue
		}
		if earliest == 0 || r.ResetAt < earliest {
			earliest = r.ResetAt
		}
	}
	return earliest, earliest > 0
}

// AccountRateLimitCooldownUntil returns the reset time for an active exhausted
// limiter that applies to the requested provider/model. A blank model row is treated
// as account-wide; a model-specific row applies only to that model.
func (s *Store) AccountRateLimitCooldownUntil(ctx context.Context, accountID, provider, model string, now int64) (int64, bool, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits WHERE account_id = ? AND reset_at > ? AND (? = '' OR provider = '' OR provider = ?) AND (model = '' OR model = ?) ORDER BY reset_at ASC`,
		accountID, now, provider, provider, model)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanAccountRateLimit(rows.Scan)
		if err != nil {
			return 0, false, err
		}
		if accountRateLimitExhausted(r) {
			return r.ResetAt, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

func accountRateLimitExhausted(r AccountRateLimit) bool {
	if strings.EqualFold(strings.TrimSpace(r.Status), "rejected") {
		return true
	}
	switch strings.TrimSpace(r.LimiterType) {
	case "requests":
		return r.RemainingRequests == 0
	case "tokens", "input_tokens", "output_tokens", "unified", "5h_polled":
		return r.RemainingTokens == 0
	}
	return r.RemainingTokens == 0 || r.RemainingRequests == 0
}

func scanCodexResetCreditConsumption(scan func(...interface{}) error) (CodexResetCreditConsumption, error) {
	var row CodexResetCreditConsumption
	err := scan(&row.AccountID, &row.SevenDayResetAt, &row.RedeemRequestID, &row.Status, &row.CreatedAt, &row.UpdatedAt)
	return row, err
}

const codexResetCreditConsumptionCols = `account_id, seven_day_reset_at, redeem_request_id, status, created_at, updated_at`

func (s *Store) ClaimCodexResetCreditConsumption(ctx context.Context, accountID string, sevenDayResetAt int64, redeemRequestID string, now int64) (CodexResetCreditClaim, error) {
	accountID = strings.TrimSpace(accountID)
	redeemRequestID = strings.TrimSpace(redeemRequestID)
	if now == 0 {
		now = Now()
	}
	if accountID == "" || sevenDayResetAt <= 0 || redeemRequestID == "" {
		return CodexResetCreditClaim{}, fmt.Errorf("invalid codex reset credit claim")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodexResetCreditClaim{}, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, `SELECT `+codexResetCreditConsumptionCols+` FROM codex_reset_credit_consumptions WHERE account_id = ? AND seven_day_reset_at = ?`, accountID, sevenDayResetAt)
	existing, err := scanCodexResetCreditConsumption(row.Scan)
	if err == nil {
		if strings.TrimSpace(existing.Status) == "in_progress" && now-existing.UpdatedAt > 120 {
			if _, err := tx.ExecContext(ctx, `UPDATE codex_reset_credit_consumptions SET status = 'unknown', updated_at = ? WHERE account_id = ? AND seven_day_reset_at = ?`, now, accountID, sevenDayResetAt); err != nil {
				return CodexResetCreditClaim{}, err
			}
			existing.Status = "unknown"
			existing.UpdatedAt = now
		}
		if err := tx.Commit(); err != nil {
			return CodexResetCreditClaim{}, err
		}
		return CodexResetCreditClaim{Claimed: false, Row: existing}, nil
	}
	if err != sql.ErrNoRows {
		return CodexResetCreditClaim{}, err
	}

	inserted := CodexResetCreditConsumption{
		AccountID:       accountID,
		SevenDayResetAt: sevenDayResetAt,
		RedeemRequestID: redeemRequestID,
		Status:          "in_progress",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO codex_reset_credit_consumptions(account_id, seven_day_reset_at, redeem_request_id, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		inserted.AccountID, inserted.SevenDayResetAt, inserted.RedeemRequestID, inserted.Status, inserted.CreatedAt, inserted.UpdatedAt); err != nil {
		return CodexResetCreditClaim{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodexResetCreditClaim{}, err
	}
	return CodexResetCreditClaim{Claimed: true, Row: inserted}, nil
}

func (s *Store) UpdateCodexResetCreditConsumptionStatus(ctx context.Context, accountID string, sevenDayResetAt int64, status string, now int64) error {
	accountID = strings.TrimSpace(accountID)
	status = strings.TrimSpace(status)
	if now == 0 {
		now = Now()
	}
	if accountID == "" || sevenDayResetAt <= 0 || status == "" {
		return fmt.Errorf("invalid codex reset credit consumption status")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE codex_reset_credit_consumptions SET status = ?, updated_at = ? WHERE account_id = ? AND seven_day_reset_at = ?`, status, now, accountID, sevenDayResetAt)
	return err
}

func (s *Store) CreateBillingHold(ctx context.Context, routeKeyHash, accountID string, estimatedTokens int64) (string, error) {
	now := Now()
	id := fmt.Sprintf("hold_%d_%s", time.Now().UnixNano(), accountID)
	_, err := s.db.ExecContext(ctx, `INSERT INTO billing_holds(id, route_key_hash, account_id, estimated_tokens, status, created_at, updated_at) VALUES(?, ?, ?, ?, 'held', ?, ?)`,
		id, routeKeyHash, accountID, estimatedTokens, now, now)
	return id, err
}

func (s *Store) SettleBillingHold(ctx context.Context, id, status string) error {
	if id == "" {
		return nil
	}
	if status == "" {
		status = "settled"
	}
	// Detach from ctx cancellation: a streaming downstream client that disconnects
	// cancels the request context, and settling on that cancelled context makes the
	// UPDATE fail silently — the historical `expired_unsettled` leak. WithoutCancel
	// keeps request values but drops cancellation so the terminal status always lands.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE billing_holds SET status = ?, updated_at = ? WHERE id = ?`, status, Now(), id)
	return err
}

// SettleBillingHoldIfHeld settles a hold ONLY while it is still in the 'held' state.
// It is the deferred backstop each inference handler arms right after creating a hold:
// whichever branch the handler returns through, the hold reaches a terminal status,
// but a status already written by the handler (settled / failed_upstream / …) is never
// overwritten because the WHERE clause no longer matches. Like SettleBillingHold it is
// cancellation-proof so a disconnected client cannot leave a hold unsettled.
func (s *Store) SettleBillingHoldIfHeld(ctx context.Context, id, status string) error {
	if id == "" {
		return nil
	}
	if status == "" {
		status = "settled"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE billing_holds SET status = ?, updated_at = ? WHERE id = ? AND status = 'held'`, status, Now(), id)
	return err
}

func (s *Store) ExpireStaleBillingHolds(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = time.Hour
	}
	cutoff := Now() - int64(olderThan/time.Second)
	res, err := s.db.ExecContext(ctx, `UPDATE billing_holds SET status = 'expired_unsettled', updated_at = ? WHERE status = 'held' AND created_at < ?`, Now(), cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) GetBillingHold(ctx context.Context, id string) (BillingHold, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, route_key_hash, account_id, estimated_tokens, status, created_at, updated_at FROM billing_holds WHERE id = ?`, id)
	var hold BillingHold
	err := row.Scan(&hold.ID, &hold.RouteKeyHash, &hold.AccountID, &hold.EstimatedTokens, &hold.Status, &hold.CreatedAt, &hold.UpdatedAt)
	return hold, err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sqlPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func stringArgs(values []string) []interface{} {
	args := make([]interface{}, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

type scanner interface {
	Scan(dest ...interface{}) error
}

func scanAccount(row scanner) (Account, error) {
	var acc Account
	var fed int
	err := row.Scan(&acc.ID, &acc.Label, &acc.GroupName, &acc.UpstreamAccountID, &acc.ChatGPTUserID, &acc.Email, &acc.PlanType, &acc.Provider, &acc.Status, &fed, &acc.QuarantineUntil, &acc.QuarantineReason, &acc.CreatedAt, &acc.UpdatedAt)
	acc.IsFedramp = fed != 0
	return acc, err
}

func scanEgress(row scanner) (EgressProfile, error) {
	var p EgressProfile
	var stream int
	err := row.Scan(&p.ID, &p.Name, &p.Type, &p.Endpoint, &p.ChainProxy, &p.Region, &p.ExitIP, &stream, &p.Health, &p.LatencyMillis, &p.CFScore, &p.LastCFRay, &p.CooldownUntil, &p.MaxConcurrency, &p.CreatedAt, &p.UpdatedAt, &p.ProxyAuthMode, &p.ProxyAPIKey, &p.IPMode, &p.ProviderKey, &p.DynamicConfigJSON)
	p.StreamCapable = stream != 0
	if strings.TrimSpace(p.DynamicConfigJSON) == "" {
		p.DynamicConfigJSON = "{}"
	}
	return p, err
}

func NotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

func MustJSON(v interface{}) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(raw)
}
