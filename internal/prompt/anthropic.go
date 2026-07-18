package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"codex-account-pool/internal/anthropicwire"
)

// ChatCompletionToAnthropic converts an OpenAI Chat Completions request body
// into an Anthropic Messages (/v1/messages) request body, including full
// tool-calling: OpenAI `tools` (function) → Anthropic `tools` (input_schema),
// `tool_choice`, assistant `tool_calls` → `tool_use` blocks, and `tool`-role
// messages → `tool_result` blocks (consecutive results merged into one user
// turn). System/developer messages are hoisted to the top-level `system` field.
// Anthropic requires max_tokens, so a default is supplied when absent.
func ChatCompletionToAnthropic(raw []byte) ([]byte, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return nil, err
	}
	messages, _ := root["messages"].([]interface{})

	out := map[string]interface{}{}
	if m, ok := root["model"].(string); ok {
		out["model"] = m
	}
	maxTokens := firstNum(root, "max_tokens", "max_completion_tokens")
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	out["max_tokens"] = maxTokens
	if v, ok := root["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := root["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := root["stream"].(bool); ok && v {
		out["stream"] = true
	}
	if stop := toStringSlice(root["stop"]); len(stop) > 0 {
		out["stop_sequences"] = stop
	}
	if tools := convertOpenAITools(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := convertToolChoice(root["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}

	var systemParts []string
	anthMessages := make([]interface{}, 0, len(messages))
	appendToolResult := func(block map[string]interface{}) {
		if n := len(anthMessages); n > 0 {
			if last, ok := anthMessages[n-1].(map[string]interface{}); ok && last["role"] == "user" {
				if blocks, ok := last["content"].([]interface{}); ok && isToolResultContent(blocks) {
					last["content"] = append(blocks, block)
					return
				}
			}
		}
		anthMessages = append(anthMessages, map[string]interface{}{
			"role":    "user",
			"content": []interface{}{block},
		})
	}

	for _, mi := range messages {
		m, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if txt := chatContentToText(m["content"]); txt != "" {
				systemParts = append(systemParts, txt)
			}
		case "tool":
			appendToolResult(map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": stringOr(m["tool_call_id"], ""),
				"content":     chatToolResultContent(m["content"]),
			})
		case "assistant":
			blocks := []interface{}{}
			if content, blocky := chatContentToAnthropicContent(m["content"]); blocky {
				blocks = append(blocks, content.([]interface{})...)
			} else if txt, _ := content.(string); txt != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": txt})
			}
			for _, tc := range toSlice(m["tool_calls"]) {
				tcm, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := tcm["function"].(map[string]interface{})
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    stringOr(tcm["id"], ""),
					"name":  stringOr(mapGet(fn, "name"), ""),
					"input": parseJSONObject(mapGet(fn, "arguments")),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": ""})
			}
			anthMessages = append(anthMessages, map[string]interface{}{"role": "assistant", "content": blocks})
		default: // user
			content, _ := chatContentToAnthropicContent(m["content"])
			anthMessages = append(anthMessages, map[string]interface{}{
				"role":    "user",
				"content": content,
			})
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = anthMessages
	return json.Marshal(out)
}

// AnthropicToChatCompletion converts an Anthropic Messages response into an
// OpenAI Chat Completions response (non-streaming), mapping tool_use blocks to
// OpenAI tool_calls.
func AnthropicToChatCompletion(raw []byte, model string) ([]byte, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return raw, nil
	}
	text, toolCalls := anthropicContentToOpenAI(root["content"])
	if model == "" {
		if m, ok := root["model"].(string); ok {
			model = m
		}
	}
	id, _ := root["id"].(string)
	if id == "" {
		id = "chatcmpl-claude"
	}
	message := map[string]interface{}{"role": "assistant"}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text != "" {
			message["content"] = text
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = text
	}
	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": StopReasonToFinish(root["stop_reason"]),
			},
		},
	}
	if u, ok := root["usage"].(map[string]interface{}); ok {
		resp["usage"] = anthropicUsageToOpenAI(u)
	}
	return json.Marshal(resp)
}

