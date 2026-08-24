package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestDiskGuardThresholdsAndHysteresis(t *testing.T) {
	cases := []struct {
		free           float64
		bytes          uint64
		previous, want string
	}{
		{20, 8 << 30, "normal", "normal"},
		{9.9, 8 << 30, "normal", "pressure"},
		{20, (2 << 30) - 1, "normal", "pressure"},
		{4.9, 8 << 30, "pressure", "critical"},
		{20, (512 << 20) - 1, "pressure", "critical"},
		{1.9, 8 << 30, "critical", "emergency"},
		{20, (128 << 20) - 1, "critical", "emergency"},
		{14.9, 8 << 30, "critical", "pressure"},
		{20, (4 << 30) - 1, "pressure", "pressure"},
		{15, 4 << 30, "pressure", "normal"},
	}
	for _, tc := range cases {
		if got := diskGuardLevel(tc.free, tc.bytes, tc.previous); got != tc.want {
			t.Errorf("free=%v bytes=%d previous=%s got=%s want=%s", tc.free, tc.bytes, tc.previous, got, tc.want)
		}
	}
}

func TestBodySpoolReservePreservesEmergencyHeadroom(t *testing.T) {
	reserve := bodySpoolMinimumFreeBytes(t.TempDir(), 0)
	if reserve < int64(diskEmergencyFreeBytes) {
		t.Fatalf("automatic spool reserve = %d, want at least %d", reserve, diskEmergencyFreeBytes)
	}
	explicit := reserve + 64<<20
	if got := bodySpoolMinimumFreeBytes(t.TempDir(), explicit); got < explicit {
		t.Fatalf("explicit spool reserve = %d, want at least %d", got, explicit)
	}
}

func TestGoalStorageMaintenanceTarget(t *testing.T) {
	cases := []struct {
		max, target, reserve int64
	}{
		{0, 0, 0},
		{1 << 20, 512 << 10, 512 << 10},
		{256 << 20, 224 << 20, 32 << 20},
		{2 << 30, 1920 << 20, 128 << 20},
	}
	for _, tc := range cases {
		target, reserve := goalStorageMaintenanceTarget(tc.max)
		if target != tc.target || reserve != tc.reserve {
			t.Errorf("max=%d target/reserve=%d/%d, want %d/%d", tc.max, target, reserve, tc.target, tc.reserve)
		}
	}
}

func TestDiskGuardChangeIgnoresCumulativeCleanupProgress(t *testing.T) {
	previous := DiskGuardSnapshot{
		Level: "normal", DatabaseWritable: true, JournalWritable: true, SpoolWritable: true,
		GoalStorageTargetBytes:  896 << 20,
		GoalStorageReserveBytes: 128 << 20,
	}
	current := previous
	current.ContextsDeleted = 10
	current.GoalsDeleted = 3
	current.GoalBytesReclaimed = 64 << 20
	current.CodexMappingsDeleted = 4
	current.RouteBindingsDeleted = 8
	if diskGuardChanged(previous, current) {
		t.Fatal("normal cleanup counters must not emit a storage-pressure transition")
	}
	current.Level = "pressure"
	if !diskGuardChanged(previous, current) {
		t.Fatal("an operational disk state transition must still be emitted")
	}
}

func TestDiskGuardChangeIgnoresCleanupErrorChurn(t *testing.T) {
	previous := DiskGuardSnapshot{
		Level: "normal", DatabaseWritable: true, JournalWritable: true, SpoolWritable: true,
		GoalStorageTargetBytes: 896 << 20, GoalStorageReserveBytes: 128 << 20,
		LastError: "context_cleanup_failed",
	}
	current := previous
	current.LastError = "goal_cleanup_failed,mapping_cleanup_failed"
	current.CleanupFailureEvents = 9000
	current.CleanupErrorOperation = "mapping_cleanup"
	current.CleanupErrorClass = "timeout"
	if diskGuardChanged(previous, current) {
		t.Fatal("cleanup error churn must not recursively write storage-pressure events")
	}
}

