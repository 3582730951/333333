package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type schedulerWaitKey struct{}
type schedulerWaitState struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	protocol  string
	stream    bool
	committed bool
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
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.committed {
			state.w.Header().Set("Content-Type", "text/event-stream")
			state.w.Header().Set("Cache-Control", "no-cache")
			state.committed = true
		}
		_, _ = state.w.Write([]byte(": pool-scheduler-wait\n\n"))
		if f, ok := state.w.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func schedulerWaitTerminal(ctx context.Context, message string) bool {
	state, _ := ctx.Value(schedulerWaitKey{}).(*schedulerWaitState)
	if state == nil {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.committed {
		return false
	}
	data, _ := json.Marshal(map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": message}})
	if state.protocol == "openai" {
		data, _ = json.Marshal(map[string]interface{}{"error": map[string]interface{}{"type": "server_error", "message": message}})
	}
	_, _ = fmt.Fprintf(state.w, "event: error\ndata: %s\n\n", data)
	if state.protocol == "openai" {
		_, _ = state.w.Write([]byte("data: [DONE]\n\n"))
	}
	if f, ok := state.w.(http.Flusher); ok {
		f.Flush()
	}
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
