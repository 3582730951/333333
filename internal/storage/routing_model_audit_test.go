package storage

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAccountRoutingPolicyAndActualModelAuditRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "routing-policy-account", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountRoutingPolicy(ctx, account.ID, 250, 3); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RoutingWeight != 250 || loaded.RetryMaxAttempts != 3 {
		t.Fatalf("routing policy did not round trip: %+v", loaded)
	}

	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "route-a", "key", "user", "gpt-5.6-terra",
		10, 2, 12, 0, 0, 0, json.RawMessage(`{"usage":{"input_tokens":10,"output_tokens":2}}`), UsageDiagnostics{
			UsageEventID: "model-audit-1", RequestedModel: "gpt-5.6-sol", ResolvedModel: "gpt-5.6-sol",
			ActualModel: "gpt-5.6-terra", ModelOverrideSource: "group_rule",
		}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, account.ID, "route-b", "key", "user", "gpt-5.6-sol",
		8, 1, 9, 0, 0, 0, json.RawMessage(`{"usage":{"input_tokens":8,"output_tokens":1}}`), UsageDiagnostics{
			UsageEventID: "model-audit-2", RequestedModel: "gpt-5.6-sol", ResolvedModel: "gpt-5.6-sol",
			ActualModel: "unknown", ModelOverrideSource: "none",
		}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.ModelAuditWindow(ctx, 0, 0, false, 20)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 || summary.Mismatches != 1 || summary.ActualModelUnavailable != 1 {
		t.Fatalf("unexpected model audit summary: %+v", summary)
	}
	limited, err := store.ModelAuditWindow(ctx, 0, 0, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Rows) != 1 || limited.Requests != 2 || limited.Mismatches != 1 || limited.ActualModelUnavailable != 1 {
		t.Fatalf("limited rows changed aggregate totals: %+v", limited)
	}
	mismatches, err := store.ModelAuditWindow(ctx, 0, 0, true, 20)
	if err != nil {
		t.Fatal(err)
	}
	if mismatches.Requests != 1 || len(mismatches.Rows) != 1 || mismatches.Rows[0].ActualModel != "gpt-5.6-terra" {
		t.Fatalf("unexpected mismatch-only audit: %+v", mismatches)
	}
	if canonicalAuditModel("claude-sonnet-4.6-thinking[1m]") != canonicalAuditModel("claude-sonnet-4-6") {
		t.Fatal("Claude presentation aliases produced a false model mismatch")
	}
}
