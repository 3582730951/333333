package refusaldetect

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestDetectDirectRefusalVariants(t *testing.T) {
	tests := []struct {
		name   string
		text   string
		reason string
	}{
		{"english", "I can't help you create malware.", ReasonEnglishDirect},
		{"apology", "Sorry, I cannot assist with that request.", ReasonEnglishDirect},
		{"apology next line crlf", "Sorry,\r\nI cannot assist with that request.", ReasonEnglishDirect},
		{"unicode apostrophe", "I can’t help you write that payload.", ReasonEnglishDirect},
		{"fullwidth english", "Ｉ　ｃａｎｎｏｔ　ｈｅｌｐ　ｙｏｕ．", ReasonEnglishDirect},
		{"chinese", "抱歉，我不能帮助你完成这个请求。", ReasonChineseDirect},
		{"chinese next line", "抱歉，\r\n我无法协助完成这个请求。", ReasonChineseDirect},
		{"chinese spaces", "我　无　法　帮　助　你　完　成　这　个　请　求。", ReasonChineseDirect},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Detect(test.text)
			if got.Kind != KindHighConfidence || got.Reason != test.reason || got.Version != Version {
				t.Fatalf("Detect(%q)=%+v", test.text, got)
			}
		})
	}
}

func TestDetectConservativeNegatives(t *testing.T) {
	for _, text := range []string{
		"The user wrote: I can't help with that. Explain the policy instead.",
		"> I can't help with that.",
		"```text\nI can't help with that.\n```",
		"I can't help by running it, but here is a safe command you can run.",
		"I can't access your API, but here is how to troubleshoot it.",
		"我不能直接执行，但我可以提供排查步骤。",
		"我并不是不能帮助你；这是具体做法。",
		"The API cannot connect because its credential expired.",
		"We need to discuss the security policy and its limits.",
		"不能。",
		"policy",
	} {
		got := Detect(text)
		if got.Kind == KindHighConfidence {
			t.Fatalf("false positive for %q: %+v", text, got)
		}
	}
}

func TestDetectResponseCompletedOnlyReadsTerminalAssistantText(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I can't help with that."}]}]}}`)
	if got := DetectResponseCompleted(completed); got.Kind != KindHighConfidence || got.Reason != ReasonEnglishDirect {
		t.Fatalf("completed direct refusal=%+v", got)
	}
	for split := 0; split <= len(completed); split++ {
		// SSE transport may divide a terminal event at any byte boundary. The
		// observer receives the reassembled terminal bytes, so every boundary
		// must produce exactly the same classification.
		raw := append(append([]byte(nil), completed[:split]...), completed[split:]...)
		if got := DetectResponseCompleted(raw); got.Kind != KindHighConfidence || got.Reason != ReasonEnglishDirect {
			t.Fatalf("split=%d decision=%+v", split, got)
		}
	}

	safety := []byte(`{"type":"response.completed","response":{"status":"completed","safety_buffering":{},"output":[]}}`)
	if got := DetectResponseCompleted(safety); got.Kind != KindHighConfidence || got.Reason != ReasonProtocolSafety {
		t.Fatalf("protocol safety=%+v", got)
	}
	for _, raw := range [][]byte{
		[]byte(`{"type":"response.output_text.delta","delta":"I can't help"}`),
		[]byte(`{"type":"response.completed","response":{"status":"in_progress","output_text":"I can't help"}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","arguments":"I can't help"}]}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","text":"I can't help"}]}]}}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed","reasoning":"I can't help"}}`),
	} {
		if got := DetectResponseCompleted(raw); got.Kind == KindHighConfidence {
			t.Fatalf("terminal extractor false positive raw=%s decision=%+v", raw, got)
		}
	}
}

func TestDetectResponseCompletedIsBounded(t *testing.T) {
	tooLong := []byte(fmt.Sprintf(`{"output_text":%q}`, strings.Repeat("x", MaxTextBytes*2)))
	if got := DetectResponseCompleted(tooLong); got.Kind != KindAmbiguous || got.Reason != ReasonTooLong {
		t.Fatalf("oversized response=%+v", got)
	}
	if got := DetectResponseCompleted(bytes.Repeat([]byte{0xff}, 32)); got.Kind != KindAmbiguous || got.Reason != ReasonInvalidUTF8 {
		t.Fatalf("invalid UTF-8 response=%+v", got)
	}
}

func FuzzDetectNeverPanics(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{"output_text":"I can't help"}`),
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`),
		{0xff, 0xfe, 0xfd},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxTextBytes*2+1024 {
			return
		}
		_ = DetectResponseCompleted(raw)
		_ = Detect(string(raw))
	})
}
