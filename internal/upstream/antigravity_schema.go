package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
)

const antigravitySchemaPlaceholderDescription = "Brief explanation of why you are calling this tool"

var antigravityUnsupportedSchemaKeywords = map[string]bool{
	"$schema": true, "$defs": true, "definitions": true, "$id": true,
	"additionalProperties": true, "propertyNames": true, "patternProperties": true,
	"$comment": true, "enumDescriptions": true, "enumTitles": true, "prefill": true,
	"deprecated": true, "minLength": true, "maxLength": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "pattern": true, "minItems": true, "maxItems": true,
	"uniqueItems": true, "format": true, "default": true, "examples": true,
}

func sanitizeAntigravityToolSchema(raw string, validated bool) (json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var schema interface{}
	if err := decoder.Decode(&schema); err != nil {
		return nil, err
	}
	cleaned, ok := cleanAntigravitySchemaNode(schema, validated).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("schema root must be an object")
	}
	out, err := json.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// cleanAntigravitySchemaNode mirrors the subset of the native client's schema
// cleanup that affects acceptance by Google's proto-JSON endpoint. Property names
// are never interpreted as schema keywords, so tool arguments named "format" or
// "default" remain intact.
func cleanAntigravitySchemaNode(node interface{}, validated bool) interface{} {
	switch value := node.(type) {
	case []interface{}:
		out := make([]interface{}, 0, len(value))
		for _, item := range value {
			out = append(out, cleanAntigravitySchemaNode(item, validated))
		}
		return out
	case map[string]interface{}:
		value = normalizeAntigravitySchemaComposition(value)
		out := make(map[string]interface{}, len(value))
		hints := make([]string, 0, 4)

		if ref, _ := value["$ref"].(string); strings.TrimSpace(ref) != "" {
			parts := strings.Split(strings.TrimSpace(ref), "/")
			hints = append(hints, "See: "+parts[len(parts)-1])
			if _, exists := value["type"]; !exists {
				value["type"] = "object"
			}
		}
		if constant, exists := value["const"]; exists {
			if _, hasEnum := value["enum"]; !hasEnum {
				value["enum"] = []interface{}{constant}
			}
		}

		var nullableProperties map[string]bool
		for key, child := range value {
			switch {
			case key == "properties":
				properties, ok := child.(map[string]interface{})
				if !ok {
					continue
				}
				cleanedProperties := make(map[string]interface{}, len(properties))
				nullableProperties = make(map[string]bool)
				for propertyName, propertySchema := range properties {
					nullableProperties[propertyName] = antigravitySchemaAllowsNull(propertySchema)
					cleanedProperties[propertyName] = cleanAntigravitySchemaNode(propertySchema, validated)
				}
				out[key] = cleanedProperties
			case key == "const" || key == "$ref" || key == "allOf" || key == "anyOf" || key == "oneOf":
				continue
			case strings.HasPrefix(key, "x-") || antigravityUnsupportedSchemaKeywords[key]:
				if key == "additionalProperties" && child == false {
					hints = append(hints, "No extra properties allowed")
				} else if child != nil && key != "$defs" && key != "definitions" {
					hints = append(hints, fmt.Sprintf("%s: %v", key, child))
				}
				continue
			case key == "type":
				cleanedType, typeHints := flattenAntigravitySchemaType(child)
				out[key] = cleanedType
				hints = append(hints, typeHints...)
			case key == "enum":
				if items, ok := child.([]interface{}); ok {
					converted := make([]interface{}, 0, len(items))
					labels := make([]string, 0, len(items))
					for _, item := range items {
						label := fmt.Sprint(item)
						converted = append(converted, label)
						labels = append(labels, label)
					}
					out[key] = converted
					if len(labels) > 1 && len(labels) <= 10 {
						hints = append(hints, "Allowed: "+strings.Join(labels, ", "))
					}
				}
			default:
				out[key] = cleanAntigravitySchemaNode(child, validated)
			}
		}

		cleanAntigravityRequired(out, nullableProperties)
		if len(hints) > 0 {
			appendAntigravitySchemaDescription(out, strings.Join(hints, "; "))
		}
		if validated {
			ensureAntigravityValidatedObject(out)
		}
		return out
	default:
		return node
	}
}

func normalizeAntigravitySchemaComposition(schema map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		out[key] = value
	}
	if allOf, ok := out["allOf"].([]interface{}); ok {
		for _, raw := range allOf {
			part, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			mergeAntigravitySchemaMap(out, part)
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		variants, ok := out[key].([]interface{})
		if !ok || len(variants) == 0 {
			continue
		}
		if selected, ok := selectAntigravitySchemaVariant(variants).(map[string]interface{}); ok {
			mergeAntigravitySchemaMap(out, selected)
		}
		delete(out, key)
	}
	return out
}

