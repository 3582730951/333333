package storage

import (
	"strings"
	"testing"
)

// The chunk pipeline's CPU cost is what used to run inside the write transaction.
// Measuring prepareGoalChunks alone quantifies how much work moved out of the
// writer-lock window, which is what sizes the async threshold for large payloads.
func benchPayload(mib int) string {
	unit := strings.Repeat("goal-continuity-chunk-payload-0123456789 ", 512) // ~20 KiB
	var b strings.Builder
	target := mib << 20
	b.Grow(target + len(unit))
	for b.Len() < target {
		b.WriteString(unit)
	}
	return b.String()
}

// A production store has a token master key, so sealToken really encrypts. Without
// one it returns its input unchanged and the benchmark would omit the encryption cost
// entirely, understating the work that moved out of the writer-lock window.
func benchStore(t testing.TB) *Store {
	t.Helper()
	store := newTestStore(t)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7)
	}
	if err := store.SetTokenMasterKey(key); err != nil {
		t.Fatalf("set master key: %v", err)
	}
	return store
}

func BenchmarkPrepareGoalChunks1MiB(b *testing.B) {
	store := benchStore(b)
	payload := benchPayload(1)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, stored := store.prepareGoalChunks(payload)
		if len(chunks) == 0 || stored <= 0 {
			b.Fatal("no chunks produced")
		}
	}
}

func BenchmarkPrepareGoalChunks16MiB(b *testing.B) {
	store := benchStore(b)
	payload := benchPayload(16)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		chunks, stored := store.prepareGoalChunks(payload)
		if len(chunks) == 0 || stored <= 0 {
			b.Fatal("no chunks produced")
		}
	}
}

// estimateGoalChunkStorage is the second compression pass that the prepared-chunk
// refactor removed. Comparing it against prepareGoalChunks shows the duplicated cost.
func BenchmarkEstimateGoalChunkStorage16MiB(b *testing.B) {
	store := benchStore(b)
	payload := benchPayload(16)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if store.estimateGoalChunkStorage(payload) <= 0 {
			b.Fatal("no bytes estimated")
		}
	}
}
