package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/streamrewrite"
)

func TestStreamAccumulatorSpillsReplaysAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	budget := bodysource.NewBudget(64, 1<<20)
	acc := newStreamAccumulator(context.Background(), bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 32, TempDir: dir, Budget: budget,
	}, "stream-accumulator-test-*")
	want := strings.Repeat("large-tool-argument-", 8192)
	if err := acc.WriteString(want[:len(want)/2]); err != nil {
		t.Fatal(err)
	}
	if err := acc.WriteString(want[len(want)/2:]); err != nil {
		t.Fatal(err)
	}
	if !acc.buffer.Spilled() {
		t.Fatal("large stream value did not spill")
	}
	got, err := acc.String()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("replayed stream value differs: got=%d want=%d", len(got), len(want))
	}
	if err = acc.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("spool cleanup entries=%v err=%v", entries, err)
	}
	snapshot := budget.Snapshot()
	if snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("budget leaked after close: %+v", snapshot)
	}
}

func TestStreamAccumulatorRejectsConfiguredLimit(t *testing.T) {
	acc := newStreamAccumulator(context.Background(), bodysource.CaptureOptions{MaxBytes: 64, MemoryThreshold: 8, TempDir: t.TempDir()}, "stream-limit-test-*")
	defer acc.Close()
	if err := acc.WriteString(strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}
	if err := acc.WriteString("y"); !errors.Is(err, bodysource.ErrBodyTooLarge) {
		t.Fatalf("limit error=%v", err)
	}
}

func TestForEachSSEFrameSpoolsFrameBeyondLegacyScannerLimit(t *testing.T) {
	dir := t.TempDir()
	payload := strings.Repeat("z", 17<<20)
	raw := "event: response.output_text.delta\ndata: " + payload + "\n\n"
	options := bodysource.CaptureOptions{MaxBytes: 20 << 20, MemoryThreshold: 1024, TempDir: dir, Budget: bodysource.NewBudget(1024, 20<<20)}
	calls := 0
	if err := forEachSSEFrameWithOptions(context.Background(), strings.NewReader(raw), options, func(frame []byte) error {
		calls++
		if len(frame) != len(raw) || string(frame[:42]) != raw[:42] || string(frame[len(frame)-2:]) != "\n\n" {
			t.Fatalf("large frame was changed: got=%d want=%d", len(frame), len(raw))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("callback count=%d", calls)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("frame spool cleanup entries=%v err=%v", entries, err)
	}
}

func TestClaudeStreamTapSpoolsPartialText(t *testing.T) {
	dir := t.TempDir()
	text := strings.Repeat("partial-", 32768)
	data, _ := json.Marshal(map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	})
	tap := newClaudeStreamTap(context.Background(), bodysource.CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 32, TempDir: dir})
	if _, err := tap.Write([]byte("event: content_block_delta\ndata: " + string(data) + "\n\n")); err != nil {
		t.Fatal(err)
	}
	if tap.text == nil || !tap.text.buffer.Spilled() {
		t.Fatal("Claude partial text did not spill")
	}
	if got := tap.partialText(); got != text {
		t.Fatalf("partial text differs: got=%d want=%d", len(got), len(text))
	}
	if err := tap.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("Claude partial spool cleanup entries=%v err=%v", entries, err)
	}
}

func TestGoalResponseFromSSEPartsPreservesBeyondAliasSample(t *testing.T) {
	text := strings.Repeat("goal-context-", 30000)
	half := len(text) / 2
	event := func(value map[string]interface{}) string {
		raw, _ := json.Marshal(value)
		return "data: " + string(raw) + "\n\n"
	}
	first := event(map[string]interface{}{"type": "message_start", "message": map[string]interface{}{"id": "msg_initial", "role": "assistant"}}) +
		event(map[string]interface{}{"type": "content_block_start", "index": 0, "content_block": map[string]interface{}{"type": "text", "text": ""}}) +
		event(map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "text_delta", "text": text[:half]}})
	continuation := event(map[string]interface{}{"type": "content_block_delta", "index": 0, "delta": map[string]interface{}{"type": "text_delta", "text": text[half:]}}) +
		event(map[string]interface{}{"type": "content_block_stop", "index": 0}) +
		event(map[string]interface{}{"type": "message_stop"})
	raw := goalResponseFromSSEParts([]byte(first), []byte(continuation))
	var response map[string]interface{}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]interface{})[0].(map[string]interface{})
	message := output["assistant_message"].(map[string]interface{})
	content := message["content"].([]interface{})
	got := content[0].(map[string]interface{})["text"]
	if got != text {
		t.Fatalf("Goal text was truncated: got=%d want=%d", len(got.(string)), len(text))
	}
}

