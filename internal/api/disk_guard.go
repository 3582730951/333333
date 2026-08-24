package api

import (
	"context"
	"encoding/json"
	"errors"
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
	diskGuardProbeInterval            = 15 * time.Second
	diskGuardNormalCleanupInterval    = 5 * time.Minute
	diskGuardCleanupTimeout           = 10 * time.Second
	diskGuardContextCleanupTimeout    = 1500 * time.Millisecond
	diskGuardMappingCleanupTimeout    = 2500 * time.Millisecond
	diskGuardRouteCleanupTimeout      = 1500 * time.Millisecond
	diskGuardGoalCleanupTimeout       = 3 * time.Second
	diskGuardGoalBudgetTimeout        = 3 * time.Second
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
	Level                      string                   `json:"level"`
	FreePercent                float64                  `json:"free_percent"`
	FreeBytes                  uint64                   `json:"free_bytes"`
	Filesystems                []DiskFilesystemSnapshot `json:"filesystems,omitempty"`
	ForcedContextTTLSeconds    int                      `json:"forced_context_ttl_seconds"`
	ContextsDeleted            int64                    `json:"contexts_deleted"`
	GoalsDeleted               int64                    `json:"goals_deleted"`
	GoalBytesReclaimed         int64                    `json:"goal_bytes_reclaimed"`
	GoalStorageTargetBytes     int64                    `json:"goal_storage_target_bytes"`
	GoalStorageReserveBytes    int64                    `json:"goal_storage_reserve_bytes"`
	CodexMappingsDeleted       int64                    `json:"codex_mappings_deleted"`
	RouteBindingsDeleted       int64                    `json:"route_bindings_deleted"`
	LogsDeleted                int64                    `json:"logs_deleted"`
	LastRunAt                  int64                    `json:"last_run_at"`
	LastMaintenanceAt          int64                    `json:"last_maintenance_at,omitempty"`
	LastLogCleanupAt           int64                    `json:"last_log_cleanup_at,omitempty"`
	DatabaseWritable           bool                     `json:"database_writable"`
	DatabaseBackpressured      bool                     `json:"database_backpressured,omitempty"`
	DatabaseErrorClass         string                   `json:"database_error_class,omitempty"`
	DatabaseBackpressureEvents uint64                   `json:"database_backpressure_events,omitempty"`
	LastDatabaseErrorAt        int64                    `json:"last_database_error_at,omitempty"`
	JournalWritable            bool                     `json:"journal_writable"`
	SpoolWritable              bool                     `json:"spool_writable"`
	BackgroundPaused           bool                     `json:"background_paused"`
	LargeRequestsPaused        bool                     `json:"large_requests_paused"`
	AdmissionBlocked           bool                     `json:"admission_blocked"`
	CleanupFailureEvents       uint64                   `json:"cleanup_failure_events,omitempty"`
	CleanupErrorOperation      string                   `json:"cleanup_error_operation,omitempty"`
	CleanupErrorClass          string                   `json:"cleanup_error_class,omitempty"`
	LastCleanupErrorAt         int64                    `json:"last_cleanup_error_at,omitempty"`
	LastError                  string                   `json:"last_error,omitempty"`
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
		s.runDiskGuardCycle(ctx, false)
		t := time.NewTicker(diskGuardProbeInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runDiskGuardCycle(ctx, false)
			}
		}
	})
}

func (s *Server) runDiskGuard(ctx context.Context) {
	s.runDiskGuardCycle(ctx, true)
}

