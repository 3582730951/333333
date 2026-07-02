package reliability

import (
	"strings"
	"testing"
)

func TestExtractResponseTextOutputText(t *testing.T) {
	got := ExtractResponseText([]byte(`{"output_text":"hello"}`))
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractResponseTextFromOutputArray(t *testing.T) {
	body := []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"part1 "},{"type":"output_text","text":"part2"}]}]}`)
	if got := ExtractResponseText(body); got != "part1 part2" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractChatText(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"the answer"}}]}`)
	if got := ExtractChatText(body); got != "the answer" {
		t.Fatalf("got %q", got)
	}
}

func TestPrependResponsesNotice(t *testing.T) {
	body := []byte(`{"output_text":"done","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	out := PrependResponsesNotice(body, "NOTICE: ")
	if txt := ExtractResponseText(out); !strings.HasPrefix(txt, "NOTICE: ") {
		t.Fatalf("notice not prepended: %q", txt)
	}
}

func TestPrependResponsesNoticePreservesToolCalls(t *testing.T) {
	body := []byte(`{"output_text":"","output":[{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}]}`)
	out := PrependResponsesNotice(body, "NOTICE: ")
	if !strings.Contains(string(out), `"function_call"`) || !strings.Contains(string(out), `"shell"`) {
		t.Fatalf("function_call lost after notice prepend: %s", out)
	}
}

func TestPrependChatNotice(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`)
	out := PrependChatNotice(body, "NOTICE: ")
	if got := ExtractChatText(out); got != "NOTICE: answer" {
		t.Fatalf("got %q", got)
	}
}

func TestPrependChatNoticeNullContent(t *testing.T) {
	// A pure tool-call turn has null content; notice should still attach as a string.
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}}]}`)
	out := PrependChatNotice(body, "NOTICE")
	if got := ExtractChatText(out); got != "NOTICE" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(string(out), "tool_calls") {
		t.Fatalf("tool_calls lost: %s", out)
	}
}

func TestPrependNoticeFailOpen(t *testing.T) {
	bad := []byte(`not json`)
	if got := PrependResponsesNotice(bad, "X"); string(got) != "not json" {
		t.Fatalf("should return body unchanged on parse error: %s", got)
	}
	if got := PrependChatNotice(bad, "X"); string(got) != "not json" {
		t.Fatalf("should return body unchanged on parse error: %s", got)
	}
}
