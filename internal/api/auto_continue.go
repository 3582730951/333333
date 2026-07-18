package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/streamrewrite"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

// Auto-continue subsystem: when a streaming response ends WITHOUT its terminal event
// (Codex response.completed / Anthropic message_stop), re-issue the request once with a
// "continue" turn and STITCH the continuation into a single coherent downstream
// response. Off by default (config StreamAutoContinueEnabled). It never fabricates model
// content: the only synthetic text is the operator-configured continue instruction, sent
// ONLY upstream. The partial output already produced is re-injected into the
// continuation request as prior turns, so the prompt-cache prefix stays intact and the
// model resumes instead of restarting.
//
// The stitch contract is "append", not "merge": the continuation's content is emitted as
// NEW content blocks (Claude) / output items (Codex) with indices offset past everything
// already relayed, so indices never collide and no already-sent bytes are rewritten.
// This is correct-by-construction for multi-block/multi-item protocols and robust to the
// continuation opening a different content type (reasoning, tool call) than the truncated
// tail.

// reissueFunc issues one continuation request upstream and returns the raw upstream SSE
// body. The wiring supplies a closure bound to the same account/lease/egress as the
// original attempt (so context affinity and prompt cache are preserved); tests supply a
// synthetic stream. The caller closes the returned reader.
type reissueFunc func(ctx context.Context, continuationBody []byte) (io.ReadCloser, error)

func (s *Server) autoContinueEnabled(ctx context.Context, decision *upstreamErrorRuleDecision) bool {
	if s.flagEnabled(ctx, "stream_auto_continue_enabled", s.cfg.StreamAutoContinueEnabled) {
		return true
	}
	// A matched rule may opt a scoped slice of traffic in even when the global switch
	// is off (e.g. only a specific model/provider).
	if decision != nil && decision.Match.AccountAction == upstreamrules.AccountActionAutoContinue {
		return true
	}
	return false
}

func (s *Server) autoContinueText(ctx context.Context) string {
	if v := strings.TrimSpace(s.settingString(ctx, "stream_continue_text", s.cfg.StreamContinueText)); v != "" {
		return v
	}
	return "Please continue from exactly where you left off, without repeating anything."
}

func (s *Server) autoContinueMaxAttempts(ctx context.Context) int {
	n := s.settingInt(ctx, "stream_auto_continue_max_attempts", s.cfg.StreamAutoContinueMaxAttempts)
	if n <= 0 {
		n = 1
	}
	if n > 3 {
		n = 3
	}
	return n
}

// forEachSSEFrame reads src and invokes fn for each complete "\n\n"-terminated SSE frame,
// then once more for any unterminated trailing bytes. It mirrors the framing every other
// relay path uses so stitched output aligns with upstream framing.
func forEachSSEFrame(src io.Reader, fn func(frame []byte) error) error {
	buf := make([]byte, 0, 32768)
	tmp := make([]byte, 32768)
	for {
		n, readErr := src.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := bytes.Index(buf, []byte("\n\n"))
				if idx < 0 {
					break
				}
				frame := append([]byte(nil), buf[:idx+2]...)
				buf = buf[idx+2:]
				if err := fn(frame); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				if len(buf) > 0 {
					return fn(buf)
				}
				return nil
			}
			return readErr
		}
	}
}

func writeSSEEvent(w io.Writer, event string, payload map[string]interface{}) error {
	payload["type"] = event
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+event+"\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data: "); err != nil {
		return err
	}
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}

func flushWriter(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// scrubbingFrameWriter applies the SAME per-frame leak filter and sensitive-word scrub
// that streamSSE applies, so a stitched continuation — which is written directly to the
// client rather than through streamSSE — never leaks pool-internal rate-limit frames or
// unscrubbed sensitive words. Every frame the stitcher emits is "\n\n"-terminated, so
// frames are processed whole.
type scrubbingFrameWriter struct {
	dst      io.Writer
	leak     bool
	words    *streamrewrite.Matcher
	provider string
	buf      []byte
}

func newScrubbingFrameWriter(dst io.Writer, leak bool, words *streamrewrite.Matcher, provider string) *scrubbingFrameWriter {
	return &scrubbingFrameWriter{dst: dst, leak: leak, words: words, provider: provider}
}

func (s *scrubbingFrameWriter) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		idx := bytes.Index(s.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), s.buf[:idx+2]...)
		s.buf = s.buf[idx+2:]
		out := frame
		if s.leak {
			out = leakfilter.NewSSEFilter(s.provider, s.words).ProcessFrameForRelay(out)
		} else if s.words != nil && !s.words.Empty() {
			out = s.words.ReplaceAll(out)
		}
		if len(out) > 0 {
			if _, err := s.dst.Write(out); err != nil {
				return 0, err
			}
		}
	}
	return len(p), nil
}

