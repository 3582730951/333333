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

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/datadir"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

const (
	diskPressureFreeBytes             = uint64(2 << 30)
	diskCriticalFreeBytes             = uint64(512 << 20)
	diskEmergencyFreeBytes            = uint64(128 << 20)
	diskRecoveryFreeBytes             = uint64(4 << 30)
	goalStorageReserveMin             = int64(8 << 20)
	goalStorageReserveMax             = int64(128 << 20)
	goalStorageMaintenanceStepsPerRun = 16
	// Headroom maintenance leaves below the new-goal admission ceiling, so a fresh
	// session is admitted without a foreground reclaim. Bounded on both sides: large
	// enough for several ordinary turns, small enough to stay a rounding-scale slice
	// of the reserve rather than a second budget.
	goalStorageAdmissionHeadroomMin = int64(1 << 20)
	goalStorageAdmissionHeadroomMax = int64(16 << 20)
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
	GoalBytesReclaimed      int64                    `json:"goal_bytes_reclaimed"`
	GoalStorageTargetBytes  int64                    `json:"goal_storage_target_bytes"`
	GoalStorageReserveBytes int64                    `json:"goal_storage_reserve_bytes"`
	CodexMappingsDeleted    int64                    `json:"codex_mappings_deleted"`
	RouteBindingsDeleted    int64                    `json:"route_bindings_deleted"`
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
	policyMigration, policyMigrationErr := s.store.MigrateGoalPolicyDefaults(ctx,
		s.cfg.GoalStorageMaxMB, config.LegacyDefaultGoalStorageMaxMB, config.DefaultGoalStorageMaxMB,
		s.cfg.GoalLegacyJournalDualWrite)
	if policyMigrationErr == nil && (policyMigration.StorageDefaultUpgraded || policyMigration.LegacyDualWriteDisabled) {
		log.Printf("[GOAL-CONTINUITY] migrated inherited defaults storage_1gib=%t legacy_dual_write_disabled=%t",
			policyMigration.StorageDefaultUpgraded, policyMigration.LegacyDualWriteDisabled)
	}
	previous := s.diskGuardSnapshot()
	probes, probeCodes := s.probeDiskFilesystems(previous.Level)
	snap := DiskGuardSnapshot{
		Level:                "normal",
		LastRunAt:            storage.Now(),
		ContextsDeleted:      previous.ContextsDeleted,
		GoalsDeleted:         previous.GoalsDeleted,
		GoalBytesReclaimed:   previous.GoalBytesReclaimed,
		CodexMappingsDeleted: previous.CodexMappingsDeleted,
		RouteBindingsDeleted: previous.RouteBindingsDeleted,
		LogsDeleted:          previous.LogsDeleted,
		LastLogCleanupAt:     previous.LastLogCleanupAt,
		DatabaseWritable:     s.databaseWritable(ctx),
		JournalWritable:      s.journalWritable(),
		SpoolWritable:        s.managedDirectoryWritable(s.cfg.BodySpoolDir),
	}
	snap.GoalStorageTargetBytes, snap.GoalStorageReserveBytes = goalStorageMaintenanceTarget(s.goalStorageMaxBytes(ctx))
	snap.Filesystems, snap.Level, snap.FreePercent, snap.FreeBytes = summarizeFilesystemProbes(probes)
	if len(probeCodes) > 0 {
		snap.LastError = strings.Join(probeCodes, ",")
	}
	if policyMigrationErr != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "goal_policy_default_migration_failed")
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
		log.Printf("[DISK-GUARD] level=%s free_pct=%.1f free_bytes=%d db_writable=%t journal_writable=%t spool_writable=%t admission_blocked=%t cleanup_contexts=%d cleanup_goals=%d goal_bytes_reclaimed=%d goal_target_bytes=%d goal_reserve_bytes=%d cleanup_mappings=%d cleanup_route_bindings=%d error_code=%s",
			snap.Level, snap.FreePercent, snap.FreeBytes, snap.DatabaseWritable, snap.JournalWritable,
			snap.SpoolWritable, snap.AdmissionBlocked, snap.ContextsDeleted, snap.GoalsDeleted,
			snap.GoalBytesReclaimed, snap.GoalStorageTargetBytes, snap.GoalStorageReserveBytes,
			snap.CodexMappingsDeleted, snap.RouteBindingsDeleted, snap.LastError)
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
	// Converge below the target, not to it. See goalStorageMaintenanceFloor: the
	// target doubles as CommitGoalTurn's admission ceiling for a new goal.
	maintenanceFloor := goalStorageMaintenanceFloor(s.goalStorageMaxBytes(ctx))
	for step := 0; step < goalStorageMaintenanceStepsPerRun; step++ {
		used, err := s.store.GoalStorageBytes(cleanupCtx)
		if err != nil {
			snap.LastError = appendDiskGuardCode(snap.LastError, "goal_budget_measure_failed")
			break
		}
		if used <= maintenanceFloor {
			break
		}
		reclaimed, err := s.store.EnforceGoalStorageBudgetStep(cleanupCtx, maintenanceFloor)
		if err != nil {
			snap.LastError = appendDiskGuardCode(snap.LastError, "goal_budget_cleanup_failed")
			break
		}
		snap.GoalBytesReclaimed += reclaimed.BytesFreed
		snap.GoalsDeleted += reclaimed.Goals
		if !reclaimed.Progressed {
			break
		}
	}
	if mappings, err := s.store.CleanupCodexSessionMappings(cleanupCtx); err != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "mapping_cleanup_failed")
	} else {
		snap.CodexMappingsDeleted += mappings
	}
	if bindings, err := s.store.CleanupInactiveRouteBindings(cleanupCtx, 256); err != nil {
		snap.LastError = appendDiskGuardCode(snap.LastError, "route_binding_cleanup_failed")
	} else {
		snap.RouteBindingsDeleted += bindings.Total()
	}
	s.cleanupExpiredDiagnosticJobs(cleanupCtx)
}

