package storage

// Codex remote compaction replaces the active model history with a bounded set of
// real user messages followed by one opaque `compaction` item. Durable CPA replay
// must mirror that replacement boundary. Appending every pre-compaction segment
// again defeats the official compaction and can expand a 372k session into a
// multi-million-token fresh-root request after an account migration.

import (
	"strings"
	"unicode/utf8"
)

const (
	// Keep this synchronized with RETAINED_MESSAGE_TOKEN_BUDGET in Codex
	// core/src/compact_remote_v2.rs.
	codexRemoteCompactionRetainedMessageTokenBudget = 64_000
	codexApproxBytesPerToken                        = 4
)

// appendGoalReplayTurn appends one durable segment. A successful Codex remote
// compaction is a history replacement, not an ordinary assistant output: retain
// the same user-message shape as Codex and discard the now-encapsulated tool,
// reasoning, and assistant items before installing the opaque compaction item.
func appendGoalReplayTurn(items []interface{}, turn goalReplaySegment, historyKey string) []interface{} {
	if historyKey == "messages" {
		if len(turn.ReplacementHistory) > 0 {
			return append([]interface{}(nil), turn.ReplacementHistory...)
		}
		if turn.ReplaceInput {
			items = appendGoalItems(nil, turn.Input)
		} else {
			items = appendGoalItems(items, turn.Input)
		}
		return append(items, claudeAssistantMessages(turn.Output)...)
	}

	// Responses Lite request state has two different lifetimes: the leading tool /
	// developer prefix is current request configuration, while the remaining items
	// are semantic history. Keep the latest prefix at input[0] without letting every
	// wire turn append another copy into the history.
	prefix, history := codexSeparateLiteReplayPrefix(items)
	turnPrefix, turnInput := codexSeparateLiteReplayPrefix(appendGoalItems(nil, turn.Input))
	if len(turnPrefix) > 0 {
		prefix = turnPrefix
	}
	if len(turn.ReplacementPrefix) > 0 {
		prefix = append([]interface{}(nil), turn.ReplacementPrefix...)
	}
	if len(turn.ReplacementHistory) > 0 {
		// RemoteCompactionV2 installs replacement_history atomically. The trigger
		// request and compaction response are evidence for the checkpoint, not
		// additional history after it.
		return append(append([]interface{}(nil), prefix...), turn.ReplacementHistory...)
	}
	if turn.ReplaceInput {
		history = turnInput
	} else {
		history = append(history, turnInput...)
	}
	// Compatibility for segments written before replacement_history existed.
	// Requiring the request trigger prevents an unrelated output item (or the
	// legacy /responses/compact endpoint) from erasing durable history.
	if !turn.CodexCompactionEvaluated && codexRemoteCompactionTrigger(turn.Input) {
		if compacted, ok := codexCompactionOutput(turn.Output); ok {
			replacement := codexRemoteCompactedHistory(history, compacted)
			return append(append([]interface{}(nil), prefix...), replacement...)
		}
	}
	history = appendGoalItems(history, turn.Output)
	return append(append([]interface{}(nil), prefix...), history...)
}

func codexSeparateLiteReplayPrefix(items []interface{}) ([]interface{}, []interface{}) {
	var latestPrefix []interface{}
	history := make([]interface{}, 0, len(items))
	for index := 0; index < len(items); {
		if !codexStorageLiteAdditionalTools(items[index]) {
			history = append(history, items[index])
			index++
			continue
		}
		end := index + 1
		for end < len(items) && codexStorageDeveloperMessage(items[end]) {
			end++
		}
		latestPrefix = append([]interface{}(nil), items[index:end]...)
		index = end
	}
	return latestPrefix, history
}

func codexStorageLiteAdditionalTools(raw interface{}) bool {
	item, ok := raw.(map[string]interface{})
	if !ok || streamStringStorage(item["type"]) != "additional_tools" || streamStringStorage(item["role"]) != "developer" {
		return false
	}
	_, ok = item["tools"].([]interface{})
	return ok
}