func mergeAntigravitySchemaMap(dst, src map[string]interface{}) {
	for key, value := range src {
		if key == "properties" {
			srcProps, _ := value.(map[string]interface{})
			dstProps, _ := dst[key].(map[string]interface{})
			if dstProps == nil {
				dstProps = map[string]interface{}{}
			}
			for property, schema := range srcProps {
				dstProps[property] = schema
			}
			dst[key] = dstProps
			continue
		}
		if key == "required" {
			dst[key] = mergeAntigravityRequiredLists(dst[key], value)
			continue
		}
		if _, exists := dst[key]; !exists || key == "type" {
			dst[key] = value
		}
	}
}

func mergeAntigravityRequiredLists(left, right interface{}) []interface{} {
	seen := map[string]bool{}
	out := []interface{}{}
	for _, source := range []interface{}{left, right} {
		items, _ := source.([]interface{})
		for _, item := range items {
			name, _ := item.(string)
			if name != "" && !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

func selectAntigravitySchemaVariant(variants []interface{}) interface{} {
	best := variants[0]
	bestScore := -1
	for _, variant := range variants {
		schema, _ := variant.(map[string]interface{})
		typeName, _ := schema["type"].(string)
		score := 0
		switch {
		case typeName == "object" || schema["properties"] != nil:
			score = 3
		case typeName == "array" || schema["items"] != nil:
			score = 2
		case typeName != "" && typeName != "null":
			score = 1
		}
		if score > bestScore {
			best, bestScore = variant, score
		}
	}
	return best
}

func flattenAntigravitySchemaType(raw interface{}) (interface{}, []string) {
	items, ok := raw.([]interface{})
	if !ok {
		return raw, nil
	}
	types := make([]string, 0, len(items))
	for _, item := range items {
		name, _ := item.(string)
		if name != "" && name != "null" {
			types = append(types, name)
		}
	}
	if len(types) == 0 {
		return "string", []string{"nullable"}
	}
	hints := []string{}
	if len(types) > 1 {
		hints = append(hints, "Accepts: "+strings.Join(types, " | "))
	}
	return types[0], hints
}

func antigravitySchemaAllowsNull(raw interface{}) bool {
	schema, _ := raw.(map[string]interface{})
	types, _ := schema["type"].([]interface{})
	for _, item := range types {
		if item == "null" {
			return true
		}
	}
	return false
}

func cleanAntigravityRequired(schema map[string]interface{}, nullable map[string]bool) {
	properties, _ := schema["properties"].(map[string]interface{})
	required, _ := schema["required"].([]interface{})
	if len(required) == 0 || properties == nil {
		delete(schema, "required")
		return
	}
	cleaned := make([]interface{}, 0, len(required))
	seen := map[string]bool{}
	for _, item := range required {
		name, _ := item.(string)
		if _, exists := properties[name]; name == "" || !exists || nullable[name] || seen[name] {
			continue
		}
		seen[name] = true
		cleaned = append(cleaned, name)
	}
	if len(cleaned) == 0 {
		delete(schema, "required")
	} else {
		schema["required"] = cleaned
	}
}

func ensureAntigravityValidatedObject(schema map[string]interface{}) {
	typeName, _ := schema["type"].(string)
	properties, hasProperties := schema["properties"].(map[string]interface{})
	if typeName != "object" && !hasProperties {
		return
	}
	if properties == nil {
		properties = map[string]interface{}{}
		schema["properties"] = properties
	}
	required, _ := schema["required"].([]interface{})
	if len(properties) == 0 {
		properties["reason"] = map[string]interface{}{"type": "string", "description": antigravitySchemaPlaceholderDescription}
		schema["required"] = []interface{}{"reason"}
		return
	}
	if len(required) == 0 {
		if _, exists := properties["_"]; !exists {
			properties["_"] = map[string]interface{}{"type": "boolean"}
		}
		schema["required"] = []interface{}{"_"}
	}
}

func appendAntigravitySchemaDescription(schema map[string]interface{}, hint string) {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return
	}
	existing, _ := schema["description"].(string)
	if strings.TrimSpace(existing) == "" {
		schema["description"] = hint
		return
	}
	schema["description"] = strings.TrimSpace(existing) + " (" + hint + ")"
}
