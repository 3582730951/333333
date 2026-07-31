package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var ErrRegistrationIdentityExists = errors.New("registered remote identity already exists")

const (
	RegistrationItemPreflight              = "preflight"
	RegistrationItemResourcesLeased        = "resources_leased"
	RegistrationItemRegistering            = "registering"
	RegistrationItemCredentialsObtained    = "credentials_obtained"
	RegistrationItemRemoteAccountVerifying = "remote_account_verifying"
	RegistrationItemImportedProvisioning   = "imported_provisioning"
	RegistrationItemActive                 = "active"
	RegistrationItemRetryWait              = "retry_wait"
	RegistrationItemQuarantined            = "quarantined"
	RegistrationItemFailed                 = "failed"
)

type RegistrationCommit struct {
	Account             Account
	Token               AccountToken
	EgressID            string
	Method              string
	JobID               string
	RecordID            string
	WorkflowItemID      string
	RemoteIdentityAlias string
	// SessionCookie and LoginPassword are transient plaintext inputs. When
	// present they are sealed inside the same transaction as the account so a
	// later workspace switch/OAuth repair never depends on a child-process file.
	SessionCookie string
	LoginPassword string
}

// RegistrationRecoveryResult reports durable work isolated after an unclean
// active-worker exit. Interrupted registrations are never resumed automatically:
// doing so could consume a second paid provider resource or create a duplicate
// upstream identity.
type RegistrationRecoveryResult struct {
	JobsFinalized    int64
	RecordsFailed    int64
	ItemsQuarantined int64
}

// RecoverInterruptedRegistrationWorkflows atomically moves every unfinished
// registration owned by a previous active worker into an operator-review state.
// Successfully committed records/items are retained exactly as-is.
func (s *Store) RecoverInterruptedRegistrationWorkflows(ctx context.Context) (RegistrationRecoveryResult, error) {
	var recovered RegistrationRecoveryResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return recovered, err
	}
	defer tx.Rollback()

	now := Now()
	result, err := tx.ExecContext(ctx, `
UPDATE registration_workflow_items
SET state='quarantined',error_class='interrupted_requires_review',updated_at=?
WHERE job_id IN (
  SELECT id FROM registration_jobs WHERE status IN ('queued','pending','running')
)
AND state IN (
  'preflight','resources_leased','registering','credentials_obtained',
  'remote_account_verifying','imported_provisioning','retry_wait'
)`, now)
	if err != nil {
		return recovered, fmt.Errorf("quarantine interrupted registration items: %w", err)
	}
	recovered.ItemsQuarantined, _ = result.RowsAffected()

	result, err = tx.ExecContext(ctx, `
UPDATE registration_records
SET status='failed',error='interrupted_requires_review'
WHERE job_id IN (
  SELECT id FROM registration_jobs WHERE status IN ('queued','pending','running')
)
AND status IN ('pending','running')`)
	if err != nil {
		return recovered, fmt.Errorf("fail interrupted registration records: %w", err)
	}
	recovered.RecordsFailed, _ = result.RowsAffected()

	// Recalculate succeeded first so a crash between the atomic account commit and
	// the job-counter update does not hide a successfully imported account.
	if _, err = tx.ExecContext(ctx, `
UPDATE registration_jobs
SET succeeded=(
  SELECT COUNT(*) FROM registration_records
  WHERE registration_records.job_id=registration_jobs.id
    AND registration_records.status='success'
)
WHERE status IN ('queued','pending','running')`); err != nil {
		return recovered, fmt.Errorf("recount interrupted registration successes: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE registration_jobs
SET status=CASE WHEN succeeded>0 THEN 'completed_with_review' ELSE 'failed' END,
    failed=CASE WHEN total>succeeded THEN total-succeeded ELSE 0 END,
    error='interrupted_requires_review',
    completed_at=CASE WHEN completed_at=0 THEN ? ELSE completed_at END,
    updated_at=?
WHERE status IN ('queued','pending','running')`, now, now)
	if err != nil {
		return recovered, fmt.Errorf("finalize interrupted registration jobs: %w", err)
	}
	recovered.JobsFinalized, _ = result.RowsAffected()

	if err = tx.Commit(); err != nil {
		return RegistrationRecoveryResult{}, err
	}
	return recovered, nil
}

func (s *Store) CreateRegistrationWorkflowItem(ctx context.Context, id, jobID, method, platform string) error {
	now := Now()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO registration_workflow_items(id,job_id,method,state,platform,created_at,updated_at)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`,
		id, jobID, method, RegistrationItemPreflight, firstNonEmptyString(platform, "chatgpt"), now, now)
	return err
}

