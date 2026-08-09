package api

import (
	"reflect"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/storage"
)

func TestSortedCapabilityModelsPreservesCanonicalNames(t *testing.T) {
	caps := []storage.ModelCapability{
		{AccountID: "a", ModelSlug: "gpt-5.10-sol"}, {AccountID: "a", ModelSlug: "claude-opus-4.8[1m]"},
		{AccountID: "b", ModelSlug: "GPT-5.2-sol"}, {AccountID: "b", ModelSlug: "gpt-5.10-sol"},
	}
	want := []string{"claude-opus-4.8[1m]", "GPT-5.2-sol", "gpt-5.10-sol"}
	if got := sortedCapabilityModels(caps, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("models=%q want=%q", got, want)
	}
	if got := sortedCapabilityModels(caps, map[string]bool{"a": true}); !reflect.DeepEqual(got, []string{"claude-opus-4.8[1m]", "gpt-5.10-sol"}) {
		t.Fatalf("filtered=%q", got)
	}
}

func TestSummarizeCapabilityModelsAggregatesPerModel(t *testing.T) {
	caps := []storage.ModelCapability{
		{AccountID: "a", ModelSlug: "gpt-5.2", AvailabilityState: capability.AvailabilityVerified, Context1MState: capability.Context1MSupported, NativeMaxContextWindow: 1_000_000, LastProbeAt: 100},
		{AccountID: "b", ModelSlug: "GPT-5.2", AvailabilityState: capability.AvailabilityUnsupported, Context1MState: capability.Context1MUnknown, NativeContextWindow: 400_000, LastProbeAt: 250},
		{AccountID: "c", ModelSlug: "gpt-5.2", AvailabilityState: "", Context1MState: capability.Context1MUnsupported, LastProbeAt: 50},
		{AccountID: "a", ModelSlug: "claude-opus-4.8", AvailabilityState: capability.AvailabilityVerified, Context1MState: capability.Context1MSupported, NativeMaxContextWindow: 200_000, LastProbeAt: 10},
		{AccountID: "a", ModelSlug: "   ", AvailabilityState: capability.AvailabilityVerified},
	}
	got := summarizeCapabilityModels(caps)
	if len(got) != 2 {
		t.Fatalf("rows=%+v", got)
	}
	// Natural order, and the blank slug contributed no row.
	if got[0].Model != "claude-opus-4.8" || got[1].Model != "gpt-5.2" {
		t.Fatalf("order=%q,%q", got[0].Model, got[1].Model)
	}
	// First spelling wins, matching sortedCapabilityModels, so the two lists agree by name.
	gpt := got[1]
	if gpt.Accounts != 3 {
		t.Fatalf("accounts=%d", gpt.Accounts)
	}
	// An empty AvailabilityState counts as unverified rather than being dropped.
	if gpt.Verified != 1 || gpt.Unsupported != 1 || gpt.Unverified != 1 {
		t.Fatalf("availability=%+v", gpt)
	}
	if gpt.Context1MSupported != 1 || gpt.Context1MUnsupported != 1 || gpt.Context1MUnknown != 1 {
		t.Fatalf("context1m=%+v", gpt)
	}
	// Max spans both window fields, and LastProbeAt is the newest not the last seen.
	if gpt.MaxContextWindow != 1_000_000 || gpt.LastProbeAt != 250 {
		t.Fatalf("window=%d probe=%d", gpt.MaxContextWindow, gpt.LastProbeAt)
	}
	// Each triple must partition Accounts exactly, which is what lets a reader draw them as shares.
	for _, row := range got {
		if row.Verified+row.Unverified+row.Unsupported != row.Accounts {
			t.Fatalf("availability does not partition accounts: %+v", row)
		}
		if row.Context1MSupported+row.Context1MUnsupported+row.Context1MUnknown != row.Accounts {
			t.Fatalf("context1m does not partition accounts: %+v", row)
		}
	}
}

func TestSummarizeCapabilityModelsAgreesWithSortedModels(t *testing.T) {
	caps := []storage.ModelCapability{
		{AccountID: "a", ModelSlug: "gpt-5.10-sol"}, {AccountID: "a", ModelSlug: "claude-opus-4.8[1m]"},
		{AccountID: "b", ModelSlug: "GPT-5.2-sol"}, {AccountID: "b", ModelSlug: "gpt-5.10-sol"},
	}
	names := sortedCapabilityModels(caps, nil)
	summary := summarizeCapabilityModels(caps)
	if len(names) != len(summary) {
		t.Fatalf("names=%q summary=%d rows", names, len(summary))
	}
	for i := range names {
		if names[i] != summary[i].Model {
			t.Fatalf("index %d: names=%q summary=%q", i, names[i], summary[i].Model)
		}
	}
}

func TestFilterDisplayCapabilitiesRejectsHiddenAndUnsafeBootstrap(t *testing.T) {
	caps := []storage.ModelCapability{{AccountID: "a", ModelSlug: "hidden", Visibility: "hide"}, {AccountID: "a", ModelSlug: "claude-opus-4.8", Source: "kiro_static_unknown"}, {AccountID: "a", ModelSlug: "claude-sonnet-4.6", Source: "kiro_static_unknown"}}
	got := filterDisplayCapabilities(caps, []storage.Account{{ID: "a", PlanType: "KIRO FREE"}})
	if len(got) != 1 || got[0].ModelSlug != "claude-sonnet-4.6" {
		t.Fatalf("filtered=%+v", got)
	}
}
