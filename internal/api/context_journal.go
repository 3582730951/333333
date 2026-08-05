package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/storage"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// journalReplayBody creates the stateless Responses body used when account-local
// previous_response_id state is unavailable. Payloads are encrypted by storage.
func (s *Server) journalReplayBody(ctx context.Context, current []byte) ([]byte, bool) {
	// v2 is the durable first choice.  The v1 journal below remains a read fallback
	// during the dual-write migration, so an existing task is never stranded merely
	// because its latest successful turn predates the new tables.
	if replay := s.goalReplayBody(ctx, nil, "codex", current); replay.Kind == goalResumeFound {
		return replay.Body, true
	}
	cur, err := decodeContextJSONMap(current)
	if err != nil {
		return current, false
	}
	prev, _ := cur["previous_response_id"].(string)
	if strings.TrimSpace(prev) == "" {
		return current, false
	}
	j, err := s.store.GetContextJournal(ctx, prev)
	if err != nil {
		return current, false
	}
	// Sliding TTL: a resumed tail refreshes its own expiry so an arbitrary-duration
	// task stays restorable indefinitely without growing disk.
	s.touchContextJournal(ctx, prev)
	base, err := decodeContextJSONMap([]byte(j.Payload))
	if err != nil {
		return current, false
	}
	base["model"] = cur["model"]
	for _, k := range []string{"instructions", "tools", "reasoning", "stream", "include"} {
		if v, ok := cur[k]; ok {
			base[k] = v
		}
	}
	base["input"] = appendItems(base["input"], cur["input"])
	delete(base, "previous_response_id")
	delete(base, "turn_state")
	out, e := json.Marshal(base)
	return out, e == nil
}

func appendItems(a, b interface{}) []interface{} {
	out := []interface{}{}
	if x, ok := a.([]interface{}); ok {
		out = append(out, x...)
	} else if a != nil {
		out = append(out, a)
	}
	if x, ok := b.([]interface{}); ok {
		out = append(out, x...)
	} else if b != nil {
		out = append(out, b)
	}
	return out
}

func (s *Server) persistContextJournal(ctx context.Context, requestBody, responseBody []byte, affinityHash, accountID string) error {
	if s.goalContinuityEnabled(ctx) && !s.flagEnabled(ctx, "goal_legacy_journal_dual_write", s.cfg.GoalLegacyJournalDualWrite) {
		// Migration phase 4: keep v1 rows readable until their natural TTL expires,
		// but do not keep copying complete histories once v2 recovery is validated.
		return nil
	}
	req, reqErr := decodeContextJSONMap(requestBody)
	resp, respErr := decodeContextJSONMap(responseBody)
	if reqErr != nil || respErr != nil {
		return errors.New("invalid context journal payload")
	}
	id, _ := resp["id"].(string)
	if id == "" {
		return errors.New("context journal response id missing")
	}
	if prev, _ := req["previous_response_id"].(string); prev != "" {
		if j, e := s.store.GetContextJournal(ctx, prev); e == nil {
			if base, decodeErr := decodeContextJSONMap([]byte(j.Payload)); decodeErr == nil {
				base["input"] = appendItems(base["input"], req["input"])
				req = base
			}
			// Keep the chain's live tail warm as the conversation advances.
			s.touchContextJournal(ctx, prev)
		}
	}
	delete(req, "previous_response_id")
	if output, ok := resp["output"]; ok {
		req["input"] = appendItems(req["input"], output)
	}
	payload, e := json.Marshal(req)
	if e != nil {
		return e
	}
	ttl := s.contextJournalTTLSeconds(ctx)
	return s.store.PutContextJournal(ctx, storage.ContextJournal{ResponseID: id, AffinityHash: affinityHash, AccountID: accountID, Payload: string(payload), ExpiresAt: time.Now().Add(time.Duration(ttl) * time.Second).Unix()})
}

