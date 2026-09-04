package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// RouteChoice is one same-tier routing target. ChoiceKey is opaque to the
// scheduler and is returned unchanged with the physical-account lease.
type RouteChoice struct {
	ChoiceKey string
	Route     Route
}

// RoutedLease identifies both the actual account lease and the target whose
// route admitted it. Lease is embedded for source-compatible Account/Egress use.
type RoutedLease struct {
	Lease
	ChoiceKey string
}

type acrossEvaluatedCandidate struct {
	choiceKey string
	route     Route
	candidate candidate
	pending   int
}

// acrossTrialCandidate is an intelligent-routing cooldown trial candidate found
// while evaluating one across-choice. When every choice came up empty because
// its accounts were cooldown-stuck, SelectAcross probes the soonest-recovering
// few instead of failing the whole request (see tryAcrossCooldownTrials).
type acrossTrialCandidate struct {
	choiceKey     string
	route         Route
	candidate     candidate
	cooldownUntil int64
}

// SelectAcross evaluates every exact-capability account in the supplied tier,
// merges duplicate physical account IDs, and lets the coordinator choose+lease
// in one atomic operation. Callers preserve strict tier priority by invoking it
// once per tier rather than combining fallback tiers.
func (s *Scheduler) SelectAcross(ctx context.Context, choices []RouteChoice) (RoutedLease, error) {
	started := time.Now()
	defer s.observeRouteSelection(started)
	if len(choices) == 0 {
		return RoutedLease{}, ErrNoAccount
	}
	if err := s.admission.Wait(ctx); err != nil {
		return RoutedLease{}, err
	}

	normalized := make([]RouteChoice, 0, len(choices))
	egressPolicy, hasEgressPolicy := dynamicPoolBalancePolicyFromContext(ctx)
	var egressOrder []string
	if hasEgressPolicy && egressPolicy.Enabled && egressPolicy.EgressRPMBalanceEnabled &&
		egressPolicy.EgressRPMBalanceThreshold > 0 && s.dynamicBalance != nil {
		egressOrder = s.orderedEgressRPMIDs(ctx, egressPolicy, Route{}, storage.AccountEgressBinding{}, storage.Now())
	}
	seenChoices := make(map[string]struct{}, len(choices))
	for index, choice := range choices {
		choice.ChoiceKey = strings.TrimSpace(choice.ChoiceKey)
		if choice.ChoiceKey == "" {
			choice.ChoiceKey = fmt.Sprintf("choice:%d", index)
		}
		if _, duplicate := seenChoices[choice.ChoiceKey]; duplicate {
			return RoutedLease{}, fmt.Errorf("duplicate route choice key %q", choice.ChoiceKey)
		}
		if hasEgressPolicy && egressPolicy.Enabled && egressPolicy.EgressRPMBalanceEnabled && egressPolicy.Fresh && !egressPolicy.Bound && egressPolicy.OnlyAccountPoolTier && storage.NormalizeAgentClass(egressPolicy.AgentClass) == storage.AgentClassRoot && len(egressPolicy.EgressRPMBalanceEgressIDs) > 0 && !choice.Route.ImmutableAffinity && choice.Route.RequiredEgressID == "" && !choice.Route.ServerSideState && !choice.Route.FairScheduling {
			if len(egressOrder) == 0 {
				egressOrder = egressPolicy.EgressRPMBalanceEgressIDs
			}
			choice.Route.PreferredEgressIDs = append([]string(nil), egressOrder...)
		}
		seenChoices[choice.ChoiceKey] = struct{}{}
		if err := s.normalizeAcrossRoute(ctx, &choice.Route); err != nil {
			return RoutedLease{}, err
		}
		normalized = append(normalized, choice)
	}

	// A real conversation reuses its already-established physical account. Prefix
	// and coarse affinities deliberately bypass this path and remain freshly fair.
	trueAffinity := routing.AffinityKey{}
	for _, choice := range normalized {
		if routing.IsTrueConversationAffinity(choice.Route.Affinity) {
			trueAffinity = choice.Route.Affinity
			break
		}
	}
	if trueAffinity.Hash != "" {
		if bound, err := s.affinitySnapshot(ctx, trueAffinity.Hash); err == nil {
			account, accountErr := s.store.GetAccount(ctx, bound.AccountID)
			if accountErr == nil {
				provider := s.providerOfAccount(ctx, account)
				for _, choice := range normalized {
					if choice.Route.Group != account.GroupName || !routeAllowsProvider(choice.Route, provider) {
						continue
					}
					lease, selectErr := s.Select(ctx, choice.Route)
					if selectErr == nil {
						return RoutedLease{Lease: lease, ChoiceKey: choice.ChoiceKey}, nil
					}
					if choice.Route.ServerSideState || choice.Route.ImmutableAffinity {
						return RoutedLease{}, selectErr
					}
				}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return RoutedLease{}, err
		}
	}

	evaluated, trials, counters, poolSize, err := s.evaluateAcrossCandidates(ctx, normalized)
	if err != nil {
		return RoutedLease{}, err
	}
	if len(evaluated) == 0 {
		// A cooldown-only empty result is a stale-quota probe opportunity, not a
		// hard miss: probe the soonest-recovering accounts before concluding the
		// tier is unavailable.
		if lease, ok := s.tryAcrossCooldownTrials(ctx, trials, counters); ok {
			return lease, nil
		}
		return RoutedLease{}, &NoAccountError{Group: acrossGroups(normalized), Model: normalized[0].Route.Model, Counters: counters, EmptyPool: poolSize == 0}
	}
	policy, policyPresent := dynamicPoolBalancePolicyFromContext(ctx)
	dynamicDecision := dynamicPoolBalanceDecision{}
	if policyPresent && policy.Enabled && policy.RPMThreshold > 0 {
		dynamicDecision = s.dynamicPoolBalanceDecision(ctx, policy, evaluated, storage.Now())
		s.recordDynamicPoolBalanceDecision(dynamicDecision)
	}

	requests := make([]LeaseCandidateRequest, 0, len(evaluated))
	lookup := make(map[string]acrossEvaluatedCandidate, len(evaluated))
	cfg := s.Config()
	for _, item := range evaluated {
		key := item.choiceKey + "\x00" + item.candidate.account.ID
		if _, duplicate := lookup[key]; duplicate {
			continue
		}
		lookup[key] = item
		balancePriority := 0
		if dynamicDecision.Priorities != nil {
			balancePriority = dynamicDecision.Priorities[key]
		}
		requests = append(requests, LeaseCandidateRequest{
			ChoiceKey: item.choiceKey, Request: leaseRequestForCandidate(item.candidate, item.route, cfg),
			AccountScore: item.candidate.score, Pending: item.pending, BalancePriority: balancePriority,
		})
	}
	if len(requests) == 0 {
		return RoutedLease{}, &NoAccountError{Group: acrossGroups(normalized), Model: normalized[0].Route.Model, Counters: counters, EmptyPool: poolSize == 0}
	}

	selection, reason, acquireErr := s.acquireAcross(ctx, requests)
	if acquireErr != nil {
		return RoutedLease{}, acquireErr
	}
	if selection.Lease == nil {
		if reason == leaseBlockTokenBudget {
			counters.TokenBudget++
		} else if reason == leaseBlockCoordinator {
			counters.Coordinator++
		} else {
			counters.Concurrency++
		}
		return RoutedLease{}, &NoAccountError{Group: acrossGroups(normalized), Model: normalized[0].Route.Model, Counters: counters}
	}
	item, found := lookup[selection.ChoiceKey+"\x00"+selection.AccountID]
	if !found {
		_ = selection.Lease.Release(context.Background())
		return RoutedLease{}, fmt.Errorf("coordinator selected unknown route/account pair %q/%q", selection.ChoiceKey, selection.AccountID)
	}
	lease := s.activateCoordinatedCandidate(item.candidate, item.route, selection.Lease)
	var balanceReleases []func()
	if dynamicDecision.Applied {
		now := storage.Now()
		s.reserveDynamicPoolBalance(ctx, policy, lease.Account.ID, now)
		balanceReleases = append(balanceReleases, func() { s.releaseDynamicPoolBalanceReservation(ctx, policy) })
		atomic.AddInt64(&s.metrics.DynamicPoolBalanceSelections, 1)
		if len(normalized) > 1 {
			atomic.AddInt64(&s.metrics.DynamicPoolBalanceTargetSelections, 1)
		}
	}
	if egressPolicy, active := egressRPMBalancePolicyActive(ctx, item.route, lease.Binding); active {
		egressID := strings.TrimSpace(lease.Egress.ID)
		if egressID != "" {
			s.reserveEgressRPMBalance(ctx, egressPolicy, egressID, storage.Now())
			balanceReleases = append(balanceReleases, func() { s.releaseDynamicPoolBalanceReservation(ctx, egressPolicy) })
		}
	}
	if len(balanceReleases) > 0 {
		lease.dynamicRelease = func() {
			for _, release := range balanceReleases {
				fn := release
				fn()
			}
		}
	}
	if trueAffinity.Hash != "" {
		stored, bindErr := s.upsertAffinityResult(ctx, storage.AffinityBinding{
			RouteKeyHash: trueAffinity.Hash, RouteKey: trueAffinity.Key, Source: trueAffinity.Source,
			AccountID: lease.Account.ID, Provider: s.providerOfAccount(ctx, lease.Account),
			Model: firstRouteValue(lease.ResolvedModel, item.route.Model), EgressID: lease.Egress.ID,
		})
		if bindErr == nil {
			lease.RouteEpoch = stored.Epoch
		}
	}
	return RoutedLease{Lease: lease, ChoiceKey: selection.ChoiceKey}, nil
}

func (s *Scheduler) normalizeAcrossRoute(ctx context.Context, route *Route) error {
	if route.NoEgressFallback {
		route.ImmutableAffinity = true
	}
	if len(route.PreferredEgressIDs) > 0 {
		if _, active := egressRPMBalancePolicyActive(ctx, *route, storage.AccountEgressBinding{}); active {
			return nil
		}
		// PreferredEgressIDs is a legacy caller hint and cannot override the
		// account-pool group's primary outlet unless the explicit user-group RPM
		// policy is active.
		route.PreferredEgressIDs = nil
	}
	if route.Group == "" {
		route.Group = s.Config().DefaultGroup
	}
	egressID, err := s.groupPrimaryEgressID(ctx, route.Group)
	if err != nil {
		return err
	}
	route.PreferredEgressIDs = []string{egressID}
	return nil
}

func (s *Scheduler) evaluateAcrossCandidates(ctx context.Context, choices []RouteChoice) ([]acrossEvaluatedCandidate, []acrossTrialCandidate, NoAccountCounters, int, error) {
	all := make([]acrossEvaluatedCandidate, 0)
	trials := make([]acrossTrialCandidate, 0)
	counters := NoAccountCounters{}
	// poolSize is the aggregate raw account count across every choice. All-zero
	// means every target group is itself empty — a configuration error no skip
	// counter can express — and SelectAcross reports that as EmptyPool so the
	// audit reason reads group_has_no_accounts rather than looking like load.
	poolSize := 0
	// Stable ordering keeps coordinator inputs reproducible; fairness comes from
	// the coordinator's single global round-robin, not map iteration.
	sort.SliceStable(choices, func(i, j int) bool { return choices[i].ChoiceKey < choices[j].ChoiceKey })
	for _, choice := range choices {
		selection, err := s.accountsSnapshot(ctx, choice.Route.Group)
		if err != nil {
			return nil, nil, counters, 0, err
		}
		index, err := s.candidateIndexSnapshot(ctx, selection, choice.Route)
		if err != nil {
			return nil, nil, counters, 0, err
		}
		poolSize += index.poolSize
		addNoAccountCounters(&counters, index.staticCounters)
		evaluation := s.newCandidateEvaluationContext(ctx, choice.Route, selection)
		pending := s.queuedForRoute(choice.Route)
		for _, indexed := range index.candidates {
			candidateCounters := NoAccountCounters{}
			item, ok := s.evaluateIndexedCandidate(indexed, &evaluation, &candidateCounters)
			addNoAccountCounters(&counters, candidateCounters)
			if !ok {
				// Only a cooldown-stuck candidate is trial-eligible; everything else
				// (quarantine, recheck, egress down, capacity, token) must not probe.
				if trial, until, trialOK := s.evaluateIndexedTrialCandidate(indexed, &evaluation); trialOK {
					trials = append(trials, acrossTrialCandidate{choiceKey: choice.ChoiceKey, route: choice.Route, candidate: trial, cooldownUntil: until})
				}
				continue
			}
			all = append(all, acrossEvaluatedCandidate{choiceKey: choice.ChoiceKey, route: choice.Route, candidate: item, pending: pending})
		}
	}
	return all, trials, counters, poolSize, nil
}

// tryAcrossCooldownTrials probes intelligent-routing cooldown trials across every
// empty across-choice. It engages only when cooldown-class counters fired and each
// probe is still gated by the coordinator, exactly like the single-route trial in
// tryCooldownTrial. A successful probe returns a RoutedLease carrying the probed
// candidate's choice key.
func (s *Scheduler) tryAcrossCooldownTrials(ctx context.Context, trials []acrossTrialCandidate, counters NoAccountCounters) (RoutedLease, bool) {
	if !s.Config().IntelligentRoutingEnabled {
		return RoutedLease{}, false
	}
	if counters.RateLimitCooldown == 0 && counters.EgressCooldown == 0 {
		return RoutedLease{}, false
	}
	if len(trials) == 0 {
		return RoutedLease{}, false
	}
	sort.SliceStable(trials, func(i, j int) bool { return trials[i].cooldownUntil < trials[j].cooldownUntil })
	if len(trials) > maxCooldownTrialAttempts {
		trials = trials[:maxCooldownTrialAttempts]
	}
	for _, trial := range trials {
		if cooldownTrialsRestrictedToOverrides(ctx) && !trial.candidate.account.IgnoreRateLimitControls {
			continue
		}
		lease, _, ok := s.tryLeaseAccountFromCandidate(ctx, trial.candidate, trial.route)
		if !ok {
			continue
		}
		atomic.AddInt64(&s.metrics.TrialSelections, 1)
		return RoutedLease{Lease: lease, ChoiceKey: trial.choiceKey}, true
	}
	return RoutedLease{}, false
}

func addNoAccountCounters(dst *NoAccountCounters, src NoAccountCounters) {
	dst.Inactive += src.Inactive
	dst.Quarantined += src.Quarantined
	dst.Excluded += src.Excluded
	dst.ProviderMismatch += src.ProviderMismatch
	dst.ModelUnsupported += src.ModelUnsupported
	dst.RateLimitCooldown += src.RateLimitCooldown
	dst.RecheckPending += src.RecheckPending
	dst.EgressUnavailable += src.EgressUnavailable
	dst.EgressCooldown += src.EgressCooldown
	dst.Concurrency += src.Concurrency
	dst.TokenBudget += src.TokenBudget
	dst.Coordinator += src.Coordinator
}

func acrossGroups(choices []RouteChoice) string {
	groups := make([]string, 0, len(choices))
	seen := map[string]struct{}{}
	for _, choice := range choices {
		if _, ok := seen[choice.Route.Group]; ok {
			continue
		}
		seen[choice.Route.Group] = struct{}{}
		groups = append(groups, choice.Route.Group)
	}
	sort.Strings(groups)
	return strings.Join(groups, ",")
}

func leaseRequestForCandidate(item candidate, route Route, cfg config.Config) LeaseRequest {
	resources := []LeaseResource{{ID: item.egress.ID, Limit: item.egress.MaxConcurrency}}
	if sidecarID := strings.TrimSpace(item.egress.TransportSidecarID); sidecarID != "" && sidecarID != item.egress.ID {
		resources = append(resources, LeaseResource{ID: sidecarID, Limit: item.egress.TransportSidecarMaxConcurrency})
	}
	ttl := 2 * cfg.RequestTimeout()
	if ttl < 2*time.Minute {
		ttl = 2 * time.Minute
	}
	return LeaseRequest{AccountID: item.account.ID, EstimatedTokens: route.EstimatedTokens, TokenBudget: cfg.AccountTokenBudget, Compaction: route.Compaction, Resources: resources, TTL: ttl}
}

func (s *Scheduler) acquireAcross(ctx context.Context, requests []LeaseCandidateRequest) (CoordinatedLeaseSelection, leaseBlockReason, error) {
	if coordinator, ok := s.coordinator.(MultiLeaseCoordinator); ok {
		return coordinator.TryAcquireAcross(ctx, requests)
	}
	// Compatibility for injected legacy coordinators: serialize the best-effort
	// scan so one process cannot make two non-atomic fresh choices concurrently.
	s.acrossMu.Lock()
	defer s.acrossMu.Unlock()
	ordered := append([]LeaseCandidateRequest(nil), requests...)
	// An injected legacy coordinator has no cross-target priority axis. Preserve
	// the soft overlay by trying relief candidates first, while leaving the
	// historical request order untouched when every priority is zero.
	if hasDynamicBalancePriority(ordered) {
		sort.SliceStable(ordered, func(i, j int) bool {
			return ordered[i].BalancePriority > ordered[j].BalancePriority
		})
	}
	for _, request := range ordered {
		lease, reason, err := s.coordinator.TryAcquire(ctx, request.Request)
		if err != nil {
			return CoordinatedLeaseSelection{}, reason, err
		}
		if lease != nil {
			return CoordinatedLeaseSelection{Lease: lease, ChoiceKey: request.ChoiceKey, AccountID: request.Request.AccountID}, leaseBlockNone, nil
		}
	}
	return CoordinatedLeaseSelection{}, leaseBlockConcurrency, nil
}
