package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

type userGroupRouteOverrideKey struct{}
type userGroupFallbackProbeKey struct{}
type trafficFallbackExecutionKey struct{}
type trafficFallbackProbeKey struct{}

const maxTrafficFallbackDepth = 8

type trafficFallbackExecution struct {
	SourceUserGroupID string
	TargetUserGroupID string
	SourceModel       string
	TargetModel       string
	Family            string
	Trail             []string
}

type trafficFallbackCandidate struct {
	UserGroupID string
	SourceModel string
	TargetModel string
	Family      string
}

var (
	// A request that currently has no eligible account-pool target stays queued
	// for one full request window. Polling watches only cheap DB generations; the
	// original inference body is retried only after capacity changes (plus a slow
	// safety recheck for external providers).
	userGroupCapacityWaitTimeout         = 10 * time.Minute
	userGroupCapacityPollInterval        = 5 * time.Second
	userGroupCapacityHeartbeatInterval   = 15 * time.Second
	userGroupCapacitySafetyRetryInterval = time.Minute
)

type userGroupCapacityWaitState struct {
	deadline      time.Time
	generation    string
	lastHeartbeat time.Time
	lastRetry     time.Time
	started       bool
}

type userGroupCapacityUnavailableError struct {
	UserGroupID string
	Model       string
}

func (e *userGroupCapacityUnavailableError) Error() string {
	return fmt.Sprintf("user group %s has no eligible target for model %q", e.UserGroupID, e.Model)
}

func (s *Server) newUserGroupCapacityWaitState(ctx context.Context, pol downstreamPolicy) userGroupCapacityWaitState {
	now := time.Now()
	deadline := now.Add(userGroupCapacityWaitTimeout)
	if requestDeadline, ok := ctx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	generation, _ := s.userGroupRouteGeneration(ctx, pol)
	return userGroupCapacityWaitState{
		deadline:   deadline,
		generation: generation,
		lastRetry:  now,
	}
}

func (s *Server) userGroupRouteGeneration(ctx context.Context, pol downstreamPolicy) (string, error) {
	type pendingGroup struct {
		id        string
		baseGroup string
		depth     int
	}
	queue := []pendingGroup{{id: strings.TrimSpace(pol.UserGroupID), baseGroup: pol.Group}}
	seen := make(map[string]struct{})
	parts := make([]string, 0, maxTrafficFallbackDepth)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.id == "" || current.depth >= maxTrafficFallbackDepth {
			continue
		}
		if _, duplicate := seen[current.id]; duplicate {
			continue
		}
		seen[current.id] = struct{}{}
		generation, err := s.store.UserGroupRouteGeneration(ctx, current.id, current.baseGroup)
		if err != nil {
			return "", err
		}
		parts = append(parts, current.id+"="+generation)
		group, found, err := s.store.GetUserGroup(ctx, current.id)
		if err != nil {
			return "", err
		}
		if !found {
			continue
		}
		nextIDs := append(append(append([]string{}, group.TrafficFallbackGroups.GPT...), group.TrafficFallbackGroups.Claude...), group.TrafficFallbackGroups.Gemini...)
		for _, id := range nextIDs {
			queue = append(queue, pendingGroup{id: id, depth: current.depth + 1})
		}
	}
	return strings.Join(parts, "|"), nil
}

// waitForUserGroupCapacityChange keeps a streaming client alive without
// repeatedly replaying its potentially large body. A target/policy/account/model
// generation change wakes it immediately; a slow safety retry covers provider
// recovery that is external to the local database.
func (s *Server) waitForUserGroupCapacityChange(ctx context.Context, pol downstreamPolicy, state *userGroupCapacityWaitState) bool {
	if state == nil || !time.Now().Before(state.deadline) {
		return false
	}
	heartbeat := schedulerWaitCallback(ctx)
	if !state.started {
		state.started = true
		state.lastHeartbeat = time.Now()
		if heartbeat != nil {
			heartbeat("user_group_capacity", 0)
		}
	}
	for {
		now := time.Now()
		if !now.Before(state.deadline) {
			return false
		}
		wait := userGroupCapacityPollInterval
		if remaining := time.Until(state.deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return false
		case <-timer.C:
		}

		now = time.Now()
		if heartbeat != nil && now.Sub(state.lastHeartbeat) >= userGroupCapacityHeartbeatInterval {
			heartbeat("user_group_capacity", now.Sub(state.lastRetry))
			state.lastHeartbeat = now
		}
		generation, err := s.userGroupRouteGeneration(ctx, pol)
		if err == nil && generation != state.generation {
			state.generation = generation
			state.lastRetry = now
			return true
		}
		if now.Sub(state.lastRetry) >= userGroupCapacitySafetyRetryInterval {
			state.lastRetry = now
			return true
		}
	}
}

func finishUserGroupCapacityWait(ctx context.Context, w http.ResponseWriter) {
	if schedulerWaitTerminal(ctx, "The configured group has no available service.") {
		return
	}
	writePublicServiceUnavailable(w)
}

func blockedUserGroupTarget(group storage.UserGroup, target storage.TargetRef, model string) bool {
	if target.Kind != storage.TargetKindAccountPoolGroup {
		return false
	}
	var blocked []string
	switch modelInstructionFamily(model) {
	case storage.ModelInstructionFamilyClaude:
		blocked = group.BlockClaudeTargetGroups
	case storage.ModelInstructionFamilyGPT:
		blocked = group.BlockGPTTargetGroups
	}
	for _, groupName := range blocked {
		if strings.TrimSpace(groupName) == target.ID {
			return true
		}
	}
	return false
}

type userGroupRoutePlan struct {
	UserGroupID  string
	AffinityKey  string
	BindingModel string
	Candidates   []storage.TargetRef
	Tiers        [][]storage.TargetRef
	Affinity     routing.AffinityKey
	Persist      bool
	Bound        bool
	BoundTarget  storage.TargetRef
	// PolicyTransfer is set when an existing model-scoped binding now points at
	// an account-pool target blocked by the user-group policy. The next eligible
	// target atomically takes over that binding instead of returning a rejection.
	PolicyTransfer bool
}

func withUserGroupRouteOverride(ctx context.Context, target storage.TargetRef) context.Context {
	return context.WithValue(ctx, userGroupRouteOverrideKey{}, target)
}

func userGroupRouteOverride(ctx context.Context) (storage.TargetRef, bool) {
	target, ok := ctx.Value(userGroupRouteOverrideKey{}).(storage.TargetRef)
	return target, ok
}

func withUserGroupFallbackProbe(ctx context.Context) context.Context {
	return context.WithValue(ctx, userGroupFallbackProbeKey{}, true)
}

func userGroupFallbackProbe(ctx context.Context) bool {
	probe, _ := ctx.Value(userGroupFallbackProbeKey{}).(bool)
	return probe
}

func withTrafficFallbackExecution(ctx context.Context, execution trafficFallbackExecution) context.Context {
	execution.Trail = append([]string(nil), execution.Trail...)
	ctx = context.WithValue(ctx, trafficFallbackExecutionKey{}, execution)
	return context.WithValue(ctx, trafficFallbackProbeKey{}, true)
}

func trafficFallbackExecutionFromContext(ctx context.Context) (trafficFallbackExecution, bool) {
	execution, ok := ctx.Value(trafficFallbackExecutionKey{}).(trafficFallbackExecution)
	return execution, ok
}

func trafficFallbackProbe(ctx context.Context) bool {
	probe, _ := ctx.Value(trafficFallbackProbeKey{}).(bool)
	return probe
}

