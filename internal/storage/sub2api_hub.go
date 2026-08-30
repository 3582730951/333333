package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const sub2APIHubSchemaSQL = `
CREATE TABLE IF NOT EXISTS sub2api_hub_connections (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  key_prefix TEXT NOT NULL,
  previous_key_hash TEXT NOT NULL DEFAULT '',
  previous_key_expires_at BIGINT NOT NULL DEFAULT 0,
  enabled INTEGER NOT NULL DEFAULT 0,
  provider_allowlist_json TEXT NOT NULL DEFAULT '["codex"]',
  target_group_id TEXT NOT NULL,
  inventory_scope TEXT NOT NULL DEFAULT 'connection_only',
  allowed_proxy_ids_json TEXT NOT NULL DEFAULT '[]',
  allowed_cidrs_json TEXT NOT NULL DEFAULT '[]',
  default_concurrency INTEGER NOT NULL DEFAULT 3,
  default_priority INTEGER NOT NULL DEFAULT 50,
  max_accounts INTEGER NOT NULL DEFAULT 1000,
  max_import_batch INTEGER NOT NULL DEFAULT 100,
  requests_per_minute INTEGER NOT NULL DEFAULT 120,
  max_concurrent_requests INTEGER NOT NULL DEFAULT 4,
  duplicate_policy TEXT NOT NULL DEFAULT 'reject_cross_connection',
  activation_policy TEXT NOT NULL DEFAULT 'verify_then_activate',
  expires_at BIGINT NOT NULL DEFAULT 0,
  last_seen_at BIGINT NOT NULL DEFAULT 0,
  created_by TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sub2api_hub_connections_enabled
  ON sub2api_hub_connections(enabled,expires_at);

CREATE TABLE IF NOT EXISTS sub2api_hub_accounts (
  connection_id TEXT NOT NULL,
  local_account_id TEXT NOT NULL,
  external_identity_hash TEXT NOT NULL,
  credential_fingerprint TEXT NOT NULL,
  source_request_id TEXT NOT NULL,
  state TEXT NOT NULL,
  first_seen_at BIGINT NOT NULL,
  last_seen_at BIGINT NOT NULL,
  PRIMARY KEY (connection_id,local_account_id),
  UNIQUE (connection_id,external_identity_hash),
  FOREIGN KEY(connection_id) REFERENCES sub2api_hub_connections(id) ON DELETE CASCADE,
  FOREIGN KEY(local_account_id) REFERENCES accounts(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sub2api_hub_accounts_credential
  ON sub2api_hub_accounts(credential_fingerprint,connection_id);

CREATE TABLE IF NOT EXISTS sub2api_hub_import_runs (
  id TEXT PRIMARY KEY,
  connection_id TEXT NOT NULL,
  protocol_route TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  request_fingerprint TEXT NOT NULL,
  total INTEGER NOT NULL DEFAULT 0,
  created_count INTEGER NOT NULL DEFAULT 0,
  updated_count INTEGER NOT NULL DEFAULT 0,
  skipped_count INTEGER NOT NULL DEFAULT 0,
  failed_count INTEGER NOT NULL DEFAULT 0,
  response_status INTEGER NOT NULL DEFAULT 0,
  response_redacted_json TEXT NOT NULL DEFAULT '',
  started_at BIGINT NOT NULL,
  finished_at BIGINT NOT NULL DEFAULT 0,
  UNIQUE (connection_id,idempotency_key),
  FOREIGN KEY(connection_id) REFERENCES sub2api_hub_connections(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_sub2api_hub_import_runs_finished
  ON sub2api_hub_import_runs(finished_at,started_at);
`

var (
	ErrSub2APIHubConnectionNotFound = errors.New("Sub2API Hub connection not found")
	ErrSub2APIHubIdempotencyBusy    = errors.New("Sub2API Hub idempotency key is already in progress")
	ErrSub2APIHubIdempotencyReuse   = errors.New("Sub2API Hub idempotency key was reused with a different request")
)

