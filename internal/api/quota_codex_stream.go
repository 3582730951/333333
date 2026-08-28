package api

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"codex-account-pool/internal/plantier"
	"codex-account-pool/internal/storage"
)

// Real-time Codex quota: chatgpt.com/backend-api normally does not return
// x-ratelimit-* headers on successful responses. Codex does, however, publish a
// bounded quota snapshot in codex.rate_limits stream frames. Capture those frames
// before the downstream leak filter removes them so the quota page stays current
// between /wham/usage polls.

const (
	maxCodexStreamAdditionalRateLimits = 8
	// Feature-specific windows are deliberately observation-only. They must not be
	// selected as the account's ordinary 5h quota or participate in routing.
	codexStreamFeaturesLimiterType = "codex_feature_observations"
)

type codexStreamQuotaWindow struct {
	present           bool
	usedPct           float64
	windowMinutes     int64
	resetAfterSeconds int64
	resetAfterPresent bool
	resetAt           int64
}

func (w codexStreamQuotaWindow) resolvedResetAt(now int64) int64 {
	if w.resetAt > 0 {
		return w.resetAt
	}
	if w.resetAfterPresent {
		return now + w.resetAfterSeconds
	}
	if w.windowMinutes > 0 {
		return now + w.windowMinutes*60
	}
	return 0
}

type codexStreamFeatureRateLimit struct {
	name      string
	primary   codexStreamQuotaWindow
	secondary codexStreamQuotaWindow
}

func (f codexStreamFeatureRateLimit) any() bool {
	return f.primary.present || f.secondary.present
}

// codexStreamRateLimits is a normalized, bounded projection of both the legacy
// snake_case SSE payload and the camelCase websocket payload observed by
// CLIProxyAPI. Credits and reset-credit fields are intentionally absent: this
// passive path performs no payment, spending, or quota-reset action.
type codexStreamRateLimits struct {
	planType    string
	activeLimit string
	primary     codexStreamQuotaWindow
	secondary   codexStreamQuotaWindow
	additional  []codexStreamFeatureRateLimit
	codeReview  *codexStreamFeatureRateLimit
}

func (r codexStreamRateLimits) any() bool {
	return r.planType != "" || r.activeLimit != "" || r.primary.present || r.secondary.present ||
		len(r.additional) > 0 || (r.codeReview != nil && r.codeReview.any())
}

func (r codexStreamRateLimits) featureObservationsPresent() bool {
	return r.activeLimit != "" || len(r.additional) > 0 || (r.codeReview != nil && r.codeReview.any())
}

// mergeCodexStreamRateLimits keeps earlier complete window observations when a
// later frame contains only a plan update. This matters for rate_limits.updated
// frames emitted after the full codex.rate_limits frame in the same response.
func mergeCodexStreamRateLimits(current, next codexStreamRateLimits) codexStreamRateLimits {
	if next.planType != "" {
		current.planType = next.planType
	}
	if next.activeLimit != "" {
		current.activeLimit = next.activeLimit
	}
	if next.primary.present {
		current.primary = next.primary
	}
	if next.secondary.present {
		current.secondary = next.secondary
	}
	if len(next.additional) > 0 {
		current.additional = append([]codexStreamFeatureRateLimit(nil), next.additional...)
	}
	if next.codeReview != nil && next.codeReview.any() {
		copyReview := *next.codeReview
		current.codeReview = &copyReview
	}
	return current
}