func codexStorageDeveloperMessage(raw interface{}) bool {
	item, ok := raw.(map[string]interface{})
	return ok && streamStringStorage(item["type"]) == "message" && streamStringStorage(item["role"]) == "developer"
}

// codexCompactionOutput accepts the wire spelling used by current Codex and the
// serde alias accepted by the official protocol. RemoteCompactionV2 succeeds only
// when exactly one compaction item is returned; other output items are ignored.
func codexCompactionOutput(raw interface{}) (interface{}, bool) {
	candidates, ok := raw.([]interface{})
	if !ok {
		return nil, false
	}
	var found interface{}
	count := 0
	for _, candidate := range candidates {
		item, ok := candidate.(map[string]interface{})
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(streamStringStorage(item["type"])))
		if kind != "compaction" && kind != "compaction_summary" {
			continue
		}
		count++
		// ResponseItem::Compaction requires encrypted_content to be a string.
		// The official stream fails before installing history when that field is
		// absent or malformed, so accepting it here would erase the last durable
		// pre-compaction history on a response the client itself rejects.
		if _, valid := item["encrypted_content"].(string); !valid {
			return nil, false
		}
		if kind == "compaction_summary" {
			// serde accepts compaction_summary as an input alias but serializes the
			// installed ResponseItem back with the canonical compaction spelling.
			canonical := make(map[string]interface{}, len(item))
			for key, value := range item {
				canonical[key] = value
			}
			canonical["type"] = "compaction"
			found = canonical
		} else {
			found = candidate
		}
	}
	return found, count == 1
}

func codexRemoteCompactionTrigger(raw interface{}) bool {
	items := appendGoalItems(nil, raw)
	if len(items) == 0 {
		return false
	}
	last, ok := items[len(items)-1].(map[string]interface{})
	return ok && strings.EqualFold(strings.TrimSpace(streamStringStorage(last["type"])), "compaction_trigger")
}

// CodexRemoteCompactionV2Replacement derives the exact durable baseline installed
// by the official client after a successful V2 compact response. Callers must pass
// the logical prompt input (durable previous-response state plus this wire delta),
// not merely an incremental WebSocket body.
func CodexRemoteCompactionV2Replacement(input, output interface{}) ([]interface{}, bool) {
	items, ok := input.([]interface{})
	if !ok {
		return nil, false
	}
	if !codexRemoteCompactionTrigger(input) {
		return nil, false
	}
	compacted, ok := codexCompactionOutput(output)
	if !ok {
		return nil, false
	}
	items = items[:len(items)-1]
	return codexRemoteCompactedHistory(items, compacted), true
}

func codexRemoteCompactedHistory(input []interface{}, compacted interface{}) []interface{} {
	retained := make([]interface{}, 0, len(input)+1)
	for _, raw := range input {
		item, ok := raw.(map[string]interface{})
		if !ok || !codexRetainsMessageAfterRemoteCompaction(item) {
			continue
		}
		retained = append(retained, raw)
	}
	retained = codexTruncateRetainedMessages(retained, codexRemoteCompactionRetainedMessageTokenBudget)
	return append(retained, compacted)
}

