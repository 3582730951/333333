package storage

// Team lifecycle storage is deliberately connector-neutral. Remote membership,
// browser, OAuth, and phone-provider implementations exchange opaque references
// with the workflow engine; credentials continue to live only in the encrypted
// account_auth_tokens path.

const teamManagementSchemaSQL = `
CREATE TABLE IF NOT EXISTS team_workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  parent_account_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  workspace_type TEXT NOT NULL DEFAULT 'fixture',
  max_members INTEGER NOT NULL DEFAULT 10,
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_team_workspaces_connector_ref
  ON team_workspaces(workspace_type, workspace_id);
CREATE INDEX IF NOT EXISTS idx_team_workspaces_parent
  ON team_workspaces(parent_account_id);
CREATE INDEX IF NOT EXISTS idx_team_workspaces_status
  ON team_workspaces(status);

CREATE TABLE IF NOT EXISTS team_members (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  identity_ref TEXT NOT NULL DEFAULT '',
  display_label TEXT NOT NULL DEFAULT '',
  invite_status TEXT NOT NULL DEFAULT 'pending',
  quota_remaining_bps INTEGER NOT NULL DEFAULT -1,
  last_activity_at INTEGER NOT NULL DEFAULT 0,
  last_quota_check_at INTEGER NOT NULL DEFAULT 0,
  added_at INTEGER NOT NULL,
  removed_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_team_members_workspace_account
  ON team_members(workspace_id, account_id);
CREATE INDEX IF NOT EXISTS idx_team_members_workspace_status
  ON team_members(workspace_id, invite_status);
CREATE INDEX IF NOT EXISTS idx_team_members_account
  ON team_members(account_id);

-- This table stores account references only. Secret material is persisted through
-- account_auth_tokens, where the deployment identity key protects it at rest.
CREATE TABLE IF NOT EXISTS child_account_pool (
  id TEXT PRIMARY KEY,
  account_id TEXT NOT NULL UNIQUE,
  identity_ref TEXT NOT NULL DEFAULT '',
  display_label TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'available',
  last_used_at INTEGER NOT NULL DEFAULT 0,
  use_count INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_child_account_pool_status
  ON child_account_pool(status);

CREATE TABLE IF NOT EXISTS member_rotation_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL DEFAULT '',
  removed_account_id TEXT NOT NULL DEFAULT '',
  removed_reason TEXT NOT NULL DEFAULT '',
  added_account_id TEXT NOT NULL DEFAULT '',
  success INTEGER NOT NULL,
  error_class TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id)
);
CREATE INDEX IF NOT EXISTS idx_rotation_log_workspace
  ON member_rotation_log(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_rotation_log_time
  ON member_rotation_log(created_at DESC);

CREATE TABLE IF NOT EXISTS quota_check_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL,
  workflow_id TEXT NOT NULL DEFAULT '',
  account_id TEXT NOT NULL,
  quota_remaining_bps INTEGER NOT NULL,
  source TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id),
  FOREIGN KEY(account_id) REFERENCES accounts(id)
);
CREATE INDEX IF NOT EXISTS idx_quota_check_workspace
  ON quota_check_log(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_quota_check_account
  ON quota_check_log(account_id, created_at DESC);

-- Durable, idempotent workflow state. Every externally visible operation gets a
-- stable operation key derived from workflow id + state. Leases and optimistic
-- versions prevent duplicate workers; retry state survives process restarts.
CREATE TABLE IF NOT EXISTS team_lifecycle_workflows (
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  workspace_id TEXT NOT NULL,
  parent_account_id TEXT NOT NULL,
  child_account_id TEXT NOT NULL,
  state TEXT NOT NULL DEFAULT 'queued',
  resume_state TEXT NOT NULL DEFAULT '',
  credential_path TEXT NOT NULL DEFAULT '',
  membership_ref TEXT NOT NULL DEFAULT '',
  credential_ref TEXT NOT NULL DEFAULT '',
  phone_challenge_ref TEXT NOT NULL DEFAULT '',
  imported_account_id TEXT NOT NULL DEFAULT '',
  replacement_method TEXT NOT NULL DEFAULT '',
  replacement_job_ref TEXT NOT NULL DEFAULT '',
  quota_remaining_bps INTEGER NOT NULL DEFAULT -1,
  rotate_threshold_bps INTEGER NOT NULL DEFAULT 100,
  attempt INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 5,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  lease_owner TEXT NOT NULL DEFAULT '',
  lease_expires_at INTEGER NOT NULL DEFAULT 0,
  error_class TEXT NOT NULL DEFAULT '',
  shadow_mode INTEGER NOT NULL DEFAULT 1,
  version INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  completed_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_team_lifecycle_due
  ON team_lifecycle_workflows(state, next_attempt_at, lease_expires_at);
CREATE INDEX IF NOT EXISTS idx_team_lifecycle_workspace
  ON team_lifecycle_workflows(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_team_lifecycle_child
  ON team_lifecycle_workflows(child_account_id, created_at DESC);

CREATE TABLE IF NOT EXISTS team_lifecycle_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workflow_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  from_state TEXT NOT NULL,
  to_state TEXT NOT NULL,
  event_type TEXT NOT NULL,
  detail_json TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  UNIQUE(workflow_id, sequence),
  FOREIGN KEY(workflow_id) REFERENCES team_lifecycle_workflows(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_team_lifecycle_events_workflow
  ON team_lifecycle_events(workflow_id, sequence);
`