// StopReasonToFinish maps an Anthropic stop_reason to an OpenAI finish_reason.
func StopReasonToFinish(v interface{}) string {
	switch s, _ := v.(string); s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default: // end_turn, stop_sequence, "" ...
		return "stop"
	}
}

// EnsureAnthropicCacheControl adds Anthropic prompt-cache breakpoints to stable
// Claude /v1/messages prefixes. It is intentionally structural: tools, system
// text, Claude Code's auto-context reminder, and prior user turns can be cached
// without changing model quality, reasoning, tool schemas, or the latest user
// request. ttl == "1h" requests the extended (1-hour) cache; anything else uses
// the standard 5-minute ephemeral cache.
func EnsureAnthropicCacheControl(body []byte, ttl string) []byte {
	return EnsureAnthropicCacheControlWithPolicy(body, ttl, "legacy")
}

type AnthropicCacheControlOptions struct {
	TTL                  string
	Policy               string
	LatestTailWrite      bool
	LosslessBlockSplit   bool
	PreferRecentTurnRead bool
}

func EnsureAnthropicCacheControlWithOptions(body []byte, opts AnthropicCacheControlOptions) []byte {
	return ensureAnthropicCacheControlWithOptions(body, opts.TTL, opts.Policy, opts.LatestTailWrite, opts.LosslessBlockSplit, opts.PreferRecentTurnRead)
}

func EnsureAnthropicCacheControlWithPolicy(body []byte, ttl, policy string) []byte {
	return ensureAnthropicCacheControlWithOptions(body, ttl, policy, true, false, false)
}

func ensureAnthropicCacheControlWithOptions(body []byte, ttl, policy string, latestTailWrite, losslessBlockSplit, preferRecentTurnRead bool) []byte {
	if losslessBlockSplit {
		body = LosslessSplitAnthropicTextBlocks(body)
	}
	root, err := decodeJSONMapUseNumber(body)
	if err != nil {
		return body
	}
	changed := false
	if anthropicwire.SanitizeVolatileCacheControls(root) {
		changed = true
	}
	if anthropicwire.NormalizeCacheControlTTLForPolicy(root, ttl) {
		changed = true
	}
	finish := func() []byte {
		if anthropicwire.CapCacheControlBreakpoints(root, 4) {
			changed = true
		}
		if anthropicwire.NormalizeCacheControlTTLForPolicy(root, ttl) {
			changed = true
		}
		if !changed {
			return body
		}
		out, err := json.Marshal(root)
		if err != nil {
			return body
		}
		return out
	}
	existing := countCacheControl(root["system"]) + countCacheControl(root["tools"]) + countCacheControlMessages(root["messages"])
	budget := 4 - existing
	if budget <= 0 {
		return finish()
	}
	mk := func() map[string]interface{} {
		m := map[string]interface{}{"type": "ephemeral"}
		if strings.TrimSpace(ttl) == "1h" {
			m["ttl"] = "1h"
		}
		return m
	}
	if normalizeAnthropicCachePolicy(policy) == "max_hit" {
		if planAnthropicMaxHitCacheControl(root, mk, latestTailWrite, preferRecentTurnRead) {
			changed = true
		}
		return finish()
	}

	// 1) End of the tool definition prefix. Tool schemas are stable across turns
	// and should be preferred over marking volatile last-user content.
	if budget > 0 {
		if markListTail(root["tools"], mk) {
			budget--
			changed = true
		}
	}
	// 2) End of the non-billing system prompt. Claude Code's billing header
	// changes per request and must not become a cache breakpoint.
	if budget > 0 {
		if sys, ok := markSystemTail(root["system"], mk); ok {
			root["system"] = sys
			budget--
			changed = true
		}
	}
	if normalizeAnthropicCachePolicy(policy) == "coarse_safe" {
		return finish()
	}
	// 3) Claude Code native auto-context block: this is the stable prefix before
	// the real user request in current Claude Code request shapes.
	if budget > 0 {
		if markClaudeNativeAutoContext(root["messages"], mk) {
			budget--
			changed = true
		}
	}
	// 4) Multi-turn conversation history. The newest user message stays unmarked
	// so different final questions can share the earlier prefix.
	if budget > 0 {
		if markSecondToLastUserTurn(root["messages"], mk) {
			budget--
			changed = true
		}
	}
	return finish()
}

func normalizeAnthropicCachePolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "coarse_safe", "stable_prefix_safe", "aggressive", "max_hit":
		return strings.ToLower(strings.TrimSpace(policy))
	default:
		return "balanced"
	}
}

func planAnthropicMaxHitCacheControl(root map[string]interface{}, mk func() map[string]interface{}, latestTailWrite, preferRecentTurnRead bool) bool {
	changed := clearCacheControls(root)
	budget := 4
	if markListTail(root["tools"], mk) {
		budget--
		changed = true
	}
	if budget > 0 {
		if sys, ok := markSystemTail(root["system"], mk); ok {
			root["system"] = sys
			budget--
			changed = true
		}
	}
	if budget > 0 && !preferRecentTurnRead {
		if markClaudeNativeAutoContext(root["messages"], mk) {
			budget--
			changed = true
		}
	}
	latestReserve := 0
	if latestTailWrite && latestCacheableMessageIndex(root["messages"]) >= 0 {
		latestReserve = 1
	}
	// Kiro can reuse a cache written at the previous request's latest-user
	// boundary only if the next request recreates a cachePoint at that same
	// boundary after it moves into history. Prefer that rolling read over an
	// extra early auto-context marker while still reserving one slot to write the
	// current tail for the following turn. This changes markers only; message
	// bytes, roles, ordering, tool results, and generation controls are untouched.
	if preferRecentTurnRead && budget > latestReserve {
		if markSecondToLastUserTurn(root["messages"], mk) {
			budget--
			changed = true
		}
	}
	if preferRecentTurnRead && budget > latestReserve {
		if markClaudeNativeAutoContext(root["messages"], mk) {
			budget--
			changed = true
		}
	}
	if budget > latestReserve {
		used := markRollingHistoryAnchors(root["messages"], mk, budget-latestReserve)
		if used > 0 {
			budget -= used
			changed = true
		}
	}
	if latestTailWrite && budget > 0 {
		if markLatestCacheableMessageTail(root["messages"], mk) {
			changed = true
		}
	}
	return changed
}

func clearCacheControls(root map[string]interface{}) bool {
	changed := false
	visit := func(v interface{}) {
		arr, ok := v.([]interface{})
		if !ok {
			return
		}
		for _, item := range arr {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := block["cache_control"]; has {
				delete(block, "cache_control")
				changed = true
			}
		}
	}
	visit(root["tools"])
	visit(root["system"])
	if msgs, ok := root["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := m["cache_control"]; has {
				delete(m, "cache_control")
				changed = true
			}
			visit(m["content"])
		}
	}
	return changed
}

func markRollingHistoryAnchors(messages interface{}, mk func() map[string]interface{}, slots int) int {
	if slots <= 0 {
		return 0
	}
	msgs, ok := messages.([]interface{})
	if !ok {
		return 0
	}
	latest := latestCacheableMessageIndex(messages)
	if latest <= 0 {
		return 0
	}
	used := 0
	for target := latest - 15; target >= 0 && used < slots; target -= 15 {
		idx := nearestUnmarkedCacheableMessageAtOrBefore(msgs, target)
		if idx < 0 {
			continue
		}
		if markMessageTail(msgs[idx], mk) {
			used++
		}
	}
	return used
}

func nearestUnmarkedCacheableMessageAtOrBefore(msgs []interface{}, target int) int {
	if target >= len(msgs) {
		target = len(msgs) - 1
	}
	for i := target; i >= 0; i-- {
		if !messageHasCacheableTail(msgs[i]) || messageHasCacheControl(msgs[i]) {
			continue
		}
		return i
	}
	return -1
}

