package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// chat_custom.go serves a request from a custom OpenAI-compatible provider
// (DeepSeek, Kimi, OpenRouter, native Responses gateways, …). The three entrypoints
// differ only in how the downstream protocol is converted or transparently forwarded:
//   - handleChatViaCustom     : /v1/chat/completions — near-passthrough or chat ↔ Responses
//   - handleResponsesViaCustom: /v1/responses (Codex) — native passthrough or Responses ↔ chat
//   - handleMessagesViaCustom : /v1/messages (Claude Code) — Anthropic ↔ chat
// The shared lease → upstream → error plumbing lives in callCustom.

// customProvidersForModel returns every enabled custom provider that advertises a
// model, in deterministic preference order. Exact mappings/catalog membership win;
// a provider-id prefix (e.g. "deepseek-chat" → "deepseek") is only a fallback for a
// model requested before the first discovery probe ran.
func (s *Server) customProvidersForModel(ctx context.Context, model string) []storage.CustomProvider {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	providers, err := s.store.ListCustomProviders(ctx)
	if err != nil {
		return nil
	}
	matches := make([]storage.CustomProvider, 0, len(providers))
	matched := make(map[string]bool, len(providers))
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		if _, mapped := customProviderMappedModel(p, model); mapped {
			matches = append(matches, p)
			matched[p.ID] = true
			continue
		}
		for _, m := range p.Models {
			if strings.EqualFold(strings.TrimSpace(m), model) {
				matches = append(matches, p)
				matched[p.ID] = true
				break
			}
		}
		if matched[p.ID] {
			continue
		}
		// Anthropic-compatible relays frequently omit /models. Until their first
		// account probe has produced an authoritative list, the maintained Claude
		// candidate table makes an auto-discovery provider immediately routable.
		if p.AutoDiscoverModels && len(p.Models) == 0 &&
			customProviderSupportsProtocol(p, storage.CustomProviderProtocolAnthropicMessages) &&
			capability.IsClaudeProbeModel(model) {
			matches = append(matches, p)
			matched[p.ID] = true
		}
	}
	lower := strings.ToLower(model)
	for _, p := range providers {
		if !p.Enabled || matched[p.ID] {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if id != "" && (strings.HasPrefix(lower, id+"-") || strings.HasPrefix(lower, id+"/") || strings.HasPrefix(lower, id+":")) {
			matches = append(matches, p)
		}
	}
	return matches
}

func customProviderSupportsProtocol(provider storage.CustomProvider, protocol string) bool {
	if provider.UpstreamProtocol == protocol {
		return true
	}
	for _, route := range provider.Routes {
		if route.UpstreamProtocol == protocol {
			return true
		}
	}
	return false
}

// customProviderForModel returns the first matching custom provider that the
// production scheduler can lease in this request's account-pool group. A matching
// provider with no active account, no healthy configured egress, or exhausted
// admission capacity therefore cannot shadow a later healthy provider. When every
// custom candidate is unavailable, ok=false deliberately lets automatic routing
// continue to the built-in Codex/Claude providers.
func (s *Server) customProviderForModel(ctx context.Context, model, group string, body []byte) (storage.CustomProvider, bool) {
	for _, provider := range s.customProvidersForModel(ctx, model) {
		targetModel, mapped := customProviderMappedModel(provider, model)
		if !mapped {
			targetModel = model
		}
		lease, err := s.scheduler.Select(ctx, scheduler.Route{
			Group:           group,
			Provider:        provider.ID,
			Model:           targetModel,
			EstimatedTokens: virtual.EstimateTokensJSON(body),
			SkipWait:        true,
		})
		if err != nil {
			continue
		}
		lease.Release()
		return provider, true
	}
	return storage.CustomProvider{}, false
}

// customProviderMappedModel resolves an operator model rewrite. Exact mappings
// are case-insensitive; "*" is a provider-wide fallback. The boolean distinguishes
// an explicit identity mapping from no mapping.
func customProviderMappedModel(provider storage.CustomProvider, requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if target := strings.TrimSpace(provider.ModelMappings[strings.ToLower(requested)]); target != "" {
		return target, true
	}
	// Keep compatibility with provider rows written before mapping keys were
	// canonicalized by the storage layer. Claude Code spells concrete versions
	// with hyphens while the routing core canonicalizes them with a dot; compare
	// only recognized concrete Claude aliases so arbitrary provider-owned model
	// slugs retain exact semantics. Sort map keys so an old row containing two
	// equivalent aliases has a deterministic winner. The exact lookup above
	// always takes priority and the wildcard remains last.
	sources := make([]string, 0, len(provider.ModelMappings))
	for source := range provider.ModelMappings {
		if strings.TrimSpace(source) != "*" {
			sources = append(sources, source)
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		left, right := strings.ToLower(strings.TrimSpace(sources[i])), strings.ToLower(strings.TrimSpace(sources[j]))
		if left == right {
			return sources[i] < sources[j]
		}
		return left < right
	})
	for _, source := range sources {
		if !strings.EqualFold(strings.TrimSpace(source), requested) &&
			!customProviderClaudeModelAliasesEquivalent(source, requested) {
			continue
		}
		target := provider.ModelMappings[source]
		target = strings.TrimSpace(target)
		return target, target != ""
	}
	if target := strings.TrimSpace(provider.ModelMappings["*"]); target != "" {
		return target, true
	}
	return requested, false
}

