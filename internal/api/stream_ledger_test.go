package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-account-pool/internal/bodysource"
)

func TestCodexStreamLedgerReleasesSupersededTerminalState(t *testing.T) {
	text := strings.Repeat("terminal-output-", 4096)
	recorder := newCodexStreamLedgerRecorder()
	defer recorder.Close()
	frames := []string{
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":" + strconvJSON(text) + "}\n\n",
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":" + strconvJSON(text) + "}]}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_terminal\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":" + strconvJSON(text) + "}]}]}}\n\n",
	}
	for _, frame := range frames {
		if _, err := recorder.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if recorder.text.Len() != 0 || recorder.added.Count() != 0 || recorder.done.Count() != 0 {
		t.Fatalf("superseded stream state retained: text=%d added=%d done=%d", recorder.text.Len(), recorder.added.Count(), recorder.done.Count())
	}
	if got := recorder.partialText(); got != text {
		t.Fatalf("terminal text changed: got=%d want=%d", len(got), len(text))
	}
	var response map[string]interface{}
	if err := json.Unmarshal(recorder.ResponseJSON(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "resp_terminal" || response["output_text"] != text || len(recorder.partialItems()) != 1 {
		t.Fatalf("terminal response changed: id=%v text=%d items=%d", response["id"], len(streamString(response["output_text"])), len(recorder.partialItems()))
	}
}

func TestCodexStreamLedgerKeepsDeltaFallbackForMinimalTerminal(t *testing.T) {
	recorder := newCodexStreamLedgerRecorder()
	defer recorder.Close()
	_, _ = recorder.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n"))
	_, _ = recorder.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_minimal\",\"status\":\"completed\"}}\n\n"))
	if recorder.text.Len() == 0 || recorder.partialText() != "fallback" {
		t.Fatal("minimal terminal lost delta fallback")
	}
	if !strings.Contains(string(recorder.ResponseJSON()), `"output_text":"fallback"`) {
		t.Fatalf("minimal terminal response=%s", recorder.ResponseJSON())
	}
}

func TestCodexStreamLedgerPreservesCompactionAndCompletedUsage(t *testing.T) {
	recorder := newCodexStreamLedgerRecorder()
	defer recorder.Close()
	frames := []string{
		`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"type":"compaction","id":"cmp_usage","encrypted_content":"opaque-usage-exact"}}

`,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_compaction_usage","model":"gpt-5.6-sol","status":"completed","output":[{"type":"compaction","id":"cmp_usage","encrypted_content":"opaque-usage-exact"}],"usage":{"input_tokens":272001,"output_tokens":37,"total_tokens":272038,"input_tokens_details":{"cached_tokens":123456}}}}

`,
	}
	for _, frame := range frames {
		if _, err := recorder.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if !recorder.completedSuccessfully() {
		t.Fatal("completed compaction terminal was not recognized")
	}

	decoder := json.NewDecoder(strings.NewReader(string(recorder.ResponseJSON())))
	decoder.UseNumber()
	var response map[string]interface{}
	if err := decoder.Decode(&response); err != nil {
		t.Fatal(err)
	}
	output, _ := response["output"].([]interface{})
	if len(output) != 1 {
		t.Fatalf("compaction output=%#v", response["output"])
	}
	compaction, _ := output[0].(map[string]interface{})
	if compaction["type"] != "compaction" || compaction["id"] != "cmp_usage" || compaction["encrypted_content"] != "opaque-usage-exact" {
		t.Fatalf("compaction changed: %#v", compaction)
	}
	usage, _ := response["usage"].(map[string]interface{})
	details, _ := usage["input_tokens_details"].(map[string]interface{})
	for field, want := range map[string]string{
		"input_tokens":  "272001",
		"output_tokens": "37",
		"total_tokens":  "272038",
	} {
		got, _ := usage[field].(json.Number)
		if got.String() != want {
			t.Fatalf("usage.%s=%q want=%q response=%s", field, got.String(), want, recorder.ResponseJSON())
		}
	}
	if got, _ := details["cached_tokens"].(json.Number); got.String() != "123456" {
		t.Fatalf("cached_tokens=%q response=%s", got.String(), recorder.ResponseJSON())
	}
}

func TestCodexStreamLedgerSpoolsAndPreservesLargeDeltaFrame(t *testing.T) {
	delta := strings.Repeat("large-\\\"-delta-\U0001f642-", 128<<10)
	payload, err := json.Marshal(map[string]interface{}{"type": "response.output_text.delta", "delta": delta})
	if err != nil {
		t.Fatal(err)
	}
	wire := append(append([]byte("event: response.output_text.delta\r\ndata: "), payload...), []byte("\r\n\r\n")...)
	budget := bodysource.NewBudget(64<<10, 16<<20)
	recorder := newCodexStreamLedgerRecorderWithOptions(context.Background(), bodysource.CaptureOptions{
		MaxBytes: 8 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	})
	for len(wire) > 0 {
		size := 7919
		if size > len(wire) {
			size = len(wire)
		}
		if _, err = recorder.Write(wire[:size]); err != nil {
			t.Fatal(err)
		}
		wire = wire[size:]
	}
	recorder.finish()
	if got := recorder.partialText(); got != delta {
		t.Fatalf("large delta changed: got=%d want=%d", len(got), len(delta))
	}
	if snapshot := budget.Snapshot(); snapshot.SpoolUsed == 0 {
		t.Fatalf("large delta did not spill: %+v", snapshot)
	}
	if err = recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("stream ledger buffers leaked: %+v", snapshot)
	}
}

func TestCodexStreamLedgerSpoolsLargeTerminalResponse(t *testing.T) {
	text := strings.Repeat("terminal-output-", 128<<10)
	payload, err := json.Marshal(map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id": "resp_large_terminal", "object": "response", "model": "gpt-5.6-sol", "status": "completed",
			"output": []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := append(append([]byte("event: response.completed\ndata: "), payload...), []byte("\n\n")...)
	budget := bodysource.NewBudget(64<<10, 16<<20)
	recorder := newCodexStreamLedgerRecorderWithOptions(context.Background(), bodysource.CaptureOptions{
		MaxBytes: 8 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	})
	for len(wire) > 0 {
		size := 8191
		if size > len(wire) {
			size = len(wire)
		}
		if _, err = recorder.Write(wire[:size]); err != nil {
			t.Fatal(err)
		}
		wire = wire[size:]
	}
	if !recorder.completedSuccessfully() {
		t.Fatal("large terminal event was not observed")
	}
	var response map[string]interface{}
	if err = json.Unmarshal(recorder.ResponseJSON(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "resp_large_terminal" || response["output_text"] != text {
		t.Fatalf("large terminal changed: id=%v text=%d", response["id"], len(streamString(response["output_text"])))
	}
	if snapshot := budget.Snapshot(); snapshot.SpoolUsed == 0 {
		t.Fatalf("large terminal did not spill: %+v", snapshot)
	}
	if err = recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("terminal buffers leaked: %+v", snapshot)
	}
}

func strconvJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
