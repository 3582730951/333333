package api

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	routeAvailabilityRefreshInterval = time.Second
	routeAvailabilityScanTimeout     = 5 * time.Second
	routeAvailabilityMaxKeys         = 4096
	routeAvailabilityMaxBatch        = 256
)

// routeAvailabilityIndex is a negative-only preflight cache for UserGroup
// targets. The background worker continuously scans routes that traffic has
// observed; the request path can synchronously refresh a missing/stale key.
//
// It publishes two narrowly proven labels: structural impossibility, and a
// transient "every compatible ordinary account is cooling/backing off" state.
// The transient label is forbidden when any compatible account opted into
// ForceCodex429 or IgnoreRateLimitControls, and it is refreshed every second so
// quota recovery/cooldown expiry removes the fast skip without request traffic.
// Concurrency, token pressure, hard egress failure and mixed states remain
// request-time scheduler decisions.
type routeAvailabilityIndex struct {
	scheduler *scheduler.Scheduler
	store     *storage.Store

	mu                    sync.RWMutex
	marks                 map[string]routeAvailabilityMark
	routes                map[string]scheduler.Route
	customProviderVersion string
	cursor                int
	runner                supervisor.Restartable
	wake                  chan struct{}

	enabled    atomic.Bool
	scans      atomic.Uint64
	skips      atomic.Uint64
	lastScanAt atomic.Int64
}

type routeAvailabilityMark struct {
	probe            scheduler.RouteAvailabilityProbe
	schedulerVersion uint64
	checkedAt        time.Time
}

type routeAvailabilityState uint8

const (
	routeAvailabilityReady routeAvailabilityState = iota
	routeAvailabilityStructurallyUnavailable
	routeAvailabilityCoolingBackoff
)

func (m routeAvailabilityMark) state() routeAvailabilityState {
	if m.probe.StructuralCandidates == 0 {
		return routeAvailabilityStructurallyUnavailable
	}
	if m.probe.CoolingWithoutOverrides() {
		return routeAvailabilityCoolingBackoff
	}
	return routeAvailabilityReady
}

type routeAvailabilitySnapshot struct {
	Enabled       bool   `json:"enabled"`
	TrackedRoutes int    `json:"tracked_routes"`
	MarkedEmpty   int    `json:"marked_empty"`
	MarkedCooling int    `json:"marked_cooling"`
	Scans         uint64 `json:"scans"`
	Skips         uint64 `json:"skips"`
	LastScanAt    int64  `json:"last_scan_at"`
}

type routeAvailabilityTrackedRoute struct {
	key   string
	route scheduler.Route
}

type routeAvailabilityProviderSurface struct {
	ID            string
	Enabled       bool
	Models        []string
	ModelMappings map[string]string
}

func newRouteAvailabilityIndex(s *scheduler.Scheduler, store *storage.Store) *routeAvailabilityIndex {
	if s == nil {
		return nil
	}
	return &routeAvailabilityIndex{
		scheduler: s,
		store:     store,
		marks:     make(map[string]routeAvailabilityMark),
		routes:    make(map[string]scheduler.Route),
		wake:      make(chan struct{}, 1),
	}
}

func (i *routeAvailabilityIndex) Start(ctx context.Context) {
	if i == nil || i.scheduler == nil || ctx == nil {
		return
	}
	i.enabled.Store(true)
	i.runner.Start(ctx, supervisor.Options{Name: "user-group-route-availability"}, i.run)
}

func (i *routeAvailabilityIndex) Enable() {
	if i != nil {
		i.enabled.Store(true)
	}
}

func (i *routeAvailabilityIndex) Wake() {
	if i == nil {
		return
	}
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (s *Server) wakeRouteAvailability() {
	if s != nil && s.routeAvailability != nil {
		s.routeAvailability.Wake()
	}
}

func (i *routeAvailabilityIndex) run(ctx context.Context) {
	ticker := time.NewTicker(routeAvailabilityRefreshInterval)
	defer ticker.Stop()
	// Seed every configured model-routing rule immediately. Observed wildcard and
	// previously unseen concrete models are still registered on the request path.
	i.seedConfiguredRoutes(ctx)
	i.refreshTracked(ctx)
	// track wakes the worker for newly seeded keys. The initial refresh above already
	// consumed their latest state, so avoid immediately repeating the same batch.
	select {
	case <-i.wake:
	default:
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-i.wake:
		}
		i.refreshTracked(ctx)
	}
}

