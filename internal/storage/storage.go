package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/secretbox"
	"codex-account-pool/internal/superinstruct"
	"codex-account-pool/internal/supervisor"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
)

const DefaultDirectEgressID = "egress_direct"

const (
	// Route bindings preserve account/provider affinity across CLI resumptions.
	// Retire only conversations that have been inactive for a full month; every
	// successful lookup refreshes updated_at at most once per hour, keeping the
	// hot path stable without one write per request.
	routeBindingRetentionSeconds   = int64((30 * 24 * time.Hour) / time.Second)
	routeBindingTouchInterval      = int64(time.Hour / time.Second)
	defaultRouteBindingCleanupSize = 256
	maxRouteBindingCleanupSize     = 1024
)

var (
	ErrUserEmailExists             = errors.New("user email already exists")
	ErrRegistrationClosed          = errors.New("user registration is disabled")
	ErrUserGroupNotFound           = errors.New("user group not found")
	ErrEgressInUse                 = errors.New("egress is in use")
	ErrTargetInUse                 = errors.New("routing target is in use")
	ErrInvalidProviderModelMapping = errors.New("invalid provider model mapping")
	ErrInvalidProviderRoute        = errors.New("invalid provider route")
)

type Store struct {
	path   string
	driver string
	// db is the single-connection WRITE pool. SQLite permits only one writer at a
	// time, so funneling every write through one connection means our own
	// concurrency never collides into SQLITE_BUSY. Init() sets WAL/synchronous on it.
	db *sql.DB
	// rdb is the multi-connection READ pool against the same WAL database. Under WAL
	// readers run concurrently with each other and with the single writer, each on
	// the latest committed snapshot, so the per-request account-selection / token /
	// group SELECTs no longer serialize behind the writer. For an in-memory test DB
	// it is the same handle as db (see Open).
	rdb ReadQuerier
	// tokenKey, when set (32 bytes), enables transparent AES-256-GCM encryption of the
	// secret columns in account_auth_tokens (access/refresh/id token, upstream api key,
	// and Agent Identity runtime/private/task credentials),
	// api_keys.secret (copyable downstream key), and session cookies. nil = encryption
	// disabled (plaintext, legacy behavior) — kept nil by tests/in-memory stores so
	// they are unaffected. Set via SetTokenEncryptionKey from main using the resolved
	// deployment identity secret.
	tokenKey     []byte
	tokenKeys    [][]byte
	cryptoStrict bool
	cryptoErrMu  sync.Mutex
	cryptoErr    error
	// undecryptable names the accounts whose stored secrets could not be opened with
	// the current key. cryptoErr keeps only the first error and no identity, which is
	// enough to gate a migration but not to fix anything: after a key rotation an
	// operator otherwise sees one generic failure while N accounts silently present
	// empty credentials and fail at routing time. Bounded so a fully unreadable
	// database cannot grow it without limit.
	undecryptableMu sync.Mutex
	undecryptable   map[string]int

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

// ReadQuerier is the common read surface implemented by sql.DB, sql.Conn, and
// sql.Tx. Diagnostic exports use it to bind an ordinary Store view to one
// repeatable snapshot without duplicating the store's row decoding logic.
type ReadQuerier interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

type contextExecer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// removeUnusedLegacySeedProviders cleans up the two example rows emitted by
// older installers. Every historical default field must still match, the row
// must never have been updated, and no account may reference it. This keeps
// operator-edited or active providers intact while making upgrades converge to
// the same empty provider list as a fresh installation.
func removeUnusedLegacySeedProviders(ctx context.Context, exec contextExecer) error {
	legacy := []struct {
		id, name, baseURL, models string
	}{
		{"deepseek", "DeepSeek", "https://api.deepseek.com/v1", `["deepseek-chat","deepseek-reasoner"]`},
		{"siliconflow", "SiliconFlow 硅基流动", "https://api.siliconflow.cn/v1", `[]`},
	}
	for _, provider := range legacy {
		if _, err := exec.ExecContext(ctx, `
DELETE FROM custom_providers
WHERE id=? AND name=? AND base_url=?
  AND upstream_protocol='chat_completions'
  AND transport_profile='generic' AND egress_ids='[]'
  AND enabled=1 AND auto_discover_models=1 AND models_json=?
  AND created_at=updated_at
  AND NOT EXISTS (SELECT 1 FROM accounts WHERE LOWER(provider)=LOWER(?))`,
			provider.id, provider.name, provider.baseURL, provider.models, provider.id); err != nil {
			return err
		}
	}
	return nil
}

type Group struct {
	Name                          string   `json:"name"`
	SystemPrompt                  string   `json:"system_prompt"`
	PromptMode                    string   `json:"prompt_mode"`
	SystemPromptApplyToCompaction bool     `json:"system_prompt_apply_to_compaction"`
	Virtual2MEnabled              bool     `json:"-"`
	ModelInstructionsEnabled      bool     `json:"model_instructions_enabled"`
	ModelInstructionsFiles        []string `json:"model_instructions_files,omitempty"`
	// ModelInstructionProfiles is populated only when a UserGroup policy is carried
	// through the request context. Account-pool groups do not persist or execute it.
	ModelInstructionProfiles            ModelInstructionProfiles `json:"model_instruction_profiles,omitempty"`
	SuperInstructEnabled                bool                     `json:"super_instruct_enabled"`
	SuperInstructSkillIDs               []string                 `json:"super_instruct_skill_ids,omitempty"`
	SuperInstructProfiles               SuperInstructProfiles    `json:"super_instruct_profiles,omitempty"`
	SuperInstructResponseRewriteEnabled bool                     `json:"super_instruct_response_rewrite_enabled"`
	SuperInstructMemoryEnabled          bool                     `json:"super_instruct_memory_enabled"`
	SuperInstructMonitorEnabled         bool                     `json:"super_instruct_monitor_enabled"`
	// Legacy policy fields are retained for one compatibility cycle only. Runtime
	// request policy comes exclusively from UserGroup and new account-pool group
	// writes reject non-empty policy values.
	ForceModel  string `json:"force_model"`
	ForceEffort string `json:"force_effort"`
	// DefaultEgressID is the deprecated alias. EgressIDs contains at most the one
	// group-owned inference outlet; legacy standby entries are ignored on read.
	DefaultEgressID string   `json:"default_egress_id"`
	EgressIDs       []string `json:"egress_ids"`
	// egressIDsConfigured distinguishes an explicitly saved [] from a legacy row
	// whose egress_ids column has not been populated yet.
	egressIDsConfigured bool
	CreatedAt           int64 `json:"created_at"`
	UpdatedAt           int64 `json:"updated_at"`
}

const (
	ModelInstructionFamilyGPT    = "gpt"
	ModelInstructionFamilyClaude = "claude"
	ModelInstructionFamilyGemini = "gemini"
)

type ModelInstructionProfile struct {
	Enabled bool     `json:"enabled"`
	Files   []string `json:"files,omitempty"`
}

// ModelInstructionProfiles stores model-family policy for a user group. A
// completely empty map means the legacy global fields remain authoritative.
// Once any profile exists, unlisted families are intentionally disabled.
type ModelInstructionProfiles map[string]ModelInstructionProfile

type SuperInstructProfile struct {
	Enabled                bool     `json:"enabled"`
	SkillIDs               []string `json:"skill_ids,omitempty"`
	ResponseRewriteEnabled bool     `json:"response_rewrite_enabled"`
	MemoryEnabled          bool     `json:"memory_enabled"`
	MonitorEnabled         bool     `json:"monitor_enabled"`
	// StreamRewriteFrontWindowSeconds and StreamRewriteFrontWindowBytes bound how
	// long a rewrite-enabled SSE may be held for M3 before it degrades to raw
	// passthrough. 0 = package default (20s / 256KiB). A protocol terminal frame
	// (response.completed / [DONE] / message_stop) still ends the packet early,
	// so the window only caps in-flight agentic turns that never produce one.
	StreamRewriteFrontWindowSeconds int64 `json:"stream_rewrite_front_window_seconds,omitempty"`
	StreamRewriteFrontWindowBytes   int64 `json:"stream_rewrite_front_window_bytes,omitempty"`
}

// SuperInstructProfiles stores per-model-family Super-Instruct policy. Empty
// means legacy global fields remain authoritative. Once any profile exists,
// unlisted or disabled families do not load Super-Instruct instructions or
// response submodules.
type SuperInstructProfiles map[string]SuperInstructProfile

// TrafficFallbackGroups is the ordered set of user groups that may receive
// replay-safe traffic after every compatible target in the current user group
// has been exhausted. Order is significant and is preserved per model family.
type TrafficFallbackGroups struct {
	GPT    []string `json:"gpt"`
	Claude []string `json:"claude"`
	Gemini []string `json:"gemini"`
}

// TrafficFallbackModelMapping rewrites one logical source model when traffic
// moves to a fallback user group. SourceModel supports the same exact / trailing
// wildcard syntax as ModelRoutingRule (for example "gpt-5.*" or "*").
type TrafficFallbackModelMapping struct {
	Family            string `json:"family"`
	SourceModel       string `json:"source_model"`
	TargetUserGroupID string `json:"target_user_group_id"`
	TargetModel       string `json:"target_model"`
}

type GroupAccountCounts struct {
	AccountCount       int `json:"account_count"`
	ActiveAccountCount int `json:"active_account_count"`
}

// UserGroup is a user-facing routing group that an APIKey points to.
// Prompt injection (SystemPrompt/PromptMode) and ForceModel/ForceEffort live
// ONLY here; the base groups (accounts.group_name) carry no prompt config.
// A user group can fan out to multiple targets (base groups, relays, kiro, antigravity)
// via UserGroupTarget rows, enabling affinity-spread and session-sticky multi-target routing.
type UserGroup struct {
	ID                                  string                   `json:"id"`
	Name                                string                   `json:"name"`
	SystemPrompt                        string                   `json:"system_prompt"`
	PromptMode                          string                   `json:"prompt_mode"`
	SystemPromptApplyToCompaction       bool                     `json:"system_prompt_apply_to_compaction"`
	ModelInstructionsEnabled            bool                     `json:"model_instructions_enabled"`
	ModelInstructionsFiles              []string                 `json:"model_instructions_files,omitempty"`
	ModelInstructionProfiles            ModelInstructionProfiles `json:"model_instruction_profiles,omitempty"`
	SuperInstructEnabled                bool                     `json:"super_instruct_enabled"`
	SuperInstructSkillIDs               []string                 `json:"super_instruct_skill_ids,omitempty"`
	SuperInstructProfiles               SuperInstructProfiles    `json:"super_instruct_profiles,omitempty"`
	SuperInstructResponseRewriteEnabled bool                     `json:"super_instruct_response_rewrite_enabled"`
	SuperInstructMemoryEnabled          bool                     `json:"super_instruct_memory_enabled"`
	SuperInstructMonitorEnabled         bool                     `json:"super_instruct_monitor_enabled"`
	ForceModel                          string                   `json:"force_model"`
	ForceEffort                         string                   `json:"force_effort"`
	// BlockClaudeTargetGroups and BlockGPTTargetGroups are user-group routing
	// policy. Each value names an account-pool target selected by this user
	// group; the underlying account-pool group itself remains unchanged and may
	// still receive that family through another user group.
	BlockClaudeTargetGroups      []string                      `json:"block_claude_target_groups"`
	BlockGPTTargetGroups         []string                      `json:"block_gpt_target_groups"`
	TrafficFallbackGroups        TrafficFallbackGroups         `json:"traffic_fallback_groups"`
	TrafficFallbackModelMappings []TrafficFallbackModelMapping `json:"traffic_fallback_model_mappings"`
	Targets                      []TargetRef                   `json:"targets"`
	ModelRouting                 []ModelRoutingRule            `json:"model_routing"`
	CreatedAt                    int64                         `json:"created_at"`
	UpdatedAt                    int64                         `json:"updated_at"`
}

const (
	TargetKindAccountPoolGroup = "account_pool_group"
	TargetKindModelProvider    = "model_provider"
)

// TargetRef is the canonical public reference used by user-group targets and
// model-routing tiers. UnmarshalJSON also accepts the legacy
// {target_type,target_ref} representation.
type TargetRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// TargetRefWithLegacyID keeps the canonical target response while exposing the
// row identifier required by the deprecated numeric target DELETE endpoint.
type TargetRefWithLegacyID struct {
	TargetRef
	LegacyID int64 `json:"legacy_id"`
}

func (t *TargetRef) UnmarshalJSON(raw []byte) error {
	var value struct {
		Kind       string          `json:"kind"`
		ID         json.RawMessage `json:"id"`
		TargetType string          `json:"target_type"`
		TargetRef  string          `json:"target_ref"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	id := strings.TrimSpace(value.TargetRef)
	if len(value.ID) > 0 && string(value.ID) != "null" {
		var stringID string
		if err := json.Unmarshal(value.ID, &stringID); err == nil {
			id = strings.TrimSpace(stringID)
		} else if id == "" {
			return errors.New("target id must be a string")
		}
	}
	kind := strings.TrimSpace(value.Kind)
	if kind == "" {
		kind = strings.TrimSpace(value.TargetType)
	}
	normalized, err := NormalizeTargetRef(TargetRef{Kind: kind, ID: id})
	if err != nil {
		return err
	}
	*t = normalized
	return nil
}

func NormalizeTargetRef(target TargetRef) (TargetRef, error) {
	kind := strings.ToLower(strings.TrimSpace(target.Kind))
	id := strings.TrimSpace(target.ID)
	switch kind {
	case TargetKindAccountPoolGroup, UserGroupTargetTypeBaseGroup:
		kind = TargetKindAccountPoolGroup
	case TargetKindModelProvider, UserGroupTargetTypeRelay:
		kind = TargetKindModelProvider
	case UserGroupTargetTypeKiro, UserGroupTargetTypeAntigravity:
		if id == "" {
			id = kind
		}
		kind = TargetKindAccountPoolGroup
	case "codex", "claude":
		if id == "" {
			id = kind
		}
		kind = TargetKindModelProvider
	default:
		return TargetRef{}, fmt.Errorf("unsupported user group target kind %q", target.Kind)
	}
	if id == "" {
		return TargetRef{}, fmt.Errorf("target id required for %s", kind)
	}
	return TargetRef{Kind: kind, ID: id}, nil
}

func (t TargetRef) key() string { return t.Kind + "\x00" + t.ID }

type ModelRoutingRule struct {
	Model string        `json:"model"`
	Tiers [][]TargetRef `json:"tiers"`
}

type UserGroupTargetBinding struct {
	UserGroupID string    `json:"user_group_id"`
	AffinityKey string    `json:"affinity_key"`
	Model       string    `json:"model"`
	Target      TargetRef `json:"target"`
	CreatedAt   int64     `json:"created_at"`
	UpdatedAt   int64     `json:"updated_at"`
}

func encodeModelRouting(rules []ModelRoutingRule) string {
	if rules == nil {
		rules = []ModelRoutingRule{}
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func validModelInstructionFamily(family string) bool {
	switch family {
	case ModelInstructionFamilyGPT, ModelInstructionFamilyClaude, ModelInstructionFamilyGemini:
		return true
	default:
		return false
	}
}

func normalizeModelInstructionProfiles(profiles ModelInstructionProfiles) (ModelInstructionProfiles, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	out := make(ModelInstructionProfiles, len(profiles))
	for rawFamily, profile := range profiles {
		family := strings.ToLower(strings.TrimSpace(rawFamily))
		if !validModelInstructionFamily(family) {
			return nil, fmt.Errorf("unsupported model instruction family %q", rawFamily)
		}
		seen := make(map[string]struct{}, len(profile.Files))
		files := make([]string, 0, len(profile.Files))
		for _, rawFile := range profile.Files {
			file := strings.TrimSpace(rawFile)
			if file == "" {
				continue
			}
			if _, duplicate := seen[file]; duplicate {
				continue
			}
			seen[file] = struct{}{}
			files = append(files, file)
		}
		if profile.Enabled && len(files) == 0 {
			return nil, fmt.Errorf("model instruction profile %q is enabled but has no files", family)
		}
		out[family] = ModelInstructionProfile{Enabled: profile.Enabled, Files: files}
	}
	return out, nil
}

func encodeModelInstructionProfiles(profiles ModelInstructionProfiles) string {
	if len(profiles) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(profiles)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func decodeModelInstructionProfiles(raw string) ModelInstructionProfiles {
	var profiles ModelInstructionProfiles
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil || len(profiles) == 0 {
		return nil
	}
	normalized, err := normalizeModelInstructionProfiles(profiles)
	if err != nil {
		return nil
	}
	return normalized
}

func normalizeSuperInstructProfileFamily(family string) string {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case ModelInstructionFamilyGPT, "chatgpt", "codex", "openai":
		return ModelInstructionFamilyGPT
	case ModelInstructionFamilyClaude, "anthropic":
		return ModelInstructionFamilyClaude
	case ModelInstructionFamilyGemini, "google":
		return ModelInstructionFamilyGemini
	default:
		return ""
	}
}

func normalizeSuperInstructProfiles(profiles SuperInstructProfiles) (SuperInstructProfiles, error) {
	if len(profiles) == 0 {
		return nil, nil
	}
	out := make(SuperInstructProfiles, len(profiles))
	for rawFamily, profile := range profiles {
		family := normalizeSuperInstructProfileFamily(rawFamily)
		if family == "" {
			return nil, fmt.Errorf("unsupported Super-Instruct profile family %q", rawFamily)
		}
		ids, err := superinstruct.NormalizeSkillIDs(profile.SkillIDs)
		if err != nil {
			return nil, fmt.Errorf("Super-Instruct profile %q: %w", family, err)
		}
		if _, duplicate := out[family]; duplicate {
			return nil, fmt.Errorf("duplicate Super-Instruct profile family %q", family)
		}
		out[family] = SuperInstructProfile{
			Enabled:                         profile.Enabled,
			SkillIDs:                        ids,
			ResponseRewriteEnabled:          profile.ResponseRewriteEnabled,
			MemoryEnabled:                   profile.MemoryEnabled,
			MonitorEnabled:                  profile.MonitorEnabled,
			StreamRewriteFrontWindowSeconds: profile.StreamRewriteFrontWindowSeconds,
			StreamRewriteFrontWindowBytes:   profile.StreamRewriteFrontWindowBytes,
		}
	}
	return out, nil
}

func encodeSuperInstructProfiles(profiles SuperInstructProfiles) string {
	if len(profiles) == 0 {
		return "{}"
	}
	normalized, err := normalizeSuperInstructProfiles(profiles)
	if err != nil || len(normalized) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func decodeSuperInstructProfiles(raw string) SuperInstructProfiles {
	var profiles SuperInstructProfiles
	if err := json.Unmarshal([]byte(raw), &profiles); err != nil || len(profiles) == 0 {
		return nil
	}
	normalized, err := normalizeSuperInstructProfiles(profiles)
	if err != nil {
		return nil
	}
	return normalized
}

const (
	// UserGroupTargetTypeBaseGroup routes to a physical base group of accounts.
	UserGroupTargetTypeBaseGroup = "base_group"
	// UserGroupTargetTypeRelay routes to a custom relay provider.
	UserGroupTargetTypeRelay = "relay"
	// UserGroupTargetTypeKiro routes explicitly to the kiro built-in base group.
	UserGroupTargetTypeKiro = "kiro"
	// UserGroupTargetTypeAntigravity routes explicitly to the antigravity built-in base group.
	UserGroupTargetTypeAntigravity = "antigravity"
)

// UserGroupTarget is one routing target within a user group.
// Multiple targets with different affinity weights implement affinity-spread;
// the session-sticky layer re-uses the same target for a given route-key hash.
type UserGroupTarget struct {
	ID             int64  `json:"id"`
	UserGroupID    string `json:"user_group_id"`
	TargetType     string `json:"target_type"` // base_group | relay | kiro | antigravity
	TargetRef      string `json:"target_ref"`  // group name, provider id, or "" for built-ins
	AffinityWeight int    `json:"affinity_weight"`
	CreatedAt      int64  `json:"created_at"`
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
	Secret string `json:"secret,omitempty"`
	// UserGroupID, when set, overrides the legacy GroupName routing path: the key is
	// resolved via the two-layer group model (user_groups → user_group_targets → base
	// group/relay/kiro/antigravity). Empty for legacy keys that still use GroupName directly.
	UserGroupID string `json:"user_group_id,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
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
	Provider  string `json:"provider,omitempty"`
	Status    string `json:"status"`
	IsFedramp bool   `json:"is_fedramp"`
	// IgnoreRateLimitControls is an account-scoped operator override. It keeps
	// this account eligible despite its own rate-limit cooldown, recheck flag,
	// or quarantine. It never re-enables a disabled account or an unhealthy
	// shared egress profile.
	IgnoreRateLimitControls bool `json:"ignore_rate_limit_controls"`
	// ForceCodex429 is an account-scoped opt-in for the "强制卡429" mode. For
	// OpenAI OAuth Codex requests it injects a synthetic custom_tool_call pair
	// into the upstream input history, and after two explicit 429s within the
	// confirmation window keeps retrying on this same account instead of
	// failing over to a fresh one. No effect for API-key accounts or chat-bridge
	// entry points.
	ForceCodex429 bool `json:"force_codex_429"`
	// RoutingWeight affects only fresh-account selection. Sticky/native sessions
	// remain bound to their account. 100 is neutral; higher values receive a
	// proportionally larger share under sustained equal-capacity load.
	RoutingWeight int `json:"routing_weight"`
	// RetryMaxAttempts is the total number of same-credential wire attempts for a
	// replay-safe request (1 = no retry). Zero preserves the historical single
	// attempt default. Runtime clamps the value to at most three.
	RetryMaxAttempts int    `json:"retry_max_attempts"`
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
	Cursor      int `json:"cursor"`
	Other       int `json:"other"`
}

type AccountToken struct {
	AccountID          string `json:"account_id"`
	AuthMethod         string `json:"auth_method,omitempty"`
	CredentialMode     string `json:"credential_mode,omitempty"`
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	OpenAIAPIKey       string `json:"openai_api_key,omitempty"`
	IDTokenRaw         string `json:"id_token_raw,omitempty"`
	AgentRuntimeID     string `json:"-"`
	AgentPrivateKey    string `json:"-"`
	AgentTaskID        string `json:"-"`
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

// EmailAccount represents a Microsoft/Outlook email account used for protocol
// registration. Password, OAuth client identity, and refresh token are
// transparently encrypted at rest whenever the deployment storage key is active.
// Password and refresh token are never exposed in API list responses.
type EmailAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	Password     string `json:"-"`
	ClientID     string `json:"client_id,omitempty"`
	RefreshToken string `json:"-"`
	Status       string `json:"status"`
	GroupName    string `json:"group_name,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	LastUsedAt   int64  `json:"last_used_at,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

type ModelCapability struct {
	AccountID                     string `json:"account_id"`
	ModelSlug                     string `json:"model_slug"`
	AvailabilityState             string `json:"availability_state"`
	Context1MState                string `json:"context_1m_state"`
	Context1MSource               string `json:"context_1m_source,omitempty"`
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

type CodexCacheCapability struct {
	AccountID               string `json:"account_id"`
	Model                   string `json:"model"`
	ExplicitBreakpointState string `json:"explicit_breakpoint_state"`
	FirstWriteTokens        int64  `json:"first_write_tokens"`
	SecondReadTokens        int64  `json:"second_read_tokens"`
	ProbedAt                int64  `json:"probed_at"`
	UpdatedAt               int64  `json:"updated_at"`
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
	// Transport* fields are request-scoped overlays and are never persisted or
	// returned by the admin egress-profile API. They let an account keep its real
	// IP egress as the routing/health/CF identity while sending the wire request
	// through a separately bound curl_cffi sidecar.
	TransportSidecarID             string `json:"-"`
	TransportSidecarMaxConcurrency int    `json:"-"`
	TransportBaseType              string `json:"-"`
	TransportBaseURL               string `json:"-"`
	TransportBaseChain             string `json:"-"`
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
	// It is stored encrypted at rest like other versioned secrets.
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

const (
	EgressBindingScopeGroup   = "group"
	EgressBindingScopeAccount = "account"
)

type AccountEgressBinding struct {
	AccountID        string `json:"account_id"`
	PrimaryEgressID  string `json:"primary_egress_id"`
	StandbyEgressIDs string `json:"standby_egress_ids"`
	// BindingScope is "group" for the normal inherited route and "account" only
	// after an operator or import explicitly pins this account to its own outlet.
	BindingScope string `json:"binding_scope"`
	// SidecarEgressID is an optional transport wrapper. Primary/standby egresses
	// continue to own the exit IP, cooldown, concurrency and CF attribution; the
	// sidecar only supplies the TLS/HTTP2 fingerprint and chains through whichever
	// real egress the scheduler selected.
	SidecarEgressID string `json:"sidecar_egress_id,omitempty"`
	CookieJarKey    string `json:"cookie_jar_key"`
	CooldownUntil   int64  `json:"cooldown_until,omitempty"`
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
	// ExpiresAt is optional for backward compatibility. Zero means the binding has
	// no explicit expiry; positive values make both routing reads and resource
	// deletion guards ignore the row once its retention window has elapsed.
	ExpiresAt int64 `json:"expires_at,omitempty"`
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
	UsageExpected   bool   `json:"usage_expected"`
	UsageRecordedAt int64  `json:"usage_recorded_at"`
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
	ID                 string                `json:"id"`
	Name               string                `json:"name"`
	BaseURL            string                `json:"base_url"`
	UpstreamProtocol   string                `json:"upstream_protocol"`
	TransportProfile   string                `json:"transport_profile"`
	Routes             []CustomProviderRoute `json:"routes"`
	EgressIDs          []string              `json:"egress_ids"`
	Enabled            bool                  `json:"enabled"`
	AutoDiscoverModels bool                  `json:"auto_discover_models"`
	Models             []string              `json:"models"`
	// ModelMappings rewrites a downstream model name to the concrete name
	// accepted by this relay. Exact keys win; "*" is an optional provider-wide
	// fallback. The requested and resolved names remain separately attributable
	// in request diagnostics.
	ModelMappings          map[string]string `json:"model_mappings"`
	CreatedAt              int64             `json:"created_at"`
	UpdatedAt              int64             `json:"updated_at"`
	ResolvedRouteID        string            `json:"-"`
	ResolvedDownstreamPath string            `json:"-"`
}

// CustomProviderRoute overrides the legacy/default endpoint for one downstream
// entrypoint. Empty endpoint/protocol/profile values inherit the provider default
// on write, so old single-route clients and partial backup documents stay valid.
type CustomProviderRoute struct {
	ID               string `json:"id"`
	DownstreamPath   string `json:"downstream_path"`
	BaseURL          string `json:"base_url"`
	UpstreamProtocol string `json:"upstream_protocol"`
	TransportProfile string `json:"transport_profile"`
}

const (
	CustomProviderDownstreamChat      = "/v1/chat/completions"
	CustomProviderDownstreamResponses = "/v1/responses"
	CustomProviderDownstreamMessages  = "/v1/messages"
	CustomProviderDownstreamWildcard  = "*"
)

const (
	CustomProviderProtocolChatCompletions   = "chat_completions"
	CustomProviderProtocolResponses         = "responses"
	CustomProviderProtocolAnthropicMessages = "anthropic_messages"
)

const (
	CustomProviderTransportGeneric    = "generic"
	CustomProviderTransportCodexCLI   = "codex_cli"
	CustomProviderTransportClaudeCode = "claude_code"
)

func NormalizeCustomProviderTransportProfile(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", CustomProviderTransportGeneric:
		return CustomProviderTransportGeneric, true
	case CustomProviderTransportCodexCLI:
		return CustomProviderTransportCodexCLI, true
	case CustomProviderTransportClaudeCode:
		return CustomProviderTransportClaudeCode, true
	default:
		return "", false
	}
}

func NormalizeCustomProviderProtocol(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return CustomProviderProtocolChatCompletions, true
	case CustomProviderProtocolChatCompletions:
		return CustomProviderProtocolChatCompletions, true
	case CustomProviderProtocolResponses:
		return CustomProviderProtocolResponses, true
	case CustomProviderProtocolAnthropicMessages:
		return CustomProviderProtocolAnthropicMessages, true
	default:
		return "", false
	}
}

func NormalizeCustomProviderDownstreamPath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	switch strings.ToLower(raw) {
	case "chat", "chat_completions", "/chat/completions", CustomProviderDownstreamChat:
		return CustomProviderDownstreamChat, true
	case "responses", "/responses", CustomProviderDownstreamResponses:
		return CustomProviderDownstreamResponses, true
	case "messages", "anthropic_messages", "/messages", CustomProviderDownstreamMessages:
		return CustomProviderDownstreamMessages, true
	case "*", "passthrough":
		return CustomProviderDownstreamWildcard, true
	}
	if !strings.HasPrefix(raw, "/") || strings.ContainsAny(raw, "?#") {
		return "", false
	}
	cleaned := pathpkg.Clean(raw)
	if cleaned == "." || cleaned == "/" || strings.Contains(cleaned, "..") {
		return "", false
	}
	return cleaned, true
}

func customProviderRouteID(raw, downstreamPath string) (string, bool) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		switch downstreamPath {
		case CustomProviderDownstreamChat:
			raw = "chat"
		case CustomProviderDownstreamResponses:
			raw = "responses"
		case CustomProviderDownstreamMessages:
			raw = "messages"
		case CustomProviderDownstreamWildcard:
			raw = "passthrough"
		default:
			raw = "path-" + strings.Trim(strings.NewReplacer("/", "-", ".", "-", "_", "-").Replace(downstreamPath), "-")
		}
	}
	if raw == "" || len(raw) > 64 {
		return "", false
	}
	for _, r := range raw {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", false
		}
	}
	return raw, true
}

func canonicalCustomProviderRoutes(routes []CustomProviderRoute, provider CustomProvider, reject bool) ([]CustomProviderRoute, error) {
	out := make([]CustomProviderRoute, 0, len(routes))
	seenPaths := make(map[string]struct{}, len(routes))
	seenIDs := make(map[string]struct{}, len(routes))
	for index, route := range routes {
		downstreamPath, ok := NormalizeCustomProviderDownstreamPath(route.DownstreamPath)
		if !ok {
			if reject {
				return nil, fmt.Errorf("%w: route %d has an invalid downstream_path", ErrInvalidProviderRoute, index+1)
			}
			continue
		}
		id, ok := customProviderRouteID(route.ID, downstreamPath)
		if !ok {
			if reject {
				return nil, fmt.Errorf("%w: route %d has an invalid id", ErrInvalidProviderRoute, index+1)
			}
			continue
		}
		if _, duplicate := seenPaths[downstreamPath]; duplicate {
			if reject {
				return nil, fmt.Errorf("%w: downstream_path %q is duplicated", ErrInvalidProviderRoute, downstreamPath)
			}
			continue
		}
		if _, duplicate := seenIDs[id]; duplicate {
			if reject {
				return nil, fmt.Errorf("%w: route id %q is duplicated", ErrInvalidProviderRoute, id)
			}
			continue
		}
		baseURL := strings.TrimSpace(route.BaseURL)
		if baseURL == "" {
			baseURL = strings.TrimSpace(provider.BaseURL)
		}
		protocol, ok := NormalizeCustomProviderProtocol(route.UpstreamProtocol)
		if strings.TrimSpace(route.UpstreamProtocol) == "" {
			protocol, ok = NormalizeCustomProviderProtocol(provider.UpstreamProtocol)
		}
		if !ok {
			if reject {
				return nil, fmt.Errorf("%w: route %q has an invalid upstream_protocol", ErrInvalidProviderRoute, id)
			}
			continue
		}
		profile, ok := NormalizeCustomProviderTransportProfile(route.TransportProfile)
		if strings.TrimSpace(route.TransportProfile) == "" {
			profile, ok = NormalizeCustomProviderTransportProfile(provider.TransportProfile)
		}
		if !ok || baseURL == "" {
			if reject {
				return nil, fmt.Errorf("%w: route %q requires a base_url and valid transport_profile", ErrInvalidProviderRoute, id)
			}
			continue
		}
		seenPaths[downstreamPath] = struct{}{}
		seenIDs[id] = struct{}{}
		out = append(out, CustomProviderRoute{
			ID: id, DownstreamPath: downstreamPath, BaseURL: baseURL,
			UpstreamProtocol: protocol, TransportProfile: profile,
		})
	}
	return out, nil
}

// ResolveCustomProviderRoute returns an effective provider for an incoming
// downstream path. Exact entries win, then "*", then the legacy fields.
func ResolveCustomProviderRoute(provider CustomProvider, downstreamPath string) (CustomProvider, string) {
	normalized, ok := NormalizeCustomProviderDownstreamPath(downstreamPath)
	if !ok {
		normalized = CustomProviderDownstreamWildcard
	}
	var selected *CustomProviderRoute
	for i := range provider.Routes {
		if provider.Routes[i].DownstreamPath == normalized {
			selected = &provider.Routes[i]
			break
		}
		if provider.Routes[i].DownstreamPath == CustomProviderDownstreamWildcard {
			selected = &provider.Routes[i]
		}
	}
	if selected == nil {
		provider.ResolvedRouteID = "default"
		switch normalized {
		case CustomProviderDownstreamChat, CustomProviderDownstreamResponses, CustomProviderDownstreamMessages:
			provider.ResolvedDownstreamPath = normalized
		default:
			provider.ResolvedDownstreamPath = CustomProviderDownstreamWildcard
		}
		return provider, provider.ResolvedRouteID
	}
	provider.BaseURL = selected.BaseURL
	provider.UpstreamProtocol = selected.UpstreamProtocol
	provider.TransportProfile = selected.TransportProfile
	provider.ResolvedRouteID = selected.ID
	provider.ResolvedDownstreamPath = selected.DownstreamPath
	return provider, selected.ID
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
	return &Store{path: path, driver: "sqlite", db: db, rdb: rdb}, nil
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
	if rdb, ok := s.rdb.(*sql.DB); ok && rdb != s.db {
		_ = rdb.Close()
	}
	return s.db.Close()
}

func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *Store) InMemory() bool {
	return s == nil || strings.Contains(s.path, "mode=memory") || s.path == ":memory:"
}

// ReadDB exposes the WAL read pool for read-only streaming operations such as
// diagnostics. Callers must never execute mutations through this handle.
func (s *Store) ReadDB() ReadQuerier {
	return s.rdb
}

func Now() int64 {
	return time.Now().Unix()
}

func (s *Store) Init(ctx context.Context) error {
	return s.init(ctx, nil)
}

