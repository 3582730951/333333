package anthropicwire

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
