// Package entitlement keeps ChatGPT plan family, seat type, and measured
// capacity as separate dimensions. In particular, a Business/Team plan string
// never implies a Premium 5x seat.
package entitlement

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type PlanFamily string
type SeatType string

const (
	PlanUnknown    PlanFamily = "unknown"
	PlanFree       PlanFamily = "free"
	PlanPlus       PlanFamily = "plus"
	PlanPro        PlanFamily = "pro"
	PlanBusiness   PlanFamily = "business"
	PlanEnterprise PlanFamily = "enterprise"
	PlanEdu        PlanFamily = "edu"
	PlanAPI        PlanFamily = "api"

	SeatUnknown            SeatType = "unknown"
	SeatPersonal           SeatType = "personal"
	SeatBusinessStandard   SeatType = "business_standard"
	SeatBusinessPremium    SeatType = "business_premium"
	SeatLegacyCodex        SeatType = "legacy_codex"
	SeatEnterpriseStandard SeatType = "enterprise_standard"
)

type Normalized struct {
	PlanFamily           PlanFamily `json:"plan_family"`
	SeatType             SeatType   `json:"seat_type"`
	UsageMultiplierMilli *int64     `json:"usage_multiplier_milli"`
	NoFiveHourLimit      *bool      `json:"no_five_hour_limit"`
	Confidence           string     `json:"confidence"`
	Reason               string     `json:"reason"`
}

