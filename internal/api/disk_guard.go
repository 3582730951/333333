package api

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	diskPressureFreeBytes  = uint64(2 << 30)
	diskCriticalFreeBytes  = uint64(512 << 20)
	diskEmergencyFreeBytes = uint64(128 << 20)
	diskRecoveryFreeBytes  = uint64(4 << 30)
)

type DiskFilesystemSnapshot struct {
	Roles       []string `json:"roles"`
	Level       string   `json:"level"`
	FreePercent float64  `json:"free_percent"`
	FreeBytes   uint64   `json:"free_bytes"`
}

type DiskGuardSnapshot struct {
	Level                   string                   `json:"level"`
	FreePercent             float64                  `json:"free_percent"`
	FreeBytes               uint64                   `json:"free_bytes"`
	Filesystems             []DiskFilesystemSnapshot `json:"filesystems,omitempty"`
	ForcedContextTTLSeconds int                      `json:"forced_context_ttl_seconds"`
	ContextsDeleted         int64                    `json:"contexts_deleted"`
	GoalsDeleted            int64                    `json:"goals_deleted"`
	CodexMappingsDeleted    int64                    `json:"codex_mappings_deleted"`
	LogsDeleted             int64                    `json:"logs_deleted"`
	LastRunAt               int64                    `json:"last_run_at"`
	LastLogCleanupAt        int64                    `json:"last_log_cleanup_at,omitempty"`
	DatabaseWritable        bool                     `json:"database_writable"`
	JournalWritable         bool                     `json:"journal_writable"`
	SpoolWritable           bool                     `json:"spool_writable"`
	BackgroundPaused        bool                     `json:"background_paused"`
	LargeRequestsPaused     bool                     `json:"large_requests_paused"`
	AdmissionBlocked        bool                     `json:"admission_blocked"`
	LastError               string                   `json:"last_error,omitempty"`
}

type diskGuardPath struct {
	role    string
	path    string
	managed bool
}

type diskFilesystemProbe struct {
	deviceID string
	role     string
	level    string
	freePct  float64
	freeByte uint64
}

func (s *Server) diskGuardSnapshot() DiskGuardSnapshot {
	if v := s.diskGuard.Load(); v != nil {
		return v.(DiskGuardSnapshot)
	}
	return DiskGuardSnapshot{Level: "normal", DatabaseWritable: true, JournalWritable: true, SpoolWritable: true}
}

// Disk pressure must never shorten the lifetime of a live replay chain. Retention
// cleanup below removes only rows whose normal expiry has already elapsed.
func (s *Server) diskGuardTTL() int { return 0 }

func (s *Server) diskGuardPausesBackground() bool {
	return s.diskGuardSnapshot().BackgroundPaused
}

// StorageAdmissionReady is consumed by the deployment handler. Registration
// readiness is intentionally separate and does not affect core relay readiness.
func (s *Server) StorageAdmissionReady() bool {
	return s != nil && !s.diskGuardSnapshot().AdmissionBlocked
}

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
	previous := s.diskGuardSnapshot()
	probes, probeCodes := s.probeDiskFilesystems(previous.Level)
	snap := DiskGuardSnapshot{
		Level:                "normal",
		LastRunAt:            storage.Now(),
		ContextsDeleted:      previous.ContextsDeleted,
		GoalsDeleted:         previous.GoalsDeleted,
		CodexMappingsDeleted: previous.CodexMappingsDeleted,
		LogsDeleted:          previous.LogsDeleted,
		LastLogCleanupAt:     previous.LastLogCleanupAt,
		DatabaseWritable:     s.databaseWritable(ctx),
		JournalWritable:      s.journalWritable(),
		SpoolWritable:        s.managedDirectoryWritable(s.cfg.BodySpoolDir),
	}
	snap.Filesystems, snap.Level, snap.FreePercent, snap.FreeBytes = summarizeFilesystemProbes(probes)
	if len(probeCodes) > 0 {
		snap.LastError = strings.Join(probeCodes, ",")
	}
	if snap.Level == "unknown" && previous.Level != "" {
		snap.Level = previous.Level
	}
	if snap.Level == "" {
		snap.Level = "normal"
	}
	snap.BackgroundPaused = diskLevelAtLeast(snap.Level, "pressure")
	snap.LargeRequestsPaused = diskLevelAtLeast(snap.Level, "critical")
	snap.AdmissionBlocked = snap.Level == "emergency" || !snap.SpoolWritable ||
		(!snap.DatabaseWritable && !snap.JournalWritable)
	if !snap.DatabaseWritable {
		snap.LastError = appendDiskGuardCode(snap.LastError, "database_unwritable")
	}
	if !snap.JournalWritable {
		snap.LastError = appendDiskGuardCode(snap.LastError, "journal_unwritable")
	}
	if !snap.SpoolWritable {
		snap.LastError = appendDiskGuardCode(snap.LastError, "spool_unwritable")
	}

	s.storageAdmissionBlocked.Store(snap.AdmissionBlocked)
	s.storageLargeRequestsPaused.Store(snap.LargeRequestsPaused)
	// At critical pressure, prefer the idempotent database transaction over growing
	// the journal. A failed direct write still falls back to the journal.
	s.usageDirectWrites.Store(snap.LargeRequestsPaused && snap.DatabaseWritable)

	if snap.DatabaseWritable {
		s.runSafeDiskCleanup(ctx, &snap)
	}
	if snap.Level != "normal" && snap.DatabaseWritable {
		checkpointCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := s.store.CheckpointLogStorage(checkpointCtx); err != nil {
			snap.LastError = appendDiskGuardCode(snap.LastError, "checkpoint_failed")
		}
		cancel()
	}
	if diskGuardChanged(previous, snap) {
		log.Printf("[DISK-GUARD] level=%s free_pct=%.1f free_bytes=%d db_writable=%t journal_writable=%t spool_writable=%t admission_blocked=%t cleanup_contexts=%d cleanup_goals=%d cleanup_mappings=%d error_code=%s",
			snap.Level, snap.FreePercent, snap.FreeBytes, snap.DatabaseWritable, snap.JournalWritable,
			snap.SpoolWritable, snap.AdmissionBlocked, snap.ContextsDeleted, snap.GoalsDeleted,
			snap.CodexMappingsDeleted, snap.LastError)
		s.recordDiskGuardEvent(ctx, previous, snap)
	}
	s.diskGuard.Store(snap)
}

