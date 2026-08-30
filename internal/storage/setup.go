package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
)

func AdminSetupTokenMAC(secret []byte, plaintext string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte("codex-pool/admin-setup-token/v1\x00"))
	_, _ = mac.Write([]byte(strings.TrimSpace(plaintext)))
	return "hmac-sha256-v1:" + hex.EncodeToString(mac.Sum(nil))
}

const adminSetupSchemaSQL = `
CREATE TABLE IF NOT EXISTS admin_setup_state (
  singleton_id INTEGER PRIMARY KEY,
  token_mac TEXT NOT NULL DEFAULT '',
  token_version TEXT NOT NULL DEFAULT 'hmac-sha256-v1',
  issued_at INTEGER NOT NULL DEFAULT 0,
  expires_at INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  locked_at INTEGER NOT NULL DEFAULT 0,
  used_at INTEGER NOT NULL DEFAULT 0,
  completed_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0,
  CHECK(singleton_id = 1)
);
`

const AdminSetupMaxFailures = 5

var (
	ErrAdminSetupUnavailable = errors.New("admin setup is unavailable")
	ErrAdminSetupExpired     = errors.New("admin setup token expired")
	ErrAdminSetupLocked      = errors.New("admin setup is locked")
	ErrAdminSetupInvalid     = errors.New("invalid admin setup token")
	ErrAdminSetupCompleted   = errors.New("admin setup is already completed")
)

type AdminSetupStatus struct {
	Required     bool  `json:"required"`
	Provisioned  bool  `json:"-"`
	ExpiresAt    int64 `json:"expires_at"`
	FailureCount int   `json:"-"`
	Locked       bool  `json:"-"`
	CompletedAt  int64 `json:"-"`
}

func (s *Store) AdminSetupStatus(ctx context.Context) (AdminSetupStatus, error) {
	admin, err := s.HasAdminUser(ctx)
	if err != nil {
		return AdminSetupStatus{}, err
	}
	status := AdminSetupStatus{Required: !admin}
	var tokenMAC string
	var lockedAt, usedAt int64
	err = s.rdb.QueryRowContext(ctx, `SELECT token_mac,expires_at,failure_count,locked_at,used_at,completed_at FROM admin_setup_state WHERE singleton_id=1`).
		Scan(&tokenMAC, &status.ExpiresAt, &status.FailureCount, &lockedAt, &usedAt, &status.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}
	if err != nil {
		return AdminSetupStatus{}, err
	}
	status.Provisioned = strings.TrimSpace(tokenMAC) != "" && usedAt == 0
	status.Locked = lockedAt > 0 || status.FailureCount >= AdminSetupMaxFailures
	if admin || usedAt > 0 || status.CompletedAt > 0 {
		status.Required = false
	}
	return status, nil
}

// ProvisionAdminSetup atomically rotates the one-time verifier. tokenMAC is a
// versioned HMAC produced by the process from a persistent secret; plaintext never
// crosses the storage boundary.
func (s *Store) ProvisionAdminSetup(ctx context.Context, tokenMAC string, issuedAt, expiresAt int64) error {
	tokenMAC = strings.TrimSpace(tokenMAC)
	if tokenMAC == "" || issuedAt <= 0 || expiresAt <= issuedAt {
		return ErrAdminSetupUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var admins int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&admins); err != nil {
		return err
	}
	if admins > 0 {
		return ErrAdminSetupCompleted
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_setup_state(singleton_id,token_mac,token_version,issued_at,expires_at,failure_count,locked_at,used_at,completed_at,updated_at)
VALUES(1,?,'hmac-sha256-v1',?,?,0,0,0,0,?)
ON CONFLICT(singleton_id) DO UPDATE SET token_mac=excluded.token_mac,token_version=excluded.token_version,
 issued_at=excluded.issued_at,expires_at=excluded.expires_at,failure_count=0,locked_at=0,used_at=0,completed_at=0,updated_at=excluded.updated_at`,
		tokenMAC, issuedAt, expiresAt, issuedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) recordAdminSetupFailure(ctx context.Context, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_setup_state SET failure_count=failure_count+1,
 locked_at=CASE WHEN failure_count+1>=? THEN ? ELSE locked_at END,updated_at=?
WHERE singleton_id=1 AND used_at=0 AND completed_at=0 AND locked_at=0`, AdminSetupMaxFailures, now, now)
	return err
}

