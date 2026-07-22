package api

// Strict CPA session mapping for native Codex Responses.  This layer is deliberately
// small: it identifies an exact downstream session/response alias, pins a stateful
// request to the corresponding account+egress, and supplies the encrypted internal
// UUID lifecycle to upstream.  It never reads goal_session, context_journal or any
// persisted prompt body.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"github.com/tidwall/sjson"
)

type codexSessionMappingContextKey struct{}
type codexStrictCPAContextKey struct{}

type codexDownstreamIdentity struct {
	RootID       string
	ThreadID     string
	SessionID    string
	ParentID     string
	ForkedFromID string
	ResponseID   string
	TurnState    string
}

func (i codexDownstreamIdentity) stateful() bool {
	return i.ResponseID != "" || i.TurnState != ""
}

func (i codexDownstreamIdentity) directAliases() []storage.CodexSessionAlias {
	aliases := make([]storage.CodexSessionAlias, 0, 4)
	if i.RootID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "root", Value: i.RootID})
	}
	if i.ThreadID != "" && i.ThreadID != i.RootID {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "branch", Value: i.ThreadID})
	}
	if i.SessionID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "session", Value: i.SessionID})
	}
	return aliases
}

func (i codexDownstreamIdentity) stateAliases() []storage.CodexSessionAlias {
	aliases := make([]storage.CodexSessionAlias, 0, 2)
	if i.ResponseID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "response", Value: i.ResponseID})
	}
	if i.TurnState != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "turn_state", Value: i.TurnState})
	}
	return aliases
}

// aliasesForBinding keeps hierarchy aliases with the binding they actually
// identify. A child carries the root session id on every request, but persisting
// that root alias on the child would make valid root+child requests ambiguous.
func (i codexDownstreamIdentity) aliasesForBinding(binding storage.CodexSessionBinding, responseID, responseTurnState string) []storage.CodexSessionAlias {
	aliases := append([]storage.CodexSessionAlias(nil), i.stateAliases()...)
	if binding.ThreadID == binding.RootSessionID {
		if i.RootID != "" {
			aliases = append(aliases, storage.CodexSessionAlias{Type: "root", Value: i.RootID})
		}
		if i.SessionID != "" {
			aliases = append(aliases, storage.CodexSessionAlias{Type: "session", Value: i.SessionID})
		}
	} else if i.ThreadID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "branch", Value: i.ThreadID})
	}
	if responseID = strings.TrimSpace(responseID); responseID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "response", Value: responseID})
	}
	if responseTurnState = strings.TrimSpace(responseTurnState); responseTurnState != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "turn_state", Value: responseTurnState})
	}
	return aliases
}

// codexSessionMapping is per logical downstream request. prospective bindings stay
// only in memory until a successful terminal response; this prevents a failed new
// request from creating a durable session merely because it carried a thread id.
type codexSessionMapping struct {
	mu        sync.Mutex
	enabled   bool
	namespace string
	identity  codexDownstreamIdentity

	// binding is a durable exact branch. anchor is an existing root/parent used to
	// create a child/fork branch only after the new branch succeeds.
	binding *storage.CodexSessionBinding
	anchor  *storage.CodexSessionBinding

	// requiredAccount/Egress are established by an exact binding or parent branch.
	requiredAccount string
	requiredEgress  string

	// prospective is allocated after account/egress selection. It becomes binding
	// only in CommitCodexSessionBinding.
	prospective   *storage.CodexSessionBinding
	snapshot      *upstream.CodexIdentitySnapshot
	snapshotAcc   string
	snapshotEgr   string
	logicalTurnID string
	instructions  *CodexInstructionPlan
}

func withCodexSessionMapping(ctx context.Context, mapping *codexSessionMapping) context.Context {
	if mapping == nil {
		return ctx
	}
	return context.WithValue(ctx, codexSessionMappingContextKey{}, mapping)
}

func codexSessionMappingFromContext(ctx context.Context) *codexSessionMapping {
	mapping, _ := ctx.Value(codexSessionMappingContextKey{}).(*codexSessionMapping)
	return mapping
}

// withCodexStrictCPA marks the narrow native Responses path on which context and
// tool-result errors must remain verbatim. It is intentionally scoped to mapped
// Codex traffic; Claude and legacy compatibility paths retain their own policies.
func withCodexStrictCPA(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexStrictCPAContextKey{}, true)
}

func codexStrictCPAFromContext(ctx context.Context) bool {
	strict, _ := ctx.Value(codexStrictCPAContextKey{}).(bool)
	return strict
}

func (s *Server) codexSessionMappingEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "codex_session_mapping_enabled", s.cfg.CodexSessionMappingEnabled)
}

