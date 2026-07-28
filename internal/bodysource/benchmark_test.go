package bodysource

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func benchmarkPayload(size int) []byte {
	payload := make([]byte, size)
	var state uint64 = 0x9e3779b97f4a7c15
	for i := range payload {
		state ^= state << 7
		state ^= state >> 9
		state ^= state << 8
		payload[i] = byte(state)
	}
	return payload
}

func BenchmarkCaptureMemory1MiB(b *testing.B) {
	payload := benchmarkPayload(1 << 20)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		source, err := Capture(context.Background(), bytes.NewReader(payload), CaptureOptions{MaxBytes: 2 << 20, MemoryThreshold: 2 << 20, Budget: NewBudget(2<<20, 0)})
		if err != nil {
			b.Fatal(err)
		}
		if err = source.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReplayMemory1MiB(b *testing.B) {
	payload := benchmarkPayload(1 << 20)
	source, err := Capture(context.Background(), bytes.NewReader(payload), CaptureOptions{MaxBytes: 2 << 20, MemoryThreshold: 2 << 20, Budget: NewBudget(2<<20, 0)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = source.Close() })
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader, openErr := source.Open()
		if openErr != nil {
			b.Fatal(openErr)
		}
		if _, copyErr := io.Copy(io.Discard, reader); copyErr != nil {
			_ = reader.Close()
			b.Fatal(copyErr)
		}
		if closeErr := reader.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}

func BenchmarkReplaySpool1MiB(b *testing.B) {
	payload := benchmarkPayload(1 << 20)
	source, err := Capture(context.Background(), bytes.NewReader(payload), CaptureOptions{MaxBytes: 2 << 20, MemoryThreshold: 0, TempDir: b.TempDir(), Budget: NewBudget(0, 2<<20)})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = source.Close() })
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader, openErr := source.Open()
		if openErr != nil {
			b.Fatal(openErr)
		}
		if _, copyErr := io.Copy(io.Discard, reader); copyErr != nil {
			_ = reader.Close()
			b.Fatal(copyErr)
		}
		if closeErr := reader.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}

func BenchmarkPatchedReplay1MiB(b *testing.B) {
	payload := benchmarkPayload(1 << 20)
	source, err := Patched(Bytes(payload), []Patch{
		{Offset: 16, Delete: 8, Insert: Bytes([]byte("replacement"))},
		{Offset: 512 << 10, Delete: 16, Insert: Bytes([]byte("middle"))},
		{Offset: int64(len(payload) - 32), Insert: Bytes([]byte("tail"))},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = source.Close() })
	b.SetBytes(source.Size())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader, openErr := source.Open()
		if openErr != nil {
			b.Fatal(openErr)
		}
		if _, copyErr := io.Copy(io.Discard, reader); copyErr != nil {
			_ = reader.Close()
			b.Fatal(copyErr)
		}
		if closeErr := reader.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}
