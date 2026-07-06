package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	CodexReauthJobQueued            = "queued"
	CodexReauthJobRunning           = "running"
	CodexReauthJobSucceeded         = "succeeded"
	CodexReauthJobFailed            = "failed"
	CodexReauthJobNeedsManual       = "needs_manual"
	CodexReauthJobWorkspaceMismatch = "workspace_mismatch"
)

// AccountCodexReauthConfig stores the optional credentials used to repair a Codex
// OAuth login. Password and OTPURL are plaintext only in memory; storage encrypts
// them with the same Store tokenKey used for account tokens and session cookies.
type AccountCodexReauthConfig struct {
	AccountID          string `json:"account_id"`
	LoginEmail         string `json:"login_email"`
	Password           string `json:"password,omitempty"`
	OTPURL             string `json:"otp_url,omitempty"`
	TargetWorkspaceID  string `json:"target_workspace_id,omitempty"`
	AutoEnabled        bool   `json:"auto_enabled"`
	LastStatus         string `json:"last_status,omitempty"`
	LastError          string `json:"last_error,omitempty"`
	PasswordConfigured bool   `json:"password_configured,omitempty"`
	OTPURLConfigured   bool   `json:"otp_url_configured,omitempty"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
}

type AccountCodexReauthJob struct {
	ID         int64  `json:"id"`
	AccountID  string `json:"account_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	StartedAt  int64  `json:"started_at,omitempty"`
	FinishedAt int64  `json:"finished_at,omitempty"`
}

func (s *Store) UpsertCodexReauthConfig(ctx context.Context, cfg AccountCodexReauthConfig) error {
	accountID := strings.TrimSpace(cfg.AccountID)
	if accountID == "" {
		return errors.New("account_id required")
	}
	loginEmail := strings.TrimSpace(cfg.LoginEmail)
	targetWorkspaceID := strings.TrimSpace(cfg.TargetWorkspaceID)
	lastStatus := strings.TrimSpace(cfg.LastStatus)
	lastError := strings.TrimSpace(cfg.LastError)
	now := Now()
	createdAt := cfg.CreatedAt
	if createdAt == 0 {
		createdAt = now
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO account_codex_reauth_config(account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET
 login_email = excluded.login_email,
 encrypted_password = excluded.encrypted_password,
 encrypted_otp_url = excluded.encrypted_otp_url,
 target_workspace_id = excluded.target_workspace_id,
 auto_enabled = excluded.auto_enabled,
 last_status = CASE WHEN excluded.last_status <> '' THEN excluded.last_status ELSE account_codex_reauth_config.last_status END,
 last_error = CASE WHEN excluded.last_error <> '' THEN excluded.last_error ELSE account_codex_reauth_config.last_error END,
 updated_at = excluded.updated_at`,
		accountID, loginEmail, s.sealToken(cfg.Password), s.sealToken(cfg.OTPURL), targetWorkspaceID, boolInt(cfg.AutoEnabled), lastStatus, lastError, createdAt, now)
	return err
}

func (s *Store) GetCodexReauthConfig(ctx context.Context, accountID string) (AccountCodexReauthConfig, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at FROM account_codex_reauth_config WHERE account_id = ?`, strings.TrimSpace(accountID))
	cfg, err := s.scanCodexReauthConfig(row.Scan, true)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountCodexReauthConfig{}, false, nil
	}
	if err != nil {
		return AccountCodexReauthConfig{}, false, err
	}
	return cfg, true, nil
}

func (s *Store) GetCodexReauthConfigPublic(ctx context.Context, accountID string) (AccountCodexReauthConfig, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at FROM account_codex_reauth_config WHERE account_id = ?`, strings.TrimSpace(accountID))
	cfg, err := s.scanCodexReauthConfig(row.Scan, false)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountCodexReauthConfig{}, false, nil
	}
	if err != nil {
		return AccountCodexReauthConfig{}, false, err
	}
	return cfg, true, nil
}

func (s *Store) scanCodexReauthConfig(scan func(...interface{}) error, includeSecrets bool) (AccountCodexReauthConfig, error) {
	var cfg AccountCodexReauthConfig
	var autoEnabled int
	var encryptedPassword, encryptedOTPURL string
	if err := scan(&cfg.AccountID, &cfg.LoginEmail, &encryptedPassword, &encryptedOTPURL, &cfg.TargetWorkspaceID, &autoEnabled, &cfg.LastStatus, &cfg.LastError, &cfg.CreatedAt, &cfg.UpdatedAt); err != nil {
		return cfg, err
	}
	cfg.AutoEnabled = autoEnabled != 0
	cfg.PasswordConfigured = strings.TrimSpace(encryptedPassword) != ""
	cfg.OTPURLConfigured = strings.TrimSpace(encryptedOTPURL) != ""
	if includeSecrets {
		cfg.Password = s.openToken(encryptedPassword)
		cfg.OTPURL = s.openToken(encryptedOTPURL)
	}
	return cfg, nil
}

func (s *Store) UpdateCodexReauthConfigStatus(ctx context.Context, accountID, status, lastError string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE account_codex_reauth_config SET last_status = ?, last_error = ?, updated_at = ? WHERE account_id = ?`, strings.TrimSpace(status), strings.TrimSpace(lastError), Now(), strings.TrimSpace(accountID))
	return err
}

