package api

// v2 goal continuity is intentionally request-protocol aware but transport neutral.
// The gateway never sees a literal `/goal resume`; it sees the durable identifiers the
// clients send on the following request.  This layer translates only those exact
// identifiers into an encrypted checkpoint replay and never guesses from a model,
// cache prefix, account affinity, or API-key alone.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

type goalResumeKind string

const (
	goalResumeFound              goalResumeKind = "recovered"
	goalResumeUnidentified       goalResumeKind = "unidentified"
	goalResumeAmbiguous          goalResumeKind = "ambiguous"
	goalResumeRequiresToolResult goalResumeKind = "requires_tool_result"
	goalResumeStorageExhausted   goalResumeKind = "storage_exhausted"
	goalResumeInProgress         goalResumeKind = "in_progress"
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
	return context.WithValue(ctx, goalOriginalBodyKey{}, append([]byte(nil), body...))
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
	mb := s.settingInt(ctx, "goal_storage_max_mb", s.cfg.GoalStorageMaxMB)
	if mb <= 0 {
		mb = 256
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

const goalCompactionForegroundBudget = 540 * time.Second

// scheduleGoalCompaction starts a bounded, lease-protected local checkpoint job after
// a terminal turn.  It never blocks the client response and never runs two jobs for
// the same active goal; a saturated global limit leaves the committed segment chain
// intact for a later resume/turn to pick up.
func (s *Server) scheduleGoalCompaction(ctx context.Context, goalID string) {
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
	limit := s.goalCompressionConcurrency(ctx)
	s.goalCompactionMu.Lock()
	if s.goalCompactionActive >= limit {
		s.goalCompactionMu.Unlock()
		_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_pending", State: "retryable", Reason: "global_concurrency", Detail: "goal=" + goalID})
		return
	}
	s.goalCompactionActive++
	s.goalCompactionMu.Unlock()
	ratio := s.goalCompressionChunkRatio(ctx)
	supervisor.GoOnce("goal-compaction", func() {
		defer func() {
			s.goalCompactionMu.Lock()
			s.goalCompactionActive--
			s.goalCompactionMu.Unlock()
		}()
		jobCtx, cancel := context.WithTimeout(context.Background(), goalCompactionForegroundBudget)
		defer cancel()
		var finish func(string, string)
		for {
			var leaseErr error
			finish, leaseErr = s.beginGoalRunWithResult(jobCtx, goalID, "compacting")
			if leaseErr == nil {
				break
			}
			if !errors.Is(leaseErr, storage.ErrGoalInProgress) {
				_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "lease", Detail: "goal=" + goalID})
				return
			}
			select {
			case <-jobCtx.Done():
				_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_pending", State: "retryable", Reason: "lease_wait_budget", Detail: "goal=" + goalID})
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
		finishState, finishCode := "completed", ""
		defer func() { finish(finishState, finishCode) }()
		if err := s.store.SetGoalCompactionState(jobCtx, goalID, "compacting"); err != nil {
			finishState, finishCode = "retryable", "goal_compaction_failed"
			_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "state", Detail: "goal=" + goalID})
			return
		}
		completed := false
		defer func() {
			if completed {
				_ = s.store.SetGoalCompactionState(context.Background(), goalID, "ready")
			} else {
				_ = s.store.SetGoalCompactionState(context.Background(), goalID, "retryable")
				finishState = "retryable"
				if finishCode == "" {
					finishCode = "goal_compaction_pending"
				}
			}
		}()
		for {
			if err := jobCtx.Err(); err != nil {
				finishCode = "goal_compaction_pending"
				_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_pending", State: "retryable", Reason: "foreground_budget", Detail: "goal=" + goalID})
				return
			}
			needed, err := s.store.NeedsGoalCompaction(jobCtx, goalID, stages)
			if err != nil {
				finishCode = "goal_compaction_failed"
				_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "threshold", Detail: "goal=" + goalID})
				return
			}
			if !needed {
				completed = true
				return
			}
			if err := s.store.CompactGoalSegmentsWithRatio(jobCtx, goalID, stages, ratio); err != nil {
				finishCode = "goal_compaction_failed"
				_ = s.store.InsertAuditLog(context.Background(), storage.AuditLogRow{Action: "goal_compaction_failed", State: "retryable", Reason: "checkpoint", Detail: "goal=" + goalID})
				return
			}
		}
	})
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
	add("response_id", jsonStringField(body, "previous_response_id"))
	// Some Codex transports put the same state token in a JSON field.  Hashing it
	// under the same kind preserves compatibility without writing the opaque token.
	add("codex_turn_state", jsonStringField(body, "turn_state"))
	if protocol == "claude" {
		add("claude_session", jsonStringField(body, "session_id"))
	}
	// These are real client conversation identifiers, not cache-derived correlators.
	add("conversation_id", jsonStringField(body, "conversation_id"))
	add("thread_id", jsonStringField(body, "thread_id"))
	add("session_id", jsonStringField(body, "session_id"))
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
		seen[alias.Type+"\x00"+alias.Value] = true
	}
	for _, alias := range aliases {
		if alias.Type != "response_id" && alias.Type != "codex_turn_state" {
			continue
		}
		key := alias.Type + "\x00" + alias.Value
		if strings.TrimSpace(alias.Value) != "" && !seen[key] {
			seen[key] = true
			out = append(out, alias)
		}
	}
	return out
}

