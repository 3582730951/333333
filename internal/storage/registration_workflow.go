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