// contextJournalTTLSeconds resolves the effective journal TTL: the hot setting (or boot
// default), shrunk by the disk guard's forced TTL under disk pressure, floored at 1h.
func (s *Server) contextJournalTTLSeconds(ctx context.Context) int {
	ttl := s.settingInt(ctx, "context_journal_ttl_seconds", s.cfg.ContextJournalTTLSeconds)
	if forced := s.diskGuardTTL(); forced > 0 && (ttl <= 0 || forced < ttl) {
		ttl = forced
	}
	if ttl <= 0 {
		ttl = 3600
	}
	return ttl
}

// touchContextJournal slides a journal row's expiry to now+TTL on read. Best-effort: a
// write-pool hiccup never fails the read that triggered it.
func (s *Server) touchContextJournal(ctx context.Context, id string) {
	if strings.TrimSpace(id) == "" {
		return
	}
	ttl := s.contextJournalTTLSeconds(ctx)
	_ = s.store.TouchContextJournal(ctx, id, time.Now().Add(time.Duration(ttl)*time.Second).Unix())
}

func ensureEncryptedReasoningInclude(body []byte) []byte {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return body
	}
	items, _ := root["include"].([]interface{})
	for _, v := range items {
		if v == "reasoning.encrypted_content" {
			return body
		}
	}
	root["include"] = append(items, "reasoning.encrypted_content")
	out, e := json.Marshal(root)
	if e != nil {
		return body
	}
	return out
}

func degradedResponsesReplay(body []byte) []byte {
	return degradedResponsesReplayForContextError(body, leakfilter.ResponsesContextErrorNone)
}

// stripAgentMessageEncryptedContent removes only encrypted_content blocks nested
// in an agent_message's content array. Those blocks are private to the subagent
// response that produced them; a fresh root has no upstream response state capable
// of decrypting them. Ordinary input text, user history, tool items, and reasoning
// ciphertext are intentionally untouched.
//
// Deletions run from the end of each array so indexes remain stable. sjson edits
// only the selected fragments, preserving the byte representation of unrelated
// history (including large JSON numbers) instead of re-marshalling a large turn.
func stripAgentMessageEncryptedContent(body []byte) ([]byte, int) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, 0
	}
	type removal struct {
		inputIndex   int
		contentIndex int
		dropItem     bool
	}
	removals := make([]removal, 0)
	removedBlocks := 0
	for inputIndex, rawItem := range input.Array() {
		if rawItem.Get("type").String() != "agent_message" {
			continue
		}
		content := rawItem.Get("content")
		if !content.IsArray() {
			continue
		}
		blocks := content.Array()
		itemRemovals := make([]removal, 0)
		for contentIndex, block := range blocks {
			if block.Get("type").String() != "encrypted_content" {
				continue
			}
			itemRemovals = append(itemRemovals, removal{inputIndex: inputIndex, contentIndex: contentIndex})
			removedBlocks++
		}
		if len(itemRemovals) == 0 {
			continue
		}
		if len(itemRemovals) == len(blocks) {
			removals = append(removals, removal{inputIndex: inputIndex, dropItem: true})
			continue
		}
		removals = append(removals, itemRemovals...)
	}
	if removedBlocks == 0 {
		return body, 0
	}

	out := append([]byte(nil), body...)
	for index := len(removals) - 1; index >= 0; index-- {
		remove := removals[index]
		path := fmt.Sprintf("input.%d.content.%d", remove.inputIndex, remove.contentIndex)
		if remove.dropItem {
			path = fmt.Sprintf("input.%d", remove.inputIndex)
		}
		var err error
		out, err = sjson.DeleteBytes(out, path)
		if err != nil {
			return body, 0
		}
	}
	return out, removedBlocks
}

