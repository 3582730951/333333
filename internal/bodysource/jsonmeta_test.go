package bodysource

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestScanJSONMetadataAndSpans(t *testing.T) {
	body := []byte(` {"model":"gpt-5.6-sol","stream":true,"prompt_cache_key":"cache-1","previous_response_id":"resp-1","input":[{"role":"user","content":"hello"}]} `)
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
	for key, want := range map[string]string{"model": `"gpt-5.6-sol"`, "stream": "true", "input": `[{"role":"user","content":"hello"}]`} {
		span, ok := meta.Fields[key]
		if !ok || string(body[span.Offset:span.Offset+span.Length]) != want {
			t.Fatalf("span %s=%+v value=%q", key, span, body[span.Offset:span.Offset+span.Length])
		}
	}
}

func TestScanJSONTracksFirstInputItemAndMembers(t *testing.T) {
	body := []byte(` {"input" : [ {"type":"message","content":"first"}, {"type":"message","content":"second"}], "store":true } `)
	meta, err := ScanJSON(context.Background(), Bytes(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := body[meta.FirstInputItem.Offset : meta.FirstInputItem.Offset+meta.FirstInputItem.Length]
	if string(first) != `{"type":"message","content":"first"}` {
		t.Fatalf("first input item=%q", first)
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
