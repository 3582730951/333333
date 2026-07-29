package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	claudeAgentSDKIdentityLine = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	claudeCodeIdentityLine     = "You are Claude Code, Anthropic's official CLI for Claude."
)

var officialEnvBlockRE = regexp.MustCompile(`(?s)<env>.*?</env>`)

// rewriteBody only rewrites fields that are part of the gateway identity
// contract. In particular, it deliberately does not run substitutions over the
// complete request: messages, Skill instructions, tool input/result payloads,
// paths and encrypted/opaque strings are model or client state and must remain
// byte-for-byte equivalent after JSON decoding.
func rewriteBody(body []byte, identity *CachedIdentity) ([]byte, error) {
	if identity == nil || identity.Virtual == nil {
		return body, nil
	}

	root, err := decodeRawJSONObject(body)
	if err != nil {
		// Let the upstream endpoint produce its normal JSON error. The gateway
		// must not turn a malformed client request into an identity failure.
		return body, nil
	}

	metadataChanged, err := rewriteRawMetadataUserID(root, identity)
	if err != nil {
		return nil, err
	}
	systemChanged, err := rewriteRawOfficialSystem(root, identity)
	if err != nil {
		return nil, err
	}
	if !metadataChanged && !systemChanged {
		return body, nil
	}

	return marshalRawJSON(root)
}

// rewriteMetadataUserID rewrites only metadata.user_id. It is kept separate for
// focused callers/tests, while using RawMessage so unrelated numbers and opaque
// values never make a float64 round trip.
func rewriteMetadataUserID(body []byte, identity *CachedIdentity) ([]byte, error) {
	if identity == nil || identity.Virtual == nil {
		return body, nil
	}
	root, err := decodeRawJSONObject(body)
	if err != nil {
		return body, err
	}
	changed, err := rewriteRawMetadataUserID(root, identity)
	if err != nil || !changed {
		return body, err
	}
	return marshalRawJSON(root)
}

func decodeRawJSONObject(body []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var root map[string]json.RawMessage
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request body is not a JSON object")
	}

	// Reject a second JSON value while permitting trailing whitespace.
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("request body contains multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

func marshalRawJSON(value interface{}) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

func rewriteRawMetadataUserID(root map[string]json.RawMessage, identity *CachedIdentity) (bool, error) {
	rawMetadata, ok := root["metadata"]
	if !ok {
		return false, nil
	}

	var metadata map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(rawMetadata))
	decoder.UseNumber()
	if err := decoder.Decode(&metadata); err != nil || metadata == nil {
		// metadata is an extensible API field. Preserve unknown future shapes.
		return false, nil
	}
	if _, ok := metadata["user_id"]; !ok {
		return false, nil
	}

	virtualUserID, err := marshalRawJSON(struct {
		DeviceID    string `json:"device_id"`
		AccountUUID string `json:"account_uuid"`
		SessionID   string `json:"session_id"`
	}{
		DeviceID:    identity.Virtual.UserID,
		AccountUUID: "",
		SessionID:   identity.Virtual.SessionID,
	})
	if err != nil {
		return false, err
	}
	encodedUserID, err := marshalRawJSON(string(virtualUserID))
	if err != nil {
		return false, err
	}
	metadata["user_id"] = encodedUserID

	rewrittenMetadata, err := marshalRawJSON(metadata)
	if err != nil {
		return false, err
	}
	root["metadata"] = rewrittenMetadata
	return true, nil
}