func (s *Server) runDiskGuardCycle(ctx context.Context, forceCleanup bool) {
	var policyMigration storage.GoalPolicyDefaultsMigration
	var policyMigrationErr error
	if !s.goalPolicyDefaultsMigrated.Load() {
		migrationCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		policyMigration, policyMigrationErr = s.store.MigrateGoalPolicyDefaults(migrationCtx,
			s.cfg.GoalStorageMaxMB, config.LegacyDefaultGoalStorageMaxMB, config.DefaultGoalStorageMaxMB,
			s.cfg.GoalLegacyJournalDualWrite)
		cancel()
		if policyMigrationErr == nil {
			s.goalPolicyDefaultsMigrated.Store(true)
		}
	}
	if policyMigrationErr == nil && (policyMigration.StorageDefaultUpgraded || policyMigration.LegacyDualWriteDisabled) {
		log.Printf("[GOAL-CONTINUITY] migrated inherited defaults storage_1gib=%t legacy_dual_write_disabled=%t",
			policyMigration.StorageDefaultUpgraded, policyMigration.LegacyDualWriteDisabled)
	}
	previous := s.diskGuardSnapshot()
	probes, probeCodes := s.probeDiskFilesystems(previous.Level)
	databaseWritable, databaseBackpressured, databaseErrorClass := s.databaseWriteProbe(ctx, previous.DatabaseWritable)
	snap := DiskGuardSnapshot{
		Level:                      "normal",
		LastRunAt:                  storage.Now(),
		ContextsDeleted:            previous.ContextsDeleted,
		GoalsDeleted:               previous.GoalsDeleted,
		GoalBytesReclaimed:         previous.GoalBytesReclaimed,
		CodexMappingsDeleted:       previous.CodexMappingsDeleted,
		RouteBindingsDeleted:       previous.RouteBindingsDeleted,
		LogsDeleted:                previous.LogsDeleted,
		LastMaintenanceAt:          previous.LastMaintenanceAt,
		LastLogCleanupAt:           previous.LastLogCleanupAt,
		DatabaseWritable:           databaseWritable,
		DatabaseBackpressured:      databaseBackpressured,
		DatabaseErrorClass:         databaseErrorClass,
		DatabaseBackpressureEvents: previous.DatabaseBackpressureEvents,
		LastDatabaseErrorAt:        previous.LastDatabaseErrorAt,
		JournalWritable:            s.journalWritable(),
		SpoolWritable:              s.managedDirectoryWritable(s.cfg.BodySpoolDir),
		CleanupFailureEvents:       previous.CleanupFailureEvents,
		CleanupErrorOperation:      previous.CleanupErrorOperation,
		CleanupErrorClass:          previous.CleanupErrorClass,
		LastCleanupErrorAt:         previous.LastCleanupErrorAt,
	}
	if databaseBackpressured {
		snap.DatabaseBackpressureEvents++
		snap.LastDatabaseErrorAt = snap.LastRunAt
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
	s.usageDirectWrites.Store(snap.LargeRequestsPaused && snap.DatabaseWritable && !snap.DatabaseBackpressured)

	cleanupDue := diskGuardCleanupDue(snap.LastRunAt, snap.LastMaintenanceAt, snap.Level, forceCleanup)
	if cleanupDue && snap.DatabaseWritable && !snap.DatabaseBackpressured {
		// These two fields describe the most recent maintenance attempt. Preserve
		// them across probe-only ticks, then clear them only when a new attempt starts.
		snap.CleanupErrorOperation = ""
		snap.CleanupErrorClass = ""
		s.runSafeDiskCleanup(ctx, &snap)
		snap.LastMaintenanceAt = snap.LastRunAt
	}
	if snap.Level != "normal" && snap.DatabaseWritable && !snap.DatabaseBackpressured {
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

func diskGuardCleanupDue(now, lastMaintenance int64, level string, force bool) bool {
	return force || lastMaintenance == 0 || level != "normal" ||
		now-lastMaintenance >= int64(diskGuardNormalCleanupInterval/time.Second)
}

// runSafeDiskCleanup advances every retention stage once, in order of cost.
//
// A failing stage records the failure and the run CONTINUES to the next one. It used
// to return, and that turned one slow table into a total retention outage: the stages
// are independent jobs over unrelated tables, each already holds its own deadline
// (1.5s/2.5s/1.5s/3s/3s under a 10s cap), so aborting the chain buys nothing and
// costs everything downstream of the failure.
//
// Observed in a v3 diagnostics bundle: codex_session_mappings cleanup timed out at
// its 2.5s cap 941 times, and because it sits second in the chain, route-binding
// cleanup, goal reclamation, goal-budget enforcement and diagnostic-job expiry were
// all skipped on every one of those cycles -- route_bindings_deleted stayed at
// exactly 0 while the failure counter climbed into the hundreds.
// The tables those stages drain kept growing, which is visible two steps later as
// 1918 database backpressure events and a full diagnostics export that could no
// longer finish inside its deadline and fell back to emergency mode with 23 of 33
// tables omitted.
//
// The shared maintenanceCtx still bounds total work, so continuing cannot run long:
// once it expires every remaining stage fails immediately, which the deadline check
// below turns into an early stop rather than a burst of pointless failures.
func (s *Server) runSafeDiskCleanup(ctx context.Context, snap *DiskGuardSnapshot) {
	maintenanceCtx, maintenanceCancel := context.WithTimeout(ctx, diskGuardCleanupTimeout)
	defer maintenanceCancel()

	contextCtx, contextCancel := context.WithTimeout(maintenanceCtx, diskGuardContextCleanupTimeout)
	deleted, err := s.store.CleanupContextJournal(contextCtx)
	contextCancel()
	if err != nil {
		noteDiskCleanupFailure(snap, "context_cleanup", "context_cleanup_failed", err)
	} else {
		snap.ContextsDeleted += deleted
	}

	// Session/affinity retention used to run after as many as sixteen Goal
	// reclamation transactions under one shared deadline. A busy continuity store
	// therefore consumed the entire maintenance budget before mapping cleanup could
	// start. Run small metadata retention first and give every stage its own cap.
	if maintenanceCtx.Err() == nil {
		mappingCtx, mappingCancel := context.WithTimeout(maintenanceCtx, diskGuardMappingCleanupTimeout)
		mappings, mappingErr := s.store.CleanupCodexSessionMappings(mappingCtx)
		mappingCancel()
		if mappingErr != nil {
			noteDiskCleanupFailure(snap, "mapping_cleanup", "mapping_cleanup_failed", mappingErr)
		} else {
			snap.CodexMappingsDeleted += mappings
		}
	}

	if maintenanceCtx.Err() == nil {
		routeCtx, routeCancel := context.WithTimeout(maintenanceCtx, diskGuardRouteCleanupTimeout)
		bindings, routeErr := s.store.CleanupInactiveRouteBindings(routeCtx, 256)
		routeCancel()
		if routeErr != nil {
			noteDiskCleanupFailure(snap, "route_binding_cleanup", "route_binding_cleanup_failed", routeErr)
		} else {
			snap.RouteBindingsDeleted += bindings.Total()
		}
	}

	// Advance multiple bounded phases for expired goals. One step removes at most
	// 64 rows/8 MiB, so a single step every five minutes left large abandoned tool
	// sessions visible for hours after expiry.
	if maintenanceCtx.Err() == nil {
		goalCtx, goalCancel := context.WithTimeout(maintenanceCtx, diskGuardGoalCleanupTimeout)
		for step := 0; step < goalStorageMaintenanceStepsPerRun && goalCtx.Err() == nil; step++ {
			reclaimed, reclaimErr := s.store.CleanupGoalContinuityStep(goalCtx)
			if reclaimErr != nil {
				noteDiskCleanupFailure(snap, "goal_cleanup", "goal_cleanup_failed", reclaimErr)
				break
			}
			snap.GoalBytesReclaimed += reclaimed.BytesFreed
			snap.GoalsDeleted += reclaimed.Goals
			if !reclaimed.Progressed {
				break
			}
		}
		goalCancel()
	}

	// Converge below the target, not to it. See goalStorageMaintenanceFloor: the
	// target doubles as CommitGoalTurn's admission ceiling for a new goal.
	if maintenanceCtx.Err() == nil {
		maintenanceFloor := goalStorageMaintenanceFloor(s.goalStorageMaxBytes(maintenanceCtx))
		budgetCtx, budgetCancel := context.WithTimeout(maintenanceCtx, diskGuardGoalBudgetTimeout)
		for step := 0; step < goalStorageMaintenanceStepsPerRun && budgetCtx.Err() == nil; step++ {
			used, usedErr := s.store.GoalStorageBytes(budgetCtx)
			if usedErr != nil {
				noteDiskCleanupFailure(snap, "goal_budget_measure", "goal_budget_measure_failed", usedErr)
				break
			}
			if used <= maintenanceFloor {
				break
			}
			reclaimed, reclaimErr := s.store.EnforceGoalStorageBudgetStep(budgetCtx, maintenanceFloor)
			if reclaimErr != nil {
				noteDiskCleanupFailure(snap, "goal_budget_cleanup", "goal_budget_cleanup_failed", reclaimErr)
				break
			}
			snap.GoalBytesReclaimed += reclaimed.BytesFreed
			snap.GoalsDeleted += reclaimed.Goals
			if !reclaimed.Progressed {
				break
			}
		}
		budgetCancel()
	}

	if maintenanceCtx.Err() == nil {
		s.cleanupExpiredDiagnosticJobs(maintenanceCtx)
	}
}

func noteDiskCleanupFailure(snap *DiskGuardSnapshot, operation, code string, err error) {
	if snap == nil {
		return
	}
	snap.LastError = appendDiskGuardCode(snap.LastError, code)
	snap.CleanupErrorOperation = operation
	snap.CleanupErrorClass = classifyStorageFailure(err)
	snap.CleanupFailureEvents++
	snap.LastCleanupErrorAt = storage.Now()
	log.Printf("[DISK-GUARD] cleanup operation=%s error_class=%s", operation, snap.CleanupErrorClass)
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

func (s *Server) databaseWriteProbe(ctx context.Context, previousWritable bool) (writable, backpressured bool, errorClass string) {
	if s.store == nil {
		return false, false, "unavailable"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := s.store.CheckWritable(checkCtx)
	if err == nil {
		return true, false, ""
	}
	errorClass = classifyStorageFailure(err)
	if errorClass == "busy" || errorClass == "timeout" || errorClass == "cancelled" {
		// A write probe shares the deliberately single-connection SQLite writer with
		// foreground context commits. Timing out in that queue proves backpressure,
		// not a read-only filesystem. Preserve the last confirmed writability state,
		// skip optional maintenance, and let foreground writes/journal replay drain.
		return previousWritable, true, errorClass
	}
	return false, false, errorClass
}

func classifyStorageFailure(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "database is locked"),
		strings.Contains(message, "database table is locked"),
		strings.Contains(message, "sqlite_busy"),
		strings.Contains(message, "sqlite_locked"),
		strings.Contains(message, "deadlock"),
		strings.Contains(message, "serialization failure"),
		strings.Contains(message, "too many connections"):
		return "busy"
	case strings.Contains(message, "readonly"),
		strings.Contains(message, "read-only"),
		strings.Contains(message, "permission denied"),
		strings.Contains(message, "operation not permitted"):
		return "readonly"
	case strings.Contains(message, "database or disk is full"),
		strings.Contains(message, "no space left"),
		strings.Contains(message, "enospc"),
		strings.Contains(message, "disk quota exceeded"):
		return "full"
	case strings.Contains(message, "database disk image is malformed"),
		strings.Contains(message, "database corruption"),
		strings.Contains(message, "sqlite_corrupt"):
		return "corrupt"
	case strings.Contains(message, "disk i/o error"),
		strings.Contains(message, "input/output error"),
		strings.Contains(message, "sqlite_ioerr"):
		return "io"
	default:
		return "unavailable"
	}
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
		previous.GoalStorageTargetBytes != current.GoalStorageTargetBytes ||
		previous.GoalStorageReserveBytes != current.GoalStorageReserveBytes
}

func (s *Server) recordDiskGuardEvent(ctx context.Context, previous, current DiskGuardSnapshot) {
	if s.store == nil {
		return
	}
	detail, _ := json.Marshal(map[string]interface{}{
		"previous_level":               previous.Level,
		"level":                        current.Level,
		"admission_blocked":            current.AdmissionBlocked,
		"database_writable":            current.DatabaseWritable,
		"database_backpressured":       current.DatabaseBackpressured,
		"database_error_class":         current.DatabaseErrorClass,
		"database_backpressure_events": current.DatabaseBackpressureEvents,
		"cleanup_failure_events":       current.CleanupFailureEvents,
		"cleanup_error_operation":      current.CleanupErrorOperation,
		"cleanup_error_class":          current.CleanupErrorClass,
		"journal_writable":             current.JournalWritable,
		"spool_writable":               current.SpoolWritable,
		"goal_bytes_reclaimed":         current.GoalBytesReclaimed,
		"goal_storage_target_bytes":    current.GoalStorageTargetBytes,
		"goal_storage_reserve_bytes":   current.GoalStorageReserveBytes,
		"error_code":                   current.LastError,
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
