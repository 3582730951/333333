package api

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// osHint returns the OS family to present for a request, honoring the configured
// identity source and the account's egress. Empty means "use the detected VPS host".
//
//   - identity_os_source=="downstream" → infer the family from the request body so
//     the reported OS matches the downstream's (preserved) real working directory.
//   - identity_os_source=="diverse"    → full cross-OS pool, always.
//   - otherwise ("vps"/empty): host-coherent UNLESS the account routes through its
//     own non-direct egress (proxy/sidecar with a distinct exit IP). In that case a
//     single OS family across many different egress IPs is itself anomalous, so we
//     diversify — OS variety should match IP variety. Accounts on the shared VPS
//     exit IP stay coherent with the host (and thus the TLS/IP origin).
func (s *Server) osHint(raw []byte, egress storage.EgressProfile) string {
	switch strings.ToLower(strings.TrimSpace(s.settingString(context.Background(), "identity_os_source", s.cfg.IdentityOSSource))) {
	case "downstream":
		return inferDownstreamOS(raw)
	case "diverse":
		return "diverse"
	}
	if perAccountEgress(egress) {
		return "diverse"
	}
	return ""
}

// perAccountEgress reports whether the request egresses through the account's own
// proxy/sidecar (a distinct exit IP) rather than the shared VPS interface. Empty or
// "direct" egress is the host's own IP → host-coherent identity; anything else is a
// per-account/rotating exit → diversify.
func perAccountEgress(e storage.EgressProfile) bool {
	if strings.TrimSpace(e.ID) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "", "direct":
		return false
	}
	return true
}

