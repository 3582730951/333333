package api

import (
	"bytes"
	"net/http"
	"sync"
)

// terminalCommitWriter withholds the terminal Responses SSE frame until commit has
// durably stored the replay journal. Earlier deltas remain fully streaming.
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
			if w.commitErr != nil {
				return len(p), w.commitErr
			}
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
	return w.commitErr
}