// codexRetainsMessageAfterRemoteCompaction mirrors
// is_retained_for_remote_compaction_v2 + should_keep_compacted_history_item:
// only real user messages and visible hook prompts survive. Context fragments are
// injected afresh by Codex and retaining stale copies can contradict the current
// environment, permissions, skills, or AGENTS.md instructions.
func codexRetainsMessageAfterRemoteCompaction(item map[string]interface{}) bool {
	if !strings.EqualFold(strings.TrimSpace(streamStringStorage(item["role"])), "user") {
		return false
	}
	if kind := strings.TrimSpace(streamStringStorage(item["type"])); kind != "" && !strings.EqualFold(kind, "message") {
		return false
	}

	content, ok := item["content"].([]interface{})
	if !ok {
		// Older compatible clients used a plain string. It is a real user message;
		// current Codex's typed ResponseItem always uses a content array.
		_, isText := item["content"].(string)
		return isText
	}
	hasHook := false
	allHookOrContext := len(content) > 0
	anyContext := false
	for _, raw := range content {
		part, ok := raw.(map[string]interface{})
		if !ok || !strings.EqualFold(strings.TrimSpace(streamStringStorage(part["type"])), "input_text") {
			allHookOrContext = false
			continue
		}
		text := streamStringStorage(part["text"])
		if codexHookPromptFragment(text) {
			hasHook = true
			anyContext = true
			continue
		}
		if codexContextualUserText(text) {
			anyContext = true
			continue
		}
		allHookOrContext = false
	}
	if hasHook && allHookOrContext {
		return true
	}
	return !anyContext
}

func codexHookPromptFragment(text string) bool {
	trimmed := strings.TrimSpace(text)
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<hook_prompt ") && strings.Contains(lower, "hook_run_id=") && strings.HasSuffix(lower, "</hook_prompt>")
}

// This is the user-role fragment registry used by Codex 0.146.0's
// is_contextual_user_fragment. Prefix matching is deliberately narrow so an
// arbitrary user-authored XML tag is never discarded as internal context.
func codexContextualUserText(text string) bool {
	trimmed := strings.TrimSpace(text)
	for _, markers := range [][2]string{
		{"# AGENTS.md instructions", "</INSTRUCTIONS>"},
		{"<environment_context>", "</environment_context>"},
		{"<skill>", "</skill>"},
		{"<user_shell_command>", "</user_shell_command>"},
		{"<turn_aborted>", "</turn_aborted>"},
		{"<subagent_notification>", "</subagent_notification>"},
		{"<goal_context>", "</goal_context>"},
		{"<recommended_plugins>", "</recommended_plugins>"},
	} {
		if strings.HasPrefix(trimmed, markers[0]) && strings.HasSuffix(trimmed, markers[1]) {
			return true
		}
	}
	if codexExternalContextText(trimmed) || codexInternalContextText(trimmed) {
		return true
	}
	return strings.HasPrefix(trimmed, "Warning: The maximum number of unified exec processes you can keep open is") ||
		(strings.HasPrefix(trimmed, "Warning: apply_patch was requested via ") && strings.HasSuffix(trimmed, "Use the apply_patch tool instead of exec_command.")) ||
		strings.HasPrefix(trimmed, "Warning: Your account was flagged for potentially high-risk cyber activity")
}

func codexExternalContextText(text string) bool {
	const prefix = "<external_"
	if !strings.HasPrefix(text, prefix) {
		return false
	}
	end := strings.IndexByte(text[len(prefix):], '>')
	if end < 0 {
		return false
	}
	key := text[len(prefix) : len(prefix)+end]
	return key != "" && strings.HasSuffix(text, "</external_"+key+">")
}

func codexInternalContextText(text string) bool {
	const prefix = "<codex_internal_context source=\""
	const openEnd = "\">"
	const closeTag = "</codex_internal_context>"
	if !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, closeTag) {
		return false
	}
	rest := text[len(prefix):]
	end := strings.Index(rest, openEnd)
	if end <= 0 {
		return false
	}
	for index, value := range rest[:end] {
		if (index == 0 && (value < 'a' || value > 'z')) ||
			(index > 0 && !((value >= 'a' && value <= 'z') || (value >= '0' && value <= '9') || value == '_')) {
			return false
		}
	}
	return true
}

