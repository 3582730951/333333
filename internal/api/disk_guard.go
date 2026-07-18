package api

import (
	"context"
	"log"
	"path/filepath"
	"syscall"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

type DiskGuardSnapshot struct {
	Level                   string  `json:"level"`
	FreePercent             float64 `json:"free_percent"`
	ForcedContextTTLSeconds int     `json:"forced_context_ttl_seconds"`
	ContextsDeleted         int64   `json:"contexts_deleted"`
	LogsDeleted             int64   `json:"logs_deleted"`
	LastRunAt               int64   `json:"last_run_at"`
	LastLogCleanupAt        int64   `json:"last_log_cleanup_at,omitempty"`
	LastError               string  `json:"last_error,omitempty"`
}

func (s *Server) diskGuardSnapshot() DiskGuardSnapshot {
	if v := s.diskGuard.Load(); v != nil {
		return v.(DiskGuardSnapshot)
	}
	return DiskGuardSnapshot{Level: "normal"}
}
func (s *Server) diskGuardTTL() int { return s.diskGuardSnapshot().ForcedContextTTLSeconds }

func (s *Server) startDiskGuard(ctx context.Context) {
	supervisor.Go(ctx, "disk-space-guard", func(ctx context.Context) {
		s.runDiskGuard(ctx)
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runDiskGuard(ctx)
			}
		}
	})
}

func (s *Server) runDiskGuard(ctx context.Context) {
	path := filepath.Dir(s.cfg.DatabasePath)
	if path == "." {
		path = ""
	}
	if path == "" {
		path = "."
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		s.diskGuard.Store(DiskGuardSnapshot{Level: "unknown", LastRunAt: storage.Now(), LastError: err.Error()})
		return
	}
	if fs.Blocks == 0 {
		s.diskGuard.Store(DiskGuardSnapshot{Level: "unknown", LastRunAt: storage.Now(), LastError: "filesystem reports zero blocks"})
		return
	}
	free := 100 * float64(fs.Bavail) / float64(fs.Blocks)
	previous := s.diskGuardSnapshot()
	snap := DiskGuardSnapshot{Level: "normal", FreePercent: float64(int(free*10)) / 10, LastRunAt: storage.Now(), ContextsDeleted: previous.ContextsDeleted, LogsDeleted: previous.LogsDeleted, LastLogCleanupAt: previous.LastLogCleanupAt}
	// 10% recovery hysteresis prevents TTL oscillation near the 8% boundary.
	if diskGuardLevel(free, previous.Level) == "critical" {
		snap.Level = "critical"
		snap.ForcedContextTTLSeconds = 900
		deleted, err := s.store.ClearContextJournal(ctx)
		snap.ContextsDeleted += deleted
		if err != nil {
			snap.LastError = err.Error()
		}
	} else if diskGuardLevel(free, previous.Level) == "pressure" {
		snap.Level = "pressure"
		snap.ForcedContextTTLSeconds = 900
		deleted, err := s.store.CleanupContextJournalCreatedBefore(ctx, storage.Now()-900)
		snap.ContextsDeleted += deleted
		if err != nil {
			snap.LastError = err.Error()
		}
		if storage.Now()-previous.LastLogCleanupAt >= 300 {
			logs, err := s.store.PurgeLogRecordsBefore(ctx, storage.Now()-86400, 256)
			snap.LogsDeleted += logs.Total()
			snap.LastLogCleanupAt = storage.Now()
			if err != nil && snap.LastError == "" {
				snap.LastError = err.Error()
			}
		}
	}
	if snap.Level != "normal" {
		if err := s.store.CheckpointLogStorage(ctx); err != nil && snap.LastError == "" {
			snap.LastError = err.Error()
		}
	}
	// Enforce the journal size/row budget on every tick, independent of free-disk level:
	// this is the hard bound that keeps a low-config VPS from growing the replay journal
	// without limit. Lowest-expires_at (least-recently-resumed) rows are evicted first.
	// The budget is tightened under disk pressure so the journal sheds faster, trading
	// resume window for disk as the tier escalates.
	maxRows := int64(s.settingInt(ctx, "context_journal_max_rows", s.cfg.ContextJournalMaxRows))
	maxBytes := int64(s.settingInt(ctx, "context_journal_max_mb", s.cfg.ContextJournalMaxMB)) * 1024 * 1024
	switch snap.Level {
	case "pressure":
		maxRows, maxBytes = maxRows/2, maxBytes/2
	case "critical":
		maxRows, maxBytes = maxRows/10, maxBytes/10
	}
	if evicted, err := s.store.EvictContextJournalToBudget(ctx, maxRows, maxBytes); err != nil {
		if snap.LastError == "" {
			snap.LastError = err.Error()
		}
	} else {
		snap.ContextsDeleted += evicted
	}
	if snap.Level != previous.Level || snap.ContextsDeleted != previous.ContextsDeleted {
		log.Printf("[DISK-GUARD] level=%s free=%.1f%% ttl=%d contexts_deleted=%d logs_deleted=%d err=%s", snap.Level, snap.FreePercent, snap.ForcedContextTTLSeconds, snap.ContextsDeleted, snap.LogsDeleted, snap.LastError)
	}
	s.diskGuard.Store(snap)
}

func diskGuardLevel(free float64, previous string) string {
	if free < 5 {
		return "critical"
	}
	if free < 8 || (previous != "normal" && free < 10) {
		return "pressure"
	}
	return "normal"
}