func (s *scrubbingFrameWriter) Flush() {
	if f, ok := s.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func bodyHasPreviousResponseID(body []byte) bool {
	var m map[string]interface{}
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	v, _ := m["previous_response_id"].(string)
	return strings.TrimSpace(v) != ""
}

// autoContinueDecisionFromFilter lifts a streaming response-rule filter into a decision
// so a hide_safety_buffering / intercept rule that carries account_action=auto_continue
// opts its scoped traffic into a continuation even when the global switch is off
// (workstream A#3 — "send a continue on behalf of downstream" on a safety-buffered stream).
func autoContinueDecisionFromFilter(rf *responseRuleFilter) *upstreamErrorRuleDecision {
	if rf == nil || rf.Rule == nil {
		return nil
	}
	return &upstreamErrorRuleDecision{Rule: *rf.Rule, Match: upstreamrules.MatchResult{AccountAction: rf.Rule.AccountAction}}
}

// maybeAutoContinueCodex is the wiring-facing entry: it resolves the full stateless
// context for the continuation (expanding previous_response_id via the journal, and
// declining to continue rather than break context on a journal miss) and drives the
// Responses continuation loop. A terminal-reached first stream is a no-op.
func (s *Server) maybeAutoContinueCodex(ctx context.Context, w io.Writer, originalBody []byte, rec *codexStreamLedgerRecorder, reissue reissueFunc) error {
	if rec == nil || rec.reachedTerminal() {
		return nil
	}
	resolved := originalBody
	if bodyHasPreviousResponseID(originalBody) {
		expanded, ok := s.journalReplayBody(ctx, originalBody)
		if !ok {
			// Journal miss: continuing with only the latest turn would drop the prior
			// conversation. Never break context — leave the truncated answer as-is.
			return nil
		}
		resolved = expanded
	}
	return s.autoContinueCodex(ctx, w, resolved, rec, reissue)
}

// ─────────────────────────── Claude (Anthropic Messages) ───────────────────────────

// claudeStreamTap observes an upstream Anthropic Messages SSE stream (via io.TeeReader)
// to record the partial assistant text, how many content blocks were opened, whether the
// last one is still open, and whether the terminal message_stop arrived. It never blocks
// or transforms the stream — it only records.
type claudeStreamTap struct {
	buf            []byte
	text           strings.Builder
	blocksOpened   int
	openBlockIndex int
	openBlock      bool
	messageStopped bool
}

func (t *claudeStreamTap) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		idx := bytes.Index(t.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		t.observe(t.buf[:idx+2])
		t.buf = t.buf[idx+2:]
	}
	if len(t.buf) > streamLedgerMaxPartialFrame {
		t.buf = t.buf[len(t.buf)-streamLedgerMaxPartialFrame:]
	}
	return len(p), nil
}

func (t *claudeStreamTap) observe(frame []byte) {
	eventType, data := sseFrameEventData(frame)
	if len(data) == 0 {
		return
	}
	var ev map[string]interface{}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	typ := streamString(ev["type"])
	if typ == "" {
		typ = eventType
	}
	switch typ {
	case "content_block_start":
		t.blocksOpened++
		t.openBlock = true
		t.openBlockIndex = jsonIntValue(ev["index"])
	case "content_block_delta":
		if delta, ok := ev["delta"].(map[string]interface{}); ok {
			if s := streamString(delta["text"]); s != "" {
				t.text.WriteString(s)
			}
		}
	case "content_block_stop":
		t.openBlock = false
	case "message_stop":
		t.messageStopped = true
	}
}

func (t *claudeStreamTap) reachedTerminal() bool { return t.messageStopped }
func (t *claudeStreamTap) partialText() string   { return t.text.String() }