// codexTruncateRetainedMessages mirrors the newest-first 64k text-token budget in
// truncate_retained_messages_for_remote_compaction. Images/audio are retained and
// contribute no text tokens there; unknown content is likewise preserved here and
// remains subject to the final whole-request replay guard.
func codexTruncateRetainedMessages(items []interface{}, maxTokens int) []interface{} {
	remaining := maxTokens
	reversed := make([]interface{}, 0, len(items))
	for index := len(items) - 1; index >= 0; index-- {
		if remaining == 0 {
			continue
		}
		item, _ := items[index].(map[string]interface{})
		tokens := codexMessageTextTokenCount(item)
		if tokens < 1 {
			tokens = 1
		}
		if tokens <= remaining {
			reversed = append(reversed, items[index])
			remaining -= tokens
			continue
		}
		if truncated := codexTruncateMessageText(item, remaining); truncated != nil {
			reversed = append(reversed, truncated)
		}
		remaining = 0
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func codexMessageTextTokenCount(item map[string]interface{}) int {
	if item == nil {
		return 0
	}
	if text, ok := item["content"].(string); ok {
		return codexApproxTokenCount(text)
	}
	content, _ := item["content"].([]interface{})
	total := 0
	for _, raw := range content {
		part, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(streamStringStorage(part["type"])))
		if kind == "input_text" || kind == "output_text" {
			text, _ := part["text"].(string)
			total += codexApproxTokenCount(text)
		}
	}
	return total
}

func codexApproxTokenCount(text string) int {
	return (len(text) + codexApproxBytesPerToken - 1) / codexApproxBytesPerToken
}

func codexTruncateMessageText(item map[string]interface{}, maxTokens int) map[string]interface{} {
	if item == nil || maxTokens <= 0 {
		return nil
	}
	clone := make(map[string]interface{}, len(item))
	for key, value := range item {
		clone[key] = value
	}
	if text, ok := item["content"].(string); ok {
		clone["content"] = codexTruncateMiddleTokens(text, maxTokens)
		return clone
	}
	content, _ := item["content"].([]interface{})
	remaining := maxTokens
	out := make([]interface{}, 0, len(content))
	for _, raw := range content {
		part, ok := raw.(map[string]interface{})
		if !ok {
			out = append(out, raw)
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(streamStringStorage(part["type"])))
		if kind != "input_text" && kind != "output_text" {
			out = append(out, raw)
			continue
		}
		if remaining == 0 {
			continue
		}
		text, _ := part["text"].(string)
		tokens := codexApproxTokenCount(text)
		if tokens <= remaining {
			out = append(out, raw)
			remaining -= tokens
			continue
		}
		partClone := make(map[string]interface{}, len(part))
		for key, value := range part {
			partClone[key] = value
		}
		partClone["text"] = codexTruncateMiddleTokens(text, remaining)
		out = append(out, partClone)
		remaining = 0
	}
	if len(out) == 0 {
		return nil
	}
	clone["content"] = out
	return clone
}

func codexTruncateMiddleTokens(text string, maxTokens int) string {
	maxBytes := maxTokens * codexApproxBytesPerToken
	if maxTokens > 0 && len(text) <= maxBytes {
		return text
	}
	if maxBytes <= 0 {
		return ""
	}
	leftBudget := maxBytes / 2
	rightBudget := maxBytes - leftBudget
	leftEnd := leftBudget
	for leftEnd > 0 && leftEnd < len(text) && !utf8.RuneStart(text[leftEnd]) {
		leftEnd--
	}
	rightStart := len(text) - rightBudget
	for rightStart < len(text) && !utf8.RuneStart(text[rightStart]) {
		rightStart++
	}
	if rightStart < leftEnd {
		rightStart = leftEnd
	}
	removedTokens := (len(text) - maxBytes + codexApproxBytesPerToken - 1) / codexApproxBytesPerToken
	return text[:leftEnd] + "…" + streamIntStorage(removedTokens) + " tokens truncated…" + text[rightStart:]
}

func streamIntStorage(value int) string {
	// strconv.Itoa is kept behind this tiny helper so the compaction code remains
	// independent from JSON number formatting elsewhere in the storage package.
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [24]byte
	index := len(buf)
	for value > 0 {
		index--
		buf[index] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		index--
		buf[index] = '-'
	}
	return string(buf[index:])
}
