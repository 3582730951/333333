package jsonview

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGetAndParse(t *testing.T) {
	raw := []byte(`{"request":{"contents":[{"parts":[{"text":"hello"}]}]}}`)
	if got := Get(raw, "request.contents.0.parts.0.text").String(); got != "hello" {
		t.Fatalf("Get() = %q, want hello", got)
	}
	if got := Parse(raw).Get("request.contents.#").Int(); got != 1 {
		t.Fatalf("Parse() contents = %d, want 1", got)
	}
	if Get(nil, "request").Exists() || Parse(nil).Exists() {
		t.Fatal("empty input must return an empty result")
	}
}

func BenchmarkLargeDocumentGet(b *testing.B) {
	raw := append([]byte(`{"messages":[{"content":"`), bytes.Repeat([]byte("x"), 1<<20)...)
	raw = append(raw, []byte(`"}],"max_tokens":4096}`)...)
	b.Run("copy", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !gjson.GetBytes(raw, "messages").Exists() {
				b.Fatal("messages missing")
			}
		}
	})
	b.Run("view", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !Get(raw, "messages").Exists() {
				b.Fatal("messages missing")
			}
		}
	})
}
