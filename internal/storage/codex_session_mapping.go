package storage

// Durable, privacy-preserving identity bindings for native Codex Responses
// sessions.  The upstream owns conversation context; this table deliberately
// stores only the small amount of routing/identity metadata necessary to send a
// later previous_response_id back through the exact same account, egress and
// virtual Codex identity.  It never stores prompt/input/output bodies.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-account-pool/internal/secretbox"
)

const codexSessionMappingSchemaSQL = `
CREATE TABLE IF NOT EXISTS codex_session_binding(
  id TEXT PRIMARY KEY,
  tree_id TEXT NOT NULL,
  namespace_hash TEXT NOT NULL,
  account_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  epoch INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'active',
  encrypted_identity TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codex_session_binding_tree ON codex_session_binding(tree_id, state, epoch);
CREATE INDEX IF NOT EXISTS idx_codex_session_binding_expiry ON codex_session_binding(expires_at, state);
CREATE TABLE IF NOT EXISTS codex_session_alias(
  alias_hash TEXT NOT NULL,
  alias_type TEXT NOT NULL,
  binding_id TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY(alias_hash, binding_id),
  FOREIGN KEY(binding_id) REFERENCES codex_session_binding(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_codex_session_alias_lookup ON codex_session_alias(alias_hash, expires_at, binding_id);
CREATE INDEX IF NOT EXISTS idx_codex_session_alias_binding ON codex_session_alias(binding_id);
CREATE TABLE IF NOT EXISTS codex_instruction_snapshot(
  tree_id TEXT PRIMARY KEY,
  encrypted_instructions TEXT NOT NULL,
  revision_hmac TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codex_instruction_snapshot_expiry ON codex_instruction_snapshot(expires_at);
CREATE TABLE IF NOT EXISTS codex_upstream_attempt(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  tree_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  epoch INTEGER NOT NULL,
  state TEXT NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_codex_upstream_attempt_expiry ON codex_upstream_attempt(expires_at, created_at);
CREATE INDEX IF NOT EXISTS idx_codex_upstream_attempt_tree ON codex_upstream_attempt(tree_id, created_at);
CREATE TABLE IF NOT EXISTS codex_upstream_attempt_daily(
  day_start INTEGER NOT NULL,
  account_id TEXT NOT NULL,
  egress_id TEXT NOT NULL,
  state TEXT NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  attempt_count INTEGER NOT NULL,
  first_created_at INTEGER NOT NULL,
  last_created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY(day_start, account_id, egress_id, state, status_code)
);
CREATE INDEX IF NOT EXISTS idx_codex_upstream_attempt_daily_expiry ON codex_upstream_attempt_daily(expires_at, day_start);
`

var (
	ErrCodexSessionMappingNotFound      = errors.New("codex session mapping not found")
	ErrCodexSessionMappingAmbiguous     = errors.New("codex session mapping is ambiguous")
	ErrCodexSessionEpochRetired         = errors.New("codex session context epoch retired")
	ErrCodexSessionEpochConflict        = errors.New("codex session context epoch changed")
	ErrCodexInstructionSnapshotNotFound = errors.New("codex instruction snapshot not found")
)

const defaultCodexSessionCleanupBatch = 256

// CodexSessionAlias is plaintext only at the API/storage boundary.  Store methods
// HMAC it before it reaches SQLite.  Callers must not log Value.
type CodexSessionAlias struct {
	Type  string
	Value string
}

// CodexSessionBinding is the decrypted identity snapshot returned to the relay.
// None of its internal UUID fields are stored in clear text.  Account/egress are
// routing metadata rather than downstream correlators and intentionally remain
// queryable so an exact stateful continuation can be selected without guessing.
type CodexSessionBinding struct {
	ID        string
	TreeID    string
	AccountID string
	EgressID  string
	Epoch     int64
	State     string

	// InstallationID and DeviceOSHint describe the virtual Codex device selected
	// for this upstream tree. They are stored only inside encrypted_identity: a
	// continuation must not recompute either from a later request's OS-shaped
	// input. DeviceOSHintSet distinguishes an elected host-default hint ("") from
	// a legacy mapping that predates this field.
	InstallationID     string
	DeviceOSHint       string
	DeviceOSHintSet    bool
	RootSessionID      string
	ThreadID           string
	ParentThreadID     string
	ForkedFromThreadID string
	WindowGeneration   int64
	// GoalModeActive and GoalTurnID are encrypted with the rest of the native
	// identity. They latch only an observed Codex Goal continuation turn; merely
	// exposing the Goal tools never sets them. Keeping the downstream turn id lets
	// tool-output requests in that same turn retain quota grace without allowing an
	// old Goal marker in full history to affect a later ordinary user turn.
	GoalModeActive bool
	GoalTurnID     string
	CreatedAt      int64
	UpdatedAt      int64
	ExpiresAt      int64
}

// CodexSessionCommit atomically creates/refreshes a mapping and attaches aliases
// observed only after a successful upstream terminal.  Binding may be an existing
// row (for a normal continuation) or an in-memory prospective row (first root / a
// new child branch).  ResponseID and TurnState are intentionally aliases rather
// than columns, which makes every stateful resume use the same exact lookup path.
type CodexSessionCommit struct {
	Namespace           string
	Binding             CodexSessionBinding
	Aliases             []CodexSessionAlias
	ExpiresAt           int64
	InstructionSnapshot *CodexInstructionSnapshot
	// DropConflictingHierarchyAliases contains legacy/concurrent damage without
	// weakening terminal state pointers. When a commit also owns a response alias,
	// root/session/branch aliases already held by another active binding are skipped
	// atomically; response and turn-state collisions remain hard failures.
	DropConflictingHierarchyAliases bool
	// DroppedHierarchyAliases receives the number skipped after a successful
	// transaction. It is optional and used only for aggregate audit telemetry.
	DroppedHierarchyAliases *int
}

