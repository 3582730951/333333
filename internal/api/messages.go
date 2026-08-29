package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/cloak"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/tokensave"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/virtual"
)

const kiroCompactionExtendedRouteThreshold int64 = 190000

// appendVirtualHomeToWords ensures the streamrewrite scrubber replaces the real
// home directory prefix with the virtual home directory (the value of `virtualHome`).
// We know the virtual home (it is the identity's HomeDir), but we DON'T know the
// downstream's real home — so the trick is to mark the virtual home as a sentinel
// and let cloak.normalizeSystemInfo already rewrite the *system prompt's* home to
// the virtual value in the request. For the RESPONSE stream, the model may echo
// back paths containing the virtual home we wrote; that's already virtual, so it
// is safe. What this helper guards against is the operator-configured real home
// in sensitive_words not matching the actual downstream home — so we also append
// the virtual home so the scrubber is at least no-op on it (never accidentally
// scrub the virtual value). The real replacement happens via the operator's
// sensitive_words list. This is a defensive no-op-ensuring helper.
func appendVirtualHomeToWords(words []string, virtualHome string) []string {
	if virtualHome == "" {
		return words
	}
	for _, w := range words {
		if w == virtualHome {
			return words
		}
	}
	// We do NOT add virtualHome as a sensitive word — scrubbing it would replace
	// the (already-virtual) value we wrote into the request, which is wrong.
	// This helper exists to be the extension point for future per-request real-home
	// discovery; for now it is a transparent pass-through that preserves the
	// existing operator-configured sensitive_words behavior.
	return words
}