// InitWithProgress is Init with phase notifications for startup supervisors.
// Notifications are emitted immediately before each bounded schema phase so a
// stalled upgrade leaves an actionable last-known phase in the service journal.
func (s *Store) InitWithProgress(ctx context.Context, progress func(string)) error {
	return s.init(ctx, progress)
}

func (s *Store) init(ctx context.Context, progress func(string)) error {
	report := func(phase string) {
		if progress != nil {
			progress(phase)
		}
	}
	if s.driver == "postgres" {
		report("postgres_schema")
		return s.initPostgres(ctx)
	}
	report("sqlite_pragmas")
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA cache_size=-16384; PRAGMA mmap_size=67108864; PRAGMA temp_store=MEMORY;`); err != nil {
		return err
	}
	report("base_schema")
	if _, err := s.db.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	// goalContinuitySchemaSQL is deliberately additive and runs independently of the
	// legacy context_journal migration.  Existing installations start dual-writing v2
	// rows without a risky rewrite of historical snapshots.
	report("goal_schema")
	if _, err := s.db.ExecContext(ctx, goalContinuitySchemaSQL); err != nil {
		return err
	}
	// Codex session mappings are independent from the legacy goal/context replay
	// store. They contain encrypted identity metadata plus HMAC aliases only, so an
	// upgrade can begin exact native previous_response_id routing without rewriting
	// any historical prompt bodies.
	report("codex_session_schema")
	if _, err := s.db.ExecContext(ctx, codexSessionMappingSchemaSQL); err != nil {
		return err
	}
	// Quota window estimation samples (used_percent + recorded cost per cycle) are
	// additive; raw samples are pruned after cycle finalization.
	report("quota_window_schema")
	if _, err := s.db.ExecContext(ctx, quotaWindowSchemaSQL); err != nil {
		return err
	}
	// Create lifecycle management tables
	report("runtime_schemas")
	if _, err := s.db.ExecContext(ctx, lifecycleSchemaSQL); err != nil {
		return err
	}
	// Team-member rotation is an additive durable workflow. It is isolated from
	// registration_workflow_items so upgrades do not reinterpret in-flight signup
	// records, and all credential fields remain references to account_auth_tokens.
	if _, err := s.db.ExecContext(ctx, teamManagementSchemaSQL); err != nil {
		return err
	}
	// Mailbox health is kept outside provider_settings so frequent probes never
	// rewrite encrypted provider credentials or invalidate provider snapshots.
	if _, err := s.db.ExecContext(ctx, mailboxProfileSchemaSQL); err != nil {
		return err
	}
	report("additive_schema_migrations")
	if err := s.migrate(ctx); err != nil {
		return err
	}
	report("usage_rollup_schema")
	if err := s.initUsageHourlyRollups(ctx); err != nil {
		return err
	}
	report("goal_storage_accounting")
	if err := s.migrateGoalContinuityV2(ctx); err != nil {
		return err
	}
	// Historical usage rollups and legacy billing-event repair are intentionally
	// deferred until an active listener exists. Their schemas and current-write
	// triggers are ready here, so no new request loses data.
	report("small_data_migrations")
	// New previous_response_id aliases live in affinity_aliases. Legacy rows remain
	// readable for one compatibility window and are then removed in small batches.
	if _, err := s.db.ExecContext(ctx, `UPDATE affinity_bindings SET expires_at=updated_at+? WHERE source='previous_response_id' AND expires_at=0`, int64((7*24*time.Hour)/time.Second)); err != nil {
		return err
	}
	if err := s.migrateAccountRateLimits(ctx); err != nil {
		return err
	}
	if err := s.migrateLifecycle(ctx); err != nil {
		return err
	}
	report("runtime_settings")
	now := Now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('usage_accuracy_cutover_at',?,?) ON CONFLICT(key) DO NOTHING`, strconv.FormatInt(now, 10), now); err != nil {
		return err
	}
	if err := s.migrateContextJournalTTL(ctx, now); err != nil {
		return err
	}
	if err := s.migrateCodexHTTPStateless(ctx, now); err != nil {
		return err
	}
	if err := s.migrateCodexNativeCacheDefault(ctx, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO groups(name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, created_at, updated_at)
VALUES
  ('cyber', '', 'prepend', 1, 0, ?, ?),
  ('kiro', '', 'prepend', 0, 0, ?, ?),
  ('antigravity', '', 'prepend', 0, 0, ?, ?)
ON CONFLICT(name) DO NOTHING`, now, now, now, now, now, now); err != nil {
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
	if err := removeUnusedLegacySeedProviders(ctx, s.db); err != nil {
		return err
	}
	report("egress_bindings")
	if _, err := s.RepairMissingAccountEgressBindings(ctx); err != nil {
		return err
	}
	report("complete")
	return nil
}

func (s *Store) repairLegacyUsageEvents(ctx context.Context) error {
	const insertBatch = 500
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// The explicit non-empty predicates match the partial indexes. Without them,
		// SQLite scans the full usage/event history once per hold even though equality
		// logically implies a non-empty value; the reported 51k x 51k upgrade then
		// remains pre-listener for minutes.
		result, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_events(event_id, hold_id, account_id, route_key_hash, route_epoch, estimated_tokens, usage_state, terminal_status, usage_recorded_at, settled_at, created_at, updated_at)
SELECT 'legacy_hold:'||h.id, h.id, h.account_id, h.route_key_hash,
 COALESCE((SELECT MAX(route_epoch) FROM usage_records u WHERE u.billing_hold_id<>'' AND u.billing_hold_id=h.id),0), h.estimated_tokens,
 CASE WHEN EXISTS(SELECT 1 FROM usage_records u WHERE u.billing_hold_id<>'' AND u.billing_hold_id=h.id AND u.estimated=0) THEN 'real'
      WHEN EXISTS(SELECT 1 FROM usage_records u WHERE u.billing_hold_id<>'' AND u.billing_hold_id=h.id) THEN 'estimated' ELSE 'pending' END,
 CASE WHEN h.status='held' THEN '' ELSE h.status END, h.usage_recorded_at,
 CASE WHEN h.status='held' THEN 0 ELSE h.updated_at END, h.created_at, h.updated_at
FROM billing_holds h
WHERE NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.hold_id<>'' AND e.hold_id=h.id)
  AND NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.event_id='legacy_hold:'||h.id)
ORDER BY h.id LIMIT ?`, insertBatch)
		inserted := int64(0)
		if execErr == nil {
			inserted, execErr = result.RowsAffected()
		}
		if execErr == nil {
			execErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if execErr != nil {
			return execErr
		}
		if inserted == 0 {
			break
		}
		if err := yieldDeferredStorageWriter(ctx); err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		result, execErr := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_events(event_id, account_id, route_key_hash, route_epoch, usage_state, usage_recorded_at, created_at, updated_at)
SELECT usage_event_id, account_id, route_key_hash, route_epoch, CASE WHEN estimated=0 THEN 'real' ELSE 'estimated' END, created_at, created_at, created_at
FROM usage_records u WHERE usage_event_id<>'' AND NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.event_id=u.usage_event_id)
ORDER BY u.id LIMIT ?`, insertBatch)
		inserted := int64(0)
		if execErr == nil {
			inserted, execErr = result.RowsAffected()
		}
		if execErr == nil {
			execErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if execErr != nil {
			return execErr
		}
		if inserted == 0 {
			break
		}
		if err := yieldDeferredStorageWriter(ctx); err != nil {
			return err
		}
	}

	// These terminal states represent a failed/retried attempt, not missing billing.
	// The example diagnostic snapshot had 46 such false pending rows at its manifest
	// boundary (49 by the later CSV boundary); retain true usage rows when present.
	if _, err := s.db.ExecContext(ctx, `UPDATE billing_holds SET usage_expected=0
WHERE usage_recorded_at=0 AND status IN ('mapped_session_risk_rotating','stream_mapped_session_risk_rotating','stream_probe_failed','stream_interrupted_compensated')`); err != nil {
		return err
	}

	const recoveryBatch = 100
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.rdb.QueryContext(ctx, `SELECT h.id FROM billing_holds h JOIN usage_events e ON e.hold_id=h.id
WHERE h.usage_recorded_at=0 AND h.estimated_tokens>0 AND h.status IN ('settled','settled_streaming','success_body_rule') ORDER BY h.id LIMIT ?`, recoveryBatch)
		if err != nil {
			return err
		}
		holdIDs := make([]string, 0, recoveryBatch)
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			holdIDs = append(holdIDs, id)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(holdIDs) == 0 {
			return nil
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, holdID := range holdIDs {
			if err = s.recoverEstimatedUsageForHold(ctx, tx, holdID, Now()); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if err = yieldDeferredStorageWriter(ctx); err != nil {
			return err
		}
	}
}

func yieldDeferredStorageWriter(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Millisecond):
		return nil
	}
}

const legacyUsageEventsRepairMarker = "legacy_usage_events_repair_v1"

func (s *Store) repairLegacyUsageEventsOnce(ctx context.Context) error {
	var completed int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, legacyUsageEventsRepairMarker).Scan(&completed); err != nil {
		return err
	}
	if completed > 0 {
		return nil
	}
	if err := s.repairLegacyUsageEvents(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, legacyUsageEventsRepairMarker, "1", Now())
	return err
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

// migrateCodexHTTPStateless updates the one previously shipped stable-profile
// combination that enabled strict HTTP continuation. New HTTP requests then discard
// account-local response state before routing, while WebSocket connections may keep
// using mapping. The marker preserves any operator choice made after this upgrade.
func (s *Store) migrateCodexHTTPStateless(ctx context.Context, now int64) error {
	const marker = "codex_http_stateless_v1_migrated"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, marker).Scan(&migrated); err != nil {
		return err
	}
	if migrated > 0 {
		return nil
	}
	var mapping, stateless string
	if err := tx.QueryRowContext(ctx, `SELECT
COALESCE((SELECT lower(trim(value)) FROM settings WHERE key='codex_session_mapping_enabled'),''),
COALESCE((SELECT lower(trim(value)) FROM settings WHERE key='codex_stateless_passthrough'),'')`).Scan(&mapping, &stateless); err != nil {
		return err
	}
	markerValue := "observed"
	if mapping == "true" && stateless == "false" {
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value='true',updated_at=? WHERE key='codex_stateless_passthrough'`, now); err != nil {
			return err
		}
		markerValue = "forced"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)`, marker, markerValue, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSettingsCache()
	return nil
}

// migrateCodexNativeCacheDefault repairs only the stateless value that the v1
// migration itself forced. Explicit operator choices remain untouched. Older v1
// builds stored marker value "1", so matching updated_at values identify their
// atomic auto-update without guessing from the boolean alone.
func (s *Store) migrateCodexNativeCacheDefault(ctx context.Context, now int64) error {
	const marker = "codex_native_cache_default_v2_migrated"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, marker).Scan(&migrated); err != nil {
		return err
	}
	if migrated > 0 {
		return nil
	}
	var v1Value, stateless string
	var v1Updated, statelessUpdated int64
	if err := tx.QueryRowContext(ctx, `SELECT
COALESCE((SELECT lower(trim(value)) FROM settings WHERE key='codex_http_stateless_v1_migrated'),''),
COALESCE((SELECT updated_at FROM settings WHERE key='codex_http_stateless_v1_migrated'),0),
COALESCE((SELECT lower(trim(value)) FROM settings WHERE key='codex_stateless_passthrough'),''),
COALESCE((SELECT updated_at FROM settings WHERE key='codex_stateless_passthrough'),0)`).Scan(&v1Value, &v1Updated, &stateless, &statelessUpdated); err != nil {
		return err
	}
	autoForced := v1Value == "forced" || v1Value == "1" && v1Updated > 0 && v1Updated == statelessUpdated
	if autoForced && stateless == "true" {
		if _, err := tx.ExecContext(ctx, `UPDATE settings SET value='false',updated_at=? WHERE key='codex_stateless_passthrough'`, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?)`, marker, "1", now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.InvalidateSettingsCache()
	return nil
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
  egress_ids TEXT NOT NULL DEFAULT '',
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
  ignore_rate_limit_controls INTEGER NOT NULL DEFAULT 0,
  force_codex_429 INTEGER NOT NULL DEFAULT 0,
  quarantine_until INTEGER NOT NULL DEFAULT 0,
  quarantine_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_group_status ON accounts(group_name, status);
