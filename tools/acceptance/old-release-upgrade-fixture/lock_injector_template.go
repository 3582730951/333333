//go:build acceptance_old_release_injector

package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// This file is copied into the pinned old release only by
// old_release_live_upgrade.sh. It makes the old worker itself own a deterministic
// SQLite write transaction after the test arms it, reproducing the production
// lock without killing or modifying any unrelated process.
func init() {
	armFile := strings.TrimSpace(os.Getenv("CODEX_POOL_ACCEPTANCE_LOCK_ARM_FILE"))
	readyFile := strings.TrimSpace(os.Getenv("CODEX_POOL_ACCEPTANCE_LOCK_READY_FILE"))
	if armFile == "" || readyFile == "" {
		return
	}
	go acceptanceHoldOldGenerationWriteLock(armFile, readyFile)
}

func acceptanceHoldOldGenerationWriteLock(armFile, readyFile string) {
	for {
		if _, err := os.Stat(armFile); err == nil {
			break
		} else if !os.IsNotExist(err) {
			log.Printf("[ACCEPTANCE-LOCK] arm stat failed: %v", err)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	databasePath := strings.TrimSpace(os.Getenv("CODEX_POOL_DATABASE"))
	if databasePath == "" {
		log.Printf("[ACCEPTANCE-LOCK] CODEX_POOL_DATABASE is empty")
		return
	}
	separator := "?"
	if strings.Contains(databasePath, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite3", databasePath+separator+"_busy_timeout=100&_foreign_keys=on")
	if err != nil {
		log.Printf("[ACCEPTANCE-LOCK] open failed: %v", err)
		return
	}
	db.SetMaxOpenConns(1)
	for {
		tx, beginErr := db.Begin()
		if beginErr == nil {
			_, beginErr = tx.Exec(`INSERT INTO settings(key,value,updated_at)
VALUES('acceptance_old_generation_lock','held',?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, time.Now().Unix())
			if beginErr == nil {
				if err := os.MkdirAll(filepath.Dir(readyFile), 0o700); err == nil {
					err = os.WriteFile(readyFile, []byte(fmt.Sprintf("pid=%d\n", os.Getpid())), 0o600)
				}
				if err != nil {
					_ = tx.Rollback()
					log.Printf("[ACCEPTANCE-LOCK] ready marker failed: %v", err)
					return
				}
				log.Printf("[ACCEPTANCE-LOCK] held by old worker pid=%d", os.Getpid())
				signals := make(chan os.Signal, 1)
				signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
				<-signals
				signal.Stop(signals)
				_ = tx.Rollback()
				log.Printf("[ACCEPTANCE-LOCK] released during old-worker shutdown")
				return
			}
			_ = tx.Rollback()
		}
		time.Sleep(50 * time.Millisecond)
	}
}