func TestDiskGuardCleanupCadence(t *testing.T) {
	now := int64(10_000)
	interval := int64(diskGuardNormalCleanupInterval / time.Second)
	cases := []struct {
		name        string
		last        int64
		level       string
		force, want bool
	}{
		{name: "startup", last: 0, level: "normal", want: true},
		{name: "probe_only", last: now - interval + 1, level: "normal", want: false},
		{name: "normal_due", last: now - interval, level: "normal", want: true},
		{name: "pressure", last: now - 1, level: "pressure", want: true},
		{name: "forced", last: now - 1, level: "normal", force: true, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := diskGuardCleanupDue(now, tc.last, tc.level, tc.force); got != tc.want {
				t.Fatalf("cleanup due=%t want=%t", got, tc.want)
			}
		})
	}
}

func TestClassifyStorageFailureUsesStableNonSensitiveClasses(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "timeout"},
		{context.Canceled, "cancelled"},
		{errors.New("database is locked"), "busy"},
		{errors.New("attempt to write a readonly database"), "readonly"},
		{errors.New("database or disk is full"), "full"},
		{errors.New("database disk image is malformed"), "corrupt"},
		{errors.New("disk I/O error"), "io"},
		{errors.New("credential-shaped unknown failure secret=do-not-export"), "unavailable"},
	}
	for _, tc := range cases {
		if got := classifyStorageFailure(tc.err); got != tc.want {
			t.Errorf("error=%q class=%q want=%q", tc.err, got, tc.want)
		}
	}
}

func TestDatabaseWriteProbeTreatsWriterQueueTimeoutAsBackpressure(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	s := &Server{store: store}
	probeCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	writable, backpressured, class := s.databaseWriteProbe(probeCtx, true)
	if !writable || !backpressured || class != "timeout" {
		t.Fatalf("probe writable=%t backpressured=%t class=%q", writable, backpressured, class)
	}
}

func TestDiskCleanupWriterBackpressureReportsOnlyRootOperation(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	s := &Server{store: store, cfg: config.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	snap := DiskGuardSnapshot{}
	s.runSafeDiskCleanup(ctx, &snap)
	if snap.LastError != "context_cleanup_failed" || snap.CleanupFailureEvents != 1 ||
		snap.CleanupErrorOperation != "context_cleanup" || snap.CleanupErrorClass != "timeout" {
		t.Fatalf("cleanup snapshot=%+v", snap)
	}
}

func TestLegacyGoalPolicyUpgradeLetsSaturatedAwaitingToolGoalContinue(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	// Reproduce the latest deployed package: both values came from the old
	// bootstrap config and there was no runtime override.
	cfg.GoalStorageMaxMB = config.LegacyDefaultGoalStorageMaxMB
	cfg.GoalLegacyJournalDualWrite = true
	s := &Server{cfg: cfg, store: store}
	first := storage.GoalTurn{
		Protocol: "codex", DownstreamKeyHash: "saturated-key", WorkspaceHash: "saturated-workspace",
		InitialGoalHash: "saturated-initial", ResponseID: "saturated-r1",
		Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: "saturated-root"}},
		CheckpointPayload: `{"model":"gpt-test","input":[]}`,
		SegmentPayload:    `{"history_key":"input","input":[{"role":"user","content":"before-saturation"}],"output":[{"type":"custom_tool_call","call_id":"pending-call","name":"tool","arguments":"{}"}]}`,
		WorkingState:      `{"state":"waiting"}`,
		AwaitingTool:      true,
		ExpiresAt:         storage.Now() + 86400,
	}
	goal, err := store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	legacyMax := int64(config.LegacyDefaultGoalStorageMaxMB) << 20
	if _, err = store.DB().ExecContext(ctx, `UPDATE goal_session SET storage_bytes=? WHERE id=?`, legacyMax-1, goal.ID); err != nil {
		t.Fatal(err)
	}

	// The startup disk guard owns the idempotent policy-default migration. It must
	// raise both the hard admission budget and its maintenance target before trying
	// to append the next successful terminal.
	s.runDiskGuard(ctx)
	effectiveMax := s.goalStorageMaxBytes(ctx)
	target, reserve := goalStorageMaintenanceTarget(effectiveMax)
	if effectiveMax != int64(config.DefaultGoalStorageMaxMB)<<20 || target != 896<<20 || reserve != 128<<20 {
		t.Fatalf("effective max/target/reserve=%d/%d/%d, want %d/%d/%d", effectiveMax, target, reserve,
			int64(config.DefaultGoalStorageMaxMB)<<20, int64(896<<20), int64(128<<20))
	}
	storageSetting, ok, err := store.GetSetting(ctx, "goal_storage_max_mb")
	if err != nil || !ok || storageSetting != "1024" {
		t.Fatalf("migrated storage setting=%q present=%t err=%v", storageSetting, ok, err)
	}
	dualSetting, ok, err := store.GetSetting(ctx, "goal_legacy_journal_dual_write")
	if err != nil || !ok || dualSetting != "false" {
		t.Fatalf("migrated dual-write setting=%q present=%t err=%v", dualSetting, ok, err)
	}
	// Disabling future v1 writes must not delete or hide the existing fallback tail;
	// it remains readable until its normal TTL expires.
	if err = store.PutContextJournal(ctx, storage.ContextJournal{
		ResponseID: "legacy-tail", AccountID: "account", ExpiresAt: storage.Now() + 3600,
		Payload: `{"model":"gpt-test","input":[{"role":"user","content":"legacy-readable"}]}`,
	}); err != nil {
		t.Fatal(err)
	}
	legacyReplay, legacyOK := s.journalReplayBody(ctx, []byte(`{"model":"gpt-test","previous_response_id":"legacy-tail","input":[{"role":"user","content":"new-tail"}]}`))
	if !legacyOK || !strings.Contains(string(legacyReplay), "legacy-readable") || !strings.Contains(string(legacyReplay), "new-tail") {
		t.Fatalf("legacy read fallback after dual-write migration ok=%t replay=%s", legacyOK, legacyReplay)
	}

	next := first
	next.ResponseID = "saturated-r2"
	next.AwaitingTool = false
	next.WorkingState = `{"state":"continued"}`
	next.SegmentPayload = `{"history_key":"input","input":[{"type":"custom_tool_call_output","call_id":"pending-call","output":"tool-finished"}],"output":[{"type":"message","content":"after-saturation"}]}`
	next.StorageMaxBytes = effectiveMax
	continued, err := store.CommitGoalTurn(ctx, next)
	if err != nil || continued.ID != goal.ID {
		t.Fatalf("continued saturated goal=%+v err=%v", continued, err)
	}
	replay, session, err := store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || session.State != "ready" {
		t.Fatalf("continued replay session=%+v err=%v", session, err)
	}
	for _, marker := range []string{"before-saturation", "pending-call", "tool-finished", "after-saturation"} {
		if !strings.Contains(string(replay), marker) {
			t.Fatalf("continued replay lost %q: %s", marker, replay)
		}
	}
}