func latestCacheableMessageIndex(messages interface{}) int {
	msgs, ok := messages.([]interface{})
	if !ok {
		return -1
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if messageHasCacheableTail(msgs[i]) {
			return i
		}
	}
	return -1
}

func markLatestCacheableMessageTail(messages interface{}, mk func() map[string]interface{}) bool {
	msgs, ok := messages.([]interface{})
	if !ok {
		return false
	}
	idx := latestCacheableMessageIndex(messages)
	if idx < 0 {
		return false
	}
	return markMessageTail(msgs[idx], mk)
}

func messageHasCacheableTail(msg interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	switch c := m["content"].(type) {
	case string:
		return strings.TrimSpace(c) != ""
	case []interface{}:
		for i := len(c) - 1; i >= 0; i-- {
			if _, ok := c[i].(map[string]interface{}); ok {
				return true
			}
		}
	}
	return false
}

func messageHasCacheControl(msg interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	if _, has := m["cache_control"]; has {
		return true
	}
	if blocks, ok := m["content"].([]interface{}); ok {
		for _, item := range blocks {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := block["cache_control"]; has {
				return true
			}
		}
	}
	return false
}

// LosslessSplitAnthropicTextBlocks splits a single text block into adjacent text
// blocks only when their concatenated bytes are exactly identical to the original
// text. It is intentionally narrow: it recognizes Claude Code's auto-context
// system-reminder delimiter and does not summarize, drop, reorder, or edit bytes.
func LosslessSplitAnthropicTextBlocks(body []byte) []byte {
	root, err := decodeJSONMapUseNumber(body)
	if err != nil {
		return body
	}
	changed := false
	msgs, ok := root["messages"].([]interface{})
	if !ok {
		return body
	}
	for _, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		blocks, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		out := make([]interface{}, 0, len(blocks)+1)
		for _, item := range blocks {
			block, ok := item.(map[string]interface{})
			if !ok {
				out = append(out, item)
				continue
			}
			if _, has := block["cache_control"]; has {
				out = append(out, item)
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				out = append(out, item)
				continue
			}
			left, right, split := losslessAutoContextSplit(text)
			if !split || left+right != text {
				out = append(out, item)
				continue
			}
			first := cloneStringInterfaceMap(block)
			second := cloneStringInterfaceMap(block)
			first["text"] = left
			second["text"] = right
			out = append(out, first, second)
			changed = true
		}
		if changed {
			m["content"] = out
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func losslessAutoContextSplit(text string) (string, string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<system-reminder>") ||
		!strings.Contains(trimmed, "As you answer the user's questions, you can use the following context:") {
		return "", "", false
	}
	for _, marker := range []string{"</system-reminder>\n\n", "</system-reminder>\r\n\r\n"} {
		if idx := strings.Index(text, marker); idx >= 0 {
			split := idx + len(marker)
			if split > 0 && split < len(text) {
				return text[:split], text[split:], true
			}
		}
	}
	return "", "", false
}

func cloneStringInterfaceMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

type AnthropicCacheControlDiagnostics struct {
	BreakpointCount                   int
	Breakpoints                       []AnthropicCacheBreakpointDiagnostic
	UnwrittenTailTokens               int64
	MaxPossibleCacheReadTokens        int64
	LatestUserCacheControl            bool
	LatestUserAutoContextCacheControl bool
	LatestUserTailCacheControl        bool
	LatestUserToolResultCacheControl  bool
}

type AnthropicCacheBreakpointDiagnostic struct {
	Section       string `json:"section"`
	MessageIndex  int    `json:"message_index,omitempty"`
	BlockIndex    int    `json:"block_index,omitempty"`
	Type          string `json:"type"`
	TokenEstimate int64  `json:"token_estimate"`
	Hash          string `json:"hash"`
	TTL           string `json:"ttl,omitempty"`
}

func InspectAnthropicCacheControl(body []byte) AnthropicCacheControlDiagnostics {
	root, err := decodeJSONMapUseNumber(body)
	if err != nil {
		return AnthropicCacheControlDiagnostics{}
	}
	latest := latestUserCacheControlDetails(root["messages"])
	latest.BreakpointCount = countCacheControl(root["system"]) + countCacheControl(root["tools"]) + countCacheControlMessages(root["messages"])
	latest.Breakpoints, latest.UnwrittenTailTokens, latest.MaxPossibleCacheReadTokens = inspectAnthropicCacheBreakpoints(root)
	return latest
}

func inspectAnthropicCacheBreakpoints(root map[string]interface{}) ([]AnthropicCacheBreakpointDiagnostic, int64, int64) {
	type entry struct {
		block     map[string]interface{}
		section   string
		msgIdx    int
		blockIdx  int
		tokens    int64
		hasMarker bool
	}
	entries := []entry{}
	addList := func(v interface{}, section string, msgIdx int) {
		arr, ok := v.([]interface{})
		if !ok {
			return
		}
		for i, item := range arr {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			_, hasMarker := block["cache_control"]
			entries = append(entries, entry{
				block:     block,
				section:   section,
				msgIdx:    msgIdx,
				blockIdx:  i,
				tokens:    estimateAnthropicBlockTokens(block),
				hasMarker: hasMarker,
			})
		}
	}
	addList(root["tools"], "tools", -1)
	addList(root["system"], "system", -1)
	if msgs, ok := root["messages"].([]interface{}); ok {
		for msgIdx, item := range msgs {
			msg, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if _, has := msg["cache_control"]; has {
				entries = append(entries, entry{
					block:     msg,
					section:   "messages",
					msgIdx:    msgIdx,
					blockIdx:  -1,
					tokens:    estimateAnthropicBlockTokens(msg),
					hasMarker: true,
				})
			}
			addList(msg["content"], "messages", msgIdx)
		}
	}

	breakpoints := []AnthropicCacheBreakpointDiagnostic{}
	total := int64(0)
	maxRead := int64(0)
	lastMarker := -1
	for i, ent := range entries {
		total += ent.tokens
		if !ent.hasMarker {
			continue
		}
		lastMarker = i
		maxRead = total
		breakpoints = append(breakpoints, AnthropicCacheBreakpointDiagnostic{
			Section:       ent.section,
			MessageIndex:  ent.msgIdx,
			BlockIndex:    ent.blockIdx,
			Type:          anthropicBlockType(ent.block),
			TokenEstimate: ent.tokens,
			Hash:          shortBlockHash(ent.block),
			TTL:           cacheControlTTL(ent.block),
		})
	}
	if lastMarker < 0 {
		return breakpoints, total, 0
	}
	tail := int64(0)
	for _, ent := range entries[lastMarker+1:] {
		tail += ent.tokens
	}
	return breakpoints, tail, maxRead
}

func estimateAnthropicBlockTokens(block map[string]interface{}) int64 {
	if text, ok := block["text"].(string); ok {
		return estimateTextTokens(text)
	}
	if text, ok := block["content"].(string); ok {
		return estimateTextTokens(text)
	}
	raw, err := json.Marshal(stripCacheControlForHash(block))
	if err != nil {
		return 1
	}
	if len(raw) == 0 {
		return 1
	}
	return int64((len(raw) + 3) / 4)
}

func estimateTextTokens(text string) int64 {
	text = strings.TrimSpace(text)
	if text == "" {
		return 1
	}
	return int64((len(text) + 3) / 4)
}

func anthropicBlockType(block map[string]interface{}) string {
	if typ, _ := block["type"].(string); typ != "" {
		return typ
	}
	if _, has := block["role"]; has {
		return "message"
	}
	if _, has := block["name"]; has {
		return "tool"
	}
	return "block"
}

func shortBlockHash(block map[string]interface{}) string {
	raw, err := json.Marshal(stripCacheControlForHash(block))
	if err != nil {
		raw = []byte(anthropicBlockType(block))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func stripCacheControlForHash(block map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(block))
	for k, v := range block {
		if k == "cache_control" {
			continue
		}
		out[k] = v
	}
	return out
}

func cacheControlTTL(block map[string]interface{}) string {
	cc, ok := block["cache_control"].(map[string]interface{})
	if !ok {
		return ""
	}
	ttl, _ := cc["ttl"].(string)
	return ttl
}

func countCacheControl(system interface{}) int {
	n := 0
	if arr, ok := system.([]interface{}); ok {
		for _, b := range arr {
			if m, ok := b.(map[string]interface{}); ok {
				if _, has := m["cache_control"]; has {
					n++
				}
			}
		}
	}
	return n
}

func hasCacheableList(v interface{}) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, item := range arr {
		if _, ok := item.(map[string]interface{}); ok {
			return true
		}
	}
	return false
}

func countCacheControlMessages(messages interface{}) int {
	arr, ok := messages.([]interface{})
	if !ok {
		return 0
	}
	n := 0
	for _, msg := range arr {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if _, has := m["cache_control"]; has {
			n++
		}
		if blocks, ok := m["content"].([]interface{}); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]interface{}); ok {
					if _, has := bm["cache_control"]; has {
						n++
					}
				}
			}
		}
	}
	return n
}