// resolveUserGroupRoute selects the routing target for a request that uses the
// two-layer group model (pol.UserGroupID != ""). Returns:
//   - resolvedGroup: base group name (empty for built-in providers like kiro/antigravity)
//   - resolvedProvider: provider hint override ("kiro", "antigravity", "custom:<id>", or "")
//
// Selection: affinity-spread (consistent-hash weighted by AffinityWeight), with
// session-sticky re-use when an affinity binding already exists for this route key.
// Falls back to pol.Group / pol.ProviderHint when no user_group targets are configured.
func resolveUserGroupRoute(ctx context.Context, store *storage.Store, pol downstreamPolicy, r *http.Request, raw []byte) (resolvedGroup, resolvedProvider string, err error) {
	if target, ok := userGroupRouteOverride(ctx); ok {
		return targetRefToRoute(target)
	}
	plan, err := resolveUserGroupRouteCandidates(ctx, store, pol, r, raw)
	if err != nil {
		return "", "", err
	}
	if len(plan.Candidates) == 0 {
		return pol.Group, pol.ProviderHint, nil
	}
	selected := plan.Candidates[0]
	actual, err := claimUserGroupRouteBinding(ctx, store, plan, selected)
	if err != nil {
		return "", "", err
	}
	return targetRefToRoute(actual)
}

// resolveUserGroupRouteCandidates returns every compatible target in retry order.
// Targets inside one tier use rendezvous ordering; all candidates in that tier are
// exhausted before the next tier. A compatible root/session binding is promoted to
// the front, while an incompatible root target uses a model-scoped exception.
func resolveUserGroupRouteCandidates(ctx context.Context, store *storage.Store, pol downstreamPolicy, r *http.Request, raw []byte) (userGroupRoutePlan, error) {
	if pol.UserGroupID == "" {
		return userGroupRoutePlan{}, nil
	}
	group, ok, groupErr := store.GetUserGroup(ctx, pol.UserGroupID)
	if groupErr != nil {
		return userGroupRoutePlan{}, groupErr
	}
	if !ok {
		return userGroupRoutePlan{}, fmt.Errorf("user group %s not found", pol.UserGroupID)
	}
	model := routing.Model(raw)
	tiers, tierErr := compatibleUserGroupTiers(ctx, store, group, model)
	if tierErr != nil {
		return userGroupRoutePlan{}, tierErr
	}
	// Auto mode keeps every configured target and lets exact account capability
	// checks decide executability. An explicit header/key hint fixes only provider
	// targets; account-pool targets remain because they may contain that provider.
	if hint := effectiveGatewayProviderHint(r, pol); hint != "" && hint != "auto" {
		tiers = userGroupTiersForProvider(tiers, hint)
	}
	if len(tiers) == 0 {
		return userGroupRoutePlan{}, &userGroupCapacityUnavailableError{UserGroupID: group.ID, Model: model}
	}
	affinityKeyObj := routing.ExtractAffinityKey(r, raw)
	if strings.HasPrefix(strings.TrimSpace(r.URL.Path), "/v1/messages") {
		affinityKeyObj = routing.ExtractClaudeAffinityKey(r, raw)
	}
	affinityKey := affinityKeyObj.Hash
	seed := pol.UserGroupID + ":" + affinityKey
	if affinityKey == "" {
		affinityKey = fnvHashString(string(raw))
		seed = pol.UserGroupID + ":request:" + affinityKey
	}
	orderedTiers := orderUserGroupTierSlices(tiers, seed)
	ordered := flattenUserGroupTiers(orderedTiers)
	plan := userGroupRoutePlan{
		UserGroupID: group.ID,
		AffinityKey: affinityKeyObj.Hash,
		Candidates:  ordered,
		Tiers:       orderedTiers,
		Affinity:    affinityKeyObj,
		Persist:     routing.IsTrueConversationAffinity(affinityKeyObj),
	}

	// A root/session binding is shared by the main CLI and its child agents. Only
	// create a model-scoped exception when that target cannot serve the child model.
	if plan.Persist {
		base, found, bindingErr := store.GetUserGroupTargetBinding(ctx, group.ID, affinityKeyObj.Hash, "")
		if bindingErr != nil {
			return userGroupRoutePlan{}, bindingErr
		}
		if found && userGroupTargetsContain(ordered, base.Target) {
			plan.Bound = true
			plan.BoundTarget = base.Target
			plan.Tiers = prioritizeUserGroupTargetInTiers(orderedTiers, base.Target)
			plan.Candidates = flattenUserGroupTiers(plan.Tiers)
			return plan, nil
		}
		if found {
			plan.BindingModel = model
			exception, exceptionFound, exceptionErr := store.GetUserGroupTargetBinding(ctx, group.ID, affinityKeyObj.Hash, model)
			if exceptionErr != nil {
				return userGroupRoutePlan{}, exceptionErr
			}
			if exceptionFound && userGroupTargetsContain(ordered, exception.Target) {
				plan.Bound = true
				plan.BoundTarget = exception.Target
				plan.Tiers = prioritizeUserGroupTargetInTiers(orderedTiers, exception.Target)
				plan.Candidates = flattenUserGroupTiers(plan.Tiers)
			} else if exceptionFound {
				// A policy edit can invalidate a durable model-specific target while
				// no traffic is present. Preserve the old value as the CAS expected
				// target and let the first later request transfer it atomically.
				plan.Bound = true
				plan.BoundTarget = exception.Target
				plan.PolicyTransfer = true
			}
		}
	}
	return plan, nil
}

func commitUserGroupRouteBinding(ctx context.Context, store *storage.Store, plan userGroupRoutePlan, selected storage.TargetRef) error {
	_, err := claimUserGroupRouteBinding(ctx, store, plan, selected)
	return err
}

func claimUserGroupRouteBinding(ctx context.Context, store *storage.Store, plan userGroupRoutePlan, selected storage.TargetRef) (storage.TargetRef, error) {
	if plan.UserGroupID == "" || plan.AffinityKey == "" || !plan.Persist {
		return selected, nil
	}
	actual, _, err := store.ClaimUserGroupTargetBinding(ctx, storage.UserGroupTargetBinding{
		UserGroupID: plan.UserGroupID,
		AffinityKey: plan.AffinityKey,
		Model:       plan.BindingModel,
		Target:      selected,
	})
	if err != nil {
		return storage.TargetRef{}, err
	}
	return actual.Target, nil
}

func userGroupTargetsContain(targets []storage.TargetRef, target storage.TargetRef) bool {
	for _, candidate := range targets {
		if candidate.Kind == target.Kind && candidate.ID == target.ID {
			return true
		}
	}
	return false
}

func prioritizeUserGroupTarget(targets []storage.TargetRef, target storage.TargetRef) []storage.TargetRef {
	if len(targets) < 2 || (targets[0].Kind == target.Kind && targets[0].ID == target.ID) {
		return targets
	}
	ordered := make([]storage.TargetRef, 0, len(targets))
	ordered = append(ordered, target)
	for _, candidate := range targets {
		if candidate.Kind != target.Kind || candidate.ID != target.ID {
			ordered = append(ordered, candidate)
		}
	}
	return ordered
}

func prioritizeUserGroupTargetInTiers(tiers [][]storage.TargetRef, target storage.TargetRef) [][]storage.TargetRef {
	out := make([][]storage.TargetRef, 0, len(tiers))
	for _, tier := range tiers {
		copyTier := append([]storage.TargetRef(nil), tier...)
		if userGroupTargetsContain(copyTier, target) {
			copyTier = prioritizeUserGroupTarget(copyTier, target)
		}
		out = append(out, copyTier)
	}
	// A persisted conversation target outranks ordinary tier order while it remains
	// compatible. Move the containing tier to the front without merging tiers.
	for i := range out {
		if userGroupTargetsContain(out[i], target) && i > 0 {
			selected := out[i]
			copy(out[1:i+1], out[0:i])
			out[0] = selected
			break
		}
	}
	return out
}

func trafficFallbackGroupsForFamily(groups storage.TrafficFallbackGroups, family string) []string {
	switch family {
	case storage.ModelInstructionFamilyGPT:
		return groups.GPT
	case storage.ModelInstructionFamilyClaude:
		return groups.Claude
	case storage.ModelInstructionFamilyGemini:
		return groups.Gemini
	default:
		return nil
	}
}

