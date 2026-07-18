package api

import (
	"bytes"
	"encoding/json"
	"strings"
)

const streamLedgerMaxPartialFrame = 256 * 1024

type codexStreamLedgerRecorder struct {
	buf       []byte
	id        string
	model     string
	text      strings.Builder
	completed map[string]interface{}
	added     []interface{}
	done      []interface{}
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
	r.buf = append(r.buf, p...)
	for {
		idx := bytes.Index(r.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := r.buf[:idx+2]
		r.observeFrame(frame)
		r.buf = r.buf[idx+2:]
	}
	if len(r.buf) > streamLedgerMaxPartialFrame {
		r.buf = r.buf[len(r.buf)-streamLedgerMaxPartialFrame:]
	}
	return len(p), nil
}

func (r *codexStreamLedgerRecorder) ResponseJSON() []byte {
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

// reachedTerminal reports whether the stream observed a terminal Responses event
// (response.completed / response.incomplete / response.failed). When false after the
// upstream body ends, the stream was truncated — the signal the auto-continue relay
// uses to decide whether to re-issue.
func (r *codexStreamLedgerRecorder) reachedTerminal() bool { return r.completed != nil }

// partialText returns the assistant output text accumulated so far.
func (r *codexStreamLedgerRecorder) partialText() string { return r.text.String() }

// partialItems reconstructs the assistant output items produced so far (the same shape
// persistContextJournal appends as input for the next turn), used to re-inject the
// partial answer into the continuation request so the model continues instead of
// restarting — keeping the prompt-cache prefix intact.
func (r *codexStreamLedgerRecorder) partialItems() []interface{} {
	return r.outputItems(r.text.String())
}

// partialItemCount is the number of output items the stream opened, used to offset the
// continuation's output_index so its items never collide with the ones already relayed.
func (r *codexStreamLedgerRecorder) partialItemCount() int {
	if n := len(r.done); n > len(r.added) {
		return n
	}
	return len(r.added)
}

func (r *codexStreamLedgerRecorder) observeFrame(frame []byte) {
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
	case "response.created":
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			r.id = firstNonEmpty(r.id, streamString(resp["id"]))
			r.model = firstNonEmpty(r.model, streamString(resp["model"]))
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
	case "response.completed", "response.incomplete", "response.failed":
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			r.completed = cloneJSONMap(resp)
			r.id = firstNonEmpty(r.id, streamString(resp["id"]))
			r.model = firstNonEmpty(r.model, streamString(resp["model"]))
		} else if typ == "response.failed" {
			r.completed = cloneJSONMap(ev)
			r.completed["status"] = "failed"
		}
	}
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