func latestUserCacheControlDetails(messages interface{}) AnthropicCacheControlDiagnostics {
	diag := AnthropicCacheControlDiagnostics{}
	msgs, ok := messages.([]interface{})
	if !ok {
		return diag
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok || msg["role"] != "user" {
			continue
		}
		if _, has := msg["cache_control"]; has {
			diag.LatestUserTailCacheControl = true
		}
		switch c := msg["content"].(type) {
		case []interface{}:
			for idx, b := range c {
				if bm, ok := b.(map[string]interface{}); ok {
					if _, has := bm["cache_control"]; has {
						if idx == 0 && len(c) >= 2 && anthropicwire.IsClaudeCodeAutoContextBlock(c[0], c[1]) {
							diag.LatestUserAutoContextCacheControl = true
						} else if typ, _ := bm["type"].(string); typ == "tool_result" {
							diag.LatestUserToolResultCacheControl = true
						} else {
							diag.LatestUserTailCacheControl = true
						}
					}
				}
			}
		case map[string]interface{}:
			if _, has := c["cache_control"]; has {
				if typ, _ := c["type"].(string); typ == "tool_result" {
					diag.LatestUserToolResultCacheControl = true
				} else {
					diag.LatestUserTailCacheControl = true
				}
			}
		}
		diag.LatestUserCacheControl = diag.LatestUserTailCacheControl || diag.LatestUserToolResultCacheControl
		return diag
	}
	return diag
}

