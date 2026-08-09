package api

// v2 goal continuity is intentionally request-protocol aware but transport neutral.
// The gateway never sees a literal `/goal resume`; it sees the durable identifiers the
// clients send on the following request.  This layer translates only those exact
// identifiers into an encrypted checkpoint replay and never guesses from a model,
// cache prefix, account affinity, or API-key alone.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/tidwall/gjson"
)

type goalResumeKind string

const (
	goalResumeFound              goalResumeKind = "recovered"
	goalResumeUnidentified       goalResumeKind = "unidentified"
	goalResumeAmbiguous          goalResumeKind = "ambiguous"
	goalResumeRequiresToolResult goalResumeKind = "requires_tool_result"
	goalResumeStorageExhausted   goalResumeKind = "storage_exhausted"
	goalResumeInProgress         goalResumeKind = "in_progress"
	// goalResumeProtocolMismatch is returned when the identifiers resolve to durable
	// history of the other wire family and the request cannot stand on its own body.
	goalResumeProtocolMismatch goalResumeKind = "protocol_family_mismatch"
	// goalResumeFamilyRestart is the same collision when the request does carry its
	// own complete history: the turn proceeds as a new goal in the requested family.
	goalResumeFamilyRestart goalResumeKind = "family_restart"
)

type goalResumeResult struct {
	Kind    goalResumeKind
	Body    []byte
	Session storage.GoalSession
	Reason  string
}

// goalIdentityAliasesKey keeps the exact downstream identifiers from the body as it
// arrived.  A successful resume is deliberately rewritten into a self-contained
// replay before it reaches the upstream; retaining these request-scoped aliases in
// memory lets the subsequent terminal atomically advance the same durable goal rather
// than accidentally opening a sibling session with the replay body.
type goalIdentityAliasesKey struct{}
type goalOriginalBodyKey struct{}

func withGoalIdentityAliases(ctx context.Context, aliases []storage.GoalAlias) context.Context {
	copyAliases := append([]storage.GoalAlias(nil), aliases...)
	return context.WithValue(ctx, goalIdentityAliasesKey{}, copyAliases)
}

func goalIdentityAliases(ctx context.Context) []storage.GoalAlias {
	aliases, _ := ctx.Value(goalIdentityAliasesKey{}).([]storage.GoalAlias)
	return append([]storage.GoalAlias(nil), aliases...)
}

func withGoalOriginalBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, goalOriginalBodyKey{}, body)
}

func goalOriginalBody(ctx context.Context, fallback []byte) []byte {
	if body, ok := ctx.Value(goalOriginalBodyKey{}).([]byte); ok && len(body) > 0 {
		return body
	}
	return fallback
}

func hashGoalFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (s *Server) goalContinuityEnabled(ctx context.Context) bool {
	return s.flagEnabled(ctx, "goal_continuity_enabled", s.cfg.GoalContinuityEnabled)
}

func (s *Server) goalRetention(ctx context.Context) time.Duration {
	days := s.settingInt(ctx, "goal_retention_days", s.cfg.GoalRetentionDays)
	if days <= 0 {
		days = 7
	}
	return time.Duration(days) * 24 * time.Hour
}

func (s *Server) goalStorageMaxBytes(ctx context.Context) int64 {
	_, explicitRuntime := s.runtimeSetting(ctx, "goal_storage_max_mb")
	mb := s.settingInt(ctx, "goal_storage_max_mb", s.cfg.GoalStorageMaxMB)
	if mb <= 0 {
		mb = config.DefaultGoalStorageMaxMB
	}
	// The disk-guard startup migration persists this override and audit marker.
	// Keep admission race-free during the few milliseconds before that worker's
	// first pass: inherited legacy bootstrap defaults get the new floor, while an
	// explicit runtime value (including 256) remains authoritative.
	if !explicitRuntime && mb == config.LegacyDefaultGoalStorageMaxMB {
		mb = config.DefaultGoalStorageMaxMB
	}
	return int64(mb) << 20
}

func (s *Server) goalCompressionStages(ctx context.Context) int {
	stages := s.settingInt(ctx, "goal_compression_max_stages", s.cfg.GoalCompressionMaxStages)
	if stages <= 0 {
		return 16
	}
	return stages
}

func (s *Server) goalCompressionChunkRatio(ctx context.Context) float64 {
	ratio := s.settingFloat(ctx, "goal_compression_chunk_ratio", s.cfg.GoalCompressionChunkRatio)
	if ratio <= 0 || ratio > 1 {
		return 0.70
	}
	return ratio
}

func (s *Server) goalCompressionConcurrency(ctx context.Context) int {
	limit := s.settingInt(ctx, "goal_compression_concurrency", s.cfg.GoalCompressionConcurrency)
	if limit <= 0 {
		return 1
	}
	return limit
}

const (
	goalCompactionForegroundBudget = 540 * time.Second
	goalCompactionQueueDepth       = 4096
	goalPersistenceReclaimSteps    = 16
	goalPersistenceReclaimTimeout  = 5 * time.Second
	goalPersistenceAuditWindow     = 30 * time.Second
)

func (s *Server) startGoalCompactionWorkers() {
	if s == nil || s.store == nil {
		return
	}
	workers := s.goalCompressionConcurrency(context.Background())
	if workers > 32 {
		workers = 32
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	s.goalCompactionMu.Lock()
	s.goalCompactionCtx = workerCtx
	s.goalCompactionCancel = cancel
	s.goalCompactionQueue = make(chan string, goalCompactionQueueDepth)
	s.goalCompactionQueued = make(map[string]bool)
	queue := s.goalCompactionQueue
	s.goalCompactionMu.Unlock()
	for i := 0; i < workers; i++ {
		s.asyncWG.Add(1)
		supervisor.GoOnce("goal-compaction-worker", func() {
			defer s.asyncWG.Done()
			s.goalCompactionWorker(workerCtx, queue)
		})
	}
}

func (s *Server) stopGoalCompactionWorkers() {
	s.goalCompactionMu.Lock()
	cancel := s.goalCompactionCancel
	s.goalCompactionCancel = nil
	s.goalCompactionCtx = nil
	s.goalCompactionQueued = nil
	s.goalCompactionMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.goalCompactionTimers.Wait()
}

// scheduleGoalCompaction enqueues one deduplicated goal. Each dequeue compacts at
// most one chunk and requeues unfinished work at the FIFO tail, so a large history
// cannot monopolize the worker while other goals are waiting.
func (s *Server) scheduleGoalCompaction(ctx context.Context, goalID string) {
	goalID = strings.TrimSpace(goalID)
	if goalID == "" || s.store == nil {
		return
	}
	stages := s.goalCompressionStages(ctx)
	if stages <= 0 {
		return
	}
	session, sessionErr := s.store.GetGoalSession(ctx, goalID)
	if sessionErr != nil || session.State == "awaiting_tool_result" {
		return
	}
	needed, err := s.store.NeedsGoalCompaction(ctx, goalID, stages)
	if err != nil || !needed {
		return
	}
	s.goalCompactionMu.Lock()
	workerCtx, queue := s.goalCompactionCtx, s.goalCompactionQueue
	if workerCtx == nil || workerCtx.Err() != nil || s.goalCompactionQueued[goalID] {
		s.goalCompactionMu.Unlock()
		return
	}
	s.goalCompactionQueued[goalID] = true
	s.goalCompactionMu.Unlock()
	select {
	case queue <- goalID:
	case <-workerCtx.Done():
		s.finishGoalCompactionQueueItem(goalID)
	default:
		s.finishGoalCompactionQueueItem(goalID)
		_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_pending", State: "retryable", Reason: "queue_full", Detail: "goal=" + goalID})
	}
}

func (s *Server) finishGoalCompactionQueueItem(goalID string) {
	s.goalCompactionMu.Lock()
	if s.goalCompactionQueued != nil {
		delete(s.goalCompactionQueued, goalID)
	}
	s.goalCompactionMu.Unlock()
}

func (s *Server) goalCompactionWorker(ctx context.Context, queue <-chan string) {
	for {
		select {
		case <-ctx.Done():
			return
		case goalID := <-queue:
			requeue, delay := s.compactOneGoalChunk(ctx, goalID)
			s.finishGoalCompactionQueueItem(goalID)
			if requeue && ctx.Err() == nil {
				s.requeueGoalCompaction(ctx, goalID, delay)
			}
		}
	}
}

func (s *Server) requeueGoalCompaction(ctx context.Context, goalID string, delay time.Duration) {
	s.goalCompactionMu.Lock()
	if s.goalCompactionCtx != ctx || ctx.Err() != nil {
		s.goalCompactionMu.Unlock()
		return
	}
	s.goalCompactionTimers.Add(1)
	s.goalCompactionMu.Unlock()
	supervisor.GoOnce("goal-compaction-requeue", func() {
		defer s.goalCompactionTimers.Done()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
		s.scheduleGoalCompaction(ctx, goalID)
	})
}

func (s *Server) compactOneGoalChunk(parent context.Context, goalID string) (bool, time.Duration) {
	jobCtx, cancel := context.WithTimeout(parent, goalCompactionForegroundBudget)
	defer cancel()
	finish, err := s.beginGoalRunWithResult(jobCtx, goalID, "compacting")
	if err != nil {
		if errors.Is(err, storage.ErrGoalInProgress) {
			return true, 25 * time.Millisecond
		}
		_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "lease", Detail: "goal=" + goalID})
		return true, time.Second
	}
	finishState, finishCode := "completed", ""
	defer func() { finish(finishState, finishCode) }()
	if err = s.store.SetGoalCompactionState(jobCtx, goalID, "compacting"); err != nil {
		finishState, finishCode = "retryable", "goal_compaction_failed"
		return true, time.Second
	}
	stages := s.goalCompressionStages(jobCtx)
	if err = s.store.CompactGoalSegmentsWithRatio(jobCtx, goalID, stages, s.goalCompressionChunkRatio(jobCtx)); err != nil {
		finishState, finishCode = "retryable", "goal_compaction_failed"
		_ = s.store.SetGoalCompactionState(context.Background(), goalID, "retryable")
		_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "checkpoint", Detail: "goal=" + goalID})
		return true, time.Second
	}
	_ = s.store.SetGoalCompactionState(context.Background(), goalID, "ready")
	needed, err := s.store.NeedsGoalCompaction(jobCtx, goalID, stages)
	if err != nil {
		return true, time.Second
	}
	return needed, 0
}