func (s *Store) UpdateRegistrationWorkflowItem(ctx context.Context, id, state, errorClass string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	switch state {
	case RegistrationItemPreflight, RegistrationItemResourcesLeased, RegistrationItemRegistering,
		RegistrationItemCredentialsObtained, RegistrationItemRemoteAccountVerifying,
		RegistrationItemImportedProvisioning, RegistrationItemActive, RegistrationItemRetryWait,
		RegistrationItemQuarantined, RegistrationItemFailed:
	default:
		return fmt.Errorf("invalid registration workflow state %q", state)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE registration_workflow_items
SET state=?,error_class=?,attempt=CASE WHEN ?='registering' THEN attempt+1 ELSE attempt END,updated_at=?
WHERE id=?`, state, strings.TrimSpace(errorClass), state, Now(), id)
	return err
}

// CommitRegistration atomically persists a remotely verified credential. Normal
// registrations become active; disposable canaries remain quarantined and therefore
// never enter scheduler selection. Account, encrypted token, concrete egress binding,
// workflow result, and registration record either commit together or remain absent.
func (s *Store) CommitRegistration(ctx context.Context, item RegistrationCommit) error {
	if strings.TrimSpace(item.Account.ID) == "" || strings.TrimSpace(item.Token.AccessToken) == "" {
		return errors.New("verified registration account and access token are required")
	}
	if strings.TrimSpace(item.EgressID) == "" {
		return errors.New("verified registration egress is required")
	}
	item.SessionCookie = strings.TrimSpace(item.SessionCookie)
	if len(item.SessionCookie) > 64<<10 || strings.ContainsAny(item.SessionCookie, "\r\n") {
		return errors.New("registration session cookie is invalid")
	}
	if len(item.LoginPassword) > 4096 || strings.ContainsAny(item.LoginPassword, "\r\n") {
		return errors.New("registration login password is invalid")
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingID, existingTask string
	err = tx.QueryRowContext(ctx, `
SELECT id,COALESCE(registration_task_id,'')
FROM accounts
WHERE id=? OR (
  upstream_account_id<>'' AND upstream_account_id=? AND
  (?='' OR chatgpt_user_id='' OR chatgpt_user_id=?)
)
LIMIT 1`,
		item.Account.ID, item.Account.UpstreamAccountID, item.Account.ChatGPTUserID, item.Account.ChatGPTUserID).
		Scan(&existingID, &existingTask)
	switch {
	case err == nil && (existingID != item.Account.ID || existingTask != item.JobID):
		return fmt.Errorf("%w: account %s", ErrRegistrationIdentityExists, existingID)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return err
	}

	if err := s.upsertAccountTx(ctx, tx, item.Account, item.Token, now); err != nil {
		return err
	}
	if item.SessionCookie != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_session_cookies(account_id,cookie,updated_at)
VALUES(?,?,?)
ON CONFLICT(account_id) DO UPDATE SET cookie=excluded.cookie,updated_at=excluded.updated_at`,
			item.Account.ID, s.sealToken(item.SessionCookie), now); err != nil {
			return err
		}
	}
	if item.SessionCookie != "" || strings.TrimSpace(item.LoginPassword) != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO account_codex_reauth_config(
  account_id,login_email,encrypted_password,encrypted_otp_url,target_workspace_id,
  auto_enabled,last_status,last_error,created_at,updated_at
) VALUES(?,?,?,'','',1,'configured','',?,?)
ON CONFLICT(account_id) DO UPDATE SET
  login_email=CASE WHEN excluded.login_email<>'' THEN excluded.login_email ELSE account_codex_reauth_config.login_email END,
  encrypted_password=CASE WHEN excluded.encrypted_password<>'' THEN excluded.encrypted_password ELSE account_codex_reauth_config.encrypted_password END,
  auto_enabled=1,last_status='configured',last_error='',updated_at=excluded.updated_at`,
			item.Account.ID, strings.TrimSpace(item.Account.Email), s.sealToken(item.LoginPassword), now, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE accounts SET registration_method=?,registration_task_id=?,updated_at=? WHERE id=?`,
		item.Method, item.JobID, now, item.Account.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO account_egress_bindings(
  account_id,primary_egress_id,standby_egress_ids,sidecar_egress_id,cookie_jar_key,
  cooldown_until,recheck_pending,created_at,updated_at
) VALUES(?,?,'','',?,0,0,?,?)
ON CONFLICT(account_id) DO UPDATE SET
  primary_egress_id=excluded.primary_egress_id,
  standby_egress_ids='',
  sidecar_egress_id='',
  cookie_jar_key=excluded.cookie_jar_key,
  cooldown_until=0,
  recheck_pending=0,
  updated_at=excluded.updated_at`,
		item.Account.ID, item.EgressID, item.Account.ID+":"+item.EgressID, now, now); err != nil {
		return err
	}
	if strings.TrimSpace(item.WorkflowItemID) != "" {
		workflowState := RegistrationItemActive
		if !strings.EqualFold(strings.TrimSpace(item.Account.Status), "active") {
			workflowState = RegistrationItemQuarantined
		}
		result, err := tx.ExecContext(ctx, `
UPDATE registration_workflow_items
SET state=?,remote_identity_alias=?,error_class='',updated_at=?
WHERE id=?`, workflowState, item.RemoteIdentityAlias, now, item.WorkflowItemID)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected != 1 {
			return fmt.Errorf("registration workflow item %q not found", item.WorkflowItemID)
		}
	}
	if strings.TrimSpace(item.RecordID) != "" {
		result, err := tx.ExecContext(ctx, `
UPDATE registration_records SET status='success',account_id=? WHERE id=? AND job_id=?`,
			item.Account.ID, item.RecordID, item.JobID)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected != 1 {
			return fmt.Errorf("registration record %q not found", item.RecordID)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.tokenCache.Delete(item.Account.ID)
	return nil
}
