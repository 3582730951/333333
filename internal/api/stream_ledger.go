package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/leakfilter"
)

const (
	streamLedgerMaxPartialFrame = 256 * 1024
	streamLedgerDefaultMaxBytes = int64(1 << 30)
)

type codexStreamLedgerRecorder struct {
	mu               sync.Mutex
	ctx              context.Context
	options          bodysource.CaptureOptions
	frame            *bodysource.SpoolBuffer
	lineNonCR        bool
	pendingErr       error
	id               string
	model            string
	text             *streamLedgerText
	completed        map[string]interface{}
	terminal         string
	terminalPayload  *bodysource.SpoolBuffer
	terminalResponse bodysource.BodySource
	terminalMeta     bodysource.BodyMeta
	turnState        string
	// contextError is captured from the raw terminal SSE frame before any
	// downstream filtering. Strict CPA uses it only to retire a confirmed lost
	// previous_response epoch; ordinary missing tool output remains visible but
	// does not rotate the mapping.
	contextError leakfilter.ResponsesContextErrorKind
	// failure retains the last raw terminal failure before downstream filtering.
	// It drives late mapped-session retirement after output has already committed,
	// while the downstream receives only a neutral protocol terminal.
	failure *leakfilter.CodexFailureFrame
	added   *streamLedgerItems
	done    *streamLedgerItems
	// rateLimits holds the most recent codex.rate_limits frame's windows (workstream B:
	// real-time Codex quota captured before leakfilter drops the frame).
	rateLimits codexStreamRateLimits
}

func newCodexStreamLedgerRecorder() *codexStreamLedgerRecorder {
	return newCodexStreamLedgerRecorderWithOptions(context.Background(), bodysource.CaptureOptions{MaxBytes: streamLedgerDefaultMaxBytes, MemoryThreshold: 8 << 20})

}

func newCodexStreamLedgerRecorderWithOptions(ctx context.Context, options bodysource.CaptureOptions) *codexStreamLedgerRecorder {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = streamLedgerDefaultMaxBytes
	}
	if options.MemoryThreshold < 0 {
		options.MemoryThreshold = 0
	}
	options.TempFileNamePrefix = "codex-pool-stream-ledger-*"
	return &codexStreamLedgerRecorder{
		ctx: ctx, options: options,
		text:  newStreamLedgerText(ctx, options),
		added: newStreamLedgerItems(ctx, options, "codex-pool-stream-added-*"),
		done:  newStreamLedgerItems(ctx, options, "codex-pool-stream-done-*"),
	}
}

func (s *Server) newCodexStreamLedgerRecorder(ctx context.Context) *codexStreamLedgerRecorder {
	return newCodexStreamLedgerRecorderWithOptions(ctx, bodysource.CaptureOptions{
		MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
		MinDiskFreeBytes: s.cfg.BodyDiskReserveBytes, Budget: s.bodyBudget,
	})
}

func codexSSEToResponseJSON(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("data:")) || !bytes.Contains(raw, []byte("response.")) {
		return nil
	}
	rec := newCodexStreamLedgerRecorder()
	defer rec.Close()
	_, _ = rec.Write(raw)
	rec.finish()
	return rec.ResponseJSON()
}

func (r *codexStreamLedgerRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingErr != nil {
		return 0, r.pendingErr
	}
	start := 0
	for index, value := range p {
		if value != '\n' {
			if value != '\r' {
				r.lineNonCR = true
			}
			continue
		}
		boundary := !r.lineNonCR
		r.lineNonCR = false
		if !boundary {
			continue
		}
		if err := r.writeFrameBytesLocked(p[start : index+1]); err != nil {
			r.pendingErr = err
			return start, err
		}
		start = index + 1
		if err := r.flushFrameLocked(); err != nil {
			r.pendingErr = err
			return start, err
		}
	}
	if err := r.writeFrameBytesLocked(p[start:]); err != nil {
		r.pendingErr = err
		return start, err
	}
	return len(p), nil
}

