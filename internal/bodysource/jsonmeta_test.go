package bodysource

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestScanJSONMetadataAndSpans(t *testing.T) {
	body := []byte(` {"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"cache-1","previous_response_id":"resp-1","input":[{"role":"user","content":"goal status"}]} `)
	meta, err := ScanJSON(context.Background(), Bytes(body), []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Model != "gpt-5.6-sol" || !meta.StreamPresent || !meta.Stream || meta.PromptCacheKey != "cache-1" || meta.PreviousResponseID != "resp-1" {
		t.Fatalf("metadata=%+v", meta)
	}
	if meta.Size != int64(len(body)) || meta.StablePrefixHMAC == "" || meta.EstimatedTokens <= 0 {
		t.Fatalf("scan counters=%+v", meta)
	}
	if meta.InputItemCount != 1 || meta.LastInputRole != "user" || !meta.GoalSignalCandidate || !meta.GoalSignalQualified {
		t.Fatalf("input identity metadata=%+v", meta)
	}
	for key, want := range map[string]string{"model": `"gpt-5.6-sol"`, "stream": "true", "input": `[{"role":"user","content":"goal status"}]`} {
		span, ok := meta.Fields[key]
		if !ok || string(body[span.Offset:span.Offset+span.Length]) != want {
			t.Fatalf("span %s=%+v value=%q", key, span, body[span.Offset:span.Offset+span.Length])
		}
	}
}

func TestScanJSONGoalCandidateCrossesReaderSegments(t *testing.T) {
	padding := strings.Repeat("x", DefaultChunkSize-2)
	body := []byte(`{"input":"` + padding + `goal"}`)
	meta, err := ScanJSON(context.Background(), Bytes(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.GoalSignalCandidate {
		t.Fatal("goal marker crossing a buffered read boundary was missed")
	}
	if meta.GoalSignalQualified {
		t.Fatal("an unqualified random goal token became a Goal state signal")
	}
	ordinary, err := ScanJSON(context.Background(), Bytes([]byte(`{"input":"ordinary"}`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.GoalSignalCandidate {
		t.Fatal("ordinary request became a Goal candidate")
	}
	qualified, err := ScanJSON(context.Background(), Bytes([]byte(`{"input":[{"type":"function_call_output","call_id":"goal-call","output":"{\"goal\":{\"status\":\"active\"}}"}]}`)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !qualified.GoalSignalCandidate || !qualified.GoalSignalQualified {
		t.Fatalf("Goal output qualification metadata=%+v", qualified)
	}
}

func TestScanJSONTracksFirstInputItemAndMembers(t *testing.T) {
	body := []byte(` {"input" : [ {"type":"message","role":"user","content":"first"}, {"type":"agent_message","role":"assistant","content":"second"}], "store":true } `)
	meta, err := ScanJSON(context.Background(), Bytes(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := body[meta.FirstInputItem.Offset : meta.FirstInputItem.Offset+meta.FirstInputItem.Length]
	if string(first) != `{"type":"message","role":"user","content":"first"}` {
		t.Fatalf("first input item=%q", first)
	}
	if meta.InputItemCount != 2 || meta.LastInputRole != "assistant" || meta.LastInputType != "agent_message" {
		t.Fatalf("last input identity=%+v", meta)
	}
	if meta.ObjectEnd <= 0 || meta.MemberCount != 2 || meta.Members["input"].Length == 0 || meta.Kinds["store"] != 't' || string(meta.Scalars["store"]) != "true" {
		t.Fatalf("metadata=%+v", meta)
	}
}

func TestScanJSONTracksWebSocketTerminalAndToolSemantics(t *testing.T) {
	body := []byte(`{"type":"response.completed","response":{"id":"resp_ws","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}]}}`)
	meta, err := ScanJSON(context.Background(), Bytes(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Type != "response.completed" || meta.ResponseID != "resp_ws" || meta.ResponseModel != "gpt-5.6-sol" || meta.Status != "completed" || !meta.ClientToolResult || !meta.ToolContext {
		t.Fatalf("websocket metadata=%+v", meta)
	}
	compaction := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger"}]}`)
	meta, err = ScanJSON(context.Background(), Bytes(compaction), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.CompactionTrigger {
		t.Fatalf("compaction metadata=%+v", meta)
	}
	compaction = []byte(`{"model":"gpt-5.6-sol","compaction_trigger":true,"input":"compact"}`)
	meta, err = ScanJSON(context.Background(), Bytes(compaction), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.CompactionTrigger {
		t.Fatalf("top-level compaction metadata=%+v", meta)
	}
}

func TestScanJSONDoesNotCaptureLargeStrings(t *testing.T) {
	large := strings.Repeat("x", 4<<20)
	body := []byte(`{"model":"gpt-5.6-sol","input":"` + large + `","stream":false}`)
	meta, err := ScanJSON(context.Background(), Bytes(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Model != "gpt-5.6-sol" || !meta.StreamPresent || meta.Stream || meta.Fields["input"].Length != int64(len(large)+2) {
		t.Fatalf("metadata=%+v", meta)
	}
}

func TestScanJSONMarksNestedPromptCacheBreakpointWithoutCapturingPayload(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","prompt_cache_options":{"mode":"explicit"},"input":[{"role":"system","content":[{"type":"input_text","text":"keep","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":[{"type":"input_text","text":"prompt_cache_breakpoint is harmless text"}]}]}`)
	meta, err := ScanJSON(context.Background(), Bytes(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.PromptCacheBreakpoint {
		t.Fatal("nested prompt_cache_breakpoint was not marked")
	}
	if _, ok := meta.Fields["prompt_cache_options"]; !ok {
		t.Fatal("top-level prompt_cache_options was not tracked for source patching")
	}
}

func TestCaptureJSONConsumesOnceAndReplays(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","stream":true,"input":"` + strings.Repeat("x", 1<<20) + `"}`
	budget := NewBudget(64<<10, 2<<20)
	source, meta, err := CaptureJSON(context.Background(), strings.NewReader(body), CaptureOptions{MaxBytes: 2 << 20, MemoryThreshold: 64 << 10, ExpectedBytes: int64(len(body)), TempDir: t.TempDir(), Budget: budget}, []byte("key"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if meta.Model != "gpt-5.6-sol" || !meta.Stream || meta.Size != int64(len(body)) {
		t.Fatalf("metadata=%+v", meta)
	}
	for i := 0; i < 2; i++ {
		got, err := ReadAll(source)
		if err != nil || string(got) != body {
			t.Fatalf("replay %d len=%d err=%v", i, len(got), err)
		}
	}
}

func TestCaptureJSONRejectsIncorrectExpectedBytes(t *testing.T) {
	body := `{"model":"gpt-5.6-sol","input":[]}`
	source, _, err := CaptureJSON(context.Background(), strings.NewReader(body), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, ExpectedBytes: int64(len(body) - 1), Budget: NewBudget(1<<20, 0)}, nil)
	if source != nil {
		_ = source.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "body size changed") {
		t.Fatalf("error=%v", err)
	}
}

func TestScanJSONRejectsInvalidAndDeepBodies(t *testing.T) {
	for _, body := range []string{``, `[]`, `{"a":01}`, `{"a":"\x"}`, `{"a":true} trailing`, `{"a":[1,]}`} {
		if _, err := ScanJSON(context.Background(), Bytes([]byte(body)), nil); err == nil {
			t.Fatalf("accepted invalid JSON %q", body)
		}
	}
	deep := `{"input":` + strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1) + "}"
	if _, err := ScanJSON(context.Background(), Bytes([]byte(deep)), nil); !errors.Is(err, ErrJSONDepth) {
		t.Fatalf("deep error=%v", err)
	}
}

func FuzzScanJSON(f *testing.F) {
	for _, seed := range []string{`{}`, `{"model":"gpt-5.6-sol","stream":true,"input":[]}`, `{"x":[null,false,1.25e+3,{"s":"\\u263a"}]}`, `{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`, `{"input":[{"type":"function_call_output","call_id":"unknown","output":"x"}]}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		_, err := ScanJSON(context.Background(), Bytes(body), []byte("fuzz"))
		if err == nil {
			if !json.Valid(body) {
				t.Fatalf("scanner accepted invalid JSON: %q", body)
			}
			var root any
			if unmarshalErr := json.Unmarshal(body, &root); unmarshalErr != nil {
				t.Fatalf("scanner accepted JSON rejected by encoding/json: %v", unmarshalErr)
			}
			if _, object := root.(map[string]any); !object {
				t.Fatalf("scanner accepted non-object root: %T", root)
			}
		}
		if json.Valid(body) {
			var root any
			if json.Unmarshal(body, &root) == nil {
				if _, object := root.(map[string]any); object && err != nil {
					t.Fatalf("valid object rejected: %v", err)
				}
			}
		}
	})
}
