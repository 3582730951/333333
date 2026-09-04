package scheduler

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

// DynamicPoolBalancePolicy is request-scoped metadata supplied by the user-group
// router. It is deliberately explicit: a scheduler caller must opt into the
// feature, identify a fresh/root request, and assert that the choices are one
// compatible account-pool tier. Traffic-fallback groups must never set it.
type DynamicPoolBalancePolicy struct {
	Enabled             bool
	RPMThreshold        int64
	UserGroupID         string
	AgentClass          string
	Fresh               bool
	Bound               bool
	OnlyAccountPoolTier bool
	EventID             string
	// EgressRPMBalance* is a separate user-group policy carried in the same
	// request context so one route-choice evaluation can apply both balancing
	// dimensions without adding another scheduler context wrapper.
	EgressRPMBalanceEnabled   bool
	EgressRPMBalanceThreshold int64
	EgressRPMBalanceEgressIDs []string
}

type dynamicPoolBalancePolicyContextKey struct{}
type dynamicPoolBalanceEventIDContextKey struct{}

// WithDynamicPoolBalance attaches an opt-in balancing policy to a request
// context. Disabled policies are intentionally not attached by the API path.
func WithDynamicPoolBalance(ctx context.Context, policy DynamicPoolBalancePolicy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	// A scheduler-only caller may use the compact {Enabled, RPMThreshold} form;
	// API callers provide a non-empty UserGroupID and explicit gates. Keeping the
	// compact form useful makes the pure scheduler contract testable without
	// weakening the user-group traffic-fallback guard.
	if policy.Enabled && strings.TrimSpace(policy.UserGroupID) == "" {
		if strings.TrimSpace(policy.AgentClass) == "" {
			policy.AgentClass = storage.AgentClassRoot
		}
		if !policy.Fresh {
			policy.Fresh = true
		}
		if !policy.OnlyAccountPoolTier {
			policy.OnlyAccountPoolTier = true
		}
	}
	return context.WithValue(ctx, dynamicPoolBalancePolicyContextKey{}, policy)
}

func dynamicPoolBalancePolicyFromContext(ctx context.Context) (DynamicPoolBalancePolicy, bool) {
	if ctx == nil {
		return DynamicPoolBalancePolicy{}, false
	}
	policy, ok := ctx.Value(dynamicPoolBalancePolicyContextKey{}).(DynamicPoolBalancePolicy)
	return policy, ok
}

// WithDynamicPoolBalanceEventID lets the reservation overlay deduplicate the
// first arrival and subsequent retry/failover attempts for one logical request.
func WithDynamicPoolBalanceEventID(ctx context.Context, eventID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, dynamicPoolBalanceEventIDContextKey{}, strings.TrimSpace(eventID))
}

func dynamicPoolBalanceEventIDFromContext(ctx context.Context, policy DynamicPoolBalancePolicy) string {
	if ctx != nil {
		if eventID, ok := ctx.Value(dynamicPoolBalanceEventIDContextKey{}).(string); ok && strings.TrimSpace(eventID) != "" {
			return strings.TrimSpace(eventID)
		}
	}
	return strings.TrimSpace(policy.EventID)
}

// DynamicPoolBalanceSnapshot is an immutable publication from the background
// logical-root rate refresher. Rates is never mutated after publication.
type DynamicPoolBalanceSnapshot struct {
	SampledAt   int64
	PublishedAt int64
	Available   bool
	Rates       map[string]storage.AccountRootRate
	EgressRates map[string]int64
}

const (
	dynamicPoolBalanceRefreshInterval       = time.Second
	dynamicPoolBalanceSnapshotMaxAgeSeconds = int64(3)
	dynamicPoolBalanceReservationTTLSeconds = int64(60)
	dynamicPoolBalanceReservationLimit      = 4096
)

type dynamicPoolBalanceReservation struct {
	AccountID  string
	EgressID   string
	OccurredAt int64
	Confirmed  bool
}

type dynamicPoolBalanceRuntime struct {
	snapshot       atomic.Value // *DynamicPoolBalanceSnapshot
	snapshotErrors atomic.Int64

	startMu sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	reservationMu sync.Mutex
	reservations  map[string]dynamicPoolBalanceReservation
}