// finish observes a final valid SSE event even when the upstream omits the
// optional trailing blank line. Truncated JSON remains unrecognized.
func (r *codexStreamLedgerRecorder) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pendingErr != nil || r.frame == nil || r.frame.Size() == 0 {
		return
	}
	if err := r.flushFrameLocked(); err != nil {
		r.pendingErr = err
	}
}

func (r *codexStreamLedgerRecorder) writeFrameBytesLocked(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if r.frame == nil {
		options := r.options
		options.TempFileNamePrefix = "codex-pool-stream-frame-*"
		frame, err := bodysource.NewSpoolBuffer(r.ctx, options)
		if err != nil {
			return err
		}
		r.frame = frame
	}
	_, err := r.frame.Write(payload)
	return err
}

func (r *codexStreamLedgerRecorder) flushFrameLocked() error {
	frame := r.frame
	r.frame = nil
	if frame == nil || frame.Size() == 0 {
		if frame != nil {
			_ = frame.Close()
		}
		return nil
	}
	defer frame.Close()
	if frame.Size() <= streamLedgerMaxPartialFrame {
		raw, err := bodysource.ReadAll(frame)
		if err != nil {
			return err
		}
		r.observeFrame(raw)
		return r.pendingErr
	}
	payload, hasData, err := extractSSEDataSource(r.ctx, frame, r.options)
	if err != nil {
		return err
	}
	if payload == nil {
		return nil
	}
	retained := false
	defer func() {
		if !retained {
			_ = payload.Close()
		}
	}()
	if !hasData || payload.Size() == 0 {
		return nil
	}
	meta, err := bodysource.ScanJSON(r.ctx, payload, nil)
	if err != nil {
		return err
	}
	retained, err = r.observeLargeEventLocked(payload, meta)
	return err
}

func (r *codexStreamLedgerRecorder) observeLargeEventLocked(payload *bodysource.SpoolBuffer, meta bodysource.BodyMeta) (bool, error) {
	typ := strings.TrimSpace(meta.Type)
	switch typ {
	case "response.created":
		r.id = firstNonEmpty(strings.TrimSpace(meta.ResponseID), r.id)
		r.model = firstNonEmpty(strings.TrimSpace(meta.ResponseModel), r.model)
	case "response.output_text.delta":
		span, ok := meta.Fields["delta"]
		if !ok || meta.Kinds["delta"] != '"' {
			return false, nil
		}
		value, err := bodysource.Slice(payload, span.Offset, span.Length)
		if err != nil {
			return false, err
		}
		return false, r.text.AppendJSONString(value)
	case "response.output_item.added", "response.output_item.done":
		span, ok := meta.Fields["item"]
		if !ok {
			return false, nil
		}
		item, err := bodysource.Slice(payload, span.Offset, span.Length)
		if err != nil {
			return false, err
		}
		if typ == "response.output_item.done" {
			return false, r.done.AppendSource(item)
		}
		return false, r.added.AppendSource(item)
	case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
		r.terminal = typ
		if r.terminalPayload != nil {
			_ = r.terminalPayload.Close()
		}
		r.terminalPayload = payload
		r.terminalResponse = nil
		r.terminalMeta = bodysource.BodyMeta{}
		if span, ok := meta.Fields["response"]; ok && meta.Kinds["response"] == '{' {
			response, err := bodysource.Slice(payload, span.Offset, span.Length)
			if err != nil {
				return false, err
			}
			responseMeta, err := bodysource.ScanJSON(r.ctx, response, nil)
			if err != nil {
				return false, err
			}
			r.terminalResponse, r.terminalMeta = response, responseMeta
			r.id = firstNonEmpty(strings.TrimSpace(responseMeta.ID), r.id)
			r.model = firstNonEmpty(strings.TrimSpace(responseMeta.Model), r.model)
			if streamLedgerCanonicalOutput(response, responseMeta) {
				r.text.Reset()
				r.added.Reset()
				r.done.Reset()
			}
		}
		return true, nil
	}
	return false, nil
}

