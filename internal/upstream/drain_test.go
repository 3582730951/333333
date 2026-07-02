package upstream

import (
	"io"
	"strings"
	"testing"
)

func TestDrainAndCloseReadsAndCloses(t *testing.T) {
	body := &trackingReadCloser{Reader: strings.NewReader("ok")}

	raw, err := DrainAndClose(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
	if !body.closed {
		t.Fatal("body was not closed")
	}
}

func TestDrainAndCloseRejectsOversizedBodyAndCloses(t *testing.T) {
	body := &trackingReadCloser{Reader: io.LimitReader(repeatingByteReader('x'), drainAndCloseBodyLimit+1)}

	raw, err := DrainAndClose(body)
	if err == nil {
		t.Fatal("DrainAndClose returned nil error for oversized body")
	}
	if raw != nil {
		t.Fatalf("body = %d bytes, want nil on oversized response", len(raw))
	}
	if !strings.Contains(err.Error(), "upstream response body exceeds") {
		t.Fatalf("error = %q, want size context", err)
	}
	if !body.closed {
		t.Fatal("body was not closed")
	}
}

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type repeatingByteReader byte

func (r repeatingByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}