type TeamWorkspace struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	ParentAccountID     string `json:"parent_account_id"`
	WorkspaceRef        string `json:"workspace_ref"`
	ConnectorKind       string `json:"connector_kind"`
	MaxMembers          int    `json:"max_members"`
	Status              string `json:"status"`
	MailboxProviderKey  string `json:"mailbox_provider_key,omitempty"`
	RequiredEmailDomain string `json:"required_email_domain,omitempty"`
	SameDomainRequired  bool   `json:"same_domain_required"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

type TeamMember struct {
	ID                string `json:"id"`
	WorkspaceID       string `json:"workspace_id"`
	AccountID         string `json:"account_id"`
	IdentityRef       string `json:"identity_ref,omitempty"`
	DisplayLabel      string `json:"display_label,omitempty"`
	InviteStatus      string `json:"invite_status"`
	QuotaRemainingBPS int    `json:"quota_remaining_bps"`
	LastActivityAt    int64  `json:"last_activity_at"`
	LastQuotaCheckAt  int64  `json:"last_quota_check_at"`
	AddedAt           int64  `json:"added_at"`
	RemovedAt         int64  `json:"removed_at"`
}

type ChildAccount struct {
	ID           string `json:"id"`
	AccountID    string `json:"account_id"`
	IdentityRef  string `json:"identity_ref,omitempty"`
	DisplayLabel string `json:"display_label,omitempty"`
	Status       string `json:"status"`
	LastUsedAt   int64  `json:"last_used_at"`
	UseCount     int    `json:"use_count"`
	FailureCount int    `json:"failure_count"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

type MemberRotationLog struct {
	ID               int64  `json:"id"`
	WorkspaceID      string `json:"workspace_id"`
	WorkflowID       string `json:"workflow_id"`
	RemovedAccountID string `json:"removed_account_id,omitempty"`
	RemovedReason    string `json:"removed_reason,omitempty"`
	AddedAccountID   string `json:"added_account_id,omitempty"`
	Success          bool   `json:"success"`
	ErrorClass       string `json:"error_class,omitempty"`
	DurationMS       int64  `json:"duration_ms"`
	CreatedAt        int64  `json:"created_at"`
}

type QuotaCheckLog struct {
	ID                int64  `json:"id"`
	WorkspaceID       string `json:"workspace_id"`
	WorkflowID        string `json:"workflow_id"`
	AccountID         string `json:"account_id"`
	QuotaRemainingBPS int    `json:"quota_remaining_bps"`
	Source            string `json:"source"`
	CreatedAt         int64  `json:"created_at"`
}

const (
	TeamWorkspaceStatusActive   = "active"
	TeamWorkspaceStatusPaused   = "paused"
	TeamWorkspaceStatusDisabled = "disabled"

	TeamMemberInviteStatusPending  = "pending"
	TeamMemberInviteStatusActive   = "active"
	TeamMemberInviteStatusRemoved  = "removed"
	TeamMemberInviteStatusRejected = "rejected"

	ChildAccountStatusAvailable      = "available"
	ChildAccountStatusInUse          = "in_use"
	ChildAccountStatusQuotaExhausted = "quota_exhausted"
	ChildAccountStatusBanned         = "banned"
	ChildAccountStatusTokenInvalid   = "token_invalid"

	RotationReasonQuotaThreshold = "quota_threshold"
	RotationReasonBanned         = "banned"
	RotationReasonTokenInvalid   = "token_invalid"
	RotationReasonManual         = "manual"
	RotationReasonAutoRotation   = "auto_rotation"
)