func trafficFallbackMappingScore(sourcePattern, model string) int {
	sourcePattern = strings.ToLower(strings.TrimSpace(sourcePattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if sourcePattern == model {
		return 10000 + len(sourcePattern)
	}
	if sourcePattern == "*" {
		return 1
	}
	if strings.HasSuffix(sourcePattern, "*") && strings.HasPrefix(model, strings.TrimSuffix(sourcePattern, "*")) {
		return 100 + len(sourcePattern)
	}
	return -1
}

func trafficFallbackFamilies(group storage.UserGroup, model string) []string {
	if family := modelInstructionFamily(model); family != "" {
		return []string{family}
	}
	bestFamily := ""
	bestScore := -1
	for _, family := range []string{
		storage.ModelInstructionFamilyGPT,
		storage.ModelInstructionFamilyClaude,
		storage.ModelInstructionFamilyGemini,
	} {
		for _, mapping := range group.TrafficFallbackModelMappings {
			if mapping.Family != family {
				continue
			}
			score := trafficFallbackMappingScore(mapping.SourceModel, model)
			// A family-less manual model must be named explicitly (or by a
			// non-empty prefix). A global "*" in several families is otherwise
			// ambiguous and could silently cross providers.
			if score <= 1 {
				continue
			}
			if score > bestScore {
				bestScore = score
				bestFamily = family
			}
		}
	}
	if bestFamily == "" {
		return nil
	}
	return []string{bestFamily}
}

func trafficFallbackCandidates(group storage.UserGroup, model string, trail []string) []trafficFallbackCandidate {
	if len(trail) >= maxTrafficFallbackDepth {
		return nil
	}
	visited := make(map[string]struct{}, len(trail)+1)
	for _, id := range trail {
		visited[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]trafficFallbackCandidate, 0)
	for _, family := range trafficFallbackFamilies(group, model) {
		for _, targetID := range trafficFallbackGroupsForFamily(group.TrafficFallbackGroups, family) {
			targetID = strings.TrimSpace(targetID)
			if targetID == "" {
				continue
			}
			if _, seen := visited[targetID]; seen {
				continue
			}
			bestScore := -1
			targetModel := ""
			for _, mapping := range group.TrafficFallbackModelMappings {
				if mapping.Family != family || strings.TrimSpace(mapping.TargetUserGroupID) != targetID {
					continue
				}
				score := trafficFallbackMappingScore(mapping.SourceModel, model)
				if score > bestScore {
					bestScore = score
					targetModel = strings.TrimSpace(mapping.TargetModel)
				}
			}
			if bestScore < 0 || targetModel == "" {
				continue
			}
			out = append(out, trafficFallbackCandidate{
				UserGroupID: targetID,
				SourceModel: model,
				TargetModel: targetModel,
				Family:      family,
			})
		}
	}
	return out
}

func trafficFallbackTrail(ctx context.Context, currentUserGroupID string) []string {
	trail := make([]string, 0, maxTrafficFallbackDepth)
	if execution, ok := trafficFallbackExecutionFromContext(ctx); ok {
		trail = append(trail, execution.Trail...)
	}
	currentUserGroupID = strings.TrimSpace(currentUserGroupID)
	if currentUserGroupID != "" && (len(trail) == 0 || trail[len(trail)-1] != currentUserGroupID) {
		trail = append(trail, currentUserGroupID)
	}
	return trail
}

// dispatchUserGroupTrafficFallback re-enters the same inference handler with an
// authoritative destination user group and model. The response remains buffered
// until a fallback succeeds, so a failed destination never leaks headers/body to
// the client and the next configured destination may be tried safely.
func (s *Server) dispatchUserGroupTrafficFallback(
	w http.ResponseWriter,
	r *http.Request,
	originalRaw, resolvedRaw []byte,
	pol downstreamPolicy,
	dispatch func(http.ResponseWriter, *http.Request),
) bool {
	if routing.HasServerSideState(r.URL.Path, r, resolvedRaw) {
		return false
	}
	group, found, err := s.store.GetUserGroup(r.Context(), pol.UserGroupID)
	if err != nil || !found {
		return false
	}
	sourceModel := routing.Model(resolvedRaw)
	trail := trafficFallbackTrail(r.Context(), pol.UserGroupID)
	candidates := trafficFallbackCandidates(group, sourceModel, trail)
	if len(candidates) == 0 {
		return false
	}
	streamRequest := isStreamRequest(resolvedRaw)
	for index, fallback := range candidates {
		nextTrail := append(append([]string(nil), trail...), fallback.UserGroupID)
		execution := trafficFallbackExecution{
			SourceUserGroupID: pol.UserGroupID,
			TargetUserGroupID: fallback.UserGroupID,
			SourceModel:       fallback.SourceModel,
			TargetModel:       fallback.TargetModel,
			Family:            fallback.Family,
			Trail:             nextTrail,
		}
		attempt := newUserGroupAttemptWriter(r.Context(), w, streamRequest, strings.HasPrefix(strings.TrimSpace(r.URL.Path), "/v1/responses"), bodysource.CaptureOptions{
			MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
			Budget: s.responseBodyBudget, DiskReserver: s.bodyDiskReserver, TempFileNamePrefix: "codex-pool-traffic-fallback-response-*",
		}, true)
		attempt.Header().Set("X-Pool-Fallback-From-Group", pol.UserGroupID)
		attempt.Header().Set("X-Pool-Fallback-Group", fallback.UserGroupID)
		attempt.Header().Set("X-Pool-Fallback-Model", fallback.TargetModel)
		fallbackContext := withTrafficFallbackExecution(r.Context(), execution)
		fallbackContext = withBufferedSchedulerWait(fallbackContext, attempt)
		fallbackRequest := r.Clone(fallbackContext)
		fallbackRequest.Header = r.Header.Clone()
		// A configured cross-family rewrite is authoritative; an incoming provider
		// pin must not silently filter the destination group's compatible targets.
		fallbackRequest.Header.Del("X-Pool-Provider")
		fallbackRaw := setForcedModel(originalRaw, fallback.TargetModel)
		fallbackRequest.Body, fallbackRequest.GetBody = replayUserGroupRequestBody(fallbackContext, fallbackRaw)
		fallbackRequest.ContentLength = int64(len(fallbackRaw))
		dispatch(attempt, fallbackRequest)
		if fallbackRequest.Body != nil {
			_ = fallbackRequest.Body.Close()
		}

		status := attempt.Status()
		if _, terminalFailure := attempt.TerminalFailure(); terminalFailure {
			detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, fallback.UserGroupID, attempt)
			s.recordRouteAttempt(requestIDFromContext(r.Context()), -1, "user_group:"+fallback.UserGroupID, "traffic_fallback", userGroupRouteStatusClass(attempt), "", detail)
			attempt.Commit()
			return true
		}
		if status < http.StatusBadRequest || attempt.Committed() {
			detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, fallback.UserGroupID, attempt)
			s.recordRouteAttempt(requestIDFromContext(r.Context()), -1, "user_group:"+fallback.UserGroupID, "traffic_fallback", "success", "", detail)
			attempt.Commit()
			return true
		}
		retryable := attempt.RetryableFailure()
		next := ""
		if retryable && index+1 < len(candidates) {
			next = "user_group:" + candidates[index+1].UserGroupID
		}
		detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, fallback.UserGroupID, attempt)
		s.recordRouteAttempt(requestIDFromContext(r.Context()), -1, "user_group:"+fallback.UserGroupID, "traffic_fallback", userGroupRouteStatusClass(attempt), next, detail)
		if !retryable {
			attempt.Commit()
			return true
		}
		_ = attempt.Close()
	}
	return false
}

