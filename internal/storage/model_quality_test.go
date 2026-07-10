package storage

import (
	"context"
	"testing"
)

func TestModelQualityStatusAndHistory(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	status := ModelQualityStatus{GroupName: "team-a", ModelSlug: "gpt-5.6-sol", Provider: "codex", State: "suspect", LastOutcome: "confirmed_anomaly", LastProbeAt: 100, ConsecutiveAnomalies: 1, TotalChecks: 1, TotalTokens: 42}
	if err := store.UpsertModelQualityStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetModelQualityStatus(ctx, "team-a", "gpt-5.6-sol", "codex")
	if err != nil || !ok || got.State != "suspect" || got.TotalTokens != 42 {
		t.Fatalf("status = %+v ok=%v err=%v", got, ok, err)
	}
	id, err := store.InsertModelQualityRun(ctx, ModelQualityRun{GroupName: "team-a", ModelSlug: "gpt-5.6-sol", Provider: "codex", ProbeID: "algorithm-1", Phase: "primary", Outcome: "pass", Expected: "14", Actual: "14", CreatedAt: 101})
	if err != nil || id == 0 {
		t.Fatalf("insert run id=%d err=%v", id, err)
	}
	runs, err := store.ListModelQualityRuns(ctx, "team-a", "gpt-5.6-sol", 10)
	if err != nil || len(runs) != 1 || runs[0].Expected != "14" {
		t.Fatalf("runs = %+v err=%v", runs, err)
	}
	if _, err := store.PurgeModelQualityRunsBefore(ctx, 102); err != nil {
		t.Fatal(err)
	}
	runs, _ = store.ListModelQualityRuns(ctx, "", "", 10)
	if len(runs) != 0 {
		t.Fatalf("purge left runs: %+v", runs)
	}
}
