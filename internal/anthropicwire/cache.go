package anthropicwire

import (
	"sort"
	"strings"
)

// NormalizeCacheControlTTL keeps Anthropic cache_control TTLs valid in
// evaluation order. A 1h breakpoint cannot appear after a default/5m breakpoint,
// so later 1h markers are downgraded by removing their ttl field.
func NormalizeCacheControlTTL(root map[string]interface{}) bool {
	seenShort := false
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
			cc, ok := block["cache_control"].(map[string]interface{})
			if !ok {
				continue
			}
			if ttl, _ := cc["ttl"].(string); ttl == "1h" {
				if seenShort {
					delete(cc, "ttl")
					changed = true
				}
				continue
			}
			seenShort = true
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
			visit(m["content"])
		}
	}
	return changed
}

// NormalizeCacheControlTTLForPolicy upgrades retained prompt-cache breakpoints to
// the configured extended TTL before enforcing Anthropic's TTL ordering rule.
func NormalizeCacheControlTTLForPolicy(root map[string]interface{}, ttl string) bool {
	changed := upgradeCacheControlTTL(root, ttl)
	if NormalizeCacheControlTTL(root) {
		changed = true
	}
	return changed
}

func upgradeCacheControlTTL(root map[string]interface{}, ttl string) bool {
	if strings.TrimSpace(ttl) != "1h" {
		return false
	}
	changed := false
	visitPromptBlocks(root, func(block map[string]interface{}, _ string, _, _ int) {
		cc, ok := block["cache_control"].(map[string]interface{})
		if !ok {
			if _, has := block["cache_control"]; !has {
				return
			}
			block["cache_control"] = map[string]interface{}{"type": "ephemeral", "ttl": "1h"}
			changed = true
			return
		}
		if cc["type"] != "ephemeral" {
			cc["type"] = "ephemeral"
			changed = true
		}
		if cc["ttl"] != "1h" {
			cc["ttl"] = "1h"
			changed = true
		}
	})
	return changed
}

// SanitizeVolatileCacheControls removes cache breakpoints from request regions
// that change every turn. It preserves Claude Code's stable latest-user
// auto-context block, but strips markers from the latest real user request,
// latest tool results/images, message-level latest-user markers, and billing.
func SanitizeVolatileCacheControls(root map[string]interface{}) bool {
	changed := false
	if stripBillingCacheControl(root) {
		changed = true
	}
	if stripLatestUserVolatileCacheControl(root) {
		changed = true
	}
	return changed
}

func stripBillingCacheControl(root map[string]interface{}) bool {
	sys, ok := root["system"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	for _, item := range sys {
		block, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := block["text"].(string)
		if isClaudeBillingHeaderText(text) {
			if _, has := block["cache_control"]; has {
				delete(block, "cache_control")
				changed = true
			}
		}
	}
	return changed
}

func stripLatestUserVolatileCacheControl(root map[string]interface{}) bool {
	msgs, ok := root["messages"].([]interface{})
	if !ok {
		return false
	}
	idx := latestUserIndex(msgs)
	if idx < 0 {
		return false
	}
	msg, ok := msgs[idx].(map[string]interface{})
	if !ok {
		return false
	}
	changed := false
	if _, has := msg["cache_control"]; has {
		delete(msg, "cache_control")
		changed = true
	}
	switch content := msg["content"].(type) {
	case map[string]interface{}:
		if _, has := content["cache_control"]; has {
			delete(content, "cache_control")
			changed = true
		}
	case []interface{}:
		keepAutoContext := -1
		if len(content) >= 2 && IsClaudeCodeAutoContextBlock(content[0], content[1]) {
			keepAutoContext = 0
		}
		for i, item := range content {
			block, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if i == keepAutoContext {
				continue
			}
			if _, has := block["cache_control"]; has {
				delete(block, "cache_control")
				changed = true
			}
		}
	}
	return changed
}

// CapCacheControlBreakpoints enforces Anthropic's four-breakpoint limit while
// preferring stable prompt prefixes over volatile or lower-value markers.
func CapCacheControlBreakpoints(root map[string]interface{}, max int) bool {
	if max < 0 {
		max = 0
	}
	refs := cacheControlRefs(root)
	if len(refs) <= max {
		return false
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].priority != refs[j].priority {
			return refs[i].priority < refs[j].priority
		}
		return refs[i].order < refs[j].order
	})
	for _, ref := range refs[max:] {
		delete(ref.block, "cache_control")
	}
	return true
}

type cacheControlRef struct {
	block    map[string]interface{}
	priority int
	order    int
}

