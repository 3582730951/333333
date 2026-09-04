package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/admission"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

var ErrNoAccount = errors.New("no active account available")
var ErrStrictUnavailable = errors.New("strict sticky account unavailable")
var ErrBoundAccountUnavailable = errors.New("bound account unavailable")

const (
	schedulerLoadShardCount      = 64
	schedulerEWMAEntriesPerShard = 64
)

type schedulerLoadShard struct {
	mu       sync.RWMutex
	inflight map[string]int
	tokens   map[string]int64
	egress   map[string]int
}

// shardedLoadCounters prevents unrelated accounts and outlets from contending
// on the scheduler state/notification mutex at high concurrency.
type shardedLoadCounters struct {
	shards [schedulerLoadShardCount]schedulerLoadShard
}

func newShardedLoadCounters() *shardedLoadCounters {
	l := &shardedLoadCounters{}
	for i := range l.shards {
		l.shards[i].inflight = map[string]int{}
		l.shards[i].tokens = map[string]int64{}
		l.shards[i].egress = map[string]int{}
	}
	return l
}

func schedulerLoadHash(value string) uint32 {
	var sum uint32 = 2166136261
	for i := 0; i < len(value); i++ {
		sum ^= uint32(value[i])
		sum *= 16777619
	}
	return sum
}

func (l *shardedLoadCounters) shard(key string) *schedulerLoadShard {
	return &l.shards[schedulerLoadHash(key)&(schedulerLoadShardCount-1)]
}

func (l *shardedLoadCounters) addAccount(id string, inflight int, tokens int64) {
	shard := l.shard(id)
	shard.mu.Lock()
	nextInflight := shard.inflight[id] + inflight
	if nextInflight <= 0 {
		delete(shard.inflight, id)
	} else {
		shard.inflight[id] = nextInflight
	}
	nextTokens := shard.tokens[id] + tokens
	if nextTokens <= 0 {
		delete(shard.tokens, id)
	} else {
		shard.tokens[id] = nextTokens
	}
	shard.mu.Unlock()
}

func (l *shardedLoadCounters) account(id string) (int, int64) {
	shard := l.shard(id)
	shard.mu.RLock()
	inflight, tokens := shard.inflight[id], shard.tokens[id]
	shard.mu.RUnlock()
	return inflight, tokens
}

func (l *shardedLoadCounters) addEgress(id string, delta int) {
	if strings.TrimSpace(id) == "" {
		return
	}
	shard := l.shard(id)
	shard.mu.Lock()
	next := shard.egress[id] + delta
	if next <= 0 {
		delete(shard.egress, id)
	} else {
		shard.egress[id] = next
	}
	shard.mu.Unlock()
}

func (l *shardedLoadCounters) egressLoad(id string) int {
	shard := l.shard(id)
	shard.mu.RLock()
	load := shard.egress[id]
	shard.mu.RUnlock()
	return load
}

func (l *shardedLoadCounters) active() int64 {
	var active int64
	for i := range l.shards {
		shard := &l.shards[i]
		shard.mu.RLock()
		for _, count := range shard.inflight {
			active += int64(count)
		}
		shard.mu.RUnlock()
	}
	return active
}

type egressEWMA struct {
	LatencyMillis float64
	Success       float64
	Samples       uint64
	UpdatedAt     time.Time
}

type egressEWMAShard struct {
	mu   sync.RWMutex
	rows map[string]egressEWMA
}

type Scheduler struct {
	store           *storage.Store
	cfg             atomic.Value // stores config.Config
	mu              sync.Mutex
	acrossMu        sync.Mutex
	inflight        map[string]int
	inflightTokens  map[string]int64
	egressInflight  map[string]int
	loads           *shardedLoadCounters
	egressEWMA      [schedulerLoadShardCount]egressEWMAShard
	loadChanged     chan struct{}
	rr              int // round-robin cursor for spreading load across equal-load candidates
	queueMu         sync.Mutex
	waitQueues      map[string]*waitQueue
	metrics         SchedulerMetrics
	admission       *admission.Controller
	coordinator     LeaseCoordinator
	coordinatorStop chan struct{}
	coordinatorWG   sync.WaitGroup
	dynamicBalance  *dynamicPoolBalanceRuntime

	// accountCache caches the active accounts list per group with a short TTL to reduce
	// DB queries on the hot path. The cache is invalidated when an account's status
	// changes (quarantine, enable/disable) — callers that modify accounts should call
	// InvalidateAccountCache() afterward. TTL is short (5s) so a newly-enabled account
	// becomes available quickly while still amortizing the DB round-trip.
	accountCache      map[string][]storage.Account
	accountCacheTTL   time.Time
	accountCacheMutex sync.RWMutex
	selectionCache    map[string]*accountSelectionSnapshot
	selectionVersion  atomic.Uint64
	// routeStructureVersion changes only when group/provider/model compatibility
	// may have changed. Transient load, quota, cooldown and egress notifications do
	// not invalidate a structural zero-candidate marker.
	routeStructureVersion atomic.Uint64
	rateLimitCache        map[string]map[string][]storage.AccountRateLimit
	rateLimitCacheGen     map[string]uint64
	rateLimitSelectionGen map[string]uint64
	modelCache            map[string]map[string]bool
	auxCacheAt            map[string]time.Time
	affinityCache         *affinityCacheStore
	accountRefreshMu      sync.Mutex
	rateRefreshMu         sync.Mutex
	modelRefreshMu        sync.Mutex
	candidateIndexes      sync.Map // key -> *routeCandidateIndex; avoids whole-map copy on publication
	candidateIndexCount   atomic.Int64
	candidateRefreshMu    sync.Mutex

	// kiroBlockLogged dedupes the Kiro model-resolution diagnostic (see
	// logKiroModelBlockOnce). Diagnostic-only state: it never affects selection.
	kiroBlockLogMu  sync.Mutex
	kiroBlockLogged map[string]struct{}

	// egressCache is a process-level cache for the one effective egress selected for
	// each account. It is separate from the request-scoped cache so concurrent requests
	// avoid repeated DB lookups for the same group/account outlet.
	egressCache      sync.Map // map[string]storage.EgressProfile
	egressCacheTime  time.Time
	egressCacheTTL   time.Duration
	egressCacheMutex sync.RWMutex

	// providerCache caches provider inference results (from token shape) per account.
	// Provider is set explicitly on import for new accounts, so the cache is only hit
	// for legacy rows with an empty provider column.
	providerCache      sync.Map // map[string]string
	providerCacheTime  time.Time
	providerCacheTTL   time.Duration
	providerCacheMutex sync.Mutex
}

// candidate is a selection candidate combining account, egress, binding, and load
// score. Defined at package level so tryLeaseAccountFromCandidate can reference it.
// The binding is carried from the batch ListActiveAccountsWithEgress query so the
// lease can be built without a second per-request DB read while holding s.mu.
type candidate struct {
	account       storage.Account
	egress        storage.EgressProfile
	binding       storage.AccountEgressBinding
	resolvedModel string
	bootstrap     bool    // concrete Kiro model is static-known but not runtime-verified
	score         float64 // normalized account/token/egress load; lower is better
	trial         bool    // selected via intelligent-routing cooldown trial (stale cooldown)
}

type accountSelectionSnapshot struct {
	group      string
	rows       []storage.AccountWithEgress
	byID       map[string]storage.AccountWithEgress
	accountIDs []string
	generation uint64
	at         time.Time
}

type rankedCandidate struct {
	candidate candidate
	rank      uint64
}

type waiter struct {
	started time.Time
	prev    *waiter
	next    *waiter
	queue   *waitQueue
	queued  bool
}

// waitQueue is an intrusive FIFO. A waiter owns its links, so cancellation and
// successful dequeue are O(1) even with thousands of queued requests.
type waitQueue struct {
	head *waiter
	tail *waiter
	len  int
}

// SchedulerMetrics is a cheap runtime snapshot used by diagnostics and tests. Values
// are process-local and intentionally monotonic except Queued, which is a gauge.
type SchedulerMetrics struct {
	Active                                   int64 `json:"active"`
	Queued                                   int64 `json:"queued"`
	Waited                                   int64 `json:"waited"`
	Cancelled                                int64 `json:"cancelled"`
	AccountSwitches                          int64 `json:"account_switches"`
	WaitConcurrency                          int64 `json:"wait_concurrency"`
	WaitTokenBudget                          int64 `json:"wait_token_budget"`
	WaitCooldown                             int64 `json:"wait_cooldown"`
	WaitRecheck                              int64 `json:"wait_recheck"`
	WaitEgress                               int64 `json:"wait_egress"`
	CooldownWakeups                          int64 `json:"cooldown_wakeups"`
	StateWakeups                             int64 `json:"state_wakeups"`
	WaitNanos                                int64 `json:"wait_nanos"`
	RouteSelects                             int64 `json:"route_selects"`
	RouteNanos                               int64 `json:"route_nanos"`
	RouteAvgNanos                            int64 `json:"route_avg_nanos"`
	RouteMaxNanos                            int64 `json:"route_max_nanos"`
	CandidateEvaluations                     int64 `json:"candidate_evaluations"`
	CandidateFallbacks                       int64 `json:"candidate_fallbacks"`
	CandidateIndexBuilds                     int64 `json:"candidate_index_builds"`
	CandidateIndexEntries                    int64 `json:"candidate_index_entries"`
	TrialSelections                          int64 `json:"trial_selections"`
	EgressEWMAOutlets                        int64 `json:"egress_ewma_outlets"`
	EgressEWMASamples                        int64 `json:"egress_ewma_samples"`
	DynamicPoolBalanceSelections             int64 `json:"dynamic_pool_balance_selections"`
	DynamicPoolBalanceTargetSelections       int64 `json:"dynamic_pool_balance_target_selections"`
	DynamicPoolBalanceSkipped                int64 `json:"dynamic_pool_balance_skipped"`
	DynamicPoolBalanceSkippedUnknownInterval int64 `json:"dynamic_pool_balance_skipped_unknown_interval"`
	DynamicPoolBalanceSkippedSnapshot        int64 `json:"dynamic_pool_balance_skipped_snapshot"`
	DynamicPoolBalanceSnapshotErrors         int64 `json:"dynamic_pool_balance_snapshot_errors"`
	DynamicPoolBalanceReservationCount       int64 `json:"dynamic_pool_balance_reservations"`
}

type leaseBlockReason string

const (
	leaseBlockNone              leaseBlockReason = ""
	leaseBlockTokenBudget       leaseBlockReason = "token_budget"
	leaseBlockConcurrency       leaseBlockReason = "concurrency"
	leaseBlockRateLimitCooldown leaseBlockReason = "rate_limit_cooldown"
	leaseBlockRecheckPending    leaseBlockReason = "recheck_pending"
	leaseBlockEgressCooldown    leaseBlockReason = "egress_cooldown"
	leaseBlockEgressUnavailable leaseBlockReason = "egress_unavailable"
	leaseBlockInactive          leaseBlockReason = "inactive"
	leaseBlockQuarantined       leaseBlockReason = "quarantined"
	leaseBlockNotFound          leaseBlockReason = "not_found"
	leaseBlockGroupMismatch     leaseBlockReason = "group_mismatch"
	leaseBlockProviderMismatch  leaseBlockReason = "provider_mismatch"
	leaseBlockModelUnsupported  leaseBlockReason = "model_unsupported"
	leaseBlockCoordinator       leaseBlockReason = "coordinator_unavailable"

	// kiroBlockLogMaxEntries bounds the diagnostic dedupe table.
	kiroBlockLogMaxEntries = 4096
)

// InvalidateAccountCache clears selection caches and advances structural routing
// state. Call this after an operation that can change group/provider/model
// compatibility (enable/disable, group/provider/model changes, import/delete).
func (s *Scheduler) InvalidateAccountCache() {
	s.invalidateAccountCache(true)
}

// RefreshAccountCache clears selection snapshots after a transient state change
// without invalidating structural route markers. Cooldowns, quota, quarantine and
// egress recovery can change immediate eligibility, but cannot create a new
// group/provider/model-compatible account.
func (s *Scheduler) RefreshAccountCache() {
	s.invalidateAccountCache(false)
}

func (s *Scheduler) invalidateAccountCache(structural bool) {
	s.accountCacheMutex.Lock()
	s.accountCache = nil
	s.selectionCache = nil
	s.rateLimitCache = nil
	s.rateLimitCacheGen = nil
	s.rateLimitSelectionGen = nil
	s.modelCache = nil
	s.auxCacheAt = nil
	s.accountCacheMutex.Unlock()
	s.affinityCache.clear()
	s.clearCandidateIndexes()
	if structural {
		// A sync.Map must not be copied after first use. Clear is safe alongside
		// concurrent Load/Store calls from route selection and concurrent catalog
		// probes, and publishes the same structural invalidation without racing.
		s.providerCache.Clear()
		s.routeStructureVersion.Add(1)
	}
	s.NotifyStateChanged()
}

// InvalidateEgressCache publishes profile/cooldown/health mutations immediately.
// The normal 30-second cache amortizes hot-path profile reads, but a persisted
// breaker trip or operator repair must not keep routing with the previous endpoint
// or health state until that TTL elapses.
func (s *Scheduler) InvalidateEgressCache() {
	s.egressCacheMutex.Lock()
	s.egressCache = sync.Map{}
	s.egressCacheTime = time.Now()
	s.egressCacheMutex.Unlock()
	// accountSelectionSnapshot embeds the stored primary profile for sticky and
	// required-account fast paths. Drop that one-second snapshot as part of the
	// same publication so every route observes the mutation immediately.
	s.accountCacheMutex.Lock()
	s.selectionCache = nil
	s.accountCacheMutex.Unlock()
	s.NotifyStateChanged()
}

// ApplyRateLimitSnapshot publishes an upstream quota result to every live selection
// snapshot before its SQLite persistence completes. This makes an exhausted account
// disappear from new selections immediately instead of remaining eligible for the
// cache TTL or the write queue latency.
func (s *Scheduler) ApplyRateLimitSnapshot(snap storage.AccountRateLimit) {
	s.accountCacheMutex.Lock()
	for group, byAccount := range s.rateLimitCache {
		rows, present := byAccount[snap.AccountID]
		if !present {
			continue
		}
		rows = append([]storage.AccountRateLimit(nil), rows...)
		replaced := false
		for i := range rows {
			if rows[i].Provider == snap.Provider && rows[i].Model == snap.Model && rows[i].LimiterType == snap.LimiterType {
				rows[i] = snap
				replaced = true
				break
			}
		}
		if !replaced {
			rows = append(rows, snap)
		}
		updated := make(map[string][]storage.AccountRateLimit, len(byAccount))
		for accountID, cachedRows := range byAccount {
			updated[accountID] = cachedRows
		}
		updated[snap.AccountID] = rows
		s.rateLimitCache[group] = updated
	}
	s.accountCacheMutex.Unlock()
	s.NotifyStateChanged()
}

