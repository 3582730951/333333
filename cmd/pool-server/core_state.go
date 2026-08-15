package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"codex-account-pool/internal/corestate"
	"codex-account-pool/internal/datadir"
)

func openCoreStateWriter(layout datadir.Layout) (*corestate.Writer, error) {
	keyPath, explicit := coreStateKeyPath(layout)
	key, err := loadRuntimeKey(keyPath, explicit)
	if err != nil {
		return nil, fmt.Errorf("load core state key: %w", err)
	}
	writer, err := corestate.OpenWriter(layout.CoreState, key)
	if err != nil {
		return nil, fmt.Errorf("open core state writer: %w", err)
	}
	return writer, nil
}

func coreStateKeyPath(layout datadir.Layout) (string, bool) {
	if configured := strings.TrimSpace(os.Getenv("CODEX_POOL_CORE_STATE_KEY_FILE")); configured != "" {
		return configured, true
	}
	if credentialDirectory := strings.TrimSpace(os.Getenv("CREDENTIALS_DIRECTORY")); credentialDirectory != "" {
		return filepath.Join(credentialDirectory, "core-state.key"), true
	}
	return filepath.Join(layout.Keys, "core-state.key"), false
}

func commitActiveWorker(writer *corestate.Writer, releaseID, workerSocket string, fencingToken int64) error {
	if writer == nil || strings.TrimSpace(workerSocket) == "" {
		return nil
	}
	absolute, err := filepath.Abs(workerSocket)
	if err != nil {
		return fmt.Errorf("resolve active worker socket: %w", err)
	}
	absolute = filepath.Clean(absolute)
	updateID := fmt.Sprintf("activate:%s:%d", strings.TrimSpace(releaseID), fencingToken)
	_, err = writer.Commit(updateID, func(state *corestate.Snapshot) error {
		if state.ActiveWorker != "" && state.ActiveWorker != absolute {
			state.PreviousWorker = state.ActiveWorker
		}
		state.ActiveWorker = absolute
		state.ReleaseID = strings.TrimSpace(releaseID)
		state.FencingToken = fencingToken
		return nil
	})
	return err
}
