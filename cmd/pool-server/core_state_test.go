package main

import (
	"path/filepath"
	"testing"

	"codex-account-pool/internal/corestate"
)

func TestCommitActiveWorkerMaintainsFallbackAndIdempotency(t *testing.T) {
	dir := t.TempDir()
	writer, err := corestate.OpenWriter(dir, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "worker-a.sock")
	second := filepath.Join(dir, "worker-b.sock")
	if err = commitActiveWorker(writer, "a", first, 11); err != nil {
		t.Fatal(err)
	}
	if err = commitActiveWorker(writer, "b", second, 12); err != nil {
		t.Fatal(err)
	}
	if err = commitActiveWorker(writer, "b", second, 12); err != nil {
		t.Fatal(err)
	}
	reader, err := corestate.NewReader(dir, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	state, err := reader.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Generation != 2 || state.ActiveWorker != second || state.PreviousWorker != first || state.ReleaseID != "b" || state.FencingToken != 12 {
		t.Fatalf("state = %+v", state)
	}
}
