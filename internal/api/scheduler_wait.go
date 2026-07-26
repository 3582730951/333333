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
func schedulerWaitCallback(ctx context.Context) func(string, time.Duration) {
	state, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState)
	if state == nil || !state.stream {
		return nil
	}
	return func(_ string, _ time.Duration) {
		state.writeComment(": pool-scheduler-wait\n\n")
	}
}

// writeComment emits a single SSE comment frame, committing the response (and its
// SSE headers) on first use. It holds the same lock as the terminal error writer, so
// a keepalive comment can never interleave with an `event: error` frame. SSE comments
// are ignored by clients but still reset intermediary/client idle timers.
func (state *schedulerWaitState) writeComment(comment string) {
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
	_, _ = state.w.Write([]byte(comment))
	if f, ok := state.w.(http.Flusher); ok {
		f.Flush()
	}
}

// startSchedulerWaitKeepalive emits SSE keepalive comments every interval until the
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
				state.writeComment(": pool-keepalive\n\n")
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
