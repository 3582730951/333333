package kiro

import (
	"encoding/json"
	"strings"
)

// ContentLengthExceeded reports the explicit Kiro/AWS long-input failure. It is
// deliberately narrower than a generic HTTP 400 so cachePoint capability
// fallback and account health are never changed by an oversized conversation.
func ContentLengthExceeded(body []byte) bool {
	values := kiroErrorStrings(body)
	for _, value := range values {
		normalized := strings.ToUpper(strings.TrimSpace(value))
		switch normalized {
		case "CONTENT_LENGTH_EXCEEDS_THRESHOLD", "CONTENTLENGTHEXCEEDEDEXCEPTION":
			return true
		}
		lower := strings.ToLower(value)
		if strings.Contains(lower, "input is too long") || strings.Contains(lower, "content length exceeded") || strings.Contains(lower, "contentlengthexceededexception") {
			return true
		}
	}
	return false
}

// CachePointRejected reports whether an upstream validation error actually
// names the cachePoint field. A plain 400/422 is not evidence that cachePoint is
// unsupported; it may instead be a context, model, or malformed-content error.
func CachePointRejected(body []byte) bool {
	for _, value := range kiroErrorStrings(body) {
		normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(value))
		if strings.Contains(normalized, "cachepoint") {
			return true
		}
	}
	return false
}

func kiroErrorStrings(body []byte) []string {
	var root any
	if decodeUseNumber(body, &root) != nil {
		return []string{string(body)}
	}
	out := make([]string, 0, 8)
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case string:
			out = append(out, typed)
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(root)
	return out
}

// ReduceKiroMaxOutput returns an otherwise identical generated Kiro request
// with max_tokens capped for a context-error retry. This is attempted before
// dropping history because the 2026-07 regression was introduced by forcing
// 64k/128k output reservations on every request.
func ReduceKiroMaxOutput(body []byte, maximum int64) ([]byte, bool, error) {
	var root map[string]any
	if err := decodeUseNumber(body, &root); err != nil {
		return nil, false, err
	}
	fields, _ := root["additionalModelRequestFields"].(map[string]any)
	if fields == nil {
		return body, false, nil
	}
	current, ok := jsonNumberInt64(fields["max_tokens"])
	if !ok || current <= maximum || maximum <= 0 {
		return body, false, nil
	}
	fields["max_tokens"] = maximum
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

// TrimOldestKiroHistory drops complete oldest conversation groups until the
// serialized request is at most numerator/denominator of its starting size.
// The synthetic system pair and the group needed by a current tool_result are
// retained. Current message, tools, and system instructions are never truncated.
func TrimOldestKiroHistory(body []byte, numerator, denominator int) ([]byte, int, bool, error) {
	if numerator <= 0 || denominator <= 0 || numerator >= denominator {
		return body, 0, false, nil
	}
	var root map[string]any
	if err := decodeUseNumber(body, &root); err != nil {
		return nil, 0, false, err
	}
	state, _ := root["conversationState"].(map[string]any)
	history, _ := state["history"].([]any)
	if len(history) == 0 {
		return body, 0, false, nil
	}
	systemPrefix := kiroSystemHistoryPrefix(history)
	groups := kiroHistoryGroups(history, systemPrefix)
	if len(groups) == 0 {
		return body, 0, false, nil
	}
	removable := len(groups)
	if kiroCurrentHasToolResults(state) && removable > 0 {
		removable--
	}
	if removable == 0 {
		return body, 0, false, nil
	}
	targetBytes := len(body) * numerator / denominator
	updated := body
	droppedItems := 0
	for i := 0; i < removable && (len(updated) > targetBytes || i == 0); i++ {
		cutoff := groups[i].end
		trimmed := make([]any, 0, systemPrefix+len(history)-cutoff)
		trimmed = append(trimmed, history[:systemPrefix]...)
		trimmed = append(trimmed, history[cutoff:]...)
		if len(trimmed) == 0 {
			delete(state, "history")
		} else {
			state["history"] = trimmed
		}
		var err error
		updated, err = json.Marshal(root)
		if err != nil {
			return nil, 0, false, err
		}
		droppedItems = cutoff - systemPrefix
	}
	return updated, droppedItems, droppedItems > 0, nil
}

func RemoveKiroCachePoints(body []byte) ([]byte, bool, error) {
	var root any
	if err := decodeUseNumber(body, &root); err != nil {
		return nil, false, err
	}
	root, changed := removeKiroCachePoints(root)
	if !changed {
		return body, false, nil
	}
	updated, err := json.Marshal(root)
	return updated, err == nil, err
}

func removeKiroCachePoints(value any) (any, bool) {
	changed := false
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			if kiroCachePointOnlyObject(item) {
				changed = true
				continue
			}
			updated, itemChanged := removeKiroCachePoints(item)
			changed = itemChanged || changed
			out = append(out, updated)
		}
		return out, changed
	case map[string]any:
		for key, item := range typed {
			if strings.EqualFold(key, "cachePoint") {
				delete(typed, key)
				changed = true
				continue
			}
			updated, itemChanged := removeKiroCachePoints(item)
			typed[key] = updated
			changed = itemChanged || changed
		}
		return typed, changed
	}
	return value, false
}

func kiroCachePointOnlyObject(value any) bool {
	object, _ := value.(map[string]any)
	if len(object) != 1 {
		return false
	}
	for key := range object {
		return strings.EqualFold(key, "cachePoint")
	}
	return false
}

func jsonNumberInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		v, err := typed.Int64()
		return v, err == nil
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func kiroSystemHistoryPrefix(history []any) int {
	if len(history) < 2 {
		return 0
	}
	first, _ := history[0].(map[string]any)
	firstUser, _ := first["userInputMessage"].(map[string]any)
	second, _ := history[1].(map[string]any)
	secondAssistant, _ := second["assistantResponseMessage"].(map[string]any)
	content, _ := secondAssistant["content"].(string)
	if firstUser != nil && firstUser["modelId"] == nil && content == systemAcknowledgement {
		return 2
	}
	return 0
}

func kiroHistoryGroups(history []any, start int) []kiroHistoryGroup {
	groups := make([]kiroHistoryGroup, 0, len(history)-start)
	for index := start; index < len(history); index++ {
		if len(groups) == 0 || kiroHistoryItemStartsTurn(history[index]) {
			groups = append(groups, kiroHistoryGroup{start: index, end: index + 1})
			continue
		}
		groups[len(groups)-1].end = index + 1
	}
	return groups
}

func kiroHistoryItemStartsTurn(item any) bool {
	message, _ := item.(map[string]any)
	user, _ := message["userInputMessage"].(map[string]any)
	if user == nil {
		return false
	}
	context, _ := user["userInputMessageContext"].(map[string]any)
	results, _ := context["toolResults"].([]any)
	return len(results) == 0
}

func kiroCurrentHasToolResults(state map[string]any) bool {
	current, _ := state["currentMessage"].(map[string]any)
	user, _ := current["userInputMessage"].(map[string]any)
	context, _ := user["userInputMessageContext"].(map[string]any)
	results, _ := context["toolResults"].([]any)
	return len(results) > 0
}
