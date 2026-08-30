package storage

import (
	"context"
	"errors"
)

const accountExportConfirmationSchemaSQL = `
CREATE TABLE IF NOT EXISTS account_export_confirmations (
  nonce_hash TEXT PRIMARY KEY,
  expires_at BIGINT NOT NULL,
  used_at BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_account_export_confirmations_expiry
  ON account_export_confirmations(expires_at,used_at);
`

var ErrAccountExportConfirmationInvalid = errors.New("account export confirmation is invalid, expired, or already used")

func (s *Store) ProvisionAccountExportConfirmation(ctx context.Context, nonceHash string, expiresAt int64) error {
	now := Now()
	if nonceHash == "" || expiresAt <= now {
		return ErrAccountExportConfirmationInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_export_confirmations WHERE nonce_hash IN (
SELECT nonce_hash FROM account_export_confirmations WHERE expires_at<? OR used_at>0 ORDER BY expires_at LIMIT 1000)`, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO account_export_confirmations(nonce_hash,expires_at,used_at,created_at) VALUES(?,?,0,?)`, nonceHash, expiresAt, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConsumeAccountExportConfirmation(ctx context.Context, nonceHash string, now int64) error {
	if now <= 0 {
		now = Now()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE account_export_confirmations SET used_at=?
WHERE nonce_hash=? AND used_at=0 AND expires_at>=?`, now, nonceHash, now)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrAccountExportConfirmationInvalid
	}
	return nil
}