type Sub2APIHubConnection struct {
	ID                    string   `json:"id"`
	Name                  string   `json:"name"`
	KeyHash               string   `json:"-"`
	KeyPrefix             string   `json:"key_prefix"`
	PreviousKeyHash       string   `json:"-"`
	PreviousKeyExpiresAt  int64    `json:"-"`
	Enabled               bool     `json:"enabled"`
	ProviderAllowlist     []string `json:"provider_allowlist"`
	TargetGroupID         string   `json:"target_group_id"`
	InventoryScope        string   `json:"inventory_scope"`
	AllowedProxyIDs       []string `json:"allowed_proxy_ids"`
	AllowedCIDRs          []string `json:"allowed_cidrs"`
	DefaultConcurrency    int      `json:"default_concurrency"`
	DefaultPriority       int      `json:"default_priority"`
	MaxAccounts           int      `json:"max_accounts"`
	MaxImportBatch        int      `json:"max_import_batch"`
	RequestsPerMinute     int      `json:"requests_per_minute"`
	MaxConcurrentRequests int      `json:"max_concurrent_requests"`
	DuplicatePolicy       string   `json:"duplicate_policy"`
	ActivationPolicy      string   `json:"activation_policy"`
	ExpiresAt             int64    `json:"expires_at,omitempty"`
	LastSeenAt            int64    `json:"last_seen_at,omitempty"`
	CreatedBy             string   `json:"created_by"`
	CreatedAt             int64    `json:"created_at"`
	UpdatedAt             int64    `json:"updated_at"`
}

type Sub2APIHubAccount struct {
	ConnectionID          string `json:"connection_id"`
	LocalAccountID        string `json:"local_account_id"`
	ExternalIdentityHash  string `json:"-"`
	CredentialFingerprint string `json:"-"`
	SourceRequestID       string `json:"source_request_id"`
	State                 string `json:"state"`
	FirstSeenAt           int64  `json:"first_seen_at"`
	LastSeenAt            int64  `json:"last_seen_at"`
}

type Sub2APIHubImportRun struct {
	ID                   string
	ConnectionID         string
	ProtocolRoute        string
	IdempotencyKey       string
	RequestFingerprint   string
	Total                int
	CreatedCount         int
	UpdatedCount         int
	SkippedCount         int
	FailedCount          int
	ResponseStatus       int
	ResponseRedactedJSON string
	StartedAt            int64
	FinishedAt           int64
}

func hubJSONString(values []string, fallback string) string {
	if values == nil {
		return fallback
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return fallback
	}
	return string(raw)
}

func scanSub2APIHubConnection(scan func(...interface{}) error) (Sub2APIHubConnection, error) {
	var item Sub2APIHubConnection
	var enabled int
	var providers, proxies, cidrs string
	err := scan(&item.ID, &item.Name, &item.KeyHash, &item.KeyPrefix, &item.PreviousKeyHash, &item.PreviousKeyExpiresAt,
		&enabled, &providers, &item.TargetGroupID, &item.InventoryScope, &proxies, &cidrs,
		&item.DefaultConcurrency, &item.DefaultPriority, &item.MaxAccounts, &item.MaxImportBatch,
		&item.RequestsPerMinute, &item.MaxConcurrentRequests, &item.DuplicatePolicy, &item.ActivationPolicy,
		&item.ExpiresAt, &item.LastSeenAt, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return item, err
	}
	item.Enabled = enabled != 0
	_ = json.Unmarshal([]byte(providers), &item.ProviderAllowlist)
	_ = json.Unmarshal([]byte(proxies), &item.AllowedProxyIDs)
	_ = json.Unmarshal([]byte(cidrs), &item.AllowedCIDRs)
	if item.ProviderAllowlist == nil {
		item.ProviderAllowlist = []string{}
	}
	if item.AllowedProxyIDs == nil {
		item.AllowedProxyIDs = []string{}
	}
	if item.AllowedCIDRs == nil {
		item.AllowedCIDRs = []string{}
	}
	return item, nil
}

