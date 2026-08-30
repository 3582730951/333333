package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"

	"codex-account-pool/internal/entitlement"
	"codex-account-pool/internal/storage"
)

// captureEntitlementEvidence is intentionally conservative.  Wire fields are
// parsed and persisted for audit, but Premium/5x is emitted only when an
// operator has explicitly enabled a reviewed mapping.  This keeps a rolling
// upstream schema from silently changing scheduling or billing semantics.
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
	fingerprint := entitlementEvidenceFingerprint(s.identitySecretCached, accountID, source, payloadJSON)
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
	// The production default is false until a real, reviewed Business Premium
	// fixture is installed.  An explicit deployment opt-in is required to turn
	// the mapping on; it is not inferred from plan labels or capacity.
	return strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_POOL_ENTITLEMENT_MAPPING_REVIEWED")), "true")
}

func entitlementEvidenceFingerprint(secret []byte, accountID, source string, payload []byte) string {
	if len(secret) < 16 || strings.TrimSpace(accountID) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("entitlement-evidence-v1\x00"))
	_, _ = mac.Write([]byte(accountID))
	_, _ = mac.Write([]byte("\x00" + normalizedEntitlementSource(source) + "\x00"))
	_, _ = mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
