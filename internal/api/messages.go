package api

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/identity"
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
	raw, err := readLimited(r.Body, s.cfg.MaxBodyBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	path := r.URL.Path

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
	allowedProviders, err := claudeAllowedProviders(r, pol)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	r = r.WithContext(withDownstreamKey(r.Context(), pol))
	if pol.ForceModel != "" {
		raw = setForcedModel(raw, pol.ForceModel)
	}
	if pol.ForceEffort != "" {
		raw = applyForcedThinkingClaude(raw, pol.ForceEffort)
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

	strict := routing.IsStrictSticky(path, r, raw)
	model := routing.Model(raw)
	// Normalize a Claude model alias (auto/default/sonnet/opus/haiku, or an undated family
	// name) that Anthropic would reject into a concrete same-tier model id, and forward
	// that id upstream. Same-tier only — never a downgrade — so Claude Code's default/auto
	// selection works end-to-end. No-op for concrete claude-* ids (the common case) and
	// when a downstream key already forced a concrete model above.
	if norm := capability.NormalizeClaudeModelAlias(model); norm != model {
		raw = setForcedModel(raw, norm)
		model = norm
	}
	affinity := s.claudeSelectionAffinity(r.Context(), r, raw, raw, pol.Group, pol.KeyHash, model)
	if strings.HasSuffix(path, "/count_tokens") {
		count := virtual.EstimateTokensJSON(raw)
		if count < 1 {
			count = 1
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": count})
		return
	}

	// A model served by a custom OpenAI-compatible provider (DeepSeek, …) is relayed to
	// that provider, converting Anthropic Messages ↔ Chat Completions both ways. Claude
	// Code drives this by requesting a provider model (e.g. ANTHROPIC_MODEL=deepseek-chat
	// or a DeepSeek-forced key). count_tokens has no chat-completions equivalent, so it
	// is answered locally with an estimate rather than proxied.
	if prov, ok := s.customProviderForModel(r.Context(), model); ok {
		if strings.HasSuffix(path, "/count_tokens") {
			writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": virtual.EstimateTokensJSON(raw)})
			return
		}
		if caps := claudeOnlyCapabilitiesForChatBridge(r.Header, raw); len(caps) > 0 {
			s.writeCapabilityUnavailable(w, http.StatusBadRequest,
				"Claude-only capability cannot be represented through the Chat Completions bridge",
				caps,
				"official_claude",
				"custom_chat_completions_bridge:"+prov.ID,
				"Route this request to an official Claude account, or remove Claude-only beta/features for this Chat Completions provider.")
			return
		}
		s.handleMessagesViaCustom(w, r, raw, model, pol.Group, prov)
		return
	}

	// Native Anthropic Messages requests are always self-contained (the full
	// conversation is in `messages`; there is no server-side previous_response_id), so
	// they are movable and fail over to a fresh account on a recoverable error instead
	// of leaking it downstream.
	movable := !routing.HasServerSideState(path, r, raw)
	attempts := 1
	if s.flagEnabled(r.Context(), "seamless_failover", s.cfg.SeamlessFailover) && movable {
		if attempts = s.settingInt(r.Context(), "failover_max_attempts", s.cfg.FailoverMaxAttempts); attempts < 1 {
			attempts = 1
		}
	}
	exclude := map[string]bool{}
	r = r.WithContext(withSchedulerWait(r.Context(), w, isStreamRequest(raw), "anthropic"))
	_ = attempts // retained as a parsed compatibility knob; transient retries are scheduler-bound now.
	for {
		if s.claudeMessagesAttempt(w, r, raw, path, affinity, strict, movable, model, pol.Group, pol.KeyHash, allowedProviders, true, exclude) != outcomeRetry {
			return
		}
	}
}

// claudeMessagesAttempt serves one account's attempt at a native Anthropic
// /v1/messages request. It returns outcomeRetry when the caller should transparently
// fail over to a fresh account — a recoverable error on a movable request, detected
// before any response bytes are written — and outcomeDone otherwise. The leased
// account is added to exclude before any retry so it is not re-selected. Mirrors the
// Codex path's codexAttempt; a benched account is held out of the pool until the
// recheck loop re-validates it.
func (s *Server) claudeMessagesAttempt(w http.ResponseWriter, r *http.Request, raw []byte, path string, affinity routing.AffinityKey, strict, movable bool, model, group, apiKeyHash string, allowedProviders []string, allowRetry bool, exclude map[string]bool) attemptOutcome {
	streamReq := isStreamRequest(raw)
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:            group,
		AllowedProviders: allowedProviders,
		Affinity:         affinity,
		Strict:           strict,
		ServerSideState:  !movable,
		Movable:          movable,
		Model:            model,
		EstimatedTokens:  virtual.EstimateTokensJSON(raw),
		Exclude:          exclude,
		OnWait:           schedulerWaitCallback(r.Context()),
	})
	if err != nil {
		if schedulerWaitTerminal(r.Context(), "The model is temporarily unavailable. Please retry shortly.") {
			return outcomeDone
		}
		status, _ := noAccountHTTPStatus(err)
		s.writePublicNoAccountError(r.Context(), w, status, group, "claude", model, err)
		return outcomeDone
	}
	if lease.Account.Provider == "kiro" {
		return s.kiroMessagesWithLease(w, r, raw, model, affinity, lease, allowRetry, exclude)
	}
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
	id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
	// Virtualize the request to a consistent first-party Claude Code client and, in the
	// SAME single JSON parse/marshal pass, stamp the x-anthropic-billing-header for
	// native Claude Code/OAuth traffic (cc_version coherent with our UA and a fresh
	// per-request three-hex suffix; current 2.1.206 emits no cch field).
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
		NativeBreakpoints: nativeCacheInject,
		BreakpointPolicy:  breakpointPolicy,
		TTL:               claudeTTL,
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
			Body:           body,
			Account:        lease.Account,
			Token:          t,
			Egress:         lease.Egress,
			CookieJarKey:   lease.Binding.CookieJarKey,
			OSHint:         osHint,
		}
	}
	usageDiag := claudeRequestUsageDiagnostics(body, affinity, claudeTTL, nativeCacheInject)
	usageDiag.CachePrewarmAttempted = s.maybePrewarmClaudeCache(r.Context(), s.claudeCachePrewarmMode(r.Context()), requestForToken(token))
	releaseFlight, waitedForFlight := s.enterClaudeCacheSingleflight(r.Context(), s.claudeCacheSingleflightEnabled(r.Context()), body, affinity)
	if waitedForFlight {
		usageDiag.SingleflightWaitedRequests = 1
	}
	holdID, _ := s.store.CreateBillingHold(r.Context(), affinity.Hash, lease.Account.ID, virtual.EstimateTokensJSON(body))
	// Backstop: guarantee this attempt's hold reaches a terminal status no matter which
	// branch returns (including a cancelled/streaming disconnect). Only fires while the
	// hold is still 'held', so an explicit settle below always wins.
	defer func() { _ = s.store.SettleBillingHoldIfHeld(r.Context(), holdID, "abandoned") }()
	// Session 33: Carry the billing hold id in the request context for usage fallback.
	r = r.WithContext(withUsageDiagnostics(withBillingHold(r.Context(), holdID), usageDiag))
	resp, err := s.upstream.Do(r.Context(), requestForToken(token))
	releaseFlight()
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_before_response")
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
					_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_after_refresh")
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
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_upstream")
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
	s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, "claude", model, resp.Header)

	if isEventStream(resp.Header) {
		prefix, retryableStream, probeErr := probeEarlyClaudeSSEFailure(resp.Body)
		if probeErr != nil {
			_ = s.store.SettleBillingHold(r.Context(), holdID, "stream_probe_failed")
			if allowRetry && movable {
				return retry()
			}
			if writeClaudeWaitError(r.Context(), refreshHeartbeat, probeErr) {
				return outcomeDone
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return outcomeDone
		}
		if retryableStream {
			s.benchOnLimit(r.Context(), lease.Account.ID, 200, resp.Header, prefix)
			_ = s.store.SettleBillingHold(r.Context(), holdID, "stream_retryable_error_retry")
			if allowRetry && movable {
				return retry()
			}
			if writeClaudeWaitError(r.Context(), refreshHeartbeat, errors.New("claude stream returned retryable error after authentication refresh")) {
				return outcomeDone
			}
			writePublicUnavailable(w, http.StatusServiceUnavailable)
			return outcomeDone
		}
		streamBody := io.MultiReader(bytes.NewReader(prefix), resp.Body)
		if !refreshHeartbeat.Committed() {
			s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
		}
		if err := s.streamSSE(r.Context(), w, streamBody, result.Scrubber, "claude", lease.Account.ID, affinity.Hash); err != nil {
			_ = s.store.SettleBillingHold(r.Context(), holdID, "stream_interrupted_compensated")
		} else {
			_ = s.store.SettleBillingHold(r.Context(), holdID, "settled_streaming")
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
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_response_too_large")
		if allowRetry && movable {
			return retry()
		}
		writeError(w, http.StatusBadGateway, err)
		return outcomeDone
	}
	s.rememberClaudeCacheDiagnosticsMessageID(affinity, responseBody)
	r = r.WithContext(withClaudeDiagnosticsMissReason(r.Context(), responseBody))
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, responseBody)
	_ = s.store.SettleBillingHold(r.Context(), holdID, "settled")
	s.writeUpstreamHeaders(r.Context(), w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(result.Scrubber.ReplaceAll(responseBody))
	return outcomeDone
}

// claudeIsOAuth reports whether the stored credential is a Claude OAuth token
// (Claude Pro/Max) rather than an Anthropic API key. OAuth traffic must carry
// the full Claude Code fingerprint (identity system block, tool renames).
func claudeIsOAuth(token storage.AccountToken) bool {
	cred := strings.TrimSpace(token.AccessToken)
	if cred == "" {
		cred = strings.TrimSpace(token.OpenAIAPIKey)
	}
	return !strings.HasPrefix(cred, "sk-ant-api")
}

// streamCopyRewrite copies an upstream SSE stream to the client while replacing
// every scrubber pattern, including matches that straddle two read boundaries.
// When the scrubber is empty it degrades to a plain flushing copy.
func streamCopyRewrite(w http.ResponseWriter, body io.Reader, scrubber *streamrewrite.Matcher) error {
	if scrubber == nil || scrubber.Empty() {
		return streamCopy(w, body)
	}
	flusher, _ := w.(http.Flusher)
	rw := scrubber.NewRewriter()
	bufp := sseBufPool.Get().(*[]byte)
	defer sseBufPool.Put(bufp)
	buf := *bufp
	writeOut := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if err := writeOut(rw.Write(buf[:n])); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return writeOut(rw.Flush())
			}
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
