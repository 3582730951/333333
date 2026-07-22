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
`

var (
	ErrCodexSessionMappingNotFound  = errors.New("codex session mapping not found")
	ErrCodexSessionMappingAmbiguous = errors.New("codex session mapping is ambiguous")
	ErrCodexSessionEpochRetired     = errors.New("codex session context epoch retired")
	ErrCodexSessionEpochConflict    = errors.New("codex session context epoch changed")
)

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

	RootSessionID      string
	ThreadID           string
	ParentThreadID     string
	ForkedFromThreadID string
	WindowGeneration   int64
	CreatedAt          int64
	UpdatedAt          int64
	ExpiresAt          int64
}

// CodexSessionCommit atomically creates/refreshes a mapping and attaches aliases
// observed only after a successful upstream terminal.  Binding may be an existing
// row (for a normal continuation) or an in-memory prospective row (first root / a
// new child branch).  ResponseID and TurnState are intentionally aliases rather
// than columns, which makes every stateful resume use the same exact lookup path.
type CodexSessionCommit struct {
	Namespace string
	Binding   CodexSessionBinding
	Aliases   []CodexSessionAlias
	ExpiresAt int64
}

type codexSessionIdentityPayload struct {
	RootSessionID      string `json:"root_session_id"`
	ThreadID           string `json:"thread_id"`
	ParentThreadID     string `json:"parent_thread_id,omitempty"`
	ForkedFromThreadID string `json:"forked_from_thread_id,omitempty"`
	WindowGeneration   int64  `json:"window_generation"`
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

func (s *Store) sealCodexSessionIdentity(binding CodexSessionBinding) (string, error) {
	payload, err := json.Marshal(codexSessionIdentityPayload{
		RootSessionID:      binding.RootSessionID,
		ThreadID:           binding.ThreadID,
		ParentThreadID:     binding.ParentThreadID,
		ForkedFromThreadID: binding.ForkedFromThreadID,
		WindowGeneration:   binding.WindowGeneration,
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
	binding.RootSessionID = strings.TrimSpace(payload.RootSessionID)
	binding.ThreadID = strings.TrimSpace(payload.ThreadID)
	binding.ParentThreadID = strings.TrimSpace(payload.ParentThreadID)
	binding.ForkedFromThreadID = strings.TrimSpace(payload.ForkedFromThreadID)
	binding.WindowGeneration = payload.WindowGeneration
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

// ResolveCodexSessionAliases resolves a set of exact aliases.  Multiple aliases
// must converge on one binding; any disagreement is an ambiguity, never a heuristic
// opportunity to merge conversations.
func (s *Store) ResolveCodexSessionAliases(ctx context.Context, namespace string, aliases []CodexSessionAlias) (CodexSessionBinding, error) {
	aliases = normalizedCodexSessionAliases(aliases)
	if len(aliases) == 0 {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	var chosen *CodexSessionBinding
	for _, alias := range aliases {
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
	}
	if chosen == nil {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	if chosen.State != "active" {
		return *chosen, ErrCodexSessionEpochRetired
	}
	return *chosen, nil
}

// CommitCodexSessionBinding is deliberately terminal-only.  Alias ownership is
// checked inside the same SQLite transaction as the binding write, so concurrent
// requests cannot silently make a response id point at two active identities.
func (s *Store) CommitCodexSessionBinding(ctx context.Context, commit CodexSessionCommit) (CodexSessionBinding, error) {
	if strings.TrimSpace(commit.Namespace) == "" {
		return CodexSessionBinding{}, ErrCodexSessionMappingNotFound
	}
	aliases := normalizedCodexSessionAliases(commit.Aliases)
	binding := commit.Binding
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
	case err != nil:
		return CodexSessionBinding{}, err
	case existingState != "active":
		return CodexSessionBinding{}, ErrCodexSessionEpochRetired
	case existingEpoch != binding.Epoch:
		return CodexSessionBinding{}, ErrCodexSessionEpochConflict
	default:
		result, updateErr := tx.ExecContext(ctx, `UPDATE codex_session_binding
SET account_id=?,egress_id=?,state=?,encrypted_identity=?,updated_at=?,expires_at=?
WHERE id=? AND epoch=? AND state='active'`, binding.AccountID, binding.EgressID, binding.State, encrypted,
			binding.UpdatedAt, binding.ExpiresAt, binding.ID, binding.Epoch)
		if updateErr != nil {
			return CodexSessionBinding{}, updateErr
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return CodexSessionBinding{}, ErrCodexSessionEpochConflict
		}
	}

	for _, alias := range aliases {
		hash := s.codexSessionAliasHash(commit.Namespace, alias.Type, alias.Value)
		rows, queryErr := tx.QueryContext(ctx, `SELECT b.id FROM codex_session_alias a
JOIN codex_session_binding b ON b.id=a.binding_id
WHERE a.alias_hash=? AND a.expires_at>? AND b.expires_at>? AND b.state='active'`, hash, now, now)
		if queryErr != nil {
			return CodexSessionBinding{}, queryErr
		}
		owners := make([]string, 0, 1)
		for rows.Next() {
			var owner string
			if scanErr := rows.Scan(&owner); scanErr != nil {
				rows.Close()
				return CodexSessionBinding{}, scanErr
			}
			owners = append(owners, owner)
		}
		rows.Close()
		if len(owners) > 1 || (len(owners) == 1 && owners[0] != binding.ID) {
			return CodexSessionBinding{}, ErrCodexSessionMappingAmbiguous
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_session_alias(alias_hash,alias_type,binding_id,created_at,updated_at,expires_at)
VALUES(?,?,?,?,?,?) ON CONFLICT(alias_hash,binding_id) DO UPDATE SET updated_at=excluded.updated_at,expires_at=excluded.expires_at`,
			hash, alias.Type, binding.ID, now, now, binding.ExpiresAt); err != nil {
			return CodexSessionBinding{}, err
		}
	}
	// A successful terminal refreshes the complete mapping, not merely the aliases
	// that happened to be present on this particular request. A root-only alias may
	// otherwise expire while a client has been continuously resuming through newer
	// previous_response_id values, defeating the documented sliding retention window.
	if _, err := tx.ExecContext(ctx, `UPDATE codex_session_alias SET updated_at=?,expires_at=? WHERE binding_id=?`, now, binding.ExpiresAt, binding.ID); err != nil {
		return CodexSessionBinding{}, err
	}
	if err := tx.Commit(); err != nil {
		return CodexSessionBinding{}, err
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
	result, err := s.db.ExecContext(ctx, `DELETE FROM codex_session_binding WHERE expires_at<=?`, Now())
	if err != nil {
		return 0, err
	}
	// SQLite foreign keys may be disabled in an externally-opened legacy DB.  Keep
	// aliases tidy even there without relying solely on ON DELETE CASCADE.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM codex_session_alias WHERE expires_at<=? OR binding_id NOT IN (SELECT id FROM codex_session_binding)`, Now()); err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return n, nil
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
