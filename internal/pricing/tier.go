package pricing

import "strings"

const (
	TierDefault = "default"
	TierFast    = "fast"
	TierUnknown = "unknown"
)

type TierDecision struct {
	Requested  string `json:"requested"`
	Forwarded  string `json:"forwarded"`
	Observed   string `json:"observed"`
	Billed     string `json:"billed"`
	Reason     string `json:"reason"`
	Settlement string `json:"settlement"`
}

func NormalizeServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default", "standard", "auto":
		return TierDefault
	case "fast", "priority":
		return TierFast
	default:
		return TierUnknown
	}
}

func ResolveServiceTier(requested, forwarded, observed string) TierDecision {
	decision := TierDecision{
		Requested: NormalizeServiceTier(requested),
		Forwarded: NormalizeServiceTier(forwarded),
		Observed:  "absent",
	}
	if strings.TrimSpace(forwarded) == "" {
		decision.Forwarded = decision.Requested
	}
	if strings.TrimSpace(observed) != "" {
		decision.Observed = NormalizeServiceTier(observed)
		if decision.Observed == TierUnknown {
			decision.Billed = TierUnknown
			decision.Reason = "unknown_observed_tier"
			decision.Settlement = "unsettled"
			return decision
		}
		decision.Billed = decision.Observed
		decision.Settlement = "final"
		switch {
		case decision.Forwarded == TierFast && decision.Observed == TierDefault:
			decision.Reason = "upstream_downgrade"
		case decision.Forwarded == TierDefault && decision.Observed == TierFast:
			decision.Reason = "upstream_upgrade"
		default:
			decision.Reason = "observed_authoritative"
		}
		return decision
	}

	decision.Billed = decision.Forwarded
	decision.Settlement = "provisional"
	if decision.Billed == TierUnknown {
		decision.Reason = "unknown_forwarded_tier"
		decision.Settlement = "unsettled"
		return decision
	}
	if decision.Billed == TierFast {
		decision.Reason = "conservative_requested"
	} else {
		decision.Reason = "observed_tier_absent"
	}
	return decision
}