type Route struct {
	Group    string
	Provider string // "" (any) / "codex" / "claude" — selects provider-matching accounts
	// AllowedProviders is the provider union eligible for this request. It supersedes
	// Provider when non-empty and lets Claude-family traffic mix official Claude and
	// Kiro accounts without assigning either a fixed priority.
	AllowedProviders []string
	Affinity         routing.AffinityKey
	// FairScheduling bypasses persisted affinity reuse and rendezvous ranking for
	// this selection. The supplied Affinity remains available to the wire adapter
	// as a stable conversation salt, but account/provider choice is made fresh from
	// the current load-aware fair pool and is not persisted as a sticky binding.
	// Auto GPT spillover uses this when Kiro joins Codex as an equal candidate.
	FairScheduling bool
	// AffinityWait overrides the global sticky wait for this route. Kiro uses a
	// longer cache-preserving wait before switching to another exact-model account.
	AffinityWait    time.Duration
	Strict          bool
	ServerSideState bool
	// ImmutableAffinity pins provider, account, resolved model and egress after
	// the first selection. Kiro and Claude/Kiro auto sessions use this mode.
	ImmutableAffinity bool
	// ExplicitProvider records that the downstream explicitly selected Kiro.
	// Concrete-model bootstrap is also available to auto routing; aliases still
	// require a runtime-verified model in both modes.
	ExplicitProvider bool
	// ThinkingRequired is retained for route diagnostics/call-site compatibility.
	// Kiro scheduling always enforces adaptive-thinking support, even when false.
	ThinkingRequired bool
	// RequiredAccountID is an exact, externally persisted session binding owned by
	// the Codex CPA mapper. Unlike ordinary affinity it may never fall through to a
	// fresh account: previous_response_id / turn-state belongs to one upstream
	// session. RequiredEgressID completes that same identity boundary.
	RequiredAccountID string
	RequiredEgressID  string
	// NoEgressFallback requires the selected account's primary egress; an
	// unhealthy primary is terminal and must not resolve standby metadata.
	NoEgressFallback bool
	// FailFastBoundRecovery is set only when the caller owns a durable replay for
	// an exact RequiredAccountID binding. Health/quota cooldowns then return
	// ErrBoundAccountUnavailable immediately so the caller can rotate the durable
	// epoch instead of waiting behind a multi-minute account circuit breaker.
	// Concurrency, token-budget, and coordinator pressure still wait normally: they
	// do not prove that the bound upstream state is unavailable.
	FailFastBoundRecovery bool
	// AllowCodexGoalQuotaGrace lets an observed, active Codex Goal turn probe an
	// account despite local quota snapshots. It never bypasses inactive/quarantined
	// accounts, recheck-pending upstream failures, unhealthy egress, concurrency, or
	// token budgets. A plain cooldown may be ignored only when no recheck is pending,
	// which covers success-header quota telemetry rather than an upstream rejection.
	AllowCodexGoalQuotaGrace bool
	// PreferredEgressIDs is retained for wire/source compatibility with older callers.
	// Runtime inference ignores it: the selected account's persisted primary egress is
	// authoritative, so a group/provider cannot silently move that account to another IP.
	PreferredEgressIDs    []string
	KiroEndpointAllowlist []string
	KiroDefaultRegion     string
	// KiroFallbackModel lets a mixed Codex/Kiro fair route retain the downstream
	// Codex model for Codex candidates while selecting this concrete model for Kiro
	// candidates. It is used when Kiro does not expose the requested Codex model and
	// must serve the turn with gpt-5.6-sol instead.
	KiroFallbackModel string
	// Movable is kept for existing call sites/tests that already computed
	// !ServerSideState. New callers should set ServerSideState directly.
	Movable bool
	Model   string
	// ContextMode is part of routing identity and capability selection. A single
	// "1m" selection requires account-scoped evidence and never falls through on
	// its own; the Messages API may make a second, explicitly standard-window
	// selection for its Claude-Code-managed virtual-1M compaction mode.
	ContextMode     string
	EstimatedTokens int64
	Compaction      bool
	// Exclude lists account IDs that must not be selected for this call — the
	// accounts this request already tried and failed on in the current failover
	// loop. An excluded account is skipped even by the sticky/affinity path, so a
	// just-failed account can never reappear in the same request's retry (the
	// "有问题的账号不应该二次出现在下次的候选名单" requirement). A nil map excludes nothing.
	Exclude map[string]bool
	// OnWait is called at most once per scheduler heartbeat while the request is
	// queued. Streaming HTTP callers use it for a legal SSE comment; non-streaming
	// callers leave it nil and no response bytes are committed.
	OnWait func(reason string, waited time.Duration)
	// SkipWait makes this selection an immediate availability probe. The API layer
	// uses it only while another target explicitly authorized by the same user group
	// remains to be tried; the final target retains the normal cancellation-aware
	// FIFO behavior when every authorized target is saturated.
	SkipWait bool
}

type NoAccountCounters struct {
	Inactive          int `json:"inactive"`
	Quarantined       int `json:"quarantined"`
	Excluded          int `json:"excluded"`
	ProviderMismatch  int `json:"provider_mismatch"`
	ModelUnsupported  int `json:"model_unsupported"`
	RateLimitCooldown int `json:"rate_limit_cooldown"`
	RecheckPending    int `json:"recheck_pending"`
	EgressUnavailable int `json:"egress_unavailable"`
	EgressCooldown    int `json:"egress_cooldown"`
	Concurrency       int `json:"concurrency"`
	TokenBudget       int `json:"token_budget"`
	Coordinator       int `json:"coordinator_unavailable"`
}

type NoAccountError struct {
	Group            string
	Provider         string
	AllowedProviders []string
	Model            string
	Counters         NoAccountCounters
	// EmptyPool marks the group as holding no accounts at all, as opposed to holding
	// accounts that were each skipped for a reason the counters name. The counters cannot
	// express this: an empty group produces no skips, so every counter stays zero and the
	// message collapses to the same bare text a fully saturated pool produces. The two
	// need opposite operator responses — add an account versus wait — so they must not
	// read alike.
	EmptyPool bool
}

func (e *NoAccountError) Error() string {
	if e == nil {
		return ErrNoAccount.Error()
	}
	parts := []string{}
	add := func(name string, n int) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		}
	}
	add("inactive", e.Counters.Inactive)
	add("quarantined", e.Counters.Quarantined)
	add("excluded", e.Counters.Excluded)
	add("provider_mismatch", e.Counters.ProviderMismatch)
	add("model_unsupported", e.Counters.ModelUnsupported)
	add("rate_limit_cooldown", e.Counters.RateLimitCooldown)
	add("recheck_pending", e.Counters.RecheckPending)
	add("egress_unavailable", e.Counters.EgressUnavailable)
	add("egress_cooldown", e.Counters.EgressCooldown)
	add("concurrency", e.Counters.Concurrency)
	add("token_budget", e.Counters.TokenBudget)
	add("coordinator_unavailable", e.Counters.Coordinator)
	providers := strings.Join(e.AllowedProviders, ",")
	if providers == "" {
		providers = e.Provider
	}
	if e.EmptyPool {
		return fmt.Sprintf("%s: group=%q holds no accounts (providers=%q model=%q)",
			ErrNoAccount, e.Group, providers, e.Model)
	}
	if len(parts) == 0 {
		return ErrNoAccount.Error()
	}
	return fmt.Sprintf("%s: group=%q providers=%q model=%q skipped(%s)", ErrNoAccount, e.Group, providers, e.Model, strings.Join(parts, " "))
}

func (e *NoAccountError) Unwrap() error {
	return ErrNoAccount
}

// Retryable reports whether this no-account condition is transient pool saturation —
// matching accounts exist but were momentarily at their concurrency cap, over their
// per-account token budget, or on a short cooldown — rather than a terminal
// misconfiguration. Cancellable inference requests keep these conditions inside the
// scheduler and wait; the error is exposed only to non-request diagnostics.
func (e *NoAccountError) Retryable() bool {
	if e == nil {
		return false
	}
	// An empty group is configuration, not load: waiting cannot make an account appear.
	if e.EmptyPool {
		return false
	}
	c := e.Counters
	return c.Concurrency+c.TokenBudget+c.RateLimitCooldown+c.RecheckPending+c.EgressCooldown+c.Coordinator > 0
}

// transientlySaturated reports whether a scheduler selection error is retryable
// transient load pressure (see NoAccountError.Retryable).
func transientlySaturated(err error) bool {
	var nae *NoAccountError
	if errors.As(err, &nae) {
		return nae.Retryable()
	}
	return false
}

type Lease struct {
	Account       storage.Account
	Binding       storage.AccountEgressBinding
	Egress        storage.EgressProfile
	ResolvedModel string
	RouteEpoch    int64
	FencingToken  uint64
	// Trial marks an intelligent-routing cooldown trial lease: the account's
	// cooldown state said "blocked" but no other candidate existed, so the
	// scheduler tried the soonest-recovering account anyway. A successful
	// upstream response therefore proves the stale cooldown wrong and the API
	// layer clears it so the account rejoins the pool immediately.
	Trial          bool
	release        func()
	dynamicRelease func()
}

func New(store *storage.Store, cfg config.Config) *Scheduler {
	return NewWithLeaseCoordinator(store, cfg, newLocalLeaseCoordinator())
}

func NewWithLeaseCoordinator(store *storage.Store, cfg config.Config, coordinator LeaseCoordinator) *Scheduler {
	if coordinator == nil {
		coordinator = newLocalLeaseCoordinator()
	}
	s := &Scheduler{
		store:            store,
		inflight:         map[string]int{},
		inflightTokens:   map[string]int64{},
		egressInflight:   map[string]int{},
		loads:            newShardedLoadCounters(),
		loadChanged:      make(chan struct{}),
		waitQueues:       map[string]*waitQueue{},
		selectionCache:   map[string]*accountSelectionSnapshot{},
		affinityCache:    newAffinityCacheStore(),
		egressCacheTTL:   30 * time.Second,
		providerCacheTTL: 5 * time.Minute,
		admission:        admission.New(cfg.ResourceHeadroomPercent),
		coordinator:      coordinator,
		coordinatorStop:  make(chan struct{}),
		dynamicBalance:   newDynamicPoolBalanceRuntime(),
	}
	for i := range s.egressEWMA {
		s.egressEWMA[i].rows = map[string]egressEWMA{}
	}
	s.cfg.Store(cfg)
	// Apply the initial CPU admission valve state immediately. UpdateConfig
	// handles subsequent hot reloads, but admission.New defaults to enabled.
	s.admission.SetCPUEnabled(cfg.AdmissionCPUEnabled)
	s.routeStructureVersion.Store(1)
	if notifications := coordinator.Notifications(); notifications != nil {
		s.coordinatorWG.Add(1)
		go func() {
			defer supervisor.Recover("scheduler-lease-notifications")
			defer s.coordinatorWG.Done()
			for {
				select {
				case <-s.coordinatorStop:
					return
				case _, ok := <-notifications:
					if !ok {
						return
					}
					s.NotifyStateChanged()
				}
			}
		}()
	}
	return s
}

// Config returns the scheduler's current runtime configuration snapshot.
func (s *Scheduler) Config() config.Config {
	if v := s.cfg.Load(); v != nil {
		return v.(config.Config)
	}
	return config.Default()
}

// UpdateConfig hot-swaps scheduler knobs used on the selection path.
func (s *Scheduler) UpdateConfig(cfg config.Config) {
	s.cfg.Store(cfg)
	s.admission.SetHeadroom(cfg.ResourceHeadroomPercent)
	s.admission.SetCPUEnabled(cfg.AdmissionCPUEnabled)
	s.clearCandidateIndexes()
	s.routeStructureVersion.Add(1)
	s.NotifyStateChanged()
}

// NotifyStateChanged wakes queued selections after any account, quota, cooldown or
// egress mutation. Polling remains as a safety net for callers outside the API layer.
func (s *Scheduler) NotifyStateChanged() {
	s.mu.Lock()
	s.notifyLoadChangedLocked()
	s.mu.Unlock()
}

// RouteStructureVersion is a monotonic process-local group/provider/model
// compatibility generation. It is intentionally cheap enough for every
// user-group request.
func (s *Scheduler) RouteStructureVersion() uint64 {
	if s == nil {
		return 0
	}
	return s.routeStructureVersion.Load()
}

func (s *Scheduler) Metrics() SchedulerMetrics {
	active := int64(0)
	if s.loads != nil {
		active = s.loads.active()
	}
	routeSelects := atomic.LoadInt64(&s.metrics.RouteSelects)
	routeNanos := atomic.LoadInt64(&s.metrics.RouteNanos)
	routeAverage := int64(0)
	if routeSelects > 0 {
		routeAverage = routeNanos / routeSelects
	}
	var ewmaOutlets, ewmaSamples int64
	for i := range s.egressEWMA {
		shard := &s.egressEWMA[i]
		shard.mu.RLock()
		ewmaOutlets += int64(len(shard.rows))
		for _, row := range shard.rows {
			ewmaSamples += int64(row.Samples)
		}
		shard.mu.RUnlock()
	}
	dynamicSelections := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceSelections)
	dynamicTargetSelections := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceTargetSelections)
	dynamicSkipped := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceSkipped)
	dynamicUnknown := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceSkippedUnknownInterval)
	dynamicSnapshot := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceSkippedSnapshot)
	dynamicSnapshotErrors := atomic.LoadInt64(&s.metrics.DynamicPoolBalanceSnapshotErrors)
	dynamicReservations := int64(0)
	if s.dynamicBalance != nil {
		dynamicReservations = int64(s.dynamicBalance.reservationCount(storage.Now()))
		dynamicSnapshotErrors += s.dynamicBalance.snapshotErrorCount()
	}
	return SchedulerMetrics{
		Active: active,
		Queued: atomic.LoadInt64(&s.metrics.Queued), Waited: atomic.LoadInt64(&s.metrics.Waited),
		Cancelled: atomic.LoadInt64(&s.metrics.Cancelled), AccountSwitches: atomic.LoadInt64(&s.metrics.AccountSwitches),
		WaitConcurrency: atomic.LoadInt64(&s.metrics.WaitConcurrency), WaitTokenBudget: atomic.LoadInt64(&s.metrics.WaitTokenBudget),
		WaitCooldown: atomic.LoadInt64(&s.metrics.WaitCooldown), WaitRecheck: atomic.LoadInt64(&s.metrics.WaitRecheck),
		WaitEgress: atomic.LoadInt64(&s.metrics.WaitEgress), CooldownWakeups: atomic.LoadInt64(&s.metrics.CooldownWakeups),
		StateWakeups: atomic.LoadInt64(&s.metrics.StateWakeups), WaitNanos: atomic.LoadInt64(&s.metrics.WaitNanos),
		RouteSelects: routeSelects, RouteNanos: routeNanos, RouteAvgNanos: routeAverage, RouteMaxNanos: atomic.LoadInt64(&s.metrics.RouteMaxNanos),
		CandidateEvaluations: atomic.LoadInt64(&s.metrics.CandidateEvaluations), CandidateFallbacks: atomic.LoadInt64(&s.metrics.CandidateFallbacks), CandidateIndexBuilds: atomic.LoadInt64(&s.metrics.CandidateIndexBuilds),
		CandidateIndexEntries: s.candidateIndexCount.Load(),
		TrialSelections:       atomic.LoadInt64(&s.metrics.TrialSelections),
		EgressEWMAOutlets:     ewmaOutlets, EgressEWMASamples: ewmaSamples,
		DynamicPoolBalanceSelections: dynamicSelections, DynamicPoolBalanceTargetSelections: dynamicTargetSelections,
		DynamicPoolBalanceSkipped: dynamicSkipped, DynamicPoolBalanceSkippedUnknownInterval: dynamicUnknown,
		DynamicPoolBalanceSkippedSnapshot: dynamicSnapshot, DynamicPoolBalanceSnapshotErrors: dynamicSnapshotErrors,
		DynamicPoolBalanceReservationCount: dynamicReservations,
	}
}

// ObserveEgress updates a bounded, process-local EWMA from the first response
// byte (or a pre-header failure). A conservative 90% prior prevents one sample
// from monopolizing or benching an otherwise healthy outlet.
func (s *Scheduler) ObserveEgress(egressID string, latency time.Duration, success bool) {
	egressID = strings.TrimSpace(egressID)
	if s == nil || egressID == "" {
		return
	}
	if latency < time.Millisecond {
		latency = time.Millisecond
	}
	if latency > 2*time.Minute {
		latency = 2 * time.Minute
	}
	shard := &s.egressEWMA[schedulerLoadHash(egressID)&(schedulerLoadShardCount-1)]
	shard.mu.Lock()
	if shard.rows == nil {
		shard.rows = map[string]egressEWMA{}
	}
	if _, exists := shard.rows[egressID]; !exists && len(shard.rows) >= schedulerEWMAEntriesPerShard {
		var oldestID string
		var oldestAt time.Time
		for candidateID, candidate := range shard.rows {
			if oldestID == "" || candidate.UpdatedAt.Before(oldestAt) {
				oldestID, oldestAt = candidateID, candidate.UpdatedAt
			}
		}
		delete(shard.rows, oldestID)
	}
	row := shard.rows[egressID]
	const alpha = 0.20
	value := 0.0
	if success {
		value = 1
	}
	if row.Samples == 0 {
		row.Success = 0.9*(1-alpha) + value*alpha
		row.LatencyMillis = float64(latency.Milliseconds())
	} else {
		row.Success = row.Success*(1-alpha) + value*alpha
		row.LatencyMillis = row.LatencyMillis*(1-alpha) + float64(latency.Milliseconds())*alpha
	}
	row.Samples++
	row.UpdatedAt = time.Now()
	shard.rows[egressID] = row
	shard.mu.Unlock()
}

