package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// codex429ConfirmationSchemaSQL is intentionally separate from schemaSQL: the
// state is a small, additive operational record and must be safe to introduce
// on an already-running database. EnsureCodex429ConfirmationState is therefore
// also called by every public operation below, which lets older deployments use
// the durable confirmation path before their next full Store.Init migration.
const codex429ConfirmationSchemaSQL = `
CREATE TABLE IF NOT EXISTS codex_429_confirmations(
  account_id TEXT NOT NULL,
  scope TEXT NOT NULL,
  first_observed_at INTEGER NOT NULL,
  observed_count INTEGER NOT NULL DEFAULT 1 CHECK(observed_count >= 1 AND observed_count <= 2),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(account_id, scope),
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_codex_429_confirmations_first_observed
  ON codex_429_confirmations(first_observed_at);
`

// Codex429Confirmation is the bounded, account-local evidence that explicit
// upstream 429 responses were seen for one request scope. It is deliberately
// not a cooldown, retry budget, or account-health record.
type Codex429Confirmation struct {
	AccountID       string
	Scope           string
	FirstObservedAt int64
	ObservedCount   int
	UpdatedAt       int64
}

// EnsureCodex429ConfirmationState applies the additive confirmation-state
// migration. It is idempotent and is exposed so Store initialization can invoke
// it as a named migration phase; public state operations also invoke it lazily
// for rolling upgrades.
func (s *Store) EnsureCodex429ConfirmationState(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("codex 429 confirmation storage is unavailable")
	}
	_, err := s.db.ExecContext(ctx, codex429ConfirmationSchemaSQL)
	return err
}

// ObserveCodex429 stores one explicit upstream 429 for accountID and scope.
// It returns true after two signals have been observed within windowSeconds.
// observedAt is accepted explicitly so callers can use the response timestamp;
// pass zero to use the store clock. The counter is capped at two, and a stale
// or clock-rewound record is restarted rather than treated as confirmation.
func (s *Store) ObserveCodex429(ctx context.Context, accountID, scope string, observedAt, windowSeconds int64) (bool, error) {
	accountID, scope = strings.TrimSpace(accountID), strings.TrimSpace(scope)
	if accountID == "" {
		return false, errors.New("account id is required")
	}
	if scope == "" {
		return false, errors.New("codex 429 confirmation scope is required")
	}
	if windowSeconds <= 0 {
		return false, fmt.Errorf("codex 429 confirmation window must be positive: %d", windowSeconds)
	}
	if observedAt == 0 {
		observedAt = Now()
	}
	if err := s.EnsureCodex429ConfirmationState(ctx); err != nil {
		return false, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var state Codex429Confirmation
	err = tx.QueryRowContext(ctx, `
SELECT account_id, scope, first_observed_at, observed_count, updated_at
FROM codex_429_confirmations WHERE account_id=? AND scope=?`, accountID, scope).
		Scan(&state.AccountID, &state.Scope, &state.FirstObservedAt, &state.ObservedCount, &state.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `
INSERT INTO codex_429_confirmations(account_id, scope, first_observed_at, observed_count, updated_at)
VALUES(?, ?, ?, 1, ?)`, accountID, scope, observedAt, observedAt); err != nil {
			return false, err
		}
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Keep the inclusive window used by the pre-durable guard: a signal exactly
	// windowSeconds after the first is still eligible. A clock rewind always
	// starts a new window so stale evidence can never confirm a future response.
	expired := observedAt < state.FirstObservedAt ||
		(observedAt > state.FirstObservedAt && observedAt-state.FirstObservedAt > windowSeconds)
	if expired {
		_, err = tx.ExecContext(ctx, `
UPDATE codex_429_confirmations
SET first_observed_at=?, observed_count=1, updated_at=?
WHERE account_id=? AND scope=?`, observedAt, observedAt, accountID, scope)
		if err != nil {
			return false, err
		}
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}

	// A confirmed row remains evidence until it expires or an administrator
	// clears it.  Returning false here is important: concurrent/later observers
	// must not each start a new cooldown/audit transition for the same pair of
	// signals.
	if state.ObservedCount >= 2 {
		if _, err = tx.ExecContext(ctx, `
UPDATE codex_429_confirmations SET updated_at=? WHERE account_id=? AND scope=?`, observedAt, accountID, scope); err != nil {
			return false, err
		}
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	count := 2
	_, err = tx.ExecContext(ctx, `
UPDATE codex_429_confirmations SET observed_count=?, updated_at=?
WHERE account_id=? AND scope=?`, count, observedAt, accountID, scope)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return count >= 2, nil
}

// ResetCodex429 removes only the bounded confirmation evidence for one
// account/scope. It never changes account cooldown, quota, or health state.
func (s *Store) ResetCodex429(ctx context.Context, accountID, scope string) error {
	accountID, scope = strings.TrimSpace(accountID), strings.TrimSpace(scope)
	if accountID == "" {
		return errors.New("account id is required")
	}
	if scope == "" {
		return errors.New("codex 429 confirmation scope is required")
	}
	if err := s.EnsureCodex429ConfirmationState(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM codex_429_confirmations WHERE account_id=? AND scope=?`, accountID, scope)
	return err
}

// ClearExpiredCodex429 removes unconfirmed or confirmed evidence whose first
// observation is strictly before cutoff. A caller normally supplies
// Now()-windowSeconds; strict comparison preserves the inclusive window edge.
func (s *Store) ClearExpiredCodex429(ctx context.Context, cutoff int64) error {
	if err := s.EnsureCodex429ConfirmationState(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM codex_429_confirmations WHERE first_observed_at < ?`, cutoff)
	return err
}