func customProviderClaudeModelAliasesEquivalent(left, right string) bool {
	parseConcrete := func(model string) (string, bool) {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-") {
			return "", false
		}
		parsed, err := capability.ParseRequestedClaudeModel(model)
		if err != nil {
			return "", false
		}
		if _, concrete := capability.KiroCanonicalModel(parsed.BaseModel); !concrete {
			return "", false
		}
		return parsed.BaseModel, true
	}
	left, leftOK := parseConcrete(left)
	right, rightOK := parseConcrete(right)
	return leftOK && rightOK && claudeRouteModelsEquivalent(left, right)
}

func applyCustomProviderModelMapping(provider storage.CustomProvider, raw []byte, requested string) ([]byte, string, bool) {
	target, mapped := customProviderMappedModel(provider, requested)
	if !mapped || strings.EqualFold(strings.TrimSpace(target), strings.TrimSpace(requested)) {
		return raw, requested, mapped
	}
	return setForcedModel(raw, target), target, true
}

func (s *Server) customProviderByID(ctx context.Context, id string) (storage.CustomProvider, bool) {
	p, ok, err := s.store.GetCustomProvider(ctx, id)
	if err != nil || !ok || !p.Enabled {
		return storage.CustomProvider{}, false
	}
	return p, true
}

// customCall is the result of the shared lease+upstream step for a custom provider.
type customCall struct {
	resp     *upstream.Response
	lease    scheduler.Lease
	holdID   string
	scrubber *streamrewrite.Matcher
	affinity routing.AffinityKey
	provider string
}

func customProviderDownstreamScope(r *http.Request) string {
	identity := "anonymous"
	if r != nil {
		keyHash, userID := downstreamFromCtx(r.Context())
		switch {
		case strings.TrimSpace(keyHash) != "":
			identity = "key:" + strings.TrimSpace(keyHash)
		case strings.TrimSpace(userID) != "":
			identity = "user:" + strings.TrimSpace(userID)
		case strings.TrimSpace(downstreamBearer(r)) != "":
			identity = "credential:" + hashAPIKey(downstreamBearer(r))
		default:
			for _, header := range []string{"X-Pool-Client-ID", "X-Client-ID", "X-Session-ID"} {
				if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
					identity = "client:" + value
					break
				}
			}
		}
	}
	return hashAPIKey("custom-provider-downstream:" + identity)[:24]
}

func customProviderRouteScope(provider storage.CustomProvider) string {
	return hashAPIKey(strings.Join([]string{
		"custom-provider-route", strings.TrimSpace(provider.ID),
		strings.TrimSpace(provider.ResolvedRouteID), strings.TrimSpace(provider.ResolvedDownstreamPath),
		strings.TrimSpace(provider.BaseURL), strings.TrimSpace(provider.UpstreamProtocol),
		strings.TrimSpace(provider.TransportProfile),
	}, "\x00"))[:24]
}

func customProviderScopedAffinity(r *http.Request, provider storage.CustomProvider, affinity routing.AffinityKey) routing.AffinityKey {
	if strings.TrimSpace(affinity.Hash) == "" {
		return affinity
	}
	return routing.AffinityFromKey(strings.Join([]string{
		"custom-provider", strings.TrimSpace(provider.ID),
		customProviderRouteScope(provider), customProviderDownstreamScope(r), affinity.Hash,
	}, ":"), affinity.Source)
}

func customProviderCookieJarKey(r *http.Request, lease scheduler.Lease, provider storage.CustomProvider) string {
	base := lease.Binding.CookieJarKey
	if strings.TrimSpace(base) == "" {
		base = lease.Account.ID + ":" + strings.TrimSpace(lease.Egress.ID)
	}
	return base + ":custom:" + customProviderRouteScope(provider) + ":" + customProviderDownstreamScope(r)
}

func writeInvalidCustomUpstreamResponse(w http.ResponseWriter) {
	writePoolCodeError(w, http.StatusBadGateway, "invalid_upstream_response", "upstream returned an invalid response")
}

func (s *Server) rejectInvalidCustomUpstreamResponse(w http.ResponseWriter, r *http.Request, cc customCall) {
	_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_invalid_upstream_response")
	writeInvalidCustomUpstreamResponse(w)
}