// markSystemTail puts a cache_control breakpoint at the end of the system prompt,
// converting a plain-string system into a single text block when needed. It never
// marks Claude Code's x-anthropic-billing-header block, which changes per request.
func markSystemTail(system interface{}, mk func() map[string]interface{}) (interface{}, bool) {
	switch s := system.(type) {
	case string:
		if strings.TrimSpace(s) == "" || isClaudeBillingHeaderText(s) {
			return system, false
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": s, "cache_control": mk()}}, true
	case []interface{}:
		for i := len(s) - 1; i >= 0; i-- {
			if m, ok := s[i].(map[string]interface{}); ok {
				text, _ := m["text"].(string)
				if isClaudeBillingHeaderText(text) {
					continue
				}
				if _, has := m["cache_control"]; has {
					return system, false
				}
				m["cache_control"] = mk()
				return s, true
			}
		}
	}
	return system, false
}

func isClaudeBillingHeaderText(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "x-anthropic-billing-header:")
}

func markListTail(v interface{}, mk func() map[string]interface{}) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for i := len(arr) - 1; i >= 0; i-- {
		if m, ok := arr[i].(map[string]interface{}); ok {
			if _, has := m["cache_control"]; has {
				return false
			}
			m["cache_control"] = mk()
			return true
		}
	}
	return false
}

