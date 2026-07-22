package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

type upstreamErrorRuleDecision struct {
	Rule  storage.UpstreamErrorRule
	Match upstreamrules.MatchResult
}

func (s *Server) upstreamErrorRules(ctx context.Context) []storage.UpstreamErrorRule {
	s.upstreamRulesMu.RLock()
	cached, at := s.upstreamRulesCache, s.upstreamRulesCachedAt
	s.upstreamRulesMu.RUnlock()
	if cached != nil && time.Since(at) < 10*time.Second {
		return cached
	}
	rules, err := s.store.ListUpstreamErrorRules(ctx)
	if err != nil {
		return nil
	}
	s.upstreamRulesMu.Lock()
	s.upstreamRulesCache = rules
	s.upstreamRulesCachedAt = time.Now()
	s.upstreamRulesMu.Unlock()
	return rules
}
func (s *Server) invalidateUpstreamErrorRules() {
	s.upstreamRulesMu.Lock()
	s.upstreamRulesCache = nil
	s.upstreamRulesCachedAt = time.Time{}
	s.upstreamRulesMu.Unlock()
}

func (s *Server) responseRuleFilter(ctx context.Context, provider, entrypoint, model string, status int) *responseRuleFilter {
	rules := s.upstreamErrorRules(ctx)
	for _, rule := range rules {
		action := strings.ToLower(strings.TrimSpace(rule.DownstreamAction))
		if !rule.Enabled || (action != upstreamrules.DownstreamActionIntercept && action != upstreamrules.DownstreamActionHideSafetyBuffering) {
			continue
		}
		if action == upstreamrules.DownstreamActionIntercept && len(rule.BodyKeywords) == 0 {
			continue
		}
		probeRule := rule
		var probeBody []byte
		if action == upstreamrules.DownstreamActionHideSafetyBuffering {
			// This is a protocol-field transform, not a body keyword match.
			probeRule.BodyKeywords = nil
		} else if len(rule.BodyKeywords) > 0 {
			probeBody = []byte(rule.BodyKeywords[0])
		}
		probe := upstreamrules.MatchInput{Provider: provider, Entrypoint: entrypoint, Model: model, Status: status, Body: probeBody, Streaming: true}
		if m, ok := upstreamrules.Match([]storage.UpstreamErrorRule{probeRule}, probe); ok && m.DownstreamAction == action {
			return &responseRuleFilter{Keywords: append([]string(nil), rule.BodyKeywords...), CaseSensitive: rule.KeywordCaseSensitive, Mode: action, Rule: &rule}
		}
	}
	return nil
}

// hasPotentialTerminalResponseRule reports whether an enabled administrator rule
// could apply to an early terminal Responses frame.  A successful HTTP SSE response
// can carry response.failed / error as its first meaningful frame, before a status
// code or body is available to the normal matcher.  In that case the relay must hold
// only the bounded early prefix so an explicit downstream action (custom_error,
// failover, heartbeat_finish, ...) can be applied before any bytes are committed.
//
// Strict CPA keeps upstream session state native; it does not exempt an
// administrator's explicit response policy.  This scope-only probe deliberately
// clears status/body predicates and lets upstreamrules.Match perform the normal
// provider (including chatgpt<->codex), entrypoint, and model matching.
func (s *Server) hasPotentialTerminalResponseRule(ctx context.Context, provider, entrypoint, model string) bool {
	for _, rule := range s.upstreamErrorRules(ctx) {
		action := strings.ToLower(strings.TrimSpace(rule.DownstreamAction))
		if !rule.Enabled || action == upstreamrules.DownstreamActionIntercept || action == upstreamrules.DownstreamActionHideSafetyBuffering {
			continue
		}
		accountAction := strings.ToLower(strings.TrimSpace(rule.AccountAction))
		// A completely builtin rule has no administrator override to apply before
		// committing the native stream. Avoid adding a first-frame wait merely for
		// the ordinary default classifier; an explicit account action still probes.
		if (action == "" || action == upstreamrules.DownstreamActionBuiltin) &&
			(accountAction == "" || accountAction == upstreamrules.AccountActionBuiltin) {
			continue
		}
		probeRule := rule
		probeRule.StatusCodes = nil
		probeRule.BodyKeywords = nil
		if _, ok := upstreamrules.Match([]storage.UpstreamErrorRule{probeRule}, upstreamrules.MatchInput{
			Provider: provider, Entrypoint: entrypoint, Model: model, Streaming: true,
		}); ok {
			return true
		}
	}
	return false
}

func (s *Server) matchUpstreamErrorRule(ctx context.Context, input upstreamrules.MatchInput) (upstreamErrorRuleDecision, bool) {
	rules := s.upstreamErrorRules(ctx)
	if len(rules) == 0 {
		return upstreamErrorRuleDecision{}, false
	}
	// Stream transforms are evaluated by responseRuleFilter against ordinary response
	// frames. They are not terminal-error decisions and must not shadow a later 400/
	// keyword failover rule merely because they have an earlier creation timestamp.
	terminalRules := make([]storage.UpstreamErrorRule, 0, len(rules))
	for _, rule := range rules {
		action := strings.ToLower(strings.TrimSpace(rule.DownstreamAction))
		if action == upstreamrules.DownstreamActionIntercept || action == upstreamrules.DownstreamActionHideSafetyBuffering {
			continue
		}
		terminalRules = append(terminalRules, rule)
	}
	match, ok := upstreamrules.Match(terminalRules, input)
	if !ok {
		return upstreamErrorRuleDecision{}, false
	}
	return upstreamErrorRuleDecision{Rule: match.Rule, Match: match}, true
}