func validateCustomUpstreamJSONResponse(protocol string, body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]json.RawMessage
	if err := dec.Decode(&root); err != nil {
		return err
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values in upstream response")
		}
		return err
	}
	if root == nil {
		return errors.New("upstream response must be a JSON object")
	}
	if rawError, ok := root["error"]; ok && len(rawError) > 0 && string(rawError) != "null" {
		return errors.New("2xx upstream response contains an error envelope")
	}
	switch protocol {
	case storage.CustomProviderProtocolChatCompletions:
		var choices []json.RawMessage
		if raw, ok := root["choices"]; !ok || json.Unmarshal(raw, &choices) != nil || len(choices) == 0 {
			return errors.New("Chat Completions response requires choices")
		}
	case storage.CustomProviderProtocolResponses:
		var object string
		if raw, ok := root["object"]; !ok || json.Unmarshal(raw, &object) != nil || object != "response" {
			return errors.New("Responses response requires object=response")
		}
	case storage.CustomProviderProtocolAnthropicMessages:
		var messageType string
		if raw, ok := root["type"]; !ok || json.Unmarshal(raw, &messageType) != nil || messageType != "message" {
			return errors.New("Anthropic response requires type=message")
		}
		var content []json.RawMessage
		if raw, ok := root["content"]; !ok || json.Unmarshal(raw, &content) != nil {
			return errors.New("Anthropic response requires content array")
		}
	default:
		return fmt.Errorf("unsupported upstream protocol %q", protocol)
	}
	return nil
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
	retryBody := body
	if strings.TrimSpace(upstreamPath) == "" {
		upstreamPath = "/chat/completions"
	}
	affinity := customProviderScopedAffinity(r, provider, routing.ExtractAffinityKey(r, body))
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:           routeGroup,
		Provider:        provider.ID,
		Affinity:        affinity,
		Model:           model,
		EstimatedTokens: virtual.EstimateTokensJSON(body),
		Exclude:         exclude,
		SkipWait:        userGroupFallbackProbe(r.Context()),
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
	// A custom Anthropic Messages call presents the full claude-cli header/TLS profile,
	// so its body must belong to that same client family. Normalize/inject the native
	// system, billing, device and session fields before the last-mile dispatcher. Other
	// custom protocols keep the generic zero-parse scrub path.
	var scrub cloak.Result
	if provider.UpstreamProtocol == storage.CustomProviderProtocolAnthropicMessages && strings.Contains(strings.ToLower(upstreamPath), "messages") {
		body = ensureClaudeCodeIdentityBody(body)
		osHint := s.osHint(body, lease.Egress)
		id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
		billingVersion := s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion)
		scrub = cloak.VirtualizeClaudeCode(body, id, s.cfg.SensitiveWordsFor(provider.ID), claudeIsOAuth(token), billingVersion)
	} else {
		scrub = cloak.ScrubSensitive(body, s.cfg.SensitiveWordsFor(provider.ID))
	}
	body = scrub.Body
	scrubber := scrub.Scrubber
	holdID := s.createBillingHold(r.Context(), affinity.Hash, lease.Account.ID, lease.RouteEpoch, virtual.EstimateTokensJSON(body))
	// Backstop for THIS attempt's failure paths (several rule branches below return
	// without settling). On success the hold is handed to the caller via cc.holdID, which
	// settles it after streaming — so disarm the backstop before that hand-off.
	handedOff := false
	defer func() {
		if !handedOff {
			_ = s.settleBillingHoldIfHeld(r.Context(), holdID, "abandoned")
		}
	}()
	// One wire attempt always uses the selected account's persisted primary outlet.
	// Failover may choose another account, but it may not rotate this account onto a
	// group/provider/standby outlet and thereby change its network identity.
	attemptBodyHash := diagnosticBodyHash(body)
	resp, requestErr := s.upstream.Do(r.Context(), upstream.Request{
		Method:           http.MethodPost,
		Provider:         provider.ID,
		BaseURL:          strings.TrimSpace(provider.BaseURL),
		TransportProfile: provider.TransportProfile,
		UpstreamProtocol: provider.UpstreamProtocol,
		DownstreamPath:   upstreamPath,
		Headers:          r.Header.Clone(),
		Body:             bodysource.Bytes(body),
		Model:            model,
		Account:          lease.Account,
		Token:            token,
		Egress:           lease.Egress,
		CookieJarKey:     customProviderCookieJarKey(r, lease, provider),
		OSHint:           s.osHint(body, lease.Egress),
	})
	s.recordProviderAttempt(requestIDFromContext(r.Context()), lease.Account.ID, provider.ID, "inference",
		diagnosticResponseStatus(resp), diagnosticWireErrorClass(diagnosticResponseStatus(resp), requestErr),
		attemptBodyHash, diagnosticRetryAfter(resp))
	if requestErr != nil {
		_ = s.settleBillingHold(r.Context(), holdID, "failed_before_response")
		if attemptsRemaining > 1 {
			if exclude != nil {
				exclude[lease.Account.ID] = true
			}
			lease.Release()
			return s.callCustomAttempt(w, r, provider, retryBody, model, routeGroup, proto, upstreamPath, exclude, attemptsRemaining-1)
		}
		lease.Release()
		writeError(w, http.StatusBadGateway, requestErr)
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
		accountRetryable := false
		if ruleMatched {
			verdict := s.applyRuleAccountAction(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody, decision)
			accountRetryable = retryableForFailover(verdict, resp.StatusCode)
		} else {
			verdict := s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody)
			accountRetryable = retryableForFailover(verdict, resp.StatusCode)
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
					return s.callCustomAttempt(w, r, provider, retryBody, model, routeGroup, proto, upstreamPath, exclude, attemptsRemaining-1)
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
		if accountRetryable && attemptsRemaining > 1 {
			if exclude != nil {
				exclude[lease.Account.ID] = true
			}
			lease.Release()
			return s.callCustomAttempt(w, r, provider, retryBody, model, routeGroup, proto, upstreamPath, exclude, attemptsRemaining-1)
		}
		s.writeFilteredError(r.Context(), w, proto, resp.StatusCode, resp.Header, errBody, scrubber)
		lease.Release()
		return customCall{}, false
	}
	s.guardRateLimitForAccount(r.Context(), lease.Account, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, provider.ID, model, resp.Header)
	handedOff = true
	return customCall{resp: resp, lease: lease, holdID: holdID, scrubber: scrubber, affinity: affinity, provider: provider.ID}, true
}