func (i *routeAvailabilityIndex) seedConfiguredRoutes(ctx context.Context) {
	if i == nil || i.store == nil {
		return
	}
	groups, err := i.store.ListUserGroups(ctx)
	if err != nil {
		log.Printf("[USER-GROUP-ROUTE] availability seed failed: %v", err)
		return
	}
	for _, group := range groups {
		for _, rule := range group.ModelRouting {
			model := strings.TrimSpace(rule.Model)
			if model == "" || model == "*" || strings.HasSuffix(model, "*") {
				continue
			}
			for _, tier := range rule.Tiers {
				for _, target := range tier {
					if target.Kind != storage.TargetKindAccountPoolGroup {
						continue
					}
					route, ok := autoGPTAvailabilityRoute(target.ID, model, true)
					if !ok {
						continue
					}
					route = normalizeAvailabilityRoute(route)
					i.track(routeAvailabilityKey(route), route)
				}
			}
		}
	}
}

func (i *routeAvailabilityIndex) refreshTracked(parent context.Context) {
	if i == nil || i.scheduler == nil {
		return
	}
	i.publishCustomProviderChanges(parent)
	i.mu.RLock()
	tracked := make([]routeAvailabilityTrackedRoute, 0, len(i.routes))
	for key, route := range i.routes {
		tracked = append(tracked, routeAvailabilityTrackedRoute{key: key, route: route})
	}
	i.mu.RUnlock()
	if len(tracked) == 0 {
		return
	}
	sort.Slice(tracked, func(left, right int) bool { return tracked[left].key < tracked[right].key })
	if len(tracked) > routeAvailabilityMaxBatch {
		i.mu.Lock()
		start := i.cursor % len(tracked)
		i.cursor = (start + routeAvailabilityMaxBatch) % len(tracked)
		i.mu.Unlock()
		rotated := append(append(make([]routeAvailabilityTrackedRoute, 0, len(tracked)), tracked[start:]...), tracked[:start]...)
		tracked = rotated[:routeAvailabilityMaxBatch]
	}
	ctx, cancel := context.WithTimeout(parent, routeAvailabilityScanTimeout)
	defer cancel()
	// External database writers cannot call the in-process invalidation hook. A
	// periodic batch therefore clears scheduler snapshots once and rechecks the
	// tracked routes from storage; the request path still owns immediate in-process
	// correctness through RouteStructureVersion.
	i.scheduler.RefreshAccountCache()
	version := i.scheduler.RouteStructureVersion()
	for _, item := range tracked {
		if ctx.Err() != nil {
			return
		}
		probe, err := i.scheduler.ProbeRouteAvailability(ctx, item.route)
		if err == nil {
			i.storeMark(item.key, routeAvailabilityMark{
				probe:            probe,
				schedulerVersion: version,
				checkedAt:        time.Now(),
			})
		}
		if err != nil && parent.Err() == nil {
			log.Printf("[USER-GROUP-ROUTE] availability scan failed route=%s: %v", item.key, err)
		}
	}
}

// publishCustomProviderChanges covers provider edits made by another process.
// Account/model tables are re-read on every periodic batch; this small signature
// additionally invalidates request marks when a custom provider is enabled,
// disabled, remapped or changes its advertised GPT models outside this process.
func (i *routeAvailabilityIndex) publishCustomProviderChanges(ctx context.Context) bool {
	if i == nil || i.store == nil {
		return false
	}
	providers, err := i.store.ListCustomProviders(ctx)
	if err != nil {
		return false
	}
	surfaces := make([]routeAvailabilityProviderSurface, 0, len(providers))
	for _, provider := range providers {
		surfaces = append(surfaces, routeAvailabilityProviderSurface{
			ID: provider.ID, Enabled: provider.Enabled,
			Models: append([]string(nil), provider.Models...), ModelMappings: provider.ModelMappings,
		})
	}
	return i.publishCustomProviderSurface(surfaces)
}

