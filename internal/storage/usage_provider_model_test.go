package storage

import (
	"context"
	"encoding/json"
	"testing"
)

func TestUsageProviderModelKeepsSameModelSeparate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	_ = store.SetSetting(ctx, "usage_accuracy_cutover_at", "0")
	for _, provider := range []string{"codex", "kiro"} {
		if err := store.InsertUsageRecordWithDiagnostics(ctx, provider+"-account", provider+"-route", "", "", "gpt-5.6", 10, 2, 12, 0, 0, 0,
			json.RawMessage(`{"input_tokens":10}`), UsageDiagnostics{UsageProvider: provider, UsageSource: "upstream"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.UsageByProviderModelWindow(ctx, 0, usageWindowOpenEndedUntil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %#v", rows)
	}
	seen := map[string]string{}
	for _, row := range rows {
		seen[row.DimensionKey] = row.DisplayLabel
	}
	if seen["codex::gpt-5.6"] != "Codex · gpt-5.6" || seen["kiro::gpt-5.6"] != "Kiro · gpt-5.6" {
		t.Fatalf("provider/model labels = %#v", seen)
	}
}
