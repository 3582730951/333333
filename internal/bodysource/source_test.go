package bodysource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestCaptureReplayMemoryAndRelease(t *testing.T) {
	budget := NewBudget(1<<20, 1<<20)
	source, err := Capture(context.Background(), strings.NewReader(strings.Repeat("abcd", 100_000)), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("abcd", 100_000)
	for i := 0; i < 2; i++ {
		got, err := ReadAll(source)
		if err != nil || string(got) != want {
			t.Fatalf("replay %d mismatch: len=%d err=%v", i, len(got), err)
		}
	}
	if got := budget.Snapshot().MemoryUsed; got != int64(len(want)) {
		t.Fatalf("memory reservation=%d", got)
	}
	if snap := budget.Snapshot(); snap.Captures != 1 || snap.CapturedBytes != int64(len(want)) || snap.MemoryBytes != int64(len(want)) || snap.ReplayOpens != 2 || snap.ActiveSources != 1 {
		t.Fatalf("memory capture metrics=%+v", snap)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if snap := budget.Snapshot(); snap.MemoryUsed != 0 || snap.ActiveSources != 0 {
		t.Fatalf("memory metrics after close=%+v", snap)
	}
	if _, err := source.Open(); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close err=%v", err)
	}
}

func TestCaptureExpectedBytesReplaysSmallBody(t *testing.T) {
	const payload = "small payload"
	budget := NewBudget(1<<20, 0)
	source, err := Capture(context.Background(), strings.NewReader(payload), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, ExpectedBytes: int64(len(payload)), Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if source.Size() != int64(len(payload)) {
		t.Fatalf("size=%d", source.Size())
	}
	view, ok := ByteView(source)
	if !ok || string(view) != payload {
		t.Fatalf("byte view=%q ok=%v", view, ok)
	}
	for i := 0; i < 2; i++ {
		got, readErr := ReadAll(source)
		if readErr != nil || string(got) != payload {
			t.Fatalf("replay %d got=%q err=%v", i, got, readErr)
		}
	}
}

func TestCaptureExpectedBytesShortReadsRetainOneContiguousAllocation(t *testing.T) {
	payload := bytes.Repeat([]byte("bounded-short-read-"), 8192)
	budget := NewBudget(int64(len(payload)), 1<<20)
	source, err := Capture(context.Background(), &shortReader{reader: bytes.NewReader(payload), max: 17}, CaptureOptions{
		MaxBytes: int64(len(payload)), MemoryThreshold: int64(len(payload)), ExpectedBytes: int64(len(payload)), ChunkSize: 64 << 10, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	view, ok := ByteView(source)
	if !ok || !bytes.Equal(view, payload) || cap(view) != len(payload) {
		t.Fatalf("contiguous view len=%d cap=%d ok=%v", len(view), cap(view), ok)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != int64(len(payload)) || snapshot.SpoolUsed != 0 {
		t.Fatalf("unexpected budget after contiguous capture: %+v", snapshot)
	}
}

func TestCaptureExpectedBytesWholeReservationFailureSpoolsDirectly(t *testing.T) {
	payload := bytes.Repeat([]byte("spool-short-read-"), 8192)
	dir := t.TempDir()
	budget := NewBudget(int64(len(payload))/2, int64(len(payload))*2)
	source, err := Capture(context.Background(), &shortReader{reader: bytes.NewReader(payload), max: 31}, CaptureOptions{
		MaxBytes: int64(len(payload)), MemoryThreshold: int64(len(payload)), ExpectedBytes: int64(len(payload)), TempDir: dir, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	view, ok := ByteView(source)
	if !ok || !bytes.Equal(view, payload) {
		t.Fatalf("spooled byte view len=%d ok=%v", len(view), ok)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != int64(len(payload)) || snapshot.MemoryFallbacks != 1 {
		t.Fatalf("unexpected direct-spool budget: %+v", snapshot)
	}
}

func TestCaptureExpectedBytesRejectsLengthMismatchAndReleasesBudget(t *testing.T) {
	for name, test := range map[string]struct {
		payload  string
		expected int64
	}{
		"short": {"abc", 4},
		"long":  {"abcde", 4},
	} {
		t.Run(name, func(t *testing.T) {
			budget := NewBudget(1<<20, 1<<20)
			if _, err := Capture(context.Background(), strings.NewReader(test.payload), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 1 << 20, ExpectedBytes: test.expected, Budget: budget}); err == nil {
				t.Fatal("expected content-length mismatch")
			}
			if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 || snapshot.ActiveSources != 0 {
				t.Fatalf("mismatch leaked budget: %+v", snapshot)
			}
		})
	}
}

func TestCaptureSpoolReplayCleanupAndBudget(t *testing.T) {
	dir := t.TempDir()
	budget := NewBudget(64, 1<<20)
	source, err := Capture(context.Background(), strings.NewReader(strings.Repeat("z", 256<<10)), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 128 << 10, TempDir: dir, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("spool files=%d err=%v", len(entries), err)
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "plan9" && runtime.GOOS != "js" && runtime.GOOS != "wasip1" {
		if view, ok := ByteView(source); !ok || len(view) != 256<<10 || !bytes.Equal(view, bytes.Repeat([]byte("z"), 256<<10)) {
			t.Fatalf("read-only spool view len=%d ok=%v", len(view), ok)
		}
	}
	for i := 0; i < 2; i++ {
		got, err := ReadAll(source)
		if err != nil || len(got) != 256<<10 || !bytes.Equal(got, bytes.Repeat([]byte("z"), 256<<10)) {
			t.Fatalf("replay %d len=%d err=%v", i, len(got), err)
		}
	}
	if got := budget.Snapshot().SpoolUsed; got != 256<<10 {
		t.Fatalf("spool reservation=%d", got)
	}
	if snap := budget.Snapshot(); snap.Captures != 1 || snap.SpooledBytes != 256<<10 || snap.SpillCount != 1 || snap.MemoryFallbacks == 0 || snap.ReplayOpens != 2 || snap.ActiveSources != 1 {
		t.Fatalf("spool capture metrics=%+v", snap)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 0 || budget.Snapshot().SpoolUsed != 0 {
		t.Fatalf("cleanup files=%d spool=%d", len(entries), budget.Snapshot().SpoolUsed)
	}
}

func TestCaptureSpoolRejectionMetricAndCleanup(t *testing.T) {
	dir := t.TempDir()
	budget := NewBudget(1, 32)
	_, err := Capture(context.Background(), strings.NewReader(strings.Repeat("x", 128)), CaptureOptions{MaxBytes: 128, MemoryThreshold: 128, ChunkSize: 64, TempDir: dir, Budget: budget})
	if !errors.Is(err, ErrSpoolBudget) {
		t.Fatalf("spool limit error=%v", err)
	}
	if snap := budget.Snapshot(); snap.MemoryUsed != 0 || snap.SpoolUsed != 0 || snap.ActiveSources != 0 || snap.MemoryFallbacks == 0 || snap.SpoolRejections == 0 {
		t.Fatalf("spool rejection metrics=%+v", snap)
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("spool rejection leaked files=%v err=%v", entries, readErr)
	}
}

func TestCaptureLimitsAndCancellationRelease(t *testing.T) {
	budget := NewBudget(1<<20, 1<<20)
	if _, err := Capture(context.Background(), strings.NewReader(strings.Repeat("x", 129)), CaptureOptions{MaxBytes: 128, MemoryThreshold: 64, TempDir: t.TempDir(), Budget: budget}); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("limit err=%v", err)
	}
	if snap := budget.Snapshot(); snap.MemoryUsed != 0 || snap.SpoolUsed != 0 {
		t.Fatalf("leaked budget after limit: %+v", snap)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, strings.NewReader("payload"), CaptureOptions{MaxBytes: 128, MemoryThreshold: 128, Budget: budget}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err=%v", err)
	}
}

func TestSliceAndPatchedReplay(t *testing.T) {
	slice, err := Slice(Bytes([]byte("0123456789")), 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadAll(slice)
	if err != nil || string(got) != "23456" {
		t.Fatalf("slice=%q err=%v", got, err)
	}
	patched, err := Patched(Bytes([]byte("0123456789")), []Patch{{Offset: 2, Delete: 3, Insert: Bytes([]byte("abc"))}, {Offset: 8, Delete: 0, Insert: Bytes([]byte("XY"))}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		got, err = ReadAll(patched)
		if err != nil || string(got) != "01abc567XY89" {
			t.Fatalf("patch replay %d=%q err=%v", i, got, err)
		}
	}
	if patched.Size() != int64(len("01abc567XY89")) {
		t.Fatalf("patch size=%d", patched.Size())
	}
	if err := patched.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPatchTopLevelReplacesDeletesAndInserts(t *testing.T) {
	large := strings.Repeat("context-", 128<<10)
	body := []byte(` { "thread_id" : "old", "input":"` + large + `", "store" : true, "turn_state":"drop" } `)
	base := Bytes(body)
	meta, err := ScanJSON(context.Background(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := PatchTopLevel(base, meta, []JSONFieldPatch{
		{Name: "thread_id", Delete: true},
		{Name: "store", Value: []byte("false")},
		{Name: "turn_state", Delete: true},
		{Name: "parallel_tool_calls", Value: []byte("false")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer patched.Close()
	for i := 0; i < 2; i++ {
		got, err := ReadAll(patched)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		var root map[string]any
		if err := json.Unmarshal(got, &root); err != nil {
			t.Fatalf("patched JSON: %v: %s", err, got)
		}
		if _, ok := root["thread_id"]; ok {
			t.Fatalf("thread_id was not deleted")
		}
		if _, ok := root["turn_state"]; ok {
			t.Fatalf("turn_state was not deleted")
		}
		if root["store"] != false || root["parallel_tool_calls"] != false || root["input"] != large {
			t.Fatalf("patched fields mismatch")
		}
	}
}

func TestPatchTopLevelDeleteAllThenInsert(t *testing.T) {
	body := []byte(`{"thread_id":"a","turn_state":"b"}`)
	base := Bytes(body)
	meta, err := ScanJSON(context.Background(), base, nil)
	if err != nil {
		t.Fatal(err)
	}
	patched, err := PatchTopLevel(base, meta, []JSONFieldPatch{{Name: "thread_id", Delete: true}, {Name: "turn_state", Delete: true}, {Name: "store", Value: []byte("false")}})
	if err != nil {
		t.Fatal(err)
	}
	defer patched.Close()
	got, err := ReadAll(patched)
	if err != nil || string(got) != `{"store":false}` {
		t.Fatalf("patched=%q err=%v", got, err)
	}
}

func TestSpoolBufferSpillsReplaysAndReleasesBudget(t *testing.T) {
	dir := t.TempDir()
	budget := NewBudget(32, 1<<20)
	buffer, err := NewSpoolBuffer(context.Background(), CaptureOptions{MaxBytes: 1 << 20, MemoryThreshold: 32, TempDir: dir, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("replay-buffer-", 4096))
	for offset := 0; offset < len(payload); {
		end := offset + 257
		if end > len(payload) {
			end = len(payload)
		}
		if _, err = buffer.Write(payload[offset:end]); err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	if !buffer.Spilled() || buffer.Size() != int64(len(payload)) {
		t.Fatalf("spilled=%v size=%d", buffer.Spilled(), buffer.Size())
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "plan9" && runtime.GOOS != "js" && runtime.GOOS != "wasip1" {
		if view, ok := ByteView(buffer); !ok || !bytes.Equal(view, payload) {
			t.Fatalf("spool buffer view len=%d ok=%v", len(view), ok)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		got, readErr := ReadAll(buffer)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("replay %d changed payload", attempt)
		}
	}
	if _, err = buffer.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after open error=%v", err)
	}
	if err = buffer.Close(); err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.MemoryUsed != 0 || snapshot.SpoolUsed != 0 {
		t.Fatalf("budget leaked after close: %+v", snapshot)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool files remain: entries=%v err=%v", entries, err)
	}
}

func TestCapturePropagatesReaderFailure(t *testing.T) {
	boom := errors.New("boom")
	budget := NewBudget(1<<20, 1<<20)
	_, err := Capture(context.Background(), io.MultiReader(strings.NewReader("prefix"), errorReader{err: boom}), CaptureOptions{MaxBytes: 128, MemoryThreshold: 128, Budget: budget})
	if !errors.Is(err, boom) || budget.Snapshot().MemoryUsed != 0 {
		t.Fatalf("err=%v snapshot=%+v", err, budget.Snapshot())
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

type shortReader struct {
	reader io.Reader
	max    int
}

func (r *shortReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		p = p[:r.max]
	}
	return r.reader.Read(p)
}