func TestChatResponsesBridgeSpoolsLargeToolArguments(t *testing.T) {
	dir := t.TempDir()
	arguments := `{"payload":"` + strings.Repeat("a", 256<<10) + `"}`
	half := len(arguments) / 2
	frame := func(value map[string]interface{}) string {
		raw, _ := json.Marshal(value)
		return "data: " + string(raw) + "\n\n"
	}
	stream := frame(map[string]interface{}{
		"id": "bridge-large", "choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"index": 0, "id": "call_large", "function": map[string]interface{}{"name": "lookup", "arguments": arguments[:half]},
			}}},
		}},
	}) + frame(map[string]interface{}{
		"id": "bridge-large", "choices": []interface{}{map[string]interface{}{
			"delta": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"index": 0, "function": map[string]interface{}{"arguments": arguments[half:]},
			}}},
		}},
	}) + "data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	chatStreamToResponsesSSEWithOptions(context.Background(), recorder, strings.NewReader(stream), "gpt-test", streamrewrite.New(nil), bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 32, TempDir: dir,
	})
	frames := parseSSE(t, recorder.Body.String())
	completed := idxOf(frames, "response.completed")
	if completed < 0 {
		t.Fatalf("response.completed missing: %s", recorder.Body.String())
	}
	response := frames[completed].data["response"].(map[string]interface{})
	output := response["output"].([]interface{})
	if got := output[0].(map[string]interface{})["arguments"]; got != arguments {
		t.Fatalf("tool arguments changed: got=%d want=%d", len(got.(string)), len(arguments))
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("chat bridge spool cleanup entries=%v err=%v", entries, err)
	}
}

func TestResponsesAnthropicBridgeSpoolsLargeToolArguments(t *testing.T) {
	dir := t.TempDir()
	arguments := `{"payload":"` + strings.Repeat("b", 256<<10) + `"}`
	event := func(name string, value map[string]interface{}) string {
		value["type"] = name
		raw, _ := json.Marshal(value)
		return "event: " + name + "\ndata: " + string(raw) + "\n\n"
	}
	stream := event("response.created", map[string]interface{}{"response": map[string]interface{}{"id": "resp_large", "model": "gpt-test"}}) +
		event("response.output_item.added", map[string]interface{}{
			"output_index": 0, "item": map[string]interface{}{"type": "function_call", "id": "fc_large", "call_id": "call_large", "name": "lookup", "arguments": ""},
		}) +
		event("response.function_call_arguments.delta", map[string]interface{}{"output_index": 0, "item_id": "fc_large", "delta": arguments}) +
		event("response.function_call_arguments.done", map[string]interface{}{"output_index": 0, "item_id": "fc_large", "arguments": arguments}) +
		event("response.completed", map[string]interface{}{"response": map[string]interface{}{"id": "resp_large", "status": "completed", "output": []interface{}{}}})
	recorder := httptest.NewRecorder()
	responsesStreamToAnthropicSSEWithOptions(context.Background(), recorder, strings.NewReader(stream), "gpt-test", nil, nil, streamrewrite.New(nil), bodysource.CaptureOptions{
		MaxBytes: 1 << 20, MemoryThreshold: 32, TempDir: dir,
	})
	frames := parseSSE(t, recorder.Body.String())
	found := false
	for _, frame := range frames {
		if frame.event != "content_block_delta" {
			continue
		}
		delta, _ := frame.data["delta"].(map[string]interface{})
		if delta["type"] == "input_json_delta" && delta["partial_json"] == arguments {
			found = true
		}
	}
	if !found {
		t.Fatal("large Anthropic tool argument delta missing or changed")
	}
	if idxOf(frames, "message_stop") < 0 {
		t.Fatal("Anthropic terminal missing")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("Anthropic bridge spool cleanup entries=%v err=%v", entries, err)
	}
}
