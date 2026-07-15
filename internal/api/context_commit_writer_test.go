package api

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTerminalCommitWriterCommitsBeforeCompletedFrame(t *testing.T) {
	recorder := httptest.NewRecorder()
	committed := false
	w := newTerminalCommitWriter(recorder, func() error {
		if strings.Contains(recorder.Body.String(), "response.completed") {
			t.Fatal("completion frame reached client before journal commit")
		}
		committed = true
		return nil
	})
	_, _ = w.Write([]byte("event: response.output_text.delta\ndata: {}\n\n"))
	_, err := w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if err != nil || !committed || !strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("commit/order failed: committed=%v err=%v body=%q", committed, err, recorder.Body.String())
	}
}

func TestTerminalCommitWriterSuppressesCompletedFrameOnCommitFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return errors.New("disk full") })
	_, err := w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if err == nil || strings.Contains(recorder.Body.String(), "response.completed") {
		t.Fatalf("completion must be withheld on failed commit: err=%v body=%q", err, recorder.Body.String())
	}
}
