package scheduler

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

const (
	candidateIndexTTL       = time.Second
	candidateIndexRetention = 30 * time.Second
	candidateIndexMaxKeys   = 4096
)

type indexedCandidate struct {
	snapshot      storage.AccountWithEgress
	provider      string
	resolvedModel string
	bootstrap     bool
}

type routeCandidateIndex struct {
	key                 string
	selectionGeneration uint64
	candidates          []indexedCandidate
	staticCounters      NoAccountCounters
	builtAt             time.Time
}

func candidateIndexKey(route Route, cfg config.Config) string {
	allowlist := append([]string(nil), route.KiroEndpointAllowlist...)
	if route.KiroEndpointAllowlist == nil {
		allowlist = append(allowlist, cfg.KiroEndpointAllowlist...)
	}
	for i := range allowlist {
		allowlist[i] = strings.ToLower(strings.TrimSpace(allowlist[i]))
	}
	sort.Strings(allowlist)
	return strings.Join([]string{
		route.Group,
		routeProvidersKey(route),
		strings.TrimSpace(route.Model),
		strings.TrimSpace(route.KiroFallbackModel),
		strings.ToLower(strings.TrimSpace(route.ContextMode)),
		firstRouteValue(route.KiroDefaultRegion, cfg.KiroDefaultAPIRegion),
		strings.Join(allowlist, ","),
		strconv.FormatBool(route.ExplicitProvider),
		strconv.FormatBool(route.ThinkingRequired),
	}, "\x00")
}

func (s *Scheduler) candidateIndexSnapshot(ctx context.Context, selection *accountSelectionSnapshot, route Route) (*routeCandidateIndex, error) {
	key := candidateIndexKey(route, s.Config())
	now := time.Now()
	indexes, _ := s.candidateIndexes.Load().(map[string]*routeCandidateIndex)
	if index := indexes[key]; index != nil && index.selectionGeneration == selection.generation && now.Sub(index.builtAt) < candidateIndexTTL {
		return index, nil
	}
	s.candidateRefreshMu.Lock()
	defer s.candidateRefreshMu.Unlock()
	indexes, _ = s.candidateIndexes.Load().(map[string]*routeCandidateIndex)
	if index := indexes[key]; index != nil && index.selectionGeneration == selection.generation && time.Since(index.builtAt) < candidateIndexTTL {
		return index, nil
	}
	index, err := s.buildCandidateIndex(ctx, selection, route, key)
	if err != nil {
		return nil, err
	}
	atomic.AddInt64(&s.metrics.CandidateIndexBuilds, 1)
	next := make(map[string]*routeCandidateIndex, min(len(indexes)+1, candidateIndexMaxKeys))
	for existingKey, existing := range indexes {
		if existingKey == key || now.Sub(existing.builtAt) >= candidateIndexRetention || len(next) >= candidateIndexMaxKeys-1 {
			continue
		}
		next[existingKey] = existing
	}
	next[key] = index
	s.candidateIndexes.Store(next)
	return index, nil
}

func (s *Scheduler) buildCandidateIndex(ctx context.Context, selection *accountSelectionSnapshot, route Route, key string) (*routeCandidateIndex, error) {
	capabilitiesByAccount := map[string][]storage.ModelCapability{}
	if capabilityRouteModel(route.Model) {
		loaded, err := s.store.ListCapabilitiesSummaryByAccountIDs(ctx, selection.accountIDs)
		if err != nil {
			return nil, err
		}
		capabilitiesByAccount = loaded
		authority, err := s.store.ListModelCatalogAuthorityByAccountIDs(ctx, selection.accountIDs)
		if err != nil {
			return nil, err
		}
		for accountID, authoritative := range authority {
			if authoritative {
				capabilitiesByAccount[accountID] = append(capabilitiesByAccount[accountID], modelCatalogAuthorityMarker())
			}
		}
	}
	var capable map[string]bool
	if route.Model != "" {
		if loaded, err := s.modelsSnapshot(ctx, route.Group, route.Model, route.ContextMode); err == nil && len(loaded) > 0 {
			capable = loaded
		}
	}
	index := &routeCandidateIndex{key: key, selectionGeneration: selection.generation, candidates: make([]indexedCandidate, 0, len(selection.rows)), builtAt: time.Now()}
	for _, snapshot := range selection.rows {
		account := snapshot.Account
		provider := s.providerOfAccountCached(ctx, account)
		if !routeAllowsProvider(route, provider) {
			index.staticCounters.ProviderMismatch++
			continue
		}
		candidateRoute := routeForProviderModel(route, provider)
		resolvedModel := candidateRoute.Model
		bootstrap := false
		if provider == "kiro" && capabilityRouteModel(candidateRoute.Model) {
			var ok bool
			resolvedModel, bootstrap, ok = s.resolveKiroRouteModel(ctx, account, candidateRoute)
			if !ok {
				index.staticCounters.ModelUnsupported++
				continue
			}
		} else if provider == "claude" && capabilityRouteModel(candidateRoute.Model) {
			var ok bool
			resolvedModel, bootstrap, ok = resolveClaudeRouteModel(candidateRoute, capabilitiesByAccount[account.ID])
			if !ok {
				index.staticCounters.ModelUnsupported++
				continue
			}
		} else if provider == "codex" && capabilityRouteModel(candidateRoute.Model) {
			var ok bool
			resolvedModel, bootstrap, ok = resolveCodexRouteModel(candidateRoute, capabilitiesByAccount[account.ID])
			if !ok {
				index.staticCounters.ModelUnsupported++
				continue
			}
		} else if provider == "antigravity" && capabilityRouteModel(candidateRoute.Model) {
			var ok bool
			resolvedModel, ok = resolveAntigravityRouteModel(candidateRoute, capabilitiesByAccount[account.ID])
			if !ok {
				index.staticCounters.ModelUnsupported++
				continue
			}
		} else if capable != nil && !capable[account.ID] {
			index.staticCounters.ModelUnsupported++
			continue
		}
		index.candidates = append(index.candidates, indexedCandidate{snapshot: snapshot, provider: provider, resolvedModel: resolvedModel, bootstrap: bootstrap})
	}
	return index, nil
}