// parseCodexRateLimitsEvent accepts the wire spellings used by Codex SSE and
// websocket transports. A plan-only event is useful and therefore succeeds even
// without quota windows; malformed or partial windows never replace a complete
// /wham/usage snapshot.
func parseCodexRateLimitsEvent(ev map[string]interface{}) (codexStreamRateLimits, bool) {
	var out codexStreamRateLimits
	out.planType = firstCodexStreamText(ev, false, "plan_type", "planType")
	out.activeLimit = firstCodexStreamText(ev, true, "metered_limit_name", "meteredLimitName", "limit_name", "limitName")

	rateLimits := firstCodexStreamMap(ev, "rate_limits", "rateLimit", "rateLimits")
	if rateLimits != nil {
		if out.planType == "" {
			out.planType = firstCodexStreamText(rateLimits, false, "plan_type", "planType")
		}
		if out.activeLimit == "" {
			out.activeLimit = firstCodexStreamText(rateLimits, true, "metered_limit_name", "meteredLimitName", "limit_name", "limitName", "active_limit", "activeLimit")
		}
		out.primary, _ = parseCodexStreamWindow(firstCodexStreamMap(rateLimits, "primary", "primary_window", "primaryWindow"))
		out.secondary, _ = parseCodexStreamWindow(firstCodexStreamMap(rateLimits, "secondary", "secondary_window", "secondaryWindow"))
	}

	out.additional = parseCodexStreamAdditionalLimits(firstCodexStreamValue(ev, "additional_rate_limits", "additionalRateLimits"))
	if review := firstCodexStreamMap(ev, "code_review_rate_limits", "codeReviewRateLimits"); review != nil {
		if nested := firstCodexStreamMap(review, "rate_limit", "rateLimit"); nested != nil {
			review = nested
		}
		feature := codexStreamFeatureRateLimit{name: "code_review"}
		feature.primary, _ = parseCodexStreamWindow(firstCodexStreamMap(review, "primary", "primary_window", "primaryWindow"))
		feature.secondary, _ = parseCodexStreamWindow(firstCodexStreamMap(review, "secondary", "secondary_window", "secondaryWindow"))
		if feature.any() {
			out.codeReview = &feature
		}
	}
	return out, out.any()
}

func parseCodexStreamWindow(raw map[string]interface{}) (codexStreamQuotaWindow, bool) {
	var out codexStreamQuotaWindow
	if raw == nil {
		return out, false
	}
	used, ok := firstCodexStreamFloat(raw, "used_percent", "usedPercent")
	if !ok || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return out, false
	}
	out.usedPct = used
	if minutes, ok := firstCodexStreamFloat(raw, "window_minutes", "windowMinutes"); ok && minutes > 0 && minutes <= math.MaxInt64/60 {
		out.windowMinutes = int64(minutes)
	} else if seconds, ok := firstCodexStreamFloat(raw, "limit_window_seconds", "limitWindowSeconds"); ok && seconds > 0 && seconds <= math.MaxInt64 {
		out.windowMinutes = int64(seconds) / 60
	}
	if resetAfter, ok := firstCodexStreamFloat(raw, "reset_after_seconds", "resetAfterSeconds", "resets_in_seconds", "resetsInSeconds"); ok && resetAfter >= 0 && resetAfter <= math.MaxInt64 {
		out.resetAfterSeconds = int64(resetAfter)
		out.resetAfterPresent = true
	}
	if resetAt, ok := firstCodexStreamFloat(raw, "reset_at", "resetAt", "resets_at", "resetsAt"); ok && resetAt > 0 && resetAt <= math.MaxInt64 {
		out.resetAt = int64(resetAt)
		// Be liberal with producers that serialize Unix milliseconds.
		if out.resetAt >= 1_000_000_000_000 {
			out.resetAt /= 1000
		}
	}
	// window_minutes remains the existing safe fallback when an upstream frame
	// omits both reset forms. It is a duration upper bound, not an invented date.
	if !out.resetAfterPresent && out.resetAt == 0 && out.windowMinutes == 0 {
		return codexStreamQuotaWindow{}, false
	}
	out.present = true
	return out, true
}

func parseCodexStreamAdditionalLimits(raw interface{}) []codexStreamFeatureRateLimit {
	out := make([]codexStreamFeatureRateLimit, 0, maxCodexStreamAdditionalRateLimits)
	appendFeature := func(name string, rawLimit interface{}) {
		if len(out) >= maxCodexStreamAdditionalRateLimits {
			return
		}
		name = normalizeCodexStreamText(name, false)
		limit, _ := rawLimit.(map[string]interface{})
		if name == "" || limit == nil {
			return
		}
		if nested := firstCodexStreamMap(limit, "rate_limit", "rateLimit"); nested != nil {
			limit = nested
		}
		feature := codexStreamFeatureRateLimit{name: name}
		feature.primary, _ = parseCodexStreamWindow(firstCodexStreamMap(limit, "primary", "primary_window", "primaryWindow"))
		feature.secondary, _ = parseCodexStreamWindow(firstCodexStreamMap(limit, "secondary", "secondary_window", "secondaryWindow"))
		if feature.any() {
			out = append(out, feature)
		}
	}
	switch value := raw.(type) {
	case map[string]interface{}:
		// Stable ordering makes diagnostics and regression fixtures deterministic.
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			appendFeature(key, value[key])
			if len(out) >= maxCodexStreamAdditionalRateLimits {
				break
			}
		}
	case []interface{}:
		for _, entry := range value {
			item, _ := entry.(map[string]interface{})
			if item == nil {
				continue
			}
			name := firstCodexStreamText(item, false, "limit_name", "limitName", "name", "metered_feature", "meteredFeature")
			appendFeature(name, item)
			if len(out) >= maxCodexStreamAdditionalRateLimits {
				break
			}
		}
	}
	return out
}