func jsonIntValue(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// buildClaudeContinueBody appends the partial assistant answer and an operator "continue"
// user turn to the original Anthropic Messages request, so the model resumes in a fresh
// assistant turn. The stable system/tools/earlier-messages prefix is untouched, preserving
// the prompt cache. stream is forced on.
func buildClaudeContinueBody(originalBody []byte, partialText, continueText string) ([]byte, bool) {
	var root map[string]interface{}
	if json.Unmarshal(originalBody, &root) != nil {
		return nil, false
	}
	messages, _ := root["messages"].([]interface{})
	out := append([]interface{}{}, messages...)
	if strings.TrimSpace(partialText) != "" {
		out = append(out, map[string]interface{}{"role": "assistant", "content": partialText})
	}
	out = append(out, map[string]interface{}{"role": "user", "content": continueText})
	root["messages"] = out
	root["stream"] = true
	b, err := json.Marshal(root)
	return b, err == nil
}

// stitchClaudeContinuation relays continuation stream src into w as a seamless
// continuation of a first stream that already emitted priorBlocks content blocks (with
// the last still open iff priorOpen at priorOpenIndex) but no message_stop. It closes any
// still-open prior block, suppresses the continuation's own message_start, offsets every
// content-block index by priorBlocks, and relays the continuation's message_delta /
// message_stop as the true terminal. It returns the continuation's own tap so a caller can
// decide whether a further continuation is needed and with what accumulated text.
func stitchClaudeContinuation(w io.Writer, src io.Reader, priorBlocks, priorOpenIndex int, priorOpen bool) (*claudeStreamTap, error) {
	if priorOpen {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{"index": priorOpenIndex}); err != nil {
			return nil, err
		}
		flushWriter(w)
	}
	tap := &claudeStreamTap{}
	err := forEachSSEFrame(src, func(frame []byte) error {
		eventType, data := sseFrameEventData(frame)
		if len(data) == 0 {
			return nil
		}
		var ev map[string]interface{}
		if json.Unmarshal(data, &ev) != nil {
			return nil
		}
		typ := streamString(ev["type"])
		if typ == "" {
			typ = eventType
		}
		// Record raw continuation state for a possible further round.
		tap.observe(frame)
		switch typ {
		case "message_start", "ping":
			// The downstream already has message_start from the first stream; a second
			// would reset the client's message. Drop it and stray pings.
			return nil
		case "content_block_start", "content_block_delta", "content_block_stop":
			ev["index"] = jsonIntValue(ev["index"]) + priorBlocks
			if err := writeSSEEvent(w, typ, ev); err != nil {
				return err
			}
			flushWriter(w)
			return nil
		default:
			// message_delta, message_stop, error: relay verbatim as the true terminal.
			if _, err := w.Write(frame); err != nil {
				return err
			}
			flushWriter(w)
			return nil
		}
	})
	return tap, err
}

// ─────────────────────────── Codex (Responses) ───────────────────────────

// buildCodexContinueBody appends the partial assistant output items and an operator
// "continue" user turn to a resolved, stateless Responses body (full input, no
// previous_response_id). The stable prefix is untouched; stream is forced on.
func buildCodexContinueBody(resolvedBody []byte, partialItems []interface{}, continueText string) ([]byte, bool) {
	var root map[string]interface{}
	if json.Unmarshal(resolvedBody, &root) != nil {
		return nil, false
	}
	if len(partialItems) > 0 {
		root["input"] = appendItems(root["input"], partialItems)
	}
	userItem := map[string]interface{}{
		"role":    "user",
		"content": []interface{}{map[string]interface{}{"type": "input_text", "text": continueText}},
	}
	root["input"] = appendItems(root["input"], []interface{}{userItem})
	root["stream"] = true
	delete(root, "previous_response_id")
	delete(root, "turn_state")
	b, err := json.Marshal(root)
	return b, err == nil
}

// stitchCodexContinuation relays continuation stream src into w as a seamless continuation
// of a first stream that already opened priorItemCount output items (holding priorText,
// reconstructed as priorItems) but produced no response.completed. It suppresses the
// continuation's own response.created / response.in_progress, offsets every output_index by
// priorItemCount so its items append after the ones already relayed, and rewrites the final
// response.completed so its response.output = priorItems + continuation items and its
// output_text = priorText + continuation text (keeping the terminal object consistent with
// everything the client saw). It returns whether the continuation reached its terminal and
// the continuation's own recorder for a possible further round.
func stitchCodexContinuation(w io.Writer, src io.Reader, priorItems []interface{}, priorText string, priorItemCount int) (*codexStreamLedgerRecorder, error) {
	rec := newCodexStreamLedgerRecorder()
	err := forEachSSEFrame(src, func(frame []byte) error {
		rec.observeFrame(frame)
		eventType, data := sseFrameEventData(frame)
		if len(data) == 0 {
			if _, err := w.Write(frame); err != nil {
				return err
			}
			return nil
		}
		var ev map[string]interface{}
		if json.Unmarshal(data, &ev) != nil {
			if _, err := w.Write(frame); err != nil {
				return err
			}
			return nil
		}
		typ := streamString(ev["type"])
		if typ == "" {
			typ = eventType
		}
		switch typ {
		case "response.created", "response.in_progress":
			// Suppress the continuation's duplicate lifecycle preamble.
			return nil
		case "response.completed", "response.incomplete", "response.failed":
			if resp, ok := ev["response"].(map[string]interface{}); ok {
				resp["output"] = append(append([]interface{}{}, priorItems...), asSlice(resp["output"])...)
				if t := priorText + responsesRootOutputText(resp); strings.TrimSpace(t) != "" {
					resp["output_text"] = t
				}
			}
			return writeSSEEvent(w, typ, ev)
		default:
			// Content/structure events: offset output_index so items append after the
			// already-relayed ones, then relay with the same event name.
			if _, ok := ev["output_index"]; ok {
				ev["output_index"] = jsonIntValue(ev["output_index"]) + priorItemCount
			}
			if err := writeSSEEvent(w, typ, ev); err != nil {
				return err
			}
			flushWriter(w)
			return nil
		}
	})
	return rec, err
}

func asSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

func responsesRootOutputText(resp map[string]interface{}) string {
	if s := streamString(resp["output_text"]); s != "" {
		return s
	}
	return ""
}

// ─────────────────────────── Orchestrators ───────────────────────────

// autoContinueClaude drives the Anthropic continuation loop: build a continue body from
// the accumulated partial answer, re-issue, stitch, and repeat up to the configured
// attempt cap until the stream terminates. If the attempts are exhausted while the stream
// is still open, it closes the message gracefully so the client renders the partial answer
// instead of hanging — a graceful degradation, never a hard error.
func (s *Server) autoContinueClaude(ctx context.Context, w io.Writer, originalBody []byte, first *claudeStreamTap, reissue reissueFunc) error {
	maxAttempts := s.autoContinueMaxAttempts(ctx)
	continueText := s.autoContinueText(ctx)
	priorBlocks := first.blocksOpened
	priorOpen := first.openBlock
	priorOpenIndex := first.openBlockIndex
	accumulated := first.partialText()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, ok := buildClaudeContinueBody(originalBody, accumulated, continueText)
		if !ok {
			break
		}
		stream, err := reissue(ctx, body)
		if err != nil || stream == nil {
			break
		}
		tap, stitchErr := stitchClaudeContinuation(w, stream, priorBlocks, priorOpenIndex, priorOpen)
		_ = stream.Close()
		if stitchErr != nil {
			return stitchErr
		}
		if tap.reachedTerminal() {
			return nil
		}
		newOpenIndex := priorBlocks + tap.openBlockIndex
		priorBlocks += tap.blocksOpened
		priorOpen = tap.openBlock
		priorOpenIndex = newOpenIndex
		accumulated += tap.partialText()
	}
	return closeClaudeStreamGracefully(w, priorOpen, priorOpenIndex)
}

// closeClaudeStreamGracefully terminates a still-open Anthropic stream so the client
// finishes with whatever partial content it already received.
func closeClaudeStreamGracefully(w io.Writer, open bool, openIndex int) error {
	if open {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{"index": openIndex}); err != nil {
			return err
		}
	}
	if err := writeSSEEvent(w, "message_delta", map[string]interface{}{
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
	}); err != nil {
		return err
	}
	if err := writeSSEEvent(w, "message_stop", map[string]interface{}{}); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}

// autoContinueCodex drives the Responses continuation loop. resolvedBody is the stateless
// full-context body (previous_response_id already expanded and stripped). On exhaustion of
// a still-open stream it synthesizes a terminal response.completed carrying the merged
// output so the client does not hang.
func (s *Server) autoContinueCodex(ctx context.Context, w io.Writer, resolvedBody []byte, first *codexStreamLedgerRecorder, reissue reissueFunc) error {
	maxAttempts := s.autoContinueMaxAttempts(ctx)
	continueText := s.autoContinueText(ctx)
	priorItems := first.partialItems()
	priorText := first.partialText()
	priorCount := first.partialItemCount()
	id, model, _ := first.metadata()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, ok := buildCodexContinueBody(resolvedBody, priorItems, continueText)
		if !ok {
			break
		}
		stream, err := reissue(ctx, body)
		if err != nil || stream == nil {
			break
		}
		rec, stitchErr := stitchCodexContinuation(w, stream, priorItems, priorText, priorCount)
		_ = stream.Close()
		if stitchErr != nil {
			return stitchErr
		}
		if rec.reachedTerminal() {
			return nil
		}
		priorText += rec.partialText()
		priorItems = append(priorItems, rec.partialItems()...)
		priorCount += rec.partialItemCount()
		recID, recModel, _ := rec.metadata()
		if recID != "" {
			id = recID
		}
		if recModel != "" {
			model = recModel
		}
	}
	return closeCodexStreamGracefully(w, priorItems, priorText, id, model)
}

// closeCodexStreamGracefully emits a synthesized terminal response.completed carrying the
// merged output so a still-open Responses stream ends cleanly with the partial answer.
func closeCodexStreamGracefully(w io.Writer, items []interface{}, text, id, model string) error {
	resp := map[string]interface{}{
		"id":     firstNonEmpty(id, "resp_pool_autocontinue"),
		"object": "response",
		"status": "completed",
		"output": items,
	}
	if model != "" {
		resp["model"] = model
	}
	if strings.TrimSpace(text) != "" {
		resp["output_text"] = text
	}
	if err := writeSSEEvent(w, "response.completed", map[string]interface{}{"response": resp}); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}
