package storage

// This file owns the v2 long-task continuity store.  It deliberately keeps
// identifiers usable for lookup only as SHA-256 hashes: thread ids, response ids,
// Claude sessions and turn-state values are not diagnostic data and must never be
// copied into an operator-facing table.  Payload-bearing columns use Store's existing
// token encryption in exactly the same way as context_journal.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"codex-account-pool/internal/secretbox"
)

const goalContinuitySchemaSQL = `
CREATE TABLE IF NOT EXISTS goal_session(
 id TEXT PRIMARY KEY,
 protocol TEXT NOT NULL,
 parent_goal_id TEXT NOT NULL DEFAULT '',
 branch_hash TEXT NOT NULL DEFAULT '',
 downstream_key_hash TEXT NOT NULL DEFAULT '',
 workspace_hash TEXT NOT NULL DEFAULT '',
 initial_goal_hash TEXT NOT NULL DEFAULT '',
 last_response_hash TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'ready',
 current_checkpoint_id TEXT NOT NULL DEFAULT '',
 encrypted_working_state TEXT NOT NULL DEFAULT '',
	storage_bytes INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_goal_session_expiry ON goal_session(expires_at, state);
CREATE INDEX IF NOT EXISTS idx_goal_session_fallback ON goal_session(downstream_key_hash, workspace_hash, initial_goal_hash, expires_at);
CREATE TABLE IF NOT EXISTS goal_alias(
 alias_hash TEXT PRIMARY KEY,
 alias_type TEXT NOT NULL,
 goal_id TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_goal_alias_goal ON goal_alias(goal_id);
CREATE TABLE IF NOT EXISTS goal_checkpoint(
 id TEXT PRIMARY KEY,
 goal_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 through_segment_sequence INTEGER NOT NULL DEFAULT 0,
 payload_hash TEXT NOT NULL,
 payload_bytes INTEGER NOT NULL,
 encrypted_payload TEXT NOT NULL,
	format_version INTEGER NOT NULL DEFAULT 2,
 created_at INTEGER NOT NULL,
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE,
 UNIQUE(goal_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_goal_checkpoint_goal ON goal_checkpoint(goal_id, sequence DESC);
CREATE TABLE IF NOT EXISTS goal_segment(
 id TEXT PRIMARY KEY,
 goal_id TEXT NOT NULL,
 sequence INTEGER NOT NULL,
 payload_hash TEXT NOT NULL,
 payload_bytes INTEGER NOT NULL,
 encrypted_payload TEXT NOT NULL,
	format_version INTEGER NOT NULL DEFAULT 2,
 state TEXT NOT NULL DEFAULT 'committed',
 created_at INTEGER NOT NULL,
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE,
 UNIQUE(goal_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_goal_segment_goal ON goal_segment(goal_id, sequence);
CREATE TABLE IF NOT EXISTS goal_payload_chunk(
 goal_id TEXT NOT NULL,
 payload_kind TEXT NOT NULL,
 segment_sequence INTEGER NOT NULL DEFAULT 0,
 chunk_index INTEGER NOT NULL,
 payload_hash TEXT NOT NULL,
 payload_bytes INTEGER NOT NULL,
 encrypted_payload TEXT NOT NULL,
 created_at INTEGER NOT NULL,
 PRIMARY KEY(goal_id, payload_kind, segment_sequence, chunk_index),
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_goal_payload_chunk_history ON goal_payload_chunk(goal_id, payload_kind, segment_sequence, chunk_index);
CREATE TABLE IF NOT EXISTS goal_run(
 id TEXT PRIMARY KEY,
 goal_id TEXT NOT NULL,
 state TEXT NOT NULL,
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_expires_at INTEGER NOT NULL DEFAULT 0,
 heartbeat_at INTEGER NOT NULL DEFAULT 0,
 checkpoint_id TEXT NOT NULL DEFAULT '',
 failure_code TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_goal_run_active ON goal_run(goal_id, state, lease_expires_at);
`

var (
	ErrGoalNotFound             = errors.New("goal not found")
	ErrGoalAmbiguous            = errors.New("goal resume is ambiguous")
	ErrGoalInProgress           = errors.New("goal is already in progress")
	ErrGoalStorageBudget        = errors.New("goal storage budget exhausted")
	ErrGoalActiveCannotBePurged = errors.New("active goal cannot be purged")
)

// GoalStorageBudgetError carries the exact, aggregate-only headroom required by a
// rejected commit.  Callers can run one bounded cold-goal reclamation pass and retry
// the same atomic commit without guessing from plaintext payload sizes.  GoalID is
// empty for a new goal and identifies only the already-durable goal for an append.
type GoalStorageBudgetError struct {
	UsedBytes       int64
	ProjectedBytes  int64
	LimitBytes      int64
	AdditionalBytes int64
	GoalID          string
}

func (e *GoalStorageBudgetError) Error() string {
	if e == nil {
		return ErrGoalStorageBudget.Error()
	}
	return fmt.Sprintf("%s: used=%d projected=%d limit=%d additional=%d",
		ErrGoalStorageBudget, e.UsedBytes, e.ProjectedBytes, e.LimitBytes, e.AdditionalBytes)
}

func (e *GoalStorageBudgetError) Unwrap() error { return ErrGoalStorageBudget }

// ReclaimTarget is the maximum aggregate storage that leaves enough space for the
// rejected commit. A negative result means the turn itself is larger than its
// configured budget and reclamation would be futile.
func (e *GoalStorageBudgetError) ReclaimTarget() int64 {
	if e == nil {
		return -1
	}
	return e.LimitBytes - e.AdditionalBytes
}

// GoalAlias is intentionally accepted in plaintext at the storage boundary but is
// immediately hashed.  Callers must not log Value.
type GoalAlias struct {
	Type      string
	Value     string
	Namespace string
	// Family scopes the alias to a wire-history family (see GoalProtocolFamily).
	// A Claude Code session that also ran through the Anthropic→Responses bridge
	// emits the exact same client identifiers for both families; without this
	// dimension the two goals would fight over one globally unique alias_hash and
	// each turn would rebind the alias away from the other family's history.
	// Empty preserves the deployed v2 hash so existing rows stay resolvable.
	Family string
}

type GoalSession struct {
	ID                string `json:"id"`
	Protocol          string `json:"protocol"`
	ParentGoalID      string `json:"parent_goal_id,omitempty"`
	BranchHash        string `json:"branch_hash,omitempty"`
	DownstreamKeyHash string `json:"downstream_key_hash,omitempty"`
	WorkspaceHash     string `json:"workspace_hash,omitempty"`
	InitialGoalHash   string `json:"initial_goal_hash,omitempty"`
	LastResponseHash  string `json:"last_response_hash,omitempty"`
	State             string `json:"state"`
	CurrentCheckpoint string `json:"current_checkpoint_id,omitempty"`
	WorkingState      string `json:"-"`
	StorageBytes      int64  `json:"storage_bytes"`
	ExpiresAt         int64  `json:"expires_at"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

type GoalCheckpoint struct {
	ID                     string `json:"id"`
	GoalID                 string `json:"goal_id"`
	Sequence               int64  `json:"sequence"`
	ThroughSegmentSequence int64  `json:"through_segment_sequence"`
	PayloadHash            string `json:"payload_hash"`
	PayloadBytes           int64  `json:"payload_bytes"`
	Payload                string `json:"-"`
	FormatVersion          int64  `json:"format_version"`
	CreatedAt              int64  `json:"created_at"`
}

type GoalSegment struct {
	ID            string `json:"id"`
	GoalID        string `json:"goal_id"`
	Sequence      int64  `json:"sequence"`
	PayloadHash   string `json:"payload_hash"`
	PayloadBytes  int64  `json:"payload_bytes"`
	Payload       string `json:"-"`
	FormatVersion int64  `json:"format_version"`
	State         string `json:"state"`
	CreatedAt     int64  `json:"created_at"`
}

const (
	goalPayloadFormatV2     = int64(2)
	goalPayloadChunkSize    = 64 << 10
	goalChunkCheckpoint     = "checkpoint"
	goalChunkSegment        = "segment"
	goalReclaimRowsPerStep  = 64
	goalReclaimBytesPerStep = 8 << 20
	goalReclaimingState     = "reclaiming"
	// A tool-heavy goal spends most of its idle lifetime in awaiting_tool_result.
	// Treating that state as permanently unreclaimable lets a small number of
	// abandoned sessions pin the entire global budget forever. Fresh waits retain a
	// grace period, while an active run lease and the foreground protectedGoalID
	// remain hard fences regardless of age.
	goalAwaitingToolReclaimGrace = 30 * time.Minute
)

func (s *Store) estimateGoalChunkStorage(payload string) int64 {
	var stored int64
	for offset := 0; offset < len(payload); {
		end := offset + goalPayloadChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		durable := compressContextPayload(payload[offset:end])
		plainBytes := len(durable)
		if len(s.tokenKey) == 32 {
			stored += int64(len(secretbox.Prefix) + base64.StdEncoding.EncodedLen(plainBytes+12+16))
		} else {
			stored += int64(plainBytes)
		}
		offset = end
	}
	return stored
}

func (s *Store) insertGoalChunks(ctx context.Context, tx *sql.Tx, goalID, kind string, segmentSequence, createdAt int64, payload string) (int64, error) {
	var stored int64
	for offset, index := 0, 0; offset < len(payload); index++ {
		end := offset + goalPayloadChunkSize
		if end > len(payload) {
			end = len(payload)
		}
		part := payload[offset:end]
		encrypted := s.sealToken(compressContextPayload(part))
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_payload_chunk(goal_id,payload_kind,segment_sequence,chunk_index,payload_hash,payload_bytes,encrypted_payload,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			goalID, kind, segmentSequence, index, hashGoalPayload(part), len(part), encrypted, createdAt); err != nil {
			return stored, err
		}
		stored += int64(len(encrypted))
		offset = end
	}
	return stored, nil
}

func (s *Store) readGoalChunks(ctx context.Context, goalID, kind string, segmentSequence int64) (string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT p.payload_hash,p.payload_bytes,p.encrypted_payload
FROM goal_payload_chunk p JOIN goal_session s ON s.id=p.goal_id
WHERE p.goal_id=? AND p.payload_kind=? AND p.segment_sequence=? AND s.state<>'reclaiming'
ORDER BY p.chunk_index`, goalID, kind, segmentSequence)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var payload bytes.Buffer
	for rows.Next() {
		var hash, encrypted string
		var plainBytes int64
		if err = rows.Scan(&hash, &plainBytes, &encrypted); err != nil {
			return "", err
		}
		if plainBytes < 0 || plainBytes > goalPayloadChunkSize {
			return "", fmt.Errorf("goal payload chunk declares invalid plaintext length %d", plainBytes)
		}
		decoded, decodeErr := s.openContextPayload(encrypted, goalPayloadChunkSize)
		if decodeErr != nil {
			return "", fmt.Errorf("decode goal payload chunk: %w", decodeErr)
		}
		if int64(len(decoded)) != plainBytes || hashGoalPayload(decoded) != hash {
			return "", errors.New("goal payload chunk failed plaintext length/hash verification")
		}
		if int64(payload.Len())+plainBytes > maxStoredContextPayloadBytes {
			return "", fmt.Errorf("goal payload exceeds %d-byte reconstruction limit", maxStoredContextPayloadBytes)
		}
		payload.WriteString(decoded)
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if payload.Len() == 0 {
		return "", ErrGoalNotFound
	}
	return payload.String(), nil
}

