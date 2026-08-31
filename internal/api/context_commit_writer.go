package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// The byte budget bounds retained wire data; the frame budget separately bounds
// slice-header overhead from a malformed stream that repeats tiny DONE frames.
const terminalCommitWriterMaxDeferredFrames = 64

var (
	errTerminalCommitFrameTooLarge        = errors.New("terminal commit partial SSE frame exceeds limit")
	errTerminalCommitDeferredDoneTooLarge = errors.New("terminal commit SSE deferred DONE exceeds limit")
)

// terminalCommitWriter attempts a durable continuity commit before releasing the
// terminal Responses SSE frame.  A persistence fault must never turn an upstream
// success into an endless client-side "Working" state: the completed frame is still
// delivered and commitErr is retained for audit/background retry by the caller.
type terminalCommitWriter struct {
	dst           http.ResponseWriter
	commit        func() error
	afterTerminal func()
	buf           []byte
	deferred      [][]byte
	deferredBytes int
	terminal      bool
	once          sync.Once
	afterOnce     sync.Once
	commitErr     error
	writeErr      error
}

func newTerminalCommitWriter(dst http.ResponseWriter, commit func() error) *terminalCommitWriter {
	return &terminalCommitWriter{dst: dst, commit: commit}
}

// newTerminalCommitWriterAfter installs non-critical work that must happen only
// after the completed frame was actually written to the downstream connection.
// It is intentionally fire-and-forget from the stream's perspective: a durable
// post-terminal intent may fail or be cancelled without changing already-visible
// protocol bytes.
func newTerminalCommitWriterAfter(dst http.ResponseWriter, commit func() error, afterTerminal func()) *terminalCommitWriter {
	return &terminalCommitWriter{dst: dst, commit: commit, afterTerminal: afterTerminal}
}

func (w *terminalCommitWriter) Header() http.Header    { return w.dst.Header() }
func (w *terminalCommitWriter) WriteHeader(status int) { w.dst.WriteHeader(status) }
func (w *terminalCommitWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	accepted := 0
	for len(p) > 0 {
		// The frame-aligned relay normally gives us complete frames. Process those
		// directly from the caller's buffer so a large, already-complete legal frame
		// does not get copied into the partial-frame buffer.
		if len(w.buf) == 0 {
			if boundary, separatorLen := sseFrameBoundary(p); boundary >= 0 {
				frameEnd := boundary + separatorLen
				if err := w.writeFrame(p[:frameEnd]); err != nil {
					return accepted, w.fail(err)
				}
				accepted += frameEnd
				p = p[frameEnd:]
				continue
			}
			if len(p) > streamLedgerMaxPartialFrame {
				return accepted, w.fail(errTerminalCommitFrameTooLarge)
			}
			w.buf = appendTerminalCommitBytes(w.buf, p)
			accepted += len(p)
			break
		}

		if suffixEnd := terminalCommitFrameSuffixEnd(w.buf, p); suffixEnd >= 0 {
			if len(w.buf)+suffixEnd > streamLedgerMaxPartialFrame {
				return accepted, w.fail(errTerminalCommitFrameTooLarge)
			}
			w.buf = appendTerminalCommitBytes(w.buf, p[:suffixEnd])
			accepted += suffixEnd
			p = p[suffixEnd:]
			if err := w.writeFrame(w.buf); err != nil {
				return accepted, w.fail(err)
			}
			w.buf = w.buf[:0]
			continue
		}
		if len(w.buf)+len(p) > streamLedgerMaxPartialFrame {
			return accepted, w.fail(errTerminalCommitFrameTooLarge)
		}
		w.buf = appendTerminalCommitBytes(w.buf, p)
		accepted += len(p)
		break
	}
	return accepted, nil
}

// terminalCommitFrameSuffixEnd returns the number of suffix bytes through the
// first frame delimiter in prefix+suffix. prefix is known not to contain a full
// delimiter, so only a split delimiter at the join needs a bounded bridge scan.
func terminalCommitFrameSuffixEnd(prefix, suffix []byte) int {
	tailStart := len(prefix) - 3
	if tailStart < 0 {
		tailStart = 0
	}
	headLen := len(suffix)
	if headLen > 3 {
		headLen = 3
	}
	var bridge [6]byte
	bridgeLen := copy(bridge[:], prefix[tailStart:])
	bridgeLen += copy(bridge[bridgeLen:], suffix[:headLen])
	if boundary, separatorLen := sseFrameBoundary(bridge[:bridgeLen]); boundary >= 0 {
		start := tailStart + boundary
		end := start + separatorLen
		if start < len(prefix) && end > len(prefix) {
			return end - len(prefix)
		}
	}
	if boundary, separatorLen := sseFrameBoundary(suffix); boundary >= 0 {
		return boundary + separatorLen
	}
	return -1
}

func appendTerminalCommitBytes(dst, src []byte) []byte {
	needed := len(dst) + len(src)
	if needed <= cap(dst) {
		return append(dst, src...)
	}
	capacity := cap(dst) * 2
	if capacity < needed {
		capacity = needed
	}
	if capacity > streamLedgerMaxPartialFrame {
		capacity = streamLedgerMaxPartialFrame
	}
	next := make([]byte, len(dst), capacity)
	copy(next, dst)
	return append(next, src...)
}

func (w *terminalCommitWriter) fail(err error) error {
	if err == nil {
		return nil
	}
	if w.writeErr == nil {
		w.writeErr = err
	}
	w.buf = nil
	w.deferred = nil
	w.deferredBytes = 0
	return w.writeErr
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
		if len(w.deferred) >= terminalCommitWriterMaxDeferredFrames || len(frame) > streamLedgerMaxPartialFrame-w.deferredBytes {
			return errTerminalCommitDeferredDoneTooLarge
		}
		if w.deferred == nil {
			w.deferred = make([][]byte, 0, terminalCommitWriterMaxDeferredFrames)
		}
		deferred := make([]byte, len(frame))
		copy(deferred, frame)
		w.deferred = append(w.deferred, deferred)
		w.deferredBytes += len(frame)
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
	if isCompleted && w.afterTerminal != nil {
		w.afterOnce.Do(w.afterTerminal)
	}
	if isTerminal && len(w.deferred) > 0 {
		for _, deferred := range w.deferred {
			if _, err := w.dst.Write(deferred); err != nil {
				return err
			}
		}
		w.deferred = nil
		w.deferredBytes = 0
	}
	return nil
}
func (w *terminalCommitWriter) Flush() {
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *terminalCommitWriter) Close() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	if len(w.buf) > 0 {
		frame := w.buf
		w.buf = nil
		if err := w.writeFrame(frame); err != nil {
			return w.fail(err)
		}
	}
	// A deferred [DONE] without a preceding terminal is intentionally discarded:
	// the caller either starts one native continuation or writes response.failed.
	w.deferred = nil
	w.deferredBytes = 0
	// Commit failure is intentionally not returned here.  Returning it after the
	// response.completed frame causes callers to classify a successful upstream turn
	// as an interrupted stream and can leave clients retrying indefinitely.
	return nil
}

func (w *terminalCommitWriter) PersistenceError() error { return w.commitErr }
