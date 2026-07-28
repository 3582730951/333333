package api

// Antigravity (Google Cloud Code) handler — serves /v1/messages requests
// routed to accounts with provider="antigravity".
//
// Flow:
//  1. Scheduler selects an antigravity account via Lease.
//  2. credentials are loaded from account_antigravity_credentials; access token
//     is refreshed if within the expiry window.
//  3. Every model is converted to and sent through Antigravity v1internal.
//  4. Before downstream commit, transport/status/early-stream failures return a
//     retry outcome so the scheduler can select another account.
//  5. Valid Gemini wire responses are translated back to Anthropic Messages.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"github.com/google/uuid"
)

const (
	// antigravityRefreshLeadSeconds: refresh the access token this many seconds
	// before expiry — matches the CLIProxyAPI refreshSkew constant (3000s).
	antigravityRefreshLeadSeconds = 3000
)

// antigravityMessagesWithLease serves a single /v1/messages request via the
// Antigravity upstream.  It has the same signature as kiroMessagesWithLease so
// it can be plugged into claudeMessagesAttempt / tryAntigravityAttempt without
// structural changes.
func (s *Server) antigravityMessagesWithLease(w http.ResponseWriter, r *http.Request, raw []byte, model string, lease scheduler.Lease, exclude map[string]bool) attemptOutcome {
	defer lease.Release()
	ctx := r.Context()
	affinity := routing.ExtractAffinityKey(r, raw)
	retry := func() attemptOutcome {
		if exclude != nil {
			exclude[lease.Account.ID] = true
		}
		return outcomeRetry
	}

	// --- 1. Load and optionally refresh credentials ---
	creds, err := s.store.GetAntigravityCredentials(ctx, lease.Account.ID)
	if err != nil {
		log.Printf("[ANTIGRAVITY] credentials not found account=%s: %v", lease.Account.ID, err)
		return retry()
	}
	token, creds, err := s.ensureAntigravityToken(ctx, creds, lease.Account, lease.Egress, lease.Binding.CookieJarKey)
	if err != nil {
		log.Printf("[ANTIGRAVITY] token refresh failed account=%s: %v", lease.Account.ID, err)
		return retry()
	}

	resolvedModel := resolvedAntigravityModel(model, lease)

	// --- 2. Forward to upstream ---
	stream := isStreamRequest(raw)
	req := upstream.AntigravityRequest{
		AccountID:       lease.Account.ID,
		AccessToken:     token,
		ProjectID:       creds.ProjectID,
		Model:           resolvedModel,
		BaseURL:         creds.BaseURL,
		UserAgent:       creds.UserAgent,
		Body:            raw,
		Stream:          stream,
		MaxOutputTokens: s.antigravityCatalogMaxOutputTokens(ctx, lease.Account.ID, resolvedModel),
	}
	doRequest := func() (*upstream.Response, storage.EgressProfile, error) {
		resp, finalEgress, requestErr := s.doAntigravityWithEgressRetry(ctx, req, lease)
		if requestErr != nil && upstream.IsAntigravityConversionError(requestErr) {
			writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_conversion", requestErr.Error())
			return nil, finalEgress, requestErr
		}
		return resp, finalEgress, requestErr
	}
	resp, finalEgress, err := doRequest()
	if err != nil {
		if upstream.IsAntigravityConversionError(err) {
			return outcomeDone
		}
		log.Printf("[ANTIGRAVITY] transport error account=%s model=%s: %v", lease.Account.ID, req.Model, err)
		return retry()
	}

	// A token can be revoked before its local expiry. Refresh once on 401, then
	// leave the account out of this request's remaining attempts if it still fails.
	if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(creds.RefreshToken) != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		creds.ExpiresAt = 0
		token, creds, err = s.ensureAntigravityToken(ctx, creds, lease.Account, lease.Egress, lease.Binding.CookieJarKey)
		if err != nil {
			log.Printf("[ANTIGRAVITY] forced token refresh failed account=%s: %v", lease.Account.ID, err)
			return retry()
		}
		req.AccessToken = token
		resp, finalEgress, err = doRequest()
		if err != nil {
			if upstream.IsAntigravityConversionError(err) {
				return outcomeDone
			}
			return retry()
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		failure := upstream.ClassifyAntigravityFailure(resp.StatusCode, resp.Header, errorBody, nil)
		if !failure.Failover {
			writeAntigravityUpstreamError(w, resp.StatusCode, resp.Header, errorBody)
			return outcomeDone
		}
		_ = s.onUpstreamError(ctx, lease.Account, resp.StatusCode, resp.Header, errorBody)
		log.Printf("[ANTIGRAVITY] upstream status account=%s model=%s status=%d class=%s; retrying another account", lease.Account.ID, req.Model, resp.StatusCode, failure.Class)
		return retry()
	}

	s.verifyAccountModel(ctx, lease.Account, resolvedModel, requestedClaudeModelFromContext(ctx).ContextMode)
	w.Header().Set("X-Pool-Resolved-Provider", "antigravity")
	w.Header().Set("X-Pool-Resolved-Model", resolvedModel)
	msgID := "msg_ag_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	displayModel := req.Model

	if stream {
		activityBody := newUpstreamActivityReadCloser(ctx, resp.Body, s.streamStallRecoveryInterval(ctx), 0, nil)
		reader := bufio.NewReaderSize(activityBody, 64*1024)
		prefix, probeErr := upstream.ProbeAntigravitySSE(reader)
		if probeErr != nil {
			_ = activityBody.Close()
			failure := upstream.ClassifyAntigravityFailure(0, nil, nil, probeErr)
			probeHashBody := raw
			if embedded, ok := upstream.AsAntigravityUpstreamError(probeErr); ok {
				probeHashBody = embedded.Body
			}
			s.recordProviderAttempt(requestIDFromContext(ctx), lease.Account.ID, "antigravity", "sse_probe", failure.Status, string(failure.Class), antigravityBodyHash(probeHashBody), "")
			if embedded, ok := upstream.AsAntigravityUpstreamError(probeErr); ok && !failure.Failover {
				writeAntigravityUpstreamError(w, embedded.StatusCode, nil, embedded.Body)
				return outcomeDone
			}
			if embedded, ok := upstream.AsAntigravityUpstreamError(probeErr); ok {
				_ = s.onUpstreamError(ctx, lease.Account, embedded.StatusCode, nil, embedded.Body)
			}
			log.Printf("[ANTIGRAVITY] early stream failure account=%s model=%s class=%s: %v", lease.Account.ID, req.Model, failure.Class, probeErr)
			return retry()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		inputTok, outputTok, cachedTok, stopReason, streamErr := upstream.AntigravityStreamToAnthropic(ctx, io.MultiReader(bytes.NewReader(prefix), reader), w, displayModel, msgID)
		if streamErr != nil {
			// The converter always emits a valid Anthropic terminal sequence after
			// downstream commit. Never replay here because tool calls or billable
			// output may already have reached the client.
			log.Printf("[ANTIGRAVITY] stream ended after commit account=%s: %v", lease.Account.ID, streamErr)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		s.recordAntigravityUsage(r, lease.Account.ID, displayModel, affinity, inputTok, outputTok, cachedTok, stopReason)
	} else {
		chunk, parseErr := upstream.ParseAntigravityNonStream(resp.Body)
		if parseErr != nil {
			log.Printf("[ANTIGRAVITY] non-stream parse failure account=%s model=%s; retrying", lease.Account.ID, req.Model)
			return retry()
		}
		respJSON := upstream.AntigravityChunkToAnthropicJSON(chunk, displayModel, msgID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respJSON)
		s.recordAntigravityUsage(r, lease.Account.ID, displayModel, affinity, chunk.InputTokens, chunk.OutputTokens, chunk.CachedTokens, chunk.StopReason)
	}
	// Persist session-sticky affinity binding for this account.
	if affinity.Hash != "" {
		_ = s.scheduler.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{
			RouteKeyHash: affinity.Hash,
			RouteKey:     affinity.Key,
			Source:       "antigravity",
			AccountID:    lease.Account.ID,
			Provider:     "antigravity",
			Model:        displayModel,
			EgressID:     finalEgress.ID,
		})
	}
	return outcomeDone
}

// doAntigravityWithEgressRetry exhausts the selected account's ordered outlets
// before the API layer is allowed to move to another account. Only transport
// failures and upstream 5xx responses are outlet-retryable; auth, quota, safety,
// and request validation remain account/model decisions.
func (s *Server) doAntigravityWithEgressRetry(ctx context.Context, req upstream.AntigravityRequest, lease scheduler.Lease) (*upstream.Response, storage.EgressProfile, error) {
	send := func(egress storage.EgressProfile, wireReq upstream.AntigravityRequest) (*upstream.Response, error) {
		cookieJarKey := lease.Binding.CookieJarKey
		if strings.TrimSpace(egress.ID) != "" {
			cookieJarKey = lease.Account.ID + ":" + egress.ID
		}
		return s.upstream.DoAntigravity(ctx, egress, cookieJarKey, wireReq)
	}

	egresses := []storage.EgressProfile{lease.Egress}
	for _, standbyID := range lease.Binding.StandbyIDs() {
		standbyID = strings.TrimSpace(standbyID)
		if standbyID == "" || standbyID == lease.Egress.ID {
			continue
		}
		standby, err := s.store.GetEgressProfile(ctx, standbyID)
		if err != nil || !scheduler.EgressHealthy(standby, storage.Now()) {
			continue
		}
		standby, err = s.store.ApplySidecarEgressBinding(ctx, lease.Binding, standby)
		if err != nil {
			continue
		}
		egresses = append(egresses, standby)
	}

	type attemptTarget struct {
		egress storage.EgressProfile
		base   string
	}
	targets := make([]attemptTarget, 0, len(egresses)*2)
	for _, egress := range egresses {
		for _, base := range upstream.AntigravityEndpointBases(req.BaseURL) {
			targets = append(targets, attemptTarget{egress: egress, base: base})
		}
	}
	if len(targets) == 0 {
		targets = append(targets, attemptTarget{egress: lease.Egress, base: req.BaseURL})
	}
	const maxRetries = 3
	var lastResp *upstream.Response
	var lastBody []byte
	var lastErr error
	lastEgress := lease.Egress
	for attempt := 0; attempt <= maxRetries; attempt++ {
		target := targets[attempt%len(targets)]
		attemptReq := req
		attemptReq.BaseURL = target.base
		resp, requestErr := send(target.egress, attemptReq)
		if upstream.IsAntigravityConversionError(requestErr) {
			return nil, target.egress, requestErr
		}
		failure := upstream.ClassifyAntigravityFailure(0, nil, nil, requestErr)
		var body []byte
		if requestErr == nil && resp != nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				s.recordProviderAttempt(requestIDFromContext(ctx), lease.Account.ID, "antigravity", "inference", resp.StatusCode, string(upstream.AntigravityFailureNone), antigravityBodyHash(req.Body), resp.Header.Get("Retry-After"))
				return resp, target.egress, nil
			}
			body, requestErr = upstream.DrainAndClose(resp.Body)
			if requestErr == nil {
				failure = upstream.ClassifyAntigravityFailure(resp.StatusCode, resp.Header, body, nil)
				resp.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		status := 0
		retryAfter := ""
		if resp != nil {
			status = resp.StatusCode
			retryAfter = resp.Header.Get("Retry-After")
		}
		hashBody := req.Body
		if len(body) > 0 {
			hashBody = body
		}
		s.recordProviderAttempt(requestIDFromContext(ctx), lease.Account.ID, "antigravity", "inference", status, string(failure.Class), antigravityBodyHash(hashBody), retryAfter)
		lastResp, lastBody, lastEgress, lastErr = resp, body, target.egress, requestErr
		if requestErr != nil || resp == nil {
			if !failure.Retryable {
				break
			}
			continue
		}
		if !failure.Retryable {
			return resp, target.egress, nil
		}
		_ = resp.Body.Close()
	}
	if lastResp != nil && lastErr == nil {
		lastResp.Body = io.NopCloser(bytes.NewReader(lastBody))
		return lastResp, lastEgress, nil
	}
	return nil, lastEgress, lastErr
}

// ensureAntigravityToken returns a valid access token for the account, refreshing
// it when it is within antigravityRefreshLeadSeconds of expiry.
func (s *Server) ensureAntigravityToken(ctx context.Context, creds storage.AntigravityCredentials, account storage.Account, egress storage.EgressProfile, cookieJarKey string) (string, storage.AntigravityCredentials, error) {
	now := time.Now().Unix()
	if creds.AccessToken != "" && creds.ExpiresAt > now+antigravityRefreshLeadSeconds {
		return creds.AccessToken, creds, nil
	}
	if creds.RefreshToken == "" {
		return "", creds, fmt.Errorf("no refresh token for account %s", account.ID)
	}
	tr, err := s.upstream.RefreshAntigravityToken(ctx, egress, cookieJarKey, creds.RefreshToken, &s.cfg)
	if err != nil {
		return "", creds, err
	}
	creds.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		creds.RefreshToken = tr.RefreshToken
	}
	creds.ExpiresAt = now + tr.ExpiresIn
	// Persist the refreshed token; non-fatal if the write fails.
	if wErr := s.store.UpsertAntigravityCredentials(ctx, creds); wErr != nil {
		log.Printf("[ANTIGRAVITY] failed to persist refreshed token account=%s: %v", account.ID, wErr)
	}
	return creds.AccessToken, creds, nil
}