type GoalRun struct {
	ID             string `json:"id"`
	GoalID         string `json:"goal_id"`
	State          string `json:"state"`
	LeaseOwner     string `json:"-"`
	LeaseExpiresAt int64  `json:"lease_expires_at"`
	HeartbeatAt    int64  `json:"heartbeat_at"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	FailureCode    string `json:"failure_code,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

// GoalTurn is the atomic successful-terminal write.  CheckpointPayload is the stable
// request envelope without turn history; SegmentPayload is exactly one completed turn
// (the current input and upstream output), so the common path never copies history.
type GoalTurn struct {
	Protocol          string
	ParentGoalID      string
	BranchHash        string
	DownstreamKeyHash string
	WorkspaceHash     string
	InitialGoalHash   string
	ResponseID        string
	AliasNamespace    string
	Aliases           []GoalAlias
	// ResolutionAliasSets are ordered strongest-to-weakest identity candidates.
	// CommitGoalTurn resolves the first set that actually has a durable match;
	// critically, it never unions weak client/session aliases with a concrete
	// thread identity.  This prevents two independent CLI conversations that
	// happen to share a process/session marker from being merged into one goal.
	// Existing callers that leave this empty retain the legacy single-set behavior.
	ResolutionAliasSets [][]GoalAlias
	CheckpointPayload   string
	SegmentPayload      string
	// ReplaceHistory is set only when the successful terminal establishes an
	// authoritative compaction/edit boundary. The commit atomically swaps the
	// prior checkpoint/segments for this self-contained turn, allowing official
	// client compaction to reclaim storage even when the global budget is full.
	ReplaceHistory bool
	// ExpectedCurrentCheckpoint/ExpectedLastSegmentSequence fence an exact replay
	// consolidation. Commit checks both after taking the parent-row write lock, so a
	// snapshot built before a concurrent append can never erase that newer segment.
	ExpectedCurrentCheckpoint   string
	ExpectedLastSegmentSequence int64
	WorkingState                string
	AwaitingTool                bool
	ExpiresAt                   int64
	StorageMaxBytes             int64
	// StorageTargetBytes is the steady-state watermark used only when admitting a
	// new goal. Existing goals may consume the reserved gap up to StorageMaxBytes,
	// so a cold/new conversation cannot strand the next checkpoint of a live one.
	StorageTargetBytes int64
	CompressionStages  int
}

type GoalResolution struct {
	Session    GoalSession
	Candidates []string
}

type GoalDetail struct {
	Session         GoalSession `json:"session"`
	CheckpointCount int64       `json:"checkpoint_count"`
	SegmentCount    int64       `json:"segment_count"`
	PayloadBytes    int64       `json:"payload_bytes"`
	LatestRun       *GoalRun    `json:"latest_run,omitempty"`
}

// GoalMetrics contains only aggregate lifecycle counters. It is safe for the admin
// surface and diagnostics bundle because it never reads encrypted checkpoint bodies,
// aliases, tool results, or summary text.
type GoalMetrics struct {
	Sessions                  int64 `json:"sessions"`
	StorageBytes              int64 `json:"storage_bytes"`
	ResumeRecovered           int64 `json:"resume_recovered"`
	ResumeAmbiguous           int64 `json:"resume_ambiguous"`
	StreamTerminalSynthesized int64 `json:"stream_terminal_synthesized"`
	PersistenceDegraded       int64 `json:"persistence_degraded"`
	HistoryReplaced           int64 `json:"history_replaced"`
	CompactionCompleted       int64 `json:"compaction_completed"`
}

const goalContinuityV2MigrationMarker = "goal_continuity_v2_storage_accounted"
const goalPolicyDefaultsMigrationMarker = "goal_policy_defaults_v3_1gib_no_legacy_dual_write"

type GoalPolicyDefaultsMigration struct {
	AlreadyCompleted        bool
	StorageDefaultUpgraded  bool
	LegacyDualWriteDisabled bool
}

// MigrateGoalPolicyDefaults upgrades only inherited bootstrap defaults. A setting
// row is an explicit operator choice and always wins, including a deliberate
// 256-MiB cap or an explicitly enabled legacy dual-write. The marker, overrides,
// and aggregate-only audit record commit atomically so an interrupted upgrade can
// be retried without a partially effective policy.
func (s *Store) MigrateGoalPolicyDefaults(ctx context.Context, bootstrapStorageMB, legacyStorageMB, upgradedStorageMB int, bootstrapLegacyDualWrite bool) (GoalPolicyDefaultsMigration, error) {
	var result GoalPolicyDefaultsMigration
	if upgradedStorageMB <= legacyStorageMB || legacyStorageMB <= 0 {
		return result, errors.New("invalid goal policy default migration")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var completed int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, goalPolicyDefaultsMigrationMarker).Scan(&completed); err != nil {
		return result, err
	}
	if completed > 0 {
		result.AlreadyCompleted = true
		return result, tx.Commit()
	}
	now := Now()
	if bootstrapStorageMB == legacyStorageMB {
		var explicit int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key='goal_storage_max_mb' AND trim(value)<>''`).Scan(&explicit); err != nil {
			return result, err
		}
		if explicit == 0 {
			if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('goal_storage_max_mb',?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at WHERE trim(settings.value)=''`, fmt.Sprintf("%d", upgradedStorageMB), now); err != nil {
				return result, err
			}
			result.StorageDefaultUpgraded = true
		}
	}
	if bootstrapLegacyDualWrite {
		var explicit int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key='goal_legacy_journal_dual_write' AND trim(value)<>''`).Scan(&explicit); err != nil {
			return result, err
		}
		if explicit == 0 {
			if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES('goal_legacy_journal_dual_write','false',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at WHERE trim(settings.value)=''`, now); err != nil {
				return result, err
			}
			result.LegacyDualWriteDisabled = true
		}
	}
	markerValue := fmt.Sprintf("storage_upgraded=%t,legacy_dual_write_disabled=%t", result.StorageDefaultUpgraded, result.LegacyDualWriteDisabled)
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, goalPolicyDefaultsMigrationMarker, markerValue, now); err != nil {
		return result, err
	}
	if result.StorageDefaultUpgraded || result.LegacyDualWriteDisabled {
		if _, err = tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
			"goal_policy_defaults_migrated", "completed", "legacy_bootstrap_defaults", markerValue, now); err != nil {
			return result, err
		}
	}
	return result, tx.Commit()
}

func (s *Store) migrateGoalContinuityV2(ctx context.Context) error {
	var completed int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings WHERE key=?`, goalContinuityV2MigrationMarker).Scan(&completed); err != nil {
		return err
	}
	if completed > 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS goal_payload_chunk(
goal_id TEXT NOT NULL,payload_kind TEXT NOT NULL,segment_sequence INTEGER NOT NULL DEFAULT 0,chunk_index INTEGER NOT NULL,
payload_hash TEXT NOT NULL,payload_bytes INTEGER NOT NULL,encrypted_payload TEXT NOT NULL,created_at INTEGER NOT NULL,
PRIMARY KEY(goal_id,payload_kind,segment_sequence,chunk_index),FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_goal_payload_chunk_history ON goal_payload_chunk(goal_id,payload_kind,segment_sequence,chunk_index)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE goal_checkpoint SET format_version=1 WHERE format_version<=0`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE goal_segment SET format_version=1 WHERE format_version<=0`); err != nil {
		return err
	}

	// Account one goal per transaction. Older builds performed this whole-table
	// rewrite before opening the listener, so a large continuity store could make
	// an otherwise healthy staged worker miss the installer readiness deadline.
	rows, err := s.rdb.QueryContext(ctx, `SELECT id FROM goal_session ORDER BY id`)
	if err != nil {
		return err
	}
	goalIDs := make([]string, 0, 128)
	for rows.Next() {
		var goalID string
		if err = rows.Scan(&goalID); err != nil {
			_ = rows.Close()
			return err
		}
		goalIDs = append(goalIDs, goalID)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, goalID := range goalIDs {
		if err = ctx.Err(); err != nil {
			return err
		}
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		_, txErr = tx.ExecContext(ctx, `UPDATE goal_session SET storage_bytes=
COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_checkpoint WHERE goal_id=goal_session.id),0)+
COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_segment WHERE goal_id=goal_session.id),0)+
			COALESCE((SELECT SUM(LENGTH(encrypted_payload)) FROM goal_payload_chunk WHERE goal_id=goal_session.id),0)
WHERE id=?`, goalID)
		if txErr == nil {
			txErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if txErr != nil {
			return txErr
		}
	}
	now := Now()
	_, err = s.db.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO NOTHING`, goalContinuityV2MigrationMarker, "1", now)
	return err
}

func hashGoalValue(kind, value string) string {
	s := sha256.Sum256([]byte(strings.TrimSpace(kind) + "\x00" + strings.TrimSpace(value)))
	return hex.EncodeToString(s[:])
}

func hashGoalAlias(alias GoalAlias) string {
	namespace := strings.TrimSpace(alias.Namespace)
	if namespace == "" {
		// Preserve the deployed hash format for clients that do not yet emit an
		// installation namespace and for one-time exact state-alias migration.
		return hashGoalValue(alias.Type, alias.Value)
	}
	if family := strings.TrimSpace(strings.ToLower(alias.Family)); family != "" && family != GoalFamilyResponses {
		// Only the non-Responses families move to v3. Keeping Responses on the
		// deployed v2 digest means every already-persisted Codex goal — the
		// overwhelming majority of stored aliases — resumes without migration,
		// while a Messages-family turn can no longer steal the same alias row.
		s := sha256.Sum256([]byte("goal-alias-v3\x00" + family + "\x00" + namespace + "\x00" +
			strings.TrimSpace(alias.Type) + "\x00" + strings.TrimSpace(alias.Value)))
		return hex.EncodeToString(s[:])
	}
	s := sha256.Sum256([]byte("goal-alias-v2\x00" + namespace + "\x00" +
		strings.TrimSpace(alias.Type) + "\x00" + strings.TrimSpace(alias.Value)))
	return hex.EncodeToString(s[:])
}

func hashGoalPayload(payload string) string {
	s := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(s[:])
}

func newGoalID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is exceptionally rare.  Keep the id collision-resistant
		// enough for a local SQLite primary key and let the insert report a collision.
		return fmt.Sprintf("%s_%x", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func normalizedGoalAliases(in []GoalAlias) []GoalAlias {
	seen := make(map[string]bool, len(in))
	out := make([]GoalAlias, 0, len(in))
	for _, alias := range in {
		alias.Type = strings.TrimSpace(strings.ToLower(alias.Type))
		alias.Value = strings.TrimSpace(alias.Value)
		alias.Namespace = strings.TrimSpace(alias.Namespace)
		alias.Family = strings.TrimSpace(strings.ToLower(alias.Family))
		if alias.Type == "" || alias.Value == "" {
			continue
		}
		h := hashGoalAlias(alias)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, alias)
	}
	return out
}

