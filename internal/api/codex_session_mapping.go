package api

// Strict CPA session mapping for native Codex Responses. This layer identifies an
// exact downstream session/response alias, pins a normal stateful request to the
// corresponding account+egress, and supplies the encrypted internal UUID lifecycle
// to upstream. A goal checkpoint is consulted only after that binding disappears or
// the upstream confirms previous_response_id loss; steady-state turns remain native.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/upstream"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexSessionMappingContextKey struct{}
type codexStrictCPAContextKey struct{}
type codexDownstreamIdentityContextKey struct{}

type codexSessionGate struct {
	semaphore chan struct{}
	refs      int
}

type codexDownstreamIdentity struct {
	RootID       string
	ThreadID     string
	SessionID    string
	ParentID     string
	ForkedFromID string
	ResponseID   string
	TurnState    string
}

type codexDownstreamIdentityContext struct {
	body     []byte
	identity codexDownstreamIdentity
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
	// recoveryAliases retain only durable hierarchy correlators from a known-dead
	// tree while the new root is committed. They intentionally never contain a
	// response id or turn-state token, so an old upstream state pointer can never
	// become valid again through the new tree.
	recoveryAliases []storage.CodexSessionAlias
}

// codexContextMigration is a fresh, self-contained replay of a native Codex
// turn whose account-local Responses state is no longer usable.  The replay is
// assembled from the encrypted goal checkpoint when available, then committed as
// a new CPA epoch after its terminal response.  It deliberately carries no old
// previous_response_id or turn-state pointer to the replacement account.
type codexContextMigration struct {
	Retry   codexRetryRequest
	Mapping *codexSessionMapping
	Mode    string
}

var errCodexToolContextUnrecoverable = errors.New("codex tool context cannot be recovered without a paired durable checkpoint")

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
	// HTTP stateless mode intentionally wins over mapping. It is the stable default:
	// previous_response_id never reaches an account that may differ from the one that
	// created it. A downstream WebSocket remains one pinned connection, so stateless
	// mode is disabled there and mapping may still protect its native identity.
	if s.codexStatelessPassthrough(ctx) {
		return false
	}
	return s.flagEnabled(ctx, "codex_session_mapping_enabled", s.cfg.CodexSessionMappingEnabled)
}