func (s *Server) goalLeaseDuration(ctx context.Context) time.Duration {
	seconds := s.settingInt(ctx, "goal_lease_seconds", s.cfg.GoalLeaseSeconds)
	if seconds <= 0 {
		seconds = 90
	}
	return time.Duration(seconds) * time.Second
}

func (s *Server) goalHeartbeatDuration(ctx context.Context) time.Duration {
	seconds := s.settingInt(ctx, "goal_heartbeat_seconds", s.cfg.GoalHeartbeatSeconds)
	if seconds <= 0 {
		seconds = 15
	}
	lease := s.goalLeaseDuration(ctx)
	interval := time.Duration(seconds) * time.Second
	if interval >= lease {
		interval = lease / 2
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return interval
}

// beginGoalRun acquires the durable resume lease and maintains its heartbeat for the
// lifetime of a foreground relay.  The returned closer is idempotent from the caller's
// perspective and lets FinishGoalRun preserve retryable state when the stream was
// interrupted after its last checkpoint.
func (s *Server) beginGoalRun(ctx context.Context, goalID, phase string) (func(), error) {
	finish, err := s.beginGoalRunWithResult(ctx, goalID, phase)
	if err != nil {
		return nil, err
	}
	return func() { finish("completed", "") }, nil
}

// beginGoalRunWithResult is the lifecycle-aware variant used by compaction jobs.
// It lets a bounded background stage retain goal_compaction_pending rather than being
// overwritten as completed by a generic deferred cleanup.
func (s *Server) beginGoalRunWithResult(ctx context.Context, goalID, phase string) (func(string, string), error) {
	owner := "goal:" + requestIDFromContext(ctx)
	if owner == "goal:" {
		owner = fmt.Sprintf("goal:%d", time.Now().UnixNano())
	}
	lease := s.goalLeaseDuration(ctx)
	run, err := s.store.AcquireGoalRun(ctx, goalID, owner, phase, lease)
	if err != nil {
		return nil, err
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := s.goalHeartbeatDuration(ctx)
	go func() {
		defer supervisor.Recover("goal-heartbeat")
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := s.store.HeartbeatGoalRun(context.Background(), run.ID, owner, lease); err != nil {
					log.Printf("[GOAL-HEARTBEAT] run=%s: %v", run.ID, err)
					return
				}
			}
		}
	}()
	return func(state, failureCode string) {
		close(stop)
		<-done
		if state == "" {
			state = "completed"
		}
		if err := s.store.FinishGoalRun(context.Background(), run.ID, owner, state, failureCode); err != nil {
			log.Printf("[GOAL-RUN] finish run=%s: %v", run.ID, err)
		}
	}, nil
}

// goalAliases implements the documented identity order.  Values live only for the
// duration of this request and are converted to hashes inside storage.
func goalAliases(r *http.Request, body []byte, protocol string) []storage.GoalAlias {
	return goalAliasesWithMeta(r, body, protocol, nil)
}

func goalAliasesWithMeta(r *http.Request, body []byte, protocol string, meta *bodysource.BodyMeta) []storage.GoalAlias {
	aliases := make([]storage.GoalAlias, 0, 10)
	add := func(kind, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			aliases = append(aliases, storage.GoalAlias{Type: kind, Value: value})
		}
	}
	if r != nil {
		// Parent identifies the root goal.  The concrete thread is also kept as a
		// branch alias so concurrent subagents cannot merge their raw histories.
		parent := r.Header.Get("x-codex-parent-thread-id")
		thread := r.Header.Get("thread-id")
		if parent != "" {
			add("codex_root_thread", parent)
			if thread != "" && thread != parent {
				add("codex_branch_thread", thread)
			}
		} else {
			add("codex_root_thread", thread)
		}
		add("claude_code_session", r.Header.Get("x-claude-code-session-id"))
		add("codex_turn_state", r.Header.Get("x-codex-turn-state"))
	}
	field := func(key string) string {
		if meta == nil || meta.Size != int64(len(body)) {
			return jsonStringField(body, key)
		}
		switch key {
		case "previous_response_id":
			return strings.TrimSpace(meta.PreviousResponseID)
		case "conversation_id":
			return strings.TrimSpace(meta.ConversationID)
		case "session_id":
			return strings.TrimSpace(meta.SessionID)
		case "thread_id":
			return strings.TrimSpace(meta.ThreadID)
		}
		var value string
		if raw := meta.Scalars[key]; len(raw) > 0 && json.Unmarshal(raw, &value) == nil {
			return strings.TrimSpace(value)
		}
		return ""
	}
	add("response_id", field("previous_response_id"))
	// Some Codex transports put the same state token in a JSON field.  Hashing it
	// under the same kind preserves compatibility without writing the opaque token.
	add("codex_turn_state", field("turn_state"))
	// Every Messages-family provider carries the same Claude Code session field, so
	// kiro/antigravity/custom turns must contribute the same alias as native claude
	// or a mid-conversation provider switch would lose the strongest identifier.
	if storage.GoalProtocolFamily(protocol) == storage.GoalFamilyMessages {
		add("claude_session", field("session_id"))
	}
	// These are real client conversation identifiers, not cache-derived correlators.
	add("conversation_id", field("conversation_id"))
	add("thread_id", field("thread_id"))
	add("session_id", field("session_id"))
	return aliases
}

// goalAliasPrioritySets returns identity candidates in the exact recovery order.
// Do not combine different kinds into one storage lookup: a process-wide session_id
// occasionally appears in several independent CLI windows, and a union lookup would
// turn that weak collision into an incorrect shared goal or goal_in_progress lease.
func goalAliasPrioritySets(aliases []storage.GoalAlias) [][]storage.GoalAlias {
	order := []string{
		"codex_branch_thread",
		"codex_root_thread",
		"claude_code_session",
		"claude_session",
		"response_id",
		"codex_turn_state",
		"conversation_id",
		"thread_id",
		"session_id",
	}
	sets := make([][]storage.GoalAlias, 0, len(order))
	for _, kind := range order {
		set := make([]storage.GoalAlias, 0, 1)
		for _, alias := range aliases {
			if alias.Type == kind && strings.TrimSpace(alias.Value) != "" {
				set = append(set, alias)
			}
		}
		if len(set) > 0 {
			sets = append(sets, set)
		}
	}
	return sets
}

