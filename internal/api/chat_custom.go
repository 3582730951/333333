package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
)

// chat_custom.go serves a request from a custom OpenAI-Chat-Completions-compatible
// provider (DeepSeek, Kimi, …). The three entrypoints differ only in how the
// downstream protocol is converted to/from Chat Completions:
//   - handleChatViaCustom     : /v1/chat/completions — near-passthrough (chat ↔ chat)
//   - handleResponsesViaCustom: /v1/responses (Codex) — Responses ↔ chat
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

// callCustom selects a provider-matching account, calls the upstream Chat Completions
// endpoint with the already-converted chatBody, and handles every failure path
// (writing a proto-appropriate error and releasing the lease/hold). On success it
// returns the live response; the caller owns cc.lease.Release() and cc.resp.Body.Close()
// and converts/records the response. proto selects the downstream error envelope:
// "claude" for the /v1/messages path, "codex" (OpenAI shape) otherwise.
func (s *Server) callCustom(w http.ResponseWriter, r *http.Request, provider storage.CustomProvider, chatBody []byte, model, routeGroup, proto string) (customCall, bool) {
	affinity := routing.ExtractAffinityKey(r, chatBody)
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:           routeGroup,
		Provider:        provider.ID,
		Affinity:        affinity,
		Model:           model,
		EstimatedTokens: virtual.EstimateTokensJSON(chatBody),
	})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, scheduler.ErrStrictUnavailable) {
			status = http.StatusConflict
		}
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
	scrub := cloak.ScrubSensitive(chatBody, s.cfg.SensitiveWordsFor(provider.ID))
	chatBody = scrub.Body
	scrubber := scrub.Scrubber
	osHint := s.osHint(chatBody, lease.Egress)
	holdID, _ := s.store.CreateBillingHold(r.Context(), affinity.Hash, lease.Account.ID, virtual.EstimateTokensJSON(chatBody))
	resp, err := s.upstream.Do(r.Context(), upstream.Request{
		Method:         http.MethodPost,
		Provider:       provider.ID,
		BaseURL:        strings.TrimSpace(provider.BaseURL),
		DownstreamPath: "/chat/completions",
		Headers:        r.Header.Clone(),
		Body:           chatBody,
		Account:        lease.Account,
		Token:          token,
		Egress:         lease.Egress,
		CookieJarKey:   lease.Binding.CookieJarKey,
		OSHint:         osHint,
	})
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_before_response")
		lease.Release()
		writeError(w, http.StatusBadGateway, err)
		return customCall{}, false
	}
	if resp.StatusCode >= 400 {
		errBody := readUpstreamErrorBody(resp.Body)
		_ = resp.Body.Close()
		s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody)
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_upstream")
		s.writeFilteredError(r.Context(), w, proto, resp.StatusCode, resp.Header, errBody, scrubber)
		lease.Release()
		return customCall{}, false
	}
	s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, provider.ID, model, resp.Header)
	return customCall{resp: resp, lease: lease, holdID: holdID, scrubber: scrubber, affinity: affinity}, true
}

// handleChatViaCustom relays a /v1/chat/completions request to a custom provider. Both
// sides speak Chat Completions, so the body and response pass through unchanged (only
// sensitive-word scrubbing + usage capture are applied).
func (s *Server) handleChatViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	stream := isStreamRequest(raw)
	chatBody := raw
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "codex")
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
		_ = streamCopyRewrite(w, io.TeeReader(cc.resp.Body, uscan), cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(cc.scrubber.ReplaceAll(body))
}

// handleResponsesViaCustom serves a Codex /v1/responses request from a custom provider:
// Responses request → chat, then chat response/SSE → Responses response/SSE.
func (s *Server) handleResponsesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	stream := isStreamRequest(raw)
	chatBody, err := prompt.ResponsesRequestToChatCompletion(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "codex")
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
		chatStreamToResponsesSSE(w, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "settled")
	out, cerr := prompt.ChatCompletionToResponsesResponse(cc.scrubber.ReplaceAll(body), model)
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
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if stream {
		chatBody = withStreamUsage(chatBody)
	}
	cc, ok := s.callCustom(w, r, provider, chatBody, model, routeGroup, "claude")
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
		chatStreamToAnthropicSSE(w, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), cc.lease.Account.ID, cc.affinity.Hash, body)
	_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "settled")
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
	_ = s.store.SettleBillingHold(r.Context(), cc.holdID, "settled_streaming")
}

// withStreamUsage sets stream_options.include_usage=true on a streaming Chat
// Completions body so the provider emits a final usage chunk (the only way to record
// token usage on a streamed custom-provider response). A no-op for non-streaming
// bodies or unparseable input. Providers that ignore stream_options are unaffected.
func withStreamUsage(body []byte) []byte {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
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