const (
	TeamLifecycleQueued              = "queued"
	TeamLifecycleInviting            = "inviting"
	TeamLifecycleResolvingCredential = "resolving_credential"
	TeamLifecycleCredentialLogin     = "credential_login"
	TeamLifecycleOAuthLogin          = "oauth_login"
	TeamLifecyclePhoneVerification   = "phone_verification"
	TeamLifecycleImporting           = "importing"
	TeamLifecycleActive              = "active"
	TeamLifecycleRemoving            = "removing"
	TeamLifecycleEnqueueReplacement  = "enqueue_replacement"
	TeamLifecycleRetryWait           = "retry_wait"
	TeamLifecycleReviewRequired      = "review_required"
	TeamLifecycleCompleted           = "completed"
	TeamLifecycleCancelled           = "cancelled"
)

func IsTerminalTeamLifecycleState(state string) bool {
	switch state {
	case TeamLifecycleReviewRequired, TeamLifecycleCompleted, TeamLifecycleCancelled:
		return true
	default:
		return false
	}
}

func ValidTeamLifecycleState(state string) bool {
	switch state {
	case TeamLifecycleQueued, TeamLifecycleInviting, TeamLifecycleResolvingCredential,
		TeamLifecycleCredentialLogin, TeamLifecycleOAuthLogin, TeamLifecyclePhoneVerification,
		TeamLifecycleImporting, TeamLifecycleActive, TeamLifecycleRemoving,
		TeamLifecycleEnqueueReplacement, TeamLifecycleRetryWait,
		TeamLifecycleReviewRequired, TeamLifecycleCompleted, TeamLifecycleCancelled:
		return true
	default:
		return false
	}
}

type TeamLifecycleWorkflow struct {
	ID                  string `json:"id"`
	IdempotencyKey      string `json:"idempotency_key"`
	WorkspaceID         string `json:"workspace_id"`
	ParentAccountID     string `json:"parent_account_id"`
	ChildAccountID      string `json:"child_account_id"`
	State               string `json:"state"`
	ResumeState         string `json:"resume_state,omitempty"`
	CredentialPath      string `json:"credential_path,omitempty"`
	MembershipRef       string `json:"membership_ref,omitempty"`
	CredentialRef       string `json:"credential_ref,omitempty"`
	PhoneChallengeRef   string `json:"phone_challenge_ref,omitempty"`
	ImportedAccountID   string `json:"imported_account_id,omitempty"`
	ReplacementMethod   string `json:"replacement_method,omitempty"`
	ReplacementJobRef   string `json:"replacement_job_ref,omitempty"`
	MailboxProviderKey  string `json:"mailbox_provider_key,omitempty"`
	RequiredEmailDomain string `json:"required_email_domain,omitempty"`
	QuotaRemainingBPS   int    `json:"quota_remaining_bps"`
	RotateThresholdBPS  int    `json:"rotate_threshold_bps"`
	Attempt             int    `json:"attempt"`
	MaxAttempts         int    `json:"max_attempts"`
	NextAttemptAt       int64  `json:"next_attempt_at"`
	LeaseOwner          string `json:"-"`
	LeaseExpiresAt      int64  `json:"-"`
	ErrorClass          string `json:"error_class,omitempty"`
	ShadowMode          bool   `json:"shadow_mode"`
	Version             int64  `json:"version"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
	CompletedAt         int64  `json:"completed_at"`
}

type TeamLifecycleEvent struct {
	ID         int64  `json:"id"`
	WorkflowID string `json:"workflow_id"`
	Sequence   int64  `json:"sequence"`
	FromState  string `json:"from_state"`
	ToState    string `json:"to_state"`
	EventType  string `json:"event_type"`
	DetailJSON string `json:"detail_json"`
	CreatedAt  int64  `json:"created_at"`
}

type CreateTeamLifecycleWorkflowInput struct {
	ID                  string
	IdempotencyKey      string
	WorkspaceID         string
	ParentAccountID     string
	ChildAccountID      string
	ReplacementMethod   string
	MailboxProviderKey  string
	RequiredEmailDomain string
	RotateThresholdBPS  int
	MaxAttempts         int
	ShadowMode          bool
}

type TeamLifecycleUpdate struct {
	ToState           string
	ResumeState       string
	CredentialPath    string
	MembershipRef     string
	CredentialRef     string
	PhoneChallengeRef string
	ImportedAccountID string
	ReplacementJobRef string
	QuotaRemainingBPS int
	SetQuota          bool
	Attempt           int
	NextAttemptAt     int64
	ErrorClass        string
	ClearError        bool
	ClearResume       bool
	CompletedAt       int64
	EventType         string
	EventDetailJSON   string
}