func firstCodexStreamMap(object map[string]interface{}, keys ...string) map[string]interface{} {
	for _, key := range keys {
		if value, ok := object[key].(map[string]interface{}); ok {
			return value
		}
	}
	return nil
}

func firstCodexStreamValue(object map[string]interface{}, keys ...string) interface{} {
	for _, key := range keys {
		if value, ok := object[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func firstCodexStreamText(object map[string]interface{}, identifier bool, keys ...string) string {
	for _, key := range keys {
		value := normalizeCodexStreamText(streamString(object[key]), identifier)
		if value != "" {
			return value
		}
	}
	return ""
}

func normalizeCodexStreamText(value string, identifier bool) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return ""
		}
		if identifier && !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.') {
			return ""
		}
	}
	return value
}

func firstCodexStreamFloat(object map[string]interface{}, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := jsonFloatValue(object[key]); ok {
			return value, true
		}
	}
	return 0, false
}

func jsonFloatValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		end := 0
		if s[end] == '-' || s[end] == '+' {
			end++
		}
		seenDot := false
		seenDigit := false
		for end < len(s) {
			char := s[end]
			if char >= '0' && char <= '9' {
				seenDigit = true
				end++
				continue
			}
			if char == '.' && !seenDot {
				seenDot = true
				end++
				continue
			}
			break
		}
		if !seenDigit {
			return 0, false
		}
		f, err := strconv.ParseFloat(s[:end], 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	return 0, false
}

func codexStreamLimiterType(window codexStreamQuotaWindow, positionalFallback string) string {
	if window.windowMinutes > 0 {
		// The two Codex plan windows are currently five hours and seven days. A
		// day is a stable midpoint that also handles small upstream duration drift.
		if window.windowMinutes >= 24*60 {
			return "7d_polled"
		}
		return "5h_polled"
	}
	return positionalFallback
}

func codexStreamWindowJSON(window codexStreamQuotaWindow) map[string]interface{} {
	out := map[string]interface{}{"used_percent": window.usedPct}
	if window.windowMinutes > 0 {
		out["window_minutes"] = window.windowMinutes
	}
	if window.resetAfterPresent {
		out["reset_after_seconds"] = window.resetAfterSeconds
	}
	if window.resetAt > 0 {
		out["reset_at"] = window.resetAt
	}
	return out
}

func (r codexStreamRateLimits) rawObservation() string {
	root := map[string]interface{}{}
	if r.planType != "" {
		root["plan_type"] = r.planType
	}
	if r.activeLimit != "" {
		root["metered_limit_name"] = r.activeLimit
	}
	base := map[string]interface{}{}
	if r.primary.present {
		base["primary"] = codexStreamWindowJSON(r.primary)
	}
	if r.secondary.present {
		base["secondary"] = codexStreamWindowJSON(r.secondary)
	}
	if len(base) > 0 {
		root["rate_limits"] = base
	}
	if len(r.additional) > 0 {
		additional := make([]map[string]interface{}, 0, len(r.additional))
		for _, feature := range r.additional {
			entry := map[string]interface{}{"limit_name": feature.name}
			if feature.primary.present {
				entry["primary"] = codexStreamWindowJSON(feature.primary)
			}
			if feature.secondary.present {
				entry["secondary"] = codexStreamWindowJSON(feature.secondary)
			}
			additional = append(additional, entry)
		}
		root["additional_rate_limits"] = additional
	}
	if r.codeReview != nil && r.codeReview.any() {
		review := map[string]interface{}{}
		if r.codeReview.primary.present {
			review["primary"] = codexStreamWindowJSON(r.codeReview.primary)
		}
		if r.codeReview.secondary.present {
			review["secondary"] = codexStreamWindowJSON(r.codeReview.secondary)
		}
		root["code_review_rate_limits"] = review
	}
	raw, _ := json.Marshal(root)
	return string(raw)
}

