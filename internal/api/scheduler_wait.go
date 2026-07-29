package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

type schedulerWaitKey struct{}
type schedulerWaitState struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	protocol  string
	stream    bool
	committed bool
	// closed is set once a terminal error frame has been written. Any late keepalive
	// tick that wins the lock afterwards is dropped, so no comment can trail an error.
	closed bool
}

func withSchedulerWait(ctx context.Context, w http.ResponseWriter, stream bool, protocol string) context.Context {
	return context.WithValue(ctx, schedulerWaitKey{}, &schedulerWaitState{w: w, stream: stream, protocol: protocol})
}

// withBufferedSchedulerWait prevents a nested user-group candidate from writing
// a terminal event directly to the already-heartbeating outer response. The
// candidate must first return an ordinary buffered HTTP failure so the group
// router can continue waiting or transfer to another target.
func withBufferedSchedulerWait(ctx context.Context, w http.ResponseWriter) context.Context {
	protocol := "openai"
	if current, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState); current != nil && current.protocol != "" {
		protocol = current.protocol
	}
	return withSchedulerWait(ctx, w, false, protocol)
}

func schedulerWaitCallback(ctx context.Context) func(string, time.Duration) {
	state, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState)
	if state == nil || !state.stream {
		return nil
	}
	return func(_ string, _ time.Duration) {
		state.writeKeepalive(": pool-scheduler-wait\n\n")
	}
}

func schedulerWaitProtocol(path string) string {
	if path == "/v1/responses" || path == "/v1/responses/compact" {
		return "responses"
	}
	return "openai"
}

// writeKeepalive emits a protocol event for Responses and Anthropic streams because
// their official eventsource consumers discard SSE comments without resetting the
// per-event idle timeout. Generic OpenAI streams retain the non-semantic comment.
func (state *schedulerWaitState) writeKeepalive(comment string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return
	}
	if !state.committed {
		state.w.Header().Set("Content-Type", "text/event-stream")
		state.w.Header().Set("Cache-Control", "no-cache")
		state.committed = true
	}
	frame := comment
	switch state.protocol {
	case "responses":
		frame = safetyBufferingHeartbeatFrame
	case "anthropic":
		frame = claudePingHeartbeatFrame
	}
	_, _ = state.w.Write([]byte(frame))
	if f, ok := state.w.(http.Flusher); ok {
		f.Flush()
	}
}

// startSchedulerWaitKeepalive emits protocol keepalives every interval until the
// returned stop func is called. It bridges a silent pre-first-token window (e.g. a
// Kiro cache-singleflight wait + token refresh + upstream time-to-first-byte) so the
// downstream SSE idle timer never fires ("idle timeout waiting for SSE").
//
// It returns a no-op when the request is not a committable stream or the interval is
// non-positive. stop() is SYNCHRONOUS and idempotent: it guarantees the keepalive
// goroutine has exited (and released the writer) before returning, so the caller may
// write real content immediately afterwards without racing a trailing comment. The
// keepalive commits through the shared schedulerWaitState, so every error exit that
// consults schedulerWaitTerminal still degrades correctly to an SSE error event.
func startSchedulerWaitKeepalive(ctx context.Context, interval time.Duration) func() {
	state, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState)
	if state == nil || !state.stream || interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer supervisor.Recover("api.scheduler_wait_keepalive")
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				state.writeKeepalive(": pool-keepalive\n\n")
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}
func schedulerWaitTerminal(ctx context.Context, _ string) bool {
	state, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.committed {
		return false
	}
	if state.protocol == "responses" {
		resp := map[string]interface{}{
			"id":     "resp_pool_scheduler_wait",
			"object": "response",
			"status": "failed",
			"error":  map[string]string{"code": "server_error", "message": publicRetryMessage},
		}
		_ = writeSSEEvent(state.w, "response.failed", map[string]interface{}{"response": resp})
		_, _ = state.w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := state.w.(http.Flusher); ok {
			f.Flush()
		}
		state.closed = true
		return true
	}
	data, _ := json.Marshal(map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": publicRetryMessage}})
	if state.protocol == "openai" {
		data, _ = json.Marshal(map[string]interface{}{"error": map[string]interface{}{"type": "server_error", "message": publicRetryMessage}})
	}
	_, _ = fmt.Fprintf(state.w, "event: error\ndata: %s\n\n", data)
	if state.protocol == "openai" {
		_, _ = state.w.Write([]byte("data: [DONE]\n\n"))
	}
	if f, ok := state.w.(http.Flusher); ok {
		f.Flush()
	}
	state.closed = true
	return true
}

func writeClaudeWaitError(ctx context.Context, heartbeat *claudeRefreshSSEHeartbeat, err error) bool {
	if heartbeat != nil && heartbeat.writeError(err) {
		return true
	}
	message := "claude upstream failed"
	if err != nil {
		message = err.Error()
	}
	return schedulerWaitTerminal(ctx, message)
}
