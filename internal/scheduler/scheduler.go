package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

var ErrNoAccount = errors.New("no active account available")
var ErrStrictUnavailable = errors.New("strict sticky account unavailable")
var ErrBoundAccountUnavailable = errors.New("bound account unavailable")

type Scheduler struct {
	store          *storage.Store
	cfg            atomic.Value // stores config.Config
	mu             sync.Mutex
	inflight       map[string]int
	inflightTokens map[string]int64
	loadChanged    chan struct{}
	rr             int // round-robin cursor for spreading load across equal-load candidates
	queueMu        sync.Mutex
	waitQueues     map[string][]*waiter
	metrics        SchedulerMetrics

	// accountCache caches the active accounts list per group with a short TTL to reduce
	// DB queries on the hot path. The cache is invalidated when an account's status
	// changes (quarantine, enable/disable) — callers that modify accounts should call
	// InvalidateAccountCache() afterward. TTL is short (5s) so a newly-enabled account
	// becomes available quickly while still amortizing the DB round-trip.
	accountCache      map[string][]storage.Account
	accountCacheTTL   time.Time
	accountCacheMutex sync.RWMutex

	// egressCache is a process-level cache for egress profiles used by selectEgress.
	// It is separate from the request-scoped egressCache in selectFresh so that concurrent
	// requests share the same cached egress profiles, avoiding repeated DB lookups for
	// standby egresses (which are resolved per-candidate, not once per request).
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
	score         float64 // normalized account/token/egress load; lower is better
}

type waiter struct {
	id      uint64
	started time.Time
}

// SchedulerMetrics is a cheap runtime snapshot used by diagnostics and tests. Values
// are process-local and intentionally monotonic except Queued, which is a gauge.
type SchedulerMetrics struct {
	Queued          int64 `json:"queued"`
	Waited          int64 `json:"waited"`
	Cancelled       int64 `json:"cancelled"`
	AccountSwitches int64 `json:"account_switches"`
	WaitConcurrency int64 `json:"wait_concurrency"`
	WaitTokenBudget int64 `json:"wait_token_budget"`
	WaitCooldown    int64 `json:"wait_cooldown"`
	WaitRecheck     int64 `json:"wait_recheck"`
	WaitEgress      int64 `json:"wait_egress"`
	CooldownWakeups int64 `json:"cooldown_wakeups"`
	StateWakeups    int64 `json:"state_wakeups"`
	WaitNanos       int64 `json:"wait_nanos"`
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
)

// InvalidateAccountCache clears the account list cache so the next Select() call
// re-reads from the DB. Call this after any operation that changes account state
// (quarantine, enable/disable, group change, import/delete).
func (s *Scheduler) InvalidateAccountCache() {
	s.accountCacheMutex.Lock()
	s.accountCache = nil
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
	Strict           bool
	ServerSideState  bool
	// ImmutableAffinity pins provider, account, resolved model and egress after
	// the first selection. Kiro and Claude/Kiro auto sessions use this mode.
	ImmutableAffinity bool
	// ExplicitProvider permits a concrete, not-yet-verified Kiro model to be tried
	// on the explicitly requested account. Auto scheduling only considers verified
	// runtime capabilities.
	ExplicitProvider      bool
	ThinkingRequired      bool
	RequiredEgressID      string
	KiroEndpointAllowlist []string
	KiroDefaultRegion     string
	// Movable is kept for existing call sites/tests that already computed
	// !ServerSideState. New callers should set ServerSideState directly.
	Movable         bool
	Model           string
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
}

type NoAccountCounters struct {
	Inactive          int
	Quarantined       int
	Excluded          int
	ProviderMismatch  int
	ModelUnsupported  int
	RateLimitCooldown int
	RecheckPending    int
	EgressUnavailable int
	EgressCooldown    int
	Concurrency       int
	TokenBudget       int
}

type NoAccountError struct {
	Group    string
	Provider string
	Model    string
	Counters NoAccountCounters
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
	if len(parts) == 0 {
		return ErrNoAccount.Error()
	}
	return fmt.Sprintf("%s: group=%q provider=%q model=%q skipped(%s)", ErrNoAccount, e.Group, e.Provider, e.Model, strings.Join(parts, " "))
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
	c := e.Counters
	return c.Concurrency+c.TokenBudget+c.RateLimitCooldown+c.RecheckPending+c.EgressCooldown > 0
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
	release       func()
}