func (s *Server) codexCPAStrict(ctx context.Context) bool {
	return s.flagEnabled(ctx, "codex_cpa_strict", s.cfg.CodexCPAStrict)
}

func (s *Server) codexSessionMappingRetention(ctx context.Context) time.Duration {
	days := s.settingInt(ctx, "codex_session_mapping_retention_days", s.cfg.CodexSessionMappingRetentionDays)
	if days <= 0 {
		days = 7
	}
	return time.Duration(days) * 24 * time.Hour
}

func codexHeaderValue(h http.Header, key string) string {
	for name, values := range h {
		if !strings.EqualFold(name, key) || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

func codexJSONMap(raw []byte) map[string]interface{} {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	return root
}

func codexMapStringAPI(root map[string]interface{}, key string) string {
	if root == nil {
		return ""
	}
	value, _ := root[key].(string)
	return strings.TrimSpace(value)
}

func codexTurnMapAPI(root map[string]interface{}, metadata map[string]interface{}, headers http.Header) map[string]interface{} {
	for _, candidate := range []string{
		codexMapStringAPI(metadata, "x-codex-turn-metadata"),
		codexHeaderValue(headers, "x-codex-turn-metadata"),
	} {
		if candidate == "" {
			continue
		}
		var turn map[string]interface{}
		if json.Unmarshal([]byte(candidate), &turn) == nil {
			return turn
		}
	}
	if raw, ok := root["turn_metadata"].(map[string]interface{}); ok {
		return raw
	}
	return nil
}

// codexDownstreamSessionIdentity reads only true client correlators. Model name,
// prompt-cache key, request body prefix and API key are intentionally excluded: none
// is a session identity and using them would merge independent CLI conversations.
func codexDownstreamSessionIdentity(headers http.Header, body []byte) codexDownstreamIdentity {
	root := codexJSONMap(body)
	metadata, _ := root["client_metadata"].(map[string]interface{})
	turn := codexTurnMapAPI(root, metadata, headers)
	value := func(header string, bodyKeys ...string) string {
		if v := codexHeaderValue(headers, header); v != "" {
			return v
		}
		for _, key := range bodyKeys {
			if v := codexMapStringAPI(metadata, key); v != "" {
				return v
			}
			if v := codexMapStringAPI(root, key); v != "" {
				return v
			}
			if v := codexMapStringAPI(turn, key); v != "" {
				return v
			}
		}
		return ""
	}

	threadID := value("thread-id", "thread_id")
	if threadID == "" {
		// x-client-request-id is an official alias for the thread id. It is only a
		// fallback, never a general request-id heuristic.
		threadID = codexHeaderValue(headers, "x-client-request-id")
	}
	sessionID := value("session-id", "session_id")
	parentID := value("x-codex-parent-thread-id", "x-codex-parent-thread-id", "parent_thread_id")
	forkedFrom := value("x-codex-forked-from-thread-id", "forked_from_thread_id")
	responseID := codexMapStringAPI(root, "previous_response_id")
	turnState := firstNonEmpty(
		codexHeaderValue(headers, "x-codex-turn-state"),
		codexMapStringAPI(metadata, "x-codex-turn-state"),
		codexMapStringAPI(root, "turn_state"),
		codexMapStringAPI(turn, "x-codex-turn-state"),
		codexMapStringAPI(turn, "turn_state"),
	)

	// A client may carry only a window id. Its prefix is the real thread id; the
	// ordinal is not a root identity and must never be used to create a new branch.
	if threadID == "" {
		if window := firstNonEmpty(codexHeaderValue(headers, "x-codex-window-id"), codexMapStringAPI(metadata, "x-codex-window-id")); window != "" {
			if idx := strings.LastIndex(window, ":"); idx > 0 {
				threadID = strings.TrimSpace(window[:idx])
			}
		}
	}
	// A child agent uses session_id for its root and parent_thread_id for its
	// immediate branch relation. The parent must not overwrite the root identity.
	// When a child omits session_id, its thread id is *not* a root id: retain an
	// empty root here so resolution can use the parent and persist the child as a
	// branch instead of accidentally allocating another root session.
	rootID := sessionID
	if rootID == "" && parentID == "" {
		rootID = threadID
	}
	if threadID == "" {
		threadID = rootID
	}
	return codexDownstreamIdentity{
		RootID:       strings.TrimSpace(rootID),
		ThreadID:     strings.TrimSpace(threadID),
		SessionID:    strings.TrimSpace(sessionID),
		ParentID:     strings.TrimSpace(parentID),
		ForkedFromID: strings.TrimSpace(forkedFrom),
		ResponseID:   strings.TrimSpace(responseID),
		TurnState:    strings.TrimSpace(turnState),
	}
}

func codexSessionNamespace(pol downstreamPolicy, r *http.Request) string {
	if strings.TrimSpace(pol.KeyHash) != "" {
		return "key:" + strings.TrimSpace(pol.KeyHash)
	}
	if token := strings.TrimSpace(downstreamBearer(r)); token != "" {
		return "bearer:" + hashAPIKey(token)
	}
	// Open-mode requests still need a namespace, but this is never used by itself
	// to resolve a session: an exact root/thread/response/turn alias is mandatory.
	return "unauthenticated"
}

func oneBinding(rows []storage.CodexSessionBinding) (*storage.CodexSessionBinding, error) {
	if len(rows) == 0 {
		return nil, storage.ErrCodexSessionMappingNotFound
	}
	active := make([]storage.CodexSessionBinding, 0, 1)
	for _, row := range rows {
		if row.State == "active" {
			active = append(active, row)
		}
	}
	if len(active) == 1 {
		copy := active[0]
		return &copy, nil
	}
	if len(active) > 1 {
		return nil, storage.ErrCodexSessionMappingAmbiguous
	}
	// Retired direct hierarchy aliases are kept deliberately so a self-contained
	// request can create a fresh epoch and a stateful one can report retirement.
	// There may be several historical epochs; the storage query orders newest first.
	copy := rows[0]
	return &copy, nil
}

func (s *Server) lookupCodexSessionAlias(ctx context.Context, namespace string, alias storage.CodexSessionAlias) (*storage.CodexSessionBinding, error) {
	rows, err := s.store.FindCodexSessionAlias(ctx, namespace, alias)
	if err != nil {
		return nil, err
	}
	return oneBinding(rows)
}

// resolveCodexSessionMapping runs before account selection. Stateful requests must
// resolve through response/turn-state aliases; ordinary requests may use a concrete
// root/branch identity but never a key/model/cache fallback.
func (s *Server) resolveCodexSessionMapping(ctx context.Context, r *http.Request, body []byte, pol downstreamPolicy) (*codexSessionMapping, error) {
	mapping := &codexSessionMapping{enabled: s.codexSessionMappingEnabled(ctx)}
	if !mapping.enabled {
		return mapping, nil
	}
	mapping.namespace = codexSessionNamespace(pol, r)
	mapping.identity = codexDownstreamSessionIdentity(r.Header, body)
	id := mapping.identity
	lookupBranchOrRoot := func(value string) (*storage.CodexSessionBinding, error) {
		if value == "" {
			return nil, storage.ErrCodexSessionMappingNotFound
		}
		if binding, err := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "branch", Value: value}); err == nil {
			return binding, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return nil, err
		}
		return s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "root", Value: value})
	}

	if id.stateful() {
		binding, err := s.store.ResolveCodexSessionAliases(ctx, mapping.namespace, id.stateAliases())
		if err != nil {
			return mapping, err
		}
		// If a real hierarchy accompanies state aliases it must agree with the
		// same tree. A child response naturally differs from its root binding, so
		// only a claimed current branch must resolve to this exact binding.
		if len(id.directAliases()) > 0 {
			for _, alias := range id.directAliases() {
				candidate, lookupErr := s.lookupCodexSessionAlias(ctx, mapping.namespace, alias)
				if errors.Is(lookupErr, storage.ErrCodexSessionMappingNotFound) {
					return mapping, storage.ErrCodexSessionMappingNotFound
				}
				if lookupErr != nil {
					return mapping, lookupErr
				}
				if candidate.State != "active" {
					return mapping, storage.ErrCodexSessionEpochRetired
				}
				if alias.Type == "branch" && alias.Value == id.ThreadID && candidate.ID != binding.ID {
					return mapping, storage.ErrCodexSessionMappingAmbiguous
				}
				if candidate.TreeID != binding.TreeID {
					return mapping, storage.ErrCodexSessionMappingAmbiguous
				}
			}
		}
		if id.ParentID != "" && id.ParentID != id.ThreadID {
			parent, lookupErr := lookupBranchOrRoot(id.ParentID)
			if lookupErr != nil {
				return mapping, lookupErr
			}
			if parent.State != "active" {
				return mapping, storage.ErrCodexSessionEpochRetired
			}
			if parent.TreeID != binding.TreeID {
				return mapping, storage.ErrCodexSessionMappingAmbiguous
			}
		}
		mapping.binding = &binding
		mapping.requiredAccount, mapping.requiredEgress = binding.AccountID, binding.EgressID
		return mapping, nil
	}

	// Fork is a new root by definition. Its source mapping is consulted only to
	// preserve an encrypted internal fork relationship; it never inherits the old
	// root/session identity or window generation.
	if id.ForkedFromID != "" {
		if parent, err := lookupBranchOrRoot(id.ForkedFromID); err == nil {
			mapping.anchor = parent
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
		return mapping, nil
	}

	// A child normally sends parent_thread_id on every turn, including a
	// self-contained turn that has no previous_response_id.  Prefer an existing
	// branch before treating that same hierarchy as a newly-created child; otherwise
	// each child turn would receive a fresh internal thread id.
	if id.ThreadID != "" && id.ThreadID != id.RootID {
		if branch, err := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "branch", Value: id.ThreadID}); err == nil {
			if branch.State != "active" {
				return mapping, storage.ErrCodexSessionEpochRetired
			}
			if id.RootID != "" {
				if root, rootErr := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "root", Value: id.RootID}); rootErr == nil && root.TreeID != branch.TreeID {
					return mapping, storage.ErrCodexSessionMappingAmbiguous
				} else if rootErr != nil && !errors.Is(rootErr, storage.ErrCodexSessionMappingNotFound) {
					return mapping, rootErr
				}
			}
			if id.ParentID != "" && id.ParentID != id.ThreadID {
				parent, parentErr := lookupBranchOrRoot(id.ParentID)
				if parentErr != nil {
					return mapping, parentErr
				}
				if parent.State != "active" {
					return mapping, storage.ErrCodexSessionEpochRetired
				}
				if parent.TreeID != branch.TreeID || branch.ParentThreadID != parent.ThreadID {
					return mapping, storage.ErrCodexSessionMappingAmbiguous
				}
			}
			mapping.binding = branch
			mapping.requiredAccount, mapping.requiredEgress = branch.AccountID, branch.EgressID
			return mapping, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
	}

	// A fresh child may carry a never-before-seen thread id plus a known immediate
	// parent. Resolve the parent before the root so nested agents retain the true
	// branch relation rather than being flattened under the root.
	if id.ParentID != "" && id.ParentID != id.ThreadID {
		if parent, err := lookupBranchOrRoot(id.ParentID); err == nil {
			if parent.State != "active" {
				return mapping, storage.ErrCodexSessionEpochRetired
			}
			if id.RootID != "" {
				if root, rootErr := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "root", Value: id.RootID}); rootErr == nil && root.TreeID != parent.TreeID {
					return mapping, storage.ErrCodexSessionMappingAmbiguous
				} else if rootErr != nil && !errors.Is(rootErr, storage.ErrCodexSessionMappingNotFound) {
					return mapping, rootErr
				}
			}
			mapping.anchor = parent
			mapping.requiredAccount, mapping.requiredEgress = parent.AccountID, parent.EgressID
			return mapping, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
	}

	if id.RootID != "" {
		if root, err := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "root", Value: id.RootID}); err == nil {
			if root.State == "active" {
				if id.ThreadID != "" && id.ThreadID != id.RootID {
					mapping.anchor = root
					mapping.requiredAccount, mapping.requiredEgress = root.AccountID, root.EgressID
					return mapping, nil
				}
				mapping.binding = root
				mapping.requiredAccount, mapping.requiredEgress = root.AccountID, root.EgressID
				return mapping, nil
			}
			// A self-contained new root after an explicit rotation may establish a
			// new epoch. Stateful aliases were handled above and always fail retired.
			mapping.anchor = root
			return mapping, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
	}
	if id.SessionID != "" {
		if session, err := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "session", Value: id.SessionID}); err == nil {
			if session.State != "active" {
				mapping.anchor = session
				return mapping, nil
			}
			mapping.binding = session
			mapping.requiredAccount, mapping.requiredEgress = session.AccountID, session.EgressID
			return mapping, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
	}
	return mapping, nil
}

