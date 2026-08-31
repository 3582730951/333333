package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

const (
	codex429FallbackDefaultSeconds int64 = 5
	codex429FallbackMaxSeconds     int64 = 7200
	codex429RateResetMaxSeconds    int64 = 2 * 60 * 60
	codex429UsageResetMaxSeconds   int64 = 8 * 24 * 60 * 60
)

// codex429GuardFallbackSeconds resolves the bounded synthetic cooldown used
// only when a confirmed upstream signal has no trustworthy quota/reset time.
// Zero is an explicit operator opt-out; invalid values fail back to the safe
// default rather than allowing an unbounded freeze.
func (s *Server) codex429GuardFallbackSeconds(ctx context.Context) int64 {
	seconds := int64(s.settingInt(ctx, "codex_429_guard_fallback_cooldown_seconds", s.cfg.Codex429GuardFallbackCooldownSeconds))
	if seconds == 0 {
		return 0
	}
	if seconds < 1 || seconds > codex429FallbackMaxSeconds {
		return codex429FallbackDefaultSeconds
	}
	return seconds
}

// codex429BodyClass accepts only the small set of explicit upstream codes used
// to distinguish a quota reset from a short RPM/TPM limit. It intentionally
// does not use substring matching over an error body.
func codex429BodyClass(body []byte) string {
	if len(body) == 0 || len(body) > 1<<20 {
		return ""
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]json.RawMessage
	if decoder.Decode(&root) != nil {
		return ""
	}
	values := make([]string, 0, 4)
	for _, key := range []string{"code", "type", "error_code"} {
		var value string
		if raw, ok := root[key]; ok && json.Unmarshal(raw, &value) == nil {
			values = append(values, value)
		}
	}
	if raw, ok := root["error"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			for _, key := range []string{"code", "type", "error_code"} {
				var value string
				if rawValue, exists := nested[key]; exists && json.Unmarshal(rawValue, &value) == nil {
					values = append(values, value)
				}
			}
		}
	}
	for _, value := range values {
		switch strings.TrimSpace(value) {
		case "usage_limit_reached", "GoUsageLimitError":
			return "usage_limit"
		case "rate_limit_exceeded":
			return "rate_limit"
		}
	}
	return ""
}

func codex429ExhaustedWindowSeconds(header http.Header, window string, now int64, max int64) int64 {
	used, err := strconv.ParseFloat(strings.TrimSpace(header.Get("x-codex-"+window+"-used-percent")), 64)
	if err != nil || used < 100 {
		return 0
	}
	seconds := parseDurationSeconds(header.Get("x-codex-" + window + "-reset-after-seconds"))
	if seconds <= 0 {
		seconds = parseResetTimestamp(header.Get("x-codex-"+window+"-reset-at"), now)
	}
	if seconds <= 0 || seconds > max {
		return 0
	}
	return seconds
}

// codexConfirmed429CooldownSeconds never fabricates a multi-hour cooldown from
// a plain 429. Only an exhausted, bounded Codex quota window is authoritative;
// otherwise Retry-After/reset is accepted under its class-specific ceiling, then
// the opt-in short fallback applies.
func (s *Server) codexConfirmed429CooldownSeconds(ctx context.Context, header http.Header, body []byte) (int64, string) {
	now := storage.Now()
	class := codex429BodyClass(body)
	if seconds := codex429ExhaustedWindowSeconds(header, "secondary", now, codex429UsageResetMaxSeconds); seconds > 0 {
		return seconds, "codex_7d_reset"
	}
	if seconds := codex429ExhaustedWindowSeconds(header, "primary", now, codex429UsageResetMaxSeconds); seconds > 0 {
		return seconds, "codex_5h_reset"
	}
	max := codex429RateResetMaxSeconds
	if class == "usage_limit" {
		max = codex429UsageResetMaxSeconds
	}
	if seconds := retryAfterSeconds(header, now); seconds > 0 && seconds <= max {
		return seconds, "retry_after"
	}
	if seconds := resetSeconds(header, now); seconds > 0 && seconds <= max {
		return seconds, "rate_limit_reset"
	}
	if seconds := s.codex429GuardFallbackSeconds(ctx); seconds > 0 {
		return seconds, "fallback"
	}
	return 0, "fallback_disabled"
}

// installConfirmedCodex429Cooldown is deliberately narrower than the ordinary
// limiter: it advances (never shortens) only the binding deadline and never sets
// recheck_pending. A confirmed quota/rate limit says capacity is unavailable, not
// that the credential or egress needs health revalidation.
func (s *Server) installConfirmedCodex429Cooldown(ctx context.Context, account storage.Account, header http.Header, body []byte) {
	if s == nil || s.store == nil || strings.TrimSpace(account.ID) == "" {
		return
	}
	seconds, source := s.codexConfirmed429CooldownSeconds(ctx, header, body)
	if seconds <= 0 {
		s.enqueueAudit(storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label,
			Action: "codex_429_cooldown_skipped", State: "confirmed", Reason: source, Detail: "no_trusted_or_enabled_deadline"})
		return
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forceCodex429StorageTimeout)
	defer cancel()
	extended, err := s.store.SetBindingCooldownIfLater(stateCtx, account.ID, storage.Now()+seconds)
	if err != nil {
		s.enqueueAudit(storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label,
			Action: "codex_429_cooldown_persist_failed", State: "confirmed", Reason: source, Detail: "deadline_write_failed"})
		return
	}
	action := "codex_429_cooldown_started"
	if !extended {
		action = "codex_429_cooldown_retained"
	}
	s.enqueueAudit(storage.AuditLogRow{AccountID: account.ID, AccountLabel: account.Label,
		Action: action, State: "confirmed", Reason: source, Detail: "bounded_deadline_seconds=" + strconv.FormatInt(seconds, 10)})
	if s.scheduler != nil {
		s.scheduler.NotifyStateChanged()
	}
	s.wakeRouteAvailability()
}

// codex429ConfirmationRetryDelay returns one bounded delay for the sole retry
// allowed after a first signal. It honors Retry-After/reset but never sleeps past
// the feature's eight-second request-side budget.
func codex429ConfirmationRetryDelay(header http.Header) time.Duration {
	seconds := retryAfterSeconds(header, storage.Now())
	if seconds <= 0 {
		seconds = resetSeconds(header, storage.Now())
	}
	if seconds <= 0 {
		seconds = 1 // bounded confirmation starts at 500 ms below.
	}
	delay := 500 * time.Millisecond
	if candidate := time.Duration(seconds) * time.Second; candidate > delay {
		delay = candidate
	}
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	return delay
}
