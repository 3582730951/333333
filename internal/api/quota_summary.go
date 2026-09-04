package api

import (
	"encoding/json"
	"strings"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/cursorproxy"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

const quotaFreshSeconds = 15 * 60

type QuotaWindow struct {
	Source             string  `json:"source"`
	LimiterType        string  `json:"limiter_type"`
	UsedPercent        float64 `json:"used_percent"`
	RemainingTokens    int64   `json:"remaining_tokens"`
	LimitTokens        int64   `json:"limit_tokens"`
	LimitRequests      int64   `json:"limit_requests"`
	RemainingRequests  int64   `json:"remaining_requests"`
	LimitWindowSeconds int64   `json:"limit_window_seconds,omitempty"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds,omitempty"`
	ResetAt            int64   `json:"reset_at"`
	Status             string  `json:"status,omitempty"`
	UpdatedAt          int64   `json:"updated_at"`
	State              string  `json:"state"`
}

type QuotaSummary struct {
	AccountID    string             `json:"account_id"`
	Provider     string             `json:"provider"`
	Primary      *QuotaWindow       `json:"primary,omitempty"`
	Secondary    *QuotaWindow       `json:"secondary,omitempty"`
	Credits      *QuotaCredits      `json:"credits,omitempty"`
	ResetCredits *QuotaResetCredits `json:"reset_credits,omitempty"`
	Estimate     *QuotaEstimate     `json:"estimate,omitempty"`

	SyncReason        string `json:"sync_reason"`
	SyncedAt          int64  `json:"synced_at,omitempty"`
	Stale             bool   `json:"stale,omitempty"`
	Partial           bool   `json:"partial,omitempty"`
	Supported         bool   `json:"supported"`
	LastSuccessAt     int64  `json:"last_success_at,omitempty"`
	EvidenceFreshness string `json:"evidence_freshness,omitempty"`
}

// QuotaCredits is the account's extra paid balance beyond the plan's included
// windows, plus any workspace spend control applied to it. Upstream formats
// balance/limit/used as display strings (and may hide the balance entirely while
// still reporting that credits exist), so those are passed through verbatim and the
// numeric percentages carry -1 for "not signalled".
type QuotaCredits struct {
	HasCredits          bool    `json:"has_credits"`
	Unlimited           bool    `json:"unlimited"`
	SpendControl        bool    `json:"spend_control,omitempty"`
	Balance             string  `json:"balance,omitempty"`
	SpendControlReached bool    `json:"spend_control_reached,omitempty"`
	Source              string  `json:"source,omitempty"`
	Limit               string  `json:"limit,omitempty"`
	Used                string  `json:"used,omitempty"`
	Remaining           string  `json:"remaining,omitempty"`
	UsedPercent         float64 `json:"used_percent"`
	RemainingPercent    float64 `json:"remaining_percent"`
	ResetAt             int64   `json:"reset_at,omitempty"`
	Status              string  `json:"status,omitempty"`
	UpdatedAt           int64   `json:"updated_at"`
}

type QuotaResetCredits struct {
	Status         string `json:"status"`
	AvailableCount int64  `json:"available_count"`
	Source         string `json:"source"`
	UpdatedAt      int64  `json:"updated_at"`
}

func BuildQuotaSummary(account storage.Account, token *storage.AccountToken, snapshots []storage.AccountRateLimit, now int64) QuotaSummary {
	provider := quotaProvider(account, token)
	out := QuotaSummary{
		AccountID:  account.ID,
		Provider:   provider,
		SyncReason: "ok",
		Supported:  true,
	}
	primary := selectQuotaPrimary(provider, snapshots)
	secondary := selectQuotaSecondary(primary, snapshots, now)
	if primary != nil {
		w := quotaWindowFromSnapshot(*primary, now)
		out.Primary = &w
		out.SyncedAt = primary.UpdatedAt
	}
	if secondary != nil {
		out.Secondary = secondary
	}
	for _, snap := range snapshots {
		if isQuotaErrorMarker(snap) || strings.TrimSpace(snap.LimiterType) == codexStreamFeaturesLimiterType {
			continue
		}
		if snap.UpdatedAt > out.LastSuccessAt {
			out.LastSuccessAt = snap.UpdatedAt
		}
	}
	if reset := selectQuotaResetCredits(snapshots); reset != nil {
		out.ResetCredits = reset
	}
	if credits := selectQuotaCredits(snapshots); credits != nil {
		out.Credits = credits
	}

	if !quotaAccountActive(account, now) {
		out.SyncReason = "inactive"
		return out
	}
	if marker := newerQuotaErrorMarker(snapshots, primary); marker != nil {
		out.SyncReason = quotaErrorReason(*marker)
		return out
	}
	if token == nil {
		out.SyncReason = "token_missing"
		return out
	}
	if token.ExpiresAt > 0 && token.ExpiresAt <= now {
		out.SyncReason = "token_expired"
		return out
	}
	switch provider {
	case "codex", "chatgpt", "openai":
		if accountprovider.UsesAPIKey("codex", *token) {
			out.SyncReason = "unsupported_api_key_billing"
			out.Supported = false
			return out
		}
	case "claude":
		if !claudeTokenCanRefresh(*token) {
			out.SyncReason = "unsupported_claude_non_oauth"
			out.Supported = false
			return out
		}
	case "kiro":
	case "cursor":
		if !strings.EqualFold(strings.TrimSpace(token.CredentialMode), cursorproxy.CredentialBrowser) {
			out.SyncReason = "unsupported_cursor_api_key_billing"
			out.Supported = false
			return out
		}
	default:
		out.SyncReason = "unsupported_provider"
		out.Supported = false
		return out
	}
	// A never-polled account has no window data at all; estimating from nothing
	// would read as a full-price balance, so skip the estimate until the first
	// successful poll lands.
	if primary == nil {
		out.SyncReason = "never_polled"
		return out
	}
	out.Estimate = estimateQuota(account, primary, out.Credits, now)
	if primary.UpdatedAt > 0 && now-primary.UpdatedAt > quotaFreshSeconds {
		out.SyncReason = "stale"
		out.Stale = true
		return out
	}
	if primary.UsedPercent < 0 || strings.TrimSpace(primary.Source) == "" {
		out.SyncReason = "partial"
		out.Partial = true
		return out
	}
	if _, cooling := storage.AccountRateLimitEffectiveCooldownUntil(*primary, now); cooling {
		out.SyncReason = "quota_cooldown"
		return out
	}
	out.SyncReason = "ok"
	return out
}

func quotaProvider(account storage.Account, token *storage.AccountToken) string {
	provider := strings.TrimSpace(account.Provider)
	if provider != "" {
		return provider
	}
	if token != nil {
		return scheduler.ProviderFromToken(*token)
	}
	return ""
}

func quotaAccountActive(account storage.Account, now int64) bool {
	if strings.TrimSpace(account.Status) != "active" {
		return false
	}
	return account.QuarantineUntil <= now
}

func selectQuotaPrimary(provider string, snapshots []storage.AccountRateLimit) *storage.AccountRateLimit {
	provider = strings.TrimSpace(provider)
	preferred := []string{"5h_polled", "tokens", "unified"}
	if provider == "claude" {
		preferred = []string{"5h_oauth_usage", "unified"}
	} else if provider == "kiro" {
		preferred = []string{"kiro_usage", "unified"}
	} else if provider == "cursor" {
		preferred = []string{"cursor_monthly", "unified"}
	}
	for _, limiter := range preferred {
		if snap := latestQuotaPrimarySnapshotWithLimiter(provider, snapshots, limiter); snap != nil {
			return snap
		}
	}
	var latest *storage.AccountRateLimit
	for i := range snapshots {
		snap := &snapshots[i]
		if !quotaPrimarySnapshotEligible(provider, *snap, false) {
			continue
		}
		if latest == nil || snap.UpdatedAt > latest.UpdatedAt {
			latest = snap
		}
	}
	if latest != nil {
		return latest
	}

	// Premium/5x Business accounts can expose their sole primary quota as a
	// seven-day window. Keep it behind every ordinary primary candidate so a
	// normal five-hour window remains primary and seven-day stays secondary.
	for _, limiter := range quotaSevenDayPrimaryLimiters(provider) {
		if snap := latestQuotaPrimarySnapshotWithLimiter(provider, snapshots, limiter); snap != nil {
			return snap
		}
	}
	return nil
}

func quotaSevenDayPrimaryLimiters(provider string) []string {
	if provider == "claude" {
		return []string{"7d_oauth_usage", "7d_polled"}
	}
	return []string{"7d_polled", "7d_oauth_usage"}
}

func latestQuotaPrimarySnapshotWithLimiter(provider string, snapshots []storage.AccountRateLimit, limiter string) *storage.AccountRateLimit {
	var latest *storage.AccountRateLimit
	allowSevenDay := limiter == "7d_polled" || limiter == "7d_oauth_usage"
	for i := range snapshots {
		snap := &snapshots[i]
		if !snapshotHasLimiter(*snap, limiter) || !quotaPrimarySnapshotEligible(provider, *snap, allowSevenDay) {
			continue
		}
		if latest == nil || snap.UpdatedAt > latest.UpdatedAt {
			latest = snap
		}
	}
	return latest
}

func quotaPrimarySnapshotEligible(provider string, snap storage.AccountRateLimit, allowSevenDay bool) bool {
	// codexStreamFeaturesLimiterType is a passive observation row (used_percent -1,
	// status "observed") recording per-feature limit names seen on the wire. It
	// carries no routable window, so it must never become the primary quota.
	if snapshotHasLimiter(snap, codexResetCreditsLimiterType) ||
		snapshotHasLimiter(snap, codexCreditsLimiterType) ||
		snapshotHasLimiter(snap, codexStreamFeaturesLimiterType) ||
		snapshotHasLimiter(snap, "quota_poll_error") ||
		strings.HasPrefix(strings.TrimSpace(snap.Status), "error/") {
		return false
	}
	if !allowSevenDay && (snapshotHasLimiter(snap, "7d_oauth_usage") || snapshotHasLimiter(snap, "7d_polled")) {
		return false
	}
	if provider == "claude" && (snapshotHasLimiter(snap, "opus") || snapshotHasLimiter(snap, "sonnet") || snapshotHasLimiter(snap, "haiku")) {
		return false
	}
	return true
}

func snapshotLimiter(snap storage.AccountRateLimit) string {
	return firstNonEmpty(strings.TrimSpace(snap.LimiterType), strings.TrimSpace(snap.Source))
}

func snapshotHasLimiter(snap storage.AccountRateLimit, limiter string) bool {
	return strings.TrimSpace(snap.LimiterType) == limiter || strings.TrimSpace(snap.Source) == limiter
}

func latestSnapshotWithLimiter(snapshots []storage.AccountRateLimit, limiter string) *storage.AccountRateLimit {
	var latest *storage.AccountRateLimit
	for i := range snapshots {
		snap := &snapshots[i]
		if strings.TrimSpace(snap.LimiterType) != limiter && strings.TrimSpace(snap.Source) != limiter {
			continue
		}
		if latest == nil || snap.UpdatedAt > latest.UpdatedAt {
			latest = snap
		}
	}
	return latest
}

func selectQuotaSecondary(primary *storage.AccountRateLimit, snapshots []storage.AccountRateLimit, now int64) *QuotaWindow {
	if isSevenDayQuotaSnapshot(primary) {
		return nil
	}
	if snap := latestSnapshotWithLimiter(snapshots, "7d_polled"); snap != nil && !sameQuotaSnapshot(primary, snap) {
		w := quotaWindowFromSnapshot(*snap, now)
		return &w
	}
	if primary != nil {
		if secondary := secondaryFromRaw(primary.Raw, primary.UpdatedAt, now); secondary != nil {
			return secondary
		}
	}
	if snap := latestSnapshotWithLimiter(snapshots, "7d_oauth_usage"); snap != nil && !sameQuotaSnapshot(primary, snap) {
		w := quotaWindowFromSnapshot(*snap, now)
		return &w
	}
	return nil
}

func isSevenDayQuotaSnapshot(snap *storage.AccountRateLimit) bool {
	return snap != nil && (snapshotHasLimiter(*snap, "7d_polled") || snapshotHasLimiter(*snap, "7d_oauth_usage"))
}

func sameQuotaSnapshot(left, right *storage.AccountRateLimit) bool {
	return left != nil && right != nil &&
		strings.TrimSpace(left.LimiterType) == strings.TrimSpace(right.LimiterType) &&
		strings.TrimSpace(left.Source) == strings.TrimSpace(right.Source) &&
		left.UpdatedAt == right.UpdatedAt
}

// selectQuotaCredits rebuilds the extra-balance block from the snapshot the quota
// poller stored. The row's Raw payload is the source of truth for the display
// strings; UsedPercent on the row itself mirrors the spend-control percentage so
// callers that only read the flat column still see something meaningful.
func selectQuotaCredits(snapshots []storage.AccountRateLimit) *QuotaCredits {
	snap := latestSnapshotWithLimiter(snapshots, codexCreditsLimiterType)
	if snap == nil {
		return nil
	}
	detail := map[string]interface{}{}
	if strings.TrimSpace(snap.Raw) != "" {
		_ = json.Unmarshal([]byte(snap.Raw), &detail)
	}
	numeric := func(key string) float64 {
		if value, ok := detail[key].(float64); ok {
			return value
		}
		return -1
	}
	text := func(key string) string {
		if value, ok := detail[key].(string); ok {
			return strings.TrimSpace(value)
		}
		return ""
	}
	boolean := func(key string) bool {
		value, _ := detail[key].(bool)
		return value
	}
	usedPercent := numeric("used_percent")
	if usedPercent < 0 && snap.UsedPercent >= 0 {
		usedPercent = snap.UsedPercent
	}
	return &QuotaCredits{
		HasCredits:          boolean("has_credits"),
		Unlimited:           boolean("unlimited"),
		SpendControl:        boolean("spend_control"),
		Balance:             text("balance"),
		SpendControlReached: boolean("spend_control_reached"),
		Source:              text("source"),
		Limit:               text("limit"),
		Used:                text("used"),
		Remaining:           text("remaining"),
		UsedPercent:         usedPercent,
		RemainingPercent:    numeric("remaining_percent"),
		ResetAt:             snap.ResetAt,
		Status:              strings.TrimSpace(snap.Status),
		UpdatedAt:           snap.UpdatedAt,
	}
}

func selectQuotaResetCredits(snapshots []storage.AccountRateLimit) *QuotaResetCredits {
	snap := latestSnapshotWithLimiter(snapshots, codexResetCreditsLimiterType)
	if snap == nil {
		return nil
	}
	count := int64(0)
	if snap.RemainingRequests >= 0 {
		count = snap.RemainingRequests
	}
	return &QuotaResetCredits{
		Status:         firstNonEmpty(strings.TrimSpace(snap.Status), "unknown"),
		AvailableCount: count,
		Source:         firstNonEmpty(strings.TrimSpace(snap.Source), codexResetCreditsLimiterType),
		UpdatedAt:      snap.UpdatedAt,
	}
}

func secondaryFromRaw(raw string, updatedAt, now int64) *QuotaWindow {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var detail struct {
		Secondary struct {
			Source             string  `json:"source"`
			LimiterType        string  `json:"limiter_type"`
			UsedPercent        float64 `json:"used_percent"`
			RemainingTokens    int64   `json:"remaining_tokens"`
			LimitTokens        int64   `json:"limit_tokens"`
			LimitWindowSeconds int64   `json:"limit_window_seconds"`
			ResetAfterSeconds  int64   `json:"reset_after_seconds"`
			ResetAt            int64   `json:"reset_at"`
			Status             string  `json:"status"`
		} `json:"secondary"`
	}
	if json.Unmarshal([]byte(raw), &detail) != nil || detail.Secondary.LimitWindowSeconds <= 0 {
		return nil
	}
	resetAt := detail.Secondary.ResetAt
	if resetAt == 0 && detail.Secondary.ResetAfterSeconds > 0 {
		resetAt = now + detail.Secondary.ResetAfterSeconds
	}
	source := strings.TrimSpace(detail.Secondary.Source)
	if source == "" {
		source = "raw.secondary"
	}
	limiter := strings.TrimSpace(detail.Secondary.LimiterType)
	if limiter == "" {
		limiter = "7d"
	}
	return &QuotaWindow{
		Source:             source,
		LimiterType:        limiter,
		UsedPercent:        detail.Secondary.UsedPercent,
		RemainingTokens:    detail.Secondary.RemainingTokens,
		LimitTokens:        detail.Secondary.LimitTokens,
		LimitRequests:      -1,
		RemainingRequests:  -1,
		LimitWindowSeconds: detail.Secondary.LimitWindowSeconds,
		ResetAfterSeconds:  detail.Secondary.ResetAfterSeconds,
		ResetAt:            resetAt,
		Status:             detail.Secondary.Status,
		UpdatedAt:          updatedAt,
		State: func() string {
			if updatedAt <= 0 || detail.Secondary.UsedPercent < 0 {
				return "unknown"
			}
			return "known"
		}(),
	}
}

func quotaWindowFromSnapshot(snap storage.AccountRateLimit, nowValues ...int64) QuotaWindow {
	now := storage.Now()
	if len(nowValues) > 0 {
		now = nowValues[0]
	}
	return QuotaWindow{
		Source:             firstNonEmpty(strings.TrimSpace(snap.Source), strings.TrimSpace(snap.LimiterType)),
		LimiterType:        firstNonEmpty(strings.TrimSpace(snap.LimiterType), strings.TrimSpace(snap.Source)),
		UsedPercent:        snap.UsedPercent,
		RemainingTokens:    snap.RemainingTokens,
		LimitTokens:        snap.LimitTokens,
		LimitRequests:      snap.LimitRequests,
		RemainingRequests:  snap.RemainingRequests,
		LimitWindowSeconds: quotaWindowSecondsFromSnapshot(snap),
		ResetAt:            snap.ResetAt,
		Status:             snap.Status,
		UpdatedAt:          snap.UpdatedAt,
		State:              quotaWindowSnapshotState(snap, now),
	}
}

func quotaWindowSecondsFromSnapshot(snap storage.AccountRateLimit) int64 {
	limiter := snapshotLimiter(snap)
	if seconds := quotaWindowSecondsFromRaw(snap.Raw, limiter); seconds > 0 {
		return seconds
	}
	return quotaWindowSecondsFromLimiter(limiter)
}

func quotaWindowSecondsFromRaw(raw, limiter string) int64 {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	var detail struct {
		LimitWindowSeconds int64 `json:"limit_window_seconds"`
		Primary            struct {
			LimitWindowSeconds int64 `json:"limit_window_seconds"`
		} `json:"primary"`
		Secondary struct {
			LimitWindowSeconds int64 `json:"limit_window_seconds"`
		} `json:"secondary"`
	}
	if json.Unmarshal([]byte(raw), &detail) != nil {
		return 0
	}

	limiter = strings.ToLower(strings.TrimSpace(limiter))
	var candidates []int64
	switch {
	case strings.Contains(limiter, "5h"):
		candidates = []int64{detail.Primary.LimitWindowSeconds, detail.LimitWindowSeconds, detail.Secondary.LimitWindowSeconds}
	case strings.Contains(limiter, "7d"):
		candidates = []int64{detail.Secondary.LimitWindowSeconds, detail.LimitWindowSeconds, detail.Primary.LimitWindowSeconds}
	default:
		// Without a limiter we can only trust an unambiguous top-level value.
		candidates = []int64{detail.LimitWindowSeconds}
	}
	for _, seconds := range candidates {
		if seconds > 0 && quotaWindowSecondsMatchLimiter(seconds, limiter) {
			return seconds
		}
	}
	return 0
}

func quotaWindowSecondsMatchLimiter(seconds int64, limiter string) bool {
	switch {
	case strings.Contains(limiter, "5h"):
		return seconds < 24*60*60
	case strings.Contains(limiter, "7d"):
		return seconds >= 24*60*60
	default:
		return true
	}
}

func quotaWindowSecondsFromLimiter(limiter string) int64 {
	limiter = strings.ToLower(strings.TrimSpace(limiter))
	switch {
	case strings.Contains(limiter, "5h"):
		return 5 * 60 * 60
	case strings.Contains(limiter, "7d"):
		return 7 * 24 * 60 * 60
	default:
		return 0
	}
}

func quotaWindowSnapshotState(snap storage.AccountRateLimit, now int64) string {
	if snap.UpdatedAt <= 0 || snap.UsedPercent < 0 {
		return "unknown"
	}
	if _, cooling := storage.AccountRateLimitEffectiveCooldownUntil(snap, now); cooling {
		return "cooling"
	}
	return "known"
}

func newerQuotaErrorMarker(snapshots []storage.AccountRateLimit, primary *storage.AccountRateLimit) *storage.AccountRateLimit {
	var marker *storage.AccountRateLimit
	primaryUpdatedAt := int64(0)
	if primary != nil {
		primaryUpdatedAt = primary.UpdatedAt
	}
	for i := range snapshots {
		snap := &snapshots[i]
		if !isQuotaErrorMarker(*snap) || snap.UpdatedAt <= primaryUpdatedAt {
			continue
		}
		if marker == nil || snap.UpdatedAt > marker.UpdatedAt {
			marker = snap
		}
	}
	return marker
}

func isQuotaErrorMarker(snap storage.AccountRateLimit) bool {
	return strings.TrimSpace(snap.LimiterType) == "quota_poll_error" ||
		strings.TrimSpace(snap.Source) == "quota_poll_error" ||
		strings.HasPrefix(strings.TrimSpace(snap.Status), "error/")
}

func quotaErrorReason(marker storage.AccountRateLimit) string {
	status := strings.TrimSpace(marker.Status)
	if strings.HasPrefix(status, "error/") {
		return status
	}
	return "error/poll_failed"
}
