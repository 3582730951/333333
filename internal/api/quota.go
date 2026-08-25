package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// This file turns the rate-limit headers an upstream returns on every response
// into a persisted, normalized per-account quota snapshot — the data behind the
// account-quota gauges in the admin UI. It reuses the parsing helpers in
// ratelimit.go (resetSeconds / parseResetTimestamp / parseDurationSeconds) which
// the cooldown logic already relies on, so the two stay consistent.
//
// Providers signal quota differently:
//   - Anthropic (Claude):   anthropic-ratelimit-{unified,tokens,requests}-{limit,remaining,reset,status}
//     The unified window is the 5-hour rolling budget Claude Pro/Max / Claude Code
//     are actually metered on, so it is preferred as the primary gauge.
//   - OpenAI (Codex):       x-ratelimit-{limit,remaining,reset}-{tokens,requests}
//
// A snapshot is only persisted when at least one window parsed, so a response
// without rate-limit headers never clobbers a previously captured snapshot.

// rateLimitHeaderPrefixes identifies which response headers describe quota, for
// the raw snapshot kept for the drawer detail view.
func isRateLimitHeader(name string) bool {
	l := strings.ToLower(name)
	return strings.Contains(l, "ratelimit") || strings.Contains(l, "rate-limit")
}

func headerInt(h http.Header, name string) (int64, bool) {
	v := strings.TrimSpace(h.Get(name))
	if v == "" {
		return 0, false
	}
	// Some limits arrive with thousands separators or trailing units. Strip
	// separators first so "1,234,567" parses as 1234567 (the old leading-run
	// logic would have read it as 1), then keep the leading integer run.
	v = strings.ReplaceAll(v, ",", "")
	end := 0
	for end < len(v) && (v[end] >= '0' && v[end] <= '9') {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[:end], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// resetEpoch returns the absolute epoch second a window resets, from either an
// Anthropic RFC3339 timestamp or an OpenAI duration string (e.g. "6m0s").
func resetEpoch(h http.Header, now int64, names ...string) int64 {
	for _, name := range names {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if ts := parseResetTimestamp(v, now); ts > 0 {
			return now + ts
		}
		if d := parseDurationSeconds(v); d > 0 {
			return now + d
		}
	}
	return 0
}

// usedPercentFrom computes how much of a window is consumed (0..100) given its
// limit and remaining counts. Returns -1 when it cannot be determined.
func usedPercentFrom(limit, remaining int64) float64 {
	if limit <= 0 || remaining < 0 {
		return -1
	}
	used := limit - remaining
	if used < 0 {
		used = 0
	}
	p := float64(used) / float64(limit) * 100
	if p > 100 {
		p = 100
	}
	return p
}

// parseQuotaSnapshot builds a normalized snapshot from a response's rate-limit
// headers. The bool is false when no window could be parsed.
func parseQuotaSnapshot(accountID, provider string, h http.Header, now int64) (storage.AccountRateLimit, bool) {
	snap := storage.AccountRateLimit{
		AccountID:         accountID,
		Provider:          provider,
		UsedPercent:       -1,
		LimitTokens:       -1,
		RemainingTokens:   -1,
		LimitRequests:     -1,
		RemainingRequests: -1,
		UpdatedAt:         now,
	}

	// Token + request windows, from whichever provider's header family is present.
	limTok, okLimTok := headerInt(h, "anthropic-ratelimit-tokens-limit")
	if !okLimTok {
		limTok, okLimTok = headerInt(h, "x-ratelimit-limit-tokens")
	}
	remTok, okRemTok := headerInt(h, "anthropic-ratelimit-tokens-remaining")
	if !okRemTok {
		remTok, okRemTok = headerInt(h, "x-ratelimit-remaining-tokens")
	}
	limReq, okLimReq := headerInt(h, "anthropic-ratelimit-requests-limit")
	if !okLimReq {
		limReq, okLimReq = headerInt(h, "x-ratelimit-limit-requests")
	}
	remReq, okRemReq := headerInt(h, "anthropic-ratelimit-requests-remaining")
	if !okRemReq {
		remReq, okRemReq = headerInt(h, "x-ratelimit-remaining-requests")
	}
	if okLimTok {
		snap.LimitTokens = limTok
	}
	if okRemTok {
		snap.RemainingTokens = remTok
	}
	if okLimReq {
		snap.LimitRequests = limReq
	}
	if okRemReq {
		snap.RemainingRequests = remReq
	}
	limInTok, okLimInTok := headerInt(h, "anthropic-ratelimit-input-tokens-limit")
	remInTok, okRemInTok := headerInt(h, "anthropic-ratelimit-input-tokens-remaining")
	limOutTok, okLimOutTok := headerInt(h, "anthropic-ratelimit-output-tokens-limit")
	remOutTok, okRemOutTok := headerInt(h, "anthropic-ratelimit-output-tokens-remaining")

	// Anthropic unified window — the metered budget for Claude Pro/Max / Claude
	// Code. Preferred as the primary gauge when present.
	uLim, okULim := headerInt(h, "anthropic-ratelimit-unified-limit")
	uRem, okURem := headerInt(h, "anthropic-ratelimit-unified-remaining")
	uStatus := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status"))

	switch {
	case okULim || okURem || uStatus != "":
		snap.Source = "unified"
		snap.Status = uStatus
		if okULim && okURem {
			snap.UsedPercent = usedPercentFrom(uLim, uRem)
		}
		snap.ResetAt = resetEpoch(h, now, "anthropic-ratelimit-unified-reset")
		// Surface the unified counts through the token fields when no dedicated
		// token window was sent, so the gauge always has numbers to show.
		if !okLimTok && okULim {
			snap.LimitTokens = uLim
		}
		if !okRemTok && okURem {
			snap.RemainingTokens = uRem
		}
	case okLimInTok && okRemInTok:
		snap.Source = "input_tokens"
		snap.Status = strings.TrimSpace(h.Get("anthropic-ratelimit-input-tokens-status"))
		snap.LimitTokens = limInTok
		snap.RemainingTokens = remInTok
		snap.UsedPercent = usedPercentFrom(limInTok, remInTok)
		snap.ResetAt = resetEpoch(h, now, "anthropic-ratelimit-input-tokens-reset")
		if okLimOutTok || okRemOutTok {
			snap.Raw = "" // populated below with both token dimensions for detail.
		}
	case okLimOutTok && okRemOutTok:
		snap.Source = "output_tokens"
		snap.Status = strings.TrimSpace(h.Get("anthropic-ratelimit-output-tokens-status"))
		snap.LimitTokens = limOutTok
		snap.RemainingTokens = remOutTok
		snap.UsedPercent = usedPercentFrom(limOutTok, remOutTok)
		snap.ResetAt = resetEpoch(h, now, "anthropic-ratelimit-output-tokens-reset")
	case okLimTok && okRemTok:
		snap.Source = "tokens"
		snap.UsedPercent = usedPercentFrom(limTok, remTok)
		snap.ResetAt = resetEpoch(h, now, "anthropic-ratelimit-tokens-reset", "x-ratelimit-reset-tokens")
	case okLimReq && okRemReq:
		snap.Source = "requests"
		snap.UsedPercent = usedPercentFrom(limReq, remReq)
		snap.ResetAt = resetEpoch(h, now, "anthropic-ratelimit-requests-reset", "x-ratelimit-reset-requests")
	default:
		return snap, false
	}

	// Keep the raw rate-limit headers (compact JSON) for the drawer detail view.
	raw := map[string]string{}
	for name, vals := range h {
		if isRateLimitHeader(name) && len(vals) > 0 {
			raw[strings.ToLower(name)] = vals[0]
		}
	}
	if len(raw) > 0 {
		if b, err := json.Marshal(raw); err == nil {
			snap.Raw = string(b)
		}
	}
	return snap, true
}

// captureQuota persists the latest quota snapshot for an account from a
// successful upstream response. Best-effort: a parse miss or store error is
// ignored so it never affects request handling.
//
// Session 31c: Added debug logging for Codex to diagnose missing/incorrect
// rate-limit headers (ChatGPT backend-api may not return standard x-ratelimit-*).
func (s *Server) captureQuota(ctx context.Context, accountID, provider, model string, header http.Header) {
	if accountID == "" || header == nil {
		return
	}

	// Session 31c: Debug logging for Codex rate-limit headers.
	// Enable via audit log when you suspect headers are missing or malformed.
	if provider == "codex" {
		hasAnyRateLimit := false
		for name := range header {
			if isRateLimitHeader(name) {
				hasAnyRateLimit = true
				break
			}
		}
		// Log when NO rate-limit headers found (likely backend-api behavior).
		if !hasAnyRateLimit && s.shouldAuditCodexNoRateLimitHeaders(accountID, provider, storage.Now()) {
			s.enqueueAudit(storage.AuditLogRow{
				AccountID: accountID,
				Action:    "codex_no_ratelimit_headers",
				Reason:    "backend-api response missing x-ratelimit-* headers",
				Detail:    fmt.Sprintf("provider=%s status=200 (this is NORMAL for chatgpt.com/backend-api; quota tracking unavailable)", provider),
			})
		}
	}

	snap, ok := parseQuotaSnapshot(accountID, provider, header, storage.Now())
	if !ok {
		return
	}
	snap.Model = strings.TrimSpace(model)
	if snap.LimiterType == "" {
		snap.LimiterType = snap.Source
	}
	s.scheduler.ApplyRateLimitSnapshot(snap)
	s.enqueueWrite(func() {
		writeCtx, cancel := s.bgWriteContext()
		defer cancel()
		_ = s.store.UpsertAccountRateLimit(writeCtx, snap)
	})
}

func (s *Server) shouldAuditCodexNoRateLimitHeaders(accountID, provider string, now int64) bool {
	key := accountID + "\x00" + provider
	hour := now / 3600
	if previous, loaded := s.missingLimitAudit.LoadOrStore(key, hour); loaded {
		if previous.(int64) >= hour {
			return false
		}
		s.missingLimitAudit.Store(key, hour)
	}
	return true
}
