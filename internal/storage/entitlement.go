package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"codex-account-pool/internal/entitlement"

	"github.com/google/uuid"
)

type AccountEntitlementEvidence struct {
	ID                   string                 `json:"id"`
	AccountID            string                 `json:"account_id"`
	SourceKind           string                 `json:"source_kind"`
	SourceFingerprint    string                 `json:"source_fingerprint"`
	PlanFamily           entitlement.PlanFamily `json:"plan_family"`
	SeatType             entitlement.SeatType   `json:"seat_type"`
	UsageMultiplierMilli *int64                 `json:"usage_multiplier_milli"`
	NoFiveHourLimit      *bool                  `json:"no_five_hour_limit"`
	RawPlanLabel         string                 `json:"raw_plan_label"`
	Confidence           string                 `json:"confidence"`
	ObservedAt           int64                  `json:"observed_at"`
	ExpiresAt            int64                  `json:"expires_at"`
	PayloadRedacted      json.RawMessage        `json:"payload_redacted"`
}

func entitlementSourcePriority(source string) int {
	switch source {
	case "workspace_entitlement", "billing_entitlement":
		return 4
	case "verified_jwt":
		return 3
	case "quota_metadata":
		return 2
	case "imported_hint":
		return 1
	default:
		return 0
	}
}

func validateEntitlementEvidence(evidence AccountEntitlementEvidence) error {
	if strings.TrimSpace(evidence.AccountID) == "" || len(strings.TrimSpace(evidence.SourceFingerprint)) < 16 {
		return fmt.Errorf("entitlement account and source fingerprint are required")
	}
	if entitlementSourcePriority(evidence.SourceKind) == 0 {
		return fmt.Errorf("unknown entitlement source kind")
	}
	if evidence.ObservedAt <= 0 || evidence.ExpiresAt <= evidence.ObservedAt {
		return fmt.Errorf("entitlement evidence validity interval is invalid")
	}
	if evidence.Confidence != "low" && evidence.Confidence != "medium" && evidence.Confidence != "high" {
		return fmt.Errorf("entitlement confidence is invalid")
	}
	flagsKnown := evidence.UsageMultiplierMilli != nil || evidence.NoFiveHourLimit != nil
	if flagsKnown && (evidence.UsageMultiplierMilli == nil || evidence.NoFiveHourLimit == nil) {
		return fmt.Errorf("entitlement flags must be present or absent as a set")
	}
	if evidence.SeatType == entitlement.SeatBusinessPremium {
		if entitlementSourcePriority(evidence.SourceKind) < 2 || evidence.Confidence != "high" ||
			evidence.UsageMultiplierMilli == nil || *evidence.UsageMultiplierMilli != 5000 ||
			evidence.NoFiveHourLimit == nil || !*evidence.NoFiveHourLimit {
			return fmt.Errorf("Premium seat requires reviewed authoritative 5x evidence")
		}
	}
	if len(evidence.PayloadRedacted) == 0 || !json.Valid(evidence.PayloadRedacted) || len(evidence.PayloadRedacted) > 16<<10 {
		return fmt.Errorf("redacted entitlement payload is invalid")
	}
	lowerPayload := strings.ToLower(string(evidence.PayloadRedacted))
	for _, forbidden := range []string{"access_token", "refresh_token", "id_token", "authorization", "session_cookie"} {
		if strings.Contains(lowerPayload, forbidden) {
			return fmt.Errorf("redacted entitlement payload contains a forbidden credential field")
		}
	}
	return nil
}

func (s *Store) InsertAccountEntitlementEvidence(ctx context.Context, evidence AccountEntitlementEvidence) (AccountEntitlementEvidence, error) {
	if evidence.ID == "" {
		evidence.ID = "ent_" + uuid.NewString()
	}
	if err := validateEntitlementEvidence(evidence); err != nil {
		return AccountEntitlementEvidence{}, err
	}
	flagsKnown := evidence.UsageMultiplierMilli != nil && evidence.NoFiveHourLimit != nil
	noFiveHour := false
	if evidence.NoFiveHourLimit != nil {
		noFiveHour = *evidence.NoFiveHourLimit
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO account_entitlement_evidence(id,account_id,source_kind,source_fingerprint,plan_family,seat_type,
usage_multiplier_milli,no_five_hour_limit,entitlement_flags_known,raw_plan_label,confidence,observed_at,expires_at,payload_redacted_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, evidence.ID, evidence.AccountID, evidence.SourceKind,
		evidence.SourceFingerprint, string(evidence.PlanFamily), string(evidence.SeatType), nullableInt64(evidence.UsageMultiplierMilli),
		boolInt(noFiveHour), boolInt(flagsKnown), evidence.RawPlanLabel, evidence.Confidence, evidence.ObservedAt,
		evidence.ExpiresAt, string(evidence.PayloadRedacted))
	if err != nil {
		return AccountEntitlementEvidence{}, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return AccountEntitlementEvidence{}, rowsErr
		}
		return AccountEntitlementEvidence{}, fmt.Errorf("entitlement evidence id already exists")
	}
	return evidence, nil
}

func (s *Store) ListAccountEntitlementEvidence(ctx context.Context, accountID string) ([]AccountEntitlementEvidence, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id,account_id,source_kind,source_fingerprint,plan_family,seat_type,usage_multiplier_milli,
no_five_hour_limit,entitlement_flags_known,raw_plan_label,confidence,observed_at,expires_at,payload_redacted_json
FROM account_entitlement_evidence WHERE account_id=? ORDER BY observed_at DESC,id`, strings.TrimSpace(accountID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountEntitlementEvidence{}
	for rows.Next() {
		var item AccountEntitlementEvidence
		var planFamily, seatType, payload string
		var multiplier sql.NullInt64
		var noFiveHour, flagsKnown int64
		if err := rows.Scan(&item.ID, &item.AccountID, &item.SourceKind, &item.SourceFingerprint, &planFamily, &seatType,
			&multiplier, &noFiveHour, &flagsKnown, &item.RawPlanLabel, &item.Confidence, &item.ObservedAt, &item.ExpiresAt, &payload); err != nil {
			return nil, err
		}
		item.PlanFamily, item.SeatType = entitlement.PlanFamily(planFamily), entitlement.SeatType(seatType)
		item.PayloadRedacted = json.RawMessage(payload)
		if flagsKnown != 0 {
			if !multiplier.Valid {
				return nil, errors.New("entitlement evidence flags are corrupt")
			}
			item.UsageMultiplierMilli = nullableScanPointer(multiplier)
			value := noFiveHour != 0
			item.NoFiveHourLimit = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CurrentAccountEntitlementEvidence(ctx context.Context, accountID string, at int64) (*AccountEntitlementEvidence, bool, error) {
	rows, err := s.ListAccountEntitlementEvidence(ctx, accountID)
	if err != nil {
		return nil, false, err
	}
	if at <= 0 {
		at = Now()
	}
	bestPriority := -1
	var best *AccountEntitlementEvidence
	conflict := false
	for index := range rows {
		item := &rows[index]
		if item.ExpiresAt <= at {
			continue
		}
		priority := entitlementSourcePriority(item.SourceKind)
		if priority > bestPriority {
			copy := *item
			best, bestPriority, conflict = &copy, priority, false
			continue
		}
		if priority == bestPriority && best != nil && (item.SeatType != best.SeatType || item.PlanFamily != best.PlanFamily) {
			conflict = true
		}
	}
	return best, conflict, nil
}
