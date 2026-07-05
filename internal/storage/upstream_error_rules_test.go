package storage

import (
	"context"
	"testing"
)

func TestUpstreamErrorRuleCRUDSortsByPriority(t *testing.T) {
	store, err := OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	high := UpstreamErrorRule{ID: "high", Name: "High", Enabled: true, Priority: 50, Providers: []string{"codex"}, Entrypoints: []string{"responses"}, ModelPatterns: []string{"gpt-5*"}, StatusCodes: []int{429}, BodyKeywords: []string{"quota"}, AccountAction: "cooldown", DownstreamAction: "failover", CooldownSeconds: 60, PreferRetryAfter: true}
	low := UpstreamErrorRule{ID: "low", Name: "Low", Enabled: false, Priority: 5, DownstreamAction: "builtin"}
	if err := store.UpsertUpstreamErrorRule(ctx, high); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertUpstreamErrorRule(ctx, low); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ListUpstreamErrorRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("len=%d", len(rows))
	}
	if rows[0].ID != "low" || rows[1].ID != "high" {
		t.Fatalf("order = %v, want low/high", []string{rows[0].ID, rows[1].ID})
	}
	if rows[1].Providers[0] != "codex" || rows[1].StatusCodes[0] != 429 || !rows[1].PreferRetryAfter {
		t.Fatalf("decoded row incorrectly: %+v", rows[1])
	}

	high.Priority = 1
	high.Enabled = false
	high.CustomMessage = "updated"
	if err := store.UpsertUpstreamErrorRule(ctx, high); err != nil {
		t.Fatal(err)
	}
	got, ok, err := store.GetUpstreamErrorRule(ctx, "high")
	if err != nil || !ok {
		t.Fatalf("get ok=%v err=%v", ok, err)
	}
	if got.Priority != 1 || got.Enabled || got.CustomMessage != "updated" {
		t.Fatalf("upsert did not update: %+v", got)
	}

	if err := store.DeleteUpstreamErrorRule(ctx, "high"); err != nil {
		t.Fatal(err)
	}
	_, ok, err = store.GetUpstreamErrorRule(ctx, "high")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("deleted rule still found")
	}
}
