package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

type AffinityKey struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	Source string `json:"source"`
}

func ExtractAffinityKey(r *http.Request, body []byte) AffinityKey {
	if v := headerValue(r, "x-codex-parent-thread-id"); v != "" {
		return newKey("x-codex-parent-thread-id:"+v, "x-codex-parent-thread-id")
	}
	if v := JSONStringField(body, "thread_id"); v != "" {
		return newKey("thread_id:"+v, "thread_id")
	}
	if v := JSONStringField(body, "conversation_id"); v != "" {
		return newKey("conversation_id:"+v, "conversation_id")
	}
	if v := headerValue(r, "x-codex-window-id"); v != "" {
		return newKey("x-codex-window-id:"+v, "x-codex-window-id")
	}
	if v := JSONStringField(body, "prompt_cache_key"); v != "" {
		return newKey("prompt_cache_key:"+v, "prompt_cache_key")
	}
	if v := headerValue(r, "x-codex-turn-metadata"); v != "" {
		return newKey("x-codex-turn-metadata:"+v, "x-codex-turn-metadata")
	}
	if key := downstreamKey(r, body); key != "" {
		return newKey(key, "downstream_api_project_model")
	}
	if hash := stableMessagesHash(body); hash != "" {
		return newKey("stable_messages:"+hash, "stable_messages_hash")
	}
	return AffinityKey{}
}

func IsStrictSticky(path string, r *http.Request, body []byte) bool {
	if strings.Contains(path, "/responses/compact") {
		return true
	}
	if headerValue(r, "x-codex-turn-state") != "" {
		return true
	}
	if JSONStringField(body, "previous_response_id") != "" {
		return true
	}
	if bytes.Contains(body, []byte("compaction_trigger")) {
		return true
	}
	if bytes.Contains(body, []byte("tool_result")) || bytes.Contains(body, []byte("function_call_output")) {
		return true
	}
	return false
}

// HasServerSideState reports whether a request depends on per-account server-side
// state that a *different* account cannot reconstruct — specifically a
// previous_response_id (a delta against a stored response) or an x-codex-turn-state
// header. Such a request must never be silently failed over: the fresh account has no
// matching state, so the switch would corrupt the conversation rather than continue it.
//
// This is a STRICT SUBSET of IsStrictSticky. A turn can be strict-sticky purely for
// prompt-cache affinity (tool_result / function_call_output / compaction) while still
// carrying its FULL input in the body — those are self-contained and CAN move accounts
// losslessly. Failover decisions therefore key off HasServerSideState (movable?), while
// scheduler stickiness keys off IsStrictSticky (keep cache warm). The reported bug —
// "429 leaks downstream, no auto-switch" — was caused by gating failover on
// IsStrictSticky, which wrongly pinned the self-contained common case.
func HasServerSideState(path string, r *http.Request, body []byte) bool {
	if headerValue(r, "x-codex-turn-state") != "" {
		return true
	}
	if JSONStringField(body, "previous_response_id") != "" {
		return true
	}
	return false
}

func PromptCacheKey(body []byte) string {
	return JSONStringField(body, "prompt_cache_key")
}

func Model(body []byte) string {
	return JSONStringField(body, "model")
}

func newKey(key, source string) AffinityKey {
	sum := sha256.Sum256([]byte(key))
	return AffinityKey{
		Key:    key,
		Hash:   hex.EncodeToString(sum[:]),
		Source: source,
	}
}

func downstreamKey(r *http.Request, body []byte) string {
	token := bearerFingerprint(r.Header.Get("Authorization"))
	project := firstNonEmpty(
		headerValue(r, "openai-project"),
		headerValue(r, "x-project-id"),
		JSONStringField(body, "project"),
	)
	model := JSONStringField(body, "model")
	// Fold in a per-conversation anchor so two distinct conversations from the
	// same downstream identity (token+project+model) do NOT collapse onto a single
	// account. This matters most for the native Claude relay, whose /v1/messages
	// bodies carry none of the Codex correlators above, so without the anchor every
	// conversation — and every concurrently-running sub-agent — would pin to one
	// account (load collapse + cross-agent cache thrash). The anchor is stable
	// across a conversation's turns, so routing stays sticky and prompt-cache warm.
	anchor := ConversationAnchor(body)
	if token == "" && project == "" && model == "" && anchor == "" {
		return ""
	}
	return strings.Join([]string{"downstream", token, project, model, anchor}, ":")
}

// ConversationAnchor returns a stable-across-turns, distinct-across-conversations
// fingerprint derived from the conversation's first user message. Unlike a raw
// prefix hash (dominated by the identical system preamble that Codex/Claude Code
// sub-agents share), it actually distinguishes sibling agents. Returns "" when no
// message/input content is present.
func ConversationAnchor(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	seq := root["messages"]
	if seq == nil {
		seq = root["input"]
	}
	var anchor interface{}
	switch t := seq.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return ""
		}
		anchor = t
	case []interface{}:
		for _, it := range t {
			if roleOf(it) == "user" {
				anchor = it
				break
			}
		}
		if anchor == nil && len(t) > 0 {
			anchor = t[0]
		}
	}
	if anchor == nil {
		return ""
	}
	raw, err := json.Marshal(anchor)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func roleOf(v interface{}) string {
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m["role"].(string); ok {
			return s
		}
	}
	return ""
}

func bearerFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func stableMessagesHash(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	prefix := body
	if len(prefix) > 4096 {
		prefix = prefix[:4096]
	}
	sum := sha256.Sum256(prefix)
	return hex.EncodeToString(sum[:])
}

func JSONStringField(body []byte, key string) string {
	needle := []byte(`"` + key + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(needle):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = bytes.TrimLeft(rest[colon+1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	end := 1
	escaped := false
	for ; end < len(rest); end++ {
		if escaped {
			escaped = false
			continue
		}
		switch rest[end] {
		case '\\':
			escaped = true
		case '"':
			var out string
			if err := json.Unmarshal(rest[:end+1], &out); err == nil {
				return out
			}
			return ""
		}
	}
	return ""
}

func headerValue(r *http.Request, key string) string {
	return strings.TrimSpace(r.Header.Get(key))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