func newDynamicPoolBalanceRuntime() *dynamicPoolBalanceRuntime {
	runtime := &dynamicPoolBalanceRuntime{reservations: make(map[string]dynamicPoolBalanceReservation)}
	runtime.snapshot.Store(&DynamicPoolBalanceSnapshot{Rates: map[string]storage.AccountRootRate{}, EgressRates: map[string]int64{}})
	return runtime
}

func (r *dynamicPoolBalanceRuntime) start(ctx context.Context, store *storage.Store) {
	if r == nil || store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.startMu.Lock()
	if r.started {
		r.startMu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	r.started = true
	r.cancel = cancel
	r.wg.Add(1)
	r.startMu.Unlock()
	go func() {
		defer supervisor.Recover("dynamic-pool-balance")
		defer r.wg.Done()
		// An initial refresh makes the first enabled request converge quickly while
		// remaining entirely outside the selection call stack.
		refreshDynamicPoolBalanceSnapshot(workerCtx, store, r)
		ticker := time.NewTicker(dynamicPoolBalanceRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				refreshDynamicPoolBalanceSnapshot(workerCtx, store, r)
			}
		}
	}()
}

func (r *dynamicPoolBalanceRuntime) stop() {
	if r == nil {
		return
	}
	r.startMu.Lock()
	if !r.started {
		r.startMu.Unlock()
		return
	}
	cancel := r.cancel
	r.cancel = nil
	r.started = false
	r.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()
}

func (r *dynamicPoolBalanceRuntime) publish(snapshot *DynamicPoolBalanceSnapshot) {
	if r == nil || snapshot == nil {
		return
	}
	copyRates := make(map[string]storage.AccountRootRate, len(snapshot.Rates))
	for accountID, rate := range snapshot.Rates {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			copyRates[accountID] = rate
		}
	}
	copyEgress := make(map[string]int64, len(snapshot.EgressRates))
	for id, rate := range snapshot.EgressRates {
		id = strings.TrimSpace(id)
		if id != "" {
			if rate < 0 {
				rate = 0
			}
			copyEgress[id] = rate
		}
	}
	copySnapshot := &DynamicPoolBalanceSnapshot{
		SampledAt: snapshot.SampledAt, PublishedAt: snapshot.PublishedAt,
		Available: snapshot.Available, Rates: copyRates, EgressRates: copyEgress,
	}
	r.snapshot.Store(copySnapshot)
}

func (r *dynamicPoolBalanceRuntime) load(now int64) (*DynamicPoolBalanceSnapshot, bool) {
	if r == nil {
		return nil, false
	}
	value := r.snapshot.Load()
	if value == nil {
		return nil, false
	}
	snapshot, ok := value.(*DynamicPoolBalanceSnapshot)
	if !ok || snapshot == nil || !snapshot.Available || snapshot.SampledAt <= 0 {
		return snapshot, false
	}
	if now < snapshot.SampledAt {
		return snapshot, true
	}
	if now-snapshot.SampledAt > dynamicPoolBalanceSnapshotMaxAgeSeconds {
		return snapshot, false
	}
	return snapshot, true
}

func (r *dynamicPoolBalanceRuntime) rate(accountID string, now int64, snapshot *DynamicPoolBalanceSnapshot) storage.AccountRootRate {
	rate := storage.AccountRootRate{}
	if snapshot != nil {
		rate = snapshot.Rates[strings.TrimSpace(accountID)]
	}
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	for eventID, reservation := range r.reservations {
		if reservation.OccurredAt <= 0 || now-reservation.OccurredAt >= dynamicPoolBalanceReservationTTLSeconds {
			delete(r.reservations, eventID)
			continue
		}
		// A confirmed arrival is covered once a successful snapshot has sampled
		// its second. Provisional reservations are retained until the lease is
		// released, so a cancellation before the first upstream attempt cannot
		// masquerade as root traffic.
		if reservation.Confirmed && snapshot != nil && snapshot.Available && snapshot.SampledAt >= reservation.OccurredAt {
			delete(r.reservations, eventID)
			continue
		}
		if reservation.AccountID == strings.TrimSpace(accountID) {
			if rate.RootRPM < int64(^uint64(0)>>1) {
				rate.RootRPM++
			}
		}
	}
	return rate
}