func (s *Store) EnqueueCodexReauthJob(ctx context.Context, accountID, reason string) (AccountCodexReauthJob, bool, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return AccountCodexReauthJob{}, false, errors.New("account_id required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT id, account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at FROM account_codex_reauth_jobs WHERE account_id = ? AND status IN (?, ?) ORDER BY created_at ASC, id ASC LIMIT 1`, accountID, CodexReauthJobQueued, CodexReauthJobRunning)
	job, err := scanCodexReauthJob(row.Scan)
	if err == nil {
		return job, false, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AccountCodexReauthJob{}, false, err
	}
	now := Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO account_codex_reauth_jobs(account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at) VALUES(?, ?, ?, '', ?, ?, 0, 0)`, accountID, CodexReauthJobQueued, strings.TrimSpace(reason), now, now)
	if err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	return AccountCodexReauthJob{ID: id, AccountID: accountID, Status: CodexReauthJobQueued, Reason: strings.TrimSpace(reason), CreatedAt: now, UpdatedAt: now}, true, nil
}

func (s *Store) GetCodexReauthJob(ctx context.Context, id int64) (AccountCodexReauthJob, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at FROM account_codex_reauth_jobs WHERE id = ?`, id)
	job, err := scanCodexReauthJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountCodexReauthJob{}, false, nil
	}
	if err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) ListCodexReauthJobs(ctx context.Context, accountID string, limit int) ([]AccountCodexReauthJob, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at FROM account_codex_reauth_jobs WHERE account_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`, strings.TrimSpace(accountID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AccountCodexReauthJob
	for rows.Next() {
		job, err := scanCodexReauthJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) LatestCodexReauthJob(ctx context.Context, accountID string) (AccountCodexReauthJob, bool, error) {
	row := s.rdb.QueryRowContext(ctx, `SELECT id, account_id, status, reason, last_error, created_at, updated_at, started_at, finished_at FROM account_codex_reauth_jobs WHERE account_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, strings.TrimSpace(accountID))
	job, err := scanCodexReauthJob(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountCodexReauthJob{}, false, nil
	}
	if err != nil {
		return AccountCodexReauthJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) UpdateCodexReauthJobStatus(ctx context.Context, id int64, status, lastError string) error {
	status = strings.TrimSpace(status)
	if status == "" {
		return errors.New("status required")
	}
	now := Now()
	startedExpr := "started_at"
	finishedExpr := "finished_at"
	if status == CodexReauthJobRunning {
		startedExpr = fmt.Sprintf("CASE WHEN started_at = 0 THEN %d ELSE started_at END", now)
	}
	switch status {
	case CodexReauthJobSucceeded, CodexReauthJobFailed, CodexReauthJobNeedsManual, CodexReauthJobWorkspaceMismatch:
		finishedExpr = fmt.Sprintf("CASE WHEN finished_at = 0 THEN %d ELSE finished_at END", now)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE account_codex_reauth_jobs SET status = ?, last_error = ?, updated_at = ?, started_at = `+startedExpr+`, finished_at = `+finishedExpr+` WHERE id = ?`, status, strings.TrimSpace(lastError), now, id)
	return err
}

func scanCodexReauthJob(scan func(...interface{}) error) (AccountCodexReauthJob, error) {
	var job AccountCodexReauthJob
	err := scan(&job.ID, &job.AccountID, &job.Status, &job.Reason, &job.LastError, &job.CreatedAt, &job.UpdatedAt, &job.StartedAt, &job.FinishedAt)
	return job, err
}

func (s *Store) ListCodexReauthConfigPublicByAccountIDs(ctx context.Context, accountIDs []string) (map[string]AccountCodexReauthConfig, error) {
	out := make(map[string]AccountCodexReauthConfig)
	ids := make([]string, 0, len(accountIDs))
	seen := map[string]struct{}{}
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT account_id, login_email, encrypted_password, encrypted_otp_url, target_workspace_id, auto_enabled, last_status, last_error, created_at, updated_at FROM account_codex_reauth_config WHERE account_id IN (`+sqlPlaceholders(len(ids))+`)`, stringArgs(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		cfg, err := s.scanCodexReauthConfig(rows.Scan, false)
		if err != nil {
			return nil, err
		}
		out[cfg.AccountID] = cfg
	}
	return out, rows.Err()
}