func (s *Store) resolveGoalAliases(ctx context.Context, q sqlQueryer, aliases []GoalAlias, family string) (GoalResolution, error) {
	aliases = normalizedGoalAliases(aliases)
	if len(aliases) == 0 {
		return GoalResolution{}, ErrGoalNotFound
	}
	args := make([]interface{}, 0, len(aliases)+1)
	marks := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		marks = append(marks, "?")
		args = append(args, hashGoalAlias(alias))
	}
	args = append(args, Now())
	query := `SELECT DISTINCT s.id, s.protocol, s.parent_goal_id, s.branch_hash,
 s.downstream_key_hash, s.workspace_hash, s.initial_goal_hash, s.last_response_hash,
	 s.state, s.current_checkpoint_id, s.encrypted_working_state, s.storage_bytes, s.expires_at, s.created_at, s.updated_at
 FROM goal_alias a JOIN goal_session s ON s.id=a.goal_id
	 WHERE a.alias_hash IN (` + strings.Join(marks, ",") + `) AND s.expires_at>? AND s.state<>'reclaiming'`
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return GoalResolution{}, err
	}
	defer rows.Close()
	var out []GoalSession
	for rows.Next() {
		var item GoalSession
		var encrypted string
		if err := rows.Scan(&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash,
			&item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash,
			&item.State, &item.CurrentCheckpoint, &encrypted, &item.StorageBytes, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return GoalResolution{}, err
		}
		item.WorkingState, err = s.openContextPayload(encrypted, maxStoredContextPayloadBytes)
		if err != nil {
			return GoalResolution{}, fmt.Errorf("decode goal working state: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return GoalResolution{}, err
	}
	// A foreign-family match can only produce a malformed replay body or a rejected
	// commit, so it is not a candidate at all. Filtering here rather than in SQL keeps
	// the protocol→family mapping in one place and stays engine independent.
	if family = strings.TrimSpace(strings.ToLower(family)); family != "" {
		kept := out[:0]
		for _, item := range out {
			if GoalProtocolFamily(item.Protocol) == family {
				kept = append(kept, item)
			}
		}
		out = kept
	}
	if len(out) == 0 {
		return GoalResolution{}, ErrGoalNotFound
	}
	if len(out) > 1 {
		ids := make([]string, 0, len(out))
		for _, item := range out {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		return GoalResolution{Candidates: ids}, ErrGoalAmbiguous
	}
	return GoalResolution{Session: out[0]}, nil
}

// sqlQueryer lets alias resolution use either the normal read pool or the commit
// transaction without duplicating the collision-sensitive query.
type sqlQueryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func (s *Store) ResolveGoalAliases(ctx context.Context, aliases []GoalAlias) (GoalResolution, error) {
	return s.resolveGoalAliases(ctx, s.rdb, aliases, "")
}

// ResolveGoalAliasesForFamily restricts resolution to goals whose stored protocol
// belongs to family. Callers that are about to replay or append history must use it:
// a match from another family is never resumable.
func (s *Store) ResolveGoalAliasesForFamily(ctx context.Context, aliases []GoalAlias, family string) (GoalResolution, error) {
	return s.resolveGoalAliases(ctx, s.rdb, aliases, family)
}

// ResolveGoalAliasSets applies the caller's documented identity precedence.  A
// complete set is deliberately resolved together: conflicting concrete aliases in
// the same precedence level remain an ambiguity, while a missing stronger alias
// may fall through to a persisted response/turn alias.  It is used by the relay
// before recovery and by CommitGoalTurn inside its write transaction.
func (s *Store) ResolveGoalAliasSets(ctx context.Context, aliasSets [][]GoalAlias) (GoalResolution, error) {
	return s.resolveGoalAliasSets(ctx, s.rdb, aliasSets, "")
}

// ResolveGoalAliasSetsForFamily applies the same precedence as ResolveGoalAliasSets
// but only accepts a goal of the given wire-history family.
func (s *Store) ResolveGoalAliasSetsForFamily(ctx context.Context, aliasSets [][]GoalAlias, family string) (GoalResolution, error) {
	return s.resolveGoalAliasSets(ctx, s.rdb, aliasSets, family)
}

func (s *Store) resolveGoalAliasSets(ctx context.Context, q sqlQueryer, aliasSets [][]GoalAlias, family string) (GoalResolution, error) {
	for _, aliases := range aliasSets {
		aliases = normalizedGoalAliases(aliases)
		if len(aliases) == 0 {
			continue
		}
		resolution, err := s.resolveGoalAliases(ctx, q, aliases, family)
		if errors.Is(err, ErrGoalNotFound) {
			continue
		}
		return resolution, err
	}
	return GoalResolution{}, ErrGoalNotFound
}

// ResolveFallbackGoal is deliberately narrow: only the exact key/workspace/initial
// fingerprint triple is allowed and callers must reject more than one result.  It is
// never a model-name, cache-prefix, or account-affinity guess.
func (s *Store) ResolveFallbackGoal(ctx context.Context, downstreamKeyHash, workspaceHash, initialGoalHash string) (GoalResolution, error) {
	return s.ResolveFallbackGoalForFamily(ctx, downstreamKeyHash, workspaceHash, initialGoalHash, "")
}

// ResolveFallbackGoalForFamily adds the wire-history family to the fallback triple.
// The workspace and initial-prompt fingerprints are protocol agnostic, so without it
// the same repository opened once through Codex and once through Messages resolves to
// whichever goal was written last.
func (s *Store) ResolveFallbackGoalForFamily(ctx context.Context, downstreamKeyHash, workspaceHash, initialGoalHash, family string) (GoalResolution, error) {
	if strings.TrimSpace(downstreamKeyHash) == "" || strings.TrimSpace(workspaceHash) == "" || strings.TrimSpace(initialGoalHash) == "" {
		return GoalResolution{}, ErrGoalNotFound
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, protocol, parent_goal_id, branch_hash,
 downstream_key_hash, workspace_hash, initial_goal_hash, last_response_hash, state,
	 current_checkpoint_id, encrypted_working_state, storage_bytes, expires_at, created_at, updated_at
 FROM goal_session WHERE downstream_key_hash=? AND workspace_hash=? AND initial_goal_hash=? AND expires_at>? AND state<>'reclaiming'`,
		downstreamKeyHash, workspaceHash, initialGoalHash, Now())
	if err != nil {
		return GoalResolution{}, err
	}
	defer rows.Close()
	var matches []GoalSession
	for rows.Next() {
		var item GoalSession
		var encrypted string
		if err := rows.Scan(&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash,
			&item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash,
			&item.State, &item.CurrentCheckpoint, &encrypted, &item.StorageBytes, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return GoalResolution{}, err
		}
		item.WorkingState, err = s.openContextPayload(encrypted, maxStoredContextPayloadBytes)
		if err != nil {
			return GoalResolution{}, fmt.Errorf("decode goal working state: %w", err)
		}
		matches = append(matches, item)
	}
	if err := rows.Err(); err != nil {
		return GoalResolution{}, err
	}
	if family = strings.TrimSpace(strings.ToLower(family)); family != "" {
		kept := matches[:0]
		for _, item := range matches {
			if GoalProtocolFamily(item.Protocol) == family {
				kept = append(kept, item)
			}
		}
		matches = kept
	}
	if len(matches) == 0 {
		return GoalResolution{}, ErrGoalNotFound
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, item := range matches {
			ids = append(ids, item.ID)
		}
		sort.Strings(ids)
		return GoalResolution{Candidates: ids}, ErrGoalAmbiguous
	}
	return GoalResolution{Session: matches[0]}, nil
}

// CommitGoalTurn atomically creates/binds the goal, its response alias and a single
// incremental segment.  The old context_journal is intentionally not touched here;
// callers dual-write it after this transaction so a v1 row can never make a v2 commit
// appear successful when it is not.
func (s *Store) CommitGoalTurn(ctx context.Context, turn GoalTurn) (GoalSession, error) {
	turn.Protocol = strings.TrimSpace(strings.ToLower(turn.Protocol))
	if turn.Protocol == "" || strings.TrimSpace(turn.CheckpointPayload) == "" || strings.TrimSpace(turn.SegmentPayload) == "" {
		return GoalSession{}, errors.New("invalid goal turn")
	}
	if int64(len(turn.CheckpointPayload)) > maxStoredContextPayloadBytes ||
		int64(len(turn.SegmentPayload)) > maxStoredContextPayloadBytes ||
		int64(len(turn.WorkingState)) > maxStoredContextPayloadBytes {
		return GoalSession{}, fmt.Errorf("goal context exceeds %d-byte storage limit", maxStoredContextPayloadBytes)
	}
	if turn.ExpiresAt <= Now() {
		return GoalSession{}, errors.New("goal turn expiry must be in the future")
	}
	family := GoalProtocolFamily(turn.Protocol)
	turnAliases := append([]GoalAlias(nil), turn.Aliases...)
	for index := range turnAliases {
		if namespace := strings.TrimSpace(turn.AliasNamespace); namespace != "" &&
			strings.TrimSpace(turnAliases[index].Namespace) == "" {
			turnAliases[index].Namespace = namespace
		}
		// The turn's protocol is the authority on which family owns these aliases,
		// so a caller can never persist a row under a family that disagrees with the
		// history shape it just wrote.
		turnAliases[index].Family = family
	}
	aliases := normalizedGoalAliases(turnAliases)
	if strings.TrimSpace(turn.ResponseID) != "" {
		aliases = append(aliases, GoalAlias{Type: "response_id", Value: turn.ResponseID, Namespace: turn.AliasNamespace, Family: family})
		aliases = normalizedGoalAliases(aliases)
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GoalSession{}, err
	}
	defer tx.Rollback()

	// A Codex child branch always resolves by its own concrete thread first.  The
	// parent/root alias remains owned by the root session and is relationship data,
	// never an instruction to merge a child's raw segment stream into its parent.
	resolutionSets := turn.ResolutionAliasSets
	if len(resolutionSets) == 0 {
		// Backward-compatible storage callers did not supply priority sets. A
		// concrete branch remains the only special case in that legacy shape.
		resolutionSets = [][]GoalAlias{aliases}
		if strings.TrimSpace(turn.BranchHash) != "" {
			branchAliases := make([]GoalAlias, 0, 1)
			for _, alias := range aliases {
				if alias.Type == "codex_branch_thread" {
					branchAliases = append(branchAliases, alias)
				}
			}
			if len(branchAliases) > 0 {
				resolutionSets = [][]GoalAlias{branchAliases}
			}
		}
	}
	resolution, resolveErr := s.resolveGoalAliasSets(ctx, tx, resolutionSets, family)
	if resolveErr != nil && !errors.Is(resolveErr, ErrGoalNotFound) {
		return GoalSession{}, resolveErr
	}
	var session GoalSession
	var nextSegment int64
	created := errors.Is(resolveErr, ErrGoalNotFound)
	if created && strings.TrimSpace(turn.ExpectedCurrentCheckpoint) != "" {
		return GoalSession{}, ErrGoalInProgress
	}
	segmentEstimatedBytes := s.estimateGoalChunkStorage(turn.SegmentPayload)
	var checkpointEstimatedBytes int64
	if created || turn.ReplaceHistory {
		checkpointEstimatedBytes = s.estimateGoalChunkStorage(turn.CheckpointPayload)
	}
	if !created {
		// Serialize this foreground append with the maintenance CAS that changes a
		// goal to reclaiming. The no-op update takes the parent-row write lock on
		// both SQLite and PostgreSQL without changing its LRU timestamp.
		locked, lockErr := tx.ExecContext(ctx, `UPDATE goal_session SET updated_at=updated_at WHERE id=? AND state<>'reclaiming'`, resolution.Session.ID)
		if lockErr != nil {
			return GoalSession{}, lockErr
		}
		if affected, _ := locked.RowsAffected(); affected != 1 {
			return GoalSession{}, ErrGoalNotFound
		}
		var currentLastSegment int64
		if err := tx.QueryRowContext(ctx, `SELECT current_checkpoint_id,storage_bytes,updated_at,
COALESCE((SELECT MAX(sequence) FROM goal_segment WHERE goal_id=goal_session.id),0)
FROM goal_session WHERE id=? AND state<>'reclaiming'`, resolution.Session.ID).Scan(
			&resolution.Session.CurrentCheckpoint, &resolution.Session.StorageBytes, &resolution.Session.UpdatedAt, &currentLastSegment); err != nil {
			return GoalSession{}, err
		}
		if expected := strings.TrimSpace(turn.ExpectedCurrentCheckpoint); expected != "" &&
			(expected != resolution.Session.CurrentCheckpoint || turn.ExpectedLastSegmentSequence != currentLastSegment) {
			return GoalSession{}, ErrGoalInProgress
		}
	}
	if turn.StorageMaxBytes > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&used); err != nil {
			return GoalSession{}, err
		}
		estimate := segmentEstimatedBytes
		if created || turn.ReplaceHistory {
			estimate += checkpointEstimatedBytes
		}
		projected := used + estimate
		replacementShrinks := false
		if !created && turn.ReplaceHistory {
			projected = used - resolution.Session.StorageBytes + estimate
			// An upgraded database can already exceed its configured budget.
			// Permit a replacement that strictly reduces physical storage; the
			// maintenance worker can then continue converging other inactive goals.
			replacementShrinks = estimate < resolution.Session.StorageBytes
		}
		limit := turn.StorageMaxBytes
		if created && turn.StorageTargetBytes > 0 && turn.StorageTargetBytes < limit {
			limit = turn.StorageTargetBytes
		}
		if projected > limit && !replacementShrinks {
			// Physical reclamation is deliberately maintenance-only. A foreground
			// commit never cascades through a multi-gigabyte goal while holding its
			// successful-terminal transaction. The caller may reclaim in separate,
			// bounded transactions and retry this commit exactly once.
			goalID := ""
			if !created {
				goalID = resolution.Session.ID
			}
			additional := projected - used
			if additional < 0 {
				additional = 0
			}
			return GoalSession{}, &GoalStorageBudgetError{
				UsedBytes: used, ProjectedBytes: projected, LimitBytes: limit,
				AdditionalBytes: additional, GoalID: goalID,
			}
		}
	}
	if created {
		session = GoalSession{
			ID: newGoalID("goal"), Protocol: turn.Protocol, ParentGoalID: strings.TrimSpace(turn.ParentGoalID),
			BranchHash: strings.TrimSpace(turn.BranchHash), DownstreamKeyHash: strings.TrimSpace(turn.DownstreamKeyHash),
			WorkspaceHash: strings.TrimSpace(turn.WorkspaceHash), InitialGoalHash: strings.TrimSpace(turn.InitialGoalHash),
			State: "ready", ExpiresAt: turn.ExpiresAt, CreatedAt: now, UpdatedAt: now,
		}
		// The checkpoint/segment tables intentionally have foreign keys to a goal.
		// Insert the session shell first, then fill current_checkpoint_id in the
		// final upsert below; the entire sequence remains one transaction.
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_session(id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,storage_bytes,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			session.ID, session.Protocol, session.ParentGoalID, session.BranchHash, session.DownstreamKeyHash, session.WorkspaceHash, session.InitialGoalHash, "", session.State, "", s.sealToken(compressContextPayload(turn.WorkingState)), 0, session.ExpiresAt, session.CreatedAt, session.UpdatedAt); err != nil {
			return GoalSession{}, err
		}
		checkpoint := GoalCheckpoint{ID: newGoalID("gcp"), GoalID: session.ID, Sequence: 1, Payload: turn.CheckpointPayload, CreatedAt: now}
		checkpoint.PayloadHash, checkpoint.PayloadBytes = hashGoalPayload(checkpoint.Payload), int64(len(checkpoint.Payload))
		checkpoint.FormatVersion = goalPayloadFormatV2
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,format_version,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			checkpoint.ID, checkpoint.GoalID, checkpoint.Sequence, 0, checkpoint.PayloadHash, checkpoint.PayloadBytes, "", checkpoint.FormatVersion, checkpoint.CreatedAt); err != nil {
			return GoalSession{}, err
		}
		checkpointStoredBytes, insertErr := s.insertGoalChunks(ctx, tx, session.ID, goalChunkCheckpoint, 0, now, turn.CheckpointPayload)
		if insertErr != nil {
			return GoalSession{}, insertErr
		}
		session.StorageBytes += checkpointStoredBytes
		session.CurrentCheckpoint = checkpoint.ID
		nextSegment = 1
	} else {
		session = resolution.Session
		// Two providers that serialize history identically may advance one goal: the
		// stored segments and the incoming turn use the same history key, so the
		// append is well defined. A different family may not, because its history
		// would land under the wrong key and be replayed as a malformed body.
		// Exact-protocol equality was too strict — it rejected every claude→kiro and
		// claude→antigravity handoff even though those are byte-compatible.
		if GoalProtocolFamily(session.Protocol) != family {
			return GoalSession{}, ErrGoalAmbiguous
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_segment WHERE goal_id=?`, session.ID).Scan(&nextSegment); err != nil {
			return GoalSession{}, err
		}
		if turn.ReplaceHistory {
			var nextCheckpoint int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_checkpoint WHERE goal_id=?`, session.ID).Scan(&nextCheckpoint); err != nil {
				return GoalSession{}, err
			}
			// Delete and recreate inside this transaction. SQLite restores all
			// rows on rollback; PostgreSQL keeps the parent-row lock acquired
			// above, so readers observe either the old chain or the complete new
			// chain and never a partial replacement.
			if _, err := tx.ExecContext(ctx, `DELETE FROM goal_payload_chunk WHERE goal_id=?`, session.ID); err != nil {
				return GoalSession{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM goal_segment WHERE goal_id=?`, session.ID); err != nil {
				return GoalSession{}, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM goal_checkpoint WHERE goal_id=?`, session.ID); err != nil {
				return GoalSession{}, err
			}
			checkpoint := GoalCheckpoint{
				ID: newGoalID("gcp"), GoalID: session.ID, Sequence: nextCheckpoint,
				ThroughSegmentSequence: nextSegment - 1, Payload: turn.CheckpointPayload,
				FormatVersion: goalPayloadFormatV2, CreatedAt: now,
			}
			checkpoint.PayloadHash, checkpoint.PayloadBytes = hashGoalPayload(checkpoint.Payload), int64(len(checkpoint.Payload))
			if _, err := tx.ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,format_version,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
				checkpoint.ID, checkpoint.GoalID, checkpoint.Sequence, checkpoint.ThroughSegmentSequence,
				checkpoint.PayloadHash, checkpoint.PayloadBytes, "", checkpoint.FormatVersion, checkpoint.CreatedAt); err != nil {
				return GoalSession{}, err
			}
			checkpointStoredBytes, insertErr := s.insertGoalChunks(ctx, tx, session.ID, goalChunkCheckpoint, 0, now, turn.CheckpointPayload)
			if insertErr != nil {
				return GoalSession{}, insertErr
			}
			session.StorageBytes = checkpointStoredBytes
			session.CurrentCheckpoint = checkpoint.ID
		}
		if strings.TrimSpace(turn.ParentGoalID) != "" {
			session.ParentGoalID = strings.TrimSpace(turn.ParentGoalID)
		}
		if strings.TrimSpace(turn.BranchHash) != "" {
			session.BranchHash = strings.TrimSpace(turn.BranchHash)
		}
		if strings.TrimSpace(turn.DownstreamKeyHash) != "" {
			session.DownstreamKeyHash = strings.TrimSpace(turn.DownstreamKeyHash)
		}
		if strings.TrimSpace(turn.WorkspaceHash) != "" {
			session.WorkspaceHash = strings.TrimSpace(turn.WorkspaceHash)
		}
		if strings.TrimSpace(turn.InitialGoalHash) != "" {
			session.InitialGoalHash = strings.TrimSpace(turn.InitialGoalHash)
		}
		session.ExpiresAt = turn.ExpiresAt
		session.UpdatedAt = now
	}

	segment := GoalSegment{ID: newGoalID("gseg"), GoalID: session.ID, Sequence: nextSegment, Payload: turn.SegmentPayload, FormatVersion: goalPayloadFormatV2, State: "committed", CreatedAt: now}
	segment.PayloadHash, segment.PayloadBytes = hashGoalPayload(segment.Payload), int64(len(segment.Payload))
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_segment(id,goal_id,sequence,payload_hash,payload_bytes,encrypted_payload,format_version,state,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		segment.ID, segment.GoalID, segment.Sequence, segment.PayloadHash, segment.PayloadBytes, "", segment.FormatVersion, segment.State, segment.CreatedAt); err != nil {
		return GoalSession{}, err
	}
	segmentStoredBytes, err := s.insertGoalChunks(ctx, tx, session.ID, goalChunkSegment, segment.Sequence, now, turn.SegmentPayload)
	if err != nil {
		return GoalSession{}, err
	}
	session.StorageBytes += segmentStoredBytes
	if turn.AwaitingTool {
		session.State = "awaiting_tool_result"
	} else {
		session.State = "ready"
	}
	if strings.TrimSpace(turn.ResponseID) != "" {
		session.LastResponseHash = hashGoalValue("response_id", turn.ResponseID)
	}
	working := s.sealToken(compressContextPayload(turn.WorkingState))
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_session(id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,storage_bytes,expires_at,created_at,updated_at)
	VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(id) DO UPDATE SET parent_goal_id=excluded.parent_goal_id, branch_hash=excluded.branch_hash, downstream_key_hash=excluded.downstream_key_hash, workspace_hash=excluded.workspace_hash, initial_goal_hash=excluded.initial_goal_hash, last_response_hash=excluded.last_response_hash, state=excluded.state, current_checkpoint_id=excluded.current_checkpoint_id, encrypted_working_state=excluded.encrypted_working_state, storage_bytes=excluded.storage_bytes, expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		session.ID, session.Protocol, session.ParentGoalID, session.BranchHash, session.DownstreamKeyHash, session.WorkspaceHash, session.InitialGoalHash, session.LastResponseHash, session.State, session.CurrentCheckpoint, working, session.StorageBytes, session.ExpiresAt, session.CreatedAt, session.UpdatedAt); err != nil {
		return GoalSession{}, err
	}
	for _, alias := range aliases {
		if session.ParentGoalID != "" && alias.Type == "codex_root_thread" {
			// Root aliases are immutable ownership markers of the parent goal.  A
			// child stores only its branch alias and parent_goal_id.
			continue
		}
		// Never overwrite an alias owned by another visible goal. A reclaiming owner
		// is already a logical tombstone, however, and the alias must be transferred
		// atomically so a new turn cannot commit an identity that will never resolve.
		aliasHash := hashGoalAlias(alias)
		if _, err := tx.ExecContext(ctx, `DELETE FROM goal_alias
WHERE alias_hash=? AND EXISTS (
 SELECT 1 FROM goal_session owner WHERE owner.id=goal_alias.goal_id AND owner.state='reclaiming'
)`, aliasHash); err != nil {
			return GoalSession{}, err
		}
		bound, err := tx.ExecContext(ctx, `INSERT INTO goal_alias(alias_hash,alias_type,goal_id,created_at) VALUES(?,?,?,?)
ON CONFLICT(alias_hash) DO UPDATE SET alias_type=excluded.alias_type
WHERE goal_alias.goal_id=excluded.goal_id`, aliasHash, alias.Type, session.ID, now)
		if err != nil {
			return GoalSession{}, err
		}
		if affected, _ := bound.RowsAffected(); affected != 1 {
			return GoalSession{}, ErrGoalAmbiguous
		}
	}
	if created {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
			"goal_created", session.State, turn.Protocol, fmt.Sprintf("goal=%s aliases=%d", session.ID, len(aliases)), now); err != nil {
			return GoalSession{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
		"goal_alias_bound", session.State, turn.Protocol, fmt.Sprintf("goal=%s aliases=%d", session.ID, len(aliases)), now); err != nil {
		return GoalSession{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
		"goal_checkpoint_committed", session.State, turn.Protocol, fmt.Sprintf("goal=%s checkpoint=%s segment=%d bytes=%d", session.ID, session.CurrentCheckpoint, segment.Sequence, segment.PayloadBytes), now); err != nil {
		return GoalSession{}, err
	}
	if !created && turn.ReplaceHistory {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
			"goal_history_replaced", session.State, turn.Protocol, fmt.Sprintf("goal=%s checkpoint=%s segment=%d storage_bytes=%d", session.ID, session.CurrentCheckpoint, segment.Sequence, session.StorageBytes), now); err != nil {
			return GoalSession{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GoalSession{}, err
	}
	return session, nil
}

type goalReclaimRow struct {
	id     string
	kind   string
	seq    int64
	index  int64
	bytes  int64
	format int64
}

// findReclaimingGoal always resumes an already-hidden goal before claiming
// another. A live lease is checked again even though current code never creates
// one after the reclaiming CAS; this also makes recovery safe for manually edited
// and pre-release databases.
func (s *Store) findReclaimingGoal(ctx context.Context, tx *sql.Tx, now int64) (string, error) {
	var goalID string
	err := tx.QueryRowContext(ctx, `SELECT s.id FROM goal_session s
WHERE s.state='reclaiming'
  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=s.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)
ORDER BY s.updated_at ASC,s.id ASC LIMIT 1`, now).Scan(&goalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return goalID, err
}

// markGoalReclaiming is the visibility boundary for physical reclamation. The
// conditional parent-row update serializes with CommitGoalTurn and AcquireGoalRun,
// and the live-run predicate is re-evaluated while that write lock is held.
func (s *Store) markGoalReclaiming(ctx context.Context, tx *sql.Tx, goalID string, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE goal_session SET state='reclaiming',updated_at=?
WHERE id=? AND state<>'reclaiming'
  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=goal_session.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)`, now, goalID, now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) markNextBudgetGoalReclaiming(ctx context.Context, tx *sql.Tx, now int64, protectedGoalID string) (string, error) {
	var goalID string
	var storageBytes, updatedAt int64
	awaitingCutoff := now - int64(goalAwaitingToolReclaimGrace/time.Second)
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.storage_bytes,s.updated_at FROM goal_session s
WHERE s.state<>'reclaiming'
	  AND s.id<>?
	  AND (s.state IN ('ready','retryable','completed','failed')
	       OR (s.state='awaiting_tool_result' AND s.updated_at<=?))
	  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=s.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)
ORDER BY CASE s.state WHEN 'completed' THEN 0 WHEN 'failed' THEN 1 WHEN 'ready' THEN 2 WHEN 'retryable' THEN 3 ELSE 4 END,
	         s.updated_at ASC,s.id ASC LIMIT 1`, strings.TrimSpace(protectedGoalID), awaitingCutoff, now).Scan(&goalID, &storageBytes, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE goal_session SET state='reclaiming',updated_at=?
WHERE id=? AND (state IN ('ready','retryable','completed','failed')
	              OR (state='awaiting_tool_result' AND updated_at<=?))
	  AND storage_bytes=? AND updated_at=?
	  AND id<>?
	  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=goal_session.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)`,
		now, goalID, awaitingCutoff, storageBytes, updatedAt, strings.TrimSpace(protectedGoalID), now)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	marked := affected == 1
	if err != nil || !marked {
		return "", err
	}
	return goalID, nil
}

