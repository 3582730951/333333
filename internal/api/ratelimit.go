package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// This file implements proactive rate-limit avoidance (Q6): prefer the server-
// signaled cooldown window over a fixed guess, and pre-emptively rotate off an
// account whose rate-limit headers show it is at the wall — both reduce the number
// of requests that actually hit a limit (and the ban risk from hammering) without
// touching model quality, reasoning, or the conversation itself.

const maxCooldownSeconds = 3600

// retryAfterSeconds parses a Retry-After header (delta-seconds or HTTP-date).
func retryAfterSeconds(header http.Header, now int64) int64 {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n < 0 {
			return 0
		}
		return n
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Unix() - now; d > 0 {
			return d
		}
	}
	return 0
}

// limitCooldownSeconds returns the cooldown to apply on a rate-limited response,
// preferring the server-signaled Retry-After / reset window (so we retry exactly
// when allowed, not too early — which would re-trip the limit — nor too late),
// falling back to the caller's heuristic when no signal is present.
func limitCooldownSeconds(header http.Header, now, fallback int64) int64 {
	if ra := retryAfterSeconds(header, now); ra > 0 {
		return capCooldown(ra)
	}
	if r := resetSeconds(header, now); r > 0 {
		return capCooldown(r)
	}
	return fallback
}

// resetSeconds parses provider rate-limit reset windows: OpenAI duration strings
// (x-ratelimit-reset-*, e.g. "6m0s") and Anthropic RFC3339 timestamps
// (anthropic-ratelimit-*-reset). Returns the soonest non-zero reset.
func resetSeconds(header http.Header, now int64) int64 {
	var best int64
	consider := func(s int64) {
		if s > 0 && (best == 0 || s < best) {
			best = s
		}
	}
	for _, h := range []string{"x-ratelimit-reset-requests", "x-ratelimit-reset-tokens"} {
		consider(parseDurationSeconds(header.Get(h)))
	}
	for _, h := range []string{
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
		"anthropic-ratelimit-input-tokens-reset",
		"anthropic-ratelimit-output-tokens-reset",
		"anthropic-ratelimit-unified-reset",
	} {
		consider(parseResetTimestamp(header.Get(h), now))
	}
	return best
}

func parseDurationSeconds(v string) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil {
		return int64(d.Seconds() + 0.999)
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	return 0
}

func parseResetTimestamp(v string, now int64) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		if d := t.Unix() - now; d > 0 {
			return d
		}
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > now {
			return n - now // epoch seconds
		}
		return n
	}
	return 0
}

func capCooldown(s int64) int64 {
	if s > maxCooldownSeconds {
		return maxCooldownSeconds
	}
	if s < 1 {
		return 1
	}
	return s
}

// exhaustedCooldown returns a short pre-emptive cooldown when a SUCCESS response's
// rate-limit headers show the account is at the wall (remaining requests/tokens at
// zero). It only fires when truly exhausted, so an in-flight conversation that
// could still continue is never interrupted — the very next call would 429 anyway,
// so rotating now avoids a doomed request and reduces hammering/ban risk.
func exhaustedCooldown(header http.Header, now int64) int64 {
	if !isExhausted(header) {
		return 0
	}
	if s := resetSeconds(header, now); s > 0 {
		return capCooldown(s)
	}
	return 30
}

// isExhausted reports whether the account is truly at the rate-limit wall: ALL
// tracked dimensions (requests AND tokens, or unified) are at zero. A single
// dimension exhausted while others remain available is NOT exhaustion — e.g.,
// tokens=0 but requests=100 means small requests can still succeed.
//
// Session 31b fix: The old anyRemainingZero logic triggered on ANY dimension=0,
// causing false positives: "one message → immediate cooldown" when tokens ran out
// but requests remained, or when unified-remaining was 0 at account bootstrap.
//
// Session 31c caveat: If the upstream (e.g., ChatGPT backend-api) does NOT return
// x-ratelimit-* headers, this function will return false (assume not exhausted).
// That's correct behavior: without headers, we cannot proactively rotate; the
// account will only cooldown when it actually hits a 429 (via benchOnLimit).
func isExhausted(header http.Header) bool {
	// OpenAI/Codex uses separate requests+tokens dimensions (both must be zero).
	reqRemain := getRemaining(header, "x-ratelimit-remaining-requests")
	tokRemain := getRemaining(header, "x-ratelimit-remaining-tokens")
	if reqRemain.present && tokRemain.present {
		// Both dimensions tracked → exhausted only when BOTH are zero.
		return reqRemain.value <= 0 && tokRemain.value <= 0
	}

	// Anthropic uses separate requests+tokens OR a unified dimension.
	// Prefer the granular pair when both are present; fall back to unified.
	anthropicReq := getRemaining(header, "anthropic-ratelimit-requests-remaining")
	anthropicTok := getRemaining(header, "anthropic-ratelimit-tokens-remaining")
	if anthropicReq.present && anthropicTok.present {
		return anthropicReq.value <= 0 && anthropicTok.value <= 0
	}
	anthropicInput := getRemaining(header, "anthropic-ratelimit-input-tokens-remaining")
	anthropicOutput := getRemaining(header, "anthropic-ratelimit-output-tokens-remaining")
	if anthropicInput.present && anthropicOutput.present {
		return anthropicInput.value <= 0 && anthropicOutput.value <= 0
	}

	// Unified dimension is the sole signal → trust it.
	unified := getRemaining(header, "anthropic-ratelimit-unified-remaining")
	if unified.present {
		return unified.value <= 0
	}

	// No rate-limit headers present → not exhausted.
	return false
}

type remainingValue struct {
	value   int64
	present bool
}

func getRemaining(header http.Header, key string) remainingValue {
	v := strings.TrimSpace(header.Get(key))
	if v == "" {
		return remainingValue{0, false}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return remainingValue{n, true}
	}
	return remainingValue{0, false}
}

// anyRemainingZero is DEPRECATED (Session 31b) — replaced by isExhausted.
// Kept for reference; the old logic caused false positives by triggering on
// ANY dimension=0 instead of requiring ALL dimensions exhausted.
func anyRemainingZero(header http.Header) bool {
	for _, h := range []string{
		"x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens",
		"anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-tokens-remaining",
		"anthropic-ratelimit-input-tokens-remaining", "anthropic-ratelimit-output-tokens-remaining",
		"anthropic-ratelimit-unified-remaining",
	} {
		v := strings.TrimSpace(header.Get(h))
		if v == "" {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n <= 0 {
			return true
		}
	}
	return false
}
