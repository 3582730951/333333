package cf

import (
	"context"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

type Detection struct {
	Matched  bool
	Category string
	CFRay    string
	Message  string
}

func Recordable(d Detection) bool {
	switch d.Category {
	case "cf_challenge", "cf_body", "identity_region_block":
		return true
	default:
		return false
	}
}

func EdgeOnly(d Detection) bool {
	return d.Matched && d.Category == "cf_edge"
}

func Detect(status int, header http.Header, body []byte) Detection {
	cfRay := header.Get("cf-ray")
	lowerBody := strings.ToLower(string(body))
	if strings.EqualFold(header.Get("cf-mitigated"), "challenge") {
		return Detection{Matched: true, Category: "cf_challenge", CFRay: cfRay, Message: "cf-mitigated challenge"}
	}
	// OpenAI/Anthropic API errors commonly carry cf-ray/server: cloudflare on
	// ordinary JSON 401/403/429 responses. Treat only explicit interstitial body
	// evidence as Cloudflare, otherwise auth/rate-limit classification owns it.
	// Unambiguous Cloudflare interstitial markers — safe to match in any body, since
	// they appear only in the challenge page itself, never in a normal API JSON error.
	for _, signal := range []string{"just a moment", "cf-chl", "cf_chl_", "challenge-platform", "cf-browser-verification", "attention required"} {
		if strings.Contains(lowerBody, signal) {
			return Detection{Matched: true, Category: "cf_body", CFRay: cfRay, Message: signal}
		}
	}
	// Broad words ("cloudflare"/"captcha"/"access denied") are matched ONLY inside an
	// HTML interstitial. They appear legitimately in JSON API errors — an Anthropic/
	// OpenAI error that merely mentions Cloudflare, or model output — and matching those
	// would falsely bench a healthy account (the "动不动就冷却" false positive). Requiring
	// HTML keeps true challenge pages while letting auth/rate-limit classification own
	// ordinary JSON errors.
	if looksHTML(header, body) {
		for _, signal := range []string{"cloudflare", "captcha", "access denied"} {
			if strings.Contains(lowerBody, signal) {
				return Detection{Matched: true, Category: "cf_body", CFRay: cfRay, Message: signal}
			}
		}
	}
	if looksHTML(header, body) && (status == 401 || status == 403 || status == 429 || status == 503) && cfRay != "" {
		return Detection{Matched: true, Category: "cf_body", CFRay: cfRay, Message: "cf-ray html interstitial"}
	}
	if looksHTML(header, body) && (status == 401 || status == 403 || status == 429 || status == 503) {
		server := strings.ToLower(header.Get("server"))
		if strings.Contains(server, "cloudflare") {
			return Detection{Matched: true, Category: "cf_body", CFRay: cfRay, Message: "cloudflare html interstitial"}
		}
	}
	identitySignals := []string{
		"unsupported_country",
		"unsupported_country_region_territory",
		"region_restricted",
		"\"blocked\"",
	}
	for _, signal := range identitySignals {
		if strings.Contains(lowerBody, signal) {
			return Detection{Matched: true, Category: "identity_region_block", CFRay: cfRay, Message: signal}
		}
	}
	if cfRay != "" && looksLikeEdgeOnly(status, lowerBody) {
		return Detection{Matched: true, Category: "cf_edge", CFRay: cfRay, Message: "cf-ray edge error"}
	}
	return Detection{CFRay: cfRay}
}

func looksLikeEdgeOnly(status int, lowerBody string) bool {
	if status == 429 || status < 400 {
		return false
	}
	for _, sig := range []string{
		"refresh_token_invalidated",
		"refresh_token_expired",
		"refresh_token_reused",
		"invalid_grant",
		"token expired",
		"token has expired",
		"missing bearer",
		"invalid api key",
		"invalid_api_key",
		"authentication_error",
		"insufficient_quota",
		"quota exceeded",
		"usage limit",
		"rate_limit_exceeded",
		"too many requests",
		"account_deactivated",
		"workspace_deactivated",
		"account_suspended",
	} {
		if strings.Contains(lowerBody, sig) {
			return false
		}
	}
	if strings.TrimSpace(lowerBody) == "" {
		return true
	}
	if status == 401 || status == 403 || status == 503 {
		return strings.Contains(lowerBody, "challenge") || strings.Contains(lowerBody, "blocked")
	}
	return status >= 500
}

func looksHTML(header http.Header, body []byte) bool {
	ct := strings.ToLower(header.Get("content-type"))
	if strings.Contains(ct, "text/html") {
		return true
	}
	trimmed := strings.TrimSpace(strings.ToLower(string(body)))
	return strings.HasPrefix(trimmed, "<!doctype html") || strings.HasPrefix(trimmed, "<html")
}

type StormBreaker struct {
	Store *storage.Store
}

func (s StormBreaker) Record(ctx context.Context, accountID, egressID string, status int, detection Detection) error {
	now := storage.Now()
	if err := s.Store.InsertCFEvent(ctx, storage.CFEvent{
		AccountID: accountID,
		EgressID:  egressID,
		Status:    status,
		CFRay:     detection.CFRay,
		Category:  detection.Category,
		Message:   detection.Message,
		CreatedAt: now,
	}); err != nil {
		return err
	}

	// L1: first CF hit cools the account-egress binding for 5 minutes.
	_ = s.Store.SetBindingCooldown(ctx, accountID, now+int64((5*time.Minute)/time.Second))

	sameBinding, err := s.Store.CountCFEvents(ctx, "account_id = ? AND egress_id = ? AND created_at >= ?", accountID, egressID, now-int64((10*time.Minute)/time.Second))
	if err == nil && sameBinding >= 2 {
		_ = s.Store.SetBindingCooldown(ctx, accountID, now+int64((30*time.Minute)/time.Second))
	}

	egressAccounts, err := s.Store.DistinctCFAccountsForEgress(ctx, egressID, now-int64((10*time.Minute)/time.Second))
	if err == nil && egressAccounts >= 5 {
		_ = s.Store.SetEgressCooldown(ctx, egressID, now+int64((60*time.Minute)/time.Second), detection.CFRay)
		_ = s.Store.SetEgressHealth(ctx, egressID, "tripped")
	}

	accountEgresses, err := s.Store.DistinctCFEgressForAccount(ctx, accountID, now-int64((60*time.Minute)/time.Second))
	if err == nil && accountEgresses >= 3 {
		_ = s.Store.SetAccountQuarantine(ctx, accountID, now+int64((2*time.Hour)/time.Second), "cf across three egress profiles")
	}
	return nil
}