const sub2APIHubConnectionColumns = `id,name,key_hash,key_prefix,previous_key_hash,previous_key_expires_at,
enabled,provider_allowlist_json,target_group_id,inventory_scope,allowed_proxy_ids_json,allowed_cidrs_json,
default_concurrency,default_priority,max_accounts,max_import_batch,requests_per_minute,max_concurrent_requests,
duplicate_policy,activation_policy,expires_at,last_seen_at,created_by,created_at,updated_at`

func (s *Store) CreateSub2APIHubConnection(ctx context.Context, item Sub2APIHubConnection) error {
	now := Now()
	if item.CreatedAt == 0 {
		item.CreatedAt = now
	}
	item.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sub2api_hub_connections(`+sub2APIHubConnectionColumns+`)
VALUES(?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,?,?,?,?,?)`,
		item.ID, item.Name, item.KeyHash, item.KeyPrefix, item.PreviousKeyHash, item.PreviousKeyExpiresAt,
		boolInt(item.Enabled), hubJSONString(item.ProviderAllowlist, `["codex"]`), item.TargetGroupID, item.InventoryScope,
		hubJSONString(item.AllowedProxyIDs, `[]`), hubJSONString(item.AllowedCIDRs, `[]`),
		item.DefaultConcurrency, item.DefaultPriority, item.MaxAccounts, item.MaxImportBatch,
		item.RequestsPerMinute, item.MaxConcurrentRequests, item.DuplicatePolicy, item.ActivationPolicy,
		item.ExpiresAt, item.LastSeenAt, item.CreatedBy, item.CreatedAt, item.UpdatedAt)
	return err
}

func (s *Store) UpdateSub2APIHubConnection(ctx context.Context, item Sub2APIHubConnection) error {
	item.UpdatedAt = Now()
	result, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_connections SET name=?,enabled=?,provider_allowlist_json=?,target_group_id=?,inventory_scope=?,
allowed_proxy_ids_json=?,allowed_cidrs_json=?,default_concurrency=?,default_priority=?,max_accounts=?,max_import_batch=?,requests_per_minute=?,
max_concurrent_requests=?,duplicate_policy=?,activation_policy=?,expires_at=?,updated_at=? WHERE id=?`,
		item.Name, boolInt(item.Enabled), hubJSONString(item.ProviderAllowlist, `[]`), item.TargetGroupID, item.InventoryScope,
		hubJSONString(item.AllowedProxyIDs, `[]`), hubJSONString(item.AllowedCIDRs, `[]`), item.DefaultConcurrency,
		item.DefaultPriority, item.MaxAccounts, item.MaxImportBatch, item.RequestsPerMinute, item.MaxConcurrentRequests,
		item.DuplicatePolicy, item.ActivationPolicy, item.ExpiresAt, item.UpdatedAt, item.ID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrSub2APIHubConnectionNotFound
	}
	return err
}

func (s *Store) RotateSub2APIHubConnectionKey(ctx context.Context, id, keyHash, keyPrefix string, previousExpiresAt int64) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_connections SET previous_key_hash=key_hash,previous_key_expires_at=?,key_hash=?,key_prefix=?,updated_at=? WHERE id=?`,
		previousExpiresAt, keyHash, keyPrefix, Now(), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrSub2APIHubConnectionNotFound
	}
	return err
}

func (s *Store) RevokeSub2APIHubConnection(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_connections SET enabled=0,previous_key_hash='',previous_key_expires_at=0,updated_at=? WHERE id=?`, Now(), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrSub2APIHubConnectionNotFound
	}
	return err
}

