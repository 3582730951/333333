package routing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

const (
	cachePrefixAffinityMinBytes = 2048
	cachePrefixTextHeadBytes    = 4096

	// CodexRootThreadAffinitySource identifies the canonical conversation key
	// shared by an official Codex root request (thread-id) and all of its spawned
	// children (x-codex-parent-thread-id).
	CodexRootThreadAffinitySource = "codex-root-thread-id"
)

type AffinityKey struct {
	Key    string `json:"key"`
	Hash   string `json:"hash"`
	Source string `json:"source"`
}

type PromptPrefixFingerprint struct {
	Hash        string `json:"hash"`
	Source      string `json:"source"`
	Reason      string `json:"reason"`
	PrefixBytes int    `json:"prefix_bytes"`
}

func ExtractAffinityKey(r *http.Request, body []byte) AffinityKey {
	if key := extractGenericTrueAffinityKey(r, body); key.Hash != "" {
		return key
	}
	if key := cachePrefixKey(r, body); key != "" {
		return newKey(key, "cache_prefix_hash")
	}
	if key := downstreamKey(r, body); key != "" {
		return newKey(key, "downstream_api_project_model")
	}
	if hash := stableMessagesHash(body); hash != "" {
		return newKey("stable_messages:"+hash, "stable_messages_hash")
	}
	return AffinityKey{}
}

func ExtractClaudeAffinityKey(r *http.Request, body []byte) AffinityKey {
	if key := ExtractClaudeTrueAffinityKey(r, body); key.Hash != "" {
		return key
	}
	if aliases := ClaudeItemAffinityKeys(body); len(aliases) > 0 {
		return aliases[0]
	}
	if key := claudeCachePrefixKey(r, body); key != "" {
		return newKey(key, "cache_prefix_hash")
	}
	if key := downstreamKey(r, body); key != "" {
		return newKey(key, "downstream_api_project_model")
	}
	if hash := stableMessagesHash(body); hash != "" {
		return newKey("stable_messages:"+hash, "stable_messages_hash")
	}
	return AffinityKey{}
}