// CodexInstructionSnapshot is the tree-scoped, encrypted base-instructions
// snapshot used by native Codex CPA sessions. Instructions deliberately never
// appear in a mapping, alias, audit row, or diagnostic export in plaintext.
// Revision is a domain-separated HMAC, not a content hash exposed to clients.
type CodexInstructionSnapshot struct {
	TreeID       string
	Instructions string
	Revision     string
	CreatedAt    int64
	UpdatedAt    int64
	ExpiresAt    int64
}

// CodexUpstreamAttempt is intentionally metadata-only. Durable CPA traffic uses its
// tree id; stateless traffic uses a server-owned per-turn id. Both let a support
// bundle correlate transport/account/real-exit outcomes without retaining input,
// output, aliases, or upstream ids.
type CodexUpstreamAttempt struct {
	EventID    string
	TreeID     string
	AccountID  string
	EgressID   string
	Epoch      int64
	State      string
	StatusCode int
	CreatedAt  int64
	ExpiresAt  int64
}

// CodexEgressRecentOutcome is a bounded-window, exit-attributable quality
// aggregate used by fresh Codex routing. Attempts are classified outcomes (a
// completed model terminal or an explicit network/edge failure), not raw starts.
// Account quota/auth/context responses and downstream cancellation therefore do
// not silently become egress failures. Successes are completed model terminals on
// the real exit that produced the terminal.
type CodexEgressRecentOutcome struct {
	EgressID  string
	Attempts  int64
	Successes int64
}

type codexSessionIdentityPayload struct {
	InstallationID     string `json:"installation_id,omitempty"`
	DeviceOSHint       string `json:"device_os_hint,omitempty"`
	DeviceOSHintSet    bool   `json:"device_os_hint_set,omitempty"`
	RootSessionID      string `json:"root_session_id"`
	ThreadID           string `json:"thread_id"`
	ParentThreadID     string `json:"parent_thread_id,omitempty"`
	ForkedFromThreadID string `json:"forked_from_thread_id,omitempty"`
	WindowGeneration   int64  `json:"window_generation"`
	GoalModeActive     bool   `json:"goal_mode_active,omitempty"`
	GoalTurnID         string `json:"goal_turn_id,omitempty"`
}

func normalizedCodexSessionAliases(in []CodexSessionAlias) []CodexSessionAlias {
	seen := make(map[string]bool, len(in))
	out := make([]CodexSessionAlias, 0, len(in))
	for _, alias := range in {
		alias.Type = strings.ToLower(strings.TrimSpace(alias.Type))
		alias.Value = strings.TrimSpace(alias.Value)
		if alias.Type == "" || alias.Value == "" {
			continue
		}
		key := alias.Type + "\x00" + alias.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, alias)
	}
	return out
}

func newCodexSessionMappingID(prefix string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err == nil {
		return prefix + "_" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("%s_%x", prefix, time.Now().UnixNano())
}

// codexSessionMappingKey is intentionally independent from the optional legacy
// token-encryption setup.  Production stores receive tokenKey at boot; the fallback
// keeps this new table encrypted even in in-memory/unit-test stores instead of
// silently persisting internal UUIDs in clear text.
func (s *Store) codexSessionMappingKey() []byte {
	if len(s.tokenKey) == 32 {
		return append([]byte(nil), s.tokenKey...)
	}
	return secretbox.DeriveKey([]byte("micliproxy/codex-session-mapping/v1/fallback"))
}

func (s *Store) codexSessionAliasHash(namespace, typ, value string) string {
	mac := hmac.New(sha256.New, s.codexSessionMappingKey())
	_, _ = mac.Write([]byte("codex-session-alias\x00"))
	_, _ = mac.Write([]byte(strings.TrimSpace(namespace)))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(strings.ToLower(strings.TrimSpace(typ))))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) codexSessionNamespaceHash(namespace string) string {
	return s.codexSessionAliasHash(namespace, "namespace", "")
}

func (s *Store) codexInstructionRevision(instructions string) string {
	mac := hmac.New(sha256.New, s.codexSessionMappingKey())
	_, _ = mac.Write([]byte("codex-instruction-snapshot-revision/v1\x00"))
	_, _ = mac.Write([]byte(instructions))
	return hex.EncodeToString(mac.Sum(nil))
}

// CodexInstructionRevision exposes the opaque revision used by safe operational
// diagnostics. It is intentionally HMAC-derived so equal instruction text cannot
// be tested offline from an exported revision value.
func (s *Store) CodexInstructionRevision(instructions string) string {
	return s.codexInstructionRevision(instructions)
}

// CodexGroupPolicyRevision returns an opaque, domain-separated revision for
// safe diagnostics. Its caller supplies configuration metadata only (never the
// instruction file contents), so support bundles can correlate policy changes
// without creating an offline oracle for either configuration or prompt text.
func (s *Store) CodexGroupPolicyRevision(policy string) string {
	mac := hmac.New(sha256.New, s.codexSessionMappingKey())
	_, _ = mac.Write([]byte("codex-group-policy-revision/v1\x00"))
	_, _ = mac.Write([]byte(policy))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Store) sealCodexInstructionSnapshot(instructions string) (string, error) {
	return secretbox.Seal(s.codexSessionMappingKey(), instructions)
}

func (s *Store) openCodexInstructionSnapshot(value string) (string, error) {
	return secretbox.Open(s.codexSessionMappingKey(), value)
}

func scanCodexInstructionSnapshot(s *Store, scanner interface{ Scan(...interface{}) error }) (CodexInstructionSnapshot, error) {
	var snapshot CodexInstructionSnapshot
	var encrypted string
	if err := scanner.Scan(&snapshot.TreeID, &encrypted, &snapshot.Revision, &snapshot.CreatedAt, &snapshot.UpdatedAt, &snapshot.ExpiresAt); err != nil {
		return CodexInstructionSnapshot{}, err
	}
	instructions, err := s.openCodexInstructionSnapshot(encrypted)
	if err != nil {
		return CodexInstructionSnapshot{}, err
	}
	snapshot.Instructions = instructions
	return snapshot, nil
}

const codexInstructionSnapshotColumns = `tree_id,encrypted_instructions,revision_hmac,created_at,updated_at,expires_at`

