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
	FlagsState           string                 `json:"flags_state,omitempty"`
	Freshness            string                 `json:"freshness,omitempty"`
}

// EntitlementEvidenceFreshness classifies an observation without inferring a
// seat. Unknown means no observation exists; stale means observations exist but
// all validity intervals have elapsed; fresh means at least one is still valid.
func EntitlementEvidenceFreshness(rows []AccountEntitlementEvidence, at int64) string {
	if at <= 0 {
		at = Now()
	}
	if len(rows) == 0 {
		return "unknown"
	}
	for _, row := range rows {
		if row.ObservedAt > 0 && row.ExpiresAt > at {
			return "fresh"
		}
	}
	return "stale"
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

func entitlementEvidenceQuality(evidence AccountEntitlementEvidence) int {
	quality := 0
	switch evidence.Confidence {
	case "high":
		quality = 300
	case "medium":
		quality = 200
	case "low":
		quality = 100
	}
	if evidence.SeatType != entitlement.SeatUnknown {
		quality += 40
	}
	if evidence.UsageMultiplierMilli != nil && evidence.NoFiveHourLimit != nil {
		quality += 20
	}
	if evidence.PlanFamily != entitlement.PlanUnknown {
		quality += 10
	}
	return quality
}

func entitlementEvidenceConflicts(left, right AccountEntitlementEvidence) bool {
	planConflict := left.PlanFamily != entitlement.PlanUnknown && right.PlanFamily != entitlement.PlanUnknown &&
		left.PlanFamily != right.PlanFamily
	seatConflict := left.SeatType != entitlement.SeatUnknown && right.SeatType != entitlement.SeatUnknown &&
		left.SeatType != right.SeatType
	return planConflict || seatConflict
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
	if evidence.FlagsState == "" {
		switch {
		case evidence.UsageMultiplierMilli == nil && evidence.NoFiveHourLimit == nil:
			evidence.FlagsState = "unknown"
		case evidence.UsageMultiplierMilli == nil || evidence.NoFiveHourLimit == nil:
			evidence.FlagsState = "unknown"
		case *evidence.UsageMultiplierMilli == 5000 && *evidence.NoFiveHourLimit:
			evidence.FlagsState = "known"
		default:
			evidence.FlagsState = "contradictory"
		}
	}
	if evidence.FlagsState != "known" && evidence.FlagsState != "unknown" && evidence.FlagsState != "contradictory" {
		return fmt.Errorf("entitlement flags state is invalid")
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
		item, err := scanAccountEntitlementEvidence(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// ListAccountEntitlementEvidenceByAccountIDs is the batched form used by the
// account and quota admin views. It keeps presentation evidence on the same
// snapshot path as account labels and quota rows instead of issuing one query
// per card.
func (s *Store) ListAccountEntitlementEvidenceByAccountIDs(ctx context.Context, accountIDs []string) (map[string][]AccountEntitlementEvidence, error) {
	result := make(map[string][]AccountEntitlementEvidence, len(accountIDs))
	ids := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		accountID = strings.TrimSpace(accountID)
		if accountID == "" {
			continue
		}
		if _, exists := result[accountID]; exists {
			continue
		}
		result[accountID] = nil
		ids = append(ids, accountID)
	}
	if len(ids) == 0 {
		return result, nil
	}
	const batchSize = 500
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		rows, err := s.rdb.QueryContext(ctx, `SELECT id,account_id,source_kind,source_fingerprint,plan_family,seat_type,usage_multiplier_milli,
no_five_hour_limit,entitlement_flags_known,raw_plan_label,confidence,observed_at,expires_at,payload_redacted_json
FROM account_entitlement_evidence WHERE account_id IN (`+sqlPlaceholders(end-start)+`) ORDER BY account_id,observed_at DESC,id`, stringArgs(ids[start:end])...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			item, scanErr := scanAccountEntitlementEvidence(rows.Scan)
			if scanErr != nil {
				_ = rows.Close()
				return nil, scanErr
			}
			result[item.AccountID] = append(result[item.AccountID], item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func scanAccountEntitlementEvidence(scan func(dest ...interface{}) error) (AccountEntitlementEvidence, error) {
	var item AccountEntitlementEvidence
	var planFamily, seatType, payload string
	var multiplier sql.NullInt64
	var noFiveHour, flagsKnown int64
	if err := scan(&item.ID, &item.AccountID, &item.SourceKind, &item.SourceFingerprint, &planFamily, &seatType,
		&multiplier, &noFiveHour, &flagsKnown, &item.RawPlanLabel, &item.Confidence, &item.ObservedAt, &item.ExpiresAt, &payload); err != nil {
		return AccountEntitlementEvidence{}, err
	}
	item.PlanFamily, item.SeatType = entitlement.PlanFamily(planFamily), entitlement.SeatType(seatType)
	item.PayloadRedacted = json.RawMessage(payload)
	if item.ExpiresAt > Now() {
		item.Freshness = "fresh"
	} else {
		item.Freshness = "stale"
	}
	if flagsKnown != 0 {
		if multiplier.Valid {
			item.FlagsState = "known"
		} else {
			item.FlagsState = "contradictory"
		}
	} else {
		item.FlagsState = "unknown"
	}
	if flagsKnown != 0 {
		if !multiplier.Valid {
			return AccountEntitlementEvidence{}, errors.New("entitlement evidence flags are corrupt")
		}
		item.UsageMultiplierMilli = nullableScanPointer(multiplier)
		value := noFiveHour != 0
		item.NoFiveHourLimit = &value
	}
	return item, nil
}

func (s *Store) CurrentAccountEntitlementEvidence(ctx context.Context, accountID string, at int64) (*AccountEntitlementEvidence, bool, error) {
	rows, err := s.ListAccountEntitlementEvidence(ctx, accountID)
	if err != nil {
		return nil, false, err
	}
	return currentAccountEntitlementEvidence(rows, at)
}

// CurrentAccountEntitlementEvidenceByAccountIDs applies the exact same
// precedence and conflict rules as the single-account lookup, while batching
// the read for list endpoints.
func (s *Store) CurrentAccountEntitlementEvidenceByAccountIDs(ctx context.Context, accountIDs []string, at int64) (map[string]*AccountEntitlementEvidence, map[string]bool, error) {
	rowsByAccount, err := s.ListAccountEntitlementEvidenceByAccountIDs(ctx, accountIDs)
	if err != nil {
		return nil, nil, err
	}
	current := make(map[string]*AccountEntitlementEvidence, len(rowsByAccount))
	conflicts := make(map[string]bool, len(rowsByAccount))
	for accountID, rows := range rowsByAccount {
		value, conflict, currentErr := currentAccountEntitlementEvidence(rows, at)
		if currentErr != nil {
			return nil, nil, currentErr
		}
		current[accountID], conflicts[accountID] = value, conflict
	}
	return current, conflicts, nil
}

func currentAccountEntitlementEvidence(rows []AccountEntitlementEvidence, at int64) (*AccountEntitlementEvidence, bool, error) {
	if at <= 0 {
		at = Now()
	}
	bestPriority := -1
	bestQuality := -1
	var best *AccountEntitlementEvidence
	conflict := false
	for index := range rows {
		item := &rows[index]
		if item.ExpiresAt <= at {
			continue
		}
		priority := entitlementSourcePriority(item.SourceKind)
		quality := entitlementEvidenceQuality(*item)
		if priority > bestPriority {
			copy := *item
			best, bestPriority, bestQuality, conflict = &copy, priority, quality, false
			continue
		}
		if priority != bestPriority || best == nil {
			continue
		}
		if entitlementEvidenceConflicts(*best, *item) {
			conflict = true
		}
		// Rows are ordered newest-first, so an equal-quality observation keeps
		// the existing (newer) winner. A later low-confidence plan-only poll can
		// therefore never hide a still-valid reviewed Premium observation.
		if quality > bestQuality {
			copy := *item
			best, bestQuality = &copy, quality
		}
	}
	return best, conflict, nil
}
