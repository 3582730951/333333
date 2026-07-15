package api

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

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
