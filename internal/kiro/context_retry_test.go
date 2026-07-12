package kiro

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestKiroErrorClassifiersAreReasonSpecific(t *testing.T) {
	if !ContentLengthExceeded([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)) {
		t.Fatal("explicit content-length error was not recognized")
	}
	if ContentLengthExceeded([]byte(`{"message":"cachePoint is not supported"}`)) {
		t.Fatal("cachePoint validation was mistaken for a context error")
	}
	if !CachePointRejected([]byte(`{"error":{"message":"Unknown field cache_point"}}`)) {
		t.Fatal("cachePoint validation was not recognized")
	}
	if CachePointRejected([]byte(`{"message":"Input is too long.","reason":"CONTENT_LENGTH_EXCEEDS_THRESHOLD"}`)) {
		t.Fatal("long input was mistaken for cachePoint rejection")
	}
}

func TestReduceKiroMaxOutputPreservesConversation(t *testing.T) {
	body := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"content":"current"}},"history":[{"userInputMessage":{"content":"old"}}]},"additionalModelRequestFields":{"thinking":{"type":"adaptive"},"max_tokens":128000}}`)
	updated, changed, err := ReduceKiroMaxOutput(body, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || !strings.Contains(string(updated), `"max_tokens":4096`) || !strings.Contains(string(updated), `"content":"old"`) {
		t.Fatalf("max-output retry changed the wrong fields: %s", updated)
	}
}

func TestTrimOldestKiroHistoryPreservesSystemAndToolDependency(t *testing.T) {
	root := map[string]any{
		"conversationState": map[string]any{
			"currentMessage": map[string]any{"userInputMessage": map[string]any{
				"content":                 "current result",
				"userInputMessageContext": map[string]any{"toolResults": []any{map[string]any{"toolUseId": "call-2"}}},
			}},
			"history": []any{
				map[string]any{"userInputMessage": map[string]any{"content": "system"}},
				map[string]any{"assistantResponseMessage": map[string]any{"content": systemAcknowledgement}},
				map[string]any{"userInputMessage": map[string]any{"content": strings.Repeat("old-a", 200)}},
				map[string]any{"assistantResponseMessage": map[string]any{"content": strings.Repeat("old-b", 200)}},
				map[string]any{"userInputMessage": map[string]any{"content": "latest"}},
				map[string]any{"assistantResponseMessage": map[string]any{"content": "tool", "toolUses": []any{map[string]any{"toolUseId": "call-2"}}}},
			},
		},
	}
	body, _ := json.Marshal(root)
	updated, dropped, changed, err := TrimOldestKiroHistory(body, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || dropped != 2 {
		t.Fatalf("trim result changed=%v dropped=%d body=%s", changed, dropped, updated)
	}
	text := string(updated)
	for _, required := range []string{"system", systemAcknowledgement, "latest", "call-2", "current result"} {
		if !strings.Contains(text, required) {
			t.Fatalf("required context %q was dropped: %s", required, text)
		}
	}
	if strings.Contains(text, "old-a") || strings.Contains(text, "old-b") {
		t.Fatalf("oldest complete turn was retained: %s", text)
	}
}

func TestRemoveKiroCachePointsRemovesArrayMarkersWithoutEmptyTools(t *testing.T) {
	body := []byte(`{"conversationState":{"currentMessage":{"userInputMessage":{"cachePoint":{"type":"default"},"userInputMessageContext":{"tools":[{"toolSpecification":{"name":"x"}},{"cachePoint":{"type":"default"}}]}}}}}`)
	updated, changed, err := RemoveKiroCachePoints(body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || strings.Contains(string(updated), "cachePoint") || strings.Contains(string(updated), `{},`) || strings.Contains(string(updated), `,{}`) {
		t.Fatalf("cachePoint removal left invalid markers: %s", updated)
	}
}