// egressRate returns the sampled logical RPM for one outlet plus reservations
// selected locally since the last durable snapshot. Reservations close the
// sub-second race where several concurrent requests could all observe the same
// below-threshold snapshot and overrun an outlet before the next refresh.
func (r *dynamicPoolBalanceRuntime) egressRate(egressID string, now int64, snapshot *DynamicPoolBalanceSnapshot) int64 {
	if r == nil {
		return 0
	}
	egressID = strings.TrimSpace(egressID)
	if egressID == "" {
		return 0
	}
	rate := int64(0)
	if snapshot != nil {
		rate = snapshot.EgressRates[egressID]
		if rate < 0 {
			rate = 0
		}
	}
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	for eventID, reservation := range r.reservations {
		if reservation.OccurredAt <= 0 || now-reservation.OccurredAt >= dynamicPoolBalanceReservationTTLSeconds {
			delete(r.reservations, eventID)
			continue
		}
		if reservation.Confirmed && snapshot != nil && snapshot.Available && snapshot.SampledAt >= reservation.OccurredAt {
			delete(r.reservations, eventID)
			continue
		}
		if reservation.EgressID == egressID && rate < int64(^uint64(0)>>1) {
			rate++
		}
	}
	return rate
}

func (r *dynamicPoolBalanceRuntime) reserve(accountID, eventID string, now int64) {
	if r == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(eventID) == "" || now <= 0 {
		return
	}
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	for key, reservation := range r.reservations {
		if reservation.OccurredAt <= 0 || now-reservation.OccurredAt >= dynamicPoolBalanceReservationTTLSeconds {
			delete(r.reservations, key)
		}
	}
	if existing, exists := r.reservations[eventID]; exists {
		if existing.AccountID == "" {
			existing.AccountID = strings.TrimSpace(accountID)
			r.reservations[eventID] = existing
		}
		return
	}
	if len(r.reservations) >= dynamicPoolBalanceReservationLimit {
		oldestID := ""
		oldestAt := int64(0)
		for key, reservation := range r.reservations {
			if oldestID == "" || reservation.OccurredAt < oldestAt {
				oldestID, oldestAt = key, reservation.OccurredAt
			}
		}
		if oldestID != "" {
			delete(r.reservations, oldestID)
		}
	}
	r.reservations[strings.TrimSpace(eventID)] = dynamicPoolBalanceReservation{AccountID: strings.TrimSpace(accountID), OccurredAt: now}
}

func (r *dynamicPoolBalanceRuntime) reserveEgress(egressID, eventID string, now int64) {
	if r == nil || strings.TrimSpace(egressID) == "" || strings.TrimSpace(eventID) == "" || now <= 0 {
		return
	}
	egressID = strings.TrimSpace(egressID)
	eventID = strings.TrimSpace(eventID)
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	for key, reservation := range r.reservations {
		if reservation.OccurredAt <= 0 || now-reservation.OccurredAt >= dynamicPoolBalanceReservationTTLSeconds {
			delete(r.reservations, key)
		}
	}
	if existing, exists := r.reservations[eventID]; exists {
		if existing.EgressID == "" {
			existing.EgressID = egressID
			r.reservations[eventID] = existing
		}
		return
	}
	if len(r.reservations) >= dynamicPoolBalanceReservationLimit {
		oldestID := ""
		oldestAt := int64(0)
		for key, reservation := range r.reservations {
			if oldestID == "" || reservation.OccurredAt < oldestAt {
				oldestID, oldestAt = key, reservation.OccurredAt
			}
		}
		if oldestID != "" {
			delete(r.reservations, oldestID)
		}
	}
	r.reservations[eventID] = dynamicPoolBalanceReservation{EgressID: egressID, OccurredAt: now}
}

func (r *dynamicPoolBalanceRuntime) confirm(accountID, eventID string, egressIDs ...string) {
	if r == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(eventID) == "" {
		return
	}
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	reservation, ok := r.reservations[strings.TrimSpace(eventID)]
	if !ok || (reservation.AccountID != "" && reservation.AccountID != strings.TrimSpace(accountID)) {
		return
	}
	if reservation.AccountID == "" {
		reservation.AccountID = strings.TrimSpace(accountID)
	}
	if reservation.EgressID == "" && len(egressIDs) > 0 {
		reservation.EgressID = strings.TrimSpace(egressIDs[0])
	}
	reservation.Confirmed = true
	r.reservations[strings.TrimSpace(eventID)] = reservation
}

