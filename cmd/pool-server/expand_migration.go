package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// runExpandOnlyMigration is the installer-facing pre-cutover phase. It never
// opens HTTP listeners or starts background workers; the old generation remains
// active while compatible tables, columns and indexes are made available.
func runExpandOnlyMigration(configPath string) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Printf("expand migration load config: %v", err)
		return 1
	}
	store, err := storage.OpenWithConfig(cfg)
	if err != nil {
		log.Printf("expand migration open storage: %v", err)
		return 1
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	started := time.Now()
	err = initStorageWithLockRetry(ctx, store.InitExpandOnlyWithProgress, func(phase string) {
		log.Printf("expand migration phase=%s elapsed=%s", phase, time.Since(started).Round(time.Millisecond))
	}, log.Printf)
	if err != nil {
		log.Printf("expand migration failed: %v", err)
		return 1
	}
	fmt.Printf("expand-only migration complete in %s\n", time.Since(started).Round(time.Millisecond))
	return 0
}