// handleChatViaCustom relays a /v1/chat/completions request to a custom provider. Both
// sides speak Chat Completions, so the body and response pass through unchanged (only
// sensitive-word scrubbing + usage capture are applied).
func (s *Server) handleChatViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	provider, _ = storage.ResolveCustomProviderRoute(provider, storage.CustomProviderDownstreamChat)
	switch provider.UpstreamProtocol {
	case storage.CustomProviderProtocolResponses:
		s.handleChatViaNativeResponsesCustom(w, r, raw, model, routeGroup, provider)
		return
	case storage.CustomProviderProtocolAnthropicMessages:
		s.handleChatViaAnthropicMessagesCustom(w, r, raw, model, routeGroup, provider)
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
		flushWriter(w)
		uscan := usage.NewStreamScanner("openai_chat")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "chat_completions", model, cc.resp.StatusCode), provider.ID)
		terminal, _ := streamCopyRewriteValidated(rw, io.TeeReader(cc.resp.Body, uscan), cc.scrubber, customSSEChatCompletions, s.leakScrubEnabled(r.Context()))
		s.settleValidatedStreamUsage(r, cc, uscan, terminal)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	clean := cc.scrubber.ReplaceAll(body)
	if err := validateCustomUpstreamJSONResponse(storage.CustomProviderProtocolChatCompletions, clean); err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(clean)
}

// handleResponsesViaCustom serves a Codex /v1/responses request from a custom provider:
// Responses request → chat, then chat response/SSE → Responses response/SSE.
func (s *Server) handleResponsesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	provider, _ = storage.ResolveCustomProviderRoute(provider, storage.CustomProviderDownstreamResponses)
	switch provider.UpstreamProtocol {
	case storage.CustomProviderProtocolResponses:
		s.handleNativeResponsesViaCustom(w, r, raw, model, routeGroup, provider)
		return
	case storage.CustomProviderProtocolAnthropicMessages:
		s.handleResponsesViaAnthropicMessagesCustom(w, r, raw, model, routeGroup, provider)
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
		flushWriter(w)
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
	out, cerr := prompt.ChatCompletionToResponsesResponse(cc.scrubber.ReplaceAll(body), model, bridge.Plan)
	if cerr != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
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
	upstreamPath := "/responses"
	if r != nil && r.URL != nil && strings.Contains(strings.ToLower(r.URL.Path), "/responses/compact") {
		upstreamPath = "/responses/compact"
	}
	cc, ok := s.callCustom(w, r, provider, raw, model, routeGroup, "codex", upstreamPath)
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		uscan := usage.NewStreamScanner("codex")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "responses", model, cc.resp.StatusCode), provider.ID)
		terminal, _ := streamCopyRewriteValidated(rw, io.TeeReader(cc.resp.Body, uscan), cc.scrubber, customSSEResponses, s.leakScrubEnabled(r.Context()))
		s.settleValidatedStreamUsage(r, cc, uscan, terminal)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	clean := cc.scrubber.ReplaceAll(body)
	if err := validateCustomUpstreamJSONResponse(storage.CustomProviderProtocolResponses, clean); err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(clean)
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
		flushWriter(w)
		uscan := usage.NewStreamScanner("codex")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "chat_completions", model, cc.resp.StatusCode), provider.ID)
		responsesStreamToChatSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, chatStreamUsageRequested(raw), cc.scrubber)
		if parsed, ok := uscan.Parsed(); ok {
			s.recordCustomParsedUsage(r, cc, parsed)
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
	out, cerr := prompt.ResponsesToChatCompletion(cc.scrubber.ReplaceAll(body), cc.resp.Header.Get("x-request-id"), model)
	if cerr != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Server) handleChatViaAnthropicMessagesCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	if err := validateChatToAnthropicRequest(raw); err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_field", err.Error())
		return
	}
	body, err := prompt.ChatCompletionToAnthropic(raw)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "protocol_conversion_failed", err.Error())
		return
	}
	r = withAnthropicCustomHeaders(r)
	cc, ok := s.callCustom(w, r, provider, body, model, routeGroup, "codex", "/messages")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()
	stream := isStreamRequest(raw)
	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		uscan := usage.NewStreamScanner("claude")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "chat_completions", model, cc.resp.StatusCode), provider.ID)
		anthropicStreamToChatSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber, chatStreamUsageRequested(raw))
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	responseBody, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	out, err := prompt.AnthropicToChatCompletion(cc.scrubber.ReplaceAll(responseBody), model)
	if err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, responseBody)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(out)
}