func (s *Store) markNextExpiredGoalReclaiming(ctx context.Context, tx *sql.Tx, now int64) (string, error) {
	var goalID string
	var storageBytes, updatedAt int64
	err := tx.QueryRowContext(ctx, `SELECT s.id,s.storage_bytes,s.updated_at FROM goal_session s
WHERE s.state<>'reclaiming' AND s.expires_at<=?
  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=s.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)
ORDER BY s.expires_at ASC,s.updated_at ASC,s.id ASC LIMIT 1`, now, now).Scan(&goalID, &storageBytes, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	result, err := tx.ExecContext(ctx, `UPDATE goal_session SET state='reclaiming',updated_at=?
WHERE id=? AND state<>'reclaiming' AND expires_at<=? AND storage_bytes=? AND updated_at=?
  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=goal_session.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)`,
		now, goalID, now, storageBytes, updatedAt, now)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	marked := affected == 1
	if err != nil || !marked {
		return "", err
	}
	return goalID, nil
}

func goalReclaimRowsWithinBounds(rows []goalReclaimRow) ([]goalReclaimRow, int64, error) {
	selected := make([]goalReclaimRow, 0, len(rows))
	var bytes int64
	for _, row := range rows {
		if row.bytes < 0 {
			row.bytes = 0
		}
		if row.bytes > goalReclaimBytesPerStep {
			if len(selected) > 0 {
				break
			}
			return nil, 0, fmt.Errorf("goal reclaim row exceeds %d-byte maintenance bound", goalReclaimBytesPerStep)
		}
		if len(selected) >= goalReclaimRowsPerStep || bytes+row.bytes > goalReclaimBytesPerStep {
			break
		}
		selected = append(selected, row)
		bytes += row.bytes
	}
	return selected, bytes, nil
}

func (s *Store) reclaimGoalChunkStep(ctx context.Context, tx *sql.Tx, goalID string) (bool, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload_kind,segment_sequence,chunk_index,LENGTH(encrypted_payload)
FROM goal_payload_chunk WHERE goal_id=?
ORDER BY payload_kind,segment_sequence,chunk_index LIMIT ?`, goalID, goalReclaimRowsPerStep+1)
	if err != nil {
		return false, 0, err
	}
	var candidates []goalReclaimRow
	for rows.Next() {
		var row goalReclaimRow
		if err = rows.Scan(&row.kind, &row.seq, &row.index, &row.bytes); err != nil {
			_ = rows.Close()
			return false, 0, err
		}
		candidates = append(candidates, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, 0, err
	}
	if err = rows.Close(); err != nil {
		return false, 0, err
	}
	if len(candidates) == 0 {
		return false, 0, nil
	}
	selected, _, err := goalReclaimRowsWithinBounds(candidates)
	if err != nil {
		return false, 0, err
	}
	var freed int64
	for _, row := range selected {
		result, deleteErr := tx.ExecContext(ctx, `DELETE FROM goal_payload_chunk WHERE goal_id=? AND payload_kind=? AND segment_sequence=? AND chunk_index=?`, goalID, row.kind, row.seq, row.index)
		if deleteErr != nil {
			return false, freed, deleteErr
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			freed += row.bytes
		}
	}
	if freed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE goal_session SET storage_bytes=CASE WHEN storage_bytes>? THEN storage_bytes-? ELSE 0 END WHERE id=? AND state='reclaiming'`, freed, freed, goalID); err != nil {
			return false, freed, err
		}
	}
	return true, freed, nil
}

