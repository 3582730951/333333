package api

import (
	"bytes"
	"net/http"
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
		idx := bytes.Index(w.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), w.buf[:idx+2]...)
		w.buf = w.buf[idx+2:]
		if bytes.Contains(frame, []byte(`"type":"response.completed"`)) || bytes.Contains(frame, []byte("event: response.completed")) {
			w.once.Do(func() { w.commitErr = w.commit() })
		}
		if _, err := w.dst.Write(frame); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}
func (w *terminalCommitWriter) Flush() {
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *terminalCommitWriter) Close() error {
	if len(w.buf) > 0 {
		_, err := w.dst.Write(w.buf)
		w.buf = nil
		return err
	}
	// Commit failure is intentionally not returned here.  Returning it after the
	// response.completed frame causes callers to classify a successful upstream turn
	// as an interrupted stream and can leave clients retrying indefinitely.
	return nil
}

func (w *terminalCommitWriter) PersistenceError() error { return w.commitErr }