func (s *Server) handleResponsesViaAnthropicMessagesCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	if err := validateResponsesToAnthropicRequest(raw); err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_field", err.Error())
		return
	}
	bridge, err := prompt.ResponsesRequestToChatCompletionBridge(raw)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "protocol_conversion_failed", err.Error())
		return
	}
	if len(bridge.CompatibilityLosses) > 0 {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_field", "Responses fields cannot be converted safely: "+strings.Join(bridge.CompatibilityLosses, ", "))
		return
	}
	body, err := prompt.ChatCompletionToAnthropic(bridge.Body)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "protocol_conversion_failed", err.Error())
		return
	}
	r = withAnthropicCustomHeaders(r)
	cc, ok := s.callCustom(w, r, provider, body, model, routeGroup, "codex", "/messages")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()
	stream := isStreamRequest(raw)
	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		uscan := usage.NewStreamScanner("claude")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "responses", model, cc.resp.StatusCode), provider.ID)
		anthropicStreamToResponsesCustomSSE(rw, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber, bridge.Plan)
		s.settleStreamUsage(r, cc, uscan)
		return
	}
	responseBody, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	chatResponse, err := prompt.AnthropicToChatCompletion(cc.scrubber.ReplaceAll(responseBody), model)
	if err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	out, err := prompt.ChatCompletionToResponsesResponse(chatResponse, model, bridge.Plan)
	if err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, responseBody)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(out)
}

func (s *Server) handleMessagesViaResponsesCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	if err := validateMessagesToResponsesRequest(raw); err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_field", err.Error())
		return
	}
	converted, err := prompt.AnthropicRequestToResponses(raw)
	if err != nil {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "protocol_conversion_failed", err.Error())
		return
	}
	converted.Body = applyMessagesResponseControls(raw, converted.Body)
	r = withoutAnthropicContext1MBeta(r)
	cc, ok := s.callCustom(w, r, provider, converted.Body, model, routeGroup, "claude", "/responses")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()
	stream := isStreamRequest(raw)
	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		uscan := usage.NewStreamScanner("codex")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "claude_messages", model, cc.resp.StatusCode), provider.ID)
		// The goal history is the downstream Anthropic body, not the Responses body
		// this provider happens to speak upstream, so the capture sits after the
		// conversion and belongs to the Messages family.
		goalOut, finishGoal := s.captureCustomMessagesGoalStream(r.Context(), r, raw, rw)
		responsesStreamToAnthropicSSE(goalOut, io.TeeReader(cc.resp.Body, uscan), model, converted.ToolNames, converted.InheritModelTools, cc.scrubber)
		s.settleStreamUsage(r, cc, uscan)
		finishGoal(false)
		return
	}
	responseBody, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if isEventStream(cc.resp.Header) {
		responseBody = codexSSEToResponseJSON(responseBody)
		if len(responseBody) == 0 {
			_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_stream_aggregation")
			writeInvalidCustomUpstreamResponse(w)
			return
		}
	}
	out, err := prompt.ResponsesToAnthropicResponse(cc.scrubber.ReplaceAll(responseBody), model, converted.ToolNames, converted.InheritModelTools)
	if err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, responseBody)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	// `out` is already the complete downstream Anthropic Messages body in both
	// branches below, so the goal turn is persisted from it directly.
	s.persistCustomMessagesGoalContinuity(r.Context(), r, raw, out)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		_ = anthropicMessageJSONToSSE(w, out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(out)
}