func (s *Scheduler) egressEWMAQuality(egressID string) (successBucket int, latencyMillis int) {
	shard := &s.egressEWMA[schedulerLoadHash(egressID)&(schedulerLoadShardCount-1)]
	shard.mu.RLock()
	row, ok := shard.rows[egressID]
	shard.mu.RUnlock()
	if !ok || row.Samples == 0 || time.Since(row.UpdatedAt) > 30*time.Minute {
		return 100, 0
	}
	successBucket = int(row.Success*100 + 0.5)
	if successBucket < 20 {
		successBucket = 20
	}
	return successBucket, int(row.LatencyMillis + 0.5)
}

func (s *Scheduler) Select(ctx context.Context, route Route) (Lease, error) {
	if policy, ok := dynamicPoolBalancePolicyFromContext(ctx); ok && policy.Enabled && policy.EgressRPMBalanceEnabled && policy.Fresh && !policy.Bound && policy.AgentClass == storage.AgentClassRoot && route.RequiredEgressID == "" && !route.ImmutableAffinity && len(route.PreferredEgressIDs) == 0 {
		route.PreferredEgressIDs = append([]string(nil), policy.EgressRPMBalanceEgressIDs...)
	}
	if lease, handled, err := s.selectFromRouteChoiceContext(ctx, route); handled {
		return lease, err
	}
	// A no-egress-fallback route is also an immutable affinity route: once an
	// account/primary outlet has been selected, ordinary affinity recovery must
	// not rebind it to another account or resolve a standby outlet.
	if route.NoEgressFallback {
		route.ImmutableAffinity = true
	}
	started := time.Now()
	defer s.observeRouteSelection(started)
	if err := s.admission.Wait(ctx); err != nil {
		return Lease{}, err
	}
	cfg := s.Config()
	hadExclusions := len(route.Exclude) > 0
	hadBinding := false
	if route.Group == "" {
		route.Group = cfg.DefaultGroup
	}
	groupEgressID, groupErr := s.groupPrimaryEgressID(ctx, route.Group)
	if groupErr != nil {
		return Lease{}, groupErr
	}
	// A user-group RPM policy supplies an ordered set of real outlets for fresh
	// work. Stateful/required routes are left on the persisted group primary; the
	// candidate-level helper also re-checks the account-scoped pin boundary.
	if policy, ok := dynamicPoolBalancePolicyFromContext(ctx); ok && policy.Enabled &&
		policy.EgressRPMBalanceEnabled && policy.EgressRPMBalanceThreshold > 0 &&
		policy.Fresh && !policy.Bound && policy.OnlyAccountPoolTier &&
		storage.NormalizeAgentClass(policy.AgentClass) == storage.AgentClassRoot &&
		route.RequiredAccountID == "" && route.RequiredEgressID == "" &&
		!route.ServerSideState && !route.ImmutableAffinity && !route.FairScheduling {
		route.PreferredEgressIDs = normalizeEgressIDs(policy.EgressRPMBalanceEgressIDs)
	} else {
		// Ordinary callers cannot override the account-pool group's effective
		// primary outlet through PreferredEgressIDs; that field is legacy wire
		// compatibility and remains ignored outside the explicit RPM policy.
		route.PreferredEgressIDs = []string{groupEgressID}
	}
	if strings.TrimSpace(route.RequiredAccountID) != "" {
		if route.Exclude[route.RequiredAccountID] {
			return Lease{}, fmt.Errorf("%w: account=%s", ErrBoundAccountUnavailable, route.RequiredAccountID)
		}
		lease, reason, ok := s.tryLeaseAccountDetailed(ctx, route.RequiredAccountID, route, nil)
		if ok {
			return s.leaseWithAffinityEpoch(ctx, route, lease), nil
		}
		if route.ServerSideState && statefulStickyWaitReason(reason) &&
			!(route.FailFastBoundRecovery && boundHealthRecoveryReason(reason)) {
			if lease, err := s.waitForStatefulStickyLease(ctx, route.RequiredAccountID, route, reason); err == nil {
				return s.leaseWithAffinityEpoch(ctx, route, lease), nil
			}
		}
		return Lease{}, fmt.Errorf("%w: account=%s egress=%s reason=%s", ErrBoundAccountUnavailable, route.RequiredAccountID, route.RequiredEgressID, reason.humanString())
	}
	reboundFrom := ""
	if route.Affinity.Hash != "" && !route.FairScheduling {
		if bound, err := s.affinitySnapshot(ctx, route.Affinity.Hash); err == nil {
			hadBinding = true
			if route.ImmutableAffinity {
				boundProvider := firstRouteValue(bound.Provider, s.providerOf(ctx, bound.AccountID))
				if !routeAllowsProvider(route, boundProvider) || route.Exclude[bound.AccountID] {
					return Lease{}, fmt.Errorf("%w: provider=%s account=%s", ErrBoundAccountUnavailable, boundProvider, bound.AccountID)
				}
				boundRoute := route
				boundRoute.Provider = boundProvider
				boundRoute.AllowedProviders = []string{boundProvider}
				boundRoute.RequiredEgressID = bound.EgressID
				if bound.Model != "" {
					boundRoute.Model = bound.Model
				}
				lease, reason, ok := s.tryLeaseAccountDetailed(ctx, bound.AccountID, boundRoute, nil)
				if ok {
					lease.RouteEpoch = bound.Epoch
					if bound.Model != "" {
						lease.ResolvedModel = bound.Model
					}
					if bound.Provider == "" || bound.Model == "" || bound.EgressID == "" {
						if stored, upsertErr := s.upsertAffinityResult(ctx, storage.AffinityBinding{
							RouteKeyHash: bound.RouteKeyHash, RouteKey: bound.RouteKey, Source: bound.Source,
							AccountID: bound.AccountID, Provider: boundProvider,
							Model: firstRouteValue(lease.ResolvedModel, boundRoute.Model), EgressID: lease.Egress.ID,
						}); upsertErr == nil {
							lease.RouteEpoch = stored.Epoch
						}
					}
					return lease, nil
				}
				if statefulStickyWaitReason(reason) {
					lease, waitErr := s.waitForStatefulStickyLease(ctx, bound.AccountID, boundRoute, reason)
					lease.RouteEpoch = bound.Epoch
					if waitErr == nil && bound.Model != "" {
						lease.ResolvedModel = bound.Model
					}
					if waitErr == nil {
						return lease, nil
					}
				}
				log.Printf("[SCHEDULER] immutable affinity unavailable account=%s reason=%s group=%s model=%s egress=%s", bound.AccountID, reason, boundRoute.Group, boundRoute.Model, boundRoute.RequiredEgressID)
				return Lease{}, fmt.Errorf("%w: provider=%s account=%s model=%s egress=%s reason=%s", ErrBoundAccountUnavailable, boundProvider, bound.AccountID, bound.Model, bound.EgressID, reason.humanString())
			}
			// Skip the sticky account entirely when this request already tried and
			// failed on it (Exclude) — fall through to selectFresh, which rebinds the
			// affinity to the fresh account so the conversation seamlessly continues
			// on a healthy one instead of bouncing back to the broken account.
			if routeAllowsProvider(route, s.providerOf(ctx, bound.AccountID)) && !route.Exclude[bound.AccountID] {
				lease, reason, ok := s.tryLeaseAccountDetailed(ctx, bound.AccountID, route, nil)
				if ok {
					lease.RouteEpoch = bound.Epoch
					resolvedModel := firstRouteValue(lease.ResolvedModel, route.Model)
					// A normal affinity binds the conversation, not one forever-fixed
					// model. If the same capable account serves a later /model choice,
					// publish that exact model so subsequent immutable decisions never
					// resurrect the previous model.
					if bound.Provider == "" || bound.Model == "" || bound.EgressID == "" || !strings.EqualFold(strings.TrimSpace(bound.Model), strings.TrimSpace(resolvedModel)) {
						if stored, upsertErr := s.upsertAffinityResult(ctx, storage.AffinityBinding{
							RouteKeyHash: bound.RouteKeyHash, RouteKey: bound.RouteKey, Source: bound.Source,
							AccountID: bound.AccountID, Provider: s.providerOfAccount(ctx, lease.Account),
							Model: resolvedModel, EgressID: lease.Egress.ID,
						}); upsertErr == nil {
							lease.RouteEpoch = stored.Epoch
						}
					}
					return lease, nil
				}
				if route.Strict && route.ServerSideState {
					if statefulStickyWaitReason(reason) {
						lease, waitErr := s.waitForStatefulStickyLease(ctx, bound.AccountID, route, reason)
						lease.RouteEpoch = bound.Epoch
						return lease, waitErr
					}
					return Lease{}, s.diagnoseStickyUnavailability(ctx, bound.AccountID, route)
				}
				stickyWait := cfg.StickyWait()
				if route.AffinityWait > 0 {
					stickyWait = route.AffinityWait
				}
				if waitFor(ctx, stickyWait) {
					lease, reason, ok = s.tryLeaseAccountDetailed(ctx, bound.AccountID, route, nil)
					if ok {
						lease.RouteEpoch = bound.Epoch
						return lease, nil
					}
				}
				if route.Strict {
					if !s.strictStickyCanFailover(ctx, bound.AccountID, route) {
						// Within-threshold cooldown (or threshold=0, never rebind for a
						// long cooldown): wait the pinned account out instead of
						// rebinding. A rebind moves the conversation to a fresh account
						// with an empty upstream prompt-cache prefix — the next turn
						// pays the full cold-cache price. waitForStatefulStickyLease
						// re-diagnoses if the block turns non-waitable mid-wait.
						if reason, ok := s.strictStickyCooldownWait(ctx, bound.AccountID, route); ok {
							// waitForStatefulStickyLease already returns a terminal
							// diagnosis (wait timeout with the last reason, or the
							// bound account turning non-waitable mid-wait). Return it
							// as-is: re-running the diagnosis here sees a cancelled
							// ctx and reports "account not found", masking the
							// cooldown that actually pinned us.
							lease, waitErr := s.waitForStatefulStickyLease(ctx, bound.AccountID, route, reason)
							if waitErr == nil {
								lease.RouteEpoch = bound.Epoch
								return lease, nil
							}
							return Lease{}, waitErr
						}
						return Lease{}, s.diagnoseStickyUnavailability(ctx, bound.AccountID, route)
					}
					log.Printf("[SCHEDULER] strict-sticky falling through to selectFresh for route affinity=%s", route.Affinity.Hash)
				} else if cfg.StrictStickyMaxCooldownSeconds > 0 {
					// Non-strict movable request: a short cooldown on the bound
					// account is still worth riding out — the upstream session and
					// its prompt-cache prefix live on that account, and the request
					// carries its full input, so waiting is lossless. The wait is
					// bounded by the cooldown threshold; beyond it we fall through to
					// selectFresh exactly as before (movable failover semantics).
					if reason, ok := s.strictStickyCooldownWait(ctx, bound.AccountID, route); ok {
						waitCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.StrictStickyMaxCooldownSeconds)*time.Second)
						lease, waitErr := s.waitForStatefulStickyLease(waitCtx, bound.AccountID, route, reason)
						cancel()
						if waitErr == nil {
							lease.RouteEpoch = bound.Epoch
							return lease, nil
						}
						log.Printf("[SCHEDULER] affinity cooldown wait expired, rebinding affinity=%s account=%s", route.Affinity.Hash, bound.AccountID)
					}
				}
			}
			// Any fall-through from this affinity branch rebinds the conversation
			// away from its bound account (or drops the binding when the bound
			// account is excluded). Remember the old owner so a successful fresh
			// selection below can audit the switch — cross-account conversation
			// moves are the #1 prompt-cache killer, so they must be visible, not
			// silent.
			reboundFrom = bound.AccountID
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Lease{}, err
		}
	}

	var lease Lease
	var err error
	// Intelligent routing: every request attempts a fresh selection first so a
	// free account is picked instantly ("秒选择") instead of joining a wait queue
	// that may not need it. Only a request that actually finds the route
	// transiently saturated (concurrency/token/cooldown) joins the FIFO queue
	// below; queue discipline therefore applies exactly where waiting can help,
	// and a recovered pool is served immediately. Affinity selections above
	// retain priority because they preserve an existing conversation's binding.
	lease, err = s.selectFreshIndexed(ctx, route)
	if err != nil {
		var stale *NoAccountError
		if (errors.As(err, &stale) && (stale.Counters.RecheckPending > 0 || stale.Counters.RateLimitCooldown > 0 || stale.Counters.EgressCooldown > 0)) || (errors.Is(err, ErrNoAccount) && len(route.Exclude) > 0) {
			s.RefreshAccountCache()
			lease, err = s.selectFreshIndexed(ctx, route)
		}
	}
	// Exclusions belong to the caller's current failover round. If every compatible
	// account has already failed, return the no-account result so the API layer can
	// advance to the next routing target; never replay a failed account in-place.
	if err != nil && transientlySaturated(err) && ctx.Done() != nil && !route.SkipWait {
		lease, err = s.waitForFreshLease(ctx, route, err)
	}
	if err != nil {
		return Lease{}, err
	}
	if hadExclusions {
		atomic.AddInt64(&s.metrics.AccountSwitches, 1)
	}
	if route.Affinity.Hash != "" && !route.FairScheduling {
		binding := storage.AffinityBinding{
			RouteKeyHash: route.Affinity.Hash,
			RouteKey:     route.Affinity.Key,
			Source:       route.Affinity.Source,
			AccountID:    lease.Account.ID,
			Provider:     s.providerOfAccount(ctx, lease.Account),
			Model:        firstRouteValue(lease.ResolvedModel, route.Model),
			EgressID:     lease.Egress.ID,
		}
		if !hadBinding || !route.ImmutableAffinity {
			if stored, upsertErr := s.upsertAffinityResult(ctx, binding); upsertErr == nil {
				lease.RouteEpoch = stored.Epoch
			}
		}
	}
	if reboundFrom != "" && lease.Account.ID != reboundFrom {
		s.auditAffinityRebind(ctx, reboundFrom, lease.Account.ID, route)
	}
	return lease, nil
}

// auditAffinityRebind records a conversation whose affinity binding moved from one
// account to another, with the route identity that caused the rebind. Cross-account
// moves reset the upstream prompt-cache prefix, so every one should be explainable
// (cooldown past threshold, egress health, exclusion, fair-scheduling weight) rather
// than invisible. Written with a detached context so a cancelled request still leaves
// the trail; failures only log, the rebind itself is already committed.
func (s *Scheduler) auditAffinityRebind(ctx context.Context, from, to string, route Route) {
	detail := fmt.Sprintf("from=%s to=%s affinity=%s route_key=%s group=%s model=%s",
		from, to, route.Affinity.Hash, route.Affinity.Key, route.Group, route.Model)
	if len(detail) > 4000 {
		detail = detail[:4000]
	}
	if err := s.store.InsertAuditLog(context.WithoutCancel(ctx), storage.AuditLogRow{
		AccountID: to,
		Action:    "affinity_rebind",
		State:     "recovered",
		Reason:    "sticky_unavailable",
		Detail:    detail,
	}); err != nil {
		log.Printf("[SCHEDULER] affinity rebind audit failed from=%s to=%s: %v", from, to, err)
	}
}

