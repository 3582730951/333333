package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
)

// chat_custom.go serves a request from a custom OpenAI-compatible provider
// (DeepSeek, Kimi, OpenRouter, native Responses gateways, …). The three entrypoints
// differ only in how the downstream protocol is converted or transparently forwarded:
//   - handleChatViaCustom     : /v1/chat/completions — near-passthrough or chat ↔ Responses
//   - handleResponsesViaCustom: /v1/responses (Codex) — native passthrough or Responses ↔ chat
//   - handleMessagesViaCustom : /v1/messages (Claude Code) — Anthropic ↔ chat
// The shared lease → upstream → error plumbing lives in callCustom.

// customProviderForModel returns the enabled custom provider that should serve a model
// id, or ok=false for the built-in codex/claude upstreams. Model membership is the
// authoritative signal (auto-discovery unions discovered ids into the provider's model
// list); a provider-id prefix (e.g. "deepseek-chat" → "deepseek") is a fallback for a
// model requested before the first discovery probe ran.
func (s *Server) customProviderForModel(ctx context.Context, model string) (storage.CustomProvider, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return storage.CustomProvider{}, false
	}
	providers, err := s.store.ListCustomProviders(ctx)
	if err != nil {
		return storage.CustomProvider{}, false
	}
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if strings.EqualFold(strings.TrimSpace(m), model) {
				return p, true
			}
		}
	}
	lower := strings.ToLower(model)
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if id != "" && (strings.HasPrefix(lower, id+"-") || strings.HasPrefix(lower, id+"/") || strings.HasPrefix(lower, id+":")) {
			return p, true
		}
	}
	return storage.CustomProvider{}, false
}

func (s *Server) customProviderByID(ctx context.Context, id string) (storage.CustomProvider, bool) {
	p, ok, err := s.store.GetCustomProvider(ctx, id)
	if err != nil {
		return storage.CustomProvider{}, false
	}
	return p, ok
}

// customCall is the result of the shared lease+upstream step for a custom provider.
type customCall struct {
	resp     *upstream.Response
	lease    scheduler.Lease
	holdID   string
	scrubber *streamrewrite.Matcher
	affinity routing.AffinityKey
}

// callCustom selects a provider-matching account, calls the provider's selected
// upstream endpoint with the already-prepared body, and handles every failure path
// (writing a proto-appropriate error and releasing the lease/hold). On success it
// returns the live response; the caller owns cc.lease.Release() and cc.resp.Body.Close()
// and converts/records the response. proto selects the downstream error envelope:
// "claude" for the /v1/messages path, "codex" (OpenAI shape) otherwise.
func (s *Server) callCustom(w http.ResponseWriter, r *http.Request, provider storage.CustomProvider, body []byte, model, routeGroup, proto, upstreamPath string) (customCall, bool) {
	attempts := s.settingInt(r.Context(), "failover_max_attempts", s.cfg.FailoverMaxAttempts)
	if attempts < 1 {
		attempts = 1
	}
	return s.callCustomAttempt(w, r, provider, body, model, routeGroup, proto, upstreamPath, map[string]bool{}, attempts)
}

