package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/storage"
)

// journalReplayBody creates the stateless Responses body used when account-local
// previous_response_id state is unavailable. Payloads are encrypted by storage.
func (s *Server) journalReplayBody(ctx context.Context, current []byte) ([]byte, bool) {
	var cur map[string]interface{}
	if json.Unmarshal(current, &cur) != nil {
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
	var base map[string]interface{}
	if json.Unmarshal([]byte(j.Payload), &base) != nil {
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
	var req, resp map[string]interface{}
	if json.Unmarshal(requestBody, &req) != nil || json.Unmarshal(responseBody, &resp) != nil {
		return errors.New("invalid context journal payload")
	}
	id, _ := resp["id"].(string)
	if id == "" {
		return errors.New("context journal response id missing")
	}
	if prev, _ := req["previous_response_id"].(string); prev != "" {
		if j, e := s.store.GetContextJournal(ctx, prev); e == nil {
			var base map[string]interface{}
			if json.Unmarshal([]byte(j.Payload), &base) == nil {
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
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
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
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
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
		if fixed, n := neutralizeOrphanedToolOutputs(input); n > 0 {
			root["input"] = fixed
		}
	}
	out, e := json.Marshal(root)
	if e != nil {
		return body
	}
	return out
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
		if isToolCallItemType(streamString(m["type"])) {
			if cid := streamString(m["call_id"]); cid != "" {
				calls[cid] = true
			}
		}
	}
	converted := 0
	out := make([]interface{}, 0, len(input))
	for _, it := range input {
		if m, ok := it.(map[string]interface{}); ok {
			if isToolOutputItemType(streamString(m["type"])) {
				cid := streamString(m["call_id"])
				if cid == "" || !calls[cid] {
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

// isToolOutputItemType matches function_call_output / custom_tool_call_output /
// local_shell_call_output and similar tool-result items.
func isToolOutputItemType(t string) bool { return strings.HasSuffix(t, "_call_output") }

// isToolCallItemType matches function_call / custom_tool_call / local_shell_call and
// similar tool-invocation items (never an output).
func isToolCallItemType(t string) bool {
	if t == "" || strings.HasSuffix(t, "_call_output") {
		return false
	}
	return strings.HasSuffix(t, "_call")
}

func orphanedToolOutputAsUserMessage(m map[string]interface{}) map[string]interface{} {
	text := toolOutputText(m["output"])
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

// isOrphanedToolCallOutputError recognizes the upstream 400 that fires when a tool-call
// OUTPUT in the input references a call the upstream can no longer find (its server-side
// state, held by previous_response_id, is gone) — e.g. "No tool call found for custom
// tool call output with call_id ...".
func isOrphanedToolCallOutputError(status int, body []byte) bool {
	return leakfilter.IsOrphanedToolCallOutputError(status, body)
}

// responsesNeedsDegrade reports whether degradedResponsesReplay would actually change the
// request (it still carries server-side-state pointers or an orphaned tool output). It
// gates the reactive degrade-and-retry so a body that is already stateless-and-clean is
// never retried in a loop.
func responsesNeedsDegrade(body []byte) bool {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
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

// recoverOrphanedToolOutput builds the one permitted retry after the upstream says a
// tool result has no matching call. Journal replay is lossless and therefore wins;
// otherwise degradation preserves each orphan's output as a user message. Both paths
// discard every account-local state pointer before selecting a fresh account.
func (s *Server) recoverOrphanedToolOutput(ctx context.Context, body []byte, header http.Header) (codexRetryRequest, string, bool) {
	if rebuilt, ok := s.journalReplayBody(ctx, body); ok {
		return codexRetryRequest{
			Raw:       rebuilt,
			Header:    stripCodexServerStateHeaders(header),
			Recovered: true,
		}, "rebuilt", true
	}
	if !responsesRecoveryEligible(body, header) {
		return codexRetryRequest{}, "", false
	}
	return codexRetryRequest{
		Raw:       degradedResponsesReplay(body),
		Header:    stripCodexServerStateHeaders(header),
		Recovered: true,
	}, "degraded", true
}