func jsonStringField(raw []byte, key string) string {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	value, _ := root[key].(string)
	return strings.TrimSpace(value)
}

func nestedJSONStringField(raw []byte, parent, key string) string {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return ""
	}
	child, _ := root[parent].(map[string]interface{})
	value, _ := child[key].(string)
	return strings.TrimSpace(value)
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
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, `"function_call_output"`) || strings.Contains(lower, `"local_shell_call_output"`) ||
		strings.Contains(lower, `"mcp_tool_call_output"`) || strings.Contains(lower, `"custom_tool_call_output"`) ||
		strings.Contains(lower, `"tool_search_output"`) || strings.Contains(lower, `"tool_result"`) || strings.Contains(lower, `"tool_use_result"`)
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

func goalCheckpointAndSegment(requestBody, segmentRequestBody, responseBody []byte) (string, string, string, bool, error) {
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
	segment, err := json.Marshal(map[string]interface{}{"history_key": historyKey, "input": history, "output": output})
	if err != nil {
		return "", "", "", false, err
	}
	return string(checkpoint), string(segment), strings.TrimSpace(responseID), outputAwaitingTool(response), nil
}

// goalResponseFromSSE retains a protocol stream as encrypted structured segment data
// without pretending its provider-specific events are Responses output.  Only a small
// capture is used by the Claude path; unknown content and attachments remain verbatim
// JSON event payloads instead of being flattened to text.
func goalResponseFromSSE(raw []byte) []byte {
	frames := make([]interface{}, 0)
	messageID := ""
	for _, frame := range strings.Split(string(raw), "\n\n") {
		var dataLines []string
		for _, line := range strings.Split(frame, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		var event map[string]interface{}
		if json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &event) != nil {
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
	text := first.partialText() + firstNonEmpty(streamString(response["output_text"]), continuation.partialText())
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
	aliases := goalAliases(r, requestBody, protocol)
	aliases = append(aliases, goalIdentityAliases(ctx)...)
	resolved, err := s.resolveGoalAliasesByPriority(ctx, aliases, goalHasResumeAlias(aliases))
	if errors.Is(err, storage.ErrGoalNotFound) {
		keyHash, _ := downstreamFromCtx(ctx)
		resolved, err = s.store.ResolveFallbackGoal(ctx, keyHash, goalWorkspaceFingerprint(r, requestBody), goalInitialFingerprint(requestBody))
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
	checkpoint, segment, responseID, awaitingTool, err := goalCheckpointAndSegment(requestBody, goalOriginalBody(ctx, requestBody), responseBody)
	if err != nil {
		return storage.GoalSession{}, err
	}
	requestAliases := goalAliases(r, requestBody, protocol)
	requestAliases = append(requestAliases, goalIdentityAliases(ctx)...)
	resolutionSets := goalResolutionAliasSets(requestAliases, goalHasResumeAlias(requestAliases))
	aliases := goalPersistentAliases(requestAliases)
	if responseID != "" {
		aliases = append(aliases, storage.GoalAlias{Type: "response_id", Value: responseID})
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
	keyHash, _ := downstreamFromCtx(ctx)
	turn := storage.GoalTurn{
		Protocol: protocol, ParentGoalID: "", BranchHash: branchHash, DownstreamKeyHash: keyHash,
		WorkspaceHash: goalWorkspaceFingerprint(r, requestBody), InitialGoalHash: goalInitialFingerprint(requestBody),
		ResponseID: responseID, Aliases: aliases, CheckpointPayload: checkpoint, SegmentPayload: segment,
		ResolutionAliasSets: resolutionSets,
		WorkingState:        goalWorkingState(requestBody), AwaitingTool: awaitingTool,
		ExpiresAt: time.Now().Add(s.goalRetention(ctx)).Unix(), StorageMaxBytes: s.goalStorageMaxBytes(ctx),
		CompressionStages: s.goalCompressionStages(ctx),
	}
	// ParentGoalID stores an opaque parent goal reference only after a root alias has
	// already been resolved.  Keeping the thread itself out of this column avoids a
	// second plaintext identifier at rest.
	if parentThread != "" && branchThread != "" {
		if resolved, err := s.store.ResolveGoalAliases(ctx, []storage.GoalAlias{{Type: "codex_root_thread", Value: parentThread}}); err == nil {
			turn.ParentGoalID = resolved.Session.ID
		}
	}
	session, err := s.store.CommitGoalTurn(ctx, turn)
	if err != nil {
		return storage.GoalSession{}, err
	}
	// Compaction runs after the atomic terminal checkpoint write.  A worker only
	// consumes already durable segments, so a process crash merely leaves a bounded
	// chain for the next resume rather than losing the successful response.
	s.scheduleGoalCompaction(ctx, session.ID)
	return session, nil
}

func (s *Server) goalReplayBody(ctx context.Context, r *http.Request, protocol string, current []byte) goalResumeResult {
	if !s.goalContinuityEnabled(ctx) {
		return goalResumeResult{Kind: goalResumeUnidentified}
	}
	aliases := goalAliases(r, current, protocol)
	resolution, err := s.resolveGoalAliasesByPriority(ctx, aliases, goalHasResumeAlias(aliases))
	if errors.Is(err, storage.ErrGoalNotFound) {
		keyHash, _ := downstreamFromCtx(ctx)
		resolution, err = s.store.ResolveFallbackGoal(ctx, keyHash, goalWorkspaceFingerprint(r, current), goalInitialFingerprint(current))
	}
	if errors.Is(err, storage.ErrGoalAmbiguous) {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{Action: "goal_resume_ambiguous", State: "failed", Reason: protocol, Detail: "multiple hashed goal aliases matched"})
		return goalResumeResult{Kind: goalResumeAmbiguous, Reason: "more than one durable goal matches the supplied identifiers"}
	}
	if errors.Is(err, storage.ErrGoalNotFound) {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: "no durable goal matches the supplied identifiers"}
	}
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
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
	cur, err := decodeContextJSONMap(current)
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	replayed, err := decodeContextJSONMap(base)
	if err != nil {
		return goalResumeResult{Kind: goalResumeUnidentified, Reason: err.Error()}
	}
	keys := []string{"model", "instructions", "tools", "reasoning", "stream", "include"}
	if protocol == "claude" || protocol == "kiro" {
		keys = []string{"model", "system", "tools", "max_tokens", "temperature", "top_p", "thinking", "stream", "metadata"}
	}
	for _, key := range keys {
		if value, ok := cur[key]; ok {
			replayed[key] = value
		}
	}
	historyKey := "input"
	if protocol == "claude" || protocol == "kiro" {
		historyKey = "messages"
	}
	replayed[historyKey] = appendItems(replayed[historyKey], cur[historyKey])
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
	if protocol == "claude" {
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