func codexStreamBaseSnapshots(accountID string, limits codexStreamRateLimits, now int64) []storage.AccountRateLimit {
	raw := limits.rawObservation()
	byLimiter := map[string]storage.AccountRateLimit{}
	add := func(window codexStreamQuotaWindow, fallback string) {
		if !window.present {
			return
		}
		limiter := codexStreamLimiterType(window, fallback)
		snapshot := storage.AccountRateLimit{
			AccountID: accountID, Provider: "codex", LimiterType: limiter, Source: "codex_stream",
			UsedPercent: window.usedPct, RemainingTokens: -1, LimitTokens: -1,
			LimitRequests: -1, RemainingRequests: -1, ResetAt: window.resolvedResetAt(now),
			Status: "observed", Raw: raw, UpdatedAt: now,
		}
		if previous, exists := byLimiter[limiter]; !exists || snapshot.UsedPercent > previous.UsedPercent {
			byLimiter[limiter] = snapshot
		}
	}
	add(limits.primary, "5h_polled")
	add(limits.secondary, "7d_polled")
	out := make([]storage.AccountRateLimit, 0, len(byLimiter))
	for _, limiter := range []string{"5h_polled", "7d_polled"} {
		if snapshot, ok := byLimiter[limiter]; ok {
			out = append(out, snapshot)
		}
	}
	return out
}

// captureCodexStreamRateLimits performs all SQLite work on the existing async
// writer. The selected account is passed in so a matching JWT/import plan does
// not cause a redundant write on every completed CLI request. A different,
// validated live plan is persisted because it is the same authoritative source
// already used by /wham/usage; no accounts/check or billing inference is added.
//
// The "matching" test is by normalized tier, not raw text. The stored plan and the
// stream header are written by different producers with different vocabularies for
// the same entitlement (`KIRO PRO` / `Pro Plus` / `pro`), so a raw comparison
// reports a change every time and the two sources overwrite each other on every
// completed request — write amplification on precisely the path whose latency is
// the complaint. Tier equality collapses those spellings while still letting a real
// tier change, or the first plan ever seen for an account, through.
func (s *Server) captureCodexStreamRateLimits(account storage.Account, limits codexStreamRateLimits) {
	if strings.TrimSpace(account.ID) == "" || !strings.EqualFold(strings.TrimSpace(account.Provider), "codex") || !limits.any() {
		return
	}
	now := storage.Now()
	snapshots := codexStreamBaseSnapshots(account.ID, limits, now)
	if limits.featureObservationsPresent() {
		snapshots = append(snapshots, storage.AccountRateLimit{
			AccountID: account.ID, Provider: "codex", LimiterType: codexStreamFeaturesLimiterType, Source: "codex_stream",
			UsedPercent: -1, RemainingTokens: -1, LimitTokens: -1, LimitRequests: -1, RemainingRequests: -1,
			Status: "observed", Raw: limits.rawObservation(), UpdatedAt: now,
		})
	}
	planChanged := limits.planType != "" && !plantier.SameTier(account.PlanType, limits.planType)
	if len(snapshots) == 0 && !planChanged {
		return
	}
	for _, snapshot := range snapshots {
		if snapshot.LimiterType == codexStreamFeaturesLimiterType {
			continue
		}
		if s.scheduler != nil {
			s.scheduler.ApplyRateLimitSnapshot(snapshot)
		}
	}
	snapshots = append([]storage.AccountRateLimit(nil), snapshots...)
	s.enqueueWrite(func() {
		writeCtx, cancel := s.bgWriteContext()
		defer cancel()
		planUpdated := false
		if planChanged {
			if err := s.store.SetAccountPlanType(writeCtx, account.ID, limits.planType); err == nil {
				planUpdated = true
			}
		}
		for _, snapshot := range snapshots {
			_ = s.store.UpsertAccountRateLimit(writeCtx, snapshot)
		}
		if planUpdated && s.scheduler != nil {
			s.scheduler.RefreshAccountCache()
		}
	})
}