// releaseProvisional removes a reservation when a lease is released before its
// first durable logical-arrival write. Confirmed arrivals deliberately remain
// until a fresh snapshot covers them.
func (r *dynamicPoolBalanceRuntime) releaseProvisional(eventID string) {
	if r == nil || strings.TrimSpace(eventID) == "" {
		return
	}
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	key := strings.TrimSpace(eventID)
	reservation, ok := r.reservations[key]
	if ok && !reservation.Confirmed {
		delete(r.reservations, key)
	}
}

func (r *dynamicPoolBalanceRuntime) reservationCount(now int64) int {
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	for eventID, reservation := range r.reservations {
		if reservation.OccurredAt <= 0 || now-reservation.OccurredAt >= dynamicPoolBalanceReservationTTLSeconds {
			delete(r.reservations, eventID)
		}
	}
	return len(r.reservations)
}

func refreshDynamicPoolBalanceSnapshot(ctx context.Context, store *storage.Store, runtime *dynamicPoolBalanceRuntime) {
	if ctx == nil || store == nil || runtime == nil {
		return
	}
	now := storage.Now()
	rates, err := store.AccountRootRateSnapshot(ctx, now)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		runtime.snapshotErrors.Add(1)
		runtime.publish(&DynamicPoolBalanceSnapshot{PublishedAt: now, Available: false, Rates: map[string]storage.AccountRootRate{}})
		return
	}
	erates, _ := store.EgressRootRateSnapshot(ctx, now)
	runtime.publish(&DynamicPoolBalanceSnapshot{SampledAt: now, PublishedAt: now, Available: true, Rates: rates, EgressRates: erates})
}

func (r *dynamicPoolBalanceRuntime) snapshotErrorCount() int64 {
	if r == nil {
		return 0
	}
	return r.snapshotErrors.Load()
}

// StartDynamicPoolBalance starts at most one bounded worker. The caller should
// pass the server runtime context, not an individual request context.
func (s *Scheduler) StartDynamicPoolBalance(ctx context.Context) {
	if s == nil || s.dynamicBalance == nil || s.store == nil {
		return
	}
	s.dynamicBalance.start(ctx, s.store)
}

// RefreshDynamicPoolBalance performs one explicit background-style refresh. It
// is useful to startup code and deterministic tests; SelectAcross never calls it.
func (s *Scheduler) RefreshDynamicPoolBalance(ctx context.Context) error {
	if s == nil || s.dynamicBalance == nil || s.store == nil {
		return errors.New("dynamic pool balance is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := storage.Now()
	rates, err := s.store.AccountRootRateSnapshot(ctx, now)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.dynamicBalance.snapshotErrors.Add(1)
		}
		s.dynamicBalance.publish(&DynamicPoolBalanceSnapshot{PublishedAt: now, Available: false, Rates: map[string]storage.AccountRootRate{}})
		return err
	}
	erates, _ := s.store.EgressRootRateSnapshot(ctx, now)
	s.dynamicBalance.publish(&DynamicPoolBalanceSnapshot{SampledAt: now, PublishedAt: now, Available: true, Rates: rates, EgressRates: erates})
	return nil
}

// SetDynamicPoolBalanceSnapshot publishes a deterministic snapshot for tests or
// an external refresher. The map is copied before publication.
func (s *Scheduler) SetDynamicPoolBalanceSnapshot(sampledAt int64, rates map[string]storage.AccountRootRate) {
	if s == nil || s.dynamicBalance == nil {
		return
	}
	if sampledAt <= 0 {
		sampledAt = storage.Now()
	}
	var egressRates map[string]int64
	if current, _ := s.dynamicBalance.load(storage.Now()); current != nil {
		egressRates = current.EgressRates
	}
	s.dynamicBalance.publish(&DynamicPoolBalanceSnapshot{SampledAt: sampledAt, PublishedAt: storage.Now(), Available: true, Rates: rates, EgressRates: egressRates})
}