func (m *codexSessionMapping) requiredRoute() (string, string) {
	if m == nil || !m.enabled {
		return "", ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requiredAccount, m.requiredEgress
}

// instructionTreeID returns the tree whose administrator configuration a strict
// request belongs to. A child inherits its parent tree; a fork deliberately does
// not, because it is a new root/session under current operator configuration.
func (m *codexSessionMapping) instructionTreeID() string {
	if m == nil || !m.enabled {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.binding != nil {
		return strings.TrimSpace(m.binding.TreeID)
	}
	// An active parent anchor means this is a newly-created child branch and it
	// inherits the parent tree's session policy. A retired anchor, by contrast,
	// only supplies the next epoch number for a fresh root: that root must compile
	// the administrator configuration visible now, rather than reviving its old
	// tree snapshot.
	if m.anchor != nil && m.anchor.State == "active" && m.identity.ForkedFromID == "" {
		return strings.TrimSpace(m.anchor.TreeID)
	}
	return ""
}

func (m *codexSessionMapping) setInstructionPlan(plan *CodexInstructionPlan) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.instructions = plan
	m.mu.Unlock()
}

// upstreamAttemptBinding returns only the durable routing metadata needed for a
// redacted CPA diagnostic. It never exposes aliases, downstream ids, request text,
// or the encrypted identity payload. Fresh roots/forks have no tree yet and are
// intentionally omitted until their terminal commit assigns one.
func (m *codexSessionMapping) upstreamAttemptBinding() (storage.CodexSessionBinding, bool) {
	if m == nil || !m.enabled {
		return storage.CodexSessionBinding{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding := m.binding
	if binding == nil {
		binding = m.prospective
	}
	if binding == nil || strings.TrimSpace(binding.TreeID) == "" {
		return storage.CodexSessionBinding{}, false
	}
	return *binding, true
}

func (m *codexSessionMapping) identitySnapshot(secret []byte, lease scheduler.Lease, osHint string) (*upstream.CodexIdentitySnapshot, error) {
	if m == nil || !m.enabled {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot != nil && m.snapshotAcc == lease.Account.ID && m.snapshotEgr == lease.Egress.ID {
		copy := *m.snapshot
		return &copy, nil
	}
	if m.binding != nil && (m.binding.AccountID != lease.Account.ID || m.binding.EgressID != lease.Egress.ID) {
		return nil, storage.ErrCodexSessionEpochConflict
	}

	binding := m.binding
	if binding == nil {
		created := storage.CodexSessionBinding{AccountID: lease.Account.ID, EgressID: lease.Egress.ID, State: "active"}
		isChild := m.anchor != nil && m.identity.ForkedFromID == "" && m.identity.ThreadID != "" &&
			(m.identity.ThreadID != m.identity.RootID || (m.identity.ParentID != "" && m.identity.ParentID != m.identity.ThreadID))
		if isChild {
			created.TreeID = m.anchor.TreeID
			created.Epoch = m.anchor.Epoch
			created.RootSessionID = m.anchor.RootSessionID
			created.ThreadID = identity.NewUUIDv7()
			created.ParentThreadID = m.anchor.ThreadID
			// A child is a new thread, not a new physical Codex device. The
			// parent tree's encrypted device identity must survive a child request
			// whose input happens to imply a different downstream OS family.
			created.InstallationID = m.anchor.InstallationID
			created.DeviceOSHint = m.anchor.DeviceOSHint
			created.DeviceOSHintSet = m.anchor.DeviceOSHintSet
		} else {
			// Fork and a fresh root both start a new tree. A fork keeps only the
			// mapped source thread as encrypted relationship metadata.
			created.RootSessionID = identity.NewUUIDv7()
			created.ThreadID = created.RootSessionID
			if m.anchor != nil && m.identity.ForkedFromID != "" {
				created.ForkedFromThreadID = m.anchor.ThreadID
			}
			if m.anchor != nil && m.identity.ForkedFromID == "" && m.anchor.State != "active" {
				created.Epoch = m.anchor.Epoch
			}
		}
		m.prospective = &created
		binding = &created
	}
	// `identity_os_source=downstream` can legitimately infer different OS
	// families from a user turn and a later tool-result turn. The upstream
	// associates a previous_response_id with the full device identity, including
	// the OS-specific User-Agent, so a strict CPA continuation must reuse both
	// the encrypted installation id and the OS profile elected by the root.
	if !binding.DeviceOSHintSet {
		// Older rows did not retain an OS profile, and an installation id alone
		// cannot reveal one: its deterministic value is deliberately independent
		// of the chosen OS profile. Preserve the historical behavior for such a
		// row by electing the current hint once, then persist it after a successful
		// terminal; never fabricate a claimed recovery of the original profile.
		binding.DeviceOSHint = strings.TrimSpace(osHint)
		binding.DeviceOSHintSet = true
	}
	if strings.TrimSpace(binding.InstallationID) == "" {
		binding.InstallationID = identity.CodexDevice(secret, lease.Account.ID, lease.Egress.ID, binding.DeviceOSHint).MachineID
	}
	if m.logicalTurnID == "" {
		m.logicalTurnID = identity.NewUUIDv7()
	}
	snapshot := &upstream.CodexIdentitySnapshot{
		InstallationID:     binding.InstallationID,
		DeviceOSHint:       binding.DeviceOSHint,
		SessionID:          binding.RootSessionID,
		ThreadID:           binding.ThreadID,
		TurnID:             m.logicalTurnID,
		WindowGeneration:   binding.WindowGeneration,
		ParentThreadID:     binding.ParentThreadID,
		ForkedFromThreadID: binding.ForkedFromThreadID,
		TurnState:          m.identity.TurnState,
	}
	m.snapshot = snapshot
	m.snapshotAcc, m.snapshotEgr = lease.Account.ID, lease.Egress.ID
	copy := *snapshot
	return &copy, nil
}

func (m *codexSessionMapping) nextContinueSnapshot(turnState string) *upstream.CodexIdentitySnapshot {
	if m == nil || !m.enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snapshot == nil {
		return nil
	}
	copy := *m.snapshot
	copy.TurnID = identity.NewUUIDv7()
	if strings.TrimSpace(turnState) != "" {
		copy.TurnState = strings.TrimSpace(turnState)
	}
	return &copy
}

// nativeCodexContinueBody intentionally creates no local replay context. The
// upstream owns the previous response chain, so the only new input is one native
// continue turn paired with the latest upstream response id.
func nativeCodexContinueBody(original []byte, previousResponseID string) ([]byte, error) {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" {
		return nil, storage.ErrCodexSessionMappingNotFound
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(original, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("Codex continuation request is not an object")
		}
		return nil, err
	}
	continueItem, err := json.Marshal(map[string]interface{}{
		"role": "user",
		"content": []interface{}{map[string]interface{}{
			"type": "input_text",
			"text": "continue",
		}},
	})
	if err != nil {
		return nil, err
	}
	// Preserve the native Lite prefix (additional_tools + the plan's developer
	// base instructions) while dropping all prior turn content. Classic requests
	// retain their existing top-level instructions untouched. Targeted sjson edits
	// keep tool values and all unrelated large integer fragments byte-exact.
	input := []json.RawMessage{continueItem}
	if items, ok := codexRawInputItems(fields["input"]); ok && len(items) > 0 && codexLiteAdditionalTools(items[0]) {
		input = append(input[:0], items[0])
		if len(items) > 1 && codexDeveloperMessage(items[1]) {
			input = append(input, items[1])
		}
		input = append(input, continueItem)
	}
	out, err := sjson.SetRawBytes(original, "input", codexMarshalRawArray(input))
	if err != nil {
		return nil, err
	}
	if out, err = sjson.SetBytes(out, "previous_response_id", previousResponseID); err != nil {
		return nil, err
	}
	if out, err = sjson.SetBytes(out, "stream", true); err != nil {
		return nil, err
	}
	return out, nil
}

// writeCodexNativeContinuationFailure gives an EOF continuation a visible
// protocol terminal without pretending that the partial output completed.
func writeCodexNativeContinuationFailure(w io.Writer, id, model string) error {
	response := map[string]interface{}{
		"id":     firstNonEmpty(strings.TrimSpace(id), "resp_pool_native_continue"),
		"object": "response",
		"status": "failed",
		"error": map[string]string{
			"code":    "codex_native_continue_failed",
			"message": "Upstream ended without a terminal response and its native continuation failed.",
		},
	}
	if strings.TrimSpace(model) != "" {
		response["model"] = strings.TrimSpace(model)
	}
	if err := writeSSEEvent(w, "response.failed", map[string]interface{}{"response": response}); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// writeCodexSidecarStreamFailure terminates an already-started SSE response with
// a protocol-valid failure when sidecar v2 reports a post-header transport break.
// It intentionally does not claim the upstream reached a native EOF, so callers
// must not invoke a continuation or rotate the CPA epoch from this condition.
func writeCodexSidecarStreamFailure(w io.Writer, id, model, phase string) error {
	response := map[string]interface{}{
		"id":     firstNonEmpty(strings.TrimSpace(id), "resp_pool_sidecar_stream"),
		"object": "response",
		"status": "failed",
		"error": map[string]string{
			"code":    "sidecar_stream_interrupted",
			"message": "The sidecar interrupted the upstream stream after response headers were sent.",
		},
	}
	if strings.TrimSpace(model) != "" {
		response["model"] = strings.TrimSpace(model)
	}
	if strings.TrimSpace(phase) != "" {
		response["error"].(map[string]string)["phase"] = strings.TrimSpace(phase)
	}
	if err := writeSSEEvent(w, "response.failed", map[string]interface{}{"response": response}); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

func (s *Server) emitCodexNativeContinuationFailure(ctx context.Context, w io.Writer, id, model, reason string) error {
	atomic.AddUint64(&s.codexEOFCompensations, 1)
	if s.store != nil {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "codex_eof_terminal_compensation",
			State:  "emitted",
			Reason: firstNonEmpty(strings.TrimSpace(reason), "truncated_eof"),
			Detail: "native_protocol_no_raw_identifiers",
		})
	}
	return writeCodexNativeContinuationFailure(w, id, model)
}

// codexSessionMappingStats contains aggregate-only operational data. None of the
// HMAC aliases, internal UUIDs, response ids, or request content is exposed.
func (s *Server) codexSessionMappingStats(ctx context.Context) map[string]interface{} {
	stats := map[string]interface{}{
		"enabled":           s.codexSessionMappingEnabled(ctx),
		"strict_cpa":        s.codexCPAStrict(ctx),
		"retention_days":    int(s.codexSessionMappingRetention(ctx).Hours() / 24),
		"bindings_created":  atomic.LoadUint64(&s.codexMappingBindingsCreated),
		"epoch_rotations":   atomic.LoadUint64(&s.codexMappingEpochRotations),
		"unidentified":      atomic.LoadUint64(&s.codexMappingUnidentified),
		"ambiguous":         atomic.LoadUint64(&s.codexMappingAmbiguous),
		"native_continues":  atomic.LoadUint64(&s.codexNativeContinues),
		"eof_compensations": atomic.LoadUint64(&s.codexEOFCompensations),
	}
	if s.store == nil {
		return stats
	}
	if active, retired, err := s.store.CodexSessionMappingMetrics(ctx); err == nil {
		stats["active"] = active
		stats["retired"] = retired
	}
	return stats
}

func responseTurnState(header http.Header, body []byte) string {
	root := codexJSONMap(body)
	metadata, _ := root["client_metadata"].(map[string]interface{})
	turn := codexTurnMapAPI(root, metadata, header)
	return firstNonEmpty(
		codexHeaderValue(header, "x-codex-turn-state"),
		codexMapStringAPI(root, "turn_state"),
		codexMapStringAPI(metadata, "x-codex-turn-state"),
		codexMapStringAPI(turn, "x-codex-turn-state"),
		codexMapStringAPI(turn, "turn_state"),
	)
}

// commitCodexSessionMapping is the terminal-only counterpart of identitySnapshot.
// A storage fault is visible in audit/metrics but does not turn an already-successful
// upstream response into a fake failure.
func (s *Server) commitCodexSessionMapping(ctx context.Context, mapping *codexSessionMapping, lease scheduler.Lease, egress storage.EgressProfile, responseID, turnState string, compact bool) error {
	if mapping == nil || !mapping.enabled || mapping.snapshot == nil {
		return nil
	}
	mapping.mu.Lock()
	defer mapping.mu.Unlock()
	binding := mapping.binding
	if binding == nil {
		binding = mapping.prospective
	}
	if binding == nil {
		return errors.New("codex session mapping identity missing")
	}
	binding.AccountID = lease.Account.ID
	binding.EgressID = firstNonEmpty(egress.ID, lease.Egress.ID)
	// The mapping's internal identity is the source of truth. Do not derive it
	// from a retry body or a downstream correlator.
	binding.InstallationID = mapping.snapshot.InstallationID
	binding.DeviceOSHint = mapping.snapshot.DeviceOSHint
	binding.DeviceOSHintSet = true
	binding.RootSessionID = mapping.snapshot.SessionID
	binding.ThreadID = mapping.snapshot.ThreadID
	binding.ParentThreadID = mapping.snapshot.ParentThreadID
	binding.ForkedFromThreadID = mapping.snapshot.ForkedFromThreadID
	binding.WindowGeneration = mapping.snapshot.WindowGeneration
	created := binding.ID == ""
	expiresAt := time.Now().Add(s.codexSessionMappingRetention(ctx)).Unix()
	instructionSnapshot := mapping.instructions.snapshotCommit(binding.TreeID, expiresAt)
	committed, err := s.store.CommitCodexSessionBinding(ctx, storage.CodexSessionCommit{
		Namespace:           mapping.namespace,
		Binding:             *binding,
		Aliases:             mapping.identity.aliasesForBinding(*binding, responseID, turnState),
		ExpiresAt:           expiresAt,
		InstructionSnapshot: instructionSnapshot,
	})
	if err != nil {
		return err
	}
	if compact {
		advanced, advanceErr := s.store.AdvanceCodexSessionWindowGeneration(ctx, committed.ID, committed.Epoch, committed.WindowGeneration, expiresAt)
		if advanceErr != nil {
			return advanceErr
		}
		committed = advanced
	}
	mapping.binding = &committed
	mapping.prospective = nil
	mapping.requiredAccount, mapping.requiredEgress = committed.AccountID, committed.EgressID
	if created {
		atomic.AddUint64(&s.codexMappingBindingsCreated, 1)
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "codex_session_binding_created",
			State:  "active",
			Reason: "terminal_success",
			Detail: "metadata_only_no_raw_identifiers",
		})
	}
	s.recordCodexUpstreamAttemptBinding(ctx, committed, lease, egress, "terminal_success", http.StatusOK)
	return nil
}

func (s *Server) recordCodexUpstreamAttempt(ctx context.Context, mapping *codexSessionMapping, lease scheduler.Lease, egress storage.EgressProfile, state string, statusCode int) {
	if s == nil || s.store == nil {
		return
	}
	binding, ok := mapping.upstreamAttemptBinding()
	if !ok {
		return
	}
	s.recordCodexUpstreamAttemptBinding(ctx, binding, lease, egress, state, statusCode)
}

func (s *Server) recordCodexUpstreamAttemptBinding(ctx context.Context, binding storage.CodexSessionBinding, lease scheduler.Lease, egress storage.EgressProfile, state string, statusCode int) {
	if s == nil || s.store == nil {
		return
	}
	if accountID := strings.TrimSpace(lease.Account.ID); accountID != "" {
		binding.AccountID = accountID
	}
	if egressID := strings.TrimSpace(egress.ID); egressID != "" {
		binding.EgressID = egressID
	}
	expiresAt := binding.ExpiresAt
	if expiresAt <= time.Now().Unix() {
		expiresAt = time.Now().Add(s.codexSessionMappingRetention(ctx)).Unix()
	}
	if err := s.store.InsertCodexUpstreamAttempt(ctx, storage.CodexUpstreamAttempt{
		TreeID:     binding.TreeID,
		AccountID:  binding.AccountID,
		EgressID:   binding.EgressID,
		Epoch:      binding.Epoch,
		State:      state,
		StatusCode: statusCode,
		ExpiresAt:  expiresAt,
	}); err != nil {
		log.Printf("[CODEX-UPSTREAM-ATTEMPT] record request_id=%s: %v", requestIDFromContext(ctx), err)
	}
}

func (s *Server) retireCodexSessionMapping(ctx context.Context, mapping *codexSessionMapping, reason string) error {
	if mapping == nil || mapping.binding == nil {
		return storage.ErrCodexSessionMappingNotFound
	}
	retired, err := s.store.RetireCodexSessionTree(ctx, mapping.binding.ID, mapping.binding.Epoch)
	if err == nil && retired > 0 {
		atomic.AddUint64(&s.codexMappingEpochRotations, 1)
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "codex_session_epoch_rotated", State: "retired", Reason: strings.TrimSpace(reason), Detail: "native_context_tree"})
	}
	return err
}

