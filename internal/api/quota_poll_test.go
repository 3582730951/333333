package api

import (
	"reflect"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCodexQuotaPollTargetsBatchTokenFiltering(t *testing.T) {
	now := int64(1_700_000_000)
	accounts := []storage.Account{
		{ID: "codex-explicit", Provider: "codex", Status: "active"},
		{ID: "legacy-codex", Status: "active"},
		{ID: "legacy-claude", Status: "active"},
		{ID: "custom", Provider: "deepseek", Status: "active"},
		{ID: "disabled", Provider: "codex", Status: "disabled"},
		{ID: "quarantined", Provider: "codex", Status: "active", QuarantineUntil: now + 60},
		{ID: "missing-token", Provider: "codex", Status: "active"},
	}

	ids := quotaPollCandidateAccountIDs(accounts, now)
	wantIDs := []string{"codex-explicit", "legacy-codex", "legacy-claude", "missing-token"}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("candidate ids = %#v, want %#v", ids, wantIDs)
	}

	targets, missing := codexQuotaPollTargets(accounts, map[string]storage.AccountToken{
		"codex-explicit": {AccountID: "codex-explicit", AccessToken: "codex-at"},
		"legacy-codex":   {AccountID: "legacy-codex", AccessToken: "codex-legacy-at"},
		"legacy-claude":  {AccountID: "legacy-claude", AccessToken: "sk-ant-oat-test"},
	}, now)
	if missing != 1 {
		t.Fatalf("missing token count = %d, want 1", missing)
	}
	gotIDs := make([]string, 0, len(targets))
	for _, target := range targets {
		gotIDs = append(gotIDs, target.Account.ID)
	}
	wantTargets := []string{"codex-explicit", "legacy-codex"}
	if !reflect.DeepEqual(gotIDs, wantTargets) {
		t.Fatalf("target ids = %#v, want %#v", gotIDs, wantTargets)
	}
}

func TestQuotaPollEgressForAccount(t *testing.T) {
	bindings := map[string]storage.AccountEgressBinding{
		"proxied": {AccountID: "proxied", PrimaryEgressID: "egress-proxy"},
		"missing": {AccountID: "missing", PrimaryEgressID: "egress-missing"},
	}
	profiles := quotaPollEgressProfilesByID([]storage.EgressProfile{
		{ID: storage.DefaultDirectEgressID, Type: "direct"},
		{ID: "egress-proxy", Type: "http_proxy", Endpoint: "http://127.0.0.1:8080"},
	})

	if got := quotaPollEgressForAccount("proxied", bindings, profiles); got.ID != "egress-proxy" || got.Type != "http_proxy" {
		t.Fatalf("proxied egress = %#v, want egress-proxy http_proxy", got)
	}
	if got := quotaPollEgressForAccount("direct", bindings, profiles); got.ID != storage.DefaultDirectEgressID || got.Type != "direct" {
		t.Fatalf("direct egress = %#v, want default direct", got)
	}
	if got := quotaPollEgressForAccount("missing", bindings, profiles); got.Type != "direct" {
		t.Fatalf("missing profile fallback = %#v, want direct", got)
	}
}

func TestQuotaPollerUsesBatchTokenLookup(t *testing.T) {
	source := readAPISource(t, "quota_poll.go")
	pollAll := functionBody(t, source, "pollAllCodexQuotas")
	if !strings.Contains(pollAll, ".ListTokensByAccountIDs(") {
		t.Fatal("pollAllCodexQuotas must batch-load account tokens")
	}
	if strings.Contains(pollAll, ".GetToken(") {
		t.Fatal("pollAllCodexQuotas must not use per-account GetToken")
	}
	if !strings.Contains(pollAll, ".attachQuotaPollEgresses(") {
		t.Fatal("pollAllCodexQuotas must preload egress data before launching workers")
	}
	attach := functionBody(t, source, "attachQuotaPollEgresses")
	if !strings.Contains(attach, ".ListEgressBindingsByAccountIDs(") || !strings.Contains(attach, ".ListEgressProfiles(") {
		t.Fatal("attachQuotaPollEgresses must batch-load bindings and profiles")
	}
	pollOne := functionBody(t, source, "pollOneCodexQuota")
	if strings.Contains(pollOne, ".GetToken(") {
		t.Fatal("pollOneCodexQuota must receive a preloaded token instead of querying per account")
	}
	for _, forbidden := range []string{".GetEgressBinding(", ".GetEgressProfile("} {
		if strings.Contains(pollOne, forbidden) {
			t.Fatalf("pollOneCodexQuota must receive preloaded egress data instead of calling %s", forbidden)
		}
	}
}