// handleMessages is the native Anthropic relay endpoint (/v1/messages and
// /v1/messages/count_tokens). It selects a Claude-provider account from the
// pool, virtualizes the request to a consistent first-party Claude Code client
// (account-bound identity, tool-name normalization, sensitive-word scrubbing),
// forwards it upstream with the official Claude Code fingerprint, and scrubs the
// upstream response — including SSE, where the replacement is exhaustive even
// across chunk boundaries.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	raw, err := requestBodyBytes(r, s.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	originalRaw := raw
	path := r.URL.Path
	clientRequestedModel := routing.Model(raw)

	// Resolve the downstream api key's policy (routing group + forced model /
	// reasoning effort) and apply the overrides to the body BEFORE model
	// extraction and account selection — so the request both lands on an account
	// that has the forced model and is actually sent that model upstream. This
	// makes the native Anthropic path honor the same per-key/group policy as the
	// Codex and OpenAI-compat paths ("claude 也适用"); it also enforces
	// RequireDownstreamKey here, which this handler previously bypassed.
	pol, ok := s.resolveDownstreamPolicy(w, r)
	if !ok {
		return
	}
	r, ok = s.attachUserGroupPolicy(w, r, pol)
	if !ok {
		return
	}
	r = s.withIntelligentRoutingFallbacks(r, pol)
	r = r.WithContext(withDownstreamKey(r.Context(), pol))
	r = r.WithContext(withDownstreamClientScope(r.Context(), pol.KeyHash, r))
	r = r.WithContext(withGoalIdentityAliases(r.Context(), goalAliases(r, raw, "claude")))
	r = r.WithContext(withGoalOriginalBody(r.Context(), raw))
	if pol.ForceModel != "" {
		raw = setForcedModel(raw, pol.ForceModel)
	}
	if pol.ForceEffort != "" {
		raw = applyForcedThinkingClaude(raw, pol.ForceEffort)
	}
	if wrapped, wrappedReq, finish, enabled := s.maybeSuperInstructResponsePipeline(w, r, raw, routing.Model(raw)); enabled {
		w = wrapped
		r = wrappedReq
		defer finish()
	}
	if s.goalContinuityEnabled(r.Context()) {
		w.Header().Set("X-MiCliProxy-Context-Engine", "v2; build=goal-continuity-v2")
	}
	requestPolicy := requestUserGroupPolicy(r.Context())
	raw, err = s.applyModelInstructionsForEntrypoint(r.Context(), requestPolicy, routing.Model(raw), path, raw)
	if err != nil {
		writeCodexInstructionConfigurationError(w, err)
		return
	}
	policyResolvedModel := routing.Model(raw)
	r = r.WithContext(withModelDiagnostics(r.Context(), clientRequestedModel, policyResolvedModel, pol.ModelOverrideSource))
	w.Header().Set("X-Pool-Requested-Model", clientRequestedModel)
	w.Header().Set("X-Pool-Resolved-Model", policyResolvedModel)
	w.Header().Set("X-Pool-Model-Override-Source", firstNonEmpty(pol.ModelOverrideSource, "none"))
	if s.dispatchUserGroupRouteCandidates(w, r, originalRaw, raw, pol, s.handleMessages) {
		return
	}
	// Server-side token compression (opt-in, default OFF): conservatively compress large
	// tool-result blocks before forwarding upstream, cutting billed input tokens (the
	// rtk-style server-side analogue). Only tool_result content is touched.
	if s.flagEnabled(r.Context(), "token_save_enabled", s.cfg.TokenSaveEnabled) {
		if nb, saved := tokensave.CompressAnthropicToolResults(raw, tokensave.DefaultOptions()); saved > 0 {
			raw = nb
			log.Printf("[TOKEN-SAVE] /v1/messages: compressed tool results, ~%d bytes saved", saved)
		}
	}
	// Compliance: sanitize prior-turn history before forwarding (no effect on the
	// streamed reply). count_tokens runs through here too — harmless.
	raw = s.moderateHistory(r.Context(), raw, "anthropic")
	// Claude Code does not expose a literal `/goal resume` to the gateway.  Its
	// durable session correlator arrives on the next normal /v1/messages request.
	// A matching v2 checkpoint is rebuilt before account selection so the request can
	// move across accounts after restart; a first-use session simply has no match and
	// proceeds as a new goal.
	if s.goalContinuityEnabled(r.Context()) && !strings.HasSuffix(path, "/count_tokens") {
		replay := s.goalReplayBody(r.Context(), r, "claude", raw)
		switch replay.Kind {
		case goalResumeFound:
			finish, leaseErr := s.beginGoalRun(r.Context(), replay.Session.ID, "running")
			if leaseErr != nil {
				kind := goalResumeUnidentified
				if errors.Is(leaseErr, storage.ErrGoalInProgress) {
					kind = goalResumeInProgress
				}
				writeGoalResumeError(w, isStreamRequest(raw), "claude", kind, leaseErr.Error())
				return
			}
			defer finish()
			raw = replay.Body
			w.Header().Set("X-MiCliProxy-Context-Status", "rebuilt")
			w.Header().Set("X-MiCliProxy-Goal-Status", replay.Session.State)
		case goalResumeFamilyRestart:
			// Identifiers owned by a Responses-family goal (a prior turn of this same
			// session that was bridged to a chatgpt/codex account). The body carries
			// its own complete history, so the turn proceeds as a new Messages-family
			// goal instead of replaying incompatible history.
			w.Header().Set("X-MiCliProxy-Context-Status", "family-restart")
			w.Header().Set("X-MiCliProxy-Goal-Notice", "goal_resume_protocol_family_restart")
		case goalResumeAmbiguous, goalResumeRequiresToolResult, goalResumeStorageExhausted, goalResumeProtocolMismatch:
			writeGoalResumeError(w, isStreamRequest(raw), "claude", replay.Kind, replay.Reason)
			return
		}
	}

	strict := routing.IsStrictSticky(path, r, raw)
	model := routing.Model(raw)
	if pol.UserGroupID != "" {
		routeGroup, routeProvider, routeErr := resolveUserGroupRoute(r.Context(), s.store, pol, r, raw)
		if routeErr != nil {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "user_group_route_unavailable", routeErr.Error())
			return
		}
		if routeGroup != "" {
			pol.Group = routeGroup
		}
		if routeProvider != "" {
			pol.ProviderHint = routeProvider
		}
	}
	// Custom models and built-in GPT/Codex models are resolved before Claude-family
	// provider validation. Parse Claude-family suffixes early so a custom mapping can
	// match the base model (notably "[1m]"), but preserve every other custom model
	// literally: brackets are valid provider-owned model-name characters. A non-Claude
	// model that misses both custom and Codex routing is parsed later by the native
	// Claude path, retaining its original validation behavior.
	customRequestedModel := capability.RequestedClaudeModel{
		RequestedModel: model,
		BaseModel:      model,
	}
	customModelIsClaude := isClaudeModel(model)
	if customModelIsClaude {
		customRequestedModel, err = capability.ParseRequestedClaudeModel(model)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	localCountTokens := strings.HasSuffix(path, "/count_tokens")
	providerHint := effectiveGatewayProviderHint(r, pol)
	var selectedCustom storage.CustomProvider
	var selectedCustomOK bool
	explicitCustom := strings.HasPrefix(providerHint, "custom:")
	if explicitCustom {
		selectedCustom, selectedCustomOK = s.customProviderByID(r.Context(), strings.TrimPrefix(providerHint, "custom:"))
	} else if providerHint == "auto" {
		if localCountTokens {
			// Counting is a deterministic, provider-specific local estimate. It does
			// not consume an account or egress lease and must remain available while
			// the matching provider has no account, is cooling down, or is saturated.
			if providers := s.customProvidersForModel(r.Context(), customRequestedModel.BaseModel); len(providers) > 0 {
				selectedCustom, selectedCustomOK = providers[0], true
			}
		} else {
			// Real inference keeps the scheduler preflight so an unschedulable
			// provider cannot shadow a later custom or built-in route.
			selectedCustom, selectedCustomOK = s.customProviderForModel(r.Context(), customRequestedModel.BaseModel, pol.Group, raw)
		}
	}
	if explicitCustom && !selectedCustomOK {
		s.writeCapabilityUnavailable(w, http.StatusServiceUnavailable,
			"selected custom model provider is disabled or unavailable",
			[]string{"provider:" + strings.TrimPrefix(providerHint, "custom:"), "model:" + model},
			"enabled_custom_provider",
			providerHint,
			"Enable the selected model provider and import an active API-key account.")
		return
	}
	if selectedCustomOK {
		prov := selectedCustom
		if anthropicContext1MRequested(r.Header) {
			customRequestedModel.ContextMode = "1m"
		}
		if customRequestedModel.BaseModel != model {
			raw = setForcedModel(raw, customRequestedModel.BaseModel)
			model = customRequestedModel.BaseModel
			r = r.WithContext(withModelDiagnostics(r.Context(), clientRequestedModel, model, "claude_context_mode"))
			w.Header().Set("X-Pool-Resolved-Model", model)
			w.Header().Set("X-Pool-Model-Override-Source", "claude_context_mode")
		}
		r = r.WithContext(withRequestedClaudeModel(r.Context(), customRequestedModel))
		if mappedRaw, targetModel, mapped := applyCustomProviderModelMapping(prov, raw, model); mapped {
			raw = mappedRaw
			model = targetModel
			r = r.WithContext(withModelDiagnostics(r.Context(), clientRequestedModel, targetModel, "custom_provider_mapping:"+prov.ID))
			w.Header().Set("X-Pool-Resolved-Model", targetModel)
			w.Header().Set("X-Pool-Model-Override-Source", "custom_provider_mapping:"+prov.ID)
		}
		effectiveProv, _ := resolveLiveCustomProviderRoute(prov, storage.CustomProviderDownstreamMessages)
		if effectiveProv.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages &&
			strings.EqualFold(customRequestedModel.ContextMode, "1m") {
			r = withAnthropicContext1MBeta(r)
		}
		if localCountTokens {
			writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": countInputTokens(raw)})
			return
		}
		if effectiveProv.UpstreamProtocol == storage.CustomProviderProtocolChatCompletions {
			if caps := claudeOnlyCapabilitiesForChatBridge(r.Header, raw); len(caps) > 0 {
				s.writeCapabilityUnavailable(w, http.StatusBadRequest,
					"Claude-only capability cannot be represented through the Chat Completions bridge",
					caps,
					"official_claude",
					"custom_chat_completions_bridge:"+prov.ID,
					"Route this request to an official Claude account, or remove Claude-only beta/features for this Chat Completions provider.")
				return
			}
		}
		s.handleMessagesViaCustom(w, r, raw, model, pol.Group, prov)
		return
	}
	if providerHint == "codex" || (providerHint == "auto" && isCodexMessagesModel(model)) {
		s.handleMessagesViaCodex(w, r, raw, model)
		return
	}

	requestedModel := customRequestedModel
	if !customModelIsClaude {
		requestedModel, err = capability.ParseRequestedClaudeModel(model)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if anthropicContext1MRequested(r.Header) {
		requestedModel.ContextMode = "1m"
	}
	if requestedModel.BaseModel != model {
		raw = setForcedModel(raw, requestedModel.BaseModel)
		model = requestedModel.BaseModel
	}
	r = r.WithContext(withRequestedClaudeModel(r.Context(), requestedModel))
	allowedProviders, routeMode, err := s.resolveClaudeMessageProviders(r.Context(), r, pol)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	affinity := namespaceClaudeAffinity(s.claudeSelectionAffinity(r.Context(), r, raw, raw, pol.Group, pol.KeyHash, requestedModel.RequestedModel), routeMode, requestedModel.ContextMode)
	existingAffinity, affinityBindingErr := s.store.GetAffinityBinding(r.Context(), affinity.Hash)
	// Provider stickiness is valid only for the model that established it. An
	// explicit provider hint fixes the provider, not the old model: when Claude Code
	// changes /model on the same session, the scheduler must re-check the new exact
	// capability and may bind another account of that provider.
	affinityEstablished := affinity.Hash != "" && affinityBindingErr == nil && existingAffinity.Provider != "" && existingAffinity.Model != "" && existingAffinity.EgressID != "" && claudeRouteModelsEquivalent(existingAffinity.Model, model)
	// Once an auto-routed Claude session is bound to Kiro, preserve Kiro's
	// immutable-session guarantee. A later request must fail visibly if that exact
	// account is unavailable instead of switching identities behind Claude Code.
	boundKiro := affinityEstablished && strings.EqualFold(existingAffinity.Provider, "kiro")
	// Native Anthropic Messages requests are always self-contained (the full
	// conversation is in `messages`; there is no server-side previous_response_id), so
	// they are movable and fail over to a fresh account on a recoverable error instead
	// of leaking it downstream.
	movable := !routing.HasServerSideState(path, r, raw)
	attempts := 1
	if s.flagEnabled(r.Context(), "seamless_failover", s.cfg.SeamlessFailover) && movable && !pol.PinnedEgressNoFallback {
		if attempts = s.settingInt(r.Context(), "failover_max_attempts", s.cfg.FailoverMaxAttempts); attempts < 1 {
			attempts = 1
		}
	}
	if movable && !pol.PinnedEgressNoFallback && attempts < 2 {
		attempts = 2
	}
	exclude := map[string]bool{}
	modelCapabilityRejected := false
	r = r.WithContext(withSchedulerWait(r.Context(), w, isStreamRequest(raw), "anthropic"))
	for attempt := 1; attempt <= attempts; attempt++ {
		outcome := s.claudeMessagesAttempt(w, r, raw, path, affinity, strict, movable, model, pol.Group, pol.KeyHash, pol.PinnedEgressNoFallback, allowedProviders, routeMode == "kiro" || boundKiro, affinityEstablished, true, exclude)
		if outcome == outcomeModelRetry {
			modelCapabilityRejected = true
		}
		if outcome != outcomeRetry && outcome != outcomeModelRetry {
			return
		}
	}
	if modelCapabilityRejected && s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, true, pol.Group, strings.Join(allowedProviders, ","), requestedModel.RequestedModel, requestedModel.ContextMode) {
		return
	}
	writePoolCodeError(w, http.StatusBadGateway, "retry_exhausted", "upstream retry limit exhausted")
}