func (s *Store) sealCodexSessionIdentity(binding CodexSessionBinding) (string, error) {
	payload, err := json.Marshal(codexSessionIdentityPayload{
		InstallationID:     binding.InstallationID,
		DeviceOSHint:       binding.DeviceOSHint,
		DeviceOSHintSet:    binding.DeviceOSHintSet,
		RootSessionID:      binding.RootSessionID,
		ThreadID:           binding.ThreadID,
		ParentThreadID:     binding.ParentThreadID,
		ForkedFromThreadID: binding.ForkedFromThreadID,
		WindowGeneration:   binding.WindowGeneration,
		GoalModeActive:     binding.GoalModeActive,
		GoalTurnID:         binding.GoalTurnID,
	})
	if err != nil {
		return "", err
	}
	return secretbox.Seal(s.codexSessionMappingKey(), string(payload))
}

func (s *Store) openCodexSessionIdentity(value string, binding *CodexSessionBinding) error {
	plain, err := secretbox.Open(s.codexSessionMappingKey(), value)
	if err != nil {
		return err
	}
	var payload codexSessionIdentityPayload
	if err := json.Unmarshal([]byte(plain), &payload); err != nil {
		return err
	}
	binding.InstallationID = strings.TrimSpace(payload.InstallationID)
	binding.DeviceOSHint = strings.TrimSpace(payload.DeviceOSHint)
	binding.DeviceOSHintSet = payload.DeviceOSHintSet
	binding.RootSessionID = strings.TrimSpace(payload.RootSessionID)
	binding.ThreadID = strings.TrimSpace(payload.ThreadID)
	binding.ParentThreadID = strings.TrimSpace(payload.ParentThreadID)
	binding.ForkedFromThreadID = strings.TrimSpace(payload.ForkedFromThreadID)
	binding.WindowGeneration = payload.WindowGeneration
	binding.GoalModeActive = payload.GoalModeActive
	binding.GoalTurnID = strings.TrimSpace(payload.GoalTurnID)
	if binding.RootSessionID == "" || binding.ThreadID == "" {
		return errors.New("codex session mapping identity incomplete")
	}
	return nil
}

func scanCodexSessionBinding(s *Store, scanner interface{ Scan(...interface{}) error }) (CodexSessionBinding, error) {
	var binding CodexSessionBinding
	var namespaceHash string
	var encrypted string
	if err := scanner.Scan(&binding.ID, &binding.TreeID, &namespaceHash, &binding.AccountID, &binding.EgressID,
		&binding.Epoch, &binding.State, &encrypted, &binding.CreatedAt, &binding.UpdatedAt, &binding.ExpiresAt); err != nil {
		return CodexSessionBinding{}, err
	}
	if err := s.openCodexSessionIdentity(encrypted, &binding); err != nil {
		return CodexSessionBinding{}, err
	}
	return binding, nil
}

const codexSessionBindingColumns = `id,tree_id,namespace_hash,account_id,egress_id,epoch,state,encrypted_identity,created_at,updated_at,expires_at`
const codexSessionBindingColumnsQualified = `b.id,b.tree_id,b.namespace_hash,b.account_id,b.egress_id,b.epoch,b.state,b.encrypted_identity,b.created_at,b.updated_at,b.expires_at`

