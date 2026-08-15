package storage

import (
	"context"
	"errors"
	"strings"
	"time"
)

var errCodexSessionBatchRequiresFreshBinding = errors.New("codex session batch requires fresh bindings")

// CommitFreshCodexSessionBindings amortizes the transaction boundary for a burst
// of independent first terminals. SQLite's single writer otherwise pays one commit
// boundary per large request and can starve the worker fencing lease. Existing
// bindings/compactions use CommitCodexSessionBinding because they need per-tree CAS.
//
// The batch is all-or-nothing. A caller that encounters a rare alias conflict can
// safely retry its entries individually to isolate the conflicting session.
func (s *Store) CommitFreshCodexSessionBindings(ctx context.Context, commits []CodexSessionCommit) ([]CodexSessionBinding, error) {
	if len(commits) == 0 {
		return nil, nil
	}
	type preparedCommit struct {
		commit    CodexSessionCommit
		binding   CodexSessionBinding
		aliases   []CodexSessionAlias
		freshTree bool
		now       int64
		dropped   int
	}
	prepared := make([]preparedCommit, len(commits))
	for index, commit := range commits {
		if strings.TrimSpace(commit.Namespace) == "" {
			return nil, ErrCodexSessionMappingNotFound
		}
		binding := commit.Binding
		if binding.ID != "" {
			return nil, errCodexSessionBatchRequiresFreshBinding
		}
		freshTree := binding.TreeID == ""
		binding.ID = newCodexSessionMappingID("csm")
		if freshTree {
			binding.TreeID = newCodexSessionMappingID("cst")
		}
		if binding.State == "" {
			binding.State = "active"
		}
		if binding.RootSessionID == "" || binding.ThreadID == "" || binding.AccountID == "" || binding.EgressID == "" {
			return nil, errors.New("codex session binding incomplete")
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
		prepared[index] = preparedCommit{
			commit: commit, binding: binding, aliases: normalizedCodexSessionAliases(commit.Aliases),
			freshTree: freshTree, now: now,
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	bindingStmt, err := tx.PrepareContext(ctx, `INSERT INTO codex_session_binding(`+codexSessionBindingColumns+`)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer bindingStmt.Close()
	snapshotStmt, err := tx.PrepareContext(ctx, `INSERT INTO codex_instruction_snapshot(`+codexInstructionSnapshotColumns+`)
VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return nil, err
	}
	defer snapshotStmt.Close()
	aliasStmt, err := tx.PrepareContext(ctx, `INSERT INTO codex_session_alias(alias_hash,alias_type,binding_id,created_at,updated_at,expires_at)
SELECT ?,?,?,?,?,?
WHERE ?='turn_state' OR NOT EXISTS (
 SELECT 1 FROM codex_session_alias a
 JOIN codex_session_binding b ON b.id=a.binding_id
 WHERE a.alias_hash=? AND a.expires_at>? AND b.expires_at>? AND b.state='active' AND b.id<>?
)
ON CONFLICT(alias_hash,binding_id) DO UPDATE SET updated_at=excluded.updated_at,expires_at=excluded.expires_at`)
	if err != nil {
		return nil, err
	}
	defer aliasStmt.Close()
	results := make([]CodexSessionBinding, len(prepared))
	for index := range prepared {
		item := &prepared[index]
		binding := item.binding
		encrypted, sealErr := s.sealCodexSessionIdentity(binding)
		if sealErr != nil {
			return nil, sealErr
		}
		namespaceHash := s.codexSessionNamespaceHash(item.commit.Namespace)
		if _, err = bindingStmt.ExecContext(ctx, binding.ID, binding.TreeID, namespaceHash, binding.AccountID, binding.EgressID,
			binding.Epoch, binding.State, encrypted, binding.CreatedAt, binding.UpdatedAt, binding.ExpiresAt); err != nil {
			return nil, err
		}
		if item.commit.InstructionSnapshot != nil {
			snapshot := *item.commit.InstructionSnapshot
			snapshot.TreeID = binding.TreeID
			snapshot.ExpiresAt = binding.ExpiresAt
			if item.freshTree {
				now := Now()
				snapshot.Instructions = strings.TrimSpace(snapshot.Instructions)
				if snapshot.Revision == "" {
					snapshot.Revision = s.codexInstructionRevision(snapshot.Instructions)
				}
				encryptedInstructions, sealErr := s.sealCodexInstructionSnapshot(snapshot.Instructions)
				if sealErr != nil {
					return nil, sealErr
				}
				if snapshot.CreatedAt == 0 {
					snapshot.CreatedAt = now
				}
				snapshot.UpdatedAt = now
				if _, err = snapshotStmt.ExecContext(ctx, snapshot.TreeID, encryptedInstructions, snapshot.Revision,
					snapshot.CreatedAt, snapshot.UpdatedAt, snapshot.ExpiresAt); err != nil {
					return nil, err
				}
			} else if _, err = s.ensureCodexInstructionSnapshotTx(ctx, tx, snapshot, false); err != nil {
				return nil, err
			}
		}
		hasResponseAlias := false
		for _, alias := range item.aliases {
			if alias.Type == "response" && strings.TrimSpace(alias.Value) != "" {
				hasResponseAlias = true
				break
			}
		}
		for _, alias := range item.aliases {
			hash := s.codexSessionAliasHash(item.commit.Namespace, alias.Type, alias.Value)
			result, insertErr := aliasStmt.ExecContext(ctx,
				hash, alias.Type, binding.ID, item.now, item.now, binding.ExpiresAt,
				alias.Type, hash, item.now, item.now, binding.ID)
			if insertErr != nil {
				return nil, insertErr
			}
			changed, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return nil, rowsErr
			}
			if alias.Type != "turn_state" && changed == 0 {
				droppableHierarchy := item.commit.DropConflictingHierarchyAliases && hasResponseAlias &&
					(alias.Type == "root" || alias.Type == "session" || alias.Type == "branch")
				if droppableHierarchy {
					item.dropped++
					continue
				}
				return nil, ErrCodexSessionMappingAmbiguous
			}
		}
		results[index] = binding
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	for index := range prepared {
		if target := prepared[index].commit.DroppedHierarchyAliases; target != nil {
			*target = prepared[index].dropped
		}
	}
	return results, nil
}