// SetEgressRPMBalanceSnapshot publishes an outlet-rate snapshot for deterministic
// tests and external refreshers. Account rates from the current snapshot remain
// intact, so callers can update one balancing dimension without disabling the
// other.
func (s *Scheduler) SetEgressRPMBalanceSnapshot(sampledAt int64, rates map[string]int64) {
	if s == nil || s.dynamicBalance == nil {
		return
	}
	if sampledAt <= 0 {
		sampledAt = storage.Now()
	}
	current, _ := s.dynamicBalance.load(storage.Now())
	accountRates := map[string]storage.AccountRootRate{}
	if current != nil {
		accountRates = current.Rates
	}
	s.dynamicBalance.publish(&DynamicPoolBalanceSnapshot{SampledAt: sampledAt, PublishedAt: storage.Now(), Available: true, Rates: accountRates, EgressRates: rates})
}

// PublishDynamicPoolBalanceSnapshot is a descriptive alias for callers that
// already use publication terminology.
func (s *Scheduler) PublishDynamicPoolBalanceSnapshot(sampledAt int64, rates map[string]storage.AccountRootRate) {
	s.SetDynamicPoolBalanceSnapshot(sampledAt, rates)
}

type dynamicPoolBalanceCandidate struct {
	ChoiceKey  string
	AccountID  string
	RootRPM    int64
	UnknownRPM int64
}

type dynamicPoolBalanceClass uint8

const (
	dynamicPoolBalanceIndeterminate dynamicPoolBalanceClass = iota
	dynamicPoolBalanceLow
	dynamicPoolBalanceHigh
)

func saturatingDynamicAdd(left, right int64) int64 {
	if left < 0 {
		left = 0
	}
	if right < 0 || left > int64(^uint64(0)>>1)-right {
		return int64(^uint64(0) >> 1)
	}
	return left + right
}

func classifyDynamicPoolBalance(rate dynamicPoolBalanceCandidate, threshold int64) dynamicPoolBalanceClass {
	if threshold <= 0 {
		return dynamicPoolBalanceIndeterminate
	}
	lower := rate.RootRPM
	if lower < 0 {
		lower = 0
	}
	unknown := rate.UnknownRPM
	if unknown < 0 {
		unknown = 0
	}
	upper := saturatingDynamicAdd(lower, unknown)
	if upper < threshold {
		return dynamicPoolBalanceLow
	}
	if lower >= threshold {
		return dynamicPoolBalanceHigh
	}
	return dynamicPoolBalanceIndeterminate
}

