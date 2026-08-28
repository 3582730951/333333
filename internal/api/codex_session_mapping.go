package api

// Strict CPA session mapping for native Codex Responses. This layer identifies an
// exact downstream session/response alias, pins a normal stateful request to the
// corresponding account+egress, and supplies the encrypted internal UUID lifecycle
// to upstream. A goal checkpoint is consulted only after that binding disappears or
// the upstream confirms previous_response_id loss; steady-state turns remain native.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
type codexGoalQuotaGraceContextKey struct{}

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

// durableSessionAlias reports whether session_id is the canonical root selected
// for this request. Some clients expose one process-level session_id across
// several independent windows; when a concrete thread/conversation id differs,
// persisting or resolving that weak marker would merge those windows.
func (i codexDownstreamIdentity) durableSessionAlias() bool {
	return i.SessionID != "" && i.RootID != "" && i.SessionID == i.RootID
}

func (i codexDownstreamIdentity) directAliases() []storage.CodexSessionAlias {
	aliases := make([]storage.CodexSessionAlias, 0, 4)
	if i.RootID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "root", Value: i.RootID})
	}
	if i.ThreadID != "" && i.ThreadID != i.RootID {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "branch", Value: i.ThreadID})
	}
	if i.durableSessionAlias() {
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
//
// The root/session pair is only persistable when this identity actually owns a root
// (RootID != ""), which a child never does. A sub-agent whose parent thread has not
// itself contacted the pool has no anchor, so it is bound as an upstream root even
// though it is a child downstream; its own concrete thread is then the only alias
// that can name it. Testing the binding shape alone used to leave that case with no
// hierarchy alias at all, so its next turn resolved only through the turn-state
// pointer, failed the mandatory parent lookup, recovered into a second binding, and
// from then on the concrete thread and the state pointer named two different
// bindings — the permanent "aliases resolve to more than one internal session".
func (i codexDownstreamIdentity) aliasesForBinding(binding storage.CodexSessionBinding, responseID, responseTurnState string) []storage.CodexSessionAlias {
	aliases := append([]storage.CodexSessionAlias(nil), i.stateAliases()...)
	if binding.ThreadID == binding.RootSessionID && i.RootID != "" {
		aliases = append(aliases, storage.CodexSessionAlias{Type: "root", Value: i.RootID})
		if i.durableSessionAlias() {
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
	// clientScope is the Goal alias namespace captured before any recovery path
	// strips account-local Session-Id fields from a retry. It is already an opaque
	// digest and never contains a raw downstream identifier.
	clientScope string
	identity    codexDownstreamIdentity

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
	// Goal quota grace is latched by the downstream turn id after observing the
	// exact Codex Goal continuation wrapper (or a successful Goal tool result).
	// It is copied into encrypted mapping identity so tool-output requests and a
	// process restart cannot lose the decision mid-turn.
	goalModeActive bool
	goalTurnID     string
	// rotateUpstreamSessionOnSafety is the per-user-group safety-rotation toggle
	// latched at resolve. When the terminal carries the Responses safety_buffering
	// control field, safetyRotationPending marks the commit to rotate the binding's
	// upstream RootSessionID to a fresh generated id (downstream mapping unchanged).
	// safetyDetachRequest marks the single request immediately after a rotation,
	// which detaches from the retired response chain by stripping CPA state pointers
	// before the upstream request is built.
	rotateUpstreamSessionOnSafety bool
	safetyRotationPending         bool
	safetyDetachRequest           bool
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

const codexRecoveryReasonUnidentifiedMapping = "unidentified_session_mapping"

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

func withCodexGoalQuotaGrace(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, codexGoalQuotaGraceContextKey{}, active)
}

func codexGoalQuotaGraceFromContext(ctx context.Context) bool {
	active, _ := ctx.Value(codexGoalQuotaGraceContextKey{}).(bool)
	return active
}

type codexGoalTurnSignal int

const (
	codexGoalTurnUnknown codexGoalTurnSignal = iota
	codexGoalTurnInactive
	codexGoalTurnActive
)

func codexDownstreamTurnID(headers http.Header, body []byte) string {
	jsonString := func(path string) string {
		value := gjson.GetBytes(body, path)
		if value.Type != gjson.String {
			return ""
		}
		return strings.TrimSpace(value.String())
	}
	for _, value := range []string{
		jsonString("client_metadata.turn_id"),
		jsonString("turn_metadata.turn_id"),
		jsonString("turn_id"),
	} {
		if value != "" {
			return value
		}
	}
	for _, raw := range []string{
		jsonString("client_metadata.x-codex-turn-metadata"),
		codexHeaderValue(headers, "x-codex-turn-metadata"),
	} {
		if value := gjson.Get(raw, "turn_id"); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return strings.TrimSpace(value.String())
		}
	}
	return ""
}

func codexGoalContinuationText(text string) bool {
	text = strings.TrimSpace(text)
	const firstLine = "Continue working toward the active thread goal."
	if strings.HasPrefix(text, `<codex_internal_context source="goal">`) &&
		strings.HasSuffix(text, `</codex_internal_context>`) {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, `<codex_internal_context source="goal">`), `</codex_internal_context>`))
		return inner == firstLine || strings.HasPrefix(inner, firstLine+"\n")
	}
	// Stable Codex still accepts the legacy wrapper when resuming an older rollout.
	if strings.HasPrefix(text, `<goal_context>`) && strings.HasSuffix(text, `</goal_context>`) {
		inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, `<goal_context>`), `</goal_context>`))
		return inner == firstLine || strings.HasPrefix(inner, firstLine+"\n")
	}
	return false
}

