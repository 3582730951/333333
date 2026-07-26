package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// TurboGPTRegisterJob is the durable state of one automated registration.
type TurboGPTRegisterJob struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	Phase                string `json:"phase"`
	Phone                string `json:"phone,omitempty"`
	Email                string `json:"email,omitempty"`
	Password             string `json:"password,omitempty"`
	FullName             string `json:"full_name,omitempty"`
	BirthDate            string `json:"birth_date,omitempty"`
	PhoneCountryCode     string `json:"phone_country_code,omitempty"`
	PhoneCountryDialCode string `json:"phone_country_dial_code,omitempty"`
	SMSPlatform          string `json:"sms_platform,omitempty"`
	SMSOperator          string `json:"sms_operator,omitempty"`
	SMSActivationID      string `json:"sms_activation_id,omitempty"`
	MailDomain           string `json:"mail_domain,omitempty"`
	ConfigJSON           string `json:"config_json,omitempty"`
	ResultJSON           string `json:"result_json,omitempty"`
	Error                string `json:"error,omitempty"`
	Attempts             int    `json:"attempts"`
	AutoImport           bool   `json:"auto_import"`
	ImportedAccountID    string `json:"imported_account_id,omitempty"`
	Phase1CompletedAt    int64  `json:"phase1_completed_at,omitempty"`
	Phase2CompletedAt    int64  `json:"phase2_completed_at,omitempty"`
	Phase3CompletedAt    int64  `json:"phase3_completed_at,omitempty"`
	StartedAt            int64  `json:"started_at,omitempty"`
	CompletedAt          int64  `json:"completed_at,omitempty"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

// TurboGPTRegisterToken is the OAuth result produced by phase 3. Secret fields
// are transparently encrypted at rest using the store token key.
type TurboGPTRegisterToken struct {
	JobID        string `json:"job_id"`
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	RawJSON      string `json:"raw_json,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

const turboJobColumns = `id, status, phase, phone, email, password, full_name, birth_date, phone_country_code, phone_country_dial_code, sms_platform, sms_operator, sms_activation_id, mail_domain, config_json, result_json, error, attempts, auto_import, imported_account_id, phase1_completed_at, phase2_completed_at, phase3_completed_at, started_at, completed_at, created_at, updated_at`

func scanTurboGPTRegisterJob(scan func(...interface{}) error) (TurboGPTRegisterJob, error) {
	var job TurboGPTRegisterJob
	var autoImport int
	err := scan(
		&job.ID, &job.Status, &job.Phase, &job.Phone, &job.Email, &job.Password,
		&job.FullName, &job.BirthDate, &job.PhoneCountryCode, &job.PhoneCountryDialCode,
		&job.SMSPlatform, &job.SMSOperator, &job.SMSActivationID, &job.MailDomain,
		&job.ConfigJSON, &job.ResultJSON, &job.Error, &job.Attempts, &autoImport,
		&job.ImportedAccountID, &job.Phase1CompletedAt, &job.Phase2CompletedAt,
		&job.Phase3CompletedAt, &job.StartedAt, &job.CompletedAt, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return TurboGPTRegisterJob{}, err
	}
	job.AutoImport = autoImport != 0
	return job, nil
}

