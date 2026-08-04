package api

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

type flushSnapshotWriter struct {
	mu      sync.Mutex
	body    bytes.Buffer
	flushes chan []byte
}

func newFlushSnapshotWriter() *flushSnapshotWriter {
	return &flushSnapshotWriter{flushes: make(chan []byte, 8)}
}

func (w *flushSnapshotWriter) Header() http.Header { return make(http.Header) }
func (w *flushSnapshotWriter) WriteHeader(int)     {}

func (w *flushSnapshotWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *flushSnapshotWriter) Flush() {
	w.mu.Lock()
	snapshot := append([]byte(nil), w.body.Bytes()...)
	w.mu.Unlock()
	w.flushes <- snapshot
}

func (w *flushSnapshotWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.body.Bytes()...)
}

func TestCompleteSSEPrefixLenSupportsLFCRLFAndMixedBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		wire string
		want string
	}{
		{name: "lf", wire: "data: one\n\ndata: tail", want: "data: one\n\n"},
		{name: "crlf", wire: "data: one\r\n\r\ndata: tail", want: "data: one\r\n\r\n"},
		{name: "crlf_then_lf", wire: "data: one\r\n\ndata: tail", want: "data: one\r\n\n"},
		{name: "lf_then_crlf", wire: "data: one\n\r\ndata: tail", want: "data: one\n\r\n"},
		{name: "last_complete_prefix", wire: "data: one\n\ndata: two\r\n\r\ndata: ta", want: "data: one\n\ndata: two\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := completeSSEPrefixLen([]byte(tc.wire))
			if got != len(tc.want) {
				t.Fatalf("complete prefix length=%d, want %d for %q", got, len(tc.want), tc.wire)
			}
		})
	}
}

func TestStreamCopyFlushesCompletePrefixAndRetainsHalfFrame(t *testing.T) {
	for _, tc := range []struct {
		name     string
		boundary string
	}{
		{name: "lf", boundary: "\n\n"},
		{name: "crlf", boundary: "\r\n\r\n"},
		{name: "crlf_then_lf", boundary: "\r\n\n"},
		{name: "lf_then_crlf", boundary: "\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			dst := newFlushSnapshotWriter()
			done := make(chan error, 1)
			go func() { done <- streamCopy(dst, reader) }()

			complete := []byte("data: {\"type\":\"response.created\"}" + tc.boundary)
			tail := []byte("data: {\"type\":\"response.output_text.delta\"")
			wire := append(append([]byte(nil), complete...), tail...)
			if _, err := writer.Write(wire); err != nil {
				t.Fatal(err)
			}

			select {
			case snapshot := <-dst.flushes:
				if !bytes.Equal(snapshot, complete) {
					t.Fatalf("first flush=%q, want only complete prefix %q", snapshot, complete)
				}
			case <-time.After(time.Second):
				t.Fatal("complete first SSE frame was not flushed while a half frame remained")
			}
			if got := dst.Bytes(); !bytes.Equal(got, complete) {
				t.Fatalf("unterminated tail reached downstream early: %q", got)
			}

			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := dst.Bytes(); !bytes.Equal(got, wire) {
				t.Fatalf("stream bytes changed:\nwant %q\n got %q", wire, got)
			}
		})
	}
}
