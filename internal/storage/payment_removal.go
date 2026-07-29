package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type PaymentRemovalResult struct {
	CancelledTasks     int64
	ClearedPaymentRows int64
	QuarantinedMocks   int64
	AccountsForReview  int64
}

var (
	legacyMockID    = regexp.MustCompile(`^acc_[0-9]{10,20}_[0-9]+$`)
	legacyMockEmail = regexp.MustCompile(`^user[0-9]+@example\.com$`)
	legacyMockPhone = regexp.MustCompile(`^\+1234567[0-9]{4}$`)
)

// RemovePaymentFeatureData is the one-release compatibility migration for the
// deleted payment subsystem. It empties payment secret tables, cancels unfinished
// Plus tasks and removes persisted payment policies. The empty compatibility
// tables intentionally remain until the next schema version.
func (s *Store) RemovePaymentFeatureData(ctx context.Context) (PaymentRemovalResult, error) {
	var result PaymentRemovalResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	for _, table := range []string{"gopay_accounts", "paypal_accounts"} {
		res, execErr := tx.ExecContext(ctx, `DELETE FROM `+table)
		if execErr != nil {
			return result, fmt.Errorf("clear compatibility table %s: %w", table, execErr)
		}
		affected, _ := res.RowsAffected()
		result.ClearedPaymentRows += affected
	}
	res, err := tx.ExecContext(ctx, `
UPDATE lifecycle_tasks
SET status='cancelled',
    result_json='{"reason":"payment_feature_removed"}',
    finished_at=CASE WHEN finished_at=0 THEN ? ELSE finished_at END
WHERE task_type IN ('upgrade_plus','register_and_plus')
  AND status NOT IN ('completed','failed','cancelled')`, Now())
	if err != nil {
		return result, err
	}
	result.CancelledTasks, _ = res.RowsAffected()

	if _, err = tx.ExecContext(ctx, `
UPDATE registration_jobs
SET status='cancelled', error='payment_feature_removed',
    completed_at=CASE WHEN completed_at=0 THEN ? ELSE completed_at END,
    updated_at=?
WHERE lower(config_json) LIKE '%"upgrade_to_plus":true%'
  AND status NOT IN ('completed','failed','cancelled')`, Now(), Now()); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `
DELETE FROM provider_settings
WHERE lower(provider_type) IN ('payment','gopay','paypal')
   OR lower(provider_key) IN ('payment','gopay','paypal')`); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `
DELETE FROM settings
WHERE lower(key) LIKE '%gopay%'
   OR lower(key) LIKE '%paypal%'
   OR lower(key) LIKE '%payment%'
   OR lower(key) LIKE '%checkout%'
   OR lower(key) LIKE '%webhook%'`); err != nil {
		return result, err
	}
	if err = removePlusAutomationPolicy(ctx, tx); err != nil {
		return result, err
	}
	if err = ensureLegacyReviewTable(ctx, tx); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	s.InvalidateSettingsCache()

	quarantined, review, err := s.isolateLegacyMockAccounts(ctx)
	result.QuarantinedMocks = quarantined
	result.AccountsForReview = review
	return result, err
}

func removePlusAutomationPolicy(ctx context.Context, tx *sql.Tx) error {
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='automation_policies'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var policies map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &policies) != nil {
		// A corrupt policy blob is safer to disable than to preserve alongside
		// removed billing directives.
		_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key='automation_policies'`)
		return err
	}
	delete(policies, "plus")
	for key, value := range policies {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(value, &envelope) == nil && strings.EqualFold(strings.TrimSpace(envelope.Type), "plus") {
			delete(policies, key)
		}
	}
	encoded, err := json.Marshal(policies)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settings SET value=?,updated_at=? WHERE key='automation_policies'`, string(encoded), Now())
	return err
}

func ensureLegacyReviewTable(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS legacy_account_review(
  account_id TEXT PRIMARY KEY,
  reason TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  reviewed_at INTEGER NOT NULL DEFAULT 0
)`)
	return err
}

func (s *Store) isolateLegacyMockAccounts(ctx context.Context) (int64, int64, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT a.id,COALESCE(a.email,''),COALESCE(a.phone,''),COALESCE(a.registration_task_id,''),
       COALESCE(t.access_token,''),COALESCE(t.refresh_token,''),COALESCE(t.openai_api_key,''),
       COALESCE(t.id_token_raw,''),COALESCE(t.agent_private_key,'')
FROM accounts a
LEFT JOIN account_auth_tokens t ON t.account_id=a.id
WHERE a.registration_method='auto' AND COALESCE(a.registration_task_id,'')<>''`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	type candidate struct {
		id, email, phone, task string
		hasCredential          bool
	}
	var candidates []candidate
	for rows.Next() {
		var row candidate
		var access, refresh, apiKey, idToken, privateKey string
		if err := rows.Scan(&row.id, &row.email, &row.phone, &row.task, &access, &refresh, &apiKey, &idToken, &privateKey); err != nil {
			return 0, 0, err
		}
		row.hasCredential = access != "" || refresh != "" || apiKey != "" || idToken != "" || privateKey != ""
		candidates = append(candidates, row)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var quarantined, review int64
	for _, row := range candidates {
		certainMock := legacyMockID.MatchString(row.id) &&
			legacyMockEmail.MatchString(strings.ToLower(row.email)) &&
			legacyMockPhone.MatchString(row.phone) &&
			!row.hasCredential
		if certainMock {
			res, execErr := tx.ExecContext(ctx, `
UPDATE accounts
SET status='quarantined',quarantine_reason='invalid_legacy_mock',quarantine_until=0,updated_at=?
WHERE id=?`, Now(), row.id)
			if execErr != nil {
				return 0, 0, execErr
			}
			affected, _ := res.RowsAffected()
			quarantined += affected
			continue
		}
		if _, execErr := tx.ExecContext(ctx, `
INSERT INTO legacy_account_review(account_id,reason,created_at)
VALUES(?,?,?)
ON CONFLICT(account_id) DO UPDATE SET reason=excluded.reason`,
			row.id, "legacy_lifecycle_account_requires_review", Now()); execErr != nil {
			return 0, 0, execErr
		}
		review++
	}
	return quarantined, review, tx.Commit()
}