func New(store *storage.Store, cfg config.Config) *Scheduler {
	s := &Scheduler{
		store:            store,
		inflight:         map[string]int{},
		inflightTokens:   map[string]int64{},
		loadChanged:      make(chan struct{}),
		waitQueues:       map[string][]*waiter{},
		egressCacheTTL:   30 * time.Second,
		providerCacheTTL: 5 * time.Minute,
	}
	s.cfg.Store(cfg)
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
	s.NotifyStateChanged()
}

// NotifyStateChanged wakes queued selections after any account, quota, cooldown or
// egress mutation. Polling remains as a safety net for callers outside the API layer.
func (s *Scheduler) NotifyStateChanged() {
	s.mu.Lock()
	s.notifyLoadChangedLocked()
	s.mu.Unlock()
}

func (s *Scheduler) Metrics() SchedulerMetrics {
	return SchedulerMetrics{
		Queued: atomic.LoadInt64(&s.metrics.Queued), Waited: atomic.LoadInt64(&s.metrics.Waited),
		Cancelled: atomic.LoadInt64(&s.metrics.Cancelled), AccountSwitches: atomic.LoadInt64(&s.metrics.AccountSwitches),
		WaitConcurrency: atomic.LoadInt64(&s.metrics.WaitConcurrency), WaitTokenBudget: atomic.LoadInt64(&s.metrics.WaitTokenBudget),
		WaitCooldown: atomic.LoadInt64(&s.metrics.WaitCooldown), WaitRecheck: atomic.LoadInt64(&s.metrics.WaitRecheck),
		WaitEgress: atomic.LoadInt64(&s.metrics.WaitEgress), CooldownWakeups: atomic.LoadInt64(&s.metrics.CooldownWakeups),
		StateWakeups: atomic.LoadInt64(&s.metrics.StateWakeups), WaitNanos: atomic.LoadInt64(&s.metrics.WaitNanos),
	}
}

func (s *Scheduler) Select(ctx context.Context, route Route) (Lease, error) {
	cfg := s.Config()
	hadExclusions := len(route.Exclude) > 0
	hadBinding := false
	if route.Group == "" {
		route.Group = cfg.DefaultGroup
	}
	if route.Affinity.Hash != "" {
		if bound, err := s.store.GetAffinityBinding(ctx, route.Affinity.Hash); err == nil {
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
					if bound.Model != "" {
						lease.ResolvedModel = bound.Model
					}
					if bound.Provider == "" || bound.Model == "" || bound.EgressID == "" {
						_ = s.store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
							RouteKeyHash: bound.RouteKeyHash, RouteKey: bound.RouteKey, Source: bound.Source,
							AccountID: bound.AccountID, Provider: boundProvider,
							Model: firstRouteValue(lease.ResolvedModel, boundRoute.Model), EgressID: lease.Egress.ID,
						})
					}
					return lease, nil
				}
				if statefulStickyWaitReason(reason) {
					lease, waitErr := s.waitForStatefulStickyLease(ctx, bound.AccountID, boundRoute, reason)
					if waitErr == nil && bound.Model != "" {
						lease.ResolvedModel = bound.Model
					}
					if waitErr == nil {
						return lease, nil
					}
				}
				return Lease{}, fmt.Errorf("%w: provider=%s account=%s model=%s egress=%s reason=%s", ErrBoundAccountUnavailable, boundProvider, bound.AccountID, bound.Model, bound.EgressID, reason.humanString())
			}
			// Skip the sticky account entirely when this request already tried and
			// failed on it (Exclude) — fall through to selectFresh, which rebinds the
			// affinity to the fresh account so the conversation seamlessly continues
			// on a healthy one instead of bouncing back to the broken account.
			if routeAllowsProvider(route, s.providerOf(ctx, bound.AccountID)) && !route.Exclude[bound.AccountID] {
				lease, reason, ok := s.tryLeaseAccountDetailed(ctx, bound.AccountID, route, nil)
				if ok {
					if bound.Provider == "" || bound.Model == "" || bound.EgressID == "" {
						_ = s.store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
							RouteKeyHash: bound.RouteKeyHash, RouteKey: bound.RouteKey, Source: bound.Source,
							AccountID: bound.AccountID, Provider: s.providerOfAccount(ctx, lease.Account),
							Model: firstRouteValue(lease.ResolvedModel, route.Model), EgressID: lease.Egress.ID,
						})
					}
					return lease, nil
				}
				if route.Strict && route.ServerSideState {
					if statefulStickyWaitReason(reason) {
						return s.waitForStatefulStickyLease(ctx, bound.AccountID, route, reason)
					}
					return Lease{}, s.diagnoseStickyUnavailability(ctx, bound.AccountID, route)
				}
				if waitFor(ctx, cfg.StickyWait()) {
					lease, reason, ok = s.tryLeaseAccountDetailed(ctx, bound.AccountID, route, nil)
					if ok {
						return lease, nil
					}
				}
				if route.Strict {
					if !s.strictStickyCanFailover(ctx, bound.AccountID, route) {
						return Lease{}, s.diagnoseStickyUnavailability(ctx, bound.AccountID, route)
					}
					log.Printf("[SCHEDULER] strict-sticky falling through to selectFresh for route affinity=%s", route.Affinity.Hash)
				}
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Lease{}, err
		}
	}

	var lease Lease
	var err error
	// Once a route has queued work, new requests join behind it instead of racing
	// the head waiter for a newly released lease. Affinity selections above retain
	// priority because they preserve an existing conversation's account binding.
	if ctx.Done() != nil && s.routeHasWaiters(route) {
		lease, err = s.waitForFreshLease(ctx, route, &NoAccountError{
			Group: route.Group, Provider: route.Provider, Model: route.Model,
			Counters: NoAccountCounters{Concurrency: 1},
		})
	} else {
		lease, err = s.selectFresh(ctx, route)
	}
	// Once every compatible account in a retry round has failed, begin the next
	// round with a clean exclusion set. The failed accounts have already been cooled
	// or invalidated by the API layer, so this transitions into the normal wait queue
	// instead of leaking a synthetic no-account response.
	if err != nil && len(route.Exclude) > 0 {
		var nae *NoAccountError
		if errors.As(err, &nae) && nae.Counters.Excluded > 0 {
			for id := range route.Exclude {
				delete(route.Exclude, id)
			}
			lease, err = s.selectFresh(ctx, route)
		}
	}
	if err != nil && transientlySaturated(err) && ctx.Done() != nil {
		lease, err = s.waitForFreshLease(ctx, route, err)
	}
	if err != nil {
		return Lease{}, err
	}
	if hadExclusions {
		atomic.AddInt64(&s.metrics.AccountSwitches, 1)
	}
	if route.Affinity.Hash != "" {
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
			_ = s.store.UpsertAffinityBinding(ctx, binding)
		}
	}
	return lease, nil
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
	return route.Group + "\x00" + route.Model + "\x00" + routeProvidersKey(route)
}