func goalHasResumeAlias(aliases []storage.GoalAlias) bool {
	for _, alias := range aliases {
		switch alias.Type {
		case "response_id", "codex_turn_state":
			if strings.TrimSpace(alias.Value) != "" {
				return true
			}
		}
	}
	return false
}

// goalResolutionAliasSets protects a new concrete CLI thread from accidentally
// falling through to a weaker, shared process/session marker. Stateful resumes may
// fall through to their persisted response/turn aliases when the client changed its
// thread header. A child branch is always authoritative over its parent root.
func goalResolutionAliasSets(aliases []storage.GoalAlias, allowResumeFallback bool) [][]storage.GoalAlias {
	sets := goalAliasPrioritySets(aliases)
	if len(sets) == 0 {
		return nil
	}
	if sets[0][0].Type == "codex_branch_thread" {
		if !allowResumeFallback {
			return sets[:1]
		}
		out := append([][]storage.GoalAlias(nil), sets[:1]...)
		for _, set := range sets[1:] {
			if set[0].Type == "response_id" || set[0].Type == "codex_turn_state" {
				out = append(out, set)
			}
		}
		return out
	}
	if !allowResumeFallback {
		return sets[:1]
	}
	return sets
}

func (s *Server) resolveGoalAliasesByPriority(ctx context.Context, aliases []storage.GoalAlias, allowResumeFallback bool) (storage.GoalResolution, error) {
	return s.store.ResolveGoalAliasSets(ctx, goalResolutionAliasSets(aliases, allowResumeFallback))
}

// goalPersistentAliases retains the concrete identity plus stateful aliases but not
// weaker process-wide markers once a thread/session already identifies the goal.
// That makes alias persistence monotonic and prevents an old shared session_id from
// steering another CLI conversation into this goal on a later resume.
func goalPersistentAliases(aliases []storage.GoalAlias) []storage.GoalAlias {
	sets := goalAliasPrioritySets(aliases)
	if len(sets) == 0 {
		return nil
	}
	out := append([]storage.GoalAlias(nil), sets[0]...)
	seen := make(map[string]bool, len(out))
	for _, alias := range out {
		seen[alias.Namespace+"\x00"+alias.Type+"\x00"+alias.Value] = true
	}
	for _, alias := range aliases {
		if alias.Type != "response_id" && alias.Type != "codex_turn_state" {
			continue
		}
		key := alias.Namespace + "\x00" + alias.Type + "\x00" + alias.Value
		if strings.TrimSpace(alias.Value) != "" && !seen[key] {
			seen[key] = true
			out = append(out, alias)
		}
	}
	return out
}

func jsonStringField(raw []byte, key string) string {
	value := gjson.GetBytes(raw, key)
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}

func nestedJSONStringField(raw []byte, parent, key string) string {
	value := gjson.GetBytes(raw, parent+"."+key)
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}

func goalWorkspaceFingerprint(r *http.Request, body []byte) string {
	if r != nil {
		for _, key := range []string{"x-codex-workspace-id", "chatgpt-account-id", "openai-organization", "anthropic-organization-id"} {
			if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
				return hashGoalFingerprint(value)
			}
		}
	}
	for _, key := range []string{"workspace_id", "chatgpt_account_id", "organization_id"} {
		if value := jsonStringField(body, key); value != "" {
			return hashGoalFingerprint(value)
		}
	}
	if value := nestedJSONStringField(body, "client_metadata", "workspace_id"); value != "" {
		return hashGoalFingerprint(value)
	}
	return ""
}