CREATE TABLE IF NOT EXISTS account_auth_tokens(
  account_id TEXT PRIMARY KEY,
  auth_method TEXT NOT NULL DEFAULT '',
  credential_mode TEXT NOT NULL DEFAULT '',
  access_token TEXT,
  refresh_token TEXT,
  openai_api_key TEXT,
  id_token_raw TEXT,
  agent_runtime_id TEXT NOT NULL DEFAULT '',
  agent_private_key TEXT NOT NULL DEFAULT '',
  agent_task_id TEXT NOT NULL DEFAULT '',
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
  availability_state TEXT NOT NULL DEFAULT 'unverified',
  context_1m_state TEXT NOT NULL DEFAULT 'unknown',
  context_1m_source TEXT NOT NULL DEFAULT '',
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
CREATE TABLE IF NOT EXISTS account_model_catalog_status(
  account_id TEXT PRIMARY KEY,
  authoritative INTEGER NOT NULL DEFAULT 0,
  last_probe_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
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
	expires_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS affinity_aliases(
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
  expires_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_affinity_aliases_expiry ON affinity_aliases(expires_at, updated_at);
CREATE INDEX IF NOT EXISTS idx_affinity_aliases_account ON affinity_aliases(account_id, updated_at);
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
  sidecar_egress_id TEXT NOT NULL DEFAULT '',
  binding_scope TEXT NOT NULL DEFAULT 'group',
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
	usage_expected INTEGER NOT NULL DEFAULT 0,
	usage_recorded_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_billing_holds_status_updated ON billing_holds(status, updated_at);
CREATE TABLE IF NOT EXISTS usage_events(
  event_id TEXT PRIMARY KEY,
  hold_id TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  route_key_hash TEXT NOT NULL DEFAULT '',
  route_epoch INTEGER NOT NULL DEFAULT 0,
  estimated_tokens INTEGER NOT NULL DEFAULT 0,
  usage_state TEXT NOT NULL DEFAULT 'pending',
  terminal_status TEXT NOT NULL DEFAULT '',
  usage_recorded_at INTEGER NOT NULL DEFAULT 0,
  settled_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_hold_id ON usage_events(hold_id) WHERE hold_id <> '';
CREATE INDEX IF NOT EXISTS idx_usage_events_state_updated ON usage_events(usage_state, updated_at);
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
	kiro_credits REAL NOT NULL DEFAULT 0,
	kiro_credits_present INTEGER NOT NULL DEFAULT 0,
	billing_hold_id TEXT NOT NULL DEFAULT '',
	requested_model TEXT NOT NULL DEFAULT '',
	resolved_model TEXT NOT NULL DEFAULT '',
	model_override_source TEXT NOT NULL DEFAULT 'none',
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
  transport_profile TEXT NOT NULL DEFAULT 'generic',
  egress_ids TEXT NOT NULL DEFAULT '[]',
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

CREATE TABLE IF NOT EXISTS sms_country_price_snapshots(
  provider TEXT NOT NULL,
  service TEXT NOT NULL DEFAULT 'dr',
  country_id TEXT NOT NULL,
  country_iso TEXT NOT NULL DEFAULT '',
  country_name TEXT NOT NULL DEFAULT '',
  price REAL NOT NULL DEFAULT 0,
  inventory INTEGER NOT NULL DEFAULT 0,
  provider_rank INTEGER NOT NULL DEFAULT 9999,
  balance REAL NOT NULL DEFAULT -1,
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY(provider, service, country_id)
);
CREATE INDEX IF NOT EXISTS idx_sms_country_prices_fresh ON sms_country_price_snapshots(fetched_at, provider, country_iso);

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

CREATE TABLE IF NOT EXISTS email_pool(
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL,
  password TEXT NOT NULL DEFAULT '',
  client_id TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'idle',
  group_name TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  last_used_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS turbo_gpt_register_jobs(
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL DEFAULT 'pending',
  phase TEXT NOT NULL DEFAULT 'phase1',
  phone TEXT NOT NULL DEFAULT '',
  email TEXT NOT NULL DEFAULT '',
  password TEXT NOT NULL DEFAULT '',
  full_name TEXT NOT NULL DEFAULT '',
  birth_date TEXT NOT NULL DEFAULT '',
  phone_country_code TEXT NOT NULL DEFAULT '',
  phone_country_dial_code TEXT NOT NULL DEFAULT '',
  sms_platform TEXT NOT NULL DEFAULT '',
  sms_operator TEXT NOT NULL DEFAULT '',
  sms_activation_id TEXT NOT NULL DEFAULT '',
  mail_domain TEXT NOT NULL DEFAULT '',
  config_json TEXT NOT NULL DEFAULT '{}',
  result_json TEXT NOT NULL DEFAULT '{}',
  error TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  auto_import INTEGER NOT NULL DEFAULT 1,
  imported_account_id TEXT NOT NULL DEFAULT '',
  phase1_completed_at INTEGER NOT NULL DEFAULT 0,
  phase2_completed_at INTEGER NOT NULL DEFAULT 0,
  phase3_completed_at INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_turbo_gpt_register_jobs_status ON turbo_gpt_register_jobs(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_turbo_gpt_register_jobs_email ON turbo_gpt_register_jobs(email);

CREATE TABLE IF NOT EXISTS turbo_gpt_register_tokens(
  job_id TEXT PRIMARY KEY,
  email TEXT NOT NULL DEFAULT '',
  access_token TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  id_token TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  raw_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(job_id) REFERENCES turbo_gpt_register_jobs(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS turbo_gpt_register_config(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL
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
		// Some pre-release installations created email_pool before all management
		// fields existed. Add columns before creating its composite index so startup
		// can repair those databases instead of failing while executing schemaSQL.
		`ALTER TABLE email_pool ADD COLUMN password TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email_pool ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email_pool ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email_pool ADD COLUMN status TEXT NOT NULL DEFAULT 'idle'`,
		`ALTER TABLE email_pool ADD COLUMN group_name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email_pool ADD COLUMN error_message TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE email_pool ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE email_pool ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE email_pool ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0`,
		`UPDATE email_pool SET status=CASE
		  WHEN lower(trim(COALESCE(status,''))) IN ('','idle','ready','available','unused','active','valid') THEN 'idle'
		  WHEN replace(lower(trim(status)),'-','_') IN ('in_use','inuse','busy','reserved','using','processing') THEN 'in_use'
		  WHEN lower(trim(status)) IN ('error','failed','invalid','disabled','dead') THEN 'error'
		  WHEN lower(trim(status)) IN ('used','consumed','done','completed') THEN 'used'
		  ELSE lower(trim(status)) END`,
		`CREATE INDEX IF NOT EXISTS idx_email_pool_status ON email_pool(status, group_name)`,
		// Team mailbox policy is additive. teamManagementSchemaSQL has an immutable
		// PostgreSQL checksum, so released databases receive these fields here.
		`ALTER TABLE team_workspaces ADD COLUMN mailbox_provider_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE team_workspaces ADD COLUMN required_email_domain TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE team_workspaces ADD COLUMN same_domain_required INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE team_lifecycle_workflows ADD COLUMN mailbox_provider_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE team_lifecycle_workflows ADD COLUMN required_email_domain TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_team_workspaces_mailbox_provider ON team_workspaces(mailbox_provider_key)`,
		// goalContinuitySchemaSQL participates in PostgreSQL's immutable base-v1
		// checksum. Keep new indexes in the additive migration surface so upgrades
		// do not fail checksum validation.
		`CREATE INDEX IF NOT EXISTS idx_goal_session_reclaiming ON goal_session(state, updated_at)`,
		`ALTER TABLE groups ADD COLUMN force_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN force_effort TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN default_egress_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN egress_ids TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE groups ADD COLUMN model_instructions_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE groups ADD COLUMN model_instructions_files TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE custom_providers ADD COLUMN transport_profile TEXT NOT NULL DEFAULT 'generic'`,
		`ALTER TABLE custom_providers ADD COLUMN routes_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE custom_providers ADD COLUMN egress_ids TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE custom_providers ADD COLUMN model_mappings_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE egress_profiles ADD COLUMN exit_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE egress_profiles ADD COLUMN chain_proxy TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN ignore_rate_limit_controls INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN force_codex_429 INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE accounts ADD COLUMN routing_weight INTEGER NOT NULL DEFAULT 100`,
		`ALTER TABLE accounts ADD COLUMN retry_max_attempts INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE account_auth_tokens ADD COLUMN auth_method TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN credential_mode TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN agent_runtime_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN agent_private_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN agent_task_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE account_auth_tokens ADD COLUMN scopes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_auth_tokens ADD COLUMN oauth_rate_limit_tier TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_model_capabilities ADD COLUMN raw_model_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE account_model_capabilities ADD COLUMN availability_state TEXT NOT NULL DEFAULT 'unverified'`,
		`ALTER TABLE account_model_capabilities ADD COLUMN context_1m_state TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE account_model_capabilities ADD COLUMN context_1m_source TEXT NOT NULL DEFAULT ''`,
		`UPDATE account_model_capabilities
SET availability_state = 'verified'
WHERE availability_state = 'unverified'
  AND source <> ''
  AND lower(source) NOT LIKE '%static%'
  AND lower(source) NOT LIKE '%unknown%'`,
		`CREATE TABLE IF NOT EXISTS account_model_catalog_status(
  account_id TEXT PRIMARY KEY,
  authoritative INTEGER NOT NULL DEFAULT 0,
  last_probe_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE TABLE IF NOT EXISTS kiro_model_catalog(
  account_id TEXT NOT NULL,
  capability_key TEXT NOT NULL,
  upstream_id TEXT NOT NULL,
  public_id TEXT NOT NULL,
  aliases_json TEXT NOT NULL DEFAULT '[]',
  region TEXT NOT NULL DEFAULT '',
  is_default INTEGER NOT NULL DEFAULT 0,
  max_input_tokens INTEGER NOT NULL DEFAULT 0,
  max_output_tokens INTEGER NOT NULL DEFAULT 0,
  thinking_json TEXT NOT NULL DEFAULT '{}',
  effort_json TEXT NOT NULL DEFAULT '{}',
  source TEXT NOT NULL,
  generation INTEGER NOT NULL,
  observed_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  complete INTEGER NOT NULL DEFAULT 0,
  raw_json_hash TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(account_id, capability_key, upstream_id),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_kiro_catalog_public ON kiro_model_catalog(public_id,account_id,expires_at)`,
		`CREATE TABLE IF NOT EXISTS kiro_probe_state(
  account_id TEXT NOT NULL,
  capability_key TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT '',
  endpoint_hash TEXT NOT NULL DEFAULT '',
  governance_key TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  last_success_at INTEGER NOT NULL DEFAULT 0,
  last_error_at INTEGER NOT NULL DEFAULT 0,
  last_error_class TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  generation INTEGER NOT NULL DEFAULT 0,
  page_count INTEGER NOT NULL DEFAULT 0,
  complete INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(account_id, capability_key),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE TABLE IF NOT EXISTS diagnostic_jobs(
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  format_version TEXT NOT NULL DEFAULT 'v3',
  artifact_path TEXT NOT NULL DEFAULT '',
  artifact_size INTEGER NOT NULL DEFAULT 0,
  artifact_sha256 TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  download_leases INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  started_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0
)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnostic_jobs_status ON diagnostic_jobs(status,created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnostic_jobs_expiry ON diagnostic_jobs(expires_at,status)`,
		`CREATE TABLE IF NOT EXISTS diagnostic_download_leases(
  lease_id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(job_id) REFERENCES diagnostic_jobs(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnostic_download_leases_job ON diagnostic_download_leases(job_id,expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnostic_download_leases_expiry ON diagnostic_download_leases(expires_at)`,
		`CREATE TABLE IF NOT EXISTS diagnostic_events(
  id TEXT PRIMARY KEY,
  event_type TEXT NOT NULL,
  severity TEXT NOT NULL DEFAULT 'info',
  entity_type TEXT NOT NULL DEFAULT '',
  entity_alias TEXT NOT NULL DEFAULT '',
  detail_json TEXT NOT NULL DEFAULT '{}',
  diagnostic_gap INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnostic_events_created ON diagnostic_events(created_at)`,
		`CREATE TABLE IF NOT EXISTS storage_resources(
  id TEXT PRIMARY KEY,
  resource_type TEXT NOT NULL,
  path TEXT NOT NULL,
  state TEXT NOT NULL,
  owner_id TEXT NOT NULL DEFAULT '',
  lease_expires_at INTEGER NOT NULL DEFAULT 0,
  fencing_token INTEGER NOT NULL DEFAULT 0,
  mount_id TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  retention_class TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS idx_storage_resources_gc ON storage_resources(state,retention_class,lease_expires_at,updated_at)`,
		`CREATE TABLE IF NOT EXISTS maintenance_leases(
  lease_name TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  fencing_token INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS registration_workflow_items(
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  method TEXT NOT NULL,
  state TEXT NOT NULL,
  platform TEXT NOT NULL DEFAULT 'chatgpt',
  remote_identity_alias TEXT NOT NULL DEFAULT '',
  error_class TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0,
  retry_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE INDEX IF NOT EXISTS idx_registration_workflow_items_job ON registration_workflow_items(job_id,state)`,
		`CREATE TABLE IF NOT EXISTS registration_method_canaries(
  method TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  readiness_fingerprint TEXT NOT NULL DEFAULT '',
  job_id TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL DEFAULT '',
  error_class TEXT NOT NULL DEFAULT '',
  last_success_at INTEGER NOT NULL DEFAULT 0,
  last_failure_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS key_metadata(
  key_domain TEXT PRIMARY KEY,
  key_id TEXT NOT NULL,
  version INTEGER NOT NULL,
  status TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  rotated_at INTEGER NOT NULL DEFAULT 0
)`,
		`INSERT INTO account_model_catalog_status(account_id, authoritative, last_probe_at)
SELECT account_id, 1, MAX(last_probe_at)
FROM account_model_capabilities
WHERE availability_state = 'verified'
  AND lower(source) NOT LIKE '%runtime%'
  AND lower(source) NOT LIKE '%static%'
  AND lower(source) NOT LIKE '%unknown%'
GROUP BY account_id
ON CONFLICT(account_id) DO NOTHING`,
		`ALTER TABLE affinity_bindings ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN egress_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE affinity_bindings ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE INDEX IF NOT EXISTS idx_affinity_bindings_expiry ON affinity_bindings(expires_at, egress_id)`,
		// Retention runs in a short background budget. Keep both legacy
		// previous_response_id expiry and generic idle-binding selection on an
		// ordered index so cleanup does not scan the complete routing history while
		// holding the single SQLite writer.
		`CREATE INDEX IF NOT EXISTS idx_affinity_bindings_source_expiry ON affinity_bindings(source, expires_at, route_key_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_affinity_bindings_updated ON affinity_bindings(updated_at, route_key_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_affinity_aliases_updated ON affinity_aliases(updated_at, route_key_hash)`,
		// Emergency diagnostics and bounded support exports select the newest hold
		// window. The status-leading operational index cannot satisfy that order.
		`CREATE INDEX IF NOT EXISTS idx_billing_holds_created ON billing_holds(created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_action_time ON audit_log(action, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_log_state_time ON audit_log(state, created_at)`,
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
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_hash TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_shard INTEGER NOT NULL DEFAULT -1`,
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_minute_rpm INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN prompt_cache_key_concurrency_peak INTEGER NOT NULL DEFAULT 0`,
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
		`ALTER TABLE usage_records ADD COLUMN coordination_prefix_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN singleflight_wait_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN singleflight_release_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_point_state TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_state TEXT NOT NULL DEFAULT 'unknown'`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_evidence TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_credit_reduction_percent REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE kiro_runtime_capabilities ADD COLUMN cache_reuse_probed_at INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS codex_cache_capabilities(account_id TEXT NOT NULL, model TEXT NOT NULL, explicit_breakpoint_state TEXT NOT NULL DEFAULT 'unknown', first_write_tokens INTEGER NOT NULL DEFAULT 0, second_read_tokens INTEGER NOT NULL DEFAULT 0, probed_at INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL, PRIMARY KEY(account_id, model), FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE)`,
		`ALTER TABLE usage_records ADD COLUMN diagnostics_miss_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_auto_context_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_tail_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN latest_user_tool_result_cache_control INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN route_epoch INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN usage_event_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN kiro_credits REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN kiro_credits_present INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN billing_hold_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN requested_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN resolved_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN model_override_source TEXT NOT NULL DEFAULT 'none'`,
		`ALTER TABLE usage_records ADD COLUMN actual_model TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE usage_records ADD COLUMN model_mismatch INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE usage_records ADD COLUMN model_mismatch_reason TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_model_mismatch_created ON usage_records(model_mismatch, created_at)`,
		`ALTER TABLE billing_holds ADD COLUMN usage_expected INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE billing_holds ADD COLUMN usage_recorded_at INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE goal_session ADD COLUMN storage_bytes INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE goal_checkpoint ADD COLUMN format_version INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE goal_segment ADD COLUMN format_version INTEGER NOT NULL DEFAULT 1`,
		`CREATE INDEX IF NOT EXISTS idx_goal_session_updated ON goal_session(updated_at, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_records_event_id ON usage_records(usage_event_id) WHERE usage_event_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_usage_records_created_model ON usage_records(created_at, model)`,
		// Startup billing recovery resolves historical holds through this column.
		// Without the partial index, every missing hold repeatedly scans the full
		// usage history and can keep a large install pre-listener for minutes.
		`CREATE INDEX IF NOT EXISTS idx_usage_records_billing_hold ON usage_records(billing_hold_id) WHERE billing_hold_id <> ''`,
		// Per-account usage rollups (the admin account list's UsageSummaryByAccountIDs)
		// filter `account_id IN (...) AND created_at >= ?`. Without an account_id-leading
		// index SQLite scans the whole usage_records history per page load; this composite
		// turns it into per-account range seeks. Biggest single win for admin list latency.
		`CREATE INDEX IF NOT EXISTS idx_usage_records_account_created ON usage_records(account_id, created_at)`,
		// Cooldown→health-recheck gate: a benched account stays out of the candidate
		// pool until a liveness probe confirms it recovered (older DBs created before
		// the recheck loop existed).
		`ALTER TABLE account_egress_bindings ADD COLUMN recheck_pending INTEGER NOT NULL DEFAULT 0`,
		// Optional account-level TLS/HTTP2 transport wrapper. The selected primary or
		// standby remains the real IP egress; this sidecar chains through it.
		`ALTER TABLE account_egress_bindings ADD COLUMN sidecar_egress_id TEXT NOT NULL DEFAULT ''`,
		// Runtime egress precedence: groups own the single inference outlet unless an
		// account is explicitly pinned by the operator/import path.
		`ALTER TABLE account_egress_bindings ADD COLUMN binding_scope TEXT NOT NULL DEFAULT 'group'`,
		// SMS multi-platform tracking: record which provider + country a registration
		// used, and what it cost, so the local stats API can aggregate per-platform
		// per-country success rates (Phase 7 — additive migration).
		`ALTER TABLE registration_records ADD COLUMN sms_provider TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE registration_records ADD COLUMN sms_country TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE registration_records ADD COLUMN sms_cost REAL NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS sms_country_price_snapshots(
  provider TEXT NOT NULL,
  service TEXT NOT NULL DEFAULT 'dr',
  country_id TEXT NOT NULL,
  country_iso TEXT NOT NULL DEFAULT '',
  country_name TEXT NOT NULL DEFAULT '',
  price REAL NOT NULL DEFAULT 0,
  inventory INTEGER NOT NULL DEFAULT 0,
  provider_rank INTEGER NOT NULL DEFAULT 9999,
  balance REAL NOT NULL DEFAULT -1,
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY(provider, service, country_id)
)`,
		`CREATE INDEX IF NOT EXISTS idx_sms_country_prices_fresh ON sms_country_price_snapshots(fetched_at, provider, country_iso)`,
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
		// Keep additive indexes out of codexSessionMappingSchemaSQL: that schema is
		// part of PostgreSQL's immutable 20260727_base_v1 checksum.  Putting a new
		// index there makes every existing PostgreSQL installation fail startup with
		// a checksum mismatch.  state leads because the moving-window query uses a
		// small IN set, followed by the time range and all remaining referenced
		// columns so both SQLite and PostgreSQL can answer it from the index.
		`ALTER TABLE codex_upstream_attempt ADD COLUMN event_id TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_codex_upstream_attempt_event ON codex_upstream_attempt(event_id) WHERE event_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_codex_upstream_attempt_recent_egress ON codex_upstream_attempt(state, created_at, expires_at, egress_id)`,
		// These indexes deliberately live in the additive surface: the base Codex
		// schema is checksum-pinned on PostgreSQL. They bound the two slow paths
		// observed in emergency-v3 diagnostics: expired alias cleanup and newest
		// attempt export.
		`CREATE INDEX IF NOT EXISTS idx_codex_session_alias_expiry ON codex_session_alias(expires_at, alias_hash, binding_id)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_session_binding_updated ON codex_session_binding(updated_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_instruction_snapshot_updated ON codex_instruction_snapshot(updated_at, tree_id)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_upstream_attempt_created ON codex_upstream_attempt(created_at, id)`,
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
		// Multi-group membership: one account can serve traffic for multiple groups.
		// The primary group remains accounts.group_name (scheduler hot path).
		// account_group_memberships covers additional / overlapping group memberships.
		// is_primary=1 mirrors accounts.group_name; additional memberships have is_primary=0.
		`CREATE TABLE IF NOT EXISTS account_group_memberships(
  account_id TEXT NOT NULL,
  group_name TEXT NOT NULL,
  is_primary INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, group_name),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_group_memberships_group ON account_group_memberships(group_name, account_id)`,
		// Backfill: primary memberships from accounts.group_name for pre-existing rows.
		`INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
SELECT id, group_name, 1, created_at FROM accounts WHERE group_name <> ''
ON CONFLICT(account_id, group_name) DO NOTHING`,
		// Backfill: all accounts with provider='kiro' also get a "kiro" base membership
		// so operators can select them by group='kiro' regardless of their primary group.
		`INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
SELECT id, 'kiro', 0, created_at FROM accounts WHERE provider = 'kiro' AND group_name <> 'kiro'
ON CONFLICT(account_id, group_name) DO NOTHING`,
		// Antigravity (Google Cloud Code) provider credentials.
		// Stores per-account OAuth tokens and project metadata independently from the
		// generic account_auth_tokens table so Antigravity-specific fields can evolve.
		`CREATE TABLE IF NOT EXISTS account_antigravity_credentials(
  account_id TEXT PRIMARY KEY,
  email TEXT NOT NULL DEFAULT '',
  project_id TEXT NOT NULL DEFAULT '',
  access_token TEXT NOT NULL DEFAULT '',
  refresh_token TEXT NOT NULL DEFAULT '',
  expires_at INTEGER NOT NULL DEFAULT 0,
  base_url TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		// Two-layer group model: user_groups are user-facing routing containers (prompt
		// injection, force_model/effort live here). user_group_targets maps each user_group
		// to one or more base groups / relays / built-ins with affinity weights.
		`CREATE TABLE IF NOT EXISTS user_groups(
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  system_prompt TEXT NOT NULL DEFAULT '',
  prompt_mode TEXT NOT NULL DEFAULT 'prepend',
  system_prompt_apply_to_compaction INTEGER NOT NULL DEFAULT 1,
  model_instructions_enabled INTEGER NOT NULL DEFAULT 0,
  model_instructions_files TEXT NOT NULL DEFAULT '[]',
  model_instruction_profiles TEXT NOT NULL DEFAULT '{}',
  super_instruct_enabled INTEGER NOT NULL DEFAULT 0,
  super_instruct_skill_ids TEXT NOT NULL DEFAULT '[]',
  super_instruct_profiles TEXT NOT NULL DEFAULT '{}',
  super_instruct_response_rewrite_enabled INTEGER NOT NULL DEFAULT 0,
  super_instruct_memory_enabled INTEGER NOT NULL DEFAULT 0,
  super_instruct_monitor_enabled INTEGER NOT NULL DEFAULT 0,
  force_model TEXT NOT NULL DEFAULT '',
  force_effort TEXT NOT NULL DEFAULT '',
  block_claude_target_groups TEXT NOT NULL DEFAULT '[]',
  block_gpt_target_groups TEXT NOT NULL DEFAULT '[]',
  traffic_fallback_groups_json TEXT NOT NULL DEFAULT '{}',
  traffic_fallback_model_mappings_json TEXT NOT NULL DEFAULT '[]',
  model_routing_json TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_groups_name ON user_groups(name)`,
		`CREATE TABLE IF NOT EXISTS user_group_targets(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_group_id TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_ref TEXT NOT NULL DEFAULT '',
  affinity_weight INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(user_group_id) REFERENCES user_groups(id) ON DELETE CASCADE,
  UNIQUE(user_group_id, target_type, target_ref)
)`,
		`CREATE INDEX IF NOT EXISTS idx_user_group_targets_group ON user_group_targets(user_group_id)`,
		`CREATE TABLE IF NOT EXISTS user_group_target_bindings(
  user_group_id TEXT NOT NULL,
  affinity_key TEXT NOT NULL,
  model TEXT NOT NULL DEFAULT '',
  target_kind TEXT NOT NULL,
  target_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(user_group_id, affinity_key, model),
  FOREIGN KEY(user_group_id) REFERENCES user_groups(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_user_group_target_bindings_updated ON user_group_target_bindings(updated_at)`,
		`ALTER TABLE user_groups ADD COLUMN model_routing_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE user_groups ADD COLUMN model_instruction_profiles TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_skill_ids TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_profiles TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_response_rewrite_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_memory_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_groups ADD COLUMN super_instruct_monitor_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE user_groups ADD COLUMN block_claude_target_groups TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE user_groups ADD COLUMN block_gpt_target_groups TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE user_groups ADD COLUMN traffic_fallback_groups_json TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE user_groups ADD COLUMN traffic_fallback_model_mappings_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE api_keys ADD COLUMN user_group_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_user_group ON api_keys(user_group_id) WHERE user_group_id <> ''`,
		// Antigravity explicit cache entries: tracks Gemini CachedContent resources
		// created for the (account, model, conversation-prefix) triple so follow-up
		// turns can skip re-sending the full history. Expires when the resource TTL
		// lapses; entries with expires_at <= now() are pruned on startup.
		`CREATE TABLE IF NOT EXISTS antigravity_cache_entries(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id TEXT NOT NULL,
  model_id TEXT NOT NULL,
  conv_key_hash TEXT NOT NULL,
  cache_resource_name TEXT NOT NULL,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  UNIQUE(account_id, model_id, conv_key_hash),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
)`,
		`CREATE INDEX IF NOT EXISTS idx_antigravity_cache_expires ON antigravity_cache_entries(expires_at)`,
		// Public no-login browser chat links. Kept in the additive migration surface
		// so PostgreSQL's immutable base schema checksum does not change.
		`CREATE TABLE IF NOT EXISTS public_chat_links(
  id TEXT PRIMARY KEY,
  slug TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  route_type TEXT NOT NULL DEFAULT 'user_group',
  user_group_id TEXT NOT NULL DEFAULT '',
  group_name TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT 'gpt-5.6-sol',
  title TEXT NOT NULL DEFAULT '',
  welcome_message TEXT NOT NULL DEFAULT '',
  max_history_messages INTEGER NOT NULL DEFAULT 24,
  rate_limit_per_minute INTEGER NOT NULL DEFAULT 30,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_public_chat_links_slug ON public_chat_links(slug)`,
		`CREATE INDEX IF NOT EXISTS idx_public_chat_links_enabled ON public_chat_links(enabled, updated_at)`,
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
	if legacyUserGroupMigrationEnabled() {
		return s.backfillUserGroups(ctx)
	}
	return nil
}

// legacyUserGroupMigrationEnabled controls the compatibility backfill that
// copies account-pool groups into the user-facing routing layer. Keep the
// historical behavior when the variable is absent, while allowing installers
// to disable the backfill so deleted user groups stay deleted across restarts.
func legacyUserGroupMigrationEnabled() bool {
	raw, ok := os.LookupEnv("CODEX_POOL_MIGRATE_USER_GROUPS")
	if !ok || strings.TrimSpace(raw) == "" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
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
		`ALTER TABLE quota_window_samples ADD COLUMN unsettled_share REAL NOT NULL DEFAULT 0`,
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
	row := s.rdb.QueryRowContext(ctx, `SELECT name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, egress_ids, created_at, updated_at FROM groups WHERE name = ?`, name)
	var g Group
	return scanGroup(row.Scan, &g)
}

func (s *Store) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, egress_ids, created_at, updated_at FROM groups ORDER BY name`)
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
	var filesJSON, egressJSON string
	err := scan(&g.Name, &g.SystemPrompt, &g.PromptMode, &apply, &virtual, &modelInstructionsEnabled, &filesJSON, &g.ForceModel, &g.ForceEffort, &g.DefaultEgressID, &egressJSON, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return *g, err
	}
	g.SystemPromptApplyToCompaction = apply != 0
	g.Virtual2MEnabled = virtual != 0
	g.ModelInstructionsEnabled = modelInstructionsEnabled != 0
	g.ModelInstructionsFiles = decodeStringList(filesJSON)
	g.egressIDsConfigured = strings.TrimSpace(egressJSON) != ""
	if g.egressIDsConfigured {
		g.EgressIDs = decodeStringList(egressJSON)
	} else if strings.TrimSpace(g.DefaultEgressID) != "" {
		g.EgressIDs = []string{strings.TrimSpace(g.DefaultEgressID)}
	} else {
		g.EgressIDs = []string{}
	}
	if len(g.EgressIDs) > 1 {
		g.EgressIDs = g.EgressIDs[:1]
	}
	return *g, nil
}

func normalizeOrderedIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func encodeOrderedIDs(values []string) string {
	raw, err := json.Marshal(normalizeOrderedIDs(values))
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func prepareGroupEgress(g *Group) string {
	ids := g.EgressIDs
	if ids == nil && strings.TrimSpace(g.DefaultEgressID) != "" {
		ids = []string{g.DefaultEgressID}
	}
	ids = normalizeOrderedIDs(ids)
	if len(ids) > 1 {
		ids = ids[:1]
	}
	g.EgressIDs = ids
	if len(ids) > 0 {
		g.DefaultEgressID = ids[0]
	} else if g.EgressIDs != nil {
		g.DefaultEgressID = ""
	}
	return encodeOrderedIDs(ids)
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
	egressJSON := prepareGroupEgress(&g)
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET system_prompt = ?, prompt_mode = ?, system_prompt_apply_to_compaction = ?, virtual_2m_enabled = ?, model_instructions_enabled = ?, model_instructions_files = ?, force_model = ?, force_effort = ?, default_egress_id = ?, egress_ids = ?, updated_at = ? WHERE name = ?`,
		g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.Virtual2MEnabled), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), g.ForceModel, g.ForceEffort, g.DefaultEgressID, egressJSON, Now(), g.Name)
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
	ids, err := superinstruct.NormalizeSkillIDs(g.SuperInstructSkillIDs)
	if err != nil {
		return err
	}
	g.SuperInstructSkillIDs = ids
	superProfiles, err := normalizeSuperInstructProfiles(g.SuperInstructProfiles)
	if err != nil {
		return err
	}
	g.SuperInstructProfiles = superProfiles
	now := Now()
	egressJSON := prepareGroupEgress(&g)
	_, err = s.db.ExecContext(ctx, `INSERT INTO groups(name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, virtual_2m_enabled, model_instructions_enabled, model_instructions_files, force_model, force_effort, default_egress_id, egress_ids, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(g.Name), g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.Virtual2MEnabled), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), g.ForceModel, g.ForceEffort, g.DefaultEgressID, egressJSON, now, now)
	return err
}

func routingTargetReferences(ctx context.Context, tx *sql.Tx, target TargetRef) ([]string, error) {
	target, err := NormalizeTargetRef(target)
	if err != nil {
		return nil, err
	}
	references := map[string]struct{}{}
	targetRows, err := tx.QueryContext(ctx, `SELECT user_group_id, target_type, target_ref FROM user_group_targets`)
	if err != nil {
		return nil, err
	}
	for targetRows.Next() {
		var userGroupID, kind, id string
		if err := targetRows.Scan(&userGroupID, &kind, &id); err != nil {
			_ = targetRows.Close()
			return nil, err
		}
		canonical, normalizeErr := NormalizeTargetRef(TargetRef{Kind: kind, ID: id})
		if normalizeErr != nil {
			_ = targetRows.Close()
			return nil, fmt.Errorf("invalid target row for user group %q: %w", userGroupID, normalizeErr)
		}
		if canonical.key() == target.key() {
			references["user_group_target:"+userGroupID] = struct{}{}
		}
	}
	if err := targetRows.Err(); err != nil {
		_ = targetRows.Close()
		return nil, err
	}
	if err := targetRows.Close(); err != nil {
		return nil, err
	}

	routingRows, err := tx.QueryContext(ctx, `SELECT id, model_routing_json FROM user_groups`)
	if err != nil {
		return nil, err
	}
	for routingRows.Next() {
		var userGroupID, raw string
		if err := routingRows.Scan(&userGroupID, &raw); err != nil {
			_ = routingRows.Close()
			return nil, err
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var rules []ModelRoutingRule
		if err := json.Unmarshal([]byte(raw), &rules); err != nil {
			_ = routingRows.Close()
			return nil, fmt.Errorf("invalid model routing for user group %q: %w", userGroupID, err)
		}
		for _, rule := range rules {
			for _, tier := range rule.Tiers {
				for _, candidate := range tier {
					canonical, normalizeErr := NormalizeTargetRef(candidate)
					if normalizeErr != nil {
						_ = routingRows.Close()
						return nil, fmt.Errorf("invalid model routing target for user group %q: %w", userGroupID, normalizeErr)
					}
					if canonical.key() == target.key() {
						references["model_routing:"+userGroupID] = struct{}{}
					}
				}
			}
		}
	}
	if err := routingRows.Err(); err != nil {
		_ = routingRows.Close()
		return nil, err
	}
	if err := routingRows.Close(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(references))
	for reference := range references {
		out = append(out, reference)
	}
	sort.Strings(out)
	return out, nil
}

// DeleteGroup removes an account-pool group only when no user-group target or
// model-routing tier still names it. The caller separately guards the configured
// default group and account membership (see CountAccountsByGroup).
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("group name required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	references, err := routingTargetReferences(ctx, tx, TargetRef{Kind: TargetKindAccountPoolGroup, ID: name})
	if err != nil {
		return err
	}
	if len(references) > 0 {
		return fmt.Errorf("%w: account_pool_group/%s referenced by %s", ErrTargetInUse, name, strings.Join(references, ", "))
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// ── UserGroup CRUD ──────────────────────────────────────────────────────────

const userGroupCols = `id, name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, model_instructions_enabled, model_instructions_files, model_instruction_profiles, super_instruct_enabled, super_instruct_skill_ids, super_instruct_profiles, super_instruct_response_rewrite_enabled, super_instruct_memory_enabled, super_instruct_monitor_enabled, force_model, force_effort, block_claude_target_groups, block_gpt_target_groups, traffic_fallback_groups_json, traffic_fallback_model_mappings_json, model_routing_json, created_at, updated_at`

func scanUserGroup(scan func(...interface{}) error) (UserGroup, error) {
	var g UserGroup
	var apply, miEnabled, superEnabled, superResponseRewriteEnabled, superMemoryEnabled, superMonitorEnabled int
	var filesJSON, profilesJSON, superSkillIDsJSON, superProfilesJSON, blockClaudeJSON, blockGPTJSON, fallbackGroupsJSON, fallbackMappingsJSON, routingJSON string
	err := scan(&g.ID, &g.Name, &g.SystemPrompt, &g.PromptMode, &apply, &miEnabled, &filesJSON, &profilesJSON, &superEnabled, &superSkillIDsJSON, &superProfilesJSON, &superResponseRewriteEnabled, &superMemoryEnabled, &superMonitorEnabled, &g.ForceModel, &g.ForceEffort, &blockClaudeJSON, &blockGPTJSON, &fallbackGroupsJSON, &fallbackMappingsJSON, &routingJSON, &g.CreatedAt, &g.UpdatedAt)
	g.SystemPromptApplyToCompaction = apply != 0
	g.ModelInstructionsEnabled = miEnabled != 0
	g.ModelInstructionsFiles = decodeStringList(filesJSON)
	g.ModelInstructionProfiles = decodeModelInstructionProfiles(profilesJSON)
	g.SuperInstructEnabled = superEnabled != 0
	g.SuperInstructSkillIDs = decodeStringList(superSkillIDsJSON)
	g.SuperInstructProfiles = decodeSuperInstructProfiles(superProfilesJSON)
	g.SuperInstructResponseRewriteEnabled = superResponseRewriteEnabled != 0
	g.SuperInstructMemoryEnabled = superMemoryEnabled != 0
	g.SuperInstructMonitorEnabled = superMonitorEnabled != 0
	g.BlockClaudeTargetGroups = decodeStringList(blockClaudeJSON)
	g.BlockGPTTargetGroups = decodeStringList(blockGPTJSON)
	if err == nil {
		g.TrafficFallbackGroups = decodeTrafficFallbackGroups(fallbackGroupsJSON)
		_ = json.Unmarshal([]byte(fallbackMappingsJSON), &g.TrafficFallbackModelMappings)
		if g.TrafficFallbackModelMappings == nil {
			g.TrafficFallbackModelMappings = []TrafficFallbackModelMapping{}
		}
		_ = json.Unmarshal([]byte(routingJSON), &g.ModelRouting)
		if g.ModelRouting == nil {
			g.ModelRouting = []ModelRoutingRule{}
		}
	}
	return g, err
}

func (s *Store) hydrateUserGroup(ctx context.Context, g *UserGroup) error {
	targets, err := s.GetUserGroupTargetRefs(ctx, g.ID)
	if err != nil {
		return err
	}
	if targets == nil {
		targets = []TargetRef{}
	}
	g.Targets = targets
	return nil
}

func (s *Store) GetUserGroup(ctx context.Context, id string) (UserGroup, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+userGroupCols+` FROM user_groups WHERE id = ?`, id)
	g, err := scanUserGroup(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserGroup{}, false, nil
	}
	if err == nil {
		err = s.hydrateUserGroup(ctx, &g)
	}
	return g, err == nil, err
}

func (s *Store) GetUserGroupByName(ctx context.Context, name string) (UserGroup, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+userGroupCols+` FROM user_groups WHERE name = ?`, name)
	g, err := scanUserGroup(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserGroup{}, false, nil
	}
	if err == nil {
		err = s.hydrateUserGroup(ctx, &g)
	}
	return g, err == nil, err
}

func (s *Store) ListUserGroups(ctx context.Context) ([]UserGroup, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+userGroupCols+` FROM user_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []UserGroup
	for rows.Next() {
		g, err := scanUserGroup(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.hydrateUserGroup(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UserGroupRouteGeneration is a cheap, non-secret change token for a waiting
// inference request. It changes when the user-group target policy, a referenced
// provider, an account in any eligible pool, its credential metadata, or its
// model catalog changes. Callers can therefore keep a downstream connection
// alive without repeatedly replaying a large inference request against unchanged
// capacity.
func (s *Store) UserGroupRouteGeneration(ctx context.Context, userGroupID, baseGroup string) (string, error) {
	var groupUpdated, targetCount, targetUpdated, providerUpdated int64
	err := s.rdb.QueryRowContext(ctx, `
SELECT g.updated_at,
       COUNT(t.id),
       COALESCE(MAX(t.created_at), 0),
       COALESCE(MAX(p.updated_at), 0)
FROM user_groups g
LEFT JOIN user_group_targets t ON t.user_group_id = g.id
LEFT JOIN custom_providers p
       ON t.target_type = ? AND p.id = t.target_ref
WHERE g.id = ?
GROUP BY g.updated_at`, UserGroupTargetTypeRelay, strings.TrimSpace(userGroupID)).Scan(
		&groupUpdated, &targetCount, &targetUpdated, &providerUpdated,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUserGroupNotFound
	}
	if err != nil {
		return "", err
	}

	var accountCount, accountUpdated, tokenUpdated, kiroUpdated, capabilityCount, capabilityUpdated int64
	err = s.rdb.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT a.id),
       COALESCE(MAX(a.updated_at), 0),
       COALESCE(MAX(tok.updated_at), 0),
       COALESCE(MAX(kc.updated_at), 0),
       COUNT(cap.model_slug),
       COALESCE(MAX(cap.last_probe_at), 0)
FROM accounts a
LEFT JOIN account_auth_tokens tok ON tok.account_id = a.id
LEFT JOIN account_kiro_credentials kc ON kc.account_id = a.id
LEFT JOIN account_model_capabilities cap ON cap.account_id = a.id
WHERE a.group_name = ?
   OR EXISTS (
       SELECT 1
       FROM user_group_targets t
       WHERE t.user_group_id = ?
         AND t.target_type = ?
         AND t.target_ref = a.group_name
   )`, strings.TrimSpace(baseGroup), strings.TrimSpace(userGroupID), UserGroupTargetTypeBaseGroup).Scan(
		&accountCount, &accountUpdated, &tokenUpdated, &kiroUpdated, &capabilityCount, &capabilityUpdated,
	)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d:%d:%d",
		groupUpdated, targetCount, targetUpdated, providerUpdated,
		accountCount, accountUpdated, tokenUpdated, kiroUpdated,
		capabilityCount, capabilityUpdated,
	), nil
}

func (s *Store) CreateUserGroup(ctx context.Context, g UserGroup) error {
	if strings.TrimSpace(g.ID) == "" {
		return errors.New("user group id required")
	}
	if strings.TrimSpace(g.Name) == "" {
		return errors.New("user group name required")
	}
	if strings.TrimSpace(g.PromptMode) == "" {
		g.PromptMode = "prepend"
	}
	ids, err := superinstruct.NormalizeSkillIDs(g.SuperInstructSkillIDs)
	if err != nil {
		return err
	}
	g.SuperInstructSkillIDs = ids
	now := Now()
	if g.CreatedAt == 0 {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	routingJSON := encodeModelRouting(g.ModelRouting)
	_, err = s.db.ExecContext(ctx, `INSERT INTO user_groups(id, name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, model_instructions_enabled, model_instructions_files, model_instruction_profiles, super_instruct_enabled, super_instruct_skill_ids, super_instruct_profiles, super_instruct_response_rewrite_enabled, super_instruct_memory_enabled, super_instruct_monitor_enabled, force_model, force_effort, block_claude_target_groups, block_gpt_target_groups, traffic_fallback_groups_json, traffic_fallback_model_mappings_json, model_routing_json, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`,
		g.ID, strings.TrimSpace(g.Name), g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), encodeModelInstructionProfiles(g.ModelInstructionProfiles), boolInt(g.SuperInstructEnabled), encodeStringList(g.SuperInstructSkillIDs), encodeSuperInstructProfiles(g.SuperInstructProfiles), boolInt(g.SuperInstructResponseRewriteEnabled), boolInt(g.SuperInstructMemoryEnabled), boolInt(g.SuperInstructMonitorEnabled), g.ForceModel, g.ForceEffort, encodeStringList(g.BlockClaudeTargetGroups), encodeStringList(g.BlockGPTTargetGroups), encodeTrafficFallbackGroups(g.TrafficFallbackGroups), encodeTrafficFallbackModelMappings(g.TrafficFallbackModelMappings), routingJSON, g.CreatedAt, g.UpdatedAt)
	return err
}

// CreateUserGroupWithTargets atomically creates a user-facing group and all of
// its routing targets. A failed target insert never leaves an unusable empty
// user group behind.
func (s *Store) CreateUserGroupWithTargets(ctx context.Context, g UserGroup, targets []UserGroupTarget) error {
	refs := make([]TargetRef, 0, len(targets))
	for _, target := range targets {
		ref, err := targetRefFromLegacy(target)
		if err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	g.Targets = refs
	return s.CreateUserGroupDefinition(ctx, g)
}

func targetRefFromLegacy(target UserGroupTarget) (TargetRef, error) {
	switch strings.TrimSpace(target.TargetType) {
	case UserGroupTargetTypeBaseGroup:
		return NormalizeTargetRef(TargetRef{Kind: TargetKindAccountPoolGroup, ID: target.TargetRef})
	case UserGroupTargetTypeRelay:
		return NormalizeTargetRef(TargetRef{Kind: TargetKindModelProvider, ID: target.TargetRef})
	case UserGroupTargetTypeKiro, UserGroupTargetTypeAntigravity:
		return NormalizeTargetRef(TargetRef{Kind: TargetKindAccountPoolGroup, ID: target.TargetType})
	default:
		return TargetRef{}, fmt.Errorf("unsupported user group target_type %q", target.TargetType)
	}
}

func legacyTargetFromRef(userGroupID string, target TargetRef, now int64) (UserGroupTarget, error) {
	target, err := NormalizeTargetRef(target)
	if err != nil {
		return UserGroupTarget{}, err
	}
	t := UserGroupTarget{UserGroupID: userGroupID, TargetRef: target.ID, AffinityWeight: 1, CreatedAt: now}
	switch target.Kind {
	case TargetKindAccountPoolGroup:
		t.TargetType = UserGroupTargetTypeBaseGroup
	case TargetKindModelProvider:
		t.TargetType = UserGroupTargetTypeRelay
	default:
		return UserGroupTarget{}, fmt.Errorf("unsupported user group target kind %q", target.Kind)
	}
	return t, nil
}

func normalizeTargetRefs(targets []TargetRef) ([]TargetRef, error) {
	seen := make(map[string]struct{}, len(targets))
	out := make([]TargetRef, 0, len(targets))
	for _, target := range targets {
		normalized, err := NormalizeTargetRef(target)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[normalized.key()]; duplicate {
			continue
		}
		seen[normalized.key()] = struct{}{}
		out = append(out, normalized)
	}
	return out, nil
}

func targetSupportsModel(target TargetRef, model string, providerModels map[string][]string) bool {
	if target.Kind != TargetKindModelProvider {
		return true
	}
	models, known := providerModels[target.key()]
	if !known || len(models) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range models {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "*" || candidate == model || (strings.HasSuffix(candidate, "*") && strings.HasPrefix(model, strings.TrimSuffix(candidate, "*"))) {
			return true
		}
	}
	return false
}

func normalizeModelRouting(rules []ModelRoutingRule, targets []TargetRef, providerModels map[string][]string) ([]ModelRoutingRule, error) {
	selected := make(map[string]TargetRef, len(targets))
	for _, target := range targets {
		selected[target.key()] = target
	}
	seenModels := make(map[string]struct{}, len(rules))
	out := make([]ModelRoutingRule, 0, len(rules))
	for _, rule := range rules {
		model := strings.TrimSpace(rule.Model)
		if model == "" {
			return nil, errors.New("model_routing model required")
		}
		modelKey := strings.ToLower(model)
		if _, duplicate := seenModels[modelKey]; duplicate {
			return nil, fmt.Errorf("duplicate model_routing rule for %q", model)
		}
		seenModels[modelKey] = struct{}{}
		mentioned := make(map[string]struct{}, len(targets))
		tiers := make([][]TargetRef, 0, len(rule.Tiers)+1)
		for _, tier := range rule.Tiers {
			cleanTier := make([]TargetRef, 0, len(tier))
			for _, rawTarget := range tier {
				target, err := NormalizeTargetRef(rawTarget)
				if err != nil {
					return nil, err
				}
				canonical, ok := selected[target.key()]
				if !ok {
					return nil, fmt.Errorf("model_routing target %s/%s is not selected", target.Kind, target.ID)
				}
				if !targetSupportsModel(canonical, model, providerModels) {
					return nil, fmt.Errorf("target %s/%s does not support model %q", canonical.Kind, canonical.ID, model)
				}
				if _, duplicate := mentioned[canonical.key()]; duplicate {
					continue
				}
				mentioned[canonical.key()] = struct{}{}
				cleanTier = append(cleanTier, canonical)
			}
			if len(cleanTier) > 0 {
				tiers = append(tiers, cleanTier)
			}
		}
		fallback := make([]TargetRef, 0, len(targets))
		for _, target := range targets {
			if _, alreadyMentioned := mentioned[target.key()]; alreadyMentioned || !targetSupportsModel(target, model, providerModels) {
				continue
			}
			fallback = append(fallback, target)
		}
		if len(fallback) > 0 {
			tiers = append(tiers, fallback)
		}
		if len(tiers) == 0 {
			return nil, fmt.Errorf("no compatible targets for model %q", model)
		}
		out = append(out, ModelRoutingRule{Model: model, Tiers: tiers})
	}
	return out, nil
}

func normalizeBlockedAccountPoolGroups(field string, values []string, targets []TargetRef) ([]string, error) {
	selected := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.Kind == TargetKindAccountPoolGroup {
			selected[target.ID] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, ok := selected[name]; !ok {
			return nil, fmt.Errorf("%s account-pool target %q is not selected by this user group", field, name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

const (
	maxTrafficFallbackGroupsPerFamily = 16
	maxTrafficFallbackModelMappings   = 128
	maxTrafficFallbackModelLength     = 200
)

func trafficFallbackGroupIDs(groups TrafficFallbackGroups, family string) []string {
	switch strings.ToLower(strings.TrimSpace(family)) {
	case ModelInstructionFamilyGPT:
		return groups.GPT
	case ModelInstructionFamilyClaude:
		return groups.Claude
	case ModelInstructionFamilyGemini:
		return groups.Gemini
	default:
		return nil
	}
}

func setTrafficFallbackGroupIDs(groups *TrafficFallbackGroups, family string, ids []string) {
	switch family {
	case ModelInstructionFamilyGPT:
		groups.GPT = ids
	case ModelInstructionFamilyClaude:
		groups.Claude = ids
	case ModelInstructionFamilyGemini:
		groups.Gemini = ids
	}
}

func encodeTrafficFallbackGroups(groups TrafficFallbackGroups) string {
	normalized := TrafficFallbackGroups{
		GPT:    decodeStringList(encodeStringList(groups.GPT)),
		Claude: decodeStringList(encodeStringList(groups.Claude)),
		Gemini: decodeStringList(encodeStringList(groups.Gemini)),
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return `{"gpt":[],"claude":[],"gemini":[]}`
	}
	return string(raw)
}

func decodeTrafficFallbackGroups(raw string) TrafficFallbackGroups {
	var groups TrafficFallbackGroups
	_ = json.Unmarshal([]byte(raw), &groups)
	groups.GPT = decodeStringList(encodeStringList(groups.GPT))
	groups.Claude = decodeStringList(encodeStringList(groups.Claude))
	groups.Gemini = decodeStringList(encodeStringList(groups.Gemini))
	return groups
}

func encodeTrafficFallbackModelMappings(mappings []TrafficFallbackModelMapping) string {
	if mappings == nil {
		mappings = []TrafficFallbackModelMapping{}
	}
	raw, err := json.Marshal(mappings)
	if err != nil {
		return "[]"
	}
	return string(raw)
}

func normalizeTrafficFallbackConfig(ctx context.Context, tx *sql.Tx, g UserGroup) (TrafficFallbackGroups, []TrafficFallbackModelMapping, error) {
	var groups TrafficFallbackGroups
	selectedByFamily := make(map[string]map[string]struct{}, 3)
	for _, family := range []string{ModelInstructionFamilyGPT, ModelInstructionFamilyClaude, ModelInstructionFamilyGemini} {
		values := trafficFallbackGroupIDs(g.TrafficFallbackGroups, family)
		if len(values) > maxTrafficFallbackGroupsPerFamily {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic_fallback_groups.%s supports at most %d user groups", family, maxTrafficFallbackGroupsPerFamily)
		}
		seen := make(map[string]struct{}, len(values))
		clean := make([]string, 0, len(values))
		for _, rawID := range values {
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if id == g.ID {
				return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic_fallback_groups.%s cannot reference the current user group", family)
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM user_groups WHERE id = ?`, id).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic_fallback_groups.%s user group %q not found", family, id)
				}
				return TrafficFallbackGroups{}, nil, err
			}
			seen[id] = struct{}{}
			clean = append(clean, id)
		}
		if clean == nil {
			clean = []string{}
		}
		selectedByFamily[family] = seen
		setTrafficFallbackGroupIDs(&groups, family, clean)
	}

	if len(g.TrafficFallbackModelMappings) > maxTrafficFallbackModelMappings {
		return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic_fallback_model_mappings supports at most %d rules", maxTrafficFallbackModelMappings)
	}
	mappings := make([]TrafficFallbackModelMapping, 0, len(g.TrafficFallbackModelMappings))
	seenMappings := make(map[string]struct{}, len(g.TrafficFallbackModelMappings))
	mappedTargets := make(map[string]struct{}, len(g.TrafficFallbackModelMappings))
	for _, mapping := range g.TrafficFallbackModelMappings {
		family := strings.ToLower(strings.TrimSpace(mapping.Family))
		if family != ModelInstructionFamilyGPT && family != ModelInstructionFamilyClaude && family != ModelInstructionFamilyGemini {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic fallback mapping family %q is unsupported", mapping.Family)
		}
		sourceModel := strings.TrimSpace(mapping.SourceModel)
		targetModel := strings.TrimSpace(mapping.TargetModel)
		targetID := strings.TrimSpace(mapping.TargetUserGroupID)
		if sourceModel == "" || targetModel == "" {
			return TrafficFallbackGroups{}, nil, errors.New("traffic fallback mapping source_model and target_model are required")
		}
		if len(sourceModel) > maxTrafficFallbackModelLength || len(targetModel) > maxTrafficFallbackModelLength {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic fallback model names support at most %d characters", maxTrafficFallbackModelLength)
		}
		if strings.Contains(sourceModel[:len(sourceModel)-1], "*") {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic fallback source_model %q only supports a trailing wildcard", sourceModel)
		}
		if _, selected := selectedByFamily[family][targetID]; !selected {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic fallback mapping target user group %q is not selected for %s", targetID, family)
		}
		key := family + "\x00" + strings.ToLower(sourceModel) + "\x00" + targetID
		if _, duplicate := seenMappings[key]; duplicate {
			return TrafficFallbackGroups{}, nil, fmt.Errorf("duplicate traffic fallback mapping for %s model %q and user group %q", family, sourceModel, targetID)
		}
		seenMappings[key] = struct{}{}
		mappedTargets[family+"\x00"+targetID] = struct{}{}
		mappings = append(mappings, TrafficFallbackModelMapping{
			Family: family, SourceModel: sourceModel, TargetUserGroupID: targetID, TargetModel: targetModel,
		})
	}
	for family, selected := range selectedByFamily {
		for targetID := range selected {
			if _, configured := mappedTargets[family+"\x00"+targetID]; !configured {
				return TrafficFallbackGroups{}, nil, fmt.Errorf("traffic fallback user group %q requires at least one %s model mapping", targetID, family)
			}
		}
	}
	if mappings == nil {
		mappings = []TrafficFallbackModelMapping{}
	}
	return groups, mappings, nil
}

func trafficFallbackGraphIsAcyclic(ctx context.Context, tx *sql.Tx, currentID string, current TrafficFallbackGroups) error {
	graph := make(map[string][]string)
	rows, err := tx.QueryContext(ctx, `SELECT id, traffic_fallback_groups_json FROM user_groups`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		groups := decodeTrafficFallbackGroups(raw)
		graph[id] = uniqueStringsPreserveOrder(append(append(append([]string{}, groups.GPT...), groups.Claude...), groups.Gemini...))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	graph[currentID] = uniqueStringsPreserveOrder(append(append(append([]string{}, current.GPT...), current.Claude...), current.Gemini...))

	state := make(map[string]uint8, len(graph))
	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("traffic fallback cycle detected at user group %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, targetID := range graph[id] {
			if _, known := graph[targetID]; !known {
				continue
			}
			if err := visit(targetID); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range graph {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func uniqueStringsPreserveOrder(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (s *Store) normalizeUserGroupDefinition(ctx context.Context, tx *sql.Tx, g UserGroup) (UserGroup, error) {
	g.ID = strings.TrimSpace(g.ID)
	g.Name = strings.TrimSpace(g.Name)
	if g.ID == "" {
		return UserGroup{}, errors.New("user group id required")
	}
	if g.Name == "" {
		return UserGroup{}, errors.New("user group name required")
	}
	if strings.TrimSpace(g.PromptMode) == "" {
		g.PromptMode = "prepend"
	}
	g.ModelInstructionsFiles = decodeStringList(encodeStringList(g.ModelInstructionsFiles))
	profiles, err := normalizeModelInstructionProfiles(g.ModelInstructionProfiles)
	if err != nil {
		return UserGroup{}, err
	}
	g.ModelInstructionProfiles = profiles
	g.SuperInstructSkillIDs, err = superinstruct.NormalizeSkillIDs(g.SuperInstructSkillIDs)
	if err != nil {
		return UserGroup{}, err
	}
	g.SuperInstructProfiles, err = normalizeSuperInstructProfiles(g.SuperInstructProfiles)
	if err != nil {
		return UserGroup{}, err
	}
	targets, err := normalizeTargetRefs(g.Targets)
	if err != nil {
		return UserGroup{}, err
	}
	if len(targets) == 0 {
		return UserGroup{}, errors.New("at least one user group target required")
	}
	g.BlockClaudeTargetGroups, err = normalizeBlockedAccountPoolGroups("block_claude_target_groups", g.BlockClaudeTargetGroups, targets)
	if err != nil {
		return UserGroup{}, err
	}
	g.BlockGPTTargetGroups, err = normalizeBlockedAccountPoolGroups("block_gpt_target_groups", g.BlockGPTTargetGroups, targets)
	if err != nil {
		return UserGroup{}, err
	}
	providerModels := make(map[string][]string)
	for _, target := range targets {
		switch target.Kind {
		case TargetKindAccountPoolGroup:
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM groups WHERE name = ?`, target.ID).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return UserGroup{}, fmt.Errorf("account pool group %q not found", target.ID)
				}
				return UserGroup{}, err
			}
		case TargetKindModelProvider:
			if target.ID == "codex" || target.ID == "claude" || target.ID == "kiro" || target.ID == "antigravity" {
				providerModels[target.key()] = nil
				continue
			}
			var modelsJSON, modelMappingsJSON string
			if err := tx.QueryRowContext(ctx, `SELECT models_json, model_mappings_json FROM custom_providers WHERE id = ?`, target.ID).Scan(&modelsJSON, &modelMappingsJSON); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return UserGroup{}, fmt.Errorf("model provider %q not found", target.ID)
				}
				return UserGroup{}, err
			}
			models := decodeProviderModels(modelsJSON)
			for source := range decodeProviderModelMappings(modelMappingsJSON) {
				models = append(models, source)
			}
			providerModels[target.key()] = decodeProviderModelsFromSlice(models)
		}
	}
	g.Targets = targets
	g.ModelRouting, err = normalizeModelRouting(g.ModelRouting, targets, providerModels)
	if err != nil {
		return UserGroup{}, err
	}
	g.TrafficFallbackGroups, g.TrafficFallbackModelMappings, err = normalizeTrafficFallbackConfig(ctx, tx, g)
	if err != nil {
		return UserGroup{}, err
	}
	if err := trafficFallbackGraphIsAcyclic(ctx, tx, g.ID, g.TrafficFallbackGroups); err != nil {
		return UserGroup{}, err
	}
	return g, nil
}

func insertUserGroupTargets(ctx context.Context, tx *sql.Tx, g UserGroup, now int64) error {
	for _, target := range g.Targets {
		legacy, err := legacyTargetFromRef(g.ID, target, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_group_targets(user_group_id, target_type, target_ref, affinity_weight, created_at) VALUES(?,?,?,?,?)`,
			legacy.UserGroupID, legacy.TargetType, legacy.TargetRef, legacy.AffinityWeight, legacy.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

// CreateUserGroupDefinition atomically persists the full user-group policy,
// selected targets, and per-model routing tiers.
func (s *Store) CreateUserGroupDefinition(ctx context.Context, g UserGroup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	g, err = s.normalizeUserGroupDefinition(ctx, tx, g)
	if err != nil {
		return err
	}
	now := Now()
	if g.CreatedAt == 0 {
		g.CreatedAt = now
	}
	g.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_groups(id, name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, model_instructions_enabled, model_instructions_files, model_instruction_profiles, super_instruct_enabled, super_instruct_skill_ids, super_instruct_profiles, super_instruct_response_rewrite_enabled, super_instruct_memory_enabled, super_instruct_monitor_enabled, force_model, force_effort, block_claude_target_groups, block_gpt_target_groups, traffic_fallback_groups_json, traffic_fallback_model_mappings_json, model_routing_json, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.Name, g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), encodeModelInstructionProfiles(g.ModelInstructionProfiles), boolInt(g.SuperInstructEnabled), encodeStringList(g.SuperInstructSkillIDs), encodeSuperInstructProfiles(g.SuperInstructProfiles), boolInt(g.SuperInstructResponseRewriteEnabled), boolInt(g.SuperInstructMemoryEnabled), boolInt(g.SuperInstructMonitorEnabled), g.ForceModel, g.ForceEffort, encodeStringList(g.BlockClaudeTargetGroups), encodeStringList(g.BlockGPTTargetGroups), encodeTrafficFallbackGroups(g.TrafficFallbackGroups), encodeTrafficFallbackModelMappings(g.TrafficFallbackModelMappings), encodeModelRouting(g.ModelRouting), g.CreatedAt, g.UpdatedAt); err != nil {
		return err
	}
	if err := insertUserGroupTargets(ctx, tx, g, now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceUserGroupDefinition atomically replaces base fields, targets, and
// model-routing rules. Readers can never observe a partially updated group.
func (s *Store) ReplaceUserGroupDefinition(ctx context.Context, g UserGroup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	g, err = s.normalizeUserGroupDefinition(ctx, tx, g)
	if err != nil {
		return err
	}
	now := Now()
	result, err := tx.ExecContext(ctx, `UPDATE user_groups SET name=?, system_prompt=?, prompt_mode=?, system_prompt_apply_to_compaction=?, model_instructions_enabled=?, model_instructions_files=?, model_instruction_profiles=?, super_instruct_enabled=?, super_instruct_skill_ids=?, super_instruct_profiles=?, super_instruct_response_rewrite_enabled=?, super_instruct_memory_enabled=?, super_instruct_monitor_enabled=?, force_model=?, force_effort=?, block_claude_target_groups=?, block_gpt_target_groups=?, traffic_fallback_groups_json=?, traffic_fallback_model_mappings_json=?, model_routing_json=?, updated_at=? WHERE id=?`,
		g.Name, g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), encodeModelInstructionProfiles(g.ModelInstructionProfiles), boolInt(g.SuperInstructEnabled), encodeStringList(g.SuperInstructSkillIDs), encodeSuperInstructProfiles(g.SuperInstructProfiles), boolInt(g.SuperInstructResponseRewriteEnabled), boolInt(g.SuperInstructMemoryEnabled), boolInt(g.SuperInstructMonitorEnabled), g.ForceModel, g.ForceEffort, encodeStringList(g.BlockClaudeTargetGroups), encodeStringList(g.BlockGPTTargetGroups), encodeTrafficFallbackGroups(g.TrafficFallbackGroups), encodeTrafficFallbackModelMappings(g.TrafficFallbackModelMappings), encodeModelRouting(g.ModelRouting), now, g.ID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrUserGroupNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_group_targets WHERE user_group_id = ?`, g.ID); err != nil {
		return err
	}
	if err := insertUserGroupTargets(ctx, tx, g, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateUserGroup(ctx context.Context, g UserGroup) error {
	if strings.TrimSpace(g.PromptMode) == "" {
		g.PromptMode = "prepend"
	}
	ids, err := superinstruct.NormalizeSkillIDs(g.SuperInstructSkillIDs)
	if err != nil {
		return err
	}
	g.SuperInstructSkillIDs = ids
	superProfiles, err := normalizeSuperInstructProfiles(g.SuperInstructProfiles)
	if err != nil {
		return err
	}
	g.SuperInstructProfiles = superProfiles
	_, err = s.db.ExecContext(ctx, `UPDATE user_groups SET name=?, system_prompt=?, prompt_mode=?, system_prompt_apply_to_compaction=?, model_instructions_enabled=?, model_instructions_files=?, model_instruction_profiles=?, super_instruct_enabled=?, super_instruct_skill_ids=?, super_instruct_profiles=?, super_instruct_response_rewrite_enabled=?, super_instruct_memory_enabled=?, super_instruct_monitor_enabled=?, force_model=?, force_effort=?, block_claude_target_groups=?, block_gpt_target_groups=?, traffic_fallback_groups_json=?, traffic_fallback_model_mappings_json=?, model_routing_json=?, updated_at=? WHERE id=?`,
		strings.TrimSpace(g.Name), g.SystemPrompt, g.PromptMode, boolInt(g.SystemPromptApplyToCompaction), boolInt(g.ModelInstructionsEnabled), encodeStringList(g.ModelInstructionsFiles), encodeModelInstructionProfiles(g.ModelInstructionProfiles), boolInt(g.SuperInstructEnabled), encodeStringList(g.SuperInstructSkillIDs), encodeSuperInstructProfiles(g.SuperInstructProfiles), boolInt(g.SuperInstructResponseRewriteEnabled), boolInt(g.SuperInstructMemoryEnabled), boolInt(g.SuperInstructMonitorEnabled), g.ForceModel, g.ForceEffort, encodeStringList(g.BlockClaudeTargetGroups), encodeStringList(g.BlockGPTTargetGroups), encodeTrafficFallbackGroups(g.TrafficFallbackGroups), encodeTrafficFallbackModelMappings(g.TrafficFallbackModelMappings), encodeModelRouting(g.ModelRouting), Now(), g.ID)
	return err
}

func (s *Store) DeleteUserGroup(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id, traffic_fallback_groups_json FROM user_groups WHERE id <> ?`, id)
	if err != nil {
		return err
	}
	var references []string
	for rows.Next() {
		var sourceID, raw string
		if err := rows.Scan(&sourceID, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		groups := decodeTrafficFallbackGroups(raw)
		for _, candidate := range append(append(append([]string{}, groups.GPT...), groups.Claude...), groups.Gemini...) {
			if candidate == id {
				references = append(references, sourceID)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(references) > 0 {
		sort.Strings(references)
		return fmt.Errorf("%w: user_group/%s referenced as traffic fallback by %s", ErrTargetInUse, id, strings.Join(references, ", "))
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_groups WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetUserGroupTargets returns all targets for a user group, ordered by affinity_weight DESC.
func (s *Store) GetUserGroupTargets(ctx context.Context, userGroupID string) ([]UserGroupTarget, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, user_group_id, target_type, target_ref, affinity_weight, created_at FROM user_group_targets WHERE user_group_id = ? ORDER BY affinity_weight DESC, id`, userGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserGroupTarget
	for rows.Next() {
		var t UserGroupTarget
		if err := rows.Scan(&t.ID, &t.UserGroupID, &t.TargetType, &t.TargetRef, &t.AffinityWeight, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetUserGroupTargetRefs exposes legacy target rows through the canonical public
// {kind,id} representation. Ordering follows insertion order and is stable.
func (s *Store) GetUserGroupTargetRefs(ctx context.Context, userGroupID string) ([]TargetRef, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT target_type, target_ref FROM user_group_targets WHERE user_group_id = ? ORDER BY id`, userGroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TargetRef, 0)
	seen := make(map[string]struct{})
	for rows.Next() {
		var targetType, targetRef string
		if err := rows.Scan(&targetType, &targetRef); err != nil {
			return nil, err
		}
		canonical, err := targetRefFromLegacy(UserGroupTarget{TargetType: targetType, TargetRef: targetRef})
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[canonical.key()]; duplicate {
			continue
		}
		seen[canonical.key()] = struct{}{}
		out = append(out, canonical)
	}
	return out, rows.Err()
}

// GetUserGroupTargetRefsWithLegacyIDs exposes the numeric row identifiers used
// by the deprecated /targets/{id} DELETE route without reverting the public
// target representation to base_group/relay.
func (s *Store) GetUserGroupTargetRefsWithLegacyIDs(ctx context.Context, userGroupID string) ([]TargetRefWithLegacyID, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, target_type, target_ref FROM user_group_targets WHERE user_group_id = ? ORDER BY id`, strings.TrimSpace(userGroupID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TargetRefWithLegacyID, 0)
	for rows.Next() {
		var legacyID int64
		var kind, id string
		if err := rows.Scan(&legacyID, &kind, &id); err != nil {
			return nil, err
		}
		canonical, err := NormalizeTargetRef(TargetRef{Kind: kind, ID: id})
		if err != nil {
			return nil, err
		}
		out = append(out, TargetRefWithLegacyID{TargetRef: canonical, LegacyID: legacyID})
	}
	return out, rows.Err()
}

func (s *Store) GetUserGroupTargetBinding(ctx context.Context, userGroupID, affinityKey, model string) (UserGroupTargetBinding, bool, error) {
	userGroupID = strings.TrimSpace(userGroupID)
	affinityKey = strings.TrimSpace(affinityKey)
	model = strings.TrimSpace(model)
	binding, err := scanUserGroupTargetBinding(ctx, s.rdb, userGroupID, affinityKey, model)
	if errors.Is(err, sql.ErrNoRows) {
		return UserGroupTargetBinding{}, false, nil
	}
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}

	now := Now()
	if _, liveStore := s.rdb.(*sql.DB); liveStore && binding.UpdatedAt <= now-routeBindingTouchInterval {
		result, touchErr := s.db.ExecContext(ctx, `UPDATE user_group_target_bindings
SET updated_at=?
WHERE user_group_id=? AND affinity_key=? AND model=? AND updated_at=?`,
			now, userGroupID, affinityKey, model, binding.UpdatedAt)
		if touchErr != nil {
			return UserGroupTargetBinding{}, false, touchErr
		}
		touched, touchErr := result.RowsAffected()
		if touchErr != nil {
			return UserGroupTargetBinding{}, false, touchErr
		}
		if touched == 1 {
			binding.UpdatedAt = now
		} else {
			// A concurrent lookup may already have refreshed the row. If bounded
			// cleanup won the race instead, report a miss so the router claims a
			// new binding rather than using a row that is no longer durable.
			binding, err = scanUserGroupTargetBinding(ctx, s.rdb, userGroupID, affinityKey, model)
			if errors.Is(err, sql.ErrNoRows) {
				return UserGroupTargetBinding{}, false, nil
			}
			if err != nil {
				return UserGroupTargetBinding{}, false, err
			}
		}
	}
	return binding, true, nil
}

func (s *Store) UpsertUserGroupTargetBinding(ctx context.Context, binding UserGroupTargetBinding) error {
	target, binding, err := normalizeUserGroupTargetBinding(binding)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO user_group_target_bindings(user_group_id, affinity_key, model, target_kind, target_id, created_at, updated_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(user_group_id, affinity_key, model) DO UPDATE SET target_kind=excluded.target_kind, target_id=excluded.target_id, updated_at=excluded.updated_at`,
		binding.UserGroupID, binding.AffinityKey, binding.Model, target.Kind, target.ID, binding.CreatedAt, binding.UpdatedAt)
	return err
}

// ClaimUserGroupTargetBinding atomically publishes the first target selected for
// a conversation. Concurrent first turns all observe the same winner: exactly one
// insert succeeds and every loser receives the already-committed binding instead
// of overwriting it with its speculative choice.
func (s *Store) ClaimUserGroupTargetBinding(ctx context.Context, binding UserGroupTargetBinding) (UserGroupTargetBinding, bool, error) {
	target, binding, err := normalizeUserGroupTargetBinding(binding)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO user_group_target_bindings(user_group_id, affinity_key, model, target_kind, target_id, created_at, updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(user_group_id, affinity_key, model) DO NOTHING`,
		binding.UserGroupID, binding.AffinityKey, binding.Model, target.Kind, target.ID, binding.CreatedAt, binding.UpdatedAt)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	actual, err := scanUserGroupTargetBinding(ctx, tx, binding.UserGroupID, binding.AffinityKey, binding.Model)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	return actual, rows == 1, nil
}

// CompareAndSwapUserGroupTargetBinding migrates a replayable conversation only
// while the binding still names expected. A concurrent migration winner is never
// overwritten; callers receive the current binding and can follow it.
func (s *Store) CompareAndSwapUserGroupTargetBinding(ctx context.Context, expected TargetRef, replacement UserGroupTargetBinding) (UserGroupTargetBinding, bool, error) {
	expected, err := NormalizeTargetRef(expected)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	target, replacement, err := normalizeUserGroupTargetBinding(replacement)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE user_group_target_bindings
SET target_kind=?, target_id=?, updated_at=?
WHERE user_group_id=? AND affinity_key=? AND model=? AND target_kind=? AND target_id=?`,
		target.Kind, target.ID, replacement.UpdatedAt,
		replacement.UserGroupID, replacement.AffinityKey, replacement.Model, expected.Kind, expected.ID)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	actual, err := scanUserGroupTargetBinding(ctx, tx, replacement.UserGroupID, replacement.AffinityKey, replacement.Model)
	if err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return UserGroupTargetBinding{}, false, err
	}
	return actual, rows == 1, nil
}

type userGroupTargetBindingQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func scanUserGroupTargetBinding(ctx context.Context, q userGroupTargetBindingQuerier, userGroupID, affinityKey, model string) (UserGroupTargetBinding, error) {
	var binding UserGroupTargetBinding
	var kind, targetID string
	err := q.QueryRowContext(ctx, `SELECT user_group_id, affinity_key, model, target_kind, target_id, created_at, updated_at
FROM user_group_target_bindings WHERE user_group_id=? AND affinity_key=? AND model=?`, userGroupID, affinityKey, model).Scan(
		&binding.UserGroupID, &binding.AffinityKey, &binding.Model, &kind, &targetID, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return UserGroupTargetBinding{}, err
	}
	binding.Target, err = NormalizeTargetRef(TargetRef{Kind: kind, ID: targetID})
	if err != nil {
		return UserGroupTargetBinding{}, err
	}
	return binding, nil
}

func normalizeUserGroupTargetBinding(binding UserGroupTargetBinding) (TargetRef, UserGroupTargetBinding, error) {
	target, err := NormalizeTargetRef(binding.Target)
	if err != nil {
		return TargetRef{}, UserGroupTargetBinding{}, err
	}
	binding.UserGroupID = strings.TrimSpace(binding.UserGroupID)
	binding.AffinityKey = strings.TrimSpace(binding.AffinityKey)
	binding.Model = strings.TrimSpace(binding.Model)
	if binding.UserGroupID == "" || binding.AffinityKey == "" {
		return TargetRef{}, UserGroupTargetBinding{}, errors.New("user group target binding requires group and affinity key")
	}
	now := Now()
	if binding.CreatedAt == 0 {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	binding.Target = target
	return target, binding, nil
}

func (s *Store) UpsertUserGroupTarget(ctx context.Context, t UserGroupTarget) error {
	if t.AffinityWeight < 1 {
		t.AffinityWeight = 1
	}
	now := Now()
	if t.CreatedAt == 0 {
		t.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_group_targets(user_group_id, target_type, target_ref, affinity_weight, created_at) VALUES(?,?,?,?,?) ON CONFLICT(user_group_id, target_type, target_ref) DO UPDATE SET affinity_weight=excluded.affinity_weight`,
		t.UserGroupID, t.TargetType, t.TargetRef, t.AffinityWeight, t.CreatedAt)
	return err
}

func (s *Store) RemoveUserGroupTarget(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_group_targets WHERE id = ?`, id)
	return err
}

// RemoveUserGroupTargetForGroup is the scoped form used by the legacy admin
// route. A row ID discovered under one user group can never delete another
// group's target.
func (s *Store) RemoveUserGroupTargetForGroup(ctx context.Context, userGroupID string, id int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM user_group_targets WHERE user_group_id = ? AND id = ?`, strings.TrimSpace(userGroupID), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAPIKeyUserGroup links an api key to a user_group by ID.
func (s *Store) SetAPIKeyUserGroup(ctx context.Context, keyHash, userGroupID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET user_group_id = ?, updated_at = ? WHERE key_hash = ?`, userGroupID, Now(), keyHash)
	return err
}

// GetUserGroupForAPIKey resolves the user_group for a key that has a user_group_id set.
// Returns (zero, false, nil) when the key has no user_group_id or the group no longer exists.
func (s *Store) GetUserGroupForAPIKey(ctx context.Context, keyHash string) (UserGroup, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT `+userGroupCols+` FROM user_groups WHERE id = (SELECT user_group_id FROM api_keys WHERE key_hash = ? AND user_group_id <> '')`, keyHash)
	g, err := scanUserGroup(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserGroup{}, false, nil
	}
	return g, err == nil, err
}

// SetAccountGroup reassigns an account to a different primary group.
// Keeps account_group_memberships in sync: the old primary membership is demoted
// (is_primary=0) and the new group is upserted as is_primary=1.  Additional
// non-primary memberships (e.g. the "kiro" base group) are never touched.
func (s *Store) SetAccountGroup(ctx context.Context, accountID, group string) error {
	group = strings.TrimSpace(group)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET group_name = ?, updated_at = ? WHERE id = ?`,
		group, now, accountID,
	); err != nil {
		return err
	}
	// Demote any previously-primary membership row.
	if _, err := tx.ExecContext(ctx,
		`UPDATE account_group_memberships SET is_primary = 0 WHERE account_id = ? AND is_primary = 1`,
		accountID,
	); err != nil {
		return err
	}
	// Upsert the new primary membership.
	if group != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
			 VALUES(?, ?, 1, ?)
			 ON CONFLICT(account_id, group_name) DO UPDATE SET is_primary = 1`,
			accountID, group, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AccountGroupBatchSize keeps dynamic IN clauses below the parameter limits of
// every supported SQLite and PostgreSQL deployment.
const AccountGroupBatchSize = 500

type accountGroupBatchQueries struct {
	existing          string
	updateAccounts    string
	demoteMemberships string
	upsertMemberships string
}

func buildAccountGroupBatchQueries(accountCount int) accountGroupBatchQueries {
	ids := sqlPlaceholders(accountCount)
	return accountGroupBatchQueries{
		existing:          `SELECT id FROM accounts WHERE id IN (` + ids + `)`,
		updateAccounts:    `UPDATE accounts SET group_name = ?, updated_at = ? WHERE id IN (` + ids + `)`,
		demoteMemberships: `UPDATE account_group_memberships SET is_primary = 0 WHERE account_id IN (` + ids + `) AND is_primary = 1`,
		upsertMemberships: `INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
			SELECT id, ?, 1, ? FROM accounts WHERE id IN (` + ids + `)
			ON CONFLICT(account_id, group_name) DO UPDATE SET is_primary = 1`,
	}
}

// SetAccountsGroup reassigns one bounded batch and returns the IDs that existed
// and were updated. Input IDs are trimmed and deduplicated for SQL execution; the
// caller can use the returned IDs to retain occurrence-based accounting.
//
// The three writes execute in one transaction so accounts.group_name and primary
// memberships cannot diverge. Callers handling larger lists should use batches of
// AccountGroupBatchSize and may fall back to SetAccountGroup if a batch fails.
func (s *Store) SetAccountsGroup(ctx context.Context, accountIDs []string, group string) ([]string, error) {
	ids := make([]string, 0, len(accountIDs))
	seen := make(map[string]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []string{}, nil
	}
	if len(ids) > AccountGroupBatchSize {
		return nil, fmt.Errorf("account group batch has %d unique IDs; maximum is %d", len(ids), AccountGroupBatchSize)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	queries := buildAccountGroupBatchQueries(len(ids))
	rows, err := tx.QueryContext(ctx, queries.existing, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	existing := make([]string, 0, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		existing = append(existing, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return []string{}, nil
	}

	queries = buildAccountGroupBatchQueries(len(existing))
	group = strings.TrimSpace(group)
	now := Now()
	accountArgs := make([]interface{}, 0, len(existing)+2)
	accountArgs = append(accountArgs, group, now)
	accountArgs = append(accountArgs, stringArgs(existing)...)
	result, err := tx.ExecContext(ctx, queries.updateAccounts, accountArgs...)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != int64(len(existing)) {
		return nil, fmt.Errorf("account group batch updated %d of %d existing accounts", affected, len(existing))
	}
	if _, err := tx.ExecContext(ctx, queries.demoteMemberships, stringArgs(existing)...); err != nil {
		return nil, err
	}
	if group != "" {
		membershipArgs := make([]interface{}, 0, len(existing)+2)
		membershipArgs = append(membershipArgs, group, now)
		membershipArgs = append(membershipArgs, stringArgs(existing)...)
		if _, err := tx.ExecContext(ctx, queries.upsertMemberships, membershipArgs...); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return existing, nil
}

// AddAccountToGroup adds account to an extra (non-primary) group membership so the
// scheduler can select the account when routing requests for that group.
func (s *Store) AddAccountToGroup(ctx context.Context, accountID, group string) error {
	group = strings.TrimSpace(group)
	if group == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
		 VALUES(?, ?, 0, ?)
		 ON CONFLICT(account_id, group_name) DO NOTHING`,
		accountID, group, Now(),
	)
	return err
}

// RemoveAccountFromGroup removes a non-primary group membership.  The primary
// group (accounts.group_name) cannot be removed here; use SetAccountGroup instead.
func (s *Store) RemoveAccountFromGroup(ctx context.Context, accountID, group string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM account_group_memberships WHERE account_id = ? AND group_name = ? AND is_primary = 0`,
		accountID, strings.TrimSpace(group),
	)
	return err
}

// GetAccountGroups returns all group names the account belongs to, primary first.
func (s *Store) GetAccountGroups(ctx context.Context, accountID string) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx,
		`SELECT group_name FROM account_group_memberships
		 WHERE account_id = ?
		 ORDER BY is_primary DESC, group_name`,
		accountID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AutoJoinKiroBaseGroup ensures the account is in the "kiro" membership group.
// Called after creating or importing a Kiro account so the base group is always
// populated without requiring a manual AddAccountToGroup call.
func (s *Store) AutoJoinKiroBaseGroup(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_group_memberships(account_id, group_name, is_primary, created_at)
		 VALUES(?, 'kiro', 0, ?)
		 ON CONFLICT(account_id, group_name) DO NOTHING`,
		accountID, Now(),
	)
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

const apiKeyCols = `key_hash, COALESCE(label,''), COALESCE(key_type,'downstream'), COALESCE(group_name,''), COALESCE(force_model,''), COALESCE(force_effort,''), COALESCE(provider_hint,'auto'), enabled, expires_at, last_used_at, COALESCE(tenant_id,''), COALESCE(project_id,''), COALESCE(user_id,''), created_at, updated_at, COALESCE(secret,''), COALESCE(user_group_id,'')`

func scanAPIKey(scan func(...interface{}) error) (APIKey, error) {
	var k APIKey
	var enabled int
	err := scan(&k.KeyHash, &k.Label, &k.KeyType, &k.GroupName, &k.ForceModel, &k.ForceEffort, &k.ProviderHint, &enabled, &k.ExpiresAt, &k.LastUsedAt, &k.TenantID, &k.ProjectID, &k.UserID, &k.CreatedAt, &k.UpdatedAt, &k.Secret, &k.UserGroupID)
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
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(key_hash, tenant_id, project_id, user_id, key_type, label, group_name, user_group_id, force_model, force_effort, provider_hint, enabled, expires_at, last_used_at, secret, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(key_hash) DO UPDATE SET tenant_id=excluded.tenant_id, project_id=excluded.project_id, user_id=excluded.user_id, key_type=excluded.key_type, label=excluded.label, group_name=excluded.group_name, user_group_id=excluded.user_group_id, force_model=excluded.force_model, force_effort=excluded.force_effort, provider_hint=excluded.provider_hint, enabled=excluded.enabled, expires_at=excluded.expires_at, last_used_at=excluded.last_used_at, secret=excluded.secret, updated_at=excluded.updated_at`,
		k.KeyHash, k.TenantID, k.ProjectID, k.UserID, k.KeyType, k.Label, k.GroupName, k.UserGroupID, k.ForceModel, k.ForceEffort, k.ProviderHint, boolInt(k.Enabled), k.ExpiresAt, k.LastUsedAt, s.sealToken(k.Secret), k.CreatedAt, now)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertAccountTx(ctx, tx, account, token, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tokenCache.Delete(account.ID)
	return nil
}

func (s *Store) UpsertAccountWithAntigravityCredentials(ctx context.Context, account Account, token AccountToken, credentials AntigravityCredentials) error {
	if strings.TrimSpace(account.ID) == "" {
		return errors.New("antigravity account id required")
	}
	now := Now()
	credentials.AccountID = account.ID
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertAccountTx(ctx, tx, account, token, now); err != nil {
		return err
	}
	if err := s.upsertAntigravityCredentialsTx(ctx, tx, credentials, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tokenCache.Delete(account.ID)
	return nil
}

func (s *Store) upsertAccountTx(ctx context.Context, tx *sql.Tx, account Account, token AccountToken, now int64) error {
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
	if account.RoutingWeight <= 0 {
		account.RoutingWeight = 100
	}
	if account.RoutingWeight > 1000 {
		account.RoutingWeight = 1000
	}
	if account.RetryMaxAttempts < 0 || account.RetryMaxAttempts > 3 {
		account.RetryMaxAttempts = 0
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO accounts(id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		account.ID, account.Label, account.GroupName, account.UpstreamAccountID, account.ChatGPTUserID, account.Email, account.PlanType, account.Provider, account.Status, boolInt(account.IsFedramp), boolInt(account.IgnoreRateLimitControls), boolInt(account.ForceCodex429), account.RoutingWeight, account.RetryMaxAttempts, account.QuarantineUntil, account.QuarantineReason, account.CreatedAt, account.UpdatedAt)
	if err != nil {
		return err
	}
	token.AccountID = account.ID
	if token.CreatedAt == 0 {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `
INSERT INTO account_auth_tokens(account_id, auth_method, credential_mode, access_token, refresh_token, openai_api_key, id_token_raw, agent_runtime_id, agent_private_key, agent_task_id, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
 auth_method = excluded.auth_method,
 credential_mode = excluded.credential_mode,
 access_token = excluded.access_token,
 refresh_token = excluded.refresh_token,
 openai_api_key = excluded.openai_api_key,
 id_token_raw = excluded.id_token_raw,
 agent_runtime_id = excluded.agent_runtime_id,
 agent_private_key = excluded.agent_private_key,
 agent_task_id = excluded.agent_task_id,
 last_refresh = excluded.last_refresh,
 expires_at = excluded.expires_at,
 scopes = excluded.scopes,
 oauth_rate_limit_tier = excluded.oauth_rate_limit_tier,
 updated_at = excluded.updated_at`,
		token.AccountID, token.AuthMethod, token.CredentialMode, s.sealToken(token.AccessToken), s.sealToken(token.RefreshToken), s.sealToken(token.OpenAIAPIKey), s.sealToken(token.IDTokenRaw), s.sealToken(token.AgentRuntimeID), s.sealToken(token.AgentPrivateKey), s.sealToken(token.AgentTaskID), token.LastRefresh, token.ExpiresAt, token.Scopes, token.OAuthRateLimitTier, token.CreatedAt, token.UpdatedAt)
	if err != nil {
		return err
	}
	primaryEgressID, err := restoreAccountBackupDefaultEgressIDTx(ctx, tx, account)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, cookie_jar_key, cooldown_until, created_at, updated_at)
VALUES(?, ?, '', '', ?, 0, ?, ?)
ON CONFLICT(account_id) DO NOTHING`, account.ID, primaryEgressID, account.ID+":"+primaryEgressID, now, now)
	if err != nil {
		return err
	}
	return err
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts ORDER BY created_at, id`)
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
		case "cursor":
			summary.Cursor++
		default:
			summary.Other++
		}
	}
	return summary, rows.Err()
}

func accountProviderSummary(provider, accessToken, apiKey string) string {
	if provider = strings.TrimSpace(provider); provider != "" {
		return strings.ToLower(provider)
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
	return s.listAccountsPage(ctx, limit, offset, search, status, "", "", "created_at, id DESC")
}

func (s *Store) ListAccountsPageDesc(ctx context.Context, limit, offset int, search, status string) ([]Account, int, error) {
	return s.listAccountsPage(ctx, limit, offset, search, status, "", "", "created_at DESC, id DESC")
}

// ListAccountsPageByAuthType keeps pagination totals exact when the admin console
// separates API-key identities from login/OAuth accounts. authType accepts
// "api_key", "account", or an empty string for the original unfiltered view.
func (s *Store) ListAccountsPageByAuthType(ctx context.Context, limit, offset int, search, status, authType string) ([]Account, int, error) {
	return s.ListAccountsPageFiltered(ctx, limit, offset, search, status, authType, "")
}

// ListAccountsPageFiltered applies the account-pool filters in SQL so pagination
// totals and page boundaries remain exact. group is an exact group name; an empty
// value keeps the original all-groups view.
func (s *Store) ListAccountsPageFiltered(ctx context.Context, limit, offset int, search, status, authType, group string) ([]Account, int, error) {
	return s.listAccountsPage(ctx, limit, offset, search, status, authType, group, "created_at, id DESC")
}

func (s *Store) listAccountsPage(ctx context.Context, limit, offset int, search, status, authType, group, orderBy string) ([]Account, int, error) {
	where := ""
	args := []interface{}{}
	appendCondition := func(condition string, values ...interface{}) {
		if where == "" {
			where = " WHERE " + condition
		} else {
			where += " AND " + condition
		}
		args = append(args, values...)
	}
	if search != "" {
		s := "%" + search + "%"
		appendCondition("(label LIKE ? OR email LIKE ? OR group_name LIKE ? OR id LIKE ?)", s, s, s, s)
	}
	if status != "" {
		appendCondition("status = ?", status)
	}
	if group = strings.TrimSpace(group); group != "" {
		appendCondition("group_name = ?", group)
	}
	apiKeyCredential := `(EXISTS (SELECT 1 FROM account_auth_tokens aat WHERE aat.account_id = accounts.id AND LOWER(aat.auth_method) = 'api_key') OR EXISTS (SELECT 1 FROM account_kiro_credentials akc WHERE akc.account_id = accounts.id AND LOWER(akc.auth_method) = 'api_key'))`
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "api_key":
		appendCondition(apiKeyCredential)
	case "account":
		appendCondition("NOT " + apiKeyCredential)
	}
	var total int
	countQuery := "SELECT COUNT(*) FROM accounts" + where
	if err := s.rdb.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := "SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts" + where + " ORDER BY " + orderBy + " LIMIT ? OFFSET ?"
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE group_name = ? AND status = 'active' ORDER BY created_at, id`, group)
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
		       a.email, a.plan_type, a.provider, a.status, a.is_fedramp, a.ignore_rate_limit_controls, a.force_codex_429, a.quarantine_until,
		       a.routing_weight, a.retry_max_attempts, a.quarantine_reason, a.created_at, a.updated_at,
		       b.account_id, b.primary_egress_id, b.standby_egress_ids, b.sidecar_egress_id, b.binding_scope, b.cookie_jar_key, b.cooldown_until,
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
		WHERE (a.group_name = ? OR EXISTS (
		  SELECT 1 FROM account_group_memberships m
		  WHERE m.account_id = a.id AND m.group_name = ?
		)) AND a.status = 'active'
		ORDER BY a.created_at, a.id
	`, group, group)
	if err != nil {
		return nil, err
	}
	var out []AccountWithEgress
	for rows.Next() {
		var a AccountWithEgress
		var isFedramp int
		var ignoreRateLimitControls int
		var forceCodex429 int
		var recheck int
		var streamCapable int
		err := rows.Scan(
			&a.Account.ID, &a.Account.Label, &a.Account.GroupName, &a.Account.UpstreamAccountID,
			&a.Account.ChatGPTUserID, &a.Account.Email, &a.Account.PlanType, &a.Account.Provider,
			&a.Account.Status, &isFedramp, &ignoreRateLimitControls, &forceCodex429, &a.Account.QuarantineUntil, &a.Account.RoutingWeight, &a.Account.RetryMaxAttempts, &a.Account.QuarantineReason,
			&a.Account.CreatedAt, &a.Account.UpdatedAt,
			&a.Binding.AccountID, &a.Binding.PrimaryEgressID, &a.Binding.StandbyEgressIDs, &a.Binding.SidecarEgressID, &a.Binding.BindingScope, &a.Binding.CookieJarKey,
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
			_ = rows.Close()
			return nil, err
		}
		a.Account.IsFedramp = isFedramp != 0
		a.Account.IgnoreRateLimitControls = ignoreRateLimitControls != 0
		a.Account.ForceCodex429 = forceCodex429 != 0
		// The JOIN guarantees equality, but the account row is the canonical
		// identity for the returned aggregate even on a legacy database.
		a.Binding.AccountID = a.Account.ID
		a.Binding.RecheckPending = recheck != 0
		a.Egress.StreamCapable = streamCapable != 0
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetAccount(ctx context.Context, id string) (Account, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE id = ?`, id)
	return scanAccount(row)
}

// GetAccountByEmail resolves a lifecycle identity without exposing a broad
// account-list scan to callers. Email comparison is case-insensitive and the
// newest row wins for legacy databases that predate unique remote-identity
// enforcement.
func (s *Store) GetAccountByEmail(ctx context.Context, email string) (Account, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return Account{}, sql.ErrNoRows
	}
	row := s.rdb.QueryRowContext(ctx, `
SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status,
	   is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at
FROM accounts
WHERE LOWER(email)=LOWER(?)
ORDER BY updated_at DESC,id
LIMIT 1`, email)
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, label, group_name, upstream_account_id, chatgpt_user_id, email, plan_type, provider, status, is_fedramp, ignore_rate_limit_controls, force_codex_429, routing_weight, retry_max_attempts, quarantine_until, quarantine_reason, created_at, updated_at FROM accounts WHERE id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
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

// SetAccountIgnoreRateLimitControls changes the account-scoped scheduling
// override. Existing cooldown/quarantine records are intentionally retained for
// diagnostics and become effective again as soon as the override is disabled.
func (s *Store) SetAccountIgnoreRateLimitControls(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET ignore_rate_limit_controls = ?, updated_at = ? WHERE id = ?`, boolInt(enabled), Now(), id)
	return err
}

// SetAccountForceCodex429 changes the account-scoped "强制卡429" opt-in. Like
// SetAccountIgnoreRateLimitControls, cooldown/quarantine records are retained
// and become effective again when the override is disabled.
func (s *Store) SetAccountForceCodex429(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET force_codex_429 = ?, updated_at = ? WHERE id = ?`, boolInt(enabled), Now(), id)
	return err
}

// SetAccountRoutingPolicy updates fresh-selection share and bounded same-
// credential retry policy atomically.
func (s *Store) SetAccountRoutingPolicy(ctx context.Context, id string, routingWeight, retryMaxAttempts int) error {
	if routingWeight < 1 || routingWeight > 1000 {
		return fmt.Errorf("routing weight must be between 1 and 1000")
	}
	if retryMaxAttempts < 0 || retryMaxAttempts > 3 {
		return fmt.Errorf("retry max attempts must be between 0 and 3")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE accounts SET routing_weight = ?, retry_max_attempts = ?, updated_at = ? WHERE id = ?`, routingWeight, retryMaxAttempts, Now(), id)
	if err != nil {
		return err
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 0 {
		return sql.ErrNoRows
	}
	return nil
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
		`DELETE FROM affinity_aliases WHERE account_id = ?`,
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
	return s.loadToken(ctx, accountID, true)
}

// GetTokenFresh bypasses the process-local decrypted-token cache. OAuth refresh
// tokens rotate on use, so a second worker that receives invalid_grant must
// re-read the committed database row before declaring the credential dead.
// The fresh value replaces this process's cache after a successful read.
func (s *Store) GetTokenFresh(ctx context.Context, accountID string) (AccountToken, error) {
	s.tokenCache.Delete(accountID)
	return s.loadToken(ctx, accountID, true)
}

func (s *Store) loadToken(ctx context.Context, accountID string, cache bool) (AccountToken, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, auth_method, credential_mode, access_token, refresh_token, openai_api_key, id_token_raw, agent_runtime_id, agent_private_key, agent_task_id, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at FROM account_auth_tokens WHERE account_id = ?`, accountID)
	var t AccountToken
	err := row.Scan(&t.AccountID, &t.AuthMethod, &t.CredentialMode, &t.AccessToken, &t.RefreshToken, &t.OpenAIAPIKey, &t.IDTokenRaw, &t.AgentRuntimeID, &t.AgentPrivateKey, &t.AgentTaskID, &t.LastRefresh, &t.ExpiresAt, &t.Scopes, &t.OAuthRateLimitTier, &t.CreatedAt, &t.UpdatedAt)
	// A field that was stored non-empty but opens empty was rejected by the current
	// key. openToken has no identity to report, but this call site does.
	sealed := []string{t.AccessToken, t.RefreshToken, t.OpenAIAPIKey, t.IDTokenRaw,
		t.AgentRuntimeID, t.AgentPrivateKey, t.AgentTaskID}
	t.AccessToken = s.openToken(t.AccessToken)
	t.RefreshToken = s.openToken(t.RefreshToken)
	t.OpenAIAPIKey = s.openToken(t.OpenAIAPIKey)
	t.IDTokenRaw = s.openToken(t.IDTokenRaw)
	t.AgentRuntimeID = s.openToken(t.AgentRuntimeID)
	t.AgentPrivateKey = s.openToken(t.AgentPrivateKey)
	t.AgentTaskID = s.openToken(t.AgentTaskID)
	opened := []string{t.AccessToken, t.RefreshToken, t.OpenAIAPIKey, t.IDTokenRaw,
		t.AgentRuntimeID, t.AgentPrivateKey, t.AgentTaskID}
	for i := range sealed {
		if sealed[i] != "" && opened[i] == "" {
			s.noteUndecryptableAccount(accountID)
			break
		}
	}
	if err == nil && cache {
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, auth_method, credential_mode, access_token, refresh_token, openai_api_key, id_token_raw, agent_runtime_id, agent_private_key, agent_task_id, last_refresh, expires_at, scopes, oauth_rate_limit_tier, created_at, updated_at FROM account_auth_tokens WHERE account_id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t AccountToken
		if err := rows.Scan(&t.AccountID, &t.AuthMethod, &t.CredentialMode, &t.AccessToken, &t.RefreshToken, &t.OpenAIAPIKey, &t.IDTokenRaw, &t.AgentRuntimeID, &t.AgentPrivateKey, &t.AgentTaskID, &t.LastRefresh, &t.ExpiresAt, &t.Scopes, &t.OAuthRateLimitTier, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.AccessToken = s.openToken(t.AccessToken)
		t.RefreshToken = s.openToken(t.RefreshToken)
		t.OpenAIAPIKey = s.openToken(t.OpenAIAPIKey)
		t.IDTokenRaw = s.openToken(t.IDTokenRaw)
		t.AgentRuntimeID = s.openToken(t.AgentRuntimeID)
		t.AgentPrivateKey = s.openToken(t.AgentPrivateKey)
		t.AgentTaskID = s.openToken(t.AgentTaskID)
		out[t.AccountID] = t
	}
	return out, rows.Err()
}

func (s *Store) UpdateToken(ctx context.Context, t AccountToken) error {
	s.tokenCache.Delete(t.AccountID)
	_, err := s.db.ExecContext(ctx, `UPDATE account_auth_tokens SET auth_method = ?, credential_mode = ?, access_token = ?, refresh_token = ?, openai_api_key = ?, id_token_raw = ?, agent_runtime_id = ?, agent_private_key = ?, agent_task_id = ?, last_refresh = ?, expires_at = ?, scopes = ?, oauth_rate_limit_tier = ?, updated_at = ? WHERE account_id = ?`,
		t.AuthMethod, t.CredentialMode, s.sealToken(t.AccessToken), s.sealToken(t.RefreshToken), s.sealToken(t.OpenAIAPIKey), s.sealToken(t.IDTokenRaw), s.sealToken(t.AgentRuntimeID), s.sealToken(t.AgentPrivateKey), s.sealToken(t.AgentTaskID), t.LastRefresh, t.ExpiresAt, t.Scopes, t.OAuthRateLimitTier, Now(), t.AccountID)
	return err
}

// UpdateTokenAfterCredentialRefresh persists a successfully refreshed credential and
// atomically returns an auth_expired account to service. The conditional status
// transition deliberately preserves explicit administrative states such as disabled
// or invalid. Clearing the binding bench in the same transaction prevents a recovered
// credential from remaining invisible to the scheduler.
func (s *Store) UpdateTokenAfterCredentialRefresh(ctx context.Context, t AccountToken) (bool, error) {
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE account_auth_tokens SET auth_method = ?, credential_mode = ?, access_token = ?, refresh_token = ?, openai_api_key = ?, id_token_raw = ?, agent_runtime_id = ?, agent_private_key = ?, agent_task_id = ?, last_refresh = ?, expires_at = ?, scopes = ?, oauth_rate_limit_tier = ?, updated_at = ? WHERE account_id = ?`,
		t.AuthMethod, t.CredentialMode, s.sealToken(t.AccessToken), s.sealToken(t.RefreshToken), s.sealToken(t.OpenAIAPIKey), s.sealToken(t.IDTokenRaw), s.sealToken(t.AgentRuntimeID), s.sealToken(t.AgentPrivateKey), s.sealToken(t.AgentTaskID), t.LastRefresh, t.ExpiresAt, t.Scopes, t.OAuthRateLimitTier, now, t.AccountID)
	if err != nil {
		return false, err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return false, rowsErr
	} else if affected != 1 {
		return false, sql.ErrNoRows
	}

	statusResult, err := tx.ExecContext(ctx, `UPDATE accounts SET status = 'active', quarantine_until = 0, quarantine_reason = '', updated_at = ? WHERE id = ? AND status = 'auth_expired'`, now, t.AccountID)
	if err != nil {
		return false, err
	}
	affected, err := statusResult.RowsAffected()
	if err != nil {
		return false, err
	}
	reactivated := affected == 1
	if reactivated {
		if _, err := tx.ExecContext(ctx, `UPDATE account_egress_bindings SET cooldown_until = 0, recheck_pending = 0, updated_at = ? WHERE account_id = ?`, now, t.AccountID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.tokenCache.Delete(t.AccountID)
	return reactivated, nil
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

// AntigravityCredentials holds per-account OAuth and project metadata for the
// Antigravity (Google Cloud Code) upstream provider.
type AntigravityCredentials struct {
	AccountID    string
	Email        string
	ProjectID    string
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	BaseURL      string // empty = production endpoint
	UserAgent    string // empty = default Antigravity UA
	CreatedAt    int64
	UpdatedAt    int64
}

func (s *Store) UpsertAntigravityCredentials(ctx context.Context, c AntigravityCredentials) error {
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.upsertAntigravityCredentialsTx(ctx, tx, c, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) upsertAntigravityCredentialsTx(ctx context.Context, tx *sql.Tx, c AntigravityCredentials, now int64) error {
	if strings.TrimSpace(c.AccountID) == "" {
		return errors.New("antigravity account id required")
	}
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := tx.ExecContext(ctx, `
		INSERT INTO account_antigravity_credentials
		  (account_id, email, project_id, access_token, refresh_token, expires_at, base_url, user_agent, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id) DO UPDATE SET
		  email = excluded.email, project_id = excluded.project_id,
		  access_token = excluded.access_token, refresh_token = excluded.refresh_token,
		  expires_at = excluded.expires_at, base_url = excluded.base_url,
		  user_agent = excluded.user_agent, updated_at = excluded.updated_at`,
		c.AccountID, c.Email, c.ProjectID, s.sealToken(c.AccessToken), s.sealToken(c.RefreshToken),
		c.ExpiresAt, c.BaseURL, c.UserAgent, c.CreatedAt, c.UpdatedAt,
	)
	return err
}

func (s *Store) GetAntigravityCredentials(ctx context.Context, accountID string) (AntigravityCredentials, error) {
	var c AntigravityCredentials
	err := s.rdb.QueryRowContext(ctx, `
		SELECT account_id, email, project_id, access_token, refresh_token, expires_at, base_url, user_agent, created_at, updated_at
		FROM account_antigravity_credentials WHERE account_id = ?`, accountID,
	).Scan(&c.AccountID, &c.Email, &c.ProjectID, &c.AccessToken, &c.RefreshToken,
		&c.ExpiresAt, &c.BaseURL, &c.UserAgent, &c.CreatedAt, &c.UpdatedAt)
	if err == nil {
		c.AccessToken = s.openToken(c.AccessToken)
		c.RefreshToken = s.openToken(c.RefreshToken)
	}
	return c, err
}

// AntigravityCacheEntry tracks a Gemini explicit CachedContent resource created for
// a given (account, model, conversation-prefix) triple. Subsequent turns in the same
// conversation reference the resource name so Gemini serves the prefix from its KV
// cache instead of re-ingesting it. Expired entries are pruned before each request.
type AntigravityCacheEntry struct {
	ID                int64
	AccountID         string
	ModelID           string
	ConvKeyHash       string // FNV-64a hex of accountID+"\x00"+modelID+"\x00"+stablePrefix
	CacheResourceName string // e.g. "projects/.../cachedContents/abc123"
	TotalTokens       int64
	ExpiresAt         int64 // Unix seconds; 0 = unknown
	CreatedAt         int64
	UpdatedAt         int64
}

// GetAntigravityCacheEntry returns the cache entry for (account, model, hash), or
// (zero, false, nil) when none exists or the entry has already expired.
func (s *Store) GetAntigravityCacheEntry(ctx context.Context, accountID, modelID, convKeyHash string) (AntigravityCacheEntry, bool, error) {
	var e AntigravityCacheEntry
	err := s.rdb.QueryRowContext(ctx, `
		SELECT id, account_id, model_id, conv_key_hash, cache_resource_name, total_tokens, expires_at, created_at, updated_at
		FROM antigravity_cache_entries
		WHERE account_id=? AND model_id=? AND conv_key_hash=?`,
		accountID, modelID, convKeyHash,
	).Scan(&e.ID, &e.AccountID, &e.ModelID, &e.ConvKeyHash, &e.CacheResourceName,
		&e.TotalTokens, &e.ExpiresAt, &e.CreatedAt, &e.UpdatedAt)
	if err == sql.ErrNoRows {
		return AntigravityCacheEntry{}, false, nil
	}
	if err != nil {
		return AntigravityCacheEntry{}, false, err
	}
	// Treat as expired if expires_at is set and already past.
	if e.ExpiresAt > 0 && e.ExpiresAt <= Now() {
		return AntigravityCacheEntry{}, false, nil
	}
	return e, true, nil
}

// UpsertAntigravityCacheEntry inserts or replaces the cache entry.
func (s *Store) UpsertAntigravityCacheEntry(ctx context.Context, e AntigravityCacheEntry) error {
	now := Now()
	if e.CreatedAt == 0 {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO antigravity_cache_entries
		  (account_id, model_id, conv_key_hash, cache_resource_name, total_tokens, expires_at, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, model_id, conv_key_hash) DO UPDATE SET
		  cache_resource_name=excluded.cache_resource_name,
		  total_tokens=excluded.total_tokens,
		  expires_at=excluded.expires_at,
		  updated_at=excluded.updated_at`,
		e.AccountID, e.ModelID, e.ConvKeyHash, e.CacheResourceName,
		e.TotalTokens, e.ExpiresAt, e.CreatedAt, e.UpdatedAt,
	)
	return err
}

// DeleteAntigravityCacheEntry removes the entry for (account, model, hash). Safe to
// call when the entry does not exist.
func (s *Store) DeleteAntigravityCacheEntry(ctx context.Context, accountID, modelID, convKeyHash string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM antigravity_cache_entries WHERE account_id=? AND model_id=? AND conv_key_hash=?`,
		accountID, modelID, convKeyHash)
	return err
}

// PruneExpiredAntigravityCacheEntries deletes all rows whose expires_at is in the past.
func (s *Store) PruneExpiredAntigravityCacheEntries(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM antigravity_cache_entries WHERE expires_at > 0 AND expires_at <= ?`, Now())
	return err
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

// SetTokenEncryptionKey is the compatibility entry point used by tests and older
// embedders. New deployments should call SetTokenMasterKey with a persistent key.
func (s *Store) SetTokenEncryptionKey(secret []byte) {
	if len(secret) == 0 {
		return
	}
	_ = s.SetTokenMasterKey(secretbox.DeriveKey(secret))
}

// SetTokenMasterKey installs an exact 32-byte primary key plus any exact legacy
// keys that remain available during a bounded rotation migration.
func (s *Store) SetTokenMasterKey(primary []byte, legacy ...[]byte) error {
	if len(primary) != 32 {
		return errors.New("storage master key must contain exactly 32 bytes")
	}
	s.tokenKey = append([]byte(nil), primary...)
	s.tokenKeys = [][]byte{s.tokenKey}
	for _, key := range legacy {
		if len(key) != 32 {
			return errors.New("legacy storage key must contain exactly 32 bytes")
		}
		s.tokenKeys = append(s.tokenKeys, append([]byte(nil), key...))
	}
	s.cryptoStrict = false
	s.cryptoErrMu.Lock()
	s.cryptoErr = nil
	s.cryptoErrMu.Unlock()
	return nil
}

// EnableStrictEncryption rejects any later plaintext secret read. It is called
// only after the startup migration and sentinel validation succeed.
func (s *Store) EnableStrictEncryption() {
	s.cryptoStrict = true
}

func (s *Store) recordCryptoError(err error) {
	if err == nil {
		return
	}
	s.cryptoErrMu.Lock()
	if s.cryptoErr == nil {
		s.cryptoErr = err
	}
	s.cryptoErrMu.Unlock()
}

func (s *Store) CryptoError() error {
	s.cryptoErrMu.Lock()
	defer s.cryptoErrMu.Unlock()
	return s.cryptoErr
}

// maxUndecryptableAccountsTracked bounds the identity list. Past this many distinct
// accounts the problem is the key, not the accounts, and the list has already told
// the operator everything it can.
const maxUndecryptableAccountsTracked = 512

// noteUndecryptableAccount records that an account's stored secret failed to open.
func (s *Store) noteUndecryptableAccount(accountID string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return
	}
	s.undecryptableMu.Lock()
	defer s.undecryptableMu.Unlock()
	if s.undecryptable == nil {
		s.undecryptable = make(map[string]int, 8)
	}
	_, seen := s.undecryptable[accountID]
	if !seen && len(s.undecryptable) >= maxUndecryptableAccountsTracked {
		return
	}
	s.undecryptable[accountID]++
	if !seen {
		// Once per account, not per read: this is discovered lazily on every token load,
		// so logging each occurrence would bury the signal it exists to raise. The
		// startup CryptoError gate cannot catch these — it runs before any account token
		// is read.
		log.Printf("[SECURITY] account %s has stored secrets that cannot be decrypted with the current key; it will present empty credentials until re-imported or re-authorized", accountID)
	}
}

// UndecryptableAccountIDs returns the accounts whose stored secrets could not be
// opened with the current key, sorted. Empty is the healthy case.
//
// This is the actionable half of CryptoError: that reports *that* decryption failed,
// this reports *what* to re-import or re-authorize. A rotated or per-boot-generated
// key turns every affected account into one that looks configured but presents empty
// credentials, so without the list the failure surfaces only as unexplained routing
// errors.
func (s *Store) UndecryptableAccountIDs() []string {
	s.undecryptableMu.Lock()
	defer s.undecryptableMu.Unlock()
	if len(s.undecryptable) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.undecryptable))
	for id := range s.undecryptable {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sealToken never falls back to plaintext after a master key is installed.
func (s *Store) sealToken(v string) string {
	if v == "" || len(s.tokenKey) == 0 {
		return v
	}
	out, err := secretbox.Seal(s.tokenKey, v)
	if err != nil {
		s.recordCryptoError(err)
		return ""
	}
	return out
}

// openToken fails closed: ciphertext is never returned as an upstream credential.
func (s *Store) openToken(v string) string {
	if v == "" || len(s.tokenKey) == 0 {
		return v
	}
	if s.cryptoStrict && !secretbox.IsSealed(v) {
		s.recordCryptoError(errors.New("plaintext secret encountered after encryption migration"))
		return ""
	}
	out, err := secretbox.OpenDomainWithKeys(s.tokenKeys, secretbox.DefaultDomain, v)
	if err != nil {
		s.recordCryptoError(err)
		return ""
	}
	return out
}

func (s *Store) resealToken(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	plain, err := secretbox.OpenDomainWithKeys(s.tokenKeys, secretbox.DefaultDomain, v)
	if err != nil {
		return "", err
	}
	return secretbox.Seal(s.tokenKey, plain)
}

func (s *Store) resealTokens(values ...string) ([]string, error) {
	out := make([]string, len(values))
	for i, value := range values {
		sealed, err := s.resealToken(value)
		if err != nil {
			return nil, err
		}
		out[i] = sealed
	}
	return out, nil
}

// EncryptExistingTokens re-encrypts plaintext and legacy-key rows in all core
// credential tables. A value that cannot be decrypted aborts startup; it is never
// copied forward or treated as a token.
func (s *Store) EncryptExistingTokens(ctx context.Context) (int, error) {
	if len(s.tokenKey) == 0 {
		return 0, nil
	}
	n := 0
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, access_token, refresh_token, openai_api_key, id_token_raw, agent_runtime_id, agent_private_key, agent_task_id FROM account_auth_tokens`)
	if err != nil {
		return 0, err
	}
	type rec struct{ id, at, rt, ak, it, ar, ap, task string }
	var pending []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.at, &r.rt, &r.ak, &r.it, &r.ar, &r.ap, &r.task); err != nil {
			rows.Close()
			return 0, err
		}
		// Only rows with at least one non-empty, not-yet-sealed secret need an upgrade.
		if anyNonCurrentSecret(s.tokenKey, r.at, r.rt, r.ak, r.it, r.ar, r.ap, r.task) {
			pending = append(pending, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range pending {
		sealed, sealErr := s.resealTokens(r.at, r.rt, r.ak, r.it, r.ar, r.ap, r.task)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt account credentials %s: %w", r.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_auth_tokens SET access_token = ?, refresh_token = ?, openai_api_key = ?, id_token_raw = ?, agent_runtime_id = ?, agent_private_key = ?, agent_task_id = ? WHERE account_id = ?`,
			sealed[0], sealed[1], sealed[2], sealed[3], sealed[4], sealed[5], sealed[6], r.id); err != nil {
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
		if anyNonCurrentSecret(s.tokenKey, r.secret) {
			keyPending = append(keyPending, r)
		}
	}
	keyRows.Close()
	if err := keyRows.Err(); err != nil {
		return n, err
	}
	for _, r := range keyPending {
		sealed, sealErr := s.resealToken(r.secret)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt downstream key %s: %w", r.hash, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE api_keys SET secret = ?, updated_at = ? WHERE key_hash = ?`, sealed, Now(), r.hash); err != nil {
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
		if anyNonCurrentSecret(s.tokenKey, r.secret, r.apiKey) {
			kiroPending = append(kiroPending, r)
		}
	}
	kiroRows.Close()
	if err := kiroRows.Err(); err != nil {
		return n, err
	}
	for _, r := range kiroPending {
		sealed, sealErr := s.resealTokens(r.secret, r.apiKey)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt Kiro credentials %s: %w", r.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_kiro_credentials SET client_secret=?, kiro_api_key=? WHERE account_id=?`, sealed[0], sealed[1], r.id); err != nil {
			return n, err
		}
		n++
	}
	antigravityRows, err := s.rdb.QueryContext(ctx, `SELECT account_id, access_token, refresh_token FROM account_antigravity_credentials`)
	if err != nil {
		return n, err
	}
	type antigravityRec struct{ id, accessToken, refreshToken string }
	var antigravityPending []antigravityRec
	for antigravityRows.Next() {
		var r antigravityRec
		if err := antigravityRows.Scan(&r.id, &r.accessToken, &r.refreshToken); err != nil {
			antigravityRows.Close()
			return n, err
		}
		if anyNonCurrentSecret(s.tokenKey, r.accessToken, r.refreshToken) {
			antigravityPending = append(antigravityPending, r)
		}
	}
	antigravityRows.Close()
	if err := antigravityRows.Err(); err != nil {
		return n, err
	}
	for _, r := range antigravityPending {
		sealed, sealErr := s.resealTokens(r.accessToken, r.refreshToken)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt Antigravity credentials %s: %w", r.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_antigravity_credentials SET access_token=?, refresh_token=?, updated_at=? WHERE account_id=?`, sealed[0], sealed[1], Now(), r.id); err != nil {
			return n, err
		}
		n++
	}

	sessionRows, err := s.rdb.QueryContext(ctx, `SELECT account_id, cookie FROM account_session_cookies WHERE cookie <> ''`)
	if err != nil {
		return n, err
	}
	var sessions []struct{ id, cookie string }
	for sessionRows.Next() {
		var row struct{ id, cookie string }
		if err := sessionRows.Scan(&row.id, &row.cookie); err != nil {
			sessionRows.Close()
			return n, err
		}
		if anyNonCurrentSecret(s.tokenKey, row.cookie) {
			sessions = append(sessions, row)
		}
	}
	sessionRows.Close()
	if err := sessionRows.Err(); err != nil {
		return n, err
	}
	for _, row := range sessions {
		sealed, sealErr := s.resealToken(row.cookie)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt session cookie %s: %w", row.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_session_cookies SET cookie=?,updated_at=? WHERE account_id=?`, sealed, Now(), row.id); err != nil {
			return n, err
		}
		n++
	}

	injectedRows, err := s.rdb.QueryContext(ctx, `SELECT account_id,egress_id,upstream_host,cookie_header FROM account_injected_cookies WHERE cookie_header <> ''`)
	if err != nil {
		return n, err
	}
	type injectedRec struct{ accountID, egressID, host, cookie string }
	var injected []injectedRec
	for injectedRows.Next() {
		var row injectedRec
		if err := injectedRows.Scan(&row.accountID, &row.egressID, &row.host, &row.cookie); err != nil {
			injectedRows.Close()
			return n, err
		}
		if anyNonCurrentSecret(s.tokenKey, row.cookie) {
			injected = append(injected, row)
		}
	}
	injectedRows.Close()
	if err := injectedRows.Err(); err != nil {
		return n, err
	}
	for _, row := range injected {
		sealed, sealErr := s.resealToken(row.cookie)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt injected cookie %s: %w", row.accountID, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_injected_cookies SET cookie_header=?,updated_at=? WHERE account_id=? AND egress_id=? AND upstream_host=?`, sealed, Now(), row.accountID, row.egressID, row.host); err != nil {
			return n, err
		}
		n++
	}

	reauthRows, err := s.rdb.QueryContext(ctx, `SELECT account_id,encrypted_password,encrypted_otp_url FROM account_codex_reauth_config`)
	if err != nil {
		return n, err
	}
	type reauthRec struct{ id, password, otp string }
	var reauth []reauthRec
	for reauthRows.Next() {
		var row reauthRec
		if err := reauthRows.Scan(&row.id, &row.password, &row.otp); err != nil {
			reauthRows.Close()
			return n, err
		}
		if anyNonCurrentSecret(s.tokenKey, row.password, row.otp) {
			reauth = append(reauth, row)
		}
	}
	reauthRows.Close()
	if err := reauthRows.Err(); err != nil {
		return n, err
	}
	for _, row := range reauth {
		sealed, sealErr := s.resealTokens(row.password, row.otp)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt reauth credentials %s: %w", row.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE account_codex_reauth_config SET encrypted_password=?,encrypted_otp_url=?,updated_at=? WHERE account_id=?`, sealed[0], sealed[1], Now(), row.id); err != nil {
			return n, err
		}
		n++
	}

	emailRows, err := s.rdb.QueryContext(ctx, `
SELECT COALESCE(id,''),COALESCE(password,''),COALESCE(client_id,''),COALESCE(refresh_token,'')
FROM email_pool
WHERE COALESCE(password,'')<>'' OR COALESCE(client_id,'')<>'' OR COALESCE(refresh_token,'')<>''`)
	if err != nil {
		return n, err
	}
	type emailCredentialRec struct {
		id, password, clientID, refreshToken string
	}
	var emailCredentials []emailCredentialRec
	for emailRows.Next() {
		var row emailCredentialRec
		if err := emailRows.Scan(&row.id, &row.password, &row.clientID, &row.refreshToken); err != nil {
			emailRows.Close()
			return n, err
		}
		if anyNonCurrentSecret(s.tokenKey, row.password, row.clientID, row.refreshToken) {
			emailCredentials = append(emailCredentials, row)
		}
	}
	emailRows.Close()
	if err := emailRows.Err(); err != nil {
		return n, err
	}
	for _, row := range emailCredentials {
		sealed, sealErr := s.resealTokens(row.password, row.clientID, row.refreshToken)
		if sealErr != nil {
			return n, fmt.Errorf("re-encrypt email pool credentials %s: %w", row.id, sealErr)
		}
		if _, err := s.db.ExecContext(ctx, `
UPDATE email_pool
SET password=?,client_id=?,refresh_token=?,updated_at=?
WHERE id=?`, sealed[0], sealed[1], sealed[2], Now(), row.id); err != nil {
			return n, err
		}
		n++
	}
	providerRows, err := s.migrateProviderSecrets(ctx)
	if err != nil {
		return n, err
	}
	n += providerRows
	return n, nil
}

func anyNonCurrentSecret(key []byte, vals ...string) bool {
	for _, v := range vals {
		if v != "" && !secretbox.IsCurrent(key, v) {
			return true
		}
	}
	return false
}

// anyPlaintextSecret is retained for callers/tests that only care whether a row
// predates encryption.
func anyPlaintextSecret(vals ...string) bool {
	for _, v := range vals {
		if v != "" && !secretbox.IsSealed(v) {
			return true
		}
	}
	return false
}

const encryptionSentinelSetting = "_encryption_sentinel_v2"

// ValidateEncryptionSentinel proves that the configured master key can decrypt
// persistent state. A missing sentinel is created once; a wrong or missing key
// fails startup rather than allowing ciphertext to reach an upstream.
func (s *Store) ValidateEncryptionSentinel(ctx context.Context) error {
	if len(s.tokenKey) != 32 {
		return errors.New("persistent storage master key is not configured")
	}
	const marker = "codex-pool-encryption-sentinel"
	var stored string
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, encryptionSentinelSetting).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		sealed, sealErr := secretbox.SealDomain(s.tokenKey, "sentinel", marker)
		if sealErr != nil {
			return sealErr
		}
		_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, encryptionSentinelSetting, sealed, Now())
		s.InvalidateSettingsCache()
		return err
	}
	if err != nil {
		return err
	}
	plain, err := secretbox.OpenDomainWithKeys(s.tokenKeys, "sentinel", stored)
	if err != nil {
		return fmt.Errorf("decrypt encryption sentinel: %w", err)
	}
	if plain != marker {
		return errors.New("encryption sentinel validation failed")
	}
	if !secretbox.IsCurrent(s.tokenKey, stored) {
		sealed, sealErr := secretbox.SealDomain(s.tokenKey, "sentinel", marker)
		if sealErr != nil {
			return sealErr
		}
		if _, err = s.db.ExecContext(ctx, `UPDATE settings SET value=?,updated_at=? WHERE key=?`, sealed, Now(), encryptionSentinelSetting); err != nil {
			return err
		}
		s.InvalidateSettingsCache()
	}
	return nil
}

// CheckWritable is the readiness DB probe. It performs a tiny transactional
// write so read-only mounts and exhausted databases are not reported ready.
func (s *Store) CheckWritable(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE settings SET updated_at=updated_at WHERE key=?`, encryptionSentinelSetting); err != nil {
		return err
	}
	return tx.Commit()
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
		c.AccountID, c.EgressID, c.UpstreamHost, s.sealToken(c.CookieHeader), c.UserAgent, c.ExitIP, Now())
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
		c.CookieHeader = s.openToken(c.CookieHeader)
		out = append(out, c)
	}
	return out, rows.Err()
}

func normalizeCapabilityAvailability(state, source string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "verified", "unverified", "unsupported":
		return strings.ToLower(strings.TrimSpace(state))
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(source)), "static") || strings.Contains(strings.ToLower(strings.TrimSpace(source)), "unknown") {
		return "unverified"
	}
	return "verified"
}

func normalizeContext1MState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "supported", "unsupported", "unknown":
		return strings.ToLower(strings.TrimSpace(state))
	default:
		return "unknown"
	}
}

func capabilityProvesAuthoritativeCatalog(c ModelCapability) bool {
	if c.AvailabilityState != "verified" {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(c.Source))
	if strings.Contains(source, "probe") && !strings.Contains(source, "static") && !strings.Contains(source, "unknown") {
		return true
	}
	return !strings.Contains(source, "runtime") &&
		!strings.Contains(source, "static") &&
		!strings.Contains(source, "unknown")
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
	authoritative := map[string]bool{}
	for _, c := range capabilities {
		if deleted[c.AccountID] {
			continue
		}
		deleted[c.AccountID] = true
		authoritative[c.AccountID] = false
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_model_capabilities WHERE account_id = ?`, c.AccountID); err != nil {
			return err
		}
	}
	for _, c := range capabilities {
		if c.LastProbeAt == 0 {
			c.LastProbeAt = Now()
		}
		c.AvailabilityState = normalizeCapabilityAvailability(c.AvailabilityState, c.Source)
		c.Context1MState = normalizeContext1MState(c.Context1MState)
		if capabilityProvesAuthoritativeCatalog(c) {
			authoritative[c.AccountID] = true
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO account_model_capabilities(account_id, model_slug, availability_state, context_1m_state, context_1m_source, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id, model_slug) DO UPDATE SET
 availability_state = excluded.availability_state,
 context_1m_state = excluded.context_1m_state,
 context_1m_source = excluded.context_1m_source,
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
			c.AccountID, c.ModelSlug, c.AvailabilityState, c.Context1MState, c.Context1MSource, c.NativeContextWindow, c.NativeMaxContextWindow, c.EffectiveContextWindowPercent, c.AutoCompactTokenLimit, c.Visibility, c.ETag, c.RawModelJSONHash, c.RawModelJSON, c.Source, c.LastProbeAt)
		if err != nil {
			return err
		}
	}
	for accountID, isAuthoritative := range authoritative {
		value := 0
		if isAuthoritative {
			value = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_model_catalog_status(account_id, authoritative, last_probe_at)
VALUES(?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET authoritative=excluded.authoritative, last_probe_at=excluded.last_probe_at`, accountID, value, Now()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceCapabilities persists the complete authoritative catalog for one account.
// Unlike UpsertCapabilities it can represent a successful empty /models response,
// which must remove stale rows rather than silently retaining a static fallback.
func (s *Store) ReplaceCapabilities(ctx context.Context, accountID string, capabilities []ModelCapability) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return errors.New("capability account id is required")
	}
	if len(capabilities) == 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_model_capabilities WHERE account_id = ?`, accountID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_model_catalog_status(account_id, authoritative, last_probe_at)
VALUES(?, 1, ?)
ON CONFLICT(account_id) DO UPDATE SET authoritative=1, last_probe_at=excluded.last_probe_at`, accountID, Now()); err != nil {
			return err
		}
		return tx.Commit()
	}
	for i := range capabilities {
		if capabilities[i].AccountID == "" {
			capabilities[i].AccountID = accountID
		}
		if capabilities[i].AccountID != accountID {
			return errors.New("capability replacement spans multiple accounts")
		}
	}
	if err := s.UpsertCapabilities(ctx, capabilities); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_model_catalog_status(account_id, authoritative, last_probe_at)
VALUES(?, 1, ?)
ON CONFLICT(account_id) DO UPDATE SET authoritative=1, last_probe_at=excluded.last_probe_at`, accountID, Now())
	return err
}

// SetModelCapabilityState records account/model-scoped runtime evidence without
// touching account quarantine. In particular, a model_not_found response marks
// only that model unsupported, while a successful inference promotes a static
// discovery hint to verified.
func (s *Store) SetModelCapabilityState(ctx context.Context, accountID, model, availability, context1MState, context1MSource, evidenceSource string) error {
	availability = normalizeCapabilityAvailability(availability, "runtime")
	context1MState = normalizeContext1MState(context1MState)
	evidenceSource = strings.TrimSpace(evidenceSource)
	if evidenceSource == "" {
		if availability == "verified" {
			evidenceSource = "runtime_inference"
		} else {
			evidenceSource = "runtime_rejected"
		}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_model_capabilities(
account_id, model_slug, availability_state, context_1m_state, context_1m_source,
native_context_window, native_max_context_window, effective_context_window_percent,
auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at)
VALUES(?, ?, ?, ?, ?, 0, 0, 100, 0, '', '', '', '', ?, ?)
ON CONFLICT(account_id, model_slug) DO UPDATE SET
 availability_state = excluded.availability_state,
 context_1m_state = CASE WHEN excluded.context_1m_state = 'unknown' THEN account_model_capabilities.context_1m_state ELSE excluded.context_1m_state END,
 context_1m_source = CASE WHEN excluded.context_1m_source = '' THEN account_model_capabilities.context_1m_source ELSE excluded.context_1m_source END,
 source = CASE
   WHEN instr(account_model_capabilities.source, excluded.source) > 0 THEN account_model_capabilities.source
   WHEN lower(account_model_capabilities.source) LIKE '%probe%'
     AND lower(account_model_capabilities.source) NOT LIKE '%static%'
     AND lower(account_model_capabilities.source) NOT LIKE '%unknown%'
     THEN account_model_capabilities.source || '+' || excluded.source
   ELSE excluded.source END,
 last_probe_at = excluded.last_probe_at`,
		accountID, model, availability, context1MState, context1MSource,
		evidenceSource, Now())
	return err
}

// AccountsWithModelAndContext answers a single question: which accounts in this
// group can serve this model at all. It deliberately does NOT consider transient
// health (cooldown, recheck_pending, quarantine, primary-egress breaker state).
//
// Mixing health into this query produced two operator-visible lies, both recorded
// in production diagnostics:
//
//  1. The scheduler's only use of this map is the "does this account support the
//     model" gate, so a benched-but-capable account fell out of the map and was
//     reported as `model_unsupported`. An operator whose one provider account was
//     cooling was told their admin-verified model was unsupported — 716 audit rows
//     of `Routing rejected normalized model … model_unsupported=1` for a model the
//     capability table listed as `verified`.
//  2. shortestCooldown/shortestCooldownBatch skip accounts outside this map before
//     they account for cooldowns, so an account excluded *because* it was cooling
//     could never become a wait target. "All accounts cooling" degraded into
//     "no capable account", and the request failed immediately instead of waiting
//     out a cooldown that was about to elapse.
//
// Every caller re-checks liveness independently and with correct attribution
// (evaluateIndexedCandidate, pressureModelEligible, tryLeaseAccountDetailed), so
// the predicates removed here were redundant as well as misattributed.
//
// The bound-sidecar clause stays: an account whose declared curl_cffi transport is
// missing, malformed, or disabled has no usable path to the upstream at all, so it
// must not advertise the capability (fail closed — see
// TestUnavailableSidecarDoesNotAdvertiseRoutableCapabilities).
func (s *Store) AccountsWithModelAndContext(ctx context.Context, group, model, contextMode string) (map[string]bool, error) {
	if model == "" {
		return nil, nil
	}
	now := Now()
	query := `SELECT DISTINCT c.account_id FROM account_model_capabilities c
JOIN accounts a ON a.id = c.account_id
LEFT JOIN account_egress_bindings b ON b.account_id = a.id
LEFT JOIN egress_profiles se ON se.id = b.sidecar_egress_id
WHERE a.group_name = ?
  AND c.model_slug = ? AND c.availability_state = 'verified'
  AND (b.account_id IS NULL OR b.sidecar_egress_id = '' OR
       (se.id IS NOT NULL AND lower(se.type) = 'curl_cffi_sidecar' AND trim(se.endpoint) <> ''
        AND se.health NOT IN ('disabled','tripped') AND se.cooldown_until <= ?))`
	args := []interface{}{group, model, now}
	if strings.EqualFold(strings.TrimSpace(contextMode), "1m") {
		query += ` AND c.context_1m_state = 'supported'`
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
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

func (s *Store) ListCapabilities(ctx context.Context, accountID string) ([]ModelCapability, error) {
	query := `SELECT account_id, model_slug, availability_state, context_1m_state, context_1m_source, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at FROM account_model_capabilities`
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
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.AvailabilityState, &c.Context1MState, &c.Context1MSource, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.RawModelJSON, &c.Source, &c.LastProbeAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRoutableCapabilities returns only verified capabilities backed by an active
// account and at least one egress the scheduler can select now. Accounts with the
// account-local rate-limit override remain visible despite their own quarantine,
// cooldown, or recheck state; disabled accounts and unhealthy shared egresses stay
// excluded.
func (s *Store) ListRoutableCapabilities(ctx context.Context, group string) ([]ModelCapability, error) {
	now := Now()
	groupConfig, err := s.GetGroup(ctx, group)
	if err != nil {
		return nil, err
	}
	groupEgressID := strings.TrimSpace(groupConfig.DefaultEgressID)
	for _, id := range groupConfig.EgressIDs {
		if id = strings.TrimSpace(id); id != "" {
			groupEgressID = id
			break
		}
	}
	if groupEgressID == "" {
		groupEgressID = DefaultDirectEgressID
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT c.account_id, c.model_slug, c.availability_state, c.context_1m_state, c.context_1m_source,
c.native_context_window, c.native_max_context_window, c.effective_context_window_percent, c.auto_compact_token_limit,
c.visibility, c.etag, c.raw_model_json_hash, c.raw_model_json, c.source, c.last_probe_at,
b.primary_egress_id, b.standby_egress_ids, b.sidecar_egress_id, b.binding_scope, b.cooldown_until,
a.ignore_rate_limit_controls
FROM account_model_capabilities c
JOIN accounts a ON a.id = c.account_id
JOIN account_egress_bindings b ON b.account_id = a.id
WHERE a.group_name = ? AND a.status = 'active'
  AND (a.quarantine_until <= ? OR a.ignore_rate_limit_controls = 1)
  AND (b.recheck_pending = 0 OR a.ignore_rate_limit_controls = 1)
  AND c.availability_state = 'verified'
ORDER BY c.account_id, c.model_slug`, group, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		capability              ModelCapability
		binding                 AccountEgressBinding
		ignoreRateLimitControls bool
	}
	var candidates []candidate
	for rows.Next() {
		var c ModelCapability
		var binding AccountEgressBinding
		var ignoreRateLimitControls int
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.AvailabilityState, &c.Context1MState, &c.Context1MSource,
			&c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit,
			&c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.RawModelJSON, &c.Source, &c.LastProbeAt,
			&binding.PrimaryEgressID, &binding.StandbyEgressIDs, &binding.SidecarEgressID, &binding.BindingScope, &binding.CooldownUntil,
			&ignoreRateLimitControls); err != nil {
			return nil, err
		}
		binding.AccountID = c.AccountID
		candidates = append(candidates, candidate{capability: c, binding: binding, ignoreRateLimitControls: ignoreRateLimitControls != 0})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	routable := map[string]bool{}
	checked := map[string]bool{}
	egresses := map[string]EgressProfile{}
	loadEgress := func(id string) (EgressProfile, bool) {
		id = strings.TrimSpace(id)
		if id == "" {
			return EgressProfile{}, false
		}
		if egress, ok := egresses[id]; ok {
			return egress, true
		}
		egress, err := s.GetEgressProfile(ctx, id)
		if err != nil {
			return EgressProfile{}, false
		}
		egresses[id] = egress
		return egress, true
	}
	egressHealthy := func(egress EgressProfile) bool {
		if egress.CooldownUntil > now {
			return false
		}
		switch egress.Health {
		case "", "healthy", "cooldown", "tripped":
			return true
		default:
			return false
		}
	}
	var out []ModelCapability
	for _, item := range candidates {
		accountID := item.capability.AccountID
		if !checked[accountID] {
			checked[accountID] = true
			binding := item.binding
			if !strings.EqualFold(strings.TrimSpace(binding.BindingScope), EgressBindingScopeAccount) {
				binding.PrimaryEgressID = groupEgressID
			}
			binding.StandbyEgressIDs = ""
			if sidecarID := strings.TrimSpace(binding.SidecarEgressID); sidecarID != "" {
				sidecar, ok := loadEgress(sidecarID)
				if !ok || !IsSidecarEgress(sidecar) || strings.TrimSpace(sidecar.Endpoint) == "" || !egressHealthy(sidecar) {
					continue
				}
			}
			if item.ignoreRateLimitControls || binding.CooldownUntil <= now {
				if egress, ok := loadEgress(binding.PrimaryEgressID); ok && egressHealthy(egress) {
					routable[accountID] = true
				}
			}
		}
		if routable[accountID] {
			out = append(out, item.capability)
		}
	}
	return out, nil
}

func (s *Store) ListCapabilitiesByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]ModelCapability, error) {
	out := make(map[string][]ModelCapability, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, model_slug, availability_state, context_1m_state, context_1m_source, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, raw_model_json, source, last_probe_at FROM account_model_capabilities WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`) ORDER BY account_id, model_slug`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ModelCapability
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.AvailabilityState, &c.Context1MState, &c.Context1MSource, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.RawModelJSON, &c.Source, &c.LastProbeAt); err != nil {
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, model_slug, availability_state, context_1m_state, context_1m_source, native_context_window, native_max_context_window, effective_context_window_percent, auto_compact_token_limit, visibility, etag, raw_model_json_hash, source, last_probe_at FROM account_model_capabilities WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`) ORDER BY account_id, model_slug`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ModelCapability
		if err := rows.Scan(&c.AccountID, &c.ModelSlug, &c.AvailabilityState, &c.Context1MState, &c.Context1MSource, &c.NativeContextWindow, &c.NativeMaxContextWindow, &c.EffectiveContextWindowPercent, &c.AutoCompactTokenLimit, &c.Visibility, &c.ETag, &c.RawModelJSONHash, &c.Source, &c.LastProbeAt); err != nil {
			return nil, err
		}
		out[c.AccountID] = append(out[c.AccountID], c)
	}
	return out, rows.Err()
}

func (s *Store) ListModelCatalogAuthorityByAccountIDs(ctx context.Context, accountIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, authoritative FROM account_model_catalog_status WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`)`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID string
		var authoritative int
		if err := rows.Scan(&accountID, &authoritative); err != nil {
			return nil, err
		}
		out[accountID] = authoritative != 0
	}
	return out, rows.Err()
}

func (s *Store) ModelCatalogAuthoritative(ctx context.Context, accountID string) (bool, error) {
	var authoritative int
	err := s.rdb.QueryRowContext(ctx, `SELECT authoritative FROM account_model_catalog_status WHERE account_id = ?`, accountID).Scan(&authoritative)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return authoritative != 0, err
}

func (s *Store) BestNativeWindow(ctx context.Context, accountID, model string) (int64, error) {
	var value int64
	err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(native_context_window), 0) FROM account_model_capabilities WHERE account_id = ? AND (? = '' OR model_slug = ?) AND availability_state <> 'unsupported'`, accountID, model, model).Scan(&value)
	return value, err
}

// BestNativeMaxWindow returns the model's technical/account-verified ceiling. It
// is used only after routing has independently required the extended context mode;
// standard requests continue to use BestNativeWindow.
func (s *Store) BestNativeMaxWindow(ctx context.Context, accountID, model string) (int64, error) {
	var value int64
	err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(MAX(native_max_context_window), 0) FROM account_model_capabilities WHERE account_id = ? AND (? = '' OR model_slug = ?) AND availability_state <> 'unsupported'`, accountID, model, model).Scan(&value)
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT c.account_id FROM account_model_capabilities c JOIN accounts a ON a.id = c.account_id WHERE a.group_name = ? AND a.status = 'active' AND (a.quarantine_until <= ? OR a.ignore_rate_limit_controls = 1) AND c.model_slug = ? AND c.availability_state = 'verified'`, group, Now(), model)
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

func (s *Store) SetCodexCacheCapability(ctx context.Context, capability CodexCacheCapability) error {
	capability.AccountID = strings.TrimSpace(capability.AccountID)
	capability.Model = strings.ToLower(strings.TrimSpace(capability.Model))
	capability.ExplicitBreakpointState = strings.ToLower(strings.TrimSpace(capability.ExplicitBreakpointState))
	if capability.ExplicitBreakpointState == "" {
		capability.ExplicitBreakpointState = "unknown"
	}
	if capability.ProbedAt == 0 {
		capability.ProbedAt = Now()
	}
	if capability.UpdatedAt == 0 {
		capability.UpdatedAt = Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO codex_cache_capabilities(account_id, model, explicit_breakpoint_state, first_write_tokens, second_read_tokens, probed_at, updated_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(account_id,model) DO UPDATE SET explicit_breakpoint_state=excluded.explicit_breakpoint_state,
first_write_tokens=excluded.first_write_tokens, second_read_tokens=excluded.second_read_tokens, probed_at=excluded.probed_at, updated_at=excluded.updated_at`,
		capability.AccountID, capability.Model, capability.ExplicitBreakpointState, capability.FirstWriteTokens, capability.SecondReadTokens, capability.ProbedAt, capability.UpdatedAt)
	return err
}