// dispatchUserGroupRouteCandidates runs an entrypoint once per ordered target and
// discards only pre-commit, target-scoped failures. A successful stream is passed
// through on its first flush/write; after that point another target is never tried.
func (s *Server) dispatchUserGroupRouteCandidates(w http.ResponseWriter, r *http.Request, originalRaw, resolvedRaw []byte, pol downstreamPolicy, dispatch func(http.ResponseWriter, *http.Request)) bool {
	if pol.UserGroupID == "" {
		return false
	}
	if _, forced := userGroupRouteOverride(r.Context()); forced {
		return false
	}
	capacityWait := s.newUserGroupCapacityWaitState(r.Context(), pol)

retryUserGroupRoute:
	plan, err := resolveUserGroupRouteCandidates(r.Context(), s.store, pol, r, resolvedRaw)
	if err != nil {
		if _, unavailable := err.(*userGroupCapacityUnavailableError); unavailable {
			if s.dispatchUserGroupTrafficFallback(w, r, originalRaw, resolvedRaw, pol, dispatch) {
				return true
			}
			if trafficFallbackProbe(r.Context()) {
				finishUserGroupCapacityWait(r.Context(), w)
				return true
			}
			if s.waitForUserGroupCapacityChange(r.Context(), pol, &capacityWait) {
				goto retryUserGroupRoute
			}
			finishUserGroupCapacityWait(r.Context(), w)
			return true
		}
		writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", err.Error())
		return true
	}
	if len(plan.Candidates) == 0 {
		return false
	}

	replaySafe := !routing.HasServerSideState(r.URL.Path, r, resolvedRaw)
	streamRequest := isStreamRequest(resolvedRaw)
	units := buildUserGroupDispatchUnits(plan)
	allowBindingMigration := plan.PolicyTransfer
	for index := 0; index < len(units); index++ {
		unit := units[index]
		target := unit.Targets[0]
		moreUnits := index+1 < len(units)
		// A grouped unit retains a speculative probe until we know the downstream
		// handler consumed its scheduler context. Test/extension dispatchers that do
		// not select an account fall back to the remaining targets one by one.
		speculative := replaySafe && (moreUnits || len(unit.Targets) > 1)
		attempt := newUserGroupAttemptWriter(r.Context(), w, streamRequest, strings.HasPrefix(strings.TrimSpace(r.URL.Path), "/v1/responses"), bodysource.CaptureOptions{
			MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
			Budget: s.responseBodyBudget, DiskReserver: s.bodyDiskReserver, TempFileNamePrefix: "codex-pool-route-response-*",
		}, speculative)
		candidateContext := withUserGroupRouteOverride(r.Context(), target)
		stopCapacityHeartbeat := func() {}
		if capacityWait.started {
			// The outer response has already emitted a protocol heartbeat. Keep
			// that heartbeat running while a changed target is tried, but buffer
			// any nested scheduler failure so it cannot terminate the outer stream
			// before the full 10-minute takeover window expires.
			stopCapacityHeartbeat = startSchedulerWaitKeepalive(r.Context(), userGroupCapacityHeartbeatInterval)
			candidateContext = withBufferedSchedulerWait(candidateContext, attempt)
		}
		var choiceState *scheduler.RouteChoiceState
		if replaySafe && userGroupTargetsAreAccountPools(unit.Targets) {
			choices := make([]scheduler.RouteChoice, 0, len(unit.Targets))
			for _, choiceTarget := range unit.Targets {
				choices = append(choices, scheduler.RouteChoice{
					ChoiceKey: userGroupTargetChoiceKey(choiceTarget),
					Route:     scheduler.Route{Group: choiceTarget.ID},
				})
			}
			var claim scheduler.RouteChoiceClaim
			switch {
			case plan.Persist && !plan.Bound:
				claim = func(ctx context.Context, proposed string) (string, error) {
					proposedTarget, found := userGroupTargetForChoiceKey(unit.Targets, proposed)
					if !found {
						return "", fmt.Errorf("unknown proposed user-group target %q", proposed)
					}
					actual, _, err := s.store.ClaimUserGroupTargetBinding(ctx, storage.UserGroupTargetBinding{
						UserGroupID: plan.UserGroupID, AffinityKey: plan.AffinityKey,
						Model: plan.BindingModel, Target: proposedTarget,
					})
					return userGroupTargetChoiceKey(actual.Target), err
				}
			case plan.Persist && plan.Bound && userGroupTargetsContain(unit.Targets, plan.BoundTarget):
				claim = func(context.Context, string) (string, error) {
					return userGroupTargetChoiceKey(plan.BoundTarget), nil
				}
			case plan.Persist && plan.Bound && allowBindingMigration:
				expected := plan.BoundTarget
				claim = func(ctx context.Context, proposed string) (string, error) {
					proposedTarget, found := userGroupTargetForChoiceKey(unit.Targets, proposed)
					if !found {
						return "", fmt.Errorf("unknown migration target %q", proposed)
					}
					actual, swapped, err := s.store.CompareAndSwapUserGroupTargetBinding(ctx, expected, storage.UserGroupTargetBinding{
						UserGroupID: plan.UserGroupID, AffinityKey: plan.AffinityKey,
						Model: plan.BindingModel, Target: proposedTarget,
					})
					if err == nil && swapped {
						plan.BoundTarget = actual.Target
					}
					return userGroupTargetChoiceKey(actual.Target), err
				}
			}
			candidateContext, choiceState = scheduler.WithRouteChoices(candidateContext, choices, claim)
		}
		if replaySafe && (moreUnits || len(unit.Targets) > 1) {
			candidateContext = withUserGroupFallbackProbe(candidateContext)
		}
		candidate := r.Clone(candidateContext)
		// An account_pool_group keeps the caller's explicit provider hint and lets
		// that pool choose any capable account for it. A model_provider target is
		// itself the explicit provider decision, so it must override a conflicting
		// request header rather than silently executing another provider while the
		// user-group binding records this target.
		if target.Kind == storage.TargetKindModelProvider {
			_, providerHint, _ := targetRefToRoute(target)
			candidate.Header.Set("X-Pool-Provider", providerHint)
		}
		candidate.Body, candidate.GetBody = replayUserGroupRequestBody(r.Context(), originalRaw)
		candidate.ContentLength = int64(len(originalRaw))
		dispatch(attempt, candidate)
		stopCapacityHeartbeat()
		if candidate.Body != nil {
			_ = candidate.Body.Close()
		}

		if choiceState != nil && choiceState.Used() {
			if selected, found := userGroupTargetForChoiceKey(unit.Targets, choiceState.SelectedChoice()); found {
				target = selected
				if plan.Persist && !plan.Bound {
					plan.Bound = true
					plan.BoundTarget = selected
					if claimed, claimedFound := userGroupTargetForChoiceKey(unit.Targets, choiceState.ClaimedChoice()); claimedFound {
						plan.BoundTarget = claimed
					}
				}
			}
		}
		status := attempt.Status()
		if _, terminalFailure := attempt.TerminalFailure(); terminalFailure {
			detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, pol.UserGroupID, attempt)
			s.recordRouteAttempt(requestIDFromContext(r.Context()), unit.Tier, userGroupTargetDiagnosticName(target), unit.SelectionType, userGroupRouteStatusClass(attempt), "", detail)
			attempt.Commit()
			return true
		}
		if status < http.StatusBadRequest {
			// The client may close immediately after receiving the terminal frame.
			// Route/response bindings are durability work for the *next* turn, so
			// retain request values but detach cancellation and bound the write.
			bindingCtx, bindingCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			// Scheduler-backed pool migrations CAS before releasing the winning lease.
			// Provider targets have no lease boundary, so migrate only after the
			// replacement produced a successful response. A concurrent CAS winner is
			// retained, while this response ID is still aliased to the target that
			// actually produced it below.
			if plan.Persist && plan.Bound && allowBindingMigration && target != plan.BoundTarget {
				actual, _, migrationErr := s.store.CompareAndSwapUserGroupTargetBinding(bindingCtx, plan.BoundTarget, storage.UserGroupTargetBinding{
					UserGroupID: plan.UserGroupID, AffinityKey: plan.AffinityKey,
					Model: plan.BindingModel, Target: target,
				})
				if migrationErr != nil {
					log.Printf("[USER-GROUP-ROUTE] binding migration failed request_id=%s user_group=%s target_kind=%s target_id=%s model=%s: %v",
						requestIDFromContext(r.Context()), plan.UserGroupID, target.Kind, target.ID, plan.BindingModel, migrationErr)
				} else {
					plan.BoundTarget = actual.Target
				}
			}
			if bindingErr := commitUserGroupRouteBinding(bindingCtx, s.store, plan, target); bindingErr != nil {
				log.Printf("[USER-GROUP-ROUTE] binding persistence failed request_id=%s user_group=%s target_kind=%s target_id=%s model=%s: %v",
					requestIDFromContext(r.Context()), plan.UserGroupID, target.Kind, target.ID, plan.BindingModel, bindingErr)
			}
			// A client is allowed to continue with only previous_response_id. Bind the
			// successful terminal response hash to this exact target so that request
			// cannot rendezvous onto a different pool/provider on its next turn.
			if responseID := attempt.CompletedResponseID(); responseID != "" {
				aliasPlan := plan
				aliasPlan.Affinity = routing.ResponseAffinityKey(responseID)
				aliasPlan.AffinityKey = aliasPlan.Affinity.Hash
				aliasPlan.BindingModel = ""
				aliasPlan.Persist = true
				aliasPlan.Bound = false
				if bindingErr := commitUserGroupRouteBinding(bindingCtx, s.store, aliasPlan, target); bindingErr != nil {
					log.Printf("[USER-GROUP-ROUTE] response binding persistence failed request_id=%s user_group=%s target_kind=%s target_id=%s: %v",
						requestIDFromContext(r.Context()), plan.UserGroupID, target.Kind, target.ID, bindingErr)
				}
			}
			bindingCancel()
			detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, pol.UserGroupID, attempt)
			s.recordRouteAttempt(requestIDFromContext(r.Context()), unit.Tier, userGroupTargetDiagnosticName(target), unit.SelectionType, "success", "", detail)
			attempt.Commit()
			return true
		}
		if attempt.Committed() {
			detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, pol.UserGroupID, attempt)
			s.recordRouteAttempt(requestIDFromContext(r.Context()), unit.Tier, userGroupTargetDiagnosticName(target), unit.SelectionType, userGroupRouteStatusClass(attempt), "", detail)
			_ = attempt.Close()
			return true
		}
		retryable := attempt.RetryableFailure()
		// Preserve the legacy extension surface: when dispatch did not invoke the
		// scheduler, expand a grouped unit into its remaining individual targets.
		if retryable && replaySafe && choiceState != nil && !choiceState.Used() && len(unit.Targets) > 1 {
			extras := make([]userGroupDispatchUnit, 0, len(unit.Targets)-1)
			for _, fallback := range unit.Targets[1:] {
				extras = append(extras, userGroupDispatchUnit{Tier: unit.Tier, Targets: []storage.TargetRef{fallback}, SelectionType: "single"})
			}
			units = append(units[:index+1], append(extras, units[index+1:]...)...)
		}
		moreUnits = index+1 < len(units)
		fallbackName := ""
		if moreUnits && retryable && replaySafe {
			fallbackName = userGroupTargetDiagnosticName(units[index+1].Targets[0])
		}
		detail := userGroupRouteDiagnosticDetail(r, resolvedRaw, pol.UserGroupID, attempt)
		s.recordRouteAttempt(requestIDFromContext(r.Context()), unit.Tier, userGroupTargetDiagnosticName(target), unit.SelectionType, userGroupRouteStatusClass(attempt), fallbackName, detail)
		if !moreUnits || !replaySafe || !retryable {
			if !moreUnits && replaySafe && retryable {
				if s.dispatchUserGroupTrafficFallback(w, r, originalRaw, resolvedRaw, pol, dispatch) {
					_ = attempt.Close()
					return true
				}
				if attempt.PermanentTargetFailure() {
					_ = attempt.Close()
					if trafficFallbackProbe(r.Context()) {
						finishUserGroupCapacityWait(r.Context(), w)
						return true
					}
					if s.waitForUserGroupCapacityChange(r.Context(), pol, &capacityWait) {
						goto retryUserGroupRoute
					}
					finishUserGroupCapacityWait(r.Context(), w)
					return true
				}
			}
			attempt.Commit()
			return true
		}
		if plan.Bound && userGroupTargetsContain(unit.Targets, plan.BoundTarget) && attempt.PermanentTargetFailure() {
			allowBindingMigration = true
		}
		_ = attempt.Close()
	}
	return false
}