func (s *Server) writeCodexSessionMappingError(w http.ResponseWriter, stream bool, code string) {
	message := map[string]string{
		"codex_session_mapping_unidentified": "Codex stateful request has no exact session mapping; start a new session or retry with its original response id.",
		"codex_session_mapping_ambiguous":    "Codex session aliases resolve to more than one internal session.",
		"codex_context_epoch_retired":        "Codex context epoch was retired; this previous_response_id cannot be migrated to a new account.",
	}[code]
	if message == "" {
		message = "Codex session mapping failed."
	}
	w.Header().Set("X-MiCliProxy-Context-Engine", "cpa-v2")
	w.Header().Set("X-MiCliProxy-Context-Status", code)
	if !stream {
		writePoolCodeError(w, http.StatusConflict, code, message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	resp := map[string]interface{}{
		"object": "response",
		"status": "failed",
		"error":  map[string]string{"code": code, "message": message},
	}
	_ = writeSSEEvent(w, "response.failed", map[string]interface{}{"response": resp})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func codexMappingErrorCode(err error) string {
	switch {
	case errors.Is(err, storage.ErrCodexSessionMappingAmbiguous):
		return "codex_session_mapping_ambiguous"
	case errors.Is(err, storage.ErrCodexSessionEpochRetired), errors.Is(err, storage.ErrCodexSessionEpochConflict):
		return "codex_context_epoch_retired"
	default:
		return "codex_session_mapping_unidentified"
	}
}

func (s *Server) auditCodexMappingFailure(ctx context.Context, code string) {
	switch code {
	case "codex_session_mapping_ambiguous":
		atomic.AddUint64(&s.codexMappingAmbiguous, 1)
	default:
		atomic.AddUint64(&s.codexMappingUnidentified, 1)
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "codex_session_mapping_" + strings.TrimPrefix(code, "codex_session_mapping_"), State: "visible_error", Reason: code, Detail: "no_raw_identifiers"})
}

func (s *Server) codexMappingContextHeader(w http.ResponseWriter) {
	w.Header().Set("X-MiCliProxy-Context-Engine", "cpa-v2")
	w.Header().Set("X-MiCliProxy-Context-Status", "native")
}

func codexMappingRequiredRoute(mapping *codexSessionMapping, route scheduler.Route) scheduler.Route {
	if mapping == nil || !mapping.enabled {
		return route
	}
	accountID, egressID := mapping.requiredRoute()
	if accountID == "" {
		return route
	}
	route.RequiredAccountID = accountID
	route.RequiredEgressID = egressID
	route.ImmutableAffinity = true
	return route
}
