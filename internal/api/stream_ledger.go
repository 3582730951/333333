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
	if !hasNonEmptyArray(root["output"]) && strings.TrimSpace(streamString(root["output_text"])) == "" {
		return nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil
	}
	return out
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
	case "response.completed":
		if resp, ok := ev["response"].(map[string]interface{}); ok {
			r.completed = cloneJSONMap(resp)
			r.id = firstNonEmpty(r.id, streamString(resp["id"]))
			r.model = firstNonEmpty(r.model, streamString(resp["model"]))
		}
	}
}

func (r *codexStreamLedgerRecorder) outputItems(text string) []interface{} {
	if len(r.done) > 0 {
		return cloneJSONArray(r.done)
	}
	if strings.TrimSpace(text) != "" {
		return []interface{}{map[string]interface{}{
			"type": "message",
			"role": "assistant",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text",
				"text": text,
			}},
		}}
	}
	if len(r.added) > 0 {
		return cloneJSONArray(r.added)
	}
	return nil
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