type candidateEvaluationContext struct {
	ctx              context.Context
	route            Route
	cfg              config.Config
	now              int64
	rateLimits       map[string][]storage.AccountRateLimit
	requestEgress    map[string]storage.EgressProfile
	egressCacheTime  time.Time
	egressCacheMutex *sync.RWMutex
	egressOutcomes   map[string]storage.CodexEgressRecentOutcome
}

func (s *Scheduler) newCandidateEvaluationContext(ctx context.Context, route Route, selection *accountSelectionSnapshot) candidateEvaluationContext {
	rateLimits, err := s.rateLimitsForSelection(ctx, selection)
	if err != nil {
		log.Printf("[SCHEDULER] batch rate-limit snapshot lookup failed: group=%s err=%v", route.Group, err)
	}
	s.egressCacheMutex.Lock()
	if time.Since(s.egressCacheTime) > s.egressCacheTTL {
		s.egressCache = sync.Map{}
		s.egressCacheTime = time.Now()
	}
	egTime := s.egressCacheTime
	egMutex := &s.egressCacheMutex
	s.egressCacheMutex.Unlock()
	return candidateEvaluationContext{ctx: ctx, route: route, cfg: s.Config(), now: storage.Now(), rateLimits: rateLimits, requestEgress: make(map[string]storage.EgressProfile), egressCacheTime: egTime, egressCacheMutex: egMutex}
}