func (s *Server) callCustomAttempt(w http.ResponseWriter, r *http.Request, provider storage.CustomProvider, body []byte, model, routeGroup, proto, upstreamPath string, exclude map[string]bool, attemptsRemaining int) (customCall, bool) {
	if strings.TrimSpace(upstreamPath) == "" {
		upstreamPath = "/chat/completions"
	}
	affinity := routing.ExtractAffinityKey(r, body)
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:           routeGroup,
		Provider:        provider.ID,
		Affinity:        affinity,
		Model:           model,
		EstimatedTokens: virtual.EstimateTokensJSON(body),
		Exclude:         exclude,
	})
	if err != nil {
		status, _ := noAccountHTTPStatus(err)
		s.writePublicNoAccountError(r.Context(), w, status, routeGroup, provider.ID, model, err)
		return customCall{}, false
	}
	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		lease.Release()
		writeError(w, http.StatusInternalServerError, err)
		return customCall{}, false
	}
	// Scrub operator sensitive words from the request body, and reuse the same matcher
	// on the response/stream (zero-cost pass-through when no words are configured).
	scrub := cloak.ScrubSensitive(body, s.cfg.SensitiveWordsFor(provider.ID))
	body = scrub.Body
	scrubber := scrub.Scrubber
	osHint := s.osHint(body, lease.Egress)
	holdID := s.createBillingHold(affinity.Hash, lease.Account.ID, virtual.EstimateTokensJSON(body))
	// Backstop for THIS attempt's failure paths (several rule branches below return
	// without settling). On success the hold is handed to the caller via cc.holdID, which
	// settles it after streaming — so disarm the backstop before that hand-off.
	handedOff := false
	defer func() {
		if !handedOff {
			_ = s.settleBillingHoldIfHeld(r.Context(), holdID, "abandoned")
		}
	}()
	resp, err := s.upstream.Do(r.Context(), upstream.Request{
		Method:         http.MethodPost,
		Provider:       provider.ID,
		BaseURL:        strings.TrimSpace(provider.BaseURL),
		DownstreamPath: upstreamPath,
		Headers:        r.Header.Clone(),
		Body:           body,
		Account:        lease.Account,
		Token:          token,
		Egress:         lease.Egress,
		CookieJarKey:   lease.Binding.CookieJarKey,
		OSHint:         osHint,
	})
	if err != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		lease.Release()
		writeError(w, http.StatusBadGateway, err)
		return customCall{}, false
	}
	if resp.StatusCode >= 400 {
		errBody := readUpstreamErrorBody(resp.Body)
		_ = resp.Body.Close()
		entrypoint := "custom_openai"
		switch strings.TrimSpace(upstreamPath) {
		case "/responses", "responses":
			entrypoint = "responses"
		case "/chat/completions", "chat/completions":
			entrypoint = "chat_completions"
		}
		decision, ruleMatched := s.matchUpstreamErrorRule(r.Context(), upstreamrules.MatchInput{
			Provider:   provider.ID,
			Entrypoint: entrypoint,
			Model:      model,
			Status:     resp.StatusCode,
			Header:     resp.Header,
			Body:       errBody,
			Streaming:  isStreamRequest(body),
		})
		if ruleMatched {
			s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody, decision)
		} else {
			s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody)
		}
		_ = s.settleBillingHold(r.Context(), holdID, "failed_upstream")
		if ruleMatched {
			switch decision.Match.DownstreamAction {
			case upstreamrules.DownstreamActionFailover:
				if attemptsRemaining > 1 {
					if exclude != nil {
						exclude[lease.Account.ID] = true
					}
					lease.Release()
					return s.callCustomAttempt(w, r, provider, body, model, routeGroup, proto, upstreamPath, exclude, attemptsRemaining-1)
				}
				s.writeRuleNeutralError(w)
				lease.Release()
				return customCall{}, false
			case upstreamrules.DownstreamActionIdleStream:
				if isStreamRequest(body) {
					lease.Release()
				} else {
					lease.Release()
				}
				if s.writeRuleDownstream(r.Context(), w, proto, resp.StatusCode, resp.Header, errBody, scrubber, decision, isStreamRequest(body)) {
					return customCall{}, false
				}
			case upstreamrules.DownstreamActionPass, upstreamrules.DownstreamActionCustomError, upstreamrules.DownstreamActionNeutralize:
				if s.writeRuleDownstream(r.Context(), w, proto, resp.StatusCode, resp.Header, errBody, scrubber, decision, isStreamRequest(body)) {
					lease.Release()
					return customCall{}, false
				}
			}
		}
		s.writeFilteredError(r.Context(), w, proto, resp.StatusCode, resp.Header, errBody, scrubber)
		lease.Release()
		return customCall{}, false
	}
	s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, provider.ID, model, resp.Header)
	handedOff = true
	return customCall{resp: resp, lease: lease, holdID: holdID, scrubber: scrubber, affinity: affinity}, true
}

// handleChatViaCustom relays a /v1/chat/completions request to a custom provider. Both
// sides speak Chat Completions, so the body and response pass through unchanged (only
// sensitive-word scrubbing + usage capture are applied).
func (s *Server) handleChatViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	if provider.UpstreamProtocol == storage.CustomProviderProtocolResponses {
		s.handleChatViaNativeResponsesCustom(w, r, raw, model, routeGroup, provider)
		return
	}
	stream := isStreamRequest(raw)
	chatBody := raw
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "codex", "/chat/completions")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		uscan := usage.NewStreamScanner("openai_chat")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "chat_completions", model, cc.resp.StatusCode), provider.ID)
		_ = streamCopyRewrite(rw, io.TeeReader(cc.resp.Body, uscan), cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(cc.scrubber.ReplaceAll(body))
}

