package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/cloak"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/usage"
	"codex-account-pool/internal/virtual"
)

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
func (s *Server) handleChatViaClaude(w http.ResponseWriter, r *http.Request, raw []byte, model, routeGroup, forceEffort string) {
	stream := isStreamRequest(raw)
	anthBody, err := prompt.ChatCompletionToAnthropic(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	affinity := routing.ExtractAffinityKey(r, raw)
	lease, err := s.scheduler.Select(r.Context(), scheduler.Route{
		Group:           routeGroup,
		Provider:        "claude",
		Affinity:        affinity,
		Model:           model,
		EstimatedTokens: virtual.EstimateTokensJSON(raw),
	})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, scheduler.ErrStrictUnavailable) {
			status = http.StatusConflict
		}
		s.writePublicNoAccountError(r.Context(), w, status, routeGroup, "claude", model, err)
		return
	}
	defer lease.Release()

	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	osHint := s.osHint(raw, lease.Egress)
	id := identity.ForOS(s.identitySecret(), lease.Account.ID, osHint)
	cacheInject := s.cacheInjectEnabled(r.Context())
	claudeTTL := s.claudeCacheTTL(r.Context())
	result := cloak.VirtualizeClaudeCodeWithCache(anthBody, id, s.cfg.SensitiveWordsFor("claude"), claudeIsOAuth(token), "", cloak.ClaudeCodeCacheOptions{TTL: claudeTTL})
	// The OpenAI→Anthropic conversion emits no cache_control, so without this the
	// compat path is billed at full input price every turn. Inject the standard
	// Claude Code breakpoints on stable prefixes (system/tools/history, ≤4) so it
	// caches like native Claude Code. Quality/reasoning unchanged; only cached-token
	// cost drops.
	if cacheInject {
		result.Body = prompt.EnsureAnthropicCacheControl(result.Body, claudeTTL)
	}
	// Final body step (after cache_control injection): stamp the Claude Code
	// x-anthropic-billing-header so OAuth traffic relayed from an OpenAI-compatible
	// client still carries the header every genuine Claude Code request has, with a
	// cc_version coherent with our User-Agent and a fresh per-request cch.
	if claudeIsOAuth(token) {
		result.Body = cloak.EnsureClaudeCodeBillingHeader(result.Body, s.cfg.ClaudeCLIVersionOrDefault(id.ClaudeCLIVersion))
	}

	usageDiag := claudeRequestUsageDiagnostics(result.Body, affinity, claudeTTL, cacheInject)
	holdID, _ := s.store.CreateBillingHold(r.Context(), affinity.Hash, lease.Account.ID, virtual.EstimateTokensJSON(result.Body))
	r = r.WithContext(withUsageDiagnostics(withBillingHold(r.Context(), holdID), usageDiag))
	resp, err := s.upstream.Do(r.Context(), upstream.Request{
		Method:         http.MethodPost,
		Provider:       "claude",
		DownstreamPath: "/v1/messages",
		Headers:        r.Header.Clone(),
		Body:           result.Body,
		Account:        lease.Account,
		Token:          token,
		Egress:         lease.Egress,
		CookieJarKey:   lease.Binding.CookieJarKey,
		OSHint:         osHint,
	})
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_before_response")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		errBody := readUpstreamErrorBody(resp.Body)
		s.onUpstreamError(r.Context(), lease.Account, resp.StatusCode, resp.Header, errBody)
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_upstream")
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
	s.guardRateLimit(r.Context(), lease.Account.ID, resp.Header)
	s.captureQuota(r.Context(), lease.Account.ID, "claude", model, resp.Header)

	if stream && isEventStream(resp.Header) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		// Tee the upstream Anthropic SSE through a usage scanner so streamed
		// /v1/chat/completions traffic records token usage just like the native
		// /v1/messages path (the scanner reads the raw Anthropic frames, not the
		// rewritten chat chunks).
		uscan := usage.NewStreamScanner("claude")
		anthropicStreamToChatSSE(w, io.TeeReader(resp.Body, uscan), model, result.Scrubber)
		if parsed, ok := uscan.Parsed(); ok {
			s.recordParsedUsage(r.Context(), lease.Account.ID, affinity.Hash, parsed)
		}
		_ = s.store.SettleBillingHold(r.Context(), holdID, "settled_streaming")
		return
	}

	anthResp, err := s.readUpstreamResponseBody(resp.Body)
	if err != nil {
		_ = s.store.SettleBillingHold(r.Context(), holdID, "failed_response_too_large")
		writeError(w, http.StatusBadGateway, err)
		return
	}
	s.recordUsage(r.Context(), lease.Account.ID, affinity.Hash, anthResp)
	_ = s.store.SettleBillingHold(r.Context(), holdID, "settled")
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
func anthropicStreamToChatSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	chatID := "chatcmpl-claude"
	roleSent := false
	toolIdx := -1
	var finishReason interface{}

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
	done := func() {
		if finishReason == nil {
			finishReason = "stop"
		}
		emit(map[string]interface{}{}, finishReason)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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
		case "message_start":
			if m, ok := ev["message"].(map[string]interface{}); ok {
				if idv, ok := m["id"].(string); ok && idv != "" {
					chatID = idv
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
			if d, ok := ev["delta"].(map[string]interface{}); ok {
				if sr, ok := d["stop_reason"].(string); ok && sr != "" {
					finishReason = prompt.StopReasonToFinish(sr)
				}
			}
		case "message_stop":
			done()
			return
		}
	}
	done()
}

func asString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