func degradedResponsesReplayForContextError(body []byte, contextError leakfilter.ResponsesContextErrorKind) []byte {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return body
	}
	delete(root, "previous_response_id")
	delete(root, "turn_state")
	// Stripping previous_response_id abandons the server-side state that held the tool
	// CALLS. Any tool-call OUTPUT still in the input then references a call the upstream
	// can no longer find — a hard 400 "No tool call found for custom tool call output
	// with call_id ...". Rewrite each orphaned output into a plain user message that
	// preserves the tool-result text, so the degraded request is valid and the turn still
	// succeeds (a paired call+output already in the input is left untouched).
	if input, ok := root["input"].([]interface{}); ok {
		fixed, n := neutralizeOrphanedToolOutputs(input)
		if contextError == leakfilter.ResponsesContextErrorOrphanedToolOutput ||
			contextError == leakfilter.ResponsesContextErrorEncryptedFunctionOutput {
			// The upstream explicitly rejected a tool output. Even if the current
			// payload contains a superficially paired call, that pair was not valid in
			// upstream context. Remove completed calls and preserve their results as
			// user context so retrying cannot execute the tools a second time.
			fixed, n = neutralizeCompletedToolExchanges(input, contextError == leakfilter.ResponsesContextErrorEncryptedFunctionOutput)
		}
		if n > 0 {
			root["input"] = fixed
		}
	}
	out, e := json.Marshal(root)
	if e != nil {
		return body
	}
	return out
}

// neutralizeCompletedToolExchanges is the stronger fallback used only after an exact
// rejected-tool-output response. Every client-managed tool output becomes user
// context, and a matching call in the same input is removed with it. Calls without an
// output remain untouched because they may still require execution.
func neutralizeCompletedToolExchanges(input []interface{}, discardEncryptedContent bool) ([]interface{}, int) {
	completed := map[string]bool{}
	for _, it := range input {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		kind := toolOutputPairKind(m)
		if kind == "" || kind == "tool_search_server" {
			continue
		}
		if cid := streamString(m["call_id"]); cid != "" {
			completed[kind+"\x00"+cid] = true
		}
	}

	converted := 0
	out := make([]interface{}, 0, len(input))
	for _, it := range input {
		m, ok := it.(map[string]interface{})
		if !ok {
			out = append(out, it)
			continue
		}
		callKind := toolCallPairKind(streamString(m["type"]))
		if callKind == "" && strings.EqualFold(strings.TrimSpace(streamString(m["type"])), "mcp_tool_call") {
			callKind = "function"
		}
		if cid := streamString(m["call_id"]); callKind != "" && cid != "" && completed[callKind+"\x00"+cid] {
			continue
		}
		outputKind := toolOutputPairKind(m)
		if outputKind != "" && outputKind != "tool_search_server" {
			if discardEncryptedContent {
				clean := make(map[string]interface{}, len(m))
				for key, value := range m {
					if key != "encrypted_content" {
						clean[key] = value
					}
				}
				m = clean
			}
			out = append(out, orphanedToolOutputAsUserMessage(m))
			converted++
			continue
		}
		out = append(out, it)
	}
	return out, converted
}

// neutralizeOrphanedToolOutputs converts every tool-call output item whose matching tool
// call is NOT present in the same input list into a user message carrying the result
// text. It returns the rewritten list and the number of items converted. Paired
// call/output items and non-tool items pass through unchanged.
func neutralizeOrphanedToolOutputs(input []interface{}) ([]interface{}, int) {
	calls := map[string]bool{}
	for _, it := range input {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		if kind := toolCallPairKind(streamString(m["type"])); kind != "" {
			if cid := streamString(m["call_id"]); cid != "" {
				calls[kind+"\x00"+cid] = true
			}
		}
	}
	converted := 0
	out := make([]interface{}, 0, len(input))
	for _, it := range input {
		if m, ok := it.(map[string]interface{}); ok {
			if kind := toolOutputPairKind(m); kind != "" {
				// A server-executed tool search is already resolved by the Responses
				// service and is explicitly allowed to omit call_id in stable Codex.
				if kind == "tool_search_server" {
					out = append(out, it)
					continue
				}
				cid := streamString(m["call_id"])
				if cid == "" || !calls[kind+"\x00"+cid] {
					out = append(out, orphanedToolOutputAsUserMessage(m))
					converted++
					continue
				}
			}
		}
		out = append(out, it)
	}
	return out, converted
}