func (s *Scheduler) observeRouteSelection(started time.Time) {
	elapsed := time.Since(started).Nanoseconds()
	atomic.AddInt64(&s.metrics.RouteSelects, 1)
	atomic.AddInt64(&s.metrics.RouteNanos, elapsed)
	for {
		current := atomic.LoadInt64(&s.metrics.RouteMaxNanos)
		if elapsed <= current || atomic.CompareAndSwapInt64(&s.metrics.RouteMaxNanos, current, elapsed) {
			return
		}
	}
}

func (s *Scheduler) AdmissionSnapshot() admission.Snapshot { return s.admission.Snapshot() }
func (s *Scheduler) Close() {
	if s.admission != nil {
		s.admission.Close()
	}
	if s.dynamicBalance != nil {
		s.dynamicBalance.stop()
	}
	if s.coordinatorStop != nil {
		select {
		case <-s.coordinatorStop:
		default:
			close(s.coordinatorStop)
		}
		s.coordinatorWG.Wait()
	}
	if s.coordinator != nil {
		_ = s.coordinator.Close()
	}
}
func (s *Scheduler) Reserve(ctx context.Context, bytes int64) (func(), error) {
	return s.admission.Reserve(ctx, bytes)
}

func firstRouteValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func routeAllowsProvider(route Route, provider string) bool {
	provider = strings.TrimSpace(provider)
	if len(route.AllowedProviders) == 0 {
		return strings.TrimSpace(route.Provider) == "" || strings.EqualFold(strings.TrimSpace(route.Provider), provider)
	}
	for _, allowed := range route.AllowedProviders {
		if strings.EqualFold(strings.TrimSpace(allowed), provider) {
			return true
		}
	}
	return false
}

// routeForProviderModel applies a provider-specific fallback without changing the
// route's downstream model. Keeping the original Route.Model intact is important:
// a Codex candidate must still be capability-checked and sent the exact model the
// downstream requested, while a Kiro candidate can use its documented fallback.
func routeForProviderModel(route Route, provider string) Route {
	if strings.EqualFold(strings.TrimSpace(provider), "kiro") && strings.TrimSpace(route.KiroFallbackModel) != "" {
		route.Model = strings.TrimSpace(route.KiroFallbackModel)
	}
	return route
}

func routeProvidersKey(route Route) string {
	providers := append([]string(nil), route.AllowedProviders...)
	if len(providers) == 0 && strings.TrimSpace(route.Provider) != "" {
		providers = []string{strings.TrimSpace(route.Provider)}
	}
	for i := range providers {
		providers[i] = strings.ToLower(strings.TrimSpace(providers[i]))
	}
	sort.Strings(providers)
	return strings.Join(providers, ",")
}

func schedulerQueueKey(route Route) string {
	return route.Group + "\x00" + route.Model + "\x00" + strings.TrimSpace(route.KiroFallbackModel) + "\x00" + strings.ToLower(strings.TrimSpace(route.ContextMode)) + "\x00" + routeProvidersKey(route)
}

func (s *Scheduler) enqueue(route Route, reason string) (string, *waiter) {
	w := &waiter{started: time.Now(), queued: true}
	key := schedulerQueueKey(route)
	s.queueMu.Lock()
	q := s.waitQueues[key]
	if q == nil {
		q = &waitQueue{}
		s.waitQueues[key] = q
	}
	w.queue = q
	w.prev = q.tail
	if q.tail != nil {
		q.tail.next = w
	} else {
		q.head = w
	}
	q.tail = w
	q.len++
	s.queueMu.Unlock()
	atomic.AddInt64(&s.metrics.Queued, 1)
	atomic.AddInt64(&s.metrics.Waited, 1)
	s.recordWaitReason(reason)
	return key, w
}

func (s *Scheduler) recordWaitReason(reason string) {
	switch reason {
	case "concurrency":
		atomic.AddInt64(&s.metrics.WaitConcurrency, 1)
	case "token_budget":
		atomic.AddInt64(&s.metrics.WaitTokenBudget, 1)
	case "cooldown":
		atomic.AddInt64(&s.metrics.WaitCooldown, 1)
	case "recheck":
		atomic.AddInt64(&s.metrics.WaitRecheck, 1)
	case "egress_cooldown":
		atomic.AddInt64(&s.metrics.WaitEgress, 1)
	}
}

func (s *Scheduler) removeWaiter(key string, target *waiter) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q := target.queue
	if q == nil || !target.queued || s.waitQueues[key] != q {
		return false
	}
	wasHead := q.head == target
	if target.prev != nil {
		target.prev.next = target.next
	} else {
		q.head = target.next
	}
	if target.next != nil {
		target.next.prev = target.prev
	} else {
		q.tail = target.prev
	}
	q.len--
	target.prev, target.next, target.queue, target.queued = nil, nil, nil, false
	if q.len == 0 {
		delete(s.waitQueues, key)
	}
	atomic.AddInt64(&s.metrics.Queued, -1)
	return wasHead
}

func (s *Scheduler) waiterIsHead(key string, target *waiter) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q := s.waitQueues[key]
	return q != nil && q.head == target
}

func (s *Scheduler) routeHasWaiters(route Route) bool {
	key := schedulerQueueKey(route)
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q := s.waitQueues[key]
	return q != nil && q.len > 0
}

// A single process-wide broadcast clock replaces two tickers per queued request.
// Closing the generation channel wakes every subscriber without making timer and
// goroutine counts grow with queue length.
type schedulerClock struct {
	once sync.Once
	mu   sync.Mutex
	ch   chan struct{}
}

var sharedSchedulerClock schedulerClock

const schedulerRecoveryPollInterval = 250 * time.Millisecond
const schedulerRecoveryPollJitter = 50 * time.Millisecond

var schedulerRecoveryJitterState atomic.Uint64

func schedulerRecoveryPollDelay() time.Duration {
	state := schedulerRecoveryJitterState.Add(0x9e3779b97f4a7c15)
	state ^= state >> 30
	state *= 0xbf58476d1ce4e5b9
	state ^= state >> 27
	state *= 0x94d049bb133111eb
	state ^= state >> 31
	span := uint64(2*schedulerRecoveryPollJitter + time.Nanosecond)
	return schedulerRecoveryPollInterval - schedulerRecoveryPollJitter + time.Duration(state%span)
}

func (c *schedulerClock) subscribe() <-chan struct{} {
	c.once.Do(func() {
		c.ch = make(chan struct{})
		go func() {
			defer supervisor.Recover("scheduler-shared-wait-clock")
			timer := time.NewTimer(schedulerRecoveryPollDelay())
			defer timer.Stop()
			for {
				<-timer.C
				c.mu.Lock()
				close(c.ch)
				c.ch = make(chan struct{})
				c.mu.Unlock()
				timer.Reset(schedulerRecoveryPollDelay())
			}
		}()
	})
	c.mu.Lock()
	ch := c.ch
	c.mu.Unlock()
	return ch
}

// waitForFreshLease is the central unbounded admission queue. There is deliberately
// no queue length or scheduler timeout: only downstream cancellation removes a waiter.
func (s *Scheduler) waitForFreshLease(ctx context.Context, route Route, lastErr error) (Lease, error) {
	key, w := s.enqueue(route, waitReason(lastErr))
	removed := false
	defer func() {
		if !removed {
			if s.removeWaiter(key, w) {
				s.NotifyStateChanged()
			}
		}
		atomic.AddInt64(&s.metrics.WaitNanos, time.Since(w.started).Nanoseconds())
	}()
	heartbeatEvery := s.Config().SchedulerHeartbeat()
	lastHeartbeat := time.Now()
	lastPoll := time.Time{}
	for {
		now := time.Now()
		if s.waiterIsHead(key, w) && (lastPoll.IsZero() || now.Sub(lastPoll) >= schedulerRecoveryPollInterval) {
			lastPoll = now
			if err := s.admission.Wait(ctx); err != nil {
				return Lease{}, err
			}
			lease, err := s.selectFreshIndexed(ctx, route)
			if err == nil {
				s.removeWaiter(key, w)
				removed = true
				s.NotifyStateChanged()
				return lease, nil
			}
			lastErr = err
			if !transientlySaturated(err) {
				return Lease{}, err
			}
		}
		changed := s.loadChangedChan()
		clock := sharedSchedulerClock.subscribe()
		select {
		case <-ctx.Done():
			atomic.AddInt64(&s.metrics.Cancelled, 1)
			return Lease{}, ctx.Err()
		case <-changed:
			atomic.AddInt64(&s.metrics.StateWakeups, 1)
			lastPoll = time.Time{}
		case <-clock:
			now = time.Now()
			atomic.AddInt64(&s.metrics.CooldownWakeups, 1)
			lastPoll = time.Time{}
			if route.OnWait != nil && now.Sub(lastHeartbeat) >= heartbeatEvery {
				route.OnWait(waitReason(lastErr), now.Sub(w.started))
				lastHeartbeat = now
			}
		}
	}
}

func waitReason(err error) string {
	var nae *NoAccountError
	if !errors.As(err, &nae) {
		return "scheduler_wait"
	}
	c := nae.Counters
	switch {
	case c.Concurrency > 0:
		return "concurrency"
	case c.TokenBudget > 0:
		return "token_budget"
	case c.RateLimitCooldown > 0:
		return "cooldown"
	case c.RecheckPending > 0:
		return "recheck"
	case c.EgressCooldown > 0:
		return "egress_cooldown"
	default:
		return "scheduler_wait"
	}
}

func (l Lease) Release() {
	if l.release != nil {
		l.release()
	}
	if l.dynamicRelease != nil {
		l.dynamicRelease()
	}
}

func candidateLess(a, b candidate) bool {
	if a.bootstrap != b.bootstrap {
		return !a.bootstrap
	}
	return a.score < b.score
}

func insertRendezvousTop3(top []rankedCandidate, item rankedCandidate) []rankedCandidate {
	if len(top) < 3 {
		top = append(top, item)
	} else if item.rank <= top[len(top)-1].rank {
		return top
	} else {
		top[len(top)-1] = item
	}
	for i := len(top) - 1; i > 0 && top[i].rank > top[i-1].rank; i-- {
		top[i], top[i-1] = top[i-1], top[i]
	}
	return top
}

func addPowerTwoCandidate(sample []candidate, item candidate, seen uint64, rng *uint64) []candidate {
	if len(sample) < 2 {
		return append(sample, item)
	}
	*rng = xorshift64(*rng)
	if slot := *rng % seen; slot < 2 {
		sample[slot] = item
	}
	return sample
}

func xorshift64(x uint64) uint64 {
	if x == 0 {
		x = 0x9e3779b97f4a7c15
	}
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	return x
}

func (s *Scheduler) accountsSnapshot(ctx context.Context, group string) (*accountSelectionSnapshot, error) {
	s.accountCacheMutex.RLock()
	snapshot, ok := s.selectionCache[group]
	s.accountCacheMutex.RUnlock()
	if ok && time.Since(snapshot.at) < time.Second {
		return snapshot, nil
	}
	s.accountRefreshMu.Lock()
	defer s.accountRefreshMu.Unlock()
	s.accountCacheMutex.RLock()
	snapshot, ok = s.selectionCache[group]
	s.accountCacheMutex.RUnlock()
	if ok && time.Since(snapshot.at) < time.Second {
		return snapshot, nil
	}
	rows, err := s.store.ListActiveAccountsWithEgress(ctx, group)
	if err != nil {
		return nil, err
	}
	snapshot = &accountSelectionSnapshot{
		group: group, rows: append([]storage.AccountWithEgress(nil), rows...),
		byID: make(map[string]storage.AccountWithEgress, len(rows)), accountIDs: make([]string, 0, len(rows)),
		generation: s.selectionVersion.Add(1), at: time.Now(),
	}
	for _, row := range snapshot.rows {
		snapshot.byID[row.Account.ID] = row
		snapshot.accountIDs = append(snapshot.accountIDs, row.Account.ID)
	}
	s.accountCacheMutex.Lock()
	if s.selectionCache == nil {
		s.selectionCache = map[string]*accountSelectionSnapshot{}
	}
	s.selectionCache[group] = snapshot
	s.accountCacheMutex.Unlock()
	return snapshot, nil
}

func (s *Scheduler) rateLimitsSnapshot(ctx context.Context, group string, accountIDs []string) (map[string][]storage.AccountRateLimit, error) {
	key := "rate:" + group
	generation := s.store.RateLimitGeneration()
	s.accountCacheMutex.RLock()
	rows, ok := s.rateLimitCache[group]
	at := s.auxCacheAt[key]
	cachedGeneration := s.rateLimitCacheGen[group]
	s.accountCacheMutex.RUnlock()
	complete := ok && cachedGeneration == generation
	for _, id := range accountIDs {
		if _, present := rows[id]; !present {
			complete = false
			break
		}
	}
	if complete && time.Since(at) < time.Second {
		return rows, nil
	}
	s.rateRefreshMu.Lock()
	defer s.rateRefreshMu.Unlock()
	s.accountCacheMutex.RLock()
	rows, ok = s.rateLimitCache[group]
	at = s.auxCacheAt[key]
	cachedGeneration = s.rateLimitCacheGen[group]
	s.accountCacheMutex.RUnlock()
	complete = ok && cachedGeneration == generation
	for _, id := range accountIDs {
		if _, present := rows[id]; !present {
			complete = false
			break
		}
	}
	if complete && time.Since(at) < time.Second {
		return rows, nil
	}
	rows, err := s.store.ListAccountRateLimitsByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, err
	}
	s.accountCacheMutex.Lock()
	if s.rateLimitCache == nil {
		s.rateLimitCache = map[string]map[string][]storage.AccountRateLimit{}
	}
	if s.rateLimitCacheGen == nil {
		s.rateLimitCacheGen = map[string]uint64{}
	}
	if s.auxCacheAt == nil {
		s.auxCacheAt = map[string]time.Time{}
	}
	s.rateLimitCache[group] = rows
	s.rateLimitCacheGen[group] = generation
	s.auxCacheAt[key] = time.Now()
	s.accountCacheMutex.Unlock()
	return rows, nil
}

func (s *Scheduler) rateLimitsForSelection(ctx context.Context, selection *accountSelectionSnapshot) (map[string][]storage.AccountRateLimit, error) {
	generation := s.store.RateLimitGeneration()
	s.accountCacheMutex.RLock()
	rows, ok := s.rateLimitCache[selection.group]
	at := s.auxCacheAt["rate:"+selection.group]
	cachedGeneration := s.rateLimitCacheGen[selection.group]
	cachedSelection := s.rateLimitSelectionGen[selection.group]
	s.accountCacheMutex.RUnlock()
	if ok && cachedGeneration == generation && cachedSelection == selection.generation && time.Since(at) < time.Second {
		return rows, nil
	}
	s.rateRefreshMu.Lock()
	defer s.rateRefreshMu.Unlock()
	s.accountCacheMutex.RLock()
	rows, ok = s.rateLimitCache[selection.group]
	at = s.auxCacheAt["rate:"+selection.group]
	cachedGeneration = s.rateLimitCacheGen[selection.group]
	cachedSelection = s.rateLimitSelectionGen[selection.group]
	s.accountCacheMutex.RUnlock()
	if ok && cachedGeneration == generation && cachedSelection == selection.generation && time.Since(at) < time.Second {
		return rows, nil
	}
	rows, err := s.store.ListAccountRateLimitsByAccountIDs(ctx, selection.accountIDs)
	if err != nil {
		return nil, err
	}
	s.accountCacheMutex.Lock()
	if s.rateLimitCache == nil {
		s.rateLimitCache = map[string]map[string][]storage.AccountRateLimit{}
	}
	if s.rateLimitCacheGen == nil {
		s.rateLimitCacheGen = map[string]uint64{}
	}
	if s.rateLimitSelectionGen == nil {
		s.rateLimitSelectionGen = map[string]uint64{}
	}
	if s.auxCacheAt == nil {
		s.auxCacheAt = map[string]time.Time{}
	}
	s.rateLimitCache[selection.group] = rows
	s.rateLimitCacheGen[selection.group] = generation
	s.rateLimitSelectionGen[selection.group] = selection.generation
	s.auxCacheAt["rate:"+selection.group] = time.Now()
	s.accountCacheMutex.Unlock()
	return rows, nil
}