// handleMessagesViaCustom serves a Claude Code /v1/messages request from a custom
// provider: Anthropic request → chat, then chat response/SSE → Anthropic response/SSE.
func (s *Server) handleMessagesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	provider, _ = storage.ResolveCustomProviderRoute(provider, storage.CustomProviderDownstreamMessages)
	switch provider.UpstreamProtocol {
	case storage.CustomProviderProtocolAnthropicMessages:
		s.handleNativeAnthropicMessagesViaCustom(w, r, raw, model, routeGroup, provider)
		return
	case storage.CustomProviderProtocolResponses:
		s.handleMessagesViaResponsesCustom(w, r, raw, model, routeGroup, provider)
		return
	}
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
		flushWriter(w)
		uscan := usage.NewStreamScanner("openai_chat")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "claude_messages", model, cc.resp.StatusCode), provider.ID)
		goalOut, finishGoal := s.captureCustomMessagesGoalStream(r.Context(), r, raw, rw)
		chatStreamToAnthropicSSEWithOptions(r.Context(), goalOut, io.TeeReader(cc.resp.Body, uscan), model, cc.scrubber, bodysource.CaptureOptions{
			MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
			Budget: s.responseBodyBudget, DiskReserver: s.bodyDiskReserver,
		})
		s.settleStreamUsage(r, cc, uscan)
		finishGoal(false)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	out, cerr := prompt.ChatCompletionToAnthropicResponse(cc.scrubber.ReplaceAll(body), model)
	if cerr != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	s.persistCustomMessagesGoalContinuity(r.Context(), r, raw, out)
}

// handleNativeAnthropicMessagesViaCustom relays /v1/messages to a custom provider that
// natively speaks the Anthropic Messages API. The raw request body is forwarded unchanged
// to the provider's base_url + /messages endpoint; the response passes through as-is.
func (s *Server) handleNativeAnthropicMessagesViaCustom(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup string, provider storage.CustomProvider) {
	raw = withExplicitAnthropicAutoToolChoice(raw)
	stream := isStreamRequest(raw)
	// A bare URL+API-key client is allowed to omit Anthropic-Version. The official
	// Messages API and most compatible relays require it, so supply the protocol
	// default before the custom transport builds its clean upstream header set.
	r = withAnthropicCustomHeaders(r)
	cc, ok := s.callCustom(w, r, provider, raw, model, routeGroup, "claude", "/messages")
	if !ok {
		return
	}
	defer cc.lease.Release()
	defer cc.resp.Body.Close()

	if stream && isEventStream(cc.resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(cc.resp.StatusCode)
		flushWriter(w)
		uscan := usage.NewStreamScanner("claude")
		rw := newRuleFilteringWriter(w, s.responseRuleFilter(r.Context(), provider.ID, "claude_messages", model, cc.resp.StatusCode), provider.ID)
		goalOut, finishGoal := s.captureCustomMessagesGoalStream(r.Context(), r, raw, rw)
		terminal, copyErr := streamCopyRewriteValidated(goalOut, io.TeeReader(cc.resp.Body, uscan), cc.scrubber, customSSEAnthropicMessages, s.leakScrubEnabled(r.Context()))
		s.settleValidatedStreamUsage(r, cc, uscan, terminal)
		finishGoal(copyErr != nil || !terminal)
		return
	}
	body, err := s.readUpstreamResponseBody(cc.resp.Body)
	if err != nil {
		_ = s.settleBillingHold(r.Context(), cc.holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	clean := cc.scrubber.ReplaceAll(body)
	if err := validateCustomUpstreamJSONResponse(storage.CustomProviderProtocolAnthropicMessages, clean); err != nil {
		s.rejectInvalidCustomUpstreamResponse(w, r, cc)
		return
	}
	s.recordCustomUsage(r, cc, body)
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cc.resp.StatusCode)
	_, _ = w.Write(clean)
	s.persistCustomMessagesGoalContinuity(r.Context(), r, raw, clean)
}

// persistCustomMessagesGoalContinuity records the durable turn for a custom provider
// serving /v1/messages. All three upstream bridges (native Anthropic Messages,
// Responses, Chat Completions) return an Anthropic Messages body downstream, so the
// persisted history belongs to the Messages family regardless of upstream protocol.
func (s *Server) persistCustomMessagesGoalContinuity(ctx context.Context, r *http.Request, requestBody, responseBody []byte) {
	if len(responseBody) == 0 {
		return
	}
	if _, err := s.persistGoalContinuity(ctx, r, "custom_messages", requestBody, responseBody); err != nil {
		log.Printf("[GOAL-CONTINUITY] custom messages persistence degraded request_id=%s: %v", requestIDFromContext(ctx), err)
		s.auditGoalPersistenceDegraded(ctx, "custom_messages_terminal", err)
	}
}

// goalCapturingWriter mirrors everything written downstream into a bounded spool so a
// goal checkpoint can be rebuilt from the exact bytes the client received. Flush is
// forwarded so SSE latency is unchanged; a spool write failure only loses the
// checkpoint and never breaks the relay.
type goalCapturingWriter struct {
	http.ResponseWriter
	capture *bodysource.SpoolBuffer
}

func (g *goalCapturingWriter) Write(p []byte) (int, error) {
	n, err := g.ResponseWriter.Write(p)
	if n > 0 && g.capture != nil {
		if _, captureErr := g.capture.Write(p[:n]); captureErr != nil {
			g.capture = nil
		}
	}
	return n, err
}

func (g *goalCapturingWriter) Flush() {
	if flusher, ok := g.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// captureCustomMessagesGoalStream tees a downstream Anthropic SSE relay so the goal
// checkpoint can be rebuilt from the exact events the client received. It returns the
// writer to relay through plus a finish func that persists or marks retryable.
func (s *Server) captureCustomMessagesGoalStream(ctx context.Context, r *http.Request, requestBody []byte, w http.ResponseWriter) (http.ResponseWriter, func(streamFailed bool)) {
	if !s.goalContinuityEnabled(ctx) {
		return w, func(bool) {}
	}
	capture, err := bodysource.NewSpoolBuffer(ctx, s.responseBodyCaptureOptions(ctx))
	if err != nil {
		log.Printf("[GOAL-CONTINUITY] custom messages stream capture unavailable request_id=%s: %v", requestIDFromContext(ctx), err)
		return w, func(bool) {}
	}
	wrapped := &goalCapturingWriter{ResponseWriter: w, capture: capture}
	return wrapped, func(streamFailed bool) {
		defer capture.Close()
		if streamFailed {
			s.markGoalStreamRetryable(ctx, r, "custom_messages", requestBody, "upstream_stream_error")
			return
		}
		if wrapped.capture == nil {
			return
		}
		frames, readErr := responseSpoolBytes(capture)
		if readErr != nil {
			log.Printf("[GOAL-CONTINUITY] custom messages stream capture unreadable request_id=%s: %v", requestIDFromContext(ctx), readErr)
			return
		}
		s.persistCustomMessagesGoalContinuity(ctx, r, requestBody, goalResponseFromSSE(frames))
	}
}

func withExplicitAnthropicAutoToolChoice(raw []byte) []byte {
	if gjson.GetBytes(raw, "tool_choice").Exists() {
		return raw
	}
	tools := gjson.GetBytes(raw, "tools")
	if !tools.IsArray() || len(tools.Array()) == 0 {
		return raw
	}
	updated, err := sjson.SetRawBytes(raw, "tool_choice", []byte(`{"type":"auto"}`))
	if err != nil {
		return raw
	}
	return updated
}

func validateProtocolTopLevel(raw []byte, protocol string, allowed map[string]bool) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]interface{}
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	for field := range root {
		if !allowed[field] {
			return nil, fmt.Errorf("%s field %q cannot be represented by the selected upstream protocol", protocol, field)
		}
	}
	return root, nil
}