func (s *Scheduler) enqueue(route Route, reason string) (string, *waiter) {
	w := &waiter{id: uint64(time.Now().UnixNano()), started: time.Now()}
	key := schedulerQueueKey(route)
	s.queueMu.Lock()
	s.waitQueues[key] = append(s.waitQueues[key], w)
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
	q := s.waitQueues[key]
	for i, w := range q {
		if w == target {
			s.waitQueues[key] = append(q[:i], q[i+1:]...)
			if len(s.waitQueues[key]) == 0 {
				delete(s.waitQueues, key)
			}
			atomic.AddInt64(&s.metrics.Queued, -1)
			return i == 0
		}
	}
	return false
}

func (s *Scheduler) waiterIsHead(key string, target *waiter) bool {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	q := s.waitQueues[key]
	return len(q) > 0 && q[0] == target
}

func (s *Scheduler) routeHasWaiters(route Route) bool {
	key := schedulerQueueKey(route)
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	return len(s.waitQueues[key]) > 0
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
	heartbeat := time.NewTicker(heartbeatEvery)
	poll := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	defer poll.Stop()
	for {
		if s.waiterIsHead(key, w) {
			lease, err := s.selectFresh(ctx, route)
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
		select {
		case <-ctx.Done():
			atomic.AddInt64(&s.metrics.Cancelled, 1)
			return Lease{}, ctx.Err()
		case <-changed:
			atomic.AddInt64(&s.metrics.StateWakeups, 1)
		case <-poll.C:
			atomic.AddInt64(&s.metrics.CooldownWakeups, 1)
		case <-heartbeat.C:
			if route.OnWait != nil {
				route.OnWait(waitReason(lastErr), time.Since(w.started))
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
}

func (s *Scheduler) selectFresh(ctx context.Context, route Route) (Lease, error) {
	cfg := s.Config()
	// Batch query: load all accounts + bindings + primary egress in ONE query
	// instead of N queries per account (GetEgressBinding + selectEgress per candidate).
	accountsWithEgress, err := s.store.ListActiveAccountsWithEgress(ctx, route.Group)
	if err != nil {
		return Lease{}, err
	}
	now := storage.Now()
	accountIDs := make([]string, 0, len(accountsWithEgress))
	for _, awe := range accountsWithEgress {
		accountIDs = append(accountIDs, awe.Account.ID)
	}
	// Load every candidate's quota snapshots in one query (chunked by storage for
	// large pools). The old per-candidate AccountRateLimitCooldownUntil call made
	// account selection O(N) SQLite round-trips under load.
	rateLimitsByAccount, rateLimitsErr := s.store.ListAccountRateLimitsByAccountIDs(ctx, accountIDs)
	if rateLimitsErr != nil {
		// Preserve the historical fail-open behavior of accountRateLimitCooldownUntil:
		// a diagnostics-table read failure is logged but does not take the whole pool
		// offline. Upstream errors still bench/recheck an exhausted account normally.
		log.Printf("[SCHEDULER] batch rate-limit snapshot lookup failed: group=%s err=%v", route.Group, rateLimitsErr)
		rateLimitsByAccount = nil
	}
	// Process-level egress profile cache with 30s TTL.
	// The request-scoped map (below) still handles per-selection deduplication,
	// but for standby egress lookups (which happen per-candidate, not just once)
	// the process-level cache prevents repeated DB round-trips across concurrent requests.
	s.egressCacheMutex.Lock()
	if time.Since(s.egressCacheTime) > s.egressCacheTTL {
		s.egressCache = sync.Map{}
	}
	egressCacheTime := s.egressCacheTime
	egressCacheMutex := &s.egressCacheMutex
	s.egressCacheMutex.Unlock()

	var capable map[string]bool
	if route.Model != "" {
		if m, err := s.store.AccountsWithModel(ctx, route.Group, route.Model); err == nil && len(m) > 0 {
			capable = m
		}
	}
	reqEgressCache := make(map[string]storage.EgressProfile)
	var candidates []candidate
	var counters NoAccountCounters
	for _, awe := range accountsWithEgress {
		account := awe.Account
		binding := awe.Binding
		if account.Status != "active" {
			counters.Inactive++
			continue
		}
		if account.QuarantineUntil > now {
			counters.Quarantined++
			continue
		}
		if route.Exclude[account.ID] {
			counters.Excluded++
			continue
		}
		accountProvider := s.providerOfAccountCached(ctx, account)
		if !routeAllowsProvider(route, accountProvider) {
			counters.ProviderMismatch++
			continue
		}
		resolvedModel := route.Model
		if accountProvider == "kiro" && route.Model != "" {
			var modelOK bool
			resolvedModel, modelOK = s.resolveKiroRouteModel(ctx, account, route)
			if !modelOK {
				counters.ModelUnsupported++
				continue
			}
		} else if capable != nil && !capable[account.ID] {
			counters.ModelUnsupported++
			continue
		} else if accountProvider == "claude" {
			resolvedModel = capability.NormalizeClaudeModelAlias(route.Model)
		}
		if _, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], accountProvider, route.Model, now); limited {
			counters.RateLimitCooldown++
			continue
		}
		// An account benched after an error stays out of the pool until the recheck
		// loop confirms it is healthy again — even once its cooldown has elapsed.
		if binding.RecheckPending {
			counters.RecheckPending++
			continue
		}
		egress, ok := s.selectEgressWithCache(ctx, binding, now, &reqEgressCache, egressCacheMutex, &egressCacheTime)
		if !ok {
			if s.egressTemporarilyUnavailable(ctx, binding, now) {
				counters.EgressCooldown++
			} else {
				counters.EgressUnavailable++
			}
			continue
		}
		inflight, tokens := s.currentLoad(account.ID)
		if inflight >= egress.MaxConcurrency {
			counters.Concurrency++
			continue
		}
		// Token budget guards CONCURRENT over-commit, not single-request size: only
		// reject when the account is already serving traffic (inflight > 0). A solo
		// request (inflight 0) is always admitted regardless of estimate, because a
		// request larger than the whole budget can never be placed on ANY account, so
		// rejecting it would only manufacture a 409/503 the pool cannot avoid — the
		// upstream's real context limit is the right place for that to fail. Virtual-
		// context materialization (which runs after selection) then compresses an
		// oversized body to the account's native window where possible.
		if !route.Compaction && route.EstimatedTokens > 0 && inflight > 0 && tokens+route.EstimatedTokens > cfg.AccountTokenBudget {
			counters.TokenBudget++
			continue
		}
		concurrencyLoad := float64(inflight) / float64(maxInt(1, egress.MaxConcurrency))
		tokenLoad := float64(tokens) / float64(maxInt64(1, cfg.AccountTokenBudget))
		latencyPenalty := float64(maxInt64(0, egress.LatencyMillis)) / 100000.0
		candidates = append(candidates, candidate{account: account, egress: egress, binding: binding, resolvedModel: resolvedModel, score: concurrencyLoad + tokenLoad + latencyPenalty})
	}
	if len(candidates) == 0 {
		return Lease{}, s.noAccountError(route, counters)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score < candidates[j].score
	})
	// Round-robin among the LEAST-loaded candidates so new conversations spread evenly
	// across idle accounts instead of always landing on the oldest one (stable-sort
	// order). Hammering a single account rate-limits it first and leaves the rest cold;
	// spreading widens the warm-cache footprint and the effective rate-limit headroom.
	// Sticky/affinity routing is unaffected (handled before selectFresh). We then try the
	// rotated order so a lease race just falls through to the next candidate.
	minScore := candidates[0].score
	nEq := 0
	for nEq < len(candidates) && candidates[nEq].score == minScore {
		nEq++
	}
	start := 0
	if nEq > 1 {
		s.mu.Lock()
		start = s.rr % nEq
		s.rr++
		s.mu.Unlock()
	}
	order := make([]int, 0, len(candidates))
	for k := 0; k < nEq; k++ {
		order = append(order, (start+k)%nEq)
	}
	for k := nEq; k < len(candidates); k++ {
		order = append(order, k)
	}
	for _, idx := range order {
		if lease, ok := s.tryLeaseAccountFromCandidate(ctx, candidates[idx], route); ok {
			return lease, nil
		}
	}
	return Lease{}, s.noAccountError(route, counters)
}

func (s *Scheduler) egressTemporarilyUnavailable(ctx context.Context, binding storage.AccountEgressBinding, now int64) bool {
	if binding.CooldownUntil > now {
		return true
	}
	ids := append([]string{binding.PrimaryEgressID}, binding.StandbyIDs()...)
	for _, id := range ids {
		if e, err := s.store.GetEgressProfile(ctx, id); err == nil && (e.CooldownUntil > now || e.Health == "cooldown" || e.Health == "tripped") {
			return true
		}
	}
	return false
}

func (s *Scheduler) resolveKiroRouteModel(ctx context.Context, account storage.Account, route Route) (string, bool) {
	credentials, err := s.store.GetKiroCredentials(ctx, account.ID)
	if err != nil {
		return "", false
	}
	region := firstRouteValue(credentials.APIRegion, route.KiroDefaultRegion, s.Config().KiroDefaultAPIRegion, "us-east-1")
	allowlist := route.KiroEndpointAllowlist
	if allowlist == nil {
		allowlist = s.Config().KiroEndpointAllowlist
	}
	endpointHash, err := kirowire.EndpointHash(credentials.Endpoint, region, allowlist)
	if err != nil {
		return "", false
	}
	verified, err := s.store.VerifiedKiroModels(ctx, account.ID, endpointHash, route.ThinkingRequired)
	if err != nil {
		return "", false
	}
	resolved, ok := capability.ResolveKiroModel(route.Model, verified)
	if !ok {
		return "", false
	}
	if route.ThinkingRequired && !capability.KiroSupportsAdaptiveThinking(resolved) {
		return "", false
	}
	if route.ExplicitProvider && !capability.KiroModelAlias(route.Model) {
		return resolved, true
	}
	for _, model := range verified {
		canonical, modelOK := capability.KiroCanonicalModel(model)
		if modelOK && canonical == resolved {
			return resolved, true
		}
	}
	return "", false
}

func (s *Scheduler) tryLeaseAccount(ctx context.Context, accountID string, route Route, egressCache map[string]storage.EgressProfile) (Lease, bool) {
	lease, _, ok := s.tryLeaseAccountDetailed(ctx, accountID, route, egressCache)
	return lease, ok
}

func (s *Scheduler) tryLeaseAccountDetailed(ctx context.Context, accountID string, route Route, egressCache map[string]storage.EgressProfile) (Lease, leaseBlockReason, bool) {
	cfg := s.Config()
	account, err := s.store.GetAccount(ctx, accountID)
	if err != nil {
		return Lease{}, leaseBlockNotFound, false
	}
	now := storage.Now()
	if account.Status != "active" {
		return Lease{}, leaseBlockInactive, false
	}
	if account.QuarantineUntil > now {
		return Lease{}, leaseBlockQuarantined, false
	}
	if route.Group != "" && account.GroupName != route.Group {
		return Lease{}, leaseBlockGroupMismatch, false
	}
	accountProvider := s.providerOfAccount(ctx, account)
	if !routeAllowsProvider(route, accountProvider) {
		return Lease{}, leaseBlockProviderMismatch, false
	}
	resolvedModel := route.Model
	if accountProvider == "kiro" && route.Model != "" {
		var ok bool
		resolvedModel, ok = s.resolveKiroRouteModel(ctx, account, route)
		if !ok {
			return Lease{}, leaseBlockModelUnsupported, false
		}
	} else if accountProvider == "claude" && route.Model != "" {
		resolvedModel = capability.NormalizeClaudeModelAlias(route.Model)
	}
	if s.accountRateLimitedForRoute(ctx, account, route, now) {
		return Lease{}, leaseBlockRateLimitCooldown, false
	}
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return Lease{}, leaseBlockEgressUnavailable, false
	}
	// Benched-after-error accounts are ineligible until the recheck loop clears them,
	// even via the sticky path (which calls this directly).
	if binding.RecheckPending {
		return Lease{}, leaseBlockRecheckPending, false
	}
	var egress storage.EgressProfile
	var ok bool
	if route.RequiredEgressID != "" {
		if !bindingContainsEgress(binding, route.RequiredEgressID) {
			return Lease{}, leaseBlockEgressUnavailable, false
		}
		egress, err = s.egressProfile(ctx, route.RequiredEgressID, egressCache)
		ok = err == nil && EgressHealthy(egress, now) && binding.CooldownUntil <= now
	} else {
		egress, ok = s.selectEgress(ctx, binding, now, egressCache)
	}
	if !ok {
		if s.egressTemporarilyUnavailable(ctx, binding, now) {
			return Lease{}, leaseBlockEgressCooldown, false
		}
		return Lease{}, leaseBlockEgressUnavailable, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[accountID] >= egress.MaxConcurrency {
		return Lease{}, leaseBlockConcurrency, false
	}
	// Budget gate applies only when stacking onto existing in-flight load (see the
	// rationale in selectFresh): a solo request is always admitted.
	if !route.Compaction && route.EstimatedTokens > 0 && s.inflight[accountID] > 0 && s.inflightTokens[accountID]+route.EstimatedTokens > cfg.AccountTokenBudget {
		return Lease{}, leaseBlockTokenBudget, false
	}
	s.inflight[accountID]++
	s.inflightTokens[accountID] += route.EstimatedTokens
	released := false
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		if s.inflight[accountID] > 0 {
			s.inflight[accountID]--
		}
		if route.EstimatedTokens > 0 {
			s.inflightTokens[accountID] -= route.EstimatedTokens
			if s.inflightTokens[accountID] < 0 {
				s.inflightTokens[accountID] = 0
			}
		}
		s.notifyLoadChangedLocked()
	}
	return Lease{Account: account, Binding: binding, Egress: egress, ResolvedModel: resolvedModel, release: release}, leaseBlockNone, true
}