func (s *Scheduler) modelsSnapshot(ctx context.Context, group, model, contextMode string) (map[string]bool, error) {
	key := group + "\x00" + model + "\x00" + strings.ToLower(strings.TrimSpace(contextMode))
	s.accountCacheMutex.RLock()
	rows, ok := s.modelCache[key]
	at := s.auxCacheAt["model:"+key]
	s.accountCacheMutex.RUnlock()
	if ok && time.Since(at) < time.Second {
		return rows, nil
	}
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	s.accountCacheMutex.RLock()
	rows, ok = s.modelCache[key]
	at = s.auxCacheAt["model:"+key]
	s.accountCacheMutex.RUnlock()
	if ok && time.Since(at) < time.Second {
		return rows, nil
	}
	rows, err := s.store.AccountsWithModelAndContext(ctx, group, model, contextMode)
	if err != nil {
		return nil, err
	}
	s.accountCacheMutex.Lock()
	if s.modelCache == nil {
		s.modelCache = map[string]map[string]bool{}
	}
	if s.auxCacheAt == nil {
		s.auxCacheAt = map[string]time.Time{}
	}
	s.modelCache[key] = rows
	s.auxCacheAt["model:"+key] = time.Now()
	s.accountCacheMutex.Unlock()
	return rows, nil
}

func (s *Scheduler) affinitySnapshot(ctx context.Context, hash string) (storage.AffinityBinding, error) {
	now := time.Now()
	entry, ok := s.affinityCache.get(hash, now)
	if ok {
		if entry.found {
			return entry.binding, nil
		}
		return storage.AffinityBinding{}, sql.ErrNoRows
	}
	shard := s.affinityCache.shard(hash)
	shard.loadMu.Lock()
	defer shard.loadMu.Unlock()
	now = time.Now()
	entry, ok = s.affinityCache.get(hash, now)
	if ok {
		if entry.found {
			return entry.binding, nil
		}
		return storage.AffinityBinding{}, sql.ErrNoRows
	}
	binding, err := s.store.GetAffinityBinding(ctx, hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return storage.AffinityBinding{}, err
	}
	s.affinityCache.put(hash, binding, err == nil, now)
	return binding, err
}

func (s *Scheduler) upsertAffinityResult(ctx context.Context, binding storage.AffinityBinding) (storage.AffinityBinding, error) {
	stored, err := s.store.UpsertAffinityBindingResult(ctx, binding)
	if err != nil {
		return storage.AffinityBinding{}, err
	}
	s.affinityCache.put(stored.RouteKeyHash, stored, true, time.Now())
	return stored, nil

}

func (s *Scheduler) upsertAffinity(ctx context.Context, binding storage.AffinityBinding) error {
	_, err := s.upsertAffinityResult(ctx, binding)
	return err
}

func (s *Scheduler) leaseWithAffinityEpoch(ctx context.Context, route Route, lease Lease) Lease {
	if route.Affinity.Hash == "" {
		return lease
	}
	bound, err := s.affinitySnapshot(ctx, route.Affinity.Hash)
	if err == nil && bound.AccountID == lease.Account.ID {
		lease.RouteEpoch = bound.Epoch
	}
	return lease
}

// UpsertAffinityBinding persists a routing alias and publishes the authoritative
// epoch and expiry to the process cache in the same call.
func (s *Scheduler) UpsertAffinityBinding(ctx context.Context, binding storage.AffinityBinding) error {
	return s.upsertAffinity(ctx, binding)
}

func (s *Scheduler) egressTemporarilyUnavailable(ctx context.Context, binding storage.AccountEgressBinding, now int64, ignoreBindingCooldown bool) bool {
	if binding.CooldownUntil > now && !ignoreBindingCooldown {
		return true
	}
	if e, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID); err == nil && (e.CooldownUntil > now || e.Health == "cooldown" || e.Health == "tripped") {
		return true
	}
	return false
}

func capabilityRouteModel(model string) bool {
	model = strings.TrimSpace(model)
	return model != "" && !strings.HasPrefix(strings.ToLower(model), "resource:")
}

func resolveClaudeRouteModel(route Route, caps []storage.ModelCapability) (string, bool, bool) {
	parsed, err := capability.ParseRequestedClaudeModel(route.Model)
	if err != nil {
		return "", false, false
	}
	requested := parsed.BaseModel
	context1M := strings.EqualFold(strings.TrimSpace(route.ContextMode), "1m") || parsed.ContextMode == "1m"
	if capability.ClaudeModelAlias(requested) {
		resolved, ok := capability.ResolveClaudeModel(requested, caps, context1M)
		return resolved, false, ok
	}
	canonical := canonicalClaudeRouteModel(requested)
	for _, c := range caps {
		if canonicalClaudeRouteModel(c.ModelSlug) != canonical {
			continue
		}
		if c.AvailabilityState == capability.AvailabilityUnsupported {
			// model_not_found is scoped to this exact account/model. It must
			// survive catalog bootstrap while leaving other models eligible.
			return "", false, false
		}
		if context1M && c.Context1MState != capability.Context1MSupported {
			return "", false, false
		}
		// A persisted exact discovery hint is sufficient to try this account and is
		// stronger than a Kiro static bootstrap. Runtime success still promotes the
		// row to verified; this flag only controls candidate ordering.
		return c.ModelSlug, false, true
	}
	// No authoritative catalog exists yet: a concrete standard-window request may
	// bootstrap and turn the inference response into runtime evidence. Static hints
	// must not turn absence into an unsupported verdict. Aliases and 1M still require
	// an explicit candidate capability.
	if !capabilityCatalogAuthoritative(caps) && !context1M && canonical != "" && strings.HasPrefix(strings.ToLower(requested), "claude-") {
		return requested, true, true
	}
	return "", false, false
}

// resolveAntigravityRouteModel is intentionally exact and fail-closed. The
// Antigravity model catalogue is live account-scoped evidence, so a model may be
// routed only to an account whose probe verified that exact slug. Unlike the
// Claude/Kiro bootstrap paths, absence is never permission to guess. A 1M request
// additionally requires explicit account-scoped 1M support.
func resolveAntigravityRouteModel(route Route, caps []storage.ModelCapability) (string, bool) {
	parsed, err := capability.ParseRequestedClaudeModel(route.Model)
	if err != nil {
		return "", false
	}
	requested := strings.TrimSpace(parsed.BaseModel)
	context1M := strings.EqualFold(strings.TrimSpace(route.ContextMode), "1m") || parsed.ContextMode == "1m"
	for _, c := range caps {
		if !strings.EqualFold(strings.TrimSpace(c.ModelSlug), requested) || c.AvailabilityState != capability.AvailabilityVerified {
			continue
		}
		if context1M && c.Context1MState != capability.Context1MSupported {
			return "", false
		}
		return strings.TrimSpace(c.ModelSlug), true
	}
	return "", false
}

func resolveCodexRouteModel(route Route, caps []storage.ModelCapability) (string, bool, bool) {
	requested := capability.NormalizeCodexModelAlias(route.Model)
	for _, c := range caps {
		if !strings.EqualFold(strings.TrimSpace(c.ModelSlug), requested) {
			continue
		}
		if c.AvailabilityState == capability.AvailabilityUnsupported {
			return "", false, false
		}
		return c.ModelSlug, false, true
	}
	if !capabilityCatalogAuthoritative(caps) && requested != "" {
		return requested, true, true
	}
	return "", false, false
}

func capabilityCatalogAuthoritative(caps []storage.ModelCapability) bool {
	for _, c := range caps {
		if c.AvailabilityState != capability.AvailabilityVerified {
			continue
		}
		source := strings.ToLower(strings.TrimSpace(c.Source))
		// A successful /models catalog is authoritative for absence. Runtime
		// inference/rejection is evidence for one exact model only and must not
		// prevent a different concrete model from being tried on the account.
		if strings.Contains(source, "probe") && !strings.Contains(source, "static") && !strings.Contains(source, "unknown") {
			return true
		}
		if !strings.Contains(source, "runtime") && !strings.Contains(source, "static") && !strings.Contains(source, "unknown") {
			return true
		}
	}
	return false
}

func modelCatalogAuthorityMarker() storage.ModelCapability {
	return storage.ModelCapability{
		AvailabilityState: capability.AvailabilityVerified,
		Source:            "catalog_probe_marker",
	}
}

func canonicalClaudeRouteModel(model string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(model)), ".", "-")
}

// resolveKiroRouteModel returns a canonical Kiro model plus whether selecting it
// would be a bootstrap attempt. Aliases are resolved exclusively from runtime-
// verified capabilities. A concrete model may bootstrap only when the account's
// persisted static capability table contains the same canonical model and its
// current plan permits it. Claude-family bootstrap additionally requires static
// adaptive-thinking support; Kiro's exact GPT models do not use that envelope.
func (s *Scheduler) resolveKiroRouteModel(ctx context.Context, account storage.Account, route Route) (string, bool, bool) {
	model, bootstrap, ok, _ := s.resolveKiroRouteModelDetailed(ctx, account, route)
	return model, bootstrap, ok
}

// resolveKiroRouteModelDetailed is resolveKiroRouteModel plus the reason it declined.
// Every rejection here used to be an indistinguishable `return "", false, false`, so an
// operator seeing "this Kiro account has quota but is never selected" had roughly fifteen
// candidate explanations — a missing catalog, a 1m-context demand the catalog cannot
// prove, an unverified alias, a plan that forbids bootstrap — and no way to tell which.
// The reason is a diagnostic string only: it never changes which accounts are eligible.
func (s *Scheduler) resolveKiroRouteModelDetailed(ctx context.Context, account storage.Account, route Route) (string, bool, bool, string) {
	credentials, err := s.store.GetKiroCredentials(ctx, account.ID)
	if err != nil {
		return "", false, false, "kiro_credentials_unavailable"
	}
	region := firstRouteValue(credentials.APIRegion, route.KiroDefaultRegion, s.Config().KiroDefaultAPIRegion, "us-east-1")
	allowlist := route.KiroEndpointAllowlist
	if allowlist == nil {
		allowlist = s.Config().KiroEndpointAllowlist
	}
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, region, allowlist)
	if err != nil {
		// The account's endpoint/region is outside the configured allowlist, so no
		// capability row can ever key to it. This is a configuration mismatch, not quota.
		return "", false, false, "kiro_endpoint_not_allowlisted"
	}
	capabilityKey, _ := kirowire.KiroCapabilityKey(endpointHash, region, credentials.ProfileARN)
	catalog, catalogErr := s.store.ListKiroModelCatalog(ctx, account.ID, capabilityKey)
	if catalogErr != nil {
		return "", false, false, "kiro_catalog_read_failed"
	}
	if len(catalog) > 0 {
		descriptor, found := capability.ResolveKiroCatalogModel(route.Model, catalog)
		if !found {
			return "", false, false, "kiro_model_absent_from_live_catalog"
		}
		if strings.EqualFold(strings.TrimSpace(route.ContextMode), "1m") && descriptor.MaxInputTokens < 1_000_000 {
			return "", false, false, "kiro_catalog_model_lacks_1m_context"
		}
		return strings.TrimSpace(descriptor.UpstreamID), false, true, ""
	}
	// One-million-token entitlement is established only by an account-scoped,
	// complete live catalog. Runtime inference and plan labels are deliberately
	// insufficient because they do not prove the current region/governance scope.
	if strings.EqualFold(strings.TrimSpace(route.ContextMode), "1m") {
		return "", false, false, "kiro_1m_context_requires_live_catalog"
	}
	// Claude-family Kiro inference is a mandatory-thinking path, so its runtime
	// evidence must include a successful thinking request. Kiro's GPT-5.6 models
	// use a distinct non-thinking envelope: use ordinary model verification for
	// those exact models so a successful GPT request is reusable on the next
	// selection instead of being treated as a bootstrap forever.
	isGPTRequest := capability.KiroSupportsGPTModel(route.Model)
	verified, err := s.store.VerifiedKiroModels(ctx, account.ID, endpointHash, !isGPTRequest)
	if err != nil {
		return "", false, false, "kiro_verified_models_read_failed"
	}
	resolved, ok := capability.ResolveKiroModel(route.Model, verified)
	if !ok {
		if len(verified) == 0 {
			// Nothing has ever succeeded on this account+endpoint, and the requested
			// model is not bootstrappable — the common "imported but never usable" state.
			return "", false, false, "kiro_no_verified_models_yet"
		}
		return "", false, false, "kiro_model_not_verified"
	}
	if capability.KiroModelAlias(route.Model) {
		normalizedAlias := strings.ToLower(strings.TrimSpace(route.Model))
		if normalizedAlias == "default" {
			return "", false, false, "kiro_default_alias_unresolvable"
		}
		if normalizedAlias == "auto" {
			foundAuto := false
			for _, model := range verified {
				if strings.EqualFold(strings.TrimSpace(model), "auto") {
					foundAuto = true
					break
				}
			}
			if !foundAuto {
				return "", false, false, "kiro_auto_alias_not_verified"
			}
		}
		return resolved, false, true, ""
	}
	for _, model := range verified {
		canonical, modelOK := capability.KiroCanonicalModel(model)
		if modelOK && canonical == resolved {
			return resolved, false, true, ""
		}
	}
	// GPT models are deliberately exact and have already passed
	// KiroSupportsGPTModel above. They do not support the Claude adaptive-thinking
	// envelope, but may still bootstrap from their persisted static capability.
	if !capability.KiroSupportsGPTModel(resolved) && !capability.KiroSupportsAdaptiveThinking(resolved) {
		return "", false, false, "kiro_model_lacks_adaptive_thinking"
	}
	if !capability.KiroPlanAllowsBootstrap(account.PlanType, resolved) {
		// The account's plan label forbids bootstrapping this model. This one is worth
		// distinguishing because the fix is a plan/label correction, not a retry.
		return "", false, false, "kiro_plan_forbids_bootstrap"
	}
	capabilities, err := s.store.ListCapabilities(ctx, account.ID)
	if err != nil {
		return "", false, false, "kiro_static_capabilities_read_failed"
	}
	for _, staticCapability := range capabilities {
		canonical, modelOK := capability.KiroCanonicalModel(staticCapability.ModelSlug)
		if modelOK && canonical == resolved {
			return resolved, true, true, ""
		}
	}
	// Static capabilities are seeded at import (kiro_import.go), so an empty/mismatched
	// table here means the import never completed or the model is outside the seed set.
	return "", false, false, "kiro_model_absent_from_static_capabilities"
}

func (s *Scheduler) tryLeaseAccount(ctx context.Context, accountID string, route Route, egressCache map[string]storage.EgressProfile) (Lease, bool) {
	lease, _, ok := s.tryLeaseAccountDetailed(ctx, accountID, route, egressCache)
	return lease, ok
}

