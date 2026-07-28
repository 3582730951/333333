package storage

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCodexRemoteCompactionReplacementSurvivesChunkingAndFullLiteFollowup(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	oldNoise := strings.Repeat("old-tool-noise-", 32<<10)
	first := goalTurnForTest("compact-v2-root", "resp-before-compact", "unused", "unused")
	first.CompressionStages = 1
	first.StorageMaxBytes = 32 << 20
	first.SegmentPayload = fmt.Sprintf(`{
		"history_key":"input",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"old_patch","format":{"const":900719925474099312345}}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base instructions"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>old</environment_context>"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"retain the original task"}]}
		],
		"output":[
			{"type":"reasoning","encrypted_content":"old-reasoning"},
			{"type":"custom_tool_call","call_id":"old-call","name":"old_patch","input":%q}
		]
	}`, oldNoise)
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}

	compact := goalTurnForTest("compact-v2-root", "resp-compact", "unused", "unused")
	compact.CompressionStages = 1
	compact.StorageMaxBytes = 32 << 20
	compact.SegmentPayload = `{
		"history_key":"input",
		"input":[{"type":"compaction_trigger"}],
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ignored side output"}]},
			{"type":"compaction","encrypted_content":"opaque-compact-v2"}
		],
		"replacement_history":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"retain the original task"}]},
			{"type":"compaction","encrypted_content":"opaque-compact-v2"}
		]
	}`
	if _, err = store.CommitGoalTurn(ctx, compact); err != nil {
		t.Fatal(err)
	}
	boundaryBody, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	boundaryText := string(boundaryBody)
	for _, marker := range []string{"old_patch", "900719925474099312345", "old base instructions", "retain the original task", "opaque-compact-v2"} {
		if strings.Count(boundaryText, marker) != 1 {
			t.Fatalf("compact boundary marker %q count=%d body=%s", marker, strings.Count(boundaryText, marker), boundaryText)
		}
	}
	if strings.Index(boundaryText, `"type":"additional_tools"`) < 0 || strings.Index(boundaryText, `"type":"additional_tools"`) > strings.Index(boundaryText, "retain the original task") || strings.Contains(boundaryText, "old-tool-noise") || strings.Contains(boundaryText, "old-reasoning") || strings.Contains(boundaryText, "old-call") {
		t.Fatalf("compact boundary lost Lite prefix or retained compacted noise: %s", boundaryText)
	}

	followup := goalTurnForTest("compact-v2-root", "resp-after-compact", "unused", "unused")
	followup.CompressionStages = 1
	followup.StorageMaxBytes = 32 << 20
	followup.SegmentPayload = `{
		"history_key":"input",
		"replace_input":true,
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"apply_patch","format":{"const":900719925474099312345}}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"current base instructions"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"retain the original task"}]},
			{"type":"compaction","id":"cmp-new-id","encrypted_content":"opaque-compact-v2"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>fresh</environment_context>"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue after compact"}]}
		],
		"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"continued result"}]}]
	}`
	if _, err = store.CommitGoalTurn(ctx, followup); err != nil {
		t.Fatal(err)
	}

	assertReplay := func(stage string) {
		t.Helper()
		body, _, replayErr := store.BuildGoalReplay(ctx, goal.ID)
		if replayErr != nil {
			t.Fatalf("%s replay: %v", stage, replayErr)
		}
		text := string(body)
		for _, marker := range []string{"apply_patch", "900719925474099312345", "current base instructions", "retain the original task", "opaque-compact-v2", "fresh", "continue after compact", "continued result"} {
			if strings.Count(text, marker) != 1 {
				t.Fatalf("%s marker %q count=%d body=%s", stage, marker, strings.Count(text, marker), text)
			}
		}
		for _, stale := range []string{"old_patch", "old base instructions", "old-reasoning", "old-call", "old-tool-noise", "ignored side output"} {
			if strings.Contains(text, stale) {
				t.Fatalf("%s retained compacted history %q", stage, stale)
			}
		}
	}
	assertReplay("live segments")
	if err = store.CompactGoalSegmentsWithRatio(ctx, goal.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	assertReplay("compacted chunks")
}