func (s *Store) GetSub2APIHubConnection(ctx context.Context, id string) (Sub2APIHubConnection, error) {
	item, err := scanSub2APIHubConnection(s.rdb.QueryRowContext(ctx, `SELECT `+sub2APIHubConnectionColumns+` FROM sub2api_hub_connections WHERE id=?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrSub2APIHubConnectionNotFound
	}
	return item, err
}

func (s *Store) FindSub2APIHubConnectionByKeyHash(ctx context.Context, keyHash string, now int64) (Sub2APIHubConnection, error) {
	item, err := scanSub2APIHubConnection(s.rdb.QueryRowContext(ctx, `SELECT `+sub2APIHubConnectionColumns+` FROM sub2api_hub_connections
WHERE key_hash=? OR (previous_key_hash=? AND previous_key_expires_at>=?)`, keyHash, keyHash, now).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrSub2APIHubConnectionNotFound
	}
	return item, err
}

func (s *Store) ListSub2APIHubConnections(ctx context.Context) ([]Sub2APIHubConnection, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+sub2APIHubConnectionColumns+` FROM sub2api_hub_connections ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sub2APIHubConnection{}
	for rows.Next() {
		item, scanErr := scanSub2APIHubConnection(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) TouchSub2APIHubConnection(ctx context.Context, id string, now int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_connections SET last_seen_at=?,updated_at=CASE WHEN updated_at<? THEN ? ELSE updated_at END WHERE id=?`, now, now-60, now, id)
	return err
}

func (s *Store) UpsertSub2APIHubAccount(ctx context.Context, item Sub2APIHubAccount) error {
	now := Now()
	if item.FirstSeenAt == 0 {
		item.FirstSeenAt = now
	}
	item.LastSeenAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO sub2api_hub_accounts(connection_id,local_account_id,external_identity_hash,credential_fingerprint,source_request_id,state,first_seen_at,last_seen_at)
VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(connection_id,local_account_id) DO UPDATE SET external_identity_hash=excluded.external_identity_hash,
credential_fingerprint=excluded.credential_fingerprint,source_request_id=excluded.source_request_id,state=excluded.state,last_seen_at=excluded.last_seen_at`,
		item.ConnectionID, item.LocalAccountID, item.ExternalIdentityHash, item.CredentialFingerprint,
		item.SourceRequestID, item.State, item.FirstSeenAt, item.LastSeenAt)
	return err
}

func (s *Store) SetSub2APIHubAccountState(ctx context.Context, connectionID, accountID, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_accounts SET state=?,last_seen_at=? WHERE connection_id=? AND local_account_id=?`, state, Now(), connectionID, accountID)
	return err
}

func (s *Store) GetSub2APIHubAccount(ctx context.Context, connectionID, accountID string) (Sub2APIHubAccount, bool, error) {
	var item Sub2APIHubAccount
	err := s.rdb.QueryRowContext(ctx, `SELECT connection_id,local_account_id,external_identity_hash,credential_fingerprint,source_request_id,state,first_seen_at,last_seen_at
FROM sub2api_hub_accounts WHERE connection_id=? AND local_account_id=?`, connectionID, accountID).Scan(
		&item.ConnectionID, &item.LocalAccountID, &item.ExternalIdentityHash, &item.CredentialFingerprint,
		&item.SourceRequestID, &item.State, &item.FirstSeenAt, &item.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

func (s *Store) FindSub2APIHubAccountByExternalIdentity(ctx context.Context, connectionID, externalIdentity string) (Sub2APIHubAccount, bool, error) {
	var item Sub2APIHubAccount
	err := s.rdb.QueryRowContext(ctx, `SELECT connection_id,local_account_id,external_identity_hash,credential_fingerprint,source_request_id,state,first_seen_at,last_seen_at
FROM sub2api_hub_accounts WHERE connection_id=? AND external_identity_hash=?`, connectionID, externalIdentity).Scan(
		&item.ConnectionID, &item.LocalAccountID, &item.ExternalIdentityHash, &item.CredentialFingerprint,
		&item.SourceRequestID, &item.State, &item.FirstSeenAt, &item.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return item, false, nil
	}
	return item, err == nil, err
}

func (s *Store) FindSub2APIHubAccountsByCredential(ctx context.Context, credentialFingerprint string) ([]Sub2APIHubAccount, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT connection_id,local_account_id,external_identity_hash,credential_fingerprint,source_request_id,state,first_seen_at,last_seen_at
FROM sub2api_hub_accounts WHERE credential_fingerprint=? ORDER BY first_seen_at`, credentialFingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sub2APIHubAccount{}
	for rows.Next() {
		var item Sub2APIHubAccount
		if err = rows.Scan(&item.ConnectionID, &item.LocalAccountID, &item.ExternalIdentityHash, &item.CredentialFingerprint,
			&item.SourceRequestID, &item.State, &item.FirstSeenAt, &item.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListSub2APIHubAccountIDs(ctx context.Context, connectionID string) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT local_account_id FROM sub2api_hub_accounts WHERE connection_id=? ORDER BY first_seen_at,local_account_id`, connectionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) CountSub2APIHubAccounts(ctx context.Context, connectionID string) (int, error) {
	var count int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM sub2api_hub_accounts WHERE connection_id=?`, connectionID).Scan(&count)
	return count, err
}

// ClaimSub2APIHubImportRun returns claimed=true for a new key. A completed replay
// returns the stored redacted response; an in-flight or mismatched reuse fails.
func (s *Store) ClaimSub2APIHubImportRun(ctx context.Context, run Sub2APIHubImportRun) (bool, Sub2APIHubImportRun, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO sub2api_hub_import_runs(id,connection_id,protocol_route,idempotency_key,request_fingerprint,started_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(connection_id,idempotency_key) DO NOTHING`,
		run.ID, run.ConnectionID, run.ProtocolRoute, run.IdempotencyKey, run.RequestFingerprint, run.StartedAt)
	if err != nil {
		return false, Sub2APIHubImportRun{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, Sub2APIHubImportRun{}, err
	}
	if affected == 1 {
		return true, run, nil
	}
	existing, err := s.GetSub2APIHubImportRun(ctx, run.ConnectionID, run.IdempotencyKey)
	if err != nil {
		return false, existing, err
	}
	if !strings.EqualFold(existing.RequestFingerprint, run.RequestFingerprint) {
		return false, existing, ErrSub2APIHubIdempotencyReuse
	}
	if existing.ResponseStatus == 0 || existing.FinishedAt == 0 {
		return false, existing, ErrSub2APIHubIdempotencyBusy
	}
	return false, existing, nil
}

func (s *Store) GetSub2APIHubImportRun(ctx context.Context, connectionID, idempotencyKey string) (Sub2APIHubImportRun, error) {
	var run Sub2APIHubImportRun
	err := s.rdb.QueryRowContext(ctx, `SELECT id,connection_id,protocol_route,idempotency_key,request_fingerprint,total,created_count,updated_count,
skipped_count,failed_count,response_status,response_redacted_json,started_at,finished_at FROM sub2api_hub_import_runs WHERE connection_id=? AND idempotency_key=?`,
		connectionID, idempotencyKey).Scan(&run.ID, &run.ConnectionID, &run.ProtocolRoute, &run.IdempotencyKey, &run.RequestFingerprint,
		&run.Total, &run.CreatedCount, &run.UpdatedCount, &run.SkippedCount, &run.FailedCount, &run.ResponseStatus,
		&run.ResponseRedactedJSON, &run.StartedAt, &run.FinishedAt)
	return run, err
}

func (s *Store) FinishSub2APIHubImportRun(ctx context.Context, run Sub2APIHubImportRun) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sub2api_hub_import_runs SET total=?,created_count=?,updated_count=?,skipped_count=?,failed_count=?,
response_status=?,response_redacted_json=?,finished_at=? WHERE id=?`, run.Total, run.CreatedCount, run.UpdatedCount, run.SkippedCount,
		run.FailedCount, run.ResponseStatus, run.ResponseRedactedJSON, run.FinishedAt, run.ID)
	if err != nil {
		return err
	}
	cutoff := Now() - 30*24*60*60
	_, _ = s.db.ExecContext(ctx, `DELETE FROM sub2api_hub_import_runs WHERE id IN (SELECT id FROM sub2api_hub_import_runs
WHERE finished_at>0 AND finished_at<? ORDER BY finished_at LIMIT 500)`, cutoff)
	return nil
}