func (s *Store) reclaimGoalIDTableStep(ctx context.Context, tx *sql.Tx, goalID, table, idColumn string, accountPayloadBytes bool) (bool, int64, error) {
	lengthExpr := "0"
	if accountPayloadBytes {
		lengthExpr = "LENGTH(encrypted_payload),format_version"
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+idColumn+`,`+lengthExpr+` FROM `+table+` WHERE goal_id=? LIMIT ?`, goalID, goalReclaimRowsPerStep+1)
	if err != nil {
		return false, 0, err
	}
	var candidates []goalReclaimRow
	for rows.Next() {
		var row goalReclaimRow
		if accountPayloadBytes {
			err = rows.Scan(&row.id, &row.bytes, &row.format)
		} else {
			err = rows.Scan(&row.id, &row.bytes)
		}
		if err != nil {
			_ = rows.Close()
			return false, 0, err
		}
		candidates = append(candidates, row)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return false, 0, err
	}
	if err = rows.Close(); err != nil {
		return false, 0, err
	}
	if len(candidates) == 0 {
		return false, 0, nil
	}
	var selected []goalReclaimRow
	if accountPayloadBytes && candidates[0].bytes > goalReclaimBytesPerStep && candidates[0].format < goalPayloadFormatV2 {
		// v1 stored the whole encrypted checkpoint/segment in one SQL value. It
		// cannot be split without first decrypting and rewriting that same giant
		// value, so guarantee convergence by deleting exactly one legacy row. All
		// current v2 large payloads take the hard byte-bounded chunk path above.
		selected = candidates[:1]
	} else {
		selected, _, err = goalReclaimRowsWithinBounds(candidates)
		if err != nil {
			return false, 0, err
		}
	}
	var freed int64
	for _, row := range selected {
		result, deleteErr := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE goal_id=? AND `+idColumn+`=?`, goalID, row.id)
		if deleteErr != nil {
			return false, freed, deleteErr
		}
		if affected, _ := result.RowsAffected(); affected == 1 {
			freed += row.bytes
		}
	}
	if freed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE goal_session SET storage_bytes=CASE WHEN storage_bytes>? THEN storage_bytes-? ELSE 0 END WHERE id=? AND state='reclaiming'`, freed, freed, goalID); err != nil {
			return false, freed, err
		}
	}
	return true, freed, nil
}

// reclaimGoalStep advances exactly one child-table phase for one hidden goal.
// Large v2 payloads live in fixed-size goal_payload_chunk rows, so both the row
// count and ciphertext bytes removed by a transaction have hard upper bounds.
// The parent is deleted only after every cascading child table is empty.
func (s *Store) reclaimGoalStep(ctx context.Context, tx *sql.Tx, goalID string, now int64) (int64, int64, bool, error) {
	if progressed, freed, err := s.reclaimGoalChunkStep(ctx, tx, goalID); progressed || err != nil {
		return freed, 0, progressed, err
	}
	for _, phase := range []struct {
		table, idColumn string
		payload         bool
	}{
		{table: "goal_segment", idColumn: "id", payload: true},
		{table: "goal_checkpoint", idColumn: "id", payload: true},
		{table: "goal_alias", idColumn: "alias_hash"},
		{table: "goal_run", idColumn: "id"},
	} {
		if progressed, freed, err := s.reclaimGoalIDTableStep(ctx, tx, goalID, phase.table, phase.idColumn, phase.payload); progressed || err != nil {
			return freed, 0, progressed, err
		}
	}
	var remainingStorage int64
	if err := tx.QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=? AND state='reclaiming'`, goalID).Scan(&remainingStorage); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM goal_session
WHERE id=? AND state='reclaiming'
  AND NOT EXISTS (SELECT 1 FROM goal_payload_chunk WHERE goal_id=goal_session.id)
  AND NOT EXISTS (SELECT 1 FROM goal_segment WHERE goal_id=goal_session.id)
  AND NOT EXISTS (SELECT 1 FROM goal_checkpoint WHERE goal_id=goal_session.id)
  AND NOT EXISTS (SELECT 1 FROM goal_alias WHERE goal_id=goal_session.id)
  AND NOT EXISTS (SELECT 1 FROM goal_run WHERE goal_id=goal_session.id)
  AND NOT EXISTS (SELECT 1 FROM goal_run r WHERE r.goal_id=goal_session.id AND r.state IN ('running','compacting','awaiting_tool_result') AND r.lease_expires_at>?)`, goalID, now)
	if err != nil {
		return 0, 0, false, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, 0, false, err
	}
	return remainingStorage, deleted, deleted > 0, nil
}

type GoalStorageReclaimStep struct {
	BytesFreed int64
	Goals      int64
	Progressed bool
}

// EnforceGoalStorageBudget converges databases created by older releases to the
// configured global budget. CommitGoalTurn already enforces the same limit for new
// writes, but an upgraded database may begin above the limit and otherwise remain
// oversized until every retained goal expires. Live runs are protected by the same
// lease predicate used on the foreground commit path.
func (s *Store) EnforceGoalStorageBudget(ctx context.Context, maxBytes int64) (int64, int64, error) {
	step, err := s.EnforceGoalStorageBudgetStep(ctx, maxBytes)
	return step.BytesFreed, step.Goals, err
}