// toolCallPairKind and toolOutputPairKind intentionally enumerate the stable
// rust-v0.146.0 pairing rules. Unknown future *_call fields are not guessed at: native
// Responses forwarding preserves them, while context recovery leaves them untouched.
func toolCallPairKind(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "function_call", "local_shell_call":
		return "function"
	case "custom_tool_call":
		return "custom"
	case "tool_search_call":
		return "tool_search"
	default:
		return ""
	}
}

// hasPendingClientToolCall reports whether a stateless Responses history carries a
// client-managed tool call without its matching result.  A replay must never forward
// such a history: the upstream correctly rejects it with e.g. "No tool output found
// for custom tool call ...", while executing the call again would be unsafe.
//
// Server-executed tool search calls are excluded because their result is owned by the
// Responses service rather than the downstream client.  Pairing is intentionally
// ordered: an output that appears before its call is invalid and does not make a later
// call look complete.
func hasPendingClientToolCall(input interface{}) bool {
	pending := map[string]struct{}{}
	for _, raw := range appendItems(nil, input) {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if kind := clientToolCallPairKind(item); kind != "" {
			if callID := streamString(item["call_id"]); callID != "" {
				pending[kind+"\x00"+callID] = struct{}{}
			}
			continue
		}
		if kind := toolOutputPairKind(item); kind != "" && kind != "tool_search_server" {
			if callID := streamString(item["call_id"]); callID != "" {
				delete(pending, kind+"\x00"+callID)
			}
		}
	}
	return len(pending) > 0
}

func clientToolCallPairKind(item map[string]interface{}) string {
	kind := toolCallPairKind(streamString(item["type"]))
	if kind != "tool_search" {
		return kind
	}
	if strings.EqualFold(strings.TrimSpace(streamString(item["execution"])), "client") {
		return kind
	}
	return ""
}

func toolOutputPairKind(item map[string]interface{}) string {
	t := strings.ToLower(strings.TrimSpace(streamString(item["type"])))
	switch t {
	case "function_call_output", "local_shell_call_output", "mcp_tool_call_output":
		return "function"
	case "custom_tool_call_output":
		return "custom"
	case "tool_search_output":
		if strings.EqualFold(strings.TrimSpace(streamString(item["execution"])), "server") {
			return "tool_search_server"
		}
		return "tool_search"
	default:
		return ""
	}
}

func isToolOutputItemType(t string) bool {
	return toolOutputPairKind(map[string]interface{}{"type": t}) != ""
}

func isToolCallItemType(t string) bool {
	return toolCallPairKind(t) != ""
}

func orphanedToolOutputAsUserMessage(m map[string]interface{}) map[string]interface{} {
	text := degradedToolOutputText(m)
	if strings.TrimSpace(text) == "" {
		text = "(tool result unavailable)"
	}
	return map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": "[Earlier tool result] " + text},
		},
	}
}

func degradedToolOutputText(item map[string]interface{}) string {
	output, hasOutput := item["output"]
	if text, ok := output.(string); ok && toolOutputHasOnlyTextFields(item) {
		return text
	}
	if !hasOutput && streamString(item["type"]) != "tool_search_output" {
		return ""
	}
	// Marshal the complete item rather than only output so image URLs, error/status
	// fields, encrypted payloads, tool-search execution metadata, and future fields all
	// remain model-visible after the stateless downgrade. Maps are encoded with sorted
	// keys by encoding/json and numbers came from a UseNumber decoder.
	if raw, err := json.Marshal(item); err == nil {
		return string(raw)
	}
	return toolOutputText(output)
}

func toolOutputHasOnlyTextFields(item map[string]interface{}) bool {
	for key := range item {
		switch key {
		case "type", "id", "call_id", "name", "output":
		default:
			return false
		}
	}
	return true
}

func toolOutputText(v interface{}) string {
	switch out := v.(type) {
	case string:
		return out
	case nil:
		return ""
	default:
		if b, err := json.Marshal(out); err == nil {
			return string(b)
		}
		return ""
	}
}

