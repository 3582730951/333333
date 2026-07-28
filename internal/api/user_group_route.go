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

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

type userGroupRouteOverrideKey struct{}
type userGroupFallbackProbeKey struct{}

type userGroupRoutePlan struct {
	UserGroupID  string
	AffinityKey  string
	BindingModel string
	Candidates   []storage.TargetRef
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
	if err := commitUserGroupRouteBinding(ctx, store, plan, selected); err != nil {
		return "", "", err
	}
	return targetRefToRoute(selected)
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
		return userGroupRoutePlan{}, fmt.Errorf("user group %s has no target compatible with model %q", group.ID, model)
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
	ordered := orderUserGroupTiers(tiers, seed)
	plan := userGroupRoutePlan{
		UserGroupID: group.ID,
		AffinityKey: affinityKeyObj.Hash,
		Candidates:  ordered,
	}

	// A root/session binding is shared by the main CLI and its child agents. Only
	// create a model-scoped exception when that target cannot serve the child model.
	if affinityKeyObj.Hash != "" {
		base, found, bindingErr := store.GetUserGroupTargetBinding(ctx, group.ID, affinityKeyObj.Hash, "")
		if bindingErr != nil {
			return userGroupRoutePlan{}, bindingErr
		}
		if found && userGroupTargetsContain(ordered, base.Target) {
			plan.Candidates = prioritizeUserGroupTarget(ordered, base.Target)
			return plan, nil
		}
		if found {
			plan.BindingModel = model
			exception, exceptionFound, exceptionErr := store.GetUserGroupTargetBinding(ctx, group.ID, affinityKeyObj.Hash, model)
			if exceptionErr != nil {
				return userGroupRoutePlan{}, exceptionErr
			}
			if exceptionFound && userGroupTargetsContain(ordered, exception.Target) {
				plan.Candidates = prioritizeUserGroupTarget(ordered, exception.Target)
			}
		}
	}
	return plan, nil
}

func commitUserGroupRouteBinding(ctx context.Context, store *storage.Store, plan userGroupRoutePlan, selected storage.TargetRef) error {
	if plan.UserGroupID == "" || plan.AffinityKey == "" {
		return nil
	}
	return store.UpsertUserGroupTargetBinding(ctx, storage.UserGroupTargetBinding{
		UserGroupID: plan.UserGroupID,
		AffinityKey: plan.AffinityKey,
		Model:       plan.BindingModel,
		Target:      selected,
	})
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
	plan, err := resolveUserGroupRouteCandidates(r.Context(), s.store, pol, r, resolvedRaw)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", err.Error())
		return true
	}
	if len(plan.Candidates) == 0 {
		return false
	}

	replaySafe := !routing.HasServerSideState(r.URL.Path, r, resolvedRaw)
	streamRequest := isStreamRequest(resolvedRaw)
	for index, target := range plan.Candidates {
		attempt := newUserGroupAttemptWriter(r.Context(), w, streamRequest, strings.HasPrefix(strings.TrimSpace(r.URL.Path), "/v1/responses"), bodysource.CaptureOptions{
			MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
			MinDiskFreeBytes: s.cfg.BodyDiskReserveBytes, Budget: s.bodyBudget, TempFileNamePrefix: "codex-pool-route-response-*",
		})
		candidateContext := withUserGroupRouteOverride(r.Context(), target)
		moreTargets := index+1 < len(plan.Candidates)
		if replaySafe && moreTargets {
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
		candidate.Body = io.NopCloser(bytes.NewReader(originalRaw))
		candidate.ContentLength = int64(len(originalRaw))
		dispatch(attempt, candidate)

		status := attempt.Status()
		if status < http.StatusBadRequest {
			if bindingErr := commitUserGroupRouteBinding(r.Context(), s.store, plan, target); bindingErr != nil {
				log.Printf("[USER-GROUP-ROUTE] binding persistence failed request_id=%s user_group=%s target_kind=%s target_id=%s model=%s: %v",
					requestIDFromContext(r.Context()), plan.UserGroupID, target.Kind, target.ID, plan.BindingModel, bindingErr)
			}
			// A client is allowed to continue with only previous_response_id. Bind the
			// successful terminal response hash to this exact target so that request
			// cannot rendezvous onto a different pool/provider on its next turn.
			if responseID := attempt.CompletedResponseID(); responseID != "" {
				aliasPlan := plan
				aliasPlan.AffinityKey = routing.ResponseAffinityKey(responseID).Hash
				aliasPlan.BindingModel = ""
				if bindingErr := commitUserGroupRouteBinding(r.Context(), s.store, aliasPlan, target); bindingErr != nil {
					log.Printf("[USER-GROUP-ROUTE] response binding persistence failed request_id=%s user_group=%s target_kind=%s target_id=%s: %v",
						requestIDFromContext(r.Context()), plan.UserGroupID, target.Kind, target.ID, bindingErr)
				}
			}
			attempt.Commit()
			return true
		}
		if attempt.Committed() {
			_ = attempt.Close()
			return true
		}
		if !moreTargets || !replaySafe || !attempt.RetryableFailure() {
			attempt.Commit()
			return true
		}
		_ = attempt.Close()
	}
	return false
}

type userGroupAttemptWriter struct {
	downstream http.ResponseWriter
	header     http.Header
	status     int
	body       *bodysource.SpoolBuffer
	bodyErr    error
	committed  bool
	stream     bool
	responses  *responsesRouteAliasTracker
}

func newUserGroupAttemptWriter(ctx context.Context, downstream http.ResponseWriter, stream, trackResponses bool, options bodysource.CaptureOptions) *userGroupAttemptWriter {
	header := make(http.Header, len(downstream.Header()))
	copyUserGroupHeaders(header, downstream.Header())
	body, err := bodysource.NewSpoolBuffer(ctx, options)
	w := &userGroupAttemptWriter{downstream: downstream, header: header, stream: stream, body: body, bodyErr: err}
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
	n, err := w.body.Write(payload)
	if err != nil {
		w.bodyErr = err
		w.status = http.StatusServiceUnavailable
		w.header.Set("Retry-After", "1")
		w.header.Set("Content-Type", "application/json")
	}
	return n, err
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
	if w.body == nil || w.bodyErr != nil {
		return false
	}
	if retryableUserGroupTargetStatus(w.Status()) {
		return true
	}
	r, err := w.body.Open()
	if err != nil {
		return false
	}
	defer r.Close()
	return retryableUserGroupTargetFailure(w.Status(), r)
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
	w.commitHeaders()
	if w.bodyErr != nil {
		_, _ = io.WriteString(w.downstream, `{"error":{"type":"resource_exhausted","message":"response buffer exhausted"}}`)
	} else if body != nil {
		_, _ = io.Copy(w.downstream, body)
	}
	if body != nil {
		_ = body.Close()
	}
	_ = w.Close()
}

func (w *userGroupAttemptWriter) Close() error {
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
		return err == nil && ok && provider.Enabled
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
			if _, duplicate := seen[key]; duplicate || !userGroupTargetSupportsModel(ctx, store, normalized, model) {
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
	ordered := make([]storage.TargetRef, 0)
	for tierIndex, tier := range tiers {
		copyTier := append([]storage.TargetRef(nil), tier...)
		sort.SliceStable(copyTier, func(i, j int) bool {
			return userGroupRendezvousRank(seed, tierIndex, copyTier[i]) > userGroupRendezvousRank(seed, tierIndex, copyTier[j])
		})
		ordered = append(ordered, copyTier...)
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