func (i *routeAvailabilityIndex) publishCustomProviderSurface(providers []routeAvailabilityProviderSurface) bool {
	if i == nil || i.scheduler == nil {
		return false
	}
	sort.Slice(providers, func(left, right int) bool { return providers[left].ID < providers[right].ID })
	var signature strings.Builder
	for _, provider := range providers {
		signature.WriteString(provider.ID)
		signature.WriteByte('\x00')
		signature.WriteString(boolString(provider.Enabled))
		signature.WriteByte('\x00')
		models := append([]string(nil), provider.Models...)
		sort.Strings(models)
		signature.WriteString(strings.Join(models, ","))
		signature.WriteByte('\x00')
		keys := make([]string, 0, len(provider.ModelMappings))
		for source := range provider.ModelMappings {
			keys = append(keys, source)
		}
		sort.Strings(keys)
		for _, source := range keys {
			signature.WriteString(source)
			signature.WriteByte('=')
			signature.WriteString(provider.ModelMappings[source])
			signature.WriteByte(',')
		}
		signature.WriteByte('\n')
	}
	next := signature.String()
	i.mu.Lock()
	previous := i.customProviderVersion
	i.customProviderVersion = next
	i.mu.Unlock()
	if previous != "" && previous != next {
		i.scheduler.InvalidateAccountCache()
		return true
	}
	return false
}

// definitelyUnavailable is fail-open. It accepts either an exact structural zero
// or the narrowly proven all-ordinary-candidates cooling/backoff state. A stale
// mark or concurrent structural publication forces synchronous re-evaluation.
func (i *routeAvailabilityIndex) definitelyUnavailable(ctx context.Context, route scheduler.Route) bool {
	return i.allDefinitelyUnavailable(ctx, []scheduler.Route{route})
}

// allDefinitelyUnavailable treats several provider-specific routes as one target.
// This is needed for automatic GPT routing: a pool can execute through native
// Codex/Kiro or through any enabled custom provider advertising the downstream
// model. The target is skipped only when every concrete route has a current
// unavailable label under one stable scheduler generation.
func (i *routeAvailabilityIndex) allDefinitelyUnavailable(ctx context.Context, routes []scheduler.Route) bool {
	return i.allUnavailableState(ctx, routes) != routeAvailabilityReady
}

func (i *routeAvailabilityIndex) allUnavailableState(ctx context.Context, routes []scheduler.Route) routeAvailabilityState {
	if i == nil || i.scheduler == nil || ctx == nil || !i.enabled.Load() || len(routes) == 0 {
		return routeAvailabilityReady
	}
	version := i.scheduler.RouteStructureVersion()
	refreshNeeded := false
	for _, route := range routes {
		route = normalizeAvailabilityRoute(route)
		key := routeAvailabilityKey(route)
		i.track(key, route)
		i.mu.RLock()
		mark, found := i.marks[key]
		i.mu.RUnlock()
		age := time.Duration(-1)
		if found && !mark.checkedAt.IsZero() {
			age = time.Since(mark.checkedAt)
		}
		if !found || mark.schedulerVersion != version || age < 0 || age >= routeAvailabilityRefreshInterval*2 {
			refreshNeeded = true
			break
		}
	}
	if refreshNeeded {
		// A missing/expired mark is also the cross-process publication boundary.
		// Clear scheduler snapshots once for the whole target, then synchronously
		// re-evaluate every concrete provider route from storage.
		i.scheduler.RefreshAccountCache()
	}
	combined := routeAvailabilityStructurallyUnavailable
	for _, route := range routes {
		state := i.routeUnavailableState(ctx, route)
		if state == routeAvailabilityReady || version != i.scheduler.RouteStructureVersion() {
			return routeAvailabilityReady
		}
		if state == routeAvailabilityCoolingBackoff {
			combined = routeAvailabilityCoolingBackoff
		}
	}
	if version != i.scheduler.RouteStructureVersion() {
		return routeAvailabilityReady
	}
	i.skips.Add(1)
	return combined
}