func (s *Scheduler) tryLeaseAccountDetailed(ctx context.Context, accountID string, route Route, egressCache map[string]storage.EgressProfile) (Lease, leaseBlockReason, bool) {
	cfg := s.Config()
	if egressCache == nil {
		egressCache = make(map[string]storage.EgressProfile)
	}
	selection, err := s.accountsSnapshot(ctx, route.Group)
	if err != nil {
		return Lease{}, leaseBlockNotFound, false
	}
	snapshot, found := selection.byID[accountID]
	if !found {
		// An import may race a previously cached empty group snapshot. Invalidate
		// once so state changes are immediately visible; the steady path remains
		// entirely in memory.
		s.InvalidateAccountCache()
		selection, err = s.accountsSnapshot(ctx, route.Group)
		if err == nil {
			snapshot, found = selection.byID[accountID]
		}
	}
	if !found {
		return Lease{}, leaseBlockNotFound, false
	}
	account := snapshot.Account
	now := storage.Now()
	if account.Status != "active" {
		return Lease{}, leaseBlockInactive, false
	}
	if account.QuarantineUntil > now && !account.IgnoreRateLimitControls {
		return Lease{}, leaseBlockQuarantined, false
	}
	if route.Group != "" && account.GroupName != route.Group {
		return Lease{}, leaseBlockGroupMismatch, false
	}
	accountProvider := s.providerOfAccount(ctx, account)
	if !routeAllowsProvider(route, accountProvider) {
		return Lease{}, leaseBlockProviderMismatch, false
	}
	candidateRoute := routeForProviderModel(route, accountProvider)
	goalQuotaGrace := route.AllowCodexGoalQuotaGrace && accountProvider == "codex"
	resolvedModel := candidateRoute.Model
	if accountProvider == "kiro" && capabilityRouteModel(candidateRoute.Model) {
		var ok bool
		var detail string
		resolvedModel, _, ok, detail = s.resolveKiroRouteModelDetailed(ctx, account, candidateRoute)
		if !ok {
			s.logKiroModelBlockOnce(account.ID, candidateRoute.Model, detail)
			return Lease{}, qualifiedBlockReason(leaseBlockModelUnsupported, detail), false
		}
	} else if (accountProvider == "claude" || accountProvider == "codex" || accountProvider == "antigravity") && capabilityRouteModel(candidateRoute.Model) {
		caps, err := s.store.ListCapabilities(ctx, account.ID)
		if err != nil {
			return Lease{}, leaseBlockModelUnsupported, false
		}
		if authoritative, authorityErr := s.store.ModelCatalogAuthoritative(ctx, account.ID); authorityErr != nil {
			return Lease{}, leaseBlockModelUnsupported, false
		} else if authoritative {
			caps = append(caps, modelCatalogAuthorityMarker())
		}
		var ok bool
		if accountProvider == "claude" {
			resolvedModel, _, ok = resolveClaudeRouteModel(candidateRoute, caps)
		} else if accountProvider == "codex" {
			resolvedModel, _, ok = resolveCodexRouteModel(candidateRoute, caps)
		} else {
			resolvedModel, ok = resolveAntigravityRouteModel(candidateRoute, caps)
		}
		if !ok {
			return Lease{}, leaseBlockModelUnsupported, false
		}
	}
	rateRows, rateErr := s.rateLimitsSnapshot(ctx, route.Group, []string{accountID})
	provider := providerForRoute(s, ctx, account, route)
	if rateErr == nil {
		if _, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(rateRows[accountID], provider, candidateRoute.Model, now); limited && !account.IgnoreRateLimitControls && !goalQuotaGrace {
			return Lease{}, leaseBlockRateLimitCooldown, false
		}
	}
	binding := snapshot.Binding
	if route.RequiredEgressID == "" {
		binding = effectiveAccountEgressBinding(binding, accountID, firstRouteValue(route.PreferredEgressIDs...))
	} else {
		// A required account+egress pair is persisted server-side session identity.
		// Group/provider outlet reorderings apply only to fresh work and must not
		// move an established CPA session to a different network path.
		binding.AccountID = accountID
		binding.PrimaryEgressID = strings.TrimSpace(route.RequiredEgressID)
		binding.StandbyEgressIDs = ""
		binding.CookieJarKey = accountID + ":" + binding.PrimaryEgressID
	}
	if binding.PrimaryEgressID == "" {
		return Lease{}, leaseBlockEgressUnavailable, false
	}
	// Benched-after-error accounts are ineligible until the recheck loop clears them,
	// even via the sticky path (which calls this directly).
	if binding.RecheckPending && !account.IgnoreRateLimitControls {
		return Lease{}, leaseBlockRecheckPending, false
	}
	ignoreTelemetryCooldown := account.IgnoreRateLimitControls || (goalQuotaGrace && !binding.RecheckPending)
	var egress storage.EgressProfile
	var ok bool
	if route.RequiredEgressID != "" {
		egress, err = s.egressProfile(ctx, route.RequiredEgressID, egressCache)
		ok = err == nil && EgressHealthy(egress, now) && (ignoreTelemetryCooldown || binding.CooldownUntil <= now)
	} else {
		_, balanced := egressRPMBalancePolicyActive(ctx, route, binding)
		if snapshot.Egress.ID == binding.PrimaryEgressID && !balanced && (ignoreTelemetryCooldown || binding.CooldownUntil <= now) && EgressHealthy(snapshot.Egress, now) {
			egress, ok = snapshot.Egress, true
		} else {
			egress, ok = s.selectEgressForRoute(ctx, binding, route, now, ignoreTelemetryCooldown, &egressCache, &s.egressCacheMutex, &s.egressCacheTime)
		}
	}
	if !ok {
		if s.egressTemporarilyUnavailable(ctx, binding, now, ignoreTelemetryCooldown) {
			return Lease{}, leaseBlockEgressCooldown, false
		}
		return Lease{}, leaseBlockEgressUnavailable, false
	}
	egress, ok = s.applyBoundSidecar(ctx, binding, egress, now, egressCache)
	if !ok {
		return Lease{}, leaseBlockEgressUnavailable, false
	}
	if !strings.EqualFold(strings.TrimSpace(binding.BindingScope), storage.EgressBindingScopeAccount) && route.RequiredEgressID == "" {
		binding.PrimaryEgressID = egress.ID
		binding.CookieJarKey = accountID + ":" + egress.ID
	}
	coordinated, reason, coordinateErr := s.acquireCoordinatedLease(ctx, accountID, egress, cfg.AccountTokenBudget, route)
	if coordinateErr != nil {
		log.Printf("[SCHEDULER] lease coordinator acquire failed account=%s egress=%s: %v", accountID, egress.ID, coordinateErr)
		return Lease{}, leaseBlockCoordinator, false
	}
	if coordinated == nil {
		return Lease{}, reason, false
	}
	s.addLocalLoad(accountID, egress, 1, route.EstimatedTokens)
	released := false
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := coordinated.Release(releaseCtx); err != nil {
			log.Printf("[SCHEDULER] lease coordinator release failed account=%s egress=%s: %v", accountID, egress.ID, err)
		}
		cancel()
		s.addLocalLoad(accountID, egress, -1, -route.EstimatedTokens)
		s.NotifyStateChanged()
	}
	lease := Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: resolvedModel, FencingToken: coordinated.FencingToken(), release: release}
	s.attachEgressRPMReservation(ctx, route, &lease)
	return lease, leaseBlockNone, true
}

func (s *Scheduler) acquireCoordinatedLease(ctx context.Context, accountID string, egress storage.EgressProfile, tokenBudget int64, route Route) (CoordinatedLease, leaseBlockReason, error) {
	resources := []LeaseResource{{ID: egress.ID, Limit: egress.MaxConcurrency}}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && sidecarID != egress.ID {
		resources = append(resources, LeaseResource{ID: sidecarID, Limit: egress.TransportSidecarMaxConcurrency})
	}
	cfg := s.Config()
	ttl := 2 * cfg.RequestTimeout()
	if ttl < 2*time.Minute {
		ttl = 2 * time.Minute
	}
	return s.coordinator.TryAcquire(ctx, LeaseRequest{
		AccountID: accountID, EstimatedTokens: route.EstimatedTokens, TokenBudget: tokenBudget,
		Compaction: route.Compaction, Resources: resources, TTL: ttl,
	})
}

func (s *Scheduler) groupPrimaryEgressID(ctx context.Context, groupName string) (string, error) {
	group, err := s.store.GetGroup(ctx, strings.TrimSpace(groupName))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return storage.DefaultDirectEgressID, nil
		}
		return "", err
	}
	for _, id := range group.EgressIDs {
		if id = strings.TrimSpace(id); id != "" {
			return id, nil
		}
	}
	if id := strings.TrimSpace(group.DefaultEgressID); id != "" {
		return id, nil
	}
	return storage.DefaultDirectEgressID, nil
}

// effectiveAccountEgressBinding applies the single-outlet precedence contract:
// an explicit account pin wins; otherwise the current group primary wins. Standby
// lists are legacy metadata and never participate in inference routing.
func effectiveAccountEgressBinding(binding storage.AccountEgressBinding, accountID, groupEgressID string) storage.AccountEgressBinding {
	binding.AccountID = strings.TrimSpace(accountID)
	accountScoped := strings.EqualFold(strings.TrimSpace(binding.BindingScope), storage.EgressBindingScopeAccount)
	if !strings.EqualFold(strings.TrimSpace(binding.BindingScope), storage.EgressBindingScopeAccount) {
		binding.BindingScope = storage.EgressBindingScopeGroup
		if groupEgressID = strings.TrimSpace(groupEgressID); groupEgressID != "" {
			binding.PrimaryEgressID = groupEgressID
		}
	} else {
		binding.BindingScope = storage.EgressBindingScopeAccount
	}
	binding.PrimaryEgressID = strings.TrimSpace(binding.PrimaryEgressID)
	binding.StandbyEgressIDs = ""
	if binding.AccountID != "" && binding.PrimaryEgressID != "" {
		if !accountScoped || strings.TrimSpace(binding.CookieJarKey) == "" {
			binding.CookieJarKey = binding.AccountID + ":" + binding.PrimaryEgressID
		}
	}
	return binding
}

func (s *Scheduler) currentLoad(accountID string) (int, int64) {
	if s.loads == nil {
		return 0, 0
	}
	return s.loads.account(accountID)
}

func (s *Scheduler) currentEgressLoad(id string) int {
	if s.loads == nil {
		return 0
	}
	return s.loads.egressLoad(id)
}

func (s *Scheduler) addLocalLoad(accountID string, egress storage.EgressProfile, inflightDelta int, tokenDelta int64) {
	if s.loads == nil {
		s.loads = newShardedLoadCounters()
	}
	s.loads.addAccount(accountID, inflightDelta, tokenDelta)
	s.loads.addEgress(egress.ID, inflightDelta)
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && sidecarID != egress.ID {
		s.loads.addEgress(sidecarID, inflightDelta)
	}
}

func (s *Scheduler) egressConcurrencyLimited(egress storage.EgressProfile) bool {
	if concurrencyLimited(egress.MaxConcurrency, s.currentEgressLoad(egress.ID)) {
		return true
	}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" {
		return concurrencyLimited(egress.TransportSidecarMaxConcurrency, s.currentEgressLoad(sidecarID))
	}
	return false
}

func egressConcurrencyLimited(egress storage.EgressProfile, inflight map[string]int) bool {
	if concurrencyLimited(egress.MaxConcurrency, inflight[egress.ID]) {
		return true
	}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" {
		return concurrencyLimited(egress.TransportSidecarMaxConcurrency, inflight[sidecarID])
	}
	return false
}

func incrementEgressInflight(inflight map[string]int, egress storage.EgressProfile) {
	inflight[egress.ID]++
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && sidecarID != egress.ID {
		inflight[sidecarID]++
	}
}

func decrementEgressInflight(inflight map[string]int, egress storage.EgressProfile) {
	if inflight[egress.ID] > 0 {
		inflight[egress.ID]--
	}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && sidecarID != egress.ID && inflight[sidecarID] > 0 {
		inflight[sidecarID]--
	}
}

func (s *Scheduler) loadChangedChan() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadChanged
}

func (s *Scheduler) notifyLoadChangedLocked() {
	close(s.loadChanged)
	s.loadChanged = make(chan struct{})
}

func statefulStickyWaitReason(reason leaseBlockReason) bool {
	// Compare on the base so a diagnostically-qualified reason ("base:detail", see
	// qualifiedBlockReason) is classified identically to its base.
	reason = baseBlockReason(reason)
	return reason == leaseBlockTokenBudget || reason == leaseBlockConcurrency ||
		reason == leaseBlockRateLimitCooldown || reason == leaseBlockRecheckPending ||
		reason == leaseBlockEgressCooldown || reason == leaseBlockCoordinator
}

// boundHealthRecoveryReason identifies account/egress health states where a
// durable CPA root can safely stop waiting and ask its owner to rebuild on a new
// epoch. Capacity pressure is intentionally excluded so ordinary concurrent turns
// keep their exact account and context rather than rotating under load.
func boundHealthRecoveryReason(reason leaseBlockReason) bool {
	switch baseBlockReason(reason) {
	case leaseBlockRateLimitCooldown, leaseBlockRecheckPending, leaseBlockEgressCooldown:
		return true
	default:
		return false
	}
}

func (reason leaseBlockReason) humanString() string {
	if reason == leaseBlockNone {
		return "unknown"
	}
	return strings.ReplaceAll(string(reason), "_", " ")
}

// qualifiedBlockReason appends a diagnostic detail to a base block reason as
// "base:detail". Every consumer of leaseBlockReason compares against the exact base
// constants (statefulStickyWaitReason, waitReasonForBlock) and treats anything else via
// its existing fallthrough, so qualifying a reason adds diagnosis without changing any
// scheduling decision. The base is returned unchanged when there is no detail.
func qualifiedBlockReason(base leaseBlockReason, detail string) leaseBlockReason {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return base
	}
	return base + leaseBlockReason(":"+detail)
}

// baseBlockReason strips any diagnostic qualifier, for callers that must compare against
// the base constants.
func baseBlockReason(reason leaseBlockReason) leaseBlockReason {
	if base, _, found := strings.Cut(string(reason), ":"); found {
		return leaseBlockReason(base)
	}
	return reason
}

// logKiroModelBlockOnce reports a Kiro model-resolution rejection at most once per
// (account, model, reason). Without this the rejection is entirely silent on the pool
// selection path — the account is simply skipped, so "has quota, never selected" has no
// evidence anywhere. The reason is deterministic per account+model, so one line is
// enough; deduping keeps a per-request hot path from flooding the log.
func (s *Scheduler) logKiroModelBlockOnce(accountID, model, detail string) {
	if detail == "" {
		return
	}
	key := accountID + "\x00" + model + "\x00" + detail
	s.kiroBlockLogMu.Lock()
	if s.kiroBlockLogged == nil {
		s.kiroBlockLogged = make(map[string]struct{}, 64)
	}
	_, seen := s.kiroBlockLogged[key]
	if !seen {
		// Bounded: a full table is cleared rather than grown, so a churning account set
		// cannot leak memory. Re-logging after a reset is acceptable for a diagnostic.
		if len(s.kiroBlockLogged) >= kiroBlockLogMaxEntries {
			s.kiroBlockLogged = make(map[string]struct{}, 64)
		}
		s.kiroBlockLogged[key] = struct{}{}
	}
	s.kiroBlockLogMu.Unlock()
	if seen {
		return
	}
	log.Printf("[SCHEDULER] kiro account=%s model=%s not selectable: %s", accountID, model, detail)
}

func (s *Scheduler) waitForStatefulStickyLease(ctx context.Context, accountID string, route Route, initialReason leaseBlockReason) (Lease, error) {
	start := time.Now()
	atomic.AddInt64(&s.metrics.Queued, 1)
	atomic.AddInt64(&s.metrics.Waited, 1)
	s.recordWaitReason(waitReasonForBlock(initialReason))
	defer func() {
		atomic.AddInt64(&s.metrics.Queued, -1)
		atomic.AddInt64(&s.metrics.WaitNanos, time.Since(start).Nanoseconds())
	}()
	lastReason := initialReason
	heartbeatEvery := s.Config().SchedulerHeartbeat()
	lastHeartbeat := time.Now()
	lastPoll := time.Time{}
	for {
		// A clock/load wake can race the caller deadline. Never issue a storage
		// lookup with an already-cancelled context: that turns the pinned account
		// into a misleading "not found" diagnosis and discards the real wait
		// reason (rate limit, concurrency, token budget, and so on).
		if cause := ctx.Err(); cause != nil {
			atomic.AddInt64(&s.metrics.Cancelled, 1)
			return Lease{}, s.statefulStickyWaitTimeoutError(accountID, route, lastReason, time.Since(start), cause)
		}
		loadChanged := s.loadChangedChan()
		now := time.Now()
		if lastPoll.IsZero() || now.Sub(lastPoll) >= schedulerRecoveryPollInterval {
			lastPoll = now
			lease, reason, ok := s.tryLeaseAccountDetailed(ctx, accountID, route, nil)
			if ok {
				return lease, nil
			}
			lastReason = reason
			if !statefulStickyWaitReason(reason) {
				return Lease{}, s.diagnoseStickyUnavailability(ctx, accountID, route)
			}
		}
		clock := sharedSchedulerClock.subscribe()
		select {
		case <-loadChanged:
			atomic.AddInt64(&s.metrics.StateWakeups, 1)
			lastPoll = time.Time{}
		case <-clock:
			now = time.Now()
			atomic.AddInt64(&s.metrics.CooldownWakeups, 1)
			lastPoll = time.Time{}
			if route.OnWait != nil && now.Sub(lastHeartbeat) >= heartbeatEvery {
				route.OnWait(lastReason.humanString(), now.Sub(start))
				lastHeartbeat = now
			}
		case <-ctx.Done():
			atomic.AddInt64(&s.metrics.Cancelled, 1)
			return Lease{}, s.statefulStickyWaitTimeoutError(accountID, route, lastReason, time.Since(start), ctx.Err())
		}
	}
}