func streamLedgerCanonicalOutput(response bodysource.BodySource, meta bodysource.BodyMeta) bool {
	span, ok := meta.Fields["output"]
	if !ok || meta.Kinds["output"] != '[' || span.Length <= 2 {
		return false
	}
	view, err := bodysource.Slice(response, span.Offset, min64(span.Length, 64))
	if err != nil {
		return false
	}
	raw, err := bodysource.ReadAll(view)
	return err == nil && len(bytes.TrimSpace(raw)) > 2
}

func min64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func (r *codexStreamLedgerRecorder) ResponseJSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var root map[string]interface{}
	if r.completed != nil {
		root = make(map[string]interface{}, len(r.completed)+4)
		for key, value := range r.completed {
			root[key] = value
		}
	} else if r.terminalResponse != nil {
		raw, err := bodysource.ReadAll(r.terminalResponse)
		if err != nil || json.Unmarshal(raw, &root) != nil {
			return nil
		}
	} else if r.terminalPayload != nil {
		raw, err := bodysource.ReadAll(r.terminalPayload)
		if err != nil || json.Unmarshal(raw, &root) != nil {
			return nil
		}
	} else {
		root = map[string]interface{}{}
	}
	if r.id != "" {
		if _, ok := root["id"]; !ok {
			root["id"] = r.id
		}
	}
	if r.model != "" {
		if _, ok := root["model"]; !ok {
			root["model"] = r.model
		}
	}
	if _, ok := root["object"]; !ok {
		root["object"] = "response"
	}
	if _, ok := root["status"]; !ok {
		root["status"] = "completed"
	}
	text := r.currentTextLocked()
	if !hasNonEmptyArray(root["output"]) {
		if output := r.outputItems(text); len(output) > 0 {
			root["output"] = output
		}
	}
	if text != "" {
		if _, ok := root["output_text"]; !ok {
			root["output_text"] = text
		}
	}
	status := strings.ToLower(streamString(root["status"]))
	if !hasNonEmptyArray(root["output"]) && strings.TrimSpace(streamString(root["output_text"])) == "" &&
		status != "failed" && status != "incomplete" && r.id == "" && r.completed == nil && r.terminalResponse == nil && r.terminalPayload == nil {
		return nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil
	}
	return out
}

// reachedTerminal reports whether the stream observed a terminal Responses event,
// including an explicit error event. When false after the upstream body ends, the
// stream was truncated and may be eligible for a continuation.
func (r *codexStreamLedgerRecorder) reachedTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal != "" || r.completed != nil
}

// completedSuccessfully distinguishes a real response.completed from every failure
// or incomplete terminal. Only response.completed may advance a goal checkpoint as
// a successful turn.
func (r *codexStreamLedgerRecorder) completedSuccessfully() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal == "response.completed"
}

func (r *codexStreamLedgerRecorder) terminalContextError() leakfilter.ResponsesContextErrorKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contextError
}

func (r *codexStreamLedgerRecorder) terminalFailure() (leakfilter.CodexFailureFrame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == nil {
		return leakfilter.CodexFailureFrame{}, false
	}
	failure := *r.failure
	failure.Header = r.failure.Header.Clone()
	failure.Body = append([]byte(nil), r.failure.Body...)
	return failure, true
}

// responseTurnState returns the opaque upstream continuation token observed in a
// response.metadata SSE event. WebSocket Responses transports carry this value in
// the event headers rather than the HTTP response headers, so it must be retained
// separately from the synthesized terminal response JSON.
func (r *codexStreamLedgerRecorder) responseTurnState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turnState
}

// partialText returns the assistant output text accumulated so far.
func (r *codexStreamLedgerRecorder) partialText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.currentTextLocked()
}