func (i *routeAvailabilityIndex) routeUnavailableState(ctx context.Context, route scheduler.Route) routeAvailabilityState {
	if i == nil || i.scheduler == nil || ctx == nil || !i.enabled.Load() || strings.TrimSpace(route.Group) == "" {
		return routeAvailabilityReady
	}
	route = normalizeAvailabilityRoute(route)
	key := routeAvailabilityKey(route)
	i.track(key, route)
	version := i.scheduler.RouteStructureVersion()

	i.mu.RLock()
	mark, found := i.marks[key]
	i.mu.RUnlock()
	age := time.Duration(-1)
	if found && !mark.checkedAt.IsZero() {
		age = time.Since(mark.checkedAt)
	}
	if found && mark.schedulerVersion == version && age >= 0 && age < routeAvailabilityRefreshInterval*2 {
		if state := mark.state(); state != routeAvailabilityReady {
			// Linearize the skip after the mark read. An account/capability publication
			// racing this request changes the generation and forces a fail-open retry.
			if version == i.scheduler.RouteStructureVersion() {
				return state
			}
			return routeAvailabilityReady
		}
		return routeAvailabilityReady
	}
	mark, err := i.refresh(ctx, key)
	if err != nil || mark.schedulerVersion != i.scheduler.RouteStructureVersion() {
		return routeAvailabilityReady
	}
	if state := mark.state(); state != routeAvailabilityReady && mark.schedulerVersion == i.scheduler.RouteStructureVersion() {
		return state
	}
	return routeAvailabilityReady
}

func (i *routeAvailabilityIndex) track(key string, route scheduler.Route) {
	added := false
	i.mu.Lock()
	if _, exists := i.routes[key]; !exists {
		if len(i.routes) >= routeAvailabilityMaxKeys {
			// Drop an arbitrary old key. Marks are only an optimization and the next
			// request reconstructs an evicted key synchronously.
			for oldKey := range i.routes {
				delete(i.routes, oldKey)
				delete(i.marks, oldKey)
				break
			}
		}
		i.routes[key] = route
		added = true
	}
	i.mu.Unlock()
	if added {
		select {
		case i.wake <- struct{}{}:
		default:
		}
	}
}

func (i *routeAvailabilityIndex) refresh(ctx context.Context, key string) (routeAvailabilityMark, error) {
	i.mu.RLock()
	route, exists := i.routes[key]
	i.mu.RUnlock()
	if !exists {
		return routeAvailabilityMark{}, nil
	}
	version := i.scheduler.RouteStructureVersion()
	probe, err := i.scheduler.ProbeRouteAvailability(ctx, route)
	if err != nil {
		return routeAvailabilityMark{}, err
	}
	mark := routeAvailabilityMark{
		probe:            probe,
		schedulerVersion: version,
		checkedAt:        time.Now(),
	}
	// If the scheduler changed while the scan was running, retain the old mark
	// only as diagnostic history; no request will trust its older generation.
	i.storeMark(key, mark)
	return mark, nil
}

func (i *routeAvailabilityIndex) storeMark(key string, mark routeAvailabilityMark) {
	i.mu.Lock()
	i.marks[key] = mark
	i.mu.Unlock()
	i.scans.Add(1)
	i.lastScanAt.Store(mark.checkedAt.Unix())
}

func (i *routeAvailabilityIndex) Snapshot() routeAvailabilitySnapshot {
	if i == nil {
		return routeAvailabilitySnapshot{}
	}
	version := i.scheduler.RouteStructureVersion()
	empty, cooling := 0, 0
	i.mu.RLock()
	tracked := len(i.routes)
	for _, mark := range i.marks {
		if mark.schedulerVersion != version {
			continue
		}
		switch mark.state() {
		case routeAvailabilityStructurallyUnavailable:
			empty++
		case routeAvailabilityCoolingBackoff:
			cooling++
		}
	}
	i.mu.RUnlock()
	return routeAvailabilitySnapshot{
		Enabled:       i.enabled.Load(),
		TrackedRoutes: tracked,
		MarkedEmpty:   empty,
		MarkedCooling: cooling,
		Scans:         i.scans.Load(),
		Skips:         i.skips.Load(),
		LastScanAt:    i.lastScanAt.Load(),
	}
}