func (s *Scheduler) evaluateIndexedCandidate(indexed indexedCandidate, evaluation *candidateEvaluationContext, counters *NoAccountCounters) (candidate, bool) {
	atomic.AddInt64(&s.metrics.CandidateEvaluations, 1)
	account := indexed.snapshot.Account
	if account.Status != "active" {
		counters.Inactive++
		return candidate{}, false
	}
	if account.QuarantineUntil > evaluation.now && !account.IgnoreRateLimitControls {
		counters.Quarantined++
		return candidate{}, false
	}
	candidateRoute := routeForProviderModel(evaluation.route, indexed.provider)
	goalQuotaGrace := evaluation.route.AllowCodexGoalQuotaGrace && indexed.provider == "codex"
	if _, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(evaluation.rateLimits[account.ID], indexed.provider, candidateRoute.Model, evaluation.now); limited && !account.IgnoreRateLimitControls && !goalQuotaGrace {
		counters.RateLimitCooldown++
		return candidate{}, false
	}
	binding := routeEgressBinding(indexed.snapshot.Binding, account.ID, evaluation.route.PreferredEgressIDs)
	if binding.RecheckPending && !account.IgnoreRateLimitControls {
		counters.RecheckPending++
		return candidate{}, false
	}
	ignoreTelemetryCooldown := account.IgnoreRateLimitControls || (goalQuotaGrace && !binding.RecheckPending)
	var egress storage.EgressProfile
	var egressLoad, sidecarLoad int
	var ok bool
	if indexed.provider == "codex" {
		if evaluation.egressOutcomes == nil {
			evaluation.egressOutcomes = s.recentCodexEgressOutcomes(evaluation.ctx)
		}
		var capacityBlocked bool
		egress, egressLoad, sidecarLoad, capacityBlocked, ok = s.selectFreshEgressWithCache(evaluation.ctx, binding, evaluation.now, ignoreTelemetryCooldown, &evaluation.requestEgress, evaluation.egressCacheMutex, evaluation.egressOutcomes)
		if !ok {
			if capacityBlocked {
				counters.Concurrency++
			} else if s.egressTemporarilyUnavailable(evaluation.ctx, binding, evaluation.now, ignoreTelemetryCooldown) {
				counters.EgressCooldown++
			} else {
				counters.EgressUnavailable++
			}
			return candidate{}, false
		}
		// Codex fresh work may start on any pool outlet.  Rotate only the lease's
		// retry order and cookie namespace; the stored account binding remains
		// untouched and an existing CPA RequiredEgressID never reaches this path.
		binding = selectedEgressBinding(binding, account.ID, egress.ID)
	} else {
		// Other providers retain their established ordered primary/standby
		// semantics.  Codex transport observations and fresh-pool balancing must
		// not silently turn a Claude/Kiro/custom standby into an active outlet.
		egress, ok = s.selectEgressWithCache(evaluation.ctx, binding, evaluation.now, account.IgnoreRateLimitControls, &evaluation.requestEgress, evaluation.egressCacheMutex, &evaluation.egressCacheTime)
		if !ok {
			if s.egressTemporarilyUnavailable(evaluation.ctx, binding, evaluation.now, account.IgnoreRateLimitControls) {
				counters.EgressCooldown++
			} else {
				counters.EgressUnavailable++
			}
			return candidate{}, false
		}
		egress, ok = s.applyBoundSidecarWithCache(evaluation.ctx, binding, egress, evaluation.now, &evaluation.requestEgress, evaluation.egressCacheMutex)
		if !ok {
			counters.EgressUnavailable++
			return candidate{}, false
		}
		egressLoad, sidecarLoad = s.currentEgressLoads(egress)
		if concurrencyLimited(egress.MaxConcurrency, egressLoad) ||
			(strings.TrimSpace(egress.TransportSidecarID) != "" && concurrencyLimited(egress.TransportSidecarMaxConcurrency, sidecarLoad)) {
			counters.Concurrency++
			return candidate{}, false
		}
		if len(evaluation.route.PreferredEgressIDs) > 0 {
			binding.CookieJarKey = account.ID + ":" + egress.ID
		}
	}
	if evaluation.route.Exclude[account.ID] {
		counters.Excluded++
		return candidate{}, false
	}
	inflight, tokens := s.currentLoad(account.ID)
	if tokenBudgetLimited(evaluation.cfg.AccountTokenBudget, evaluation.route.Compaction, inflight, tokens, evaluation.route.EstimatedTokens) {
		counters.TokenBudget++
		return candidate{}, false
	}
	bootstrap := indexed.bootstrap
	if evaluation.route.FairScheduling && indexed.provider == "kiro" && capability.KiroSupportsGPTModel(candidateRoute.Model) {
		bootstrap = false
	}
	concurrencyLoad := float64(inflight) + normalizedLoad(egressLoad, egress.MaxConcurrency) + normalizedLoad(sidecarLoad, egress.TransportSidecarMaxConcurrency)
	tokenLoad := normalizedTokenLoad(tokens, evaluation.cfg.AccountTokenBudget)
	latencyPenalty := float64(maxInt64(0, egress.LatencyMillis)) / 100000.0
	return candidate{account: account, egress: egress, binding: binding, resolvedModel: indexed.resolvedModel, bootstrap: bootstrap, score: concurrencyLoad + tokenLoad + latencyPenalty}, true
}

func (s *Scheduler) candidateSampleIndexes(route Route, size int) ([3]int, int) {
	var indexes [3]int
	if size <= 3 {
		for i := 0; i < size; i++ {
			indexes[i] = i
		}
		return indexes, size
	}
	count := 2
	var seed uint64
	if !route.FairScheduling && route.Affinity.Hash != "" {
		count = 3
		seed = rendezvous(route.Affinity.Hash, "candidate-table")
	} else {
		s.mu.Lock()
		s.rr++
		seed = uint64(s.rr)
		s.mu.Unlock()
	}
	written := 0
	for written < count {
		seed = xorshift64(seed)
		index := int(seed % uint64(size))
		duplicate := false
		for i := 0; i < written; i++ {
			if indexes[i] == index {
				duplicate = true
				break
			}
		}
		if !duplicate {
			indexes[written] = index
			written++
		}
	}
	return indexes, written
}

