package superinstruct

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// InjectSystem is the headless M1 SystemPromptInjector port. It intentionally
// mirrors the source project's supported carriers and replacement semantics:
// direct instruction fields, Chat messages, and Responses input items.
func InjectSystem(raw []byte, instructions string) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, false, errors.New("Super-Instruct M1 request contains trailing JSON data")
	}
	root, object := value.(map[string]interface{})
	if !object {
		return raw, false, nil
	}
	injected := false
	for _, field := range []string{"instructions", "system", "system_prompt", "personality"} {
		if _, exists := root[field]; exists {
			root[field] = instructions
			injected = true
		}
	}
	if messages, ok := root["messages"].([]interface{}); ok {
		found := false
		for _, value := range messages {
			message, object := value.(map[string]interface{})
			if !object || message["role"] != "system" {
				continue
			}
			message["content"] = instructions
			found = true
			injected = true
		}
		if !found {
			root["messages"] = append([]interface{}{map[string]interface{}{
				"role": "system", "content": instructions,
			}}, messages...)
			injected = true
		}
	}
	if input, ok := root["input"].([]interface{}); ok {
		found := false
		for _, value := range input {
			item, object := value.(map[string]interface{})
			if !object || item["role"] != "system" {
				continue
			}
			if content, array := item["content"].([]interface{}); array {
				for _, partValue := range content {
					if part, object := partValue.(map[string]interface{}); object {
						part["text"] = instructions
					}
				}
			} else {
				item["content"] = instructions
			}
			found = true
			injected = true
		}
		if !found {
			root["input"] = append([]interface{}{map[string]interface{}{
				"role": "system",
				"content": []interface{}{map[string]interface{}{
					"type": "input_text", "text": instructions,
				}},
			}}, input...)
			injected = true
		}
	}
	if !injected {
		return raw, false, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}