// FindCodexSessionAlias resolves one exact downstream alias in one authentication
// namespace.  It intentionally returns retired rows too: callers need to surface a
// deterministic context-epoch-retired error instead of treating an old state token
// as a brand-new conversation.
func (s *Store) FindCodexSessionAlias(ctx context.Context, namespace string, alias CodexSessionAlias) ([]CodexSessionBinding, error) {
	aliases := normalizedCodexSessionAliases([]CodexSessionAlias{alias})
	if strings.TrimSpace(namespace) == "" || len(aliases) == 0 {
		return nil, ErrCodexSessionMappingNotFound
	}
	aliasHash := s.codexSessionAliasHash(namespace, aliases[0].Type, aliases[0].Value)
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+codexSessionBindingColumnsQualified+`
FROM codex_session_alias a JOIN codex_session_binding b ON b.id=a.binding_id
WHERE a.alias_hash=? AND a.expires_at>? AND b.expires_at>? ORDER BY b.updated_at DESC, b.id ASC`, aliasHash, Now(), Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := make([]CodexSessionBinding, 0, 1)
	for rows.Next() {
		binding, scanErr := scanCodexSessionBinding(s, rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if !seen[binding.ID] {
			seen[binding.ID] = true
			out = append(out, binding)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrCodexSessionMappingNotFound
	}
	return out, nil
}

// ResolveCodexSessionAliases resolves a set of exact aliases. Response ids remain
// globally unique within a namespace. Codex turn-state tokens are different: recent
// clients can reuse one token across sibling branches, so a unique response id may
// disambiguate a shared turn state but a shared turn state alone remains ambiguous.
func (s *Store) ResolveCodexSessionAliases(ctx context.Context, namespace string, aliases []CodexSessionAlias) (CodexSessionBinding, error) {
	aliases = normalizedCodexSessionAliases(aliases)
	if len(aliases) == 0 {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	var chosen *CodexSessionBinding
	// Resolve strict aliases before shared turn-state tokens so disambiguation does
	// not depend on the caller's JSON/header field order.
	for pass := 0; pass < 2; pass++ {
		for _, alias := range aliases {
			turnState := alias.Type == "turn_state"
			if turnState != (pass == 1) {
				continue
			}
			rows, err := s.FindCodexSessionAlias(ctx, namespace, alias)
			if errors.Is(err, ErrCodexSessionMappingNotFound) {
				// Stateful aliases are a set, not hints. Accepting an unknown
				// turn-state beside a known previous_response_id would graft context
				// from two sessions together.
				return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
			}
			if err != nil {
				return CodexSessionBinding{}, err
			}
			if !turnState {
				if len(rows) != 1 {
					return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
				}
				if chosen == nil {
					copy := rows[0]
					chosen = &copy
					continue
				}
				if chosen.ID != rows[0].ID {
					return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
				}
				continue
			}
			if chosen == nil {
				if len(rows) != 1 {
					return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
				}
				copy := rows[0]
				chosen = &copy
				continue
			}
			matched := false
			for _, row := range rows {
				if row.ID == chosen.ID {
					matched = true
					break
				}
			}
			if !matched {
				return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
			}
		}
	}
	if chosen == nil {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	if chosen.State != "active" {
		return *chosen, ErrCodexSessionEpochRetired
	}
	return *chosen, nil
}

// GetCodexInstructionSnapshot resolves one tree's immutable administrator
// instruction snapshot. An expired row is treated as absent; callers can then
// safely create a fresh root rather than resurrecting stale session policy.
func (s *Store) GetCodexInstructionSnapshot(ctx context.Context, treeID string) (CodexInstructionSnapshot, error) {
	treeID = strings.TrimSpace(treeID)
	if treeID == "" {
		return CodexInstructionSnapshot{}, ErrCodexInstructionSnapshotNotFound
	}
	row := s.rdb.QueryRowContext(ctx, `SELECT `+codexInstructionSnapshotColumns+`
FROM codex_instruction_snapshot WHERE tree_id=? AND expires_at>?`, treeID, Now())
	snapshot, err := scanCodexInstructionSnapshot(s, row)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexInstructionSnapshot{}, ErrCodexInstructionSnapshotNotFound
	}
	return snapshot, err
}

// ensureCodexInstructionSnapshotTx is the tree-level compare-and-set used both
// by lazy migration and terminal mapping commits. The first writer determines a
// tree's instruction text forever (until expiry); concurrent branches receive
// that exact stored value instead of racing current group files into the tree.
func (s *Store) ensureCodexInstructionSnapshotTx(ctx context.Context, tx *sql.Tx, candidate CodexInstructionSnapshot, freshTree bool) (CodexInstructionSnapshot, error) {
	candidate.TreeID = strings.TrimSpace(candidate.TreeID)
	if candidate.TreeID == "" {
		return CodexInstructionSnapshot{}, ErrCodexInstructionSnapshotNotFound
	}
	now := Now()
	if candidate.ExpiresAt <= now {
		candidate.ExpiresAt = now + int64((7*24*time.Hour)/time.Second)
	}
	candidate.Instructions = strings.TrimSpace(candidate.Instructions)
	if candidate.Revision == "" {
		candidate.Revision = s.codexInstructionRevision(candidate.Instructions)
	}
	encrypted, err := s.sealCodexInstructionSnapshot(candidate.Instructions)
	if err != nil {
		return CodexInstructionSnapshot{}, err
	}
	if candidate.CreatedAt == 0 {
		candidate.CreatedAt = now
	}
	candidate.UpdatedAt = now
	if freshTree {
		// A fresh root/fork receives a cryptographically random tree id in the same
		// transaction as its first binding. No previous snapshot can own that id, so
		// the general conflict/read-back/TTL-refresh path is redundant. A collision
		// remains a hard transaction error instead of silently changing policy.
		_, err = tx.ExecContext(ctx, `INSERT INTO codex_instruction_snapshot(`+codexInstructionSnapshotColumns+`)
VALUES(?,?,?,?,?,?)`, candidate.TreeID, encrypted, candidate.Revision,
			candidate.CreatedAt, candidate.UpdatedAt, candidate.ExpiresAt)
		return candidate, err
	}
	// The conflict update intentionally fires only for an expired snapshot. This
	// is a CAS, not a configuration refresh: active trees must never observe file
	// edits made after their root turn began.
	if _, err := tx.ExecContext(ctx, `INSERT INTO codex_instruction_snapshot(`+codexInstructionSnapshotColumns+`)
VALUES(?,?,?,?,?,?)
ON CONFLICT(tree_id) DO UPDATE SET
 encrypted_instructions=excluded.encrypted_instructions,
 revision_hmac=excluded.revision_hmac,
 created_at=excluded.created_at,
 updated_at=excluded.updated_at,
 expires_at=excluded.expires_at
WHERE codex_instruction_snapshot.expires_at<=?`, candidate.TreeID, encrypted, candidate.Revision,
		candidate.CreatedAt, candidate.UpdatedAt, candidate.ExpiresAt, now); err != nil {
		return CodexInstructionSnapshot{}, err
	}
	row := tx.QueryRowContext(ctx, `SELECT `+codexInstructionSnapshotColumns+` FROM codex_instruction_snapshot WHERE tree_id=?`, candidate.TreeID)
	stored, err := scanCodexInstructionSnapshot(s, row)
	if err != nil {
		return CodexInstructionSnapshot{}, err
	}
	if stored.ExpiresAt > now {
		// A successful use slides the TTL but never changes the stored content or
		// revision. This also refreshes a concurrently-created snapshot safely.
		expiresAt := candidate.ExpiresAt
		if expiresAt > stored.ExpiresAt {
			stored.ExpiresAt = expiresAt
		}
		stored.UpdatedAt = now
		if _, err := tx.ExecContext(ctx, `UPDATE codex_instruction_snapshot SET updated_at=?,expires_at=? WHERE tree_id=?`, stored.UpdatedAt, stored.ExpiresAt, stored.TreeID); err != nil {
			return CodexInstructionSnapshot{}, err
		}
	}
	return stored, nil
}

// EnsureCodexInstructionSnapshot lazily migrates legacy CPA trees. It is safe to
// call before a stateful continuation: a single SQLite transaction elects one
// snapshot for all concurrent child branches and returns the elected value.
func (s *Store) EnsureCodexInstructionSnapshot(ctx context.Context, candidate CodexInstructionSnapshot) (CodexInstructionSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodexInstructionSnapshot{}, err
	}
	defer tx.Rollback()
	stored, err := s.ensureCodexInstructionSnapshotTx(ctx, tx, candidate, false)
	if err != nil {
		return CodexInstructionSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodexInstructionSnapshot{}, err
	}
	return stored, nil
}

// CommitCodexSessionBinding is deliberately terminal-only.  Alias ownership is
// checked inside the same SQLite transaction as the binding write, so concurrent
// requests cannot silently make a response id point at two active identities.
func (s *Store) CommitCodexSessionBinding(ctx context.Context, commit CodexSessionCommit) (CodexSessionBinding, error) {
	if strings.TrimSpace(commit.Namespace) == "" {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	aliases := normalizedCodexSessionAliases(commit.Aliases)
	hasResponseAlias := false
	for _, alias := range aliases {
		if alias.Type == "response" && strings.TrimSpace(alias.Value) != "" {
			hasResponseAlias = true
			break
		}
	}
	binding := commit.Binding
	freshBindingID := binding.ID == ""
	freshTreeID := binding.TreeID == ""
	if binding.ID == "" {
		binding.ID = newCodexSessionMappingID("csm")
	}
	if binding.TreeID == "" {
		binding.TreeID = newCodexSessionMappingID("cst")
	}
	if binding.State == "" {
		binding.State = "active"
	}
	if binding.RootSessionID == "" || binding.ThreadID == "" || binding.AccountID == "" || binding.EgressID == "" {
		return CodexSessionBinding{}, errors.New("codex session binding incomplete")
	}
	now := Now()
	if binding.CreatedAt == 0 {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	if commit.ExpiresAt > now {
		binding.ExpiresAt = commit.ExpiresAt
	}
	if binding.ExpiresAt <= now {
		binding.ExpiresAt = now + int64((7*24*time.Hour)/time.Second)
	}
	encrypted, err := s.sealCodexSessionIdentity(binding)
	if err != nil {
		return CodexSessionBinding{}, err
	}
	namespaceHash := s.codexSessionNamespaceHash(commit.Namespace)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodexSessionBinding{}, err
	}
	defer tx.Rollback()

	bindingInserted := false
	if freshBindingID {
		if _, err = tx.ExecContext(ctx, `INSERT INTO codex_session_binding(`+codexSessionBindingColumns+`)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, binding.ID, binding.TreeID, namespaceHash, binding.AccountID, binding.EgressID,
			binding.Epoch, binding.State, encrypted, binding.CreatedAt, binding.UpdatedAt, binding.ExpiresAt); err != nil {
			return CodexSessionBinding{}, err
		}
		bindingInserted = true
	} else {
		var existingEpoch int64
		var existingState string
		err = tx.QueryRowContext(ctx, `SELECT epoch,state FROM codex_session_binding WHERE id=?`, binding.ID).Scan(&existingEpoch, &existingState)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err = tx.ExecContext(ctx, `INSERT INTO codex_session_binding(`+codexSessionBindingColumns+`)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, binding.ID, binding.TreeID, namespaceHash, binding.AccountID, binding.EgressID,
				binding.Epoch, binding.State, encrypted, binding.CreatedAt, binding.UpdatedAt, binding.ExpiresAt); err != nil {
				return CodexSessionBinding{}, err
			}
			bindingInserted = true
		case err != nil:
			return CodexSessionBinding{}, err
		case existingState != "active":
			return CodexSessionBinding{}, ErrCodexSessionEpochRetired
		case existingEpoch != binding.Epoch:
			return CodexSessionBinding{}, ErrCodexSessionEpochConflict
		default:
			result, updateErr := tx.ExecContext(ctx, `UPDATE codex_session_binding
SET namespace_hash=?,account_id=?,egress_id=?,state=?,encrypted_identity=?,updated_at=?,expires_at=?
WHERE id=? AND epoch=? AND state='active'`, namespaceHash, binding.AccountID, binding.EgressID, binding.State,
				encrypted, binding.UpdatedAt, binding.ExpiresAt, binding.ID, binding.Epoch)
			if updateErr != nil {
				return CodexSessionBinding{}, updateErr
			}
			changed, _ := result.RowsAffected()
			if changed != 1 {
				return CodexSessionBinding{}, ErrCodexSessionEpochConflict
			}
		}
	}
	if commit.InstructionSnapshot != nil {
		snapshot := *commit.InstructionSnapshot
		snapshot.TreeID = binding.TreeID
		snapshot.ExpiresAt = binding.ExpiresAt
		if _, err := s.ensureCodexInstructionSnapshotTx(ctx, tx, snapshot, freshTreeID); err != nil {
			return CodexSessionBinding{}, err
		}
	}

	droppedHierarchyAliases := 0
	for _, alias := range aliases {
		hash := s.codexSessionAliasHash(commit.Namespace, alias.Type, alias.Value)
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO codex_session_alias(alias_hash,alias_type,binding_id,created_at,updated_at,expires_at)
SELECT ?,?,?,?,?,?
WHERE ?='turn_state' OR NOT EXISTS (
 SELECT 1 FROM codex_session_alias a
 JOIN codex_session_binding b ON b.id=a.binding_id
 WHERE a.alias_hash=? AND a.expires_at>? AND b.expires_at>? AND b.state='active' AND b.id<>?
)
ON CONFLICT(alias_hash,binding_id) DO UPDATE SET updated_at=excluded.updated_at,expires_at=excluded.expires_at`,
			hash, alias.Type, binding.ID, now, now, binding.ExpiresAt,
			alias.Type, hash, now, now, binding.ID)
		if insertErr != nil {
			return CodexSessionBinding{}, insertErr
		}
		if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
			return CodexSessionBinding{}, rowsErr
		} else if alias.Type != "turn_state" && changed == 0 {
			droppableHierarchy := commit.DropConflictingHierarchyAliases && hasResponseAlias &&
				(alias.Type == "root" || alias.Type == "session" || alias.Type == "branch")
			if droppableHierarchy {
				droppedHierarchyAliases++
				continue
			}
			return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
		}
	}
	// Newly inserted aliases already carry the terminal expiry. Existing bindings
	// still need a sliding refresh for aliases omitted by this particular turn.
	if !bindingInserted {
		if _, err := tx.ExecContext(ctx, `UPDATE codex_session_alias SET updated_at=?,expires_at=? WHERE binding_id=?`, now, binding.ExpiresAt, binding.ID); err != nil {
			return CodexSessionBinding{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return CodexSessionBinding{}, err
	}
	if commit.DroppedHierarchyAliases != nil {
		*commit.DroppedHierarchyAliases = droppedHierarchyAliases
	}
	return binding, nil
}

// AdvanceCodexSessionWindowGeneration performs the only allowed window increment:
// after an upstream compaction reached a successful terminal.  The epoch CAS makes
// two concurrent compaction completions advance at most once per observed version.
func (s *Store) AdvanceCodexSessionWindowGeneration(ctx context.Context, bindingID string, expectedEpoch, expectedGeneration int64, expiresAt int64) (CodexSessionBinding, error) {
	if strings.TrimSpace(bindingID) == "" {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CodexSessionBinding{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+codexSessionBindingColumns+` FROM codex_session_binding WHERE id=?`, bindingID)
	binding, err := scanCodexSessionBinding(s, row)
	if errors.Is(err, sql.ErrNoRows) {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	if err != nil {
		return CodexSessionBinding{}, err
	}
	if binding.State != "active" {
		return binding, ErrCodexSessionEpochRetired
	}
	if binding.Epoch != expectedEpoch || binding.WindowGeneration != expectedGeneration {
		return binding, ErrCodexSessionEpochConflict
	}
	binding.WindowGeneration++
	binding.UpdatedAt = Now()
	if expiresAt > binding.UpdatedAt {
		binding.ExpiresAt = expiresAt
	}
	encrypted, err := s.sealCodexSessionIdentity(binding)
	if err != nil {
		return CodexSessionBinding{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE codex_session_binding SET encrypted_identity=?,updated_at=?,expires_at=?
WHERE id=? AND epoch=? AND state='active'`, encrypted, binding.UpdatedAt, binding.ExpiresAt, binding.ID, expectedEpoch)
	if err != nil {
		return CodexSessionBinding{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return CodexSessionBinding{}, ErrCodexSessionEpochConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE codex_session_alias SET updated_at=?,expires_at=? WHERE binding_id=?`, binding.UpdatedAt, binding.ExpiresAt, binding.ID); err != nil {
		return CodexSessionBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodexSessionBinding{}, err
	}
	return binding, nil
}

// RetireCodexSessionTree atomically retires every branch of the root tree.  The
// caller may then create a new root tree for a self-contained request; a request
// carrying old response/turn-state aliases sees the retired epoch and is never
// silently migrated across accounts.
func (s *Store) RetireCodexSessionTree(ctx context.Context, bindingID string, expectedEpoch int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var treeID string
	var epoch int64
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT tree_id,epoch,state FROM codex_session_binding WHERE id=?`, bindingID).Scan(&treeID, &epoch, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrCodexSessionMappingNotFound
		}
		return 0, err
	}
	if state != "active" {
		return 0, ErrCodexSessionEpochRetired
	}
	if epoch != expectedEpoch {
		return 0, ErrCodexSessionEpochConflict
	}
	now := Now()
	result, err := tx.ExecContext(ctx, `UPDATE codex_session_binding SET state='retired',epoch=epoch+1,updated_at=?
WHERE tree_id=? AND state='active'`, now, treeID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	changed, _ := result.RowsAffected()
	return changed, nil
}

// CleanupCodexSessionMappings removes only expired metadata and their HMAC aliases.
// It is safe to invoke opportunistically; no prompt or response content is involved.
func (s *Store) CleanupCodexSessionMappings(ctx context.Context) (int64, error) {
	if _, err := s.CleanupAffinityAliases(ctx, 256); err != nil {
		return 0, err
	}
	now := Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Aggregate and delete one bounded detail batch in the same transaction. The
	// production table can exceed 100k rows; selecting stable IDs prevents a long
	// writer monopoly while preserving exactly-once daily counts under retries.
	if _, err := cleanupExpiredCodexUpstreamAttempts(ctx, tx, now, defaultCodexSessionCleanupBatch); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM codex_upstream_attempt_daily
WHERE (day_start,account_id,egress_id,state,status_code) IN (
 SELECT day_start,account_id,egress_id,state,status_code
 FROM codex_upstream_attempt_daily
 WHERE expires_at<=?
 ORDER BY expires_at,day_start,account_id,egress_id,state,status_code
 LIMIT ?
)`, now, defaultCodexSessionCleanupBatch); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM codex_session_binding WHERE id IN (
 SELECT id FROM codex_session_binding WHERE expires_at<=? ORDER BY expires_at,id LIMIT ?
)`, now, defaultCodexSessionCleanupBatch)
	if err != nil {
		return 0, err
	}
	// SQLite foreign keys may be disabled in an externally-opened legacy DB. Keep
	// aliases tidy there too, but cap orphan detection and deletion to one batch.
	if _, err := tx.ExecContext(ctx, `DELETE FROM codex_session_alias
WHERE (alias_hash,binding_id) IN (
 SELECT a.alias_hash,a.binding_id FROM codex_session_alias a
 WHERE a.expires_at<=? OR NOT EXISTS (SELECT 1 FROM codex_session_binding b WHERE b.id=a.binding_id)
 ORDER BY a.expires_at,a.alias_hash,a.binding_id
 LIMIT ?
)`, now, defaultCodexSessionCleanupBatch); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM codex_instruction_snapshot WHERE tree_id IN (
 SELECT i.tree_id FROM codex_instruction_snapshot i
 WHERE i.expires_at<=? OR NOT EXISTS (SELECT 1 FROM codex_session_binding b WHERE b.tree_id=i.tree_id)
 ORDER BY i.expires_at,i.tree_id
 LIMIT ?
)`, now, defaultCodexSessionCleanupBatch); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
}

func cleanupExpiredCodexUpstreamAttempts(ctx context.Context, tx *sql.Tx, now int64, batch int) (int64, error) {
	if batch <= 0 {
		batch = defaultCodexSessionCleanupBatch
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM codex_upstream_attempt
WHERE expires_at<=? ORDER BY expires_at,id LIMIT ?`, now, batch)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	aggregateQuery := `INSERT INTO codex_upstream_attempt_daily(
day_start,account_id,egress_id,state,status_code,attempt_count,first_created_at,last_created_at,updated_at,expires_at)
SELECT created_at-(created_at%86400),account_id,egress_id,state,status_code,COUNT(*),MIN(created_at),MAX(created_at),?,
       created_at-(created_at%86400)+?
FROM codex_upstream_attempt WHERE id IN (` + placeholders + `)
GROUP BY created_at-(created_at%86400),account_id,egress_id,state,status_code
ON CONFLICT(day_start,account_id,egress_id,state,status_code) DO UPDATE SET
attempt_count=codex_upstream_attempt_daily.attempt_count+excluded.attempt_count,
first_created_at=CASE WHEN codex_upstream_attempt_daily.first_created_at<excluded.first_created_at
 THEN codex_upstream_attempt_daily.first_created_at ELSE excluded.first_created_at END,
last_created_at=CASE WHEN codex_upstream_attempt_daily.last_created_at>excluded.last_created_at
 THEN codex_upstream_attempt_daily.last_created_at ELSE excluded.last_created_at END,
updated_at=excluded.updated_at,
expires_at=CASE WHEN codex_upstream_attempt_daily.expires_at>excluded.expires_at
 THEN codex_upstream_attempt_daily.expires_at ELSE excluded.expires_at END`
	aggregateArgs := make([]interface{}, 0, len(ids)+2)
	aggregateArgs = append(aggregateArgs, now, int64((30*24*time.Hour)/time.Second))
	for _, id := range ids {
		aggregateArgs = append(aggregateArgs, id)
	}
	if _, err := tx.ExecContext(ctx, aggregateQuery, aggregateArgs...); err != nil {
		return 0, err
	}
	deleteArgs := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		deleteArgs = append(deleteArgs, id)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM codex_upstream_attempt WHERE id IN (`+placeholders+`)`, deleteArgs...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// CodexSessionMappingMetrics exposes aggregate-only counters for safe observability.
func (s *Store) CodexSessionMappingMetrics(ctx context.Context) (active, retired int64, err error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT state,COUNT(*) FROM codex_session_binding WHERE expires_at>? GROUP BY state`, Now())
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			return 0, 0, err
		}
		switch state {
		case "active":
			active += count
		default:
			retired += count
		}
	}
	return active, retired, rows.Err()
}