func (s *Scheduler) sortCandidateChoices(candidates []candidate, rotateEquals bool) {
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && candidateLess(candidates[j], candidates[j-1]); j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}
	if !rotateEquals || len(candidates) < 2 {
		return
	}
	nEq := 1
	for nEq < len(candidates) && !candidateLess(candidates[0], candidates[nEq]) && !candidateLess(candidates[nEq], candidates[0]) {
		nEq++
	}
	if nEq < 2 {
		return
	}
	s.mu.Lock()
	start := s.rr % nEq
	s.rr++
	s.mu.Unlock()
	rotated := append([]candidate(nil), candidates[start:nEq]...)
	rotated = append(rotated, candidates[:start]...)
	copy(candidates[:nEq], rotated)
}

func insertBestCandidate(best []candidate, item candidate) []candidate {
	best = append(best, item)
	for i := len(best) - 1; i > 0 && candidateLess(best[i], best[i-1]); i-- {
		best[i], best[i-1] = best[i-1], best[i]
	}
	if len(best) > 3 {
		best = best[:3]
	}
	return best
}

func (s *Scheduler) tryCandidateChoices(ctx context.Context, route Route, choices []candidate, counters *NoAccountCounters) (Lease, bool) {
	for _, choice := range choices {
		if lease, reason, ok := s.tryLeaseAccountFromCandidate(ctx, choice, route); ok {
			return lease, true
		} else if reason == leaseBlockTokenBudget {
			counters.TokenBudget++
		} else if reason == leaseBlockCoordinator {
			counters.Coordinator++
		} else {
			counters.Concurrency++
		}
	}
	return Lease{}, false
}

func (s *Scheduler) selectFreshIndexed(ctx context.Context, route Route) (Lease, error) {
	selection, err := s.accountsSnapshot(ctx, route.Group)
	if err != nil {
		return Lease{}, err
	}
	cfg := s.Config()
	var index *routeCandidateIndex
	if cfg.SchedulerIndexEnabled {
		index, err = s.candidateIndexSnapshot(ctx, selection, route)
	} else {
		index, err = s.buildCandidateIndex(ctx, selection, route, candidateIndexKey(route, cfg))
	}
	if err != nil {
		return Lease{}, err
	}
	counters := index.staticCounters
	if len(index.candidates) == 0 {
		return Lease{}, s.noAccountError(route, counters)
	}
	evaluation := s.newCandidateEvaluationContext(ctx, route, selection)
	if !cfg.SchedulerIndexEnabled {
		return s.selectFreshFullScan(ctx, route, index, &evaluation, counters)
	}
	sampleIndexes, sampleCount := s.candidateSampleIndexes(route, len(index.candidates))
	choices := make([]candidate, 0, sampleCount)
	for _, candidateIndex := range sampleIndexes[:sampleCount] {
		if choice, ok := s.evaluateIndexedCandidate(index.candidates[candidateIndex], &evaluation, &counters); ok {
			choices = append(choices, choice)
		}
	}
	s.sortCandidateChoices(choices, len(index.candidates) <= 3)
	if lease, ok := s.tryCandidateChoices(ctx, route, choices, &counters); ok {
		return lease, nil
	}
	if len(index.candidates) <= 3 {
		return Lease{}, s.noAccountError(route, counters)
	}
	atomic.AddInt64(&s.metrics.CandidateFallbacks, 1)
	counters = index.staticCounters
	best := make([]candidate, 0, 3)
	for _, indexed := range index.candidates {
		if choice, ok := s.evaluateIndexedCandidate(indexed, &evaluation, &counters); ok {
			best = insertBestCandidate(best, choice)
		}
	}
	s.sortCandidateChoices(best, true)
	if lease, ok := s.tryCandidateChoices(ctx, route, best, &counters); ok {
		return lease, nil
	}
	return Lease{}, s.noAccountError(route, counters)
}

func (s *Scheduler) selectFreshFullScan(ctx context.Context, route Route, index *routeCandidateIndex, evaluation *candidateEvaluationContext, counters NoAccountCounters) (Lease, error) {
	best := make([]candidate, 0, 3)
	for _, indexed := range index.candidates {
		if choice, ok := s.evaluateIndexedCandidate(indexed, evaluation, &counters); ok {
			best = insertBestCandidate(best, choice)
		}
	}
	s.sortCandidateChoices(best, true)
	if lease, ok := s.tryCandidateChoices(ctx, route, best, &counters); ok {
		return lease, nil
	}
	return Lease{}, s.noAccountError(route, counters)
}
