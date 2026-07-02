package streamrewrite

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

func TestReplaceAllBasic(t *testing.T) {
	m := NewFromMap(map[string]string{"SECRET": "PUBLIC"})
	got := m.ReplaceString("a SECRET and another SECRET here")
	want := "a PUBLIC and another PUBLIC here"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEmptyMatcherPassthrough(t *testing.T) {
	m := New(nil)
	if !m.Empty() {
		t.Fatal("expected empty matcher")
	}
	in := []byte("nothing changes")
	out := m.ReplaceAll(in)
	if &out[0] != &in[0] {
		t.Fatal("empty matcher must return input slice unchanged (zero copy)")
	}
}

func TestLeftmostLongest(t *testing.T) {
	// Nested: longest wins at the same start.
	m := NewFromMap(map[string]string{"ab": "X", "abc": "Y"})
	if got := m.ReplaceString("zabcz"); got != "zYz" {
		t.Fatalf("nested: got %q want %q", got, "zYz")
	}
	// Overlapping: leftmost wins even if the later one is longer.
	m2 := NewFromMap(map[string]string{"abc": "X", "bcde": "Y"})
	if got := m2.ReplaceString("abcde"); got != "Xde" {
		t.Fatalf("overlap: got %q want %q", got, "Xde")
	}
}

func TestReplacementNotRescanned(t *testing.T) {
	// "a"->"b" and "b"->"c": the produced "b" must NOT become "c".
	m := NewFromMap(map[string]string{"a": "b", "b": "c"})
	if got := m.ReplaceString("a"); got != "b" {
		t.Fatalf("got %q want %q", got, "b")
	}
	if got := m.ReplaceString("ab"); got != "bc" {
		t.Fatalf("got %q want %q", got, "bc")
	}
}

func TestManyPatterns(t *testing.T) {
	m := NewFromMap(map[string]string{
		"/Users/realuser":      "/Users/agent",
		"realuser-macbook":     "host-7f3a",
		"darwin":               "darwin",
		"sk-secret-token-1234": "sk-virtual-aaaa",
	})
	in := "cwd=/Users/realuser/proj host=realuser-macbook.local tok=sk-secret-token-1234"
	want := "cwd=/Users/agent/proj host=host-7f3a.local tok=sk-virtual-aaaa"
	if got := m.ReplaceString(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// TestStreamingEquivalence is the core 100%-replacement guarantee: feeding the
// input through the streaming Rewriter under ANY chunk boundary must produce
// byte-identical output to a single ReplaceAll.
func TestStreamingEquivalence(t *testing.T) {
	m := NewFromMap(map[string]string{
		"SECRET":          "PUBLIC",
		"abcdef":          "XY",
		"ab":              "Q",
		"/Users/realuser": "/Users/agent",
		"token":           "TKN",
	})
	corpus := "x SECRET y abcdef ab token /Users/realuser/p SECRETabcdef tokentoken end SECRE"
	want := m.ReplaceString(corpus)

	// Every fixed chunk size from 1..len+2.
	for size := 1; size <= len(corpus)+2; size++ {
		r := m.NewRewriter()
		var out bytes.Buffer
		for i := 0; i < len(corpus); i += size {
			end := i + size
			if end > len(corpus) {
				end = len(corpus)
			}
			out.Write(r.Write([]byte(corpus[i:end])))
		}
		out.Write(r.Flush())
		if out.String() != want {
			t.Fatalf("chunk size %d: got %q want %q", size, out.String(), want)
		}
	}

	// Randomized chunk boundaries (deterministic seed).
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		r := m.NewRewriter()
		var out bytes.Buffer
		i := 0
		for i < len(corpus) {
			step := 1 + rng.Intn(7)
			end := i + step
			if end > len(corpus) {
				end = len(corpus)
			}
			out.Write(r.Write([]byte(corpus[i:end])))
			i = end
		}
		out.Write(r.Flush())
		if out.String() != want {
			t.Fatalf("trial %d: got %q want %q", trial, out.String(), want)
		}
	}
}

// TestStreamingByteSplitOnPattern hammers the boundary case where a pattern is
// split at each internal index.
func TestStreamingByteSplitOnPattern(t *testing.T) {
	m := NewFromMap(map[string]string{"SECRET": "PUBLIC"})
	const word = "<<SECRET>>"
	want := "<<PUBLIC>>"
	for split := 0; split <= len(word); split++ {
		r := m.NewRewriter()
		var out bytes.Buffer
		out.Write(r.Write([]byte(word[:split])))
		out.Write(r.Write([]byte(word[split:])))
		out.Write(r.Flush())
		if out.String() != want {
			t.Fatalf("split %d: got %q want %q", split, out.String(), want)
		}
	}
}

func TestNoMatchZeroCopy(t *testing.T) {
	m := NewFromMap(map[string]string{"zzz": "qqq"})
	in := []byte("no relevant content here")
	out := m.ReplaceAll(in)
	if !bytes.Equal(out, in) {
		t.Fatalf("unexpected change: %q", out)
	}
}

func TestStreamingLargeRandomCorpus(t *testing.T) {
	patterns := map[string]string{
		"alpha": "A", "bravo": "BB", "charlie": "CCC",
		"alphabravo": "Z", "delta-echo-foxtrot": "DEF",
	}
	m := NewFromMap(patterns)
	rng := rand.New(rand.NewSource(42))
	tokens := []string{"alpha", "bravo", "charlie", "alphabravo", "delta-echo-foxtrot", " ", "x", "..", "\n"}
	var sb strings.Builder
	for i := 0; i < 5000; i++ {
		sb.WriteString(tokens[rng.Intn(len(tokens))])
	}
	corpus := sb.String()
	want := m.ReplaceString(corpus)

	r := m.NewRewriter()
	var out bytes.Buffer
	i := 0
	for i < len(corpus) {
		step := 1 + rng.Intn(13)
		end := i + step
		if end > len(corpus) {
			end = len(corpus)
		}
		out.Write(r.Write([]byte(corpus[i:end])))
		i = end
	}
	out.Write(r.Flush())
	if out.String() != want {
		t.Fatalf("large corpus mismatch")
	}
}
