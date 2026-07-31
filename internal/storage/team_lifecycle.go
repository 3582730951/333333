package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

var (
	ErrTeamLifecycleNotFound        = errors.New("team lifecycle workflow not found")
	ErrTeamLifecycleVersionConflict = errors.New("team lifecycle workflow version conflict")
	ErrTeamLifecycleLeaseMismatch   = errors.New("team lifecycle workflow lease mismatch")
)

const teamLifecycleColumns = `
id,idempotency_key,workspace_id,parent_account_id,child_account_id,state,resume_state,
credential_path,membership_ref,credential_ref,phone_challenge_ref,imported_account_id,
replacement_method,replacement_job_ref,quota_remaining_bps,rotate_threshold_bps,attempt,max_attempts,
next_attempt_at,lease_owner,lease_expires_at,error_class,shadow_mode,version,
created_at,updated_at,completed_at,mailbox_provider_key,required_email_domain`

type rowScanner interface {
	Scan(...interface{}) error
}

func scanTeamLifecycleWorkflow(row rowScanner) (TeamLifecycleWorkflow, error) {
	var item TeamLifecycleWorkflow
	var shadow int
	err := row.Scan(
		&item.ID, &item.IdempotencyKey, &item.WorkspaceID, &item.ParentAccountID,
		&item.ChildAccountID, &item.State, &item.ResumeState, &item.CredentialPath,
		&item.MembershipRef, &item.CredentialRef, &item.PhoneChallengeRef,
		&item.ImportedAccountID, &item.ReplacementMethod, &item.ReplacementJobRef, &item.QuotaRemainingBPS,
		&item.RotateThresholdBPS, &item.Attempt, &item.MaxAttempts,
		&item.NextAttemptAt, &item.LeaseOwner, &item.LeaseExpiresAt,
		&item.ErrorClass, &shadow, &item.Version, &item.CreatedAt, &item.UpdatedAt,
		&item.CompletedAt, &item.MailboxProviderKey, &item.RequiredEmailDomain,
	)
	item.ShadowMode = shadow != 0
	return item, err
}

func normalizeTeamLifecycleIdentifier(name, value string, max int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	if len(value) > max {
		return "", fmt.Errorf("%s exceeds %d bytes", name, max)
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", fmt.Errorf("%s contains a control character", name)
		}
	}
	return value, nil
}

