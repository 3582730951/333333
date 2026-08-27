package tokenizer

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// legacyEstimate is the rune/4 heuristic this package replaces, kept here so the tests
// assert the direction and rough size of the error it produced.
func legacyEstimate(raw []byte) int64 { return int64(utf8.RuneCount(raw)/4 + 1) }

func TestEmbeddedVocabularyLoadsWithoutNetwork(t *testing.T) {
	if !Available() {
		t.Fatal("embedded o200k_base vocabulary failed to load")
	}
	// A known-stable short ASCII pair; o200k encodes this as two tokens.
	if got, ok := CountText("hello world"); !ok || got != 2 {
		t.Fatalf("CountText(\"hello world\") = %d (ok=%v), want 2", got, ok)
	}
	if got, ok := CountText(""); !ok || got != 0 {
		t.Fatalf("empty text = %d (ok=%v), want 0", got, ok)
	}
}

func TestEmbeddedLoaderRefusesNonEmbeddedEncoding(t *testing.T) {
	// The loader must never silently fall through to the network for another vocabulary.
	if _, err := (embeddedBpeLoader{}).LoadTiktokenBpe(
		"https://openaipublic.blob.core.windows.net/encodings/cl100k_base.tiktoken"); err == nil {
		t.Fatal("loader accepted a non-embedded encoding instead of refusing it")
	}
	if _, err := (embeddedBpeLoader{}).LoadTiktokenBpe(o200kBaseURL); err != nil {
		t.Fatalf("loader rejected the embedded encoding: %v", err)
	}
}

func TestCJKIsNotUndercountedLikeTheRuneHeuristic(t *testing.T) {
	zh := strings.Repeat("这是一段用于验证分词准确性的中文文本。", 12)
	exact, ok := CountText(zh)
	if !ok {
		t.Fatal("tokenizer unavailable")
	}
	runes := int64(utf8.RuneCountInString(zh))
	// o200k spends well under one token per Chinese character but far more than the
	// quarter-token the heuristic assumed; anything near runes/4 means the fix regressed.
	if exact < runes/2 {
		t.Fatalf("CJK count %d is implausibly low for %d runes", exact, runes)
	}
	if heuristic := runes/4 + 1; exact <= heuristic {
		t.Fatalf("exact %d did not exceed the rune/4 heuristic %d", exact, heuristic)
	}
}

func TestCountRequestTokensIgnoresJSONEnvelope(t *testing.T) {
	// Two bodies with identical model-visible text but very different envelope sizes
	// must count the same: the envelope is not content.
	compact, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.6-sol",
		"input": []interface{}{map[string]interface{}{"role": "user", "content": "the quick brown fox"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	padded, err := json.Marshal(map[string]interface{}{
		"model":     "gpt-5.6-sol",
		"store":     false,
		"stream":    true,
		"reasoning": map[string]interface{}{"effort": "high", "summary": "auto"},
		"metadata":  map[string]interface{}{"session_id": "0199aaaa-bbbb-cccc-dddd-eeeeffff0000"},
		"input": []interface{}{map[string]interface{}{
			"role": "user", "status": "completed", "id": "msg_0199aaaabbbbccccddddeeeeffff",
			"content": []interface{}{map[string]interface{}{"type": "input_text", "text": "the quick brown fox"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compactCount, ok := CountRequestTokens(compact)
	if !ok {
		t.Fatal("compact body not counted")
	}
	paddedCount, ok := CountRequestTokens(padded)
	if !ok {
		t.Fatal("padded body not counted")
	}
	if compactCount != paddedCount {
		t.Fatalf("envelope changed the count: compact=%d padded=%d", compactCount, paddedCount)
	}
	// The heuristic, by contrast, charged for the envelope.
	if legacyEstimate(compact) == legacyEstimate(padded) {
		t.Fatal("test fixture is not exercising an envelope difference")
	}
}

func TestCountRequestTokensRejectsNonObject(t *testing.T) {
	if _, ok := CountRequestTokens([]byte(`["not","an","object"]`)); ok {
		t.Fatal("array body reported a count")
	}
	if _, ok := CountRequestTokens([]byte(`not json`)); ok {
		t.Fatal("invalid JSON reported a count")
	}
}