func (s *Store) CreateTurboGPTRegisterJob(ctx context.Context, job TurboGPTRegisterJob) error {
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" {
		return errors.New("turbo register job id required")
	}
	if job.Status == "" {
		job.Status = "pending"
	}
	if job.Phase == "" {
		job.Phase = "phase1"
	}
	if job.ConfigJSON == "" {
		job.ConfigJSON = "{}"
	}
	if job.ResultJSON == "" {
		job.ResultJSON = "{}"
	}
	now := Now()
	if job.CreatedAt == 0 {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO turbo_gpt_register_jobs(`+turboJobColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ID, job.Status, job.Phase, job.Phone, job.Email, s.sealToken(job.Password), job.FullName,
		job.BirthDate, job.PhoneCountryCode, job.PhoneCountryDialCode, job.SMSPlatform,
		job.SMSOperator, job.SMSActivationID, job.MailDomain, job.ConfigJSON, job.ResultJSON,
		job.Error, job.Attempts, boolInt(job.AutoImport), job.ImportedAccountID,
		job.Phase1CompletedAt, job.Phase2CompletedAt, job.Phase3CompletedAt, job.StartedAt,
		job.CompletedAt, job.CreatedAt, job.UpdatedAt)
	return err
}

func (s *Store) UpdateTurboGPTRegisterJob(ctx context.Context, job TurboGPTRegisterJob) error {
	job.ID = strings.TrimSpace(job.ID)
	if job.ID == "" {
		return errors.New("turbo register job id required")
	}
	job.UpdatedAt = Now()
	result, err := s.db.ExecContext(ctx, `UPDATE turbo_gpt_register_jobs SET status=?, phase=?, phone=?, email=?, password=?, full_name=?, birth_date=?, phone_country_code=?, phone_country_dial_code=?, sms_platform=?, sms_operator=?, sms_activation_id=?, mail_domain=?, config_json=?, result_json=?, error=?, attempts=?, auto_import=?, imported_account_id=?, phase1_completed_at=?, phase2_completed_at=?, phase3_completed_at=?, started_at=?, completed_at=?, updated_at=? WHERE id=?`,
		job.Status, job.Phase, job.Phone, job.Email, s.sealToken(job.Password), job.FullName,
		job.BirthDate, job.PhoneCountryCode, job.PhoneCountryDialCode, job.SMSPlatform,
		job.SMSOperator, job.SMSActivationID, job.MailDomain, job.ConfigJSON, job.ResultJSON,
		job.Error, job.Attempts, boolInt(job.AutoImport), job.ImportedAccountID,
		job.Phase1CompletedAt, job.Phase2CompletedAt, job.Phase3CompletedAt, job.StartedAt,
		job.CompletedAt, job.UpdatedAt, job.ID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetTurboGPTRegisterJob(ctx context.Context, id string) (TurboGPTRegisterJob, error) {
	job, err := scanTurboGPTRegisterJob(s.rdb.QueryRowContext(ctx, `SELECT `+turboJobColumns+` FROM turbo_gpt_register_jobs WHERE id=?`, id).Scan)
	if err != nil {
		return TurboGPTRegisterJob{}, err
	}
	job.Password = s.openToken(job.Password)
	return job, nil
}

func (s *Store) ListTurboGPTRegisterJobs(ctx context.Context, status string, limit int) ([]TurboGPTRegisterJob, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + turboJobColumns + ` FROM turbo_gpt_register_jobs`
	args := []interface{}{}
	if strings.TrimSpace(status) != "" {
		query += ` WHERE status=?`
		args = append(args, strings.TrimSpace(status))
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TurboGPTRegisterJob, 0)
	for rows.Next() {
		job, scanErr := scanTurboGPTRegisterJob(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		job.Password = s.openToken(job.Password)
		out = append(out, job)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTurboGPTRegisterJob(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM turbo_gpt_register_jobs WHERE id=?`, id)
	return err
}

func (s *Store) UpsertTurboGPTRegisterToken(ctx context.Context, token TurboGPTRegisterToken) error {
	if strings.TrimSpace(token.JobID) == "" {
		return errors.New("turbo register token job_id required")
	}
	now := Now()
	if token.CreatedAt == 0 {
		token.CreatedAt = now
	}
	token.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO turbo_gpt_register_tokens(job_id,email,access_token,refresh_token,id_token,account_id,expires_at,raw_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET email=excluded.email, access_token=excluded.access_token, refresh_token=excluded.refresh_token, id_token=excluded.id_token, account_id=excluded.account_id, expires_at=excluded.expires_at, raw_json=excluded.raw_json, updated_at=excluded.updated_at`,
		token.JobID, token.Email, s.sealToken(token.AccessToken), s.sealToken(token.RefreshToken),
		s.sealToken(token.IDToken), token.AccountID, token.ExpiresAt, s.sealToken(token.RawJSON),
		token.CreatedAt, token.UpdatedAt)
	return err
}

func (s *Store) GetTurboGPTRegisterToken(ctx context.Context, jobID string) (TurboGPTRegisterToken, error) {
	var token TurboGPTRegisterToken
	err := s.rdb.QueryRowContext(ctx, `SELECT job_id,email,access_token,refresh_token,id_token,account_id,expires_at,raw_json,created_at,updated_at FROM turbo_gpt_register_tokens WHERE job_id=?`, jobID).Scan(
		&token.JobID, &token.Email, &token.AccessToken, &token.RefreshToken, &token.IDToken,
		&token.AccountID, &token.ExpiresAt, &token.RawJSON, &token.CreatedAt, &token.UpdatedAt)
	if err != nil {
		return TurboGPTRegisterToken{}, err
	}
	token.AccessToken = s.openToken(token.AccessToken)
	token.RefreshToken = s.openToken(token.RefreshToken)
	token.IDToken = s.openToken(token.IDToken)
	token.RawJSON = s.openToken(token.RawJSON)
	return token, nil
}

func (s *Store) SetTurboGPTRegisterConfig(ctx context.Context, key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("turbo register config key required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO turbo_gpt_register_config(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, key, s.sealToken(value), Now())
	return err
}

func (s *Store) GetTurboGPTRegisterConfig(ctx context.Context) (map[string]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT key,value FROM turbo_gpt_register_config ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		out[key] = s.openToken(value)
	}
	return out, rows.Err()
}
