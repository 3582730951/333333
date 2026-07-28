package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
)

type chatClaudeModelRetryKey struct{}

func chatClaudeModelRetryCount(ctx context.Context) int {
	count, _ := ctx.Value(chatClaudeModelRetryKey{}).(int)
	return count
}

// isClaudeModel reports whether a model id targets Anthropic Claude, so an
// OpenAI-compatible /v1/chat/completions request can be transparently relayed to
// the Claude upstream instead of Codex.
func isClaudeModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude")
}

// handleChatViaClaude serves an OpenAI Chat Completions request against a Claude
// account: it converts the request to Anthropic Messages, virtualizes it, relays
// it with the Claude Code fingerprint, and converts the response back to OpenAI
// shape — both for non-streaming JSON and for streaming (Anthropic SSE events →
// chat.completion.chunk SSE). Scrubbing is applied throughout.
func (s *Server) handleChatViaClaude(w http.ResponseWriter, r *http.Request, raw []byte, model string, pol downstreamPolicy) {
	stream := isStreamRequest(raw)
	anthBody, err := prompt.ChatCompletionToAnthropic(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	routeGroup := pol.Group
	requestedModel, err := capability.ParseRequestedClaudeModel(model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if anthropicContext1MRequested(r.Header) {
		requestedModel.ContextMode = "1m"
	}
	if requestedModel.BaseModel != model {
		anthBody = setForcedModel(anthBody, requestedModel.BaseModel)
		model = requestedModel.BaseModel
	}
	r = r.WithContext(withRequestedClaudeModel(r.Context(), requestedModel))
	allowedProviders, routeMode, err := s.resolveClaudeProviders(r.Context(), r, pol)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	r = r.WithContext(withSchedulerWait(r.Context(), w, stream, "openai"))
	affinity := namespaceClaudeAffinity(s.claudeSelectionAffinity(r.Context(), r, raw, anthBody, routeGroup, pol.KeyHash, requestedModel.RequestedModel), routeMode, requestedModel.ContextMode)
	existingAffinity, affinityBindingErr := s.store.GetAffinityBinding(r.Context(), affinity.Hash)
	affinityEstablished := affinity.Hash != "" && affinityBindingErr == nil && existingAffinity.Provider != "" && existingAffinity.Model != "" && existingAffinity.EgressID != ""
	exclude := map[string]bool{}
	explicitKiro := routeMode == "kiro"
	immutableAffinity := affinityEstablished && explicitKiro
	kiroCfg := s.effectiveKiroConfig(r.Context())
	// Kiro is never allowed to run with thinking disabled. The flag is harmless
	// for official Claude candidates and filters Kiro to thinking-capable models.
	thinkingRequired := true
	var lease scheduler.Lease
	for {
		lease, err = s.scheduler.Select(r.Context(), scheduler.Route{
			Group:                 routeGroup,
			AllowedProviders:      allowedProviders,
			Affinity:              affinity,
			AffinityWait:          kiroAffinityWait(r.Context(), s, allowedProviders),
			ImmutableAffinity:     immutableAffinity,
			ExplicitProvider:      explicitKiro,
			ThinkingRequired:      thinkingRequired,
			KiroEndpointAllowlist: kiroCfg.KiroEndpointAllowlist,
			KiroDefaultRegion:     kiroCfg.KiroDefaultAPIRegion,
			Model:                 model,
			ContextMode:           requestedModel.ContextMode,
			EstimatedTokens:       virtual.EstimateTokensJSON(raw),
			Exclude:               exclude,
			OnWait:                schedulerWaitCallback(r.Context()),
			SkipWait:              userGroupFallbackProbe(r.Context()),
		})
		if err != nil {
			if errors.Is(err, scheduler.ErrBoundAccountUnavailable) {
				writePoolCodeError(w, http.StatusConflict, "bound_account_unavailable", "the account bound to this session is unavailable")
				return
			}
			if s.handleCapabilitySelectionError(r.Context(), w, err, false, routeGroup, strings.Join(allowedProviders, ","), requestedModel.RequestedModel, requestedModel.ContextMode) {
				return
			}
			if schedulerWaitTerminal(r.Context(), "The model is temporarily unavailable. Please retry shortly.") {
				return
			}
			status, _ := noAccountHTTPStatus(err)
			s.writePublicNoAccountError(r.Context(), w, status, routeGroup, strings.Join(allowedProviders, ","), model, err)
			return
		}
		if lease.Account.Provider != "kiro" {
			break
		}
		s.kiroChatWithLease(w, r, anthBody, model, affinity, lease, false, nil)
		return
	}
	resolvedModel := firstNonEmpty(lease.ResolvedModel, model)
	if resolvedModel != model {
		anthBody = setForcedModel(anthBody, resolvedModel)
		model = resolvedModel
	}
	w.Header().Set("X-Pool-Resolved-Provider", "claude")
	w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
	leaseReleased := false
	releaseLease := func() {
		if leaseReleased {
			return
		}
		leaseReleased = true
		lease.Release()
	}
	defer releaseLease()

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	refreshHeartbeat := newClaudeRefreshSSEHeartbeat(w, true)
	if !stream {
		refreshHeartbeat = nil
	}
	token, err = s.prepareClaudeTokenWithHeartbeat(r.Context(), lease.Account, token, "chat_claude_preflight", refreshHeartbeat.beat)
	if err != nil {
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	osHint := s.osHint(raw, lease.Egress)
	id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
	cacheInject := s.cacheInjectEnabled(r.Context())
	claudeTTL := s.claudeCacheTTLForRoute(r.Context(), affinity)
	breakpointPolicy := s.claudeCacheBreakpointPolicy(r.Context(), affinity, lease.Account.ID, routeGroup, pol.KeyHash)
	result := cloak.VirtualizeClaudeCodeWithCache(anthBody, id, s.cfg.SensitiveWordsFor("claude"), claudeIsOAuth(token), "", cloak.ClaudeCodeCacheOptions{TTL: claudeTTL})
	// The OpenAI→Anthropic conversion emits no cache_control, so without this the
	// compat path is billed at full input price every turn. Inject the standard
	// Claude Code breakpoints on stable prefixes (system/tools/history, ≤4) so it
	// caches like native Claude Code. Quality/reasoning unchanged; only cached-token
	// cost drops.
	if cacheInject {
		result.Body = prompt.EnsureAnthropicCacheControlWithOptions(result.Body, prompt.AnthropicCacheControlOptions{
			TTL:                claudeTTL,
			Policy:             breakpointPolicy,
			LatestTailWrite:    s.claudeCacheLatestTailWriteEnabled(r.Context()),
			LosslessBlockSplit: s.claudeCacheLosslessBlockSplitEnabled(r.Context()),
		})
	}
	// Final body step (after cache_control injection): stamp the Claude Code
	// x-anthropic-billing-header so OAuth traffic relayed from an OpenAI-compatible
	// client still carries the header every genuine Claude Code request has, with a
	// cc_version coherent with our User-Agent and a fresh three-hex suffix (no cch).
	if claudeIsOAuth(token) {
		result.Body = cloak.EnsureClaudeCodeBillingHeader(result.Body, s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion))
	}
	result.Body = s.applyClaudeCacheDiagnostics(r.Context(), r.Header, result.Body, affinity)

	requestForToken := func(t storage.AccountToken) upstream.Request {
		return upstream.Request{
			Method:         http.MethodPost,
			Provider:       "claude",
			DownstreamPath: "/v1/messages",
			Headers:        r.Header.Clone(),
			Body:           bodysource.Bytes(result.Body),
			Account:        lease.Account,
			Token:          t,
			Egress:         lease.Egress,
			CookieJarKey:   lease.Binding.CookieJarKey,
			OSHint:         osHint,
		}
	}
	usageDiag := claudeRequestUsageDiagnostics(result.Body, affinity, claudeTTL, cacheInject)
	usageDiag.CachePrewarmAttempted = s.maybePrewarmClaudeCache(r.Context(), s.claudeCachePrewarmMode(r.Context()), requestForToken(token))
	releaseFlight, waitedForFlight := s.enterClaudeCacheSingleflight(r.Context(), s.claudeCacheSingleflightEnabled(r.Context()), lease.Account.ID, resolvedModel, result.Body, affinity)
	if waitedForFlight {
		usageDiag.SingleflightWaitedRequests = 1
	}
	usageDiag.RouteEpoch = lease.RouteEpoch
	holdID := s.createBillingHold(r.Context(), affinity.Hash, lease.Account.ID, lease.RouteEpoch, virtual.EstimateTokensJSON(result.Body))
	// Backstop: settle-if-held on return so a cancelled/streaming disconnect can't leak
	// the hold; an explicit settle below always wins (WHERE status='held' no longer matches).
	defer func() { _ = s.settleBillingHoldIfHeld(r.Context(), holdID, "abandoned") }()
	r = r.WithContext(withUsageDiagnostics(withBillingHold(r.Context(), holdID), usageDiag))
	resp, err := s.upstream.Do(r.Context(), requestForToken(token))
	releaseFlight()
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody := readUpstreamErrorBody(resp.Body)
		if claudeAuthError(resp.StatusCode, resp.Header, errBody) && claudeTokenCanRefresh(token) {
			if refreshed, rerr := s.forceRefreshClaudeTokenWithHeartbeat(r.Context(), lease.Account, "auth_error", refreshHeartbeat.beat); rerr == nil {
				token = refreshed
				resp.Body.Close()
				resp, err = s.upstream.Do(r.Context(), requestForToken(token))
				if err != nil {
					_ = s.settleBillingHold(r.Context(), holdID, "failed_after_refresh")
					if writeClaudeWaitError(r.Context(), refreshHeartbeat, err) {
						return
					}
					writeError(w, http.StatusBadGateway, err)
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode < 400 {
					goto chatClaudeSuccess
				}
				errBody = readUpstreamErrorBody(resp.Body)
			} else if writeClaudeWaitError(r.Context(), refreshHeartbeat, rerr) {
				return
			}
		}
		if isModelNotFoundError(resp.StatusCode, errBody) {
			s.rejectAccountModel(r.Context(), lease.Account, model, resp.StatusCode)
			_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
			maxAttempts := s.settingInt(r.Context(), "failover_max_attempts", s.cfg.FailoverMaxAttempts)
			if maxAttempts < 2 {
				maxAttempts = 2
			}
			retryCount := chatClaudeModelRetryCount(r.Context())
			if retryCount+1 < maxAttempts && !refreshHeartbeat.Committed() {
				releaseLease()
				retryContext := context.WithValue(r.Context(), chatClaudeModelRetryKey{}, retryCount+1)
				s.handleChatViaClaude(w, r.WithContext(retryContext), raw, requestedModel.RequestedModel, pol)
				return
			}
			_ = s.handleCapabilitySelectionError(r.Context(), w, &scheduler.NoAccountError{Model: model, Counters: scheduler.NoAccountCounters{ModelUnsupported: 1}}, false, routeGroup, "claude", requestedModel.RequestedModel, requestedModel.ContextMode)
			return
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider:   "claude",
			Entrypoint: "chat_completions",
			Model:      model,
			Status:     resp.StatusCode,
			Header:     resp.Header,
			Body:       errBody,
			Streaming:  stream,
		})
		if ruleMatched {
			s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody, decision)
		} else {
			s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody)
		}
		_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
		if writeClaudeWaitError(r.Context(), refreshHeartbeat, errors.New("claude upstream returned an error after authentication refresh")) {
			return
		}
		if ruleMatched {
			switch decision.Match.DownstreamAction {
			case upstreamrules.DownstreamActionIdleStream:
				if stream {
					resp.Body.Close()
					releaseLease()
				}
				if s.writeRuleDownstream(r.Context(), w, "codex", resp.StatusCode, resp.Header, errBody, result.Scrubber, decision, stream) {
					return
				}
			case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize:
				if s.writeRuleDownstream(r.Context(), w, "codex", resp.StatusCode, resp.Header, errBody, result.Scrubber, decision, stream) {
					return
				}
			}
		}
		// Downstream is an OpenAI-compatible client, so neutralize into the OpenAI
		// error envelope (hides the Anthropic limit/overload/billing specifics).
		status, out := resp.StatusCode, errBody
		if s.leakScrubEnabled(r.Context()) {
			if ns, nb, changed := leakfilter.NeutralizeErrorBody("codex", status, errBody); changed {
				status, out = ns, nb
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(result.Scrubber.ReplaceAll(out))
		return
	}
chatClaudeSuccess:
	s.verifyAccountModel(r.Context(), lease.Account, model, requestedModel.ContextMode)
	s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, "claude", model, resp.Header)

	if stream && isEventStream(resp.Header) {
		if !refreshHeartbeat.Committed() {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
		}
		// Tee the upstream Anthropic SSE through a usage scanner so streamed
		// /v1/chat/completions traffic records token usage just like the native
		// /v1/messages path (the scanner reads the raw Anthropic frames, not the
		// rewritten chat chunks).
		uscan := usage.NewStreamScanner("claude")
		aliasCapture := &claudeAliasCapture{}
		anthropicStreamToChatSSE(w, io.TeeReader(resp.Body, io.MultiWriter(uscan, aliasCapture)), model, result.Scrubber, chatStreamUsageRequested(raw))
		if parsed, ok := uscan.Parsed(); ok {
			s.recordParsedUsage(r.Context(), lease.Account.ID, affinity.Hash, parsed)
		}
		s.persistClaudeItemAliases(r.Context(), anthBody, aliasCapture.Bytes(), lease, model)
		_ = s.settleBillingHold(r.Context(), holdID, "settled_streaming")
		return
	}

	anthResp, err := s.readUpstreamResponseBody(resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.rememberClaudeCacheDiagnosticsMessageID(affinity, anthResp)
	s.persistClaudeItemAliases(r.Context(), anthBody, anthResp, lease, model)
	r = r.WithContext(withClaudeDiagnosticsMissReason(r.Context(), anthResp))
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, anthResp)
	_ = s.settleBillingHold(r.Context(), holdID, "settled")
	chatBody, err := prompt.AnthropicToChatCompletion(result.Scrubber.ReplaceAll(anthResp), model)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(chatBody)
}

