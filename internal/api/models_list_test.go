package api

import (
	"reflect"
	"testing"

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

func TestFilterDisplayCapabilitiesRejectsHiddenAndUnsafeBootstrap(t *testing.T) {
	caps := []storage.ModelCapability{{AccountID: "a", ModelSlug: "hidden", Visibility: "hide"}, {AccountID: "a", ModelSlug: "claude-opus-4.8", Source: "kiro_static_unknown"}, {AccountID: "a", ModelSlug: "claude-sonnet-4.6", Source: "kiro_static_unknown"}}
	got := filterDisplayCapabilities(caps, []storage.Account{{ID: "a", PlanType: "KIRO FREE"}})
	if len(got) != 1 || got[0].ModelSlug != "claude-sonnet-4.6" {
		t.Fatalf("filtered=%+v", got)
	}
}
