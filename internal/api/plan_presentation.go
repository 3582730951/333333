package api

import (
	"strings"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/storage"
)

// accountPlanPresentation is an additive, product-facing projection of an
// account's raw plan and its current entitlement evidence. The raw plan stays
// in storage and evidence/audit records; main management surfaces consume this
// projection so an unreviewed string can never become a misleading seat claim.
type accountPlanPresentation struct {
	PlanFamily      string `json:"plan_family"`
	SeatType        string `json:"seat_type"`
	PlanDisplayName string `json:"plan_display_name"`
	SeatDisplayName string `json:"seat_display_name,omitempty"`
	Combined        string `json:"combined"`
}

func accountPlanPresentationFor(rawPlan string, evidence *storage.AccountEntitlementEvidence, conflict bool) accountPlanPresentation {
	normalized := entitlement.FromPlanLabel(rawPlan)
	presentation := accountPlanPresentation{
		PlanFamily:      string(normalized.PlanFamily),
		SeatType:        string(entitlement.SeatUnknown),
		PlanDisplayName: planFamilyDisplayName(normalized.PlanFamily),
		Combined:        planFamilyDisplayName(normalized.PlanFamily),
	}
	if presentation.PlanDisplayName == "" {
		presentation.PlanDisplayName, presentation.Combined = "Unknown", "Unknown"
	}
	if evidence == nil || conflict {
		return presentation
	}
	if evidence.PlanFamily != entitlement.PlanUnknown {
		presentation.PlanFamily = string(evidence.PlanFamily)
		presentation.PlanDisplayName = planFamilyDisplayName(evidence.PlanFamily)
		presentation.Combined = presentation.PlanDisplayName
	}
	presentation.SeatType = string(evidence.SeatType)
	if hasReviewedBusinessPremiumEvidence(evidence) {
		return accountPlanPresentation{
			PlanFamily:      string(entitlement.PlanBusiness),
			SeatType:        string(entitlement.SeatBusinessPremium),
			PlanDisplayName: "Team",
			SeatDisplayName: "Premium (5×)",
			Combined:        "Team (5×)",
		}
	}
	if evidence.SeatType == entitlement.SeatBusinessStandard && evidence.Confidence == "high" {
		presentation.PlanDisplayName = "Team"
		presentation.SeatDisplayName = "Standard"
		presentation.Combined = "Team"
	}
	return presentation
}

func hasReviewedBusinessPremiumEvidence(evidence *storage.AccountEntitlementEvidence) bool {
	return evidence != nil && entitlement.IsReviewedBusinessPremiumEvidence(
		evidence.RawPlanLabel,
		evidence.SourceKind,
		evidence.SeatType,
		evidence.Confidence,
		evidence.UsageMultiplierMilli,
		evidence.NoFiveHourLimit,
	)
}

func planFamilyDisplayName(family entitlement.PlanFamily) string {
	switch family {
	case entitlement.PlanBusiness:
		return "Business / Team"
	case entitlement.PlanEnterprise:
		return "Enterprise"
	case entitlement.PlanPro:
		return "Pro"
	case entitlement.PlanPlus:
		return "Plus"
	case entitlement.PlanFree:
		return "Free"
	case entitlement.PlanEdu:
		return "Education"
	case entitlement.PlanAPI:
		return "API"
	default:
		return "Unknown"
	}
}

func displayPlanOrUnknown(p accountPlanPresentation) string {
	if value := strings.TrimSpace(p.Combined); value != "" {
		return value
	}
	return "Unknown"
}