func codexGoalStatusSignal(output interface{}) codexGoalTurnSignal {
	var value interface{} = output
	if text, ok := output.(string); ok {
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			return codexGoalTurnUnknown
		}
	}
	root, ok := value.(map[string]interface{})
	if !ok {
		return codexGoalTurnUnknown
	}
	goal, _ := root["goal"].(map[string]interface{})
	status := strings.ToLower(strings.TrimSpace(streamString(goal["status"])))
	switch status {
	case "active":
		return codexGoalTurnActive
	case "complete", "completed", "blocked", "paused", "usage_limited", "usagelimited":
		return codexGoalTurnInactive
	default:
		return codexGoalTurnUnknown
	}
}

// codexGoalSignal inspects input in order and returns the newest explicit state.
// Looking only for a marker anywhere in the body is wrong because full-history
// transports retain old Goal wrappers after a later ordinary user turn starts.
func codexGoalSignal(body []byte) codexGoalTurnSignal {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return codexGoalTurnUnknown
	}
	input, present := root["input"]
	if !present {
		return codexGoalTurnUnknown
	}
	items := appendItems(nil, input)
	signal := codexGoalTurnUnknown
	goalCalls := map[string]string{}
	for _, raw := range items {
		if text, ok := raw.(string); ok {
			if codexGoalContinuationText(text) {
				signal = codexGoalTurnActive
			} else if strings.TrimSpace(text) != "" {
				signal = codexGoalTurnInactive
			}
			continue
		}
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(streamString(item["type"])))
		role := strings.ToLower(strings.TrimSpace(streamString(item["role"])))
		if role == "user" && (kind == "" || kind == "message") {
			texts := make([]string, 0, 2)
			switch content := item["content"].(type) {
			case string:
				texts = append(texts, content)
			case []interface{}:
				for _, part := range content {
					if object, ok := part.(map[string]interface{}); ok && strings.EqualFold(streamString(object["type"]), "input_text") {
						texts = append(texts, streamString(object["text"]))
					}
				}
			}
			for _, text := range texts {
				if codexGoalContinuationText(text) {
					signal = codexGoalTurnActive
				} else if strings.TrimSpace(text) != "" {
					signal = codexGoalTurnInactive
				}
			}
		}
		if kind == "function_call" || kind == "custom_tool_call" {
			name := strings.ToLower(strings.TrimSpace(streamString(item["name"])))
			if name == "create_goal" || name == "update_goal" {
				if callID := strings.TrimSpace(streamString(item["call_id"])); callID != "" {
					goalCalls[callID] = name
				}
			}
			continue
		}
		if toolOutputPairKind(item) != "" {
			callID := strings.TrimSpace(streamString(item["call_id"]))
			_, paired := goalCalls[callID]
			candidate := codexGoalStatusSignal(item["output"])
			// An incremental tool-output request may omit the preceding call because it
			// lives behind previous_response_id. The official Goal output is still
			// self-identifying through its {goal:{status:...}} envelope.
			if candidate != codexGoalTurnUnknown && (paired || callID != "") {
				signal = candidate
			}
		}
	}
	return signal
}

