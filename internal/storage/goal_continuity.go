package storage

// This file owns the v2 long-task continuity store.  It deliberately keeps
// identifiers usable for lookup only as SHA-256 hashes: thread ids, response ids,
// Claude sessions and turn-state values are not diagnostic data and must never be
// copied into an operator-facing table.  Payload-bearing columns use Store's existing
// token encryption in exactly the same way as context_journal.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
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
 state TEXT NOT NULL DEFAULT 'committed',
 created_at INTEGER NOT NULL,
 FOREIGN KEY(goal_id) REFERENCES goal_session(id) ON DELETE CASCADE,
 UNIQUE(goal_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_goal_segment_goal ON goal_segment(goal_id, sequence);
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

// GoalAlias is intentionally accepted in plaintext at the storage boundary but is
// immediately hashed.  Callers must not log Value.
type GoalAlias struct {
	Type  string
	Value string
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
	CreatedAt              int64  `json:"created_at"`
}

type GoalSegment struct {
	ID           string `json:"id"`
	GoalID       string `json:"goal_id"`
	Sequence     int64  `json:"sequence"`
	PayloadHash  string `json:"payload_hash"`
	PayloadBytes int64  `json:"payload_bytes"`
	Payload      string `json:"-"`
	State        string `json:"state"`
	CreatedAt    int64  `json:"created_at"`
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
	Aliases           []GoalAlias
	CheckpointPayload string
	SegmentPayload    string
	WorkingState      string
	AwaitingTool      bool
	ExpiresAt         int64
	StorageMaxBytes   int64
	CompressionStages int
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
}

func hashGoalValue(kind, value string) string {
	s := sha256.Sum256([]byte(strings.TrimSpace(kind) + "\x00" + strings.TrimSpace(value)))
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
		if alias.Type == "" || alias.Value == "" {
			continue
		}
		h := hashGoalValue(alias.Type, alias.Value)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, alias)
	}
	return out
}

func (s *Store) resolveGoalAliases(ctx context.Context, q sqlQueryer, aliases []GoalAlias) (GoalResolution, error) {
	aliases = normalizedGoalAliases(aliases)
	if len(aliases) == 0 {
		return GoalResolution{}, ErrGoalNotFound
	}
	args := make([]interface{}, 0, len(aliases)+1)
	marks := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		marks = append(marks, "?")
		args = append(args, hashGoalValue(alias.Type, alias.Value))
	}
	args = append(args, Now())
	query := `SELECT DISTINCT s.id, s.protocol, s.parent_goal_id, s.branch_hash,
 s.downstream_key_hash, s.workspace_hash, s.initial_goal_hash, s.last_response_hash,
 s.state, s.current_checkpoint_id, s.encrypted_working_state, s.expires_at, s.created_at, s.updated_at
 FROM goal_alias a JOIN goal_session s ON s.id=a.goal_id
 WHERE a.alias_hash IN (` + strings.Join(marks, ",") + `) AND s.expires_at>?`
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
			&item.State, &item.CurrentCheckpoint, &encrypted, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return GoalResolution{}, err
		}
		item.WorkingState = s.openToken(encrypted)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return GoalResolution{}, err
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
	return s.resolveGoalAliases(ctx, s.rdb, aliases)
}