// anthropicStreamToChatSSE transforms an Anthropic Messages SSE stream into an
// OpenAI chat.completion.chunk SSE stream, scrubbing each text delta.
func anthropicStreamToChatSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher, usageOptions ...bool) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	chatID := "chatcmpl-claude"
	roleSent := false
	toolIdx := -1
	var finishReason interface{}
	includeUsage := len(usageOptions) > 0 && usageOptions[0]
	if len(usageOptions) == 0 {
		// The Anthropic -> Responses bridge pipes this Chat stream into the Responses
		// converter. It always needs the terminal usage chunk so response.completed can
		// expose usage, even though no Chat client supplied stream_options.
		_, includeUsage = w.(*customProtocolPipeWriter)
	}
	var inputTokens, outputTokens, cacheReadTokens int64
	usageSeen := false

	emit := func(delta map[string]interface{}, finish interface{}) {
		chunk := map[string]interface{}{
			"id":     chatID,
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish},
			},
		}
		b, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitError := func(value map[string]interface{}) {
		// OpenAI Chat Completions SSE represents a terminal failure as an error
		// payload, not a successful finish chunk followed by [DONE].  This is
		// particularly important for the Kiro bridge: its EventStream decoder emits
		// an Anthropic `error` frame after a stream has started.
		_ = value
		value = map[string]interface{}{"error": map[string]interface{}{
			"type": "server_error", "code": "server_error", "message": publicRetryMessage,
		}}
		b, _ := json.Marshal(value)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitUsage := func() {
		if !includeUsage || !usageSeen {
			return
		}
		usage := map[string]interface{}{
			"prompt_tokens":     inputTokens,
			"completion_tokens": outputTokens,
			"total_tokens":      inputTokens + outputTokens,
		}
		if cacheReadTokens > 0 {
			usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cacheReadTokens}
		}
		chunk := map[string]interface{}{
			"id": chatID, "object": "chat.completion.chunk", "model": model,
			"choices": []interface{}{}, "usage": usage,
		}
		encoded, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(encoded)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev map[string]interface{}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "error":
			emitError(ev)
			return
		case "message_start":
			if m, ok := ev["message"].(map[string]interface{}); ok {
				if idv, ok := m["id"].(string); ok && idv != "" {
					chatID = idv
				}
				if streamUsage, ok := m["usage"].(map[string]interface{}); ok {
					inputTokens = anthropicStreamUsageInt64(streamUsage["input_tokens"])
					outputTokens = anthropicStreamUsageInt64(streamUsage["output_tokens"])
					cacheReadTokens = anthropicStreamUsageInt64(streamUsage["cache_read_input_tokens"])
					usageSeen = true
				}
			}
			if !roleSent {
				emit(map[string]interface{}{"role": "assistant"}, nil)
				roleSent = true
			}
		case "content_block_start":
			if cb, _ := ev["content_block"].(map[string]interface{}); cb != nil && cb["type"] == "tool_use" {
				if !roleSent {
					emit(map[string]interface{}{"role": "assistant"}, nil)
					roleSent = true
				}
				toolIdx++
				emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
					"index": toolIdx,
					"id":    asString(cb["id"]),
					"type":  "function",
					"function": map[string]interface{}{
						"name":      asString(cb["name"]),
						"arguments": "",
					},
				}}}, nil)
			}
		case "content_block_delta":
			d, _ := ev["delta"].(map[string]interface{})
			if d == nil {
				continue
			}
			switch d["type"] {
			case "text_delta":
				if txt, ok := d["text"].(string); ok && txt != "" {
					if !roleSent {
						emit(map[string]interface{}{"role": "assistant"}, nil)
						roleSent = true
					}
					emit(map[string]interface{}{"content": scrubber.ReplaceString(txt)}, nil)
				}
			case "input_json_delta":
				if pj, ok := d["partial_json"].(string); ok && pj != "" && toolIdx >= 0 {
					emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
						"index": toolIdx,
						"function": map[string]interface{}{
							"arguments": scrubber.ReplaceString(pj),
						},
					}}}, nil)
				}
			}
		case "message_delta":
			if streamUsage, ok := ev["usage"].(map[string]interface{}); ok {
				if value := anthropicStreamUsageInt64(streamUsage["input_tokens"]); value > 0 {
					inputTokens = value
				}
				if value := anthropicStreamUsageInt64(streamUsage["output_tokens"]); value > 0 {
					outputTokens = value
				}
				if value := anthropicStreamUsageInt64(streamUsage["cache_read_input_tokens"]); value > 0 {
					cacheReadTokens = value
				}
				usageSeen = true
			}
			if d, ok := ev["delta"].(map[string]interface{}); ok {
				if sr, ok := d["stop_reason"].(string); ok && sr != "" {
					finishReason = prompt.StopReasonToFinish(sr)
				}
			}
		case "message_stop":
			if finishReason == nil {
				finishReason = "stop"
			}
			emit(map[string]interface{}{}, finishReason)
			emitUsage()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}
	emitError(map[string]interface{}{"error": map[string]interface{}{
		"type": "server_error", "code": "server_error", "message": publicRetryMessage,
	}})
}

func anthropicStreamUsageInt64(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
