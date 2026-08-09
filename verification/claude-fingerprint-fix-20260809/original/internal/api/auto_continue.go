package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
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

// errAutoContinueUnavailable means no lossless continuation request could be built.
// Callers must synthesize a protocol failure terminal rather than silently EOF.
var errAutoContinueUnavailable = errors.New("lossless auto-continue context unavailable")

func appendBoundedStreamText(base, extra string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultStreamAccumulatorMaxBytes
	}
	if int64(len(extra)) > maxBytes-int64(len(base)) {
		return "", bodysource.ErrBodyTooLarge
	}
	return base + extra, nil
}

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

// forEachSSEFrame reads src and invokes fn for each complete LF- or CRLF-delimited
// SSE frame, then once more for any unterminated trailing bytes.
func forEachSSEFrame(src io.Reader, fn func(frame []byte) error) error {
	return forEachSSEFrameWithOptions(context.Background(), src, bodysource.CaptureOptions{}, fn)
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
	originalLen := len(p)
	if len(s.buf) == 0 {
		for {
			boundary, separatorLen := sseFrameBoundary(p)
			if boundary < 0 {
				break
			}
			frameEnd := boundary + separatorLen
			if err := s.relay(p[:frameEnd]); err != nil {
				return 0, err
			}
			p = p[frameEnd:]
		}
		if len(p) == 0 {
			return originalLen, nil
		}
	}
	if int64(len(p)) > defaultStreamAccumulatorMaxBytes-int64(len(s.buf)) {
		return 0, bodysource.ErrBodyTooLarge
	}
	s.buf = append(s.buf, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(s.buf)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		frame := s.buf[:frameEnd]
		if err := s.relay(frame); err != nil {
			return 0, err
		}
		s.buf = s.buf[frameEnd:]
		if len(s.buf) == 0 {
			s.buf = nil
		}
	}
	return originalLen, nil
}

