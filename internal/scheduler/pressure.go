package scheduler

import (
	"context"
	"log"
	"math"
	"strings"

	"codex-account-pool/internal/storage"
)

const (
	// DefaultKiroSpilloverMinAvailable is the number of immediately schedulable
	// accounts below which auto routing should allow Kiro to take GPT traffic.
	DefaultKiroSpilloverMinAvailable = 2
	// DefaultKiroSpilloverPressurePercent is the group downstream-pressure
	// threshold for the same auto-routing decision.
	DefaultKiroSpilloverPressurePercent = 50.0
)

// ProviderPressureSnapshot is a point-in-time, process-local view of a single
// group/provider/model route. EligibleAccounts have passed the same persistent
// selection checks as Select (status, model, quota, recheck and egress). Available
// accounts additionally have capacity for a new lease right now.
//
// PressurePercent treats every eligible account as one scheduling unit and is
// (in_flight + queued) / eligible * 100. It deliberately includes queued requests:
// a group with a growing downstream queue is under pressure even before all of its
// egress limits have been reached. The value may be above 100 when the queue grows.
type ProviderPressureSnapshot struct {
	Group             string  `json:"group"`
	Provider          string  `json:"provider"`
	Model             string  `json:"model"`
	EligibleAccounts  int     `json:"eligible_accounts"`
	AvailableAccounts int     `json:"available_accounts"`
	SaturatedAccounts int     `json:"saturated_accounts"`
	InFlight          int     `json:"in_flight"`
	Queued            int     `json:"queued"`
	PressurePercent   float64 `json:"pressure_percent"`
}

// ShouldSpillover reports whether a caller should expand an auto route to its
// fallback provider. Non-positive arguments select the GPT/Kiro defaults.
func (p ProviderPressureSnapshot) ShouldSpillover(minAvailable int, pressurePercent float64) bool {
	if minAvailable <= 0 {
		minAvailable = DefaultKiroSpilloverMinAvailable
	}
	if pressurePercent <= 0 {
		pressurePercent = DefaultKiroSpilloverPressurePercent
	}
	return p.AvailableAccounts < minAvailable || p.PressurePercent > pressurePercent
}

// ShouldAdmitKiroFairly applies the GPT auto-mode admission policy. Kiro is never
// a priority override: when admitted it joins Codex in the scheduler's fair pool.
// It is admitted for downstream pressure strictly above 50%, or for the low-capacity
// case only while pressure remains strictly below 50%. The exact 50% boundary leaves
// the route on the native Codex pool.
func (p ProviderPressureSnapshot) ShouldAdmitKiroFairly() bool {
	threshold := DefaultKiroSpilloverPressurePercent
	if p.PressurePercent > threshold {
		return true
	}
	return p.AvailableAccounts < DefaultKiroSpilloverMinAvailable && p.PressurePercent < threshold
}

// ShouldSpillToKiro is retained for callers built against the original name. The
// current product behavior is fair-pool admission rather than a Kiro-priority spill.
func (p ProviderPressureSnapshot) ShouldSpillToKiro() bool {
	return p.ShouldAdmitKiroFairly()
}

