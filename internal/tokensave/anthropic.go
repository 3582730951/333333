package tokensave

import "encoding/json"

// CompressAnthropicToolResults walks an Anthropic /v1/messages request body and applies
// Compress to the text of every tool_result block, returning the rewritten body and the
// number of bytes saved. ONLY tool_result content is touched — never the user's prompt,
// the system prompt, or assistant turns — because tool output (machine-generated, often
// huge) is where the tokens are and where the model is most robust to truncation. When
// nothing is compressed (or the body is not parseable) the original body is returned
// unchanged with saved=0, so this is a safe no-op on non-matching payloads.
//
// tool_result.content may be a plain string or an array of blocks; both forms are
// handled. The body is re-marshaled only when something actually changed.
func CompressAnthropicToolResults(body []byte, o Options) ([]byte, int) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return body, 0
	}
	msgs, ok := root["messages"].([]interface{})
	if !ok {
		return body, 0
	}
	saved := 0
	for _, mi := range msgs {
		m, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := m["content"].([]interface{})
		if !ok {
			continue
		}
		for _, bi := range content {
			b, ok := bi.(map[string]interface{})
			if !ok || b["type"] != "tool_result" {
				continue
			}
			switch c := b["content"].(type) {
			case string:
				if nc := Compress(c, o); nc != c {
					saved += len(c) - len(nc)
					b["content"] = nc
				}
			case []interface{}:
				for _, blki := range c {
					blk, ok := blki.(map[string]interface{})
					if !ok || blk["type"] != "text" {
						continue
					}
					if txt, ok := blk["text"].(string); ok {
						if nt := Compress(txt, o); nt != txt {
							saved += len(txt) - len(nt)
							blk["text"] = nt
						}
					}
				}
			}
		}
	}
	if saved == 0 {
		return body, 0
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return out, saved
}