// resolvedAntigravityModel picks the effective Gemini/Claude model slug for this
// request, honouring an account-level override and falling back to the requested model.
func resolvedAntigravityModel(requested string, lease scheduler.Lease) string {
	if rm := strings.TrimSpace(lease.ResolvedModel); rm != "" {
		return rm
	}
	return strings.TrimSpace(requested)
}

func (s *Server) antigravityCatalogMaxOutputTokens(ctx context.Context, accountID, exactModel string) int64 {
	caps, err := s.store.ListCapabilities(ctx, accountID)
	if err != nil {
		return 0
	}
	exactModel = strings.TrimSpace(exactModel)
	for _, cap := range caps {
		if strings.TrimSpace(cap.ModelSlug) != exactModel || strings.TrimSpace(cap.RawModelJSON) == "" {
			continue
		}
		var model struct {
			MaxOutputTokens int64 `json:"maxOutputTokens"`
		}
		if json.Unmarshal([]byte(cap.RawModelJSON), &model) == nil && model.MaxOutputTokens > 0 {
			return model.MaxOutputTokens
		}
	}
	return 0
}

func writeAntigravityUpstreamError(w http.ResponseWriter, status int, header http.Header, body []byte) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if contentType := header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if retryAfter := header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	if len(body) == 0 {
		body = []byte(`{"error":{"message":"` + http.StatusText(status) + `"}}`)
	}
	_, _ = w.Write(body)
}

func antigravityBodyHash(body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%x", digest[:])
}

// recordAntigravityUsage writes a lightweight usage row for billing/analytics.
// cachedTok is the number of tokens served from the Gemini explicit cache (0 = no hit).
func (s *Server) recordAntigravityUsage(r *http.Request, accountID, model string, affinity routing.AffinityKey, inputTok, outputTok, cachedTok int64, stopReason string) {
	ctx := context.Background()
	keyHash, userID := downstreamFromCtx(r.Context())
	raw, _ := json.Marshal(map[string]interface{}{
		"stop_reason":           stopReason,
		"provider":              "antigravity",
		"cached_content_tokens": cachedTok,
	})
	_ = s.store.InsertUsageRecordWithDiagnostics(ctx, accountID, affinity.Hash, keyHash, userID, model,
		inputTok, outputTok, inputTok+outputTok, cachedTok, cachedTok, 0, json.RawMessage(raw), storage.UsageDiagnostics{
			UsageProvider:    "antigravity",
			UsageSource:      "upstream",
			CacheReadPresent: cachedTok > 0,
			AffinitySource:   affinity.Source,
		})
}