// ClaimAdminSetup verifies and consumes the one-time token and creates the sole
// bootstrap administrator in one transaction. The conditional consume is the
// concurrency gate: exactly one of simultaneous valid claims can commit.
func (s *Store) ClaimAdminSetup(ctx context.Context, candidateMAC string, admin User, session UserSession, now int64) (User, error) {
	if now <= 0 {
		now = Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var expectedMAC string
	var expiresAt, failures, lockedAt, usedAt, completedAt int64
	err = tx.QueryRowContext(ctx, `SELECT token_mac,expires_at,failure_count,locked_at,used_at,completed_at FROM admin_setup_state WHERE singleton_id=1`).
		Scan(&expectedMAC, &expiresAt, &failures, &lockedAt, &usedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrAdminSetupUnavailable
	}
	if err != nil {
		return User{}, err
	}
	if usedAt > 0 || completedAt > 0 {
		return User{}, ErrAdminSetupCompleted
	}
	if lockedAt > 0 || failures >= AdminSetupMaxFailures {
		return User{}, ErrAdminSetupLocked
	}
	if expiresAt < now {
		return User{}, ErrAdminSetupExpired
	}
	if subtle.ConstantTimeCompare([]byte(expectedMAC), []byte(strings.TrimSpace(candidateMAC))) != 1 {
		// Roll back the read transaction before recording the bounded failure on
		// the normal writer connection.
		_ = tx.Rollback()
		if failureErr := s.recordAdminSetupFailure(ctx, now); failureErr != nil {
			return User{}, failureErr
		}
		return User{}, ErrAdminSetupInvalid
	}
	var admins int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&admins); err != nil {
		return User{}, err
	}
	if admins > 0 {
		return User{}, ErrAdminSetupCompleted
	}
	result, err := tx.ExecContext(ctx, `UPDATE admin_setup_state SET used_at=?,completed_at=?,updated_at=?
WHERE singleton_id=1 AND used_at=0 AND completed_at=0 AND locked_at=0 AND failure_count<? AND expires_at>=?`,
		now, now, now, AdminSetupMaxFailures, now)
	if err != nil {
		return User{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		if err != nil {
			return User{}, err
		}
		return User{}, ErrAdminSetupCompleted
	}
	admin.ID = strings.TrimSpace(admin.ID)
	admin.Email = strings.TrimSpace(strings.ToLower(admin.Email))
	admin.Role = "admin"
	admin.Status = "active"
	admin.CreatedAt = now
	admin.UpdatedAt = now
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,tenant_id,email,name,role,status,password_hash,created_at,updated_at)
VALUES(?,?,?,?, 'admin','active',?,?,?)`, admin.ID, admin.TenantID, admin.Email, admin.Name, admin.PasswordHash, now, now)
	if err != nil {
		return User{}, err
	}
	session.UserID = admin.ID
	if session.CreatedAt == 0 {
		session.CreatedAt = now
	}
	if strings.TrimSpace(session.TokenHash) == "" || session.ExpiresAt <= session.CreatedAt {
		return User{}, ErrAdminSetupUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_sessions(token_hash,user_id,user_agent,created_at,expires_at) VALUES(?,?,?,?,?)`,
		session.TokenHash, session.UserID, session.UserAgent, session.CreatedAt, session.ExpiresAt); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return admin, nil
}

// ClaimAdminWithRecoveryCredential is the migration bridge for deployments that
// already have an explicitly configured legacy admin_token but no browser admin.
// The API authenticates that credential before entering this transaction. No
// token material crosses the storage boundary; this method only performs the
// atomic no-admin -> one-admin transition and consumes any stale setup state.
func (s *Store) ClaimAdminWithRecoveryCredential(ctx context.Context, admin User, session UserSession, now int64) (User, error) {
	if now <= 0 {
		now = Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()

	admin.ID = strings.TrimSpace(admin.ID)
	admin.Email = strings.TrimSpace(strings.ToLower(admin.Email))
	if admin.ID == "" || admin.Email == "" || strings.TrimSpace(admin.PasswordHash) == "" {
		return User{}, ErrAdminSetupUnavailable
	}
	admin.Role = "admin"
	admin.Status = "active"
	admin.CreatedAt = now
	admin.UpdatedAt = now
	result, err := tx.ExecContext(ctx, `INSERT INTO users(id,tenant_id,email,name,role,status,password_hash,created_at,updated_at)
SELECT ?,?,?,?,'admin','active',?,?,?
WHERE NOT EXISTS(SELECT 1 FROM users WHERE role='admin' AND status='active')`,
		admin.ID, admin.TenantID, admin.Email, admin.Name, admin.PasswordHash, now, now)
	if err != nil {
		return User{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return User{}, err
	}
	if affected != 1 {
		return User{}, ErrAdminSetupCompleted
	}

	session.UserID = admin.ID
	if session.CreatedAt == 0 {
		session.CreatedAt = now
	}
	if strings.TrimSpace(session.TokenHash) == "" || session.ExpiresAt <= session.CreatedAt {
		return User{}, ErrAdminSetupUnavailable
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_sessions(token_hash,user_id,user_agent,created_at,expires_at) VALUES(?,?,?,?,?)`,
		session.TokenHash, session.UserID, session.UserAgent, session.CreatedAt, session.ExpiresAt); err != nil {
		return User{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO admin_setup_state(singleton_id,token_mac,token_version,issued_at,expires_at,failure_count,locked_at,used_at,completed_at,updated_at)
VALUES(1,'','legacy-admin-token-v1',?,?,0,0,?,?,?)
ON CONFLICT(singleton_id) DO UPDATE SET token_mac='',token_version='legacy-admin-token-v1',used_at=excluded.used_at,
 completed_at=excluded.completed_at,updated_at=excluded.updated_at`, now, now, now, now, now); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return admin, nil
}
