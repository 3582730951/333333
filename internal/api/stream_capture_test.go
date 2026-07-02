package api

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCaptureSSELimitExceededWhenDiskSpillDisabled(t *testing.T) {
	_, retryable, err := captureSSEForFailoverWithOptions(
		strings.NewReader("event: content_block_delta\ndata: "+strings.Repeat("x", 128)+"\n\n"),
		func([]byte) bool { return false },
		sseCaptureOptions{MemoryLimit: 32, DiskLimit: 0, TempDir: t.TempDir()},
	)
	if !errors.Is(err, errSSECaptureLimitExceeded) {
		t.Fatalf("err = %v, want errSSECaptureLimitExceeded", err)
	}
	if retryable {
		t.Fatal("oversized success stream should not be classified as upstream retryable error")
	}
}

func TestCaptureSSESpillUsesConfiguredDiskLimitAndCleansTempFile(t *testing.T) {
	dir := t.TempDir()
	captured, retryable, err := captureSSEForFailoverWithOptions(
		strings.NewReader("event: content_block_delta\ndata: "+strings.Repeat("x", 48)+"\n\n"),
		func([]byte) bool { return false },
		sseCaptureOptions{MemoryLimit: 16, DiskLimit: 128, TempDir: dir},
	)
	if err != nil {
		t.Fatal(err)
	}
	if retryable {
		t.Fatal("ordinary stream should not be retryable")
	}
	if captured == nil || captured.file == nil {
		t.Fatalf("expected spill file, captured=%+v", captured)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp files before close = %d, want 1", len(entries))
	}
	captured.Close()
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp files after close = %d, want 0", len(entries))
	}
}

func TestCaptureSSESpillStopsAtConfiguredDiskLimit(t *testing.T) {
	_, _, err := captureSSEForFailoverWithOptions(
		strings.NewReader("event: content_block_delta\ndata: "+strings.Repeat("x", 256)+"\n\n"),
		func([]byte) bool { return false },
		sseCaptureOptions{MemoryLimit: 16, DiskLimit: 32, TempDir: t.TempDir()},
	)
	if !errors.Is(err, errSSECaptureLimitExceeded) {
		t.Fatalf("err = %v, want errSSECaptureLimitExceeded", err)
	}
}