func (s *Store) GetCodexCacheCapability(ctx context.Context, accountID, model string) (CodexCacheCapability, error) {
	var capability CodexCacheCapability
	err := s.rdb.QueryRowContext(ctx, `SELECT account_id, model, explicit_breakpoint_state, first_write_tokens, second_read_tokens, probed_at, updated_at
FROM codex_cache_capabilities WHERE account_id=? AND model=?`, strings.TrimSpace(accountID), strings.ToLower(strings.TrimSpace(model))).Scan(
		&capability.AccountID, &capability.Model, &capability.ExplicitBreakpointState, &capability.FirstWriteTokens, &capability.SecondReadTokens, &capability.ProbedAt, &capability.UpdatedAt)
	return capability, err
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
	if p.MaxConcurrency <= 0 && p.ID != DefaultDirectEgressID {
		// A non-positive concurrency means "unlimited" in concurrencyLimited, and the
		// migration seeds the default direct outlet with 0 for exactly that reason.
		// Coercing 0 to 16 here would silently turn the shared default outlet into a
		// hard cap on the first admin save / import that touches it, so the default
		// outlet keeps 0 while fresh proxy egresses still get a sane 16.
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

// DeleteEgressProfile refuses to remove an outlet while any live routing
// configuration still references it. References are never silently detached.
func (s *Store) DeleteEgressProfile(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("egress id required")
	}
	if id == DefaultDirectEgressID {
		return fmt.Errorf("%w: system_default:%s", ErrEgressInUse, id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	references := make([]string, 0)

	groupRows, err := tx.QueryContext(ctx, `SELECT name, default_egress_id, egress_ids FROM groups`)
	if err != nil {
		return err
	}
	for groupRows.Next() {
		var name, legacyID, idsJSON string
		if err := groupRows.Scan(&name, &legacyID, &idsJSON); err != nil {
			_ = groupRows.Close()
			return err
		}
		ids := decodeStringList(idsJSON)
		if strings.TrimSpace(idsJSON) == "" && strings.TrimSpace(legacyID) != "" {
			ids = []string{strings.TrimSpace(legacyID)}
		}
		if len(ids) > 0 && ids[0] == id {
			references = append(references, "account_pool_group:"+name)
		}
	}
	if err := groupRows.Err(); err != nil {
		_ = groupRows.Close()
		return err
	}
	_ = groupRows.Close()

	poolRows, err := tx.QueryContext(ctx, `SELECT pool_id FROM egress_pool_members WHERE egress_id = ?`, id)
	if err != nil {
		return err
	}
	for poolRows.Next() {
		var poolID string
		if err := poolRows.Scan(&poolID); err != nil {
			_ = poolRows.Close()
			return err
		}
		references = append(references, "egress_pool:"+poolID)
	}
	if err := poolRows.Err(); err != nil {
		_ = poolRows.Close()
		return err
	}
	_ = poolRows.Close()

	bindingRows, err := tx.QueryContext(ctx, `SELECT account_id, primary_egress_id, sidecar_egress_id, binding_scope FROM account_egress_bindings`)
	if err != nil {
		return err
	}
	for bindingRows.Next() {
		var accountID, primaryID, sidecarID, bindingScope string
		if err := bindingRows.Scan(&accountID, &primaryID, &sidecarID, &bindingScope); err != nil {
			_ = bindingRows.Close()
			return err
		}
		accountScoped := strings.EqualFold(strings.TrimSpace(bindingScope), EgressBindingScopeAccount)
		inUse := sidecarID == id || (accountScoped && primaryID == id)
		if inUse {
			references = append(references, "account_binding:"+accountID)
		}
	}
	if err := bindingRows.Err(); err != nil {
		_ = bindingRows.Close()
		return err
	}
	_ = bindingRows.Close()

	now := Now()
	affinityRows, err := tx.QueryContext(ctx, `SELECT route_key_hash FROM affinity_bindings WHERE egress_id = ? AND (expires_at = 0 OR expires_at > ?)`, id, now)
	if err != nil {
		return err
	}
	for affinityRows.Next() {
		var routeKeyHash string
		if err := affinityRows.Scan(&routeKeyHash); err != nil {
			_ = affinityRows.Close()
			return err
		}
		references = append(references, "affinity_binding:"+routeKeyHash)
	}
	if err := affinityRows.Err(); err != nil {
		_ = affinityRows.Close()
		return err
	}
	_ = affinityRows.Close()

	sessionRows, err := tx.QueryContext(ctx, `SELECT id FROM codex_session_binding WHERE egress_id = ? AND state = 'active' AND expires_at > ?`, id, now)
	if err != nil {
		return err
	}
	for sessionRows.Next() {
		var bindingID string
		if err := sessionRows.Scan(&bindingID); err != nil {
			_ = sessionRows.Close()
			return err
		}
		references = append(references, "codex_session:"+bindingID)
	}
	if err := sessionRows.Err(); err != nil {
		_ = sessionRows.Close()
		return err
	}
	_ = sessionRows.Close()

	if len(references) > 0 {
		sort.Strings(references)
		return fmt.Errorf("%w: %s", ErrEgressInUse, strings.Join(references, ", "))
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM egress_profiles WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
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
	binding, bindingErr := s.GetEgressBinding(ctx, accountID)
	if bindingErr == nil && memberIDs[binding.PrimaryEgressID] {
		return binding, nil
	} else if bindingErr != nil && !errors.Is(bindingErr, sql.ErrNoRows) {
		return AccountEgressBinding{}, bindingErr
	}
	chosen, err := s.selectEgressPoolMember(ctx, poolID)
	if err != nil {
		return AccountEgressBinding{}, err
	}
	// Pool reassignment changes the real IP exit only. Preserve the account-level
	// sidecar wrapper and any other binding state instead of erasing it with a fresh
	// struct literal.
	binding.AccountID = accountID
	binding.PrimaryEgressID = chosen.EgressID
	binding.CookieJarKey = accountID + ":" + chosen.EgressID
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

// RepairMissingAccountEgressBindings heals legacy account-only imports that
// predate portable egress bindings. The chosen primary follows the account's
// pool group outlet order, with the direct outlet used only as the final
// installed fallback.
func (s *Store) RepairMissingAccountEgressBindings(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT a.id, a.group_name
FROM accounts a
LEFT JOIN account_egress_bindings b ON b.account_id = a.id
WHERE b.account_id IS NULL
ORDER BY a.id`)
	if err != nil {
		return 0, err
	}
	var accounts []Account
	for rows.Next() {
		var account Account
		if err = rows.Scan(&account.ID, &account.GroupName); err != nil {
			rows.Close()
			return 0, err
		}
		accounts = append(accounts, account)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	now := Now()
	repaired := 0
	for _, account := range accounts {
		primaryEgressID, err := restoreAccountBackupDefaultEgressIDTx(ctx, tx, account)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `
INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at)
VALUES(?, ?, '', '', ?, 0, 0, ?, ?)
ON CONFLICT(account_id) DO NOTHING`, account.ID, primaryEgressID, account.ID+":"+primaryEgressID, now, now)
		if err != nil {
			return 0, err
		}
		if affected, err := result.RowsAffected(); err == nil {
			repaired += int(affected)
		} else {
			repaired++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if repaired > 0 {
		s.affinityGen.Add(1)
	}
	return repaired, nil
}

func (s *Store) GetEgressBinding(ctx context.Context, accountID string) (AccountEgressBinding, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, binding_scope, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE account_id = ?`, accountID)
	var b AccountEgressBinding
	var recheck int
	err := row.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.SidecarEgressID, &b.BindingScope, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt)
	b.RecheckPending = recheck != 0
	return b, err
}

func (s *Store) ListEgressBindingsByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountEgressBinding, error) {
	out := make(map[string]AccountEgressBinding, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, binding_scope, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE account_id IN (`+sqlPlaceholders(len(accountIDs))+`)`, stringArgs(accountIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.SidecarEgressID, &b.BindingScope, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.RecheckPending = recheck != 0
		out[b.AccountID] = b
	}
	return out, rows.Err()
}

func (s *Store) UpsertEgressBinding(ctx context.Context, b AccountEgressBinding) error {
	now := Now()
	b.BindingScope = strings.ToLower(strings.TrimSpace(b.BindingScope))
	if b.BindingScope == "" {
		// Direct callers historically use UpsertEgressBinding to make a deliberate
		// account assignment. Automatic creation paths insert scope=group explicitly.
		b.BindingScope = EgressBindingScopeAccount
	}
	if b.BindingScope != EgressBindingScopeGroup && b.BindingScope != EgressBindingScopeAccount {
		return fmt.Errorf("invalid egress binding scope %q", b.BindingScope)
	}
	if b.CookieJarKey == "" {
		b.CookieJarKey = b.AccountID + ":" + b.PrimaryEgressID
	}
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_egress_bindings(account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, binding_scope, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
 primary_egress_id = excluded.primary_egress_id,
 standby_egress_ids = excluded.standby_egress_ids,
 sidecar_egress_id = excluded.sidecar_egress_id,
 binding_scope = excluded.binding_scope,
 cookie_jar_key = excluded.cookie_jar_key,
 cooldown_until = excluded.cooldown_until,
 recheck_pending = excluded.recheck_pending,
 updated_at = excluded.updated_at`,
		b.AccountID, b.PrimaryEgressID, b.StandbyEgressIDs, b.SidecarEgressID, b.BindingScope, b.CookieJarKey, b.CooldownUntil, boolInt(b.RecheckPending), b.CreatedAt, b.UpdatedAt)
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

// ClearAccountCooldown atomically releases every account-local scheduling cooldown:
// the binding cooldown/recheck gate and any active exhausted quota snapshot. Shared
// egress profile state is deliberately outside this operation because clearing it
// would affect every account using that exit.
//
// Quota rows are retained as diagnostic snapshots, but their active reset window is
// cleared and status records the administrator action. A later upstream quota poll
// remains authoritative and may establish a new cooldown when the provider still
// reports exhaustion.
func (s *Store) ClearAccountCooldown(ctx context.Context, accountID string) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, errors.New("account_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM accounts WHERE id = ?`, accountID).Scan(&exists); err != nil {
		return 0, err
	}
	now := Now()
	if _, err := tx.ExecContext(ctx, `UPDATE account_egress_bindings SET cooldown_until = 0, recheck_pending = 0, updated_at = ? WHERE account_id = ?`, now, accountID); err != nil {
		return 0, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT `+rateLimitCols+` FROM account_rate_limits WHERE account_id = ? AND reset_at > ?`, accountID, now)
	if err != nil {
		return 0, err
	}
	var active []AccountRateLimit
	for rows.Next() {
		row, scanErr := scanAccountRateLimit(rows.Scan)
		if scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		if accountRateLimitExhausted(row) {
			active = append(active, row)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	for _, row := range active {
		if _, err := tx.ExecContext(ctx, `
UPDATE account_rate_limits
SET reset_at = 0, status = 'manual_cleared', updated_at = ?
WHERE account_id = ? AND provider = ? AND model = ? AND limiter_type = ?`,
			now, row.AccountID, row.Provider, row.Model, row.LimiterType); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if len(active) > 0 {
		s.rateLimitGen.Add(1)
	}
	return int64(len(active)), nil
}

// ListBindingsNeedingRecheck returns the bindings whose cooldown has elapsed but
// which are still recheck-pending — i.e. the accounts the recheck loop should now
// probe. Gating on cooldown_until <= now means the initial cooldown window (often a
// server-signaled Retry-After) is always honored before the first probe.
func (s *Store) ListBindingsNeedingRecheck(ctx context.Context, now int64) ([]AccountEgressBinding, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, binding_scope, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings WHERE recheck_pending = 1 AND cooldown_until <= ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountEgressBinding
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.SidecarEgressID, &b.BindingScope, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, primary_egress_id, standby_egress_ids, sidecar_egress_id, binding_scope, cookie_jar_key, cooldown_until, recheck_pending, created_at, updated_at FROM account_egress_bindings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountEgressBinding
	for rows.Next() {
		var b AccountEgressBinding
		var recheck int
		if err := rows.Scan(&b.AccountID, &b.PrimaryEgressID, &b.StandbyEgressIDs, &b.SidecarEgressID, &b.BindingScope, &b.CookieJarKey, &b.CooldownUntil, &recheck, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, err
		}
		b.RecheckPending = recheck != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) getAffinityBinding(ctx context.Context, routeKeyHash string) (AffinityBinding, error) {
	now := Now()
	row := s.rdb.QueryRowContext(ctx, `SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at FROM (
SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at,0 AS source_rank FROM affinity_aliases WHERE route_key_hash=? AND expires_at>?
UNION ALL
SELECT route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at,1 AS source_rank FROM affinity_bindings WHERE route_key_hash=? AND (expires_at=0 OR expires_at>?)
) ORDER BY source_rank LIMIT 1`, routeKeyHash, now, routeKeyHash, now)
	var b AffinityBinding
	err := row.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Provider, &b.Model, &b.EgressID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt, &b.ExpiresAt)
	return b, err
}

func (s *Store) GetAffinityBinding(ctx context.Context, routeKeyHash string) (AffinityBinding, error) {
	routeKeyHash = strings.TrimSpace(routeKeyHash)
	binding, err := s.getAffinityBinding(ctx, routeKeyHash)
	if err != nil {
		return AffinityBinding{}, err
	}
	// Aliases already carry their own seven-day expiry and are intentionally not
	// converted into permanent session rows by the sliding-retention policy.
	now := Now()
	if _, liveStore := s.rdb.(*sql.DB); liveStore &&
		binding.Source != "previous_response_id" &&
		binding.UpdatedAt <= now-routeBindingTouchInterval {
		result, touchErr := s.db.ExecContext(ctx, `UPDATE affinity_bindings
SET updated_at=?
WHERE route_key_hash=? AND updated_at=?`,
			now, routeKeyHash, binding.UpdatedAt)
		if touchErr != nil {
			return AffinityBinding{}, touchErr
		}
		touched, touchErr := result.RowsAffected()
		if touchErr != nil {
			return AffinityBinding{}, touchErr
		}
		if touched == 1 {
			binding.UpdatedAt = now
		} else {
			// Preserve a concurrently refreshed/rebound row, or surface not-found
			// when cleanup removed the inactive binding before this touch.
			return s.getAffinityBinding(ctx, routeKeyHash)
		}
	}
	return binding, nil
}

func (s *Store) UpsertAffinityBinding(ctx context.Context, b AffinityBinding) error {
	_, err := s.UpsertAffinityBindingResult(ctx, b)
	return err
}

func (s *Store) UpsertAffinityBindingResult(ctx context.Context, b AffinityBinding) (AffinityBinding, error) {
	now := Now()
	if b.CreatedAt == 0 {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
	table := "affinity_bindings"
	if b.Source == "previous_response_id" {
		table = "affinity_aliases"
		if b.ExpiresAt <= now {
			b.ExpiresAt = now + int64((7*24*time.Hour)/time.Second)
		}
	}
	row := s.db.QueryRowContext(ctx, `
INSERT INTO `+table+`(route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at, expires_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(route_key_hash) DO UPDATE SET
 account_id = excluded.account_id,
 source = excluded.source,
 route_key = excluded.route_key,
 provider = excluded.provider,
 model = excluded.model,
	 egress_id = excluded.egress_id,
	expires_at = excluded.expires_at,
	 epoch = `+table+`.epoch + CASE WHEN `+table+`.account_id<>excluded.account_id OR `+table+`.provider<>excluded.provider OR `+table+`.model<>excluded.model OR `+table+`.egress_id<>excluded.egress_id THEN 1 ELSE 0 END,
	 updated_at = excluded.updated_at
RETURNING route_key_hash,route_key,source,account_id,provider,model,egress_id,epoch,created_at,updated_at,expires_at`,
		b.RouteKeyHash, b.RouteKey, b.Source, b.AccountID, b.Provider, b.Model, b.EgressID, b.Epoch, b.CreatedAt, b.UpdatedAt, b.ExpiresAt)
	var stored AffinityBinding
	err := row.Scan(&stored.RouteKeyHash, &stored.RouteKey, &stored.Source, &stored.AccountID, &stored.Provider, &stored.Model, &stored.EgressID, &stored.Epoch, &stored.CreatedAt, &stored.UpdatedAt, &stored.ExpiresAt)
	if err == nil {
		s.affinityGen.Add(1)
	}
	return stored, err
}

// CleanupAffinityAliases removes expired v2 aliases and legacy previous-response
// rows in bounded batches. It returns the total deleted rows.
func (s *Store) CleanupAffinityAliases(ctx context.Context, batch int) (int64, error) {
	if batch <= 0 {
		batch = 256
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var deleted int64
	for _, statement := range []string{
		`DELETE FROM affinity_aliases WHERE route_key_hash IN (SELECT route_key_hash FROM affinity_aliases WHERE expires_at<=? ORDER BY expires_at,route_key_hash LIMIT ?)`,
		`DELETE FROM affinity_bindings WHERE route_key_hash IN (SELECT route_key_hash FROM affinity_bindings WHERE source='previous_response_id' AND expires_at>0 AND expires_at<=? ORDER BY expires_at,route_key_hash LIMIT ?)`,
	} {
		result, execErr := tx.ExecContext(ctx, statement, now, batch)
		if execErr != nil {
			return 0, execErr
		}
		rows, _ := result.RowsAffected()
		deleted += rows
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	if deleted > 0 {
		s.affinityGen.Add(1)
	}
	return deleted, nil
}

type RouteBindingCleanupResult struct {
	UserGroupTargetBindings int64
	AffinityBindings        int64
}

func (r RouteBindingCleanupResult) Total() int64 {
	return r.UserGroupTargetBindings + r.AffinityBindings
}

// CleanupInactiveRouteBindings removes only long-idle, non-alias routing state
// in one bounded transaction. The batch cap is shared by both tables so a disk
// maintenance tick has a predictable write budget. Bindings with a future
// explicit expiry remain intact even when their last lookup is old.
func (s *Store) CleanupInactiveRouteBindings(ctx context.Context, batch int) (RouteBindingCleanupResult, error) {
	if batch <= 0 {
		batch = defaultRouteBindingCleanupSize
	}
	if batch > maxRouteBindingCleanupSize {
		batch = maxRouteBindingCleanupSize
	}
	now := Now()
	cutoff := now - routeBindingRetentionSeconds
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RouteBindingCleanupResult{}, err
	}
	defer tx.Rollback()

	var cleaned RouteBindingCleanupResult
	result, err := tx.ExecContext(ctx, `DELETE FROM user_group_target_bindings
WHERE (user_group_id, affinity_key, model) IN (
 SELECT user_group_id, affinity_key, model
 FROM user_group_target_bindings
 WHERE updated_at<?
 ORDER BY updated_at, user_group_id, affinity_key, model
 LIMIT ?
)`, cutoff, batch)
	if err != nil {
		return RouteBindingCleanupResult{}, err
	}
	cleaned.UserGroupTargetBindings, err = result.RowsAffected()
	if err != nil {
		return RouteBindingCleanupResult{}, err
	}

	remaining := int64(batch) - cleaned.UserGroupTargetBindings
	if remaining > 0 {
		result, err = tx.ExecContext(ctx, `DELETE FROM affinity_bindings
WHERE route_key_hash IN (
 SELECT route_key_hash
 FROM affinity_bindings
 WHERE source<>'previous_response_id'
   AND updated_at<?
   AND (expires_at=0 OR expires_at<=?)
 ORDER BY updated_at, route_key_hash
 LIMIT ?
)`, cutoff, now, remaining)
		if err != nil {
			return RouteBindingCleanupResult{}, err
		}
		cleaned.AffinityBindings, err = result.RowsAffected()
		if err != nil {
			return RouteBindingCleanupResult{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return RouteBindingCleanupResult{}, err
	}
	if cleaned.AffinityBindings > 0 {
		s.affinityGen.Add(1)
	}
	return cleaned, nil
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT route_key_hash, route_key, source, account_id, provider, model, egress_id, epoch, created_at, updated_at, expires_at FROM affinity_bindings WHERE account_id = ? AND (expires_at = 0 OR expires_at > ?) ORDER BY updated_at DESC LIMIT ?`, accountID, Now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AffinityBinding
	for rows.Next() {
		var b AffinityBinding
		if err := rows.Scan(&b.RouteKeyHash, &b.RouteKey, &b.Source, &b.AccountID, &b.Provider, &b.Model, &b.EgressID, &b.Epoch, &b.CreatedAt, &b.UpdatedAt, &b.ExpiresAt); err != nil {
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
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM affinity_bindings WHERE account_id = ? AND (expires_at = 0 OR expires_at > ?)`, accountID, Now()).Scan(&n)
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
	var modelsJSON, modelMappingsJSON, routesJSON, egressJSON string
	if err := scan(&p.ID, &p.Name, &p.BaseURL, &p.UpstreamProtocol, &p.TransportProfile, &routesJSON, &egressJSON, &enabled, &auto, &modelsJSON, &modelMappingsJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return CustomProvider{}, err
	}
	if proto, ok := NormalizeCustomProviderProtocol(p.UpstreamProtocol); ok {
		p.UpstreamProtocol = proto
	} else {
		p.UpstreamProtocol = CustomProviderProtocolChatCompletions
	}
	if profile, ok := NormalizeCustomProviderTransportProfile(p.TransportProfile); ok {
		p.TransportProfile = profile
	} else {
		p.TransportProfile = CustomProviderTransportGeneric
	}
	p.EgressIDs = decodeStringList(egressJSON)
	if p.EgressIDs == nil {
		p.EgressIDs = []string{}
	}
	p.Enabled = enabled != 0
	p.AutoDiscoverModels = auto != 0
	p.Models = decodeProviderModels(modelsJSON)
	p.ModelMappings = decodeProviderModelMappings(modelMappingsJSON)
	var routes []CustomProviderRoute
	if json.Unmarshal([]byte(strings.TrimSpace(routesJSON)), &routes) == nil {
		p.Routes, _ = canonicalCustomProviderRoutes(routes, p, false)
	}
	if p.Routes == nil {
		p.Routes = []CustomProviderRoute{}
	}
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

func decodeProviderModelMappings(raw string) map[string]string {
	var mappings map[string]string
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &mappings) != nil {
		return map[string]string{}
	}
	normalized, _ := canonicalProviderModelMappings(mappings, false)
	return normalized
}

func canonicalProviderModelMappings(in map[string]string, rejectConflicts bool) (map[string]string, error) {
	out := make(map[string]string, len(in))
	keys := make([]string, 0, len(in))
	for requested := range in {
		keys = append(keys, requested)
	}
	sort.Strings(keys)
	for _, original := range keys {
		requested, target := original, in[original]
		// Model matching in the gateway is case-insensitive. Persist one canonical
		// key so differently-cased duplicates cannot make map iteration choose a
		// random target.
		requested = strings.ToLower(strings.TrimSpace(requested))
		target = strings.TrimSpace(target)
		if requested == "" || target == "" {
			continue
		}
		if existing, duplicate := out[requested]; duplicate {
			if existing != target && rejectConflicts {
				return nil, fmt.Errorf("%w: %q has conflicting targets %q and %q", ErrInvalidProviderModelMapping, requested, existing, target)
			}
			// Legacy rows can contain differently-cased duplicate keys. Sorted
			// source keys make the compatibility winner deterministic.
			continue
		}
		out[requested] = target
	}
	return out, nil
}

const customProviderCols = `id, name, base_url, upstream_protocol, transport_profile, routes_json, egress_ids, enabled, auto_discover_models, models_json, model_mappings_json, created_at, updated_at`

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
	if profile, ok := NormalizeCustomProviderTransportProfile(p.TransportProfile); ok {
		p.TransportProfile = profile
	} else {
		p.TransportProfile = CustomProviderTransportGeneric
	}
	p.EgressIDs = normalizeOrderedIDs(p.EgressIDs)
	routes, err := canonicalCustomProviderRoutes(p.Routes, p, true)
	if err != nil {
		return err
	}
	p.Routes = routes
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
	modelMappingsJSON := "{}"
	mappings, err := canonicalProviderModelMappings(p.ModelMappings, true)
	if err != nil {
		return err
	}
	if len(mappings) > 0 {
		raw, err := json.Marshal(mappings)
		if err != nil {
			return err
		}
		modelMappingsJSON = string(raw)
	}
	routesJSON := "[]"
	if len(p.Routes) > 0 {
		raw, err := json.Marshal(p.Routes)
		if err != nil {
			return err
		}
		routesJSON = string(raw)
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO custom_providers(id, name, base_url, upstream_protocol, transport_profile, routes_json, egress_ids, enabled, auto_discover_models, models_json, model_mappings_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 name = excluded.name,
 base_url = excluded.base_url,
 upstream_protocol = excluded.upstream_protocol,
 transport_profile = excluded.transport_profile,
 routes_json = excluded.routes_json,
 egress_ids = excluded.egress_ids,
 enabled = excluded.enabled,
 auto_discover_models = excluded.auto_discover_models,
 models_json = excluded.models_json,
 model_mappings_json = excluded.model_mappings_json,
 updated_at = excluded.updated_at`,
		p.ID, p.Name, p.BaseURL, p.UpstreamProtocol, p.TransportProfile, routesJSON, encodeOrderedIDs(p.EgressIDs), boolInt(p.Enabled), boolInt(p.AutoDiscoverModels), modelsJSON, modelMappingsJSON, p.CreatedAt, p.UpdatedAt)
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
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("provider id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	references, err := routingTargetReferences(ctx, tx, TargetRef{Kind: TargetKindModelProvider, ID: id})
	if err != nil {
		return err
	}
	if len(references) > 0 {
		return fmt.Errorf("%w: model_provider/%s referenced by %s", ErrTargetInUse, id, strings.Join(references, ", "))
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM custom_providers WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
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

// ListAuditLogByActionSince returns audit records for one action created at or
// after `since` (ascending, oldest first), bounded by `limit`. Used by the
// cache-export diagnostics that correlate lifecycle events (affinity rebinds)
// with the usage rows that followed them.
func (s *Store) ListAuditLogByActionSince(ctx context.Context, action string, since int64, limit int) ([]AuditLogRow, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return nil, errors.New("audit action filter is empty")
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, account_id, account_label, action, state, reason, detail, created_at FROM audit_log WHERE action = ? AND created_at >= ? ORDER BY id ASC LIMIT ?`, action, since, limit)
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
	if int64(len(item.Content)) > maxStoredContextPayloadBytes || int64(len(item.RawJSON)) > maxStoredContextPayloadBytes {
		return fmt.Errorf("virtual context payload exceeds %d-byte storage limit", maxStoredContextPayloadBytes)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO virtual_context_ledger(route_key_hash, account_id, model, prompt_cache_key, role, content, token_estimate, raw_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.RouteKeyHash, item.AccountID, item.Model, item.PromptCacheKey, item.Role, compressContextPayload(item.Content), item.TokenEstimate, compressContextPayload(item.RawJSON), item.CreatedAt)
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
		item.Content, err = decompressContextPayloadChecked(item.Content, maxStoredContextPayloadBytes)
		if err != nil {
			return nil, fmt.Errorf("decode virtual context content: %w", err)
		}
		item.RawJSON, err = decompressContextPayloadChecked(item.RawJSON, maxStoredContextPayloadBytes)
		if err != nil {
			return nil, fmt.Errorf("decode virtual context raw_json: %w", err)
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
	PromptCacheKeyHash                string
	PromptCacheKeyShard               int
	PromptCacheKeyMinuteRPM           int64
	PromptCacheKeyConcurrencyPeak     int64
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
	CoordinationPrefixSource          string
	SingleflightWaitReason            string
	SingleflightReleaseReason         string
	DiagnosticsMissReason             string
	LatestUserCacheControl            bool
	LatestUserAutoContextCacheControl bool
	LatestUserTailCacheControl        bool
	LatestUserToolResultCacheControl  bool
	RouteEpoch                        int64
	KiroCredits                       float64
	KiroCreditsPresent                bool
	BillingHoldID                     string
	RequestedModel                    string
	ResolvedModel                     string
	ModelOverrideSource               string
	ActualModel                       string
	ModelMismatch                     bool
	ModelMismatchReason               string
}

type UsageRecordWrite struct {
	AccountID, RouteKeyHash, APIKeyHash, UserID, Model string
	Prompt, Completion, Total, Cached                  int64
	CacheRead, CacheCreation                           int64
	Raw                                                json.RawMessage
	Diagnostics                                        UsageDiagnostics
}

type BillingHoldWrite struct {
	ID, EventID, RouteKeyHash, AccountID, Status string
	EstimatedTokens, RouteEpoch, CreatedAt       int64
	Create, IfHeld                               bool
}

type sqlExecContext interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func (s *Store) InsertUsageRecordWithDiagnostics(ctx context.Context, accountID, routeKeyHash, apiKeyHash, userID, model string, prompt, completion, total, cached, cacheRead, cacheCreation int64, raw json.RawMessage, diag UsageDiagnostics) error {
	return s.insertUsageRecord(ctx, s.db, UsageRecordWrite{AccountID: accountID, RouteKeyHash: routeKeyHash, APIKeyHash: apiKeyHash, UserID: userID, Model: model, Prompt: prompt, Completion: completion, Total: total, Cached: cached, CacheRead: cacheRead, CacheCreation: cacheCreation, Raw: raw, Diagnostics: diag})
}

func (s *Store) BatchInsertUsageRecords(ctx context.Context, writes []UsageRecordWrite) error {
	return s.BatchWriteTelemetry(ctx, writes, nil, nil, nil)
}

func (s *Store) BatchWriteTelemetry(ctx context.Context, writes []UsageRecordWrite, apiKeyUsed map[string]int64, holds []BillingHoldWrite, audits []AuditLogRow) error {
	return s.BatchWriteTelemetryAndAttempts(ctx, writes, apiKeyUsed, holds, audits, nil)
}

// BatchWriteTelemetryAndAttempts commits metering and metadata-only upstream
// observations in one writer transaction. Codex emits several transport lifecycle
// observations per request; batching them keeps those diagnostic writes from
// queueing ahead of the terminal context-pointer commit on SQLite's sole writer.
func (s *Store) BatchWriteTelemetryAndAttempts(ctx context.Context, writes []UsageRecordWrite, apiKeyUsed map[string]int64, holds []BillingHoldWrite, audits []AuditLogRow, attempts []CodexUpstreamAttempt) error {
	normalizedAttempts := make([]CodexUpstreamAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		normalized, err := normalizeCodexUpstreamAttempt(attempt)
		if err != nil {
			return err
		}
		normalizedAttempts = append(normalizedAttempts, normalized)
	}
	if len(writes) == 0 && len(apiKeyUsed) == 0 && len(holds) == 0 && len(audits) == 0 && len(normalizedAttempts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	persistedKeyMinutes := make(map[string]int64, len(apiKeyUsed))
	// A batch can contain the hold create, usage and settlement for one request.
	// Create shells first so usage can mark the hold, then settle after usage merge.
	for _, hold := range holds {
		if hold.Create {
			if err := s.applyBillingHoldCreate(ctx, tx, hold); err != nil {
				return err
			}
		}
	}
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
		if !hold.Create {
			if err := s.applyBillingHoldSettlement(ctx, tx, hold); err != nil {
				return err
			}
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
	for _, attempt := range normalizedAttempts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_upstream_attempt(event_id,tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, attempt.EventID, attempt.TreeID, attempt.AccountID, attempt.EgressID, attempt.Epoch, attempt.State, attempt.StatusCode, attempt.CreatedAt, attempt.ExpiresAt); err != nil {
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

func (s *Store) applyBillingHoldCreate(ctx context.Context, tx *sql.Tx, hold BillingHoldWrite) error {
	now := hold.CreatedAt
	if now == 0 {
		now = Now()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO billing_holds(id, route_key_hash, account_id, estimated_tokens, status, usage_expected, created_at, updated_at) VALUES(?, ?, ?, ?, 'held', 1, ?, ?) ON CONFLICT(id) DO NOTHING`, hold.ID, hold.RouteKeyHash, hold.AccountID, hold.EstimatedTokens, now, now); err != nil {
		return err
	}
	eventID := strings.TrimSpace(hold.EventID)
	if eventID == "" {
		eventID = "usage_" + hold.ID
	}
	// A journal replay may contain a historical duplicate hold ID from the old
	// timestamp-based generator. Treat either unique-key conflict as an idempotent
	// shell replay so one poison record cannot block every later metering event.
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_events(event_id, hold_id, account_id, route_key_hash, route_epoch, estimated_tokens, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		eventID, hold.ID, hold.AccountID, hold.RouteKeyHash, hold.RouteEpoch, hold.EstimatedTokens, now, now)
	return err
}

func billingStatusExpectsUsage(status string) bool {
	switch strings.TrimSpace(status) {
	case "held", "settled", "settled_streaming", "success_body_rule":
		return true
	default:
		return false
	}
}

func (s *Store) applyBillingHoldSettlement(ctx context.Context, tx *sql.Tx, hold BillingHoldWrite) error {
	now := hold.CreatedAt
	if now == 0 {
		now = Now()
	}
	status := strings.TrimSpace(hold.Status)
	if status == "" {
		status = "settled"
	}
	expectsUsage := billingStatusExpectsUsage(status)
	query := `UPDATE billing_holds SET status=?, updated_at=?, usage_expected=CASE WHEN usage_recorded_at>0 THEN 1 ELSE ? END WHERE id=?`
	args := []interface{}{status, now, boolInt(expectsUsage), hold.ID}
	if hold.IfHeld {
		query += ` AND status='held'`
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET terminal_status=?, settled_at=?, updated_at=MAX(updated_at,?) WHERE hold_id=?`, status, now, now, hold.ID); err != nil {
		return err
	}
	if !expectsUsage {
		return nil
	}
	return s.recoverEstimatedUsageForHold(ctx, tx, hold.ID, now)
}

func (s *Store) recoverEstimatedUsageForHold(ctx context.Context, tx *sql.Tx, holdID string, now int64) error {
	var eventID, accountID, routeKeyHash string
	var estimatedTokens, routeEpoch, usageRecordedAt int64
	err := tx.QueryRowContext(ctx, `SELECT e.event_id, h.account_id, h.route_key_hash, h.estimated_tokens, e.route_epoch, h.usage_recorded_at
FROM billing_holds h JOIN usage_events e ON e.hold_id=h.id WHERE h.id=?`, holdID).Scan(&eventID, &accountID, &routeKeyHash, &estimatedTokens, &routeEpoch, &usageRecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || usageRecordedAt > 0 {
		return err
	}
	if estimatedTokens <= 0 {
		_, err = tx.ExecContext(ctx, `UPDATE billing_holds SET usage_expected=0 WHERE id=?`, holdID)
		return err
	}
	write := UsageRecordWrite{AccountID: accountID, RouteKeyHash: routeKeyHash, Model: "unknown", Prompt: estimatedTokens, Total: estimatedTokens,
		Raw: json.RawMessage(`{"estimated":true,"recovered_from_hold":true}`), Diagnostics: UsageDiagnostics{
			UsageEventID: eventID, UsageSource: "hold_recovery", Estimated: true, BillingHoldID: holdID, RouteEpoch: routeEpoch,
		}}
	if err = s.insertUsageRecord(ctx, tx, write); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE usage_events SET updated_at=MAX(updated_at,?) WHERE event_id=?`, now, eventID)
	return err
}

func (s *Store) insertUsageRecord(ctx context.Context, exec sqlExecContext, write UsageRecordWrite) error {
	accountID, routeKeyHash, apiKeyHash, userID, model := write.AccountID, write.RouteKeyHash, write.APIKeyHash, write.UserID, write.Model
	prompt, completion, total, cached := write.Prompt, write.Completion, write.Total, write.Cached
	cacheRead, cacheCreation, raw, diag := write.CacheRead, write.CacheCreation, write.Raw, write.Diagnostics
	if diag.BillingHoldID != "" {
		var eventID string
		err := exec.QueryRowContext(ctx, `SELECT event_id FROM usage_events WHERE hold_id=?`, diag.BillingHoldID).Scan(&eventID)
		if err == nil {
			diag.UsageEventID = eventID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	if diag.UsageEventID == "" && diag.BillingHoldID != "" {
		diag.UsageEventID = "usage_" + diag.BillingHoldID
	}
	diag = finalizeUsageDiagnostics(model, prompt, cached, cacheRead, cacheCreation, raw, diag)
	now := Now()
	if diag.UsageEventID != "" {
		if _, err := exec.ExecContext(ctx, `INSERT INTO usage_events(event_id, hold_id, account_id, route_key_hash, route_epoch, estimated_tokens, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(event_id) DO UPDATE SET hold_id=CASE WHEN excluded.hold_id<>'' THEN excluded.hold_id ELSE usage_events.hold_id END,
 account_id=CASE WHEN excluded.account_id<>'' THEN excluded.account_id ELSE usage_events.account_id END,
 route_key_hash=CASE WHEN excluded.route_key_hash<>'' THEN excluded.route_key_hash ELSE usage_events.route_key_hash END,
 route_epoch=CASE WHEN excluded.route_epoch>0 THEN excluded.route_epoch ELSE usage_events.route_epoch END,
 updated_at=MAX(usage_events.updated_at,excluded.updated_at)`, diag.UsageEventID, diag.BillingHoldID, accountID, routeKeyHash, diag.RouteEpoch, total, now, now); err != nil {
			return err
		}
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO usage_records(
usage_event_id, account_id, route_key_hash, api_key_hash, user_id, model, prompt_tokens, completion_tokens, total_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
usage_provider, usage_source, cache_read_present, cache_creation_present, compatibility_losses_json, cache_capability,
estimated, cache_miss_tokens, cache_total_input_tokens, cache_creation_5m_tokens, cache_creation_1h_tokens,
affinity_source, prompt_cache_key_present, prompt_cache_key_source, prompt_cache_key_hash, prompt_cache_key_shard, prompt_cache_key_minute_rpm, prompt_cache_key_concurrency_peak, stable_prefix_source, stable_prefix_reason, stable_prefix_bytes,
retention_effective, retention_source, claude_cache_ttl, cache_control_injected, cache_breakpoint_count,
cache_breakpoints_json, unwritten_tail_tokens, max_possible_cache_read_tokens, cache_hit_after_prewarm, singleflight_waited_requests, coordination_prefix_source, singleflight_wait_reason, singleflight_release_reason, diagnostics_miss_reason,
latest_user_cache_control, latest_user_auto_context_cache_control, latest_user_tail_cache_control, latest_user_tool_result_cache_control, route_epoch,
	kiro_credits, kiro_credits_present, billing_hold_id, requested_model, resolved_model, model_override_source, actual_model, model_mismatch, model_mismatch_reason,
raw_usage_json, created_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(usage_event_id) WHERE usage_event_id <> '' DO UPDATE SET
	 account_id=excluded.account_id, route_key_hash=excluded.route_key_hash, api_key_hash=excluded.api_key_hash, user_id=excluded.user_id,
	 model=excluded.model, prompt_tokens=excluded.prompt_tokens, completion_tokens=excluded.completion_tokens, total_tokens=excluded.total_tokens,
	 cached_tokens=excluded.cached_tokens, cache_read_tokens=excluded.cache_read_tokens, cache_creation_tokens=excluded.cache_creation_tokens,
	 usage_provider=excluded.usage_provider, usage_source=excluded.usage_source, cache_read_present=excluded.cache_read_present,
	 cache_creation_present=excluded.cache_creation_present, compatibility_losses_json=excluded.compatibility_losses_json, cache_capability=excluded.cache_capability,
	 estimated=excluded.estimated, cache_miss_tokens=excluded.cache_miss_tokens, cache_total_input_tokens=excluded.cache_total_input_tokens,
	 cache_creation_5m_tokens=excluded.cache_creation_5m_tokens, cache_creation_1h_tokens=excluded.cache_creation_1h_tokens,
	 affinity_source=excluded.affinity_source, prompt_cache_key_present=excluded.prompt_cache_key_present, prompt_cache_key_source=excluded.prompt_cache_key_source,
	 prompt_cache_key_hash=excluded.prompt_cache_key_hash, prompt_cache_key_shard=excluded.prompt_cache_key_shard,
	 prompt_cache_key_minute_rpm=excluded.prompt_cache_key_minute_rpm, prompt_cache_key_concurrency_peak=excluded.prompt_cache_key_concurrency_peak,
	 stable_prefix_source=excluded.stable_prefix_source, stable_prefix_reason=excluded.stable_prefix_reason, stable_prefix_bytes=excluded.stable_prefix_bytes,
	 retention_effective=excluded.retention_effective, retention_source=excluded.retention_source, claude_cache_ttl=excluded.claude_cache_ttl,
	 cache_control_injected=excluded.cache_control_injected, cache_breakpoint_count=excluded.cache_breakpoint_count, cache_breakpoints_json=excluded.cache_breakpoints_json,
	 unwritten_tail_tokens=excluded.unwritten_tail_tokens, max_possible_cache_read_tokens=excluded.max_possible_cache_read_tokens,
	 cache_hit_after_prewarm=excluded.cache_hit_after_prewarm, singleflight_waited_requests=excluded.singleflight_waited_requests,
	 coordination_prefix_source=excluded.coordination_prefix_source, singleflight_wait_reason=excluded.singleflight_wait_reason,
	 singleflight_release_reason=excluded.singleflight_release_reason,
	 diagnostics_miss_reason=excluded.diagnostics_miss_reason, latest_user_cache_control=excluded.latest_user_cache_control,
	 latest_user_auto_context_cache_control=excluded.latest_user_auto_context_cache_control, latest_user_tail_cache_control=excluded.latest_user_tail_cache_control,
	 latest_user_tool_result_cache_control=excluded.latest_user_tool_result_cache_control, route_epoch=excluded.route_epoch, kiro_credits=excluded.kiro_credits,
	 kiro_credits_present=excluded.kiro_credits_present, billing_hold_id=excluded.billing_hold_id, requested_model=excluded.requested_model,
	 resolved_model=excluded.resolved_model, model_override_source=excluded.model_override_source,
	 actual_model=excluded.actual_model, model_mismatch=excluded.model_mismatch, model_mismatch_reason=excluded.model_mismatch_reason,
	 raw_usage_json=excluded.raw_usage_json
WHERE usage_records.estimated > 0 AND excluded.estimated = 0`,
		diag.UsageEventID, accountID, routeKeyHash, apiKeyHash, userID, model, prompt, completion, total, cached, cacheRead, cacheCreation,
		diag.UsageProvider, diag.UsageSource, boolInt(diag.CacheReadPresent), boolInt(diag.CacheCreationPresent), diag.CompatibilityLossesJSON, diag.CacheCapability,
		boolInt(diag.Estimated), diag.CacheMissTokens, diag.CacheTotalInputTokens, diag.CacheCreation5mTokens, diag.CacheCreation1hTokens,
		diag.AffinitySource, boolInt(diag.PromptCacheKeyPresent), diag.PromptCacheKeySource, diag.PromptCacheKeyHash, diag.PromptCacheKeyShard, diag.PromptCacheKeyMinuteRPM, diag.PromptCacheKeyConcurrencyPeak, diag.StablePrefixSource, diag.StablePrefixReason, diag.StablePrefixBytes,
		diag.RetentionEffective, diag.RetentionSource, diag.ClaudeCacheTTL, boolInt(diag.CacheControlInjected), diag.CacheBreakpointCount,
		diag.CacheBreakpointsJSON, diag.UnwrittenTailTokens, diag.MaxPossibleCacheReadTokens, boolInt(diag.CacheHitAfterPrewarm), diag.SingleflightWaitedRequests, diag.CoordinationPrefixSource, diag.SingleflightWaitReason, diag.SingleflightReleaseReason, diag.DiagnosticsMissReason,
		boolInt(diag.LatestUserCacheControl),
		boolInt(diag.LatestUserAutoContextCacheControl), boolInt(diag.LatestUserTailCacheControl), boolInt(diag.LatestUserToolResultCacheControl), diag.RouteEpoch,
		diag.KiroCredits, boolInt(diag.KiroCreditsPresent), diag.BillingHoldID, diag.RequestedModel, diag.ResolvedModel, firstNonEmptyStorage(diag.ModelOverrideSource, "none"),
		diag.ActualModel, boolInt(diag.ModelMismatch), diag.ModelMismatchReason,
		string(raw), now)
	if err != nil {
		return err
	}
	if diag.UsageEventID != "" {
		state := "real"
		if diag.Estimated {
			state = "estimated"
		}
		if _, err = exec.ExecContext(ctx, `UPDATE usage_events SET usage_state=CASE WHEN usage_state='real' OR ?='real' THEN 'real' ELSE 'estimated' END,
 usage_recorded_at=CASE WHEN usage_recorded_at>0 THEN usage_recorded_at ELSE ? END, route_epoch=CASE WHEN ?>0 THEN ? ELSE route_epoch END,
 updated_at=MAX(updated_at,?) WHERE event_id=?`, state, now, diag.RouteEpoch, diag.RouteEpoch, now, diag.UsageEventID); err != nil {
			return err
		}
	}
	if diag.BillingHoldID != "" {
		_, err = exec.ExecContext(ctx, `UPDATE billing_holds SET usage_recorded_at=CASE WHEN usage_recorded_at>0 THEN usage_recorded_at ELSE ? END, usage_expected=1 WHERE id=?`, now, diag.BillingHoldID)
	}
	return err
}

func firstNonEmptyStorage(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func finalizeUsageDiagnostics(model string, prompt, cached, cacheRead, cacheCreation int64, raw json.RawMessage, diag UsageDiagnostics) UsageDiagnostics {
	usageMap := rawUsageMap(raw)
	if strings.TrimSpace(diag.PromptCacheKeyHash) == "" {
		diag.PromptCacheKeyShard = -1
	}
	if strings.TrimSpace(diag.ActualModel) == "" {
		diag.ActualModel = strings.TrimSpace(model)
	}
	expectedModel := firstNonEmptyStorage(diag.ResolvedModel, diag.RequestedModel)
	switch {
	case modelAuditUnavailable(diag.ActualModel):
		diag.ModelMismatch = false
		diag.ModelMismatchReason = "actual_model_unavailable"
	case strings.TrimSpace(expectedModel) == "":
		diag.ModelMismatch = false
		diag.ModelMismatchReason = "resolved_model_unavailable"
	case canonicalAuditModel(expectedModel) != canonicalAuditModel(diag.ActualModel):
		diag.ModelMismatch = true
		diag.ModelMismatchReason = "upstream_model_differs_from_resolved"
	default:
		diag.ModelMismatch = false
		diag.ModelMismatchReason = ""
	}
	_, openAICacheReadReported := nestedUsageIntPresent(usageMap, "input_tokens_details", "cached_tokens")
	if !openAICacheReadReported {
		_, openAICacheReadReported = nestedUsageIntPresent(usageMap, "prompt_tokens_details", "cached_tokens")
	}
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
	// OpenAI Responses reports cache reads under
	// usage.input_tokens_details.cached_tokens (chat-compatible responses may use
	// prompt_tokens_details).  The token parser has always populated cached_tokens,
	// but diagnostics previously looked only for Anthropic's top-level spelling,
	// leaving every Codex row marked as cache-unreported despite the raw evidence.
	if openAICacheReadReported || cacheRead > 0 || cached > 0 {
		diag.CacheReadPresent = true
	}
	if _, ok := usageMap["cache_creation_input_tokens"]; ok {
		diag.CacheCreationPresent = true
	}
	// OpenAI Responses/Chat-compatible usage reports cache writes in the same
	// details object as cache reads. Presence (including an explicit zero) is
	// meaningful for diagnostics, so do not gate this on the parsed value.
	if _, present := nestedUsageIntPresent(usageMap, "input_tokens_details", "cache_write_tokens"); present {
		diag.CacheCreationPresent = true
	}
	if _, present := nestedUsageIntPresent(usageMap, "prompt_tokens_details", "cache_write_tokens"); present {
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
	if diag.CacheCapability == "" || diag.CacheCapability == "unknown" {
		switch {
		case diag.CacheReadPresent && (cacheRead > 0 || cached > 0):
			diag.CacheCapability = "hit_observed"
		case diag.CacheReadPresent || diag.CacheCreationPresent:
			diag.CacheCapability = "reported"
		case strings.EqualFold(diag.UsageProvider, "codex") || strings.EqualFold(diag.UsageProvider, "openai"):
			diag.CacheCapability = "unreported"
		}
	}
	if !diag.Estimated && diag.CacheReadPresent && cacheRead <= 0 && cached <= 0 && diag.DiagnosticsMissReason == "" {
		diag.DiagnosticsMissReason = "upstream_reported_zero_cache_read"
	}
	if diag.MaxPossibleCacheReadTokens <= 0 && prompt > 0 && diag.CacheReadPresent &&
		!isAnthropicUsageMap(usageMap, cacheCreation) {
		// Only Anthropic bodies carry breakpoint metadata, from which the real ceiling is
		// derived. For every other shape the reported input total is the only lossless
		// upper bound available for the same request, and is strictly >= the observed
		// cached-token count.
		//
		// This was gated to codex and openai by name, so antigravity and every custom
		// relay stored 0 here while still reporting a real cache read. Zero is not
		// distinguishable from "no cache was reusable", so a read/max ratio computed over
		// a mixed pool divided by zero on those rows: one export summed to a 1.06 ratio
		// against a bound that is by definition never exceeded.
		diag.MaxPossibleCacheReadTokens = prompt
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

func modelAuditUnavailable(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "", "unknown", "estimated", "unavailable":
		return true
	default:
		return false
	}
}

func canonicalAuditModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimSuffix(model, "[1m]")
	if strings.HasPrefix(model, "claude-") {
		model = strings.TrimSuffix(model, "-thinking")
		model = strings.ReplaceAll(model, ".", "-")
	}
	if model == "gpt-5.6" {
		return "gpt-5.6-sol"
	}
	return model
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
	// A parsed cacheCreation value historically identified the Anthropic
	// ephemeral-write shape.  OpenAI now uses the same destination field, so
	// prefer the explicit nested spelling when it is present and only then fall
	// back to the legacy numeric discriminator.
	if _, present := nestedUsageIntPresent(usageMap, "input_tokens_details", "cache_write_tokens"); present {
		return false
	}
	if _, present := nestedUsageIntPresent(usageMap, "prompt_tokens_details", "cache_write_tokens"); present {
		return false
	}
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
	value, _ := nestedUsageIntPresent(m, parent, child)
	return value
}

func nestedUsageIntPresent(m map[string]interface{}, parent, child string) (int64, bool) {
	detail, ok := m[parent].(map[string]interface{})
	if !ok {
		return 0, false
	}
	if _, present := detail[child]; !present {
		return 0, false
	}
	return usageInt(detail, child), true
}

const usageCacheDiagnosticsMigrationMarker = "usage_cache_diagnostics_v3_backfilled"
const openAICacheDiagnosticsMigrationMarker = "usage_cache_diagnostics_v4_openai_nested_cache_backfilled"

// RunDeferredMigrations performs historical repairs after the HTTP listener is
// available. Only additive schema and current-write invariants remain synchronous;
// these potentially large, restartable rewrites must not hold a socket-activated
// service in the pre-listener state long enough for the install health gate to
// roll it back.
func (s *Store) RunDeferredMigrations(ctx context.Context) error {
	if s == nil || s.driver == "postgres" {
		return nil
	}
	if err := s.repairLegacyUsageEventsOnce(ctx); err != nil {
		return err
	}
	if err := s.backfillUsageHourlyRollups(ctx); err != nil {
		return err
	}
	var completed int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, usageCacheDiagnosticsMigrationMarker).Scan(&completed); err != nil {
		return err
	}
	if completed == 0 {
		if err := s.backfillUsageCacheDiagnostics(ctx); err != nil {
			return err
		}
		now := Now()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, usageCacheDiagnosticsMigrationMarker, "1", now); err != nil {
			return err
		}
	}
	completed = 0
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, openAICacheDiagnosticsMigrationMarker).Scan(&completed); err != nil {
		return err
	}
	if completed == 0 {
		if err := s.backfillOpenAICacheDiagnostics(ctx); err != nil {
			return err
		}
		now := Now()
		if _, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, openAICacheDiagnosticsMigrationMarker, "1", now); err != nil {
			return err
		}
	}
	return s.migrateStoredContextCompression(ctx)
}

// backfillOpenAICacheDiagnostics repairs already-durable Codex/OpenAI rows whose
// raw usage contains input_tokens_details.cached_tokens.  It is deliberately
// bounded and restartable; the marker is written only after all batches commit.
func (s *Store) backfillOpenAICacheDiagnostics(ctx context.Context) error {
	const batchSize = 500
	type row struct {
		id                                                    int64
		model, provider, source, capability, missReason, raw  string
		prompt, cached, cacheRead, cacheCreation, maxPossible int64
		cacheReadPresent, cacheCreationPresent, estimated     int
	}
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.rdb.QueryContext(ctx, `
SELECT id,model,usage_provider,usage_source,cache_capability,diagnostics_miss_reason,raw_usage_json,
       prompt_tokens,cached_tokens,cache_read_tokens,cache_creation_tokens,max_possible_cache_read_tokens,
       cache_read_present,cache_creation_present,estimated
FROM usage_records
WHERE id>? AND (lower(trim(usage_provider)) IN ('codex','openai')
  OR raw_usage_json LIKE '%"input_tokens_details"%' OR raw_usage_json LIKE '%"prompt_tokens_details"%')
  AND (cache_read_present=0 OR cache_capability='' OR cache_capability='unknown'
       OR max_possible_cache_read_tokens=0
       OR (diagnostics_miss_reason='' AND cached_tokens=0 AND cache_read_tokens=0))
ORDER BY id LIMIT ?`, afterID, batchSize)
		if err != nil {
			return err
		}
		items := make([]row, 0, batchSize)
		for rows.Next() {
			var item row
			if err = rows.Scan(&item.id, &item.model, &item.provider, &item.source, &item.capability, &item.missReason, &item.raw,
				&item.prompt, &item.cached, &item.cacheRead, &item.cacheCreation, &item.maxPossible,
				&item.cacheReadPresent, &item.cacheCreationPresent, &item.estimated); err != nil {
				_ = rows.Close()
				return err
			}
			afterID = item.id
			items = append(items, item)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, item := range items {
			diag := finalizeUsageDiagnostics(item.model, item.prompt, item.cached, item.cacheRead, item.cacheCreation,
				json.RawMessage(item.raw), UsageDiagnostics{
					UsageProvider: item.provider, UsageSource: item.source, CacheCapability: item.capability,
					DiagnosticsMissReason: item.missReason, MaxPossibleCacheReadTokens: item.maxPossible,
					CacheReadPresent: item.cacheReadPresent != 0, CacheCreationPresent: item.cacheCreationPresent != 0,
					Estimated: item.estimated != 0,
				})
			if _, err = tx.ExecContext(ctx, `UPDATE usage_records SET cache_read_present=?,cache_creation_present=?,
cache_capability=?,cache_miss_tokens=?,cache_total_input_tokens=?,max_possible_cache_read_tokens=?,diagnostics_miss_reason=? WHERE id=?`,
				boolInt(diag.CacheReadPresent), boolInt(diag.CacheCreationPresent), diag.CacheCapability,
				diag.CacheMissTokens, diag.CacheTotalInputTokens, diag.MaxPossibleCacheReadTokens, diag.DiagnosticsMissReason, item.id); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		if len(items) < batchSize {
			return nil
		}
	}
}

func (s *Store) backfillUsageCacheDiagnostics(ctx context.Context) error {
	const batchSize = 500
	type creditBackfill struct {
		id    int64
		value float64
	}

	// Parse only payloads that can contain the legacy credit field. The previous
	// query decoded every Kiro usage row on every startup, including rows that could
	// never be updated.
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.rdb.QueryContext(ctx, `
SELECT id,raw_usage_json FROM usage_records
WHERE usage_provider='kiro' AND kiro_credits_present=0 AND id>?
  AND raw_usage_json LIKE '%"kiro_credits"%'
ORDER BY id LIMIT ?`, afterID, batchSize)
		if err != nil {
			return err
		}
		updates := make([]creditBackfill, 0, batchSize)
		scanned := 0
		for rows.Next() {
			var id int64
			var raw string
			if err = rows.Scan(&id, &raw); err != nil {
				_ = rows.Close()
				return err
			}
			afterID = id
			scanned++
			decoder := json.NewDecoder(strings.NewReader(raw))
			decoder.UseNumber()
			var payload map[string]interface{}
			if decoder.Decode(&payload) != nil {
				continue
			}
			switch value := payload["kiro_credits"].(type) {
			case json.Number:
				if parsed, parseErr := value.Float64(); parseErr == nil {
					updates = append(updates, creditBackfill{id: id, value: parsed})
				}
			case float64:
				updates = append(updates, creditBackfill{id: id, value: value})
			}
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(updates) > 0 {
			if err = func() error {
				tx, txErr := s.db.BeginTx(ctx, nil)
				if txErr != nil {
					return txErr
				}
				defer tx.Rollback()
				for _, update := range updates {
					if _, txErr = tx.ExecContext(ctx, `UPDATE usage_records SET kiro_credits=?,kiro_credits_present=1 WHERE id=? AND kiro_credits_present=0`, update.value, update.id); txErr != nil {
						return txErr
					}
				}
				return tx.Commit()
			}(); err != nil {
				return err
			}
		}
		if scanned < batchSize {
			break
		}
	}

	// Apply the fixed-shape repairs in one transaction so a large history incurs one
	// commit rather than a separate durable write per statement.
	if err := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// Older Kiro converters used the wire spelling as the persisted model while
		// newer responses use the canonical dotted version. Keep one reporting key.
		if _, err = tx.ExecContext(ctx, `UPDATE usage_records SET model='claude-opus-4.8' WHERE usage_provider='kiro' AND lower(trim(model))='claude-opus-4-8'`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE affinity_bindings SET model='claude-opus-4.8' WHERE provider='kiro' AND lower(trim(model))='claude-opus-4-8'`); err != nil {
			return err
		}
		// A historical meteringEvent containing only credits was previously treated as
		// authoritative zero-token usage. Preserve the original raw payload for audit,
		// but mark the derived row unreported rather than fabricating token values.
		if _, err = tx.ExecContext(ctx, `
UPDATE usage_records
SET usage_source='unreported', cache_capability=CASE WHEN cache_capability='' OR cache_capability='unknown' THEN 'unreported' ELSE cache_capability END,
    cache_miss_tokens=0, cache_total_input_tokens=0
WHERE usage_provider='kiro' AND usage_source='upstream'
  AND prompt_tokens=0 AND completion_tokens=0 AND total_tokens=0
  AND cached_tokens=0 AND cache_read_tokens=0 AND cache_creation_tokens=0
  AND cache_read_present=0 AND cache_creation_present=0`); err != nil {
			return err
		}
		return tx.Commit()
	}(); err != nil {
		return err
	}

	type row struct {
		id                                int64
		model, provider, source, raw      string
		cacheReadPresent, createPresent   int
		prompt, cached, cacheRead, create int64
	}
	afterID = 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := s.rdb.QueryContext(ctx, `
SELECT id, model, usage_provider, usage_source, cache_read_present, cache_creation_present, prompt_tokens, cached_tokens,
       CASE WHEN cache_read_tokens > 0 THEN cache_read_tokens ELSE cached_tokens END,
       cache_creation_tokens, raw_usage_json
FROM usage_records
WHERE id > ? AND cache_total_input_tokens = 0
  AND (prompt_tokens > 0 OR cached_tokens > 0 OR cache_read_tokens > 0 OR cache_creation_tokens > 0
       OR raw_usage_json LIKE '%cache_read_input_tokens%' OR raw_usage_json LIKE '%cache_creation_input_tokens%' OR raw_usage_json LIKE '%estimated%')
ORDER BY id LIMIT ?`, afterID, batchSize)
		if err != nil {
			return err
		}
		items := make([]row, 0, batchSize)
		scanned := 0
		for rows.Next() {
			var r row
			if err = rows.Scan(&r.id, &r.model, &r.provider, &r.source, &r.cacheReadPresent, &r.createPresent, &r.prompt, &r.cached, &r.cacheRead, &r.create, &r.raw); err != nil {
				_ = rows.Close()
				return err
			}
			afterID = r.id
			scanned++
			items = append(items, r)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		if len(items) > 0 {
			if err = func() error {
				tx, txErr := s.db.BeginTx(ctx, nil)
				if txErr != nil {
					return txErr
				}
				defer tx.Rollback()
				for _, r := range items {
					diag := finalizeUsageDiagnostics(r.model, r.prompt, r.cached, r.cacheRead, r.create, json.RawMessage(r.raw), UsageDiagnostics{
						UsageProvider: r.provider, UsageSource: r.source,
						CacheReadPresent: r.cacheReadPresent != 0, CacheCreationPresent: r.createPresent != 0,
					})
					if _, txErr = tx.ExecContext(ctx, `
UPDATE usage_records
SET usage_provider = ?, usage_source = ?, cache_read_present = ?, cache_creation_present = ?, estimated = ?, cache_miss_tokens = ?, cache_total_input_tokens = ?,
    cache_creation_5m_tokens = ?, cache_creation_1h_tokens = ?
WHERE id = ?`,
						diag.UsageProvider, diag.UsageSource, boolInt(diag.CacheReadPresent), boolInt(diag.CacheCreationPresent), boolInt(diag.Estimated), diag.CacheMissTokens, diag.CacheTotalInputTokens,
						diag.CacheCreation5mTokens, diag.CacheCreation1hTokens, r.id); txErr != nil {
						return txErr
					}
				}
				return tx.Commit()
			}(); err != nil {
				return err
			}
		}
		if scanned < batchSize {
			break
		}
	}
	return nil
}

// backfillUserGroups seeds the user_groups / user_group_targets tables from the
// legacy groups table (zero-downtime P1 migration). For every existing group G
// that doesn't already have a user_group record, it:
//  1. Creates a user_group with id = deterministic UUID v5 (namespace + G.Name),
//     copying the prompt-injection config verbatim.
//  2. Adds a user_group_target(target_type='base_group', target_ref=G.Name).
//  3. Re-points api_keys.user_group_id for all keys in group G (idempotent).
//
// Runs inside its own transaction; skips rows that already exist (ON CONFLICT DO NOTHING).
func (s *Store) backfillUserGroups(ctx context.Context) error {
	existing := map[string]bool{}
	nameRows, err := s.rdb.QueryContext(ctx, `SELECT name FROM user_groups`)
	if err != nil {
		return err
	}
	for nameRows.Next() {
		var n string
		if err := nameRows.Scan(&n); err != nil {
			nameRows.Close()
			return err
		}
		existing[n] = true
	}
	if err := nameRows.Err(); err != nil {
		return err
	}
	nameRows.Close()

	// Load all legacy groups.
	gRows, err := s.rdb.QueryContext(ctx, `SELECT name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, model_instructions_enabled, model_instructions_files, force_model, force_effort, created_at FROM groups`)
	if err != nil {
		return err
	}
	type legacyGroup struct {
		name, systemPrompt, promptMode, filesJSON, forceModel, forceEffort string
		apply, miEnabled                                                   int
		createdAt                                                          int64
	}
	var groups []legacyGroup
	for gRows.Next() {
		var g legacyGroup
		if err := gRows.Scan(&g.name, &g.systemPrompt, &g.promptMode, &g.apply, &g.miEnabled, &g.filesJSON, &g.forceModel, &g.forceEffort, &g.createdAt); err != nil {
			gRows.Close()
			return err
		}
		groups = append(groups, g)
	}
	if err := gRows.Err(); err != nil {
		return err
	}
	gRows.Close()

	now := Now()
	for _, g := range groups {
		if existing[g.name] {
			continue
		}
		// Deterministic ID: sha256 of "user_group:" + name, hex-encoded first 32 chars.
		h := sha256userGroupID(g.name)
		if g.promptMode == "" {
			g.promptMode = "prepend"
		}
		if g.createdAt == 0 {
			g.createdAt = now
		}
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_groups(id, name, system_prompt, prompt_mode, system_prompt_apply_to_compaction, model_instructions_enabled, model_instructions_files, force_model, force_effort, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`,
			h, g.name, g.systemPrompt, g.promptMode, g.apply, g.miEnabled, g.filesJSON, g.forceModel, g.forceEffort, g.createdAt, now)
		if err != nil {
			tx.Rollback()
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO user_group_targets(user_group_id, target_type, target_ref, affinity_weight, created_at) VALUES(?,?,?,?,?) ON CONFLICT(user_group_id, target_type, target_ref) DO NOTHING`,
			h, UserGroupTargetTypeBaseGroup, g.name, 1, g.createdAt)
		if err != nil {
			tx.Rollback()
			return err
		}
		// Re-point api_keys that are still on the legacy group_name path.
		_, err = tx.ExecContext(ctx, `UPDATE api_keys SET user_group_id = ?, updated_at = ? WHERE group_name = ? AND (user_group_id = '' OR user_group_id IS NULL)`,
			h, now, g.name)
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// sha256userGroupID derives a stable, collision-resistant string ID for a user_group
// from the group name using the first 32 hex chars of SHA-256("user_group:\x00"+name).
func sha256userGroupID(name string) string {
	h := fnv32userGroup("user_group:\x00" + name)
	// Use a longer deterministic form: hex of the 4-byte FNV hash padded to 32 chars
	// so it looks like a compact UUID without the google/uuid dependency in storage.
	return fmt.Sprintf("ug_%016x", h)
}

func fnv32userGroup(s string) uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
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
	ByProvider          []CacheUsageMetricRow `json:"by_provider"`
	ByProviderModel     []CacheUsageMetricRow `json:"by_provider_model"`
	ByRoute             []CacheUsageMetricRow `json:"by_route"`
	ByRouteAccountModel []CacheUsageMetricRow `json:"by_route_account_model"`
	ByTimeBucket        []CacheUsageBucket    `json:"by_time_bucket"`
}

type CacheUsageMetricRow struct {
	AccountID                         string   `json:"account_id,omitempty"`
	Model                             string   `json:"model,omitempty"`
	Provider                          string   `json:"provider,omitempty"`
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
	ActualRequests                    int64    `json:"actual_requests"`
	ActualPromptTokens                int64    `json:"actual_prompt_tokens"`
	ActualCompletionTokens            int64    `json:"actual_completion_tokens"`
	ActualTotalTokens                 int64    `json:"actual_total_tokens"`
	EstimatedPromptTokens             int64    `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens         int64    `json:"estimated_completion_tokens"`
	EstimatedTotalTokens              int64    `json:"estimated_total_tokens"`
	CombinedRequests                  int64    `json:"combined_requests"`
	CombinedTotalTokens               int64    `json:"combined_total_tokens"`
	CacheReportedRequests             int64    `json:"cache_reported_requests"`
	CacheUnreportedRequests           int64    `json:"cache_unreported_requests"`
	CacheReportingRate                float64  `json:"cache_reporting_rate"`
	CacheReportingState               string   `json:"cache_reporting_state"`
	KiroCredits                       float64  `json:"kiro_credits"`
	KiroCreditsReportedRequests       int64    `json:"kiro_credits_reported_requests"`
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
	Partial                       bool    `json:"partial"`
}

type UsageCompleteness struct {
	DataSnapshotAt         int64 `json:"data_snapshot_at"`
	UsageCompleteThroughAt int64 `json:"usage_complete_through_at"`
	UsageLagSeconds        int64 `json:"usage_lag_seconds"`
	PendingUsageRequests   int64 `json:"pending_usage_requests"`
	PartialData            bool  `json:"partial_data"`
	TelemetryFlushTimedOut bool  `json:"telemetry_flush_timed_out"`
	CompletenessGapCount   int64 `json:"completeness_gap_count"`
}

func (s *Store) UsageCompleteness(ctx context.Context, snapshotAt int64) (UsageCompleteness, error) {
	if snapshotAt <= 0 {
		snapshotAt = Now()
	}
	cutoff := snapshotAt - 2*60*60
	var pending, earliest, gaps int64
	// This endpoint feeds every usage/dashboard read. It must remain read-only:
	// turning an observability GET into a write transaction made concurrent UI
	// refreshes contend with context and telemetry commits on SQLite's single
	// writer. Unreconciled old holds are classified as gaps directly in the
	// aggregate, so the answer stays correct while the background reconciler is
	// waiting for a safe write window.
	err := s.rdb.QueryRowContext(ctx, `SELECT
	COALESCE(SUM(CASE WHEN usage_expected=1 AND usage_recorded_at=0 AND created_at>=? AND status<>'usage_missing' THEN 1 ELSE 0 END),0),
	COALESCE(MIN(CASE WHEN usage_expected=1 AND usage_recorded_at=0 AND created_at>=? AND status<>'usage_missing' THEN created_at END),0),
	COALESCE(SUM(CASE WHEN status='usage_missing' OR (usage_expected=1 AND usage_recorded_at=0 AND created_at<?) THEN 1 ELSE 0 END),0)
FROM billing_holds`, cutoff, cutoff, cutoff).Scan(&pending, &earliest, &gaps)
	if err != nil {
		return UsageCompleteness{}, err
	}
	watermark := snapshotAt - 5
	if earliest > 0 && earliest < watermark {
		watermark = earliest
	}
	if watermark < 0 {
		watermark = 0
	}
	return UsageCompleteness{
		DataSnapshotAt: snapshotAt, UsageCompleteThroughAt: watermark,
		UsageLagSeconds: snapshotAt - watermark, PendingUsageRequests: pending,
		PartialData: pending > 0 || gaps > 0, CompletenessGapCount: gaps,
	}, nil
}

// ReconcileUsageMissing durably marks billing holds whose upstream usage never
// arrived. It is deliberately separate from UsageCompleteness so control-plane
// reads never acquire the SQLite write lock. The active-role billing maintenance
// loop calls this operation with a bounded context; the predicate and transaction
// make retries idempotent.
func (s *Store) ReconcileUsageMissing(ctx context.Context, snapshotAt int64) (int64, error) {
	if snapshotAt <= 0 {
		snapshotAt = Now()
	}
	cutoff := snapshotAt - 2*60*60
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_log(account_id, action, state, reason, detail, created_at)
SELECT account_id, 'usage_missing', 'warning', 'billing hold exceeded two-hour usage deadline', id, ?
FROM billing_holds WHERE usage_expected=1 AND usage_recorded_at=0 AND created_at < ? AND status <> 'usage_missing'`, snapshotAt, cutoff); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE billing_holds SET status='usage_missing', updated_at=? WHERE usage_expected=1 AND usage_recorded_at=0 AND created_at < ? AND status <> 'usage_missing'`, snapshotAt, cutoff)
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
	var (
		report              CacheUsageReport
		summary             CacheUsageMetricRow
		byAccount           []CacheUsageMetricRow
		byModel             []CacheUsageMetricRow
		byAPIKey            []CacheUsageMetricRow
		byAccountModel      []CacheUsageMetricRow
		byProvider          []CacheUsageMetricRow
		byProviderModel     []CacheUsageMetricRow
		byRoute             []CacheUsageMetricRow
		byRouteAccountModel []CacheUsageMetricRow
		byTimeBucket        []CacheUsageBucket
		tasks               []func() error
	)
	if want("summary") {
		tasks = append(tasks, func() error {
			var err error
			summary, err = s.cacheUsageSummary(ctx, since, until)
			return err
		})
	}
	if want("by_account") {
		tasks = append(tasks, func() error {
			var err error
			byAccount, err = s.cacheUsageRows(ctx, since, until, "account", 200)
			return err
		})
	}
	if want("by_model") {
		tasks = append(tasks, func() error {
			var err error
			byModel, err = s.cacheUsageRows(ctx, since, until, "model", 200)
			return err
		})
	}
	if want("by_api_key") {
		tasks = append(tasks, func() error {
			var err error
			byAPIKey, err = s.cacheUsageRows(ctx, since, until, "api_key", 200)
			return err
		})
	}
	if want("by_account_model") {
		tasks = append(tasks, func() error {
			var err error
			byAccountModel, err = s.cacheUsageRows(ctx, since, until, "account_model", 200)
			return err
		})
	}
	if want("by_provider") {
		tasks = append(tasks, func() error {
			var err error
			byProvider, err = s.cacheUsageRows(ctx, since, until, "provider", 200)
			return err
		})
	}
	if want("by_provider_model") {
		tasks = append(tasks, func() error {
			var err error
			byProviderModel, err = s.cacheUsageRows(ctx, since, until, "provider_model", 200)
			return err
		})
	}
	if want("by_route") {
		tasks = append(tasks, func() error {
			var err error
			byRoute, err = s.cacheUsageRows(ctx, since, until, "route", routeLimit)
			return err
		})
	}
	if want("by_route_account_model") {
		tasks = append(tasks, func() error {
			var err error
			byRouteAccountModel, err = s.cacheUsageRows(ctx, since, until, "route_account_model", routeLimit)
			return err
		})
	}
	if want("by_time_bucket") {
		tasks = append(tasks, func() error {
			var err error
			byTimeBucket, err = s.CacheUsageBucketsWindow(ctx, since, until, 3600)
			return err
		})
	}
	var wg sync.WaitGroup
	errorsByTask := make(chan error, len(tasks))
	for _, task := range tasks {
		wg.Add(1)
		go func(run func() error) {
			defer wg.Done()
			defer func() {
				if panicValue := recover(); panicValue != nil {
					supervisor.LogPanic("cache-usage-query", panicValue)
					errorsByTask <- errors.New("cache usage query failed")
				}
			}()
			if err := run(); err != nil {
				errorsByTask <- err
			}
		}(task)
	}
	wg.Wait()
	close(errorsByTask)
	if err := <-errorsByTask; err != nil {
		return CacheUsageReport{}, err
	}
	report.Summary = summary
	report.ByAccount = byAccount
	report.ByModel = byModel
	report.ByAPIKey = byAPIKey
	report.ByAccountModel = byAccountModel
	report.ByProvider = byProvider
	report.ByProviderModel = byProviderModel
	report.ByRoute = byRoute
	report.ByRouteAccountModel = byRouteAccountModel
	report.ByTimeBucket = byTimeBucket
	return report, nil
}

func (s *Store) cacheUsageSummary(ctx context.Context, since, until int64) (CacheUsageMetricRow, error) {
	if rolled, ok, err := s.cacheUsageSummaryHourlyWindow(ctx, since, until); err != nil {
		return CacheUsageMetricRow{}, err
	} else if ok {
		return rolled, nil
	}
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
       COALESCE(SUM(cache_creation_present),0),
       COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN prompt_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN completion_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN total_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN usage_provider='kiro' AND (cache_read_present>0 OR cache_creation_present>0) THEN 1 ELSE 0 END),0),
       COALESCE(SUM(CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 1 ELSE 0 END),0),
       COALESCE(SUM(kiro_credits),0), COALESCE(SUM(kiro_credits_present),0)
FROM usage_records
WHERE created_at >= ? AND created_at < ?`, since, until).Scan(&row.Requests, &row.RealRequests, &row.HitRequests, &row.PromptTokens, &row.CachedTokens, &row.CacheInputTokens, &row.CacheMissTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.CacheCreation5mTokens, &row.CacheCreation1hTokens, &row.EstimatedRequests, &row.realCacheInputTokens, &row.realCacheReadTokens, &row.CacheCreationReportedRequests,
		&row.ActualRequests, &row.ActualPromptTokens, &row.ActualCompletionTokens, &row.ActualTotalTokens,
		&row.EstimatedPromptTokens, &row.EstimatedCompletionTokens, &row.EstimatedTotalTokens,
		&row.CacheReportedRequests, &row.CacheUnreportedRequests, &row.KiroCredits, &row.KiroCreditsReportedRequests)
	if err != nil {
		return CacheUsageMetricRow{}, err
	}
	finalizeCacheUsageMetric(&row)
	return row, nil
}

func (s *Store) cacheUsageRows(ctx context.Context, since, until int64, dimension string, limit int) ([]CacheUsageMetricRow, error) {
	selectCols := "COALESCE(account_id,''), '', '', '', '', '', '', '', '', '', '', ''"
	groupBy := "account_id"
	switch dimension {
	case "account":
	case "model":
		selectCols = "'', " + normalizedUsageModelSQL + ", '', '', '', '', '', '', '', '', '', ''"
		groupBy = normalizedUsageModelSQL
	case "api_key":
		selectCols = "'', '', '', CASE WHEN COALESCE(api_key_hash,'') = '' THEN '' ELSE substr(api_key_hash,1,12) END, '', '', '', '', '', '', '', ''"
		groupBy = "CASE WHEN COALESCE(api_key_hash,'') = '' THEN '' ELSE substr(api_key_hash,1,12) END"
	case "account_model":
		selectCols = "COALESCE(account_id,''), " + normalizedUsageModelSQL + ", '', '', '', '', '', '', '', '', '', ''"
		groupBy = "account_id, " + normalizedUsageModelSQL
	case "provider":
		selectCols = "'', '', COALESCE(usage_provider,''), '', '', '', '', '', '', '', '', ''"
		groupBy = "usage_provider"
	case "provider_model":
		selectCols = "'', " + normalizedUsageModelSQL + ", COALESCE(usage_provider,''), '', '', '', '', '', '', '', '', ''"
		groupBy = "usage_provider, " + normalizedUsageModelSQL
	case "route":
		selectCols = "'', '', '', '', CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, COALESCE(affinity_source,''), COALESCE(prompt_cache_key_source,''), COALESCE(stable_prefix_source,''), COALESCE(stable_prefix_reason,''), COALESCE(retention_effective,''), COALESCE(retention_source,''), COALESCE(claude_cache_ttl,'')"
		groupBy = "CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, affinity_source, prompt_cache_key_source, stable_prefix_source, stable_prefix_reason, retention_effective, retention_source, claude_cache_ttl"
	case "route_account_model":
		selectCols = "COALESCE(account_id,''), " + normalizedUsageModelSQL + ", '', '', CASE WHEN COALESCE(route_key_hash,'') = '' THEN '' ELSE substr(route_key_hash,1,12) END, COALESCE(affinity_source,''), COALESCE(prompt_cache_key_source,''), COALESCE(stable_prefix_source,''), COALESCE(stable_prefix_reason,''), COALESCE(retention_effective,''), COALESCE(retention_source,''), COALESCE(claude_cache_ttl,'')"
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
       ,COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN 1 ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN prompt_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN completion_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated=0 AND usage_source='upstream' THEN total_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN usage_provider='kiro' AND (cache_read_present>0 OR cache_creation_present>0) THEN 1 ELSE 0 END),0)
       ,COALESCE(SUM(CASE WHEN usage_provider='kiro' AND cache_read_present=0 AND cache_creation_present=0 THEN 1 ELSE 0 END),0)
       ,COALESCE(SUM(kiro_credits),0), COALESCE(SUM(kiro_credits_present),0)
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
			&row.AccountID, &row.Model, &row.Provider, &row.APIKeyHashPrefix, &row.RouteKeyHashPrefix,
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
			&row.ActualRequests, &row.ActualPromptTokens, &row.ActualCompletionTokens, &row.ActualTotalTokens,
			&row.EstimatedPromptTokens, &row.EstimatedCompletionTokens, &row.EstimatedTotalTokens,
			&row.CacheReportedRequests, &row.CacheUnreportedRequests, &row.KiroCredits, &row.KiroCreditsReportedRequests,
		); err != nil {
			return nil, err
		}
		if dimension == "model" || dimension == "account_model" || dimension == "route_account_model" || dimension == "provider_model" {
			row.Model = applyUsageModelFields(row.Model, &row.ModelKey, &row.ModelLabel)
		}
		finalizeCacheUsageMetric(&row)
		out = append(out, row)
	}
	return out, rows.Err()
}

func finalizeCacheUsageMetric(row *CacheUsageMetricRow) {
	row.CombinedRequests = row.Requests
	row.CombinedTotalTokens = row.ActualTotalTokens + row.EstimatedTotalTokens
	kiroRequests := row.CacheReportedRequests + row.CacheUnreportedRequests
	if kiroRequests > 0 {
		row.CacheReportingRate = float64(row.CacheReportedRequests) / float64(kiroRequests)
		switch {
		case row.CacheReportedRequests == 0:
			row.CacheReportingState = "unreported"
		case row.CacheUnreportedRequests == 0:
			row.CacheReportingState = "reported"
		default:
			row.CacheReportingState = "partial"
		}
	} else if row.Provider != "" {
		row.CacheReportingState = "not_applicable"
	}
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
	if rolled, err := s.cacheUsageHourlyWindow(ctx, since, until, bucketSeconds); err != nil {
		return nil, err
	} else if rolled != nil {
		return rolled, nil
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
	AccountID                 string  `json:"account_id"`
	Requests                  int64   `json:"requests"`
	PromptTokens              int64   `json:"prompt_tokens"`
	CompletionTokens          int64   `json:"completion_tokens"`
	TotalTokens               int64   `json:"total_tokens"`
	CachedTokens              int64   `json:"cached_tokens"`
	CacheReadTokens           int64   `json:"cache_read_tokens"`
	CacheCreationTokens       int64   `json:"cache_creation_tokens"`
	ActualRequests            int64   `json:"actual_requests"`
	ActualTokens              int64   `json:"actual_tokens"`
	ActualPromptTokens        int64   `json:"actual_prompt_tokens"`
	ActualCompletionTokens    int64   `json:"actual_completion_tokens"`
	ActualTotalTokens         int64   `json:"actual_total_tokens"`
	EstimatedRequests         int64   `json:"estimated_requests"`
	EstimatedTokens           int64   `json:"estimated_tokens"`
	EstimatedPromptTokens     int64   `json:"estimated_prompt_tokens"`
	EstimatedCompletionTokens int64   `json:"estimated_completion_tokens"`
	EstimatedTotalTokens      int64   `json:"estimated_total_tokens"`
	CombinedRequests          int64   `json:"combined_requests"`
	CombinedTotalTokens       int64   `json:"combined_total_tokens"`
	EstimatedRate             float64 `json:"estimated_rate"`
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
	   SUM(CASE WHEN estimated>0 THEN 1 ELSE 0 END), COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0),
	   COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0), COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0)
FROM usage_records WHERE created_at >= ? AND created_at < ? GROUP BY account_id ORDER BY SUM(total_tokens) DESC`, since, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageSummaryRow
	for rows.Next() {
		var row UsageSummaryRow
		if err := rows.Scan(&row.AccountID, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.ActualRequests, &row.ActualTokens, &row.EstimatedRequests, &row.EstimatedTokens, &row.EstimatedPromptTokens, &row.EstimatedCompletionTokens); err != nil {
			return nil, err
		}
		finalizeUsageSummaryRow(&row)
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
	   COALESCE(SUM(CASE WHEN estimated>0 THEN total_tokens ELSE 0 END),0),
	   COALESCE(SUM(CASE WHEN estimated>0 THEN prompt_tokens ELSE 0 END),0),
	   COALESCE(SUM(CASE WHEN estimated>0 THEN completion_tokens ELSE 0 END),0)
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
		if err := rows.Scan(&row.AccountID, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.CachedTokens, &row.CacheReadTokens, &row.CacheCreationTokens, &row.ActualRequests, &row.ActualTokens, &row.EstimatedRequests, &row.EstimatedTokens, &row.EstimatedPromptTokens, &row.EstimatedCompletionTokens); err != nil {
			return nil, err
		}
		finalizeUsageSummaryRow(&row)
		out[row.AccountID] = row
	}
	return out, rows.Err()
}

func finalizeUsageSummaryRow(row *UsageSummaryRow) {
	row.ActualPromptTokens, row.ActualCompletionTokens, row.ActualTotalTokens = row.PromptTokens, row.CompletionTokens, row.ActualTokens
	row.EstimatedTotalTokens = row.EstimatedTokens
	row.CombinedRequests = row.ActualRequests + row.EstimatedRequests
	row.CombinedTotalTokens = row.ActualTotalTokens + row.EstimatedTotalTokens
	if row.CombinedRequests > 0 {
		row.EstimatedRate = float64(row.EstimatedRequests) / float64(row.CombinedRequests)
	}
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
	Partial             bool  `json:"partial"`
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
	if rolled, err := s.usageTimeseriesHourlyWindow(ctx, since, until, bucketSeconds); err != nil {
		return nil, err
	} else if rolled != nil {
		return rolled, nil
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
	id := "hold_" + uuid.NewString()
	err := s.BatchWriteTelemetry(ctx, nil, nil, []BillingHoldWrite{{ID: id, EventID: "usage_" + id, RouteKeyHash: routeKeyHash, AccountID: accountID, EstimatedTokens: estimatedTokens, CreatedAt: now, Create: true}}, nil)
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
	return s.BatchWriteTelemetry(ctx, nil, nil, []BillingHoldWrite{{ID: id, Status: status, CreatedAt: Now()}}, nil)
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
	return s.BatchWriteTelemetry(ctx, nil, nil, []BillingHoldWrite{{ID: id, Status: status, CreatedAt: Now(), IfHeld: true}}, nil)
}

func (s *Store) ExpireStaleBillingHolds(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = time.Hour
	}
	cutoff := Now() - int64(olderThan/time.Second)
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE billing_holds SET status='expired_unsettled', usage_expected=0, updated_at=? WHERE status='held' AND created_at<?`, now, cutoff)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_events SET terminal_status='expired_unsettled', settled_at=?, updated_at=MAX(updated_at,?)
WHERE hold_id IN (SELECT id FROM billing_holds WHERE status='expired_unsettled' AND updated_at=?)`, now, now, now); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) GetBillingHold(ctx context.Context, id string) (BillingHold, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, route_key_hash, account_id, estimated_tokens, status, usage_expected, usage_recorded_at, created_at, updated_at FROM billing_holds WHERE id = ?`, id)
	var hold BillingHold
	var usageExpected int
	err := row.Scan(&hold.ID, &hold.RouteKeyHash, &hold.AccountID, &hold.EstimatedTokens, &hold.Status, &usageExpected, &hold.UsageRecordedAt, &hold.CreatedAt, &hold.UpdatedAt)
	hold.UsageExpected = usageExpected != 0
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
	var ignoreRateLimitControls int
	var forceCodex429 int
	// These identity fields were nullable in the original schema and can still be
	// NULL in databases populated by older importers or direct administrative
	// tooling. Scanning a SQL NULL into a Go string aborts the entire account-list
	// response, so normalize the legacy representation at the storage boundary.
	var upstreamAccountID sql.NullString
	var chatGPTUserID sql.NullString
	var email sql.NullString
	var planType sql.NullString
	err := row.Scan(&acc.ID, &acc.Label, &acc.GroupName, &upstreamAccountID, &chatGPTUserID, &email, &planType, &acc.Provider, &acc.Status, &fed, &ignoreRateLimitControls, &forceCodex429, &acc.RoutingWeight, &acc.RetryMaxAttempts, &acc.QuarantineUntil, &acc.QuarantineReason, &acc.CreatedAt, &acc.UpdatedAt)
	if err != nil {
		return Account{}, err
	}
	acc.UpstreamAccountID = upstreamAccountID.String
	acc.ChatGPTUserID = chatGPTUserID.String
	acc.Email = email.String
	acc.PlanType = planType.String
	acc.IsFedramp = fed != 0
	acc.IgnoreRateLimitControls = ignoreRateLimitControls != 0
	acc.ForceCodex429 = forceCodex429 != 0
	if acc.RoutingWeight <= 0 {
		acc.RoutingWeight = 100
	}
	return acc, nil
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