func validateChatToAnthropicRequest(raw []byte) error {
	root, err := validateProtocolTopLevel(raw, "Chat Completions", map[string]bool{
		"model": true, "messages": true, "max_tokens": true, "max_completion_tokens": true,
		"temperature": true, "top_p": true, "stream": true, "stream_options": true,
		"stop": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true, "n": true,
	})
	if err != nil {
		return err
	}
	if n, ok := root["n"].(json.Number); ok && n.String() != "1" {
		return errors.New("Chat Completions n>1 cannot be represented by Anthropic Messages")
	}
	if choice, _ := root["tool_choice"].(string); choice == "none" {
		return errors.New("Chat Completions tool_choice=none cannot be represented safely by Anthropic Messages")
	}
	messages, _ := root["messages"].([]interface{})
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]interface{})
		if message == nil {
			return errors.New("Chat Completions messages must contain objects")
		}
		role, _ := message["role"].(string)
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return fmt.Errorf("Chat Completions role %q cannot be represented by Anthropic Messages", role)
		}
		parts, isParts := message["content"].([]interface{})
		if !isParts {
			continue
		}
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]interface{})
			partType, _ := part["type"].(string)
			switch partType {
			case "text", "input_text", "image_url", "file":
			default:
				return fmt.Errorf("Chat Completions content part %q cannot be represented by Anthropic Messages", partType)
			}
		}
	}
	return nil
}

func validateResponsesToAnthropicRequest(raw []byte) error {
	_, err := validateProtocolTopLevel(raw, "Responses", map[string]bool{
		"model": true, "instructions": true, "input": true, "stream": true,
		"temperature": true, "top_p": true, "max_output_tokens": true,
		"max_tokens": true, "max_completion_tokens": true, "parallel_tool_calls": true,
		"tools": true, "tool_choice": true,
	})
	return err
}

