package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

func claudeOnlyCapabilitiesForChatBridge(h http.Header, raw []byte) []string {
	var caps []string
	if beta := strings.TrimSpace(h.Get("Anthropic-Beta")); beta != "" {
		caps = append(caps, "anthropic_beta:"+beta)
	}
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return caps
	}
	for _, key := range []string{"context_management", "output_config"} {
		if _, ok := root[key]; ok {
			caps = append(caps, "claude_"+key)
		}
	}
	if claudeMessagesContainImage(root["messages"]) {
		caps = append(caps, "claude_image")
	}
	return caps
}

func claudeMessagesContainImage(v interface{}) bool {
	msgs, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, item := range msgs {
		msg, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		for _, block := range blocks {
			bm, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if typ, _ := bm["type"].(string); typ == "image" {
				return true
			}
		}
	}
	return false
}
