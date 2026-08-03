package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

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
