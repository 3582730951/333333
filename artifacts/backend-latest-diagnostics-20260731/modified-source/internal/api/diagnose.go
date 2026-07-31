package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

const (
	routingAuditLogLimit   = 1
	routingAuditLogWindow  = 30 * time.Second
	routingAuditLogMaxKeys = 2048
)

// noAccountHTTPStatus maps a scheduler selection failure to a downstream HTTP status
// and a Retry-After hint in seconds (0 = none). Transient pool saturation becomes
// 429 + Retry-After so the client transparently re-sends the SAME request — no model,
// context, or reasoning change — instead of treating it as a hard server error. A busy
// server-side-state pin stays 409 (also retryable, short hint). Only a genuinely
// unroutable request (no matching account exists at all) stays 503.
func noAccountHTTPStatus(err error) (int, int) {
	if errors.Is(err, scheduler.ErrStrictUnavailable) {
		return http.StatusConflict, 2
	}
	var nae *scheduler.NoAccountError
	if errors.As(err, &nae) && nae.Retryable() {
		return http.StatusTooManyRequests, 3
	}
	return http.StatusServiceUnavailable, 0
}

// describeNoAccount turns the scheduler's opaque "no active account available" into
// an actionable 503 message. The overwhelmingly common cause of "I have GPT accounts
// in the pool but the API key says there is no candidate" is either a group mismatch
// or a provider/model capability filter that disagrees with the group-scoped model
// catalogue. This re-derives the high-signal facts — how many usable accounts
// the routed group has (by provider), and which OTHER groups DO have active accounts
// — so the operator can immediately see and fix the misroute. Best-effort and
// read-only; for a strict-sticky failure or any non-ErrNoAccount error (incl. DB
// errors) it returns the original error unchanged so status mapping stays correct.
func (s *Server) describeNoAccount(ctx context.Context, group, provider, model string, err error) error {
	if err == nil || !errors.Is(err, scheduler.ErrNoAccount) {
		return err
	}
	all, lerr := s.store.ListAccounts(ctx)
	if lerr != nil {
		return err
	}
	accountIDs := make([]string, 0, len(all))
	for _, account := range all {
		accountIDs = append(accountIDs, account.ID)
	}
	tokensByID, _ := s.store.ListTokensByAccountIDs(ctx, accountIDs)
	allowedProviders, normalizedModel, counters := noAccountRouteDiagnostics(provider, model, err)
	type gcount struct{ active, parked, codex, claude, kiro, antigravity, custom int }
	byGroup := map[string]*gcount{}
	now := storage.Now()
	for _, a := range all {
		g := byGroup[a.GroupName]
		if g == nil {
			g = &gcount{}
			byGroup[a.GroupName] = g
		}
		if a.Status != "active" || a.QuarantineUntil > now {
			g.parked++
			continue
		}
		g.active++
		token, tokenFound := tokensByID[a.ID]
		switch accountprovider.EffectiveProvider(a.Provider, token, tokenFound) {
		case "claude":
			g.claude++
		case "codex":
			g.codex++
		case "kiro":
			g.kiro++
		case "antigravity":
			g.antigravity++
		default:
			g.custom++
		}
	}
	prov := strings.Join(allowedProviders, ",")
	if prov == "" {
		prov = "any"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "no available account for this request (group=%s, provider=%s", group, prov)
	if normalizedModel != "" {
		fmt.Fprintf(&b, ", model=%s", normalizedModel)
	}
	b.WriteString("). ")
	cur := byGroup[group]
	switch {
	case cur == nil || (cur.active == 0 && cur.parked == 0):
		fmt.Fprintf(&b, "Group %q has no accounts. ", group)
	case cur.active == 0:
		fmt.Fprintf(&b, "Group %q has %d account(s) but all are quarantined or disabled. ", group, cur.parked)
	default:
		fmt.Fprintf(&b, "Group %q: %d active (codex=%d claude=%d kiro=%d antigravity=%d custom=%d), %d quarantined/disabled. ", group, cur.active, cur.codex, cur.claude, cur.kiro, cur.antigravity, cur.custom, cur.parked)
		if len(allowedProviders) == 1 && allowedProviders[0] == "codex" && cur.codex == 0 {
			b.WriteString("None of the active accounts are Codex/GPT accounts. ")
		} else if len(allowedProviders) == 1 && allowedProviders[0] == "claude" && cur.claude == 0 {
			b.WriteString("None of the active accounts are Claude accounts. ")
		} else if len(allowedProviders) == 1 && allowedProviders[0] == "kiro" && cur.kiro == 0 {
			b.WriteString("None of the active accounts are Kiro accounts. ")
		} else if len(allowedProviders) == 1 && allowedProviders[0] == "antigravity" && cur.antigravity == 0 {
			b.WriteString("None of the active accounts are Antigravity accounts. ")
		}
	}
	modelFiltered := counters.ModelUnsupported > 0
	if modelFiltered {
		claudeCount, kiroCount, antigravityCount := 0, 0, 0
		if cur != nil {
			claudeCount, kiroCount, antigravityCount = cur.claude, cur.kiro, cur.antigravity
		}
		fmt.Fprintf(&b, "Routing rejected normalized model %q for in-group provider candidates (claude=%d kiro=%d antigravity=%d); model_unsupported=%d. ", normalizedModel, claudeCount, kiroCount, antigravityCount, counters.ModelUnsupported)
	} else {
		// Where ARE the usable accounts? The smoking gun for a genuine group mismatch.
		var others []string
		for g, c := range byGroup {
			if g == group || c.active == 0 {
				continue
			}
			others = append(others, fmt.Sprintf("%q(%d active)", g, c.active))
		}
		if len(others) > 0 {
			sort.Strings(others)
			fmt.Fprintf(&b, "Active accounts ARE present in other group(s): %s. Point this API key's group there, or import accounts into %q.", strings.Join(others, ", "), group)
		}
	}
	// Capability hint: accounts present in-group but none advertise the requested model.
	if !modelFiltered && normalizedModel != "" && cur != nil && cur.active > 0 {
		if m, e := s.store.AccountsWithModel(ctx, group, normalizedModel); e == nil && len(m) == 0 {
			fmt.Fprintf(&b, " Note: no account in %q has model %q probed (other models may still work).", group, normalizedModel)
		}
	}
	return errors.New(strings.TrimSpace(b.String()))
}