// inferDownstreamOS guesses the downstream OS family from path / platform markers
// commonly present in Codex and Claude Code request bodies (the working directory
// and, for Claude, the env "Platform:" line).
func inferDownstreamOS(raw []byte) string {
	switch {
	case bytes.Contains(raw, []byte("/Users/")) || bytes.Contains(raw, []byte("Platform: darwin")) || bytes.Contains(raw, []byte(`"darwin"`)):
		return "Mac OS"
	case bytes.Contains(raw, []byte("/home/")) || bytes.Contains(raw, []byte("Platform: linux")) || bytes.Contains(raw, []byte(`"linux"`)):
		return "Linux"
	case bytes.Contains(raw, []byte(`\Users\`)) || bytes.Contains(raw, []byte("Platform: win32")) || bytes.Contains(raw, []byte(`"win32"`)):
		return "Windows"
	}
	return ""
}

// ledgerKey returns the key under which virtual-context turns are pooled for a
// conversation. It pools turns only when the affinity came from a TRUE
// per-conversation correlator; for coarse/heuristic sources (downstream
// token+model, or the stable-prefix hash) it returns "" so the planner treats the
// request as self-contained and never merges sibling agents' turns into one
// reconstructed context (the multi-agent context-contamination guard).
func ledgerKey(a routing.AffinityKey) string {
	switch a.Source {
	case "x-codex-parent-thread-id", "thread_id", "conversation_id",
		"x-codex-window-id", "prompt_cache_key", "x-codex-turn-metadata":
		return a.Hash
	}
	return ""
}

// flagEnabled resolves a boolean runtime control flag: a stored override (set from
// the admin UI / settings table) wins, otherwise the config boot default.
func (s *Server) flagEnabled(ctx context.Context, key string, def bool) bool {
	if v, ok := s.runtimeSetting(ctx, key); ok {
		trimmed := strings.TrimSpace(v)
		switch strings.ToLower(trimmed) {
		case "1", "true", "on", "yes":
			return true
		case "0", "false", "off", "no":
			return false
		}
		logInvalidRuntimeSetting(key, trimmed, "boolean")
	}
	return def
}

// isolationEnabled reports whether per-account conversation isolation ("串号隔离")
// is currently active.
func (s *Server) isolationEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "conversation_isolation", s.cfg.ConversationIsolation)
}

// cacheInjectEnabled reports whether Anthropic cache_control auto-injection on the
// OpenAI-compat→Claude path is active.
func (s *Server) cacheInjectEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "claude_cache_control_inject", s.cfg.ClaudeCacheControlInject)
}

func (s *Server) nativeCacheBreakpointInjectEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "claude_native_cache_breakpoint_inject", s.cfg.ClaudeNativeCacheBreakpointInject)
}

// leakScrubEnabled reports whether pool-internal upstream signals (quota/limit
// headers, rate-limit SSE frames, limit/quota/overload error bodies, model-switch
// suggestions) are hidden from the downstream client.
func (s *Server) leakScrubEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "leak_scrub", s.cfg.LeakScrubEnabled)
}

// claudeCacheTTL returns the normalized injected-cache TTL ("1h" or "").
func (s *Server) claudeCacheTTL(ctx context.Context) string {
	if strings.TrimSpace(s.settingStringAllowEmpty(ctx, "claude_cache_ttl", s.cfg.ClaudeCacheTTL)) == "1h" {
		return "1h"
	}
	return ""
}

// isolateCodexConversation rewrites the downstream conversation-correlation
// identifiers to per-account values so the upstream can never tie one account's
// (possibly rate-limited / risk-flagged) session to another account that later
// serves the same conversation — the cause of the cross-account 401 cascade. The
// Session_id header is already per-account in the upstream layer; here we also
// namespace conversation_id and the x-codex-* thread/window/turn-metadata headers
// (request-only, never echoed) plus the body prompt_cache_key. Values are
// deterministic per (account, original), so a single account keeps stable
// conversation continuity and prompt-cache hits.
//
// Server-state identifiers (previous_response_id, x-codex-turn-state) are left
// untouched: they are strict-sticky and never cross accounts.
func isolateCodexConversation(header http.Header, body []byte, id identity.Identity) []byte {
	seed := id.MachineID
	for _, h := range []string{"Conversation_id", "X-Codex-Window-Id", "X-Codex-Parent-Thread-Id", "X-Codex-Turn-Metadata"} {
		if v := header.Get(h); v != "" {
			header.Set(h, identity.DerivedUUID(seed, v))
		}
	}
	if orig := routing.PromptCacheKey(body); orig != "" {
		ns := "cp_" + identity.DerivedKey(seed, orig)
		body = bytes.Replace(body, []byte(`"`+orig+`"`), []byte(`"`+ns+`"`), -1)
	}
	// Also namespace any conversation correlators carried in the BODY (some clients
	// put them there rather than in headers). A value the header and body share maps
	// to the SAME per-account namespaced value (DerivedUUID is a pure function of
	// seed+original), so header/body stay consistent — and account B never sees the
	// identifiers account A presented upstream, which is what prevents the upstream
	// from correlating the two and flagging B (the "串号" / cross-account-401 leak).
	for _, key := range []string{"conversation_id", "thread_id", "session_id"} {
		if orig := routing.JSONStringField(body, key); orig != "" {
			ns := identity.DerivedUUID(seed, orig)
			body = bytes.Replace(body, []byte(`"`+orig+`"`), []byte(`"`+ns+`"`), -1)
		}
	}
	return body
}

func stripCodexServerStateHeaders(header http.Header) http.Header {
	next := header.Clone()
	next.Del("X-Codex-Turn-State")
	return next
}

// onUpstreamError is the single reaction point for a 4xx/5xx upstream response.
// It first asks the ban classifier (high-confidence terminal verdicts only); a
// confirmed ban is audited and the account is deleted (or quarantined when
// ban_auto_delete is off). Anything recoverable (rate limit, auth, region) falls
// through to the rate-limit cooldown, which honors the server-signaled Retry-After.
// It returns the verdict so the caller can decide whether to fail over.
func (s *Server) onUpstreamError(ctx context.Context, account storage.Account, status int, header http.Header, body []byte) ban.Verdict {
	v := ban.Classify(false, status, header, body)
	if v.IsBanned() {
		s.handleBannedAccount(ctx, account, v, status, body, "upstream_error")
		return v
	}
	// PermissionDenied is a FUNCTION-level restriction (missing scope for a specific
	// API), not an ACCOUNT-level failure. It should NOT trigger automatic quarantine:
	//   - High false-positive rate: model output mentioning "api.responses.write" or
	//     "missing scopes" is mis-classified as a real error (Session 31 root cause).
	//   - Recoverable: the account is valid; only certain endpoints are restricted.
	//   - Transparent failover: if all accounts lack the scope, the 403 surfaces to
	//     the client (correct behavior); if one account has it, failover succeeds.
	//
	// The old auto-quarantine behavior caused cascading failures: one false positive
	// → all accounts quarantined → entire group offline. Now we only audit-log the
	// event and let failover + transparent error handling do their job.
	if v.State == ban.PermissionDenied {
		s.recordPermissionDeniedNoQuarantine(ctx, account, v, status, body, "upstream_error")
		return v
	}
	s.benchOnLimit(ctx, account.ID, status, header, body)
	return v
}

// retryableForFailover reports whether an error is account-specific and recoverable
// by moving to a fresh account (rate limit, region block, stale auth, or a ban that
// was just removed). A request-level error (400/404/422 bad request) is NOT
// retryable — a different account would fail identically — so it surfaces as-is.
func retryableForFailover(v ban.Verdict, status int) bool {
	switch v.State {
	case ban.RateLimited, ban.RegionBlocked, ban.AuthExpired, ban.PermissionDenied, ban.Banned:
		return true
	}
	return status == 429 || status >= 500
}

// handleBannedAccount records an audit entry (BEFORE any destructive action, so
// the trail survives) and then removes the account: hard delete when
// ban_auto_delete is on (the operator-requested behavior), else a long quarantine.
// It only ever runs on the high-confidence "banned" verdict.
func (s *Server) handleBannedAccount(ctx context.Context, account storage.Account, v ban.Verdict, status int, body []byte, source string) {
	action := "ban_quarantine"
	if s.flagEnabled(ctx, "ban_auto_delete", s.cfg.BanAutoDelete) {
		action = "ban_delete"
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID:    account.ID,
		AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action:       action,
		State:        string(v.State),
		Reason:       v.Reason,
		Detail:       fmt.Sprintf("source=%s provider=%s http=%d body=%s", source, account.PlanType, status, bodySnippet(body, 600)),
	})
	if s.flagEnabled(ctx, "ban_auto_delete", s.cfg.BanAutoDelete) {
		if err := s.store.DeleteAccount(ctx, account.ID); err != nil {
			_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
				AccountID:    account.ID,
				AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
				Action:       "ban_delete_failed",
				State:        string(v.State),
				Reason:       v.Reason,
				Detail:       fmt.Sprintf("source=%s provider=%s http=%d error=%v body=%s", source, account.PlanType, status, err, bodySnippet(body, 600)),
			})
			return
		}
		if s.scheduler != nil {
			s.scheduler.InvalidateAccountCache()
		}
		return
	}
	// Quarantine duration is now configurable (default 72h). Set 0 to disable.
	if s.cfg.QuarantineDurationHours > 0 {
		until := storage.Now() + int64(s.cfg.QuarantineDurationHours*3600)
		_ = s.store.SetAccountQuarantine(ctx, account.ID, until, "ban: "+v.Reason)
	}
}

// handlePermissionDeniedAccount is DEPRECATED and no longer called (Session 31).
//
// Historical context: This was the auto-quarantine handler for PermissionDenied
// verdicts. It has been removed from the call path because PermissionDenied is a
// FUNCTION-level restriction (missing scope for specific endpoints like /v1/agents),
// not an ACCOUNT-level failure. Auto-quarantine caused cascading outages when model
// output mentioned scope-related keywords ("api.responses.write", "missing scopes"),
// triggering false positives that quarantined entire account pools.
//
// Current behavior (onUpstreamError): PermissionDenied now only audit-logs. The
// current request can still fail over through the caller's exclude map, but the
// account remains active and schedulable for later requests.
//
// This function is kept for reference and potential future use under explicit admin
// control (e.g., a manual "quarantine this account for scope issues" button).
func (s *Server) handlePermissionDeniedAccount(ctx context.Context, account storage.Account, v ban.Verdict, status int, body []byte, source string) {
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID:    account.ID,
		AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action:       "auth_quarantine",
		State:        string(v.State),
		Reason:       v.Reason,
		Detail:       fmt.Sprintf("source=%s provider=%s http=%d body=%s", source, account.PlanType, status, bodySnippet(body, 600)),
	})
	// Quarantine duration is now configurable (default 72h). Set 0 to disable.
	if s.cfg.QuarantineDurationHours > 0 {
		until := storage.Now() + int64(s.cfg.QuarantineDurationHours*3600)
		_ = s.store.SetAccountQuarantine(ctx, account.ID, until, "permission denied: "+v.Reason+"; re-login with updated scopes required")
	}
}

func (s *Server) recordPermissionDeniedNoQuarantine(ctx context.Context, account storage.Account, v ban.Verdict, status int, body []byte, source string) {
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID:    account.ID,
		AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action:       "permission_denied_no_quarantine",
		State:        string(v.State),
		Reason:       v.Reason,
		Detail:       fmt.Sprintf("source=%s provider=%s http=%d body=%s", source, account.PlanType, status, bodySnippet(body, 600)),
	})
}

// guardRateLimit applies the proactive (success-path) cooldown so new conversations
// rotate off an account whose rate-limit headers show it is exhausted.
func (s *Server) guardRateLimit(ctx context.Context, accountID string, header http.Header) {
	if !s.flagEnabled(ctx, "rate_limit_guard_enabled", s.cfg.RateLimitGuardEnabled) {
		// Session 32: Added diagnostic log when guard is disabled
		log.Printf("[RATE-GUARD] DISABLED: account=%s (set rate_limit_guard_enabled=true in config to enable proactive switching)", accountID)
		return
	}
	if cd := exhaustedCooldown(header, storage.Now()); cd > 0 {
		log.Printf("[RATE-GUARD] COOLDOWN: account=%s, duration=%ds, reason=exhausted", accountID, cd)
		_ = s.store.SetBindingCooldown(ctx, accountID, storage.Now()+cd)
	}
}

func bodySnippet(body []byte, n int) string {
	s := strings.TrimSpace(string(body))
	if len(s) > n {
		return s[:n]
	}
	return s
}

// usageLimitCooldown returns how many seconds to bench an account whose upstream
// response indicates a usage/rate limit. Returns 0 when not a limit.
func usageLimitCooldown(status int, body []byte) int64 {
	lb := strings.ToLower(string(body))
	for _, sig := range []string{
		"usage_limit_exceeded", "usage limit", "usage_limit",
		"quota", "insufficient_quota", "exceeded your current",
		"rate_limit_exceeded", "too many requests",
	} {
		if strings.Contains(lb, sig) {
			log.Printf("[RATE-LIMIT-DETECT] matched keyword=%q status=%d body_len=%d", sig, status, len(body))
			return 1800
		}
	}
	if status == 429 {
		log.Printf("[RATE-LIMIT-DETECT] status=429 body_len=%d", len(body))
		return 60
	}
	return 0
}

// benchOnLimit cools the account's egress binding when the upstream signals a
// usage/rate limit, so the pool stops routing to it: non-strict conversations
// fail over to a fresh (namespaced) account, strict ones return 409 and the
// downstream restarts the conversation. When a Retry-After / rate-limit reset is
// present it is honored in preference to the heuristic window.
func (s *Server) benchOnLimit(ctx context.Context, accountID string, status int, header http.Header, body []byte) {
	cd := usageLimitCooldown(status, body)
	if cd == 0 {
		return
	}
	if s.flagEnabled(ctx, "rate_limit_guard_enabled", s.cfg.RateLimitGuardEnabled) && header != nil {
		cd = limitCooldownSeconds(header, storage.Now(), cd)
	}
	log.Printf("[RATE-LIMIT] COOLDOWN: account=%s, status=%d, duration=%ds, reason=usage_limit", accountID, status, cd)
	// Bench-for-recheck (not a plain cooldown): a rate-/usage-limited account must
	// pass a liveness probe before it re-enters the pool, so it can never silently
	// resurface still-limited the instant its cooldown elapses.
	_ = s.store.BenchBindingForRecheck(ctx, accountID, storage.Now()+cd)
}
