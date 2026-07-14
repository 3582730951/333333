package responsefilter

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Match reports whether any configured keyword occurs in body.
func Match(body []byte, keywords []string, caseSensitive bool) bool {
	s := string(body)
	if !caseSensitive {
		s = strings.ToLower(s)
	}
	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		needle := kw
		if !caseSensitive {
			needle = strings.ToLower(needle)
		}
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// FilterSSEFrame drops ordinary matching events. Terminal Responses events are
// retained and have matching string-valued content removed so the protocol can
// complete cleanly.
func FilterSSEFrame(frame []byte, keywords []string, caseSensitive bool) []byte {
	if !Match(frame, keywords, caseSensitive) {
		return frame
	}
	lower := strings.ToLower(string(frame))
	if bytes.Contains([]byte(lower), []byte("response.completed")) || bytes.Contains([]byte(lower), []byte("response.done")) {
		if out, ok := FilterJSONInSSE(frame, keywords, caseSensitive); ok {
			return out
		}
	}
	return nil
}

// StripSafetyBufferingJSON removes only the top-level Responses API
// safety_buffering control field. All other protocol fields are preserved.
func StripSafetyBufferingJSON(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	if _, ok := root["safety_buffering"]; !ok {
		return body, false
	}
	delete(root, "safety_buffering")
	out, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return out, true
}

// StripSafetyBufferingSSE removes only the Responses API safety_buffering
// control field. The surrounding event remains byte-valid and continues through
// the normal Codex state machine, including response.created/completed.
func StripSafetyBufferingSSE(frame []byte) ([]byte, bool) {
	lines := bytes.Split(frame, []byte("\n"))
	changed := false
	for i, line := range lines {
		trim := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trim, []byte("data:")) {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(trim, []byte("data:")))
		enc, ok := StripSafetyBufferingJSON(raw)
		if !ok {
			continue
		}
		prefix := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		lines[i] = append(append(prefix, []byte("data: ")...), enc...)
		if bytes.HasSuffix(line, []byte("\r")) {
			lines[i] = append(lines[i], '\r')
		}
		changed = true
	}
	if !changed {
		return frame, false
	}
	return bytes.Join(lines, []byte("\n")), true
}

func FilterJSONInSSE(frame []byte, keywords []string, caseSensitive bool) ([]byte, bool) {
	lines := bytes.Split(frame, []byte("\n"))
	changed := false
	for i, line := range lines {
		trim := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trim, []byte("data:")) {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(trim, []byte("data:")))
		var v interface{}
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		v, c := prune(v, keywords, caseSensitive)
		if !c {
			continue
		}
		enc, err := json.Marshal(v)
		if err != nil {
			continue
		}
		prefix := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		lines[i] = append(append(prefix, []byte("data: ")...), enc...)
		changed = true
	}
	if !changed {
		return frame, false
	}
	return bytes.Join(lines, []byte("\n")), true
}

// FilterJSON removes matching string-valued content leaves from a JSON body.
func FilterJSON(body []byte, keywords []string, caseSensitive bool) ([]byte, bool) {
	var v interface{}
	if json.Unmarshal(body, &v) != nil {
		return body, false
	}
	v, changed := prune(v, keywords, caseSensitive)
	if !changed {
		return body, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return body, false
	}
	return out, true
}

func prune(v interface{}, keywords []string, caseSensitive bool) (interface{}, bool) {
	switch x := v.(type) {
	case string:
		return x, Match([]byte(x), keywords, caseSensitive)
	case []interface{}:
		out := make([]interface{}, 0, len(x))
		changed := false
		for _, item := range x {
			if s, ok := item.(string); ok && Match([]byte(s), keywords, caseSensitive) {
				changed = true
				continue
			}
			if next, hit := prune(item, keywords, caseSensitive); hit {
				item = next
				changed = true
			}
			out = append(out, item)
		}
		return out, changed
	case map[string]interface{}:
		changed := false
		for k, item := range x {
			if s, ok := item.(string); ok && Match([]byte(s), keywords, caseSensitive) {
				delete(x, k)
				changed = true
				continue
			}
			if next, hit := prune(item, keywords, caseSensitive); hit {
				x[k] = next
				changed = true
			}
		}
		return x, changed
	default:
		return v, false
	}
}
