package api

// Antigravity (Google Cloud Code) handler — serves /v1/messages requests
// routed to accounts with provider="antigravity".
//
// Flow:
//  1. Scheduler selects an antigravity account via Lease.
//  2. credentials are loaded from account_antigravity_credentials; access token
//     is refreshed if within the expiry window.
//  3. For Claude models: cache_control breakpoints are injected (max_hit policy,
//     same as Kiro), then the request is forwarded to Vertex AI Anthropic publisher.
//     For Gemini models: the body is converted to Gemini format and forwarded to
//     cloudcode-pa.googleapis.com/v1internal.
//  4. The response is forwarded to the downstream client. Claude responses are
//     native Anthropic format (pass-through); Gemini responses are translated.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
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
func (s *Server) antigravityMessagesWithLease(w http.ResponseWriter, r *http.Request, raw []byte, model string, lease scheduler.Lease) attemptOutcome {
	defer lease.Release()
	ctx := r.Context()
	affinity := routing.ExtractAffinityKey(r, raw)

	// --- 1. Load and optionally refresh credentials ---
	creds, err := s.store.GetAntigravityCredentials(ctx, lease.Account.ID)
	if err != nil {
		log.Printf("[ANTIGRAVITY] credentials not found account=%s: %v", lease.Account.ID, err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("antigravity credentials not configured"))
		return outcomeDone
	}
	token, creds, err := s.ensureAntigravityToken(ctx, creds, lease.Account)
	if err != nil {
		log.Printf("[ANTIGRAVITY] token refresh failed account=%s: %v", lease.Account.ID, err)
		writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity token refresh failed"))
		return outcomeDone
	}

	resolvedModel := resolvedAntigravityModel(model, lease)
	isClaudePath := upstream.AntigravityIsClaudeModel(resolvedModel)

	// --- 2a. Claude: inject cache_control breakpoints (max_hit policy, same as Kiro).
	//         Gemini: resolve explicit CachedContent resource name (P8).
	var cacheRef, convKeyHash string
	if isClaudePath {
		raw = prompt.EnsureAnthropicCacheControlWithOptions(raw, prompt.AnthropicCacheControlOptions{
			Policy: "max_hit", LatestTailWrite: true, PreferRecentTurnRead: true,
		})
	} else {
		cacheRef, convKeyHash = s.resolveAntigravityCache(ctx, raw, resolvedModel, creds)
	}

	// --- 3. Forward to upstream ---
	stream := isStreamRequest(raw)
	req := upstream.AntigravityRequest{
		AccessToken:       token,
		ProjectID:         creds.ProjectID,
		Model:             resolvedModel,
		BaseURL:           creds.BaseURL,
		UserAgent:         creds.UserAgent,
		Body:              raw,
		Stream:            stream,
		CachedContentName: cacheRef,
	}
	resp, err := upstream.DoAntigravity(ctx, req)
	if err != nil {
		log.Printf("[ANTIGRAVITY] request error account=%s model=%s: %v", lease.Account.ID, req.Model, err)
		writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity upstream error: %v", err))
		return outcomeDone
	}

	// --- 4a. Gemini: evict stale Vertex cachedContent on 404/410 and retry once.
	if !isClaudePath && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) && cacheRef != "" {
		resp.Body.Close()
		if convKeyHash != "" {
			_ = s.store.DeleteAntigravityCacheEntry(ctx, lease.Account.ID, req.Model, convKeyHash)
		}
		req.CachedContentName = ""
		resp, err = upstream.DoAntigravity(ctx, req)
		if err != nil {
			log.Printf("[ANTIGRAVITY] retry (no-cache) error account=%s model=%s: %v", lease.Account.ID, req.Model, err)
			writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity upstream error: %v", err))
			return outcomeDone
		}
		log.Printf("[ANTIGRAVITY] cache miss evicted and retried account=%s model=%s hash=%s", lease.Account.ID, req.Model, convKeyHash)
	}

	// --- 4b. Claude: if Vertex rejected cache_control (400/422), strip and retry once.
	if isClaudePath && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity) {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if upstream.AntigravityClaudeVertexIsCacheRejected(resp.StatusCode, errBody) {
			log.Printf("[ANTIGRAVITY-CLAUDE] cache_control rejected, retrying without — account=%s model=%s", lease.Account.ID, req.Model)
			req.Body = stripAntigravityCacheControl(raw)
			resp, err = upstream.DoAntigravity(ctx, req)
			if err != nil {
				log.Printf("[ANTIGRAVITY] retry (no-cc) error account=%s model=%s: %v", lease.Account.ID, req.Model, err)
				writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity upstream error: %v", err))
				return outcomeDone
			}
		} else {
			// Non-cache error — surface it.
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(errBody)
			return outcomeDone
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		w.WriteHeader(resp.StatusCode)
		return outcomeDone
	}

	msgID := "msg_ag_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	displayModel := req.Model

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		var inputTok, outputTok, cachedTok int64
		var stopReason string
		var streamErr error
		if isClaudePath {
			// Vertex returns native Anthropic SSE — pass through directly.
			inputTok, outputTok, cachedTok, stopReason, streamErr = upstream.AntigravityClaudeVertexStreamToAnthropic(ctx, resp.Body, w, displayModel, msgID)
		} else {
			// Gemini SSE — translate to Anthropic format.
			inputTok, outputTok, cachedTok, stopReason, streamErr = upstream.AntigravityStreamToAnthropic(ctx, resp.Body, w, displayModel, msgID)
		}
		if streamErr != nil {
			log.Printf("[ANTIGRAVITY] stream error account=%s: %v", lease.Account.ID, streamErr)
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		s.recordAntigravityUsage(r, lease.Account.ID, displayModel, affinity, inputTok, outputTok, cachedTok, stopReason)
	} else {
		w.Header().Set("Content-Type", "application/json")
		if isClaudePath {
			// Vertex returns native Anthropic JSON — forward as-is; extract usage for billing.
			result, parseErr := upstream.ParseAntigravityClaudeVertexNonStream(resp.Body)
			if parseErr != nil {
				log.Printf("[ANTIGRAVITY-CLAUDE] non-stream parse error account=%s: %v", lease.Account.ID, parseErr)
				writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity response parse error"))
				return outcomeDone
			}
			_, _ = w.Write(result.RawBody)
			s.recordAntigravityUsage(r, lease.Account.ID, displayModel, affinity, result.InputTokens, result.OutputTokens, result.CachedTokens, result.StopReason)
		} else {
			// Gemini JSON — translate to Anthropic format.
			chunk, parseErr := upstream.ParseAntigravityNonStream(resp.Body)
			if parseErr != nil {
				log.Printf("[ANTIGRAVITY] non-stream parse error account=%s: %v", lease.Account.ID, parseErr)
				writeError(w, http.StatusBadGateway, fmt.Errorf("antigravity response parse error"))
				return outcomeDone
			}
			respJSON := upstream.AntigravityChunkToAnthropicJSON(chunk, displayModel, msgID)
			_, _ = w.Write(respJSON)
			s.recordAntigravityUsage(r, lease.Account.ID, displayModel, affinity, chunk.InputTokens, chunk.OutputTokens, chunk.CachedTokens, chunk.StopReason)
		}
	}
	// Persist session-sticky affinity binding for this account.
	if affinity.Hash != "" {
		_ = s.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{
			RouteKeyHash: affinity.Hash,
			RouteKey:     affinity.Key,
			Source:       "antigravity",
			AccountID:    lease.Account.ID,
			Provider:     "antigravity",
			Model:        displayModel,
		})
	}
	return outcomeDone
}