// handleResponsesViaCustom serves a Codex /v1/responses request from a custom provider:
// Responses request → chat, then chat response/SSE → Responses response/SSE.
func (s *Server) handleResponsesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	if provider.UpstreamProtocol == storage.CustomProviderProtocolResponses {
		s.handleNativeResponsesViaCustom(w, r, raw, model, routeGroup, provider)
		return
	}
	stream := isStreamRequest(raw)
	bridge, err := prompt.ResponsesRequestToChatCompletionBridge(raw)
	if err != nil {
		if s.writePromptCompatibilityError(w, err, "official_codex_or_custom_native_responses", "custom_chat_completions_bridge:"+provider.ID, "Set this provider's upstream_protocol=\"responses\" if it truly supports /v1/responses, or route this request to an official Codex account.") {
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	r = withResponsesCompatibilityLosses(r, bridge.CompatibilityLosses)
	if stream {
		declareResponsesCompatibilityTrailer(w)
		defer setResponsesCompatibilityTrailer(w, bridge.CompatibilityLosses)
	} else {
		setResponsesCompatibilityHeader(w, bridge.CompatibilityLosses)
	}
	chatBody := bridge.Body
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "codex", "/chat/completions")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		uscan := usage.NewStreamScanner("openai_chat")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "custom_openai", model, cc.resp.StatusCode), provider.ID)
		chatStreamToResponsesSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber, bridge.Plan)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	out, cerr := prompt.ChatCompletionToResponsesResponse(cc.scrubber.ReplaceAll(body), model, bridge.Plan)
	if cerr != nil {
		writeError(w, http.StatusBadGateway, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleNativeResponsesViaCustom relays /v1/responses to a custom provider that
// explicitly advertises native Responses support. Unlike the chat_completions bridge,
// it does not translate or drop typed tools, include fields, previous_response_id, or
// future Responses fields/events.
func (s *Server) handleNativeResponsesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	stream := isStreamRequest(raw)
	cc, ok := s.callCustom(w, r, provider, raw, model, routeGroup, "codex", "/responses")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		uscan := usage.NewStreamScanner("codex")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "responses", model, cc.resp.StatusCode), provider.ID)
		_ = streamCopyRewrite(rw, io.TeeReader(cc.resp.Body, uscan), cc.scrubber)
		if parsed, ok := uscan.Parsed(); ok {
			s.recordParsedUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, parsed)
		}
		_ = s.settleBillingHold(r.Context(), cc.holdID, "settled_streaming")
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(cc.scrubber.ReplaceAll(body))
}

// handleChatViaNativeResponsesCustom supports Chat Completions clients against a
// native-Responses custom provider by using the same chat↔Responses adapter the
// official Codex path uses, but without routing through ChatGPT accounts.
func (s *Server) handleChatViaNativeResponsesCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	responsesBody, err := prompt.ChatCompletionToResponses(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cc, ok := s.callCustom(w, r, provider, responsesBody, model, routeGroup, "codex", "/responses")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if isStreamRequest(raw) && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		uscan := usage.NewStreamScanner("codex")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "chat_completions", model, cc.resp.StatusCode), provider.ID)
		responsesStreamToChatSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, chatStreamUsageRequested(raw), cc.scrubber)
		if parsed, ok := uscan.Parsed(); ok {
			s.recordParsedUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, parsed)
		}
		_ = s.settleBillingHold(r.Context(), cc.holdID, "settled_streaming")
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	out, cerr := prompt.ResponsesToChatCompletion(cc.scrubber.ReplaceAll(body), cc.resp.Header.Get("x-request-id"), model)
	if cerr != nil {
		writeError(w, http.StatusBadGateway, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// handleMessagesViaCustom serves a Claude Code /v1/messages request from a custom
// provider: Anthropic request → chat, then chat response/SSE → Anthropic response/SSE.
func (s *Server) handleMessagesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	stream := isStreamRequest(raw)
	chatBody, err := prompt.AnthropicRequestToChatCompletion(raw)
	if err != nil {
		if s.writePromptCompatibilityError(w, err, "official_claude", "custom_chat_completions_bridge:"+provider.ID, "Route this request to an official Claude account, or remove Claude-only typed server tools/plugins for this Chat Completions provider.") {
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "claude", "/chat/completions")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		uscan := usage.NewStreamScanner("openai_chat")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "claude_messages", model, cc.resp.StatusCode), provider.ID)
		chatStreamToAnthropicSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	out, cerr := prompt.ChatCompletionToAnthropicResponse(cc.scrubber.ReplaceAll(body), model)
	if cerr != nil {
		writeError(w, http.StatusBadGateway, cerr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// settleStreamUsage records the streamed usage (if any) and settles the billing hold —
// the common tail of the three custom streaming paths.
func (s *Server) settleStreamUsage(r *http.Request, cc customCall, uscan *usage.StreamScanner) {
	if parsed, ok := uscan.Parsed(); ok {
		s.recordParsedUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, parsed)
	}
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled_streaming")
}

// withStreamUsage sets stream_options.include_usage=true on a streaming Chat
// Completions body so the provider emits a final usage chunk (the only way to record
// token usage on a streamed custom-provider response). A no-op for non-streaming
// bodies or unparseable input. Providers that ignore stream_options are unaffected.
func withStreamUsage(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]interface{}
	if dec.Decode(&root) != nil {
		return body
	}
	if stream, _ := root["stream"].(bool); !stream {
		return body
	}
	opts, _ := root["stream_options"].(map[string]interface{})
	if opts == nil {
		opts = map[string]interface{}{}
	}
	opts["include_usage"] = true
	root["stream_options"] = opts
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
