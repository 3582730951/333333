package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"github.com/gorilla/websocket"
)

type recordingWebSocketWriter struct {
	mu       sync.Mutex
	messages [][]byte
	notify   chan struct{}
}

func (w *recordingWebSocketWriter) WriteMessage(messageType int, data []byte) error {
	if messageType != websocket.TextMessage {
		return nil
	}
	w.mu.Lock()
	w.messages = append(w.messages, append([]byte(nil), data...))
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
	return nil
}

func (w *recordingWebSocketWriter) lastMessage() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.messages) == 0 {
		return nil
	}
	return append([]byte(nil), w.messages[len(w.messages)-1]...)
}

type streamingRecordingWebSocketWriter struct {
	recordingWebSocketWriter
}

func (w *streamingRecordingWebSocketWriter) NextWriter(messageType int) (io.WriteCloser, error) {
	return &recordingMessage{messageType: messageType, owner: w}, nil
}

type recordingMessage struct {
	messageType int
	owner       *streamingRecordingWebSocketWriter
	body        bytes.Buffer
}

func (w *recordingMessage) Write(payload []byte) (int, error) { return w.body.Write(payload) }

func (w *recordingMessage) Close() error {
	return w.owner.WriteMessage(w.messageType, w.body.Bytes())
}

func TestResponsesWebSocketRequestConversionPreservesContextBytes(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"gpt-5.6-sol","instructions":"keep","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	kind, body, err := responsesWebSocketRequestToBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "response.create" {
		t.Fatalf("kind = %q", kind)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "instructions", "previous_response_id", "tools", "input"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatalf("context field %q changed\nbefore=%s\n after=%s", key, before[key], after[key])
		}
	}
	if _, present := after["type"]; present {
		t.Fatalf("downstream-only type remained: %s", body)
	}
	if string(after["stream"]) != "true" {
		t.Fatalf("stream default missing: %s", body)
	}
}

func TestResponsesWebSocketAppendUsesTheSameLosslessRequestEnvelope(t *testing.T) {
	raw := []byte(`{"type":"response.append","model":"gpt-5.6-sol","previous_response_id":"resp_keep","reasoning":{"effort":"high"},"tools":[{"parameters":{"const":900719925474099312345}}],"input":[{"type":"custom_tool_call_output","call_id":"call_1","output":{"n":900719925474099312345}}]}`)
	kind, body, err := responsesWebSocketRequestToBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "response.append" {
		t.Fatalf("kind = %q", kind)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "previous_response_id", "reasoning", "tools", "input"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatalf("append field %q changed\nbefore=%s\n after=%s", key, before[key], after[key])
		}
	}
	if _, present := after["type"]; present || string(after["stream"]) != "true" {
		t.Fatalf("append envelope was not normalized: %s", body)
	}
}