func waitReasonForBlock(reason leaseBlockReason) string {
	switch baseBlockReason(reason) {
	case leaseBlockConcurrency:
		return "concurrency"
	case leaseBlockTokenBudget:
		return "token_budget"
	case leaseBlockRateLimitCooldown:
		return "cooldown"
	case leaseBlockRecheckPending:
		return "recheck"
	case leaseBlockEgressCooldown:
		return "egress_cooldown"
	case leaseBlockCoordinator:
		return "coordinator"
	default:
		return "scheduler_wait"
	}
}

func (s *Scheduler) statefulStickyWaitTimeoutError(accountID string, route Route, reason leaseBlockReason, waited time.Duration, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: stateful sticky wait timeout for account %s after %s (last reason %s / %s): %v", ErrStrictUnavailable, accountID, waited.Round(time.Millisecond), reason, reason.humanString(), cause)
	}
	return fmt.Errorf("%w: stateful sticky wait timeout for account %s after %s (last reason %s / %s)", ErrStrictUnavailable, accountID, waited.Round(time.Millisecond), reason, reason.humanString())
}

func (s *Scheduler) accountRateLimitedForRoute(ctx context.Context, account storage.Account, route Route, now int64) bool {
	_, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now)
	return ok
}

func (s *Scheduler) accountRateLimitCooldownUntil(ctx context.Context, account storage.Account, route Route, now int64) (int64, bool) {
	provider := providerForRoute(s, ctx, account, route)
	candidateRoute := routeForProviderModel(route, provider)
	until, ok, err := s.store.AccountRateLimitCooldownUntil(ctx, account.ID, provider, candidateRoute.Model, now)
	if err != nil {
		log.Printf("[SCHEDULER] rate-limit snapshot lookup failed: account=%s provider=%s model=%s err=%v", account.ID, provider, candidateRoute.Model, err)
		return 0, false
	}
	return until, ok
}

func providerForRoute(s *Scheduler, ctx context.Context, account storage.Account, route Route) string {
	if len(route.AllowedProviders) > 0 {
		return s.providerOfAccountCached(ctx, account)
	}
	if p := strings.TrimSpace(route.Provider); p != "" {
		return p
	}
	return s.providerOfAccountCached(ctx, account)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// A non-positive administrative limit means adaptive/unlimited. Resource admission
// is intentionally separate from these explicit hard limits.
func concurrencyLimited(limit, inflight int) bool {
	return limit > 0 && inflight >= limit
}

func tokenBudgetLimited(budget int64, compaction bool, inflight int, tokens, estimated int64) bool {
	return budget > 0 && !compaction && estimated > 0 && inflight > 0 && tokens+estimated > budget
}

// normalizedLoad expresses an outlet's saturation as a fraction so the egress and
// sidecar terms stay inside the sub-1 tiebreaker band that sits under the caller's
// raw per-account inflight count. Keeping them under 1 is what makes account
// fairness the primary axis and outlet load the tiebreaker.
//
// The non-positive case returned the raw inflight count, which inverted the
// convention every admission path shares: concurrencyLimited, the in-process lease
// coordinator, and the Redis acquire script all gate on `limit > 0`, so 0 means
// unlimited. Scoring it as the raw count made "no cap" the single heaviest term in
// the sum, so an uncapped outlet with 8 inflight contributed 8.0 while an outlet
// capped at 16 and equally loaded contributed 0.5 -- the near-saturated outlet won.
// Worse, 8.0 outranks any realistic per-account inflight difference, so one busy
// uncapped egress could override account fairness entirely. An export carrying two
// proxies at max_concurrency=9999999999 next to the only account-bound egress at 0
// is exactly that shape: both spellings mean unlimited, but only the huge cap
// scored like it.
//
// A saturating fraction keeps the uncapped case in the same band while still
// preferring the less-loaded of two uncapped outlets, which returning a flat 0
// would discard.
func normalizedLoad(inflight, limit int) float64 {
	if inflight <= 0 {
		return 0
	}
	if limit <= 0 {
		return float64(inflight) / float64(inflight+1)
	}
	return float64(inflight) / float64(limit)
}

func normalizedTokenLoad(tokens, budget int64) float64 {
	if budget <= 0 {
		return 0
	}
	return float64(tokens) / float64(budget)
}

func rendezvous(key, id string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(id))
	return h.Sum64()
}

func (s *Scheduler) noAccountError(route Route, counters NoAccountCounters) error {
	return s.noAccountErrorForPool(route, counters, -1)
}

// noAccountErrorForPool builds the selection failure, with poolSize reporting how many
// accounts the group held before filtering; pass a negative value when that is unknown.
// A zero pool is surfaced explicitly instead of being left to the counters, which cannot
// represent it.
func (s *Scheduler) noAccountErrorForPool(route Route, counters NoAccountCounters, poolSize int) error {
	if poolSize == 0 {
		model := route.Model
		if routeAllowsProvider(route, "kiro") {
			if canonical, ok := capability.KiroCanonicalModel(model); ok {
				model = canonical
			}
		}
		return &NoAccountError{
			Group:            route.Group,
			Provider:         route.Provider,
			AllowedProviders: append([]string(nil), route.AllowedProviders...),
			Model:            model,
			Counters:         counters,
			EmptyPool:        true,
		}
	}
	if route.Provider == "" && len(route.AllowedProviders) == 0 && route.Model == "" && route.Affinity.Hash == "" {
		return ErrNoAccount
	}
	model := route.Model
	if routeAllowsProvider(route, "kiro") {
		if canonical, ok := capability.KiroCanonicalModel(model); ok {
			model = canonical
		}
	}
	return &NoAccountError{
		Group:            route.Group,
		Provider:         route.Provider,
		AllowedProviders: append([]string(nil), route.AllowedProviders...),
		Model:            model,
		Counters:         counters,
	}
}

func (s *Scheduler) strictStickyCanFailover(ctx context.Context, accountID string, route Route) bool {
	if route.ServerSideState {
		return false
	}
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return true
	}
	now := storage.Now()
	if account.Status != "active" || (account.QuarantineUntil > now && !account.IgnoreRateLimitControls) {
		return false
	}
	cooldownThreshold := time.Duration(s.Config().StrictStickyMaxCooldownSeconds) * time.Second
	if until, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now); !account.IgnoreRateLimitControls && ok && until > now {
		// A short cooldown is worth riding out: rebinding moves the conversation to
		// a fresh account whose upstream prompt-cache prefix is empty, so the very
		// next turn pays the full cold-cache price. Only a cooldown beyond the
		// configured threshold (or threshold=0, "never rebind for a long cooldown")
		// is allowed to fail over.
		if cooldownThreshold == 0 || time.Duration(until-now)*time.Second <= cooldownThreshold {
			return false
		}
		return true
	}
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return true
	}
	if !account.IgnoreRateLimitControls && binding.CooldownUntil > now {
		if cooldownThreshold == 0 || time.Duration(binding.CooldownUntil-now)*time.Second <= cooldownThreshold {
			return false
		}
		return true
	}
	if !account.IgnoreRateLimitControls && binding.RecheckPending {
		return true
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil || !EgressHealthy(egress, now) {
		return true
	}
	egress, ok := s.applyBoundSidecar(ctx, binding, egress, now, nil)
	if !ok {
		return true
	}
	inflight, tokens := s.currentLoad(accountID)
	egressLimited := s.egressConcurrencyLimited(egress)
	if egressLimited {
		return true
	}
	if tokenBudgetLimited(s.Config().AccountTokenBudget, route.Compaction, inflight, tokens, route.EstimatedTokens) {
		return true
	}
	return route.Movable
}

// strictStickyCooldownWait reports a waitable transient cooldown on the bound
// account whose remaining duration sits within the strict-sticky cooldown
// threshold. A caller that gets false from strictStickyCanFailover uses this to
// distinguish "cooldown short enough to ride out — wait for the pinned account"
// from "account is genuinely gone — fail/diagnose instead of waiting pointlessly".
// threshold=0 keeps the "never rebind for a long cooldown" meaning: any cooldown
// is reported as waitable. Non-cooldown blocks (inactive, quarantined, egress
// health) never report waitable.
func (s *Scheduler) strictStickyCooldownWait(ctx context.Context, accountID string, route Route) (leaseBlockReason, bool) {
	threshold := time.Duration(s.Config().StrictStickyMaxCooldownSeconds) * time.Second
	now := storage.Now()
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil || account.Status != "active" || (account.QuarantineUntil > now && !account.IgnoreRateLimitControls) {
		return leaseBlockNone, false
	}
	withinThreshold := func(remaining int64) bool {
		if threshold == 0 {
			return true
		}
		return time.Duration(remaining)*time.Second <= threshold
	}
	if until, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now); !account.IgnoreRateLimitControls && ok && until > now {
		if withinThreshold(until - now) {
			return leaseBlockRateLimitCooldown, true
		}
		return leaseBlockNone, false
	}
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return leaseBlockNone, false
	}
	if !account.IgnoreRateLimitControls && binding.CooldownUntil > now && withinThreshold(binding.CooldownUntil-now) {
		return leaseBlockEgressCooldown, true
	}
	return leaseBlockNone, false
}

// diagnoseStickyUnavailability returns an ErrStrictUnavailable error annotated with the
// specific reason the bound account could not be leased, so the downstream client sees
// an actionable message ("account cooldown 27s" vs "account quarantined").
func (s *Scheduler) diagnoseStickyUnavailability(ctx context.Context, accountID string, route Route) error {
	cfg := s.Config()
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: account %s not found: %v", ErrStrictUnavailable, accountID, err)
	}
	now := storage.Now()

	// 1. Account status checks
	if account.Status != "active" {
		return fmt.Errorf("%w: account %s status=%q", ErrStrictUnavailable, accountID, account.Status)
	}
	if account.QuarantineUntil > now && !account.IgnoreRateLimitControls {
		remaining := account.QuarantineUntil - now
		return fmt.Errorf("%w: account %s quarantined for %ds (reason: %s)", ErrStrictUnavailable, accountID, remaining, account.QuarantineReason)
	}
	if until, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now); !account.IgnoreRateLimitControls && ok {
		remaining := until - now
		hint := ""
		if cfg.StrictStickyMaxCooldownSeconds > 0 && remaining <= int64(cfg.StrictStickyMaxCooldownSeconds) {
			hint = " (pinned: within strict-sticky cooldown threshold)"
		}
		return fmt.Errorf("%w: account %s provider=%q model=%q rate-limit cooldown for %ds%s", ErrStrictUnavailable, accountID, providerForRoute(s, ctx, account, route), route.Model, remaining, hint)
	}

	// 2. Egress binding / cooldown
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: account %s has no egress binding: %v", ErrStrictUnavailable, accountID, err)
	}
	if binding.RecheckPending && !account.IgnoreRateLimitControls {
		return fmt.Errorf("%w: account %s pending health re-check after error", ErrStrictUnavailable, accountID)
	}
	if binding.CooldownUntil > now && !account.IgnoreRateLimitControls {
		remaining := binding.CooldownUntil - now
		return fmt.Errorf("%w: account %s on cooldown for %ds (rate-limited by upstream)", ErrStrictUnavailable, accountID, remaining)
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil {
		return fmt.Errorf("%w: account %s primary egress %q not found", ErrStrictUnavailable, accountID, binding.PrimaryEgressID)
	}
	if !EgressHealthy(egress, now) {
		return fmt.Errorf("%w: account %s egress %q health=%q (cooldown until %d, now %d)", ErrStrictUnavailable, accountID, binding.PrimaryEgressID, egress.Health, egress.CooldownUntil, now)
	}
	egress, err = s.store.ApplySidecarEgressBinding(ctx, binding, egress)
	if err != nil {
		return fmt.Errorf("%w: account %s sidecar transport unavailable: %v", ErrStrictUnavailable, accountID, err)
	}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" {
		sidecar, sidecarErr := s.store.GetEgressProfile(ctx, sidecarID)
		if sidecarErr != nil || !EgressHealthy(sidecar, now) {
			return fmt.Errorf("%w: account %s sidecar transport %q is unhealthy", ErrStrictUnavailable, accountID, sidecarID)
		}
	}

	// 3. Concurrency / token budget
	inflight, tokens := s.currentLoad(accountID)
	if concurrencyLimited(egress.MaxConcurrency, s.currentEgressLoad(egress.ID)) {
		return fmt.Errorf("%w: account %s at max concurrency (%d/%d in-flight)", ErrStrictUnavailable, accountID, inflight, egress.MaxConcurrency)
	}
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && concurrencyLimited(egress.TransportSidecarMaxConcurrency, s.currentEgressLoad(sidecarID)) {
		return fmt.Errorf("%w: account %s sidecar %q at max concurrency", ErrStrictUnavailable, accountID, sidecarID)
	}
	if tokenBudgetLimited(cfg.AccountTokenBudget, route.Compaction, inflight, tokens, route.EstimatedTokens) {
		return fmt.Errorf("%w: account %s token budget exceeded (in-flight %d + estimated %d > budget %d)", ErrStrictUnavailable, accountID, tokens, route.EstimatedTokens, cfg.AccountTokenBudget)
	}

	return fmt.Errorf("%w: account %s unavailable (unknown reason)", ErrStrictUnavailable, accountID)
}