// EnforceGoalStorageBudgetStep exposes whether a bounded transaction advanced a
// reclaiming goal even when it removed metadata rather than payload bytes. The disk
// guard uses this signal to stop immediately when every over-budget goal is live.
func (s *Store) EnforceGoalStorageBudgetStep(ctx context.Context, maxBytes int64) (GoalStorageReclaimStep, error) {
	if maxBytes <= 0 {
		return GoalStorageReclaimStep{}, nil
	}
	return s.enforceGoalStorageBudgetStep(ctx, maxBytes, "")
}

func (s *Store) enforceGoalStorageBudgetStep(ctx context.Context, maxBytes int64, protectedGoalID string) (GoalStorageReclaimStep, error) {
	if maxBytes < 0 {
		return GoalStorageReclaimStep{}, nil
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GoalStorageReclaimStep{}, err
	}
	defer tx.Rollback()
	goalID, err := s.findReclaimingGoal(ctx, tx, now)
	if err != nil {
		return GoalStorageReclaimStep{}, err
	}
	if goalID == "" {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&used); err != nil {
			return GoalStorageReclaimStep{}, err
		}
		if used <= maxBytes {
			return GoalStorageReclaimStep{}, nil
		}
		goalID, err = s.markNextBudgetGoalReclaiming(ctx, tx, now, protectedGoalID)
		if err != nil {
			return GoalStorageReclaimStep{}, err
		}
		if goalID == "" {
			return GoalStorageReclaimStep{}, nil
		}
	}
	freed, deleted, progressed, err := s.reclaimGoalStep(ctx, tx, goalID, now)
	if err != nil {
		return GoalStorageReclaimStep{BytesFreed: freed, Goals: deleted, Progressed: progressed}, err
	}
	if deleted > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
			"goal_storage_reclaimed", "completed", "storage_budget_maintenance", fmt.Sprintf("goal=%s goals=%d bytes=%d max_bytes=%d", goalID, deleted, freed, maxBytes), now); err != nil {
			return GoalStorageReclaimStep{BytesFreed: freed, Goals: deleted, Progressed: progressed}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return GoalStorageReclaimStep{BytesFreed: freed, Goals: deleted, Progressed: progressed}, err
	}
	return GoalStorageReclaimStep{BytesFreed: freed, Goals: deleted, Progressed: progressed}, nil
}

// ReclaimGoalStorageHeadroom advances at most maxSteps bounded transactions toward
// targetBytes. The currently committing goal is excluded from new reclamation; live
// runs and awaiting-tool sessions retain the stricter predicates enforced by each
// step. The hard step bound guarantees that a successful upstream terminal can never
// enter an unbounded storage-maintenance loop.
func (s *Store) ReclaimGoalStorageHeadroom(ctx context.Context, targetBytes int64, protectedGoalID string, maxSteps int) (GoalStorageReclaimStep, error) {
	if targetBytes < 0 || maxSteps <= 0 {
		return GoalStorageReclaimStep{}, nil
	}
	var total GoalStorageReclaimStep
	for stepIndex := 0; stepIndex < maxSteps; stepIndex++ {
		step, err := s.enforceGoalStorageBudgetStep(ctx, targetBytes, protectedGoalID)
		total.BytesFreed += step.BytesFreed
		total.Goals += step.Goals
		total.Progressed = total.Progressed || step.Progressed
		if err != nil {
			return total, err
		}
		if !step.Progressed {
			break
		}
	}
	return total, nil
}

// GoalStorageBytes returns the transaction-writer view used by the hard budget
// check. Maintenance uses it between bounded reclaim steps so it can stop as soon
// as reserve headroom is restored without relying on a potentially stale replica.
func (s *Store) GoalStorageBytes(ctx context.Context) (int64, error) {
	var used int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&used)
	return used, err
}

type goalReplaySegment struct {
	HistoryKey               string        `json:"history_key"`
	Input                    interface{}   `json:"input"`
	Output                   interface{}   `json:"output"`
	ReplacementHistory       []interface{} `json:"replacement_history,omitempty"`
	ReplacementPrefix        []interface{} `json:"replacement_prefix,omitempty"`
	ReplaceInput             bool          `json:"replace_input,omitempty"`
	CodexCompactionEvaluated bool          `json:"codex_compaction_evaluated,omitempty"`
}

func appendGoalItems(dst []interface{}, raw interface{}) []interface{} {
	switch value := raw.(type) {
	case []interface{}:
		return append(dst, value...)
	case nil:
		return dst
	default:
		return append(dst, value)
	}
}

// Goal protocol families. A family is the shape of the durable history, not the
// upstream vendor: every provider whose downstream contract is Anthropic Messages
// stores history under "messages", and every Responses-shaped protocol stores it
// under "input". Replay, the commit-time mismatch gate, and alias scoping are all
// keyed on the family so two providers that speak the identical wire shape can
// advance one goal, while two providers that do not can never exchange history.
const (
	GoalFamilyMessages  = "messages"
	GoalFamilyResponses = "responses"
)

// GoalProtocolFamily maps a persisted protocol to its wire-history family.
// Unknown protocols intentionally fall back to Responses: that is the historical
// default of goalHistoryKey, so an older row keeps resolving exactly as before.
func GoalProtocolFamily(protocol string) string {
	switch strings.TrimSpace(strings.ToLower(protocol)) {
	case "claude", "kiro", "antigravity", "custom_messages":
		return GoalFamilyMessages
	default:
		return GoalFamilyResponses
	}
}

func goalHistoryKey(protocol string) string {
	if GoalProtocolFamily(protocol) == GoalFamilyMessages {
		return "messages"
	}
	return "input"
}

// claudeAssistantMessages converts the encrypted response representation back to
// native Claude assistant turns.  The original response/event objects remain in the
// segment; this only selects already-structured content, never summarizes or
// stringifies unknown blocks.
func claudeAssistantMessages(raw interface{}) []interface{} {
	items, ok := raw.([]interface{})
	if !ok {
		items = []interface{}{raw}
	}
	out := make([]interface{}, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]interface{})
		if !ok {
			continue
		}
		if message, ok := item["assistant_message"].(map[string]interface{}); ok {
			out = append(out, message)
			continue
		}
		if strings.EqualFold(streamStringStorage(item["role"]), "assistant") && item["content"] != nil {
			out = append(out, map[string]interface{}{"role": "assistant", "content": item["content"]})
			continue
		}
		// A non-stream Claude response is retained as an opaque output object by the
		// API layer.  Its `content` is already protocol-native and safe to reuse.
		if item["content"] != nil {
			out = append(out, map[string]interface{}{"role": "assistant", "content": item["content"]})
		}
	}
	return out
}

func streamStringStorage(value interface{}) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// GoalReplayVersion is an aggregate structural fence for an exact replay snapshot.
// The checkpoint id changes on replacement/compaction and the last sequence changes
// on an append; together they cover every writer that can alter reconstructed history.
type GoalReplayVersion struct {
	CurrentCheckpoint   string
	LastSegmentSequence int64
}

func (s *Store) goalReplayVersion(ctx context.Context, goalID string) (GoalReplayVersion, error) {
	var version GoalReplayVersion
	err := s.rdb.QueryRowContext(ctx, `SELECT current_checkpoint_id,
COALESCE((SELECT MAX(sequence) FROM goal_segment WHERE goal_id=goal_session.id),0)
FROM goal_session WHERE id=? AND expires_at>? AND state<>'reclaiming'`, goalID, Now()).Scan(
		&version.CurrentCheckpoint, &version.LastSegmentSequence,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return GoalReplayVersion{}, ErrGoalNotFound
	}
	return version, err
}

// BuildGoalReplaySnapshot returns a replay only when its structural version was
// unchanged across reconstruction. A later CommitGoalTurn must still supply the
// returned version, closing the remaining build-to-commit race under the row lock.
func (s *Store) BuildGoalReplaySnapshot(ctx context.Context, goalID string) ([]byte, GoalSession, GoalReplayVersion, error) {
	before, err := s.goalReplayVersion(ctx, goalID)
	if err != nil {
		return nil, GoalSession{}, GoalReplayVersion{}, err
	}
	replay, session, err := s.BuildGoalReplay(ctx, goalID)
	if err != nil {
		return nil, GoalSession{}, GoalReplayVersion{}, err
	}
	after, err := s.goalReplayVersion(ctx, goalID)
	if err != nil {
		return nil, GoalSession{}, GoalReplayVersion{}, err
	}
	if before != after {
		return nil, GoalSession{}, GoalReplayVersion{}, ErrGoalInProgress
	}
	return replay, session, before, nil
}

// BuildGoalReplay reconstructs a protocol payload from the latest compacted
// checkpoint and only its not-yet-compacted segments.  No raw alias is read or
// returned; callers can safely use this after a process restart or account switch.
func (s *Store) BuildGoalReplay(ctx context.Context, goalID string) ([]byte, GoalSession, error) {
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return nil, GoalSession{}, err
	}
	if session.CurrentCheckpoint == "" {
		return nil, session, ErrGoalNotFound
	}
	checkpoint, err := s.getGoalCheckpoint(ctx, session.CurrentCheckpoint)
	if err != nil {
		return nil, session, err
	}
	var root map[string]interface{}
	if err := decodeGoalReplayJSON(checkpoint.Payload, &root); err != nil {
		return nil, session, fmt.Errorf("invalid goal checkpoint: %w", err)
	}
	historyKey := goalHistoryKey(session.Protocol)
	items := make([]interface{}, 0)
	items = appendGoalItems(items, root[historyKey])
	compacted, err := s.listGoalCompactedSegments(ctx, session.ID, checkpoint.ThroughSegmentSequence)
	if err != nil {
		return nil, session, err
	}
	for _, segment := range compacted {
		var turn goalReplaySegment
		if err := decodeGoalReplayJSON(segment.Payload, &turn); err != nil {
			return nil, session, fmt.Errorf("invalid compacted goal segment: %w", err)
		}
		if turn.HistoryKey != "" && turn.HistoryKey != historyKey {
			return nil, session, fmt.Errorf("goal segment protocol history mismatch: %s", turn.HistoryKey)
		}
		items = appendGoalReplayTurn(items, turn, historyKey)
	}
	segments, err := s.listGoalSegmentsAfter(ctx, session.ID, checkpoint.ThroughSegmentSequence)
	if err != nil {
		return nil, session, err
	}
	for _, segment := range segments {
		var turn goalReplaySegment
		if err := decodeGoalReplayJSON(segment.Payload, &turn); err != nil {
			return nil, session, fmt.Errorf("invalid goal segment: %w", err)
		}
		if turn.HistoryKey != "" && turn.HistoryKey != historyKey {
			return nil, session, fmt.Errorf("goal segment protocol history mismatch: %s", turn.HistoryKey)
		}
		items = appendGoalReplayTurn(items, turn, historyKey)
	}
	root[historyKey] = items
	if historyKey == "messages" {
		delete(root, "input")
	} else {
		delete(root, "messages")
	}
	delete(root, "previous_response_id")
	delete(root, "turn_state")
	// Each payload query excludes reclaiming. Recheck after the last query as
	// well, so a maintenance CAS between the initial session lookup and an empty
	// segment result cannot return a truncated replay that looks successful.
	if _, err := s.GetGoalSession(ctx, goalID); err != nil {
		return nil, GoalSession{}, err
	}
	body, err := json.Marshal(root)
	return body, session, err
}

