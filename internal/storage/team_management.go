package storage

// ChatGPT Team 空间管理相关的数据结构和SQL schema

const teamManagementSchemaSQL = `
-- Team空间管理表
CREATE TABLE IF NOT EXISTS team_workspaces (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  parent_account_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  workspace_type TEXT NOT NULL DEFAULT 'chatgpt_team',
  max_members INTEGER NOT NULL DEFAULT 10,
  status TEXT NOT NULL DEFAULT 'active',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_team_workspaces_parent ON team_workspaces(parent_account_id);
CREATE INDEX IF NOT EXISTS idx_team_workspaces_status ON team_workspaces(status);

-- Team成员管理表
CREATE TABLE IF NOT EXISTS team_members (
  id TEXT PRIMARY KEY,
  workspace_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  email TEXT NOT NULL,
  invite_status TEXT NOT NULL DEFAULT 'pending',
  codex_quota_used INTEGER NOT NULL DEFAULT 0,
  codex_quota_limit INTEGER NOT NULL DEFAULT 0,
  last_activity_at INTEGER,
  last_quota_check_at INTEGER,
  added_at INTEGER NOT NULL,
  removed_at INTEGER,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id) ON DELETE CASCADE,
  FOREIGN KEY(account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_team_members_workspace ON team_members(workspace_id, invite_status);
CREATE INDEX IF NOT EXISTS idx_team_members_account ON team_members(account_id);

-- 子账号池表
CREATE TABLE IF NOT EXISTS child_account_pool (
  id TEXT PRIMARY KEY,
  email TEXT NOT NULL UNIQUE,
  encrypted_password TEXT,
  oauth_token TEXT,
  oauth_refresh_token TEXT,
  token_expires_at INTEGER,
  status TEXT NOT NULL DEFAULT 'available',
  sms_receive_method TEXT,
  sms_api_config TEXT,
  phone_number TEXT,
  last_used_at INTEGER,
  use_count INTEGER NOT NULL DEFAULT 0,
  ban_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_child_account_pool_status ON child_account_pool(status);
CREATE INDEX IF NOT EXISTS idx_child_account_pool_email ON child_account_pool(email);

-- 成员轮换历史表
CREATE TABLE IF NOT EXISTS member_rotation_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL,
  removed_account_id TEXT,
  removed_email TEXT,
  removed_reason TEXT,
  added_account_id TEXT,
  added_email TEXT,
  success INTEGER NOT NULL,
  error_message TEXT,
  duration_ms INTEGER,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id)
);
CREATE INDEX IF NOT EXISTS idx_rotation_log_workspace ON member_rotation_log(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_rotation_log_time ON member_rotation_log(created_at DESC);

-- 额度监控历史表
CREATE TABLE IF NOT EXISTS quota_check_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  workspace_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  quota_used INTEGER NOT NULL,
  quota_limit INTEGER NOT NULL,
  quota_remaining INTEGER NOT NULL,
  check_method TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  FOREIGN KEY(workspace_id) REFERENCES team_workspaces(id),
  FOREIGN KEY(account_id) REFERENCES accounts(id)
);
CREATE INDEX IF NOT EXISTS idx_quota_check_workspace ON quota_check_log(workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_quota_check_account ON quota_check_log(account_id, created_at DESC);
`

// TeamWorkspace 表示一个 ChatGPT Team 工作区
type TeamWorkspace struct {
	ID              string
	Name            string
	ParentAccountID string
	WorkspaceID     string
	WorkspaceType   string
	MaxMembers      int
	Status          string
	CreatedAt       int64
	UpdatedAt       int64
}

// TeamMember 表示 Team 中的一个成员
type TeamMember struct {
	ID                string
	WorkspaceID       string
	AccountID         string
	Email             string
	InviteStatus      string
	CodexQuotaUsed    int64
	CodexQuotaLimit   int64
	LastActivityAt    int64
	LastQuotaCheckAt  int64
	AddedAt           int64
	RemovedAt         int64
}

// ChildAccount 表示子账号池中的账号
type ChildAccount struct {
	ID                 string
	Email              string
	EncryptedPassword  string
	OAuthToken         string
	OAuthRefreshToken  string
	TokenExpiresAt     int64
	Status             string
	SMSReceiveMethod   string
	SMSAPIConfig       string
	PhoneNumber        string
	LastUsedAt         int64
	UseCount           int
	BanCount           int
	CreatedAt          int64
	UpdatedAt          int64
}

// MemberRotationLog 表示成员轮换历史记录
type MemberRotationLog struct {
	ID                int64
	WorkspaceID       string
	RemovedAccountID  string
	RemovedEmail      string
	RemovedReason     string
	AddedAccountID    string
	AddedEmail        string
	Success           bool
	ErrorMessage      string
	DurationMs        int64
	CreatedAt         int64
}

// QuotaCheckLog 表示额度检查历史记录
type QuotaCheckLog struct {
	ID              int64
	WorkspaceID     string
	AccountID       string
	QuotaUsed       int64
	QuotaLimit      int64
	QuotaRemaining  int64
	CheckMethod     string
	CreatedAt       int64
}

// 状态常量
const (
	// TeamWorkspace 状态
	TeamWorkspaceStatusActive   = "active"
	TeamWorkspaceStatusPaused   = "paused"
	TeamWorkspaceStatusDisabled = "disabled"

	// TeamMember 邀请状态
	TeamMemberInviteStatusPending  = "pending"
	TeamMemberInviteStatusActive   = "active"
	TeamMemberInviteStatusRemoved  = "removed"
	TeamMemberInviteStatusRejected = "rejected"

	// ChildAccount 状态
	ChildAccountStatusAvailable      = "available"
	ChildAccountStatusInUse          = "in_use"
	ChildAccountStatusQuotaExhausted = "quota_exhausted"
	ChildAccountStatusBanned         = "banned"
	ChildAccountStatusTokenInvalid   = "token_invalid"

	// 轮换原因
	RotationReasonQuotaExhausted = "quota_exhausted"
	RotationReasonBanned         = "banned"
	RotationReasonTokenInvalid   = "token_invalid"
	RotationReasonManual         = "manual"
	RotationReasonAutoRotation   = "auto_rotation"
)