// goalStorageMaintenanceFloor is the value background maintenance actually converges
// to. It must be strictly below the target, because the target is simultaneously the
// admission ceiling CommitGoalTurn applies to a brand-new goal: converging to exactly
// the target leaves a new session only the rounding remainder of headroom (217 KiB of
// a 896 MiB target in the reported deployment), so almost every new goal is rejected
// with a storage-budget error and has to pay a bounded foreground reclaim first — or
// fails outright and leaves nothing for the next turn to resume.
func goalStorageMaintenanceFloor(maxBytes int64) int64 {
	target, reserve := goalStorageMaintenanceTarget(maxBytes)
	if target <= 0 {
		return target
	}
	headroom := reserve / 8
	if headroom < goalStorageAdmissionHeadroomMin {
		headroom = goalStorageAdmissionHeadroomMin
	}
	if headroom > goalStorageAdmissionHeadroomMax {
		headroom = goalStorageAdmissionHeadroomMax
	}
	if headroom >= target {
		// A tiny budget cannot give up a fixed slice; halve the target instead so
		// maintenance still lands strictly below the admission ceiling.
		headroom = target / 2
	}
	return target - headroom
}

func goalStorageMaintenanceTarget(maxBytes int64) (target, reserve int64) {
	if maxBytes <= 0 {
		return 0, 0
	}
	reserve = maxBytes / 8
	if reserve < goalStorageReserveMin {
		reserve = goalStorageReserveMin
	}
	if reserve > goalStorageReserveMax {
		reserve = goalStorageReserveMax
	}
	if reserve >= maxBytes {
		reserve = maxBytes / 2
	}
	return maxBytes - reserve, reserve
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
		previous.GoalBytesReclaimed != current.GoalBytesReclaimed ||
		previous.GoalStorageTargetBytes != current.GoalStorageTargetBytes ||
		previous.GoalStorageReserveBytes != current.GoalStorageReserveBytes ||
		previous.CodexMappingsDeleted != current.CodexMappingsDeleted ||
		previous.RouteBindingsDeleted != current.RouteBindingsDeleted ||
		previous.LastError != current.LastError
}

func (s *Server) recordDiskGuardEvent(ctx context.Context, previous, current DiskGuardSnapshot) {
	if s.store == nil {
		return
	}
	detail, _ := json.Marshal(map[string]interface{}{
		"previous_level":             previous.Level,
		"level":                      current.Level,
		"admission_blocked":          current.AdmissionBlocked,
		"database_writable":          current.DatabaseWritable,
		"journal_writable":           current.JournalWritable,
		"spool_writable":             current.SpoolWritable,
		"goal_bytes_reclaimed":       current.GoalBytesReclaimed,
		"goal_storage_target_bytes":  current.GoalStorageTargetBytes,
		"goal_storage_reserve_bytes": current.GoalStorageReserveBytes,
		"error_code":                 current.LastError,
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
