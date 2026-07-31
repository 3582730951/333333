package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestGoalStorageHeadroomBehaviorProbe(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: config.Default(), store: store}
	const maxBytes, target = int64(1 << 20), int64(512 << 10)
	var before int64
	for i := 0; i < 8 && before <= target+(64<<10); i++ {
		raw := make([]byte, 96<<10)
		state := uint32(i + 1)
		for j := range raw {
			state = state*1664525 + 1013904223
			raw[j] = byte(state >> 24)
		}
		payload := base64.StdEncoding.EncodeToString(raw)
		_, err = store.CommitGoalTurn(ctx, storage.GoalTurn{
			Protocol:          "codex",
			DownstreamKeyHash: fmt.Sprintf("probe-key-%d", i),
			WorkspaceHash:     fmt.Sprintf("probe-workspace-%d", i),
			InitialGoalHash:   fmt.Sprintf("probe-goal-%d", i),
			ResponseID:        fmt.Sprintf("probe-response-%d", i),
			Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: fmt.Sprintf("probe-root-%d", i)}},
			CheckpointPayload: `{"model":"gpt-test","input":[]}`,
			SegmentPayload:    fmt.Sprintf(`{"input":"%s","output":"ok"}`, payload),
			ExpiresAt:         storage.Now() + 86400,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&before); err != nil {
			t.Fatal(err)
		}
	}
	if before <= target || before >= maxBytes {
		t.Fatalf("fixture bytes=%d, want %d < bytes < %d", before, target, maxBytes)
	}
	if err = store.SetSetting(ctx, "goal_storage_max_mb", "1"); err != nil {
		t.Fatal(err)
	}
	snapshot := DiskGuardSnapshot{}
	field := reflect.ValueOf(&snapshot).Elem().FieldByName("GoalStorageTargetBytes")
	if field.IsValid() && field.CanSet() {
		field.SetInt(target)
	}
	after := before
	for i := 0; i < 8 && after > target; i++ {
		server.runSafeDiskCleanup(ctx, &snapshot)
		if err = store.DB().QueryRowContext(ctx, `SELECT COALESCE(SUM(storage_bytes),0) FROM goal_session`).Scan(&after); err != nil {
			t.Fatal(err)
		}
	}
	behavior := "hard-limit-only"
	if after <= target {
		behavior = "proactive-low-watermark"
	}
	fmt.Printf("HEADROOM_PROBE before=%d after=%d hard_max=%d target=%d reclaimed=%d behavior=%s\n",
		before, after, maxBytes, target, before-after, behavior)
}
