package kiro

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClaudeCodeSummaryFallbackBuildsBoundedMapReduce(t *testing.T) {
	compactInstruction := claudeCodeCompactionInstruction + ", preserving all decisions. FINAL_ONLY_MARKER\n\n" + claudeCodeCompactionReminder
	raw, err := json.Marshal(map[string]any{
		"model":  "claude-opus-4-8[1m]",
		"system": claudeCodeCompactionSystem,
		"stream": true,
		"tools": []any{map[string]any{
			"name": "large_schema_tool", "description": strings.Repeat("schema-only-", 20_000),
			"input_schema": map[string]any{"type": "object"},
		}},
		"messages": []any{
			map[string]any{"role": "user", "content": "HISTORY_A_" + strings.Repeat("alpha-", 20_000)},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_keep_pair", "name": "large_schema_tool", "input": map[string]any{"path": "/tmp/重要.txt"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_keep_pair", "content": "literal-result-42"}}},
			map[string]any{"role": "assistant", "content": "HISTORY_B_" + strings.Repeat("beta-", 20_000)},
			map[string]any{"role": "user", "content": compactInstruction},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requests, err := BuildClaudeCodeSummaryFallbackRequests(raw, 100_000, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) < 2 || len(requests) > 8 {
		t.Fatalf("map stages=%d, want bounded split", len(requests))
	}
	joined := bytes.Join(requests, nil)
	for _, marker := range [][]byte{[]byte("HISTORY_A_"), []byte("HISTORY_B_"), []byte("toolu_keep_pair"), []byte("literal-result-42")} {
		if !bytes.Contains(joined, marker) {
			t.Fatalf("map stages lost %q", marker)
		}
	}
	if bytes.Contains(joined, []byte("FINAL_ONLY_MARKER")) || bytes.Contains(joined, []byte("schema-only-")) {
		t.Fatalf("map stages carried final instruction or no-tools schema")
	}
	for index, request := range requests {
		if !json.Valid(request) || !IsClaudeCodeCompactionRequest(request) {
			t.Fatalf("stage %d is not a valid native compaction request: %s", index+1, request)
		}
		var envelope map[string]any
		if err := json.Unmarshal(request, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope["stream"] != false {
			t.Fatalf("stage %d stream=%v", index+1, envelope["stream"])
		}
		if _, present := envelope["tools"]; present {
			t.Fatalf("stage %d unexpectedly carries tools", index+1)
		}
	}

	summaries := make([]string, len(requests))
	for i := range summaries {
		summaries[i] = "SUMMARY_PART_" + string(rune('A'+i))
	}
	finalRaw, err := BuildClaudeCodeSummaryFallbackFinal(raw, summaries)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(finalRaw) || !IsClaudeCodeCompactionRequest(finalRaw) {
		t.Fatalf("final reduce is not a native compaction request: %s", finalRaw)
	}
	for _, marker := range append(summaries, "FINAL_ONLY_MARKER") {
		if !bytes.Contains(finalRaw, []byte(marker)) {
			t.Fatalf("final reduce lost %q: %s", marker, finalRaw)
		}
	}
	for _, removed := range [][]byte{[]byte("HISTORY_A_"), []byte("schema-only-"), []byte("toolu_keep_pair")} {
		if bytes.Contains(finalRaw, removed) {
			t.Fatalf("final reduce retained summarized payload %q", removed)
		}
	}
	var finalEnvelope map[string]any
	if err := json.Unmarshal(finalRaw, &finalEnvelope); err != nil {
		t.Fatal(err)
	}
	if finalEnvelope["stream"] != true {
		t.Fatalf("final stream=%v, want original true", finalEnvelope["stream"])
	}
}

func TestClaudeCodeSummaryFallbackEnforcesStageBound(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"model":  "claude-sonnet-4-6",
		"system": claudeCodeCompactionSystem,
		"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("oversized-history-", 20_000)},
			map[string]any{"role": "user", "content": claudeCodeCompactionInstruction},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildClaudeCodeSummaryFallbackRequests(raw, 1, 1)
	if !errors.Is(err, ErrSummaryFallbackExhausted) {
		t.Fatalf("bound error=%v", err)
	}
}