type userGroupDispatchUnit struct {
	Tier          int
	Targets       []storage.TargetRef
	SelectionType string
}

func buildUserGroupDispatchUnits(plan userGroupRoutePlan) []userGroupDispatchUnit {
	tiers := plan.Tiers
	if len(tiers) == 0 && len(plan.Candidates) > 0 {
		tiers = [][]storage.TargetRef{plan.Candidates}
	}
	units := make([]userGroupDispatchUnit, 0, len(plan.Candidates))
	for tierIndex, tier := range tiers {
		remaining := append([]storage.TargetRef(nil), tier...)
		if plan.Bound && userGroupTargetsContain(remaining, plan.BoundTarget) {
			units = append(units, userGroupDispatchUnit{Tier: tierIndex, Targets: []storage.TargetRef{plan.BoundTarget}, SelectionType: "bound"})
			filtered := remaining[:0]
			for _, target := range remaining {
				if target != plan.BoundTarget {
					filtered = append(filtered, target)
				}
			}
			remaining = filtered
		}
		poolTargets := make([]storage.TargetRef, 0, len(remaining))
		for _, target := range remaining {
			if target.Kind == storage.TargetKindAccountPoolGroup {
				poolTargets = append(poolTargets, target)
			}
		}
		poolsEmitted := false
		for _, target := range remaining {
			if target.Kind == storage.TargetKindAccountPoolGroup {
				if poolsEmitted {
					continue
				}
				selectionType := "pool"
				if len(poolTargets) > 1 {
					selectionType = "across"
				}
				units = append(units, userGroupDispatchUnit{Tier: tierIndex, Targets: poolTargets, SelectionType: selectionType})
				poolsEmitted = true
				continue
			}
			units = append(units, userGroupDispatchUnit{Tier: tierIndex, Targets: []storage.TargetRef{target}, SelectionType: "provider"})
		}
	}
	return units
}

func userGroupTargetsAreAccountPools(targets []storage.TargetRef) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		if target.Kind != storage.TargetKindAccountPoolGroup {
			return false
		}
	}
	return true
}

func userGroupTargetChoiceKey(target storage.TargetRef) string {
	return target.Kind + "\x00" + target.ID
}

func userGroupTargetForChoiceKey(targets []storage.TargetRef, key string) (storage.TargetRef, bool) {
	for _, target := range targets {
		if userGroupTargetChoiceKey(target) == key {
			return target, true
		}
	}
	return storage.TargetRef{}, false
}

func userGroupTargetDiagnosticName(target storage.TargetRef) string {
	return target.Kind + ":" + target.ID
}

func userGroupRouteDiagnosticDetail(r *http.Request, resolvedRaw []byte, userGroupID string, attempt *userGroupAttemptWriter) diagnosticRouteDetail {
	detail := diagnosticRouteDetail{
		TerminalErrorClass:            "none",
		EffectiveStatus:               attempt.Status(),
		SuperInstructClientChoice:     superInstructClientChoiceClass(r),
		SuperInstructEffectiveModules: "none",
		UserGroupID:                   strings.TrimSpace(userGroupID),
	}
	if r != nil {
		detail.SuperInstructEffectiveModules = superInstructEffectiveModules(requestUserGroupPolicy(r.Context()), routing.Model(resolvedRaw))
	}
	if failure, ok := attempt.TerminalFailure(); ok {
		if failure.StatusCode > 0 {
			detail.EffectiveStatus = failure.StatusCode
		}
		detail.TerminalErrorClass = safeTerminalErrorClass(failure)
	}
	return detail
}

func safeTerminalErrorClass(failure leakfilter.CodexFailureFrame) string {
	switch failure.ErrorCode {
	case "cyber_policy":
		return "cyber_policy"
	case string(leakfilter.ResponsesRequestErrorContextLengthExceeded):
		return "context_length_exceeded"
	}
	if failure.ContextError != leakfilter.ResponsesContextErrorNone {
		switch failure.ContextError {
		case leakfilter.ResponsesContextErrorOrphanedToolOutput:
			return "orphaned_tool_output"
		case leakfilter.ResponsesContextErrorEncryptedFunctionOutput:
			return "encrypted_function_output"
		case leakfilter.ResponsesContextErrorPreviousResponseNotFound:
			return "previous_response_not_found"
		}
	}
	if failure.BuiltinRetryable {
		return "retryable_terminal"
	}
	return "other_terminal"
}