func (s *Server) runSafeDiskCleanup(ctx context.Context, snap *DiskGuardSnapshot) {
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if deleted, err := s.store.CleanupContextJournal(cleanupCtx); err != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "context_cleanup_failed")
	} else {
		snap.ContextsDeleted += deleted
	}
	if goals, err := s.store.CleanupGoalContinuity(cleanupCtx); err != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "goal_cleanup_failed")
	} else {
		snap.GoalsDeleted += goals
	}
	if mappings, err := s.store.CleanupCodexSessionMappings(cleanupCtx); err != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "mapping_cleanup_failed")
	} else {
		snap.CodexMappingsDeleted += mappings
	}
	s.cleanupExpiredDiagnosticJobs(cleanupCtx)
}

func (s *Server) databaseWritable(ctx context.Context) bool {
	if s.store == nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.store.CheckWritable(checkCtx) == nil
}

func (s *Server) journalWritable() bool {
	if s.usageJournal == nil {
		return false
	}
	if !s.managedDirectoryWritable(s.cfg.UsageJournalDir) {
		return false
	}
	return s.usageJournal.Sync() == nil
}

func (s *Server) managedDirectoryWritable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	return datadir.RecoverDirectory(path) == nil
}

func (s *Server) probeDiskFilesystems(previousLevel string) ([]diskFilesystemProbe, []string) {
	paths := []diskGuardPath{
		{role: "data", path: s.cfg.DataDir, managed: true},
		{role: "spool", path: s.cfg.BodySpoolDir, managed: true},
		{role: "journal", path: s.cfg.UsageJournalDir, managed: s.usageJournal != nil},
		{role: "diagnostics", path: s.cfg.DiagnosticsDir, managed: true},
	}
	if s.store != nil && !s.store.InMemory() && strings.ToLower(strings.TrimSpace(s.cfg.StorageDriver)) != "postgres" {
		paths = append(paths, diskGuardPath{role: "database", path: filepath.Dir(s.store.Path())})
	}
	var probes []diskFilesystemProbe
	var codes []string
	for _, candidate := range paths {
		if strings.TrimSpace(candidate.path) == "" {
			continue
		}
		if candidate.managed {
			if err := datadir.RecoverDirectory(candidate.path); err != nil {
				codes = append(codes, candidate.role+"_filesystem_unavailable")
				continue
			}
		}
		probe, err := probeDiskFilesystem(candidate.role, candidate.path, previousLevel)
		if err != nil {
			codes = append(codes, candidate.role+"_filesystem_unavailable")
			continue
		}
		probes = append(probes, probe)
	}
	return probes, uniqueSortedStrings(codes)
}

func probeDiskFilesystem(role, path, previousLevel string) (diskFilesystemProbe, error) {
	info, err := os.Stat(path)
	if err != nil {
		return diskFilesystemProbe{}, err
	}
	var fs syscall.Statfs_t
	if err = syscall.Statfs(path, &fs); err != nil {
		return diskFilesystemProbe{}, err
	}
	if fs.Blocks == 0 || fs.Bsize <= 0 {
		return diskFilesystemProbe{}, syscall.EIO
	}
	freeBytes := uint64(fs.Bavail) * uint64(fs.Bsize)
	freePct := 100 * float64(fs.Bavail) / float64(fs.Blocks)
	deviceID := filepath.Clean(path)
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		deviceID = strconv.FormatUint(uint64(stat.Dev), 10)
	}
	return diskFilesystemProbe{
		deviceID: deviceID,
		role:     role,
		level:    diskGuardLevel(freePct, freeBytes, previousLevel),
		freePct:  float64(int(freePct*10)) / 10,
		freeByte: freeBytes,
	}, nil
}