func normalizeTeamLifecycleEventJSON(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}", nil
	}
	if len(raw) > 4096 {
		return "", errors.New("team lifecycle event detail exceeds 4096 bytes")
	}
	var value map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", errors.New("team lifecycle event detail must be a JSON object")
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func validTeamLifecycleTransition(from, to string) bool {
	if from == to {
		return from == TeamLifecycleActive
	}
	if to == TeamLifecycleCancelled && !IsTerminalTeamLifecycleState(from) {
		return true
	}
	allowed := map[string][]string{
		TeamLifecycleQueued: {
			TeamLifecycleInviting,
		},
		TeamLifecycleInviting: {
			TeamLifecycleResolvingCredential, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleResolvingCredential: {
			TeamLifecycleCredentialLogin, TeamLifecycleOAuthLogin,
			TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleCredentialLogin: {
			TeamLifecycleImporting, TeamLifecycleOAuthLogin,
			TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleOAuthLogin: {
			TeamLifecyclePhoneVerification, TeamLifecycleImporting,
			TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecyclePhoneVerification: {
			TeamLifecycleImporting, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleImporting: {
			TeamLifecycleActive, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleActive: {
			TeamLifecycleRemoving, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleRemoving: {
			TeamLifecycleEnqueueReplacement, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleEnqueueReplacement: {
			TeamLifecycleCompleted, TeamLifecycleRetryWait, TeamLifecycleReviewRequired,
		},
		TeamLifecycleRetryWait: {
			TeamLifecycleInviting, TeamLifecycleResolvingCredential,
			TeamLifecycleCredentialLogin, TeamLifecycleOAuthLogin,
			TeamLifecyclePhoneVerification, TeamLifecycleImporting,
			TeamLifecycleActive, TeamLifecycleRemoving,
			TeamLifecycleEnqueueReplacement, TeamLifecycleReviewRequired,
		},
		TeamLifecycleReviewRequired: {
			TeamLifecycleQueued, TeamLifecycleInviting, TeamLifecycleResolvingCredential,
			TeamLifecycleCredentialLogin, TeamLifecycleOAuthLogin,
			TeamLifecyclePhoneVerification, TeamLifecycleImporting,
			TeamLifecycleActive, TeamLifecycleRemoving,
			TeamLifecycleEnqueueReplacement,
		},
	}
	for _, candidate := range allowed[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

func (s *Store) UpsertTeamWorkspace(ctx context.Context, workspace TeamWorkspace) (TeamWorkspace, error) {
	var err error
	if workspace.ID, err = normalizeTeamLifecycleIdentifier("workspace id", workspace.ID, 128); err != nil {
		return TeamWorkspace{}, err
	}
	if workspace.Name, err = normalizeTeamLifecycleIdentifier("workspace name", workspace.Name, 256); err != nil {
		return TeamWorkspace{}, err
	}
	if workspace.ParentAccountID, err = normalizeTeamLifecycleIdentifier("parent account id", workspace.ParentAccountID, 256); err != nil {
		return TeamWorkspace{}, err
	}
	if workspace.WorkspaceRef, err = normalizeTeamLifecycleIdentifier("workspace reference", workspace.WorkspaceRef, 512); err != nil {
		return TeamWorkspace{}, err
	}
	workspace.ConnectorKind = strings.ToLower(strings.TrimSpace(workspace.ConnectorKind))
	if workspace.ConnectorKind == "" {
		workspace.ConnectorKind = "native"
	}
	if workspace.MaxMembers <= 0 {
		workspace.MaxMembers = 10
	}
	if workspace.MaxMembers > 10000 {
		return TeamWorkspace{}, errors.New("workspace max members exceeds 10000")
	}
	if workspace.Status == "" {
		workspace.Status = TeamWorkspaceStatusActive
	}
	switch workspace.Status {
	case TeamWorkspaceStatusActive, TeamWorkspaceStatusPaused, TeamWorkspaceStatusDisabled:
	default:
		return TeamWorkspace{}, fmt.Errorf("invalid team workspace status %q", workspace.Status)
	}
	workspace.MailboxProviderKey = strings.ToLower(strings.TrimSpace(workspace.MailboxProviderKey))
	if len(workspace.MailboxProviderKey) > 128 {
		return TeamWorkspace{}, errors.New("mailbox provider key exceeds 128 bytes")
	}
	workspace.RequiredEmailDomain, err = NormalizeMailboxDomain(workspace.RequiredEmailDomain)
	if err != nil {
		return TeamWorkspace{}, err
	}
	if workspace.RequiredEmailDomain != "" {
		workspace.SameDomainRequired = true
	}
	// When the parent is already in the local pool, prevent an accidental
	// workspace policy that can never produce same-domain children.
	if workspace.SameDomainRequired && workspace.RequiredEmailDomain != "" {
		var parentEmail string
		parentErr := s.rdb.QueryRowContext(ctx,
			`SELECT COALESCE(email,'') FROM accounts WHERE id=?`,
			workspace.ParentAccountID,
		).Scan(&parentEmail)
		if parentErr == nil && EmailDomain(parentEmail) != workspace.RequiredEmailDomain {
			return TeamWorkspace{}, errors.New("parent account email does not match required team mailbox domain")
		}
		if parentErr != nil && !errors.Is(parentErr, sql.ErrNoRows) {
			return TeamWorkspace{}, parentErr
		}
	}
	now := Now()
	if workspace.CreatedAt == 0 {
		workspace.CreatedAt = now
	}
	workspace.UpdatedAt = now
	_, err = s.db.ExecContext(ctx, `
INSERT INTO team_workspaces(
  id,name,parent_account_id,workspace_id,workspace_type,max_members,status,created_at,updated_at,
  mailbox_provider_key,required_email_domain,same_domain_required
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  name=excluded.name,parent_account_id=excluded.parent_account_id,
  workspace_id=excluded.workspace_id,workspace_type=excluded.workspace_type,
  max_members=excluded.max_members,status=excluded.status,updated_at=excluded.updated_at,
  mailbox_provider_key=excluded.mailbox_provider_key,
  required_email_domain=excluded.required_email_domain,
  same_domain_required=excluded.same_domain_required`,
		workspace.ID, workspace.Name, workspace.ParentAccountID, workspace.WorkspaceRef,
		workspace.ConnectorKind, workspace.MaxMembers, workspace.Status,
		workspace.CreatedAt, workspace.UpdatedAt, workspace.MailboxProviderKey,
		workspace.RequiredEmailDomain, boolInt(workspace.SameDomainRequired))
	if err != nil {
		return TeamWorkspace{}, err
	}
	return s.GetTeamWorkspace(ctx, workspace.ID)
}

func (s *Store) GetTeamWorkspace(ctx context.Context, id string) (TeamWorkspace, error) {
	var item TeamWorkspace
	err := s.rdb.QueryRowContext(ctx, `
SELECT id,name,parent_account_id,workspace_id,workspace_type,max_members,status,created_at,updated_at,
       mailbox_provider_key,required_email_domain,same_domain_required
FROM team_workspaces WHERE id=?`, strings.TrimSpace(id)).Scan(
		&item.ID, &item.Name, &item.ParentAccountID, &item.WorkspaceRef,
		&item.ConnectorKind, &item.MaxMembers, &item.Status, &item.CreatedAt, &item.UpdatedAt,
		&item.MailboxProviderKey, &item.RequiredEmailDomain, &item.SameDomainRequired,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamWorkspace{}, ErrTeamLifecycleNotFound
	}
	return item, err
}

func (s *Store) ListTeamWorkspaces(ctx context.Context, limit int) ([]TeamWorkspace, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT id,name,parent_account_id,workspace_id,workspace_type,max_members,status,created_at,updated_at,
       mailbox_provider_key,required_email_domain,same_domain_required
FROM team_workspaces ORDER BY updated_at DESC,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TeamWorkspace, 0)
	for rows.Next() {
		var item TeamWorkspace
		if err := rows.Scan(
			&item.ID, &item.Name, &item.ParentAccountID, &item.WorkspaceRef,
			&item.ConnectorKind, &item.MaxMembers, &item.Status, &item.CreatedAt, &item.UpdatedAt,
			&item.MailboxProviderKey, &item.RequiredEmailDomain, &item.SameDomainRequired,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CreateTeamLifecycleWorkflow(ctx context.Context, input CreateTeamLifecycleWorkflowInput) (TeamLifecycleWorkflow, bool, error) {
	var err error
	if input.WorkspaceID, err = normalizeTeamLifecycleIdentifier("workspace id", input.WorkspaceID, 128); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if input.ParentAccountID, err = normalizeTeamLifecycleIdentifier("parent account id", input.ParentAccountID, 256); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if input.ChildAccountID, err = normalizeTeamLifecycleIdentifier("child account id", input.ChildAccountID, 256); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if input.IdempotencyKey, err = normalizeTeamLifecycleIdentifier("idempotency key", input.IdempotencyKey, 256); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if input.ID == "" {
		input.ID = "teamwf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if input.ID, err = normalizeTeamLifecycleIdentifier("workflow id", input.ID, 128); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if input.RotateThresholdBPS <= 0 {
		input.RotateThresholdBPS = 100
	}
	if input.RotateThresholdBPS > 10000 {
		return TeamLifecycleWorkflow{}, false, errors.New("rotation threshold exceeds 10000 basis points")
	}
	if input.MaxAttempts <= 0 {
		input.MaxAttempts = 5
	}
	if input.MaxAttempts > 20 {
		return TeamLifecycleWorkflow{}, false, errors.New("maximum attempts exceeds 20")
	}
	input.ReplacementMethod = strings.ToLower(strings.TrimSpace(input.ReplacementMethod))
	switch input.ReplacementMethod {
	case "", "protocol", "protocol_v2", "node", "browser", "browser_v3":
	default:
		return TeamLifecycleWorkflow{}, false, fmt.Errorf("invalid replacement registration method %q", input.ReplacementMethod)
	}
	workspace, err := s.GetTeamWorkspace(ctx, input.WorkspaceID)
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if workspace.ParentAccountID != input.ParentAccountID {
		return TeamLifecycleWorkflow{}, false, errors.New("workflow parent account does not match workspace parent")
	}
	if input.MailboxProviderKey == "" {
		input.MailboxProviderKey = workspace.MailboxProviderKey
	}
	input.MailboxProviderKey = strings.ToLower(strings.TrimSpace(input.MailboxProviderKey))
	if input.RequiredEmailDomain == "" {
		input.RequiredEmailDomain = workspace.RequiredEmailDomain
	}
	input.RequiredEmailDomain, err = NormalizeMailboxDomain(input.RequiredEmailDomain)
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if workspace.SameDomainRequired && input.RequiredEmailDomain != "" {
		if input.ReplacementMethod == "" {
			// A same-domain workspace must not inherit an unrelated global
			// registrar later. Pin the connector-neutral email protocol by
			// default so a resumed workflow remains deterministic.
			input.ReplacementMethod = "protocol_v2"
		}
		if input.ReplacementMethod == "node" || input.ReplacementMethod == "browser" {
			return TeamLifecycleWorkflow{}, false, errors.New("selected replacement method does not support same-domain email allocation")
		}
		childEmail := input.ChildAccountID
		var storedEmail string
		childErr := s.rdb.QueryRowContext(ctx,
			`SELECT COALESCE(email,'') FROM accounts WHERE id=?`,
			input.ChildAccountID,
		).Scan(&storedEmail)
		if childErr == nil {
			childEmail = storedEmail
		} else if !errors.Is(childErr, sql.ErrNoRows) {
			return TeamLifecycleWorkflow{}, false, childErr
		}
		if EmailDomain(childEmail) != input.RequiredEmailDomain {
			return TeamLifecycleWorkflow{}, false, errors.New("child account email does not match required team mailbox domain")
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	defer tx.Rollback()
	now := Now()
	shadow := 0
	if input.ShadowMode {
		shadow = 1
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO team_lifecycle_workflows(
  id,idempotency_key,workspace_id,parent_account_id,child_account_id,state,
  replacement_method,quota_remaining_bps,rotate_threshold_bps,max_attempts,next_attempt_at,
  shadow_mode,created_at,updated_at,mailbox_provider_key,required_email_domain
) VALUES(?,?,?,?,?,'queued',?,-1,?,?,?,?,?,?,?,?)
ON CONFLICT(idempotency_key) DO NOTHING`,
		input.ID, input.IdempotencyKey, input.WorkspaceID, input.ParentAccountID,
		input.ChildAccountID, input.ReplacementMethod, input.RotateThresholdBPS, input.MaxAttempts, now,
		shadow, now, now, input.MailboxProviderKey, input.RequiredEmailDomain)
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	created := affected == 1
	var workflow TeamLifecycleWorkflow
	if created {
		if _, err = tx.ExecContext(ctx, `
INSERT INTO team_lifecycle_events(
  workflow_id,sequence,from_state,to_state,event_type,detail_json,created_at
) VALUES(?,1,'','queued','created','{}',?)`, input.ID, now); err != nil {
			return TeamLifecycleWorkflow{}, false, err
		}
		workflow, err = getTeamLifecycleWorkflowWith(ctx, tx, input.ID, "")
	} else {
		workflow, err = getTeamLifecycleWorkflowWith(ctx, tx, "", input.IdempotencyKey)
	}
	if err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return TeamLifecycleWorkflow{}, false, err
	}
	return workflow, created, nil
}

func getTeamLifecycleWorkflowWith(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, id, idempotencyKey string) (TeamLifecycleWorkflow, error) {
	query := `SELECT ` + teamLifecycleColumns + ` FROM team_lifecycle_workflows WHERE id=?`
	value := id
	if id == "" {
		query = `SELECT ` + teamLifecycleColumns + ` FROM team_lifecycle_workflows WHERE idempotency_key=?`
		value = idempotencyKey
	}
	item, err := scanTeamLifecycleWorkflow(queryer.QueryRowContext(ctx, query, value))
	if errors.Is(err, sql.ErrNoRows) {
		return TeamLifecycleWorkflow{}, ErrTeamLifecycleNotFound
	}
	return item, err
}

func (s *Store) GetTeamLifecycleWorkflow(ctx context.Context, id string) (TeamLifecycleWorkflow, error) {
	return getTeamLifecycleWorkflowWith(ctx, s.rdb, strings.TrimSpace(id), "")
}

func (s *Store) ListTeamLifecycleWorkflows(ctx context.Context, workspaceID, state string, limit int) ([]TeamLifecycleWorkflow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	state = strings.TrimSpace(state)
	if state != "" && !ValidTeamLifecycleState(state) {
		return nil, fmt.Errorf("invalid team lifecycle state %q", state)
	}
	var query strings.Builder
	query.WriteString(`SELECT ` + teamLifecycleColumns + ` FROM team_lifecycle_workflows WHERE 1=1`)
	args := make([]interface{}, 0, 3)
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID != "" {
		query.WriteString(` AND workspace_id=?`)
		args = append(args, workspaceID)
	}
	if state != "" {
		query.WriteString(` AND state=?`)
		args = append(args, state)
	}
	query.WriteString(` ORDER BY updated_at DESC,id LIMIT ?`)
	args = append(args, limit)
	rows, err := s.rdb.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TeamLifecycleWorkflow, 0)
	for rows.Next() {
		item, err := scanTeamLifecycleWorkflow(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ListTeamLifecycleEvents(ctx context.Context, workflowID string, limit int) ([]TeamLifecycleEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.rdb.QueryContext(ctx, `
SELECT id,workflow_id,sequence,from_state,to_state,event_type,detail_json,created_at
FROM team_lifecycle_events WHERE workflow_id=? ORDER BY sequence LIMIT ?`,
		strings.TrimSpace(workflowID), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TeamLifecycleEvent, 0)
	for rows.Next() {
		var item TeamLifecycleEvent
		if err := rows.Scan(
			&item.ID, &item.WorkflowID, &item.Sequence, &item.FromState,
			&item.ToState, &item.EventType, &item.DetailJSON, &item.CreatedAt,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) ClaimTeamLifecycleWorkflow(ctx context.Context, owner string, now, leaseSeconds int64) (TeamLifecycleWorkflow, bool, error) {
	var empty TeamLifecycleWorkflow
	var err error
	if owner, err = normalizeTeamLifecycleIdentifier("lease owner", owner, 256); err != nil {
		return empty, false, err
	}
	if leaseSeconds < 5 {
		leaseSeconds = 30
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, false, err
	}
	defer tx.Rollback()
	candidate, err := scanTeamLifecycleWorkflow(tx.QueryRowContext(ctx, `
SELECT `+teamLifecycleColumns+`
FROM team_lifecycle_workflows w
WHERE w.state NOT IN ('review_required','completed','cancelled')
  AND (w.next_attempt_at=0 OR w.next_attempt_at<=?)
  AND (w.lease_expires_at=0 OR w.lease_expires_at<=?)
  AND EXISTS (
    SELECT 1 FROM team_workspaces t
    WHERE t.id=w.workspace_id AND t.status='active'
  )
ORDER BY w.next_attempt_at,w.created_at,w.id
LIMIT 1`, now, now))
	if errors.Is(err, sql.ErrNoRows) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE team_lifecycle_workflows
SET lease_owner=?,lease_expires_at=?,version=version+1,updated_at=?
WHERE id=? AND version=? AND (lease_expires_at=0 OR lease_expires_at<=?)`,
		owner, now+leaseSeconds, now, candidate.ID, candidate.Version, now)
	if err != nil {
		return empty, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return empty, false, err
	}
	if affected != 1 {
		return empty, false, nil
	}
	claimed, err := getTeamLifecycleWorkflowWith(ctx, tx, candidate.ID, "")
	if err != nil {
		return empty, false, err
	}
	if err = tx.Commit(); err != nil {
		return empty, false, err
	}
	return claimed, true, nil
}

func (s *Store) RenewTeamLifecycleLease(
	ctx context.Context,
	id string,
	expectedVersion int64,
	owner string,
	expiresAt int64,
) error {
	if expiresAt <= Now() {
		return errors.New("team lifecycle lease renewal must extend into the future")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE team_lifecycle_workflows
SET lease_expires_at=?,updated_at=?
WHERE id=? AND version=? AND lease_owner=? AND state NOT IN ('review_required','completed','cancelled')`,
		expiresAt, Now(), strings.TrimSpace(id), expectedVersion, strings.TrimSpace(owner))
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrTeamLifecycleLeaseMismatch
	}
	return nil
}

func (s *Store) TransitionTeamLifecycleWorkflow(
	ctx context.Context,
	id string,
	expectedVersion int64,
	leaseOwner string,
	update TeamLifecycleUpdate,
) (TeamLifecycleWorkflow, error) {
	if !ValidTeamLifecycleState(update.ToState) {
		return TeamLifecycleWorkflow{}, fmt.Errorf("invalid destination state %q", update.ToState)
	}
	detail, err := normalizeTeamLifecycleEventJSON(update.EventDetailJSON)
	if err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	defer tx.Rollback()
	current, err := getTeamLifecycleWorkflowWith(ctx, tx, strings.TrimSpace(id), "")
	if err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	if current.Version != expectedVersion {
		return TeamLifecycleWorkflow{}, ErrTeamLifecycleVersionConflict
	}
	if leaseOwner != "" && current.LeaseOwner != leaseOwner {
		return TeamLifecycleWorkflow{}, ErrTeamLifecycleLeaseMismatch
	}
	if !validTeamLifecycleTransition(current.State, update.ToState) {
		return TeamLifecycleWorkflow{}, fmt.Errorf("invalid team lifecycle transition %s -> %s", current.State, update.ToState)
	}
	fromState := current.State
	if update.ToState == TeamLifecycleRetryWait && !ValidTeamLifecycleState(update.ResumeState) {
		return TeamLifecycleWorkflow{}, errors.New("retry transition requires a valid resume state")
	}
	if update.ResumeState != "" {
		current.ResumeState = update.ResumeState
	}
	if update.ClearResume {
		current.ResumeState = ""
	}
	if update.CredentialPath != "" {
		current.CredentialPath = update.CredentialPath
	}
	if update.MembershipRef != "" {
		current.MembershipRef = update.MembershipRef
	}
	if update.CredentialRef != "" {
		current.CredentialRef = update.CredentialRef
	}
	if update.PhoneChallengeRef != "" {
		current.PhoneChallengeRef = update.PhoneChallengeRef
	}
	if update.ImportedAccountID != "" {
		current.ImportedAccountID = update.ImportedAccountID
	}
	if update.ReplacementJobRef != "" {
		current.ReplacementJobRef = update.ReplacementJobRef
	}
	if update.SetQuota {
		if update.QuotaRemainingBPS < 0 || update.QuotaRemainingBPS > 10000 {
			return TeamLifecycleWorkflow{}, errors.New("quota remaining basis points must be between 0 and 10000")
		}
		current.QuotaRemainingBPS = update.QuotaRemainingBPS
	}
	current.State = update.ToState
	current.Attempt = update.Attempt
	current.NextAttemptAt = update.NextAttemptAt
	current.ErrorClass = strings.TrimSpace(update.ErrorClass)
	if update.ClearError {
		current.ErrorClass = ""
	}
	if update.CompletedAt != 0 {
		current.CompletedAt = update.CompletedAt
	}
	now := Now()
	if current.State == TeamLifecycleCompleted && current.CompletedAt == 0 {
		current.CompletedAt = now
	}
	eventType := strings.TrimSpace(update.EventType)
	if eventType == "" {
		eventType = "transition"
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(sequence),0)+1 FROM team_lifecycle_events WHERE workflow_id=?`,
		current.ID).Scan(&sequence); err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	result, err := tx.ExecContext(ctx, `
UPDATE team_lifecycle_workflows
SET state=?,resume_state=?,credential_path=?,membership_ref=?,credential_ref=?,
    phone_challenge_ref=?,imported_account_id=?,replacement_method=?,replacement_job_ref=?,
    quota_remaining_bps=?,attempt=?,next_attempt_at=?,lease_owner='',
    lease_expires_at=0,error_class=?,version=version+1,updated_at=?,completed_at=?
WHERE id=? AND version=?`,
		current.State, current.ResumeState, current.CredentialPath, current.MembershipRef,
		current.CredentialRef, current.PhoneChallengeRef, current.ImportedAccountID,
		current.ReplacementMethod, current.ReplacementJobRef, current.QuotaRemainingBPS, current.Attempt,
		current.NextAttemptAt, current.ErrorClass, now, current.CompletedAt,
		current.ID, expectedVersion)
	if err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return TeamLifecycleWorkflow{}, err
	} else if affected != 1 {
		return TeamLifecycleWorkflow{}, ErrTeamLifecycleVersionConflict
	}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO team_lifecycle_events(
  workflow_id,sequence,from_state,to_state,event_type,detail_json,created_at
) VALUES(?,?,?,?,?,?,?)`,
		current.ID, sequence, fromState,
		current.State, eventType, detail, now); err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	next, err := getTeamLifecycleWorkflowWith(ctx, tx, current.ID, "")
	if err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	if err = tx.Commit(); err != nil {
		return TeamLifecycleWorkflow{}, err
	}
	return next, nil
}

func (s *Store) CancelTeamLifecycleWorkflow(ctx context.Context, id string) (TeamLifecycleWorkflow, error) {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.GetTeamLifecycleWorkflow(ctx, id)
		if err != nil {
			return TeamLifecycleWorkflow{}, err
		}
		if current.State == TeamLifecycleCancelled {
			return current, nil
		}
		if IsTerminalTeamLifecycleState(current.State) {
			return TeamLifecycleWorkflow{}, fmt.Errorf("workflow in terminal state %q", current.State)
		}
		next, err := s.TransitionTeamLifecycleWorkflow(ctx, id, current.Version, "", TeamLifecycleUpdate{
			ToState:         TeamLifecycleCancelled,
			Attempt:         current.Attempt,
			CompletedAt:     Now(),
			ClearError:      true,
			EventType:       "cancelled",
			EventDetailJSON: `{"source":"operator"}`,
		})
		if errors.Is(err, ErrTeamLifecycleVersionConflict) {
			continue
		}
		return next, err
	}
	return TeamLifecycleWorkflow{}, ErrTeamLifecycleVersionConflict
}

func (s *Store) RetryTeamLifecycleWorkflow(ctx context.Context, id string) (TeamLifecycleWorkflow, error) {
	for attempt := 0; attempt < 3; attempt++ {
		current, err := s.GetTeamLifecycleWorkflow(ctx, id)
		if err != nil {
			return TeamLifecycleWorkflow{}, err
		}
		if current.State != TeamLifecycleReviewRequired {
			return TeamLifecycleWorkflow{}, errors.New("only review-required workflows can be retried")
		}
		resume := current.ResumeState
		if !ValidTeamLifecycleState(resume) || IsTerminalTeamLifecycleState(resume) || resume == TeamLifecycleRetryWait {
			resume = TeamLifecycleQueued
		}
		next, err := s.TransitionTeamLifecycleWorkflow(ctx, id, current.Version, "", TeamLifecycleUpdate{
			ToState:         resume,
			Attempt:         0,
			NextAttemptAt:   Now(),
			ClearError:      true,
			ClearResume:     true,
			EventType:       "operator_retry",
			EventDetailJSON: `{"source":"operator"}`,
		})
		if errors.Is(err, ErrTeamLifecycleVersionConflict) {
			continue
		}
		return next, err
	}
	return TeamLifecycleWorkflow{}, ErrTeamLifecycleVersionConflict
}

func (s *Store) TeamLifecycleStateCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.rdb.QueryContext(ctx, `
SELECT state,COUNT(*) FROM team_lifecycle_workflows GROUP BY state ORDER BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return nil, err
		}
		counts[state] = count
	}
	return counts, rows.Err()
}