func decodeGoalReplayJSON(raw string, dst interface{}) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(dst)
}

func (s *Store) GetGoalSession(ctx context.Context, id string) (GoalSession, error) {
	var item GoalSession
	var encrypted string
	err := s.rdb.QueryRowContext(ctx, `SELECT id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,storage_bytes,expires_at,created_at,updated_at FROM goal_session WHERE id=? AND expires_at>? AND state<>'reclaiming'`, id, Now()).Scan(
		&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash, &item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash, &item.State, &item.CurrentCheckpoint, &encrypted, &item.StorageBytes, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalSession{}, ErrGoalNotFound
		}
		return GoalSession{}, err
	}
	item.WorkingState, err = s.openContextPayload(encrypted, maxStoredContextPayloadBytes)
	if err != nil {
		return GoalSession{}, fmt.Errorf("decode goal working state: %w", err)
	}
	return item, nil
}

func (s *Store) getGoalCheckpoint(ctx context.Context, id string) (GoalCheckpoint, error) {
	var item GoalCheckpoint
	var encrypted string
	err := s.rdb.QueryRowContext(ctx, `SELECT c.id,c.goal_id,c.sequence,c.through_segment_sequence,c.payload_hash,c.payload_bytes,c.encrypted_payload,c.format_version,c.created_at
FROM goal_checkpoint c JOIN goal_session s ON s.id=c.goal_id
WHERE c.id=? AND s.state<>'reclaiming'`, id).Scan(
		&item.ID, &item.GoalID, &item.Sequence, &item.ThroughSegmentSequence, &item.PayloadHash, &item.PayloadBytes, &encrypted, &item.FormatVersion, &item.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalCheckpoint{}, ErrGoalNotFound
		}
		return GoalCheckpoint{}, err
	}
	if item.FormatVersion >= goalPayloadFormatV2 {
		item.Payload, err = s.readGoalChunks(ctx, item.GoalID, goalChunkCheckpoint, 0)
	} else {
		item.Payload = s.openToken(encrypted)
	}
	return item, nil
}

func (s *Store) listGoalSegmentsAfter(ctx context.Context, goalID string, after int64) ([]GoalSegment, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT g.id,g.goal_id,g.sequence,g.payload_hash,g.payload_bytes,g.encrypted_payload,g.format_version,g.state,g.created_at
FROM goal_segment g JOIN goal_session s ON s.id=g.goal_id
WHERE g.goal_id=? AND g.sequence>? AND s.state<>'reclaiming'
ORDER BY g.sequence ASC`, goalID, after)
	if err != nil {
		return nil, err
	}
	var out []GoalSegment
	var encryptedPayloads []string
	for rows.Next() {
		var item GoalSegment
		var encrypted string
		if err := rows.Scan(&item.ID, &item.GoalID, &item.Sequence, &item.PayloadHash, &item.PayloadBytes, &encrypted, &item.FormatVersion, &item.State, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
		encryptedPayloads = append(encryptedPayloads, encrypted)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].FormatVersion >= goalPayloadFormatV2 {
			out[i].Payload, err = s.readGoalChunks(ctx, out[i].GoalID, goalChunkSegment, out[i].Sequence)
			if err != nil {
				return nil, err
			}
		} else {
			out[i].Payload = s.openToken(encryptedPayloads[i])
		}
	}
	return out, nil
}

func (s *Store) listGoalCompactedSegments(ctx context.Context, goalID string, through int64) ([]GoalSegment, error) {
	if through <= 0 {
		return nil, nil
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT p.segment_sequence,p.payload_hash,p.payload_bytes,p.encrypted_payload
FROM goal_payload_chunk p JOIN goal_session s ON s.id=p.goal_id
WHERE p.goal_id=? AND p.payload_kind=? AND p.segment_sequence>0 AND p.segment_sequence<=? AND s.state<>'reclaiming'
ORDER BY p.segment_sequence,p.chunk_index`, goalID, goalChunkSegment, through)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]GoalSegment, 0)
	var sequence int64
	var payload bytes.Buffer
	flush := func() {
		if sequence > 0 {
			out = append(out, GoalSegment{GoalID: goalID, Sequence: sequence, Payload: payload.String(), FormatVersion: goalPayloadFormatV2, State: "compacted"})
		}
		payload.Reset()
	}
	for rows.Next() {
		var current int64
		var hash, encrypted string
		var plainBytes int64
		if err = rows.Scan(&current, &hash, &plainBytes, &encrypted); err != nil {
			return nil, err
		}
		if sequence != 0 && current != sequence {
			flush()
		}
		sequence = current
		if plainBytes < 0 || plainBytes > goalPayloadChunkSize {
			return nil, fmt.Errorf("goal payload chunk declares invalid plaintext length %d", plainBytes)
		}
		decoded, decodeErr := s.openContextPayload(encrypted, goalPayloadChunkSize)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if int64(len(decoded)) != plainBytes || hashGoalPayload(decoded) != hash {
			return nil, errors.New("goal payload chunk failed plaintext length/hash verification")
		}
		if int64(payload.Len())+plainBytes > maxStoredContextPayloadBytes {
			return nil, fmt.Errorf("goal segment exceeds %d-byte reconstruction limit", maxStoredContextPayloadBytes)
		}
		payload.WriteString(decoded)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}

// NeedsGoalCompaction is a metadata-only threshold check used by the asynchronous
// compaction scheduler. It does not decrypt any payload or expose conversation data.
func (s *Store) NeedsGoalCompaction(ctx context.Context, goalID string, maxStages int) (bool, error) {
	if maxStages <= 0 {
		return false, nil
	}
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return false, err
	}
	checkpoint, err := s.getGoalCheckpoint(ctx, session.CurrentCheckpoint)
	if err != nil {
		return false, err
	}
	var count int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_segment WHERE goal_id=? AND sequence>?`, goalID, checkpoint.ThroughSegmentSequence).Scan(&count); err != nil {
		return false, err
	}
	return count > maxStages, nil
}

// CompactGoalSegments keeps the historical entry point for callers that need one
// bounded compaction chunk.  New work should use CompactGoalSegmentsWithRatio so the
// configured block ratio is honored by the resumable worker.
func (s *Store) CompactGoalSegments(ctx context.Context, goalID string, maxStages int) error {
	return s.CompactGoalSegmentsWithRatio(ctx, goalID, maxStages, 1)
}

// CompactGoalSegmentsWithRatio coalesces only a bounded prefix of full committed
// turns. It never splits an unknown block or a tool pair, so arbitrary JSON and
// attachments remain byte-preserved in encrypted storage. Returning after one chunk
// gives the caller a durable resume point for very large histories instead of holding
// SQLite or a foreground request open for an unbounded rewrite.
func (s *Store) CompactGoalSegmentsWithRatio(ctx context.Context, goalID string, maxStages int, chunkRatio float64) error {
	if maxStages <= 0 {
		return nil
	}
	if chunkRatio <= 0 || chunkRatio > 1 {
		chunkRatio = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked, err := tx.ExecContext(ctx, `UPDATE goal_session SET updated_at=updated_at WHERE id=? AND expires_at>? AND state<>'reclaiming'`, goalID, Now())
	if err != nil {
		return err
	}
	if affected, _ := locked.RowsAffected(); affected != 1 {
		return ErrGoalNotFound
	}
	var checkpoint GoalCheckpoint
	var encryptedCheckpoint string
	if err = tx.QueryRowContext(ctx, `SELECT c.id,c.goal_id,c.sequence,c.through_segment_sequence,c.payload_hash,c.payload_bytes,c.encrypted_payload,c.format_version,c.created_at
FROM goal_session s JOIN goal_checkpoint c ON c.id=s.current_checkpoint_id
WHERE s.id=? AND s.expires_at>? AND s.state<>'reclaiming'`, goalID, Now()).Scan(
		&checkpoint.ID, &checkpoint.GoalID, &checkpoint.Sequence, &checkpoint.ThroughSegmentSequence, &checkpoint.PayloadHash, &checkpoint.PayloadBytes, &encryptedCheckpoint, &checkpoint.FormatVersion, &checkpoint.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGoalNotFound
		}
		return err
	}
	var segmentCount int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_segment WHERE goal_id=? AND sequence>?`, goalID, checkpoint.ThroughSegmentSequence).Scan(&segmentCount); err != nil {
		return err
	}
	if segmentCount <= maxStages {
		return nil
	}
	required := segmentCount - maxStages
	chunk := int(float64(maxStages)*chunkRatio + 0.999999)
	if chunk < 1 {
		chunk = 1
	}
	if required < chunk {
		chunk = required
	}
	type compactSegment struct {
		id, encrypted    string
		sequence, format int64
		createdAt        int64
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,sequence,encrypted_payload,format_version,created_at FROM goal_segment WHERE goal_id=? AND sequence>? ORDER BY sequence LIMIT ?`, goalID, checkpoint.ThroughSegmentSequence, chunk)
	if err != nil {
		return err
	}
	selected := make([]compactSegment, 0, chunk)
	for rows.Next() {
		var segment compactSegment
		if err = rows.Scan(&segment.id, &segment.sequence, &segment.encrypted, &segment.format, &segment.createdAt); err != nil {
			_ = rows.Close()
			return err
		}
		selected = append(selected, segment)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(selected) == 0 {
		return nil
	}
	now := Now()
	var addedStorage, removedStorage int64
	if checkpoint.FormatVersion < goalPayloadFormatV2 {
		checkpoint.Payload = s.openToken(encryptedCheckpoint)
		stored, insertErr := s.insertGoalChunks(ctx, tx, goalID, goalChunkCheckpoint, 0, now, checkpoint.Payload)
		if insertErr != nil {
			return insertErr
		}
		addedStorage += stored
	}
	for _, segment := range selected {
		if segment.format >= goalPayloadFormatV2 {
			continue
		}
		stored, insertErr := s.insertGoalChunks(ctx, tx, goalID, goalChunkSegment, segment.sequence, segment.createdAt, s.openToken(segment.encrypted))
		if insertErr != nil {
			return insertErr
		}
		addedStorage += stored
	}
	through := selected[len(selected)-1].sequence
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_checkpoint WHERE goal_id=?`, goalID).Scan(&next); err != nil {
		return err
	}
	newCheckpointID := newGoalID("gcp")
	if _, err = tx.ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,format_version,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		newCheckpointID, goalID, next, through, checkpoint.PayloadHash, checkpoint.PayloadBytes, "", goalPayloadFormatV2, now); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_checkpoint WHERE goal_id=?`, goalID).Scan(&removedStorage); err != nil {
		return err
	}
	for _, segment := range selected {
		removedStorage += int64(len(segment.encrypted))
	}
	updated, err := tx.ExecContext(ctx, `UPDATE goal_session SET current_checkpoint_id=?,storage_bytes=MAX(0,storage_bytes+?),updated_at=? WHERE id=? AND current_checkpoint_id=? AND state<>'reclaiming'`, newCheckpointID, addedStorage-removedStorage, now, goalID, checkpoint.ID)
	if err != nil {
		return err
	}
	if changed, _ := updated.RowsAffected(); changed != 1 {
		return ErrGoalInProgress
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM goal_segment WHERE goal_id=? AND sequence<=?`, goalID, through); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM goal_checkpoint WHERE goal_id=? AND id<>?`, goalID, newCheckpointID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`, "goal_compaction_completed", "ready", "incremental_checkpoint_v2", fmt.Sprintf("goal=%s checkpoint=%s through_segment=%d chunk=%d", goalID, newCheckpointID, through, len(selected)), now); err != nil {
		return err
	}
	return tx.Commit()
}

func activeGoalRunState(state string) bool {
	switch state {
	case "running", "compacting", "awaiting_tool_result":
		return true
	}
	return false
}