func summarizeFilesystemProbes(probes []diskFilesystemProbe) ([]DiskFilesystemSnapshot, string, float64, uint64) {
	type aggregate struct {
		roles    []string
		level    string
		freePct  float64
		freeByte uint64
	}
	byDevice := make(map[string]*aggregate)
	for _, probe := range probes {
		item := byDevice[probe.deviceID]
		if item == nil {
			item = &aggregate{level: probe.level, freePct: probe.freePct, freeByte: probe.freeByte}
			byDevice[probe.deviceID] = item
		}
		item.roles = append(item.roles, probe.role)
		if diskLevelRank(probe.level) > diskLevelRank(item.level) {
			item.level = probe.level
		}
		if probe.freePct < item.freePct {
			item.freePct = probe.freePct
		}
		if probe.freeByte < item.freeByte {
			item.freeByte = probe.freeByte
		}
	}
	out := make([]DiskFilesystemSnapshot, 0, len(byDevice))
	worstLevel := "normal"
	minPct := float64(100)
	minBytes := ^uint64(0)
	for _, item := range byDevice {
		sort.Strings(item.roles)
		out = append(out, DiskFilesystemSnapshot{
			Roles: item.roles, Level: item.level, FreePercent: item.freePct, FreeBytes: item.freeByte,
		})
		if diskLevelRank(item.level) > diskLevelRank(worstLevel) {
			worstLevel = item.level
		}
		if item.freePct < minPct {
			minPct = item.freePct
		}
		if item.freeByte < minBytes {
			minBytes = item.freeByte
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].Roles, ",") < strings.Join(out[j].Roles, ",")
	})
	if len(out) == 0 {
		return nil, "unknown", 0, 0
	}
	return out, worstLevel, minPct, minBytes
}

func diskGuardLevel(freePercent float64, freeBytes uint64, previous string) string {
	switch {
	case freePercent < 2 || freeBytes < diskEmergencyFreeBytes:
		return "emergency"
	case freePercent < 5 || freeBytes < diskCriticalFreeBytes:
		return "critical"
	case freePercent < 10 || freeBytes < diskPressureFreeBytes:
		return "pressure"
	case previous != "" && previous != "normal" && (freePercent < 15 || freeBytes < diskRecoveryFreeBytes):
		return "pressure"
	default:
		return "normal"
	}
}

func bodySpoolMinimumFreeBytes(path string, configured int64) int64 {
	reserve := int64(diskEmergencyFreeBytes)
	if configured > reserve {
		reserve = configured
	}
	var stat syscall.Statfs_t
	if strings.TrimSpace(path) == "" || syscall.Statfs(path, &stat) != nil || stat.Blocks == 0 || stat.Bsize <= 0 {
		return reserve
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	twoPercent := int64(total / 50)
	if twoPercent > reserve {
		reserve = twoPercent
	}
	return reserve
}

func diskLevelRank(level string) int {
	switch level {
	case "pressure":
		return 1
	case "critical":
		return 2
	case "emergency":
		return 3
	case "unknown":
		return 4
	default:
		return 0
	}
}

func diskLevelAtLeast(level, minimum string) bool {
	return diskLevelRank(level) >= diskLevelRank(minimum)
}

func appendDiskGuardCode(existing, code string) string {
	if code == "" {
		return existing
	}
	if existing == "" {
		return code
	}
	for _, value := range strings.Split(existing, ",") {
		if value == code {
			return existing
		}
	}
	return existing + "," + code
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func diskGuardChanged(previous, current DiskGuardSnapshot) bool {
	return previous.Level != current.Level ||
		previous.DatabaseWritable != current.DatabaseWritable ||
		previous.JournalWritable != current.JournalWritable ||
		previous.SpoolWritable != current.SpoolWritable ||
		previous.AdmissionBlocked != current.AdmissionBlocked ||
		previous.ContextsDeleted != current.ContextsDeleted ||
		previous.GoalsDeleted != current.GoalsDeleted ||
		previous.CodexMappingsDeleted != current.CodexMappingsDeleted ||
		previous.LastError != current.LastError
}

func (s *Server) recordDiskGuardEvent(ctx context.Context, previous, current DiskGuardSnapshot) {
	if s.store == nil {
		return
	}
	detail, _ := json.Marshal(map[string]interface{}{
		"previous_level":    previous.Level,
		"level":             current.Level,
		"admission_blocked": current.AdmissionBlocked,
		"database_writable": current.DatabaseWritable,
		"journal_writable":  current.JournalWritable,
		"spool_writable":    current.SpoolWritable,
		"error_code":        current.LastError,
	})
	eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = s.store.AddDiagnosticEvent(eventCtx, storage.DiagnosticEvent{
		ID:         newRequestID(),
		EventType:  "storage_pressure",
		Severity:   current.Level,
		EntityType: "host",
		DetailJSON: string(detail),
	})
}