// rewriteRawOfficialSystem changes environment descriptors only when the
// request has an explicit official Claude Code/Agent SDK identity system block.
// Merely containing "<env>" is not sufficient: users, Skills and tool payloads
// are allowed to discuss or emit that syntax.
func rewriteRawOfficialSystem(root map[string]json.RawMessage, identity *CachedIdentity) (bool, error) {
	rawSystem, ok := root["system"]
	if !ok {
		return false, nil
	}

	trimmed := bytes.TrimSpace(rawSystem)
	if len(trimmed) == 0 {
		return false, nil
	}

	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(rawSystem, &text); err != nil {
			return false, nil
		}
		if !isOfficialIdentitySystemText(text) {
			return false, nil
		}
		rewritten := rewriteEnvBlock([]byte(text), identity)
		if bytes.Equal(rewritten, []byte(text)) {
			return false, nil
		}
		encoded, err := marshalRawJSON(string(rewritten))
		if err != nil {
			return false, err
		}
		root["system"] = encoded
		return true, nil

	case '[':
		var blocks []json.RawMessage
		decoder := json.NewDecoder(bytes.NewReader(rawSystem))
		decoder.UseNumber()
		if err := decoder.Decode(&blocks); err != nil {
			return false, nil
		}

		type parsedTextBlock struct {
			index  int
			fields map[string]json.RawMessage
			text   string
		}
		textBlocks := make([]parsedTextBlock, 0, len(blocks))
		official := false
		for i, rawBlock := range blocks {
			var fields map[string]json.RawMessage
			decoder := json.NewDecoder(bytes.NewReader(rawBlock))
			decoder.UseNumber()
			if err := decoder.Decode(&fields); err != nil || fields == nil {
				continue
			}
			var blockType string
			if rawType, exists := fields["type"]; exists {
				if err := json.Unmarshal(rawType, &blockType); err != nil || blockType != "text" {
					continue
				}
			} else {
				// Anthropic system blocks are explicitly typed. Do not infer a
				// future/third-party object with a text member to be identity.
				continue
			}
			var text string
			if err := json.Unmarshal(fields["text"], &text); err != nil {
				continue
			}
			if isOfficialIdentitySystemText(text) {
				official = true
			}
			textBlocks = append(textBlocks, parsedTextBlock{index: i, fields: fields, text: text})
		}
		if !official {
			return false, nil
		}

		changed := false
		for _, block := range textBlocks {
			rewritten := rewriteEnvBlock([]byte(block.text), identity)
			if bytes.Equal(rewritten, []byte(block.text)) {
				continue
			}
			encodedText, err := marshalRawJSON(string(rewritten))
			if err != nil {
				return false, err
			}
			block.fields["text"] = encodedText
			rewrittenBlock, err := marshalRawJSON(block.fields)
			if err != nil {
				return false, err
			}
			blocks[block.index] = rewrittenBlock
			changed = true
		}
		if !changed {
			return false, nil
		}

		rewrittenSystem, err := marshalRawJSON(blocks)
		if err != nil {
			return false, err
		}
		root["system"] = rewrittenSystem
		return true, nil
	}

	return false, nil
}

func isOfficialIdentitySystemText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, claudeAgentSDKIdentityLine) ||
		strings.HasPrefix(trimmed, claudeCodeIdentityLine)
}

// rewriteEnvBlock rewrites only explicit descriptor lines inside <env> blocks.
// Working directories, home paths, usernames, DNS values and free text are
// intentionally untouched because they can be consumed verbatim by tools.
func rewriteEnvBlock(body []byte, identity *CachedIdentity) []byte {
	if identity == nil || identity.Virtual == nil {
		return body
	}
	return officialEnvBlockRE.ReplaceAllFunc(body, func(match []byte) []byte {
		content := string(match)
		content = replaceEnvMarkerValue(content, "Platform:", identity.Virtual.OSName)
		content = replaceEnvMarkerValue(content, "OS Version:", identity.Virtual.OSRelease)
		content = replaceEnvMarkerValue(content, "Terminal:", identity.Virtual.Terminal)
		content = replaceEnvMarkerValue(content, "Hostname:", identity.Virtual.Hostname)
		content = replaceEnvMarkerValue(content, "Architecture:", identity.Virtual.Arch)
		return []byte(content)
	})
}

// replaceEnvMarkerValue changes a marker only when it begins a line (allowing
// indentation). This avoids rewriting prose that happens to contain the same
// words.
func replaceEnvMarkerValue(text, marker, value string) string {
	if value == "" {
		return text
	}

	lines := strings.SplitAfter(text, "\n")
	for i, line := range lines {
		lineEnding := ""
		content := line
		if strings.HasSuffix(content, "\n") {
			lineEnding = "\n"
			content = strings.TrimSuffix(content, "\n")
		}
		if strings.HasSuffix(content, "\r") {
			lineEnding = "\r" + lineEnding
			content = strings.TrimSuffix(content, "\r")
		}

		leading := len(content) - len(strings.TrimLeft(content, " \t"))
		if !strings.HasPrefix(content[leading:], marker) {
			continue
		}
		valueStart := leading + len(marker)
		if valueStart < len(content) && content[valueStart] != ' ' && content[valueStart] != '\t' {
			continue
		}
		for valueStart < len(content) && (content[valueStart] == ' ' || content[valueStart] == '\t') {
			valueStart++
		}
		lines[i] = content[:valueStart] + value + lineEnding
	}
	return strings.Join(lines, "")
}