func TestResponsesWebSocketAppendCompletesPreviousResponseID(t *testing.T) {
	state := &responsesWebSocketState{}
	state.observe([]byte(`{"type":"response.completed","response":{"id":"resp_ws_previous","output":[]}}`))
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":"next"}],"stream":true}`)
	completed, err := state.completeAppend(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := routing.JSONStringField(completed, "previous_response_id"); got != "resp_ws_previous" {
		t.Fatalf("previous_response_id=%q body=%s", got, completed)
	}
	explicit := []byte(`{"previous_response_id":"resp_client","input":[]}`)
	unchanged, err := state.completeAppend(explicit)
	if err != nil || !bytes.Equal(unchanged, explicit) {
		t.Fatalf("explicit client context changed: err=%v body=%s", err, unchanged)
	}
}

func TestResponsesWebSocketSourceConversionSpoolsAndPreservesUnknownToolOutput(t *testing.T) {
	toolOutput := strings.Repeat("x", 2<<20)
	raw := `{"type":"response.append","model":"gpt-5.6-sol","input":[{"type":"custom_tool_call_output","call_id":"historical_unknown_tool_id","output":"` + toolOutput + `"}]}`
	budget := bodysource.NewBudget(64<<10, 8<<20)
	source, meta, err := bodysource.CaptureJSON(context.Background(), strings.NewReader(raw), bodysource.CaptureOptions{
		MaxBytes: 4 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	}, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	kind, source, meta, err := responsesWebSocketRequestToSource(context.Background(), source, meta, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if kind != "response.append" || !meta.ClientToolResult || meta.Type != "" || !meta.Stream {
		t.Fatalf("kind=%q meta=%+v", kind, meta)
	}
	body, err := bodysource.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	if _, exists := root["type"]; exists || string(root["stream"]) != "true" || !bytes.Equal(root["input"], json.RawMessage(`[{"type":"custom_tool_call_output","call_id":"historical_unknown_tool_id","output":"`+toolOutput+`"}]`)) {
		t.Fatal("source conversion changed the tool result or omitted the stream default")
	}
	if snapshot := budget.Snapshot(); snapshot.SpoolUsed == 0 {
		t.Fatalf("large websocket message did not spill: %+v", snapshot)
	}
}

func TestResponsesWebSocketAppendSourceAddsOnlyPreviousResponseID(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"function_call_output","call_id":"unknown_old_call","output":"done"}]}`)
	source := bodysource.Bytes(raw)
	meta, err := bodysource.ScanJSON(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	state := &responsesWebSocketState{}
	state.observeMeta(bodysource.BodyMeta{Type: "response.completed", ResponseID: "resp_previous"})
	source, meta, err = state.completeAppendSource(context.Background(), source, meta, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	body, err := bodysource.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PreviousResponseID != "resp_previous" || routing.JSONStringField(body, "previous_response_id") != "resp_previous" || !bytes.Contains(body, []byte(`"call_id":"unknown_old_call"`)) {
		t.Fatalf("append source=%s meta=%+v", body, meta)
	}
}

func TestResponsesWebSocketWriterStreamsLargeSSEFrameThroughSpool(t *testing.T) {
	output := strings.Repeat("z", 2<<20)
	payload := []byte(`{"type":"response.completed","response":{"id":"resp_spooled","status":"completed"},"output":"` + output + `"}`)
	recorder := &streamingRecordingWebSocketWriter{recordingWebSocketWriter: recordingWebSocketWriter{notify: make(chan struct{}, 1)}}
	conn := newResponsesWebSocketConn(recorder)
	var observed bodysource.BodyMeta
	budget := bodysource.NewBudget(64<<10, 8<<20)
	writer := newResponsesWebSocketWriter(context.Background(), conn, func(meta bodysource.BodyMeta) { observed = meta }, bodysource.CaptureOptions{
		MaxBytes: 4 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	}, []byte("secret"))
	writer.Header().Set("Content-Type", "text/event-stream")
	wire := append(append([]byte("event: response.completed\r\ndata: "), payload...), []byte("\r\n\r\n")...)
	for len(wire) > 0 {
		size := 7919
		if size > len(wire) {
			size = len(wire)
		}
		if _, err := writer.Write(wire[:size]); err != nil {
			t.Fatal(err)
		}
		wire = wire[size:]
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := recorder.lastMessage(); !bytes.Equal(got, payload) {
		t.Fatalf("streamed websocket payload differs: got=%d want=%d", len(got), len(payload))
	}
	if observed.Type != "response.completed" || observed.ResponseID != "resp_spooled" {
		t.Fatalf("observed=%+v", observed)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("websocket response buffers leaked: %+v", snapshot)
	}
}

func TestResponsesWebSocketWriterPreservesCompactionAndCompletedUsage(t *testing.T) {
	opaque := strings.Repeat("opaque-compaction-", 20<<10)
	largeUsage := json.Number("900719925474099312345")
	payload, err := json.Marshal(map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id": "resp_compaction_usage_ws", "status": "completed",
			"output": []interface{}{map[string]interface{}{
				"type": "compaction", "id": "cmp_ws", "encrypted_content": opaque,
			}},
			"usage": map[string]interface{}{
				"input_tokens": largeUsage, "output_tokens": json.Number("41"), "total_tokens": largeUsage,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &streamingRecordingWebSocketWriter{recordingWebSocketWriter: recordingWebSocketWriter{notify: make(chan struct{}, 1)}}
	conn := newResponsesWebSocketConn(recorder)
	budget := bodysource.NewBudget(64<<10, 8<<20)
	writer := newResponsesWebSocketWriter(context.Background(), conn, nil, bodysource.CaptureOptions{
		MaxBytes: 4 << 20, MemoryThreshold: 64 << 10, TempDir: t.TempDir(), Budget: budget,
	}, []byte("secret"))
	writer.Header().Set("Content-Type", "text/event-stream")
	wire := append(append([]byte("event: response.completed\ndata: "), payload...), []byte("\n\n")...)
	for len(wire) > 0 {
		size := 8191
		if size > len(wire) {
			size = len(wire)
		}
		if _, err = writer.Write(wire[:size]); err != nil {
			t.Fatal(err)
		}
		wire = wire[size:]
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := recorder.lastMessage()
	if !bytes.Equal(got, payload) {
		t.Fatalf("completed compaction payload changed: got=%d want=%d", len(got), len(payload))
	}
	decoder := json.NewDecoder(bytes.NewReader(got))
	decoder.UseNumber()
	var event map[string]interface{}
	if err = decoder.Decode(&event); err != nil {
		t.Fatal(err)
	}
	response, _ := event["response"].(map[string]interface{})
	usage, _ := response["usage"].(map[string]interface{})
	if input, _ := usage["input_tokens"].(json.Number); input.String() != largeUsage.String() {
		t.Fatalf("large usage changed: got=%q want=%q", input.String(), largeUsage.String())
	}
	output, _ := response["output"].([]interface{})
	compaction, _ := output[0].(map[string]interface{})
	if compaction["type"] != "compaction" || compaction["id"] != "cmp_ws" || compaction["encrypted_content"] != opaque {
		t.Fatalf("websocket compaction changed: %#v", compaction)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("websocket compaction buffers leaked: %+v", snapshot)
	}
}

func TestResponsesWebSocketHeartbeatEmitsProtocolProgress(t *testing.T) {
	recorder := &recordingWebSocketWriter{notify: make(chan struct{}, 1)}
	conn := newResponsesWebSocketConn(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- keepResponsesWebSocketAlive(ctx, conn, 10*time.Millisecond)
	}()
	select {
	case <-recorder.notify:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("websocket protocol heartbeat was not emitted")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := string(recorder.lastMessage()); got != responseInProgressPayload {
		t.Fatalf("heartbeat = %q", got)
	}
}
