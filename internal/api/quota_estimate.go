package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// QuotaEstimate expresses the account's remaining subscription quota in USD. It
// follows the sub2api billing model: usage windows (5h/7d) are RATE LIMITS
// measured in requests/tokens, not dollar spend, so the estimate never converts a
// plan's list price into dollars. The only USD figures emitted are real dollar
// amounts the upstream itself reports — the pay-as-you-go credit balance. An
// account whose plan carries no reported balance gets Method "window_based" with
// no fabricated numbers; its truthful utilization lives in
// QuotaSummary.Primary/Secondary.UsedPercent (the console renders those as
// percentage windows, matching sub2api's UsageProgressBar).
type QuotaEstimate struct {
	Estimated    bool    `json:"estimated"`
	Plan         string  `json:"plan,omitempty"`
	LimitUSD     float64 `json:"limit_usd,omitempty"`
	LimitUSDMin  float64 `json:"limit_usd_min,omitempty"`
	LimitUSDMax  float64 `json:"limit_usd_max,omitempty"`
	UsedUSD      float64 `json:"used_usd,omitempty"`
	RemainingUSD float64 `json:"remaining_usd,omitempty"`
	ExtraUSD     float64 `json:"extra_usd,omitempty"`
	UsedPercent  float64 `json:"used_percent"`
	Window       string  `json:"window,omitempty"`
	Currency     string  `json:"currency"`
	Method       string  `json:"method"`
	Confidence   string  `json:"confidence,omitempty"`
	Note         string  `json:"note,omitempty"`
	UpdatedAt    int64   `json:"updated_at,omitempty"`
}

// estimateQuota derives a USD figure from the account's plan and the primary
// usage window. Only a real upstream-reported balance can produce dollars
// (Method "payg_credits_balance"); subscription windows are rate limits and are
// surfaced as percentages, never converted to a dollar figure. Callers that gate
// earlier (inactive, unsupported provider, API-key billing, token missing) never
// see one.
func estimateQuota(account storage.Account, primary *storage.AccountRateLimit, credits *QuotaCredits, now int64) *QuotaEstimate {
	planType := strings.TrimSpace(account.PlanType)
	est := &QuotaEstimate{
		Currency:    "USD",
		Method:      "not_supported",
		Note:        "provider has no USD plan baseline",
		UpdatedAt:   now,
		UsedPercent: -1,
	}
	if primary != nil {
		est.UsedPercent = primary.UsedPercent
		est.Window = strings.TrimSpace(primary.LimiterType)
		if est.Window == "" {
			est.Window = strings.TrimSpace(primary.Source)
		}
		est.UpdatedAt = primary.UpdatedAt
	}
	if quotaPlanFamily(account.Provider) == "" {
		return est
	}
	switch strings.ToLower(planType) {
	case "api", "payg", "pay_as_you_go", "pay-as-you-go":
		// Pay-as-you-go has no plan window, but when the upstream reports a
		// spendable credit balance it is the only real dollar figure available —
		// surface it as the remaining allowance instead of refusing to estimate.
		if credits != nil && credits.HasCredits {
			if extra := parseUSDString(credits.Balance); extra > 0 {
				est.Estimated = true
				est.Method = "payg_credits_balance"
				est.ExtraUSD = extra
				est.RemainingUSD = extra
				est.Note = "pay-as-you-go balance from upstream credits"
				return est
			}
		}
		est.Method = "pay_as_you_go"
		est.Note = "pay-as-you-go billing has no plan window; use upstream balance"
		return est
	}
	// Subscription plan. The 5h/7d windows constrain requests/tokens, not dollar
	// spend — multiplying a list price by a window's used_percent would fabricate
	// a figure upstream never reported (the sub2api model renders the windows
	// verbatim). The only legitimate dollar is a reported credit balance.
	if credits != nil && credits.HasCredits {
		if extra := parseUSDString(credits.Balance); extra > 0 {
			est.Estimated = true
			est.Method = "payg_credits_balance"
			est.Plan = planType
			est.ExtraUSD = extra
			est.RemainingUSD = extra
			est.Note = "upstream credit balance"
			return est
		}
	}
	est.Plan = planType
	est.Method = "window_based"
	est.Note = "plan usage is a rate-limit window (requests/tokens), not a USD balance; see primary.used_percent"
	if planType != "" {
		est.Note = fmt.Sprintf("%s usage is a rate-limit window (requests/tokens), not a USD balance; see primary.used_percent", planType)
	}
	return est
}

// quotaPlanFamily maps the account's provider to the subscription family used to
// decide whether the estimate can speak for it at all. Providers without a
// subscription model (or with no USD baseline) return "".
func quotaPlanFamily(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex", "chatgpt", "openai":
		return "openai"
	case "claude":
		return "claude"
	case "cursor":
		return "cursor"
	default:
		return ""
	}
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// parseUSDString leniently parses a balance display string ("$5.00", "5.00", "hidden")
// into a positive dollar figure; unparseable or non-positive values return 0.
func parseUSDString(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '.', r == '-', r == '+':
			b.WriteRune(r)
		}
	}
	v, err := strconv.ParseFloat(b.String(), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return round2(v)
}