// claudeMessagesAttempt serves one account's attempt at a native Anthropic
// /v1/messages request. It returns outcomeRetry when the caller should transparently
// fail over to a fresh account — a recoverable error on a movable request, detected
// before any response bytes are written — and outcomeDone otherwise. The leased
// account is added to exclude before any retry so it is not re-selected. Mirrors the
// Codex path's codexAttempt; a benched account is held out of the pool until the
// recheck loop re-validates it.
func (s *Server) claudeMessagesAttempt(w http.ResponseWriter, r *http.Request, raw []byte, path string, affinity routing.AffinityKey, strict, movable bool, model, group, apiKeyHash string, pinnedNoFallback bool, allowedProviders []string, explicitKiro, affinityEstablished, allowRetry bool, exclude map[string]bool) attemptOutcome {
	streamReq := isStreamRequest(raw)
	countTokens := strings.HasSuffix(path, "/count_tokens")
	compaction := kirowire.IsClaudeCodeCompactionRequest(raw)
	routeContextMode := requestedClaudeModelFromContext(r.Context()).ContextMode
	// A large genuine Claude Code /compact request needs the selected Kiro account
	// to be 1M-eligible before conversion. Keep smaller compactions on the standard
	// route so Free/200K accounts can still summarize conversations that fit.
	if explicitKiro && compaction && virtual.EstimateTokensJSON(raw) >= kiroCompactionExtendedRouteThreshold {
		routeContextMode = "1m"
	}
	// User-group pinning extends the normal Kiro-only immutable affinity boundary
	// to every Claude provider.  The selected account/primary egress remains the
	// sole retry target; an unavailable binding is surfaced directly.
	immutableAffinity := pinnedNoFallback || (affinityEstablished && explicitKiro)
	if !movable && !affinityEstablished {
		writePoolCodeError(w, http.StatusConflict, "state_binding_missing", "request depends on server-side state but no persisted session binding exists")
		return outcomeDone
	}
	kiroCfg := s.effectiveKiroConfig(r.Context())
	// Kiro is a mandatory native-thinking path. Official Claude ignores this
	// scheduler flag; every Kiro candidate must support adaptive thinking.
	thinkingRequired := true
	if explicitKiro {
		if canonical, ok := capability.KiroCanonicalModel(model); ok && thinkingRequired && !capability.KiroSupportsAdaptiveThinking(canonical) {
			writeKiroError(w, r, http.StatusBadRequest, fmt.Errorf("%w: model %s", kirowire.ErrReasoningUnavailable, canonical))
			return outcomeDone
		}
	}
	route := scheduler.Route{
		Group:                 group,
		NoEgressFallback:      pinnedNoFallback,
		AllowedProviders:      allowedProviders,
		Affinity:              affinity,
		AffinityWait:          kiroAffinityWait(r.Context(), s, allowedProviders),
		Strict:                strict,
		ServerSideState:       !movable,
		ImmutableAffinity:     immutableAffinity,
		ExplicitProvider:      explicitKiro,
		ThinkingRequired:      thinkingRequired,
		KiroEndpointAllowlist: kiroCfg.KiroEndpointAllowlist,
		KiroDefaultRegion:     kiroCfg.KiroDefaultAPIRegion,
		Movable:               movable,
		Model:                 model,
		ContextMode:           routeContextMode,
		Compaction:            compaction,
		EstimatedTokens:       virtual.EstimateTokensJSON(raw),
		Exclude:               exclude,
		OnWait:                schedulerWaitCallback(r.Context()),
		SkipWait:              userGroupFallbackProbe(r.Context()),
	}
	lease, err := s.scheduler.Select(r.Context(), route)
	virtualContext1M := false
	standardContextFallback := false
	// Prefer an account that genuinely exposes 1M. When none is schedulable, keep
	// Claude Code's client-side 1M mode but route against the selected account's
	// normal window; the shared guard below asks the client to compact before that
	// real boundary. This makes the fallback explicit instead of sending a 1M beta
	// marker to an account that did not prove the entitlement.
	if err != nil && strings.EqualFold(routeContextMode, "1m") {
		fallbackRoute := route
		fallbackRoute.ContextMode = ""
		if fallbackLease, fallbackErr := s.scheduler.Select(r.Context(), fallbackRoute); fallbackErr == nil {
			lease, err = fallbackLease, nil
			standardContextFallback = true
			virtualContext1M = strings.EqualFold(requestedClaudeModelFromContext(r.Context()).ContextMode, "1m")
		}
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
			writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this session is unavailable")
			return outcomeDone
		}
		if s.handleCapabilitySelectionError(r.Context(), w, err, true, group, strings.Join(allowedProviders, ","), requestedClaudeModelFromContext(r.Context()).RequestedModel, routeContextMode) {
			return outcomeDone
		}
		if schedulerWaitTerminal(r.Context(), "The model is temporarily unavailable. Please retry shortly.") {
			return outcomeDone
		}
		status, _ := noAccountHTTPStatus(err)
		s.writePublicNoAccountError(r.Context(), w, status, group, strings.Join(allowedProviders, ","), model, err)
		return outcomeDone
	}
	resolvedForContext := firstNonEmpty(lease.ResolvedModel, model)
	if standardContextFallback {
		providerModel := requestedClaudeModelFromContext(r.Context())
		providerModel.ContextMode = ""
		r = r.WithContext(withRequestedClaudeModel(r.Context(), providerModel))
		r = withoutAnthropicContext1MBeta(r)
	}
	if virtualContext1M {
		w.Header().Set("X-MiCliProxy-Context-Mode", "virtual_1m")
	}
	if !countTokens && !compaction {
		if plan, compactRequired := s.selectedClaudeAutoCompactPlan(r.Context(), raw, lease, resolvedForContext, routeContextMode, virtualContext1M); compactRequired {
			lease.Release()
			writeClaudeAutoCompactRequired(w, plan)
			return outcomeDone
		}
	}
	if lease.Account.Provider == "kiro" {
		if countTokens {
			return s.kiroCountTokensWithLease(w, r, raw, affinity, lease)
		}
		return s.kiroMessagesWithLease(w, r, raw, model, affinity, lease, false, nil)
	}
	if lease.Account.Provider == "antigravity" {
		if countTokens {
			lease.Release()
			writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": countInputTokens(raw)})
			return outcomeDone
		}
		return s.antigravityMessagesWithLease(w, r, raw, model, lease, exclude)
	}
	resolvedModel := firstNonEmpty(lease.ResolvedModel, model)
	if resolvedModel != model {
		raw = setForcedModel(raw, resolvedModel)
		model = resolvedModel
	}
	w.Header().Set("X-Pool-Resolved-Provider", "claude")
	w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
	modelDiag := modelDiagnosticsFromCtx(r.Context())
	r = r.WithContext(withModelDiagnostics(r.Context(), modelDiag.Requested, resolvedModel, modelDiag.Source))
	leaseReleased := false
	releaseLease := func() {
		if leaseReleased {
			return
		}
		leaseReleased = true
		lease.Release()
	}
	defer releaseLease()
	retry := func() attemptOutcome {
		if exclude != nil {
			exclude[lease.Account.ID] = true
		}
		return outcomeRetry
	}

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return outcomeDone
	}
	refreshHeartbeat := newClaudeRefreshSSEHeartbeat(w, false)
	if !streamReq {
		refreshHeartbeat = nil
	}
	token, err = s.prepareClaudeTokenWithHeartbeat(r.Context(), lease.Account, token, "messages_preflight", refreshHeartbeat.beat)
	if err != nil {
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
			return outcomeDone
		}
		writeError(w, http.StatusBadGateway, err)
		return outcomeDone
	}

	osHint := s.osHint(raw, lease.Egress)
	id := s.virtualIdentity(r.Context(), lease.Account.ID, osHint)
	// Virtualize the request to a consistent first-party Claude Code client and, in the
	// SAME single JSON parse/marshal pass, stamp the x-anthropic-billing-header for
	// native Claude Code/OAuth traffic (cc_version coherent with our UA). The captured
	// 2.1.226–2.1.241 wire carries the real client's message-derived attribution suffix
	// (cc_version=<v>.<3-hex>; no cch — NATIVE_CLIENT_ATTESTATION is off), and the pool
	// mirrors it by DEFAULT (ClaudeAttributionFingerprint). A signed_custom manifest
	// attribution_suffix assertion ("live"/"plain") or the env flag flips it, so the
	// pool tracks a server-side attribution rollout without a recompile.
	// Folding the billing stamp in here avoids a second full unmarshal+marshal over what
	// is often a very large Claude Code request body.
	oauth := claudeIsOAuth(token)
	billingVer := s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
	// Build the sensitive-word list with the virtual home directory auto-injected,
	// so the real home directory prefix is replaced by the virtual one in both the
	// request body and the response stream — without the operator needing to
	// manually add it to sensitive_words. The virtual home is known here (from the
	// per-account identity), so we append it and let cloak.VirtualizeClaudeCode's
	// streamrewrite replace every occurrence of the real home prefix in the stream.
	// Project paths are deliberately NOT rewritten.
	wordsForClaude := s.cfg.SensitiveWordsFor("claude")
	wordsForClaude = appendVirtualHomeToWords(wordsForClaude, id.HomeDir)
	nativeCacheInject := s.nativeCacheBreakpointInjectEnabled(r.Context())
	claudeTTL := s.claudeCacheTTLForRoute(r.Context(), affinity)
	breakpointPolicy := s.claudeCacheBreakpointPolicy(r.Context(), affinity, lease.Account.ID, group, apiKeyHash)
	result := cloak.VirtualizeClaudeCodeWithCache(raw, id, wordsForClaude, oauth, billingVer, cloak.ClaudeCodeCacheOptions{
		NativeBreakpoints:      nativeCacheInject,
		BreakpointPolicy:       breakpointPolicy,
		TTL:                    claudeTTL,
		SessionHeaders:         r.Header,
		AttributionFingerprint: s.cfg.ClaudeAttributionFingerprintEnabled(),
	})
	body := result.Body
	if nativeCacheInject {
		body = prompt.EnsureAnthropicCacheControlWithOptions(body, prompt.AnthropicCacheControlOptions{
			TTL:                claudeTTL,
			Policy:             breakpointPolicy,
			LatestTailWrite:    s.claudeCacheLatestTailWriteEnabled(r.Context()),
			LosslessBlockSplit: s.claudeCacheLosslessBlockSplitEnabled(r.Context()),
		})
	}
	body = s.applyClaudeCacheDiagnostics(r.Context(), r.Header, body, affinity)

	// Anthropic is not behind a CF challenge wall the way chatgpt.com is, so we
	// call upstream directly rather than through the Codex CF-retry/egress-rotation
	// path — and below we only treat an actual interstitial challenge as CF, never
	// a normal API 4xx/429 (which still carries cf-ray/server: cloudflare).
	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         http.MethodPost,
			Provider:       "claude",
			DownstreamPath: path,
			Headers:        r.Header.Clone(),
			Body:           bodysource.Bytes(body),
			Account:        lease.Account,
			Token:          t,
			Egress:         lease.Egress,
			CookieJarKey:   lease.Binding.CookieJarKey,
			OSHint:         osHint,
		}
	}
	if countTokens {
		resp, requestErr, _ := s.doAccountCredentialRetry(r.Context(), lease.Account, movable && !strict, func() (*upstream.Response, error) {
			return s.upstream.Do(r.Context(), requestForToken(token))
		})
		if requestErr != nil {
			if allowRetry && movable {
				return retry()
			}
			writeError(w, http.StatusBadGateway, requestErr)
			return outcomeDone
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			errorBody := readUpstreamErrorBody(resp.Body)
			if claudeAuthError(resp.StatusCode, resp.Header, errorBody) && claudeTokenCanRefresh(token) {
				if refreshed, refreshErr := s.forceRefreshClaudeToken(r.Context(), lease.Account, "count_tokens_auth_error"); refreshErr == nil {
					token = refreshed
					resp.Body.Close()
					resp, requestErr = s.upstream.Do(r.Context(), requestForToken(token))
					if requestErr != nil {
						if allowRetry && movable {
							return retry()
						}
						writeError(w, http.StatusBadGateway, requestErr)
						return outcomeDone
					}
					defer resp.Body.Close()
					if resp.StatusCode >= 400 {
						errorBody = readUpstreamErrorBody(resp.Body)
					} else {
						errorBody = nil
					}
				}
			}
			if resp.StatusCode >= 400 {
				if isModelNotFoundError(resp.StatusCode, errorBody) {
					s.rejectAccountModel(r.Context(), lease.Account, model, resp.StatusCode)
					if allowRetry && movable {
						retry()
						return outcomeModelRetry
					}
					_ = s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, true, group, "claude", model, requestedClaudeModelFromContext(r.Context()).ContextMode)
					return outcomeDone
				}
				verdict := s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
				if allowRetry && movable && retryableForFailover(verdict, resp.StatusCode) {
					return retry()
				}
				s.writeFilteredError(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, result.Scrubber)
				return outcomeDone
			}
		}
		responseBody, readErr := s.readUpstreamResponseBody(resp.Body)
		if readErr != nil {
			if allowRetry && movable {
				return retry()
			}
			writeError(w, http.StatusBadGateway, readErr)
			return outcomeDone
		}
		s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header, lease.Trial)
		s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(result.Scrubber.ReplaceAll(responseBody))
		return outcomeDone
	}
	usageDiag := claudeRequestUsageDiagnostics(body, affinity, claudeTTL, nativeCacheInject)
	usageDiag.CachePrewarmAttempted = s.maybePrewarmClaudeCache(r.Context(), s.claudeCachePrewarmMode(r.Context()), requestForToken(token))
	releaseFlight, waitedForFlight := s.enterClaudeCacheSingleflight(r.Context(), s.claudeCacheSingleflightEnabled(r.Context()), lease.Account.ID, resolvedModel, body, affinity)
	if waitedForFlight {
		usageDiag.SingleflightWaitedRequests = 1
	}
	usageDiag.RouteEpoch = lease.RouteEpoch
	holdID := s.createBillingHold(r.Context(), affinity.Hash, lease.Account.ID, lease.RouteEpoch, virtual.EstimateTokensJSON(body))
	// Backstop: guarantee this attempt's hold reaches a terminal status no matter which
	// branch returns (including a cancelled/streaming disconnect). Only fires while the
	// hold is still 'held', so an explicit settle below always wins.
	defer func() { _ = s.settleBillingHoldIfHeld(r.Context(), holdID, "abandoned") }()
	// Session 33: Carry the billing hold id in the request context for usage fallback.
	r = r.WithContext(withUsageDiagnostics(withBillingHold(r.Context(), holdID), usageDiag))
	resp, err, _ := s.doAccountCredentialRetry(r.Context(), lease.Account, movable && !strict, func() (*upstream.Response, error) {
		return s.upstream.Do(r.Context(), requestForToken(token))
	})
	emptyEndTurnContinued := false
	releaseFlight()
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		if allowRetry && movable {
			return retry() // transport error — a fresh account/egress may succeed
		}
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
			return outcomeDone
		}
		writeError(w, http.StatusBadGateway, err)
		return outcomeDone
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errorBody := readUpstreamErrorBody(resp.Body)
		if claudeAuthError(resp.StatusCode, resp.Header, errorBody) && claudeTokenCanRefresh(token) {
			if refreshed, rerr := s.forceRefreshClaudeTokenWithHeartbeat(r.Context(), lease.Account, "auth_error", refreshHeartbeat.beat); rerr == nil {
				token = refreshed
				resp.Body.Close()
				resp, err = s.upstream.Do(r.Context(), requestForToken(token))
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "failed_after_refresh")
					if allowRetry && movable {
						return retry()
					}
					if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
						return outcomeDone
					}
					writeError(w, http.StatusBadGateway, err)
					return outcomeDone
				}
				defer resp.Body.Close()
				if resp.StatusCode < 400 {
					goto claudeSuccess
				}
				errorBody = readUpstreamErrorBody(resp.Body)
			} else {
				log.Printf("claude auth refresh %s: %v", lease.Account.ID, rerr)
				if writeClaudeWaitError(r.Context(), refreshHeartbeat, rerr) {
					return outcomeDone
				}
			}
		}
		if d := cf.Detect(resp.StatusCode, resp.Header, errorBody); cf.Recordable(d) {
			s.handleCFEvent(r.Context(), lease.Account, lease.Egress, resp.StatusCode, d)
		}
		if isModelNotFoundError(resp.StatusCode, errorBody) {
			s.rejectAccountModel(r.Context(), lease.Account, model, resp.StatusCode)
			_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
			if allowRetry && movable {
				retry()
				return outcomeModelRetry
			}
			_ = s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, true, group, "claude", model, requestedClaudeModelFromContext(r.Context()).ContextMode)
			return outcomeDone
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider:   "claude",
			Entrypoint: "claude_messages",
			Model:      model,
			Status:     resp.StatusCode,
			Header:     resp.Header,
			Body:       errorBody,
			Streaming:  isStreamRequest(raw),
		})
		var v ban.Verdict
		if ruleMatched {
			v = s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody, decision)
		} else {
			v = s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
		}
		_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
		if ruleMatched {
			switch decision.Match.DownstreamAction {
			case upstreamrules.DownstreamActionFailover:
				if allowRetry && movable {
					return retry()
				}
			case upstreamrules.DownstreamActionIdleStream:
				if isStreamRequest(raw) {
					resp.Body.Close()
					releaseLease()
				}
				if s.writeRuleDownstream(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, result.Scrubber, decision, isStreamRequest(raw)) {
					return outcomeDone
				}
			case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize:
				if s.writeRuleDownstream(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, result.Scrubber, decision, isStreamRequest(raw)) {
					return outcomeDone
				}
			}
		}
		// Recoverable error (rate limit / region / stale auth / ban) on a movable
		// request → move to a fresh account. No bytes have been written, so the
		// downstream never sees this.
		if allowRetry && movable && retryableForFailover(v, resp.StatusCode) {
			return retry()
		}
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, errors.New("claude upstream returned an error after authentication refresh")) {
			return outcomeDone
		}
		s.writeFilteredError(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, result.Scrubber)
		return outcomeDone
	}