func bindingContainsEgress(binding storage.AccountEgressBinding, egressID string) bool {
	if binding.PrimaryEgressID == egressID {
		return true
	}
	for _, standby := range binding.StandbyIDs() {
		if standby == egressID {
			return true
		}
	}
	return false
}

func (s *Scheduler) currentLoad(accountID string) (int, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[accountID], s.inflightTokens[accountID]
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
	return reason == leaseBlockTokenBudget || reason == leaseBlockConcurrency ||
		reason == leaseBlockRateLimitCooldown || reason == leaseBlockRecheckPending ||
		reason == leaseBlockEgressCooldown
}

func (reason leaseBlockReason) humanString() string {
	if reason == leaseBlockNone {
		return "unknown"
	}
	return strings.ReplaceAll(string(reason), "_", " ")
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
	heartbeat := time.NewTicker(s.Config().SchedulerHeartbeat())
	poll := time.NewTicker(time.Second)
	defer heartbeat.Stop()
	defer poll.Stop()
	for {
		loadChanged := s.loadChangedChan()
		lease, reason, ok := s.tryLeaseAccountDetailed(ctx, accountID, route, nil)
		if ok {
			return lease, nil
		}
		lastReason = reason
		if !statefulStickyWaitReason(reason) {
			return Lease{}, s.diagnoseStickyUnavailability(ctx, accountID, route)
		}
		select {
		case <-loadChanged:
			atomic.AddInt64(&s.metrics.StateWakeups, 1)
			continue
		case <-poll.C:
			atomic.AddInt64(&s.metrics.CooldownWakeups, 1)
			continue
		case <-heartbeat.C:
			if route.OnWait != nil {
				route.OnWait(lastReason.humanString(), time.Since(start))
			}
		case <-ctx.Done():
			atomic.AddInt64(&s.metrics.Cancelled, 1)
			return Lease{}, s.statefulStickyWaitTimeoutError(accountID, route, lastReason, time.Since(start), ctx.Err())
		}
	}
}

