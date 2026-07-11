package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/upstream"
	"codex-account-pool/internal/usage"
	"github.com/google/uuid"
)

func (s *Server) kiroMessagesWithLease(w http.ResponseWriter, r *http.Request, raw []byte, model string, affinity routing.AffinityKey, lease scheduler.Lease, allowRetry bool, exclude map[string]bool) attemptOutcome {
	defer lease.Release()
	kiroCfg := s.effectiveKiroConfig(r.Context())
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(raw, affinity.Hash, kirowire.ConversionOptions{DefaultThinking: kiroCfg.KiroDefaultThinking})
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return outcomeDone
	}
	data, outcome := s.doKiroAttempt(w, r, converted, lease, allowRetry, exclude)
	if outcome == outcomeRetry {
		return outcome
	}
	if data == nil {
		return outcomeDone
	}
	s.recordKiroUsage(r, lease.Account.ID, affinity, model, *data)
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if isStreamRequest(raw) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(kirowire.AnthropicSSE(*data, model, id))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return outcomeDone
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(kirowire.AnthropicJSON(*data, model, id))
	return outcomeDone
}

func (s *Server) kiroChatWithLease(w http.ResponseWriter, r *http.Request, anthBody []byte, model string, affinity routing.AffinityKey, lease scheduler.Lease, allowRetry bool, exclude map[string]bool) attemptOutcome {
	defer lease.Release()
	kiroCfg := s.effectiveKiroConfig(r.Context())
	converted, err := kirowire.ConvertAnthropicRequestWithOptions(anthBody, affinity.Hash, kirowire.ConversionOptions{DefaultThinking: kiroCfg.KiroDefaultThinking})
	if err != nil {
		writeKiroError(w, r, http.StatusBadRequest, err)
		return outcomeDone
	}
	data, outcome := s.doKiroAttempt(w, r, converted, lease, allowRetry, exclude)
	if outcome == outcomeRetry {
		return outcomeRetry
	}
	if data == nil {
		return outcomeDone
	}
	s.recordKiroUsage(r, lease.Account.ID, affinity, model, *data)
	id := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if isStreamRequest(anthBody) {
		w.Header().Set("Content-Type", "text/event-stream")
		anthropicStreamToChatSSE(w, bytes.NewReader(kirowire.AnthropicSSE(*data, model, id)), model, streamrewrite.New(nil))
		return outcomeDone
	}
	body, err := prompt.AnthropicToChatCompletion(kirowire.AnthropicJSON(*data, model, id), model)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return outcomeDone
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
	return outcomeDone
}

func (s *Server) doKiroAttempt(w http.ResponseWriter, r *http.Request, converted kirowire.Conversion, lease scheduler.Lease, allowRetry bool, exclude map[string]bool) (*kirowire.ResponseData, attemptOutcome) {
	kiroCfg := s.effectiveKiroConfig(r.Context())
	s.kiro.UpdateConfig(kiroCfg)
	cred, err := s.store.GetKiroCredentials(r.Context(), lease.Account.ID)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return nil, outcomeDone
	}
	token, err := s.store.GetToken(r.Context(), lease.Account.ID)
	if err != nil {
		writeKiroError(w, r, http.StatusBadGateway, err)
		return nil, outcomeDone
	}
	bearer, token, cred, err := s.kiro.Prepare(r.Context(), lease.Account, cred, token, lease.Egress, false)
	if err != nil {
		if errors.Is(err, kirowire.ErrInvalidGrant) {
			s.scheduler.InvalidateAccountCache()
		}
		return nil, s.kiroAttemptError(w, r, lease, 0, nil, []byte(err.Error()), allowRetry, exclude, err)
	}
	body := converted.Body
	if cred.ProfileARN != "" {
		var root map[string]interface{}
		if json.Unmarshal(body, &root) == nil {
			root["profileArn"] = cred.ProfileARN
			body, _ = json.Marshal(root)
		}
	}
	region := firstNonEmpty(cred.APIRegion, s.cfg.KiroDefaultAPIRegion, "us-east-1")
	target := "https://q." + region + ".amazonaws.com/generateAssistantResponse"
	if strings.HasPrefix(cred.Endpoint, "http://") || strings.HasPrefix(cred.Endpoint, "https://") {
		target = strings.TrimRight(cred.Endpoint, "/")
		if !strings.HasSuffix(target, "/generateAssistantResponse") {
			target += "/generateAssistantResponse"
		}
	}
	requestHeaders := kirowire.Headers(kiroCfg, cred, bearer, true)
	if converted.WebSearch != nil {
		target = strings.TrimSuffix(target, "/generateAssistantResponse") + "/mcp"
		body, _ = json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "id": converted.WebSearch.ToolUseID, "method": "tools/call",
			"params": map[string]interface{}{"name": "web_search", "arguments": map[string]interface{}{"query": converted.WebSearch.Query}},
		})
		requestHeaders.Set("accept", "application/json")
	}
	send := func(b string) (*upstream.Response, error) {
		headers := requestHeaders.Clone()
		headers.Set("authorization", "Bearer "+b)
		return s.upstream.DoRaw(r.Context(), lease.Egress, http.MethodPost, target, headers, body, lease.Binding.CookieJarKey)
	}
	resp, err := send(bearer)
	if err != nil {
		return nil, s.kiroAttemptError(w, r, lease, 0, nil, nil, allowRetry, exclude, err)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		rawErr := readUpstreamErrorBody(resp.Body)
		resp.Body.Close()
		if refreshed, _, updated, e := s.kiro.Prepare(r.Context(), lease.Account, cred, token, lease.Egress, true); e == nil {
			cred = updated
			resp, err = send(refreshed)
			if err != nil {
				return nil, s.kiroAttemptError(w, r, lease, 0, nil, nil, allowRetry, exclude, err)
			}
		} else {
			return nil, s.kiroAttemptError(w, r, lease, resp.StatusCode, resp.Header, rawErr, allowRetry, exclude, e)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawErr := readUpstreamErrorBody(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			_ = s.store.SetAccountStatus(r.Context(), lease.Account.ID, "invalid")
			s.scheduler.InvalidateAccountCache()
		}
		return nil, s.kiroAttemptError(w, r, lease, resp.StatusCode, resp.Header, rawErr, allowRetry, exclude, nil)
	}
	var data kirowire.ResponseData
	if converted.WebSearch != nil {
		raw, readErr := readLimited(resp.Body, s.cfg.MaxBodyBytes)
		if readErr == nil {
			data, readErr = kirowire.DecodeWebSearchResponse(raw, *converted.WebSearch, converted.InputTokens)
		}
		err = readErr
	} else {
		data, err = kirowire.DecodeResponse(resp.Body, converted.ToolNameMap)
		if data.InputTokens <= 1 && converted.InputTokens > data.InputTokens {
			data.InputTokens = converted.InputTokens
		}
	}
	resp.Body.Close()
	if err != nil {
		return nil, s.kiroAttemptError(w, r, lease, 0, resp.Header, nil, allowRetry, exclude, err)
	}
	return &data, outcomeDone
}