func userGroupRouteStatusClass(attempt *userGroupAttemptWriter) string {
	if failure, ok := attempt.TerminalFailure(); ok {
		if failure.ErrorCode == "cyber_policy" {
			return "upstream_cyber_policy"
		}
		return "upstream_terminal_failure"
	}
	status := attempt.Status()
	if attempt.bodyErr != nil {
		return "local_response_storage"
	}
	if attempt.probeClass != "" {
		return attempt.probeClass
	}
	probe := attempt.probeBytes()
	switch {
	case bytes.Contains(probe, []byte("no available account")), bytes.Contains(probe, []byte("no_account")):
		return "no_account"
	case bytes.Contains(probe, []byte("rate_limit_cooldown")), bytes.Contains(probe, []byte("quota_cooldown")), bytes.Contains(probe, []byte("quota exhausted")):
		return "quota_cooldown"
	case bytes.Contains(probe, []byte("egress_saturated")), bytes.Contains(probe, []byte("egress unavailable")), bytes.Contains(probe, []byte("outlet capacity")):
		return "egress_saturated"
	case bytes.Contains(probe, []byte("transport_error")), bytes.Contains(probe, []byte("connection reset")), bytes.Contains(probe, []byte("connection refused")):
		return "transport"
	}
	switch {
	case status < http.StatusBadRequest:
		return "success"
	case status == http.StatusTooManyRequests:
		return "upstream_429"
	case status >= http.StatusInternalServerError:
		return "upstream_5xx"
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return "permanent_4xx"
	default:
		return "unknown"
	}
}

func (w *userGroupAttemptWriter) probeBytes() []byte {
	if w != nil && w.probe {
		return bytes.ToLower(append([]byte(nil), w.probeBody...))
	}
	if w == nil || w.body == nil || w.bodyErr != nil {
		return nil
	}
	r, err := w.body.Open()
	if err != nil {
		return nil
	}
	defer r.Close()
	payload, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return nil
	}
	return bytes.ToLower(payload)
}

func replayUserGroupRequestBody(ctx context.Context, original []byte) (io.ReadCloser, func() (io.ReadCloser, error)) {
	if source := bodySourceFromContext(ctx); source != nil && source.Size() == int64(len(original)) {
		if body, err := source.Open(); err == nil {
			return body, source.Open
		}
	}
	getBody := func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(original)), nil
	}
	body, _ := getBody()
	return body, getBody
}

type userGroupAttemptWriter struct {
	downstream     http.ResponseWriter
	header         http.Header
	status         int
	body           *bodysource.SpoolBuffer
	bodyErr        error
	probeBody      []byte
	probeReserved  int64
	probeTail      []byte
	probeRetryable bool
	probePermanent bool
	probeClass     string
	ctx            context.Context
	options        bodysource.CaptureOptions
	speculative    bool
	probe          bool
	probeTruncated bool
	committed      bool
	stream         bool
	responses      *responsesRouteAliasTracker
	terminal       *leakfilter.CodexFailureFrame
}

func newUserGroupAttemptWriter(ctx context.Context, downstream http.ResponseWriter, stream, trackResponses bool, options bodysource.CaptureOptions, speculative ...bool) *userGroupAttemptWriter {
	header := make(http.Header, len(downstream.Header()))
	copyUserGroupHeaders(header, downstream.Header())
	w := &userGroupAttemptWriter{downstream: downstream, header: header, stream: stream, ctx: ctx, options: options}
	if len(speculative) > 0 {
		w.speculative = speculative[0]
	}
	if stream && trackResponses {
		w.responses = &responsesRouteAliasTracker{}
	}
	return w
}

func (w *userGroupAttemptWriter) Header() http.Header { return w.header }

func (w *userGroupAttemptWriter) WriteHeader(status int) {
	if w.status != 0 || w.committed {
		return
	}
	w.status = status
}

func (w *userGroupAttemptWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= http.StatusBadRequest {
		return w.writeBuffered(payload)
	}
	if !w.stream {
		return w.writeBuffered(payload)
	}
	if w.responses != nil {
		_, _ = w.responses.Write(payload)
	}
	w.commitHeaders()
	return w.downstream.Write(payload)
}

func (w *userGroupAttemptWriter) writeBuffered(payload []byte) (int, error) {
	if w.bodyErr != nil {
		return 0, w.bodyErr
	}
	if w.speculative && w.Status() >= http.StatusBadRequest {
		return w.writeProbe(payload)
	}
	if w.body == nil {
		options := w.options
		w.body, w.bodyErr = bodysource.NewSpoolBuffer(w.ctx, options)
		if w.bodyErr != nil {
			return 0, w.bodyErr
		}
	}
	n, err := w.body.Write(payload)
	if err != nil {
		w.bodyErr = err
		w.status = http.StatusServiceUnavailable
		w.header.Set("Retry-After", "1")
		w.header.Set("Content-Type", "application/json")
	}
	return n, err
}

func (w *userGroupAttemptWriter) writeProbe(payload []byte) (int, error) {
	w.probe = true
	w.observeProbeMarkers(payload)
	remaining := int64(64<<10) - int64(len(w.probeBody))
	if remaining <= 0 {
		w.probeTruncated = true
		return len(payload), nil
	}
	accepted := payload
	if int64(len(accepted)) > remaining {
		accepted = accepted[:remaining]
		w.probeTruncated = true
	}
	if len(accepted) > 0 {
		if w.options.Budget != nil && !w.options.Budget.ReserveMemory(int64(len(accepted))) {
			// Status and the streaming marker classifier remain sufficient for
			// fallback; a saturated response budget never spills this probe to disk.
			w.probeTruncated = true
			return len(payload), nil
		}
		w.probeReserved += int64(len(accepted))
		w.probeBody = append(w.probeBody, accepted...)
	}
	return len(payload), nil
}

func (w *userGroupAttemptWriter) observeProbeMarkers(payload []byte) {
	markers := []struct {
		value     []byte
		class     string
		permanent bool
	}{
		{[]byte("no available account"), "no_account", true},
		{[]byte("no_account"), "no_account", true},
		{[]byte("capability_unavailable"), "no_account", true},
		{[]byte("claude_context_1m_unavailable"), "no_account", true},
		{[]byte("model_fallback_required"), "no_account", true},
		{[]byte("model_not_found"), "no_account", true},
		{[]byte("model_unsupported"), "no_account", true},
		{[]byte("bound_account_unavailable"), "no_account", true},
		{[]byte("rate_limit_cooldown"), "quota_cooldown", false},
		{[]byte("quota_cooldown"), "quota_cooldown", false},
		{[]byte("quota exhausted"), "quota_cooldown", false},
		{[]byte("egress_saturated"), "egress_saturated", false},
		{[]byte("egress unavailable"), "egress_saturated", false},
		{[]byte("transport_error"), "transport", false},
		{[]byte("temporarily unavailable"), "upstream_5xx", false},
	}
	maxMarker := 1
	for _, marker := range markers {
		if len(marker.value) > maxMarker {
			maxMarker = len(marker.value)
		}
	}
	for len(payload) > 0 {
		n := min(len(payload), 4096)
		window := make([]byte, 0, len(w.probeTail)+n)
		window = append(window, w.probeTail...)
		window = append(window, payload[:n]...)
		window = bytes.ToLower(window)
		for _, marker := range markers {
			if bytes.Contains(window, marker.value) {
				w.probeRetryable = true
				w.probePermanent = w.probePermanent || marker.permanent
				if w.probeClass == "" {
					w.probeClass = marker.class
				}
			}
		}
		keep := min(maxMarker-1, len(window))
		w.probeTail = append(w.probeTail[:0], window[len(window)-keep:]...)
		payload = payload[n:]
	}
}

func (w *userGroupAttemptWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= http.StatusBadRequest || !w.stream {
		return
	}
	w.commitHeaders()
	if flusher, ok := w.downstream.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *userGroupAttemptWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *userGroupAttemptWriter) Committed() bool { return w.committed }

func (w *userGroupAttemptWriter) RetryableFailure() bool {
	if retryableUserGroupTargetStatus(w.Status()) {
		return true
	}
	if w.probe {
		return w.probeRetryable
	}
	if w.body == nil || w.bodyErr != nil {
		return false
	}
	r, err := w.body.Open()
	if err != nil {
		return false
	}
	defer r.Close()
	return retryableUserGroupTargetFailure(w.Status(), r)
}