// ClaudeItemAffinityKeys derives durable aliases from assistant/tool identifiers.
// They keep a truncated conversation sticky even when its first user anchor has
// fallen out of the client-provided history.
func ClaudeItemAffinityKeys(body []byte) []AffinityKey {
	var root interface{}
	if json.Unmarshal(body, &root) != nil {
		items := make([]interface{}, 0, 4)
		for _, line := range bytes.Split(body, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			var item interface{}
			if json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))), &item) == nil {
				items = append(items, item)
			}
		}
		root = items
	}
	seen := map[string]bool{}
	out := make([]AffinityKey, 0, 4)
	var walk func(interface{})
	walk = func(value interface{}) {
		switch value := value.(type) {
		case map[string]interface{}:
			for _, field := range []string{"tool_use_id", "call_id", "id"} {
				id, _ := value[field].(string)
				id = strings.TrimSpace(id)
				if id != "" && !seen[id] {
					seen[id] = true
					out = append(out, newKey("claude_item_id:"+id, "claude_item_id"))
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []interface{}:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(root)
	return out
}

func ExtractClaudeTrueAffinityKey(r *http.Request, body []byte) AffinityKey {
	if v := headerValue(r, "x-claude-code-session-id"); v != "" {
		return newKey("x-claude-code-session-id:"+v, "x-claude-code-session-id")
	}
	return extractGenericTrueAffinityKey(r, body)
}

func extractGenericTrueAffinityKey(r *http.Request, body []byte) AffinityKey {
	if v := headerValue(r, "x-codex-parent-thread-id"); v != "" {
		return codexRootThreadAffinity(v)
	}
	if v := headerValue(r, "thread-id"); v != "" {
		return codexRootThreadAffinity(v)
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
	if v := JSONStringField(body, "previous_response_id"); v != "" {
		return ResponseAffinityKey(v)
	}
	if v := headerValue(r, "x-codex-turn-metadata"); v != "" {
		return newKey("x-codex-turn-metadata:"+v, "x-codex-turn-metadata")
	}
	return AffinityKey{}
}

// ResponseAffinityKey is the durable lookup key for a Responses continuation.
// The API stores this alias as soon as an upstream response ID is observed, so a
// later request containing only previous_response_id returns to the same account,
// model, and egress even after a process restart.
func ResponseAffinityKey(responseID string) AffinityKey {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return AffinityKey{}
	}
	return newKey("previous_response_id:"+responseID, "previous_response_id")
}

func codexRootThreadAffinity(threadID string) AffinityKey {
	return newKey(CodexRootThreadAffinitySource+":"+threadID, CodexRootThreadAffinitySource)
}

func IsStrictSticky(path string, r *http.Request, body []byte) bool {
	if headerValue(r, "x-codex-turn-state") != "" {
		return true
	}
	if JSONStringField(body, "previous_response_id") != "" {
		return true
	}
	if IsCompaction(path, body) {
		return true
	}
	if bytes.Contains(body, []byte("tool_result")) || bytes.Contains(body, []byte("function_call_output")) {
		return true
	}
	return false
}

func IsCompaction(path string, body []byte) bool {
	return strings.Contains(path, "/responses/compact") || bytes.Contains(body, []byte("compaction_trigger"))
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

func StablePromptPrefixHash(body []byte) string {
	return StablePromptPrefixFingerprint(body).Hash
}

func StablePromptPrefixFingerprint(body []byte) PromptPrefixFingerprint {
	return stablePromptPrefixFingerprint(body)
}

func AnthropicStablePromptPrefixHash(body []byte) string {
	return AnthropicStablePromptPrefixFingerprint(body).Hash
}

func AnthropicStablePromptPrefixFingerprint(body []byte) PromptPrefixFingerprint {
	if len(bytes.TrimSpace(body)) == 0 {
		return PromptPrefixFingerprint{Reason: "empty_body"}
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return PromptPrefixFingerprint{Reason: "invalid_json"}
	}
	stable := map[string]interface{}{}
	if sys, ok := stableAnthropicSystem(root["system"]); ok {
		stable["system"] = sys
	}
	if tools, ok := root["tools"]; ok {
		stable["tools"] = tools
	}
	if toolChoice, ok := root["tool_choice"]; ok {
		stable["tool_choice"] = toolChoice
	}
	msgs, ok := root["messages"].([]interface{})
	if ok && len(msgs) > 0 {
		prefix, removed := withoutFinalUserTurn(msgs)
		if !removed {
			return PromptPrefixFingerprint{Reason: "final_turn_not_user"}
		}
		stable["messages"] = prefix
	} else if _, hasSystem := stable["system"]; !hasSystem {
		if _, hasTools := stable["tools"]; !hasTools {
			return PromptPrefixFingerprint{Reason: "empty_sequence"}
		}
	}
	return hashPromptPrefixCandidate(stable, "anthropic_message_prefix")
}

func Model(body []byte) string {
	return JSONStringField(body, "model")
}

func AffinityFromKey(key, source string) AffinityKey {
	key = strings.TrimSpace(key)
	source = strings.TrimSpace(source)
	if key == "" || source == "" {
		return AffinityKey{}
	}
	return newKey(key, source)
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

func cachePrefixKey(r *http.Request, body []byte) string {
	token := bearerFingerprint(r.Header.Get("Authorization"))
	project := firstNonEmpty(
		headerValue(r, "openai-project"),
		headerValue(r, "x-project-id"),
		JSONStringField(body, "project"),
	)
	model := JSONStringField(body, "model")
	if token == "" && project == "" {
		return ""
	}
	prefixHash := StablePromptPrefixHash(body)
	if prefixHash == "" {
		return ""
	}
	return strings.Join([]string{"cache_prefix", token, project, model, prefixHash}, ":")
}

func claudeCachePrefixKey(r *http.Request, body []byte) string {
	token := bearerFingerprint(r.Header.Get("Authorization"))
	project := firstNonEmpty(
		headerValue(r, "openai-project"),
		headerValue(r, "x-project-id"),
		JSONStringField(body, "project"),
	)
	model := JSONStringField(body, "model")
	if token == "" && project == "" {
		return ""
	}
	prefixHash := AnthropicStablePromptPrefixHash(body)
	if prefixHash == "" {
		return ""
	}
	return strings.Join([]string{"claude_cache_prefix", token, project, model, prefixHash}, ":")
}

func stablePromptPrefixFingerprint(body []byte) PromptPrefixFingerprint {
	if len(bytes.TrimSpace(body)) == 0 {
		return PromptPrefixFingerprint{Reason: "empty_body"}
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return PromptPrefixFingerprint{Reason: "invalid_json"}
	}
	seqName := "messages"
	seq := root[seqName]
	if seq == nil {
		seqName = "input"
		seq = root[seqName]
	}
	switch items := seq.(type) {
	case []interface{}:
		if len(items) == 0 {
			return PromptPrefixFingerprint{Reason: "empty_sequence"}
		}
		if fp := messagePrefixFingerprint(root, seqName, items); fp.Hash != "" {
			return fp
		}
		return finalUserTextPrefixFingerprint(root, items[len(items)-1])
	case string:
		return textPrefixFingerprint(root, seqName, items)
	default:
		return PromptPrefixFingerprint{Reason: "unsupported_sequence"}
	}
}

func stableAnthropicSystem(v interface{}) (interface{}, bool) {
	switch sys := v.(type) {
	case string:
		if strings.TrimSpace(sys) == "" || isClaudeBillingHeaderText(sys) {
			return nil, false
		}
		return sys, true
	case []interface{}:
		out := make([]interface{}, 0, len(sys))
		for _, item := range sys {
			if m, ok := item.(map[string]interface{}); ok {
				text, _ := m["text"].(string)
				if isClaudeBillingHeaderText(text) {
					continue
				}
			}
			out = append(out, item)
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	default:
		return nil, false
	}
}

func isClaudeBillingHeaderText(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "x-anthropic-billing-header:")
}

func messagePrefixFingerprint(root map[string]interface{}, seqName string, items []interface{}) PromptPrefixFingerprint {
	prefix, removed := withoutFinalUserTurn(items)
	if !removed {
		return PromptPrefixFingerprint{Reason: "final_turn_not_user"}
	}
	stable := map[string]interface{}{
		seqName: prefix,
	}
	copyPromptPrefixRootFields(root, stable)
	return hashPromptPrefixCandidate(stable, "message_prefix")
}

func finalUserTextPrefixFingerprint(root map[string]interface{}, item interface{}) PromptPrefixFingerprint {
	if roleOf(item) != "user" {
		return PromptPrefixFingerprint{Reason: "final_turn_not_user"}
	}
	m, ok := item.(map[string]interface{})
	if !ok {
		return PromptPrefixFingerprint{Reason: "final_user_not_object"}
	}
	text, ok := simpleTextContent(m["content"])
	if !ok {
		text, ok = leadingTextContentPrefix(m["content"])
		if !ok {
			return PromptPrefixFingerprint{Reason: "complex_final_user_content"}
		}
	}
	return textPrefixFingerprint(root, "input_text", text)
}

func textPrefixFingerprint(root map[string]interface{}, fieldName, text string) PromptPrefixFingerprint {
	if len([]byte(text)) < cachePrefixTextHeadBytes {
		return PromptPrefixFingerprint{Reason: "text_prefix_too_short"}
	}
	stable := map[string]interface{}{
		fieldName: firstNBytes(text, cachePrefixTextHeadBytes),
	}
	copyPromptPrefixRootFields(root, stable)
	return hashPromptPrefixCandidate(stable, "text_prefix")
}

func copyPromptPrefixRootFields(root, dst map[string]interface{}) {
	for _, key := range []string{
		"instructions",
		"reasoning",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
	} {
		if v, ok := root[key]; ok {
			dst[key] = v
		}
	}
}

func hashPromptPrefixCandidate(stable map[string]interface{}, source string) PromptPrefixFingerprint {
	raw, err := json.Marshal(stable)
	if err != nil || len(raw) < cachePrefixAffinityMinBytes {
		return PromptPrefixFingerprint{Reason: "prefix_too_short"}
	}
	sum := sha256.Sum256(raw)
	return PromptPrefixFingerprint{
		Hash:        hex.EncodeToString(sum[:]),
		Source:      source,
		Reason:      "ok",
		PrefixBytes: len(raw),
	}
}

func withoutFinalUserTurn(items []interface{}) ([]interface{}, bool) {
	last := len(items) - 1
	if roleOf(items[last]) != "user" {
		return nil, false
	}
	out := make([]interface{}, last)
	copy(out, items[:last])
	return out, true
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

func simpleTextContent(v interface{}) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, true
	case []interface{}:
		var b strings.Builder
		for _, part := range c {
			text, ok := simpleTextPart(part)
			if !ok {
				return "", false
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
		return b.String(), true
	case map[string]interface{}:
		return simpleTextPart(c)
	default:
		return "", false
	}
}

func leadingTextContentPrefix(v interface{}) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, true
	case map[string]interface{}:
		return simpleTextPart(c)
	case []interface{}:
		var b strings.Builder
		for _, part := range c {
			text, ok := simpleTextPart(part)
			if !ok {
				break
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
		if b.Len() == 0 {
			return "", false
		}
		return b.String(), true
	default:
		return "", false
	}
}

func simpleTextPart(v interface{}) (string, bool) {
	switch part := v.(type) {
	case string:
		return part, true
	case map[string]interface{}:
		t, _ := part["type"].(string)
		if t != "input_text" && t != "text" {
			return "", false
		}
		text, ok := part["text"].(string)
		return text, ok
	default:
		return "", false
	}
}

func firstNBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isUTF8Boundary(s[n]) {
		n--
	}
	if n <= 0 {
		return ""
	}
	return s[:n]
}

func isUTF8Boundary(b byte) bool {
	return b&0xc0 != 0x80
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
