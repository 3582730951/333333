package anthropicwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"unicode/utf8"
)

// claudeRootKeyOrder is the top-level insertion order emitted by the shipping
// Claude Code client.  encoding/json sorts map keys, which turns every body the
// gateway touches into a stable Go-specific wire fingerprint.  Known root keys
// are projected back into the native order; future/unknown keys retain their
// input position.
var claudeRootKeyOrder = []string{
	"model",
	"messages",
	"system",
	"tools",
	"metadata",
	"max_tokens",
	"thinking",
	"context_management",
	"output_config",
	"stream",
}

// jsonOrder records object insertion order without replacing the map-based
// representation used by the existing Claude normalizers.  Children are kept
// by key/index so nested tool schemas and message content preserve their exact
// downstream order as well.
type jsonOrder struct {
	keys     []string
	fields   map[string]*jsonOrder
	elements []*jsonOrder
}

// MarshalPreservingOrder serializes a mutated Claude request while retaining
// the key order of original.  This is deliberately narrower than a general
// ordered-JSON API: the normalizers decode JSON objects into
// map[string]interface{}, mutate them, and call this function with the bytes
// from which that map was decoded.
//
// Existing keys keep their original relative order at every nesting level.
// Newly inserted keys are deterministic and use the native Claude shape where
// it is known (for example type/text/cache_control and type/ttl).  Top-level
// Claude request keys always use claudeRootKeyOrder so a non-Claude-compatible
// caller normalized into a Claude Code request gets the same outer wire shape.
func MarshalPreservingOrder(original []byte, root map[string]interface{}) ([]byte, error) {
	order, err := parseJSONOrder(original)
	if err != nil {
		// A valid root was already decoded by the caller, so failure to recover
		// optional ordering metadata must not make the request unusable.
		order = nil
	}
	var out bytes.Buffer
	if err := marshalOrderedValue(&out, root, order, "", true); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func parseJSONOrder(raw []byte) (*jsonOrder, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	node, err := readJSONOrder(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, err
	}
	return node, nil
}

func readJSONOrder(decoder *json.Decoder) (*jsonOrder, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil, nil
	}
	switch delim {
	case '{':
		node := &jsonOrder{fields: map[string]*jsonOrder{}}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("JSON object key is %T", keyToken)
			}
			child, err := readJSONOrder(decoder)
			if err != nil {
				return nil, err
			}
			node.keys = append(node.keys, key)
			node.fields[key] = child
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim('}') {
			return nil, fmt.Errorf("unterminated JSON object: %v", err)
		}
		return node, nil
	case '[':
		node := &jsonOrder{}
		for decoder.More() {
			child, err := readJSONOrder(decoder)
			if err != nil {
				return nil, err
			}
			node.elements = append(node.elements, child)
		}
		closeToken, err := decoder.Token()
		if err != nil || closeToken != json.Delim(']') {
			return nil, fmt.Errorf("unterminated JSON array: %v", err)
		}
		return node, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

func marshalOrderedValue(out *bytes.Buffer, value interface{}, order *jsonOrder, contextKey string, root bool) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		return marshalOrderedObject(out, typed, order, contextKey, root)
	case []interface{}:
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			var child *jsonOrder
			if order != nil && i < len(order.elements) {
				child = order.elements[i]
			}
			if err := marshalOrderedValue(out, item, child, contextKey, false); err != nil {
				return err
			}
		}
		out.WriteByte(']')
		return nil
	default:
		if text, ok := value.(string); ok {
			writeJSONString(out, text)
			return nil
		}
		encoded, err := marshalJSONScalar(value)
		if err != nil {
			return err
		}
		out.Write(encoded)
		return nil
	}
}

func marshalOrderedObject(out *bytes.Buffer, object map[string]interface{}, order *jsonOrder, contextKey string, root bool) error {
	keys := orderedObjectKeys(object, order, contextKey, root)
	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		writeJSONString(out, key)
		out.WriteByte(':')
		var child *jsonOrder
		if order != nil {
			child = order.fields[key]
		}
		if err := marshalOrderedValue(out, object[key], child, key, false); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

// writeJSONString matches JSON.stringify's string escaping rather than encoding/json's
// HTML-safe mode. The shipping Bun client leaves <, >, &, U+2028 and U+2029 as UTF-8 on
// the wire. Go's default encoder changes them to \u003c/\u003e/\u0026/\u2028/\u2029,
// which was especially visible in Claude's frequent <session> and XML-like system text.
func writeJSONString(out *bytes.Buffer, value string) {
	const hex = "0123456789abcdef"
	out.WriteByte('"')
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 {
			// JSON strings are Unicode. Match encoding/json's safe handling for an
			// invalid Go string, while valid U+FFFD remains a normal UTF-8 rune.
			out.WriteRune(utf8.RuneError)
			value = value[1:]
			continue
		}
		value = value[size:]
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[byte(r)>>4])
				out.WriteByte(hex[byte(r)&0x0f])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

func marshalJSONScalar(value interface{}) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := out.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, nil
}

func orderedObjectKeys(object map[string]interface{}, order *jsonOrder, contextKey string, root bool) []string {
	keys := make([]string, 0, len(object))
	placed := make(map[string]bool, len(object))
	appendPresent := func(candidates []string) {
		for _, key := range candidates {
			if placed[key] {
				continue
			}
			if _, ok := object[key]; ok {
				keys = append(keys, key)
				placed[key] = true
			}
		}
	}
	if root {
		appendPresent(claudeRootKeyOrder)
	}
	if order != nil {
		appendPresent(order.keys)
	}
	// These only affect newly-created objects. Existing native objects have an
	// order node and retain their exact captured sequence above.
	switch contextKey {
	case "cache_control":
		appendPresent([]string{"type", "ttl"})
	case "metadata":
		appendPresent([]string{"user_id"})
	default:
		appendPresent([]string{
			"type", "text", "role", "content", "name", "description",
			"input_schema", "id", "tool_use_id", "input", "cache_control",
		})
	}
	remaining := make([]string, 0, len(object)-len(keys))
	for key := range object {
		if !placed[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	return append(keys, remaining...)
}