// partialItems reconstructs the assistant output items produced so far (the same shape
// persistContextJournal appends as input for the next turn), used to re-inject the
// partial answer into the continuation request so the model continues instead of
// restarting — keeping the prompt-cache prefix intact.
func (r *codexStreamLedgerRecorder) partialItems() []interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outputItems(r.currentTextLocked())
}

// partialItemCount is the number of output items the stream opened, used to offset the
// continuation's output_index so its items never collide with the ones already relayed.
func (r *codexStreamLedgerRecorder) partialItemCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := r.done.Count(); n > r.added.Count() {
		return n
	}
	return r.added.Count()
}

// metadata returns a consistent copy of the fields consumed after streaming. The
// heartbeat relay reads upstream data on a helper goroutine; a downstream disconnect
// can make streamSSE return while that goroutine is completing its final recorder
// write, so direct field reads would race even though normal EOF is fully drained.
func (r *codexStreamLedgerRecorder) metadata() (string, string, codexStreamRateLimits) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.id, r.model, r.rateLimits
}

func (r *codexStreamLedgerRecorder) observeFrame(frame []byte) {
	if failure, ok := leakfilter.ParseCodexFailureFrame(frame); ok {
		copyFailure := failure
		copyFailure.Header = failure.Header.Clone()
		copyFailure.Body = append([]byte(nil), failure.Body...)
		r.failure = &copyFailure
		if failure.ContextError != leakfilter.ResponsesContextErrorNone {
			r.contextError = failure.ContextError
		}
	}
	eventType, data := sseFrameEventData(frame)
	if len(data) == 0 || strings.TrimSpace(string(data)) == "[DONE]" {
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
	case "response.metadata":
		r.turnState = firstNonEmpty(r.turnState, codexSSEHeaderValue(ev, "x-codex-turn-state"))
	case "response.created":
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			if id := strings.TrimSpace(streamString(resp["id"])); id != "" {
				r.id = id
			}
			if model := strings.TrimSpace(streamString(resp["model"])); model != "" {
				r.model = model
			}
		}
	case "response.output_text.delta":
		if delta := streamString(ev["delta"]); delta != "" {
			if err := r.text.WriteString(delta); err != nil {
				r.pendingErr = err
			}
		}
	case "response.output_item.added":
		if item := ev["item"]; item != nil {
			if err := r.added.Append(item); err != nil {
				r.pendingErr = err
			}
		}
	case "response.output_item.done":
		if item := ev["item"]; item != nil {
			if err := r.done.Append(item); err != nil {
				r.pendingErr = err
			}
		}
	case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
		r.terminal = typ
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			r.completed = resp
			if id := strings.TrimSpace(streamString(resp["id"])); id != "" {
				r.id = id
			}
			if model := strings.TrimSpace(streamString(resp["model"])); model != "" {
				r.model = model
			}
		} else {
			// A terminal event without a response object is still a real protocol
			// terminal. Treat it as such so EOF compensation does not append a second,
			// misleading "stream closed before response.completed" failure.
			r.completed = ev
			switch typ {
			case "response.completed":
				r.completed["status"] = "completed"
			case "response.incomplete":
				r.completed["status"] = "incomplete"
			default:
				r.completed["status"] = "failed"
			}
		}
		if hasNonEmptyArray(r.completed["output"]) {
			r.added.Reset()
			r.done.Reset()
		}
		if completedResponseText(r.completed) != "" {
			r.text.Reset()
		}
	case "codex.rate_limits":
		if rl, ok := parseCodexRateLimitsEvent(ev); ok {
			r.rateLimits = rl
		}
	}
}

func codexSSEHeaderValue(event map[string]interface{}, name string) string {
	headers, _ := event["headers"].(map[string]interface{})
	for key, value := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		text, _ := value.(string)
		return strings.TrimSpace(text)
	}
	return ""
}

