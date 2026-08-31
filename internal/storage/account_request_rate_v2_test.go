package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAccountRateV2ReconcilesLineageAndTerminalUsage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(Now(), 0).UTC()
	meter := NewAccountRateMeter(store, "release-v2")
	meter.now = func() time.Time { return now }
	meter.ObserveAttemptClass("acc-v2", "codex", "responses", AgentClassRoot, now)
	meter.ObserveAttemptClass("acc-v2", "codex", "responses", AgentClassSubagent, now)
	meter.ObserveAttemptClass("acc-v2", "codex", "responses", AgentClassSubagent, now)
	if err := meter.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	estimated := UsageDiagnostics{UsageEventID: "evt-v2", AgentClass: AgentClassSubagent, Estimated: true}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "acc-v2", "route", "", "", "gpt-5.6", 90, 0, 90, 0, 0, 0,
		json.RawMessage(`{"estimated":true}`), estimated); err != nil {
		t.Fatal(err)
	}
	real := UsageDiagnostics{UsageEventID: "evt-v2", AgentClass: AgentClassSubagent}
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "acc-v2", "route", "", "", "gpt-5.6", 100, 20, 120, 40, 40, 0,
		json.RawMessage(`{"input_tokens":100,"output_tokens":20}`), real); err != nil {
		t.Fatal(err)
	}
	// A duplicate real terminal frame cannot increment or replace the first real
	// settlement.
	if err := store.InsertUsageRecordWithDiagnostics(ctx, "acc-v2", "route", "", "", "gpt-5.6", 999, 999, 1998, 0, 0, 0,
		json.RawMessage(`{"input_tokens":999,"output_tokens":999}`), real); err != nil {
		t.Fatal(err)
	}

	rates, err := meter.Rates(ctx, []string{"acc-v2"}, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	rate := rates["acc-v2"]
	if rate.AttemptRPM != 3 || rate.AttemptRootRPM != 1 || rate.AttemptSubagentRPM != 2 || rate.AttemptUnknownRPM != 0 {
		t.Fatalf("attempt rate mismatch: %+v", rate)
	}
	if rate.LogicalRPM != 1 || rate.RootRPM != 0 || rate.SubagentRPM != 1 || rate.UnknownRPM != 0 {
		t.Fatalf("logical lineage mismatch: %+v", rate)
	}
	if rate.InputTPM != 100 || rate.CachedInputTPM != 40 || rate.OutputTPM != 20 || rate.TPM != 120 {
		t.Fatalf("terminal token rate mismatch: %+v", rate)
	}
	if rate.RootRPM+rate.SubagentRPM+rate.UnknownRPM != rate.LogicalRPM {
		t.Fatalf("logical lineage does not reconcile: %+v", rate)
	}
	if rate.AttemptRootRPM+rate.AttemptSubagentRPM+rate.AttemptUnknownRPM != rate.AttemptRPM {
		t.Fatalf("attempt lineage does not reconcile: %+v", rate)
	}
}

func TestAccountRateV2ReconcilesClientFamiliesAndArrival(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(Now(), 0).UTC()
	meter := NewAccountRateMeter(store, "release-client-v2")
	meter.now = func() time.Time { return now }
	meter.ObserveAttemptDimensions("acc-client-v2", "codex", "responses", AgentClassRoot, ClientFamilyCodexCLI, now)
	meter.ObserveAttemptDimensions("acc-client-v2", "claude", "messages", AgentClassSubagent, ClientFamilyClaudeCode, now)
	meter.ObserveAttemptDimensions("acc-client-v2", "openai", "responses", AgentClassUnknown, ClientFamilyUnknown, now)
	if err := meter.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	if err := store.RecordAccountRequestRateArrival(ctx, AccountUsageRateEvent{
		EventID: "client-arrival", AccountID: "acc-client-v2", OccurredAt: now.Unix(),
		AgentClass: AgentClassRoot, ClientFamily: ClientFamilyOpenAISDK, ClientConfidence: "high",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleAccountRequestRateEvent(ctx, AccountUsageRateEvent{
		EventID: "client-arrival", InputTokens: 12, CachedInputTokens: 2, OutputTokens: 3, TotalTokens: 15,
		AgentClass: AgentClassRoot, ClientFamily: ClientFamilyOpenAISDK, ClientConfidence: "high", SettlementState: "settled",
	}); err != nil {
		t.Fatal(err)
	}

	rates, err := meter.Rates(ctx, []string{"acc-client-v2"}, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	rate := rates["acc-client-v2"]
	if rate.AttemptRPM != 3 || rate.AttemptCodexCLIRPM != 1 || rate.AttemptClaudeCodeRPM != 1 || rate.AttemptUnknownClientRPM != 1 {
		t.Fatalf("attempt client dimensions mismatch: %+v", rate)
	}
	if rate.LogicalRPM != 1 || rate.OpenAISDKRPM != 1 || rate.UnknownClientRPM != 0 {
		t.Fatalf("logical client dimensions mismatch: %+v", rate)
	}
	if rate.AttemptClaudeCodeRPM+rate.AttemptCodexCLIRPM+rate.AttemptOpenAISDKRPM+rate.AttemptOtherRPM+rate.AttemptUnknownClientRPM != rate.AttemptRPM {
		t.Fatalf("attempt client dimensions do not reconcile: %+v", rate)
	}
	if rate.ClaudeCodeRPM+rate.CodexCLIRPM+rate.OpenAISDKRPM+rate.OtherRPM+rate.UnknownClientRPM != rate.LogicalRPM {
		t.Fatalf("logical client dimensions do not reconcile: %+v", rate)
	}
}