func cacheControlRefs(root map[string]interface{}) []cacheControlRef {
	refs := []cacheControlRef{}
	order := 0
	toolTail := lastBlockIndex(root["tools"], nil)
	systemTail := lastBlockIndex(root["system"], func(block map[string]interface{}) bool {
		text, _ := block["text"].(string)
		return !isClaudeBillingHeaderText(text)
	})
	msgs, _ := root["messages"].([]interface{})
	latestUser := latestUserIndex(msgs)

	collectList := func(v interface{}, section string, msgIdx int) {
		arr, ok := v.([]interface{})
		if !ok {
			return
		}
		for blockIdx, item := range arr {
			block, ok := item.(map[string]interface{})
			if !ok {
				order++
				continue
			}
			if _, has := block["cache_control"]; has {
				refs = append(refs, cacheControlRef{
					block:    block,
					priority: cacheControlPriority(root, section, msgIdx, blockIdx, toolTail, systemTail, latestUser),
					order:    order,
				})
			}
			order++
		}
	}

	collectList(root["tools"], "tools", -1)
	collectList(root["system"], "system", -1)
	for msgIdx, msg := range msgs {
		m, ok := msg.(map[string]interface{})
		if !ok {
			order++
			continue
		}
		if _, has := m["cache_control"]; has {
			refs = append(refs, cacheControlRef{block: m, priority: cacheControlPriority(root, "message", msgIdx, -1, toolTail, systemTail, latestUser), order: order})
		}
		order++
		collectList(m["content"], "message_content", msgIdx)
	}
	return refs
}

func cacheControlPriority(root map[string]interface{}, section string, msgIdx, blockIdx, toolTail, systemTail, latestUser int) int {
	switch section {
	case "tools":
		if blockIdx == toolTail {
			return 10
		}
		return 60
	case "system":
		sys, _ := root["system"].([]interface{})
		if blockIdx >= 0 && blockIdx < len(sys) {
			if block, ok := sys[blockIdx].(map[string]interface{}); ok {
				text, _ := block["text"].(string)
				if isClaudeBillingHeaderText(text) {
					return 1000
				}
			}
		}
		if blockIdx == systemTail {
			return 20
		}
		return 30
	case "message", "message_content":
		if msgIdx == latestUser {
			if section == "message" {
				return 900
			}
			if latestUserBlockIsAutoContext(root, msgIdx, blockIdx) {
				return 25
			}
			return 900
		}
		return 40
	default:
		return 100
	}
}

func latestUserBlockIsAutoContext(root map[string]interface{}, msgIdx, blockIdx int) bool {
	if blockIdx != 0 {
		return false
	}
	msgs, ok := root["messages"].([]interface{})
	if !ok || msgIdx < 0 || msgIdx >= len(msgs) {
		return false
	}
	msg, ok := msgs[msgIdx].(map[string]interface{})
	if !ok {
		return false
	}
	blocks, ok := msg["content"].([]interface{})
	if !ok || len(blocks) < 2 {
		return false
	}
	return IsClaudeCodeAutoContextBlock(blocks[0], blocks[1])
}

func lastBlockIndex(v interface{}, include func(map[string]interface{}) bool) int {
	arr, ok := v.([]interface{})
	if !ok {
		return -1
	}
	for i := len(arr) - 1; i >= 0; i-- {
		block, ok := arr[i].(map[string]interface{})
		if !ok {
			continue
		}
		if include == nil || include(block) {
			return i
		}
	}
	return -1
}

func visitPromptBlocks(root map[string]interface{}, fn func(block map[string]interface{}, section string, msgIdx, blockIdx int)) {
	visitList := func(v interface{}, section string, msgIdx int) {
		arr, ok := v.([]interface{})
		if !ok {
			return
		}
		for i, item := range arr {
			if block, ok := item.(map[string]interface{}); ok {
				fn(block, section, msgIdx, i)
			}
		}
	}
	visitList(root["tools"], "tools", -1)
	visitList(root["system"], "system", -1)
	if msgs, ok := root["messages"].([]interface{}); ok {
		for msgIdx, msg := range msgs {
			if m, ok := msg.(map[string]interface{}); ok {
				fn(m, "message", msgIdx, -1)
				visitList(m["content"], "message_content", msgIdx)
			}
		}
	}
}

func latestUserIndex(msgs []interface{}) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if ok && msg["role"] == "user" {
			return i
		}
	}
	return -1
}

// IsClaudeCodeAutoContextBlock reports whether block is Claude Code's stable
// auto-context prefix immediately followed by the real user request.
func IsClaudeCodeAutoContextBlock(block, next interface{}) bool {
	auto, ok := block.(map[string]interface{})
	if !ok {
		return false
	}
	if typ, _ := auto["type"].(string); typ != "" && typ != "text" {
		return false
	}
	text, _ := auto["text"].(string)
	if !strings.HasPrefix(strings.TrimSpace(text), "<system-reminder>") ||
		!strings.Contains(text, "As you answer the user's questions, you can use the following context:") {
		return false
	}
	nextBlock, ok := next.(map[string]interface{})
	if !ok {
		return false
	}
	if typ, _ := nextBlock["type"].(string); typ != "" && typ != "text" {
		return false
	}
	nextText, _ := nextBlock["text"].(string)
	return strings.TrimSpace(nextText) != "" && !strings.HasPrefix(strings.TrimSpace(nextText), "<system-reminder>")
}

func isClaudeBillingHeaderText(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "x-anthropic-billing-header:")
}