func (r *codexStreamLedgerRecorder) outputItems(text string) []interface{} {
	var items []interface{}
	if r.completed != nil && hasNonEmptyArray(r.completed["output"]) {
		items = cloneJSONArray(asSlice(r.completed["output"]))
	} else if r.terminalResponse != nil {
		raw, err := bodysource.ReadAll(r.terminalResponse)
		var response map[string]interface{}
		if err == nil && json.Unmarshal(raw, &response) == nil && hasNonEmptyArray(response["output"]) {
			items = cloneJSONArray(asSlice(response["output"]))
		}
	} else if r.done.Count() > 0 {
		items = r.done.Values()
	} else if r.added.Count() > 0 {
		items = r.added.Values()
	}
	if strings.TrimSpace(text) != "" {
		for _, rawItem := range items {
			item, _ := rawItem.(map[string]interface{})
			if streamString(item["type"]) != "message" {
				continue
			}
			if strings.TrimSpace(responsesOutputItemText(item)) == "" {
				item["content"] = []interface{}{map[string]interface{}{"type": "output_text", "text": text}}
			}
			return items
		}
		message := map[string]interface{}{
			"type": "message",
			"role": "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text",
				"text": text,
			}},
		}
		// Responses ordering is reasoning -> assistant message -> function calls.
		// Insert the delta-only message before the first non-reasoning item.
		insertAt := len(items)
		for index, rawItem := range items {
			item, _ := rawItem.(map[string]interface{})
			if streamString(item["type"]) != "reasoning" {
				insertAt = index
				break
			}
		}
		items = append(items, nil)
		copy(items[insertAt+1:], items[insertAt:])
		items[insertAt] = message
	}
	return items
}

func (r *codexStreamLedgerRecorder) currentTextLocked() string {
	if text := completedResponseText(r.completed); text != "" {
		return text
	}
	if r.terminalResponse != nil {
		raw, err := bodysource.ReadAll(r.terminalResponse)
		var response map[string]interface{}
		if err == nil && json.Unmarshal(raw, &response) == nil {
			if text := completedResponseText(response); text != "" {
				return text
			}
		}
	}
	return r.text.String()
}

func (r *codexStreamLedgerRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var closeErr error
	if r.frame != nil {
		closeErr = errors.Join(closeErr, r.frame.Close())
		r.frame = nil
	}
	if r.terminalPayload != nil {
		closeErr = errors.Join(closeErr, r.terminalPayload.Close())
		r.terminalPayload, r.terminalResponse = nil, nil
	}
	closeErr = errors.Join(closeErr, r.text.Close(), r.added.Close(), r.done.Close())
	return closeErr
}

type streamLedgerText struct {
	ctx     context.Context
	options bodysource.CaptureOptions
	buffer  *bodysource.SpoolBuffer
}

func newStreamLedgerText(ctx context.Context, options bodysource.CaptureOptions) *streamLedgerText {
	return &streamLedgerText{ctx: ctx, options: options}
}

func (t *streamLedgerText) ensure() error {
	if t.buffer != nil {
		return nil
	}
	options := t.options
	options.TempFileNamePrefix = "codex-pool-stream-text-*"
	buffer, err := bodysource.NewSpoolBuffer(t.ctx, options)
	if err != nil {
		return err
	}
	t.buffer = buffer
	return nil
}

func (t *streamLedgerText) WriteString(value string) error {
	if value == "" {
		return nil
	}
	if err := t.ensure(); err != nil {
		return err
	}
	_, err := io.WriteString(t.buffer, value)
	return err
}

func (t *streamLedgerText) AppendJSONString(source bodysource.BodySource) error {
	if err := t.ensure(); err != nil {
		return err
	}
	return decodeJSONStringSource(source, t.buffer)
}

