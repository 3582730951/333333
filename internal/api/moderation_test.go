package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestModerationDisabledPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	// Moderation disabled by default
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"unsafe word"}]}`)
	out := h.app.moderateHistory(ctx, body, "chat")
	if string(out) != string(body) {
		t.Fatal("disabled moderation should be pass-through")
	}
}

func TestModerationInternalCallSkipsRecursion(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := withInternal(context.Background())

	cfg := storage.ModerationConfig{Enabled: true, Model: "gpt-4", Words: []string{"暴力"}}
	if err := h.store.SetModerationConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"messages":[{"role":"assistant","content":"暴力"}]}`)
	out := h.app.moderateHistory(ctx, body, "chat")

	if string(out) != string(body) {
		t.Fatal("internal call should skip moderation (recursion guard)")
	}
}

func TestModerationNoKeywordPassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()

	cfg := storage.ModerationConfig{Enabled: true, Model: "gpt-4", Words: []string{"暴力"}}
	if err := h.store.SetModerationConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	// Body contains no configured word
	body := []byte(`{"messages":[{"role":"assistant","content":"This is safe content."}]}`)
	out := h.app.moderateHistory(ctx, body, "chat")

	if string(out) != string(body) {
		t.Fatal("no keyword match should pass through unchanged")
	}
}

func TestContainsAnyWord(t *testing.T) {
	cases := []struct {
		body  string
		words []string
		want  bool
	}{
		{"hello world", []string{"world"}, true},
		{"hello world", []string{"WORLD"}, true}, // case-insensitive
		{"暴力内容", []string{"暴力"}, true},
		{"暴力内容", []string{"violence", "暴力"}, true},
		{"safe text", []string{"暴力", "violence"}, false},
		{"", []string{"word"}, false},
		{"text", []string{}, false},
	}
	for _, c := range cases {
		got := containsAnyWord([]byte(c.body), c.words)
		if got != c.want {
			t.Errorf("containsAnyWord(%q, %v) = %v, want %v", c.body, c.words, got, c.want)
		}
	}
}

func TestModerateMessagesOnlyTouchesAssistant(t *testing.T) {
	called := false
	rewrite := func(s string) string {
		called = true
		return "rewritten"
	}

	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "user text"},
		map[string]interface{}{"role": "assistant", "content": "assistant text"},
		map[string]interface{}{"role": "system", "content": "system text"},
	}

	moderateMessages(msgs, rewrite)

	if !called {
		t.Fatal("rewrite should have been called for assistant message")
	}
	if msgs[0].(map[string]interface{})["content"] != "user text" {
		t.Error("user message should be unchanged")
	}
	if msgs[1].(map[string]interface{})["content"] != "rewritten" {
		t.Error("assistant message should be rewritten")
	}
	if msgs[2].(map[string]interface{})["content"] != "system text" {
		t.Error("system message should be unchanged")
	}
}

func TestModerateResponsesInputOnlyTouchesAssistantMessages(t *testing.T) {
	called := false
	rewrite := func(s string) string {
		called = true
		return "rewritten"
	}

	input := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "user"},
		map[string]interface{}{"type": "message", "role": "assistant", "content": "assistant"},
		map[string]interface{}{"type": "function_call", "role": "assistant", "content": "tool data"},
	}

	moderateResponsesInput(input, rewrite)

	if !called {
		t.Fatal("rewrite should have been called for assistant message")
	}
	if input[0].(map[string]interface{})["content"] != "user" {
		t.Error("user message should be unchanged")
	}
	if input[1].(map[string]interface{})["content"] != "rewritten" {
		t.Error("assistant message should be rewritten")
	}
	if input[2].(map[string]interface{})["content"] != "tool data" {
		t.Error("function_call should be unchanged")
	}
}

func TestRewriteContentFieldHandlesStringAndArray(t *testing.T) {
	rewrite := func(s string) string { return "[CLEANED]" }

	// String content
	out := rewriteContentField("text", rewrite)
	if out != "[CLEANED]" {
		t.Errorf("string content = %v", out)
	}

	// Array with text parts
	arr := []interface{}{
		map[string]interface{}{"type": "text", "text": "hello"},
		map[string]interface{}{"type": "image", "source": "..."},
		map[string]interface{}{"type": "tool_use", "id": "1"},
	}
	out = rewriteContentField(arr, rewrite)
	outArr := out.([]interface{})
	if outArr[0].(map[string]interface{})["text"] != "[CLEANED]" {
		t.Error("text part should be rewritten")
	}
	if outArr[1].(map[string]interface{})["source"] != "..." {
		t.Error("image part should be unchanged")
	}
	if outArr[2].(map[string]interface{})["id"] != "1" {
		t.Error("tool_use part should be unchanged")
	}
}

func TestHasCJK(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"hello", false},
		{"暴力", true},
		{"中文", true},
		{"mixed 暴力 text", true},
		{"", false},
		{"日本語", true},
	}
	for _, c := range cases {
		got := hasCJK(c.s)
		if got != c.want {
			t.Errorf("hasCJK(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}
