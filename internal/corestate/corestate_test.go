package corestate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testKey(seed byte) []byte { return bytes.Repeat([]byte{seed}, 32) }

func commitWorker(t *testing.T, writer *Writer, id, worker string) Snapshot {
	t.Helper()
	state, err := writer.Commit(id, func(state *Snapshot) error {
		if state.ActiveWorker != "" && state.ActiveWorker != worker {
			state.PreviousWorker = state.ActiveWorker
		}
		state.ActiveWorker = worker
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestWriterCreatesEncryptedABSnapshotsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenWriter(dir, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	firstWorker := filepath.Join(dir, "worker-a.sock")
	secondWorker := filepath.Join(dir, "worker-b.sock")
	first := commitWorker(t, writer, "activate-a", firstWorker)
	second := commitWorker(t, writer, "activate-b", secondWorker)
	again := commitWorker(t, writer, "activate-b", secondWorker)
	if first.Generation != 1 || second.Generation != 2 || again.Generation != second.Generation {
		t.Fatalf("generations first=%d second=%d again=%d", first.Generation, second.Generation, again.Generation)
	}
	reader, err := NewReader(dir, testKey(1))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveWorker != secondWorker || loaded.PreviousWorker != firstWorker {
		t.Fatalf("loaded route = %+v", loaded)
	}
	for _, name := range []string{snapshotAName, snapshotBName} {
		raw, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if bytes.Contains(raw, []byte(secondWorker)) || jsonLike(raw) {
			t.Fatalf("%s contains plaintext state", name)
		}
	}
}

func jsonLike(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func TestReaderFallsBackWhenNewestSnapshotIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenWriter(dir, testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	workerA := filepath.Join(dir, "worker-a.sock")
	workerB := filepath.Join(dir, "worker-b.sock")
	commitWorker(t, writer, "a", workerA)
	commitWorker(t, writer, "b", workerB)
	if err = os.WriteFile(filepath.Join(dir, snapshotBName), []byte("half-write"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, _ := NewReader(dir, testKey(2))
	loaded, err := reader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveWorker != workerA || loaded.Generation != 1 {
		t.Fatalf("fallback = %+v", loaded)
	}
}

func TestWrongKeyAndTamperingAreRejected(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenWriter(dir, testKey(3))
	if err != nil {
		t.Fatal(err)
	}
	commitWorker(t, writer, "a", filepath.Join(dir, "worker.sock"))
	wrong, _ := NewReader(dir, testKey(4))
	if _, err = wrong.Load(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("wrong key error = %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, snapshotAName))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err = os.WriteFile(filepath.Join(dir, snapshotAName), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, _ := NewReader(dir, testKey(3))
	if _, err = reader.Load(); !errors.Is(err, ErrNoSnapshot) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestWriterReplaysJournalAndRepairsPartialTail(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenWriter(dir, testKey(5))
	if err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(dir, "worker.sock")
	committed := commitWorker(t, writer, "a", worker)
	for _, name := range []string{snapshotAName, snapshotBName} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	journal := filepath.Join(dir, journalName)
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte{0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err = OpenWriter(dir, testKey(5)); err != nil {
		t.Fatal(err)
	}
	reader, _ := NewReader(dir, testKey(5))
	loaded, err := reader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != committed.Generation || loaded.ActiveWorker != worker {
		t.Fatalf("recovered = %+v", loaded)
	}
}
