package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
)

const (
	codexResetCreditsLimiterType = "codex_reset_credits"
	codexSevenDayLimiterType     = "7d_polled"
	codexResetAuditAttempt       = "codex_reset_credit_attempt"
	codexResetAuditSuccess       = "codex_reset_credit_success"
	codexResetAuditFailure       = "codex_reset_credit_failure"
	codexResetAuditSkip          = "codex_reset_credit_skip"
	codexResetAuditUnknown       = "codex_reset_credit_unknown"
)

var (
	whamResetCreditsURL        = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	whamResetCreditsConsumeURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume"
)

type codexResetCreditsSnapshot struct {
	Known          bool
	Status         string
	AvailableCount int64
	Source         string
	UpdatedAt      int64
	Raw            string
}

type codexResetGateDecision struct {
	AllowConsume    bool
	Reason          string
	SevenDayResetAt int64
	QuotaUpdatedAt  int64
}

func parseCodexResetCredits(body []byte, source string) codexResetCreditsSnapshot {
	source = firstNonEmpty(strings.TrimSpace(source), "rate-limit-reset-credits")
	out := codexResetCreditsSnapshot{
		Known:  false,
		Status: "unknown",
		Source: source,
		Raw:    bodySnippet(body, 1000),
	}
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return out
	}
	target := root
	if source == "usage_fallback" {
		if nested, ok := root["rate_limit_reset_credits"].(map[string]interface{}); ok {
			target = nested
		} else if nested, ok := root["rateLimitResetCredits"].(map[string]interface{}); ok {
			target = nested
		}
	}
	if n, ok := resetCreditCount(target, "available_count", "availableCount"); ok {
		out.Known = true
		out.Status = "ok"
		out.AvailableCount = n
		return out
	}
	if source != "usage_fallback" {
		if credits, ok := root["credits"].([]interface{}); ok {
			var count int64
			for _, item := range credits {
				credit, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				status := strings.TrimSpace(jsonStringAny(credit, "status"))
				// This endpoint contains only rate-limit reset credits. Requiring
				// optional metadata such as reset_type or expires_at undercounts
				// otherwise valid entries when available_count is absent.
				if strings.EqualFold(status, "available") {
					count++
				}
			}
			out.Known = true
			out.Status = "ok"
			out.AvailableCount = count
			return out
		}
	}
	return out
}

// resetCreditCount accepts only a complete non-negative integer. The generic
// quota parser intentionally tolerates loose numeric strings, but a partial
// value such as "3abc" or a fractional count must not become a usable reset.
func resetCreditCount(m map[string]interface{}, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case float64:
			if v >= 0 && v < 1<<63 && math.Trunc(v) == v {
				return int64(v), true
			}
		case int64:
			if v >= 0 {
				return v, true
			}
		case int:
			if v >= 0 {
				return int64(v), true
			}
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err == nil && n >= 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func (s *Server) upsertCodexResetCreditsSnapshot(ctx context.Context, accountID string, snap codexResetCreditsSnapshot) {
	if strings.TrimSpace(accountID) == "" {
		return
	}
	now := snap.UpdatedAt
	if now == 0 {
		now = storage.Now()
	}
	remaining := int64(-1)
	if snap.Known {
		remaining = snap.AvailableCount
	}
	raw, _ := json.Marshal(map[string]interface{}{
		"status":          firstNonEmpty(snap.Status, "unknown"),
		"available_count": snap.AvailableCount,
		"known":           snap.Known,
		"source":          firstNonEmpty(snap.Source, "rate-limit-reset-credits"),
		"body_snippet":    snap.Raw,
	})
	_ = s.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         accountID,
		Provider:          "codex",
		LimiterType:       codexResetCreditsLimiterType,
		Source:            firstNonEmpty(strings.TrimSpace(snap.Source), "rate-limit-reset-credits"),
		UsedPercent:       -1,
		LimitTokens:       -1,
		RemainingTokens:   -1,
		LimitRequests:     -1,
		RemainingRequests: remaining,
		ResetAt:           0,
		Status:            firstNonEmpty(strings.TrimSpace(snap.Status), "unknown"),
		Raw:               string(raw),
		UpdatedAt:         now,
	})
}