func markClaudeNativeAutoContext(messages interface{}, mk func() map[string]interface{}) bool {
	msgs, ok := messages.([]interface{})
	if !ok || len(msgs) == 0 {
		return false
	}
	msg, ok := msgs[0].(map[string]interface{})
	if !ok || msg["role"] != "user" {
		return false
	}
	blocks, ok := msg["content"].([]interface{})
	if !ok || len(blocks) < 2 {
		return false
	}
	auto, ok := blocks[0].(map[string]interface{})
	if !ok {
		return false
	}
	if _, has := auto["cache_control"]; has {
		return false
	}
	if !anthropicwire.IsClaudeCodeAutoContextBlock(blocks[0], blocks[1]) {
		return false
	}
	auto["cache_control"] = mk()
	return true
}

func markSecondToLastUserTurn(messages interface{}, mk func() map[string]interface{}) bool {
	msgs, ok := messages.([]interface{})
	if !ok {
		return false
	}
	userIdx := make([]int, 0, len(msgs))
	for i, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if ok && m["role"] == "user" {
			userIdx = append(userIdx, i)
		}
	}
	if len(userIdx) < 2 {
		return false
	}
	return markMessageTail(msgs[userIdx[len(userIdx)-2]], mk)
}

// markMessageTail puts a cache_control breakpoint on the last content block of a
// message, converting a plain-string content into a single text block when needed.
func markMessageTail(msg interface{}, mk func() map[string]interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	switch c := m["content"].(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return false
		}
		m["content"] = []interface{}{map[string]interface{}{"type": "text", "text": c, "cache_control": mk()}}
		return true
	case []interface{}:
		for i := len(c) - 1; i >= 0; i-- {
			if bm, ok := c[i].(map[string]interface{}); ok {
				if _, has := bm["cache_control"]; has {
					return false
				}
				bm["cache_control"] = mk()
				return true
			}
		}
	}
	return false
}

