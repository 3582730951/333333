package asynclog

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCloseReturnsAndFlushes(t *testing.T) {
	var out bytes.Buffer
	logger := New(Config{
		BufferSize:  8,
		FlushPeriod: time.Hour,
		Output:      &out,
	})
	logger.Printf("hello %s", "world")

	done := make(chan error, 1)
	go func() { done <- logger.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked")
	}
	if !strings.Contains(out.String(), "hello world") {
		t.Fatalf("log was not flushed: %q", out.String())
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	logger := New(Config{FlushPeriod: time.Hour})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrintfAfterCloseIsNoop(t *testing.T) {
	var out bytes.Buffer
	logger := New(Config{FlushPeriod: time.Hour, Output: &out})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	logger.Printf("after close")
	if strings.Contains(out.String(), "after close") {
		t.Fatalf("write after close was flushed: %q", out.String())
	}
}

func TestCloseUnblocksBlockingPrintfWhenBufferFull(t *testing.T) {
	flusherDone := make(chan struct{})
	close(flusherDone)
	logger := &Logger{
		cfg: Config{
			BufferSize:     1,
			FlushPeriod:    time.Hour,
			DropOnOverflow: false,
			Output:         &bytes.Buffer{},
		},
		entries:     make(chan logEntry, 1),
		flusherDone: flusherDone,
		stop:        make(chan struct{}),
	}
	logger.entries <- logEntry{msg: "full", ts: time.Now().UnixNano()}

	printfDone := make(chan struct{})
	go func() {
		logger.Printf("blocked")
		close(printfDone)
	}()

	select {
	case <-printfDone:
		t.Fatal("Printf returned before Close despite a full buffer and blocking overflow mode")
	case <-time.After(25 * time.Millisecond):
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- logger.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked while a Printf was waiting for buffer space")
	}

	select {
	case <-printfDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock the waiting Printf")
	}
}

func TestCloseWaitsForInFlightSenderBeforeDrain(t *testing.T) {
	logger := New(Config{
		BufferSize:  4,
		FlushPeriod: time.Hour,
		Output:      &bytes.Buffer{},
	})
	logger.sendWG.Add(1)

	closeDone := make(chan error, 1)
	go func() { closeDone <- logger.Close() }()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("Close returned before the in-flight sender finished")
	case <-time.After(25 * time.Millisecond):
	}

	logger.sendWG.Done()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not return after the in-flight sender finished")
	}
}

func TestDropOnOverflowKeepsQueueBounded(t *testing.T) {
	logger := New(Config{
		BufferSize:     1,
		FlushPeriod:    time.Hour,
		DropOnOverflow: true,
		Output:         &bytes.Buffer{},
	})
	for i := 0; i < 1000; i++ {
		logger.Printf("line %d", i)
	}
	used, capacity := logger.Stats()
	if capacity != 1 {
		t.Fatalf("capacity = %d, want 1", capacity)
	}
	if used > capacity {
		t.Fatalf("used = %d exceeds capacity %d", used, capacity)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterPanicDoesNotBlockClose(t *testing.T) {
	logger := New(Config{
		BufferSize:  4,
		FlushPeriod: time.Hour,
		Output:      panicWriter{},
	})
	logger.Printf("boom")

	done := make(chan error, 1)
	go func() { done <- logger.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close blocked after writer panic")
	}
}

func TestFlusherGoroutineHasSupervisorBoundary(t *testing.T) {
	source, err := os.ReadFile("asynclog.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if strings.Contains(text, "go l.flusher()") {
		t.Fatal("async log flusher must not run as a bare goroutine")
	}
	if !strings.Contains(text, `supervisor.Recover("async-log-flusher")`) {
		t.Fatal("async log flusher goroutine must report panics through supervisor")
	}
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) {
	panic("writer failed")
}
