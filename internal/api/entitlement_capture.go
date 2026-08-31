package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/storage"
)

// captureEntitlementEvidence is intentionally conservative. Wire fields are
// parsed and persisted for audit, but Premium/5x is emitted only by a built-in,
// versioned real-fixture mapping or an operator-enabled explicit seat mapping.
// This keeps a rolling upstream schema from silently changing scheduling or
// billing semantics.
func (s *Server) captureEntitlementEvidence(ctx context.Context, accountID, source string, root map[string]interface{}) {
	if s == nil || s.store == nil || strings.TrimSpace(accountID) == "" || len(root) == 0 {
		return
	}
	normalized, observation := entitlement.NormalizedFromWire(root, source, entitlementMappingReviewed(), false)
	if normalized.PlanFamily == entitlement.PlanUnknown && normalized.SeatType == entitlement.SeatUnknown {
		return
	}
	payload := observation.Payload
	if payload == nil {
		payload = map[string]interface{}{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return
	}
	now := storage.Now()
	fingerprint := entitlementEvidenceFingerprint(s.identitySecretCached, accountID, source, payloadJSON, now)
	if fingerprint == "" {
		return
	}
	id := "ent_" + fingerprint[:32]
	evidence := storage.AccountEntitlementEvidence{
		ID: id, AccountID: accountID, SourceKind: normalizedEntitlementSource(source),
		SourceFingerprint: fingerprint, PlanFamily: normalized.PlanFamily, SeatType: normalized.SeatType,
		UsageMultiplierMilli: normalized.UsageMultiplierMilli, NoFiveHourLimit: normalized.NoFiveHourLimit,
		RawPlanLabel: observation.PlanLabel, Confidence: normalized.Confidence, ObservedAt: now,
		ExpiresAt: now + 24*60*60, PayloadRedacted: payloadJSON,
	}
	// A repeated poll of the same response is a harmless idempotent replay.
	if _, err := s.store.InsertAccountEntitlementEvidence(ctx, evidence); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return
	}
}

func normalizedEntitlementSource(source string) string {
	switch strings.TrimSpace(source) {
	case "workspace_entitlement", "billing_entitlement", "quota_metadata", "verified_jwt", "imported_hint":
		return strings.TrimSpace(source)
	default:
		return "quota_metadata"
	}
}

func entitlementMappingReviewed() bool {
	// The built-in Business Premium quota mapping reviews its own exact wire
	// shape and does not depend on this switch. This legacy opt-in remains only
	// for deployments with separately reviewed, explicit seat fields; it never
	// makes a generic team/business plan label sufficient.
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_POOL_ENTITLEMENT_MAPPING_REVIEWED")), "true")
}

const entitlementEvidenceRefreshBucketSeconds int64 = 60 * 60

// entitlementEvidenceFingerprint is content- and observation-time-derived.
// The hourly bucket makes a poll/stream observation of the same response
// idempotent during normal convergence, while a later successful observation
// gets a fresh evidence ID and can extend the 24-hour freshness interval.
func entitlementEvidenceFingerprint(secret []byte, accountID, source string, payload []byte, observedAt int64) string {
	if len(secret) < 16 || strings.TrimSpace(accountID) == "" {
		return ""
	}
	if observedAt <= 0 {
		observedAt = storage.Now()
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("entitlement-evidence-v1\x00"))
	_, _ = mac.Write([]byte(accountID))
	_, _ = mac.Write([]byte("\x00" + normalizedEntitlementSource(source) + "\x00"))
	_, _ = mac.Write(payload)
	_, _ = mac.Write([]byte("\x00bucket=" + strconv.FormatInt(observedAt/entitlementEvidenceRefreshBucketSeconds, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}