func TestLegacyGoalPolicyUpgradePreservesExplicitRuntimeCap(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err = store.SetSetting(ctx, "goal_storage_max_mb", "256"); err != nil {
		t.Fatal(err)
	}
	if err = store.SetSetting(ctx, "goal_legacy_journal_dual_write", "true"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.GoalStorageMaxMB = config.LegacyDefaultGoalStorageMaxMB
	cfg.GoalLegacyJournalDualWrite = true
	s := &Server{cfg: cfg, store: store}
	s.runDiskGuard(ctx)
	if max := s.goalStorageMaxBytes(ctx); max != 256<<20 {
		t.Fatalf("explicit runtime cap changed to %d", max)
	}
	if enabled := s.flagEnabled(ctx, "goal_legacy_journal_dual_write", cfg.GoalLegacyJournalDualWrite); !enabled {
		t.Fatal("explicit runtime dual-write choice was changed")
	}
}

func TestRunSafeDiskCleanupCreatesGoalStorageHeadroomBeforeHardLimit(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	s := &Server{cfg: cfg, store: store}
	maxBytes := int64(1 << 20)
	target, reserve := goalStorageMaintenanceTarget(maxBytes)
	var used int64
	for i := 0; i < 8 && used <= target+(64<<10); i++ {
		raw := make([]byte, 96<<10)
		state := uint32(i + 1)
		for j := range raw {
			state = state*1664525 + 1013904223
			raw[j] = byte(state >> 24)
		}
		payload := base64.StdEncoding.EncodeToString(raw)
		_, err = store.CommitGoalTurn(ctx, storage.GoalTurn{
			Protocol:          "codex",
			DownstreamKeyHash: fmt.Sprintf("disk-guard-key-%d", i),
			WorkspaceHash:     fmt.Sprintf("disk-guard-workspace-%d", i),
			InitialGoalHash:   fmt.Sprintf("disk-guard-goal-%d", i),
			ResponseID:        fmt.Sprintf("disk-guard-response-%d", i),
			Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: fmt.Sprintf("disk-guard-root-%d", i)}},
			CheckpointPayload: `{"model":"gpt-test","input":[]}`,
			SegmentPayload:    fmt.Sprintf(`{"input":"%s","output":"ok"}`, payload),
			ExpiresAt:         storage.Now() + 86400,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&used); err != nil {
			t.Fatal(err)
		}
	}
	if used <= target || used >= maxBytes {
		t.Fatalf("fixture storage=%d, want maintenance target %d < used < hard max %d", used, target, maxBytes)
	}
	if err = store.SetSetting(ctx, "goal_storage_max_mb", "1"); err != nil {
		t.Fatal(err)
	}
	snap := DiskGuardSnapshot{GoalStorageTargetBytes: target, GoalStorageReserveBytes: reserve}
	s.runSafeDiskCleanup(ctx, &snap)
	var remaining int64
	if err = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if snap.GoalBytesReclaimed <= 0 || remaining >= used {
		t.Fatalf("cleanup reclaimed=%d storage before/after=%d/%d", snap.GoalBytesReclaimed, used, remaining)
	}
	if remaining > target {
		t.Fatalf("cleanup left storage=%d above maintenance target=%d", remaining, target)
	}
}