// ResolveFallbackGoal is deliberately narrow: only the exact key/workspace/initial
// fingerprint triple is allowed and callers must reject more than one result.  It is
// never a model-name, cache-prefix, or account-affinity guess.
func (s *Store) ResolveFallbackGoal(ctx context.Context, downstreamKeyHash, workspaceHash, initialGoalHash string) (GoalResolution, error) {
	if strings.TrimSpace(downstreamKeyHash) == "" || strings.TrimSpace(workspaceHash) == "" || strings.TrimSpace(initialGoalHash) == "" {
		return GoalResolution{}, ErrGoalNotFound
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, protocol, parent_goal_id, branch_hash,
 downstream_key_hash, workspace_hash, initial_goal_hash, last_response_hash, state,
 current_checkpoint_id, encrypted_working_state, expires_at, created_at, updated_at
 FROM goal_session WHERE downstream_key_hash=? AND workspace_hash=? AND initial_goal_hash=? AND expires_at>?`,
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
			&item.State, &item.CurrentCheckpoint, &encrypted, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return GoalResolution{}, err
		}
		item.WorkingState = s.openToken(encrypted)
		matches = append(matches, item)
	}
	if err := rows.Err(); err != nil {
		return GoalResolution{}, err
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
	if turn.ExpiresAt <= Now() {
		return GoalSession{}, errors.New("goal turn expiry must be in the future")
	}
	aliases := normalizedGoalAliases(turn.Aliases)
	if strings.TrimSpace(turn.ResponseID) != "" {
		aliases = append(aliases, GoalAlias{Type: "response_id", Value: turn.ResponseID})
		aliases = normalizedGoalAliases(aliases)
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GoalSession{}, err
	}
	defer tx.Rollback()

	// Expired completed/retryable work is always reclaimed before rejecting a new
	// checkpoint.  Active runs never participate in eviction.
	if _, err := tx.ExecContext(ctx, `DELETE FROM goal_session WHERE expires_at<=? AND id NOT IN (SELECT goal_id FROM goal_run WHERE state IN ('running','compacting','awaiting_tool_result') AND lease_expires_at>?)`, now, now); err != nil {
		return GoalSession{}, err
	}
	// A Codex child branch always resolves by its own concrete thread first.  The
	// parent/root alias remains owned by the root session and is relationship data,
	// never an instruction to merge a child's raw segment stream into its parent.
	resolutionAliases := aliases
	if strings.TrimSpace(turn.BranchHash) != "" {
		branchAliases := make([]GoalAlias, 0, 1)
		for _, alias := range aliases {
			if alias.Type == "codex_branch_thread" {
				branchAliases = append(branchAliases, alias)
			}
		}
		if len(branchAliases) > 0 {
			resolutionAliases = branchAliases
		}
	}
	resolution, resolveErr := s.resolveGoalAliases(ctx, tx, resolutionAliases)
	if resolveErr != nil && !errors.Is(resolveErr, ErrGoalNotFound) {
		return GoalSession{}, resolveErr
	}
	var session GoalSession
	var nextSegment, nextCheckpoint int64
	created := errors.Is(resolveErr, ErrGoalNotFound)
	if turn.StorageMaxBytes > 0 {
		var checkpointUsed, segmentUsed int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_checkpoint`).Scan(&checkpointUsed); err != nil {
			return GoalSession{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(encrypted_payload)),0) FROM goal_segment`).Scan(&segmentUsed); err != nil {
			return GoalSession{}, err
		}
		estimate := int64(len(turn.SegmentPayload))
		if created {
			estimate += int64(len(turn.CheckpointPayload))
		}
		// Existing turns append one segment only; charging their base checkpoint a
		// second time would prematurely reject long jobs even though the chain is
		// incremental.  Encryption overhead is checked conservatively by the next
		// write/cleanup pass without evicting active work.
		if checkpointUsed+segmentUsed+estimate > turn.StorageMaxBytes {
			return GoalSession{}, ErrGoalStorageBudget
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_session(id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			session.ID, session.Protocol, session.ParentGoalID, session.BranchHash, session.DownstreamKeyHash, session.WorkspaceHash, session.InitialGoalHash, "", session.State, "", s.sealToken(turn.WorkingState), session.ExpiresAt, session.CreatedAt, session.UpdatedAt); err != nil {
			return GoalSession{}, err
		}
		checkpoint := GoalCheckpoint{ID: newGoalID("gcp"), GoalID: session.ID, Sequence: 1, Payload: turn.CheckpointPayload, CreatedAt: now}
		checkpoint.PayloadHash, checkpoint.PayloadBytes = hashGoalPayload(checkpoint.Payload), int64(len(checkpoint.Payload))
		checkpointEncrypted := s.sealToken(checkpoint.Payload)
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,created_at) VALUES(?,?,?,?,?,?,?,?)`,
			checkpoint.ID, checkpoint.GoalID, checkpoint.Sequence, 0, checkpoint.PayloadHash, checkpoint.PayloadBytes, checkpointEncrypted, checkpoint.CreatedAt); err != nil {
			return GoalSession{}, err
		}
		session.CurrentCheckpoint = checkpoint.ID
		nextSegment, nextCheckpoint = 1, 2
	} else {
		session = resolution.Session
		if strings.TrimSpace(session.Protocol) != turn.Protocol {
			return GoalSession{}, ErrGoalAmbiguous
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_segment WHERE goal_id=?`, session.ID).Scan(&nextSegment); err != nil {
			return GoalSession{}, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_checkpoint WHERE goal_id=?`, session.ID).Scan(&nextCheckpoint); err != nil {
			return GoalSession{}, err
		}
		_ = nextCheckpoint // retained for the post-commit compaction call.
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

	segment := GoalSegment{ID: newGoalID("gseg"), GoalID: session.ID, Sequence: nextSegment, Payload: turn.SegmentPayload, State: "committed", CreatedAt: now}
	segment.PayloadHash, segment.PayloadBytes = hashGoalPayload(segment.Payload), int64(len(segment.Payload))
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_segment(id,goal_id,sequence,payload_hash,payload_bytes,encrypted_payload,state,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		segment.ID, segment.GoalID, segment.Sequence, segment.PayloadHash, segment.PayloadBytes, s.sealToken(segment.Payload), segment.State, segment.CreatedAt); err != nil {
		return GoalSession{}, err
	}
	if turn.AwaitingTool {
		session.State = "awaiting_tool_result"
	} else {
		session.State = "ready"
	}
	if strings.TrimSpace(turn.ResponseID) != "" {
		session.LastResponseHash = hashGoalValue("response_id", turn.ResponseID)
	}
	working := s.sealToken(turn.WorkingState)
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_session(id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,expires_at,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET parent_goal_id=excluded.parent_goal_id, branch_hash=excluded.branch_hash, downstream_key_hash=excluded.downstream_key_hash, workspace_hash=excluded.workspace_hash, initial_goal_hash=excluded.initial_goal_hash, last_response_hash=excluded.last_response_hash, state=excluded.state, current_checkpoint_id=excluded.current_checkpoint_id, encrypted_working_state=excluded.encrypted_working_state, expires_at=excluded.expires_at, updated_at=excluded.updated_at`,
		session.ID, session.Protocol, session.ParentGoalID, session.BranchHash, session.DownstreamKeyHash, session.WorkspaceHash, session.InitialGoalHash, session.LastResponseHash, session.State, session.CurrentCheckpoint, working, session.ExpiresAt, session.CreatedAt, session.UpdatedAt); err != nil {
		return GoalSession{}, err
	}
	for _, alias := range aliases {
		if session.ParentGoalID != "" && alias.Type == "codex_root_thread" {
			// Root aliases are immutable ownership markers of the parent goal.  A
			// child stores only its branch alias and parent_goal_id.
			continue
		}
		// Never overwrite an alias owned by another goal.  That would silently merge
		// sibling agents after a client bug/retry; the resolution query above catches
		// normal conflicts before we get here.
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_alias(alias_hash,alias_type,goal_id,created_at) VALUES(?,?,?,?) ON CONFLICT(alias_hash) DO UPDATE SET alias_type=excluded.alias_type WHERE goal_alias.goal_id=excluded.goal_id`,
			hashGoalValue(alias.Type, alias.Value), alias.Type, session.ID, now); err != nil {
			return GoalSession{}, err
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
	if err := tx.Commit(); err != nil {
		return GoalSession{}, err
	}
	return session, nil
}

type goalReplaySegment struct {
	HistoryKey string      `json:"history_key"`
	Input      interface{} `json:"input"`
	Output     interface{} `json:"output"`
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

func goalHistoryKey(protocol string) string {
	if strings.EqualFold(strings.TrimSpace(protocol), "claude") || strings.EqualFold(strings.TrimSpace(protocol), "kiro") {
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
	if err := json.Unmarshal([]byte(checkpoint.Payload), &root); err != nil {
		return nil, session, fmt.Errorf("invalid goal checkpoint: %w", err)
	}
	historyKey := goalHistoryKey(session.Protocol)
	items := make([]interface{}, 0)
	items = appendGoalItems(items, root[historyKey])
	segments, err := s.listGoalSegmentsAfter(ctx, session.ID, checkpoint.ThroughSegmentSequence)
	if err != nil {
		return nil, session, err
	}
	for _, segment := range segments {
		var turn goalReplaySegment
		if err := json.Unmarshal([]byte(segment.Payload), &turn); err != nil {
			return nil, session, fmt.Errorf("invalid goal segment: %w", err)
		}
		if turn.HistoryKey != "" && turn.HistoryKey != historyKey {
			return nil, session, fmt.Errorf("goal segment protocol history mismatch: %s", turn.HistoryKey)
		}
		items = appendGoalItems(items, turn.Input)
		if historyKey == "messages" {
			items = append(items, claudeAssistantMessages(turn.Output)...)
		} else {
			items = appendGoalItems(items, turn.Output)
		}
	}
	root[historyKey] = items
	if historyKey == "messages" {
		delete(root, "input")
	} else {
		delete(root, "messages")
	}
	delete(root, "previous_response_id")
	delete(root, "turn_state")
	body, err := json.Marshal(root)
	return body, session, err
}

func (s *Store) GetGoalSession(ctx context.Context, id string) (GoalSession, error) {
	var item GoalSession
	var encrypted string
	err := s.rdb.QueryRowContext(ctx, `SELECT id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,encrypted_working_state,expires_at,created_at,updated_at FROM goal_session WHERE id=? AND expires_at>?`, id, Now()).Scan(
		&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash, &item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash, &item.State, &item.CurrentCheckpoint, &encrypted, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalSession{}, ErrGoalNotFound
		}
		return GoalSession{}, err
	}
	item.WorkingState = s.openToken(encrypted)
	return item, nil
}

func (s *Store) getGoalCheckpoint(ctx context.Context, id string) (GoalCheckpoint, error) {
	var item GoalCheckpoint
	var encrypted string
	err := s.rdb.QueryRowContext(ctx, `SELECT id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,created_at FROM goal_checkpoint WHERE id=?`, id).Scan(
		&item.ID, &item.GoalID, &item.Sequence, &item.ThroughSegmentSequence, &item.PayloadHash, &item.PayloadBytes, &encrypted, &item.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalCheckpoint{}, ErrGoalNotFound
		}
		return GoalCheckpoint{}, err
	}
	item.Payload = s.openToken(encrypted)
	return item, nil
}

func (s *Store) listGoalSegmentsAfter(ctx context.Context, goalID string, after int64) ([]GoalSegment, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id,goal_id,sequence,payload_hash,payload_bytes,encrypted_payload,state,created_at FROM goal_segment WHERE goal_id=? AND sequence>? ORDER BY sequence ASC`, goalID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GoalSegment
	for rows.Next() {
		var item GoalSegment
		var encrypted string
		if err := rows.Scan(&item.ID, &item.GoalID, &item.Sequence, &item.PayloadHash, &item.PayloadBytes, &encrypted, &item.State, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Payload = s.openToken(encrypted)
		out = append(out, item)
	}
	return out, rows.Err()
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
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return err
	}
	checkpoint, err := s.getGoalCheckpoint(ctx, session.CurrentCheckpoint)
	if err != nil {
		return err
	}
	segments, err := s.listGoalSegmentsAfter(ctx, goalID, checkpoint.ThroughSegmentSequence)
	if err != nil || len(segments) <= maxStages {
		return err
	}
	_ = s.InsertAuditLog(ctx, AuditLogRow{Action: "goal_compaction_started", State: "compacting", Reason: "segment_threshold", Detail: fmt.Sprintf("goal=%s segments=%d", goalID, len(segments))})
	var root map[string]interface{}
	if err := json.Unmarshal([]byte(checkpoint.Payload), &root); err != nil {
		return err
	}
	historyKey := goalHistoryKey(session.Protocol)
	items := make([]interface{}, 0)
	items = appendGoalItems(items, root[historyKey])
	var through int64 = checkpoint.ThroughSegmentSequence
	// Keep at most maxStages recent tail segments. A ratio below 1 bounds how much
	// of an oversized backlog one checkpoint transaction absorbs, making repeated
	// invocations resumable and predictable under the foreground budget.
	required := len(segments) - maxStages
	chunk := int(float64(maxStages)*chunkRatio + 0.999999)
	if chunk < 1 {
		chunk = 1
	}
	if required < chunk {
		chunk = required
	}
	for _, segment := range segments[:chunk] {
		var turn goalReplaySegment
		if err := json.Unmarshal([]byte(segment.Payload), &turn); err != nil {
			return err
		}
		if turn.HistoryKey != "" && turn.HistoryKey != historyKey {
			return fmt.Errorf("goal segment protocol history mismatch: %s", turn.HistoryKey)
		}
		items = appendGoalItems(items, turn.Input)
		if historyKey == "messages" {
			items = append(items, claudeAssistantMessages(turn.Output)...)
		} else {
			items = appendGoalItems(items, turn.Output)
		}
		through = segment.Sequence
	}
	root[historyKey] = items
	if historyKey == "messages" {
		delete(root, "input")
	} else {
		delete(root, "messages")
	}
	payload, err := json.Marshal(root)
	if err != nil {
		return err
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM goal_checkpoint WHERE goal_id=?`, goalID).Scan(&next); err != nil {
		return err
	}
	checkpoint = GoalCheckpoint{ID: newGoalID("gcp"), GoalID: goalID, Sequence: next, ThroughSegmentSequence: through, Payload: string(payload), PayloadHash: hashGoalPayload(string(payload)), PayloadBytes: int64(len(payload)), CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO goal_checkpoint(id,goal_id,sequence,through_segment_sequence,payload_hash,payload_bytes,encrypted_payload,created_at) VALUES(?,?,?,?,?,?,?,?)`, checkpoint.ID, checkpoint.GoalID, checkpoint.Sequence, checkpoint.ThroughSegmentSequence, checkpoint.PayloadHash, checkpoint.PayloadBytes, s.sealToken(checkpoint.Payload), checkpoint.CreatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM goal_segment WHERE goal_id=? AND sequence<=?`, goalID, through); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goal_session SET current_checkpoint_id=?, updated_at=? WHERE id=?`, checkpoint.ID, now, goalID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log(account_id,account_label,action,state,reason,detail,created_at) VALUES('', '', ?, ?, ?, ?, ?)`, "goal_compaction_completed", "ready", "incremental_checkpoint", fmt.Sprintf("goal=%s checkpoint=%s through_segment=%d chunk=%d", goalID, checkpoint.ID, through, chunk), now); err != nil {
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
	result, err := s.db.ExecContext(ctx, `UPDATE goal_run SET heartbeat_at=?,lease_expires_at=?,updated_at=? WHERE id=? AND lease_owner=? AND state IN ('running','compacting','awaiting_tool_result')`, now, now+int64(lease/time.Second), now, runID, owner)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrGoalNotFound
	}
	return nil
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
	result, err := s.db.ExecContext(ctx, `UPDATE goal_session SET state='retryable', updated_at=? WHERE id=? AND expires_at>?`, now, goalID, now)
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
	result, err := s.db.ExecContext(ctx, `UPDATE goal_session SET state=?,updated_at=? WHERE id=? AND expires_at>?`, state, now, strings.TrimSpace(goalID), now)
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
	rows, err := s.rdb.QueryContext(ctx, `SELECT id,protocol,parent_goal_id,branch_hash,downstream_key_hash,workspace_hash,initial_goal_hash,last_response_hash,state,current_checkpoint_id,expires_at,created_at,updated_at FROM goal_session ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GoalSession
	for rows.Next() {
		var item GoalSession
		if err := rows.Scan(&item.ID, &item.Protocol, &item.ParentGoalID, &item.BranchHash, &item.DownstreamKeyHash, &item.WorkspaceHash, &item.InitialGoalHash, &item.LastResponseHash, &item.State, &item.CurrentCheckpoint, &item.ExpiresAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
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
	if err := s.rdb.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?), (SELECT COUNT(*) FROM goal_segment WHERE goal_id=?), (SELECT COALESCE(SUM(payload_bytes),0) FROM goal_checkpoint WHERE goal_id=?) + (SELECT COALESCE(SUM(payload_bytes),0) FROM goal_segment WHERE goal_id=?)`, goalID, goalID, goalID, goalID).Scan(&detail.CheckpointCount, &detail.SegmentCount, &detail.PayloadBytes); err != nil {
		return GoalDetail{}, err
	}
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
	if err := s.rdb.QueryRowContext(ctx, `SELECT (SELECT COALESCE(SUM(payload_bytes),0) FROM goal_checkpoint) + (SELECT COALESCE(SUM(payload_bytes),0) FROM goal_segment)`).Scan(&metrics.StorageBytes); err != nil {
		return GoalMetrics{}, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT action,COUNT(*) FROM audit_log WHERE action IN ('goal_resume_recovered','goal_resume_ambiguous','goal_stream_terminal_synthesized','goal_persistence_degraded') GROUP BY action`)
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
		}
	}
	return metrics, rows.Err()
}

func (s *Store) DeleteGoalSafely(ctx context.Context, goalID string) error {
	session, err := s.GetGoalSession(ctx, goalID)
	if err != nil {
		return err
	}
	var activeRuns int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_run WHERE goal_id=? AND state IN ('running','compacting','awaiting_tool_result') AND lease_expires_at>?`, goalID, Now()).Scan(&activeRuns); err != nil {
		return err
	}
	if activeGoalRunState(session.State) || session.State == "awaiting_tool_result" || activeRuns > 0 {
		return ErrGoalActiveCannotBePurged
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM goal_session WHERE id=?`, goalID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrGoalNotFound
	}
	return s.InsertAuditLog(ctx, AuditLogRow{Action: "goal_cleaned", State: "deleted", Reason: "admin_safe_cleanup", Detail: "goal=" + goalID})
}

// CleanupGoalContinuity removes only expired work that has no live run.  It is safe to
// call on every maintenance pass and returns the number of sessions reclaimed.
func (s *Store) CleanupGoalContinuity(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM goal_session WHERE expires_at<=? AND id NOT IN (SELECT goal_id FROM goal_run WHERE state IN ('running','compacting','awaiting_tool_result') AND lease_expires_at>?)`, Now(), Now())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