// PermanentTargetFailure is deliberately narrower than RetryableFailure. Quota,
// transient 5xx and outlet saturation may use another target for this replayable
// request, but they do not rewrite a durable conversation target. Only evidence
// that the target cannot execute this route permits compare-and-swap migration.
func (w *userGroupAttemptWriter) PermanentTargetFailure() bool {
	if w.Status() == http.StatusNotFound {
		return true
	}
	if w.probe {
		return w.probePermanent
	}
	if w.body == nil || w.bodyErr != nil {
		return false
	}
	r, err := w.body.Open()
	if err != nil {
		return false
	}
	defer r.Close()
	payload, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return false
	}
	payload = bytes.ToLower(payload)
	for _, marker := range [][]byte{
		[]byte("no available account"),
		[]byte("capability_unavailable"),
		[]byte("model_not_found"),
		[]byte("model_unsupported"),
		[]byte("bound_account_unavailable"),
	} {
		if bytes.Contains(payload, marker) {
			return true
		}
	}
	return false
}

func (w *userGroupAttemptWriter) CompletedResponseID() string {
	if w.responses != nil {
		w.responses.Finish()
		return w.responses.CompletedResponseID()
	}
	if w.Status() >= http.StatusBadRequest || w.body == nil || w.body.Size() == 0 || w.bodyErr != nil {
		return ""
	}
	meta, err := bodysource.ScanJSON(context.Background(), w.body, nil)
	if err != nil {
		return ""
	}
	var id, status string
	if json.Unmarshal(meta.Scalars["id"], &id) != nil || id == "" {
		return ""
	}
	_ = json.Unmarshal(meta.Scalars["status"], &status)
	if status != "" && !strings.EqualFold(status, "completed") {
		return ""
	}
	return strings.TrimSpace(id)
}

func (w *userGroupAttemptWriter) TerminalFailure() (leakfilter.CodexFailureFrame, bool) {
	if w == nil {
		return leakfilter.CodexFailureFrame{}, false
	}
	if w.terminal != nil {
		copy := *w.terminal
		copy.Header = w.terminal.Header.Clone()
		copy.Body = append([]byte(nil), w.terminal.Body...)
		return copy, true
	}
	if w.responses == nil {
		return leakfilter.CodexFailureFrame{}, false
	}
	return w.responses.TerminalFailure()
}

func (w *userGroupAttemptWriter) markCodexTerminalFailure(failure leakfilter.CodexFailureFrame) {
	if w == nil {
		return
	}
	copy := failure
	copy.Header = failure.Header.Clone()
	copy.Body = append([]byte(nil), failure.Body...)
	w.terminal = &copy
}

func markUserGroupCodexTerminalFailure(w http.ResponseWriter, failure leakfilter.CodexFailureFrame) {
	if marker, ok := w.(interface {
		markCodexTerminalFailure(leakfilter.CodexFailureFrame)
	}); ok {
		marker.markCodexTerminalFailure(failure)
	}
}

func (w *userGroupAttemptWriter) Commit() {
	if w.committed {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	var body io.ReadCloser
	if w.body != nil && w.body.Size() > 0 && w.bodyErr == nil {
		var err error
		body, err = w.body.Open()
		if err != nil {
			w.bodyErr = err
			w.status = http.StatusServiceUnavailable
			w.header.Set("Retry-After", "1")
			w.header.Set("Content-Type", "application/json")
		}
	}
	if w.probeTruncated {
		w.header.Set("X-Pool-Error-Probe-Truncated", "1")
	}
	w.commitHeaders()
	if w.bodyErr != nil {
		_, _ = io.WriteString(w.downstream, `{"error":{"type":"resource_exhausted","message":"response buffer exhausted"}}`)
	} else if body != nil {
		_, _ = io.Copy(w.downstream, body)
	} else if w.probe && len(w.probeBody) > 0 {
		_, _ = w.downstream.Write(w.probeBody)
	} else if w.probe {
		_, _ = io.WriteString(w.downstream, `{"error":{"type":"target_unavailable","message":"target failure response probe unavailable"}}`)
	}
	if body != nil {
		_ = body.Close()
	}
	_ = w.Close()
}

func (w *userGroupAttemptWriter) Close() error {
	if w.probe {
		if w.probeReserved > 0 && w.options.Budget != nil {
			w.options.Budget.ReleaseMemory(w.probeReserved)
		}
		w.probeReserved = 0
		w.probeBody = nil
		w.probeTail = nil
	}
	if w.body == nil {
		return nil
	}
	err := w.body.Close()
	w.body = nil
	return err
}

func (w *userGroupAttemptWriter) commitHeaders() {
	if w.committed {
		return
	}
	destination := w.downstream.Header()
	for key := range destination {
		destination.Del(key)
	}
	copyUserGroupHeaders(destination, w.header)
	w.downstream.WriteHeader(w.Status())
	w.committed = true
}

func copyUserGroupHeaders(destination, source http.Header) {
	for key, values := range source {
		destination[key] = append([]string(nil), values...)
	}
}

// responsesRouteAliasTracker keeps only enough SSE state to bind a successful
// response ID to its user-group target. Raw IDs remain in memory for this request;
// storage receives only the routing hash.
type responsesRouteAliasTracker struct {
	pending     []byte
	createdID   string
	completedID string
	failure     *leakfilter.CodexFailureFrame
}

func (t *responsesRouteAliasTracker) Write(payload []byte) (int, error) {
	t.pending = append(t.pending, payload...)
	for {
		boundary, separatorLen := sseFrameBoundary(t.pending)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		t.observe(t.pending[:frameEnd])
		t.pending = append(t.pending[:0], t.pending[frameEnd:]...)
	}
	if len(t.pending) > streamLedgerMaxPartialFrame {
		t.pending = t.pending[:0]
	}
	return len(payload), nil
}

func (t *responsesRouteAliasTracker) Finish() {
	if len(bytes.TrimSpace(t.pending)) > 0 {
		t.observe(t.pending)
	}
	t.pending = nil
}

func (t *responsesRouteAliasTracker) observe(frame []byte) {
	if failure, ok := leakfilter.ParseCodexFailureFrame(frame); ok {
		copy := failure
		t.failure = &copy
	}
	eventType, data := sseFrameEventData(frame)
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return
	}
	var envelope struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return
	}
	if strings.TrimSpace(envelope.Type) != "" {
		eventType = strings.TrimSpace(envelope.Type)
	}
	switch eventType {
	case "response.created":
		if id := strings.TrimSpace(envelope.Response.ID); id != "" {
			t.createdID = id
		}
	case "response.completed":
		t.completedID = firstNonEmpty(strings.TrimSpace(envelope.Response.ID), t.createdID)
	}
}

func (t *responsesRouteAliasTracker) CompletedResponseID() string {
	return strings.TrimSpace(t.completedID)
}

func (t *responsesRouteAliasTracker) TerminalFailure() (leakfilter.CodexFailureFrame, bool) {
	if t == nil {
		return leakfilter.CodexFailureFrame{}, false
	}
	t.Finish()
	if t.failure == nil {
		return leakfilter.CodexFailureFrame{}, false
	}
	copy := *t.failure
	copy.Header = t.failure.Header.Clone()
	copy.Body = append([]byte(nil), t.failure.Body...)
	return copy, true
}

func retryableUserGroupTargetStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly,
		http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError
}

func retryableUserGroupTargetFailure(status int, body io.Reader) bool {
	if retryableUserGroupTargetStatus(status) {
		return true
	}
	markers := [][]byte{
		[]byte("capability_unavailable"),
		[]byte("claude_context_1m_unavailable"),
		[]byte("model_fallback_required"),
		[]byte("model_not_found"),
		[]byte("bound_account_unavailable"),
		[]byte("no available account"),
		[]byte("temporarily unavailable"),
	}
	maxMarker := 0
	for _, marker := range markers {
		if len(marker) > maxMarker {
			maxMarker = len(marker)
		}
	}
	window := make([]byte, (32<<10)+maxMarker)
	tail := 0
	for {
		n, err := body.Read(window[tail:])
		end := tail + n
		for i := tail; i < end; i++ {
			if window[i] >= 'A' && window[i] <= 'Z' {
				window[i] += 'a' - 'A'
			}
		}
		for _, marker := range markers {
			if bytes.Contains(window[:end], marker) {
				return true
			}
		}
		if err != nil {
			return false
		}
		tail = min(maxMarker-1, end)
		copy(window[:tail], window[end-tail:end])
	}
}