// responsesContextError recognizes the precise upstream 400s that mean account-local
// Responses context is unavailable: previous_response_id is gone or rejected after a
// transport fallback, or a tool output references a call that disappeared with that
// context.
func responsesContextError(status int, body []byte) leakfilter.ResponsesContextErrorKind {
	return leakfilter.DetectResponsesContextError(status, body)
}

// responsesNeedsDegrade reports whether degradedResponsesReplay would actually change the
// request (it still carries server-side-state pointers or an orphaned tool output). It
// gates the reactive degrade-and-retry so a body that is already stateless-and-clean is
// never retried in a loop.
func responsesNeedsDegrade(body []byte) bool {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return false
	}
	if v, _ := root["previous_response_id"].(string); strings.TrimSpace(v) != "" {
		return true
	}
	if _, ok := root["turn_state"]; ok {
		return true
	}
	if input, ok := root["input"].([]interface{}); ok {
		if _, n := neutralizeOrphanedToolOutputs(input); n > 0 {
			return true
		}
	}
	return false
}

// responsesHasUnpairedToolOutput reports whether removing upstream-owned response
// state would strand a client tool result. Such a result must never be disguised as
// an ordinary user message during mapped-session rotation: either a durable replay
// restores its matching call, or recovery stops before contacting a fresh session.
func responsesHasUnpairedToolOutput(body []byte, contextError leakfilter.ResponsesContextErrorKind) bool {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return bodyHasClientToolResult(body)
	}
	input, _ := root["input"].([]interface{})
	if contextError == leakfilter.ResponsesContextErrorOrphanedToolOutput ||
		contextError == leakfilter.ResponsesContextErrorEncryptedFunctionOutput {
		return bodyHasClientToolResult(body)
	}
	_, unpaired := neutralizeOrphanedToolOutputs(input)
	return unpaired > 0
}

func responsesHasEncryptedToolOutput(body []byte) bool {
	root, err := decodeContextJSONMap(body)
	if err != nil {
		return false
	}
	input, _ := root["input"].([]interface{})
	for _, raw := range input {
		item, ok := raw.(map[string]interface{})
		if !ok || toolOutputPairKind(item) == "" {
			continue
		}
		if encrypted, present := item["encrypted_content"]; present && encrypted != nil && strings.TrimSpace(streamString(encrypted)) != "" {
			return true
		}
	}
	return false
}

func decodeContextJSONMap(raw []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]interface{}
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

// responsesRecoveryEligible reports whether a failed request contains account-local
// Responses state or an orphaned tool result that can be made stateless. It is also
// used when allocating the retry budget: recovery must have a second attempt even
// when no context journal exists.
func responsesRecoveryEligible(body []byte, header http.Header) bool {
	if strings.TrimSpace(header.Get("X-Codex-Turn-State")) != "" || responsesNeedsDegrade(body) {
		return true
	}
	return false
}

// recoverResponsesContext builds the one permitted retry after the upstream reports
// missing Responses context. Journal replay is lossless and therefore wins; otherwise
// degradation preserves current input and rewrites orphaned tool outputs as user
// context. Both paths discard every account-local state pointer before retrying.
func (s *Server) recoverResponsesContext(ctx context.Context, body []byte, header http.Header, contextError leakfilter.ResponsesContextErrorKind) (codexRetryRequest, string, bool) {
	if rebuilt, ok := s.journalReplayBody(ctx, body); ok {
		return codexRetryRequest{
			Raw:    rebuilt,
			Header: stripCodexServerStateHeaders(header),
		}, "rebuilt", true
	}
	if contextError == leakfilter.ResponsesContextErrorNone && !responsesRecoveryEligible(body, header) {
		return codexRetryRequest{}, "", false
	}
	return codexRetryRequest{
		Raw:    degradedResponsesReplayForContextError(body, contextError),
		Header: stripCodexServerStateHeaders(header),
	}, "degraded", true
}