func validateMessagesToResponsesRequest(raw []byte) error {
	root, err := validateProtocolTopLevel(raw, "Anthropic Messages", map[string]bool{
		"model": true, "max_tokens": true, "system": true, "messages": true,
		"tools": true, "tool_choice": true, "metadata": true, "stream": true,
		"thinking": true, "output_config": true, "temperature": true, "top_p": true,
		"stop_sequences": true,
	})
	if err != nil {
		return err
	}
	if stops, ok := root["stop_sequences"].([]interface{}); ok && len(stops) > 0 {
		return errors.New("Anthropic stop_sequences cannot be represented safely by Responses")
	}
	return nil
}

func applyMessagesResponseControls(source, converted []byte) []byte {
	decode := func(raw []byte) map[string]interface{} {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var value map[string]interface{}
		if dec.Decode(&value) != nil {
			return nil
		}
		return value
	}
	sourceFields, output := decode(source), decode(converted)
	if sourceFields == nil || output == nil {
		return converted
	}
	if value, ok := sourceFields["max_tokens"]; ok {
		output["max_output_tokens"] = value
	}
	for _, field := range []string{"temperature", "top_p"} {
		if value, ok := sourceFields[field]; ok {
			output[field] = value
		}
	}
	raw, err := json.Marshal(output)
	if err != nil {
		return converted
	}
	return raw
}

func withAnthropicCustomHeaders(r *http.Request) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	if strings.TrimSpace(clone.Header.Get("Anthropic-Version")) == "" {
		clone.Header.Set("Anthropic-Version", "2023-06-01")
	}
	return clone
}

func withAnthropicContext1MBeta(r *http.Request) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	if anthropicContext1MRequested(clone.Header) {
		return clone
	}
	betas := make([]string, 0)
	for _, value := range clone.Header.Values("Anthropic-Beta") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				betas = append(betas, token)
			}
		}
	}
	betas = append(betas, anthropicContext1MBeta)
	clone.Header.Del("Anthropic-Beta")
	clone.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
	return clone
}

func withoutAnthropicContext1MBeta(r *http.Request) *http.Request {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	betas := make([]string, 0)
	for _, value := range clone.Header.Values("Anthropic-Beta") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" || strings.EqualFold(token, anthropicContext1MBeta) {
				continue
			}
			betas = append(betas, token)
		}
	}
	clone.Header.Del("Anthropic-Beta")
	if len(betas) > 0 {
		clone.Header.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	return clone
}

type customProtocolPipeWriter struct {
	header http.Header
	pipe   *io.PipeWriter
}

func (w *customProtocolPipeWriter) Header() http.Header         { return w.header }
func (w *customProtocolPipeWriter) WriteHeader(_ int)           {}
func (w *customProtocolPipeWriter) Write(p []byte) (int, error) { return w.pipe.Write(p) }
func (w *customProtocolPipeWriter) Flush()                      {}

func anthropicStreamToResponsesCustomSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher, plan *prompt.ResponsesToolBridgePlan) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer supervisor.Recover("custom-anthropic-responses-stream")
		defer close(done)
		defer writer.Close()
		pipeWriter := &customProtocolPipeWriter{header: make(http.Header), pipe: writer}
		anthropicStreamToChatSSE(pipeWriter, body, model, scrubber)
	}()
	chatStreamToResponsesSSE(w, reader, model, streamrewrite.New(nil), plan)
	_ = reader.Close()
	<-done
}

// settleStreamUsage records the streamed usage (if any) and settles the billing hold —
// the common tail of the three custom streaming paths.
func (s *Server) settleStreamUsage(r *http.Request, cc customCall, uscan *usage.StreamScanner) {
	if parsed, ok := uscan.Parsed(); ok {
		s.recordCustomParsedUsage(r, cc, parsed)
	}
	_ = s.settleBillingHold(r.Context(), cc.holdID, "settled_streaming")
}

func (s *Server) settleValidatedStreamUsage(r *http.Request, cc customCall, uscan *usage.StreamScanner, terminal bool) {
	if parsed, ok := uscan.Parsed(); ok {
		s.recordCustomParsedUsage(r, cc, parsed)
	}
	status := "failed_streaming"
	if terminal {
		status = "settled_streaming"
	}
	_ = s.settleBillingHold(r.Context(), cc.holdID, status)
}

func customUsageContext(ctx context.Context, cc customCall) context.Context {
	diagnostics := usageDiagnosticsFromCtx(ctx)
	diagnostics.UsageProvider = strings.TrimSpace(cc.provider)
	diagnostics.RouteEpoch = cc.lease.RouteEpoch
	ctx = withUsageDiagnostics(ctx, diagnostics)
	return withBillingHold(ctx, cc.holdID)
}

func (s *Server) recordCustomUsage(r *http.Request, cc customCall, body []byte) {
	s.recordUsage(customUsageContext(r.Context(), cc), cc.lease.Account.ID, cc.affinity.Hash, body)
}

func (s *Server) recordCustomParsedUsage(r *http.Request, cc customCall, parsed usage.Parsed) {
	s.recordParsedUsage(customUsageContext(r.Context(), cc), cc.lease.Account.ID, cc.affinity.Hash, parsed)
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