func (m *codexSessionMapping) observeGoalTurn(headers http.Header, body []byte) bool {
	return m.observeGoalTurnWithMeta(headers, body, nil)
}

func (m *codexSessionMapping) observeGoalTurnWithMeta(headers http.Header, body []byte, meta *bodysource.BodyMeta) bool {
	if m == nil || !m.enabled {
		return false
	}
	// Goal parsing intentionally understands full tool history, but decoding every
	// 128K-1M ordinary root turn is unnecessary. A non-goal mapping can activate
	// only through the stable continuation wrapper or a Goal tool/result marker.
	m.mu.Lock()
	binding := m.binding
	if binding == nil {
		binding = m.prospective
	}
	active := m.goalModeActive
	if binding != nil {
		active = binding.GoalModeActive
	}
	rootOwner := strings.TrimSpace(m.identity.ParentID) == "" && strings.TrimSpace(m.identity.ForkedFromID) == "" &&
		(strings.TrimSpace(m.identity.ThreadID) == "" || strings.TrimSpace(m.identity.RootID) == "" || m.identity.ThreadID == m.identity.RootID)
	if !rootOwner {
		m.goalModeActive, m.goalTurnID = false, ""
		if binding != nil {
			binding.GoalModeActive, binding.GoalTurnID = false, ""
		}
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	if !active && !codexBodyMayContainGoalSignalWithMeta(body, meta) {
		return false
	}
	signal := codexGoalSignal(body)
	turnID := ""
	if signal != codexGoalTurnInactive {
		turnID = codexDownstreamTurnID(headers, body)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding = m.binding
	if binding == nil {
		binding = m.prospective
	}
	active, latchedTurn := m.goalModeActive, m.goalTurnID
	if binding != nil {
		active, latchedTurn = binding.GoalModeActive, binding.GoalTurnID
	}
	rootOwner = strings.TrimSpace(m.identity.ParentID) == "" && strings.TrimSpace(m.identity.ForkedFromID) == "" &&
		(strings.TrimSpace(m.identity.ThreadID) == "" || strings.TrimSpace(m.identity.RootID) == "" || m.identity.ThreadID == m.identity.RootID)
	if !rootOwner {
		active, latchedTurn = false, ""
	} else {
		switch signal {
		case codexGoalTurnActive:
			active = true
			if turnID != "" {
				latchedTurn = turnID
			}
		case codexGoalTurnInactive:
			active, latchedTurn = false, ""
		case codexGoalTurnUnknown:
			if active && turnID != "" && latchedTurn != "" && turnID != latchedTurn {
				active = false
			}
		}
	}
	m.goalModeActive, m.goalTurnID = active, latchedTurn
	if binding != nil {
		binding.GoalModeActive, binding.GoalTurnID = active, latchedTurn
	}
	return active && (turnID == "" || latchedTurn == "" || turnID == latchedTurn)
}

func codexBodyMayContainGoalSignal(body []byte) bool {
	// Every supported marker contains the stable lowercase token "goal". Reject
	// ordinary large contexts with one scan before checking the rarer exact forms.
	if !bytes.Contains(body, []byte("goal")) {
		return false
	}
	if bytes.Contains(body, []byte("Continue working toward the active thread goal.")) ||
		bytes.Contains(body, []byte(`"create_goal"`)) || bytes.Contains(body, []byte(`"update_goal"`)) {
		return true
	}
	// Tool outputs are commonly JSON encoded inside a JSON string, so their quotes
	// are escaped. The stable key names remain a narrow, allocation-free prefilter.
	return bytes.Contains(body, []byte("status"))
}

func codexBodyMayContainGoalSignalWithMeta(body []byte, meta *bodysource.BodyMeta) bool {
	if meta != nil && meta.Size == int64(len(body)) {
		return meta.GoalSignalQualified
	}
	return codexBodyMayContainGoalSignal(body)
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
	// Once that upstream connection has failed, the first HTTPS turn must replace
	// its connection-scoped response id with a lossless durable replay. A completed
	// HTTP response then becomes the native continuation point, so later HTTPS turns
	// keep mapping enabled and must not rebuild the full history again.
	if forceCodexResponsesWebSocket(ctx) {
		return codexResponsesWebSocketNeedsHTTPSRecovery(ctx)
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
	conversationID := value("conversation-id", "conversation_id")
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
	// A concrete root thread/conversation is stronger than session_id. Several
	// downstream CLIs use one process-level session marker for independent windows;
	// choosing that marker first made those windows share one upstream tree.
	//
	// A child keeps RootID empty: its exact parent alias establishes tree ownership.
	// Treating either the child thread or a weak session marker as a root here can
	// allocate or attach the branch to an unrelated tree.
	rootID := ""
	if parentID == "" {
		rootID = firstNonEmpty(threadID, conversationID, sessionID)
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

// safetySessionRotationEnabledFor reports whether the user-group safety-rotation
// toggle is on for a request. Opt-in is per user group via
// config.SafetySessionRotationGroups; legacy keys without a user group are off.
func (s *Server) safetySessionRotationEnabledFor(userGroupID string) bool {
	if s == nil {
		return false
	}
	return s.cfg.SafetySessionRotationGroups[strings.TrimSpace(userGroupID)]
}

// codexSafetyRotationFreshRequest detaches the single post-rotation turn from the
// retired safety-buffered chain. Unlike codexRetiredEpochFreshRootRequest it is
// allowed to operate on a stateful downstream request — the whole point is to keep
// the downstream session mapping intact while presenting a clean new upstream turn.
func codexSafetyRotationFreshRequest(body []byte, header http.Header) ([]byte, http.Header, bool) {
	out := append([]byte(nil), body...)
	for _, path := range codexFreshRootStatePaths {
		var err error
		out, err = sjson.DeleteBytes(out, path)
		if err != nil {
			return body, header, false
		}
	}
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
	return out, nextHeader, true
}

// codexResponseSafetyBuffered reports whether a completed non-streaming upstream
// Responses body carries the safety_buffering control field — the codex protocol's
// "security-not-displayed" signal that withheld content from the response.
func codexResponseSafetyBuffered(body []byte) bool {
	return gjson.ParseBytes(body).Get("safety_buffering").Exists()
}

// bindingHasSafetyRotation reports whether the resolved binding still carries a
// pending safety-rotation marker, i.e. this request is the one immediately after a
// rotation and must detach from the retired chain.
func (m *codexSessionMapping) bindingHasSafetyRotation() bool {
	if m == nil || !m.enabled {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	binding := m.binding
	if binding == nil {
		binding = m.prospective
	}
	return binding != nil && binding.SafetyRotatedAt != 0
}

// noteSafetyBuffering latches that the current terminal carried the upstream
// safety_buffering field so the commit rotates the binding to a fresh session id.
func (m *codexSessionMapping) noteSafetyBuffering() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.safetyRotationPending = true
	m.mu.Unlock()
}

// markSafetyDetached records that this request was the post-rotation detach turn,
// so the commit clears the binding's pending rotation marker after persisting.
func (m *codexSessionMapping) markSafetyDetached() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.safetyDetachRequest = true
	m.mu.Unlock()
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
	if !hasBinding {
		switch reason {
		case "mapped_session_risk":
			if !hasProspective {
				return codexContextMigration{}, false, nil
			}
		case codexRecoveryReasonUnidentifiedMapping:
			// A terminal may have committed its encrypted Goal checkpoint while the
			// optional CPA alias write was lost, conflicted, or interrupted. In that
			// narrow state there is no old binding to retire, but the exact durable
			// response alias is still sufficient to build a safe fresh epoch below.
		default:
			return codexContextMigration{}, false, nil
		}
	}

	var retry codexRetryRequest
	mode := ""
	if replay := s.goalReplayBody(ctx, r, "codex", body); replay.Kind == goalResumeFound {
		retry = codexRetryRequest{Raw: replay.Body, Header: stripCodexServerStateHeaders(header)}
		mode = "rebuilt"
	} else if reason == codexRecoveryReasonUnidentifiedMapping {
		// Never degrade or guess when the CPA lookup itself is missing. The only
		// authorized source for a replacement epoch is an exact Goal checkpoint.
		return codexContextMigration{}, false, nil
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
	if contextError == leakfilter.ResponsesContextErrorEncryptedFunctionOutput {
		// Even a lossless durable replay can contain the exact completed tool
		// exchange whose encrypted payload the upstream rejected. Preserve its
		// readable result as inert user context and remove the unusable ciphertext
		// before installing the fresh CPA epoch.
		retry.Raw = degradedResponsesReplayForContextError(retry.Raw, contextError)
		retry.Header = stripCodexServerStateHeaders(retry.Header)
		mode = "degraded"
	}
	if mode == "rebuilt" {
		if responsesHasUnpairedToolOutput(retry.Raw, leakfilter.ResponsesContextErrorNone) {
			return codexContextMigration{}, false, errCodexToolContextUnrecoverable
		}
	} else if contextError == leakfilter.ResponsesContextErrorEncryptedFunctionOutput {
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
	freshMapping, err := s.resolveCodexSessionMappingInNamespace(
		ctx, recoveryRequest, retry.Raw, pol, mapping.namespace, mapping.clientScope,
	)
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

// codexLegacySessionNamespace reproduces the pre-native-client namespace exactly.
// It is used only as a one-turn compatibility bridge for an exact response or
// turn-state alias created before automatic protocol-native isolation shipped.
func codexLegacySessionNamespace(pol downstreamPolicy, r *http.Request) string {
	policyKeyPart := strings.TrimSpace(pol.KeyHash)
	keyPart := policyKeyPart
	bearerDerived := false
	if keyPart == "" && r != nil {
		if token := strings.TrimSpace(downstreamBearer(r)); token != "" {
			keyPart = hashAPIKey(token)
			bearerDerived = true
		}
	}
	if policyKeyPart != "" {
		return "key:" + policyKeyPart
	}
	if bearerDerived {
		return "bearer:" + keyPart
	}
	// Open-mode requests still need a namespace, but this is never used by itself
	// to resolve a session: an exact root/thread/response/turn alias is mandatory.
	return "unauthenticated"
}

func codexSessionNamespace(pol downstreamPolicy, r *http.Request) string {
	policyKeyPart := strings.TrimSpace(pol.KeyHash)
	keyPart := policyKeyPart
	if keyPart == "" && r != nil {
		if token := strings.TrimSpace(downstreamBearer(r)); token != "" {
			keyPart = hashAPIKey(token)
		}
	}
	if r != nil {
		// Preserve both previously supported explicit namespace formats.
		if strings.TrimSpace(r.Header.Get(poolClientInstanceHeader)) != "" {
			return "client:" + downstreamClientScope(keyPart, r)
		}
		if sessionID := strings.TrimSpace(r.Header.Get("X-Session-ID")); sessionID != "" {
			if keyPart != "" {
				return "key:" + keyPart + ":session:" + sessionID
			}
			return "session:" + sessionID
		}

		// Codex emits Session-Id without provider-specific configuration. It is a
		// process/client boundary while Thread-Id remains the exact tree alias.
		// Conversation-Id is accepted for compatible clients. A bare Thread-Id is
		// deliberately not used as the CPA namespace because child agents need to
		// resolve parent aliases in the same namespace; it still scopes Goal state.
		kind, _ := downstreamClientIdentity(r)
		if kind == "codex_session" || kind == "codex_conversation" {
			return "client:" + downstreamClientScope(keyPart, r)
		}
	}
	return codexLegacySessionNamespace(pol, r)
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
	return s.resolveCodexSessionMappingInNamespace(ctx, r, body, pol, "", "")
}

// resolveCodexSessionMappingInNamespace keeps an already-identified downstream
// namespace across context-recovery retries. Those retries intentionally strip the
// downstream Session-Id before building a fresh upstream identity; recomputing the
// namespace from that sanitized request would silently move the tree back to the
// shared API-key namespace.
func (s *Server) resolveCodexSessionMappingInNamespace(ctx context.Context, r *http.Request, body []byte, pol downstreamPolicy, forcedNamespace, forcedClientScope string) (*codexSessionMapping, error) {
	mapping := &codexSessionMapping{enabled: s.codexSessionMappingEnabled(ctx)}
	if !mapping.enabled {
		return mapping, nil
	}
	mapping.rotateUpstreamSessionOnSafety = s.safetySessionRotationEnabledFor(pol.UserGroupID)
	keyPart := strings.TrimSpace(pol.KeyHash)
	if keyPart == "" && r != nil {
		if token := strings.TrimSpace(downstreamBearer(r)); token != "" {
			keyPart = hashAPIKey(token)
		}
	}
	mapping.clientScope = downstreamClientScope(keyPart, r)
	if strings.TrimSpace(forcedNamespace) != "" {
		mapping.namespace = strings.TrimSpace(forcedNamespace)
		mapping.clientScope = strings.TrimSpace(forcedClientScope)
	} else {
		mapping.namespace = codexSessionNamespace(pol, r)
	}
	mapping.identity = codexDownstreamSessionIdentityForRequest(r, body)
	id := mapping.identity
	resolutionNamespace := mapping.namespace
	lookupBranchOrRoot := func(value string) (*storage.CodexSessionBinding, error) {
		if value == "" {
			return nil, storage.ErrCodexSessionMappingNotFound
		}
		if binding, err := s.lookupCodexSessionAlias(ctx, resolutionNamespace, storage.CodexSessionAlias{Type: "branch", Value: value}); err == nil {
			return binding, nil
		} else if !errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			return nil, err
		}
		return s.lookupCodexSessionAlias(ctx, resolutionNamespace, storage.CodexSessionAlias{Type: "root", Value: value})
	}

	if id.stateful() {
		binding, err := s.store.ResolveCodexSessionAliases(ctx, mapping.namespace, id.stateAliases())
		if errors.Is(err, storage.ErrCodexSessionMappingNotFound) {
			legacyNamespace := codexLegacySessionNamespace(pol, r)
			if legacyNamespace != mapping.namespace {
				legacyBinding, legacyErr := s.store.ResolveCodexSessionAliases(ctx, legacyNamespace, id.stateAliases())
				if legacyErr == nil || errors.Is(legacyErr, storage.ErrCodexSessionEpochRetired) {
					binding, err = legacyBinding, legacyErr
					resolutionNamespace = legacyNamespace
				} else if !errors.Is(legacyErr, storage.ErrCodexSessionMappingNotFound) {
					return mapping, legacyErr
				}
			}
		}
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
				candidate, lookupErr := s.lookupCodexSessionAlias(ctx, resolutionNamespace, alias)
				if errors.Is(lookupErr, storage.ErrCodexSessionMappingNotFound) {
					// A state pointer is the authoritative, terminal-issued alias.
					// Tolerate an unbound hierarchy value so a pre-fix
					// session_id-root mapping can migrate to the concrete thread on
					// its next successful terminal. A hierarchy alias already owned
					// by another tree is still rejected below.
					continue
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
			switch {
			case errors.Is(lookupErr, storage.ErrCodexSessionMappingNotFound):
				// An unbound parent is an absent relation, not a conflicting one, and
				// the state alias above already named this tree exactly. A sub-agent's
				// parent thread need never issue a request of its own: `codex review`
				// and remote compaction drive only the sub-agent thread, so its root
				// thread has no binding to agree with. Rejecting that as unidentified
				// sent every such turn through context recovery, which minted a second
				// binding in the same namespace.
			case lookupErr != nil:
				return mapping, lookupErr
			case parent.State != "active":
				return mapping, storage.ErrCodexSessionEpochRetired
			case parent.TreeID != binding.TreeID:
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
				switch {
				case errors.Is(parentErr, storage.ErrCodexSessionMappingNotFound):
					// Same absent-parent tolerance as the stateful path above: this
					// branch alias is the sub-agent's own concrete thread, so it
					// already identifies the tree without the parent.
				case parentErr != nil:
					return mapping, parentErr
				case parent.State != "active":
					return mapping, storage.ErrCodexSessionEpochRetired
				case parent.TreeID != branch.TreeID || branch.ParentThreadID != parent.ThreadID:
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
	if id.durableSessionAlias() {
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

// sanitizePendingRootAgentEncryptedContent removes subagent-owned ciphertext only
// while this request is creating a prospective root. A durable continuation must
// keep its native payload byte-for-byte because its bound upstream can decrypt the
// original blocks; child/fork requests likewise retain their parent-owned state.
func (m *codexSessionMapping) sanitizePendingRootAgentEncryptedContent(body []byte, _ http.Header, metadata ...*bodysource.BodyMeta) ([]byte, int) {
	if m == nil || !m.enabled {
		return body, 0
	}
	m.mu.Lock()
	identity := m.identity
	prospective := m.prospective
	anchor := m.anchor
	// A nil anchor is an ordinary first root. A retired root anchor is also a new
	// cryptographic epoch: it exists only to carry the next epoch number and must
	// not make foreign agent ciphertext look decryptable. Active anchors remain
	// excluded because they represent a real parent/child relationship.
	retiredRootAnchor := anchor != nil && anchor.State != "active" &&
		identity.RootID != "" && identity.ThreadID == identity.RootID
	pendingRoot := m.binding == nil && (anchor == nil || retiredRootAnchor) && !identity.stateful() &&
		identity.ParentID == "" && identity.ForkedFromID == "" &&
		(identity.RootID == "" || identity.ThreadID == identity.RootID)
	if pendingRoot && prospective != nil {
		pendingRoot = prospective.RootSessionID != "" &&
			prospective.ThreadID == prospective.RootSessionID &&
			prospective.ParentThreadID == "" && prospective.ForkedFromThreadID == ""
	}
	m.mu.Unlock()
	if !pendingRoot {
		return body, 0
	}
	if len(metadata) > 0 && metadata[0] != nil && metadata[0].Size == int64(len(body)) && !metadata[0].EncryptedContentKey {
		return body, 0
	}
	return stripAgentMessageEncryptedContent(body)
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

func (m *codexSessionMapping) identitySnapshot(secret []byte, lease scheduler.Lease, osHint string, convergenceMode ...string) (*upstream.CodexIdentitySnapshot, error) {
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
		created := storage.CodexSessionBinding{
			AccountID: lease.Account.ID, EgressID: lease.Egress.ID, State: "active",
			GoalModeActive: m.goalModeActive, GoalTurnID: m.goalTurnID,
		}
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
		// An omitted mode converges rather than diverges: a caller that did not state a
		// policy must not silently mint an exit-scoped device for this account.
		mode := "account"
		if len(convergenceMode) > 0 {
			mode = convergenceMode[0]
		}
		binding.InstallationID = identity.CodexDeviceWithConvergence(secret, lease.Account.ID, lease.Egress.ID, binding.DeviceOSHint, mode).MachineID
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
	// Preserve the complete native Lite prefix (additional_tools, base instructions,
	// group/Super-Instruct additions, permissions, collaboration mode, skills,
	// environment, and AGENTS developer items) while dropping prior conversation
	// content. Targeted sjson edits keep tool values and unrelated large integers
	// byte-exact.
	input := []json.RawMessage{continueItem}
	if items, ok := codexRawInputItems(fields["input"]); ok && len(items) > 0 && codexLiteAdditionalTools(items[0]) {
		input = append(input[:0], items[0])
		for index := 1; index < len(items) && codexDeveloperMessage(items[index]); index++ {
			input = append(input, items[index])
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
	persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), codexStateCommitTimeout)
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
	// Safety-buffering rotation: when the terminal carried the Responses
	// safety_buffering field under the user-group toggle, present a fresh upstream
	// session id on the binding while the downstream aliases (which resolve this
	// same binding) stay untouched. The single following turn detaches from the
	// retired response chain and then clears the marker. The in-flight snapshot is
	// deliberately left alone: a separately issued native continue within this
	// request still owns the old session chain.
	if mapping.rotateUpstreamSessionOnSafety {
		if mapping.safetyRotationPending {
			if binding.ThreadID != "" && binding.ThreadID != binding.RootSessionID {
				// Child branch: the safety-buffered turn lived in a child thread
				// under a still-valid root session. Rotating the root here would
				// leave ThreadID/ParentThreadID pointing into the old (retired)
				// tree while the root names a new session — an upstream thread can
				// never sit under a different session id, so the next continuation
				// surfaces as a corrupt session. Instead open a fresh child thread
				// under the same parent: session-id and x-codex-parent-thread-id
				// stay valid, the child thread id is new, and the stripped
				// previous_response_id cannot resolve into the retired child chain.
				binding.ThreadID = identity.NewUUIDv7()
			} else {
				previousSession := binding.RootSessionID
				binding.RootSessionID = identity.NewUUIDv7()
				if binding.ThreadID != "" && binding.ThreadID == previousSession {
					binding.ThreadID = binding.RootSessionID
				}
			}
			binding.SafetyRotatedAt = storage.Now()
			s.enqueueAudit(storage.AuditLogRow{
				Action: "codex_upstream_session_safety_rotated",
				State:  "active",
				Reason: "safety_buffering",
				Detail: "downstream_mapping_preserved_upstream_session_rotated",
			})
		} else if mapping.safetyDetachRequest {
			binding.SafetyRotatedAt = 0
		}
	}
	created := binding.ID == ""
	expiresAt := time.Now().Add(s.codexSessionMappingRetention(persistCtx)).Unix()
	instructionSnapshot := mapping.instructions.snapshotCommit(binding.TreeID, expiresAt)
	aliases := mapping.identity.aliasesForBinding(*binding, responseID, turnState)
	aliases = append(aliases, mapping.recoveryAliases...)
	droppedHierarchyAliases := 0
	committed, err := s.persistCodexStateCommit(persistCtx, storage.CodexSessionCommit{
		Namespace:                       mapping.namespace,
		Binding:                         *binding,
		Aliases:                         aliases,
		ExpiresAt:                       expiresAt,
		InstructionSnapshot:             instructionSnapshot,
		DropConflictingHierarchyAliases: true,
		DroppedHierarchyAliases:         &droppedHierarchyAliases,
	}, compact)
	if err != nil {
		return err
	}
	mapping.binding = &committed
	mapping.prospective = nil
	mapping.requiredAccount, mapping.requiredEgress = committed.AccountID, committed.EgressID
	if created {
		atomic.AddUint64(&s.codexMappingBindingsCreated, 1)
		s.enqueueAudit(storage.AuditLogRow{
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
	if ctx == nil {
		ctx = context.Background()
	}
	if accountID := strings.TrimSpace(lease.Account.ID); accountID != "" {
		binding.AccountID = accountID
	}
	if egressID := strings.TrimSpace(egress.ID); egressID != "" {
		binding.EgressID = egressID
	}
	expiresAt := binding.ExpiresAt
	if expiresAt <= time.Now().Unix() {
		expiresAt = time.Now().Add(s.codexSessionMappingRetention(context.WithoutCancel(ctx))).Unix()
	}
	s.enqueueCodexUpstreamAttempt(storage.CodexUpstreamAttempt{
		EventID:    "ATT-" + strings.TrimPrefix(newRequestID(), "REQ-"),
		TreeID:     binding.TreeID,
		AccountID:  binding.AccountID,
		EgressID:   binding.EgressID,
		Epoch:      binding.Epoch,
		State:      state,
		StatusCode: statusCode,
		ExpiresAt:  expiresAt,
	})
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
	// A completed main-CLI epoch owns an encrypted replay checkpoint. If its exact
	// account is already quota-cooled/recheck-pending, return control to the CPA
	// recovery layer immediately; waiting for the account circuit breaker would
	// strand unrelated mapped sessions that have not yet observed the same outage.
	// Child/fork and prospective roots remain strict and never rotate this way.
	route.FailFastBoundRecovery = mapping.durableMainCLI()
	return route
}