func normalizeAvailabilityRoute(route scheduler.Route) scheduler.Route {
	route.Group = strings.TrimSpace(route.Group)
	route.Provider = strings.ToLower(strings.TrimSpace(route.Provider))
	route.Model = strings.TrimSpace(route.Model)
	route.ContextMode = strings.ToLower(strings.TrimSpace(route.ContextMode))
	route.KiroFallbackModel = strings.TrimSpace(route.KiroFallbackModel)
	providers := make([]string, 0, len(route.AllowedProviders))
	seen := make(map[string]struct{}, len(route.AllowedProviders))
	for _, provider := range route.AllowedProviders {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		if _, duplicate := seen[provider]; duplicate {
			continue
		}
		seen[provider] = struct{}{}
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	route.AllowedProviders = providers
	// Keep only the fields consumed by group/provider/model compatibility. Clearing
	// the remaining request, quota, egress and scheduling fields also makes the key
	// independent from transient or per-call policy.
	route.Affinity = scheduler.Route{}.Affinity
	route.AffinityWait = 0
	route.Strict = false
	route.ServerSideState = false
	route.ImmutableAffinity = false
	route.Movable = false
	route.FairScheduling = false
	route.KiroEndpointAllowlist = nil
	route.KiroDefaultRegion = ""
	route.EstimatedTokens = 0
	route.Compaction = false
	route.AllowCodexGoalQuotaGrace = false
	route.SkipWait = false
	route.PreferredEgressIDs = nil
	route.Exclude = nil
	route.RequiredAccountID = ""
	route.RequiredEgressID = ""
	route.OnWait = nil
	return route
}

func routeAvailabilityKey(route scheduler.Route) string {
	return strings.Join([]string{
		route.Group,
		route.Provider,
		strings.Join(route.AllowedProviders, ","),
		route.Model,
		route.ContextMode,
		route.KiroFallbackModel,
		boolString(route.ExplicitProvider),
		boolString(route.ThinkingRequired),
	}, "\x00")
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// userGroupAvailabilityRoutes deliberately recognizes only the exact fast path we
// can prove structurally: replay-safe automatic GPT routing through an account-pool
// target. Custom/provider targets and local count-token surfaces fail open. This is
// a negative cache, so declining an optimization is always safer than guessing at
// an adapter-specific mapping and skipping a route that could execute.
func (s *Server) userGroupAvailabilityRoutes(r *http.Request, pol downstreamPolicy, target storage.TargetRef, model string, customProviders []storage.CustomProvider) ([]scheduler.Route, bool) {
	if s == nil || s.scheduler == nil || r == nil || strings.TrimSpace(model) == "" {
		return nil, false
	}
	if target.Kind != storage.TargetKindAccountPoolGroup || effectiveGatewayProviderHint(r, pol) != "auto" {
		return nil, false
	}
	path := strings.TrimSpace(r.URL.Path)
	if strings.HasSuffix(path, "/count_tokens") || (path != "/v1/responses" && path != "/v1/chat/completions" && path != "/v1/responses/compact") {
		return nil, false
	}
	if modelInstructionFamily(model) != storage.ModelInstructionFamilyGPT {
		return nil, false
	}
	base, ok := autoGPTAvailabilityRoute(target.ID, model, path != "/v1/responses/compact")
	if !ok {
		return nil, false
	}
	routes := []scheduler.Route{base}
	for _, provider := range customProviders {
		targetModel, mapped := customProviderMappedModel(provider, model)
		if !mapped {
			targetModel = model
		}
		routes = append(routes, scheduler.Route{
			Group: target.ID, Provider: provider.ID, Model: targetModel, ExplicitProvider: true,
		})
	}
	return routes, true
}

func autoGPTAvailabilityRoute(group, model string, allowKiro bool) (scheduler.Route, bool) {
	group = strings.TrimSpace(group)
	model = strings.TrimSpace(model)
	if group == "" || modelInstructionFamily(model) != storage.ModelInstructionFamilyGPT {
		return scheduler.Route{}, false
	}
	route := scheduler.Route{Group: group, Provider: "codex", Model: model}
	if allowKiro {
		if fallback, eligible := autoKiroGPTModelForCodex(model); eligible {
			route.Provider = ""
			route.AllowedProviders = []string{"codex", "kiro"}
			route.KiroFallbackModel = fallback
		}
	}
	return route, true
}