func writeKiroError(w http.ResponseWriter, r *http.Request, status int, err error) {
	if schedulerWaitTerminal(r.Context(), err.Error()) {
		return
	}
	writeError(w, status, err)
}

func (s *Server) effectiveKiroConfig(ctx context.Context) config.Config {
	cfg := s.cfg
	cfg.KiroVersion = s.settingString(ctx, "kiro_version", cfg.KiroVersion)
	cfg.KiroNodeVersion = s.settingString(ctx, "kiro_node_version", cfg.KiroNodeVersion)
	cfg.KiroDefaultAuthRegion = s.settingString(ctx, "kiro_default_auth_region", cfg.KiroDefaultAuthRegion)
	cfg.KiroDefaultAPIRegion = s.settingString(ctx, "kiro_default_api_region", cfg.KiroDefaultAPIRegion)
	cfg.KiroDefaultThinking = s.flagEnabled(ctx, "kiro_default_thinking", cfg.KiroDefaultThinking)
	return cfg
}

func (s *Server) kiroAttemptError(w http.ResponseWriter, r *http.Request, lease scheduler.Lease, status int, header http.Header, body []byte, allowRetry bool, exclude map[string]bool, cause error) attemptOutcome {
	transient := status == 0 || status == 408 || status == 429 || status == 503 || status >= 500
	if status == 401 || status == 403 {
		transient = true
	}
	if transient {
		s.onUpstreamError(r.Context(), lease.Account, status, header, body)
		if allowRetry {
			if exclude != nil {
				exclude[lease.Account.ID] = true
			}
			return outcomeRetry
		}
	}
	if cause != nil {
		if schedulerWaitTerminal(r.Context(), cause.Error()) {
			return outcomeDone
		}
		writeError(w, http.StatusBadGateway, cause)
	} else {
		code := status
		if code == 0 {
			code = http.StatusBadGateway
		}
		message := strings.TrimSpace(string(body))
		if schedulerWaitTerminal(r.Context(), message) {
			return outcomeDone
		}
		writeError(w, code, errors.New(message))
	}
	return outcomeDone
}

func (s *Server) recordKiroUsage(r *http.Request, accountID string, affinity routing.AffinityKey, model string, d kirowire.ResponseData) {
	raw, _ := json.Marshal(map[string]interface{}{"input_tokens": d.InputTokens, "output_tokens": d.OutputTokens, "cache_read_input_tokens": d.CacheReadTokens, "cache_creation_input_tokens": d.CacheCreationTokens})
	ctx := withUsageDiagnostics(r.Context(), storage.UsageDiagnostics{UsageProvider: "kiro", AffinitySource: affinity.Source})
	s.recordParsedUsage(ctx, accountID, affinity.Hash, usage.Parsed{Model: model, PromptTokens: d.InputTokens, CompletionTokens: d.OutputTokens, TotalTokens: d.InputTokens + d.OutputTokens, CachedTokens: d.CacheReadTokens, CacheReadTokens: d.CacheReadTokens, CacheCreationTokens: d.CacheCreationTokens, RawUsage: raw})
}