// computeDynamicPoolBalancePriorities applies the strict interval policy. A
// positive value is relief (preferred), a negative value is high-load (deprioritized),
// and zero preserves the ordinary scheduler ordering. No candidate is removed.
func computeDynamicPoolBalancePriorities(candidates []dynamicPoolBalanceCandidate, threshold int64) (map[string]int, string) {
	priorities := make(map[string]int, len(candidates))
	if threshold <= 0 || len(candidates) == 0 {
		return priorities, "disabled_or_invalid_threshold"
	}
	type classified struct {
		candidate dynamicPoolBalanceCandidate
		class     dynamicPoolBalanceClass
	}
	byChoice := make(map[string][]classified)
	choiceOrder := make([]string, 0)
	for _, candidate := range candidates {
		candidate.ChoiceKey = strings.TrimSpace(candidate.ChoiceKey)
		candidate.AccountID = strings.TrimSpace(candidate.AccountID)
		if candidate.ChoiceKey == "" || candidate.AccountID == "" {
			continue
		}
		key := candidate.ChoiceKey + "\x00" + candidate.AccountID
		priorities[key] = 0
		if _, exists := byChoice[candidate.ChoiceKey]; !exists {
			choiceOrder = append(choiceOrder, candidate.ChoiceKey)
		}
		byChoice[candidate.ChoiceKey] = append(byChoice[candidate.ChoiceKey], classified{candidate: candidate, class: classifyDynamicPoolBalance(candidate, threshold)})
	}
	if len(byChoice) == 0 {
		return priorities, "no_candidates"
	}
	// Unknown intervals are deliberately conservative. One uncertain account can
	// make a target's all-high/all-low conclusion unsafe, so this tier fails open.
	for _, choice := range choiceOrder {
		for _, item := range byChoice[choice] {
			if item.class == dynamicPoolBalanceIndeterminate {
				return priorities, "unknown_interval"
			}
		}
	}

	// Case 1: a target has both certain low and certain high accounts. Keep
	// walking all targets: a mixed target can also be the relief target for Case 2.
	applied := false
	for _, choice := range choiceOrder {
		items := byChoice[choice]
		hasLow, hasHigh := false, false
		for _, item := range items {
			hasLow = hasLow || item.class == dynamicPoolBalanceLow
			hasHigh = hasHigh || item.class == dynamicPoolBalanceHigh
		}
		if !hasLow || !hasHigh {
			continue
		}
		for _, item := range items {
			key := item.candidate.ChoiceKey + "\x00" + item.candidate.AccountID
			if item.class == dynamicPoolBalanceLow {
				priorities[key] = 1
			} else if item.class == dynamicPoolBalanceHigh {
				priorities[key] = -1
			}
		}
		applied = true
	}
	// Case 2: every account in one target is high, while another target has at
	// least one certain low account. The same threshold applies to both targets;
	// choices were assembled by the API from one compatible model-routing tier.
	type targetSummary struct {
		allHigh bool
		hasLow  bool
	}
	summaries := make(map[string]targetSummary, len(choiceOrder))
	for _, choice := range choiceOrder {
		items := byChoice[choice]
		summary := targetSummary{allHigh: len(items) > 0}
		for _, item := range items {
			if item.class != dynamicPoolBalanceHigh {
				summary.allHigh = false
			}
			if item.class == dynamicPoolBalanceLow {
				summary.hasLow = true
			}
		}
		summaries[choice] = summary
	}
	case2Applied := false
	for _, highChoice := range choiceOrder {
		if !summaries[highChoice].allHigh {
			continue
		}
		for _, reliefChoice := range choiceOrder {
			if highChoice == reliefChoice || !summaries[reliefChoice].hasLow {
				continue
			}
			for _, item := range byChoice[highChoice] {
				priorities[item.candidate.ChoiceKey+"\x00"+item.candidate.AccountID] = -1
			}
			for _, item := range byChoice[reliefChoice] {
				if item.class == dynamicPoolBalanceLow {
					priorities[item.candidate.ChoiceKey+"\x00"+item.candidate.AccountID] = 1
				}
			}
			case2Applied = true
			break
		}
		if case2Applied {
			break
		}
	}
	if case2Applied {
		return priorities, "case2_target_relief"
	}
	if applied {
		return priorities, "case1_account_relief"
	}
	return priorities, "no_certain_relief"
}

// dynamicPoolBalancePriorities is kept small and pure so boundary tests can
// prove the interval and soft-priority semantics without constructing a Store.
func dynamicPoolBalancePriorities(candidates []dynamicPoolBalanceCandidate, threshold int64) map[string]int {
	priorities, _ := computeDynamicPoolBalancePriorities(candidates, threshold)
	return priorities
}

type dynamicPoolBalanceDecision struct {
	Priorities  map[string]int
	Applied     bool
	Reason      string
	SnapshotAge int64
}

func (s *Scheduler) recordDynamicPoolBalanceDecision(decision dynamicPoolBalanceDecision) {
	if s == nil {
		return
	}
	if decision.Applied {
		return
	}
	atomic.AddInt64(&s.metrics.DynamicPoolBalanceSkipped, 1)
	switch decision.Reason {
	case "unknown_interval":
		atomic.AddInt64(&s.metrics.DynamicPoolBalanceSkippedUnknownInterval, 1)
	case "snapshot_unavailable_or_stale":
		atomic.AddInt64(&s.metrics.DynamicPoolBalanceSkippedSnapshot, 1)
	}
}

func hasDynamicBalancePriority(requests []LeaseCandidateRequest) bool {
	for _, request := range requests {
		if request.BalancePriority != 0 {
			return true
		}
	}
	return false
}

