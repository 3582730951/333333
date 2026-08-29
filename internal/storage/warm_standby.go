package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"codex-account-pool/internal/secretbox"
)

// PrepareWarmStandby validates the already-expanded schema without running
// migrations or writing markers. SQLite query_only and PostgreSQL's
// default_transaction_read_only guard make accidental constructor-time writes fail
// closed at the database layer, not merely by convention in the caller.
func (s *Store) PrepareWarmStandby(ctx context.Context) error {
	if s == nil || s.db == nil || s.rdb == nil {
		return errors.New("warm standby storage is unavailable")
	}
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("warm standby storage ping: %w", err)
	}
	switch s.driver {
	case "sqlite":
		if _, err := s.db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
			return fmt.Errorf("enable warm standby query-only mode: %w", err)
		}
	case "postgres":
		if err := configurePostgresStandbyPool(ctx, s.db); err != nil {
			return err
		}
	}
	if err := s.CheckWarmStandby(ctx); err != nil {
		return err
	}
	s.standbyReadOnly.Store(true)
	s.warmStandbyMode.Store(true)
	return nil
}

// RestoreWarmStandby is used only when promotion fails before the process ever
// becomes active. A draining former-active worker must stay writable for final
// request/journal/RPM flushes and therefore never calls this method.
func (s *Store) RestoreWarmStandby(ctx context.Context) error {
	if s == nil || !s.warmStandbyMode.Load() || s.standbyReadOnly.Load() {
		return nil
	}
	switch s.driver {
	case "sqlite":
		if _, err := s.db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
			return fmt.Errorf("restore warm standby query-only mode: %w", err)
		}
	case "postgres":
		if err := configurePostgresStandbyPool(ctx, s.db); err != nil {
			return err
		}
	}
	s.standbyReadOnly.Store(true)
	return nil
}

func (s *Store) CheckWarmStandby(ctx context.Context) error {
	if s == nil || s.db == nil || s.rdb == nil {
		return errors.New("warm standby storage is unavailable")
	}
	checks := []string{
		`SELECT key,value FROM settings WHERE 1=0`,
		`SELECT id,status FROM accounts WHERE 1=0`,
		`SELECT writer_id,account_id,bucket_start,request_count,updated_at FROM account_request_rate_buckets WHERE 1=0`,
	}
	for _, query := range checks {
		rows, err := s.rdb.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("warm standby schema is not expand-compatible: %w", err)
		}
		_ = rows.Close()
	}
	return nil
}

// PromoteWarmStandby enables the write connection only after the stable worker
// link selects this process. Active-lease acquisition remains the fencing gate.
func (s *Store) PromoteWarmStandby(ctx context.Context) error {
	if s == nil || !s.standbyReadOnly.Load() {
		return nil
	}
	switch s.driver {
	case "sqlite":
		if _, err := s.db.ExecContext(ctx, `PRAGMA query_only=OFF`); err != nil {
			return fmt.Errorf("disable warm standby query-only mode: %w", err)
		}
	case "postgres":
		if err := promotePostgresPool(ctx, s.db); err != nil {
			return err
		}
	}
	s.standbyReadOnly.Store(false)
	return nil
}

func (s *Store) WarmStandbyReadOnly() bool {
	return s != nil && s.standbyReadOnly.Load()
}

// ValidateEncryptionSentinelReadOnly proves key compatibility but never creates or
// rotates the sentinel. The expand-only migration/active promotion owns those writes.
func (s *Store) ValidateEncryptionSentinelReadOnly(ctx context.Context) error {
	if s == nil || len(s.tokenKey) != 32 {
		return errors.New("persistent storage master key is not configured")
	}
	const marker = "codex-pool-encryption-sentinel"
	var stored string
	err := s.rdb.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, encryptionSentinelSetting).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("encryption sentinel is missing; run expand-only migration before warm standby")
	}
	if err != nil {
		return err
	}
	plain, err := secretbox.OpenDomainWithKeys(s.tokenKeys, "sentinel", stored)
	if err != nil {
		return fmt.Errorf("decrypt encryption sentinel: %w", err)
	}
	if plain != marker {
		return errors.New("encryption sentinel validation failed")
	}
	return nil
}