func noAccountRouteDiagnostics(provider, model string, err error) ([]string, string, scheduler.NoAccountCounters) {
	providers := []string{}
	normalizedModel := strings.TrimSpace(model)
	var counters scheduler.NoAccountCounters
	var noAccount *scheduler.NoAccountError
	if errors.As(err, &noAccount) {
		providers = append(providers, noAccount.AllowedProviders...)
		if len(providers) == 0 && strings.TrimSpace(noAccount.Provider) != "" {
			providers = append(providers, noAccount.Provider)
		}
		if strings.TrimSpace(noAccount.Model) != "" {
			normalizedModel = strings.TrimSpace(noAccount.Model)
		}
		counters = noAccount.Counters
	}
	if len(providers) == 0 && strings.TrimSpace(provider) != "" {
		providers = append(providers, strings.Split(provider, ",")...)
	}
	seen := map[string]bool{}
	clean := providers[:0]
	for _, candidate := range providers {
		candidate = strings.ToLower(strings.TrimSpace(candidate))
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		clean = append(clean, candidate)
	}
	sort.Strings(clean)
	return clean, normalizedModel, counters
}

func (s *Server) activeKiroAccountsInGroup(ctx context.Context, group string) int {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return 0
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	tokensByID, _ := s.store.ListTokensByAccountIDs(ctx, ids)
	now := storage.Now()
	count := 0
	for _, account := range accounts {
		if account.GroupName != group || account.Status != "active" || account.QuarantineUntil > now {
			continue
		}
		token, found := tokensByID[account.ID]
		if accountprovider.EffectiveProvider(account.Provider, token, found) == "kiro" {
			count++
		}
	}
	return count
}

func (s *Server) writePublicNoAccountError(ctx context.Context, w http.ResponseWriter, status int, group, provider, model string, err error) {
	if err == nil {
		writePublicUnavailable(w, status)
		return
	}
	if !errors.Is(err, scheduler.ErrNoAccount) && !errors.Is(err, scheduler.ErrStrictUnavailable) {
		writeError(w, status, err)
		return
	}
	detail := err.Error()
	if d := s.describeNoAccount(ctx, group, provider, model, err); d != nil {
		detail = d.Error()
	}
	// Advertise a short Retry-After on retryable saturation so well-behaved clients
	// (Claude Code / Codex CLI both honor it) back off and re-send the identical request.
	if _, retryAfter := noAccountHTTPStatus(err); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	allowedProviders, normalizedModel, counters := noAccountRouteDiagnostics(provider, model, err)
	counterJSON, _ := json.Marshal(counters)
	reason := "no_public_account_detail"
	if counters.ModelUnsupported > 0 {
		reason = "model_unsupported"
	}
	auditKey := shortHash(fmt.Sprintf("%d|%s|%s|%s|%s|%s", status, group,
		strings.Join(allowedProviders, ","), normalizedModel, reason, counterJSON))
	if status == http.StatusConflict || s.routingAudits.allow(auditKey, time.Now()) {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "routing_unavailable",
			State:  "unavailable",
			Reason: reason,
			Detail: fmt.Sprintf("group_hash=%s allowed_providers=%s requested_model=%s normalized_model=%s kiro_accounts=%d status=%d counters=%s detail=%s",
				shortHash(group), strings.Join(allowedProviders, ","), model, normalizedModel, s.activeKiroAccountsInGroup(ctx, group), status, counterJSON, bodySnippet([]byte(detail), 600)),
		})
	} else {
		s.routingAuditSuppressed.Add(1)
	}
	writePublicUnavailable(w, status)
}

func (s *Server) routingAuditDiagnostics() map[string]interface{} {
	suppressed := uint64(0)
	if s != nil {
		suppressed = s.routingAuditSuppressed.Load()
	}
	return map[string]interface{}{
		"coalesce_limit":           routingAuditLogLimit,
		"coalesce_window_seconds":  int(routingAuditLogWindow / time.Second),
		"max_keys":                 routingAuditLogMaxKeys,
		"suppressed_repetitions":   suppressed,
		"strict_409_uncoalesced":   true,
		"route_attempts_preserved": true,
	}
}

func writePublicUnavailable(w http.ResponseWriter, status int) {
	if status <= 0 {
		status = http.StatusServiceUnavailable
	}
	errorBody := map[string]interface{}{
		"message": "Please retry.",
		"type":    "server_error",
	}
	if requestID := strings.TrimSpace(w.Header().Get(requestIDHeader)); requestID != "" {
		errorBody["request_id"] = requestID
	}
	writeJSON(w, status, map[string]interface{}{"error": errorBody})
}

func shortHash(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(v))
	return fmt.Sprintf("%x", sum[:6])
}
