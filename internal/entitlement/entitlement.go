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

	// BusinessPremiumMappingVersion identifies the only built-in mapping that
	// may derive a Premium seat from Codex quota metadata. The versioned review
	// binds the operator-confirmed 5x subtype self_serve_business_prolite to the
	// fixture's single seven-day window with no five-hour window. Neither the
	// subtype nor the missing window is sufficient on its own.
	BusinessPremiumMappingVersion = "codex_quota_business_prolite_5x_no_5h_v1"
	// BusinessPremiumRawPlanLabel is the exact upstream plan label covered by
	// BusinessPremiumMappingVersion. It intentionally is not a generic Team or
	// Business classifier: presentation may call a seat Premium only when the
	// reviewed evidence contract below is complete.
	BusinessPremiumRawPlanLabel = "self_serve_business_prolite"
)

type Normalized struct {
	PlanFamily           PlanFamily `json:"plan_family"`
	SeatType             SeatType   `json:"seat_type"`
	UsageMultiplierMilli *int64     `json:"usage_multiplier_milli"`
	NoFiveHourLimit      *bool      `json:"no_five_hour_limit"`
	Confidence           string     `json:"confidence"`
	Reason               string     `json:"reason"`
	// FlagsState is a fail-closed three-state marker for capacity flags:
	// known (both flags observed), unknown (not signalled), or contradictory.
	FlagsState string `json:"flags_state"`
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
	return Normalized{PlanFamily: NormalizePlanFamily(raw), SeatType: SeatUnknown, Confidence: "low", Reason: "plan_label_has_no_seat_evidence", FlagsState: "unknown"}
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
// for JWT evidence, verify the signature. Generic or contradictory payloads
// cannot satisfy these gates and safely remain unknown.
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
	if observation.SeatType == SeatUnknown {
		result.Reason = "reviewed_mapping_has_no_seat"
		return result
	}
	result.Confidence = "high"
	result.Reason = "reviewed_authoritative_mapping"
	if observation.SeatType == SeatBusinessPremium || observation.SeatType == SeatBusinessStandard {
		result.FlagsState = "known"
	}
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

// IsReviewedBusinessPremiumEvidence is the shared, fail-closed predicate for
// the reviewed Team (5x) fixture. It does not infer a seat from a plan label;
// callers must supply a current, validated evidence row captured from the
// exact quota-metadata shape. Keeping this predicate here prevents API/UI
// surfaces from growing their own slightly different Premium classifiers.
func IsReviewedBusinessPremiumEvidence(planLabel, sourceKind string, seat SeatType, confidence string, multiplier *int64, noFiveHour *bool) bool {
	return strings.EqualFold(strings.TrimSpace(planLabel), BusinessPremiumRawPlanLabel) &&
		strings.TrimSpace(sourceKind) == "quota_metadata" &&
		seat == SeatBusinessPremium &&
		strings.TrimSpace(confidence) == "high" &&
		multiplier != nil && *multiplier == 5000 &&
		noFiveHour != nil && *noFiveHour
}

// WireObservation is a deliberately small, credential-free projection of an
// upstream entitlement response.  It is useful for collectors that receive
// rolling wire schemas without making the collector itself a source of seat
// claims.
type WireObservation struct {
	PlanLabel         string
	SeatRaw           string
	MultiplierMilli   *int64
	MultiplierPresent bool
	MultiplierValid   bool
	NoFiveHour        *bool
	NoFiveHourPresent bool
	NoFiveHourValid   bool
	SourceKind        string
	MappingReviewed   bool
	MappingVersion    string
	SignatureValid    bool
	Payload           map[string]interface{}
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
	// Keep presence and validity distinct. An explicitly malformed wire field is
	// evidence against a reviewed Premium mapping, never an absent field that can
	// be silently filled with the fixture defaults below.
	if value, present, valid := explicitMultiplierAt(root); present {
		out.MultiplierPresent, out.MultiplierValid = true, valid
		if valid {
			out.MultiplierMilli = &value
		}
	}
	if value, present, valid := explicitBoolAt(root, "no_five_hour_limit", "noFiveHourLimit", "has_no_five_hour_limit"); present {
		out.NoFiveHourPresent, out.NoFiveHourValid = true, valid
		if valid {
			out.NoFiveHour = &value
		}
	}
	// Only copy scalar, non-secret evidence fields into the optional redacted
	// payload.  The storage validator applies a second forbidden-key check.
	for _, key := range []string{"plan_type", "planType", "subscription_type", "subscriptionType", "seat_type", "seatType", "usage_multiplier_milli", "usageMultiplierMilli", "usage_multiplier", "usageMultiplier", "multiplier", "no_five_hour_limit", "noFiveHourLimit", "has_no_five_hour_limit"} {
		if value, ok := root[key]; ok && scalarEvidenceValue(value) {
			out.Payload[key] = value
		}
	}
	explicitFlagsCompatible :=
		(!out.MultiplierPresent || (out.MultiplierValid && out.MultiplierMilli != nil && *out.MultiplierMilli == 5000)) &&
			(!out.NoFiveHourPresent || (out.NoFiveHourValid && out.NoFiveHour != nil && *out.NoFiveHour))
	if version, reviewed := ReviewedBusinessPremiumQuotaMapping(root, sourceKind); reviewed && explicitFlagsCompatible &&
		(out.SeatRaw == "" || premiumSeatRaw(out.SeatRaw)) &&
		(out.MultiplierMilli == nil || *out.MultiplierMilli == 5000) &&
		(out.NoFiveHour == nil || *out.NoFiveHour) {
		multiplier, noFiveHour := int64(5000), true
		out.SeatRaw = string(SeatBusinessPremium)
		out.MultiplierMilli = &multiplier
		out.NoFiveHour = &noFiveHour
		out.MappingReviewed = true
		out.MappingVersion = version
		// Persist only the reviewed, credential-free projection. Including the
		// mapping version also gives upgraded evidence a new immutable HMAC
		// fingerprint instead of colliding with an older plan-only observation.
		out.Payload["mapping_version"] = version
		out.Payload["observed_5x_subtype"] = true
		out.Payload["observed_primary_window_minutes"] = int64(7 * 24 * 60)
		out.Payload["observed_no_five_hour_window"] = true
	}
	return out
}

// ReviewedBusinessPremiumQuotaMapping recognizes the exact, reviewed Codex
// quota shape from the authorized 2026-08-30 fixture. Generic team/business
// labels, a missing five-hour window, or the 5x subtype by itself never pass.
// self_serve_business_usage_based deliberately remains unknown.
func ReviewedBusinessPremiumQuotaMapping(root map[string]interface{}, sourceKind string) (string, bool) {
	if strings.TrimSpace(sourceKind) != "quota_metadata" || len(root) == 0 {
		return "", false
	}
	plan := firstStringAt(root, "plan_type", "planType", "subscription_type", "subscriptionType", "plan")
	if !strings.EqualFold(strings.TrimSpace(plan), BusinessPremiumRawPlanLabel) {
		return "", false
	}

	limits := firstMapAt(root, "rate_limits", "rateLimit", "rateLimits")
	primaryKeys, secondaryKeys := []string{"primary", "primary_window", "primaryWindow"}, []string{"secondary", "secondary_window", "secondaryWindow"}
	if limits == nil {
		limits = firstMapAt(root, "rate_limit")
		primaryKeys, secondaryKeys = []string{"primary_window", "primaryWindow", "primary"}, []string{"secondary_window", "secondaryWindow", "secondary"}
	}
	if limits == nil {
		return "", false
	}
	primary := firstMapAt(limits, primaryKeys...)
	if quotaWindowDurationSeconds(primary) != 7*24*60*60 {
		return "", false
	}
	if secondary := firstMapAt(limits, secondaryKeys...); quotaWindowDurationSeconds(secondary) > 0 {
		return "", false
	}
	return BusinessPremiumMappingVersion, true
}

func firstMapAt(root map[string]interface{}, keys ...string) map[string]interface{} {
	for _, key := range keys {
		if value, ok := root[key].(map[string]interface{}); ok {
			return value
		}
	}
	return nil
}

func quotaWindowDurationSeconds(window map[string]interface{}) int64 {
	if len(window) == 0 {
		return 0
	}
	if minutes, ok := firstNumberAt(window, "window_minutes", "windowMinutes"); ok && minutes > 0 && minutes <= math.MaxInt64/60 {
		return minutes * 60
	}
	if seconds, ok := firstNumberAt(window, "limit_window_seconds", "limitWindowSeconds", "window_seconds", "windowSeconds"); ok && seconds > 0 {
		return seconds
	}
	return 0
}

func premiumSeatRaw(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "business_premium", "premium", "premium_5x", "5x", "5x_team", "business_5x":
		return true
	default:
		return false
	}
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

// explicitMultiplierAt accepts only positive integral numeric values. It checks
// every supported alias so a malformed or conflicting duplicate cannot hide
// behind a valid sibling field. `usage_multiplier` is normalized from 5 to
// 5000 milli-units, while an explicit milli-unit field is left as-is.
func explicitMultiplierAt(root map[string]interface{}) (int64, bool, bool) {
	var value int64
	have := false
	for _, candidate := range []struct {
		key   string
		scale bool
	}{
		{key: "usage_multiplier_milli"},
		{key: "usageMultiplierMilli"},
		{key: "usage_multiplier", scale: true},
		{key: "usageMultiplier", scale: true},
		{key: "multiplier", scale: true},
	} {
		raw, present := root[candidate.key]
		if !present {
			continue
		}
		parsed, valid := nonNegativeNumber(raw)
		if !valid || parsed <= 0 {
			return 0, true, false
		}
		if candidate.scale && parsed < 100 {
			parsed *= 1000
		}
		if have && value != parsed {
			return 0, true, false
		}
		value, have = parsed, true
	}
	return value, have, have
}

// explicitBoolAt preserves the same three-state contract for booleans. JSON
// strings such as "true" are intentionally invalid instead of coerced.
func explicitBoolAt(root map[string]interface{}, keys ...string) (bool, bool, bool) {
	var value bool
	have := false
	for _, key := range keys {
		raw, present := root[key]
		if !present {
			continue
		}
		parsed, valid := raw.(bool)
		if !valid {
			return false, true, false
		}
		if have && value != parsed {
			return false, true, false
		}
		value, have = parsed, true
	}
	return value, have, have
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
	observation.MappingReviewed, observation.SignatureValid = mappingReviewed || observation.MappingReviewed, signatureValid
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
	// Contradictory explicit flags always fail closed, including when a legacy
	// deployment-wide mapping opt-in is enabled. A Premium spelling cannot turn
	// a 1x seat or a seat with a five-hour limit into a 5x entitlement.
	invalidExplicitFlags := (observation.MultiplierPresent && !observation.MultiplierValid) ||
		(observation.NoFiveHourPresent && !observation.NoFiveHourValid)
	if seat == SeatBusinessPremium &&
		(invalidExplicitFlags ||
			(observation.MultiplierMilli != nil && *observation.MultiplierMilli != 5000) ||
			(observation.NoFiveHour != nil && !*observation.NoFiveHour)) {
		seat = SeatUnknown
	}
	result := FromReviewedSeatObservation(ReviewedSeatObservation{
		PlanLabel: observation.PlanLabel, SeatType: seat, SourceKind: observation.SourceKind,
		MappingReviewed: observation.MappingReviewed, SignatureValid: observation.SignatureValid,
	})
	if result.SeatType == SeatUnknown && observation.PlanLabel == "" {
		result.Reason = fmt.Sprintf("%s_no_plan_or_seat_evidence", firstNonEmpty(sourceKind, "wire"))
	}
	if invalidExplicitFlags {
		result.FlagsState = "contradictory"
	} else if observation.MultiplierMilli != nil || observation.NoFiveHour != nil {
		if observation.MultiplierMilli == nil || observation.NoFiveHour == nil {
			result.FlagsState = "unknown"
		} else if *observation.MultiplierMilli != 5000 || !*observation.NoFiveHour {
			result.FlagsState = "contradictory"
		} else {
			result.FlagsState = "known"
		}
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