func (s *Server) codexStatelessPassthrough(ctx context.Context) bool {
	// The downstream Responses-over-WebSocket path is a single persistent upstream
	// connection: its previous_response_id always references a response created moments
	// earlier on that same connection/account (warmup-state reuse via completeAppend),
	// never a cross-account continuation. Stripping it there would defeat the warmup
	// optimization for zero stability gain and cannot produce the cross-account 409/400
	// this mode exists to prevent, so stateless passthrough never applies to a WS turn.
	//
	// Once that upstream connection has failed and the session has permanently switched
	// to the HTTPS bridge, the premise is reversed: completeAppend's response id is
	// connection-scoped and the HTTP transport rejects it. Force the fallback leg through
	// the lossless stateless rebuild path regardless of the operator's ordinary HTTP
	// setting. This also disables fresh CPA mappings for later turns on that bridge.
	if forceCodexResponsesWebSocket(ctx) {
		return codexResponsesWebSocketUsesHTTPSFallback(ctx)
	}
	return s.flagEnabled(ctx, "codex_stateless_passthrough", s.cfg.CodexStatelessPassthrough)
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

// codexDownstreamSessionIdentity reads only true client correlators. Model name,
// prompt-cache key, request body prefix and API key are intentionally excluded: none
// is a session identity and using them would merge independent CLI conversations.
func codexDownstreamSessionIdentity(headers http.Header, body []byte) codexDownstreamIdentity {
	return codexDownstreamSessionIdentityWithMeta(headers, body, nil)
}

func codexDownstreamSessionIdentityWithMeta(headers http.Header, body []byte, meta *bodysource.BodyMeta) codexDownstreamIdentity {
	metaUsable := meta != nil && meta.Size == int64(len(body))
	hasMetadata := !metaUsable || meta.Fields["client_metadata"].Length > 0
	hasTurnObject := !metaUsable || meta.Fields["turn_metadata"].Length > 0
	metaString := func(key string) string {
		if !metaUsable {
			return ""
		}
		switch key {
		case "thread_id":
			return strings.TrimSpace(meta.ThreadID)
		case "session_id":
			return strings.TrimSpace(meta.SessionID)
		case "conversation_id":
			return strings.TrimSpace(meta.ConversationID)
		case "previous_response_id":
			return strings.TrimSpace(meta.PreviousResponseID)
		}
		var value string
		if raw := meta.Scalars[key]; len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
			return strings.TrimSpace(value)
		}
		return ""
	}
	jsonString := func(path string) string {
		value := gjson.GetBytes(body, path)
		if value.Type != gjson.String {
			return ""
		}
		return strings.TrimSpace(value.String())
	}
	rootString := func(key string) string {
		if metaUsable {
			return metaString(key)
		}
		return jsonString(key)
	}
	metadataString := func(key string) string {
		if !hasMetadata {
			return ""
		}
		return jsonString("client_metadata." + key)
	}
	embeddedTurn := ""
	for _, candidate := range []string{metadataString("x-codex-turn-metadata"), codexHeaderValue(headers, "x-codex-turn-metadata")} {
		if candidate != "" && gjson.Parse(candidate).IsObject() {
			embeddedTurn = candidate
			break
		}
	}
	turnString := func(key string) string {
		if embeddedTurn != "" {
			value := gjson.Get(embeddedTurn, key)
			if value.Type == gjson.String {
				return strings.TrimSpace(value.String())
			}
			return ""
		}
		if !hasTurnObject {
			return ""
		}
		return jsonString("turn_metadata." + key)
	}
	value := func(header string, bodyKeys ...string) string {
		if v := codexHeaderValue(headers, header); v != "" {
			return v
		}
		for _, key := range bodyKeys {
			if v := metadataString(key); v != "" {
				return v
			}
			if v := rootString(key); v != "" {
				return v
			}
			if v := turnString(key); v != "" {
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
	responseID := rootString("previous_response_id")
	turnState := firstNonEmpty(
		codexHeaderValue(headers, "x-codex-turn-state"),
		metadataString("x-codex-turn-state"),
		rootString("turn_state"),
		turnString("x-codex-turn-state"),
		turnString("turn_state"),
	)

	// A client may carry only a window id. Its prefix is the real thread id; the
	// ordinal is not a root identity and must never be used to create a new branch.
	if threadID == "" {
		if window := firstNonEmpty(codexHeaderValue(headers, "x-codex-window-id"), metadataString("x-codex-window-id")); window != "" {
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

func withCodexDownstreamIdentity(ctx context.Context, body []byte, identity codexDownstreamIdentity) context.Context {
	return context.WithValue(ctx, codexDownstreamIdentityContextKey{}, codexDownstreamIdentityContext{body: body, identity: identity})
}

func codexDownstreamSessionIdentityForRequest(r *http.Request, body []byte) codexDownstreamIdentity {
	if r != nil {
		if cached, ok := r.Context().Value(codexDownstreamIdentityContextKey{}).(codexDownstreamIdentityContext); ok &&
			len(cached.body) == len(body) && (len(body) == 0 || &cached.body[0] == &body[0]) {
			return cached.identity
		}
		return codexDownstreamSessionIdentity(r.Header, body)
	}
	return codexDownstreamSessionIdentity(nil, body)
}

// codexRetiredEpochFreshRootRequest removes only the account-local state pointers
// from a request whose mapped epoch has already been retired after an upstream
// context-loss confirmation.  Codex CLI's /goal resume control is consumed by the
// client: the gateway sees its next ordinary Responses request, not the literal
// command.  Starting that request as a fresh root prevents an already-proven-dead
// previous_response_id from being submitted forever.
//
// This is deliberately narrower than legacy context recovery.  It never rebuilds
// history, never rewrites a tool result, and never turns a client tool output into a
// user message.  A request containing such an output remains a visible retired-epoch
// error because its call can only be understood in the lost upstream context.
var codexFreshRootStatePaths = []string{
	"previous_response_id",
	"turn_state",
	"session_id",
	"parent_thread_id",
	"x-codex-parent-thread-id",
	"forked_from_thread_id",
	"x-codex-forked-from-thread-id",
	"client_metadata.x-codex-turn-state",
	"client_metadata.turn_state",
	"client_metadata.session_id",
	"client_metadata.parent_thread_id",
	"client_metadata.x-codex-parent-thread-id",
	"client_metadata.forked_from_thread_id",
	"client_metadata.x-codex-forked-from-thread-id",
	"turn_metadata.x-codex-turn-state",
	"turn_metadata.turn_state",
	"turn_metadata.session_id",
	"turn_metadata.parent_thread_id",
	"turn_metadata.x-codex-parent-thread-id",
	"turn_metadata.forked_from_thread_id",
	"turn_metadata.x-codex-forked-from-thread-id",
}

var codexFreshRootStateHeaders = []string{
	"X-Codex-Turn-State",
	"Session-Id",
	"X-Codex-Parent-Thread-Id",
	"X-Codex-Forked-From-Thread-Id",
}

func codexRetiredEpochFreshRootRequest(body []byte, header http.Header) ([]byte, http.Header, bool) {
	if !codexDownstreamSessionIdentity(header, body).stateful() || bodyHasClientToolResult(body) {
		return body, header, false
	}

	// Preserve every untouched request fragment, including input/tool definitions and
	// large JSON numbers.  sjson is used only on the fixed set of CPA state fields.
	out := append([]byte(nil), body...)
	for _, path := range codexFreshRootStatePaths {
		var err error
		out, err = sjson.DeleteBytes(out, path)
		if err != nil {
			return body, header, false
		}
	}
	// The transport commonly embeds turn metadata as a JSON string.  Preserve useful
	// non-state fields such as request_kind and turn_started_at, but remove the same
	// stale state/hierarchy pointers before the mapped upstream metadata builder sees
	// them.
	if updated, metadataChanged := stripCodexRetiredEpochEmbeddedTurnMetadata(out, "client_metadata.x-codex-turn-metadata"); metadataChanged {
		out = updated
	}

	nextHeader := header.Clone()
	for _, name := range codexFreshRootStateHeaders {
		nextHeader.Del(name)
	}
	if value := codexHeaderValue(nextHeader, "X-Codex-Turn-Metadata"); value != "" {
		if cleaned, metadataChanged := stripCodexRetiredEpochTurnMetadata(value); metadataChanged {
			nextHeader.Set("X-Codex-Turn-Metadata", cleaned)
		}
	}
	// Do not make a best-effort reset if a future client moves a state alias to a
	// currently unrecognised location.  Sending that alias to a new root would break
	// the strict CPA boundary.
	if codexDownstreamSessionIdentity(nextHeader, out).stateful() {
		return body, header, false
	}
	return out, nextHeader, true
}

// stripCodexStateForRecoveredRoot is the recovery counterpart to
// codexRetiredEpochFreshRootRequest.  Unlike the manual /goal-resume fallback it
// is allowed to retain a client tool result: a durable replay contains the paired
// historical call, so dropping the output would lose the user's completed work.
// It removes every account-local pointer from the body and headers before the
// replay is allowed onto a new account/CPA epoch.
func stripCodexStateForRecoveredRoot(body []byte, header http.Header) ([]byte, http.Header, bool) {
	out := append([]byte(nil), body...)
	for _, path := range codexFreshRootStatePaths {
		var err error
		out, err = sjson.DeleteBytes(out, path)
		if err != nil {
			return body, header, false
		}
	}
	if updated, changed := stripCodexRetiredEpochEmbeddedTurnMetadata(out, "client_metadata.x-codex-turn-metadata"); changed {
		out = updated
	}

	nextHeader := header.Clone()
	for _, name := range codexFreshRootStateHeaders {
		nextHeader.Del(name)
	}
	if value := codexHeaderValue(nextHeader, "X-Codex-Turn-Metadata"); value != "" {
		if cleaned, changed := stripCodexRetiredEpochTurnMetadata(value); changed {
			nextHeader.Set("X-Codex-Turn-Metadata", cleaned)
		}
	}
	if codexDownstreamSessionIdentity(nextHeader, out).stateful() {
		return body, header, false
	}
	return out, nextHeader, true
}

// recoverCodexSessionMapping transitions a strict, account-bound CPA tree into a
// fresh epoch after the bound account disappeared or the upstream rejected its
// previous_response_id.  A goal checkpoint is lossless and preferred; the legacy
// encrypted journal/degraded body remains a compatibility fallback.  The caller
// retries exactly once with the returned self-contained request.
func (s *Server) recoverCodexSessionMapping(ctx context.Context, r *http.Request, body []byte, header http.Header, pol downstreamPolicy, mapping *codexSessionMapping, contextError leakfilter.ResponsesContextErrorKind, reason string) (codexContextMigration, bool, error) {
	if s == nil || mapping == nil || !mapping.enabled {
		return codexContextMigration{}, false, nil
	}
	mapping.mu.Lock()
	hasBinding := mapping.binding != nil
	hasProspective := mapping.prospective != nil
	mapping.mu.Unlock()
	// A first root already has an in-memory upstream UUID by the time an upstream
	// risk response arrives, but it has no durable binding until response.completed.
	// Permit that one case to rotate too; all other recovery still requires a known
	// durable epoch.
	if !hasBinding && (!hasProspective || reason != "mapped_session_risk") {
		return codexContextMigration{}, false, nil
	}

	var retry codexRetryRequest
	mode := ""
	if replay := s.goalReplayBody(ctx, r, "codex", body); replay.Kind == goalResumeFound {
		retry = codexRetryRequest{Raw: replay.Body, Header: stripCodexServerStateHeaders(header)}
		mode = "rebuilt"
	} else {
		var ok bool
		retry, mode, ok = s.recoverResponsesContext(ctx, body, header, contextError)
		if !ok {
			// A root turn without previous_response_id already carries all context
			// required for this request. It can rotate the mapped upstream UUID without
			// reconstructing or weakening the payload.
			if codexDownstreamSessionIdentity(header, body).stateful() {
				return codexContextMigration{}, false, nil
			}
			retry = codexRetryRequest{Raw: append([]byte(nil), body...), Header: stripCodexServerStateHeaders(header)}
			mode = "rotated"
		}
	}
	if mode == "rebuilt" {
		if responsesHasUnpairedToolOutput(retry.Raw, leakfilter.ResponsesContextErrorNone) {
			return codexContextMigration{}, false, errCodexToolContextUnrecoverable
		}
	} else if responsesHasUnpairedToolOutput(body, contextError) {
		return codexContextMigration{}, false, errCodexToolContextUnrecoverable
	}
	cleanBody, cleanHeader, cleaned := stripCodexStateForRecoveredRoot(retry.Raw, retry.Header)
	if !cleaned {
		return codexContextMigration{}, false, nil
	}
	retry.Raw, retry.Header = cleanBody, cleanHeader

	retiredIdentity := codexDownstreamSessionIdentity(header, body)
	if hasBinding {
		if err := s.retireCodexSessionMapping(ctx, mapping, reason); err != nil &&
			!errors.Is(err, storage.ErrCodexSessionMappingNotFound) && !errors.Is(err, storage.ErrCodexSessionEpochRetired) {
			return codexContextMigration{}, false, err
		}
	}

	recoveryRequest := r.Clone(ctx)
	recoveryRequest.Header = retry.Header
	freshMapping, err := s.resolveCodexSessionMapping(ctx, recoveryRequest, retry.Raw, pol)
	if err != nil {
		return codexContextMigration{}, false, err
	}
	freshMapping.retainRetiredEpochHierarchy(retiredIdentity)
	return codexContextMigration{Retry: retry, Mapping: freshMapping, Mode: mode}, true, nil
}

func stripCodexRetiredEpochEmbeddedTurnMetadata(body []byte, path string) ([]byte, bool) {
	value := gjson.GetBytes(body, path)
	if !value.Exists() || value.Type != gjson.String {
		return body, false
	}
	cleaned, changed := stripCodexRetiredEpochTurnMetadata(value.String())
	if !changed {
		return body, false
	}
	out, err := sjson.SetBytes(body, path, cleaned)
	if err != nil {
		return body, false
	}
	return out, true
}

func stripCodexRetiredEpochTurnMetadata(raw string) (string, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
		return raw, false
	}
	changed := false
	for _, key := range []string{
		"x-codex-turn-state",
		"turn_state",
		"session_id",
		"parent_thread_id",
		"x-codex-parent-thread-id",
		"forked_from_thread_id",
		"x-codex-forked-from-thread-id",
	} {
		if _, ok := fields[key]; ok {
			delete(fields, key)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	cleaned, err := json.Marshal(fields)
	if err != nil {
		return raw, false
	}
	return string(cleaned), true
}

func codexSessionNamespace(pol downstreamPolicy, r *http.Request) string {
	// Check for X-Session-ID header for explicit session isolation
	// This allows multiple CLI instances with the same API key to maintain separate contexts
	if sessionID := strings.TrimSpace(r.Header.Get("X-Session-ID")); sessionID != "" {
		keyPart := strings.TrimSpace(pol.KeyHash)
		if keyPart == "" {
			if token := strings.TrimSpace(downstreamBearer(r)); token != "" {
				keyPart = hashAPIKey(token)
			}
		}
		if keyPart != "" {
			return "key:" + keyPart + ":session:" + sessionID
		}
		return "session:" + sessionID
	}

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

// codexSessionGateKey identifies the exact downstream branch that must commit in
// order. It is hashed before entering process memory shared state: neither raw
// session/response ids nor bearer values are retained in the gate map. Child agent
// threads use their own branch key and therefore remain independently concurrent.
func codexSessionGateKey(pol downstreamPolicy, r *http.Request, body []byte) string {
	if r == nil {
		return ""
	}
	id := codexDownstreamSessionIdentityForRequest(r, body)
	kind, value := "branch", id.ThreadID
	if value == "" {
		kind, value = "root", id.RootID
	}
	if value == "" {
		kind, value = "response", id.ResponseID
	}
	if value == "" {
		kind, value = "turn_state", id.TurnState
	}
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(codexSessionNamespace(pol, r) + "\x00" + kind + "\x00" + value))
	return fmt.Sprintf("%x", sum[:])
}

func (s *Server) releaseCodexSessionGateRef(key string, gate *codexSessionGate) {
	if s == nil || key == "" || gate == nil {
		return
	}
	s.codexSessionGatesMu.Lock()
	defer s.codexSessionGatesMu.Unlock()
	gate.refs--
	if gate.refs == 0 && s.codexSessionGates[key] == gate {
		delete(s.codexSessionGates, key)
	}
}

// acquireCodexSessionGate serializes commits for one downstream branch. Waiting is
// context-aware so a disconnected CLI cannot leave a stale waiter or retain the
// gate indefinitely.
func (s *Server) acquireCodexSessionGate(ctx context.Context, key string) (func(), error) {
	if s == nil || key == "" {
		return func() {}, nil
	}
	s.codexSessionGatesMu.Lock()
	gate := s.codexSessionGates[key]
	if gate == nil {
		gate = &codexSessionGate{semaphore: make(chan struct{}, 1)}
		s.codexSessionGates[key] = gate
	}
	gate.refs++
	s.codexSessionGatesMu.Unlock()

	select {
	case gate.semaphore <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() {
				<-gate.semaphore
				s.releaseCodexSessionGateRef(key, gate)
			})
		}, nil
	case <-ctx.Done():
		s.releaseCodexSessionGateRef(key, gate)
		return nil, ctx.Err()
	}
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
	mapping.identity = codexDownstreamSessionIdentityForRequest(r, body)
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
			// Preserve the retired binding long enough for the gateway to build a
			// durable fresh-root replay. It is never selected as an upstream route:
			// recoverCodexSessionMapping retires idempotently and resolves a new
			// mapping before its retry is issued.
			if errors.Is(err, storage.ErrCodexSessionEpochRetired) && binding.ID != "" {
				mapping.binding = &binding
				mapping.requiredAccount, mapping.requiredEgress = binding.AccountID, binding.EgressID
			}
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

	// A Codex client can retain forked_from_thread_id on every later turn of the
	// fork, even when that turn is self-contained and has no previous_response_id.
	// Resolve the fork's own durable root first. Treating each such turn as a new
	// fork creates a second upstream thread with the same downstream root alias;
	// its terminal commit then conflicts and leaves the returned response id
	// impossible to resume.
	if id.ForkedFromID != "" {
		if id.RootID != "" {
			if root, err := s.lookupCodexSessionAlias(ctx, mapping.namespace, storage.CodexSessionAlias{Type: "root", Value: id.RootID}); err == nil {
				if root.State == "active" {
					// A known root that was not created as this fork is an incompatible
					// downstream hierarchy claim. Do not silently turn it into a new
					// upstream session under the same root alias.
					if root.ForkedFromThreadID == "" {
						return mapping, storage.ErrCodexSessionMappingAmbiguous
					}
					if parent, parentErr := lookupBranchOrRoot(id.ForkedFromID); parentErr == nil && root.ForkedFromThreadID != parent.ThreadID {
						return mapping, storage.ErrCodexSessionMappingAmbiguous
					} else if parentErr != nil && !errors.Is(parentErr, storage.ErrCodexSessionMappingNotFound) {
						return mapping, parentErr
					}
					mapping.binding = root
					mapping.requiredAccount, mapping.requiredEgress = root.AccountID, root.EgressID
					return mapping, nil
				}
				// A retired fork root must not become the source of its replacement:
				// that would set ForkedFromThreadID to the retired fork itself rather
				// than to the original source thread. Fall through and resolve the
				// explicit fork source below.
			} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
				return mapping, err
			}
		}
		// This is the first turn of a new fork. Its source mapping is consulted
		// only to preserve an encrypted relationship; it never inherits the old
		// root/session identity or window generation.
		if parent, err := lookupBranchOrRoot(id.ForkedFromID); err == nil {
			mapping.anchor = parent
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return mapping, err
		}
		return mapping, nil
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

// mainCLI reports whether this mapping is the root CLI session rather than a
// child/fork. A root risk rotation retires the tree atomically; child failures do
// not independently rewrite the root session identity.
func (m *codexSessionMapping) mainCLI() bool {
	if m == nil || !m.enabled {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding := m.binding
	if binding == nil {
		binding = m.prospective
	}
	if binding == nil {
		return false
	}
	return strings.TrimSpace(binding.RootSessionID) != "" &&
		binding.RootSessionID == binding.ThreadID &&
		strings.TrimSpace(binding.ParentThreadID) == "" &&
		strings.TrimSpace(binding.ForkedFromThreadID) == ""
}

// durableMainCLI excludes a prospective first turn: there is no persisted epoch
// to retire until a root has completed successfully. Child and fork mappings are
// also excluded so their failures cannot invalidate the main CLI tree.
func (m *codexSessionMapping) durableMainCLI() bool {
	if m == nil || !m.enabled {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding := m.binding
	if binding == nil {
		return false
	}
	return strings.TrimSpace(binding.RootSessionID) != "" &&
		binding.RootSessionID == binding.ThreadID &&
		strings.TrimSpace(binding.ParentThreadID) == "" &&
		strings.TrimSpace(binding.ForkedFromThreadID) == ""
}

// codexMappedSessionRiskError identifies transport/account responses for which
// reusing the same upstream session identity creates a repeated-risk loop. Client
// schema/model errors are deliberately excluded because a new UUID cannot fix them.
func codexMappedSessionRiskError(status int, body []byte) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusConflict, http.StatusLocked, http.StatusTooEarly,
		http.StatusTooManyRequests:
		return true
	}
	if status >= http.StatusInternalServerError {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	return codexExplicitSessionRisk(body)
}

func codexExplicitSessionRisk(body []byte) bool {
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "session") {
		return false
	}
	for _, marker := range []string{"risk", "flagged", "blocked", "invalid", "expired", "revoked", "suspicious", "abuse"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// codexMappedSessionRotationRequired separates errors that may be tied to the
// generated upstream UUID from failures with a proven account/egress cause. A
// self-contained root can use the normal account failover path for the latter;
// a stateful root must first rebuild its durable context, but only when another
// account can actually serve it. CF/region failures are the exception because a
// new epoch can also elect a replacement egress on the same account.
func codexMappedSessionRotationRequired(status int, header http.Header, body []byte, movable, hasFailoverCandidate bool) bool {
	if !codexMappedSessionRiskError(status, body) {
		return false
	}
	detection := cf.Detect(status, header, body)
	if detection.Matched {
		return !movable
	}
	verdict := ban.Classify(false, status, header, body)
	switch verdict.State {
	case ban.RegionBlocked:
		return !movable
	case ban.PermissionDenied, ban.Banned:
		return !movable && hasFailoverCandidate
	case ban.AuthExpired:
		if verdict.Reason != "http_401" && verdict.Reason != "http_403" {
			return !movable && hasFailoverCandidate
		}
	case ban.RateLimited:
		if verdict.Reason != "http_429" {
			return !movable && hasFailoverCandidate
		}
	}
	return true
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

// retainRetiredEpochHierarchy lets the first successful fresh-root turn reclaim
// its client's stable root/session/branch aliases from a retired tree. Without
// this, a CLI that continues to send Session-Id after /goal resume would resolve
// that harmless hierarchy alias to the old retired binding on the very next turn.
// State aliases are deliberately excluded.
func (m *codexSessionMapping) retainRetiredEpochHierarchy(identity codexDownstreamIdentity) {
	if m == nil {
		return
	}
	aliases := identity.directAliases()
	if parent := strings.TrimSpace(identity.ParentID); parent != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "branch", Value: parent})
	}
	m.mu.Lock()
	m.recoveryAliases = append(m.recoveryAliases, aliases...)
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
func nativeCodexContinueBody(original []byte, previousResponseID string, continueText ...string) ([]byte, error) {
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
	text := "continue"
	if len(continueText) > 0 && strings.TrimSpace(continueText[0]) != "" {
		text = strings.TrimSpace(continueText[0])
	}
	continueItem, err := json.Marshal(map[string]interface{}{
		"role": "user",
		"content": []interface{}{map[string]interface{}{
			"type": "input_text",
			"text": text,
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
			"code":    "server_error",
			"message": publicRetryMessage,
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
func writeCodexSidecarStreamFailure(w io.Writer, id, model, _ string) error {
	response := map[string]interface{}{
		"id":     firstNonEmpty(strings.TrimSpace(id), "resp_pool_sidecar_stream"),
		"object": "response",
		"status": "failed",
		"error": map[string]string{
			"code":    "server_error",
			"message": publicRetryMessage,
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

func (s *Server) emitCodexNativeContinuationFailure(ctx context.Context, w io.Writer, id, model, reason string) error {
	atomic.AddUint64(&s.codexEOFCompensations, 1)
	if err := writeCodexNativeContinuationFailure(w, id, model); err != nil {
		return err
	}
	if s.store != nil {
		s.enqueueAudit(storage.AuditLogRow{
			Action: "codex_eof_terminal_compensation",
			State:  "emitted",
			Reason: firstNonEmpty(strings.TrimSpace(reason), "truncated_eof"),
			Detail: "native_protocol_no_raw_identifiers",
		})
	}
	return nil
}

// codexSessionMappingStats contains aggregate-only operational data. None of the
// HMAC aliases, internal UUIDs, response ids, or request content is exposed.
func (s *Server) codexSessionMappingStats(ctx context.Context) map[string]interface{} {
	stateless := s.codexStatelessPassthrough(ctx)
	return s.codexSessionMappingStatsConfigured(ctx, s.store, !stateless && s.codexSessionMappingEnabled(ctx), stateless, s.codexCPAStrict(ctx), int(s.codexSessionMappingRetention(ctx).Hours()/24))
}

func (s *Server) codexSessionMappingStatsConfigured(ctx context.Context, metricsStore *storage.Store, enabled, stateless, strict bool, retentionDays int) map[string]interface{} {
	stats := map[string]interface{}{
		"enabled":                        enabled,
		"stateless_passthrough":          stateless,
		"strict_cpa":                     strict,
		"retention_days":                 retentionDays,
		"bindings_created":               atomic.LoadUint64(&s.codexMappingBindingsCreated),
		"epoch_rotations":                atomic.LoadUint64(&s.codexMappingEpochRotations),
		"fresh_roots_after_context_loss": atomic.LoadUint64(&s.codexMappingFreshRoots),
		"unidentified":                   atomic.LoadUint64(&s.codexMappingUnidentified),
		"ambiguous":                      atomic.LoadUint64(&s.codexMappingAmbiguous),
		"native_continues":               atomic.LoadUint64(&s.codexNativeContinues),
		"eof_compensations":              atomic.LoadUint64(&s.codexEOFCompensations),
	}
	if metricsStore == nil {
		return stats
	}
	if active, retired, err := metricsStore.CodexSessionMappingMetrics(ctx); err == nil {
		stats["active"] = active
		stats["retired"] = retired
	}
	return stats
}

func responseTurnState(header http.Header, body []byte) string {
	return codexDownstreamSessionIdentity(header, body).TurnState
}

// commitCodexSessionMapping is the terminal-only counterpart of identitySnapshot.
// A storage fault is visible in audit/metrics but does not turn an already-successful
// upstream response into a fake failure.
func (s *Server) commitCodexSessionMapping(ctx context.Context, mapping *codexSessionMapping, lease scheduler.Lease, egress storage.EgressProfile, responseID, turnState string, compact bool) error {
	if mapping == nil || !mapping.enabled || mapping.snapshot == nil {
		s.recordCodexUpstreamAttempt(ctx, mapping, lease, egress, "terminal_success", http.StatusOK)
		return nil
	}
	// A client is allowed to close as soon as it receives the terminal frame. The
	// response/turn-state aliases are durability work for the following turn, so
	// detach downstream cancellation while keeping all request-scoped values and
	// cap the write. This also prevents a graceful worker drain from abandoning a
	// terminal it has already delivered.
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer persistCancel()
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
	expiresAt := time.Now().Add(s.codexSessionMappingRetention(persistCtx)).Unix()
	instructionSnapshot := mapping.instructions.snapshotCommit(binding.TreeID, expiresAt)
	aliases := mapping.identity.aliasesForBinding(*binding, responseID, turnState)
	aliases = append(aliases, mapping.recoveryAliases...)
	filteredAliases, droppedHierarchyAliases, err := s.filterLegacyCodexHierarchyAliasConflicts(
		persistCtx, mapping.namespace, binding.ID, aliases,
	)
	if err != nil {
		return err
	}
	committed, err := s.store.CommitCodexSessionBinding(persistCtx, storage.CodexSessionCommit{
		Namespace:           mapping.namespace,
		Binding:             *binding,
		Aliases:             filteredAliases,
		ExpiresAt:           expiresAt,
		InstructionSnapshot: instructionSnapshot,
	})
	if err != nil {
		return err
	}
	if compact {
		advanced, advanceErr := s.store.AdvanceCodexSessionWindowGeneration(persistCtx, committed.ID, committed.Epoch, committed.WindowGeneration, expiresAt)
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
		_ = s.store.InsertAuditLog(persistCtx, storage.AuditLogRow{
			Action: "codex_session_binding_created",
			State:  "active",
			Reason: "terminal_success",
			Detail: "metadata_only_no_raw_identifiers",
		})
	}
	if droppedHierarchyAliases > 0 {
		_ = s.store.InsertAuditLog(persistCtx, storage.AuditLogRow{
			Action: "codex_session_hierarchy_alias_conflict",
			State:  "degraded",
			Reason: "legacy_active_owner",
			Detail: fmt.Sprintf("dropped_count=%d;terminal_state_alias_preserved", droppedHierarchyAliases),
		})
	}
	s.recordCodexUpstreamAttemptBinding(persistCtx, committed, lease, egress, "terminal_success", http.StatusOK)
	return nil
}

// filterLegacyCodexHierarchyAliasConflicts contains damage left by older or
// concurrently active workers without weakening the state-pointer contract.
// A unique terminal response alias is still committed, so the next stateful turn
// can resume exactly. Conflicting root/session/branch hints are omitted rather
// than rolling back that terminal mapping. Response aliases remain strict and
// CommitCodexSessionBinding will reject any collision involving them.
func (s *Server) filterLegacyCodexHierarchyAliasConflicts(ctx context.Context, namespace, bindingID string, aliases []storage.CodexSessionAlias) ([]storage.CodexSessionAlias, int, error) {
	hasResponseAlias := false
	for _, alias := range aliases {
		if strings.EqualFold(strings.TrimSpace(alias.Type), "response") && strings.TrimSpace(alias.Value) != "" {
			hasResponseAlias = true
			break
		}
	}
	if !hasResponseAlias {
		return aliases, 0, nil
	}
	filtered := make([]storage.CodexSessionAlias, 0, len(aliases))
	dropped := 0
	for _, alias := range aliases {
		typ := strings.ToLower(strings.TrimSpace(alias.Type))
		if typ != "root" && typ != "session" && typ != "branch" {
			filtered = append(filtered, alias)
			continue
		}
		rows, err := s.store.FindCodexSessionAlias(ctx, namespace, alias)
		if errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			filtered = append(filtered, alias)
			continue
		}
		if err != nil {
			return nil, dropped, err
		}
		conflict := false
		for _, owner := range rows {
			if owner.State == "active" && owner.ID != bindingID {
				conflict = true
				break
			}
		}
		if conflict {
			dropped++
			continue
		}
		filtered = append(filtered, alias)
	}
	return filtered, dropped, nil
}

func (s *Server) recordCodexUpstreamAttempt(ctx context.Context, mapping *codexSessionMapping, lease scheduler.Lease, egress storage.EgressProfile, state string, statusCode int) {
	if s == nil || s.store == nil {
		return
	}
	binding, ok := mapping.upstreamAttemptBinding()
	if !ok {
		// Stateless passthrough deliberately has no durable CPA tree. Keep those
		// transport attempts observable under a server-owned per-turn identifier so
		// diagnostics still cover current traffic without retaining client aliases,
		// request bodies, response ids, or other protocol state.
		eventID := strings.TrimSpace(usageEventIDFromContext(ctx))
		if eventID == "" {
			eventID = strings.TrimSpace(requestIDFromContext(ctx))
		}
		if eventID == "" {
			return
		}
		binding = storage.CodexSessionBinding{
			TreeID:    "request:" + eventID,
			AccountID: lease.Account.ID,
			EgressID:  egress.ID,
			Epoch:     lease.RouteEpoch,
		}
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
		"codex_context_epoch_retired":        "Codex context epoch was retired and no recoverable checkpoint is available for this previous_response_id.",
		"codex_tool_context_unrecoverable":   "The mapped session cannot be rotated because this tool result has no recoverable matching call. Start a new root turn instead of replaying the tool result.",
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
	case errors.Is(err, errCodexToolContextUnrecoverable):
		return "codex_tool_context_unrecoverable"
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
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: code, State: "visible_error", Reason: code, Detail: "no_raw_identifiers"})
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