// AcquireGoalRun provides the SQLite lease used by concurrent /goal resumes.  A stale
// lease is first marked retryable, then a new run starts from the latest committed
// checkpoint.  No tool call is ever re-executed here.
func (s *Store) AcquireGoalRun(ctx context.Context, goalID, owner, phase string, lease time.Duration) (GoalRun, error) {
	if lease <= 0 {
		lease = 90 * time.Second
	}
	// Validate/read before opening the single-writer transaction.  In-memory tests
	// intentionally share the read and write handle; querying rdb while tx owns its
	// sole connection would deadlock even though the session already exists.
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return GoalRun{}, err
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GoalRun{}, err
	}
	defer tx.Rollback()
	locked, err := tx.ExecContext(ctx, `UPDATE goal_session SET updated_at=updated_at WHERE id=? AND expires_at>? AND state<>'reclaiming'`, goalID, now)
	if err != nil {
		return GoalRun{}, err
	}
	if affected, _ := locked.RowsAffected(); affected != 1 {
		return GoalRun{}, ErrGoalNotFound
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,goal_id,state,lease_owner,lease_expires_at,heartbeat_at,checkpoint_id,failure_code,created_at,updated_at FROM goal_run WHERE goal_id=? AND state IN ('running','compacting','awaiting_tool_result') ORDER BY updated_at DESC`, goalID)
	if err != nil {
		return GoalRun{}, err
	}
	for rows.Next() {
		var prior GoalRun
		if err := rows.Scan(&prior.ID, &prior.GoalID, &prior.State, &prior.LeaseOwner, &prior.LeaseExpiresAt, &prior.HeartbeatAt, &prior.CheckpointID, &prior.FailureCode, &prior.CreatedAt, &prior.UpdatedAt); err != nil {
			rows.Close()
			return GoalRun{}, err
		}
		if prior.LeaseExpiresAt > now {
			rows.Close()
			return prior, ErrGoalInProgress
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goal_run SET state='retryable',failure_code='goal_run_heartbeat_expired',updated_at=? WHERE id=?`, now, prior.ID); err != nil {
			rows.Close()
			return GoalRun{}, err
		}
	}
	if err := rows.Close(); err != nil {
		return GoalRun{}, err
	}
	if phase == "" {
		phase = "running"
	}
	run := GoalRun{ID: newGoalID("grun"), GoalID: goalID, State: phase, LeaseOwner: owner, LeaseExpiresAt: now + int64(lease/time.Second), HeartbeatAt: now, CheckpointID: session.CurrentCheckpoint, CreatedAt: now, UpdatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_run(id,goal_id,state,lease_owner,lease_expires_at,heartbeat_at,checkpoint_id,failure_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, run.ID, run.GoalID, run.State, run.LeaseOwner, run.LeaseExpiresAt, run.HeartbeatAt, run.CheckpointID, "", run.CreatedAt, run.UpdatedAt); err != nil {
		return GoalRun{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`, "goal_resume_started", run.State, "lease_acquired", fmt.Sprintf("goal=%s run=%s checkpoint=%s", goalID, run.ID, run.CheckpointID), now); err != nil {
		return GoalRun{}, err
	}
	if err := tx.Commit(); err != nil {
		return GoalRun{}, err
	}
	return run, nil
}

func (s *Store) HeartbeatGoalRun(ctx context.Context, runID, owner string, lease time.Duration) error {
	if lease <= 0 {
		lease = 90 * time.Second
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked, err := tx.ExecContext(ctx, `UPDATE goal_session SET updated_at=updated_at
WHERE id=(SELECT goal_id FROM goal_run WHERE id=? AND lease_owner=?)
  AND state<>'reclaiming'`, runID, owner)
	if err != nil {
		return err
	}
	if affected, _ := locked.RowsAffected(); affected != 1 {
		return ErrGoalNotFound
	}
	result, err := tx.ExecContext(ctx, `UPDATE goal_run SET heartbeat_at=?,lease_expires_at=?,updated_at=?
WHERE id=? AND lease_owner=? AND state IN ('running','compacting','awaiting_tool_result')`, now, now+int64(lease/time.Second), now, runID, owner)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrGoalNotFound
	}
	return tx.Commit()
}

func (s *Store) FinishGoalRun(ctx context.Context, runID, owner, state, failureCode string) error {
	if !activeGoalRunState(state) && state != "retryable" && state != "completed" && state != "failed" {
		return errors.New("invalid goal run state")
	}
	// A relay can defer its run cleanup after it has observed a terminal transport
	// failure.  Preserve the retryable result set on the session instead of turning
	// that interrupted run into a misleading completed row during deferred cleanup.
	if state == "completed" {
		var sessionState string
		if err := s.rdb.QueryRowContext(ctx, `SELECT s.state FROM goal_run r JOIN goal_session s ON s.id=r.goal_id WHERE r.id=? AND r.lease_owner=?`, runID, owner).Scan(&sessionState); err == nil && sessionState == "retryable" {
			state = "retryable"
			if strings.TrimSpace(failureCode) == "" {
				failureCode = "goal_stream_interrupted"
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	now := Now()
	result, err := s.db.ExecContext(ctx, `UPDATE goal_run SET state=?,failure_code=?,lease_expires_at=0,updated_at=? WHERE id=? AND lease_owner=?`, state, strings.TrimSpace(failureCode), now, runID, owner)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrGoalNotFound
	}
	return nil
}

// MarkGoalRetryable retains the latest committed checkpoint after an upstream relay
// fault.  It never creates a checkpoint and therefore cannot make partial assistant
// output look durable.  A later successful resume resets the state to ready inside
// CommitGoalTurn.
func (s *Store) MarkGoalRetryable(ctx context.Context, goalID, reason string) error {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" {
		return ErrGoalNotFound
	}
	now := Now()
	result, err := s.db.ExecContext(ctx, `UPDATE goal_session SET state='retryable', updated_at=? WHERE id=? AND expires_at>? AND state<>'reclaiming'`, now, goalID, now)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrGoalNotFound
	}
	return s.InsertAuditLog(ctx, AuditLogRow{Action: "goal_stream_terminal_synthesized", State: "retryable", Reason: strings.TrimSpace(reason), Detail: "goal=" + goalID})
}

// SetGoalCompactionState records lifecycle-only state for a checkpoint worker.  It
// never touches encrypted payloads and refuses arbitrary states so admin metadata
// cannot claim a non-existent phase.
func (s *Store) SetGoalCompactionState(ctx context.Context, goalID, state string) error {
	state = strings.TrimSpace(state)
	if state != "compacting" && state != "ready" && state != "retryable" {
		return errors.New("invalid goal compaction state")
	}
	now := Now()
	result, err := s.db.ExecContext(ctx, `UPDATE goal_session SET state=?,updated_at=? WHERE id=? AND expires_at>? AND state<>'reclaiming'`, state, now, strings.TrimSpace(goalID), now)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrGoalNotFound
	}
	return nil
}

func (s *Store) ListGoalSessions(ctx context.Context, limit int) ([]GoalSession, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,storage_bytes,expires_at,created_at,updated_at FROM goal_session WHERE state<>'reclaiming' ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GoalSession
	for rows.Next() {
		var item GoalSession
		if err := rows.Scan(&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash, &item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash, &item.State, &item.CurrentCheckpoint, &item.StorageBytes, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GetGoalDetail(ctx context.Context, goalID string) (GoalDetail, error) {
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return GoalDetail{}, err
	}
	detail := GoalDetail{Session: session}
	if err := s.rdb.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?), (SELECT COUNT(*) FROM goal_segment WHERE goal_id=?)`, goalID, goalID).Scan(&detail.CheckpointCount, &detail.SegmentCount); err != nil {
		return GoalDetail{}, err
	}
	detail.PayloadBytes = session.StorageBytes
	var run GoalRun
	err = s.rdb.QueryRowContext(ctx, `SELECT id,goal_id,state,lease_expires_at,heartbeat_at,checkpoint_id,failure_code,created_at,updated_at FROM goal_run WHERE goal_id=? ORDER BY updated_at DESC LIMIT 1`, goalID).Scan(&run.ID, &run.GoalID, &run.State, &run.LeaseExpiresAt, &run.HeartbeatAt, &run.CheckpointID, &run.FailureCode, &run.CreatedAt, &run.UpdatedAt)
	if err == nil {
		detail.LatestRun = &run
	} else if !errors.Is(err, sql.ErrNoRows) {
		return GoalDetail{}, err
	}
	return detail, nil
}

func (s *Store) GoalContinuityMetrics(ctx context.Context) (GoalMetrics, error) {
	var metrics GoalMetrics
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_session`).Scan(&metrics.Sessions); err != nil {
		return GoalMetrics{}, err
	}
	if err := s.rdb.QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&metrics.StorageBytes); err != nil {
		return GoalMetrics{}, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT action,COUNT(*) FROM audit_log WHERE action IN ('goal_resume_recovered','goal_resume_ambiguous','goal_stream_terminal_synthesized','goal_persistence_degraded','goal_history_replaced','goal_compaction_completed') GROUP BY action`)
	if err != nil {
		return GoalMetrics{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		var count int64
		if err := rows.Scan(&action, &count); err != nil {
			return GoalMetrics{}, err
		}
		switch action {
		case "goal_resume_recovered":
			metrics.ResumeRecovered = count
		case "goal_resume_ambiguous":
			metrics.ResumeAmbiguous = count
		case "goal_stream_terminal_synthesized":
			metrics.StreamTerminalSynthesized = count
		case "goal_persistence_degraded":
			metrics.PersistenceDegraded = count
		case "goal_history_replaced":
			metrics.HistoryReplaced = count
		case "goal_compaction_completed":
			metrics.CompactionCompleted = count
		}
	}
	return metrics, rows.Err()
}

func (s *Store) DeleteGoalSafely(ctx context.Context, goalID string) error {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" {
		return ErrGoalNotFound
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM goal_session WHERE id=?`, goalID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGoalNotFound
		}
		return err
	}
	var activeRuns int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_run WHERE goal_id=? AND state IN ('running','compacting','awaiting_tool_result') AND lease_expires_at>?`, goalID, now).Scan(&activeRuns); err != nil {
		return err
	}
	if state != goalReclaimingState && (activeGoalRunState(state) || activeRuns > 0) {
		return ErrGoalActiveCannotBePurged
	}
	if state != goalReclaimingState {
		marked, markErr := s.markGoalReclaiming(ctx, tx, goalID, now)
		if markErr != nil {
			return markErr
		}
		if !marked {
			return ErrGoalActiveCannotBePurged
		}
	}
	_, deleted, _, err := s.reclaimGoalStep(ctx, tx, goalID, now)
	if err != nil {
		return err
	}
	auditState := goalReclaimingState
	if deleted > 0 {
		auditState = "deleted"
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`,
		"goal_cleaned", auditState, "admin_safe_cleanup", "goal="+goalID, now); err != nil {
		return err
	}
	return tx.Commit()
}

// CleanupGoalContinuity marks at most one expired inactive goal and advances one
// bounded physical-reclamation phase. Existing reclaiming work is always resumed,
// including work originally claimed by the budget or admin paths.
func (s *Store) CleanupGoalContinuity(ctx context.Context) (int64, error) {
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	goalID, err := s.findReclaimingGoal(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	if goalID == "" {
		goalID, err = s.markNextExpiredGoalReclaiming(ctx, tx, now)
		if err != nil {
			return 0, err
		}
		if goalID == "" {
			return 0, nil
		}
	}
	_, deleted, _, err := s.reclaimGoalStep(ctx, tx, goalID, now)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}
