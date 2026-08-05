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

func TestTerminalCommitWriterDeliversCompletedFrameOnCommitFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return errors.New("disk full") })
	_, err := w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	if err != nil || !strings.Contains(recorder.Body.String(), "response.completed") || w.PersistenceError() == nil {
		t.Fatalf("successful terminal must survive failed persistence: err=%v commit_err=%v body=%q", err, w.PersistenceError(), recorder.Body.String())
	}
}

func TestTerminalCommitWriterPreservesFragmentedFrames(t *testing.T) {
	recorder := httptest.NewRecorder()
	commits := 0
	w := newTerminalCommitWriter(recorder, func() error {
		commits++
		return nil
	})
	chunks := []string{
		"event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\r",
		"\n",
		"\r",
		"\nevent: response.completed\ndata: {\"type\":\"response.completed\"}",
	}
	for _, chunk := range chunks {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write n=%d want=%d err=%v", n, len(chunk), err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	want := strings.Join(chunks, "")
	if recorder.Body.String() != want || commits != 1 {
		t.Fatalf("commits=%d body_len=%d want_len=%d", commits, recorder.Body.Len(), len(want))
	}
}

func TestTerminalCommitWriterPartialBufferLimitBoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return nil })
	prefix := strings.Repeat(":", streamLedgerMaxPartialFrame-2)
	if n, err := w.Write([]byte(prefix)); err != nil || n != len(prefix) {
		t.Fatalf("boundary prefix n=%d want=%d err=%v", n, len(prefix), err)
	}
	if len(w.buf) != len(prefix) || cap(w.buf) > streamLedgerMaxPartialFrame {
		t.Fatalf("partial buffer len=%d cap=%d", len(w.buf), cap(w.buf))
	}
	if n, err := w.Write([]byte("\n\n")); err != nil || n != 2 {
		t.Fatalf("boundary delimiter n=%d err=%v", n, err)
	}
	if len(w.buf) != 0 || cap(w.buf) > streamLedgerMaxPartialFrame {
		t.Fatalf("flushed buffer len=%d cap=%d", len(w.buf), cap(w.buf))
	}
	if recorder.Body.Len() != streamLedgerMaxPartialFrame {
		t.Fatalf("body_len=%d want=%d", recorder.Body.Len(), streamLedgerMaxPartialFrame)
	}
}

func TestTerminalCommitWriterPreservesOversizedCompleteFrame(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return nil })
	prefix := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\""
	suffix := "\"}\n\n"
	frame := prefix + strings.Repeat("x", streamLedgerMaxPartialFrame) + suffix
	if n, err := w.Write([]byte(frame)); n != len(frame) || err != nil {
		t.Fatalf("oversized complete frame n=%d want=%d err=%v", n, len(frame), err)
	}
	if len(w.buf) != 0 || cap(w.buf) > streamLedgerMaxPartialFrame {
		t.Fatalf("oversized complete frame was retained: len=%d cap=%d", len(w.buf), cap(w.buf))
	}
	if got := recorder.Body.String(); got != frame {
		t.Fatalf("oversized complete frame changed: got=%d want=%d", len(got), len(frame))
	}
}

func TestTerminalCommitWriterPreservesOversizedFrameAfterSplitBoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return nil })
	first := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n"
	if _, err := w.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	prefix := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\""
	suffix := "\"}\n\n"
	largeFrame := prefix + strings.Repeat("x", streamLedgerMaxPartialFrame) + suffix
	second := "\n" + largeFrame
	if n, err := w.Write([]byte(second)); n != len(second) || err != nil {
		t.Fatalf("split boundary n=%d want=%d err=%v", n, len(second), err)
	}
	if got, want := recorder.Body.String(), first+"\n"+largeFrame; got != want {
		t.Fatalf("relayed bytes=%d want=%d", len(got), len(want))
	}
	if len(w.buf) != 0 || cap(w.buf) > streamLedgerMaxPartialFrame {
		t.Fatalf("large frame retained: len=%d cap=%d", len(w.buf), cap(w.buf))
	}
}