func (s *scrubbingFrameWriter) relay(frame []byte) error {
	out := frame
	if s.leak {
		out = leakfilter.NewSSEFilter(s.provider, s.words).ProcessFrameForRelay(out)
	} else {
		if s.provider == "codex" {
			out, _ = leakfilter.NeutralizeResponsesContextErrorSSEFrame(out)
		}
		if s.words != nil && !s.words.Empty() {
			out = s.words.ReplaceAll(out)
		}
	}
	if len(out) == 0 {
		return nil
	}
	_, err := s.dst.Write(out)
	return err
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
func (s *Server) maybeAutoContinueCodex(ctx context.Context, w io.Writer, originalBody []byte, rec *codexStreamLedgerRecorder, reissue reissueFunc) (*codexStreamLedgerRecorder, error) {
	if rec == nil || rec.reachedTerminal() {
		return rec, nil
	}
	if hasPendingClientToolCall(rec.partialItems()) {
		// A model-emitted client tool call cannot be replayed as assistant history
		// until the downstream supplies its matching output. Reissuing here would
		// either duplicate the tool or produce a missing-tool-output failure.
		return nil, errAutoContinueUnavailable
	}
	resolved := originalBody
	if bodyHasPreviousResponseID(originalBody) {
		expanded, ok := s.journalReplayBody(ctx, originalBody)
		if !ok {
			// Journal miss: continuing with only the latest turn would drop the prior
			// conversation. Do not send it upstream; the caller will make the failure
			// visible instead of leaving the client at a silent EOF.
			return nil, errAutoContinueUnavailable
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
	ctx            context.Context
	options        bodysource.CaptureOptions
	text           *streamAccumulator
	err            error
	blocksOpened   int
	openBlockIndex int
	openBlock      bool
	messageStopped bool
	terminalError  bool
}

func newClaudeStreamTap(ctx context.Context, options bodysource.CaptureOptions) *claudeStreamTap {
	return &claudeStreamTap{ctx: ctx, options: options}
}

func (t *claudeStreamTap) ensureText() *streamAccumulator {
	if t.text == nil {
		t.text = newStreamAccumulator(t.ctx, t.options, "codex-pool-claude-partial-text-*")
	}
	return t.text
}

func (t *claudeStreamTap) Write(p []byte) (int, error) {
	if t.err != nil {
		return 0, t.err
	}
	t.buf = append(t.buf, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(t.buf)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		t.observe(t.buf[:frameEnd])
		if t.err != nil {
			return 0, t.err
		}
		t.buf = t.buf[frameEnd:]
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
				t.err = t.ensureText().WriteString(s)
			}
		}
	case "content_block_stop":
		t.openBlock = false
	case "message_stop":
		t.messageStopped = true
	case "error":
		// An Anthropic error event is already a terminal event. Treating it as a
		// truncated EOF would reissue the turn after output has committed, duplicate
		// content/tool effects, and append a second synthetic error downstream.
		t.terminalError = true
	}
}

func (t *claudeStreamTap) reachedTerminal() bool       { return t.messageStopped || t.terminalError }
func (t *claudeStreamTap) completedSuccessfully() bool { return t.messageStopped && !t.terminalError }
func (t *claudeStreamTap) partialText() string {
	if t.text == nil {
		return ""
	}
	value, err := t.text.String()
	if err != nil {
		t.err = err
		return ""
	}
	return value
}
func (t *claudeStreamTap) Close() error {
	if t.text == nil {
		return nil
	}
	return t.text.Close()
}

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
func stitchClaudeContinuation(w io.Writer, src io.Reader, priorBlocks, priorOpenIndex int, priorOpen bool, options ...bodysource.CaptureOptions) (*claudeStreamTap, error) {
	if priorOpen {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{"index": priorOpenIndex}); err != nil {
			return nil, err
		}
		flushWriter(w)
	}
	var captureOptions bodysource.CaptureOptions
	if len(options) > 0 {
		captureOptions = options[0]
	}
	tap := newClaudeStreamTap(context.Background(), captureOptions)
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
		if tap.err != nil {
			return tap.err
		}
		switch typ {
		case "message_start":
			// The downstream already has message_start from the first stream; a second
			// would reset the client's message.
			return nil
		case "ping":
			// Preserve Anthropic's protocol-native heartbeat while the continuation is
			// thinking. It carries no message identity or model content.
			if _, err := w.Write(frame); err != nil {
				return err
			}
			flushWriter(w)
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
	return stitchCodexContinuationWithRecorder(w, src, priorItems, priorText, priorItemCount, newCodexStreamLedgerRecorder())
}

func stitchCodexContinuationWithRecorder(w io.Writer, src io.Reader, priorItems []interface{}, priorText string, priorItemCount int, rec *codexStreamLedgerRecorder, limits ...int64) (*codexStreamLedgerRecorder, error) {
	maxBytes := defaultStreamAccumulatorMaxBytes
	if len(limits) > 0 && limits[0] > 0 {
		maxBytes = limits[0]
	}
	err := forEachSSEFrame(src, func(frame []byte) error {
		eventType, data := sseFrameEventData(frame)
		if len(data) == 0 {
			rec.observeFrame(frame)
			if _, err := w.Write(frame); err != nil {
				return err
			}
			return nil
		}
		var ev map[string]interface{}
		if json.Unmarshal(data, &ev) != nil {
			rec.observeFrame(frame)
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
		case "response.created":
			rec.observeFrame(frame)
			// Suppress the continuation's duplicate lifecycle preamble.
			return nil
		case "response.in_progress":
			rec.observeFrame(frame)
			// A minimal in_progress event is the relay's downstream heartbeat. It has
			// no replacement response id and is safe to preserve while a continuation
			// is thinking. Suppress upstream lifecycle snapshots with extra fields.
			if len(ev) == 1 {
				return writeSSEEvent(w, typ, ev)
			}
			return nil
		case "response.completed", "response.incomplete", "response.failed":
			if resp, ok := ev["response"].(map[string]interface{}); ok {
				resp["output"] = append(append([]interface{}{}, priorItems...), asSlice(resp["output"])...)
				t, appendErr := appendBoundedStreamText(priorText, responsesRootOutputText(resp), maxBytes)
				if appendErr != nil {
					return appendErr
				}
				if strings.TrimSpace(t) != "" {
					resp["output_text"] = t
				}
			}
			var rewritten bytes.Buffer
			if err := writeSSEEvent(&rewritten, typ, ev); err != nil {
				return err
			}
			rec.observeFrame(rewritten.Bytes())
			_, err := w.Write(rewritten.Bytes())
			return err
		default:
			rec.observeFrame(frame)
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

// autoContinueClaude drives the Anthropic continuation loop.  The returned tap is
// non-nil only when a real upstream message_stop arrived; callers use that signal to
// commit the merged stream to the durable goal chain.  A nil tap with nil error means
// this function has already emitted the native error terminal after the bounded retry.
// capture, when non-nil, receives the raw continuation events for encrypted replay.
func (s *Server) autoContinueClaude(ctx context.Context, w io.Writer, originalBody []byte, first *claudeStreamTap, reissue reissueFunc, capture io.Writer) (*claudeStreamTap, error) {
	if capture == nil {
		capture = io.Discard
	}
	maxAttempts := s.autoContinueMaxAttempts(ctx)
	continueText := s.autoContinueText(ctx)
	maxBytes := s.cfg.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = defaultStreamAccumulatorMaxBytes
	}
	priorBlocks := first.blocksOpened
	priorOpen := first.openBlock
	priorOpenIndex := first.openBlockIndex
	accumulated := first.partialText()
	if first.err != nil {
		return nil, first.err
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, ok := buildClaudeContinueBody(originalBody, accumulated, continueText)
		if !ok {
			break
		}
		if int64(len(body)) > maxBytes {
			return nil, bodysource.ErrBodyTooLarge
		}
		stream, err := reissue(ctx, body)
		if err != nil || stream == nil {
			break
		}
		tap, stitchErr := stitchClaudeContinuation(w, io.TeeReader(stream, capture), priorBlocks, priorOpenIndex, priorOpen, first.options)
		_ = stream.Close()
		if stitchErr != nil {
			if tap == nil {
				return nil, stitchErr
			}
			// stitchClaudeContinuation closes the prior block before it starts the
			// continuation. If the new upstream stream then stalls or breaks, terminate
			// using the new block's offset index here. Returning nil,nil means the
			// protocol terminal has already been emitted and prevents the caller from
			// closing the old block a second time.
			openIndex := priorBlocks + tap.openBlockIndex
			if terminalErr := closeClaudeStreamGracefully(w, tap.openBlock, openIndex); terminalErr != nil {
				return tap, errors.Join(stitchErr, terminalErr)
			}
			return nil, nil
		}
		if tap.reachedTerminal() {
			return tap, nil
		}
		newOpenIndex := priorBlocks + tap.openBlockIndex
		priorBlocks += tap.blocksOpened
		priorOpen = tap.openBlock
		priorOpenIndex = newOpenIndex
		var appendErr error
		accumulated, appendErr = appendBoundedStreamText(accumulated, tap.partialText(), maxBytes)
		if tap.err != nil {
			_ = tap.Close()
			return nil, tap.err
		}
		if appendErr != nil {
			_ = tap.Close()
			return nil, appendErr
		}
		_ = tap.Close()
	}
	if err := closeClaudeStreamGracefully(w, priorOpen, priorOpenIndex); err != nil {
		return nil, err
	}
	return nil, nil
}

// closeClaudeStreamGracefully terminates a truncated Anthropic stream with its native
// error event.  The previous synthetic end_turn made a missing upstream terminal look
// successful and left long-running clients unable to know they must resume.
func closeClaudeStreamGracefully(w io.Writer, open bool, openIndex int) error {
	if open {
		if err := writeSSEEvent(w, "content_block_stop", map[string]interface{}{"index": openIndex}); err != nil {
			return err
		}
	}
	if err := writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"code":    "server_error",
			"message": publicRetryMessage,
		},
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
// full-context body (previous_response_id already expanded and stripped).  A non-nil
// recorder means the continuation reached a real upstream terminal and can be committed
// as the new durable response.  A nil recorder with a nil error means an explicit
// response.failed was already emitted after the bounded retry was exhausted.
func (s *Server) autoContinueCodex(ctx context.Context, w io.Writer, resolvedBody []byte, first *codexStreamLedgerRecorder, reissue reissueFunc) (*codexStreamLedgerRecorder, error) {
	maxAttempts := s.autoContinueMaxAttempts(ctx)
	continueText := s.autoContinueText(ctx)
	maxBytes := s.cfg.MaxBodyBytes
	if maxBytes <= 0 {
		maxBytes = defaultStreamAccumulatorMaxBytes
	}
	priorItems := first.partialItems()
	priorText := first.partialText()
	priorCount := first.partialItemCount()
	id, model, _ := first.metadata()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, ok := buildCodexContinueBody(resolvedBody, priorItems, continueText)
		if !ok {
			break
		}
		if int64(len(body)) > maxBytes {
			return nil, bodysource.ErrBodyTooLarge
		}
		stream, err := reissue(ctx, body)
		if err != nil || stream == nil {
			break
		}
		rec, stitchErr := stitchCodexContinuationWithRecorder(w, stream, priorItems, priorText, priorCount, s.newCodexStreamLedgerRecorder(ctx), maxBytes)
		_ = stream.Close()
		if stitchErr != nil {
			_ = rec.Close()
			return nil, stitchErr
		}
		if rec.reachedTerminal() {
			return rec, nil
		}
		var appendErr error
		priorText, appendErr = appendBoundedStreamText(priorText, rec.partialText(), maxBytes)
		if appendErr != nil {
			_ = rec.Close()
			return nil, appendErr
		}
		priorItems = append(priorItems, rec.partialItems()...)
		priorCount += rec.partialItemCount()
		recID, recModel, _ := rec.metadata()
		if recID != "" {
			id = recID
		}
		if recModel != "" {
			model = recModel
		}
		_ = rec.Close()
	}
	if err := closeCodexStreamGracefully(w, priorItems, priorText, id, model); err != nil {
		return nil, err
	}
	return nil, nil
}

// closeCodexStreamGracefully emits an explicit failure terminal for a stream whose
// upstream ended without a terminal event.  Partial text may already be visible, but
// declaring it completed would make clients commit an unknowingly truncated result.
func closeCodexStreamGracefully(w io.Writer, items []interface{}, text, id, model string) error {
	_ = items
	_ = text
	resp := map[string]interface{}{
		"id":     firstNonEmpty(id, "resp_pool_autocontinue"),
		"object": "response",
		"status": "failed",
		"error": map[string]interface{}{
			"code":    "server_error",
			"message": publicRetryMessage,
		},
	}
	if model != "" {
		resp["model"] = model
	}
	if err := writeSSEEvent(w, "response.failed", map[string]interface{}{"type": "response.failed", "response": resp}); err != nil {
		return err
	}
	flushWriter(w)
	return nil
}
