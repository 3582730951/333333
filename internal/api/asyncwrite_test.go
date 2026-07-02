package api

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncWriterRecoversPanicAndContinues(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	defer s.FlushWrites()

	done := make(chan struct{})
	s.enqueueWrite(func() {
		panic("bad async write")
	})
	s.enqueueWrite(func() {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("async writer did not continue after a panicking write")
	}
}

func TestAsyncWriterRecoversInlineAfterFlush(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	s.FlushWrites()

	mustNotPanic(t, func() {
		s.enqueueWrite(func() {
			panic("inline after flush")
		})
	})
}

func TestAsyncWriterRecoversInlineBudgetOverflow(t *testing.T) {
	s := &Server{}
	s.startAsyncWriter()
	defer s.FlushWrites()

	mustNotPanic(t, func() {
		s.enqueueWriteSized(asyncWriteBudgetBytes+1, func() {
			panic("budget overflow")
		})
	})

	if got := atomic.LoadInt64(&s.asyncBytes); got != 0 {
		t.Fatalf("async byte budget leaked after overflow fallback: got %d", got)
	}
}

func TestAsyncWriterRecoversInlineQueueFull(t *testing.T) {
	s := &Server{
		asyncWrites: make(chan func(), 1),
	}
	s.asyncWrites <- func() {}
	defer close(s.asyncWrites)

	mustNotPanic(t, func() {
		s.enqueueWrite(func() {
			panic("inline queue full")
		})
	})
}

func mustNotPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("unexpected panic: %v", recovered)
		}
	}()
	fn()
}