func TestTerminalCommitWriterRejectsOversizedPartialWithoutCommit(t *testing.T) {
	recorder := httptest.NewRecorder()
	commits := 0
	w := newTerminalCommitWriter(recorder, func() error {
		commits++
		return nil
	})
	partial := strings.Repeat("x", streamLedgerMaxPartialFrame)
	if n, err := w.Write([]byte(partial)); err != nil || n != len(partial) {
		t.Fatalf("limit write n=%d want=%d err=%v", n, len(partial), err)
	}
	if n, err := w.Write([]byte("x")); n != 0 || !errors.Is(err, errTerminalCommitFrameTooLarge) {
		t.Fatalf("overflow n=%d err=%v", n, err)
	}
	completed := []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	if n, err := w.Write(completed); n != 0 || !errors.Is(err, errTerminalCommitFrameTooLarge) {
		t.Fatalf("write after overflow n=%d err=%v", n, err)
	}
	if err := w.Close(); !errors.Is(err, errTerminalCommitFrameTooLarge) {
		t.Fatalf("close err=%v", err)
	}
	if commits != 0 || recorder.Body.Len() != 0 {
		t.Fatalf("overflow produced success: commits=%d body=%q", commits, recorder.Body.String())
	}
}

func TestTerminalCommitWriterRejectsOversizedPartialInSingleWrite(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error {
		t.Fatal("oversized partial frame committed")
		return nil
	})
	partial := []byte(strings.Repeat("x", streamLedgerMaxPartialFrame+1))
	if n, err := w.Write(partial); n != 0 || !errors.Is(err, errTerminalCommitFrameTooLarge) {
		t.Fatalf("overflow n=%d err=%v", n, err)
	}
	if len(w.buf) != 0 || cap(w.buf) != 0 || recorder.Body.Len() != 0 {
		t.Fatalf("overflow retained data: len=%d cap=%d body=%d", len(w.buf), cap(w.buf), recorder.Body.Len())
	}
}

func TestTerminalCommitWriterDefersDoneUntilTerminal(t *testing.T) {
	recorder := httptest.NewRecorder()
	commits := 0
	w := newTerminalCommitWriter(recorder, func() error {
		commits++
		return nil
	})
	done := "data: [DONE]\n\n"
	if _, err := w.Write([]byte(done)); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("DONE escaped before terminal: %q", recorder.Body.String())
	}
	completed := "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	if _, err := w.Write([]byte(completed)); err != nil {
		t.Fatal(err)
	}
	if got, want := recorder.Body.String(), completed+done; got != want || commits != 1 {
		t.Fatalf("commits=%d body=%q want=%q", commits, got, want)
	}
}

func TestTerminalCommitWriterDeferredDoneByteLimit(t *testing.T) {
	suffix := "\ndata: [DONE]\n\n"
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "boundary", size: streamLedgerMaxPartialFrame},
		{name: "over_limit", size: streamLedgerMaxPartialFrame + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			w := newTerminalCommitWriter(recorder, func() error { return nil })
			frame := strings.Repeat(":", test.size-len(suffix)) + suffix
			n, err := w.Write([]byte(frame))
			if test.wantErr {
				if n != 0 || !errors.Is(err, errTerminalCommitDeferredDoneTooLarge) {
					t.Fatalf("overflow n=%d err=%v", n, err)
				}
				if closeErr := w.Close(); !errors.Is(closeErr, errTerminalCommitDeferredDoneTooLarge) {
					t.Fatalf("close err=%v", closeErr)
				}
				return
			}
			if err != nil || n != len(frame) {
				t.Fatalf("boundary n=%d want=%d err=%v", n, len(frame), err)
			}
			if w.deferredBytes != streamLedgerMaxPartialFrame || len(w.deferred) != 1 || cap(w.deferred) > terminalCommitWriterMaxDeferredFrames {
				t.Fatalf("deferred frames=%d cap=%d bytes=%d", len(w.deferred), cap(w.deferred), w.deferredBytes)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("unterminated DONE escaped: %d bytes", recorder.Body.Len())
			}
		})
	}
}

func TestTerminalCommitWriterDeferredDoneFrameLimit(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := newTerminalCommitWriter(recorder, func() error { return nil })
	frame := []byte("data: [DONE]\n\n")
	for index := 0; index < terminalCommitWriterMaxDeferredFrames; index++ {
		if _, err := w.Write(frame); err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
	}
	if n, err := w.Write(frame); n != 0 || !errors.Is(err, errTerminalCommitDeferredDoneTooLarge) {
		t.Fatalf("overflow n=%d err=%v", n, err)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("deferred frames escaped: %q", recorder.Body.String())
	}
}
