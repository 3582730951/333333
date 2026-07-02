package tokensave

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestCompressBelowThresholdIsNoop(t *testing.T) {
	in := "line1\nline2\nline3\n"
	if got := Compress(in, DefaultOptions()); got != in {
		t.Fatalf("small clean block should be unchanged:\nin=%q\ngot=%q", in, got)
	}
}

func TestCompressStripsANSIAndTrailingAndBlanks(t *testing.T) {
	in := "\x1b[31mred\x1b[0m   \n\n\n\nnext\n"
	got := Compress(in, DefaultOptions())
	if strings.Contains(got, "\x1b[") {
		t.Fatalf("ANSI not stripped: %q", got)
	}
	if strings.Contains(got, "red   ") {
		t.Fatalf("trailing ws not trimmed: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Fatalf("blank lines not collapsed: %q", got)
	}
}

func TestCompressCollapsesDuplicateRuns(t *testing.T) {
	in := strings.Repeat("same\n", 20) + "end\n"
	got := Compress(in, DefaultOptions())
	if strings.Count(got, "same") > 2 {
		t.Fatalf("duplicate run not collapsed: %q", got)
	}
	if !strings.Contains(got, "repeated") || !strings.Contains(got, "end") {
		t.Fatalf("expected repeat marker + preserved tail: %q", got)
	}
}

func TestCompressTruncatesHugeBlockKeepingHeadAndTail(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("L")
		sb.WriteString(strings.Repeat("x", 1)) // distinct-ish lines so dupe-collapse doesn't fire
		sb.WriteByte(byte('0' + i%10))
		sb.WriteByte('\n')
	}
	in := sb.String()
	got := Compress(in, DefaultOptions())
	if !strings.Contains(got, "lines elided by token-saver") {
		t.Fatalf("expected elision marker on huge block")
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) > DefaultOptions().HeadLines+DefaultOptions().TailLines+2 {
		t.Fatalf("truncation kept too many lines: %d", len(lines))
	}
	// Head and tail must survive.
	if !strings.HasPrefix(got, "Lx0\n") {
		t.Fatalf("head not preserved: %q", got[:20])
	}
}

func TestCompressIsIdempotent(t *testing.T) {
	in := strings.Repeat("dup\n", 30) + strings.Repeat("uniq-line-content\n", 600)
	once := Compress(in, DefaultOptions())
	twice := Compress(once, DefaultOptions())
	if once != twice {
		t.Fatalf("Compress not idempotent:\nonce len=%d twice len=%d", len(once), len(twice))
	}
}

func TestCompressAnthropicToolResultsOnlyTouchesToolResults(t *testing.T) {
	var hb strings.Builder
	for i := 0; i < 1000; i++ {
		hb.WriteString("build step ")
		hb.WriteString(strconv.Itoa(i))
		hb.WriteString(" completed ok\n")
	}
	huge := hb.String()
	body := map[string]interface{}{
		"model": "claude",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": []interface{}{
				map[string]interface{}{"type": "text", "text": "please keep this prompt exactly"},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t1", "content": huge},
				map[string]interface{}{"type": "tool_result", "tool_use_id": "t2", "content": []interface{}{
					map[string]interface{}{"type": "text", "text": huge},
				}},
			}},
		},
	}
	raw, _ := json.Marshal(body)
	out, saved := CompressAnthropicToolResults(raw, DefaultOptions())
	if saved <= 0 {
		t.Fatalf("expected bytes saved on huge tool results, got %d", saved)
	}
	if len(out) >= len(raw) {
		t.Fatalf("body did not shrink: before=%d after=%d", len(raw), len(out))
	}
	// The user prompt text must be untouched.
	if !strings.Contains(string(out), "please keep this prompt exactly") {
		t.Fatalf("user prompt was altered/lost")
	}
	// Both tool results compressed → elision markers present.
	if strings.Count(string(out), "lines elided by token-saver") < 2 {
		t.Fatalf("expected both tool_result forms compressed")
	}
}

func TestCompressAnthropicNoopOnNonMatching(t *testing.T) {
	raw := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	out, saved := CompressAnthropicToolResults(raw, DefaultOptions())
	if saved != 0 || string(out) != string(raw) {
		t.Fatalf("expected no-op on body with no tool results: saved=%d", saved)
	}
}