func userGroupRuleMatchesModel(ruleModel, model string) bool {
	ruleModel = strings.ToLower(strings.TrimSpace(ruleModel))
	model = strings.ToLower(strings.TrimSpace(model))
	return ruleModel == "*" || ruleModel == model || (strings.HasSuffix(ruleModel, "*") && strings.HasPrefix(model, strings.TrimSuffix(ruleModel, "*")))
}

func userGroupTargetSupportsModel(ctx context.Context, store *storage.Store, target storage.TargetRef, model string) bool {
	if target.Kind == storage.TargetKindAccountPoolGroup || strings.TrimSpace(model) == "" {
		return true
	}
	switch target.ID {
	case "codex", "claude", "kiro", "antigravity":
		// Model names do not prove provider capability. Runtime-verified exact
		// account capabilities are authoritative, so auto routing must not discard
		// a built-in target before its scheduler gets to evaluate the model.
		return true
	}
	provider, ok, err := store.GetCustomProvider(ctx, target.ID)
	if err != nil || !ok || !provider.Enabled || len(provider.Models) == 0 {
		if err != nil || !ok || !provider.Enabled {
			return false
		}
		for source := range provider.ModelMappings {
			if userGroupRuleMatchesModel(source, model) {
				return true
			}
		}
		return len(provider.Models) == 0
	}
	for source := range provider.ModelMappings {
		if userGroupRuleMatchesModel(source, model) {
			return true
		}
	}
	for _, candidate := range provider.Models {
		if userGroupRuleMatchesModel(candidate, model) {
			return true
		}
	}
	return false
}

func compatibleUserGroupTiers(ctx context.Context, store *storage.Store, group storage.UserGroup, model string) ([][]storage.TargetRef, error) {
	tiers := [][]storage.TargetRef{group.Targets}
	for _, rule := range group.ModelRouting {
		if userGroupRuleMatchesModel(rule.Model, model) {
			tiers = rule.Tiers
			break
		}
	}
	selected := make(map[string]struct{}, len(group.Targets))
	for _, target := range group.Targets {
		selected[target.Kind+"\x00"+target.ID] = struct{}{}
	}
	out := make([][]storage.TargetRef, 0, len(tiers))
	for _, tier := range tiers {
		clean := make([]storage.TargetRef, 0, len(tier))
		seen := make(map[string]struct{}, len(tier))
		for _, target := range tier {
			normalized, err := storage.NormalizeTargetRef(target)
			if err != nil {
				return nil, err
			}
			key := normalized.Kind + "\x00" + normalized.ID
			if _, configured := selected[key]; !configured {
				return nil, fmt.Errorf("user group routing target %s/%s is not selected", normalized.Kind, normalized.ID)
			}
			if _, duplicate := seen[key]; duplicate ||
				blockedUserGroupTarget(group, normalized, model) ||
				!userGroupTargetSupportsModel(ctx, store, normalized, model) {
				continue
			}
			seen[key] = struct{}{}
			clean = append(clean, normalized)
		}
		if len(clean) > 0 {
			out = append(out, clean)
		}
	}
	return out, nil
}

func userGroupTiersForProvider(tiers [][]storage.TargetRef, provider string) [][]storage.TargetRef {
	provider = strings.ToLower(strings.TrimSpace(provider))
	filtered := make([][]storage.TargetRef, 0, len(tiers))
	for _, tier := range tiers {
		eligible := make([]storage.TargetRef, 0, len(tier))
		for _, target := range tier {
			if target.Kind == storage.TargetKindAccountPoolGroup {
				eligible = append(eligible, target)
				continue
			}
			_, routeProvider, err := targetRefToRoute(target)
			if err == nil && strings.EqualFold(routeProvider, provider) {
				eligible = append(eligible, target)
			}
		}
		if len(eligible) > 0 {
			filtered = append(filtered, eligible)
		}
	}
	return filtered
}

func orderUserGroupTiers(tiers [][]storage.TargetRef, seed string) []storage.TargetRef {
	return flattenUserGroupTiers(orderUserGroupTierSlices(tiers, seed))
}

func orderUserGroupTierSlices(tiers [][]storage.TargetRef, seed string) [][]storage.TargetRef {
	ordered := make([][]storage.TargetRef, 0, len(tiers))
	for tierIndex, tier := range tiers {
		copyTier := append([]storage.TargetRef(nil), tier...)
		sort.SliceStable(copyTier, func(i, j int) bool {
			return userGroupRendezvousRank(seed, tierIndex, copyTier[i]) > userGroupRendezvousRank(seed, tierIndex, copyTier[j])
		})
		ordered = append(ordered, copyTier)
	}
	return ordered
}

func flattenUserGroupTiers(tiers [][]storage.TargetRef) []storage.TargetRef {
	ordered := make([]storage.TargetRef, 0)
	for _, tier := range tiers {
		ordered = append(ordered, tier...)
	}
	return ordered
}

func userGroupRendezvousRank(seed string, tier int, target storage.TargetRef) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", seed, tier, target.Kind, target.ID)))
	return h.Sum64()
}

func targetRefToRoute(target storage.TargetRef) (string, string, error) {
	target, err := storage.NormalizeTargetRef(target)
	if err != nil {
		return "", "", err
	}
	if target.Kind == storage.TargetKindAccountPoolGroup {
		return target.ID, "", nil
	}
	switch target.ID {
	case "codex", "claude", "kiro", "antigravity":
		return "", target.ID, nil
	default:
		return "", "custom:" + target.ID, nil
	}
}

// userGroupTargetToRoute converts a UserGroupTarget to (group, providerHint).
func userGroupTargetToRoute(t storage.UserGroupTarget) (string, string) {
	switch t.TargetType {
	case storage.UserGroupTargetTypeKiro:
		return "", "kiro"
	case storage.UserGroupTargetTypeAntigravity:
		return "", "antigravity"
	case storage.UserGroupTargetTypeRelay:
		return "", "custom:" + t.TargetRef
	default: // base_group
		return t.TargetRef, ""
	}
}

// decodeUserGroupTarget reverses the encoding stored in AffinityBinding.Provider.
func decodeUserGroupTarget(encoded string) (string, string, error) {
	switch {
	case encoded == "kiro":
		return "", "kiro", nil
	case encoded == "antigravity":
		return "", "antigravity", nil
	case len(encoded) > 7 && encoded[:7] == "relay::":
		return "", "custom:" + encoded[7:], nil
	default:
		return encoded, "", nil
	}
}

// encodeUserGroupTarget produces the AffinityBinding.Provider value for a target.
func encodeUserGroupTarget(rg, rp string) string {
	switch {
	case rp == "kiro":
		return "kiro"
	case rp == "antigravity":
		return "antigravity"
	case len(rp) > 7 && rp[:7] == "custom:":
		return "relay::" + rp[7:]
	default:
		return rg
	}
}

// weightedSelectTarget picks a target using consistent hashing over affinity weights.
func weightedSelectTarget(targets []storage.UserGroupTarget, seed string) storage.UserGroupTarget {
	if len(targets) == 1 {
		return targets[0]
	}
	sorted := make([]storage.UserGroupTarget, len(targets))
	copy(sorted, targets)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	total := 0
	for _, t := range sorted {
		w := t.AffinityWeight
		if w < 1 {
			w = 1
		}
		total += w
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	pick := int(h.Sum32()) % total
	cum := 0
	for _, t := range sorted {
		w := t.AffinityWeight
		if w < 1 {
			w = 1
		}
		cum += w
		if pick < cum {
			return t
		}
	}
	return sorted[len(sorted)-1]
}

func fnvHashString(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%016x", h.Sum64())
}