func firstGoalText(value interface{}) string {
	switch item := value.(type) {
	case string:
		return strings.TrimSpace(item)
	case []interface{}:
		for _, child := range item {
			if text := firstGoalText(child); text != "" {
				return text
			}
		}
	case map[string]interface{}:
		if role, _ := item["role"].(string); role == "user" {
			if text := firstGoalText(item["content"]); text != "" {
				return text
			}
		}
		for _, key := range []string{"text", "input_text", "content"} {
			if text := firstGoalText(item[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func goalInitialFingerprint(body []byte) string {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if text := firstGoalText(root["input"]); text != "" {
		return hashGoalFingerprint(text)
	}
	if text := firstGoalText(root["messages"]); text != "" {
		return hashGoalFingerprint(text)
	}
	return ""
}

func bodyHasClientToolResult(body []byte) bool {
	for _, marker := range [][]byte{
		[]byte(`"function_call_output"`), []byte(`"local_shell_call_output"`), []byte(`"mcp_tool_call_output"`),
		[]byte(`"custom_tool_call_output"`), []byte(`"tool_search_output"`), []byte(`"tool_result"`), []byte(`"tool_use_result"`),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

func outputAwaitingTool(response map[string]interface{}) bool {
	// Inspect ordinary Responses history with exact call/result pairing first. The
	// generic walk below remains for protocol-specific nested tool-use blocks.
	if hasPendingClientToolCall(response["output"]) || hasPendingClientToolCall(response["content"]) {
		return true
	}
	var walk func(interface{}) bool
	walk = func(value interface{}) bool {
		switch item := value.(type) {
		case map[string]interface{}:
			typ, _ := item["type"].(string)
			switch strings.ToLower(typ) {
			case "tool_use", "mcp_tool_call":
				return true
			}
			for _, child := range item {
				if walk(child) {
					return true
				}
			}
		case []interface{}:
			for _, child := range item {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(response["output"]) || walk(response["content"])
}

func goalWorkingState(body []byte) string {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	state := map[string]interface{}{
		"initial_goal_fingerprint": goalInitialFingerprint(body),
		"latest_goal_fingerprint":  hashGoalFingerprint(firstGoalText(root["input"])),
	}
	encoded, _ := json.Marshal(state)
	return string(encoded)
}

type goalSegmentSemantics struct {
	ReplacementHistory       []interface{}
	ReplacementPrefix        []interface{}
	ReplaceInput             bool
	CodexCompactionEvaluated bool
}

func claudeCodeCompactionReplacement(requestBody, responseBody []byte) ([]interface{}, bool) {
	if !kirowire.IsClaudeCodeCompactionRequest(requestBody) {
		return nil, false
	}
	request, requestErr := decodeContextJSONMap(requestBody)
	response, responseErr := decodeContextJSONMap(responseBody)
	if requestErr != nil || responseErr != nil {
		return nil, false
	}
	messages, ok := request["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil, false
	}
	var finalUser interface{}
	for index := len(messages) - 1; index >= 0; index-- {
		message, ok := messages[index].(map[string]interface{})
		if ok && strings.EqualFold(strings.TrimSpace(streamString(message["role"])), "user") {
			finalUser = messages[index]
			break
		}
	}
	if finalUser == nil {
		return nil, false
	}
	candidates := []interface{}{response}
	if output, ok := response["output"].([]interface{}); ok {
		candidates = output
	}
	assistants := make([]interface{}, 0, 1)
	for _, raw := range candidates {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if assistant, ok := item["assistant_message"].(map[string]interface{}); ok && assistant["content"] != nil {
			assistants = append(assistants, assistant)
			continue
		}
		if item["content"] != nil &&
			(strings.EqualFold(strings.TrimSpace(streamString(item["role"])), "assistant") ||
				strings.EqualFold(strings.TrimSpace(streamString(item["type"])), "message")) {
			assistants = append(assistants, map[string]interface{}{"role": "assistant", "content": item["content"]})
		}
	}
	if len(assistants) == 0 {
		return nil, false
	}
	replacement := []interface{}{finalUser}
	replacement = append(replacement, assistants...)
	return replacement, true
}

func goalCheckpointAndSegment(requestBody, segmentRequestBody, responseBody []byte, semantics ...goalSegmentSemantics) (string, string, string, bool, error) {
	request, err := decodeContextJSONMap(requestBody)
	if err != nil {
		return "", "", "", false, err
	}
	response, err := decodeContextJSONMap(responseBody)
	if err != nil {
		return "", "", "", false, err
	}
	segmentRequest := request
	if len(segmentRequestBody) > 0 {
		if decoded, decodeErr := decodeContextJSONMap(segmentRequestBody); decodeErr == nil {
			segmentRequest = decoded
		}
	}
	responseID, _ := response["id"].(string)
	base := make(map[string]interface{}, len(request))
	historyKey := "input"
	if _, hasMessages := request["messages"]; hasMessages {
		historyKey = "messages"
	}
	for key, value := range request {
		if key == "input" || key == "messages" || key == "previous_response_id" || key == "turn_state" {
			continue
		}
		base[key] = value
	}
	// Preserve the native protocol's history field.  Previous code always wrote
	// `input`, which made an otherwise valid Claude checkpoint unreplayable because
	// /v1/messages requires `messages` and must retain its system/tool envelope.
	base[historyKey] = []interface{}{}
	history := segmentRequest[historyKey]
	output := response["output"]
	if output == nil {
		// Claude's response content is preserved as a full opaque object in the
		// segment.  We do not textify attachments or unknown blocks.
		output = []interface{}{response}
	}
	checkpoint, err := json.Marshal(base)
	if err != nil {
		return "", "", "", false, err
	}
	segmentFields := map[string]interface{}{"history_key": historyKey, "input": history, "output": output}
	if len(semantics) > 0 {
		if len(semantics[0].ReplacementHistory) > 0 {
			segmentFields["replacement_history"] = semantics[0].ReplacementHistory
		}
		if len(semantics[0].ReplacementPrefix) > 0 {
			segmentFields["replacement_prefix"] = semantics[0].ReplacementPrefix
		}
		if semantics[0].ReplaceInput {
			segmentFields["replace_input"] = true
		}
		if semantics[0].CodexCompactionEvaluated {
			// New Codex segments record that the response terminal was evaluated by
			// the strict RemoteCompactionV2 gate. Storage may use its trigger-only
			// compatibility inference solely for older segments that lack this bit.
			segmentFields["codex_compaction_evaluated"] = true
		}
	}
	segment, err := json.Marshal(segmentFields)
	if err != nil {
		return "", "", "", false, err
	}
	return string(checkpoint), string(segment), strings.TrimSpace(responseID), outputAwaitingTool(response), nil
}

// incrementalGoalRequest removes only a byte-for-byte-equivalent decoded history
// prefix that is already durable. Claude Code and Kiro resend the complete messages
// array on every turn; persisting that full array as every segment creates O(n^2)
// storage. Any mismatch keeps the original body, preferring duplication over loss.
func incrementalGoalRequest(currentBody, durableReplay []byte) []byte {
	trimmed, _ := incrementalGoalRequestWithMode(currentBody, durableReplay)
	return trimmed
}

// incrementalGoalRequestWithMode distinguishes an ordinary delta from the full
// replacement snapshot Codex emits after remote compaction. The latter must replace
// the durable input instead of being appended to it.
func incrementalGoalRequestWithMode(currentBody, durableReplay []byte) ([]byte, bool) {
	current, currentErr := decodeContextJSONMap(currentBody)
	durable, durableErr := decodeContextJSONMap(durableReplay)
	if currentErr != nil || durableErr != nil {
		return currentBody, false
	}
	historyKey := "input"
	if _, ok := current["messages"]; ok {
		historyKey = "messages"
	}
	currentHistory, currentOK := current[historyKey].([]interface{})
	durableHistory, durableOK := durable[historyKey].([]interface{})
	if !currentOK || !durableOK || len(durableHistory) == 0 || len(durableHistory) > len(currentHistory) {
		if historyKey == "messages" {
			return currentBody, claudeFullHistoryReplacesDurable(currentHistory)
		}
		return currentBody, goalFullInputReplacesDurable(currentHistory, durableHistory)
	}
	for index := range durableHistory {
		if !reflect.DeepEqual(durableHistory[index], currentHistory[index]) {
			if historyKey == "messages" {
				return currentBody, claudeFullHistoryReplacesDurable(currentHistory)
			}
			return currentBody, goalFullInputReplacesDurable(currentHistory, durableHistory)
		}
	}
	current[historyKey] = append([]interface{}(nil), currentHistory[len(durableHistory):]...)
	encoded, err := json.Marshal(current)
	if err != nil {
		return currentBody, false
	}
	return encoded, false
}

// A native Messages delta contains only the new user/tool-result side. A
// self-contained snapshot contains at least one user and one assistant turn; when
// such a snapshot no longer has the durable prefix, Claude Code has compacted,
// rewound, or edited history and the snapshot must be authoritative.
func claudeFullHistoryReplacesDurable(items []interface{}) bool {
	var user, assistant bool
	for _, raw := range items {
		message, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(streamString(message["role"]))) {
		case "user":
			user = true
		case "assistant":
			assistant = true
		}
	}
	return user && assistant
}

func goalFullInputReplacesDurable(current, durable []interface{}) bool {
	if goalInputSnapshotReplacesDurable(current, durable) {
		return true
	}
	currentPrefix, currentHistory := splitLeadingCodexLiteReplayPrefix(current)
	_, durableHistory := removeCodexLiteReplayPrefixes(durable)
	return len(currentPrefix) > 0 && goalHistoryPrefixEqual(durableHistory, currentHistory)
}

func goalInputSnapshotReplacesDurable(current, durable []interface{}) bool {
	signature, ok := latestGoalCompactionSignature(durable)
	if !ok {
		return false
	}
	for _, raw := range current {
		if currentSignature, currentOK := goalCompactionSignature(raw); currentOK && currentSignature == signature {
			return true
		}
	}
	return false
}

func latestGoalCompactionSignature(items []interface{}) (string, bool) {
	for index := len(items) - 1; index >= 0; index-- {
		if signature, ok := goalCompactionSignature(items[index]); ok {
			return signature, true
		}
	}
	return "", false
}

func goalCompactionSignature(raw interface{}) (string, bool) {
	item, ok := raw.(map[string]interface{})
	if !ok {
		return "", false
	}
	kind := strings.ToLower(strings.TrimSpace(streamString(item["type"])))
	if kind != "compaction" && kind != "compaction_summary" {
		return "", false
	}
	encrypted, ok := item["encrypted_content"].(string)
	if !ok {
		return "", false
	}
	return "compaction\x00" + encrypted, true
}

func mergeCodexGoalReplayInput(durableRaw, currentRaw interface{}) []interface{} {
	durablePrefix, durableHistory := removeCodexLiteReplayPrefixes(appendItems(nil, durableRaw))
	currentPrefix, currentHistory := splitLeadingCodexLiteReplayPrefix(appendItems(nil, currentRaw))
	prefix := currentPrefix
	if len(prefix) == 0 {
		prefix = durablePrefix
	}

	mergedHistory := append([]interface{}(nil), durableHistory...)
	switch {
	case goalInputSnapshotReplacesDurable(currentHistory, durableHistory):
		// A post-compaction full request already contains the official replacement
		// history. Appending it would duplicate the retained messages and opaque
		// compaction item.
		mergedHistory = append([]interface{}(nil), currentHistory...)
	case goalHistoryPrefixEqual(durableHistory, currentHistory):
		mergedHistory = append(mergedHistory, currentHistory[len(durableHistory):]...)
	default:
		mergedHistory = append(mergedHistory, currentHistory...)
	}
	return append(append([]interface{}(nil), prefix...), mergedHistory...)
}

func goalHistoryPrefixEqual(prefix, items []interface{}) bool {
	if len(prefix) == 0 || len(prefix) > len(items) {
		return false
	}
	for index := range prefix {
		if !reflect.DeepEqual(prefix[index], items[index]) {
			return false
		}
	}
	return true
}

func splitLeadingCodexLiteReplayPrefix(items []interface{}) ([]interface{}, []interface{}) {
	if len(items) == 0 || !codexGoalLiteAdditionalTools(items[0]) {
		return nil, items
	}
	end := 1
	for end < len(items) && codexGoalDeveloperMessage(items[end]) {
		end++
	}
	return append([]interface{}(nil), items[:end]...), append([]interface{}(nil), items[end:]...)
}

func removeCodexLiteReplayPrefixes(items []interface{}) ([]interface{}, []interface{}) {
	var latestPrefix []interface{}
	history := make([]interface{}, 0, len(items))
	for index := 0; index < len(items); {
		if !codexGoalLiteAdditionalTools(items[index]) {
			history = append(history, items[index])
			index++
			continue
		}
		end := index + 1
		for end < len(items) && codexGoalDeveloperMessage(items[end]) {
			end++
		}
		latestPrefix = append([]interface{}(nil), items[index:end]...)
		index = end
	}
	return latestPrefix, history
}

func codexGoalLiteAdditionalTools(raw interface{}) bool {
	item, ok := raw.(map[string]interface{})
	if !ok || streamString(item["type"]) != "additional_tools" || streamString(item["role"]) != "developer" {
		return false
	}
	_, ok = item["tools"].([]interface{})
	return ok
}

func codexGoalDeveloperMessage(raw interface{}) bool {
	item, ok := raw.(map[string]interface{})
	return ok && streamString(item["type"]) == "message" && streamString(item["role"]) == "developer"
}

func codexRemoteCompactionReplacement(durableReplay, incrementalBody, responseBody []byte, replaceInput bool) ([]interface{}, bool) {
	request, err := decodeContextJSONMap(incrementalBody)
	if err != nil {
		return nil, false
	}
	response, err := decodeContextJSONMap(responseBody)
	if err != nil {
		return nil, false
	}
	// RemoteCompactionV2 installs a replacement only after the official client has
	// observed response.completed. Treating a status-less or partial response as a
	// boundary could erase the only durable copy of the pre-compaction history.
	if !strings.EqualFold(strings.TrimSpace(streamString(response["status"])), "completed") {
		return nil, false
	}
	logicalInput := appendItems(nil, request["input"])
	previousResponseID := strings.TrimSpace(streamString(request["previous_response_id"]))
	if previousResponseID != "" && !replaceInput && len(durableReplay) > 0 {
		if durable, decodeErr := decodeContextJSONMap(durableReplay); decodeErr == nil {
			durableInput := appendItems(nil, durable["input"])
			// A transport fallback can carry previous_response_id together with a
			// self-contained full input. Only WebSocket deltas need the durable prefix;
			// prepending it to an already complete logical request duplicates every
			// retained message at the compaction boundary.
			if !goalHistoryPrefixEqual(durableInput, logicalInput) {
				logicalInput = append(durableInput, logicalInput...)
			}
		}
	}
	return storage.CodexRemoteCompactionV2Replacement(logicalInput, response["output"])
}

func codexCompactionReplacementPrefix(durableReplay, logicalBody []byte) []interface{} {
	for _, body := range [][]byte{logicalBody, durableReplay} {
		root, err := decodeContextJSONMap(body)
		if err != nil {
			continue
		}
		prefix, _ := removeCodexLiteReplayPrefixes(appendItems(nil, root["input"]))
		if len(prefix) > 0 {
			return prefix
		}
	}
	return nil
}

// goalResponseFromSSE retains a protocol stream as encrypted structured segment data
// without pretending its provider-specific events are Responses output.
func goalResponseFromSSE(raw []byte) []byte {
	return goalResponseFromSSEParts(raw)
}

// goalResponseFromSSEParts parses independently captured initial and continuation
// streams without concatenating their potentially large raw byte ranges in memory.
func goalResponseFromSSEParts(parts ...[]byte) []byte {
	frames := make([]interface{}, 0)
	messageID := ""
	for _, raw := range parts {
		for _, frame := range bytes.Split(raw, []byte("\n\n")) {
			var dataLines [][]byte
			for _, line := range bytes.Split(frame, []byte("\n")) {
				line = bytes.TrimSpace(line)
				if bytes.HasPrefix(line, []byte("data:")) {
					dataLines = append(dataLines, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
				}
			}
			if len(dataLines) == 0 {
				continue
			}
			var event map[string]interface{}
			if json.Unmarshal(bytes.Join(dataLines, []byte("\n")), &event) != nil {
				continue
			}
			if message, ok := event["message"].(map[string]interface{}); ok {
				if id, _ := message["id"].(string); strings.TrimSpace(id) != "" {
					// A stitched continuation has a new upstream message id; bind the
					// durable alias to the final completed response, not the truncated one.
					messageID = id
				}
			}
			frames = append(frames, event)
		}
	}
	if len(frames) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(map[string]interface{}{"id": messageID, "output": []interface{}{map[string]interface{}{"type": "claude_sse", "events": frames, "assistant_message": claudeAssistantMessageFromEvents(frames)}}})
	return encoded
}

// claudeAssistantMessageFromEvents derives only the native message blocks needed to
// resume a completed Claude stream.  The complete original event list remains beside
// it in the encrypted segment, so unfamiliar blocks are never deleted or flattened.
func claudeAssistantMessageFromEvents(frames []interface{}) map[string]interface{} {
	message := map[string]interface{}{"role": "assistant", "content": []interface{}{}}
	blocks := make([]interface{}, 0)
	blockBase := 0
	for _, raw := range frames {
		event, _ := raw.(map[string]interface{})
		typ := streamString(event["type"])
		switch typ {
		case "message_start":
			// A stitched continuation starts a fresh upstream Claude message.  Keep
			// its provider-local block indexes distinct from the interrupted message
			// while retaining every original event verbatim in the segment.
			blockBase = len(blocks)
			if started, ok := event["message"].(map[string]interface{}); ok {
				if role := streamString(started["role"]); role != "" {
					message["role"] = role
				}
			}
		case "content_block_start":
			if block, ok := event["content_block"].(map[string]interface{}); ok {
				clone, err := decodeContextJSONMap(goalJSONBytes(block))
				if err == nil {
					blocks = append(blocks, clone)
				}
			}
		case "content_block_delta":
			index := blockBase + jsonIntValue(event["index"])
			if index < 0 || index >= len(blocks) {
				continue
			}
			block, _ := blocks[index].(map[string]interface{})
			delta, _ := event["delta"].(map[string]interface{})
			switch streamString(delta["type"]) {
			case "text_delta":
				block["text"] = streamString(block["text"]) + streamString(delta["text"])
			case "input_json_delta":
				// Claude tool input arrives incrementally.  Keep the final JSON as an
				// object when possible; otherwise preserve the raw partial string.
				rawInput := streamString(block["_goal_partial_json"]) + streamString(delta["partial_json"])
				block["_goal_partial_json"] = rawInput
			}
		}
	}
	for _, raw := range blocks {
		block, _ := raw.(map[string]interface{})
		if partial := streamString(block["_goal_partial_json"]); partial != "" {
			var value interface{}
			if json.Unmarshal([]byte(partial), &value) == nil {
				block["input"] = value
			} else {
				block["input"] = partial
			}
			delete(block, "_goal_partial_json")
		}
	}
	message["content"] = blocks
	return message
}

func goalJSONBytes(value interface{}) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

// mergedCodexContinuationResponse reconstructs the exact successful response seen by
// the downstream after a stitched continuation.  The first upstream response had no
// terminal and is therefore not independently durable; the checkpoint advances only
// once, using its partial output plus the continuation's real response.completed.
func mergedCodexContinuationResponse(first, continuation *codexStreamLedgerRecorder) []byte {
	if first == nil || continuation == nil || !continuation.completedSuccessfully() {
		return nil
	}
	final := continuation.ResponseJSON()
	if len(final) == 0 {
		return nil
	}
	response, err := decodeContextJSONMap(final)
	if err != nil {
		return nil
	}
	output := append([]interface{}{}, first.partialItems()...)
	continuationOutput := asSlice(response["output"])
	if len(continuationOutput) == 0 {
		continuationOutput = continuation.partialItems()
	}
	response["output"] = append(output, continuationOutput...)
	text, appendErr := appendBoundedStreamText(first.partialText(), firstNonEmpty(streamString(response["output_text"]), continuation.partialText()), defaultStreamAccumulatorMaxBytes)
	if appendErr != nil {
		return nil
	}
	if strings.TrimSpace(text) != "" {
		response["output_text"] = text
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil
	}
	return encoded
}

// markGoalStreamRetryable leaves an already durable checkpoint intact after a relay
// interruption.  It deliberately uses only the documented exact aliases (with the
// same child-branch precedence as replay), never account/model/cache heuristics.  A
// first-ever request may have no checkpoint yet; its visible failure terminal is still
// emitted, but there is nothing safe to resume in that case.
func (s *Server) markGoalStreamRetryable(ctx context.Context, r *http.Request, protocol string, requestBody []byte, reason string) {
	if !s.goalContinuityEnabled(ctx) {
		return
	}
	rawAliases := goalAliases(r, requestBody, protocol)
	rawAliases = append(rawAliases, goalIdentityAliases(ctx)...)
	keyHash, _ := downstreamFromCtx(ctx)
	namespace := goalDownstreamClientScope(ctx, keyHash, r)
	family := storage.GoalProtocolFamily(protocol)
	resolved, err := s.store.ResolveGoalAliasSetsForFamily(ctx,
		goalResolutionSetsForClientFamily(rawAliases, namespace, family, goalHasResumeAlias(rawAliases)), family)
	if errors.Is(err, storage.ErrGoalNotFound) {
		resolved, err = s.store.ResolveFallbackGoalForFamily(ctx, scopedGoalDownstreamKeyHash(keyHash, namespace), goalWorkspaceFingerprint(r, requestBody), goalInitialFingerprint(requestBody), family)
	}
	if err != nil {
		return
	}
	if err := s.store.MarkGoalRetryable(ctx, resolved.Session.ID, reason); err != nil {
		log.Printf("[GOAL-CONTINUITY] unable to mark retryable goal=%s: %v", resolved.Session.ID, err)
	}
}

func (s *Server) persistGoalContinuity(ctx context.Context, r *http.Request, protocol string, requestBody, responseBody []byte) (storage.GoalSession, error) {
	if !s.goalContinuityEnabled(ctx) {
		return storage.GoalSession{}, nil
	}
	rawRequestAliases := goalAliases(r, requestBody, protocol)
	rawRequestAliases = append(rawRequestAliases, goalIdentityAliases(ctx)...)
	keyHash, _ := downstreamFromCtx(ctx)
	namespace := goalDownstreamClientScope(ctx, keyHash, r)
	family := storage.GoalProtocolFamily(protocol)
	requestAliases := familyGoalAliases(namespacedGoalAliases(rawRequestAliases, namespace), family)
	resolutionSets := goalResolutionSetsForClientFamily(rawRequestAliases, namespace, family, goalHasResumeAlias(rawRequestAliases))
	scopedKeyHash := scopedGoalDownstreamKeyHash(keyHash, namespace)
	workspaceHash := goalWorkspaceFingerprint(r, requestBody)
	initialGoalHash := goalInitialFingerprint(requestBody)
	// A terminal without an exact identity or the complete fallback tuple can never
	// be resumed. Persisting it would create an unreachable goal on every ordinary
	// stateless API call and eventually starve real long-running sessions.
	if len(resolutionSets) == 0 && (keyHash == "" || workspaceHash == "" || initialGoalHash == "") {
		return storage.GoalSession{}, nil
	}
	segmentRequestBody := goalOriginalBody(ctx, requestBody)
	checkpointRequestBody := requestBody
	if family == storage.GoalFamilyMessages && len(segmentRequestBody) > 0 {
		// Messages checkpoints retain the request envelope (not only history), so
		// persisting the already-transformed upstream body used to make an M1
		// system prompt durable.  A later request could then replay it after the
		// user group or client opt-in was disabled.  Store the client body instead;
		// the current request policy is reapplied before every future replay.
		checkpointRequestBody = segmentRequestBody
	}
	// Keep the model-visible request before removing an already durable prefix.
	// HTTP Responses sends the complete prompt without previous_response_id, while
	// WebSocket reuse sends only a delta with previous_response_id. Persistence may
	// trim either form, but RemoteCompactionV2 must derive its replacement from the
	// complete logical prompt that preceded compaction_trigger.
	logicalRequestBody := segmentRequestBody
	var durableReplay []byte
	replaceInput := false
	if resolved, resolveErr := s.store.ResolveGoalAliasSetsForFamily(ctx, resolutionSets, family); resolveErr == nil {
		if replay, _, replayErr := s.store.BuildGoalReplay(ctx, resolved.Session.ID); replayErr == nil {
			durableReplay = replay
			segmentRequestBody, replaceInput = incrementalGoalRequestWithMode(segmentRequestBody, replay)
		}
	}
	semantics := goalSegmentSemantics{ReplaceInput: replaceInput}
	if strings.EqualFold(strings.TrimSpace(protocol), "codex") {
		semantics.CodexCompactionEvaluated = true
		semantics.ReplacementHistory, _ = codexRemoteCompactionReplacement(durableReplay, logicalRequestBody, responseBody, replaceInput)
		if len(semantics.ReplacementHistory) > 0 {
			semantics.ReplacementPrefix = codexCompactionReplacementPrefix(durableReplay, logicalRequestBody)
		}
	} else if family == storage.GoalFamilyMessages {
		// Every Messages-family provider serializes the same Claude Code native
		// compaction turn, so the replacement rule follows the family, not the vendor.
		semantics.ReplacementHistory, _ = claudeCodeCompactionReplacement(logicalRequestBody, responseBody)
	}
	checkpoint, segment, responseID, awaitingTool, err := goalCheckpointAndSegment(checkpointRequestBody, segmentRequestBody, responseBody, semantics)
	if err != nil {
		return storage.GoalSession{}, err
	}
	aliases := goalPersistentAliases(requestAliases)
	if responseID != "" {
		aliases = append(aliases, storage.GoalAlias{Type: "response_id", Value: responseID, Namespace: namespace, Family: family})
	}
	parentThread := ""
	branchThread := ""
	branchHash := ""
	if r != nil {
		parentThread = strings.TrimSpace(r.Header.Get("x-codex-parent-thread-id"))
		if branch := strings.TrimSpace(r.Header.Get("thread-id")); branch != "" && branch != parentThread {
			branchThread = branch
			branchHash = hashGoalFingerprint(branch)
		}
	}
	storageMaxBytes := s.goalStorageMaxBytes(ctx)
	storageTargetBytes, _ := goalStorageMaintenanceTarget(storageMaxBytes)
	// A foreground reclaim aims at the same floor background maintenance uses, so one
	// bounded recovery admits several subsequent turns instead of landing exactly on
	// the admission ceiling and re-triggering on the very next commit.
	storageFloorBytes := goalStorageMaintenanceFloor(storageMaxBytes)
	turn := storage.GoalTurn{
		Protocol: protocol, ParentGoalID: "", BranchHash: branchHash, DownstreamKeyHash: scopedKeyHash,
		WorkspaceHash: workspaceHash, InitialGoalHash: initialGoalHash,
		ResponseID: responseID, AliasNamespace: namespace, Aliases: aliases, CheckpointPayload: checkpoint, SegmentPayload: segment,
		ResolutionAliasSets: resolutionSets,
		ReplaceHistory:      replaceInput || len(semantics.ReplacementHistory) > 0,
		WorkingState:        goalWorkingState(requestBody), AwaitingTool: awaitingTool,
		ExpiresAt: time.Now().Add(s.goalRetention(ctx)).Unix(), StorageMaxBytes: storageMaxBytes,
		StorageTargetBytes: storageTargetBytes,
		CompressionStages:  s.goalCompressionStages(ctx),
	}
	// ParentGoalID stores an opaque parent goal reference only after a root alias has
	// already been resolved.  Keeping the thread itself out of this column avoids a
	// second plaintext identifier at rest.
	if parentThread != "" && branchThread != "" {
		if resolved, err := s.store.ResolveGoalAliasesForFamily(ctx, []storage.GoalAlias{{Type: "codex_root_thread", Value: parentThread, Namespace: namespace, Family: family}}, family); err == nil {
			turn.ParentGoalID = resolved.Session.ID
		}
	}
	session, err := s.commitGoalTurnWithStorageRecovery(ctx, turn, storageFloorBytes)
	if err != nil {
		return storage.GoalSession{}, err
	}
	// Compaction runs after the atomic terminal checkpoint write.  A worker only
	// consumes already durable segments, so a process crash merely leaves a bounded
	// chain for the next resume rather than losing the successful response.
	s.scheduleGoalCompaction(ctx, session.ID)
	return session, nil
}

// commitGoalTurnWithStorageRecovery deliberately has one retry site. The first
// transaction reports its ciphertext-aware headroom requirement, cold reclamation
// advances through a fixed number of byte/row-bounded transactions, and the exact
// same atomic turn is attempted once more. The rejected goal is protected by id in
// addition to the run/awaiting-tool predicates in storage.
func (s *Server) commitGoalTurnWithStorageRecovery(ctx context.Context, turn storage.GoalTurn, steadyTargetBytes int64) (storage.GoalSession, error) {
	session, err := s.store.CommitGoalTurn(ctx, turn)
	var budgetErr *storage.GoalStorageBudgetError
	if !errors.As(err, &budgetErr) {
		return session, err
	}
	reclaimTarget := budgetErr.ReclaimTarget()
	if steadyTargetBytes >= 0 && steadyTargetBytes < reclaimTarget {
		reclaimTarget = steadyTargetBytes
	}
	if reclaimTarget < 0 {
		return storage.GoalSession{}, err
	}
	reclaimCtx, cancel := context.WithTimeout(ctx, goalPersistenceReclaimTimeout)
	reclaimed, reclaimErr := s.store.ReclaimGoalStorageHeadroom(
		reclaimCtx, reclaimTarget, budgetErr.GoalID, goalPersistenceReclaimSteps,
	)
	cancel()
	if reclaimErr != nil {
		return storage.GoalSession{}, err
	}
	// An existing current Goal may be the only protected object in the store. Build
	// one exact, version-fenced replay checkpoint before the sole retry: this removes
	// per-segment encryption/compression/JSON fragmentation without deleting history,
	// live state, or an awaiting tool call. If a concurrent append changes either the
	// checkpoint or last segment sequence, CommitGoalTurn rejects the stale rewrite.
	recoveryTurn := turn
	consolidated := false
	if budgetErr.GoalID != "" && !turn.ReplaceHistory {
		replay, _, version, snapshotErr := s.store.BuildGoalReplaySnapshot(ctx, budgetErr.GoalID)
		if snapshotErr == nil {
			recoveryTurn.ReplaceHistory = true
			recoveryTurn.CheckpointPayload = string(replay)
			recoveryTurn.ExpectedCurrentCheckpoint = version.CurrentCheckpoint
			recoveryTurn.ExpectedLastSegmentSequence = version.LastSegmentSequence
			consolidated = true
		} else if errors.Is(snapshotErr, storage.ErrGoalInProgress) {
			return storage.GoalSession{}, snapshotErr
		}
	}
	// Retry once even if this process made no cold-reclaim progress: exact current
	// consolidation or another maintenance worker may have restored headroom.
	session, err = s.store.CommitGoalTurn(ctx, recoveryTurn)
	if err == nil && (reclaimed.Progressed || consolidated) {
		reason := "bounded_foreground_reclaim"
		if consolidated {
			reason = "bounded_reclaim_exact_current_consolidation"
		}
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "goal_storage_commit_recovered", State: session.State, Reason: reason,
			Detail: fmt.Sprintf("goal=%s bytes=%d goals=%d steps_max=%d", session.ID, reclaimed.BytesFreed, reclaimed.Goals, goalPersistenceReclaimSteps),
		})
	}
	return session, err
}

func goalPersistenceErrorCode(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, storage.ErrGoalStorageBudget):
		return "storage_budget"
	case errors.Is(err, storage.ErrGoalAmbiguous):
		return "identity_ambiguous"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "storage_timeout"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "database is locked"), strings.Contains(message, "database is busy"):
		return "storage_busy"
	case strings.Contains(message, "constraint"):
		return "storage_conflict"
	case strings.Contains(message, "invalid character"), strings.Contains(message, "cannot unmarshal"), strings.Contains(message, "unexpected end of json"):
		return "invalid_payload"
	default:
		return "storage_error"
	}
}

type goalPersistenceAuditBucket struct {
	mu         sync.Mutex
	last       time.Time
	suppressed uint64
}

var goalPersistenceAuditBuckets sync.Map

func (s *Server) auditGoalPersistenceDegraded(ctx context.Context, terminal string, err error) {
	code := goalPersistenceErrorCode(err)
	// Store identity scopes independent test/server instances without including any
	// account, thread, prompt, or workspace identifier in the key or audit payload.
	key := fmt.Sprintf("%p\x00%s\x00%s", s.store, strings.TrimSpace(terminal), code)
	value, _ := goalPersistenceAuditBuckets.LoadOrStore(key, &goalPersistenceAuditBucket{})
	bucket := value.(*goalPersistenceAuditBucket)
	now := time.Now()
	bucket.mu.Lock()
	if !bucket.last.IsZero() && now.Sub(bucket.last) < goalPersistenceAuditWindow {
		bucket.suppressed++
		bucket.mu.Unlock()
		return
	}
	suppressed := bucket.suppressed
	bucket.last = now
	bucket.suppressed = 0
	bucket.mu.Unlock()
	detail := "error_code=" + code
	if suppressed > 0 {
		detail += fmt.Sprintf(" suppressed=%d", suppressed)
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		Action: "goal_persistence_degraded", State: "retryable", Reason: terminal,
		Detail: detail,
	})
}

// crossFamilyGoal reports whether the same client identity owns a durable goal in a
// different wire family. It runs only after same-family resolution already missed, so
// it adds no query to a normal resume. It returns the goal id and the stored family.
func (s *Server) crossFamilyGoal(ctx context.Context, r *http.Request, current []byte, rawAliases []storage.GoalAlias, namespace, keyHash, family string) (string, string) {
	for _, candidate := range []string{storage.GoalFamilyMessages, storage.GoalFamilyResponses} {
		if candidate == family {
			continue
		}
		resolved, err := s.store.ResolveGoalAliasSetsForFamily(ctx,
			goalResolutionSetsForClientFamily(rawAliases, namespace, candidate, goalHasResumeAlias(rawAliases)), candidate)
		if err == nil && strings.TrimSpace(resolved.Session.ID) != "" {
			return resolved.Session.ID, candidate
		}
		resolved, err = s.store.ResolveFallbackGoalForFamily(ctx,
			scopedGoalDownstreamKeyHash(keyHash, namespace),
			goalWorkspaceFingerprint(r, current), goalInitialFingerprint(current), candidate)
		if err == nil && strings.TrimSpace(resolved.Session.ID) != "" {
			return resolved.Session.ID, candidate
		}
	}
	return "", ""
}

func (s *Server) goalReplayBody(ctx context.Context, r *http.Request, protocol string, current []byte) goalResumeResult {
	if !s.goalContinuityEnabled(ctx) {
		return goalResumeResult{Kind: goalResumeUnidentified}
	}
	rawAliases := goalAliases(r, current, protocol)
	keyHash, _ := downstreamFromCtx(ctx)
	namespace := goalDownstreamClientScope(ctx, keyHash, r)
	family := storage.GoalProtocolFamily(protocol)
	resolution, err := s.store.ResolveGoalAliasSetsForFamily(ctx,
		goalResolutionSetsForClientFamily(rawAliases, namespace, family, goalHasResumeAlias(rawAliases)), family)
	if errors.Is(err, storage.ErrGoalNotFound) {
		resolution, err = s.store.ResolveFallbackGoalForFamily(ctx, scopedGoalDownstreamKeyHash(keyHash, namespace), goalWorkspaceFingerprint(r, current), goalInitialFingerprint(current), family)
	}
	if errors.Is(err, storage.ErrGoalAmbiguous) {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "goal_resume_ambiguous", State: "failed", Reason: protocol, Detail: "multiple hashed goal aliases matched"})
		return goalResumeResult{Kind: goalResumeAmbiguous, Reason: "more than one durable goal matches the supplied identifiers"}
	}
	if errors.Is(err, storage.ErrGoalNotFound) {
		// The same client identifiers may own a goal in the other wire family — a
		// Claude Code session that previously reached a chatgpt/codex account through
		// the Anthropic→Responses bridge is exactly that case. Responses history
		// cannot be re-serialized as Messages without inventing content for encrypted
		// reasoning items and opaque item ids, so it is never converted here.
		if other, otherFamily := s.crossFamilyGoal(ctx, r, current, rawAliases, namespace, keyHash, family); other != "" {
			_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
				Action: "goal_resume_protocol_family_mismatch", State: "failed", Reason: protocol,
				Detail: "stored_family=" + otherFamily + " requested_family=" + family,
			})
			if goalHasResumeAlias(rawAliases) {
				// The request carries an upstream state handle instead of its own
				// history, so it cannot stand alone. Refusing is the only safe answer.
				return goalResumeResult{Kind: goalResumeProtocolMismatch,
					Reason: "the durable goal for these identifiers stores " + otherFamily + "-family history"}
			}
			// The request carries its own complete history, so the turn can still
			// succeed. Start a fresh same-family goal and make the restart visible.
			return goalResumeResult{Kind: goalResumeFamilyRestart,
				Reason: "durable history for these identifiers belongs to the " + otherFamily + " family"}
		}
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: "no durable goal matches the supplied identifiers"}
	}
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	// Resolution is already family filtered. This assertion keeps a future resolver
	// change from silently reaching the replay assembly below, which would serialize
	// the stored history under the wrong key and send a malformed body upstream.
	if storage.GoalProtocolFamily(resolution.Session.Protocol) != family {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: "goal_resume_protocol_family_mismatch", State: "failed", Reason: protocol,
			Detail: "stored_family=" + storage.GoalProtocolFamily(resolution.Session.Protocol) + " requested_family=" + family,
		})
		return goalResumeResult{Kind: goalResumeProtocolMismatch, Session: resolution.Session,
			Reason: "the durable goal for these identifiers stores " + storage.GoalProtocolFamily(resolution.Session.Protocol) + "-family history"}
	}
	if resolution.Session.State == "awaiting_tool_result" && !bodyHasClientToolResult(current) {
		return goalResumeResult{Kind: goalResumeRequiresToolResult, Session: resolution.Session, Reason: "the previous turn is waiting for a client tool result"}
	}
	base, session, err := s.store.BuildGoalReplay(ctx, resolution.Session.ID)
	if err != nil {
		if errors.Is(err, storage.ErrGoalStorageBudget) {
			return goalResumeResult{Kind: goalResumeStorageExhausted, Reason: err.Error()}
		}
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	if family == storage.GoalFamilyMessages {
		// Backward compatibility for checkpoints written before Messages persisted
		// the original client envelope.  Strip only exact M1-owned carriers from
		// the historical base; the already policy-processed current request below
		// remains authoritative and may add a fresh system carrier when enabled.
		base, err = stripLegacySuperInstructCarriers(base)
		if err != nil {
			return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
		}
	}
	cur, err := decodeContextJSONMap(current)
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	replayed, err := decodeContextJSONMap(base)
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	historyKey := "input"
	if family == storage.GoalFamilyMessages {
		historyKey = "messages"
		for _, key := range []string{"model", "system", "tools", "max_tokens", "temperature", "top_p", "thinking", "stream", "metadata"} {
			if value, ok := cur[key]; ok {
				replayed[key] = value
			}
		}
		incrementalBody, replaceInput := incrementalGoalRequestWithMode(current, base)
		if replaceInput {
			replayed[historyKey] = cur[historyKey]
		} else if incremental, incrementalErr := decodeContextJSONMap(incrementalBody); incrementalErr == nil {
			replayed[historyKey] = appendItems(replayed[historyKey], incremental[historyKey])
		} else {
			replayed[historyKey] = appendItems(replayed[historyKey], cur[historyKey])
		}
	} else {
		// The current request is authoritative for every current-turn setting,
		// including future fields. Keeping only a historical whitelist silently
		// dropped tool_choice, parallel_tool_calls, reasoning.context, text and
		// Responses Lite metadata during an account migration. Start from a fresh
		// envelope so fields intentionally omitted now cannot leak from an old
		// checkpoint.
		mergedInput := mergeCodexGoalReplayInput(replayed[historyKey], cur[historyKey])
		currentEnvelope := make(map[string]interface{}, len(cur)+1)
		for key, value := range cur {
			switch key {
			case "input", "messages", "previous_response_id", "turn_state":
				continue
			default:
				currentEnvelope[key] = value
			}
		}
		if _, ok := currentEnvelope["model"]; !ok {
			// Every official request includes model. Retaining it only as a malformed
			// request fallback is safer than emitting a model-less fresh root.
			currentEnvelope["model"] = replayed["model"]
		}
		currentEnvelope[historyKey] = mergedInput
		replayed = currentEnvelope
	}
	if historyKey == "messages" {
		delete(replayed, "input")
	} else {
		delete(replayed, "messages")
	}
	delete(replayed, "previous_response_id")
	delete(replayed, "turn_state")
	// Sessions written before custom_tool_call was added to the awaiting-state
	// detector may still say ready.  Validate the reconstructed, chronological
	// history as the last line of defence so those older checkpoint chains cannot be
	// sent upstream without the tool result that completes the call.
	if protocol == "codex" && hasPendingClientToolCall(replayed[historyKey]) {
		return goalResumeResult{Kind: goalResumeRequiresToolResult, Session: session, Reason: "the reconstructed history contains a client tool call without its result"}
	}
	body, err := json.Marshal(replayed)
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "goal_resume_recovered", State: session.State, Reason: protocol, Detail: fmt.Sprintf("goal=%s checkpoint=%s", session.ID, session.CurrentCheckpoint)})
	return goalResumeResult{Kind: goalResumeFound, Body: body, Session: session}
}