// ensureAntigravityToken returns a valid access token for the account, refreshing
// it when it is within antigravityRefreshLeadSeconds of expiry.
func (s *Server) ensureAntigravityToken(ctx context.Context, creds storage.AntigravityCredentials, account storage.Account) (string, storage.AntigravityCredentials, error) {
	now := time.Now().Unix()
	if creds.AccessToken != "" && creds.ExpiresAt > now+antigravityRefreshLeadSeconds {
		return creds.AccessToken, creds, nil
	}
	if creds.RefreshToken == "" {
		return "", creds, fmt.Errorf("no refresh token for account %s", account.ID)
	}
	tr, err := upstream.RefreshAntigravityToken(ctx, creds.RefreshToken, &s.cfg)
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

// antigravityCacheMinChars is the approximate character threshold below which
// creating an explicit CachedContent resource is not worthwhile. Gemini requires
// at least ~1k-4k tokens depending on the model; ~8000 chars ≈ 2000 tokens at
// 4 chars/token — a conservative lower bound that avoids expensive cache RPCs
// for short conversations.
const antigravityCacheMinChars = 8000

// resolveAntigravityCache looks up or creates a Gemini explicit CachedContent
// resource for the conversation prefix in body. It returns the cache resource
// name (for request.cachedContent) and the conversation key hash (for eviction
// on 404). Both strings are empty if caching is skipped or fails.
//
// Claude models are skipped here — they use cache_control injection instead.
// Errors are non-fatal: the caller falls back to sending the full request body.
func (s *Server) resolveAntigravityCache(ctx context.Context, body []byte, model string, creds storage.AntigravityCredentials) (cacheRef, convKeyHash string) {
	// Claude models use cache_control (Anthropic-native), not Gemini cachedContent.
	if upstream.AntigravityIsClaudeModel(model) {
		return "", ""
	}
	// Explicit caching requires a GCP project ID (Vertex AI).
	if creds.ProjectID == "" {
		return "", ""
	}
	// Skip if the prefix is too small to benefit.
	if upstream.ApproxAntigravityPrefixChars(body) < antigravityCacheMinChars {
		return "", ""
	}

	prefixTurns, _, systemText, keyHash := upstream.ExtractAntigravityPrefixForCache(body)
	if len(prefixTurns) == 0 && systemText == "" {
		return "", ""
	}
	convKeyHash = keyHash

	// Prune expired entries in the background — best-effort.
	go func() {
		defer supervisor.Recover("antigravity-cache-prune")
		_ = s.store.PruneExpiredAntigravityCacheEntries(context.Background())
	}()

	// Check for a live cache entry.
	entry, ok, err := s.store.GetAntigravityCacheEntry(ctx, creds.AccountID, model, keyHash)
	if err != nil {
		log.Printf("[ANTIGRAVITY-CACHE] lookup error account=%s model=%s: %v", creds.AccountID, model, err)
	}
	if ok && entry.CacheResourceName != "" {
		return entry.CacheResourceName, keyHash
	}

	// Create a new cache entry via Vertex AI.
	createReq := upstream.AntigravityCacheCreateRequest{
		AccessToken: creds.AccessToken,
		ProjectID:   creds.ProjectID,
		Model:       model,
		SystemText:  systemText,
		PrefixTurns: prefixTurns,
	}
	createResp, createErr := upstream.CreateAntigravityCachedContent(ctx, createReq)
	if createErr != nil {
		log.Printf("[ANTIGRAVITY-CACHE] create failed account=%s model=%s: %v", creds.AccountID, model, createErr)
		return "", convKeyHash
	}

	_ = s.store.UpsertAntigravityCacheEntry(ctx, storage.AntigravityCacheEntry{
		AccountID:         creds.AccountID,
		ModelID:           model,
		ConvKeyHash:       keyHash,
		CacheResourceName: createResp.Name,
		TotalTokens:       createResp.TotalTokens,
		ExpiresAt:         createResp.ExpiresAt,
	})
	log.Printf("[ANTIGRAVITY-CACHE] created account=%s model=%s name=%s tokens=%d",
		creds.AccountID, model, createResp.Name, createResp.TotalTokens)
	return createResp.Name, keyHash
}

// stripAntigravityCacheControl removes all cache_control fields from an Anthropic
// request body using a simple JSON walk. Used for the Claude-via-Vertex fallback
// retry when Vertex rejects the cache_control annotations.
//
// This removes cache_control from:
//   - messages[*].content[*].cache_control
//   - tools[*].cache_control
//   - system[*].cache_control (if system is an array)
func stripAntigravityCacheControl(body []byte) []byte {
	// Use gjson/sjson is complex for nested deletes; use a simple recursive approach
	// with the json.RawMessage path. For safety we fall back to the original body on
	// any error so the retry still fires.
	type contentBlock struct {
		CacheControl *json.RawMessage `json:"cache_control,omitempty"`
	}
	// Fast path: if no cache_control exists, skip the parse entirely.
	if !strings.Contains(string(body), "cache_control") {
		return body
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return body
	}

	// Strip from messages[*].content[*]
	if rawMsgs, ok := root["messages"]; ok {
		var msgs []map[string]json.RawMessage
		if err := json.Unmarshal(rawMsgs, &msgs); err == nil {
			for i, msg := range msgs {
				if rawContent, ok2 := msg["content"]; ok2 {
					var blocks []map[string]json.RawMessage
					if err2 := json.Unmarshal(rawContent, &blocks); err2 == nil {
						for j := range blocks {
							delete(blocks[j], "cache_control")
						}
						if b, e := json.Marshal(blocks); e == nil {
							msgs[i]["content"] = b
						}
					}
				}
			}
			if b, e := json.Marshal(msgs); e == nil {
				root["messages"] = b
			}
		}
	}

	// Strip from system[*] when system is an array of blocks.
	if rawSys, ok := root["system"]; ok {
		var sysBlocks []map[string]json.RawMessage
		if err := json.Unmarshal(rawSys, &sysBlocks); err == nil {
			for i := range sysBlocks {
				delete(sysBlocks[i], "cache_control")
			}
			if b, e := json.Marshal(sysBlocks); e == nil {
				root["system"] = b
			}
		}
	}

	// Strip from tools[*].
	if rawTools, ok := root["tools"]; ok {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(rawTools, &tools); err == nil {
			for i := range tools {
				delete(tools[i], "cache_control")
			}
			if b, e := json.Marshal(tools); e == nil {
				root["tools"] = b
			}
		}
	}

	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}
