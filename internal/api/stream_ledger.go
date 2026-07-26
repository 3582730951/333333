package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"codex-account-pool/internal/leakfilter"
)

const streamLedgerMaxPartialFrame = 256 * 1024

type codexStreamLedgerRecorder struct {
	mu        sync.Mutex
	buf       []byte
	id        string
	model     string
	text      strings.Builder
	completed map[string]interface{}
	terminal  string
	turnState string
	// contextError is captured from the raw terminal SSE frame before any
	// downstream filtering. Strict CPA uses it only to retire a confirmed lost
	// previous_response epoch; ordinary missing tool output remains visible but
	// does not rotate the mapping.
	contextError leakfilter.ResponsesContextErrorKind
	// failure retains the last raw terminal failure before downstream filtering.
	// It drives late mapped-session retirement after output has already committed,
	// while the downstream receives only a neutral protocol terminal.
	failure *leakfilter.CodexFailureFrame
	added   []interface{}
	done    []interface{}
	// rateLimits holds the most recent codex.rate_limits frame's windows (workstream B:
	// real-time Codex quota captured before leakfilter drops the frame).
	rateLimits codexStreamRateLimits
}

func newCodexStreamLedgerRecorder() *codexStreamLedgerRecorder {
	return &codexStreamLedgerRecorder{}
}

func codexSSEToResponseJSON(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("data:")) || !bytes.Contains(raw, []byte("response.")) {
		return nil
	}
	rec := newCodexStreamLedgerRecorder()
	_, _ = rec.Write(raw)
	return rec.ResponseJSON()
}

func (r *codexStreamLedgerRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(r.buf)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		frame := r.buf[:frameEnd]
		r.observeFrame(frame)
		r.buf = r.buf[frameEnd:]
	}
	if len(r.buf) > streamLedgerMaxPartialFrame {
		r.buf = r.buf[len(r.buf)-streamLedgerMaxPartialFrame:]
	}
	return len(p), nil
}

// finish observes a final valid SSE event even when the upstream omits the
// optional trailing blank line. Truncated JSON remains unrecognized.
func (r *codexStreamLedgerRecorder) finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(bytes.TrimSpace(r.buf)) == 0 {
		r.buf = nil
		return
	}
	r.observeFrame(r.buf)
	r.buf = nil
}

func (r *codexStreamLedgerRecorder) ResponseJSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var root map[string]interface{}
	if r.completed != nil {
		root = cloneJSONMap(r.completed)
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
	text := r.text.String()
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
		status != "failed" && status != "incomplete" && r.id == "" && r.completed == nil {
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
	return r.completed != nil
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
	return r.text.String()
}

// partialItems reconstructs the assistant output items produced so far (the same shape
// persistContextJournal appends as input for the next turn), used to re-inject the
// partial answer into the continuation request so the model continues instead of
// restarting — keeping the prompt-cache prefix intact.
func (r *codexStreamLedgerRecorder) partialItems() []interface{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outputItems(r.text.String())
}

// partialItemCount is the number of output items the stream opened, used to offset the
// continuation's output_index so its items never collide with the ones already relayed.
func (r *codexStreamLedgerRecorder) partialItemCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.done); n > len(r.added) {
		return n
	}
	return len(r.added)
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
			r.text.WriteString(delta)
		}
	case "response.output_item.added":
		if item := cloneJSONValue(ev["item"]); item != nil {
			r.added = append(r.added, item)
		}
	case "response.output_item.done":
		if item := cloneJSONValue(ev["item"]); item != nil {
			r.done = append(r.done, item)
		}
	case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
		r.terminal = typ
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			r.completed = cloneJSONMap(resp)
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
			r.completed = cloneJSONMap(ev)
			switch typ {
			case "response.completed":
				r.completed["status"] = "completed"
			case "response.incomplete":
				r.completed["status"] = "incomplete"
			default:
				r.completed["status"] = "failed"
			}
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
	if len(r.done) > 0 {
		items = cloneJSONArray(r.done)
	} else if len(r.added) > 0 {
		items = cloneJSONArray(r.added)
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