func latestCodexResetCreditsSnapshot(snapshots []storage.AccountRateLimit) (codexResetCreditsSnapshot, bool) {
	snap := latestSnapshotWithLimiter(snapshots, codexResetCreditsLimiterType)
	if snap == nil {
		return codexResetCreditsSnapshot{}, false
	}
	known := snap.RemainingRequests >= 0 && strings.TrimSpace(snap.Status) == "ok"
	count := int64(0)
	if known {
		count = snap.RemainingRequests
	}
	return codexResetCreditsSnapshot{
		Known:          known,
		Status:         firstNonEmpty(strings.TrimSpace(snap.Status), "unknown"),
		AvailableCount: count,
		Source:         firstNonEmpty(strings.TrimSpace(snap.Source), codexResetCreditsLimiterType),
		UpdatedAt:      snap.UpdatedAt,
		Raw:            snap.Raw,
	}, true
}

func codexResetGateFromSnapshots(snapshots []storage.AccountRateLimit, now int64) codexResetGateDecision {
	snap := latestSnapshotWithLimiter(snapshots, codexSevenDayLimiterType)
	if snap == nil {
		return codexResetGateDecision{Reason: "7d_missing"}
	}
	if strings.HasPrefix(strings.TrimSpace(snap.Status), "error/") {
		return codexResetGateDecision{Reason: "7d_error", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
	}
	if snap.UpdatedAt <= 0 || now-snap.UpdatedAt > quotaFreshSeconds {
		return codexResetGateDecision{Reason: "7d_stale", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
	}
	if snap.ResetAt <= now {
		return codexResetGateDecision{Reason: "7d_window_expired", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
	}
	if snap.UsedPercent < 0 {
		return codexResetGateDecision{Reason: "7d_partial", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
	}
	if snap.UsedPercent >= 100 || strings.EqualFold(strings.TrimSpace(snap.Status), "rejected") {
		return codexResetGateDecision{AllowConsume: true, Reason: "7d_exhausted", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
	}
	return codexResetGateDecision{Reason: "7d_available", SevenDayResetAt: snap.ResetAt, QuotaUpdatedAt: snap.UpdatedAt}
}

func codexResetTriggerAllowed(status int, body []byte) bool {
	lb := strings.ToLower(string(body))
	for _, sig := range []string{
		"insufficient_scope", "permission denied", "unsupported_country", "unsupported country",
		"region", "country not supported", "account banned", "account disabled",
		"account deactivated", "invalid_api_key", "invalid api key", "unauthorized",
	} {
		if strings.Contains(lb, sig) {
			return false
		}
	}
	for _, sig := range []string{
		"insufficient_quota", "usage_limit_exceeded", "usage limit", "quota",
		"weekly limit", "7d", "rate_limit_exceeded", "too many requests",
	} {
		if strings.Contains(lb, sig) {
			return true
		}
	}
	return status == http.StatusTooManyRequests
}

func (s *Server) codexResetCreditsEnabled(ctx context.Context, account storage.Account) bool {
	if !s.flagEnabled(ctx, "codex_reset_credits_auto_enabled", s.cfg.CodexResetCreditsAutoEnabled) {
		return false
	}
	accountID := strings.TrimSpace(account.ID)
	group := strings.TrimSpace(account.GroupName)
	for _, denied := range s.settingCSV(ctx, "codex_reset_credits_account_denylist", s.cfg.CodexResetCreditsAccountDenylist) {
		if strings.TrimSpace(denied) == accountID {
			return false
		}
	}
	accountAllow := s.settingCSV(ctx, "codex_reset_credits_account_allowlist", s.cfg.CodexResetCreditsAccountAllowlist)
	groupAllow := s.settingCSV(ctx, "codex_reset_credits_group_allowlist", s.cfg.CodexResetCreditsGroupAllowlist)
	if len(accountAllow) == 0 && len(groupAllow) == 0 {
		return true
	}
	for _, allowed := range accountAllow {
		if strings.TrimSpace(allowed) == accountID {
			return true
		}
	}
	for _, allowed := range groupAllow {
		if strings.TrimSpace(allowed) == group {
			return true
		}
	}
	return false
}

func (s *Server) codexResetAccountLock(accountID string) func() {
	accountID = strings.TrimSpace(accountID)
	s.codexResetMu.Lock()
	if s.codexResetLocks == nil {
		s.codexResetLocks = map[string]*sync.Mutex{}
	}
	mu := s.codexResetLocks[accountID]
	if mu == nil {
		mu = &sync.Mutex{}
		s.codexResetLocks[accountID] = mu
	}
	s.codexResetMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

func (s *Server) tryAutoConsumeCodexResetCredit(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile, triggerAllowed bool, status int, header http.Header, body []byte, source string) bool {
	if strings.TrimSpace(account.ID) == "" || accountprovider.UsesAPIKey("codex", token) || !triggerAllowed || !s.codexResetCreditsEnabled(ctx, account) {
		return false
	}
	unlock := s.codexResetAccountLock(account.ID)
	defer unlock()

	now := storage.Now()
	snaps, err := s.codexResetSnapshots(ctx, account.ID)
	if err != nil {
		s.auditCodexResetCredit(ctx, account, codexResetAuditFailure, "snapshot_load_failed", map[string]interface{}{"error": err.Error(), "source": source})
		return false
	}
	gate := codexResetGateFromSnapshots(snaps, now)
	if !gate.AllowConsume {
		_ = s.pollOneCodexQuota(ctx, account, token, egress)
		snaps, _ = s.codexResetSnapshots(ctx, account.ID)
		gate = codexResetGateFromSnapshots(snaps, storage.Now())
	}
	if !gate.AllowConsume {
		s.auditCodexResetCredit(ctx, account, codexResetAuditSkip, gate.Reason, map[string]interface{}{"source": source, "http_status": status, "quota_updated_at": gate.QuotaUpdatedAt, "body_snippet": bodySnippet(body, 600)})
		return false
	}

	credits, ok := latestCodexResetCreditsSnapshot(snaps)
	if !ok || storage.Now()-credits.UpdatedAt > quotaFreshSeconds {
		if fetched, ferr := s.fetchAndPersistCodexResetCredits(ctx, account, token, egress); ferr == nil {
			credits = fetched
			ok = true
		}
	}
	if ok && credits.Known && credits.AvailableCount == 0 {
		s.auditCodexResetCredit(ctx, account, codexResetAuditSkip, "reset_credits_zero", map[string]interface{}{"source": source, "credits_before": 0, "quota_updated_at": gate.QuotaUpdatedAt})
		return false
	}
	unknownCredits := !ok || !credits.Known
	if unknownCredits {
		if !s.flagEnabled(ctx, "codex_reset_credits_unknown_consume_enabled", s.cfg.CodexResetCreditsUnknownConsumeEnabled) {
			s.auditCodexResetCredit(ctx, account, codexResetAuditSkip, "reset_credits_unknown_disabled", map[string]interface{}{"source": source, "quota_updated_at": gate.QuotaUpdatedAt})
			return false
		}
		s.auditCodexResetCredit(ctx, account, codexResetAuditUnknown, "reset_credits_unknown", map[string]interface{}{"source": source, "quota_updated_at": gate.QuotaUpdatedAt})
	}

	redeemID := uuid.NewString()
	claim, err := s.store.ClaimCodexResetCreditConsumption(ctx, account.ID, gate.SevenDayResetAt, redeemID, storage.Now())
	if err != nil {
		s.auditCodexResetCredit(ctx, account, codexResetAuditFailure, "reservation_failed", map[string]interface{}{"error": err.Error(), "source": source})
		return false
	}
	if !claim.Claimed {
		s.auditCodexResetCredit(ctx, account, codexResetAuditSkip, "already_reserved", map[string]interface{}{"source": source, "existing_status": claim.Row.Status, "redeem_request_id": claim.Row.RedeemRequestID})
		return false
	}

	detail := map[string]interface{}{
		"source":            source,
		"endpoint_path":     "/backend-api/wham/rate-limit-reset-credits/consume",
		"redeem_request_id": redeemID,
		"quota_updated_at":  gate.QuotaUpdatedAt,
		"http_status":       status,
		"credits_before":    credits.AvailableCount,
		"credits_known":     !unknownCredits,
		"body_snippet":      bodySnippet(body, 600),
	}
	if header != nil {
		detail["request_id"] = header.Get("x-request-id")
	}
	s.auditCodexResetCredit(ctx, account, codexResetAuditAttempt, "attempt", detail)

	consumeStatus, consumeBody, refreshedToken, err := s.consumeCodexResetCredit(ctx, account, token, egress, redeemID)
	if err != nil {
		_ = s.store.UpdateCodexResetCreditConsumptionStatus(ctx, account.ID, gate.SevenDayResetAt, "failure", storage.Now())
		detail["consume_status"] = consumeStatus
		detail["consume_body_snippet"] = bodySnippet(consumeBody, 600)
		detail["error"] = err.Error()
		s.auditCodexResetCredit(ctx, account, codexResetAuditFailure, "consume_failed", detail)
		return false
	}
	token = refreshedToken
	_ = s.pollOneCodexQuota(ctx, account, token, egress)
	after, ferr := s.fetchAndPersistCodexResetCredits(ctx, account, token, egress)
	if ferr == nil {
		detail["credits_after"] = after.AvailableCount
	}
	snaps, _ = s.codexResetSnapshots(ctx, account.ID)
	afterGate := codexResetGateFromSnapshots(snaps, storage.Now())
	if afterGate.AllowConsume {
		_ = s.store.UpdateCodexResetCreditConsumptionStatus(ctx, account.ID, gate.SevenDayResetAt, "unknown", storage.Now())
		detail["post_consume_gate"] = afterGate.Reason
		s.auditCodexResetCredit(ctx, account, codexResetAuditFailure, "quota_still_exhausted", detail)
		return false
	}
	_ = s.store.UpdateCodexResetCreditConsumptionStatus(ctx, account.ID, gate.SevenDayResetAt, "success", storage.Now())
	_ = s.store.ClearBindingRecheck(ctx, account.ID)
	detail["consume_status"] = consumeStatus
	s.auditCodexResetCredit(ctx, account, codexResetAuditSuccess, "success", detail)
	return true
}

func (s *Server) codexResetSnapshots(ctx context.Context, accountID string) ([]storage.AccountRateLimit, error) {
	byID, err := s.store.ListAccountRateLimitsByAccountIDs(ctx, []string{accountID})
	if err != nil {
		return nil, err
	}
	return byID[accountID], nil
}

func (s *Server) fetchAndPersistCodexResetCredits(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile) (codexResetCreditsSnapshot, error) {
	status, body, refreshed, err := s.codexResetCreditsGET(ctx, account, token, egress)
	if err == nil && status >= 200 && status < 300 {
		snap := parseCodexResetCredits(body, "rate-limit-reset-credits")
		snap.UpdatedAt = storage.Now()
		s.upsertCodexResetCreditsSnapshot(ctx, account.ID, snap)
		return snap, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if rr, rerr := s.refreshCodexToken(ctx, token); rerr == nil && rr.Refreshed {
			status, body, refreshed, err = s.codexResetCreditsGET(ctx, account, rr.Token, egress)
			_ = refreshed
			if err == nil && status >= 200 && status < 300 {
				snap := parseCodexResetCredits(body, "rate-limit-reset-credits")
				snap.UpdatedAt = storage.Now()
				s.upsertCodexResetCreditsSnapshot(ctx, account.ID, snap)
				return snap, nil
			}
		}
	}
	snap := codexResetCreditsSnapshot{Known: false, Status: "unknown", Source: "rate-limit-reset-credits", UpdatedAt: storage.Now(), Raw: bodySnippet(body, 1000)}
	s.upsertCodexResetCreditsSnapshot(ctx, account.ID, snap)
	if err != nil {
		return snap, err
	}
	return snap, fmt.Errorf("reset credits GET http %d", status)
}

func (s *Server) consumeCodexResetCredit(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile, redeemID string) (int, []byte, storage.AccountToken, error) {
	status, body, err := s.codexResetCreditsConsumePOST(ctx, account, token, egress, redeemID)
	if err == nil && status >= 200 && status < 300 && codexResetConsumed(body) {
		return status, body, token, nil
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if rr, rerr := s.refreshCodexToken(ctx, token); rerr == nil && rr.Refreshed {
			status, body, err = s.codexResetCreditsConsumePOST(ctx, account, rr.Token, egress, redeemID)
			if err == nil && status >= 200 && status < 300 && codexResetConsumed(body) {
				return status, body, rr.Token, nil
			}
			token = rr.Token
		}
	}
	if err != nil {
		return status, body, token, err
	}
	if status >= 200 && status < 300 {
		return status, body, token, fmt.Errorf("reset credit was not consumed: %s", bodySnippet(body, 300))
	}
	return status, body, token, fmt.Errorf("reset credit consume http %d", status)
}

func codexResetConsumed(body []byte) bool {
	var response struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &response) == nil && response.Code == "reset"
}

func (s *Server) codexResetCreditsGET(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile) (int, []byte, storage.AccountToken, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, whamResetCreditsURL, nil)
	applyCodexWhamHeaders(req.Header, account, token)
	client, err := s.upstream.EgressHTTPClient(egress)
	if err != nil {
		return 0, nil, token, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, token, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, token, nil
}

func (s *Server) codexResetCreditsConsumePOST(ctx context.Context, account storage.Account, token storage.AccountToken, egress storage.EgressProfile, redeemID string) (int, []byte, error) {
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": redeemID})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, whamResetCreditsConsumeURL, bytes.NewReader(payload))
	applyCodexWhamHeaders(req.Header, account, token)
	client, err := s.upstream.EgressHTTPClient(egress)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

func applyCodexWhamHeaders(h http.Header, account storage.Account, token storage.AccountToken) {
	accessToken := accountprovider.Credential("codex", token)
	if accessToken != "" {
		h.Set("Authorization", "Bearer "+accessToken)
	}
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("OpenAI-Beta", "codex-1")
	h.Set("Originator", "Codex Desktop")
	h.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	if id := codexChatGPTAccountID(account); id != "" {
		h.Set("ChatGPT-Account-Id", id)
	}
	if account.IsFedramp {
		h.Set("X-OpenAI-Fedramp", "true")
	}
}

func codexChatGPTAccountID(account storage.Account) string {
	// account.ID is the pool's local primary key, not a ChatGPT account ID.
	// Omitting the header is safer than routing the request with a fabricated ID.
	return strings.TrimSpace(firstNonEmpty(account.ChatGPTUserID, account.UpstreamAccountID))
}

func (s *Server) auditCodexResetCredit(ctx context.Context, account storage.Account, action, reason string, detail map[string]interface{}) {
	raw, _ := json.Marshal(detail)
	if err := s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID:    account.ID,
		AccountLabel: firstNonEmpty(account.Label, account.Email, account.ID),
		Action:       action,
		State:        "codex_reset_credits",
		Reason:       reason,
		Detail:       string(raw),
	}); err != nil {
		log.Printf("[CODEX-RESET-CREDITS] audit failed account=%s action=%s: %v", account.ID, action, err)
	}
}