func (s *Scheduler) dynamicPoolBalanceDecision(ctx context.Context, policy DynamicPoolBalancePolicy, evaluated []acrossEvaluatedCandidate, now int64) dynamicPoolBalanceDecision {
	decision := dynamicPoolBalanceDecision{}
	if !policy.Enabled {
		decision.Reason = "disabled"
		return decision
	}
	if policy.RPMThreshold <= 0 {
		decision.Reason = "invalid_threshold"
		return decision
	}
	if storage.NormalizeAgentClass(policy.AgentClass) != storage.AgentClassRoot {
		decision.Reason = "non_root"
		return decision
	}
	if policy.Bound || !policy.Fresh || !policy.OnlyAccountPoolTier {
		decision.Reason = "route_not_fresh_account_pool_tier"
		return decision
	}
	for _, item := range evaluated {
		if item.route.ServerSideState || item.route.ImmutableAffinity || item.route.RequiredAccountID != "" || item.route.RequiredEgressID != "" || item.route.FairScheduling {
			decision.Reason = "affinity_or_fair_scheduling"
			return decision
		}
	}
	snapshot, fresh := s.dynamicBalanceSnapshot(now)
	if snapshot == nil || !fresh {
		decision.Reason = "snapshot_unavailable_or_stale"
		if snapshot != nil && snapshot.SampledAt > 0 && now >= snapshot.SampledAt {
			decision.SnapshotAge = now - snapshot.SampledAt
		}
		return decision
	}
	if now >= snapshot.SampledAt {
		decision.SnapshotAge = now - snapshot.SampledAt
	}
	candidates := make([]dynamicPoolBalanceCandidate, 0, len(evaluated))
	for _, item := range evaluated {
		rate := s.dynamicBalanceRate(item.candidate.account.ID, now, snapshot)
		candidates = append(candidates, dynamicPoolBalanceCandidate{ChoiceKey: item.choiceKey, AccountID: item.candidate.account.ID, RootRPM: rate.RootRPM, UnknownRPM: rate.UnknownRPM})
	}
	priorities, reason := computeDynamicPoolBalancePriorities(candidates, policy.RPMThreshold)
	decision.Priorities, decision.Reason = priorities, reason
	for _, priority := range priorities {
		if priority != 0 {
			decision.Applied = true
			break
		}
	}
	return decision
}

func (s *Scheduler) dynamicBalanceSnapshot(now int64) (*DynamicPoolBalanceSnapshot, bool) {
	if s == nil || s.dynamicBalance == nil {
		return nil, false
	}
	return s.dynamicBalance.load(now)
}

func (s *Scheduler) dynamicBalanceRate(accountID string, now int64, snapshot *DynamicPoolBalanceSnapshot) storage.AccountRootRate {
	if s == nil || s.dynamicBalance == nil {
		return storage.AccountRootRate{}
	}
	return s.dynamicBalance.rate(accountID, now, snapshot)
}

func (s *Scheduler) reserveDynamicPoolBalance(ctx context.Context, policy DynamicPoolBalancePolicy, accountID string, now int64) {
	if s == nil || s.dynamicBalance == nil || !policy.Enabled {
		return
	}
	eventID := dynamicPoolBalanceEventIDFromContext(ctx, policy)
	s.dynamicBalance.reserve(accountID, eventID, now)
}

func egressRPMBalancePolicyActive(ctx context.Context, route Route, binding storage.AccountEgressBinding) (DynamicPoolBalancePolicy, bool) {
	policy, ok := dynamicPoolBalancePolicyFromContext(ctx)
	if !ok || !policy.Enabled || !policy.EgressRPMBalanceEnabled || policy.EgressRPMBalanceThreshold <= 0 ||
		!policy.Fresh || policy.Bound || !policy.OnlyAccountPoolTier ||
		storage.NormalizeAgentClass(policy.AgentClass) != storage.AgentClassRoot ||
		route.NoEgressFallback || route.ServerSideState || route.ImmutableAffinity || route.FairScheduling ||
		routing.IsTrueConversationAffinity(route.Affinity) ||
		strings.TrimSpace(route.RequiredAccountID) != "" || strings.TrimSpace(route.RequiredEgressID) != "" ||
		strings.EqualFold(strings.TrimSpace(binding.BindingScope), storage.EgressBindingScopeAccount) {
		return DynamicPoolBalancePolicy{}, false
	}
	ids := normalizeEgressIDs(policy.EgressRPMBalanceEgressIDs)
	if len(ids) == 0 {
		return DynamicPoolBalancePolicy{}, false
	}
	policy.EgressRPMBalanceEgressIDs = ids
	return policy, true
}

func normalizeEgressIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// orderedEgressRPMIDs keeps configured order for outlets below the threshold and
// moves saturated outlets behind them. If every outlet is saturated, the least
// loaded outlet is tried first; this preserves service while still spreading load.
func (s *Scheduler) orderedEgressRPMIDs(ctx context.Context, policy DynamicPoolBalancePolicy, route Route, binding storage.AccountEgressBinding, now int64) []string {
	ids := normalizeEgressIDs(policy.EgressRPMBalanceEgressIDs)
	if len(ids) < 2 {
		return ids
	}
	if s == nil || s.dynamicBalance == nil {
		return ids
	}
	snapshot, fresh := s.dynamicBalanceSnapshot(now)
	if snapshot == nil || !fresh {
		return ids
	}
	// Account-scoped bindings and stateful routes are never moved by this policy.
	if _, active := egressRPMBalancePolicyActive(ctx, route, binding); !active {
		return ids
	}
	type measured struct {
		id   string
		rate int64
		low  bool
		pos  int
	}
	measuredIDs := make([]measured, 0, len(ids))
	for pos, id := range ids {
		rate := s.dynamicBalance.egressRate(id, now, snapshot)
		measuredIDs = append(measuredIDs, measured{id: id, rate: rate, low: rate < policy.EgressRPMBalanceThreshold, pos: pos})
	}
	low := make([]measured, 0, len(measuredIDs))
	high := make([]measured, 0, len(measuredIDs))
	for _, item := range measuredIDs {
		if item.low {
			low = append(low, item)
		} else {
			high = append(high, item)
		}
	}
	if len(low) == 0 {
		sort.SliceStable(high, func(i, j int) bool {
			if high[i].rate != high[j].rate {
				return high[i].rate < high[j].rate
			}
			return high[i].pos < high[j].pos
		})
		low = high
		high = nil
	}
	ordered := make([]string, 0, len(ids))
	for _, item := range append(low, high...) {
		ordered = append(ordered, item.id)
	}
	return ordered
}

func (s *Scheduler) reserveEgressRPMBalance(ctx context.Context, policy DynamicPoolBalancePolicy, egressID string, now int64) {
	if s == nil || s.dynamicBalance == nil {
		return
	}
	eventID := dynamicPoolBalanceEventIDFromContext(ctx, policy)
	s.dynamicBalance.reserveEgress(egressID, eventID, now)
}

func (s *Scheduler) attachEgressRPMReservation(ctx context.Context, route Route, lease *Lease) {
	if lease == nil {
		return
	}
	policy, active := egressRPMBalancePolicyActive(ctx, route, lease.Binding)
	if !active {
		return
	}
	egressID := strings.TrimSpace(lease.Egress.ID)
	if egressID == "" {
		return
	}
	allowed := false
	for _, id := range policy.EgressRPMBalanceEgressIDs {
		if strings.EqualFold(strings.TrimSpace(id), egressID) {
			allowed = true
			break
		}
	}
	if !allowed || dynamicPoolBalanceEventIDFromContext(ctx, policy) == "" {
		return
	}
	s.reserveEgressRPMBalance(ctx, policy, egressID, storage.Now())
	previous := lease.dynamicRelease
	lease.dynamicRelease = func() {
		if previous != nil {
			previous()
		}
		s.releaseDynamicPoolBalanceReservation(ctx, policy)
	}
}

// ConfirmDynamicPoolBalanceArrival marks the request's reservation only after
// the durable logical-arrival insert succeeds. It is called by the API's
// pre-upstream attempt hook; selection itself never performs this DB write.
func (s *Scheduler) ConfirmDynamicPoolBalanceArrival(ctx context.Context, accountID string, egressIDs ...string) {
	if s == nil || s.dynamicBalance == nil {
		return
	}
	eventID := ""
	if ctx != nil {
		if value, ok := ctx.Value(dynamicPoolBalanceEventIDContextKey{}).(string); ok {
			eventID = strings.TrimSpace(value)
		}
	}
	if eventID == "" {
		if policy, ok := dynamicPoolBalancePolicyFromContext(ctx); ok {
			eventID = dynamicPoolBalanceEventIDFromContext(ctx, policy)
		}
	}
	s.dynamicBalance.confirm(accountID, eventID, egressIDs...)
}

func (s *Scheduler) releaseDynamicPoolBalanceReservation(ctx context.Context, policy DynamicPoolBalancePolicy) {
	if s == nil || s.dynamicBalance == nil {
		return
	}
	eventID := dynamicPoolBalanceEventIDFromContext(ctx, policy)
	s.dynamicBalance.releaseProvisional(eventID)
}

func sortedDynamicPriorityKeys(priorities map[string]int) []string {
	keys := make([]string, 0, len(priorities))
	for key := range priorities {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