// selectEgress resolves exactly the account/group primary. A disabled, unhealthy,
// cooled-down or saturated primary makes the account unavailable; inference never
// rotates to legacy standby metadata.
func (s *Scheduler) selectEgress(ctx context.Context, binding storage.AccountEgressBinding, now int64, egressCache map[string]storage.EgressProfile, ignoreBindingCooldown bool) (storage.EgressProfile, bool) {
	if ignoreBindingCooldown || binding.CooldownUntil <= now {
		if egress, err := s.egressProfile(ctx, binding.PrimaryEgressID, egressCache); err == nil && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	return storage.EgressProfile{}, false
}

// applyBoundSidecar overlays the account's optional TLS/HTTP2 sidecar on the real
// selected IP egress. The selected egress ID and operational fields remain intact;
// only the request transport fields are replaced. Invalid explicit bindings fail
// closed so callers never fall back to a fingerprint-leaking stdlib path.
func (s *Scheduler) applyBoundSidecar(ctx context.Context, binding storage.AccountEgressBinding, egress storage.EgressProfile, now int64, egressCache map[string]storage.EgressProfile) (storage.EgressProfile, bool) {
	sidecarID := strings.TrimSpace(binding.SidecarEgressID)
	if sidecarID == "" || storage.IsSidecarEgress(egress) {
		return egress, true
	}
	sidecar, err := s.egressProfile(ctx, sidecarID, egressCache)
	if err != nil || !EgressHealthy(sidecar, now) {
		return storage.EgressProfile{}, false
	}
	wrapped, err := storage.WrapEgressWithSidecar(egress, sidecar)
	if err != nil {
		return storage.EgressProfile{}, false
	}
	return wrapped, true
}

// egressProfile fetches an egress profile, serving it from (and populating) the
// request-scoped cache when one is supplied. A cache miss reads through to the store;
// only successful reads are cached.
func (s *Scheduler) egressProfile(ctx context.Context, id string, cache map[string]storage.EgressProfile) (storage.EgressProfile, error) {
	if cache != nil {
		if e, ok := cache[id]; ok {
			return e, nil
		}
	}
	e, err := s.store.GetEgressProfile(ctx, id)
	if err == nil && cache != nil {
		cache[id] = e
	}
	return e, err
}

// Cache TTL for account list — short enough that newly-enabled accounts become
// available quickly, long enough to amortize DB round-trips on the hot path.
const accountCacheTTL = 5 * time.Second

// getActiveAccountsCached returns the active accounts for a group, using a short TTL
// in-memory cache to avoid repeated DB queries on the hot path. The cache is keyed
// by group name and invalidated when account state changes (quarantine, enable/disable).
func (s *Scheduler) getActiveAccountsCached(ctx context.Context, group string) ([]storage.Account, error) {
	s.accountCacheMutex.RLock()
	if s.accountCache != nil && time.Since(s.accountCacheTTL) < accountCacheTTL {
		if cached, ok := s.accountCache[group]; ok {
			s.accountCacheMutex.RUnlock()
			return cached, nil
		}
	}
	s.accountCacheMutex.RUnlock()

	// Cache miss — re-read from DB
	accounts, err := s.store.ListActiveAccountsByGroup(ctx, group)
	if err != nil {
		return nil, err
	}

	s.accountCacheMutex.Lock()
	defer s.accountCacheMutex.Unlock()
	if s.accountCache == nil {
		s.accountCache = make(map[string][]storage.Account)
	}
	s.accountCache[group] = accounts
	s.accountCacheTTL = time.Now()
	return accounts, nil
}

// shortestCooldown returns the shortest remaining cooldown duration across all
// active accounts in the group that match the provider and model requirements.
// Returns (duration, true) if at least one account is cooling; (0, false) if
// no accounts exist or none are cooling. This is used to implement "wait for
// cooldown" behavior when all accounts are temporarily unavailable.
func (s *Scheduler) shortestCooldown(ctx context.Context, group, provider, model string) (time.Duration, bool) {
	accounts, err := s.getActiveAccountsCached(ctx, group)
	if err != nil {
		return 0, false
	}
	now := storage.Now()
	var capable map[string]bool
	if model != "" {
		if m, err := s.modelsSnapshot(ctx, group, model, ""); err == nil && len(m) > 0 {
			capable = m
		}
	}
	var minCooldown int64
	found := false
	for _, account := range accounts {
		if account.Status != "active" || (account.QuarantineUntil > now && !account.IgnoreRateLimitControls) {
			continue
		}
		if capable != nil && !capable[account.ID] {
			continue
		}
		if provider != "" && s.providerOfAccount(ctx, account) != provider {
			continue
		}
		if until, limited := s.accountRateLimitCooldownUntil(ctx, account, Route{Provider: provider, Model: model}, now); limited && !account.IgnoreRateLimitControls {
			remaining := until - now
			if remaining > 0 && (!found || remaining < minCooldown) {
				minCooldown = remaining
				found = true
			}
			continue
		}
		binding, err := s.store.GetEgressBinding(ctx, account.ID)
		if err != nil {
			continue
		}
		binding, err = s.store.EffectiveEgressBinding(ctx, binding)
		if err != nil {
			continue
		}
		// Recheck-pending accounts won't become available the moment their cooldown
		// elapses (they must pass a probe first), so they are not something to wait on.
		if binding.RecheckPending && !account.IgnoreRateLimitControls {
			continue
		}
		if account.IgnoreRateLimitControls || binding.CooldownUntil <= now {
			// Account is not cooling — but check egress health
			egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
			if err == nil && EgressHealthy(egress, now) {
				// Found an available account — no need to wait
				return 0, false
			}
		}
		// Account is cooling — track the shortest cooldown
		if !account.IgnoreRateLimitControls {
			remaining := binding.CooldownUntil - now
			if remaining > 0 && (!found || remaining < minCooldown) {
				minCooldown = remaining
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	return time.Duration(minCooldown) * time.Second, true
}

// EgressHealthy reports whether an egress can be selected now. Breaker-managed
// "cooldown" and "tripped" states are time-boxed by CooldownUntil; once that
// deadline passes, the profile is eligible again without a manual reset.
func EgressHealthy(egress storage.EgressProfile, now int64) bool {
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

func waitFor(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return true
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// providerOf infers an account's upstream provider, preferring the explicit
// Provider column and falling back to the credential-shape heuristic for legacy
// (pre-migration) rows whose provider is still empty.
func (s *Scheduler) providerOf(ctx context.Context, accountID string) string {
	acc, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return accountprovider.UnknownProvider
	}
	return s.providerOfAccount(ctx, acc)
}

// providerOfAccount resolves the provider for an already-loaded account without a
// second GetAccount: the explicit Provider column wins, and the credential-shape
// heuristic is consulted only when it is empty (legacy rows). This keeps the hot
// selectFresh candidate loop from doing a token read per account once accounts are
// migrated (provider set on import).
func (s *Scheduler) providerOfAccount(ctx context.Context, acc storage.Account) string {
	if p := strings.TrimSpace(acc.Provider); p != "" {
		return p
	}
	token, err := s.store.GetToken(ctx, acc.ID)
	if err != nil {
		return accountprovider.UnknownProvider
	}
	return accountprovider.EffectiveProvider(acc.Provider, token, true)
}

// ProviderFromToken maps a stored credential to its provider id.
func ProviderFromToken(t storage.AccountToken) string {
	return accountprovider.InferProviderFromToken(t)
}

// selectEgressWithCache resolves the egress using both the process-level sync.Map
// cache (for cross-request sharing) and the request-scoped map. It populates the
// process-level cache on miss and returns the cached time so the caller can update
// egressCacheTime atomically.
func (s *Scheduler) selectEgressWithCache(ctx context.Context, binding storage.AccountEgressBinding, now int64, ignoreBindingCooldown bool, reqCache *map[string]storage.EgressProfile, procCache *sync.RWMutex, cacheTime *time.Time) (storage.EgressProfile, bool) {
	if ignoreBindingCooldown || binding.CooldownUntil <= now {
		if egress, ok := s.egressProfileWithCache(ctx, binding.PrimaryEgressID, now, reqCache, procCache); ok && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	return storage.EgressProfile{}, false
}

// selectEgressForRoute is the only candidate-level outlet selector. It keeps
// the historical single-primary behavior unless an explicitly attached,
// fresh-root user-group RPM policy supplies multiple outlets. Account-scoped
// bindings and stateful/required routes are intentionally delegated to the
// primary selector and can never be moved by this policy.
func (s *Scheduler) selectEgressForRoute(ctx context.Context, binding storage.AccountEgressBinding, route Route, now int64, ignoreBindingCooldown bool, reqCache *map[string]storage.EgressProfile, procCache *sync.RWMutex, cacheTime *time.Time) (storage.EgressProfile, bool) {
	policy, active := egressRPMBalancePolicyActive(ctx, route, binding)
	if !active || len(route.PreferredEgressIDs) < 2 {
		return s.selectEgressWithCache(ctx, binding, now, ignoreBindingCooldown, reqCache, procCache, cacheTime)
	}
	ordered := s.orderedEgressRPMIDs(ctx, policy, route, binding, now)
	for _, id := range ordered {
		if !ignoreBindingCooldown && binding.CooldownUntil > now {
			return storage.EgressProfile{}, false
		}
		egress, ok := s.egressProfileWithCache(ctx, id, now, reqCache, procCache)
		if !ok || !EgressHealthy(egress, now) {
			continue
		}
		sidecarLimit := 0
		// The sidecar is a transport dependency of every candidate outlet. Skip
		// only this outlet when it is unhealthy so the next configured exit can
		// still receive the request.
		if sidecarID := strings.TrimSpace(binding.SidecarEgressID); sidecarID != "" && !storage.IsSidecarEgress(egress) {
			sidecar, sidecarOK := s.egressProfileWithCache(ctx, sidecarID, now, reqCache, procCache)
			if !sidecarOK || !EgressHealthy(sidecar, now) {
				continue
			}
			if _, wrapErr := storage.WrapEgressWithSidecar(egress, sidecar); wrapErr != nil {
				continue
			}
			sidecarLimit = sidecar.MaxConcurrency
		}
		egressLoad := s.currentEgressLoad(egress.ID)
		sidecarLoad := 0
		if sidecarID := strings.TrimSpace(binding.SidecarEgressID); sidecarID != "" {
			sidecarLoad = s.currentEgressLoad(sidecarID)
		}
		if concurrencyLimited(egress.MaxConcurrency, egressLoad) {
			continue
		}
		if strings.TrimSpace(binding.SidecarEgressID) != "" && concurrencyLimited(sidecarLimit, sidecarLoad) {
			continue
		}
		return egress, true
	}
	return storage.EgressProfile{}, false
}

func (s *Scheduler) currentEgressLoads(egress storage.EgressProfile) (int, int) {
	egressLoad := s.currentEgressLoad(egress.ID)
	sidecarLoad := 0
	if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" && sidecarID != egress.ID {
		sidecarLoad = s.currentEgressLoad(sidecarID)
	}
	return egressLoad, sidecarLoad
}

func (s *Scheduler) applyBoundSidecarWithCache(ctx context.Context, binding storage.AccountEgressBinding, egress storage.EgressProfile, now int64, reqCache *map[string]storage.EgressProfile, procCache *sync.RWMutex) (storage.EgressProfile, bool) {
	sidecarID := strings.TrimSpace(binding.SidecarEgressID)
	if sidecarID == "" || storage.IsSidecarEgress(egress) {
		return egress, true
	}
	sidecar, ok := s.egressProfileWithCache(ctx, sidecarID, now, reqCache, procCache)
	if !ok || !EgressHealthy(sidecar, now) {
		return storage.EgressProfile{}, false
	}
	wrapped, err := storage.WrapEgressWithSidecar(egress, sidecar)
	if err != nil {
		return storage.EgressProfile{}, false
	}
	return wrapped, true
}

// egressProfileWithCache checks the request-scoped cache first, then the process-level
// sync.Map, then falls through to the DB. Successful DB reads populate both caches.
func (s *Scheduler) egressProfileWithCache(ctx context.Context, id string, now int64, reqCache *map[string]storage.EgressProfile, procCache *sync.RWMutex) (storage.EgressProfile, bool) {
	// Request-scoped cache (highest priority, most specific to this selection)
	if reqCache != nil {
		if e, ok := (*reqCache)[id]; ok {
			return e, true
		}
	}
	// Process-level cache (shared across concurrent requests)
	procCache.Lock()
	defer procCache.Unlock()
	if val, ok := s.egressCache.Load(id); ok {
		return val.(storage.EgressProfile), true
	}
	// DB fallback
	e, err := s.store.GetEgressProfile(ctx, id)
	if err != nil {
		return storage.EgressProfile{}, false
	}
	// Populate caches
	if reqCache != nil {
		(*reqCache)[id] = e
	}
	s.egressCache.Store(id, e)
	return e, true
}

// providerOfAccountCached returns the provider for an already-loaded account, using
// the process-level provider cache to avoid repeated token-table lookups for legacy
// accounts whose provider column is still empty.
func (s *Scheduler) providerOfAccountCached(ctx context.Context, acc storage.Account) string {
	// Explicit provider column wins (all modern accounts)
	if p := strings.TrimSpace(acc.Provider); p != "" {
		return p
	}
	// Check process-level cache for legacy accounts
	if val, ok := s.providerCache.Load(acc.ID); ok {
		return val.(string)
	}
	// Cache miss — infer from token shape and cache the result
	token, err := s.store.GetToken(ctx, acc.ID)
	var provider string
	if err != nil {
		provider = accountprovider.UnknownProvider
	} else {
		provider = accountprovider.EffectiveProvider(acc.Provider, token, true)
	}
	s.providerCache.Store(acc.ID, provider)
	return provider
}

// shortestCooldownBatch returns the shortest cooldown using the pre-loaded
// accountsWithEgress data (from ListActiveAccountsWithEgress), avoiding a
// second DB query just for cooldown-waiting. Returns (duration, true) if
// at least one account is cooling; (0, false) if none.
func (s *Scheduler) shortestCooldownBatch(ctx context.Context, group, provider, model string, accountsWithEgress []storage.AccountWithEgress, rateLimitsByAccount map[string][]storage.AccountRateLimit) (time.Duration, bool) {
	now := storage.Now()
	var capable map[string]bool
	if model != "" {
		if m, err := s.modelsSnapshot(ctx, group, model, ""); err == nil && len(m) > 0 {
			capable = m
		}
	}
	var minCooldown int64
	found := false
	for _, awe := range accountsWithEgress {
		account := awe.Account
		binding := awe.Binding
		if account.Status != "active" || (account.QuarantineUntil > now && !account.IgnoreRateLimitControls) {
			continue
		}
		if capable != nil && !capable[account.ID] {
			continue
		}
		accountProvider := strings.TrimSpace(provider)
		if actualProvider := s.providerOfAccountCached(ctx, account); accountProvider != "" && actualProvider != accountProvider {
			continue
		} else if accountProvider == "" {
			accountProvider = actualProvider
		}
		if until, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], accountProvider, model, now); limited && !account.IgnoreRateLimitControls {
			remaining := until - now
			if remaining > 0 && (!found || remaining < minCooldown) {
				minCooldown = remaining
				found = true
			}
			continue
		}
		if binding.RecheckPending && !account.IgnoreRateLimitControls {
			continue
		}
		if account.IgnoreRateLimitControls || binding.CooldownUntil <= now {
			// Account is not cooling — check egress health
			if awe.Egress.ID != "" && EgressHealthy(awe.Egress, now) {
				// Found an available account — no need to wait
				return 0, false
			}
		}
		// Account is cooling — track the shortest cooldown
		if !account.IgnoreRateLimitControls {
			remaining := binding.CooldownUntil - now
			if remaining > 0 && (!found || remaining < minCooldown) {
				minCooldown = remaining
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	return time.Duration(minCooldown) * time.Second, true
}

// tryLeaseAccountFromCandidate attempts to acquire a lease using data already
// loaded in the candidate struct (account + egress), avoiding the re-reads that
// tryLeaseAccount does for data we already have.
func (s *Scheduler) tryLeaseAccountFromCandidate(ctx context.Context, c candidate, route Route) (Lease, leaseBlockReason, bool) {
	cfg := s.Config()
	coordinated, reason, coordinateErr := s.acquireCoordinatedLease(ctx, c.account.ID, c.egress, cfg.AccountTokenBudget, route)
	if coordinateErr != nil {
		log.Printf("[SCHEDULER] lease coordinator acquire failed account=%s egress=%s: %v", c.account.ID, c.egress.ID, coordinateErr)
		return Lease{}, leaseBlockCoordinator, false
	}
	if coordinated == nil {
		return Lease{}, reason, false
	}
	return s.activateCoordinatedCandidate(c, route, coordinated), leaseBlockNone, true
}

// activateCoordinatedCandidate publishes a coordinator-owned grant to the
// process-local load snapshot and returns its idempotent release wrapper.
func (s *Scheduler) activateCoordinatedCandidate(c candidate, route Route, coordinated CoordinatedLease) Lease {
	account := c.account
	egress := c.egress
	accountID := account.ID
	estimatedTokens := route.EstimatedTokens
	s.addLocalLoad(accountID, egress, 1, estimatedTokens)
	released := false
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := coordinated.Release(releaseCtx); err != nil {
			log.Printf("[SCHEDULER] lease coordinator release failed account=%s egress=%s: %v", accountID, egress.ID, err)
		}
		cancel()
		s.addLocalLoad(accountID, egress, -1, -estimatedTokens)
		s.NotifyStateChanged()
	}
	// The binding was already loaded by selectFresh's batch query and carried on the
	// candidate, so the lease is built without a second DB read while holding s.mu —
	// keeping the hot critical section to map ops only (the previous GetEgressBinding
	// here serialized every lease grant and release behind a SQLite round-trip).
	return Lease{Account: account, Binding: c.binding, Egress: egress, ResolvedModel: c.resolvedModel, FencingToken: coordinated.FencingToken(), Trial: c.trial, release: release}
}