// writeGoalResumeError always produces a protocol-visible terminal event for streams;
// a JSON error is used only for non-streaming calls that have not started a stream.
func writeGoalResumeError(w http.ResponseWriter, stream bool, protocol string, kind goalResumeKind, detail string) {
	code := "goal_resume_context_unidentified"
	message := "Unable to identify a durable goal context for this resume request."
	switch kind {
	case goalResumeAmbiguous:
		code, message = "goal_resume_ambiguous", "More than one durable goal matches this resume request."
	case goalResumeRequiresToolResult:
		code, message = "goal_resume_requires_tool_result", "The prior turn requires a client tool result before it can resume."
	case goalResumeStorageExhausted:
		code, message = "goal_storage_budget_exhausted", "Goal storage is full; active context was retained."
	case goalResumeInProgress:
		code, message = "goal_in_progress", "This goal is already being resumed; retry after its last heartbeat lease expires."
	case goalResumeProtocolMismatch:
		code, message = "goal_resume_protocol_family_mismatch", "The durable context for this session was recorded in a different upstream wire protocol and cannot be replayed here. Start a new session, or send this turn to the original provider family."
	}
	if !stream {
		writePoolCodeError(w, http.StatusConflict, code, message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// CLI SSE implementations commonly discard a non-2xx response before parsing
	// its body. A stream-level failure therefore has to use a successful transport
	// status and carry the failure in the provider-native terminal event; otherwise
	// a concurrent goal/lease conflict looks like a silent unanswered prompt.
	w.Header().Set("X-MiCliProxy-Goal-Error", code)
	w.WriteHeader(http.StatusOK)
	if storage.GoalProtocolFamily(protocol) == storage.GoalFamilyMessages {
		_, _ = fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"code\":%q,\"message\":%q}}\n\n", code, message)
		_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	} else {
		_, _ = fmt.Fprintf(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":%q,\"message\":%q}}}\n\n", code, message)
	}
	if flush, ok := w.(http.Flusher); ok {
		flush.Flush()
	}
	_ = detail // kept out of downstream and audit logs because it can contain driver details.
}