func (t *streamLedgerText) Len() int {
	if t == nil || t.buffer == nil {
		return 0
	}
	size := t.buffer.Size()
	if size > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(size)
}

func (t *streamLedgerText) String() string {
	if t == nil || t.buffer == nil || t.buffer.Size() == 0 {
		return ""
	}
	raw, err := bodysource.ReadAll(t.buffer)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (t *streamLedgerText) Reset() {
	if t == nil || t.buffer == nil {
		return
	}
	_ = t.buffer.Close()
	t.buffer = nil
}

func (t *streamLedgerText) Close() error {
	if t == nil || t.buffer == nil {
		return nil
	}
	err := t.buffer.Close()
	t.buffer = nil
	return err
}

type streamLedgerItems struct {
	ctx     context.Context
	options bodysource.CaptureOptions
	prefix  string
	buffer  *bodysource.SpoolBuffer
	count   int
}

func newStreamLedgerItems(ctx context.Context, options bodysource.CaptureOptions, prefix string) *streamLedgerItems {
	return &streamLedgerItems{ctx: ctx, options: options, prefix: prefix}
}

func (i *streamLedgerItems) ensure() error {
	if i.buffer != nil {
		return nil
	}
	options := i.options
	options.TempFileNamePrefix = i.prefix
	buffer, err := bodysource.NewSpoolBuffer(i.ctx, options)
	if err != nil {
		return err
	}
	i.buffer = buffer
	return nil
}

func (i *streamLedgerItems) Append(value interface{}) error {
	if err := i.ensure(); err != nil {
		return err
	}
	if err := json.NewEncoder(i.buffer).Encode(value); err != nil {
		return err
	}
	i.count++
	return nil
}

func (i *streamLedgerItems) AppendSource(source bodysource.BodySource) error {
	if err := i.ensure(); err != nil {
		return err
	}
	reader, err := source.Open()
	if err != nil {
		return err
	}
	_, copyErr := io.CopyBuffer(i.buffer, reader, make([]byte, bodysource.DefaultChunkSize))
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if _, err = i.buffer.Write([]byte{'\n'}); err != nil {
		return err
	}
	i.count++
	return nil
}

func (i *streamLedgerItems) Count() int {
	if i == nil {
		return 0
	}
	return i.count
}

func (i *streamLedgerItems) Values() []interface{} {
	if i == nil || i.buffer == nil || i.count == 0 {
		return nil
	}
	reader, err := i.buffer.Open()
	if err != nil {
		return nil
	}
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	values := make([]interface{}, 0, i.count)
	for {
		var value interface{}
		if err = decoder.Decode(&value); errors.Is(err, io.EOF) {
			return values
		} else if err != nil {
			return nil
		}
		values = append(values, value)
	}
}

func (i *streamLedgerItems) Reset() {
	if i == nil {
		return
	}
	if i.buffer != nil {
		_ = i.buffer.Close()
	}
	i.buffer, i.count = nil, 0
}

func (i *streamLedgerItems) Close() error {
	if i == nil || i.buffer == nil {
		return nil
	}
	err := i.buffer.Close()
	i.buffer, i.count = nil, 0
	return err
}

func decodeJSONStringSource(source bodysource.BodySource, dst io.Writer) error {
	reader, err := source.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	buffered := bufio.NewReaderSize(reader, bodysource.DefaultChunkSize)
	writer := bufio.NewWriterSize(dst, bodysource.DefaultChunkSize)
	defer writer.Flush()
	first, err := readJSONNonSpace(buffered)
	if err != nil {
		return err
	}
	if first != '"' {
		return fmt.Errorf("stream delta is not a JSON string")
	}
	for {
		value, readErr := buffered.ReadByte()
		if readErr != nil {
			return readErr
		}
		switch value {
		case '"':
			for {
				tail, tailErr := buffered.ReadByte()
				if errors.Is(tailErr, io.EOF) {
					return writer.Flush()
				}
				if tailErr != nil {
					return tailErr
				}
				if !isJSONSpace(tail) {
					return fmt.Errorf("unexpected data after JSON string")
				}
			}
		case '\\':
			escaped, escapeErr := buffered.ReadByte()
			if escapeErr != nil {
				return escapeErr
			}
			switch escaped {
			case '"', '\\', '/':
				err = writer.WriteByte(escaped)
			case 'b':
				err = writer.WriteByte('\b')
			case 'f':
				err = writer.WriteByte('\f')
			case 'n':
				err = writer.WriteByte('\n')
			case 'r':
				err = writer.WriteByte('\r')
			case 't':
				err = writer.WriteByte('\t')
			case 'u':
				code, codeErr := readJSONHexRune(buffered)
				if codeErr != nil {
					return codeErr
				}
				r := rune(code)
				if utf16.IsSurrogate(r) {
					marker := make([]byte, 2)
					if _, codeErr = io.ReadFull(buffered, marker); codeErr != nil || marker[0] != '\\' || marker[1] != 'u' {
						return fmt.Errorf("invalid JSON surrogate pair")
					}
					low, lowErr := readJSONHexRune(buffered)
					if lowErr != nil {
						return lowErr
					}
					r = utf16.DecodeRune(r, rune(low))
					if r == utf8.RuneError {
						return fmt.Errorf("invalid JSON surrogate pair")
					}
				}
				_, err = writer.WriteRune(r)
			default:
				return fmt.Errorf("invalid JSON string escape")
			}
			if err != nil {
				return err
			}
		default:
			if value < 0x20 {
				return fmt.Errorf("invalid control byte in JSON string")
			}
			if err = writer.WriteByte(value); err != nil {
				return err
			}
		}
	}
}

func readJSONNonSpace(reader *bufio.Reader) (byte, error) {
	for {
		value, err := reader.ReadByte()
		if err != nil || !isJSONSpace(value) {
			return value, err
		}
	}
}

func isJSONSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func readJSONHexRune(reader *bufio.Reader) (uint16, error) {
	var raw [4]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(string(raw[:]), 16, 16)
	if err != nil {
		return 0, fmt.Errorf("invalid JSON unicode escape: %w", err)
	}
	return uint16(value), nil
}

func completedResponseText(response map[string]interface{}) string {
	if response == nil {
		return ""
	}
	if text := streamString(response["output_text"]); text != "" {
		return text
	}
	var text strings.Builder
	for _, rawItem := range asSlice(response["output"]) {
		item, _ := rawItem.(map[string]interface{})
		if streamString(item["type"]) == "message" {
			text.WriteString(responsesOutputItemText(item))
		}
	}
	return text.String()
}

func sseFrameEventData(frame []byte) (string, []byte) {
	var eventType string
	var dataLines []string
	for _, rawLine := range bytes.Split(frame, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimPrefix(line, "data:")
			if strings.HasPrefix(data, " ") {
				data = data[1:]
			}
			dataLines = append(dataLines, data)
		}
	}
	if len(dataLines) == 0 {
		return eventType, nil
	}
	return eventType, []byte(strings.Join(dataLines, "\n"))
}

func cloneJSONValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out interface{}
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func cloneJSONMap(v map[string]interface{}) map[string]interface{} {
	cloned, _ := cloneJSONValue(v).(map[string]interface{})
	if cloned == nil {
		return map[string]interface{}{}
	}
	return cloned
}

func cloneJSONArray(v []interface{}) []interface{} {
	out := make([]interface{}, 0, len(v))
	for _, item := range v {
		if cloned := cloneJSONValue(item); cloned != nil {
			out = append(out, cloned)
		}
	}
	return out
}

func hasNonEmptyArray(v interface{}) bool {
	arr, ok := v.([]interface{})
	return ok && len(arr) > 0
}

func streamString(v interface{}) string {
	s, _ := v.(string)
	return s
}