func (s *Server) applyRuleAccountAction(ctx context.Context, account storage.Account, status int, header http.Header, body []byte, decision upstreamErrorRuleDecision) ban.Verdict {
	switch decision.Match.AccountAction {
	case upstreamrules.AccountActionNone:
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionAutoContinue:
		// The account is healthy — the upstream merely stopped early. Never cool or
		// quarantine it; the continuation is handled by the auto-continue relay path.
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionCooldown:
		_ = s.store.SetBindingCooldown(ctx, account.ID, storage.Now()+ruleCooldownSeconds(decision.Rule, status, header, body))
		s.scheduler.NotifyStateChanged()
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionCooldownRecheck:
		_ = s.store.BenchBindingForRecheck(ctx, account.ID, storage.Now()+ruleCooldownSeconds(decision.Rule, status, header, body))
		s.scheduler.NotifyStateChanged()
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionQuarantine:
		seconds := decision.Rule.CooldownSeconds
		if seconds <= 0 {
			seconds = int64(s.cfg.QuarantineDurationHours) * 3600
		}
		if seconds <= 0 {
			seconds = 72 * 3600
		}
		_ = s.store.SetAccountQuarantine(ctx, account.ID, storage.Now()+seconds, "upstream error rule: "+decision.Rule.Name)
		if s.scheduler != nil {
			s.scheduler.InvalidateAccountCache()
		}
		return ban.Classify(false, status, header, body)
	default:
		return s.onUpstreamError(ctx, account, status, header, body)
	}
}

func ruleCooldownSeconds(rule storage.UpstreamErrorRule, status int, header http.Header, body []byte) int64 {
	seconds := rule.CooldownSeconds
	if seconds <= 0 {
		seconds = usageLimitCooldown(status, body)
	}
	if seconds <= 0 {
		seconds = 60
	}
	if rule.PreferRetryAfter && header != nil {
		seconds = limitCooldownSeconds(header, storage.Now(), seconds)
	}
	return seconds
}

func (s *Server) writeRuleCustomError(w http.ResponseWriter, rule storage.UpstreamErrorRule) {
	status := rule.ResponseStatus
	if status == 0 {
		status = http.StatusBadGateway
	}
	message := strings.TrimSpace(rule.CustomMessage)
	if message == "" {
		message = "Upstream provider returned an error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"message": message}})
}

func (s *Server) writeRuleNeutralError(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]interface{}{"message": "upstream temporarily unavailable"}})
}

func (s *Server) writeIdleStreamForRule(w http.ResponseWriter, rule storage.UpstreamErrorRule) {
	pingSeconds := rule.IdlePingSeconds
	if pingSeconds <= 0 {
		pingSeconds = 15
	}
	message := strings.TrimSpace(rule.CustomMessage)
	if message == "" {
		message = "upstream rule matched; keeping stream alive"
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeBeat := func() bool {
		_, err := fmt.Fprintf(w, ": %s\n\n", message)
		if flusher != nil {
			flusher.Flush()
		}
		return err == nil
	}
	if !writeBeat() {
		return
	}
	if rule.IdleSeconds == 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(pingSeconds) * time.Second)
	defer ticker.Stop()
	var deadline <-chan time.Time
	if rule.IdleSeconds > 0 {
		timer := time.NewTimer(time.Duration(rule.IdleSeconds) * time.Second)
		defer timer.Stop()
		deadline = timer.C
	}
	for {
		select {
		case <-deadline:
			return
		case <-ticker.C:
			if !writeBeat() {
				return
			}
		}
	}
}

func (s *Server) writeRuleDownstream(ctx context.Context, w http.ResponseWriter, provider string, status int, header http.Header, body []byte, words *streamrewrite.Matcher, decision upstreamErrorRuleDecision, streaming bool) bool {
	switch decision.Match.DownstreamAction {
	case upstreamrules.DownstreamActionCustomError:
		s.writeRuleCustomError(w, decision.Rule)
		return true
	case upstreamrules.DownstreamActionNeutralize:
		s.writeRuleNeutralError(w)
		return true
	case upstreamrules.DownstreamActionPass:
		s.writeFilteredError(ctx, w, provider, status, header, body, words)
		return true
	case upstreamrules.DownstreamActionIdleStream:
		if streaming {
			s.writeIdleStreamForRule(w, decision.Rule)
			return true
		}
		s.writeRuleNeutralError(w)
		return true
	case upstreamrules.DownstreamActionHeartbeatFinish:
		if streaming {
			s.writeHeartbeatFinishForRule(w, provider)
			return true
		}
		s.writeRuleNeutralError(w)
		return true
	default:
		return false
	}
}

// writeHeartbeatFinishForRule absorbs a matched upstream error on a streaming request
// by opening a normal SSE response, emitting exactly one provider-appropriate keepalive
// frame (Codex response.in_progress / Claude ping), and then returning so the stream
// closes cleanly. The downstream sees a benign, empty successful stream instead of the
// upstream error. This never fabricates model content — the single frame is a protocol
// keepalive, not a message delta.
func (s *Server) writeHeartbeatFinishForRule(w http.ResponseWriter, provider string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(heartbeatFrameFor(provider)))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