// convertOpenAITools maps OpenAI function tools to Anthropic tools. Tools already
// in Anthropic shape (input_schema) or typed server tools (web_search, ...) are
// passed through unchanged so server-side tools keep working.
func convertOpenAITools(v interface{}) []interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]interface{}, 0, len(arr))
	for _, t := range arr {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := tm["input_schema"]; ok {
			out = append(out, tm)
			continue
		}
		fn, ok := tm["function"].(map[string]interface{})
		if !ok {
			if _, typed := tm["type"]; typed {
				out = append(out, tm) // typed built-in tool
			}
			continue
		}
		conv := map[string]interface{}{"name": stringOr(fn["name"], "")}
		if d, ok := fn["description"]; ok {
			conv["description"] = d
		}
		if params, ok := fn["parameters"]; ok {
			conv["input_schema"] = params
		} else {
			conv["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, conv)
	}
	return out
}

// convertToolChoice maps an OpenAI tool_choice to Anthropic's form. "none"
// returns nil (omit; Anthropic decides) since there is no universal equivalent.
func convertToolChoice(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "required":
			return map[string]interface{}{"type": "any"}
		}
	case map[string]interface{}:
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name := stringOr(fn["name"], ""); name != "" {
				return map[string]interface{}{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// anthropicContentToOpenAI splits Anthropic response content blocks into the
// concatenated assistant text and OpenAI-shaped tool_calls.
func anthropicContentToOpenAI(v interface{}) (string, []interface{}) {
	arr, ok := v.([]interface{})
	if !ok {
		return "", nil
	}
	var parts []string
	var toolCalls []interface{}
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			if s, ok := m["text"].(string); ok {
				parts = append(parts, s)
			}
		case "tool_use":
			args, _ := json.Marshal(m["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   stringOr(m["id"], ""),
				"type": "function",
				"function": map[string]interface{}{
					"name":      stringOr(m["name"], ""),
					"arguments": string(args),
				},
			})
		}
	}
	return strings.Join(parts, ""), toolCalls
}

func anthropicUsageToOpenAI(u map[string]interface{}) map[string]interface{} {
	in := numField(u, "input_tokens")
	out := numField(u, "output_tokens")
	return map[string]interface{}{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
}

// chatContentToText flattens OpenAI message content (a string, or an array of
// content parts) into plain text.
func chatContentToText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var parts []string
		for _, p := range t {
			if pm, ok := p.(map[string]interface{}); ok {
				if s, ok := pm["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func chatContentToAnthropicContent(v interface{}) (interface{}, bool) {
	switch t := v.(type) {
	case string:
		return t, false
	case []interface{}:
		blocks := make([]interface{}, 0, len(t))
		hasNonText := false
		var textParts []string
		for _, p := range t {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			block, nonText := openAIContentPartToAnthropic(pm)
			if block == nil {
				continue
			}
			if nonText {
				hasNonText = true
			}
			if !hasNonText {
				if text, ok := block.(map[string]interface{})["text"].(string); ok {
					textParts = append(textParts, text)
				}
			}
			blocks = append(blocks, block)
		}
		if hasNonText {
			return blocks, true
		}
		return strings.Join(textParts, ""), false
	default:
		return "", false
	}
}

func chatToolResultContent(v interface{}) interface{} {
	content, blocky := chatContentToAnthropicContent(v)
	if blocky {
		return content
	}
	return chatContentToText(v)
}

func openAIContentPartToAnthropic(part map[string]interface{}) (interface{}, bool) {
	typ, _ := part["type"].(string)
	switch typ {
	case "text", "input_text":
		return map[string]interface{}{"type": "text", "text": stringOr(part["text"], "")}, false
	case "image_url":
		image, _ := part["image_url"].(map[string]interface{})
		url := stringOr(mapGet(image, "url"), "")
		if url == "" {
			return nil, false
		}
		if mediaType, data, ok := parseDataURI(url); ok {
			return map[string]interface{}{
				"type": "image",
				"source": map[string]interface{}{
					"type":       "base64",
					"media_type": mediaType,
					"data":       data,
				},
			}, true
		}
		return map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type": "url",
				"url":  url,
			},
		}, true
	case "file":
		file, _ := part["file"].(map[string]interface{})
		fileData := stringOr(mapGet(file, "file_data"), "")
		mediaType, data, ok := parseDataURI(fileData)
		if !ok {
			return nil, false
		}
		return map[string]interface{}{
			"type": "document",
			"source": map[string]interface{}{
				"type":       "base64",
				"media_type": mediaType,
				"data":       data,
			},
		}, true
	default:
		return nil, false
	}
}

func parseDataURI(uri string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(uri, "data:") {
		return "", "", false
	}
	parts := strings.SplitN(uri, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	meta := strings.TrimPrefix(parts[0], "data:")
	if idx := strings.Index(meta, ";"); idx >= 0 {
		meta = meta[:idx]
	}
	if meta == "" {
		meta = "application/octet-stream"
	}
	return meta, parts[1], true
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstNum(m map[string]interface{}, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n := toInt64(v); n > 0 {
				return n
			}
		}
	}
	return 0
}

func numField(m map[string]interface{}, key string) int64 {
	return toInt64(m[key])
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func stringOr(v interface{}, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func mapGet(m map[string]interface{}, k string) interface{} {
	if m == nil {
		return nil
	}
	return m[k]
}

func toSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

func isToolResultContent(blocks []interface{}) bool {
	if len(blocks) == 0 {
		return false
	}
	if b, ok := blocks[0].(map[string]interface{}); ok {
		return b["type"] == "tool_result"
	}
	return false
}

func parseJSONObject(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return map[string]interface{}{}
		}
		if obj, err := decodeJSONValueUseNumber([]byte(t)); err == nil {
			return obj
		}
		return map[string]interface{}{}
	case map[string]interface{}:
		return t
	default:
		return map[string]interface{}{}
	}
}
