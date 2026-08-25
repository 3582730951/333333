package api

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"codex-account-pool/internal/storage"
)

// quotaPlanPriceInfo maps a plan label to the plan's list monthly USD value so the
// pool can express an account's remaining subscription quota in dollars.
//
// Prices are approximate OpenAI/Anthropic/Cursor list prices and can drift; they are
// a baseline for estimation, not a billing source of truth. Neither sub2api nor
// cliproxyapi computes this for subscription accounts (sub2api reports utilization
// windows, cliproxyapi is purely reactive failover), so this is a gateway capability
// built on the existing AccountRateLimit windows.
type quotaPlanPriceInfo struct {
	matches []string // lowercased plan substrings, most-specific first
	usd     float64
	label   string
}

var quotaPlanPriceByFamily = map[string][]quotaPlanPriceInfo{
	"openai": {
		{matches: []string{"free"}, usd: 0, label: "Free"},
		{matches: []string{"plus"}, usd: 20, label: "ChatGPT Plus"},
		{matches: []string{"max", "ultra"}, usd: 200, label: "ChatGPT Max"},
		{matches: []string{"pro"}, usd: 200, label: "ChatGPT Pro"},
		{matches: []string{"team"}, usd: 30, label: "ChatGPT Team"},
		{matches: []string{"enterprise"}, usd: 0, label: "ChatGPT Enterprise"},
	},
	"claude": {
		{matches: []string{"free"}, usd: 0, label: "Claude Free"},
		{matches: []string{"max_5x", "max5x"}, usd: 100, label: "Claude Max 5x"},
		{matches: []string{"max_20x", "max20x"}, usd: 200, label: "Claude Max 20x"},
		{matches: []string{"max"}, usd: 100, label: "Claude Max"},
		{matches: []string{"pro"}, usd: 20, label: "Claude Pro"},
		{matches: []string{"team"}, usd: 25, label: "Claude Team"},
	},
	"cursor": {
		{matches: []string{"free", "hobby"}, usd: 0, label: "Cursor Free"},
		{matches: []string{"pro"}, usd: 20, label: "Cursor Pro"},
		{matches: []string{"business"}, usd: 40, label: "Cursor Business"},
	},
}

// QuotaEstimate expresses the account's remaining subscription quota in USD. It is an
// ESTIMATE: the plan's list price scaled by the current usage window (same window the
// quota summary surfaces), plus any reported pay-as-you-go credit balance. The
// Method field records the basis so callers can distinguish a plan-derived number
// from an unavailable one.
type QuotaEstimate struct {
	Estimated    bool    `json:"estimated"`
	Plan         string  `json:"plan,omitempty"`
	LimitUSD     float64 `json:"limit_usd,omitempty"`
	UsedUSD      float64 `json:"used_usd,omitempty"`
	RemainingUSD float64 `json:"remaining_usd,omitempty"`
	ExtraUSD     float64 `json:"extra_usd,omitempty"`
	UsedPercent  float64 `json:"used_percent"`
	Window       string  `json:"window,omitempty"`
	Currency     string  `json:"currency"`
	Method       string  `json:"method"`
	Note         string  `json:"note,omitempty"`
	UpdatedAt    int64   `json:"updated_at,omitempty"`
}

// estimateQuota derives a USD estimate from the account's plan and the primary usage
// window. It returns a non-nil estimate for any account that reaches supported
// billing, using Method to describe the basis; callers that gate earlier (inactive,
// unsupported provider, API-key billing, token missing) never see one.
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
	family := quotaPlanFamily(account.Provider)
	if family == "" {
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
	plan, ok := quotaPlanForFamily(family, planType)
	if !ok {
		est.Method = "unknown_plan"
		est.Note = fmt.Sprintf("plan %q not recognized for USD estimation", planType)
		return est
	}
	est.Plan = plan.label
	if plan.usd <= 0 {
		est.Estimated = true
		est.Method = "free_plan"
		return est
	}
	usedPct := est.UsedPercent
	if usedPct < 0 {
		// A window row exists but carries no percentage (e.g. only a reset time was
		// signalled). Estimating would fabricate a full-price balance; report the
		// basis instead.
		est.Method = "no_window_data"
		est.Note = "no usage window data for USD estimate"
		return est
	}
	if usedPct > 100 {
		usedPct = 100
	}
	used := plan.usd * usedPct / 100
	est.LimitUSD = round2(plan.usd)
	est.UsedUSD = round2(used)
	est.RemainingUSD = round2(plan.usd - used)
	est.Method = "plan_price_window"
	if credits != nil && credits.HasCredits {
		if extra := parseUSDString(credits.Balance); extra > 0 {
			est.ExtraUSD = extra
			est.RemainingUSD = round2(est.RemainingUSD + extra)
			est.Method = "plan_price_window_plus_credits"
		}
	}
	est.Estimated = true
	return est
}

// quotaPlanFamily maps the account's provider to the plan-price family used for USD
// estimation. Providers without a subscription price baseline return "".
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

func quotaPlanForFamily(family, planType string) (quotaPlanPriceInfo, bool) {
	plan := strings.ToLower(strings.TrimSpace(planType))
	if plan == "" {
		return quotaPlanPriceInfo{}, false
	}
	for _, p := range quotaPlanPriceByFamily[family] {
		for _, m := range p.matches {
			if strings.Contains(plan, m) {
				return p, true
			}
		}
	}
	return quotaPlanPriceInfo{}, false
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