// CodexSessionMappingDiagnostic is intentionally safe for support bundles: raw
// response ids, downstream aliases, internal UUIDs and instruction text are absent.
// Tree and namespace values are domain-separated HMAC prefixes only.
type CodexSessionMappingDiagnostic struct {
	TreeHMACPrefix      string
	NamespaceHMACPrefix string
	AccountID           string
	EgressID            string
	Epoch               int64
	State               string
	SnapshotPresent     bool
	CreatedAt           int64
	UpdatedAt           int64
	ExpiresAt           int64
}

type CodexInstructionSnapshotDiagnostic struct {
	TreeHMACPrefix string
	RevisionPrefix string
	CreatedAt      int64
	UpdatedAt      int64
	ExpiresAt      int64
}

type CodexUpstreamAttemptDiagnostic struct {
	TreeHMACPrefix string
	AccountID      string
	EgressID       string
	Epoch          int64
	State          string
	StatusCode     int
	CreatedAt      int64
}

type CodexUpstreamAttemptDailyDiagnostic struct {
	DayStart       int64
	AccountID      string
	EgressID       string
	State          string
	StatusCode     int
	AttemptCount   int64
	FirstCreatedAt int64
	LastCreatedAt  int64
}

func (s *Store) codexDiagnosticPrefix(domain, value string) string {
	mac := hmac.New(sha256.New, s.codexSessionMappingKey())
	_, _ = mac.Write([]byte("codex-safe-diagnostic/v1\x00"))
	_, _ = mac.Write([]byte(strings.TrimSpace(domain)))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// CodexDiagnosticTreePrefix exposes only the domain-separated, truncated HMAC
// used by support bundles; the raw tree identifier is never returned.
func (s *Store) CodexDiagnosticTreePrefix(treeID string) string {
	return s.codexDiagnosticPrefix("tree", treeID)
}

func (s *Store) ListCodexSessionMappingDiagnostics(ctx context.Context) ([]CodexSessionMappingDiagnostic, error) {
	return s.listCodexSessionMappingDiagnostics(ctx, 0)
}

// ListRecentCodexSessionMappingDiagnostics bounds support-export memory and read
// time independently of retention cleanup. Rows are newest-first when a limit is
// supplied; the unbounded compatibility method above preserves its historical
// ordering for existing callers.
func (s *Store) ListRecentCodexSessionMappingDiagnostics(ctx context.Context, limit int) ([]CodexSessionMappingDiagnostic, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.listCodexSessionMappingDiagnostics(ctx, limit)
}

func (s *Store) listCodexSessionMappingDiagnostics(ctx context.Context, limit int) ([]CodexSessionMappingDiagnostic, error) {
	query := `SELECT b.tree_id,b.namespace_hash,b.account_id,b.egress_id,b.epoch,b.state,
CASE WHEN i.tree_id IS NULL THEN 0 ELSE 1 END,b.created_at,b.updated_at,b.expires_at
FROM codex_session_binding b LEFT JOIN codex_instruction_snapshot i ON i.tree_id=b.tree_id
	ORDER BY b.created_at,b.id`
	args := []interface{}(nil)
	if limit > 0 {
		query = `SELECT b.tree_id,b.namespace_hash,b.account_id,b.egress_id,b.epoch,b.state,
CASE WHEN i.tree_id IS NULL THEN 0 ELSE 1 END,b.created_at,b.updated_at,b.expires_at
FROM codex_session_binding b LEFT JOIN codex_instruction_snapshot i ON i.tree_id=b.tree_id
ORDER BY b.updated_at DESC,b.id DESC LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CodexSessionMappingDiagnostic{}
	for rows.Next() {
		var treeID, namespaceHash string
		var row CodexSessionMappingDiagnostic
		var present int
		if err := rows.Scan(&treeID, &namespaceHash, &row.AccountID, &row.EgressID, &row.Epoch, &row.State, &present, &row.CreatedAt, &row.UpdatedAt, &row.ExpiresAt); err != nil {
			return nil, err
		}
		row.TreeHMACPrefix = s.codexDiagnosticPrefix("tree", treeID)
		row.NamespaceHMACPrefix = s.codexDiagnosticPrefix("namespace", namespaceHash)
		row.SnapshotPresent = present != 0
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ListCodexInstructionSnapshotDiagnostics(ctx context.Context) ([]CodexInstructionSnapshotDiagnostic, error) {
	return s.listCodexInstructionSnapshotDiagnostics(ctx, 0)
}

func (s *Store) ListRecentCodexInstructionSnapshotDiagnostics(ctx context.Context, limit int) ([]CodexInstructionSnapshotDiagnostic, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.listCodexInstructionSnapshotDiagnostics(ctx, limit)
}

func (s *Store) listCodexInstructionSnapshotDiagnostics(ctx context.Context, limit int) ([]CodexInstructionSnapshotDiagnostic, error) {
	query := `SELECT tree_id,revision_hmac,created_at,updated_at,expires_at FROM codex_instruction_snapshot ORDER BY created_at,tree_id`
	args := []interface{}(nil)
	if limit > 0 {
		query = `SELECT tree_id,revision_hmac,created_at,updated_at,expires_at FROM codex_instruction_snapshot ORDER BY updated_at DESC,tree_id DESC LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CodexInstructionSnapshotDiagnostic{}
	for rows.Next() {
		var treeID, revision string
		var row CodexInstructionSnapshotDiagnostic
		if err := rows.Scan(&treeID, &revision, &row.CreatedAt, &row.UpdatedAt, &row.ExpiresAt); err != nil {
			return nil, err
		}
		row.TreeHMACPrefix = s.codexDiagnosticPrefix("tree", treeID)
		if len(revision) > 16 {
			revision = revision[:16]
		}
		row.RevisionPrefix = revision
		out = append(out, row)
	}
	return out, rows.Err()
}

// InsertCodexUpstreamAttempt persists one safe Codex transport observation. TreeID
// is either a durable CPA tree or a server-owned stateless request namespace.
func (s *Store) InsertCodexUpstreamAttempt(ctx context.Context, attempt CodexUpstreamAttempt) error {
	normalized, err := normalizeCodexUpstreamAttempt(attempt)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO codex_upstream_attempt(event_id,tree_id,account_id,egress_id,epoch,state,status_code,created_at,expires_at)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, normalized.EventID, normalized.TreeID, normalized.AccountID, normalized.EgressID, normalized.Epoch, normalized.State, normalized.StatusCode, normalized.CreatedAt, normalized.ExpiresAt)
	return err
}

func normalizeCodexUpstreamAttempt(attempt CodexUpstreamAttempt) (CodexUpstreamAttempt, error) {
	attempt.EventID = strings.TrimSpace(attempt.EventID)
	attempt.TreeID = strings.TrimSpace(attempt.TreeID)
	attempt.AccountID = strings.TrimSpace(attempt.AccountID)
	attempt.EgressID = strings.TrimSpace(attempt.EgressID)
	attempt.State = strings.TrimSpace(attempt.State)
	if attempt.TreeID == "" || attempt.AccountID == "" || attempt.EgressID == "" || attempt.State == "" {
		return CodexUpstreamAttempt{}, errors.New("codex upstream attempt metadata incomplete")
	}
	if attempt.EventID == "" {
		attempt.EventID = newCodexSessionMappingID("attempt")
	}
	if attempt.CreatedAt <= 0 {
		attempt.CreatedAt = Now()
	}
	if attempt.ExpiresAt <= attempt.CreatedAt {
		attempt.ExpiresAt = attempt.CreatedAt + int64((7 * 24 * time.Hour).Seconds())
	}
	return attempt, nil
}

// ListRecentCodexEgressOutcomes aggregates raw recent observations. Daily rows
// are intentionally excluded: routing needs a short moving window, while daily
// aggregation exists for diagnostics and long-term retention.
func (s *Store) ListRecentCodexEgressOutcomes(ctx context.Context, since int64) (map[string]CodexEgressRecentOutcome, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT egress_id,
COUNT(*),
SUM(CASE WHEN state='terminal_success' THEN 1 ELSE 0 END)
FROM codex_upstream_attempt
WHERE created_at>=? AND expires_at>? AND state IN ('egress_failure','terminal_success')
GROUP BY egress_id`, since, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]CodexEgressRecentOutcome)
	for rows.Next() {
		var item CodexEgressRecentOutcome
		if err := rows.Scan(&item.EgressID, &item.Attempts, &item.Successes); err != nil {
			return nil, err
		}
		item.EgressID = strings.TrimSpace(item.EgressID)
		if item.EgressID != "" {
			out[item.EgressID] = item
		}
	}
	return out, rows.Err()
}

func (s *Store) ListCodexUpstreamAttemptDiagnostics(ctx context.Context) ([]CodexUpstreamAttemptDiagnostic, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT tree_id,account_id,egress_id,epoch,state,status_code,created_at
FROM codex_upstream_attempt WHERE expires_at>? ORDER BY created_at,id`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CodexUpstreamAttemptDiagnostic{}
	for rows.Next() {
		var treeID string
		var row CodexUpstreamAttemptDiagnostic
		if err := rows.Scan(&treeID, &row.AccountID, &row.EgressID, &row.Epoch, &row.State, &row.StatusCode, &row.CreatedAt); err != nil {
			return nil, err
		}
		row.TreeHMACPrefix = s.codexDiagnosticPrefix("tree", treeID)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ListCodexUpstreamAttemptDailyDiagnostics(ctx context.Context) ([]CodexUpstreamAttemptDailyDiagnostic, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT day_start,account_id,egress_id,state,status_code,attempt_count,first_created_at,last_created_at
FROM codex_upstream_attempt_daily WHERE expires_at>? ORDER BY day_start,account_id,egress_id,state,status_code`, Now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CodexUpstreamAttemptDailyDiagnostic{}
	for rows.Next() {
		var row CodexUpstreamAttemptDailyDiagnostic
		if err := rows.Scan(&row.DayStart, &row.AccountID, &row.EgressID, &row.State, &row.StatusCode, &row.AttemptCount, &row.FirstCreatedAt, &row.LastCreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
