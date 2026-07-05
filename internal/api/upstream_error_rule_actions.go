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

type upstreamSlotReleaseKey struct{}

func withUpstreamSlotRelease(ctx context.Context, release func()) context.Context {
	if release == nil {
		return ctx
	}
	return context.WithValue(ctx, upstreamSlotReleaseKey{}, release)
}

func releaseUpstreamSlot(ctx context.Context) {
	if fn, ok := ctx.Value(upstreamSlotReleaseKey{}).(func()); ok && fn != nil {
		fn()
	}
}

type upstreamErrorRuleDecision struct {
	Rule  storage.UpstreamErrorRule
	Match upstreamrules.MatchResult
}

func (s *Server) matchUpstreamErrorRule(ctx context.Context, input upstreamrules.MatchInput) (upstreamErrorRuleDecision, bool) {
	rules, err := s.store.ListUpstreamErrorRules(ctx)
	if err != nil || len(rules) == 0 {
		return upstreamErrorRuleDecision{}, false
	}
	match, ok := upstreamrules.Match(rules, input)
	if !ok {
		return upstreamErrorRuleDecision{}, false
	}
	return upstreamErrorRuleDecision{Rule: match.Rule, Match: match}, true
}

func (s *Server) applyRuleAccountAction(ctx context.Context, account storage.Account, status int, header http.Header, body []byte, decision upstreamErrorRuleDecision) ban.Verdict {
	switch decision.Match.AccountAction {
	case upstreamrules.AccountActionNone:
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionCooldown:
		_ = s.store.SetBindingCooldown(ctx, account.ID, storage.Now()+ruleCooldownSeconds(decision.Rule, status, header, body))
		return ban.Classify(false, status, header, body)
	case upstreamrules.AccountActionCooldownRecheck:
		_ = s.store.BenchBindingForRecheck(ctx, account.ID, storage.Now()+ruleCooldownSeconds(decision.Rule, status, header, body))
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
	default:
		return false
	}
}