func tokenized(raw string) []string {
	return strings.FieldsFunc(strings.ToLower(strings.TrimSpace(raw)), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

func NormalizePlanFamily(raw string) PlanFamily {
	best := PlanUnknown
	for _, token := range tokenized(raw) {
		switch token {
		case "api", "apikey", "payg", "paygo":
			best = PlanAPI
		case "free":
			if best == PlanUnknown {
				best = PlanFree
			}
		case "plus":
			if best == PlanUnknown || best == PlanFree {
				best = PlanPlus
			}
		case "pro":
			if best != PlanBusiness && best != PlanEnterprise && best != PlanEdu {
				best = PlanPro
			}
		case "team", "teams", "business":
			best = PlanBusiness
		case "enterprise":
			best = PlanEnterprise
		case "edu", "education":
			best = PlanEdu
		}
	}
	return best
}

// FromPlanLabel deliberately returns an unknown seat. Email domains, the words
// team/business, dates, and measured capacity are not seat evidence.
func FromPlanLabel(raw string) Normalized {
	return Normalized{PlanFamily: NormalizePlanFamily(raw), SeatType: SeatUnknown, Confidence: "low", Reason: "plan_label_has_no_seat_evidence"}
}

type ReviewedSeatObservation struct {
	PlanLabel       string
	SeatType        SeatType
	SourceKind      string
	MappingReviewed bool
	SignatureValid  bool
}

// FromReviewedSeatObservation is the only constructor that emits Premium 5x
// flags. Callers must first match a versioned, human-reviewed wire mapping and,
// for JWT evidence, verify the signature. With no real Premium fixture/mapping,
// callers cannot satisfy these gates and safely remain unknown.
func FromReviewedSeatObservation(observation ReviewedSeatObservation) Normalized {
	result := FromPlanLabel(observation.PlanLabel)
	if !observation.MappingReviewed {
		result.Reason = "seat_mapping_unreviewed"
		return result
	}
	switch observation.SourceKind {
	case "workspace_entitlement", "billing_entitlement", "quota_metadata":
	case "verified_jwt":
		if !observation.SignatureValid {
			result.Reason = "jwt_signature_unverified"
			return result
		}
	default:
		result.Reason = "seat_source_not_authoritative"
		return result
	}
	result.SeatType = observation.SeatType
	result.Confidence = "high"
	result.Reason = "reviewed_authoritative_mapping"
	switch observation.SeatType {
	case SeatBusinessPremium:
		multiplier, noFiveHour := int64(5000), true
		result.PlanFamily = PlanBusiness
		result.UsageMultiplierMilli = &multiplier
		result.NoFiveHourLimit = &noFiveHour
	case SeatBusinessStandard:
		multiplier, noFiveHour := int64(1000), false
		result.PlanFamily = PlanBusiness
		result.UsageMultiplierMilli = &multiplier
		result.NoFiveHourLimit = &noFiveHour
	case SeatEnterpriseStandard:
		result.PlanFamily = PlanEnterprise
	}
	return result
}

// WireObservation is a deliberately small, credential-free projection of an
// upstream entitlement response.  It is useful for collectors that receive
// rolling wire schemas without making the collector itself a source of seat
// claims.
type WireObservation struct {
	PlanLabel       string
	SeatRaw         string
	MultiplierMilli *int64
	NoFiveHour      *bool
	SourceKind      string
	MappingReviewed bool
	SignatureValid  bool
	Payload         map[string]interface{}
}

// ParseWireObservation extracts only explicit entitlement fields.  It never
// treats plan_type=team/business, an email domain, or measured capacity as a
// Premium signal.  Callers still have to provide MappingReviewed (and JWT
// SignatureValid where applicable) before FromReviewedSeatObservation can emit
// a seat classification.
func ParseWireObservation(root map[string]interface{}, sourceKind string) WireObservation {
	out := WireObservation{SourceKind: strings.TrimSpace(sourceKind), Payload: map[string]interface{}{}}
	if len(root) == 0 {
		return out
	}
	out.PlanLabel = firstStringAt(root, "plan_type", "planType", "subscription_type", "subscriptionType", "plan")
	out.SeatRaw = firstStringAt(root, "seat_type", "seatType", "seat", "workspace_seat_type", "workspaceSeatType")
	if out.SeatRaw == "" {
		out.SeatRaw = nestedStringAt(root, []string{"entitlement", "seat_type"}, []string{"entitlement", "seatType"}, []string{"workspace_entitlement", "seat_type"}, []string{"workspaceEntitlement", "seatType"})
	}
	if value, ok := firstNumberAt(root, "usage_multiplier_milli", "usageMultiplierMilli"); ok {
		out.MultiplierMilli = &value
	} else if value, ok := firstNumberAt(root, "usage_multiplier", "usageMultiplier", "multiplier"); ok {
		// Wire APIs commonly express this as 5 or 5.0 rather than 5000.
		if value > 0 && value < 100 {
			value *= 1000
		}
		out.MultiplierMilli = &value
	}
	if value, ok := firstBoolAt(root, "no_five_hour_limit", "noFiveHourLimit", "has_no_five_hour_limit"); ok {
		out.NoFiveHour = &value
	}
	// Only copy scalar, non-secret evidence fields into the optional redacted
	// payload.  The storage validator applies a second forbidden-key check.
	for _, key := range []string{"plan_type", "planType", "subscription_type", "subscriptionType", "seat_type", "seatType", "usage_multiplier_milli", "usageMultiplierMilli", "usage_multiplier", "usageMultiplier", "multiplier", "no_five_hour_limit", "noFiveHourLimit", "has_no_five_hour_limit"} {
		if value, ok := root[key]; ok && scalarEvidenceValue(value) {
			out.Payload[key] = value
		}
	}
	return out
}

func scalarEvidenceValue(value interface{}) bool {
	switch value.(type) {
	case string, bool, float64, float32, int, int64, json.Number:
		return true
	default:
		return false
	}
}

func firstStringAt(root map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nestedStringAt(root map[string]interface{}, paths ...[]string) string {
	for _, path := range paths {
		current := interface{}(root)
		for _, key := range path {
			object, ok := current.(map[string]interface{})
			if !ok {
				current = nil
				break
			}
			current = object[key]
		}
		if value, ok := current.(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNumberAt(root map[string]interface{}, keys ...string) (int64, bool) {
	for _, key := range keys {
		if value, ok := root[key]; ok {
			if number, valid := nonNegativeNumber(value); valid {
				return number, true
			}
		}
	}
	return 0, false
}

func firstBoolAt(root map[string]interface{}, keys ...string) (bool, bool) {
	for _, key := range keys {
		if value, ok := root[key].(bool); ok {
			return value, true
		}
	}
	return false, false
}

func nonNegativeNumber(value interface{}) (int64, bool) {
	const maxInt64FloatExclusive = 9223372036854775808.0 // 1<<63
	var number float64
	switch typed := value.(type) {
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		if typed < 0 {
			return 0, false
		}
		return int64(typed), true
	case int8:
		if typed < 0 {
			return 0, false
		}
		return int64(typed), true
	case int16:
		if typed < 0 {
			return 0, false
		}
		return int64(typed), true
	case int32:
		if typed < 0 {
			return 0, false
		}
		return int64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return typed, true
	case uint:
		if uint64(typed) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		// Preserve integer wire values exactly (float64 cannot represent the
		// upper half of int64). Decimal forms are accepted only when they are
		// finite, non-negative, integral, and strictly below 2^63.
		if parsed, err := strconv.ParseInt(string(typed), 10, 64); err == nil && parsed >= 0 {
			return parsed, true
		}
		parsed, err := strconv.ParseFloat(string(typed), 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number != math.Trunc(number) || number >= maxInt64FloatExclusive {
		return 0, false
	}
	return int64(number), true
}

// NormalizedFromWire is the safe convenience path for collectors.  If the
// wire mapping is not reviewed it returns an explicit unknown seat, even when
// the payload contains words such as "premium".
func NormalizedFromWire(root map[string]interface{}, sourceKind string, mappingReviewed, signatureValid bool) (Normalized, WireObservation) {
	observation := ParseWireObservation(root, sourceKind)
	observation.MappingReviewed, observation.SignatureValid = mappingReviewed, signatureValid
	seat := SeatUnknown
	switch strings.ToLower(strings.TrimSpace(observation.SeatRaw)) {
	case "business_premium", "premium", "premium_5x", "5x", "5x_team", "business_5x":
		seat = SeatBusinessPremium
	case "business_standard", "standard", "team", "business":
		seat = SeatBusinessStandard
	case "legacy_codex", "codex_legacy":
		seat = SeatLegacyCodex
	case "personal", "plus", "pro":
		seat = SeatPersonal
	}
	result := FromReviewedSeatObservation(ReviewedSeatObservation{
		PlanLabel: observation.PlanLabel, SeatType: seat, SourceKind: observation.SourceKind,
		MappingReviewed: observation.MappingReviewed, SignatureValid: observation.SignatureValid,
	})
	if result.SeatType == SeatUnknown && observation.PlanLabel == "" {
		result.Reason = fmt.Sprintf("%s_no_plan_or_seat_evidence", firstNonEmpty(sourceKind, "wire"))
	}
	return result, observation
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