claudeSuccess:
	s.verifyAccountModel(r.Context(), lease.Account, model, requestedClaudeModelFromContext(r.Context()).ContextMode)
	s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header, lease.Trial)
	s.captureQuota(r.Context(), lease.Account.ID, "claude", model, resp.Header)

	if isEventStream(resp.Header) {
		prefix, relayBody, earlyOutcome, probeErr := probeEarlyClaudeSSEWithIdleRelease(resp.Body, earlySSEIdleRelease)
		if probeErr != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "stream_probe_failed")
			if allowRetry && movable {
				return retry()
			}
			if writeClaudeWaitError(r.Context(), refreshHeartbeat, probeErr) {
				return outcomeDone
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return outcomeDone
		}
		if earlyOutcome == claudeEarlySSERetryableError {
			s.benchOnLimitForAccount(r.Context(), lease.Account, 200, resp.Header, prefix)
			_ = s.settleBillingHold(r.Context(), holdID, "stream_retryable_error_retry")
			if allowRetry && movable {
				return retry()
			}
			if writeClaudeWaitError(r.Context(), refreshHeartbeat, errors.New("claude stream returned retryable error after authentication refresh")) {
				return outcomeDone
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return outcomeDone
		}
		if earlyOutcome == claudeEarlySSEEmptyEndTurn {
			_ = resp.Body.Close()
			if !emptyEndTurnContinued {
				if continuationBody, ok := buildClaudeContinueBody(body, "", s.autoContinueText(r.Context())); ok {
					body = continuationBody
					emptyEndTurnContinued = true
					resp, err = s.upstream.Do(r.Context(), requestForToken(token))
					if err != nil {
						_ = s.settleBillingHold(r.Context(), holdID, "empty_end_turn_continuation_failed")
						if allowRetry && movable {
							return retry()
						}
						if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
							return outcomeDone
						}
						writePublicUnavailable(w, http.StatusServiceUnavailable)
						return outcomeDone
					}
					defer resp.Body.Close()
					if resp.StatusCode < http.StatusBadRequest {
						goto claudeSuccess
					}
					errorBody := readUpstreamErrorBody(resp.Body)
					verdict := s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errorBody)
					_ = s.settleBillingHold(r.Context(), holdID, "empty_end_turn_continuation_upstream_error")
					if allowRetry && movable && retryableForFailover(verdict, resp.StatusCode) {
						return retry()
					}
					s.writeFilteredError(r.Context(), w, "claude", resp.StatusCode, resp.Header, errorBody, result.Scrubber)
					return outcomeDone
				}
			}
			_ = s.settleBillingHold(r.Context(), holdID, "empty_end_turn_continuation_exhausted")
			if allowRetry && movable {
				return retry()
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return outcomeDone
		}
		streamBody := io.MultiReader(bytes.NewReader(prefix), relayBody)
		aliasCapture := &claudeAliasCapture{}
		captureWriter := io.Writer(aliasCapture)
		var goalCapture *bodysource.SpoolBuffer
		if s.goalContinuityEnabled(r.Context()) {
			goalCapture, err = bodysource.NewSpoolBuffer(r.Context(), s.responseBodyCaptureOptions(r.Context()))
			if err != nil {
				_ = resp.Body.Close()
				_ = s.settleBillingHold(r.Context(), holdID, "response_spool_exhausted")
				writeResourceExhausted(w, err.Error())
				return outcomeDone
			}
			defer goalCapture.Close()
			captureWriter = io.MultiWriter(aliasCapture, goalCapture)
		}
		streamBody = io.TeeReader(streamBody, captureWriter)
		// Tap the stream to detect a truncated response (no message_stop) and capture the
		// partial answer, so auto-continue can re-issue and stitch when enabled.
		continueTap := newClaudeStreamTap(r.Context(), s.responseBodyCaptureOptions(r.Context()))
		defer continueTap.Close()
		streamBody = io.TeeReader(streamBody, continueTap)
		if !refreshHeartbeat.Committed() {
			s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
		}
		streamCtx := r.Context()
		if rf := s.responseRuleFilter(streamCtx, "claude", "claude_messages", model, resp.StatusCode); rf != nil {
			if rf.Rule != nil && rf.Rule.FilterAccountAction {
				_ = s.applyRuleAccountAction(streamCtx, lease.Account, resp.StatusCode, resp.Header, nil, upstreamErrorRuleDecision{Rule: *rf.Rule, Match: upstreamrules.MatchResult{Rule: *rf.Rule, AccountAction: rf.Rule.AccountAction, DownstreamAction: rf.Rule.DownstreamAction}})
			}
			streamCtx = withResponseRuleFilter(streamCtx, rf)
		}
		streamErr := s.streamSSE(streamCtx, w, streamBody, result.Scrubber, "claude", lease.Account.ID, affinity.Hash)
		if errors.Is(streamErr, errUpstreamStreamStalled) {
			_ = resp.Body.Close()
			streamErr = nil
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{
				AccountID: lease.Account.ID, AccountLabel: lease.Account.Label,
				Action: "goal_stream_stall_detected", State: "recovering",
				Reason: "upstream_idle_without_terminal", Detail: "claude old stream cancelled before continuation",
			})
		}
		if streamErr != nil {
			_ = s.settleBillingHold(r.Context(), holdID, "stream_interrupted_compensated")
			if synthErr := closeClaudeStreamGracefully(w, continueTap.openBlock, continueTap.openBlockIndex); synthErr != nil {
				log.Printf("[GOAL-STREAM] claude interrupted terminal synthesis failed request_id=%s: %v", requestIDFromContext(r.Context()), synthErr)
			}
			_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "goal_stream_terminal_synthesized", State: "retryable", Reason: "upstream_stream_error", Detail: "claude error terminal emitted"})
			s.markGoalStreamRetryable(r.Context(), r, "claude", raw, "upstream_stream_error")
		} else {
			s.persistClaudeItemAliases(r.Context(), raw, aliasCapture.Bytes(), lease, model)
			if continueTap.completedSuccessfully() {
				var goalFrames []byte
				var captureErr error
				if goalCapture != nil {
					goalFrames, captureErr = responseSpoolBytes(goalCapture)
				}
				if captureErr == nil {
					if response := goalResponseFromSSE(goalFrames); len(response) > 0 {
						if _, persistErr := s.persistGoalContinuity(r.Context(), r, "claude", raw, response); persistErr != nil {
							log.Printf("[GOAL-CONTINUITY] claude stream persistence degraded request_id=%s: %v", requestIDFromContext(r.Context()), persistErr)
							s.auditGoalPersistenceDegraded(r.Context(), "claude_stream_terminal", persistErr)
						}
					}
				} else {
					log.Printf("[GOAL-CONTINUITY] claude stream capture unavailable request_id=%s: %v", requestIDFromContext(r.Context()), captureErr)
					s.auditGoalPersistenceDegraded(r.Context(), "claude_stream_capture", captureErr)
				}
			}
			_ = s.settleBillingHold(r.Context(), holdID, "settled_streaming")
			// Auto-continue on a truncated stream (no message_stop). Off by default, so
			// this block is skipped and the path is unchanged unless the operator opts in.
			if !continueTap.reachedTerminal() {
				rfForAC, _ := streamCtx.Value(responseRuleFilterKey{}).(*responseRuleFilter)
				// `ping` keepalives cover genuine long-poll silence.  An EOF without
				// message_stop is a closed upstream stream; v2 retries it once with the
				// same account and full self-contained history before emitting an error.
				if s.goalContinuityEnabled(r.Context()) || s.autoContinueEnabled(r.Context(), autoContinueDecisionFromFilter(rfForAC)) {
					sw := newScrubbingFrameWriter(w, s.leakScrubEnabled(r.Context()), result.Scrubber, "claude")
					continuationAliases := &claudeAliasCapture{}
					continuationWriter := io.Writer(continuationAliases)
					var continuationGoalCapture *bodysource.SpoolBuffer
					if goalCapture != nil {
						continuationGoalCapture, err = bodysource.NewSpoolBuffer(r.Context(), s.responseBodyCaptureOptions(r.Context()))
						if err != nil {
							log.Printf("[GOAL-CONTINUITY] claude continuation spool unavailable request_id=%s: %v", requestIDFromContext(r.Context()), err)
							_ = closeClaudeStreamGracefully(sw, continueTap.openBlock, continueTap.openBlockIndex)
							s.markGoalStreamRetryable(r.Context(), r, "claude", raw, "continuation_spool_unavailable")
							sw.Flush()
							return outcomeDone
						}
						defer continuationGoalCapture.Close()
						continuationWriter = io.MultiWriter(continuationAliases, continuationGoalCapture)
					}
					reissue := func(cctx context.Context, cbody []byte) (io.ReadCloser, error) {
						creq := requestForToken(token)
						creq.SetBodyBytes(cbody)
						cresp, cerr := s.upstream.Do(cctx, creq)
						if cerr != nil {
							return nil, cerr
						}
						if cresp.StatusCode >= 400 {
							if cresp.Body != nil {
								_ = cresp.Body.Close()
							}
							return nil, fmt.Errorf("claude continuation upstream status %d", cresp.StatusCode)
						}
						heartbeatEvery := s.streamKeepAliveInterval(cctx)
						if ruleInterval := responseRuleHeartbeatInterval(rfForAC); ruleInterval > 0 {
							heartbeatEvery = ruleInterval
						}
						return newSemanticSSERelayReadCloser(cctx, cresp.Body, rfForAC, "claude", s.streamStallRecoveryInterval(cctx), heartbeatEvery), nil
					}
					continuation, acErr := s.autoContinueClaude(r.Context(), sw, body, continueTap, reissue, continuationWriter)
					if continuation != nil {
						defer continuation.Close()
					}
					if acErr != nil {
						log.Printf("[AUTO-CONTINUE] claude request_id=%s: %v", requestIDFromContext(r.Context()), acErr)
						_ = closeClaudeStreamGracefully(sw, continueTap.openBlock, continueTap.openBlockIndex)
						s.markGoalStreamRetryable(r.Context(), r, "claude", raw, "continuation_unavailable")
					} else if continuation != nil && continuation.completedSuccessfully() {
						s.persistClaudeItemAliases(r.Context(), raw, continuationAliases.Bytes(), lease, model)
						if goalCapture != nil && continuationGoalCapture != nil {
							firstFrames, firstErr := responseSpoolBytes(goalCapture)
							continuationFrames, continuationErr := responseSpoolBytes(continuationGoalCapture)
							if captureErr := errors.Join(firstErr, continuationErr); captureErr != nil {
								log.Printf("[GOAL-CONTINUITY] claude continuation capture unavailable request_id=%s: %v", requestIDFromContext(r.Context()), captureErr)
								s.auditGoalPersistenceDegraded(r.Context(), "claude_continuation_capture", captureErr)
							} else if response := goalResponseFromSSEParts(firstFrames, continuationFrames); len(response) > 0 {
								if _, persistErr := s.persistGoalContinuity(r.Context(), r, "claude", raw, response); persistErr != nil {
									log.Printf("[GOAL-CONTINUITY] claude continuation persistence degraded request_id=%s: %v", requestIDFromContext(r.Context()), persistErr)
									s.auditGoalPersistenceDegraded(r.Context(), "claude_continuation_terminal", persistErr)
								}
							}
						}
					} else {
						s.markGoalStreamRetryable(r.Context(), r, "claude", raw, "upstream_eof_without_terminal")
					}
					sw.Flush()
				} else {
					if synthErr := closeClaudeStreamGracefully(w, continueTap.openBlock, continueTap.openBlockIndex); synthErr != nil {
						log.Printf("[GOAL-STREAM] claude terminal synthesis failed request_id=%s: %v", requestIDFromContext(r.Context()), synthErr)
					}
					_ = s.store.InsertAuditLog(r.Context(), storage.AuditLogRow{Action: "goal_stream_terminal_synthesized", State: "retryable", Reason: "upstream_eof_without_terminal", Detail: "claude error terminal emitted"})
					s.markGoalStreamRetryable(r.Context(), r, "claude", raw, "upstream_eof_without_terminal")
				}
			}
		}
		return outcomeDone
	}
	// Read the full upstream body BEFORE committing any status downstream: writing the
	// 200 first and only then reading means a mid-body upstream failure would leave the
	// client with a committed 200 followed by a pool error-envelope (a corrupt/truncated
	// response — "Failed to parse JSON" downstream). Since native /v1/messages is movable,
	// a pre-commit read failure fails over to a fresh account losslessly instead.
	responseBody, err := s.readUpstreamResponseBody(resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_response_too_large")
		if allowRetry && movable {
			return retry()
		}
		writeError(w, http.StatusBadGateway, err)
		return outcomeDone
	}
	s.rememberClaudeCacheDiagnosticsMessageID(affinity, responseBody)
	s.persistClaudeItemAliases(r.Context(), raw, responseBody, lease, model)
	if _, persistErr := s.persistGoalContinuity(r.Context(), r, "claude", raw, responseBody); persistErr != nil {
		log.Printf("[GOAL-CONTINUITY] claude persistence degraded request_id=%s: %v", requestIDFromContext(r.Context()), persistErr)
		s.auditGoalPersistenceDegraded(r.Context(), "claude_response_terminal", persistErr)
	}
	r = r.WithContext(withClaudeDiagnosticsMissReason(r.Context(), responseBody))
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
	_ = s.settleBillingHold(r.Context(), holdID, "settled")
	s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if rf := s.responseRuleFilter(r.Context(), "claude", "claude_messages", model, resp.StatusCode); rf != nil {
		responseBody = filterRuleJSON(responseBody, rf)
	}
	_, _ = w.Write(result.Scrubber.ReplaceAll(responseBody))
	return outcomeDone
}