func waitReasonForBlock(reason leaseBlockReason) string {
	switch reason {
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
	until, ok, err := s.store.AccountRateLimitCooldownUntil(ctx, account.ID, provider, route.Model, now)
	if err != nil {
		log.Printf("[SCHEDULER] rate-limit snapshot lookup failed: account=%s provider=%s model=%s err=%v", account.ID, provider, route.Model, err)
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

func (s *Scheduler) noAccountError(route Route, counters NoAccountCounters) error {
	if route.Provider == "" && route.Model == "" && route.Affinity.Hash == "" {
		return ErrNoAccount
	}
	return &NoAccountError{
		Group:    route.Group,
		Provider: route.Provider,
		Model:    route.Model,
		Counters: counters,
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
	if account.Status != "active" || account.QuarantineUntil > now {
		return false
	}
	if until, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now); ok && until > now {
		return true
	}
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return true
	}
	if binding.RecheckPending || binding.CooldownUntil > now {
		return true
	}
	egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
	if err != nil || !EgressHealthy(egress, now) {
		return true
	}
	inflight, tokens := s.currentLoad(accountID)
	if inflight >= egress.MaxConcurrency {
		return true
	}
	if !route.Compaction && route.EstimatedTokens > 0 && inflight > 0 && tokens+route.EstimatedTokens > s.Config().AccountTokenBudget {
		return true
	}
	return route.Movable
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
	if account.QuarantineUntil > now {
		remaining := account.QuarantineUntil - now
		return fmt.Errorf("%w: account %s quarantined for %ds (reason: %s)", ErrStrictUnavailable, accountID, remaining, account.QuarantineReason)
	}
	if until, ok := s.accountRateLimitCooldownUntil(ctx, account, route, now); ok {
		return fmt.Errorf("%w: account %s provider=%q model=%q rate-limit cooldown for %ds", ErrStrictUnavailable, accountID, providerForRoute(s, ctx, account, route), route.Model, until-now)
	}

	// 2. Egress binding / cooldown
	binding, err := s.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		return fmt.Errorf("%w: account %s has no egress binding: %v", ErrStrictUnavailable, accountID, err)
	}
	if binding.RecheckPending {
		return fmt.Errorf("%w: account %s pending health re-check after error", ErrStrictUnavailable, accountID)
	}
	if binding.CooldownUntil > now {
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

	// 3. Concurrency / token budget
	inflight, tokens := s.currentLoad(accountID)
	if inflight >= egress.MaxConcurrency {
		return fmt.Errorf("%w: account %s at max concurrency (%d/%d in-flight)", ErrStrictUnavailable, accountID, inflight, egress.MaxConcurrency)
	}
	if !route.Compaction && route.EstimatedTokens > 0 && inflight > 0 && tokens+route.EstimatedTokens > cfg.AccountTokenBudget {
		return fmt.Errorf("%w: account %s token budget exceeded (in-flight %d + estimated %d > budget %d)", ErrStrictUnavailable, accountID, tokens, route.EstimatedTokens, cfg.AccountTokenBudget)
	}

	return fmt.Errorf("%w: account %s unavailable (unknown reason)", ErrStrictUnavailable, accountID)
}

// selectEgress resolves the egress an account will use now: its primary if not on
// cooldown and healthy, else the first healthy standby. egressCache (when non-nil)
// memoizes GetEgressProfile lookups by id within a single selection so accounts that
// share an egress do not each re-read the same row; pass nil to read through.
func (s *Scheduler) selectEgress(ctx context.Context, binding storage.AccountEgressBinding, now int64, egressCache map[string]storage.EgressProfile) (storage.EgressProfile, bool) {
	if binding.CooldownUntil <= now {
		if egress, err := s.egressProfile(ctx, binding.PrimaryEgressID, egressCache); err == nil && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	for _, standbyID := range binding.StandbyIDs() {
		if egress, err := s.egressProfile(ctx, standbyID, egressCache); err == nil && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	return storage.EgressProfile{}, false
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
		if m, err := s.store.AccountsWithModel(ctx, group, model); err == nil && len(m) > 0 {
			capable = m
		}
	}
	var minCooldown int64
	found := false
	for _, account := range accounts {
		if account.Status != "active" || account.QuarantineUntil > now {
			continue
		}
		if capable != nil && !capable[account.ID] {
			continue
		}
		if provider != "" && s.providerOfAccount(ctx, account) != provider {
			continue
		}
		if until, limited := s.accountRateLimitCooldownUntil(ctx, account, Route{Provider: provider, Model: model}, now); limited {
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
		// Recheck-pending accounts won't become available the moment their cooldown
		// elapses (they must pass a probe first), so they are not something to wait on.
		if binding.RecheckPending {
			continue
		}
		if binding.CooldownUntil <= now {
			// Account is not cooling — but check egress health
			egress, err := s.store.GetEgressProfile(ctx, binding.PrimaryEgressID)
			if err == nil && EgressHealthy(egress, now) {
				// Found an available account — no need to wait
				return 0, false
			}
		}
		// Account is cooling — track the shortest cooldown
		remaining := binding.CooldownUntil - now
		if remaining > 0 && (!found || remaining < minCooldown) {
			minCooldown = remaining
			found = true
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
func (s *Scheduler) selectEgressWithCache(ctx context.Context, binding storage.AccountEgressBinding, now int64, reqCache *map[string]storage.EgressProfile, procCache *sync.RWMutex, cacheTime *time.Time) (storage.EgressProfile, bool) {
	if binding.CooldownUntil <= now {
		if egress, ok := s.egressProfileWithCache(ctx, binding.PrimaryEgressID, now, reqCache, procCache); ok && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	for _, standbyID := range binding.StandbyIDs() {
		if egress, ok := s.egressProfileWithCache(ctx, standbyID, now, reqCache, procCache); ok && EgressHealthy(egress, now) {
			return egress, true
		}
	}
	return storage.EgressProfile{}, false
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
		if m, err := s.store.AccountsWithModel(ctx, group, model); err == nil && len(m) > 0 {
			capable = m
		}
	}
	var minCooldown int64
	found := false
	for _, awe := range accountsWithEgress {
		account := awe.Account
		binding := awe.Binding
		if account.Status != "active" || account.QuarantineUntil > now {
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
		if until, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], accountProvider, model, now); limited {
			remaining := until - now
			if remaining > 0 && (!found || remaining < minCooldown) {
				minCooldown = remaining
				found = true
			}
			continue
		}
		if binding.RecheckPending {
			continue
		}
		if binding.CooldownUntil <= now {
			// Account is not cooling — check egress health
			if awe.Egress.ID != "" && EgressHealthy(awe.Egress, now) {
				// Found an available account — no need to wait
				return 0, false
			}
		}
		// Account is cooling — track the shortest cooldown
		remaining := binding.CooldownUntil - now
		if remaining > 0 && (!found || remaining < minCooldown) {
			minCooldown = remaining
			found = true
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
func (s *Scheduler) tryLeaseAccountFromCandidate(ctx context.Context, c candidate, route Route) (Lease, bool) {
	cfg := s.Config()
	account := c.account
	egress := c.egress
	accountID := account.ID
	estimatedTokens := route.EstimatedTokens
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[accountID] >= egress.MaxConcurrency {
		return Lease{}, false
	}
	// Budget gate applies only when stacking onto existing in-flight load (see the
	// rationale in selectFresh): a solo request is always admitted.
	if !route.Compaction && estimatedTokens > 0 && s.inflight[accountID] > 0 && s.inflightTokens[accountID]+estimatedTokens > cfg.AccountTokenBudget {
		return Lease{}, false
	}
	s.inflight[accountID]++
	s.inflightTokens[accountID] += estimatedTokens
	released := false
	release := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		if s.inflight[accountID] > 0 {
			s.inflight[accountID]--
		}
		if estimatedTokens > 0 {
			s.inflightTokens[accountID] -= estimatedTokens
			if s.inflightTokens[accountID] < 0 {
				s.inflightTokens[accountID] = 0
			}
		}
		s.notifyLoadChangedLocked()
	}
	// The binding was already loaded by selectFresh's batch query and carried on the
	// candidate, so the lease is built without a second DB read while holding s.mu —
	// keeping the hot critical section to map ops only (the previous GetEgressBinding
	// here serialized every lease grant and release behind a SQLite round-trip).
	return Lease{Account: account, Binding: c.binding, Egress: egress, ResolvedModel: c.resolvedModel, release: release}, true
}