// ProviderPressureSnapshot returns the availability and downstream-pressure view
// for group+provider+model without acquiring a lease. It intentionally shares the
// scheduler's cached snapshots and the same persistent eligibility conditions as
// selectFresh. The only extra work is counting all candidates rather than choosing
// one of them.
func (s *Scheduler) ProviderPressureSnapshot(ctx context.Context, group, provider, model string) (ProviderPressureSnapshot, error) {
	route := Route{Group: strings.TrimSpace(group), Provider: strings.TrimSpace(provider), Model: strings.TrimSpace(model)}
	if route.Group == "" {
		route.Group = s.Config().DefaultGroup
	}
	out := ProviderPressureSnapshot{Group: route.Group, Provider: route.Provider, Model: route.Model}

	accountsWithEgress, err := s.accountsSnapshot(ctx, route.Group)
	if err != nil {
		return out, err
	}
	accountIDs := make([]string, 0, len(accountsWithEgress))
	for _, awe := range accountsWithEgress {
		accountIDs = append(accountIDs, awe.Account.ID)
	}

	capabilitiesByAccount := map[string][]storage.ModelCapability{}
	if capabilityRouteModel(route.Model) {
		capabilitiesByAccount, err = s.store.ListCapabilitiesSummaryByAccountIDs(ctx, accountIDs)
		if err != nil {
			return out, err
		}
		authority, authorityErr := s.store.ListModelCatalogAuthorityByAccountIDs(ctx, accountIDs)
		if authorityErr != nil {
			return out, authorityErr
		}
		for accountID, authoritative := range authority {
			if authoritative {
				capabilitiesByAccount[accountID] = append(capabilitiesByAccount[accountID], modelCatalogAuthorityMarker())
			}
		}
	}

	rateLimitsByAccount, rateLimitsErr := s.rateLimitsSnapshot(ctx, route.Group, accountIDs)
	if rateLimitsErr != nil {
		// Keep Select's historical fail-open behavior when diagnostics storage is
		// unavailable; this view must never make auto routing more restrictive.
		log.Printf("[SCHEDULER] group pressure rate-limit lookup failed: group=%s err=%v", route.Group, rateLimitsErr)
		rateLimitsByAccount = nil
	}

	var capable map[string]bool
	if route.Model != "" {
		if models, modelErr := s.modelsSnapshot(ctx, route.Group, route.Model, route.ContextMode); modelErr == nil && len(models) > 0 {
			capable = models
		}
	}

	now := storage.Now()
	egCache := make(map[string]storage.EgressProfile)
	cfg := s.Config()
	for _, awe := range accountsWithEgress {
		account, binding := awe.Account, awe.Binding
		if account.Status != "active" || (account.QuarantineUntil > now && !account.IgnoreRateLimitControls) {
			continue
		}
		accountProvider := s.providerOfAccountCached(ctx, account)
		if !routeAllowsProvider(route, accountProvider) {
			continue
		}
		candidateRoute := routeForProviderModel(route, accountProvider)

		if !s.pressureModelEligible(ctx, account, accountProvider, candidateRoute, capabilitiesByAccount, capable) {
			continue
		}
		if _, limited := storage.AccountRateLimitCooldownUntilFromSnapshots(rateLimitsByAccount[account.ID], accountProvider, candidateRoute.Model, now); limited && !account.IgnoreRateLimitControls {
			continue
		}
		if binding.RecheckPending && !account.IgnoreRateLimitControls {
			continue
		}

		egress, ok := s.selectEgress(ctx, binding, now, egCache, account.IgnoreRateLimitControls)
		if !ok {
			continue
		}
		egress, ok = s.applyBoundSidecar(ctx, binding, egress, now, egCache)
		if !ok {
			continue
		}

		out.EligibleAccounts++
		inflight, tokens := s.currentLoad(account.ID)
		out.InFlight += inflight
		egressLoad := s.currentEgressLoad(egress.ID)
		egressSaturated := concurrencyLimited(egress.MaxConcurrency, egressLoad)
		if sidecarID := strings.TrimSpace(egress.TransportSidecarID); sidecarID != "" {
			egressSaturated = egressSaturated || concurrencyLimited(egress.TransportSidecarMaxConcurrency, s.currentEgressLoad(sidecarID))
		}
		if egressSaturated || tokenBudgetLimited(cfg.AccountTokenBudget, route.Compaction, inflight, tokens, route.EstimatedTokens) {
			out.SaturatedAccounts++
			continue
		}
		out.AvailableAccounts++
	}

	out.Queued = s.queuedForRoute(route)
	if out.EligibleAccounts > 0 {
		out.PressurePercent = math.Round(float64(out.InFlight+out.Queued)*10000/float64(out.EligibleAccounts)) / 100
	}
	return out, nil
}

// pressureModelEligible is the model portion of selectFresh's candidate filter.
func (s *Scheduler) pressureModelEligible(ctx context.Context, account storage.Account, provider string, route Route, capabilitiesByAccount map[string][]storage.ModelCapability, capable map[string]bool) bool {
	if !capabilityRouteModel(route.Model) {
		return true
	}
	switch provider {
	case "kiro":
		_, _, ok := s.resolveKiroRouteModel(ctx, account, route)
		return ok
	case "claude":
		_, _, ok := resolveClaudeRouteModel(route, capabilitiesByAccount[account.ID])
		return ok
	case "codex":
		_, _, ok := resolveCodexRouteModel(route, capabilitiesByAccount[account.ID])
		return ok
	default:
		return capable == nil || capable[account.ID]
	}
}

func (s *Scheduler) queuedForRoute(route Route) int {
	key := schedulerQueueKey(route)
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if q := s.waitQueues[key]; q != nil {
		return q.len
	}
	return 0
}
