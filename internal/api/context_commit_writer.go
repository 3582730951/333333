package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// terminalCommitWriter attempts a durable continuity commit before releasing the
// terminal Responses SSE frame.  A persistence fault must never turn an upstream
// success into an endless client-side "Working" state: the completed frame is still
// delivered and commitErr is retained for audit/background retry by the caller.
type terminalCommitWriter struct {
	dst       http.ResponseWriter
	commit    func() error
	buf       []byte
	deferred  [][]byte
	terminal  bool
	once      sync.Once
	commitErr error
}

func newTerminalCommitWriter(dst http.ResponseWriter, commit func() error) *terminalCommitWriter {
	return &terminalCommitWriter{dst: dst, commit: commit}
}

func (w *terminalCommitWriter) Header() http.Header    { return w.dst.Header() }
func (w *terminalCommitWriter) WriteHeader(status int) { w.dst.WriteHeader(status) }
func (w *terminalCommitWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(w.buf)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		frame := append([]byte(nil), w.buf[:frameEnd]...)
		w.buf = w.buf[frameEnd:]
		if err := w.writeFrame(frame); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

func (w *terminalCommitWriter) writeFrame(frame []byte) error {
	eventType, data := sseFrameEventData(frame)
	if len(data) > 0 && !bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &envelope) == nil && strings.TrimSpace(envelope.Type) != "" {
			eventType = strings.TrimSpace(envelope.Type)
		}
	}
	isCompleted := eventType == "response.completed"
	isTerminal := isCompleted || eventType == "response.incomplete" || eventType == "response.failed" || eventType == "response.error" || eventType == "error"
	isDone := bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
	if isDone && !w.terminal {
		// A malformed/truncated upstream can emit [DONE] without a Responses
		// terminal. Keep it off the wire until we know whether a native EOF
		// continuation is needed; otherwise a client would close before it.
		w.deferred = append(w.deferred, append([]byte(nil), frame...))
		return nil
	}
	if isCompleted {
		w.once.Do(func() { w.commitErr = w.commit() })
	}
	if isTerminal {
		w.terminal = true
	}
	if _, err := w.dst.Write(frame); err != nil {
		return err
	}
	if isTerminal && len(w.deferred) > 0 {
		for _, deferred := range w.deferred {
			if _, err := w.dst.Write(deferred); err != nil {
				return err
			}
		}
		w.deferred = nil
	}
	return nil
}
func (w *terminalCommitWriter) Flush() {
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *terminalCommitWriter) Close() error {
	if len(w.buf) > 0 {
		frame := append([]byte(nil), w.buf...)
		w.buf = nil
		if err := w.writeFrame(frame); err != nil {
			return err
		}
	}
	// A deferred [DONE] without a preceding terminal is intentionally discarded:
	// the caller either starts one native continuation or writes response.failed.
	w.deferred = nil
	// Commit failure is intentionally not returned here.  Returning it after the
	// response.completed frame causes callers to classify a successful upstream turn
	// as an interrupted stream and can leave clients retrying indefinitely.
	return nil
}

func (w *terminalCommitWriter) PersistenceError() error { return w.commitErr }