// claudeIsOAuth reports whether the stored credential is a Claude OAuth token
// (Claude Pro/Max) rather than an Anthropic API key. OAuth traffic must carry
// the full Claude Code fingerprint (identity system block, tool renames).
func claudeIsOAuth(token storage.AccountToken) bool {
	return accountprovider.EffectiveAuthMethod("claude", token) == accountprovider.AuthMethodOAuth
}

// streamCopyRewrite copies an upstream SSE stream to the client while replacing
// every scrubber pattern, including matches that straddle two read boundaries.
// When the scrubber is empty it degrades to a plain flushing copy.
func streamCopyRewrite(w http.ResponseWriter, body io.Reader, scrubber *streamrewrite.Matcher) error {
	if scrubber == nil || scrubber.Empty() {
		return streamCopy(w, body)
	}
	rw := scrubber.NewRewriter()
	batch := newAdaptiveSSEBatch(w)
	bufp := sseBufPool.Get().(*[]byte)
	defer sseBufPool.Put(bufp)
	buf := *bufp
	writeOut := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		return batch.append(p)
	}
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if err := writeOut(rw.Write(buf[:n])); err != nil {
				batch.abort()
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				if err := writeOut(rw.Flush()); err != nil {
					return err
				}
				return batch.close()
			}
			batch.abort()
			return readErr
		}
	}
}

// identitySecret returns the identity secret resolved once at startup (see
// Server.identitySecretCached). It MUST match upstream.NewClient's resolution so the
// request-header identity and the response-stream scrub identity are the same — both
// call identity.ResolveSecret with the same configured value, which is deterministic.
func (s *Server) identitySecret() []byte {
	return s.identitySecretCached
}