func TestCodexLegacyCompactionReplayRequiresTriggerAndUniqueOutput(t *testing.T) {
	ctx := context.Background()
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	first := goalTurnForTest("compact-legacy-root", "legacy-r1", "history-must-stay", "first-output")
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := goalTurnForTest("compact-legacy-root", "legacy-r2", "ordinary", "unused")
	ordinary.SegmentPayload = `{"history_key":"input","input":[{"role":"user","content":"ordinary"}],"output":[{"type":"compaction","encrypted_content":"not-a-boundary"}]}`
	if _, err = store.CommitGoalTurn(ctx, ordinary); err != nil {
		t.Fatal(err)
	}
	ambiguous := goalTurnForTest("compact-legacy-root", "legacy-r3", "unused", "unused")
	ambiguous.SegmentPayload = `{"history_key":"input","input":[{"type":"compaction_trigger"}],"output":[{"type":"compaction","encrypted_content":"one"},{"type":"compaction_summary","encrypted_content":"two"}]}`
	if _, err = store.CommitGoalTurn(ctx, ambiguous); err != nil {
		t.Fatal(err)
	}
	body, _, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || !strings.Contains(string(body), "history-must-stay") || !strings.Contains(string(body), "first-output") {
		t.Fatalf("non-boundary compaction erased history body=%s err=%v", body, err)
	}

	// Segments written before codex_compaction_evaluated existed inferred a valid
	// boundary from the terminal trigger plus exactly one compaction output. Keep
	// reading that format so existing encrypted goal chains remain resumable.
	legacyBoundary := goalTurnForTest("compact-legacy-root", "legacy-r4", "unused", "unused")
	legacyBoundary.SegmentPayload = `{"history_key":"input","input":[{"type":"compaction_trigger"}],"output":[{"type":"compaction","encrypted_content":"legacy-valid-boundary"}]}`
	if _, err = store.CommitGoalTurn(ctx, legacyBoundary); err != nil {
		t.Fatal(err)
	}
	body, _, err = store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, retained := range []string{"history-must-stay", "ordinary", "legacy-valid-boundary"} {
		if strings.Count(text, retained) != 1 {
			t.Fatalf("legacy boundary marker %q count=%d body=%s", retained, strings.Count(text, retained), text)
		}
	}
	for _, removed := range []string{"first-output", "not-a-boundary", `"encrypted_content":"one"`, `"encrypted_content":"two"`} {
		if strings.Contains(text, removed) {
			t.Fatalf("legacy boundary retained compacted marker %q body=%s", removed, text)
		}
	}
}

func TestCodexRemoteCompactionReplacementUsesLogicalIncrementalInput(t *testing.T) {
	logical := []interface{}{
		map[string]interface{}{"type": "message", "role": "developer", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": "stale developer"}}},
		map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": "real task"}}},
		map[string]interface{}{"type": "reasoning", "encrypted_content": "old"},
		map[string]interface{}{"type": "compaction_trigger"},
	}
	output := []interface{}{
		map[string]interface{}{"type": "message", "role": "assistant", "content": "ignored"},
		map[string]interface{}{"type": "compaction_summary", "encrypted_content": "opaque"},
	}
	replacement, ok := CodexRemoteCompactionV2Replacement(logical, output)
	if !ok || len(replacement) != 2 {
		t.Fatalf("replacement=%+v ok=%v", replacement, ok)
	}
	text := fmt.Sprintf("%v", replacement)
	if !strings.Contains(text, "real task") || !strings.Contains(text, "opaque") || strings.Contains(text, "stale developer") || strings.Contains(text, "reasoning") {
		t.Fatalf("official retention mismatch: %+v", replacement)
	}
	compaction, _ := replacement[len(replacement)-1].(map[string]interface{})
	if compaction["type"] != "compaction" {
		t.Fatalf("compaction alias was not canonicalized like ResponseItem serde: %+v", compaction)
	}

	for _, malformed := range []interface{}{
		[]interface{}{map[string]interface{}{"type": "compaction"}},
		[]interface{}{map[string]interface{}{"type": "compaction", "encrypted_content": 7}},
		map[string]interface{}{"type": "compaction", "encrypted_content": "not-an-output-array"},
	} {
		if got, accepted := CodexRemoteCompactionV2Replacement(logical, malformed); accepted || got != nil {
			t.Fatalf("malformed compaction installed replacement=%+v accepted=%v output=%+v", got, accepted, malformed)
		}
	}
}