func TestRunSafeDiskCleanupCompletesExpiredGoalReclamationInOnePass(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	goal, err := store.CommitGoalTurn(ctx, storage.GoalTurn{
		Protocol:          "codex",
		DownstreamKeyHash: "expired-maintenance-key",
		WorkspaceHash:     "expired-maintenance-workspace",
		InitialGoalHash:   "expired-maintenance-goal",
		ResponseID:        "expired-maintenance-response",
		Aliases: []storage.GoalAlias{{
			Type: "codex_root_thread", Value: "expired-maintenance-root",
		}},
		CheckpointPayload: `{"model":"gpt-test","input":[]}`,
		SegmentPayload:    `{"input":"expired","output":"complete"}`,
		ExpiresAt:         storage.Now() + 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE goal_session SET expires_at=? WHERE id=?`, storage.Now()-1, goal.ID); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: config.Default(), store: store}
	snap := DiskGuardSnapshot{}
	s.runSafeDiskCleanup(ctx, &snap)
	if _, err = store.GetGoalSession(ctx, goal.ID); !errors.Is(err, storage.ErrGoalNotFound) {
		t.Fatalf("expired goal survived one maintenance pass: %v", err)
	}
	if snap.GoalsDeleted != 1 || snap.GoalBytesReclaimed <= 0 || snap.CleanupFailureEvents != 0 {
		t.Fatalf("expired maintenance snapshot=%+v", snap)
	}
}

// A stage that fails for a reason other than the maintenance deadline must not take
// the rest of the chain with it.
//
// This is the defect a v3 diagnostics bundle exposed: codex_session_mappings cleanup
// timed out at its own 2.5s cap 941 times, and because every stage used to `return`
// on error, the four stages behind it never ran on any of those cycles --
// route_bindings_deleted sat at exactly 0 while the tables they drain kept growing.
// The stages are independent jobs over unrelated tables and each already carries its
// own deadline, so one failing has no bearing on the next.
//
// Driven by closing the store: every stage then fails immediately with a real
// database error rather than a context error, which leaves the shared maintenance
// budget intact and is precisely the case the old code collapsed. Contrast with
// TestDiskCleanupWriterBackpressureReportsOnlyRootOperation, where the budget IS
// gone and stopping after the first failure is still the right answer.
func TestRunSafeDiskCleanupContinuesAfterANonDeadlineStageFailure(t *testing.T) {
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	s := &Server{store: store, cfg: config.Default()}
	snap := DiskGuardSnapshot{}
	s.runSafeDiskCleanup(context.Background(), &snap)

	if snap.CleanupFailureEvents < 2 {
		t.Fatalf("only %d stage(s) ran; a failing stage still aborts the chain: %+v",
			snap.CleanupFailureEvents, snap)
	}
	// The reported operation is the last stage attempted, so it must have advanced
	// past the first one -- that is the observable proof the chain kept going.
	if snap.CleanupErrorOperation == "context_cleanup" {
		t.Fatalf("chain stopped at the first stage: %+v", snap)
	}
}
